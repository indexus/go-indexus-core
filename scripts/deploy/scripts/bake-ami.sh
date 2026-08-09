#!/usr/bin/env bash
# Bake a ready-to-run Indexus node AMI (AL2023 + awscli/jq + /opt/indexus/node).
# Spawn userdata then only writes runtime env and starts systemd — no dnf/S3 on boot.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
TF_DIR="$ROOT/scripts/deploy/terraform"
REGION="${AWS_REGION:-eu-west-3}"
PROJECT="${PROJECT:-indexus-aws}"
INSTANCE_TYPE="${BAKE_INSTANCE_TYPE:-t3.small}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/id_ed25519}"
SSH_OPTS=(-o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=15 -i "$SSH_KEY")

cd "$TF_DIR"
BUCKET=$(terraform output -raw artifacts_bucket)
PUBLIC_SUBNET_ID=$(python3 -c 'import re; t=open("terraform.tfvars").read(); m=re.search(r"public_subnet_id\s*=\s*\"([^\"]+)\"",t); print(m.group(1) if m else "")')
REGION=$(python3 -c 'import re; t=open("terraform.tfvars").read(); m=re.search(r"aws_region\s*=\s*\"([^\"]+)\"",t); print(m.group(1) if m else "eu-west-3")')
export AWS_REGION="$REGION"

# Reuse bootstrap networking / IAM / key
META=$(aws ec2 describe-instances --region "$REGION" \
  --filters "Name=tag:Name,Values=${PROJECT}-bootstrap" "Name=instance-state-name,Values=running,stopped" \
  --query 'Reservations[0].Instances[0].{Sg:SecurityGroups[0].GroupId,Profile:IamInstanceProfile.Arn,Key:KeyName,Subnet:SubnetId}' \
  --output json)
SG=$(echo "$META" | python3 -c 'import sys,json; print(json.load(sys.stdin)["Sg"])')
PROFILE=$(echo "$META" | python3 -c 'import sys,json; print((json.load(sys.stdin)["Profile"] or "").split("/")[-1])')
KEY=$(echo "$META" | python3 -c 'import sys,json; print(json.load(sys.stdin)["Key"])')
[[ -n "$PUBLIC_SUBNET_ID" ]] || PUBLIC_SUBNET_ID=$(echo "$META" | python3 -c 'import sys,json; print(json.load(sys.stdin)["Subnet"])')

AMI_BASE=$(aws ec2 describe-images --region "$REGION" --owners amazon \
  --filters "Name=name,Values=al2023-ami-2023*-x86_64" "Name=state,Values=available" \
  --query 'sort_by(Images,&CreationDate)[-1].ImageId' --output text)

echo "==> bake from $AMI_BASE subnet=$PUBLIC_SUBNET_ID sg=$SG profile=$PROFILE bucket=$BUCKET"

if ! aws s3 ls "s3://${BUCKET}/bin/node" --region "$REGION" >/dev/null 2>&1; then
  echo "missing s3://${BUCKET}/bin/node — build/upload binaries first" >&2
  exit 1
fi

USERDATA=$(mktemp)
IID=""
cleanup() {
  [[ -n "${IID:-}" ]] && aws ec2 terminate-instances --region "$REGION" --instance-ids "$IID" >/dev/null 2>&1 || true
  rm -f "$USERDATA"
}
trap cleanup EXIT

# Embed region/bucket into bake userdata.
cat >"$USERDATA" <<EOF
#!/bin/bash
set -euxo pipefail
exec > >(tee /var/log/indexus-bake.log) 2>&1
dnf install -y aws-cli jq || dnf install -y awscli jq
install -d -m 0755 /opt/indexus /etc/indexus /var/lib/indexus /var/lib/indexus/leave /var/lib/indexus/backup /var/lib/indexus/archive
aws s3 cp "s3://${BUCKET}/bin/node" /opt/indexus/node --region ${REGION}
chmod 0755 /opt/indexus/node
aws s3 cp "s3://${BUCKET}/bin/issuer" /opt/indexus/issuer --region ${REGION} || true
chmod 0755 /opt/indexus/issuer 2>/dev/null || true

