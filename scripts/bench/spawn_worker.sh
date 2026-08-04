#!/usr/bin/env bash
# Launch a new worker EC2, measure time until it appears in bootstrap /registered.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TF_DIR="$ROOT/deploy/terraform"
REGION="${AWS_REGION:-eu-west-3}"

BOOT_IP=$(cd "$TF_DIR" && terraform output -raw bootstrap_public_ip)
BOOT_ID=$(cd "$TF_DIR" && terraform output -raw bootstrap_instance_id)
BUCKET=$(cd "$TF_DIR" && terraform output -raw artifacts_bucket)
ISSUER="http://${BOOT_IP}:22000"
DELEGATION="${DELEGATION:-40}"
REGION="${AWS_REGION:-eu-west-3}"

META=$(aws ec2 describe-instances --region "$REGION" --instance-ids "$BOOT_ID" \
  --query 'Reservations[0].Instances[0].{Ami:ImageId,Subnet:SubnetId,Sg:SecurityGroups[0].GroupId,Profile:IamInstanceProfile.Arn,Key:KeyName,Type:InstanceType}' \
  --output json)
AMI=$(echo "$META" | python3 -c 'import sys,json; print(json.load(sys.stdin)["Ami"])')
SUBNET=$(echo "$META" | python3 -c 'import sys,json; print(json.load(sys.stdin)["Subnet"])')
SG=$(echo "$META" | python3 -c 'import sys,json; print(json.load(sys.stdin)["Sg"])')
KEY=$(echo "$META" | python3 -c 'import sys,json; print(json.load(sys.stdin)["Key"])')
TYPE=$(echo "$META" | python3 -c 'import sys,json; print(json.load(sys.stdin)["Type"])')
PROFILE_NAME=$(echo "$META" | python3 -c 'import sys,json; print((json.load(sys.stdin)["Profile"] or "").split("/")[-1])')

# shellcheck disable=SC2016
USERDATA=$(cat <<EOF
#!/bin/bash
set -euxo pipefail
dnf install -y awscli jq || true
install -d -m 0755 /opt/indexus /etc/indexus /var/lib/indexus
aws s3 cp "s3://${BUCKET}/bin/node" /opt/indexus/node --region ${REGION}
chmod +x /opt/indexus/node
TOKEN=\$(curl -sf -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 21600")
PUBLIC_IP=\$(curl -sf -H "X-aws-ec2-metadata-token: \$TOKEN" http://169.254.169.254/latest/meta-data/public-ipv4)
cat >/etc/systemd/system/indexus-node.service <<UNIT
[Unit]
Description=Indexus Worker Node
After=network-online.target
Wants=network-online.target
[Service]
Environment=HOME=/var/lib/indexus
Environment=AWS_REGION=${REGION}
Environment=SNAPSHOT_BUCKET=${BUCKET}
Environment=INDEXUS_STORAGE=/var/lib/indexus/backup
Environment=INDEXUS_LEAVE_DIR=/var/lib/indexus/leave
WorkingDirectory=/var/lib/indexus
ExecStart=/opt/indexus/node -p2pPort 21000 -monitoringPort 19000 -advertise \${PUBLIC_IP} -bootstrap ${BOOT_IP}|21000 -issuer ${ISSUER} -network indexus-aws -requireAuth -delegation ${DELEGATION} -storage /var/lib/indexus/backup -archive /var/lib/indexus/archive -nodeKey /etc/indexus/node.ed25519 -cert /etc/indexus/node.cert.json
Restart=always
RestartSec=3
[Install]
WantedBy=multi-user.target
UNIT
# Force-expand PUBLIC_IP into the unit file (systemd won't expand shell vars)
sed -i "s|\\\${PUBLIC_IP}|\${PUBLIC_IP}|g" /etc/systemd/system/indexus-node.service
systemctl daemon-reload
systemctl enable --now indexus-node.service
EOF
)

echo "==> run-instances (spawn)"
T0=$(python3 -c 'import time; print(int(time.time()*1000))')
INSTANCE=$(aws ec2 run-instances --region "$REGION" \
  --image-id "$AMI" \
  --instance-type "$TYPE" \
  --subnet-id "$SUBNET" \
  --security-group-ids "$SG" \
  --key-name "$KEY" \
  --iam-instance-profile "Name=$PROFILE_NAME" \
  --associate-public-ip-address \
  --user-data "$USERDATA" \
  --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=indexus-aws-spawned},{Key=Project,Value=indexus-aws}]" \
  --query 'Instances[0].InstanceId' --output text)

echo "instance=$INSTANCE"
aws ec2 wait instance-running --region "$REGION" --instance-ids "$INSTANCE"
T_RUN=$(python3 -c 'import time; print(int(time.time()*1000))')
NEW_IP=$(aws ec2 describe-instances --region "$REGION" --instance-ids "$INSTANCE" \
  --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
echo "public_ip=$NEW_IP running_ms=$((T_RUN - T0))"

echo "==> wait until registered on bootstrap"
JOINED=0
for i in $(seq 1 90); do
  REG=$(curl -sf --max-time 5 "http://${BOOT_IP}:19000/registered" || echo '{}')
  if echo "$REG" | grep -q "$NEW_IP"; then
    JOINED=1
    break
  fi
  sleep 2
done
T_JOIN=$(python3 -c 'import time; print(int(time.time()*1000))')

python3 - <<PY
import json
print(json.dumps({
  "instance_id": "$INSTANCE",
  "public_ip": "$NEW_IP",
  "ec2_running_ms": $((T_RUN - T0)),
  "joined_mesh": bool($JOINED),
  "total_to_registered_ms": $((T_JOIN - T0)),
  "bootstrap": "$BOOT_IP",
}, indent=2))
PY

[[ "$JOINED" == "1" ]] || { echo "WARN: not in /registered yet"; exit 2; }
