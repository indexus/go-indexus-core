#!/usr/bin/env bash
# Ensure N EC2 load generators tagged Name=indexus-aws-load, then start
# multi_client_storm against BOOT_IP. Used by mesh_dash Start (remote writers).
#
# Env:
#   BOOT_IP / ISSUER / COUNT / CLIENTS / CONCURRENCY / RAMP_S / COLLECTION
#   LOAD_HOSTS   number of load VMs (default 2)
#   LOAD_TYPE    instance type (default t3.small)
#   REGION / PROJECT / ARTIFACTS_BUCKET
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BENCH="$ROOT/scripts/bench"
TF_DIR="$ROOT/deploy/terraform"
REGION="${AWS_REGION:-${REGION:-eu-west-3}}"
PROJECT="${PROJECT:-indexus-aws}"
LOAD_HOSTS="${LOAD_HOSTS:-2}"
LOAD_TYPE="${LOAD_TYPE:-t3.small}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/id_ed25519}"
SSH_OPTS=(-o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=12 -i "$SSH_KEY")

BOOT_IP="${BOOT_IP:-}"
if [[ -z "$BOOT_IP" ]]; then
  BOOT_IP=$(cd "$TF_DIR" && terraform output -raw bootstrap_public_ip 2>/dev/null || true)
fi
[[ -n "$BOOT_IP" && "$BOOT_IP" != "None" ]] || { echo "BOOT_IP required" >&2; exit 1; }
ISSUER="${ISSUER:-http://${BOOT_IP}:22000}"
SEED="http://${BOOT_IP}:21000"

BUCKET="${ARTIFACTS_BUCKET:-}"
if [[ -z "$BUCKET" ]]; then
  BUCKET=$(cd "$TF_DIR" && terraform output -raw artifacts_bucket 2>/dev/null || true)
fi
[[ -n "$BUCKET" && "$BUCKET" != "None" ]] || { echo "ARTIFACTS_BUCKET required" >&2; exit 1; }

BOOT_ID=$(aws ec2 describe-instances --region "$REGION" \
  --filters "Name=tag:Name,Values=${PROJECT}-bootstrap" "Name=instance-state-name,Values=running,stopped" \
  --query 'Reservations[0].Instances[0].InstanceId' --output text 2>/dev/null || true)
[[ -n "$BOOT_ID" && "$BOOT_ID" != "None" ]] || { echo "bootstrap instance not found" >&2; exit 1; }

META=$(aws ec2 describe-instances --region "$REGION" --instance-ids "$BOOT_ID" \
  --query 'Reservations[0].Instances[0].{Ami:ImageId,Subnet:SubnetId,Sg:SecurityGroups[0].GroupId,Profile:IamInstanceProfile.Arn,Key:KeyName}' \
  --output json)
AMI=$(echo "$META" | python3 -c 'import sys,json; print(json.load(sys.stdin)["Ami"])')
SUBNET=$(echo "$META" | python3 -c 'import sys,json; print(json.load(sys.stdin)["Subnet"])')
SG=$(echo "$META" | python3 -c 'import sys,json; print(json.load(sys.stdin)["Sg"])')
KEY=$(echo "$META" | python3 -c 'import sys,json; print(json.load(sys.stdin)["Key"])')
PROFILE_NAME=$(echo "$META" | python3 -c 'import sys,json; print((json.load(sys.stdin)["Profile"] or "").split("/")[-1])')

echo "==> packing load bundle → s3://${BUCKET}/bench/load-bundle.tgz"
BUNDLE_DIR=$(mktemp -d)
trap 'rm -rf "$BUNDLE_DIR"' EXIT
mkdir -p "$BUNDLE_DIR/bench/lib" "$BUNDLE_DIR/bench/data"
cp "$BENCH/geo_load_density.js" "$BENCH/multi_client_storm.js" "$BUNDLE_DIR/bench/"
cp "$BENCH/lib/mesh_route.js" "$BUNDLE_DIR/bench/lib/"
cp "$BENCH/data/world_density_100k.csv" "$BUNDLE_DIR/bench/data/"
# Vendor SDK from local install if available
SDK_SRC=""
for cand in \
  "$ROOT/../sdk-js" \
  "$ROOT/sdk-js" \
  "$(cd "$ROOT/.." 2>/dev/null && pwd)/sdk-js" \
  "$BENCH/node_modules/js-indexus-sdk" \
  "$ROOT/node_modules/js-indexus-sdk"
