#!/usr/bin/env bash
# Build & push the Indexus node image to the Terraform ECR repo.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TF_DIR="$ROOT/deploy/terraform"
REGION="${AWS_REGION:-eu-west-3}"

cd "$TF_DIR"
ECR_URI=$(terraform output -raw ecr_repository_url)
ACCOUNT=$(echo "$ECR_URI" | cut -d. -f1)

echo "==> login $ECR_URI"
aws ecr get-login-password --region "$REGION" | docker login --username AWS --password-stdin "$ACCOUNT.dkr.ecr.$REGION.amazonaws.com"

echo "==> build"
cd "$ROOT"
docker build -t "$ECR_URI:latest" .

echo "==> push"
docker push "$ECR_URI:latest"
echo "pushed $ECR_URI:latest"
