#!/usr/bin/env bash
# Lifecycle battle: volume → upscale → hotspot → settle → SoftLeave down → assert.
#
# Env overrides (defaults sized for a first modest run; crank up later):
#   COUNT HOTSPOT TARGET_NODES CONCURRENCY DURATION_S WAIT_SPAWN_S WAIT_DOWN_S
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BENCH="$ROOT/scripts/bench"
TF_DIR="$ROOT/deploy/terraform"
REGION="${AWS_REGION:-eu-west-3}"
OUT="$BENCH/out/lifecycle-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$OUT"

TARGET_NODES="${TARGET_NODES:-3}"
SPAWN_TARGET=$((TARGET_NODES - 1))
COUNT="${COUNT:-8000}"
HOTSPOT="${HOTSPOT:-2000}"
# duration=0 writes as fast as concurrency allows → insert window fills sooner → faster scale-up.
DURATION_S="${DURATION_S:-0}"
CONCURRENCY="${CONCURRENCY:-48}"
COLLECTION="${COLLECTION:-LifeCycle$(date +%s | tail -c 6)}"
# Locations from the geo/hotspot SDK are full-width (16). Collection must be too
# or checkKey rejects the merge and every write comes back 503.
COLLECTION="$(python3 -c "c='$COLLECTION'; print((c+'XXXXXXXXXXXXXXXX')[:16])")"
READ_CONCURRENCY="${READ_CONCURRENCY:-12}"
READ_INTERVAL_S="${READ_INTERVAL_S:-10}"
WAIT_SPAWN_S="${WAIT_SPAWN_S:-300}"
WAIT_DOWN_S="${WAIT_DOWN_S:-600}"
SETTLE_S="${SETTLE_S:-20}"
HOT_LAT="${HOT_LAT:-48.8566}"
HOT_LNG="${HOT_LNG:-2.3522}"
HOT_SPREAD="${HOT_SPREAD:-0.05}"
PROJECT="${PROJECT:-indexus-aws}"

CSV="$BENCH/data/world_density_100k.csv"
if [[ ! -f "$CSV" ]]; then
  SRC="${DENSITY_CSV_SRC:-$HOME/Development/indexus/simulation/data/basic/items/items-100000-sigmoid.csv}"
  mkdir -p "$BENCH/data"
  cp "$SRC" "$CSV"
fi

cd "$TF_DIR"
ISSUER=$(terraform output -raw issuer_url)
BOOT_IP=$(terraform output -raw bootstrap_public_ip)
BOOTSTRAP="${BOOT_IP}|21000"
NODES="http://${BOOT_IP}:21000"
export ISSUER BOOT_IP BOOTSTRAP NODES

EXPECTED=$((COUNT + HOTSPOT))

echo "==> out=$OUT"
echo "==> ISSUER=$ISSUER target_nodes=$TARGET_NODES count=$COUNT hotspot=$HOTSPOT expected=$EXPECTED"

# Cleanup leftover spawned
ZOMBIES=$(aws ec2 describe-instances --region "$REGION" \
  --filters "Name=tag:Name,Values=${PROJECT}-spawned" "Name=instance-state-name,Values=running" \
  --query 'Reservations[].Instances[].InstanceId' --output text)
