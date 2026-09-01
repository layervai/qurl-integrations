#!/usr/bin/env bash
# Exercise scripts/verify-cli-release.sh against a fixture manifest and a stub
# `gh`. The verifier itself only ever runs on a push to main, after a release
# has already been made or dropped, so a defect in it ships green and surfaces
# exactly when it is needed — the same reason the validated-base resolver is
# tested here rather than trusted. Every branch is pinned below.
#
# Two properties matter as much as the exit codes: a lookup failure must not be
# reported as a dropped release (the recovery for a drop repairs nothing when
# the release is actually fine), and the tag it looks up must stay bare `v*`,
# never `cli-v*`.
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
verifier="$repo_root/scripts/verify-cli-release.sh"

# Run the verifier under dash when it is installed, not under whatever /bin/sh
# happens to be. The verifier is #!/bin/sh, and /bin/sh is dash on the runners
# but bash on a macOS developer's machine — a gap that already shipped one bug
# here: `${VAR:?msg}` exits 2 under dash and 1 under bash, so the missing-repo
# case passed locally and failed in CI. Preferring dash makes the local run
# faithful to the one that gates the merge.
verifier_sh='sh'
command -v dash >/dev/null 2>&1 && verifier_sh='dash'
tmp="$(mktemp -d)"

trap 'rm -rf "$tmp"' EXIT

export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_NOSYSTEM=1

# A git repo with an original release source and a later main commit. Recovery
# must use the explicit original source even when the verifier runs later.
fixture="$tmp/repo"
mkdir -p "$fixture"
git -C "$fixture" init -q -b main
git -C "$fixture" config user.name 'qURL release verifier test'
git -C "$fixture" config user.email 'qurl-release-verifier@example.invalid'
printf '%s\n' original >"$fixture/source-marker"
git -C "$fixture" add source-marker
git -C "$fixture" commit -qm 'test: original release source'
source_sha="$(git -C "$fixture" rev-parse HEAD)"
printf '%s\n' later >>"$fixture/source-marker"
git -C "$fixture" add source-marker
git -C "$fixture" commit -qm 'test: later main change'
head_sha="$(git -C "$fixture" rev-parse HEAD)"
git -C "$fixture" update-ref refs/remotes/origin/main "$head_sha"
unrelated_tree="$(git -C "$fixture" mktree </dev/null)"
unrelated_sha="$(printf '%s\n' 'test: unrelated commit' | git -C "$fixture" commit-tree "$unrelated_tree")"

write_manifest() {
  cat >"$fixture/.release-please-manifest.json" <<JSON
{
  "apps/slack": "0.3.0",
  "apps/cli": "$1"
}
JSON
}