cat >/etc/systemd/system/indexus-node.service <<'UNIT'
[Unit]
Description=Indexus Node
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
EnvironmentFile=-/etc/indexus/node.env
WorkingDirectory=/var/lib/indexus
ExecStart=/opt/indexus/node -p2pPort 21000 -monitoringPort 19000 -advertise \${PUBLIC_IP} -bootstrap \${BOOTSTRAP_HOST}|21000 -issuer \${ISSUER_URL} -network \${NETWORK_ID} -requireAuth -delegation \${DELEGATION} -autoscale -autoscaleRole \${AUTOSCALE_ROLE} -queuePressure \${QUEUE_PRESSURE} -pressureHold \${PRESSURE_HOLD} -scaleWindow \${SCALE_WINDOW} -scaleDownThreshold \${SCALE_DOWN_THRESHOLD} -scaleDownHold \${SCALE_DOWN_HOLD} -scaleCooldown \${SCALE_COOLDOWN} -storage /var/lib/indexus/backup -archive /var/lib/indexus/archive -nodeKey /etc/indexus/node.ed25519 -cert /etc/indexus/node.cert.json
Restart=always
RestartSec=2
[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable indexus-node.service
echo BAKE_OK > /opt/indexus/.baked
EOF

IID=$(aws ec2 run-instances --region "$REGION" \
  --image-id "$AMI_BASE" \
  --instance-type "$INSTANCE_TYPE" \
  --subnet-id "$PUBLIC_SUBNET_ID" \
  --security-group-ids "$SG" \
  --iam-instance-profile "Name=$PROFILE" \
  --key-name "$KEY" \
  --user-data "file://$USERDATA" \
  --block-device-mappings '[{"DeviceName":"/dev/xvda","Ebs":{"VolumeType":"gp3","VolumeSize":8,"DeleteOnTermination":true}}]' \
  --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=${PROJECT}-ami-bake},{Key=Project,Value=${PROJECT}},{Key=Role,Value=ami-bake}]" \
  --query 'Instances[0].InstanceId' --output text)
echo "==> bake instance $IID"

aws ec2 wait instance-running --region "$REGION" --instance-ids "$IID"
IP=""
for _ in $(seq 1 40); do
  IP=$(aws ec2 describe-instances --region "$REGION" --instance-ids "$IID" \
    --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
  [[ -n "$IP" && "$IP" != "None" ]] && break
  sleep 3
done
echo "==> bake ip $IP — waiting for BAKE_OK"

ok=0
for _ in $(seq 1 72); do
  if ssh "${SSH_OPTS[@]}" "ec2-user@$IP" \
    'test -f /opt/indexus/.baked && grep -q BAKE_OK /opt/indexus/.baked && test -x /opt/indexus/node' 2>/dev/null; then
    ok=1
    break
  fi
  sleep 5
done
if [[ "$ok" != "1" ]]; then
  echo "bake timed out; log:" >&2
  ssh "${SSH_OPTS[@]}" "ec2-user@$IP" \
    'sudo tail -50 /var/log/indexus-bake.log 2>/dev/null || sudo tail -50 /var/log/cloud-init-output.log' 2>&1 | tail -60 || true
  exit 1
fi

aws ec2 stop-instances --region "$REGION" --instance-ids "$IID" >/dev/null
aws ec2 wait instance-stopped --region "$REGION" --instance-ids "$IID"

STAMP=$(date -u +%Y%m%d%H%M%S)
AMI_NAME="${PROJECT}-node-${STAMP}"
AMI_ID=$(aws ec2 create-image --region "$REGION" \
  --instance-id "$IID" \
  --name "$AMI_NAME" \
  --description "Indexus node prebaked (bin+systemd+awscli)" \
  --query 'ImageId' --output text)
aws ec2 create-tags --region "$REGION" --resources "$AMI_ID" --tags \
  "Key=Name,Value=${AMI_NAME}" \
  "Key=Project,Value=${PROJECT}" \
  "Key=indexus:role,Value=node-ami" \
  "Key=indexus:baked,Value=${STAMP}"

echo "==> waiting for AMI $AMI_ID"
aws ec2 wait image-available --region "$REGION" --image-ids "$AMI_ID"

cat >"$TF_DIR/ami.auto.tfvars" <<EOF
ami_id = "$AMI_ID"
EOF
echo "==> wrote ami.auto.tfvars → ami_id=$AMI_ID"

aws ec2 terminate-instances --region "$REGION" --instance-ids "$IID" >/dev/null
IID=""
echo "AMI_ID=$AMI_ID"
echo "DONE"
