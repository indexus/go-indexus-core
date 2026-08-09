#!/usr/bin/env bash
# Count live local spawned nodes (files whose pid is still running).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=env.sh
source "$SCRIPT_DIR/env.sh"

n=0
shopt -s nullglob
for f in "$SPAWNED_DIR"/local-*.json; do
  pid=$(python3 -c "import json; print(json.load(open('$f'))['pid'])" 2>/dev/null || echo 0)
  if [[ "$pid" -gt 0 ]] && kill -0 "$pid" 2>/dev/null; then
    n=$((n + 1))
  else
    rm -f "$f"
  fi
done
echo "$n"
