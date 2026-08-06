#!/usr/bin/env bash
# Shared paths for the local mesh (issuer + bootstrap + spawned processes).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
LOCAL_DIR="${INDEXUS_LOCAL_DIR:-$ROOT/scripts/local}"
RUN_DIR="${INDEXUS_RUN_DIR:-$ROOT/.data-local}"
SPAWNED_DIR="$RUN_DIR/spawned"
KEYS_DIR="$RUN_DIR/keys"
BIN_DIR="$ROOT/bin"

# Shared object-store root for zone snapshots (S3-equivalent on disk).
# All local nodes read/write the same tree so SoftLeave / delegation can
# move large zones via snapshot files instead of item-by-item Transfer.
# Path: .data-local/snapshots/  (override with SNAPSHOT_DIR / INDEXUS_SNAPSHOT_DIR)
SNAPSHOT_DIR="${SNAPSHOT_DIR:-${INDEXUS_SNAPSHOT_DIR:-$RUN_DIR/snapshots}}"
export SNAPSHOT_DIR
export INDEXUS_SNAPSHOT_DIR="$SNAPSHOT_DIR"
# Reuse the S3 delegation protocol against the shared DirStore.
export INDEXUS_DELEGATION_S3="${INDEXUS_DELEGATION_S3:-1}"
# Classic /transfer HTTP timeout (fallback when a zone is below the snap
# threshold or the object store is unavailable). Default in binary is 5m.
export INDEXUS_TRANSFER_TIMEOUT="${INDEXUS_TRANSFER_TIMEOUT:-5m}"
# Zones at/above this item count use snapshot delegation (default 200).
export INDEXUS_TRANSFER_THRESHOLD="${INDEXUS_TRANSFER_THRESHOLD:-200}"

ISSUER_URL="${ISSUER_URL:-http://127.0.0.1:22000}"
BOOT_P2P="${BOOT_P2P:-21000}"
BOOT_MON="${BOOT_MON:-19000}"
NETWORK_ID="${NETWORK_ID:-indexus-aws}"
# Soft capacity target (~items/zone before Own split). Labs may raise.
# Also exported as INDEXUS_DELEGATION so the binary default / NewSettings override
# stay aligned when the -delegation flag is omitted.
DELEGATION="${DELEGATION:-5000}"
export INDEXUS_DELEGATION="${INDEXUS_DELEGATION:-$DELEGATION}"
# Snapshot-delegation session timeout (mirroring / catch-up).
export INDEXUS_DELEGATION_TIMEOUT="${INDEXUS_DELEGATION_TIMEOUT:-2m}"

# Optional dashboard / remesh overrides (KEY=value). Applied after defaults above.
MESH_CONFIG_FILE="${MESH_CONFIG:-$RUN_DIR/mesh-config.env}"
if [[ -f "$MESH_CONFIG_FILE" ]]; then
  set -a
  # shellcheck disable=SC1090
  source "$MESH_CONFIG_FILE"
  set +a
  # Keep INDEXUS_DELEGATION aligned with DELEGATION after overrides.
  export INDEXUS_DELEGATION="${INDEXUS_DELEGATION:-$DELEGATION}"
  DELEGATION="${DELEGATION:-$INDEXUS_DELEGATION}"
fi

mkdir -p "$SPAWNED_DIR" "$KEYS_DIR" "$BIN_DIR" "$RUN_DIR/leave" "$RUN_DIR/logs" "$SNAPSHOT_DIR"
