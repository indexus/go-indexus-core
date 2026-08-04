#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TF_DIR="$ROOT/deploy/terraform"
cd "$ROOT"

SSH_PUB="${SSH_PUB:-$HOME/.ssh/id_ed25519.pub}"
REGION="${AWS_REGION:-eu-west-3}"
VPC_ID="${VPC_ID:-vpc-090341ce12422054e}"
SUBNET_ID="${SUBNET_ID:-subnet-07b5fabb49ec95bfc}"

echo "==> build linux binaries"
mkdir -p "$ROOT/bin/linux"
GOOS=linux GOARCH=amd64 go build -o "$ROOT/bin/linux/issuer" ./app/issuer
GOOS=linux GOARCH=amd64 go build -o "$ROOT/bin/linux/node" ./app/node

echo "==> terraform init/apply"
cd "$TF_DIR"
if [[ ! -f terraform.tfvars ]]; then
  cat > terraform.tfvars <<EOF
aws_region       = "$REGION"
vpc_id           = "$VPC_ID"
public_subnet_id = "$SUBNET_ID"
worker_count     = 0
admin_cidrs      = ["0.0.0.0/0"]
ssh_public_key   = "$(cat "$SSH_PUB")"
EOF
fi

terraform init -input=false
terraform apply -auto-approve -input=false

BUCKET=$(terraform output -raw artifacts_bucket)
BOOT_IP=$(terraform output -raw bootstrap_public_ip)
ISSUER=$(terraform output -raw issuer_url)

echo "==> upload binaries to s3://$BUCKET"
aws s3 cp "$ROOT/bin/linux/issuer" "s3://$BUCKET/bin/issuer" --region "$REGION"
aws s3 cp "$ROOT/bin/linux/node" "s3://$BUCKET/bin/node" --region "$REGION"

echo "==> bake spawned AMI (preinstall binary + systemd — cuts cold spawn)"
chmod +x "$ROOT/deploy/scripts/bake-ami.sh"
"$ROOT/deploy/scripts/bake-ami.sh"

echo "==> re-apply launch template with baked AMI"
cd "$TF_DIR"
terraform apply -auto-approve -input=false
BOOT_IP=$(terraform output -raw bootstrap_public_ip)
ISSUER=$(terraform output -raw issuer_url)
BUCKET=$(terraform output -raw artifacts_bucket)
cd "$ROOT"

if command -v docker >/dev/null 2>&1; then
  echo "==> push ECR node image (optional fallback path for spawn)"
  chmod +x "$ROOT/deploy/scripts/push-ecr.sh"
  "$ROOT/deploy/scripts/push-ecr.sh" || echo "WARN: ECR push failed (spawned will use S3 binary)"
else
  echo "==> skip ECR (no docker); spawned use baked AMI"
fi

echo "==> waiting for issuer at $ISSUER"
for i in $(seq 1 60); do
  if curl -sf "$ISSUER/health" >/dev/null; then
    echo "issuer is up"
    break
  fi
  sleep 10
  if [[ $i -eq 60 ]]; then
    echo "issuer did not become healthy in time"
    exit 1
  fi
done

TOKEN=$(curl -sf -X POST "$ISSUER/v1/issue/token" \
  -H 'Content-Type: application/json' \
  -d '{"client_id":"aws-smoke","scopes":["read","write"]}' | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])')

echo "==> smoke: unauthorized read"
CODE=$(curl -s -o /dev/null -w '%{http_code}' "http://$BOOT_IP:21000/set?collection=c&location=@")
echo "no-token => $CODE (expect 401)"

echo "==> smoke: authorized write"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://$BOOT_IP:21000/item" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"item":{"collection":"aws-demo","location":"a","id":"1","metrics":[1,2,3,4,5]},"root":"@","current":"a"}')
echo "write => $CODE (expect 201)"

echo
echo "Deploy OK"
echo "  issuer:    $ISSUER"
echo "  bootstrap: $BOOT_IP"
terraform output worker_public_ips
echo "  ssh:       $(terraform output -raw ssh_bootstrap)"
