#!/usr/bin/env bash
# Exercise scripts/resolve-validated-base.sh against a fixture repo and a stub
# `gh`. Every branch matters: the resolver decides whether app CI runs at all
# on main, so a silent wrong answer reproduces #1022 rather than fixing it.
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
resolver="$repo_root/scripts/resolve-validated-base.sh"
tmp="$(mktemp -d)"

trap 'rm -rf "$tmp"' EXIT

export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.invalid
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.invalid

# base -> mid -> head on main, plus `side` branching off base so one fixture
# SHA is a real commit that is nonetheless not an ancestor of HEAD.
fixture="$tmp/repo"
mkdir -p "$fixture"
git -C "$fixture" init -q -b main
git -C "$fixture" commit -q --allow-empty -m base
base_sha="$(git -C "$fixture" rev-parse HEAD)"
git -C "$fixture" commit -q --allow-empty -m mid
git -C "$fixture" commit -q --allow-empty -m head
head_sha="$(git -C "$fixture" rev-parse HEAD)"
git -C "$fixture" checkout -q -b side "$base_sha"
git -C "$fixture" commit -q --allow-empty -m side
side_sha="$(git -C "$fixture" rev-parse HEAD)"
git -C "$fixture" checkout -q main

absent_sha="0123456789abcdef0123456789abcdef01234567"

bindir="$tmp/bin"
mkdir -p "$bindir"
cat > "$bindir/gh" <<'STUB_EOF'
#!/bin/sh
printf '%s\n' "$*" > "$GH_ARGV_OUT"
if [ "${GH_STUB_EXIT:-0}" != "0" ]; then
  exit "$GH_STUB_EXIT"
fi
printf '%s\n' "${GH_STUB_STDOUT-}"
exit 0
STUB_EOF
chmod +x "$bindir/gh"

case_no=0
argv_out=""

run_case() {
  local name="$1" expected_status="$2" expected_sha="$3" expected_force="$4" expected_log="$5"
  shift 5

  case_no=$((case_no + 1))
  local out="$tmp/output-$case_no"
  argv_out="$tmp/argv-$case_no"
  : > "$out"

  local status=0 log
  log="$(cd "$fixture" && env \
    PATH="$bindir:$PATH" \
    GITHUB_OUTPUT="$out" \
    GH_ARGV_OUT="$argv_out" \
    GITHUB_REPOSITORY=layervai/qurl-integrations \
    GITHUB_REF_NAME=main \
    GITHUB_SHA="$head_sha" \
    GITHUB_WORKFLOW_REF='layervai/qurl-integrations/.github/workflows/discord.yml@refs/heads/main' \
    GH_STUB_STDOUT="$base_sha" \
    "$@" \
    "$resolver" 2>&1)" || status=$?

  if [[ "$status" != "$expected_status" ]]; then
    printf '%s: expected exit %s, got %s\n%s\n' "$name" "$expected_status" "$status" "$log" >&2
    exit 1
  fi

  if [[ -n "$expected_log" && "$log" != *"$expected_log"* ]]; then
    printf '%s: expected log to contain %q\n%s\n' "$name" "$expected_log" "$log" >&2
    exit 1
  fi

  # A non-zero exit means the step failed before writing step outputs.
  [[ "$expected_status" == 0 ]] || return 0

  local got_sha got_force
  got_sha="$(sed -n 's/^sha=//p' "$out")"
  got_force="$(sed -n 's/^force=//p' "$out")"

  if [[ "$got_sha" != "$expected_sha" ]]; then
    printf '%s: expected sha=%q, got %q\n' "$name" "$expected_sha" "$got_sha" >&2
    exit 1
  fi

  if [[ "$got_force" != "$expected_force" ]]; then
    printf '%s: expected force=%q, got %q\n' "$name" "$expected_force" "$got_force" >&2
    exit 1
  fi
}

# --- the narrow diff this whole change exists to produce

run_case last-validated-ancestor 0 "$base_sha" false 'base='

# The workflow file name comes from GITHUB_WORKFLOW_REF, not a hardcoded
# per-caller string. A wrong name would silently query another workflow's
# history and hand back a base that this workflow never validated.
if [[ "$(cat "$argv_out")" != *"repos/layervai/qurl-integrations/actions/workflows/discord.yml/runs"* ]]; then
  printf 'expected the API query to target discord.yml, got: %s\n' "$(cat "$argv_out")" >&2
  exit 1
fi
if [[ "$(cat "$argv_out")" != *"status=success"* || "$(cat "$argv_out")" != *"event=push"* ]]; then
  printf 'expected the API query to filter on successful push runs, got: %s\n' "$(cat "$argv_out")" >&2
  exit 1
fi

run_case workflow-file-override 0 "$base_sha" false '' WORKFLOW_FILE=slack.yml
if [[ "$(cat "$argv_out")" != *"/workflows/slack.yml/runs"* ]]; then
  printf 'WORKFLOW_FILE override ignored: %s\n' "$(cat "$argv_out")" >&2
  exit 1
fi

# --- everything unknown must fail closed (force=true), never narrow silently

run_case api-error 0 '' true 'gh exited 4' GH_STUB_EXIT=4
run_case no-successful-run 0 '' true 'no successful discord.yml push run' GH_STUB_STDOUT=
run_case unusable-head-sha 0 '' true 'unusable head SHA' GH_STUB_STDOUT=not-a-sha
run_case base-is-head 0 '' true "own SHA (${head_sha:0:7})" GH_STUB_STDOUT="$head_sha"
run_case base-absent-locally 0 '' true 'not present locally' GH_STUB_STDOUT="$absent_sha"
run_case base-not-ancestor 0 '' true 'not an ancestor' GH_STUB_STDOUT="$side_sha"
run_case missing-repository 0 '' true 'GITHUB_REPOSITORY is unset' GITHUB_REPOSITORY=
run_case missing-ref-name 0 '' true 'GITHUB_REF_NAME is unset' GITHUB_REF_NAME=
run_case missing-head-sha 0 '' true 'GITHUB_SHA is not a commit SHA' GITHUB_SHA=
run_case undrivable-workflow-ref 0 '' true 'could not derive a workflow file name' GITHUB_WORKFLOW_REF=

# --- a missing GITHUB_OUTPUT is a harness bug, not a fail-closed case: there
#     is nowhere to report `force`, so the step must go red instead of letting
#     the caller read an empty output as "nothing changed".
run_case missing-github-output 1 '' '' 'GITHUB_OUTPUT must be set' GITHUB_OUTPUT=

printf 'validated-base resolver tests passed (%d cases).\n' "$case_no"
