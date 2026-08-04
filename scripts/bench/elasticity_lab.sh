#!/usr/bin/env bash
# Elasticity lab: paced inserts → expect upscale → parallel reads → quiet → downscale.
# Verifies no data loss via count_collection.
#
# Env (from terraform outputs or export):
#   ISSUER, BOOTSTRAP (ip|21000), NODES (http://ip:21000), AWS_REGION
#
# Lab thresholds (match TF defaults): up >3000 / 2m, down <200 hold 1m, cooldown 3m
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BENCH="$ROOT/scripts/bench"
TF_DIR="$ROOT/deploy/terraform"
REGION="${AWS_REGION:-eu-west-3}"
OUT="$BENCH/out/elasticity-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$OUT"

COUNT="${COUNT:-12000}"
DURATION_S="${DURATION_S:-240}"   # ~50 inserts/s over 4 min → window >3k
CONCURRENCY="${CONCURRENCY:-32}"
COLLECTION="${COLLECTION:-FrElasticity$(date +%s | tail -c 6)}"
READ_CONCURRENCY="${READ_CONCURRENCY:-16}"
READ_INTERVAL_S="${READ_INTERVAL_S:-5}"
WAIT_SPAWN_S="${WAIT_SPAWN_S:-300}"
WAIT_DOWN_S="${WAIT_DOWN_S:-600}"
PROJECT="${PROJECT:-indexus-aws}"

cd "$TF_DIR"
ISSUER="${ISSUER:-$(terraform output -raw issuer_url)}"
BOOT_IP="${BOOT_IP:-$(terraform output -raw bootstrap_public_ip)}"
BOOTSTRAP="${BOOTSTRAP:-${BOOT_IP}|21000}"
NODES="${NODES:-http://${BOOT_IP}:21000}"

echo "==> out=$OUT"
echo "==> ISSUER=$ISSUER NODES=$NODES collection=$COLLECTION count=$COUNT duration=${DURATION_S}s"

snap() {
  local name="$1"
  curl -sf "http://${BOOT_IP}:19000/autoscale" >"$OUT/${name}.autoscale.json" || echo '{}' >"$OUT/${name}.autoscale.json"
  curl -sf "http://${BOOT_IP}:19000/registered" >"$OUT/${name}.registered.json" || true
  curl -sf "http://${BOOT_IP}:19000/ownership" >"$OUT/${name}.ownership.json" || true
  aws ec2 describe-instances --region "$REGION" \
    --filters "Name=tag:Name,Values=${PROJECT}-spawned" "Name=instance-state-name,Values=pending,running" \
    --query 'Reservations[].Instances[].{Id:InstanceId,Ip:PublicIpAddress,State:State.Name}' \
    --output json >"$OUT/${name}.spawned.json" || echo '[]' >"$OUT/${name}.spawned.json"
}

count_items() {
  local label="$1"
  (cd "$BENCH" && ISSUER="$ISSUER" BOOTSTRAP="$BOOTSTRAP" NODES="$NODES" \
    node count_collection.js --collection "$COLLECTION" --expected "$COUNT") \
    | tee "$OUT/count_${label}.json"
}

# --- baseline ---
snap baseline
echo "==> paced write (${COUNT} over ${DURATION_S}s) + parallel read storm"
(
  cd "$BENCH"
  ISSUER="$ISSUER" NODES="$NODES" \
    node geo_load_france.js --count "$COUNT" --duration "$DURATION_S" \
      --concurrency "$CONCURRENCY" --collection "$COLLECTION" \
    | tee "$OUT/geo_write.json"
) &
WRITE_PID=$!

# Parallel client reads while inserts run (best-effort; may be empty early)
(
  cd "$BENCH"
  end=$((SECONDS + DURATION_S + 30))
  n=0
  while (( SECONDS < end )); do
    ISSUER="$ISSUER" NODES="$NODES" \
      node read_storm_http.js --collection "$COLLECTION" --clients "$READ_CONCURRENCY" --requests 20 \
      >"$OUT/read_during_${n}.json" 2>"$OUT/read_during_${n}.err" || true
    n=$((n + 1))
    sleep "$READ_INTERVAL_S"
  done
) &
READ_PID=$!

