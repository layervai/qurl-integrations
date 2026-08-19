#!/bin/sh
# Verify release-please-config.json and .release-please-manifest.json agree:
# package keys stay in sync, bare v* tags stay the CLI's alone
# (include-component-in-tag: false on any other package would mint a second
# bare-tag version stream that collides with the tag contract scripts/
# install.sh and GoReleaser depend on — see .github/workflows/
# release-please.yml's header), and the CLI declares no component. Nothing
# else checks these invariants before merge; drift otherwise surfaces only
# post-merge inside the release workflow or, worse, in the public installer.
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

# apps/cli must declare no `component`. It reads as harmless — with
# include-component-in-tag: false the component never reaches the tag, the
# release name, the changelog heading or the PR body section, all of which come
# from getComponent(), which returns '' for a bare-tagged package regardless.
# But getBranchComponent() does NOT consult include-component-in-tag, and
# buildRelease compares it against the merged release PR's *branch* component
# whenever that PR carries a single componentless section — which is exactly a
# release PR holding the CLI and nothing else. The manifest release PR is always
# on `release-please--branches--main`, so its branch component is undefined; any
# declared component loses that comparison and release-please skips the release
# with a `PR component: undefined does not match configured component: cli`
# warning while still exiting 0. That dropped v1.1.0, v1.3.0 and v1.4.0 — the
# manifest and apps/cli/CHANGELOG.md landed on main with no tag and no GitHub
# Release, and every later release-please run aborted with "There are untagged,
# merged release PRs outstanding", blocking release PRs for every component
# until someone relabelled the merged PR by hand. A CLI release sharing its PR
# with any other component takes a different code path and succeeds, so this is
# invisible until the CLI is released alone. scripts/verify-cli-release.sh is
# the runtime half of the guard; this is the half that stops it recurring.
cli = packages.get("apps/cli", {})
if "component" in cli:
    raise SystemExit(
        "apps/cli must not declare a component: release-please refuses to build "
        "its release when the CLI is alone in the manifest release PR "
        f"(found component: {cli['component']!r})"
    )

print(
    "release-please config/manifest in sync; bare v* tag reserved to apps/cli; "
    "apps/cli declares no component"
)
EOF
