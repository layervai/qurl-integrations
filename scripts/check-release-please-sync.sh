#!/bin/sh
# Verify release-please-config.json packages and .release-please-manifest.json
# keys stay in sync. Nothing else checks the pair before merge, and drift
# otherwise surfaces only post-merge inside the release workflow.
set -eu

cd "$(git rev-parse --show-toplevel)"

python3 - <<'EOF'
import json

config = set(json.load(open("release-please-config.json"))["packages"])
manifest = set(json.load(open(".release-please-manifest.json")))
drift = sorted(config ^ manifest)
if drift:
    raise SystemExit(f"release-please config/manifest key drift: {drift}")
print("release-please config and manifest are in sync")
EOF
