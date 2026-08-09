#!/usr/bin/env bash
# Measure cold spawn: RunInstances → :19000/health on a spawned node.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
TF_DIR="$ROOT/scripts/deploy/terraform"
REGION="${AWS_REGION:-eu-west-3}"
PROJECT="${PROJECT:-indexus-aws}"

cd "$TF_DIR"
LT=$(terraform output -raw launch_template_id)
AMI=$(aws ec2 describe-launch-template-versions --region "$REGION" \
  --launch-template-id "$LT" --versions '$Latest' \
  --query 'LaunchTemplateVersions[0].LaunchTemplateData.ImageId' --output text)

echo "==> launch template $LT image=$AMI"
T0=$(date +%s)

# '$Latest' must stay literal for the AWS CLI (set -u would trip on $Latest).
IID=$(aws ec2 run-instances --region "$REGION" \
  --launch-template "LaunchTemplateId=${LT},Version=\$Latest" \
  --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=${PROJECT}-spawned},{Key=Project,Value=${PROJECT}},{Key=Role,Value=spawned},{Key=PreferNear,Value=aaaaaaaaaaaaaaaaaaaaaaaaaaaa}]" \
  --query 'Instances[0].InstanceId' --output text)
echo "==> spawned $IID at t=0"

cleanup() {
  aws ec2 terminate-instances --region "$REGION" --instance-ids "$IID" >/dev/null 2>&1 || true
}
trap cleanup EXIT

aws ec2 wait instance-running --region "$REGION" --instance-ids "$IID"
T_RUN=$(date +%s)
echo "==> instance-running +$((T_RUN - T0))s"

IP=""
for _ in $(seq 1 60); do
  IP=$(aws ec2 describe-instances --region "$REGION" --instance-ids "$IID" \
    --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
  [[ -n "$IP" && "$IP" != "None" ]] && break
  sleep 1
done
echo "==> public ip $IP +$(($(date +%s) - T0))s"

ready=0
for _ in $(seq 1 120); do
  if curl -sf --max-time 2 "http://${IP}:19000/health" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 1
done
T_HEALTH=$(date +%s)
if [[ "$ready" != "1" ]]; then
  echo "FAIL: health not up after +$((T_HEALTH - T0))s" >&2
  ssh -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10 \
    -i "$HOME/.ssh/id_ed25519" "ec2-user@$IP" \
    'sudo systemctl status indexus-node --no-pager -l | head -40; sudo journalctl -u indexus-node -n 50 --no-pager; ls -la /opt/indexus /etc/indexus 2>/dev/null; cat /etc/indexus/node.env 2>/dev/null | head -25' 2>&1 | tail -100 || true
  exit 1
fi

echo "==> health OK +$((T_HEALTH - T0))s (running→health $((T_HEALTH - T_RUN))s)"
curl -sf --max-time 3 "http://${IP}:19000/status" | python3 -c \
  'import sys,json; d=json.load(sys.stdin); print("name",d.get("name"),"items",d.get("items"))' || true

OUT="$ROOT/scripts/deploy/out/spawn_boot_$(date +%Y%m%d-%H%M%S).json"
mkdir -p "$(dirname "$OUT")"
python3 - "$OUT" "$AMI" "$IID" "$IP" "$((T_RUN - T0))" "$((T_HEALTH - T0))" "$((T_HEALTH - T_RUN))" <<'PY'
import json, sys
path, ami, iid, ip, pending, total, run2h = sys.argv[1:]
doc = {
  "ami": ami,
  "instance_id": iid,
  "ip": ip,
  "pending_to_running_s": int(pending),
  "total_to_health_s": int(total),
  "running_to_health_s": int(run2h),
}
open(path, "w").write(json.dumps(doc, indent=2) + "\n")
print(json.dumps(doc, indent=2))
print("wrote", path)
PY
