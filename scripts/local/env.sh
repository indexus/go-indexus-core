#!/usr/bin/env bash
# Shared paths for the local mesh (issuer + bootstrap + spawned processes).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
LOCAL_DIR="${INDEXUS_LOCAL_DIR:-$ROOT/scripts/local}"
RUN_DIR="${INDEXUS_RUN_DIR:-$ROOT/.data-local}"
SPAWNED_DIR="$RUN_DIR/spawned"
KEYS_DIR="$RUN_DIR/keys"
BIN_DIR="$ROOT/bin"

ISSUER_URL="${ISSUER_URL:-http://127.0.0.1:22000}"
BOOT_P2P="${BOOT_P2P:-21000}"
BOOT_MON="${BOOT_MON:-19000}"
NETWORK_ID="${NETWORK_ID:-indexus-aws}"
DELEGATION="${DELEGATION:-40}"

mkdir -p "$SPAWNED_DIR" "$KEYS_DIR" "$BIN_DIR" "$RUN_DIR/leave" "$RUN_DIR/logs"
