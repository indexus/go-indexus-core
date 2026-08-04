#!/usr/bin/env bash
# 10-node elasticity lab with real-world population density inserts.
#
# Mesh target: 1 bootstrap + 9 spawned = 10 nodes.
# Dataset: simulation density.tif → world_density_100k.csv (South Asia /
# Europe / Africa dense; oceans nearly empty).
#
# Prerequisites:
#   - terraform stack with spawn_max=9 (see terraform.tfvars)
#   - bootstrap -autoscale -spawnMax 9 -scaleCooldown 45s (or matching TF)
#   - scripts/bench/data/world_density_100k.csv (copied from indexus/simulation)
#
# Env overrides: COUNT DURATION_S TARGET_NODES WAIT_SPAWN_S WAIT_DOWN_S
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BENCH="$ROOT/scripts/bench"
TF_DIR="$ROOT/deploy/terraform"
REGION="${AWS_REGION:-eu-west-3}"
OUT="$BENCH/out/elasticity10-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$OUT"

TARGET_NODES="${TARGET_NODES:-10}"
SPAWN_TARGET=$((TARGET_NODES - 1))
COUNT="${COUNT:-50000}"
DURATION_S="${DURATION_S:-900}"
CONCURRENCY="${CONCURRENCY:-32}"
COLLECTION="${COLLECTION:-WorldDens$(date +%s | tail -c 6)}"
READ_CONCURRENCY="${READ_CONCURRENCY:-16}"
READ_INTERVAL_S="${READ_INTERVAL_S:-10}"
WAIT_SPAWN_S="${WAIT_SPAWN_S:-1200}"
WAIT_DOWN_S="${WAIT_DOWN_S:-1800}"
PROJECT="${PROJECT:-indexus-aws}"

CSV="$BENCH/data/world_density_100k.csv"
if [[ ! -f "$CSV" ]]; then
  SRC="${DENSITY_CSV_SRC:-$HOME/Development/indexus/simulation/data/basic/items/items-100000-sigmoid.csv}"
  if [[ -f "$SRC" ]]; then
    mkdir -p "$BENCH/data"
    cp "$SRC" "$CSV"
    echo "==> copied density CSV from $SRC"
  else
    echo "missing $CSV — set DENSITY_CSV_SRC or copy world_density_100k.csv"
    exit 1
  fi
fi

cd "$TF_DIR"
ISSUER="${ISSUER:-$(terraform output -raw issuer_url)}"
BOOT_IP="${BOOT_IP:-$(terraform output -raw bootstrap_public_ip)}"
BOOTSTRAP="${BOOTSTRAP:-${BOOT_IP}|21000}"
NODES="${NODES:-http://${BOOT_IP}:21000}"

echo "==> out=$OUT"
echo "==> target_nodes=$TARGET_NODES spawn_extra=$SPAWN_TARGET"
echo "==> ISSUER=$ISSUER collection=$COLLECTION count=$COUNT duration=${DURATION_S}s"

snap() {
  local name="$1"
  curl -sf "http://${BOOT_IP}:19000/autoscale" >"$OUT/${name}.autoscale.json" || echo '{}' >"$OUT/${name}.autoscale.json"
  curl -sf "http://${BOOT_IP}:19000/registered" >"$OUT/${name}.registered.json" || true
  aws ec2 describe-instances --region "$REGION" \
    --filters "Name=tag:Name,Values=${PROJECT}-spawned" "Name=instance-state-name,Values=pending,running" \
    --query 'Reservations[].Instances[].{Id:InstanceId,Ip:PublicIpAddress,State:State.Name}' \
    --output json >"$OUT/${name}.spawned.json" || echo '[]' >"$OUT/${name}.spawned.json"
}

mesh_nodes() {
  python3 - <<'PY'
import json, os, sys
boot = os.environ["BOOT_IP"]
path = os.environ["SPAWNED_JSON"]
nodes = [f"http://{boot}:21000"]
try:
    for i in json.load(open(path)):
        ip = i.get("Ip")
        if ip:
            nodes.append(f"http://{ip}:21000")
except Exception:
    pass
print(",".join(nodes))
PY
}

count_items() {
  local label="$1"
  local nodes="$2"
  (cd "$BENCH" && ISSUER="$ISSUER" BOOTSTRAP="$BOOTSTRAP" NODES="$nodes" \
    node count_collection.js --collection "$COLLECTION" --expected "$COUNT") \
    | tee "$OUT/count_${label}.json"
}

# --- baseline ---
snap baseline
echo "==> density-weighted write (${COUNT} over ${DURATION_S}s) + parallel reads"

