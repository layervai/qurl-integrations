#!/usr/bin/env bash
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
validator="$repo_root/scripts/validate-github-actions-pins.sh"
sha="0123456789abcdef0123456789abcdef01234567"
tmp_parent="$(mktemp -d)"

trap 'rm -rf "$tmp_parent"' EXIT

case_no=0

write_file() {
  local path="$1"
  local content="$2"
  mkdir -p "$(dirname "$path")"
  printf '%s\n' "$content" > "$path"
}

run_case() {
  local name="$1"
  local expected_status="$2"
  local expected_output="$3"
  shift 3

  case_no=$((case_no + 1))
  local workdir="$tmp_parent/$case_no-$name"
  mkdir -p "$workdir"
  # The validator only needs a repo root; fixtures intentionally do not commit.
  git -C "$workdir" init -q

  while (( $# )); do
    local path="$1"
    local content="$2"
    shift 2
    write_file "$workdir/$path" "$content"
  done

  set +e
  local output
  output="$(cd "$workdir" && "$validator" 2>&1)"
  local status="$?"
  set -e

  if [[ "$status" != "$expected_status" ]]; then
    printf '%s: expected exit %s, got %s\n%s\n' "$name" "$expected_status" "$status" "$output" >&2
    exit 1
  fi

  if [[ -n "$expected_output" && "$output" != *"$expected_output"* ]]; then
    printf '%s: expected output to contain %q\n%s\n' "$name" "$expected_output" "$output" >&2
    exit 1
  fi
}

run_case valid-workflow 0 "GitHub Actions pins are SHA-pinned" \
  ".github/workflows/test.yml" "jobs:
  test:
    steps:
      - uses: actions/checkout@$sha # v1.2.3"

run_case quoted-ref 0 "GitHub Actions pins are SHA-pinned" \
  ".github/workflows/test.yml" "jobs:
  test:
    steps:
      - uses: \"actions/checkout@$sha\" # v1.2.3-rc.1+build.5"

run_case local-action 0 "GitHub Actions pins are SHA-pinned" \
  ".github/workflows/test.yml" "jobs:
  test:
    steps:
      - uses: ./local-action"

run_case reusable-workflow 0 "GitHub Actions pins are SHA-pinned" \
  ".github/workflows/test.yml" "jobs:
  test:
    uses: octo-org/reusable/.github/workflows/build.yml@$sha # v1.2.3"

run_case composite-action-valid 0 "GitHub Actions pins are SHA-pinned" \
  ".github/actions/nested/demo/action.yml" "runs:
  using: composite
  steps:
    - uses: actions/checkout@$sha # v1.2.3"

run_case composite-action-invalid 1 "external action must be pinned" \
  ".github/actions/demo/action.yml" "runs:
  using: composite
  steps:
    - uses: actions/checkout@v4 # v4.0.0"

run_case missing-at-ref 1 "external action is missing an @ ref" \
  ".github/workflows/test.yml" "jobs:
  test:
    steps:
      - uses: actions/checkout # v1.2.3"

run_case missing-version-comment 1 "SHA pin must include an exact version tag comment" \
  ".github/workflows/test.yml" "jobs:
  test:
    steps:
      - uses: actions/checkout@$sha"

run_case invalid-version-comment 1 "SHA pin must include an exact version tag comment" \
  ".github/workflows/test.yml" "jobs:
  test:
    steps:
      - uses: actions/checkout@$sha # v1.2.3.4"

run_case docker-action 1 "docker:// actions are not allowed" \
  ".github/workflows/test.yml" "jobs:
  test:
    steps:
      - uses: docker://alpine:3.20 # v1.2.3"

run_case no-action-files 2 "no workflow or composite-action files found"

printf 'GitHub Actions pin validator tests passed.\n'
