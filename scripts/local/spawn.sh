#!/usr/bin/env bash
# Start one local spawned node. Prints the instance id (local-<n>) on stdout.
# Invoked by the issuer when -localDir / LaunchTemplate=local is set.
# Node name comes from app/node (RandomName / INDEXUS_PREFER_NEAR) — do not pass -name.
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
P2P=$((BOOT_P2P + SPAWN_PORT_OFFSET + SLOT))
MON=$((BOOT_MON + SPAWN_PORT_OFFSET + SLOT))
DATA="$RUN_DIR/nodes/$ID"
mkdir -p "$DATA"

if [[ ! -x "$BIN_DIR/node" ]]; then
  echo "missing $BIN_DIR/node — run scripts/local/mesh_up.sh first" >&2
  exit 1
fi

# Always drop sticky identity for this slot. app/node stickyNodeName prefers
# an existing .cert.json over -name / PreferNear — leftover local-N keys from a
# prior mesh leave the peer with 0 zones and client_ready=false forever.
rm -f "$KEYS_DIR/${ID}.ed25519" "$KEYS_DIR/${ID}.cert.json"

LOG="$RUN_DIR/logs/$ID.log"

STORAGE_ARGS=()
if in_memory_on; then
  STORAGE_ARGS=(-storage "" -archive "")
  export INDEXUS_STORAGE=
  export SNAPSHOT_DIR=
  export INDEXUS_SNAPSHOT_DIR=
else
  STORAGE_ARGS=(-storage "$DATA/backup" -archive "$DATA/archive")
  export INDEXUS_STORAGE="$DATA/backup"
  export SNAPSHOT_DIR
  export INDEXUS_SNAPSHOT_DIR="$SNAPSHOT_DIR"
fi

ensure_p2p_tls_args

INDEXUS_INSTANCE_ID="$ID" \
INDEXUS_ROLE=spawned \
INDEXUS_LEAVE_DIR="$RUN_DIR/leave" \
INDEXUS_PREFER_NEAR="${INDEXUS_PREFER_NEAR:-}" \
INDEXUS_DELEGATION="${INDEXUS_DELEGATION:-$DELEGATION}" \
INDEXUS_TRANSFER_TIMEOUT="${INDEXUS_TRANSFER_TIMEOUT:-5m}" \
INDEXUS_ITEMS_LIMIT="${INDEXUS_ITEMS_LIMIT:-100000}" \
INDEXUS_IN_MEMORY="${INDEXUS_IN_MEMORY:-0}" \
HOME="$DATA" \
nohup "$SCRIPT_DIR/detach.sh" -- "$BIN_DIR/node" \
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
  -queuePressure "${QUEUE_PRESSURE:-50000}" \
  -pressureHold "${PRESSURE_HOLD:-30s}" \
  -scaleWindow "${SCALE_WINDOW:-60s}" \
  -scaleDownThreshold "${SCALE_DOWN_THRESHOLD:-100}" \
  -scaleDownHold "${SCALE_DOWN_HOLD:-15m}" \
  -scaleCooldown "${SCALE_COOLDOWN:-15s}" \
  "${STORAGE_ARGS[@]}" \
  ${TLS_ARGS[@]+"${TLS_ARGS[@]}"} \
  -nodeKey "$KEYS_DIR/${ID}.ed25519" \
  -cert "$KEYS_DIR/${ID}.cert.json" \
  >"$LOG" 2>&1 &
PID=$!

# Wait until monitoring answers (or die early).
NAME=""
for _ in $(seq 1 40); do
  if ! kill -0 "$PID" 2>/dev/null; then
    echo "spawned process died; see $LOG" >&2
    tail -n 40 "$LOG" >&2 || true
    rm -f "$SPAWNED_DIR/$ID.json"
    exit 1
  fi
  if curl -sf ${CURL_TLS[@]+"${CURL_TLS[@]}"} --max-time 1 "${P2P_SCHEME}://127.0.0.1:${MON}/count" >/dev/null; then
    NAME=$(curl -sf ${CURL_TLS[@]+"${CURL_TLS[@]}"} --max-time 1 "${P2P_SCHEME}://127.0.0.1:${MON}/status" \
      | python3 -c 'import sys,json; print(json.load(sys.stdin).get("name",""))' 2>/dev/null || true)
    break
  fi
  sleep 0.25
done

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

echo "$ID"
