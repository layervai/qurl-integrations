#!/usr/bin/env bash
# Exercise scripts/verify-cli-release.sh against a fixture manifest and a stub
# `gh`. The verifier itself only ever runs on a push to main, after a release
# has already been made or dropped, so a defect in it ships green and surfaces
# exactly when it is needed — the same reason the Claude review budget report
# is tested here rather than trusted. Every branch is pinned below.
#
# Two properties matter as much as the exit codes: a lookup failure must not be
# reported as a dropped release (the recovery for a drop repairs nothing when
# the release is actually fine), and the tag it looks up must stay bare `v*`,
# never `cli-v*`.
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
verifier="$repo_root/scripts/verify-cli-release.sh"
tmp="$(mktemp -d)"

trap 'rm -rf "$tmp"' EXIT

export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_NOSYSTEM=1

# A git repo, because the verifier resolves its own root before reading the
# manifest. Nothing here needs history.
fixture="$tmp/repo"
mkdir -p "$fixture"
git -C "$fixture" init -q -b main

write_manifest() {
  cat >"$fixture/.release-please-manifest.json" <<JSON
{
  "apps/slack": "0.3.0",
  "apps/cli": "$1"
}
JSON
}

# The stub records every invocation so the tests can assert both *what* was
# queried and *how many times* — a 404 must not be retried, a 502 must be.
bindir="$tmp/bin"
mkdir -p "$bindir"
cat >"$bindir/gh" <<'STUB_EOF'
#!/bin/sh
printf '%s\n' "$*" >> "$GH_ARGV_OUT"
if [ "${GH_STUB_STATUS:-0}" != "0" ]; then
  printf '%s\n' "${GH_STUB_STDERR-}" >&2
  exit "$GH_STUB_STATUS"
fi
exit 0
STUB_EOF
chmod +x "$bindir/gh"

head_sha=37bfa43d0000000000000000000000000000beef
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
    RELEASE_LOOKUP_ATTEMPTS=3 \
    RELEASE_LOOKUP_DELAY=0 \
    "$@" \
    "$verifier" 2>&1)" || status=$?

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

# --- the release exists: the common case, and it must say nothing alarming

write_manifest 1.4.0
run_case released 0 'apps/cli 1.4.0 is released as v1.4.0' '::error::'
expect_gh_calls released 1
expect_argv released 'repos/layervai/qurl-integrations/releases/tags/v1.4.0'
# The CLI is the one component tagged without a component prefix. A guard that
# looked up cli-v1.4.0 would report a drop on every successful release.
refute_argv released 'cli-v1.4.0'

# The tag follows the manifest rather than a hardcoded version.
write_manifest 2.0.0
run_case released-other-version 0 'apps/cli 2.0.0 is released as v2.0.0' '::error::'
expect_argv released-other-version 'releases/tags/v2.0.0'

# --- the release is absent: the drop, with the whole recovery attached

write_manifest 1.4.0
run_case dropped 1 '::error::release-please dropped the apps/cli release' 'Could not verify' \
  GH_STUB_STATUS=1 GH_STUB_STDERR='gh: Not Found (HTTP 404)'
# A 404 is an answer, not a transient failure: retrying it only delays the
# report.
expect_gh_calls dropped 1
expect_summary dropped 'CLI release v1.4.0 was dropped'
expect_summary dropped 'gh release create v1.4.0 --target '"$head_sha"
expect_summary dropped 'cli_tag=v1.4.0'
expect_summary dropped 'autorelease: pending`'
expect_summary dropped 'untagged, merged release PRs outstanding'

# --- the lookup itself failed: fail loudly, but do NOT call it a drop

run_case lookup-failure 1 '::error::Could not verify the apps/cli release' 'dropped the apps/cli release' \
  GH_STUB_STATUS=1 GH_STUB_STDERR='gh: Bad gateway (HTTP 502)'
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
