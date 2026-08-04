#!/usr/bin/env bash
# Stop remote load generators (pkill storm on indexus-aws-load hosts).
set -euo pipefail

REGION="${AWS_REGION:-${REGION:-eu-west-3}}"
PROJECT="${PROJECT:-indexus-aws}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/id_ed25519}"
SSH_OPTS=(-o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=8 -i "$SSH_KEY")

IPS=$(aws ec2 describe-instances --region "$REGION" \
  --filters "Name=tag:Name,Values=${PROJECT}-load,indexus-aws-load" \
    "Name=instance-state-name,Values=running" \
  --query 'Reservations[].Instances[].PublicIpAddress' --output text 2>/dev/null || true)

killed=0
for ip in $IPS; do
  [[ -z "$ip" || "$ip" == "None" ]] && continue
  if ssh "${SSH_OPTS[@]}" ec2-user@"$ip" 'sudo pkill -f "multi_client_storm|geo_load_density" || true' 2>/dev/null; then
    echo "stopped $ip"
    killed=$((killed + 1))
  else
    echo "warn: could not ssh $ip" >&2
  fi
done
echo "stopped_hosts=$killed"
