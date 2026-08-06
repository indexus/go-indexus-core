#!/usr/bin/env bash
# Run a command in a new session (macOS-safe substitute for Linux setsid).
# Usage: detach.sh [--] command [args...]
set -euo pipefail
if [[ "${1:-}" == "--" ]]; then shift; fi
if [[ $# -lt 1 ]]; then
  echo "usage: detach.sh command [args...]" >&2
  exit 2
fi
exec python3 -c '
import os, sys
os.setsid()
os.execvp(sys.argv[1], sys.argv[1:])
' "$@"
