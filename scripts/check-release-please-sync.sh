#!/bin/sh
# Verify release-please-config.json packages and .release-please-manifest.json
# keys stay in sync. Nothing else checks the pair before merge, and drift
# otherwise surfaces only post-merge inside the release workflow.
set -eu

cd "$(git rev-parse --show-toplevel)"

python3 - <<'EOF'
import json

with open("release-please-config.json") as f:
    config = set(json.load(f)["packages"])
with open(".release-please-manifest.json") as f:
    manifest = set(json.load(f))
drift = sorted(config ^ manifest)
if drift:
    raise SystemExit(f"release-please config/manifest key drift: {drift}")
print("release-please config and manifest are in sync")
EOF
