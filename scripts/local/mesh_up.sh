#!/usr/bin/env bash
# Bring up a local mesh: issuer (local scale) + bootstrap with autoscale.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=env.sh
source "$SCRIPT_DIR/env.sh"

chmod +x "$SCRIPT_DIR"/*.sh

echo "==> build binaries"
(cd "$ROOT" && go build -o "$BIN_DIR/issuer" ./app/issuer)
(cd "$ROOT" && go build -o "$BIN_DIR/node" ./app/node)
(cd "$ROOT" && go build -o "$BIN_DIR/randomname" ./scripts/randomname)

"$SCRIPT_DIR/mesh_down.sh" >/dev/null 2>&1 || true
rm -rf "$SPAWNED_DIR" "$RUN_DIR/nodes" "$RUN_DIR/logs"
mkdir -p "$SPAWNED_DIR" "$RUN_DIR/nodes" "$RUN_DIR/logs" "$KEYS_DIR"

SPAWN_MAX="${SPAWN_MAX:-4}"
# Fast local defaults: a small backlog held briefly counts as pressure, and a
# node SoftLeaves soon after it goes quiet.
QUEUE_PRESSURE="${QUEUE_PRESSURE:-50}"
PRESSURE_HOLD="${PRESSURE_HOLD:-3s}"
SCALE_DOWN_THRESHOLD="${SCALE_DOWN_THRESHOLD:-50}"
SCALE_DOWN_HOLD="${SCALE_DOWN_HOLD:-10s}"
SCALE_WINDOW="${SCALE_WINDOW:-20s}"
SCALE_COOLDOWN="${SCALE_COOLDOWN:-5s}"

echo "==> issuer (local spawn) :22000"
INDEXUS_LOCAL_DIR="$SCRIPT_DIR" \
nohup "$BIN_DIR/issuer" \
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

BOOT_NAME=$("$BIN_DIR/randomname")
BOOT_DATA="$RUN_DIR/nodes/bootstrap"
mkdir -p "$BOOT_DATA"

echo "==> bootstrap $BOOT_NAME :${BOOT_P2P}"
INDEXUS_ROLE=bootstrap \
INDEXUS_LEAVE_DIR="$RUN_DIR/leave" \
INDEXUS_STORAGE="$BOOT_DATA/backup" \
HOME="$BOOT_DATA" \
nohup "$BIN_DIR/node" \
  -name "$BOOT_NAME" \
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
  -storage "$BOOT_DATA/backup" \
  -archive "$BOOT_DATA/archive" \
  -nodeKey "$KEYS_DIR/bootstrap.ed25519" \
  -cert "$KEYS_DIR/bootstrap.cert.json" \
  >"$RUN_DIR/logs/bootstrap.log" 2>&1 &
echo $! >"$RUN_DIR/bootstrap.pid"

for _ in $(seq 1 40); do
  curl -sf "http://127.0.0.1:${BOOT_MON}/count" >/dev/null && break
  sleep 0.25
done

TOKEN=$(curl -sf -X POST "$ISSUER_URL/v1/issue/token" \
  -H 'Content-Type: application/json' \
  -d '{"client_id":"local-mesh","scopes":["read","write"]}' \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])')

CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:${BOOT_P2P}/item" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"item":{"collection":"local-smoke","location":"a","id":"1","metrics":[1,2,3,4,5]},"root":"@","current":"a"}')

echo "smoke write => $CODE (expect 201)"
echo "issuer:    $ISSUER_URL"
echo "bootstrap: http://127.0.0.1:${BOOT_P2P}  mon :${BOOT_MON}"
echo "autoscale: $(curl -sf http://127.0.0.1:${BOOT_MON}/autoscale)"
[[ "$CODE" == "201" ]]