# The stub records every invocation so the tests can assert both *what* was
# queried and *how many times*. It deliberately rejects the old REST by-tag
# route: GitHub returns 404 there for a real draft release, which is the
# production failure this regression test must preserve.
bindir="$tmp/bin"
mkdir -p "$bindir"
cat >"$bindir/gh" <<'STUB_EOF'
#!/bin/sh
printf '%s\n' "$*" >> "$GH_ARGV_OUT"
case "$*" in
  api\ repos/layervai/qurl-integrations/releases/tags/*)
    printf '%s\n' 'gh: Not Found (HTTP 404)' >&2
    exit 1
    ;;
  release\ view\ *)
    if [ -n "${GH_STUB_RELEASE_STDERR-}" ]; then
      printf '%s\n' "$GH_STUB_RELEASE_STDERR" >&2
    fi
    if [ "${GH_STUB_RELEASE_STATUS:-0}" != "0" ]; then
      exit "$GH_STUB_RELEASE_STATUS"
    fi
    if [ -n "${GH_STUB_RELEASE_JSON-}" ]; then
      printf '%s\n' "$GH_STUB_RELEASE_JSON"
      exit 0
    fi
    tag=${GH_STUB_RELEASE_TAG:-$3}
    target=${GH_STUB_RELEASE_TARGET:-$GH_STUB_TAG_COMMIT}
    draft=${GH_STUB_RELEASE_DRAFT:-true}
    printf '{"tagName":"%s","targetCommitish":"%s","isDraft":%s}\n' "$tag" "$target" "$draft"
    ;;
  api\ repos/layervai/qurl-integrations/commits/*)
    if [ -n "${GH_STUB_TAG_STDERR-}" ]; then
      printf '%s\n' "$GH_STUB_TAG_STDERR" >&2
    fi
    if [ "${GH_STUB_TAG_STATUS:-0}" != "0" ]; then
      exit "$GH_STUB_TAG_STATUS"
    fi
    printf '%s\n' "${GH_STUB_TAG_STDOUT:-$GH_STUB_TAG_COMMIT}"
    ;;
  *)
    printf '%s\n' "unexpected gh invocation: $*" >&2
    exit 2
    ;;
esac
STUB_EOF
chmod +x "$bindir/gh"

case_no=0
argv_out=""
summary_out=""

run_case() {
  local name="$1" expected_status="$2" expected_output="$3" forbidden_output="$4"
  shift 4

  case_no=$((case_no + 1))
  argv_out="$tmp/argv-$case_no"
  summary_out="$tmp/summary-$case_no"
  : >"$argv_out"
  : >"$summary_out"

  local status=0 log
  log="$(cd "$fixture" && env \
    PATH="$bindir:$PATH" \
    GH_ARGV_OUT="$argv_out" \
    GITHUB_REPOSITORY=layervai/qurl-integrations \
    GITHUB_SHA="$head_sha" \
    GITHUB_STEP_SUMMARY="$summary_out" \
    GH_STUB_TAG_COMMIT="$head_sha" \
    RELEASE_LOOKUP_ATTEMPTS=3 \
    RELEASE_LOOKUP_DELAY=0 \
    "$@" \
    "$verifier_sh" "$verifier" 2>&1)" || status=$?

  if [[ "$status" != "$expected_status" ]]; then
    printf '%s: expected exit %s, got %s\n%s\n' "$name" "$expected_status" "$status" "$log" >&2
    exit 1
  fi

  if [[ -n "$expected_output" && "$log" != *"$expected_output"* ]]; then
    printf '%s: expected output to contain %q\n%s\n' "$name" "$expected_output" "$log" >&2
    exit 1
  fi

  if [[ -n "$forbidden_output" && "$log" == *"$forbidden_output"* ]]; then
    printf '%s: expected output NOT to contain %q\n%s\n' "$name" "$forbidden_output" "$log" >&2
    exit 1
  fi
}

expect_gh_calls() {
  local name="$1" want="$2" got
  got="$(wc -l <"$argv_out" | tr -d ' ')"
  if [[ "$got" != "$want" ]]; then
    printf '%s: expected %s gh invocation(s), got %s:\n%s\n' "$name" "$want" "$got" "$(cat "$argv_out")" >&2
    exit 1
  fi
}

expect_argv() {
  local name="$1" needle="$2"
  if [[ "$(cat "$argv_out")" != *"$needle"* ]]; then
    printf '%s: expected the gh query to contain %q, got: %s\n' "$name" "$needle" "$(cat "$argv_out")" >&2
    exit 1
  fi
}

refute_argv() {
  local name="$1" needle="$2"
  if [[ "$(cat "$argv_out")" == *"$needle"* ]]; then
    printf '%s: expected the gh query NOT to contain %q, got: %s\n' "$name" "$needle" "$(cat "$argv_out")" >&2
    exit 1
  fi
}

expect_summary() {
  local name="$1" needle="$2"
  if [[ "$(cat "$summary_out")" != *"$needle"* ]]; then
    printf '%s: expected the job summary to contain %q, got:\n%s\n' "$name" "$needle" "$(cat "$summary_out")" >&2
    exit 1
  fi
}

refute_summary() {
  local name="$1" needle="$2"
  if [[ "$(cat "$summary_out")" == *"$needle"* ]]; then
    printf '%s: expected the job summary NOT to contain %q, got:\n%s\n' "$name" "$needle" "$(cat "$summary_out")" >&2
    exit 1
  fi
}

expect_summary_order() {
  local name="$1"
  shift
  local previous=0 needle line
  for needle in "$@"; do
    line="$(grep -nF -- "$needle" "$summary_out" | head -1 | cut -d: -f1 || true)"
    if [[ -z "$line" || "$line" -le "$previous" ]]; then
      printf '%s: expected summary item %q after line %s, got:\n%s\n' \
        "$name" "$needle" "$previous" "$(cat "$summary_out")" >&2
      exit 1
    fi
    previous="$line"
  done
}

# --- a tagged draft exists: the release-please handoff, and the old REST
#     by-tag route must never be used because it returns 404 for this release

write_manifest 1.4.0
run_case released 0 'apps/cli 1.4.0 exists as v1.4.0' '::error::'
expect_gh_calls released 2
expect_argv released 'release view v1.4.0 --repo layervai/qurl-integrations --json tagName,targetCommitish,isDraft'
expect_argv released 'api repos/layervai/qurl-integrations/commits/v1.4.0 --jq .sha'
refute_argv released 'releases/tags/'
# The CLI is the one component tagged without a component prefix. A guard that
# looked up cli-v1.4.0 would report a drop on every successful release.
refute_argv released 'cli-v1.4.0'

# The tag follows the manifest rather than a hardcoded version.
write_manifest 2.0.0
run_case released-other-version 0 'apps/cli 2.0.0 exists as v2.0.0' '::error::'
expect_argv released-other-version 'release view v2.0.0'

# A public release binds through the exact tag commit. targetCommitish is not
# used after publication and GitHub can return the branch name rather than the
# SHA that the draft gate required.
write_manifest 2.0.0
run_case released-public 0 'apps/cli 2.0.0 exists as v2.0.0' '::error::' \
  GH_STUB_RELEASE_DRAFT=false GH_STUB_RELEASE_TARGET=main

# Successful gh diagnostics stay on their captured stderr streams. They must
# not corrupt either the release JSON or the tag SHA read from stdout.
write_manifest 2.0.0
run_case released-with-warnings 0 'apps/cli 2.0.0 exists as v2.0.0' 'successful-gh-warning' \
  GH_STUB_RELEASE_STDERR=successful-gh-warning GH_STUB_TAG_STDERR=successful-gh-warning

# Exact tag, target commit, and boolean draft state are all fail-closed. These
# are malformed releases, not absent releases, so no recovery instructions may
# claim that release-please dropped them.
run_case wrong-release-tag 1 \
  "::error::Could not verify the apps/cli release: release tag mismatch: observed 'v9.9.9'; expected 'v2.0.0'. This is invalid release metadata, not a dropped release." \
  'release-please dropped the apps/cli release' \
  GH_STUB_RELEASE_TAG=v9.9.9
expect_gh_calls wrong-release-tag 2

run_case wrong-release-target 1 \
  "::error::Could not verify the apps/cli release: draft release target mismatch: observed '0000000000000000000000000000000000000000'; expected tag commit '$head_sha'. This is invalid release metadata, not a dropped release." \
  'release-please dropped the apps/cli release' \
  GH_STUB_RELEASE_TARGET=0000000000000000000000000000000000000000
expect_gh_calls wrong-release-target 2

run_case nonexact-draft-target 1 \
  "::error::Could not verify the apps/cli release: draft release target mismatch: observed 'main'; expected exact tag commit '$head_sha'. This is invalid release metadata, not a dropped release." \
  'release-please dropped the apps/cli release' \
  GH_STUB_RELEASE_TARGET=main
expect_gh_calls nonexact-draft-target 2

run_case missing-draft-state 1 \
  '::error::Could not verify the apps/cli release: release draft state has type NoneType; expected boolean true or false. This is invalid release metadata, not a dropped release.' \
  'release-please dropped the apps/cli release' \
  GH_STUB_RELEASE_DRAFT=null
expect_gh_calls missing-draft-state 2

run_case malformed-release-json 1 \
  '::error::Could not verify the apps/cli release: release metadata is not valid JSON:' \
  'release-please dropped the apps/cli release' \
  GH_STUB_RELEASE_JSON='{not-json'
expect_gh_calls malformed-release-json 2

# A release that exists without a resolvable tag is not a dropped release. It
# is a distinct fail-closed integrity failure, even when the tag lookup is 404.
run_case missing-release-tag 1 \
  '::error::Could not verify the apps/cli release: GitHub Release v2.0.0 exists, but tag v2.0.0 cannot be verified:' \
  'release-please dropped the apps/cli release' \
  GH_STUB_TAG_STATUS=1 GH_STUB_TAG_STDERR='gh: Not Found (HTTP 404)'
expect_gh_calls missing-release-tag 2

run_case tag-lookup-failure 1 \
  'This is a tag verification failure, not a dropped release.' \
  'release-please dropped the apps/cli release' \
  GH_STUB_TAG_STATUS=1 GH_STUB_TAG_STDERR='gh: Bad gateway (HTTP 502)'
# One release lookup plus the three bounded tag-commit attempts.
expect_gh_calls tag-lookup-failure 4

run_case invalid-tag-output 1 \
  'tag lookup returned invalid commit main; expected one 40-character lowercase hexadecimal commit' \
  'release-please dropped the apps/cli release' \
  GH_STUB_TAG_STDOUT=main
expect_gh_calls invalid-tag-output 2

# --- the release is absent: no tag command without an explicit, verified
#     original source; a delayed recovery must not use the later current HEAD

write_manifest 1.4.0
run_case dropped-without-source 1 'No recovery command was generated: CLI_RELEASE_SOURCE_SHA is unset' 'git tag v1.4.0' \
  GH_STUB_RELEASE_STATUS=1 GH_STUB_RELEASE_STDERR='release not found'
# A 404 is an answer, not a transient failure: retrying it only delays the
# report.
expect_gh_calls dropped-without-source 1
expect_summary dropped-without-source 'CLI release v1.4.0 was dropped'
expect_summary dropped-without-source "Set \`CLI_RELEASE_SOURCE_SHA\` to the original 40-character source commit"
refute_summary dropped-without-source 'git tag v1.4.0'

run_case delayed-recovery 1 '::error::release-please dropped the apps/cli release' "$head_sha" \
  CLI_RELEASE_SOURCE_SHA="$source_sha" \
  GH_STUB_RELEASE_STATUS=1 GH_STUB_RELEASE_STDERR='release not found'
expect_gh_calls delayed-recovery 1
expect_summary delayed-recovery 'CLI release v1.4.0 was dropped'
expect_summary delayed-recovery 'git tag v1.4.0 '"$source_sha"
refute_summary delayed-recovery 'git tag v1.4.0 '"$head_sha"
expect_summary delayed-recovery 'git push origin refs/tags/v1.4.0'
expect_summary delayed-recovery 'gh release create v1.4.0 --verify-tag --target '"$source_sha"' --title v1.4.0 --notes-file <notes> --draft'
expect_summary_order delayed-recovery \
  '1. Create the exact tag' \
  '2. Push the exact tag' \
  '3. Create the draft release' \
  'gh release create v1.4.0 --verify-tag' \
  '4. Attach the GoReleaser assets' \
  '5. Relabel the merged release PR'
expect_summary delayed-recovery 'cli_tag=v1.4.0'
expect_summary delayed-recovery 'autorelease: pending`'
expect_summary delayed-recovery 'untagged, merged release PRs outstanding'

run_case malformed-recovery-source 1 'CLI_RELEASE_SOURCE_SHA must be one 40-character lowercase hexadecimal commit' 'git tag v1.4.0' \
  CLI_RELEASE_SOURCE_SHA=main \
  GH_STUB_RELEASE_STATUS=1 GH_STUB_RELEASE_STDERR='release not found'
refute_summary malformed-recovery-source 'git tag v1.4.0'

run_case missing-recovery-source 1 'does not resolve to that exact commit in this checkout' 'git tag v1.4.0' \
  CLI_RELEASE_SOURCE_SHA=0000000000000000000000000000000000000000 \
  GH_STUB_RELEASE_STATUS=1 GH_STUB_RELEASE_STDERR='release not found'
refute_summary missing-recovery-source 'git tag v1.4.0'

run_case unrelated-recovery-source 1 'is not an ancestor of origin/main' 'git tag v1.4.0' \
  CLI_RELEASE_SOURCE_SHA="$unrelated_sha" \
  GH_STUB_RELEASE_STATUS=1 GH_STUB_RELEASE_STDERR='release not found'
refute_summary unrelated-recovery-source 'git tag v1.4.0'

# --- the lookup itself failed: fail loudly, but do NOT call it a drop

run_case lookup-failure 1 '::error::Could not verify the apps/cli release' 'dropped the apps/cli release' \
  GH_STUB_RELEASE_STATUS=1 GH_STUB_RELEASE_STDERR='gh: Bad gateway (HTTP 502)'
expect_gh_calls lookup-failure 3

# --- a broken manifest is not a dropped release either, and must never reach
#     the API to be mistaken for one

write_manifest 1.4.0
python3 - "$fixture/.release-please-manifest.json" <<'PY'
import json
import sys

path = sys.argv[1]
with open(path) as f:
    manifest = json.load(f)
del manifest["apps/cli"]
with open(path, "w") as f:
    json.dump(manifest, f)
PY
run_case manifest-without-cli 1 'has no usable apps/cli version' '::error::'
expect_gh_calls manifest-without-cli 0

printf '{\n  "apps/cli": "not-a-version"\n}\n' >"$fixture/.release-please-manifest.json"
run_case manifest-bad-version 1 'has no usable apps/cli version' '::error::'
expect_gh_calls manifest-bad-version 0

rm -f "$fixture/.release-please-manifest.json"
run_case manifest-absent 1 'needs the repository checked out' '::error::'
expect_gh_calls manifest-absent 0

# --- harness misconfiguration must go red rather than skip the check

write_manifest 1.4.0
run_case missing-repository 1 'GITHUB_REPOSITORY must be set' '' GITHUB_REPOSITORY=
expect_gh_calls missing-repository 0

printf 'CLI release verifier tests passed (%d cases).\n' "$case_no"
