#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

mkdir -p .keys .data-e2e/n1 .data-e2e/n2 .data-e2e/n3 bin

echo "==> building binaries"
go build -o bin/issuer ./app/issuer
go build -o bin/node ./app/node
go build -o bin/randomname ./scripts/randomname

pkill -f "$ROOT/bin/issuer" 2>/dev/null || true
pkill -f "$ROOT/bin/node" 2>/dev/null || true
sleep 0.5

echo "==> starting issuer"
./bin/issuer -addr :22000 -network indexus-aws -key .keys/issuer.ed25519 -bootstrap 127.0.0.1 &
ISSUER_PID=$!
NODE_PIDS=()
trap 'kill $ISSUER_PID ${NODE_PIDS[@]:-} 2>/dev/null || true' EXIT
sleep 0.6

curl -sf http://127.0.0.1:22000/health >/dev/null

TOKEN=$(curl -sf -X POST http://127.0.0.1:22000/v1/issue/token \
  -H 'Content-Type: application/json' \
  -d '{"client_id":"e2e","scopes":["read","write"]}' | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])')
echo "client token issued"

start_node() {
  local name=$1 port=$2 mon=$3 store=$4 bootstrap=$5
  local args=(
    -name "$name"
    -p2pPort "$port"
    -monitoringPort "$mon"
    -storage "$store/backup"
    -archive "$store/archive"
    -advertise 127.0.0.1
    -issuer http://127.0.0.1:22000
    -network indexus-aws
    -requireAuth
    -nodeKey ".keys/${name}.ed25519"
    -cert ".keys/${name}.cert.json"
  )
  if [[ -n "$bootstrap" ]]; then
    args+=(-bootstrap "$bootstrap")
  fi
  ./bin/node "${args[@]}" > ".data-e2e/${name}.log" 2>&1 &
  NODE_PIDS+=($!)
}

NAME1=$(./bin/randomname)
NAME2=$(./bin/randomname)
NAME3=$(./bin/randomname)

echo "==> bootstrap node $NAME1 :21001"
start_node "$NAME1" 21001 19001 .data-e2e/n1 ""
sleep 1.2

echo "==> joining node $NAME2 :21002"
start_node "$NAME2" 21002 19002 .data-e2e/n2 "127.0.0.1|21001"
sleep 1.2

echo "==> scale-out node $NAME3 :21003 (issuer-signed cert at boot)"
start_node "$NAME3" 21003 19003 .data-e2e/n3 "127.0.0.1|21001"
sleep 2

echo "==> negative: /set without token => 401"
CODE=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:21001/set?collection=c&location=@")
[[ "$CODE" == "401" ]] || { echo "got $CODE"; exit 1; }

echo "==> negative: ping without cert => 401"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:21001/ping" \
  -H 'Content-Type: application/json' \
  -d '{"origin":{"name":"x","ips":{"127.0.0.1":null},"port":21099}}')
[[ "$CODE" == "401" ]] || { echo "ping without cert got $CODE"; exit 1; }

echo "==> write item with token"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:21001/item" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"item":{"collection":"demo","location":"a","id":"x1","metrics":[1,2,3,4,5]},"root":"@","current":"a"}')
if [[ "$CODE" != "201" ]]; then
  echo "expected 201, got $CODE"
  tail -n 50 .data-e2e/*.log || true
  exit 1
fi

sleep 1

echo "==> read with token"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:21001/set?collection=demo&location=a")
[[ "$CODE" == "200" ]] || { echo "read got $CODE"; tail -n 40 .data-e2e/*.log; exit 1; }

echo "==> waiting for observe/refresh cycle"
sleep 12
echo "registered:"; curl -sf "http://127.0.0.1:19001/registered" | head -c 500; echo
echo "acknowledged:"; curl -sf "http://127.0.0.1:19001/acknowledged" | head -c 500; echo

echo "==> e2e OK (issuer + 3 authenticated nodes + client token)"
