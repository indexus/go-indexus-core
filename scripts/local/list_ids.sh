#!/usr/bin/env bash
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=env.sh
source "$SCRIPT_DIR/env.sh"
shopt -s nullglob
for f in "$SPAWNED_DIR"/local-*.json; do
  python3 -c "import json; print(json.load(open('$f'))['id'])"
done