wait "$WRITE_PID"
WRITE_RC=$?
echo "==> write finished rc=$WRITE_RC"
snap post_write

# Wait for spawn
echo "==> waiting up to ${WAIT_SPAWN_S}s for spawned instance..."
SPAWNED=0
for i in $(seq 1 $((WAIT_SPAWN_S / 5))); do
  snap "poll_up_$i"
  N=$(python3 -c "import json; print(len(json.load(open('$OUT/poll_up_$i.spawned.json'))))" 2>/dev/null || echo 0)
  AS=$(python3 -c "import json; d=json.load(open('$OUT/poll_up_$i.autoscale.json')); print(d.get('inserts_window'), d.get('scale_ups_done'), d.get('up_in_flight'))" 2>/dev/null || true)
  echo "  t=$((i*5))s spawned=$N autoscale=$AS"
  if [[ "$N" -ge 1 ]]; then
    SPAWNED=1
    break
  fi
  sleep 5
done

if [[ "$SPAWNED" -eq 1 ]]; then
  SP_IP=$(python3 -c "import json; print(json.load(open('$OUT/poll_up_$i.spawned.json'))[0]['Ip'])")
  NODES_MESH="http://${BOOT_IP}:21000,http://${SP_IP}:21000"
  echo "==> mesh NODES=$NODES_MESH — join settle 45s"
  sleep 45
  snap post_spawn
  # Refresh nodes for count
  NODES="$NODES_MESH"
else
  echo "WARN: no spawn observed within timeout (check thresholds / issuer logs)"
  NODES_MESH="$NODES"
fi

echo "==> count after upscale (expect $COUNT)"
count_items after_up || true

# Parallel reads on mesh
(
  cd "$BENCH"
  ISSUER="$ISSUER" NODES="$NODES_MESH" \
    node read_storm_http.js --collection "$COLLECTION" --clients "$READ_CONCURRENCY" --requests 100 \
    | tee "$OUT/read_after_up.json"
) || true

# Quiet period for downscale (spawned idle + hold + cooldown)
echo "==> quiet period for downscale (wait up to ${WAIT_DOWN_S}s)..."
for i in $(seq 1 $((WAIT_DOWN_S / 10))); do
  snap "poll_down_$i"
  N=$(python3 -c "import json; print(len(json.load(open('$OUT/poll_down_$i.spawned.json'))))" 2>/dev/null || echo 0)
  echo "  t=$((i*10))s spawned=$N"
  if [[ "$SPAWNED" -eq 1 && "$N" -eq 0 ]]; then
    echo "==> downscale observed"
    break
  fi
  sleep 10
done

snap final
NODES="$NODES"  # bootstrap only after shrink
echo "==> final count (expect $COUNT — no loss)"
count_items final || true

python3 - <<PY | tee "$OUT/SUMMARY.json"
import json, glob, os
out = "$OUT"
count_final = None
try:
    with open(f"{out}/count_final.json") as f:
        # last JSON object in stream
        text = f.read().strip().splitlines()
        for line in reversed(text):
            line=line.strip()
            if line.startswith("{"):
                count_final = json.loads(line)
                break
except Exception as e:
    count_final = {"error": str(e)}
spawned_final = json.load(open(f"{out}/final.spawned.json"))
auto = json.load(open(f"{out}/final.autoscale.json"))
summary = {
    "collection": "$COLLECTION",
    "expected": int("$COUNT"),
    "spawned_at_end": len(spawned_final),
    "autoscale": auto,
    "count_final": count_final,
    "write_rc": int("$WRITE_RC"),
}
print(json.dumps(summary, indent=2))
PY

echo "==> done → $OUT"
kill "$READ_PID" 2>/dev/null || true
