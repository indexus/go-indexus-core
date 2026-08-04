#!/usr/bin/env bash
# Controlled downscale battle test:
#   1) bootstrap only → write N density items
#   2) wait for exactly K spawned (default 2 → mesh size 3)
#   3) pause writes, settle, count (expect N)
#   4) wait for full SoftLeave downscale to bootstrap
#   5) count again (expect N) — no loss
#
# Designed to validate SoftLeave ACK-before-drop + drain-lock serialization.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BENCH="$ROOT/scripts/bench"
TF_DIR="$ROOT/deploy/terraform"
REGION="${AWS_REGION:-eu-west-3}"
OUT="$BENCH/out/downscale-battle-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$OUT"

SPAWN_TARGET="${SPAWN_TARGET:-2}"
COUNT="${COUNT:-8000}"
DURATION_S="${DURATION_S:-180}"
CONCURRENCY="${CONCURRENCY:-24}"
COLLECTION="${COLLECTION:-DownBattle$(date +%s | tail -c 6)}"
PROJECT="${PROJECT:-indexus-aws}"
WAIT_SPAWN_S="${WAIT_SPAWN_S:-600}"
WAIT_DOWN_S="${WAIT_DOWN_S:-1200}"

CSV="$BENCH/data/world_density_100k.csv"
if [[ ! -f "$CSV" ]]; then
  SRC="${DENSITY_CSV_SRC:-$HOME/Development/indexus/simulation/data/basic/items/items-100000-sigmoid.csv}"
  mkdir -p "$BENCH/data"
  cp "$SRC" "$CSV"
fi

cd "$TF_DIR"
# Force fresh env from terraform (avoid stale exported NODES)
ISSUER=$(terraform output -raw issuer_url)
BOOT_IP=$(terraform output -raw bootstrap_public_ip)
BOOTSTRAP="${BOOT_IP}|21000"
NODES="http://${BOOT_IP}:21000"
export ISSUER BOOT_IP BOOTSTRAP NODES

echo "==> out=$OUT"
echo "==> ISSUER=$ISSUER spawn_target=$SPAWN_TARGET count=$COUNT"

# cleanup zombie spawned from prior runs
ZOMBIES=$(aws ec2 describe-instances --region "$REGION" \
  --filters "Name=tag:Name,Values=${PROJECT}-spawned" "Name=instance-state-name,Values=running" \
  --query 'Reservations[].Instances[].InstanceId' --output text)
