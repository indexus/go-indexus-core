#!/usr/bin/env bash
# Shared paths for the local mesh (issuer + bootstrap + spawned processes).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
LOCAL_DIR="${INDEXUS_LOCAL_DIR:-$ROOT/scripts/local}"
RUN_DIR="${INDEXUS_RUN_DIR:-$ROOT/.data-local}"
SPAWNED_DIR="$RUN_DIR/spawned"
KEYS_DIR="$RUN_DIR/keys"
BIN_DIR="$ROOT/bin"

# Shared object-store root for durable zone snapshots (S3-equivalent on disk).
# Path: .data-local/snapshots/  (override with SNAPSHOT_DIR / INDEXUS_SNAPSHOT_DIR)
SNAPSHOT_DIR="${SNAPSHOT_DIR:-${INDEXUS_SNAPSHOT_DIR:-$RUN_DIR/snapshots}}"
export SNAPSHOT_DIR
export INDEXUS_SNAPSHOT_DIR="$SNAPSHOT_DIR"
# Repeatable /transfer HTTP timeout. Default in binary is 5m.
export INDEXUS_TRANSFER_TIMEOUT="${INDEXUS_TRANSFER_TIMEOUT:-5m}"

ISSUER_URL="${ISSUER_URL:-http://127.0.0.1:22000}"
BOOT_P2P="${BOOT_P2P:-21000}"
BOOT_MON="${BOOT_MON:-19000}"
# Spawned node local-N binds BOOT_P2P+SPAWN_PORT_OFFSET+N (and the same on the
# monitoring base). spawn.sh allocates from it and mesh_down.sh sweeps up to it,
# so the two cannot drift apart as SPAWN_MAX changes.
SPAWN_PORT_OFFSET="${SPAWN_PORT_OFFSET:-10}"
NETWORK_ID="${NETWORK_ID:-indexus-aws}"
# Soft capacity target (~items/zone before Own split). Labs may raise.
# Also exported as INDEXUS_DELEGATION so the binary default / NewSettings override
# stay aligned when the -delegation flag is omitted.
DELEGATION="${DELEGATION:-5000}"
export INDEXUS_DELEGATION="${INDEXUS_DELEGATION:-$DELEGATION}"
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

# Lab-only: keep every item in process memory. Empty -storage (NewMemory),
# no DirStore / SNAPSHOT_DIR. Disk checkpoints stay a
# durability copy in the normal path — they do not page items out of RAM —
# but reclaim / handoff via empty snaps can still skew what Count lists.
# INDEXUS_IN_MEMORY=1 wins over SNAPSHOT_DIR.
in_memory_on() {
  case "${INDEXUS_IN_MEMORY:-0}" in
    1|true|TRUE|yes|YES|on|ON) return 0 ;;
    *) return 1 ;;
  esac
}
if in_memory_on; then
  export INDEXUS_IN_MEMORY=1
  unset SNAPSHOT_DIR INDEXUS_SNAPSHOT_DIR
  export SNAPSHOT_DIR=
  export INDEXUS_SNAPSHOT_DIR=
fi

mkdir -p "$SPAWNED_DIR" "$KEYS_DIR" "$BIN_DIR" "$RUN_DIR/leave" "$RUN_DIR/logs"
if [[ -n "${SNAPSHOT_DIR:-}" ]]; then
  mkdir -p "$SNAPSHOT_DIR"
fi

# Lab TLS for P2P+monitoring (Go ServeTLS → HTTP/2). Off by default so an
# already-running http mesh keeps answering; set INDEXUS_P2P_TLS=1 to enable.
p2p_tls_on() {
  case "${INDEXUS_P2P_TLS:-0}" in
    1|true|TRUE|yes|YES|on|ON) return 0 ;;
    *) return 1 ;;
  esac
}

# Populate TLS_ARGS=(-sslStorage DIR) when TLS is on; empty otherwise.
# Echoes nothing; sets TLS_DIR / TLS_ARGS / P2P_SCHEME in the caller.
ensure_p2p_tls_args() {
  # Always define arrays so `set -u` callers can expand "${TLS_ARGS[@]}" safely.
  TLS_ARGS=()
  CURL_TLS=()
  TLS_DIR="${INDEXUS_TLS_DIR:-$RUN_DIR/tls}"
  P2P_SCHEME=http
  if p2p_tls_on; then
    TLS_DIR="$("$LOCAL_DIR/ensure_tls.sh")"
    TLS_ARGS=(-sslStorage "$TLS_DIR")
    P2P_SCHEME=https
    CURL_TLS=(-k)
    export INDEXUS_TLS_DIR="$TLS_DIR"
  fi
  export P2P_SCHEME
  # bash 3.2 + set -u: empty array expand still errors unless set +u around use,
  # so export a single optional flag string for scripts that prefer it.
  export INDEXUS_SSL_STORAGE_ARGS="${TLS_ARGS[*]-}"
}
