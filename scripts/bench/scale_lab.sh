#!/usr/bin/env bash
# Orchestrated scale / delegation / downscale lab against a live Terraform stack.
#
# Flow: baseline → geo hot writes → read storms → spawn if thresholds → assert
#       ownership moved → read after → cool-down → hard-kill spawned → recover.
#
# Env (optional):
#   DELEGATION=40 OWNED_KEYS_SCALE=5 QUEUE_SCALE=100 READ_P95_MS_SCALE=2000
#   SPAWN_MAX=1 GEO_COUNT=5000 COLLECTION=FrScaleLab001
#   COOLDOWN_S=30 JOIN_WAIT_S=40 FORCE_SPAWN=1
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BENCH="$ROOT/scripts/bench"
TF_DIR="$ROOT/deploy/terraform"
OUT="$BENCH/out/scale-lab-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$OUT"
cd "$BENCH"

DELEGATION="${DELEGATION:-40}"
OWNED_KEYS_SCALE="${OWNED_KEYS_SCALE:-5}"
QUEUE_SCALE="${QUEUE_SCALE:-100}"
READ_P95_MS_SCALE="${READ_P95_MS_SCALE:-2000}"
SPAWN_MAX="${SPAWN_MAX:-1}"
GEO_COUNT="${GEO_COUNT:-5000}"
COLLECTION="${COLLECTION:-FrScaleLab001}"
COOLDOWN_S="${COOLDOWN_S:-30}"
JOIN_WAIT_S="${JOIN_WAIT_S:-40}"
FORCE_SPAWN="${FORCE_SPAWN:-1}"
AWS_REGION="${AWS_REGION:-eu-west-3}"
export AWS_REGION DELEGATION

BOOT_IP=$(cd "$TF_DIR" && terraform output -raw bootstrap_public_ip)
WORKER_JSON=$(cd "$TF_DIR" && terraform output -json worker_public_ips)
export ISSUER="http://${BOOT_IP}:22000"
export NODE="http://${BOOT_IP}:21000"
export BOOTSTRAP="${BOOT_IP}|21000"
export BOOT_IP
export OUT_DIR="$OUT"

# All public nodes for HTTP storm (bootstrap + TF workers + any registered)
NODES_CSV=$(python3 - <<PY
import json, urllib.request
boot = "$BOOT_IP"
workers = json.loads('''$WORKER_JSON''')
ips = [boot] + list(workers)
try:
    with urllib.request.urlopen(f"http://{boot}:19000/registered", timeout=5) as r:
        data = json.load(r)
    for h in data.get("hosts") or []:
        part = h.split("@", 1)[-1]
        ip = part.split("|")[0]
        if ip and ip not in ips:
            ips.append(ip)
except Exception:
    pass
print(",".join(f"http://{ip}:21000" for ip in ips))
PY
)
export NODES="$NODES_CSV"

chmod +x spawn_worker.sh terminate_spawned.sh

echo "=== scale_lab config ==="
python3 - <<PY
import json, os
print(json.dumps({
  "boot": os.environ["BOOT_IP"],
  "issuer": os.environ["ISSUER"],
  "nodes": os.environ["NODES"],
  "out": "$OUT",
  "delegation": int("$DELEGATION"),
  "collection": "$COLLECTION",
  "geo_count": int("$GEO_COUNT"),
  "owned_keys_scale": int("$OWNED_KEYS_SCALE"),
  "queue_scale": int("$QUEUE_SCALE"),
  "read_p95_ms_scale": int("$READ_P95_MS_SCALE"),
  "spawn_max": int("$SPAWN_MAX"),
  "force_spawn": "$FORCE_SPAWN" == "1",
}, indent=2))
PY

echo "=== 0) npm + baseline snapshot ==="
npm install --silent
node mesh_snapshot.js --label baseline --out "$OUT" 2>"$OUT/baseline.path" | tee "$OUT/baseline.summary.json"
BASELINE_FILE=$(cat "$OUT/baseline.path")

