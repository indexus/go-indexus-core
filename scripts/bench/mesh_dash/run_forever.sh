#!/usr/bin/env bash
# Keep mesh_dash server.js alive: restart on crash, log to out/mesh_dash.log
set -u
ROOT="$(cd "$(dirname "$0")" && pwd)"
OUT="$(cd "$ROOT/../out" && pwd)"
mkdir -p "$OUT"
LOG="${MESH_DASH_LOG:-$OUT/mesh_dash.log}"
PID_FILE="${MESH_DASH_PID:-$OUT/mesh_dash.pid}"
NODE_BIN="${NODE_BIN:-$(command -v node)}"
cd "$ROOT"

export BOOT_IP="${BOOT_IP:-51.44.10.158}"
export LOAD_MODE="${LOAD_MODE:-remote}"
export LOAD_HOSTS="${LOAD_HOSTS:-2}"
export REGION="${REGION:-eu-west-3}"
export AWS_REGION="${AWS_REGION:-$REGION}"
export PROJECT="${PROJECT:-indexus-aws}"
export PORT="${PORT:-3847}"
export ARTIFACTS_BUCKET="${ARTIFACTS_BUCKET:-indexus-aws-artifacts-20260803151542905500000002}"

# Single-instance guard: if something already healthy on PORT, exit quietly
if curl -sf --max-time 2 "http://127.0.0.1:${PORT}/api/health" >/dev/null 2>&1; then
  echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) mesh_dash already healthy on :${PORT}" >>"$LOG"
  exit 0
fi

# Reclaim stale listener if any (only our server.js)
if lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
  echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) reclaiming :${PORT}" >>"$LOG"
  lsof -nP -iTCP:"$PORT" -sTCP:LISTEN -t 2>/dev/null | while read -r p; do
    kill "$p" 2>/dev/null || true
  done
  sleep 1
fi

echo $$ >"$PID_FILE"
echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) run_forever start BOOT_IP=$BOOT_IP LOAD_MODE=$LOAD_MODE LOAD_HOSTS=$LOAD_HOSTS" >>"$LOG"

while true; do
  echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) starting server.js" >>"$LOG"
  "$NODE_BIN" server.js >>"$LOG" 2>&1
  code=$?
  echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) server.js exited code=$code; restart in 2s" >>"$LOG"
  sleep 2
done