if [[ -n "${ZOMBIES// /}" && "$ZOMBIES" != "None" ]]; then
  echo "==> terminating leftover spawned: $ZOMBIES"
  for id in $ZOMBIES; do
    ip=$(aws ec2 describe-instances --region "$REGION" --instance-ids "$id" \
      --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
    curl -sf --max-time 120 "http://${ip}:19000/leave?timeout_s=90" >"$OUT/preleave_$id.json" || true
  done
  aws ec2 terminate-instances --region "$REGION" --instance-ids $ZOMBIES >/dev/null || true
  sleep 25
fi

snap() {
  local name="$1"
  curl -sf --max-time 5 "http://${BOOT_IP}:19000/status" >"$OUT/${name}.status.json" || echo '{}' >"$OUT/${name}.status.json"
  curl -sf --max-time 5 "http://${BOOT_IP}:19000/autoscale" >"$OUT/${name}.autoscale.json" || echo '{}' >"$OUT/${name}.autoscale.json"
  curl -sf --max-time 5 "http://${BOOT_IP}:19000/count" >"$OUT/${name}.count.json" || echo '{}' >"$OUT/${name}.count.json"
  curl -sf --max-time 5 "http://${BOOT_IP}:19000/queue" >"$OUT/${name}.queue.json" || echo '{}' >"$OUT/${name}.queue.json"
  curl -sf --max-time 5 "http://${BOOT_IP}:19000/registered" >"$OUT/${name}.registered.json" || true
  curl -sf --max-time 5 "http://${BOOT_IP}:19000/ownership" >"$OUT/${name}.ownership.json" || true
  aws ec2 describe-instances --region "$REGION" \
    --filters "Name=tag:Name,Values=${PROJECT}-spawned" "Name=instance-state-name,Values=pending,running" \
    --query 'Reservations[].Instances[].{Id:InstanceId,Ip:PublicIpAddress,State:State.Name}' \
    --output json >"$OUT/${name}.spawned.json" || echo '[]' >"$OUT/${name}.spawned.json"
}

mesh_nodes() {
  python3 - <<'PY'
import json, os, urllib.request
boot = os.environ["BOOT_IP"]
nodes = []
candidates = [boot] + [i.get("Ip") for i in json.load(open(os.environ["SPAWNED_JSON"])) if i.get("Ip")]
for ip in candidates:
    try:
        code = urllib.request.urlopen(f"http://{ip}:19000/count", timeout=3).status
        if code == 200:
            nodes.append(f"http://{ip}:21000")
    except Exception:
        pass
if not nodes:
    nodes = [f"http://{boot}:21000"]
print(",".join(nodes))
PY
}

sum_monitor_counts() {
  python3 - <<'PY'
import json, os, urllib.request
boot = os.environ["BOOT_IP"]
spawned = json.load(open(os.environ["SPAWNED_JSON"]))
ips = [boot] + [i["Ip"] for i in spawned if i.get("Ip")]
total = 0
per = {}
for ip in ips:
    try:
        c = json.load(urllib.request.urlopen(f"http://{ip}:19000/count", timeout=10)).get("count", 0)
    except Exception as e:
        c = f"err:{e}"
    per[ip] = c
    if isinstance(c, int):
        total += c
print(json.dumps({"sum": total, "per_host": per}))
PY
}

count_items() {
  local label="$1"
  local nodes="$2"
  (cd "$BENCH" && ISSUER="$ISSUER" BOOTSTRAP="$BOOTSTRAP" NODES="$nodes" \
    node count_collection.js --collection "$COLLECTION" --expected "$EXPECTED") \
    | tee "$OUT/count_${label}.json" || true
  export SPAWNED_JSON="$OUT/${label}.spawned.json"
  [[ -f "$SPAWNED_JSON" ]] || export SPAWNED_JSON="$OUT/post_hotspot.spawned.json"
  BOOT_IP="$BOOT_IP" SPAWNED_JSON="$SPAWNED_JSON" sum_monitor_counts \
    | tee "$OUT/monitor_sum_${label}.json" || true
}

METRICS="$OUT/metrics.csv"
echo "ts,phase,spawned,inserts_window,queue,items,peers,zones" >"$METRICS"
log_metrics() {
  local phase="$1"
  python3 - <<PY
import json, time
from pathlib import Path
out = Path("$OUT")
phase = "$phase"
spawned = len(json.load(open(out / f"{phase}.spawned.json")))
st = json.load(open(out / f"{phase}.status.json"))
auto = json.load(open(out / f"{phase}.autoscale.json"))
row = [
    str(int(time.time())),
    phase,
    str(spawned),
    str(auto.get("inserts_window", "")),
    str(st.get("queue", "")),
    str(st.get("items", "")),
    str(st.get("peers", "")),
    str(st.get("zones", "")),
]
open("$METRICS", "a").write(",".join(row) + "\n")
PY
}

# --- A baseline ---
snap baseline
log_metrics baseline

# --- B+C volume + parallel reads ---
echo "==> density write (${COUNT} over ${DURATION_S}s) + parallel reads"
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
  # Keep reads (and a light write trickle) alive through the upscale wait so
  # freshly spawned nodes see load before SoftLeave grace/hold can drain them.
  end=$((SECONDS + WAIT_SPAWN_S + 90))
  if [[ "$DURATION_S" != "0" ]]; then
    end=$((SECONDS + DURATION_S + 90))
  fi
  n=0
  while (( SECONDS < end )); do
    export BOOT_IP SPAWNED_JSON="$OUT/poll_live.spawned.json"
    snap poll_live >/dev/null 2>&1 || true
    MESH=$(BOOT_IP="$BOOT_IP" SPAWNED_JSON="$OUT/poll_live.spawned.json" mesh_nodes)
    ISSUER="$ISSUER" NODES="$MESH" \
      node read_storm_http.js --collection "$COLLECTION" --clients "$READ_CONCURRENCY" --requests 24 \
      >"$OUT/read_during_${n}.json" 2>"$OUT/read_during_${n}.err" || true
    # Trickle writes use the live mesh; hotspot_load routes each insert to the
    # XOR-nearest peer for the item key (same as production clients).
    # Skip when bootstrap ingress is already under pressure.
    if [[ ! -f "$OUT/upscale_done.flag" ]]; then
      Q=$(curl -sf --max-time 2 "http://${BOOT_IP}:19000/queue" 2>/dev/null \
        | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d.get("pending", d.get("queue",99999)))' 2>/dev/null || echo 99999)
      if [[ "${Q:-99999}" -lt 2000 ]]; then
        ISSUER="$ISSUER" NODES="$MESH" \
          node hotspot_load.js --count "${TRICKLE_COUNT:-2000}" --concurrency 16 \
            --collection "$COLLECTION" --lat "$HOT_LAT" --lng "$HOT_LNG" --spread "$HOT_SPREAD" \
            >"$OUT/trickle_${n}.json" 2>/dev/null || true
      else
        echo "{\"skip_trickle\":true,\"queue\":$Q}" >"$OUT/trickle_${n}.json"
      fi
    fi
    n=$((n + 1))
    sleep "$READ_INTERVAL_S"
  done
) &
READ_PID=$!