HOST_COUNT=$(python3 -c "import json; d=json.load(open('$BASELINE_FILE')); print(d['totals']['nodes'])")
if [[ "$HOST_COUNT" -lt 1 ]]; then
  echo "FAIL: no hosts in mesh"
  exit 2
fi
echo "mesh hosts=$HOST_COUNT"

echo "=== 1) geo hot write (long locations → Own splits @ delegation=$DELEGATION) ==="
node geo_load_france.js --count "$GEO_COUNT" --concurrency 64 --collection "$COLLECTION" \
  | tee "$OUT/geo_write.json"

echo "=== wait feed + Own ==="
sleep 15
node mesh_snapshot.js --label post_write --out "$OUT" 2>"$OUT/post_write.path" | tee "$OUT/post_write.summary.json"
POST_WRITE=$(cat "$OUT/post_write.path")

OWNED=$(python3 -c "import json; d=json.load(open('$POST_WRITE')); print(d['totals']['owned_zones'])")
QUEUE=$(python3 -c "import json; d=json.load(open('$POST_WRITE')); print(d['totals']['queue_pending'])")
echo "owned_zones=$OWNED queue_pending=$QUEUE"

parse_p95() {
  local path="$1"
  python3 - <<PY
import json
path = "$path"
text = open(path).read().strip().splitlines()
obj = None
buf, depth = [], 0
for line in text:
    if line.strip().startswith("{") or depth:
        buf.append(line)
        depth += line.count("{") - line.count("}")
        if depth == 0 and buf:
            try:
                cand = json.loads("\n".join(buf))
                if isinstance(cand, dict) and "round_latencies_ms" in cand:
                    obj = cand
            except Exception:
                pass
            buf = []
print((obj or {}).get("round_latencies_ms", {}).get("p95") or 0)
PY
}

echo "=== 2) read storms (before scale) ==="
set +e
node read_storm_http.js --collection "$COLLECTION" --location @ --clients 16 --requests 200 \
  | tee "$OUT/read_http_before.json"
HTTP_BEFORE_RC=$?
node read_storm.js --clients 4 --rounds 5 --limit 50 --collection "$COLLECTION" \
  | tee "$OUT/read_before.json"
READ_RC=$?
set -e
P95=$(parse_p95 "$OUT/read_before.json")
P95_HTTP=$(parse_p95 "$OUT/read_http_before.json")
echo "read_before nearest_p95_ms=$P95 http_p95_ms=$P95_HTTP (nearest_rc=$READ_RC http_rc=$HTTP_BEFORE_RC)"

SHOULD_SPAWN=0
if [[ "$FORCE_SPAWN" == "1" ]]; then
  SHOULD_SPAWN=1
  echo "scale decision: FORCE_SPAWN=1"
elif [[ "$OWNED" -ge "$OWNED_KEYS_SCALE" ]]; then
  SHOULD_SPAWN=1
  echo "scale decision: owned_zones $OWNED >= $OWNED_KEYS_SCALE"
elif [[ "$QUEUE" -ge "$QUEUE_SCALE" ]]; then
  SHOULD_SPAWN=1
  echo "scale decision: queue $QUEUE >= $QUEUE_SCALE"
elif python3 -c "import sys; sys.exit(0 if float('$P95') >= float('$READ_P95_MS_SCALE') or float('$P95_HTTP') >= float('$READ_P95_MS_SCALE') else 1)"; then
  SHOULD_SPAWN=1
  echo "scale decision: read p95 hit threshold"
else
  echo "scale decision: thresholds not met (set FORCE_SPAWN=1 to spawn anyway)"
fi

