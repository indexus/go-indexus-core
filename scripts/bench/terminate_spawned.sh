#!/usr/bin/env bash
# Soft-leave then terminate EC2 instances tagged Name=indexus-aws-spawned.
#
# Usage:
#   ./terminate_spawned.sh
#   ./terminate_spawned.sh --dry-run
#   INSTANCE_ID=i-xxx ./terminate_spawned.sh
#   SKIP_LEAVE=1 ./terminate_spawned.sh   # hard-kill only (loses data)
set -euo pipefail

REGION="${AWS_REGION:-eu-west-3}"
DRY_RUN=0
SKIP_LEAVE="${SKIP_LEAVE:-0}"
LEAVE_TIMEOUT_S="${LEAVE_TIMEOUT_S:-90}"
for a in "$@"; do
  [[ "$a" == "--dry-run" ]] && DRY_RUN=1
done

if [[ -n "${INSTANCE_ID:-}" ]]; then
  IDS_STR="$INSTANCE_ID"
else
  IDS_STR=$(aws ec2 describe-instances --region "$REGION" \
    --filters Name=tag:Name,Values=indexus-aws-spawned Name=instance-state-name,Values=pending,running \
    --query 'Reservations[].Instances[].InstanceId' --output text | tr '\t' ' ' | xargs || true)
fi

if [[ -z "${IDS_STR// }" ]]; then
  echo '{"terminated":[],"note":"no spawned instances found"}'
  exit 0
fi

# shellcheck disable=SC2086
echo "==> spawned candidates: $IDS_STR"
# shellcheck disable=SC2086
MAP=$(aws ec2 describe-instances --region "$REGION" --instance-ids $IDS_STR \
  --query 'Reservations[].Instances[].[InstanceId,PublicIpAddress,State.Name]' --output json)
echo "$MAP" | python3 -m json.tool

if [[ "$DRY_RUN" == "1" ]]; then
  python3 -c "import json; print(json.dumps({'dry_run': True, 'instance_ids': '''${IDS_STR}'''.split()}, indent=2))"
  exit 0
fi

if [[ "$SKIP_LEAVE" != "1" ]]; then
  while read -r IID IP STATE; do
    [[ "$STATE" == "running" && -n "$IP" && "$IP" != "None" ]] || continue
    echo "==> soft-leave $IID @ $IP (timeout ${LEAVE_TIMEOUT_S}s)"
    set +e
    LEAVE_JSON=$(curl -sf --max-time $((LEAVE_TIMEOUT_S + 30)) -X POST \
      "http://${IP}:19000/leave?timeout_s=${LEAVE_TIMEOUT_S}")
    RC=$?
    set -e
    if [[ $RC -ne 0 || -z "$LEAVE_JSON" ]]; then
      echo "{\"instance_id\":\"$IID\",\"ip\":\"$IP\",\"leave\":\"failed\",\"hint\":\"continuing terminate\"}"
    else
      echo "$LEAVE_JSON" | python3 -m json.tool 2>/dev/null || echo "$LEAVE_JSON"
    fi
  done < <(echo "$MAP" | python3 -c 'import sys,json
for iid, ip, st in json.load(sys.stdin):
    print(iid, ip or "None", st)')
fi

# shellcheck disable=SC2086
aws ec2 terminate-instances --region "$REGION" --instance-ids $IDS_STR \
  --query 'TerminatingInstances[].{Id:InstanceId,Prev:PreviousState.Name,Curr:CurrentState.Name}' --output json \
  > /tmp/indexus-terminate.json
python3 -c 'import json; print(json.dumps({"terminated": json.load(open("/tmp/indexus-terminate.json"))}, indent=2))'
