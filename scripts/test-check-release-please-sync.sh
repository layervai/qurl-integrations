#!/usr/bin/env bash
# Exercise scripts/check-release-please-sync.sh against crafted config/manifest
# fixtures. Each invariant that script holds is a rule nothing else checks
# before merge, so an assertion that silently stopped biting would restore the
# exact hole it was added to close — and all three are the kind that pass by
# accident when the repo is already correct. Every branch is pinned below.
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
checker="$repo_root/scripts/check-release-please-sync.sh"

# The checker is #!/bin/sh; /bin/sh is dash on the runners and bash on macOS.
# Prefer dash so a local run matches the one that gates the merge (see the same
# note in test-verify-cli-release.sh — that gap already shipped one bug).
checker_sh='sh'
command -v dash >/dev/null 2>&1 && checker_sh='dash'

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_NOSYSTEM=1

fixture="$tmp/repo"
mkdir -p "$fixture"
git -C "$fixture" init -q -b main

# write_fixture <cli-extra-json> [extra-package-json]
# The baseline is the real shape: one bare-tagged CLI plus a component-tagged
# sibling, keys mirrored into the manifest.
write_fixture() {
  local cli_extra="$1" extra_pkg="${2-}"
  cat >"$fixture/release-please-config.json" <<JSON
{
  "packages": {
    "apps/slack": {
      "release-type": "go",
      "component": "slack"${extra_pkg}
    },
    "apps/cli": {
      "release-type": "go",
      "include-component-in-tag": false${cli_extra}
    }
  }
}
JSON
  cat >"$fixture/.release-please-manifest.json" <<'JSON'
{
  "apps/slack": "0.3.0",
  "apps/cli": "1.4.0"
}
JSON
}

case_no=0

run_case() {
  local name="$1" expected_status="$2" expected_output="$3"
  case_no=$((case_no + 1))

  local status=0 log
  log="$(cd "$fixture" && "$checker_sh" "$checker" 2>&1)" || status=$?

  if [[ "$status" != "$expected_status" ]]; then
    printf '%s: expected exit %s, got %s\n%s\n' "$name" "$expected_status" "$status" "$log" >&2
    exit 1
  fi
  if [[ -n "$expected_output" && "$log" != *"$expected_output"* ]]; then
    printf '%s: expected output to contain %q\n%s\n' "$name" "$expected_output" "$log" >&2
    exit 1
  fi
}

# --- the shape the repo actually has: every invariant satisfied

write_fixture ""
run_case clean 0 'apps/cli declares no component'

# --- the invariant this PR added: a component on the bare-tagged CLI is what
#     made release-please skip its release while still exiting 0

write_fixture ',
      "component": "cli"'
run_case cli-component 1 'apps/cli must not declare a component'

# --- the two pre-existing invariants, which had no negative test either

write_fixture "" ',
      "include-component-in-tag": false'
run_case second-bare-tag 1 'bare v* tags are reserved to apps/cli'

write_fixture ""
python3 - "$fixture/.release-please-manifest.json" <<'PY'
import json
import sys

path = sys.argv[1]
with open(path) as f:
    manifest = json.load(f)
manifest["apps/teams"] = "0.1.0"
with open(path, "w") as f:
    json.dump(manifest, f)
PY
run_case key-drift 1 'key drift'

printf 'release-please sync check tests passed (%d cases).\n' "$case_no"
