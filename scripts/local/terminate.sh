#!/usr/bin/env bash
# Terminate a local spawned node.
# Usage: LEAVE=1 terminate.sh local-N
#
# LEAVE defaults to 0 because the issuer only calls this after the node reported
# a successful drain. Draining again here kept the issuer's /v1/downscale reply
# pending for the length of a second SoftLeave, and the caller holds the
# cluster-wide drain lock until that reply lands. Set LEAVE=1 for manual or
# teardown use, where nothing has drained the node yet.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=env.sh
source "$SCRIPT_DIR/env.sh"

ID="${1:-}"
if [[ -z "$ID" || "$ID" != local-* ]]; then
  echo "usage: terminate.sh local-N" >&2
  exit 2
fi

META="$SPAWNED_DIR/$ID.json"
if [[ ! -f "$META" ]]; then
  echo "unknown instance $ID" >&2
  exit 1
fi

MON=$(python3 -c "import json; print(json.load(open('$META'))['mon'])")
PID=$(python3 -c "import json; print(json.load(open('$META'))['pid'])")

if [[ "${LEAVE:-0}" == "1" ]] \
  && curl -sf --max-time 2 "http://127.0.0.1:${MON}/count" >/dev/null 2>&1; then
  curl -sf --max-time 120 "http://127.0.0.1:${MON}/leave?timeout_s=90" \
    >"$RUN_DIR/logs/${ID}.leave.json" || true
fi

if kill -0 "$PID" 2>/dev/null; then
  kill "$PID" 2>/dev/null || true
  for _ in $(seq 1 20); do
    kill -0 "$PID" 2>/dev/null || break
    sleep 0.2
  done
  kill -9 "$PID" 2>/dev/null || true
fi

rm -f "$META"
echo "terminated $ID"