(
  cd "$BENCH"
  DENSITY_CSV="$CSV" ISSUER="$ISSUER" NODES="$NODES" \
    node geo_load_density.js --count "$COUNT" --duration "$DURATION_S" \
      --concurrency "$CONCURRENCY" --collection "$COLLECTION" \
    | tee "$OUT/geo_write.json"
) &
WRITE_PID=$!

(
  cd "$BENCH"
  end=$((SECONDS + DURATION_S + 60))
  n=0
  while (( SECONDS < end )); do
    export BOOT_IP SPAWNED_JSON="$OUT/poll_live.spawned.json"
    snap poll_live >/dev/null 2>&1 || true
    MESH=$(BOOT_IP="$BOOT_IP" SPAWNED_JSON="$OUT/poll_live.spawned.json" mesh_nodes)
    ISSUER="$ISSUER" NODES="$MESH" \
      node read_storm_http.js --collection "$COLLECTION" --clients "$READ_CONCURRENCY" --requests 24 \
      >"$OUT/read_during_${n}.json" 2>"$OUT/read_during_${n}.err" || true
    n=$((n + 1))
    sleep "$READ_INTERVAL_S"
  done
) &
READ_PID=$!

echo "==> waiting up to ${WAIT_SPAWN_S}s for ${SPAWN_TARGET} spawned (${TARGET_NODES} total)..."
REACHED=0
for i in $(seq 1 $((WAIT_SPAWN_S / 10))); do
  snap "poll_up_$i"
  N=$(python3 -c "import json; print(len(json.load(open('$OUT/poll_up_$i.spawned.json'))))" 2>/dev/null || echo 0)
  AS=$(python3 -c "import json; d=json.load(open('$OUT/poll_up_$i.autoscale.json')); print(d.get('inserts_window'), d.get('scale_ups_done'), d.get('up_in_flight'))" 2>/dev/null || true)
  TOTAL=$((N + 1))
  echo "  t=$((i*10))s spawned=$N total≈$TOTAL autoscale=$AS"
  if [[ "$N" -ge "$SPAWN_TARGET" ]]; then
    REACHED=1
    echo "==> mesh size target reached"
    break
  fi
  # keep writing in background; if write finished early, still wait for spawns
  if ! kill -0 "$WRITE_PID" 2>/dev/null; then
    :
  fi
  sleep 10
done

wait "$WRITE_PID" || true
kill "$READ_PID" 2>/dev/null || true

snap post_write
export BOOT_IP SPAWNED_JSON="$OUT/post_write.spawned.json"
MESH=$(BOOT_IP="$BOOT_IP" SPAWNED_JSON="$OUT/post_write.spawned.json" mesh_nodes)
echo "==> mesh NODES=$MESH"
sleep 45

echo "==> count after growth (expect $COUNT)"
count_items after_up "$MESH" || true

(
  cd "$BENCH"
  ISSUER="$ISSUER" NODES="$MESH" \
    node read_storm_http.js --collection "$COLLECTION" --clients "$READ_CONCURRENCY" --requests 120 \
    | tee "$OUT/read_after_up.json"
) || true

if [[ "$REACHED" -ne 1 ]]; then
  echo "WARN: only reached partial mesh — continue quiet/downscale anyway"
fi

echo "==> quiet period for downscale (wait up to ${WAIT_DOWN_S}s)..."
for i in $(seq 1 $((WAIT_DOWN_S / 15))); do
  snap "poll_down_$i"
  N=$(python3 -c "import json; print(len(json.load(open('$OUT/poll_down_$i.spawned.json'))))" 2>/dev/null || echo 0)
  echo "  t=$((i*15))s spawned=$N"
  if [[ "$N" -eq 0 ]]; then
    echo "==> fully downscaled to bootstrap"
    break
  fi
  sleep 15
done

snap final
echo "==> final count on bootstrap (expect $COUNT — no loss)"
count_items final "http://${BOOT_IP}:21000" || true

python3 - <<PY | tee "$OUT/SUMMARY.json"
import json
out = "$OUT"
def load_count(label):
    try:
        text = open(f"{out}/count_{label}.json").read().strip().splitlines()
        for line in reversed(text):
            if line.startswith("{"):
                return json.loads(line)
    except Exception as e:
        return {"error": str(e)}
    return None
spawned_final = json.load(open(f"{out}/final.spawned.json"))
auto = json.load(open(f"{out}/final.autoscale.json"))
print(json.dumps({
    "collection": "$COLLECTION",
    "expected": int("$COUNT"),
    "target_nodes": int("$TARGET_NODES"),
    "spawned_at_end": len(spawned_final),
    "autoscale": auto,
    "count_after_up": load_count("after_up"),
    "count_final": load_count("final"),
    "reached_target": bool(int("$REACHED")),
}, indent=2))
PY

echo "==> done → $OUT"
