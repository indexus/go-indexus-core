#!/usr/bin/env bash
# Ensure a lab TLS cert for local P2P (enables Go http.Server HTTP/2).
# Shared by bootstrap + spawned nodes via -sslStorage.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=env.sh
source "$SCRIPT_DIR/env.sh"

TLS_DIR="${INDEXUS_TLS_DIR:-$RUN_DIR/tls}"
mkdir -p "$TLS_DIR"
CRT="$TLS_DIR/server.crt"
KEY="$TLS_DIR/server.key"

if [[ -f "$CRT" && -f "$KEY" ]]; then
  echo "$TLS_DIR"
  exit 0
fi

if ! command -v openssl >/dev/null 2>&1; then
  echo "openssl required to mint $CRT" >&2
  exit 1
fi

openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout "$KEY" \
  -out "$CRT" \
  -days 825 \
  -subj "/CN=127.0.0.1" \
  -addext "subjectAltName=IP:127.0.0.1,DNS:localhost" \
  >/dev/null 2>&1

chmod 600 "$KEY"
echo "$TLS_DIR"