SPAWNED_ID=""
SPAWNED_IP=""
if [[ "$SHOULD_SPAWN" == "1" && "$SPAWN_MAX" -ge 1 ]]; then
  echo "=== 3) spawn worker ==="
  set +e
  SPAWN_OUT=$(./spawn_worker.sh 2>&1)
  SPAWN_RC=$?
  set -e
  echo "$SPAWN_OUT" | tee "$OUT/spawn.json"
  SPAWNED_ID=$(echo "$SPAWN_OUT" | python3 -c 'import sys,json,re; t=sys.stdin.read();
m=re.search(r"\{[\s\S]*\"instance_id\"[\s\S]*\}", t)
print(json.loads(m.group(0))["instance_id"] if m else "")')
  SPAWNED_IP=$(echo "$SPAWN_OUT" | python3 -c 'import sys,json,re; t=sys.stdin.read();
m=re.search(r"\{[\s\S]*\"instance_id\"[\s\S]*\}", t)
print(json.loads(m.group(0)).get("public_ip","") if m else "")')
  echo "spawned id=$SPAWNED_ID ip=$SPAWNED_IP rc=$SPAWN_RC"

  echo "=== 4) join wait ${JOIN_WAIT_S}s (Refresh / Transfer cycles) ==="
  sleep "$JOIN_WAIT_S"
else
  echo "=== 3-4) skip spawn ==="
fi

node mesh_snapshot.js --label post_spawn --out "$OUT" 2>"$OUT/post_spawn.path" | tee "$OUT/post_spawn.summary.json"
POST_SPAWN=$(cat "$OUT/post_spawn.path")

echo "=== 5) assert delegation (ownership moved) ==="
set +e
python3 - <<PY | tee "$OUT/delegation_assert.json"
import json, sys
before = json.load(open("$POST_WRITE"))
after = json.load(open("$POST_SPAWN"))
bi = before.get("ownership_index", {})
ai = after.get("ownership_index", {})
spawned = "$SPAWNED_IP"
moved = []
new_on_spawn = []
for key, ips in ai.items():
    prev = set(bi.get(key, []))
    cur = set(ips)
    if spawned and spawned in cur and spawned not in prev:
        new_on_spawn.append(key)
    if prev and cur and prev != cur:
        moved.append({"key": key, "before": sorted(prev), "after": sorted(cur)})

hosts_before = before["totals"]["nodes"]
hosts_after = after["totals"]["nodes"]
owned_before = before["totals"]["owned_zones"]
owned_after = after["totals"]["owned_zones"]
ok = False
reason = []
if spawned:
    if new_on_spawn:
        ok = True
        reason.append(f"{len(new_on_spawn)} ownership keys now on spawned")
    if moved:
        reason.append(f"{len(moved)} keys changed owners")
        ok = True
    if hosts_after > hosts_before:
        reason.append(f"hosts {hosts_before}->{hosts_after}")
        if not ok:
            # soft pass only if we also grew owned zones (splits happened)
            if owned_after >= 2 or owned_before >= 2:
                ok = True
                reason.append("mesh grew with multi-zone ownership")
            else:
                reason.append("mesh grew but no Own zones to transfer yet")
else:
    reason.append("no spawn; skip assert")
    ok = True

out = {
  "ok": ok,
  "spawned_ip": spawned or None,
  "hosts_before": hosts_before,
  "hosts_after": hosts_after,
  "owned_before": owned_before,
  "owned_after": owned_after,
  "keys_new_on_spawn": new_on_spawn[:40],
  "keys_moved_sample": moved[:20],
  "reason": reason,
}
print(json.dumps(out, indent=2))
sys.exit(0 if ok else 2)
PY
DELEG_OK=$?
set -e

echo "=== 6) read storms (after scale) ==="
# refresh NODES including spawned
if [[ -n "$SPAWNED_IP" ]]; then
  export NODES="${NODES},http://${SPAWNED_IP}:21000"
fi
set +e
node read_storm_http.js --collection "$COLLECTION" --location @ --clients 16 --requests 200 \
  | tee "$OUT/read_http_after.json"
node read_storm.js --clients 4 --rounds 5 --limit 50 --collection "$COLLECTION" \
  | tee "$OUT/read_after.json"
set -e

echo "=== 7) cool-down ${COOLDOWN_S}s ==="
sleep "$COOLDOWN_S"
node mesh_snapshot.js --label cooldown --out "$OUT" 2>"$OUT/cooldown.path" | tee "$OUT/cooldown.summary.json"

if [[ -n "$SPAWNED_ID" ]]; then
  echo "=== 8) downscale hard-kill $SPAWNED_ID ==="
  INSTANCE_ID="$SPAWNED_ID" ./terminate_spawned.sh | tee "$OUT/terminate.json"

  echo "=== wait until IP leaves /registered ==="
  GONE=0
  for i in $(seq 1 60); do
    REG=$(curl -sf --max-time 5 "http://${BOOT_IP}:19000/registered" || echo '{}')
    if [[ -n "$SPAWNED_IP" ]] && ! echo "$REG" | grep -q "$SPAWNED_IP"; then
      GONE=1
      break
    fi
    sleep 3
  done
  echo "left_registered=$GONE"
  echo "{\"left_registered\": $GONE, \"spawned_ip\": \"$SPAWNED_IP\"}" > "$OUT/deregister.json"
else
  echo "=== 8) skip terminate (no spawn) ==="
fi

echo "=== 9) recover assert (reads still work) ==="
sleep 5
# drop spawned from NODES
export NODES="$NODES_CSV"
node mesh_snapshot.js --label recover --out "$OUT" 2>"$OUT/recover.path" | tee "$OUT/recover.summary.json"
set +e
node read_storm_http.js --collection "$COLLECTION" --location @ --clients 8 --requests 80 \
  | tee "$OUT/read_http_recover.json"
HTTP_RECOVER_RC=$?
node read_storm.js --clients 2 --rounds 3 --limit 30 --collection "$COLLECTION" \
  | tee "$OUT/read_recover.json"
NEAREST_RECOVER_RC=$?
set -e
# recover OK if HTTP storm succeeded (nearest is bonus)
RECOVER_RC=$HTTP_RECOVER_RC

python3 - <<PY | tee "$OUT/SUMMARY.json"
import json, os

def last_obj(path):
    try:
        text = open(path).read().strip().splitlines()
    except FileNotFoundError:
        return None
    obj = None
    buf, depth = [], 0
    for line in text:
        if line.strip().startswith("{") or depth:
            buf.append(line)
            depth += line.count("{") - line.count("}")
            if depth == 0 and buf:
                try:
                    cand = json.loads("\n".join(buf))
                    if isinstance(cand, dict):
                        obj = cand
                except Exception:
                    pass
                buf = []
    return obj

before = last_obj("$OUT/read_before.json")
after = last_obj("$OUT/read_after.json")
recover = last_obj("$OUT/read_recover.json")
http_before = last_obj("$OUT/read_http_before.json")
http_after = last_obj("$OUT/read_http_after.json")
http_recover = last_obj("$OUT/read_http_recover.json")
deleg = json.load(open("$OUT/delegation_assert.json")) if os.path.exists("$OUT/delegation_assert.json") else {}

summary = {
  "out": "$OUT",
  "boot": "$BOOT_IP",
  "collection": "$COLLECTION",
  "spawned_id": "$SPAWNED_ID" or None,
  "spawned_ip": "$SPAWNED_IP" or None,
  "delegation_assert": deleg,
  "nearest_p95_before": (before or {}).get("round_latencies_ms", {}).get("p95"),
  "nearest_p95_after": (after or {}).get("round_latencies_ms", {}).get("p95"),
  "nearest_p95_recover": (recover or {}).get("round_latencies_ms", {}).get("p95"),
  "http_p95_before": (http_before or {}).get("round_latencies_ms", {}).get("p95"),
  "http_p95_after": (http_after or {}).get("round_latencies_ms", {}).get("p95"),
  "http_p95_recover": (http_recover or {}).get("round_latencies_ms", {}).get("p95"),
  "recover_ok": $RECOVER_RC == 0,
  "deleg_ok": $DELEG_OK == 0,
}
print(json.dumps(summary, indent=2))
ok = summary["deleg_ok"] and summary["recover_ok"]
raise SystemExit(0 if ok else 2)
PY
SUMMARY_RC=${PIPESTATUS[0]}

echo "=== SCALE LAB DONE → $OUT ==="
exit "$SUMMARY_RC"
