#!/bin/sh
# Fail a release run that moved the CLI's manifest version without producing
# the release behind it. Runs after the release-please action on every push to
# main (see .github/workflows/release-please.yml).
#
# The failure this exists for is silent by construction. release-please builds
# a package's release only after matching the merged release PR against that
# package (strategies/base.ts, buildRelease). When the PR body carries a single
# componentless section it takes the "standalone release PR" path and compares
# the PR's *branch* component against getBranchComponent() — and
# getBranchComponent(), unlike getComponent(), ignores
# include-component-in-tag. The manifest release PR is always on
# `release-please--branches--main`, whose branch component is undefined, and
# the CLI's section is componentless because it is bare-tagged. So a `component`
# declared for apps/cli loses that comparison: release-please logs
# `PR component: undefined does not match configured component: cli`, skips the
# release, and the action still exits 0. A CLI release sharing its PR with any
# other component takes the multi-section path instead and succeeds, so this
# only ever bites when the CLI is released alone.
#
# What that leaves behind: .release-please-manifest.json and
# apps/cli/CHANGELOG.md on main with no tag and no GitHub Release, a green
# workflow run, and every later release-please run aborting with "There are
# untagged, merged release PRs outstanding" — which blocks release PRs for
# every component, not just the CLI, until someone relabels the merged PR by
# hand. It dropped v1.1.0, v1.3.0 and v1.4.0 before anyone noticed.
#
# scripts/check-release-please-sync.sh pins the config half of the fix (apps/cli
# declares no component). This is the runtime half. The config can only be wrong
# once; a green run that released nothing is invisible every time, and this is
# the only thing that looks.
#
# Silent on an ordinary push: nearly every commit to main releases nothing, the
# manifest still names the last released version, and that release exists. It
# speaks only when the manifest names a CLI version nothing released.
set -eu

CLI_PACKAGE='apps/cli'
MANIFEST='.release-please-manifest.json'
CHANGELOG='apps/cli/CHANGELOG.md'

# The release lookup is the one step here that can fail for a reason other than
# the drop, and reporting a network blip as a dropped release would send
# someone through a manual recovery that repairs nothing. Retry, then fail with
# a message that says "could not verify" rather than "dropped". Both knobs
# exist for scripts/test-verify-cli-release.sh, which must not sleep.
attempts="${RELEASE_LOOKUP_ATTEMPTS:-3}"
delay="${RELEASE_LOOKUP_DELAY:-5}"

cd "$(git rev-parse --show-toplevel)"

: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must be set (this script queries the releases of that repository)}"

for tool in python3 gh; do
    command -v "$tool" >/dev/null 2>&1 || {
        echo "Error: $tool is required by $0; install it and retry" >&2
        exit 1
    }
done

[ -f "$MANIFEST" ] || {
    echo "Error: $MANIFEST not found; this script needs the repository checked out at the commit being verified" >&2
    exit 1
}

# python3 rather than a sed of the JSON: this is the same pair of files
# scripts/check-release-please-sync.sh parses, with the same tool, and a
# hand-rolled extraction that silently returns the wrong string here would
# invent a drop rather than report one.
version="$(
    MANIFEST="$MANIFEST" CLI_PACKAGE="$CLI_PACKAGE" python3 - <<'PY'
import json
import os
import re

manifest_path = os.environ["MANIFEST"]
package = os.environ["CLI_PACKAGE"]

with open(manifest_path) as f:
    manifest = json.load(f)

version = manifest.get(package)
# An absent or unparseable version is a broken manifest, not a dropped release.
# Fail here rather than look up a release named after nonsense, fail to find
# it, and announce a drop that never happened.
if not isinstance(version, str) or not re.fullmatch(
    r"\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.+-]+)?", version
):
    raise SystemExit(f"{manifest_path} has no usable {package} version: {version!r}")

print(version)
PY
)"

