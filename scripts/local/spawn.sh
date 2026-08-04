#!/usr/bin/env bash
# Start one local spawned node. Prints the instance id (local-<n>) on stdout.
# Invoked by the issuer when -localDir / LaunchTemplate=local is set.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=env.sh
source "$SCRIPT_DIR/env.sh"

BOOTSTRAP_HOST="${INDEXUS_BOOTSTRAP_IPS%%,*}"
BOOTSTRAP_HOST="${BOOTSTRAP_HOST:-127.0.0.1}"
BOOTSTRAP="${BOOTSTRAP_HOST}|${BOOT_P2P}"

# Allocate the next free slot.
SLOT=0
while [[ -f "$SPAWNED_DIR/local-$SLOT.json" ]]; do
  SLOT=$((SLOT + 1))
done
ID="local-$SLOT"
P2P=$((21010 + SLOT))
MON=$((19010 + SLOT))
DATA="$RUN_DIR/nodes/$ID"
mkdir -p "$DATA"

if [[ ! -x "$BIN_DIR/node" ]]; then
  echo "missing $BIN_DIR/node — run scripts/local/mesh_up.sh first" >&2
  exit 1
fi
if [[ ! -x "$BIN_DIR/randomname" ]]; then
  (cd "$ROOT" && go build -o "$BIN_DIR/randomname" ./scripts/randomname)
fi

if [[ -n "${INDEXUS_PREFER_NEAR:-}" ]]; then
  NAME=$("$BIN_DIR/randomname" --near "$INDEXUS_PREFER_NEAR")
else
  NAME=$("$BIN_DIR/randomname")
fi
LOG="$RUN_DIR/logs/$ID.log"

INDEXUS_INSTANCE_ID="$ID" \
INDEXUS_ROLE=spawned \
INDEXUS_LEAVE_DIR="$RUN_DIR/leave" \
INDEXUS_STORAGE="$DATA/backup" \
INDEXUS_PREFER_NEAR="${INDEXUS_PREFER_NEAR:-}" \
HOME="$DATA" \
nohup "$BIN_DIR/node" \
  -name "$NAME" \
  -p2pPort "$P2P" \
  -monitoringPort "$MON" \
  -advertise 127.0.0.1 \
  -bootstrap "$BOOTSTRAP" \
  -issuer "$ISSUER_URL" \
  -network "$NETWORK_ID" \
  -requireAuth \
  -delegation "$DELEGATION" \
  -autoscale \
  -autoscaleRole spawned \
  -queuePressure "${QUEUE_PRESSURE:-50}" \
  -pressureHold "${PRESSURE_HOLD:-3s}" \
  -scaleWindow "${SCALE_WINDOW:-20s}" \
  -scaleDownThreshold "${SCALE_DOWN_THRESHOLD:-50}" \
  -scaleDownHold "${SCALE_DOWN_HOLD:-10s}" \
  -scaleCooldown "${SCALE_COOLDOWN:-5s}" \
  -storage "$DATA/backup" \
  -archive "$DATA/archive" \
  -nodeKey "$KEYS_DIR/${ID}.ed25519" \
  -cert "$KEYS_DIR/${ID}.cert.json" \
  >"$LOG" 2>&1 &
PID=$!

python3 - <<PY
import json, pathlib
path = pathlib.Path("$SPAWNED_DIR") / "$ID.json"
path.write_text(json.dumps({
  "id": "$ID",
  "pid": $PID,
  "name": "$NAME",
  "p2p": $P2P,
  "mon": $MON,
  "role": "spawned",
  "log": "$LOG",
}, indent=2))
PY

# Wait until monitoring answers (or die early).
for _ in $(seq 1 40); do
  if ! kill -0 "$PID" 2>/dev/null; then
    echo "spawned process died; see $LOG" >&2
    tail -n 40 "$LOG" >&2 || true
    rm -f "$SPAWNED_DIR/$ID.json"
    exit 1
  fi
  if curl -sf --max-time 1 "http://127.0.0.1:${MON}/count" >/dev/null; then
    break
  fi
  sleep 0.25
done

echo "$ID"
