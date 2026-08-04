#!/usr/bin/env bash
# Local SoftLeave battle: write N → wait for K spawned → quiet → SoftLeave to 0 → assert count.
# Same shape as scripts/bench/downscale_battle.sh, without AWS/terraform.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=env.sh
source "$SCRIPT_DIR/env.sh"
BENCH="$ROOT/scripts/bench"
OUT="$BENCH/out/local-battle-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$OUT"

SPAWN_TARGET="${SPAWN_TARGET:-2}"
COUNT="${COUNT:-1200}"
# duration=0 → write as fast as concurrency allows (spawns sooner).
DURATION_S="${DURATION_S:-0}"
CONCURRENCY="${CONCURRENCY:-48}"
COLLECTION="${COLLECTION:-LocalBattle$(date +%s | tail -c 6)}"
# Locations from the geo SDK are full-width (16). Collection must match.
COLLECTION="$(python3 -c "c='$COLLECTION'; print((c+'XXXXXXXXXXXXXXXX')[:16])")"
WAIT_SPAWN_S="${WAIT_SPAWN_S:-90}"
WAIT_DOWN_S="${WAIT_DOWN_S:-120}"

# Propagate aggressive autoscale knobs into mesh_up / spawn.sh unless caller overrides.
export SPAWN_MAX="${SPAWN_MAX:-4}"
export SCALE_UP_THRESHOLD="${SCALE_UP_THRESHOLD:-400}"
export SCALE_DOWN_THRESHOLD="${SCALE_DOWN_THRESHOLD:-50}"
export SCALE_DOWN_HOLD="${SCALE_DOWN_HOLD:-10s}"
export SCALE_WINDOW="${SCALE_WINDOW:-20s}"
export SCALE_COOLDOWN="${SCALE_COOLDOWN:-5s}"

CSV="$BENCH/data/world_density_100k.csv"
if [[ ! -f "$CSV" ]]; then
  echo "missing density CSV at $CSV" >&2
  exit 1
fi

export ISSUER="$ISSUER_URL"
export BOOT_IP=127.0.0.1
export BOOTSTRAP="127.0.0.1|${BOOT_P2P}"
export NODES="http://127.0.0.1:${BOOT_P2P}"
export INDEXUS_LOCAL_DIR="$SCRIPT_DIR"

echo "==> ensuring local mesh"
"$SCRIPT_DIR/mesh_up.sh"

# Clear leftover spawned from a previous interrupted battle.
while read -r id; do
  [[ -n "$id" ]] && "$SCRIPT_DIR/terminate.sh" "$id" || true
done < <("$SCRIPT_DIR/list_ids.sh")

snap() {
  local name="$1"
  curl -sf "http://127.0.0.1:${BOOT_MON}/autoscale" >"$OUT/${name}.autoscale.json" || echo '{}' >"$OUT/${name}.autoscale.json"
  curl -sf "http://127.0.0.1:${BOOT_MON}/count" >"$OUT/${name}.count.json" || echo '{}' >"$OUT/${name}.count.json"
  curl -sf "http://127.0.0.1:${BOOT_MON}/registered" >"$OUT/${name}.registered.json" || true
  "$SCRIPT_DIR/list_spawned.sh" >"$OUT/${name}.spawned.json"
}

mesh_nodes() {
  python3 - <<PY
import json, os
nodes=["http://127.0.0.1:${BOOT_P2P}"]
for i in json.load(open("$OUT/post_write.spawned.json")):
    p2p=i.get("p2p")
    if p2p: nodes.append(f"http://127.0.0.1:{p2p}")
print(",".join(nodes))
PY
}

sum_counts() {
  python3 - <<PY
import json, urllib.request
total=0
parts=[]
# bootstrap
try:
  c=json.load(urllib.request.urlopen("http://127.0.0.1:${BOOT_MON}/count", timeout=3))["count"]
  parts.append(("bootstrap", c)); total+=c
except Exception as e:
  parts.append(("bootstrap", str(e)))
for f in __import__("pathlib").Path("$SPAWNED_DIR").glob("local-*.json"):
  meta=json.load(open(f))
  mon=meta["mon"]; iid=meta["id"]
  try:
    c=json.load(urllib.request.urlopen(f"http://127.0.0.1:{mon}/count", timeout=3))["count"]
    parts.append((iid, c)); total+=c
  except Exception as e:
    parts.append((iid, str(e)))
print(json.dumps({"total": total, "parts": parts, "expected": int("$COUNT")}))
PY
}

echo "==> out=$OUT count=$COUNT spawn_target=$SPAWN_TARGET"
snap baseline

echo "==> density write"
(cd "$BENCH" && DENSITY_CSV="$CSV" ISSUER="$ISSUER" NODES="$NODES" \
  node geo_load_density.js --count "$COUNT" --duration "$DURATION_S" \
    --concurrency "$CONCURRENCY" --collection "$COLLECTION" | tee "$OUT/write.json") &
WPID=$!

echo "==> wait for $SPAWN_TARGET spawned"
for i in $(seq 1 $((WAIT_SPAWN_S/5))); do
  snap "up_$i"
  N=$(python3 -c "import json;print(len(json.load(open('$OUT/up_$i.spawned.json'))))")
  AS=$(python3 -c "import json;d=json.load(open('$OUT/up_$i.autoscale.json'));print(d.get('inserts_window'),d.get('scale_ups_done'))")
  echo "  t=$((i*5))s spawned=$N autoscale=$AS"
  if [[ "$N" -ge "$SPAWN_TARGET" ]]; then
    echo "==> spawn target reached"
    break
  fi
  sleep 5
done

wait "$WPID" || true
snap post_write
sleep 5
MESH=$(mesh_nodes)
echo "==> mesh=$MESH"
echo "==> counts after up"
sum_counts | tee "$OUT/count_after_up.json"

echo "==> waiting downscale to 0 spawned"
for i in $(seq 1 $((WAIT_DOWN_S/5))); do
  snap "down_$i"
  N=$(python3 -c "import json;print(len(json.load(open('$OUT/down_$i.spawned.json'))))")
  echo "  t=$((i*5))s spawned=$N"
  if [[ "$N" -eq 0 ]]; then
    echo "==> fully downscaled"
    break
  fi
  sleep 5
done

sleep 3
snap final
echo "==> FINAL counts"
sum_counts | tee "$OUT/count_final.json"

python3 - <<PY | tee "$OUT/SUMMARY.json"
import json
fin=json.load(open("$OUT/count_final.json"))
# mesh_up leaves a smoke item behind, so the target is the baseline plus writes.
try:
  base=json.load(open("$OUT/baseline.count.json")).get("count", 0)
except Exception:
  base=0
exp=int("$COUNT")+base
total=fin.get("total", -1)
summary={
  "expected": exp,
  "final_total": total,
  "final": fin,
  "pass": total == exp,
  "loss": exp - total if isinstance(total, int) else None,
}
print(json.dumps(summary, indent=2))
raise SystemExit(0 if summary["pass"] else 2)
PY

echo "==> done $OUT"