if [[ -n "${ZOMBIES// /}" && "$ZOMBIES" != "None" ]]; then
  echo "==> terminating leftover spawned: $ZOMBIES"
  # soft-leave first when monitoring is up
  for id in $ZOMBIES; do
    ip=$(aws ec2 describe-instances --region "$REGION" --instance-ids "$id" \
      --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
    curl -sf --max-time 120 "http://${ip}:19000/leave?timeout_s=90" >"$OUT/preleave_$id.json" || true
  done
  aws ec2 terminate-instances --region "$REGION" --instance-ids $ZOMBIES >/dev/null || true
  sleep 20
fi

snap() {
  local name="$1"
  curl -sf "http://${BOOT_IP}:19000/autoscale" >"$OUT/${name}.autoscale.json" || echo '{}' >"$OUT/${name}.autoscale.json"
  curl -sf "http://${BOOT_IP}:19000/count" >"$OUT/${name}.count.json" || echo '{}' >"$OUT/${name}.count.json"
  curl -sf "http://${BOOT_IP}:19000/registered" >"$OUT/${name}.registered.json" || true
  aws ec2 describe-instances --region "$REGION" \
    --filters "Name=tag:Name,Values=${PROJECT}-spawned" "Name=instance-state-name,Values=pending,running" \
    --query 'Reservations[].Instances[].{Id:InstanceId,Ip:PublicIpAddress,State:State.Name}' \
    --output json >"$OUT/${name}.spawned.json" || echo '[]' >"$OUT/${name}.spawned.json"
}

mesh_nodes() {
  python3 - <<PY
import json, os
boot=os.environ["BOOT_IP"]
nodes=[f"http://{boot}:21000"]
for i in json.load(open(os.environ["SPAWNED_JSON"])):
    ip=i.get("Ip")
    if ip: nodes.append(f"http://{ip}:21000")
print(",".join(nodes))
PY
}

bootstrap_http_count() {
  # Tree walk that NEVER follows undialable contacts — bootstrap only.
  python3 - <<PY
import json, urllib.request, urllib.parse, collections, sys
BOOT="$BOOT_IP"
ISSUER="$ISSUER"
COLL="$COLLECTION"
tok=json.load(urllib.request.urlopen(urllib.request.Request(
    f"{ISSUER}/v1/issue/token",
    data=json.dumps({"client_id":"battle-count","scopes":["read"]}).encode(),
    headers={"Content-Type":"application/json"},
    method="POST")))["token"]

def get(loc):
    req=urllib.request.Request(
        f"http://{BOOT}:21000/set?collection={urllib.parse.quote(COLL)}&location={urllib.parse.quote(loc)}",
        headers={"Authorization": f"Bearer {tok}"})
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.load(r)

visited=set(); q=collections.deque(["@"])
ids=set(); leaves=0; redirects=0
err=None
while q:
    loc=q.popleft()
    if loc in visited: continue
    visited.add(loc)
    try:
        data=get(loc)
    except Exception as e:
        err=str(e); break
    s=data.get("set") or {}
    c=data.get("contact") or {}
    port=c.get("port") or 0
    ip=c.get("ip") or ""
    if (not s) and (port<=0 or ip==""):
        redirects+=1
        continue
    if (not s) and c.get("name"):
        redirects+=1
        continue
    for key in s:
        if ":" in key:
            leaves+=1; ids.add(key.split(":")[-1])
        elif key not in visited:
            q.append(key)
local=json.load(urllib.request.urlopen(f"http://{BOOT}:19000/count")).get("count")
out={
  "collection": COLL,
  "http_unique_ids": len(ids),
  "http_leaf_keys": leaves,
  "locations": len(visited),
  "dead_or_remote_redirects": redirects,
  "monitor_count": local,
  "expected": int("$COUNT"),
}
if err:
    out["error"]=err
print(json.dumps(out, indent=2))
PY
}

snap baseline

echo "==> density write"
(cd "$BENCH" && DENSITY_CSV="$CSV" ISSUER="$ISSUER" NODES="$NODES" \
  node geo_load_density.js --count "$COUNT" --duration "$DURATION_S" \
    --concurrency "$CONCURRENCY" --collection "$COLLECTION" | tee "$OUT/write.json") &
WPID=$!

echo "==> wait for $SPAWN_TARGET spawned"
for i in $(seq 1 $((WAIT_SPAWN_S/10))); do
  snap "up_$i"
  N=$(python3 -c "import json;print(len(json.load(open('$OUT/up_$i.spawned.json'))))")
  AS=$(python3 -c "import json;d=json.load(open('$OUT/up_$i.autoscale.json'));print(d.get('inserts_window'),d.get('scale_ups_done'))")
  echo "  t=$((i*10))s spawned=$N autoscale=$AS"
  if [[ "$N" -ge "$SPAWN_TARGET" ]]; then
    echo "==> spawn target reached"
    break
  fi
  sleep 10
done

wait "$WPID" || true
snap post_write
sleep 30

export SPAWNED_JSON="$OUT/post_write.spawned.json"
MESH=$(mesh_nodes)
echo "==> mesh=$MESH"
echo "==> count after up (bootstrap-local walk + monitor)"
bootstrap_http_count | tee "$OUT/count_after_up.json"

# Force quiet: no more writes. Wait SoftLeave downscale (serialized).
echo "==> waiting downscale to 0 spawned (serialized SoftLeave)"
for i in $(seq 1 $((WAIT_DOWN_S/15))); do
  snap "down_$i"
  N=$(python3 -c "import json;print(len(json.load(open('$OUT/down_$i.spawned.json'))))")
  REG=$(python3 -c "import json;print(len(json.load(open('$OUT/down_$i.registered.json')).get('hosts',[])))" 2>/dev/null || echo '?')
  echo "  t=$((i*15))s spawned=$N registered≈$REG"
  if [[ "$N" -eq 0 ]]; then
    echo "==> fully downscaled"
    break
  fi
  sleep 15
done

sleep 20
snap final
echo "==> FINAL count"
bootstrap_http_count | tee "$OUT/count_final.json"

python3 - <<PY | tee "$OUT/SUMMARY.json"
import json
up=json.load(open("$OUT/count_after_up.json"))
fin=json.load(open("$OUT/count_final.json"))
exp=int("$COUNT")
summary={
  "expected": exp,
  "after_up": up,
  "final": fin,
  "pass_monitor_final": fin.get("monitor_count")==exp,
  "pass_http_final": fin.get("http_unique_ids")==exp,
  "pass_no_dead_root": fin.get("dead_or_remote_redirects",0)==0 or fin.get("http_unique_ids",0)>0,
}
print(json.dumps(summary, indent=2))
ok = summary["pass_monitor_final"] or summary["pass_http_final"]
raise SystemExit(0 if ok else 2)
PY

echo "==> done $OUT"
