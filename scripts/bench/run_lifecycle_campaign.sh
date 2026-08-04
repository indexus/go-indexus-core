#!/usr/bin/env bash
# Fast lab campaign: one mesh, sequential profiles, stop on first failure,
# always terminate leftover spawned instances.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
REGION="${AWS_REGION:-eu-west-3}"

cleanup_spawned() {
  echo "==> cleanup: terminate leftover spawned"
  SKIP_LEAVE=1 "$ROOT/scripts/bench/terminate_spawned.sh" || true
  IDS=$(aws ec2 describe-instances --region "$REGION" \
    --filters "Name=tag:Role,Values=spawned" "Name=instance-state-name,Values=pending,running,stopping,stopped" \
    --query 'Reservations[].Instances[].InstanceId' --output text | tr '\t' ' ' || true)
  if [[ -n "${IDS// }" && "$IDS" != "None" ]]; then
    # shellcheck disable=SC2086
    aws ec2 terminate-instances --region "$REGION" --instance-ids $IDS >/dev/null || true
  fi
}
trap cleanup_spawned EXIT

run_one() {
  local name="$1"; shift
  echo; echo "############################################################"
  echo "## PROFILE $name"; echo "############################################################"
  if ! ( cd "$ROOT"; "$@" ); then
    echo "FAIL: profile $name"
    exit 1
  fi
  echo "OK: profile $name"
  cleanup_spawned
  # Brief pause so EC2 inventory settles before next upscale.
  sleep 15
}

# A — hard-ish but lab-fast: 5 nodes, abrupt quiet
run_one A env \
  TARGET_NODES=5 COUNT=12000 HOTSPOT=3000 CONCURRENCY=32 DURATION_S=0 \
  TRICKLE_COUNT=2000 SETTLE_S=20 WAIT_SPAWN_S=180 WAIT_DOWN_S=600 \
  "$ROOT/scripts/bench/lifecycle_battle.sh"

# B — medium mesh, same abrupt quiet (decay profile approximated by longer hold wait)
run_one B env \
  TARGET_NODES=6 COUNT=15000 HOTSPOT=4000 CONCURRENCY=32 DURATION_S=0 \
  TRICKLE_COUNT=2000 SETTLE_S=20 WAIT_SPAWN_S=200 WAIT_DOWN_S=700 \
  "$ROOT/scripts/bench/lifecycle_battle.sh"

# C — chaos reads during upscale/downscale window
run_one C env \
  TARGET_NODES=5 COUNT=12000 HOTSPOT=3000 CONCURRENCY=32 DURATION_S=0 \
  TRICKLE_COUNT=2000 READ_CONCURRENCY=24 READ_INTERVAL_S=5 \
  SETTLE_S=20 WAIT_SPAWN_S=180 WAIT_DOWN_S=600 \
  "$ROOT/scripts/bench/lifecycle_battle.sh"

echo; echo "ALL PROFILES OK"
