#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BENCH="$ROOT/scripts/bench"
TF_DIR="$ROOT/deploy/terraform"
cd "$BENCH"

BOOT_IP=$(cd "$TF_DIR" && terraform output -raw bootstrap_public_ip)
WORKER_IP=$(cd "$TF_DIR" && terraform output -json worker_public_ips | python3 -c 'import sys,json; print(json.load(sys.stdin)[0])')
export ISSUER="http://${BOOT_IP}:22000"
export NODE="http://${BOOT_IP}:21000"
export BOOTSTRAP="${BOOT_IP}|21000"

echo "=== 0) npm install ==="
npm install --silent

echo "=== 1) auth negatives ==="
CODE=$(curl -s -o /dev/null -w '%{http_code}' "$NODE/set?collection=bench&location=z")
echo "no-token read => $CODE"
[[ "$CODE" == "401" ]]

echo "=== 2) write load (delegation bait: 80 items @ location z, threshold 40) ==="
node write_load.js --count 80 --concurrency 16 --location z --collection bench-deleg

echo "=== 3) wait feed + refresh ==="
sleep 8
echo "ownership bootstrap:"; curl -sf "http://${BOOT_IP}:19000/ownership" | python3 -m json.tool | head -40
echo "queue:"; curl -sf "http://${BOOT_IP}:19000/queue"; echo
echo "registered:"; curl -sf "http://${BOOT_IP}:19000/registered"; echo

echo "=== 4) more writes across locations (spread) ==="
node write_load.js --count 100 --concurrency 20 --location a --collection bench-spread
node write_load.js --count 100 --concurrency 20 --location b --collection bench-spread

echo "=== 5) SDK + HTTP reads (location @ holds leaf keys z:id) ==="
node read_http.js --collection bench-deleg --location @ || true
node read_sdk.js --collection bench-deleg --location @ || true
# also try each worker
for WIP in $(cd "$TF_DIR" && terraform output -json worker_public_ips | python3 -c 'import sys,json;print(" ".join(json.load(sys.stdin)))'); do
  NODE="http://${WIP}:21000" node read_http.js --collection bench-deleg --location @ || true
done
# spawned instances (best-effort)
aws ec2 describe-instances --region "${AWS_REGION:-eu-west-3}" \
  --filters Name=tag:Name,Values=indexus-aws-spawned Name=instance-state-name,Values=running \
  --query 'Reservations[].Instances[].PublicIpAddress' --output text 2>/dev/null | tr '\t' '\n' | while read -r SIP; do
  [[ -n "$SIP" ]] || continue
  NODE="http://${SIP}:21000" node read_http.js --collection bench-deleg --location @ || true
done

echo "=== 6) round-robin write to bootstrap + worker ==="
NODE="http://${WORKER_IP}:21000" node write_load.js --count 40 --concurrency 10 --location c --collection bench-rr
NODE="http://${BOOT_IP}:21000" node write_load.js --count 40 --concurrency 10 --location c --collection bench-rr

echo "=== 7) spawn new worker + timing ==="
chmod +x spawn_worker.sh
./spawn_worker.sh

echo "=== 8) final mesh snapshot ==="
sleep 5
echo "registered:"; curl -sf "http://${BOOT_IP}:19000/registered"; echo
echo "routing:"; curl -sf "http://${BOOT_IP}:19000/routing"; echo
echo "ownership:"; curl -sf "http://${BOOT_IP}:19000/ownership" | python3 -m json.tool | head -60

echo "=== SUITE DONE ==="