# --- D upscale wait ---
echo "==> waiting up to ${WAIT_SPAWN_S}s for ${SPAWN_TARGET} spawned..."
REACHED=0
UPSCALE_START=$SECONDS
for i in $(seq 1 $((WAIT_SPAWN_S / 10))); do
  snap "poll_up_$i"
  log_metrics "poll_up_$i"
  N=$(python3 -c "import json; print(len(json.load(open('$OUT/poll_up_$i.spawned.json'))))" 2>/dev/null || echo 0)
  AS=$(python3 -c "import json; d=json.load(open('$OUT/poll_up_$i.autoscale.json')); print(d.get('inserts_window'), d.get('scale_ups_done'), d.get('up_in_flight'))" 2>/dev/null || true)
  echo "  t=$((i*10))s spawned=$N autoscale=$AS"
  if [[ "$N" -ge "$SPAWN_TARGET" ]]; then
    REACHED=1
    echo "==> mesh target reached"
    touch "$OUT/upscale_done.flag"
    break
  fi
  sleep 10
done
UPSCALE_S=$((SECONDS - UPSCALE_START))
touch "$OUT/upscale_done.flag"

wait "$WRITE_PID" || true
kill "$READ_PID" 2>/dev/null || true
wait "$READ_PID" 2>/dev/null || true

# Trickle writes inflate expected — account for accepted trickle+hotspot later.
TRICKLE_OK=$(OUT="$OUT" python3 - <<'PY'
import json,glob,os
ok=0
for p in glob.glob(os.environ["OUT"]+"/trickle_*.json"):
    try:
        text=open(p).read()
        objs=[]; buf=""; depth=0
        for ch in text:
            if ch=='{': depth+=1
            if depth: buf+=ch
            if ch=='}':
                depth-=1
                if depth==0:
                    try: objs.append(json.loads(buf))
                    except: pass
                    buf=""
        if objs: ok+=int(objs[-1].get("ok",0))
    except: pass
print(ok)
PY
)
echo "==> trickle writes accepted: $TRICKLE_OK"

snap post_volume
log_metrics post_volume
export SPAWNED_JSON="$OUT/post_volume.spawned.json"
MESH=$(BOOT_IP="$BOOT_IP" SPAWNED_JSON="$OUT/post_volume.spawned.json" mesh_nodes)
echo "==> mesh after volume: $MESH"
sleep 20

# --- E hotspot ---
echo "==> hotspot write (${HOTSPOT} near ${HOT_LAT},${HOT_LNG})"
(
  cd "$BENCH"
  ISSUER="$ISSUER" NODES="$MESH" \
    node hotspot_load.js --count "$HOTSPOT" --concurrency "$CONCURRENCY" \
      --collection "$COLLECTION" --lat "$HOT_LAT" --lng "$HOT_LNG" --spread "$HOT_SPREAD" \
      | tee "$OUT/hotspot_write.json"
) || echo "WARN: hotspot finished with errors (see hotspot_write.json)"
snap post_hotspot
log_metrics post_hotspot
cp "$OUT/post_hotspot.ownership.json" "$OUT/hotspot.ownership.json" 2>/dev/null || true

# Adjust expected if hotspot under-delivered
HOT_OK=$(python3 - <<PY
import json
text=open("$OUT/hotspot_write.json").read()
objs=[]
buf=""
depth=0
for ch in text:
    if ch=='{': depth+=1
    if depth: buf+=ch
    if ch=='}':
        depth-=1
        if depth==0:
            try: objs.append(json.loads(buf))
            except: pass
            buf=""
print(objs[-1].get("ok",0) if objs else 0)
PY
)
EXPECTED=$((COUNT + HOT_OK + TRICKLE_OK))
echo "==> hotspot ok=$HOT_OK trickle=$TRICKLE_OK → expected=$EXPECTED"

