#!/usr/bin/env bash
# Pack dashboard load artifacts → s3://$BUCKET/bench/load-bundle.tgz
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
DASHBOARD="$(cd "$ROOT/../dashboard" && pwd)"
BUCKET="${1:?bucket}"
REGION="${2:-${AWS_REGION:-eu-west-3}}"

BUNDLE_DIR=$(mktemp -d)
trap 'rm -rf "$BUNDLE_DIR"' EXIT
mkdir -p "$BUNDLE_DIR/bench/lib" "$BUNDLE_DIR/bench/data" "$BUNDLE_DIR/bench/scripts"
cp "$DASHBOARD/scripts/load_density.js" "$BUNDLE_DIR/bench/scripts/"
cp "$DASHBOARD/lib/mesh_route.js" "$BUNDLE_DIR/bench/lib/"
if [[ -f "$DASHBOARD/data/world_density_100k.csv" ]]; then
  cp "$DASHBOARD/data/world_density_100k.csv" "$BUNDLE_DIR/bench/data/"
fi

SDK_SRC=""
for cand in \
  "$ROOT/../sdk-js" \
  "$DASHBOARD/node_modules/js-indexus-sdk" \
  "$ROOT/node_modules/js-indexus-sdk"
do
  if [[ -d "$cand/dist" || -f "$cand/package.json" ]]; then SDK_SRC="$cand"; break; fi
done
if [[ -z "$SDK_SRC" ]]; then
  SDK_SRC=$(cd "$DASHBOARD" && node --input-type=commonjs -e 'try{console.log(require.resolve("js-indexus-sdk/package.json").replace(/\/package\.json$/,""))}catch(e){}' 2>/dev/null || true)
fi
if [[ -n "$SDK_SRC" && -d "$SDK_SRC" ]]; then
  mkdir -p "$BUNDLE_DIR/bench/sdk"
  cp -R "$SDK_SRC"/. "$BUNDLE_DIR/bench/sdk/"
  mkdir -p "$BUNDLE_DIR/bench/node_modules"
  ln -sfn ../sdk "$BUNDLE_DIR/bench/node_modules/js-indexus-sdk" 2>/dev/null || \
    cp -R "$SDK_SRC" "$BUNDLE_DIR/bench/node_modules/js-indexus-sdk"
else
  echo "WARN: js-indexus-sdk not found locally" >&2
fi

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

export COPYFILE_DISABLE=1
TAR_OPTS=(-czf)
if tar --help 2>&1 | grep -q -- '--no-xattrs'; then
  TAR_OPTS+=(--no-xattrs)
fi
if tar --help 2>&1 | grep -q -- '--no-mac-metadata'; then
  TAR_OPTS+=(--no-mac-metadata)
fi
tar -C "$BUNDLE_DIR" "${TAR_OPTS[@]}" "$BUNDLE_DIR/load-bundle.tgz" bench
aws s3 cp "$BUNDLE_DIR/load-bundle.tgz" "s3://${BUCKET}/bench/load-bundle.tgz" --region "$REGION" >/dev/null
echo "uploaded s3://${BUCKET}/bench/load-bundle.tgz ($(wc -c <"$BUNDLE_DIR/load-bundle.tgz") bytes)"