do
  if [[ -d "$cand/dist" || -f "$cand/package.json" ]]; then SDK_SRC="$cand"; break; fi
done
if [[ -z "$SDK_SRC" ]]; then
  SDK_SRC=$(cd "$BENCH" && node --input-type=commonjs -e 'try{console.log(require.resolve("js-indexus-sdk/package.json").replace(/\/package\.json$/,""))}catch(e){}' 2>/dev/null || true)
fi
if [[ -n "$SDK_SRC" && -d "$SDK_SRC" ]]; then
  mkdir -p "$BUNDLE_DIR/bench/sdk"
  cp -R "$SDK_SRC"/. "$BUNDLE_DIR/bench/sdk/"
  # Ensure package name resolves as js-indexus-sdk
  if [[ ! -f "$BUNDLE_DIR/bench/node_modules/js-indexus-sdk/package.json" ]]; then
    mkdir -p "$BUNDLE_DIR/bench/node_modules"
    ln -sfn ../sdk "$BUNDLE_DIR/bench/node_modules/js-indexus-sdk" 2>/dev/null || \
      cp -R "$SDK_SRC" "$BUNDLE_DIR/bench/node_modules/js-indexus-sdk"
  fi
else
  echo "WARN: js-indexus-sdk not found locally — load VMs need it vendored" >&2
fi
# Always ship axios via npm on the VM; keep package.json simple
cat >"$BUNDLE_DIR/bench/package.json" <<'EOF'
{
  "name": "indexus-load",
  "type": "module",
  "dependencies": {
    "axios": "^1.7.0",
    "js-indexus-sdk": "file:./sdk"
  }
}
EOF
tar -C "$BUNDLE_DIR" -czf "$BUNDLE_DIR/load-bundle.tgz" bench
aws s3 cp "$BUNDLE_DIR/load-bundle.tgz" "s3://${BUCKET}/bench/load-bundle.tgz" --region "$REGION" >/dev/null

list_load() {
  aws ec2 describe-instances --region "$REGION" \
    --filters "Name=tag:Name,Values=${PROJECT}-load,indexus-aws-load" \
      "Name=instance-state-name,Values=running,pending,stopped" \
    --query 'Reservations[].Instances[].[InstanceId,State.Name,PublicIpAddress]' \
    --output text 2>/dev/null || true
}