# The CLI is the one component tagged without a component prefix — the contract
# scripts/install.sh, the Homebrew cask and GoReleaser depend on. See the
# .github/workflows/release-please.yml header.
tag="v${version}"

found=''
missing=''
lookup_error=''
attempt=1

while [ "$attempt" -le "$attempts" ]; do
    lookup_status=0
    lookup_error="$(gh api "repos/${GITHUB_REPOSITORY}/releases/tags/${tag}" --silent 2>&1)" ||
        lookup_status=$?

    if [ "$lookup_status" -eq 0 ]; then
        found=yes
        break
    fi

    # gh renders an HTTP failure as "gh: <message> (HTTP <code>)". A 404 is an
    # answer rather than an error — the release genuinely is not there, and
    # retrying cannot change that.
    case "$lookup_error" in
    *'HTTP 404'*)
        missing=yes
        break
        ;;
    esac

    printf 'Release lookup for %s failed (attempt %s of %s): %s\n' \
        "$tag" "$attempt" "$attempts" "$lookup_error" >&2
    if [ "$attempt" -lt "$attempts" ]; then
        sleep "$delay"
    fi
    attempt=$((attempt + 1))
done

if [ -n "$found" ]; then
    printf '%s %s is released as %s; nothing was dropped.\n' "$CLI_PACKAGE" "$version" "$tag"
    exit 0
fi

if [ -z "$missing" ]; then
    printf '::error::Could not verify the %s release: looking up %s failed %s time(s), most recently with "%s". This is a lookup failure, not a dropped release — re-run this job to find out which.\n' \
        "$CLI_PACKAGE" "$tag" "$attempts" "$lookup_error"
    exit 1
fi

sha="${GITHUB_SHA:-HEAD}"

# One line, because a workflow command cannot span lines. The readable version
# follows, in the log and in the job summary.
printf '::error::release-please dropped the %s release: %s names %s at %s but GitHub Release %s does not exist, and the run still reported success. Recover by (1) creating release %s at %s with notes from the "## [%s]" section of %s, (2) re-running this workflow via workflow_dispatch with cli_tag=%s to attach the GoReleaser assets, and (3) relabelling the merged release PR "autorelease: pending" -> "autorelease: tagged" — until that label moves, every later release-please run aborts with "There are untagged, merged release PRs outstanding" and no component can cut a release.\n' \
    "$CLI_PACKAGE" "$MANIFEST" "$version" "$sha" "$tag" \
    "$tag" "$sha" "$version" "$CHANGELOG" "$tag"

recovery() {
    echo "## CLI release ${tag} was dropped"
    echo ""
    echo "\`${MANIFEST}\` names \`${CLI_PACKAGE}\` ${version} at \`${sha}\` and \`${CHANGELOG}\` carries its entry, but no GitHub Release \`${tag}\` exists. release-please skipped the release and the run still reported success."
    echo ""
    echo "Recovery, in order:"
    echo ""
    echo "1. Create the release at this commit, with notes copied from the \`## [${version}]\` section of \`${CHANGELOG}\`:"
    echo "   \`gh release create ${tag} --target ${sha} --title ${tag} --notes-file <notes>\`"
    echo "2. Attach the GoReleaser assets: run this workflow via workflow_dispatch with \`cli_tag=${tag}\`. Until then \`scripts/install.sh\` 404s on ${tag} and the Homebrew tap is stale."
    echo "3. Relabel the merged release PR \`autorelease: pending\` -> \`autorelease: tagged\`. Until that label moves, every later release-please run aborts with \"There are untagged, merged release PRs outstanding\" and no component can cut a release PR."
    echo ""
    echo "Cause: a \`component\` declared for \`${CLI_PACKAGE}\` in release-please-config.json makes release-please refuse to build its release whenever the CLI is alone in the manifest release PR. \`scripts/check-release-please-sync.sh\` pins that it declares none."
}

recovery
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    recovery >>"$GITHUB_STEP_SUMMARY"
fi

exit 1
