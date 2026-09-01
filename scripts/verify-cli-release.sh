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

# Spelled out rather than `: "${GITHUB_REPOSITORY:?...}"`: POSIX leaves the
# exit status of the :? expansion unspecified, and it differs across the shells
# this runs under — dash (the runner's /bin/sh) exits 2 where bash exits 1. An
# explicit check keeps every failure here exit 1, which is what the harness and
# a reader both expect.
if [ -z "${GITHUB_REPOSITORY:-}" ]; then
    echo "Error: GITHUB_REPOSITORY must be set ($0 queries the releases of that repository)" >&2
    exit 1
fi

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
metadata_error=''
attempt=1

while [ "$attempt" -le "$attempts" ]; do
    lookup_status=0
    release_json="$(gh release view "$tag" --repo "$GITHUB_REPOSITORY" \
        --json tagName,targetCommitish,isDraft 2>&1)" ||
        lookup_status=$?

    if [ "$lookup_status" -eq 0 ]; then
        tag_status=0
        tag_commit="$(gh api "repos/${GITHUB_REPOSITORY}/commits/${tag}" --jq .sha 2>&1)" ||
            tag_status=$?
        if [ "$tag_status" -eq 0 ]; then
            metadata_status=0
            metadata_error="$(
                RELEASE_JSON="$release_json" python3 - "$tag" "$tag_commit" <<'PY'
import json
import os
import re
import sys

expected_tag, expected_target = sys.argv[1:]
try:
    release = json.loads(os.environ["RELEASE_JSON"])
except (KeyError, json.JSONDecodeError) as exc:
    raise SystemExit(f"release metadata is not valid JSON: {exc}") from exc

if release.get("tagName") != expected_tag:
    raise SystemExit("release metadata does not name the exact CLI tag")
target = release.get("targetCommitish")
if not isinstance(target, str) or not re.fullmatch(r"[0-9a-f]{40}", target):
    raise SystemExit("release metadata does not name one exact target commit")
if target != expected_target:
    raise SystemExit("release target does not match the CLI tag commit")
if type(release.get("isDraft")) is not bool:
    raise SystemExit("release metadata has no exact draft state")
PY
            )" 2>&1 || metadata_status=$?
            if [ "$metadata_status" -eq 0 ]; then
                found=yes
                break
            fi
            # Exact metadata is a deterministic contract failure. Retrying it
            # would only delay the fail-closed result.
            break
        fi
        lookup_error="resolving ${tag} to its exact commit failed: ${tag_commit}"
    else
        lookup_error="$release_json"
    fi

    # `gh release view` is draft-aware. It reports a missing release as
    # "release not found"; keep the HTTP form for older gh versions. Either is
    # an answer rather than a transient lookup error, so do not retry it.
    case "$lookup_error" in
    *'release not found'*|*'HTTP 404'*)
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
    printf '%s %s exists as %s; nothing was dropped.\n' "$CLI_PACKAGE" "$version" "$tag"
    exit 0
fi

if [ -n "$metadata_error" ]; then
    printf '::error::Could not verify the %s release: %s. This is invalid release metadata, not a dropped release.\n' \
        "$CLI_PACKAGE" "$metadata_error"
    exit 1
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
    echo "   \`gh release create ${tag} --target ${sha} --title ${tag} --notes-file <notes> --draft\`"
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
