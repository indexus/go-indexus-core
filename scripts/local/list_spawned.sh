#!/usr/bin/env bash
# List live local spawned metadata as JSON array.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=env.sh
source "$SCRIPT_DIR/env.sh"

python3 - <<PY
import json, pathlib, os
out=[]
for f in sorted(pathlib.Path("$SPAWNED_DIR").glob("local-*.json")):
    meta=json.load(open(f))
    pid=meta.get("pid", 0)
    try:
        os.kill(pid, 0)
        alive=True
    except OSError:
        alive=False
        f.unlink(missing_ok=True)
        continue
    if alive:
        out.append(meta)
print(json.dumps(out, indent=2))
PY