# --- F settle ---
echo "==> settle ${SETTLE_S}s then count (expect $EXPECTED)"
sleep "$SETTLE_S"
export SPAWNED_JSON="$OUT/post_hotspot.spawned.json"
MESH=$(BOOT_IP="$BOOT_IP" SPAWNED_JSON="$OUT/post_hotspot.spawned.json" mesh_nodes)
count_items peak "$MESH"
cp "$OUT/post_hotspot.spawned.json" "$OUT/peak.spawned.json"

# --- G downscale ---
echo "==> quiet / SoftLeave wait up to ${WAIT_DOWN_S}s..."
DOWNSCALE_START=$SECONDS
for i in $(seq 1 $((WAIT_DOWN_S / 15))); do
  snap "poll_down_$i"
  log_metrics "poll_down_$i"
  N=$(python3 -c "import json; print(len(json.load(open('$OUT/poll_down_$i.spawned.json'))))" 2>/dev/null || echo 0)
  echo "  t=$((i*15))s spawned=$N"
  if [[ "$N" -eq 0 ]]; then
    echo "==> fully downscaled"
    break
  fi
  sleep 15
done
DOWNSCALE_S=$((SECONDS - DOWNSCALE_START))

# --- H consist ---
snap final
log_metrics final
echo "==> final count on bootstrap (expect $EXPECTED)"
count_items final "http://${BOOT_IP}:21000"

PEAK_SPAWNED=$(python3 -c "import json,glob; mx=0
import os
for p in glob.glob('$OUT/poll_up_*.spawned.json')+['$OUT/post_volume.spawned.json','$OUT/post_hotspot.spawned.json']:
  try: mx=max(mx,len(json.load(open(p))))
  except: pass
print(mx)")

python3 - <<PY | tee "$OUT/SUMMARY.json"
import json, glob, os
out = "$OUT"
expected = int("$EXPECTED")

def load_count(label):
    path = f"{out}/count_{label}.json"
    try:
        raw = open(path).read().strip()
        if not raw:
            return None
        try:
            return json.loads(raw)
        except json.JSONDecodeError:
            # NDJSON / mixed logs: take last JSON object start
            for line in reversed(raw.splitlines()):
                line = line.strip()
                if line.startswith("{"):
                    try:
                        return json.loads(line)
                    except json.JSONDecodeError:
                        continue
            return {"error": "no json object found"}
    except Exception as e:
        return {"error": str(e)}

def monitor_sum(label):
    try:
        return json.load(open(f"{out}/monitor_sum_{label}.json"))
    except Exception:
        return None

def http_got(d):
    if not d or "error" in d:
        return None
    ht = d.get("http_tree") or {}
    if isinstance(ht.get("unique_ids"), int):
        return ht["unique_ids"]
    for k in ("http_unique_ids", "unique", "got", "count"):
        if k in d and isinstance(d[k], int):
            return d[k]
    return None

peak = load_count("peak")
final = load_count("final")
peak_got = http_got(peak)
final_got = http_got(final)
mon_final = monitor_sum("final")
mon_got = None
if isinstance(mon_final, dict) and isinstance(mon_final.get("sum"), int):
    mon_got = mon_final["sum"]
# Prefer HTTP walk; fall back to monitor sum so pretty-printed count files
# or parse glitches don't fake a data-loss failure. During SoftLeave the HTTP
# walk can under-count briefly while monitor sum stays exact.
loss_got = final_got if final_got is not None else mon_got
spawned_final = len(json.load(open(f"{out}/final.spawned.json")))
peak_spawned = int("$PEAK_SPAWNED")
reached = bool(int("$REACHED"))

pass_loss = (final_got == expected) or (mon_got == expected)
pass_elastic_up = peak_spawned >= int("$SPAWN_TARGET")
pass_elastic_down = spawned_final == 0
ok = pass_loss and pass_elastic_down and (reached or peak_spawned > 0)

summary = {
    "pass": ok,
    "collection": "$COLLECTION",
    "expected": expected,
    "count_volume": int("$COUNT"),
    "count_hotspot": int("$HOTSPOT"),
    "target_nodes": int("$TARGET_NODES"),
    "reached_target": reached,
    "peak_spawned": peak_spawned,
    "spawned_at_end": spawned_final,
    "upscale_s": int("$UPSCALE_S"),
    "downscale_s": int("$DOWNSCALE_S"),
    "count_peak": peak,
    "count_final": final,
    "loss_got": loss_got,
    "monitor_peak": monitor_sum("peak"),
    "monitor_final": mon_final,
    "asserts": {
        "no_loss": pass_loss,
        "upscale": pass_elastic_up,
        "downscale": pass_elastic_down,
    },
    "autoscale_final": json.load(open(f"{out}/final.autoscale.json")),
    "status_final": json.load(open(f"{out}/final.status.json")),
}
print(json.dumps(summary, indent=2))
raise SystemExit(0 if ok else 1)
PY

echo "==> done → $OUT"