RUNNING_IPS=()
while read -r id state ip; do
  [[ -z "$id" || "$id" == "None" ]] && continue
  if [[ "$state" == "stopped" ]]; then
    echo "==> starting stopped load $id"
    aws ec2 start-instances --region "$REGION" --instance-ids "$id" >/dev/null
    aws ec2 wait instance-running --region "$REGION" --instance-ids "$id"
    ip=$(aws ec2 describe-instances --region "$REGION" --instance-ids "$id" \
      --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
  fi
  if [[ "$state" == "pending" || "$state" == "running" || -n "$ip" ]]; then
    [[ "$ip" == "None" || -z "$ip" ]] && continue
    RUNNING_IPS+=("$ip")
  fi
done < <(list_load)

NEED=$((LOAD_HOSTS - ${#RUNNING_IPS[@]}))
if (( NEED > 0 )); then
  echo "==> spawning $NEED load VM(s) type=$LOAD_TYPE"
  # shellcheck disable=SC2016
  USERDATA=$(cat <<EOF
#!/bin/bash
set -euxo pipefail
dnf install -y nodejs npm tar awscli jq 2>/dev/null || yum install -y nodejs npm tar awscli jq
install -d -m 0755 /opt/indexus-load
aws s3 cp "s3://${BUCKET}/bench/load-bundle.tgz" /tmp/load-bundle.tgz --region ${REGION}
tar -C /opt/indexus-load -xzf /tmp/load-bundle.tgz
cd /opt/indexus-load/bench
npm install --omit=dev || true
cat >/usr/local/bin/indexus-load-run <<'RUN'
#!/bin/bash
set -euo pipefail
cd /opt/indexus-load/bench
exec node multi_client_storm.js "\$@"
RUN
chmod +x /usr/local/bin/indexus-load-run
EOF
)
  for _ in $(seq 1 "$NEED"); do
    INSTANCE=$(aws ec2 run-instances --region "$REGION" \
      --image-id "$AMI" \
      --instance-type "$LOAD_TYPE" \
      --subnet-id "$SUBNET" \
      --security-group-ids "$SG" \
      --key-name "$KEY" \
      --iam-instance-profile "Name=$PROFILE_NAME" \
      --associate-public-ip-address \
      --user-data "$USERDATA" \
      --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=indexus-aws-load},{Key=Project,Value=${PROJECT}},{Key=Role,Value=load}]" \
      --query 'Instances[0].InstanceId' --output text)
    echo "spawned $INSTANCE"
    aws ec2 wait instance-running --region "$REGION" --instance-ids "$INSTANCE"
    ip=$(aws ec2 describe-instances --region "$REGION" --instance-ids "$INSTANCE" \
      --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
    RUNNING_IPS+=("$ip")
  done
fi

# Cap to LOAD_HOSTS
IPS=("${RUNNING_IPS[@]:0:$LOAD_HOSTS}")
echo "==> load hosts: ${IPS[*]}"

COUNT="${COUNT:-3000000}"
CLIENTS="${CLIENTS:-48}"
CONCURRENCY="${CONCURRENCY:-4}"
RAMP_S="${RAMP_S:-360}"
COLLECTION="${COLLECTION:-Load$(date +%s | tail -c 6)}"
# Split clients across hosts
N=${#IPS[@]}
(( N >= 1 )) || { echo "no load hosts" >&2; exit 1; }
PER=$(( (CLIENTS + N - 1) / N ))
SLICE=$(( (COUNT + N - 1) / N ))

for ip in "${IPS[@]}"; do
  echo "==> waiting for ssh $ip"
  for i in $(seq 1 60); do
    if ssh "${SSH_OPTS[@]}" ec2-user@"$ip" 'test -x /usr/local/bin/indexus-load-run || test -f /opt/indexus-load/bench/multi_client_storm.js' 2>/dev/null; then
      break
    fi
    # userdata may still be installing
    sleep 5
  done
  # Refresh bundle in case instance was reused
  ssh "${SSH_OPTS[@]}" ec2-user@"$ip" "sudo bash -s" <<REMOTE
set -euo pipefail
aws s3 cp "s3://${BUCKET}/bench/load-bundle.tgz" /tmp/load-bundle.tgz --region ${REGION}
sudo mkdir -p /opt/indexus-load
sudo tar -C /opt/indexus-load -xzf /tmp/load-bundle.tgz
cd /opt/indexus-load/bench && sudo npm install --omit=dev >/tmp/npm-load.log 2>&1 || true
sudo tee /usr/local/bin/indexus-load-run >/dev/null <<'RUN'
#!/bin/bash
set -euo pipefail
cd /opt/indexus-load/bench
exec node multi_client_storm.js "\$@"
RUN
sudo chmod +x /usr/local/bin/indexus-load-run
# stop previous storm
sudo pkill -f 'multi_client_storm|geo_load_density' 2>/dev/null || true
REMOTE
  echo "==> start storm on $ip clients=$PER count=$SLICE"
  ssh "${SSH_OPTS[@]}" ec2-user@"$ip" \
    "nohup env ISSUER='${ISSUER}' NODES='${SEED}' COUNT='${SLICE}' CLIENTS='${PER}' CONCURRENCY='${CONCURRENCY}' RAMP_S='${RAMP_S}' COLLECTION='${COLLECTION}' \
      /usr/local/bin/indexus-load-run > /tmp/indexus-load.log 2>&1 & echo \$!"
done

python3 - <<PY
import json
ips = """${IPS[*]}""".split()
print(json.dumps({
  "mode": "remote",
  "hosts": len(ips),
  "ips": ips,
  "clients_total": int("${CLIENTS}"),
  "count_total": int("${COUNT}"),
  "collection": "${COLLECTION}",
  "boot_ip": "${BOOT_IP}",
  "seed": "${SEED}",
}, indent=2))
PY
