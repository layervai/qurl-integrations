#!/bin/sh
# Verify release-please-config.json and .release-please-manifest.json agree:
# package keys stay in sync, and bare v* tags stay the CLI's alone
# (include-component-in-tag: false on any other package would mint a second
# bare-tag version stream that collides with the tag contract scripts/
# install.sh and GoReleaser depend on — see .github/workflows/
# release-please.yml's header). Nothing else checks either invariant before
# merge; drift otherwise surfaces only post-merge inside the release
# workflow or, worse, in the public installer.
set -eu

cd "$(git rev-parse --show-toplevel)"

command -v python3 >/dev/null 2>&1 || {
    echo "Error: python3 is required (JSON parsing); install python3 and retry" >&2
    exit 1
}

python3 - <<'EOF'
import json

with open("release-please-config.json") as f:
    packages = json.load(f)["packages"]
with open(".release-please-manifest.json") as f:
    manifest = set(json.load(f))

drift = sorted(set(packages) ^ manifest)
if drift:
    raise SystemExit(f"release-please config/manifest key drift: {drift}")

bare_tagged = sorted(
    name for name, pkg in packages.items()
    if pkg.get("include-component-in-tag") is False
)
if bare_tagged != ["apps/cli"]:
    raise SystemExit(
        "bare v* tags are reserved to apps/cli (see the release-please.yml "
        f"header); include-component-in-tag: false found on: {bare_tagged}"
    )

print("release-please config/manifest in sync; bare v* tag reserved to apps/cli")
EOF
