#!/usr/bin/env bash
# Bake a ready-to-run Indexus load-writer AMI (Node + bench bundle + npm).
# spawn_load.sh then only boots the AMI and starts the storm — no dnf/npm on spin.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
DASHBOARD="$(cd "$ROOT/../dashboard" && pwd)"
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

echo "==> pack load bundle → s3://${BUCKET}/bench/load-bundle.tgz"
bash "$ROOT/scripts/deploy/scripts/pack_load_bundle.sh" "$BUCKET" "$REGION"

echo "==> bake load AMI from $AMI_BASE subnet=$PUBLIC_SUBNET_ID sg=$SG"

USERDATA=$(mktemp)
IID=""
cleanup() {
  [[ -n "${IID:-}" ]] && aws ec2 terminate-instances --region "$REGION" --instance-ids "$IID" >/dev/null 2>&1 || true
  rm -f "$USERDATA"
}
trap cleanup EXIT

cat >"$USERDATA" <<EOF
#!/bin/bash
set -euxo pipefail
exec > >(tee /var/log/indexus-load-bake.log) 2>&1
dnf install -y nodejs npm tar awscli jq 2>/dev/null || dnf install -y nodejs20 npm tar aws-cli jq || true
# Prefer node 18+ if modular
command -v node >/dev/null
install -d -m 0755 /opt/indexus-load
aws s3 cp "s3://${BUCKET}/bench/load-bundle.tgz" /tmp/load-bundle.tgz --region ${REGION}
tar -C /opt/indexus-load -xzf /tmp/load-bundle.tgz
cd /opt/indexus-load/bench
npm install --omit=dev
cat >/usr/local/bin/indexus-load-run <<'RUN'
#!/bin/bash
set -euo pipefail
cd /opt/indexus-load/bench
exec node multi_client_storm.js "\$@"
RUN
chmod +x /usr/local/bin/indexus-load-run
echo BAKE_OK > /opt/indexus-load/.baked
EOF

IID=$(aws ec2 run-instances --region "$REGION" \
  --image-id "$AMI_BASE" \
  --instance-type "$INSTANCE_TYPE" \
  --subnet-id "$PUBLIC_SUBNET_ID" \
  --security-group-ids "$SG" \
  --iam-instance-profile "Name=$PROFILE" \
  --key-name "$KEY" \
  --user-data "file://$USERDATA" \
  --block-device-mappings '[{"DeviceName":"/dev/xvda","Ebs":{"VolumeType":"gp3","VolumeSize":12,"DeleteOnTermination":true}}]' \
  --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=${PROJECT}-load-ami-bake},{Key=Project,Value=${PROJECT}},{Key=Role,Value=load-ami-bake}]" \
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
for _ in $(seq 1 90); do
  if ssh "${SSH_OPTS[@]}" "ec2-user@$IP" \
    'test -f /opt/indexus-load/.baked && grep -q BAKE_OK /opt/indexus-load/.baked && test -x /usr/local/bin/indexus-load-run' 2>/dev/null; then
    ok=1
    break
  fi
  sleep 5
done
if [[ "$ok" != "1" ]]; then
  echo "bake timed out; log:" >&2
  ssh "${SSH_OPTS[@]}" "ec2-user@$IP" \
    'sudo tail -80 /var/log/indexus-load-bake.log 2>/dev/null || sudo tail -80 /var/log/cloud-init-output.log' 2>&1 | tail -80 || true
  exit 1
fi

aws ec2 stop-instances --region "$REGION" --instance-ids "$IID" >/dev/null
aws ec2 wait instance-stopped --region "$REGION" --instance-ids "$IID"

STAMP=$(date -u +%Y%m%d%H%M%S)
AMI_NAME="${PROJECT}-load-${STAMP}"
AMI_ID=$(aws ec2 create-image --region "$REGION" \
  --instance-id "$IID" \
  --name "$AMI_NAME" \
  --description "Indexus load writers prebaked (node+bench+npm)" \
  --query 'ImageId' --output text)
aws ec2 create-tags --region "$REGION" --resources "$AMI_ID" --tags \
  "Key=Name,Value=${AMI_NAME}" \
  "Key=Project,Value=${PROJECT}" \
  "Key=indexus:role,Value=load-ami" \
  "Key=indexus:baked,Value=${STAMP}"

echo "==> waiting for AMI $AMI_ID"
aws ec2 wait image-available --region "$REGION" --image-ids "$AMI_ID"

cat >"$TF_DIR/load_ami.auto.tfvars" <<EOF
# written by bake-load-ami.sh — consumed by spawn_load.sh
load_ami_id = "$AMI_ID"
EOF
echo "$AMI_ID" >"$TF_DIR/load_ami_id"
echo "==> wrote load_ami_id=$AMI_ID"

aws ec2 terminate-instances --region "$REGION" --instance-ids "$IID" >/dev/null
IID=""
echo "LOAD_AMI_ID=$AMI_ID"
echo "DONE"
