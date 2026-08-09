#!/usr/bin/env bash
# Bring up a local mesh: issuer (local scale) + bootstrap with autoscale.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# Dashboard / remesh passes KEEP_SNAPSHOTS in the environment. Capture it
# before sourcing env.sh + mesh-config.env so a stale config cannot keep
# dirty object-store snapshots on Clean & Restart.
_KEEP_SNAPSHOTS_REQUESTED="${KEEP_SNAPSHOTS-}"

# shellcheck source=env.sh
source "$SCRIPT_DIR/env.sh"

if [[ -n "${_KEEP_SNAPSHOTS_REQUESTED}" ]]; then
  KEEP_SNAPSHOTS="${_KEEP_SNAPSHOTS_REQUESTED}"
  export KEEP_SNAPSHOTS
fi

chmod +x "$SCRIPT_DIR"/*.sh

echo "==> build binaries"
(cd "$ROOT" && go build -o "$BIN_DIR/issuer" ./app/issuer)
(cd "$ROOT" && go build -o "$BIN_DIR/node" ./app/node)

"$SCRIPT_DIR/mesh_down.sh" >/dev/null 2>&1 || true
rm -rf "$SPAWNED_DIR" "$RUN_DIR/nodes" "$RUN_DIR/logs"
# Drop prior spawned identities so PreferNear names are not overridden by
# sticky certs left from an earlier mesh (empty nodes that never own zones).
rm -f "$KEYS_DIR"/local-*.ed25519 "$KEYS_DIR"/local-*.cert.json
# Keep SNAPSHOT_DIR across restarts only if KEEP_SNAPSHOTS=1; else wipe for a clean mesh.
# Never `rm -rf` / `mkdir` an empty SNAPSHOT_DIR (INDEXUS_IN_MEMORY clears it).
if [[ "${KEEP_SNAPSHOTS:-0}" != "1" ]]; then
  wipe_root="${SNAPSHOT_DIR:-$RUN_DIR/snapshots}"
  echo "==> wiping snapshots (KEEP_SNAPSHOTS=${KEEP_SNAPSHOTS:-0})"
  echo "    $wipe_root"
  [[ -n "$wipe_root" ]] && rm -rf "$wipe_root"
  # Also clear leave-handoff staging if present.
  rm -rf "$RUN_DIR/leave"
  mkdir -p "$RUN_DIR/leave"
else
  echo "==> keeping snapshots: ${SNAPSHOT_DIR:-$RUN_DIR/snapshots}"
fi
mkdir -p "$SPAWNED_DIR" "$RUN_DIR/nodes" "$RUN_DIR/logs" "$KEYS_DIR"
if [[ -n "${SNAPSHOT_DIR:-}" ]]; then
  mkdir -p "$SNAPSHOT_DIR"
fi

ensure_p2p_tls_args
if p2p_tls_on; then
  echo "==> P2P TLS/HTTP/2  sslStorage=$TLS_DIR"
else
  echo "==> P2P plain HTTP (set INDEXUS_P2P_TLS=1 for HTTPS/HTTP/2)"
fi

# Cap on concurrent spawned processes (not including bootstrap). Lab default
# 15 — room past items_limit without starving hot nodes at SPAWN_MAX.
SPAWN_MAX="${SPAWN_MAX:-15}"
# Capacity-first defaults. Low QUEUE_PRESSURE over-spawns on Darwin where mem%
# is blind — scale-down guard only, not a scale-up trigger.
# Values may already be set by mesh-config.env (sourced from env.sh).
QUEUE_PRESSURE="${QUEUE_PRESSURE:-50000}"
PRESSURE_HOLD="${PRESSURE_HOLD:-30s}"
SCALE_DOWN_THRESHOLD="${SCALE_DOWN_THRESHOLD:-100}"
SCALE_DOWN_HOLD="${SCALE_DOWN_HOLD:-15m}"
SCALE_WINDOW="${SCALE_WINDOW:-60s}"
SCALE_COOLDOWN="${SCALE_COOLDOWN:-15s}"
INDEXUS_SPAWN_GRACE="${INDEXUS_SPAWN_GRACE:-10m}"
INDEXUS_ITEMS_LIMIT="${INDEXUS_ITEMS_LIMIT:-100000}"
export QUEUE_PRESSURE PRESSURE_HOLD SCALE_DOWN_THRESHOLD SCALE_DOWN_HOLD \
  SCALE_WINDOW SCALE_COOLDOWN INDEXUS_SPAWN_GRACE DELEGATION SPAWN_MAX \
  SNAPSHOT_DIR INDEXUS_SNAPSHOT_DIR INDEXUS_DELEGATION \
  INDEXUS_TRANSFER_TIMEOUT INDEXUS_ITEMS_LIMIT

echo "==> issuer (local spawn) :22000"
echo "    DELEGATION=$DELEGATION QUEUE_PRESSURE=$QUEUE_PRESSURE SPAWN_MAX=$SPAWN_MAX ITEMS_LIMIT=$INDEXUS_ITEMS_LIMIT"
# detach.sh: leave the caller’s process group so IDE/shell cleanup cannot
# SIGTERM the whole mesh when the launching terminal exits.
INDEXUS_LOCAL_DIR="$SCRIPT_DIR" \
nohup "$SCRIPT_DIR/detach.sh" -- "$BIN_DIR/issuer" \
  -addr :22000 \
  -network "$NETWORK_ID" \
  -key "$KEYS_DIR/issuer.ed25519" \
  -bootstrap 127.0.0.1 \
  -scale \
  -launchTemplate local \
  -localDir "$SCRIPT_DIR" \
  -spawnMax "$SPAWN_MAX" \
  -scaleCooldown "$SCALE_COOLDOWN" \
  >"$RUN_DIR/logs/issuer.log" 2>&1 &
echo $! >"$RUN_DIR/issuer.pid"

for _ in $(seq 1 40); do
  curl -sf "$ISSUER_URL/health" >/dev/null && break
  sleep 0.25
done
curl -sf "$ISSUER_URL/health" >/dev/null

BOOT_DATA="$RUN_DIR/nodes/bootstrap"
mkdir -p "$BOOT_DATA"

BOOT_STORAGE_ARGS=()
if in_memory_on; then
  BOOT_STORAGE_ARGS=(-storage "" -archive "")
  export INDEXUS_STORAGE=
  export SNAPSHOT_DIR=
  export INDEXUS_SNAPSHOT_DIR=
  echo "==> bootstrap :${BOOT_P2P} (name from app/node)"
  echo "    in-memory lab: no WAL dir, no object store (INDEXUS_IN_MEMORY=1)"
else
  BOOT_STORAGE_ARGS=(-storage "$BOOT_DATA/backup" -archive "$BOOT_DATA/archive")
  export INDEXUS_STORAGE="$BOOT_DATA/backup"
  export SNAPSHOT_DIR
  export INDEXUS_SNAPSHOT_DIR="$SNAPSHOT_DIR"
  echo "==> bootstrap :${BOOT_P2P} (name from app/node)"
  echo "    snapshots: $SNAPSHOT_DIR"
fi
INDEXUS_ROLE=bootstrap \
INDEXUS_LEAVE_DIR="$RUN_DIR/leave" \
INDEXUS_DELEGATION="$INDEXUS_DELEGATION" \
INDEXUS_TRANSFER_TIMEOUT="$INDEXUS_TRANSFER_TIMEOUT" \
INDEXUS_ITEMS_LIMIT="$INDEXUS_ITEMS_LIMIT" \
INDEXUS_IN_MEMORY="${INDEXUS_IN_MEMORY:-0}" \
HOME="$BOOT_DATA" \
nohup "$SCRIPT_DIR/detach.sh" -- "$BIN_DIR/node" \
  -p2pPort "$BOOT_P2P" \
  -monitoringPort "$BOOT_MON" \
  -advertise 127.0.0.1 \
  -issuer "$ISSUER_URL" \
  -network "$NETWORK_ID" \
  -requireAuth \
  -delegation "$DELEGATION" \
  -autoscale \
  -autoscaleRole bootstrap \
  -queuePressure "$QUEUE_PRESSURE" \
  -pressureHold "$PRESSURE_HOLD" \
  -scaleWindow "$SCALE_WINDOW" \
  -scaleDownThreshold "$SCALE_DOWN_THRESHOLD" \
  -scaleDownHold "$SCALE_DOWN_HOLD" \
  -scaleCooldown "$SCALE_COOLDOWN" \
  "${BOOT_STORAGE_ARGS[@]}" \
  ${TLS_ARGS[@]+"${TLS_ARGS[@]}"} \
  -nodeKey "$KEYS_DIR/bootstrap.ed25519" \
  -cert "$KEYS_DIR/bootstrap.cert.json" \
  >"$RUN_DIR/logs/bootstrap.log" 2>&1 &
echo $! >"$RUN_DIR/bootstrap.pid"

for _ in $(seq 1 40); do
  curl -sf ${CURL_TLS[@]+"${CURL_TLS[@]}"} "${P2P_SCHEME}://127.0.0.1:${BOOT_MON}/count" >/dev/null && break
  sleep 0.25
done

TOKEN=$(curl -sf -X POST "$ISSUER_URL/v1/issue/token" \
  -H 'Content-Type: application/json' \
  -d '{"client_id":"local-mesh","scopes":["read","write"]}' \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])')
[[ -n "$TOKEN" ]]

# Opt-in connectivity check. Default off so Ops/Data start empty (no phantom local-smoke).
if [[ "${SMOKE:-0}" == "1" ]]; then
  CODE=$(curl -s ${CURL_TLS[@]+"${CURL_TLS[@]}"} -o /dev/null -w '%{http_code}' -X POST "${P2P_SCHEME}://127.0.0.1:${BOOT_P2P}/item" \
    -H "Authorization: Bearer $TOKEN" \
    -H 'Content-Type: application/json' \
    -d '{"item":{"collection":"local-smoke","location":"a","id":"1","metrics":[1,2,3,4,5]},"root":"@","current":"a"}')
  echo "smoke write => $CODE (expect 201)"
  [[ "$CODE" == "201" ]]
else
  echo "smoke write: skipped (set SMOKE=1 to insert local-smoke/id=1)"
fi

echo "issuer:    $ISSUER_URL"
echo "bootstrap: ${P2P_SCHEME}://127.0.0.1:${BOOT_P2P}  mon :${BOOT_MON}"
if in_memory_on; then
  echo "mode:      in-memory (INDEXUS_IN_MEMORY=1, no snapshots / WAL dir)"
else
  echo "snapshots: $SNAPSHOT_DIR"
fi
echo "autoscale: $(curl -sf ${CURL_TLS[@]+"${CURL_TLS[@]}"} "${P2P_SCHEME}://127.0.0.1:${BOOT_MON}/autoscale")"
store=$(curl -sf ${CURL_TLS[@]+"${CURL_TLS[@]}"} "${P2P_SCHEME}://127.0.0.1:${BOOT_MON}/status" | python3 -c 'import sys,json; print(json.load(sys.stdin).get("snapshot",{}).get("store"))' 2>/dev/null || echo "?")
if in_memory_on; then
  echo "object_store attached: $store (expect False)"
else
  echo "object_store attached: $store (expect True)"
fi
[[ "$store" == "True" || "$store" == "true" ]] || true
