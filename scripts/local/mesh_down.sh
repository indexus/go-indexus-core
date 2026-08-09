#!/usr/bin/env bash
# Tear down the local mesh (spawned + bootstrap + issuer) and free lab ports.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=env.sh
source "$SCRIPT_DIR/env.sh"

shopt -s nullglob
for f in "$SPAWNED_DIR"/local-*.json; do
  id=$(python3 -c "import json; print(json.load(open('$f'))['id'])")
  "$SCRIPT_DIR/terminate.sh" "$id" >/dev/null 2>&1 || true
done

if [[ -f "$RUN_DIR/bootstrap.pid" ]]; then
  kill "$(cat "$RUN_DIR/bootstrap.pid")" 2>/dev/null || true
  rm -f "$RUN_DIR/bootstrap.pid"
fi
if [[ -f "$RUN_DIR/issuer.pid" ]]; then
  kill "$(cat "$RUN_DIR/issuer.pid")" 2>/dev/null || true
  rm -f "$RUN_DIR/issuer.pid"
fi

# Belt-and-braces: anything still listening on our port ranges (incomplete
# cleanup previously left :19010 in use and the next spawn died on bind).
# LISTEN-only: plain `lsof -ti tcp:PORT` also matches clients with ESTABLISHED
# connections (e.g. dashboard server.js polling :19000 / :22000) and would
# SIGTERM the dashboard mid-remesh; concurrently -k then kills vite too.
kill_port() {
  local port="$1"
  if command -v lsof >/dev/null 2>&1; then
    local pids
    pids=$(lsof -nP -iTCP:"$port" -sTCP:LISTEN -t 2>/dev/null || true)
    if [[ -n "$pids" ]]; then
      # shellcheck disable=SC2086
      kill $pids 2>/dev/null || true
      sleep 0.1
      pids=$(lsof -nP -iTCP:"$port" -sTCP:LISTEN -t 2>/dev/null || true)
      # shellcheck disable=SC2086
      [[ -n "$pids" ]] && kill -9 $pids 2>/dev/null || true
    fi
  fi
}

for port in 22000; do
  kill_port "$port"
done
# The sweep has to reach the last slot spawn.sh can allocate. A fixed +20 stops
# covering the mesh as soon as SPAWN_MAX passes 10, and the slots it misses are
# exactly the ones whose stale listener kills the next spawn on bind.
LAST_SPAWN_PORT=$((SPAWN_PORT_OFFSET + ${SPAWN_MAX:-15}))
for ((port=BOOT_MON; port<=BOOT_MON+LAST_SPAWN_PORT; port++)); do
  kill_port "$port"
done
for ((port=BOOT_P2P; port<=BOOT_P2P+LAST_SPAWN_PORT; port++)); do
  kill_port "$port"
done

rm -f "$SPAWNED_DIR"/local-*.json 2>/dev/null || true

echo "local mesh down"
