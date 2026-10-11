#!/usr/bin/env bash
# Exercise scripts/prune-superseded-cli-drafts.sh against a stub `gh`. The
# script only ever runs after a CLI release has been published, and what it
# does cannot be undone, so a defect in it ships green and surfaces as a
# deleted release. Every selection rule and every refusal is pinned below.
#
# What matters most is which release IDs reach DELETE: each case asserts the
# exact list, so a rule that widens is caught as surely as one that stops
# selecting.
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
pruner="$repo_root/scripts/prune-superseded-cli-drafts.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# The stub records every invocation. The list route prints GH_STUB_RELEASES as
# is: one compact object per release, the shape the script's own --jq filter
# produces, which a stub cannot evaluate.
bindir="$tmp/bin"
mkdir -p "$bindir"
cat >"$bindir/gh" <<'STUB_EOF'
#!/bin/sh
printf '%s\n' "$*" >> "$GH_ARGV_OUT"
case "$*" in
  release\ view\ *)
    [ "${GH_STUB_VIEW_STATUS:-0}" = "0" ] || exit "$GH_STUB_VIEW_STATUS"
    printf '%s\n' "${GH_STUB_PUBLISHED_DRAFT-false}"
    ;;
  api\ --paginate\ repos/layervai/qurl-integrations/releases\?per_page=100\ *)
    [ "${GH_STUB_LIST_STATUS:-0}" = "0" ] || exit "$GH_STUB_LIST_STATUS"
    printf '%s' "${GH_STUB_RELEASES-}"
    ;;
  api\ --method\ DELETE\ repos/layervai/qurl-integrations/releases/*)
    case " ${GH_STUB_DELETE_FAILS-} " in
      *" ${4##*/} "*) exit 1 ;;
    esac
    ;;
  *)
    printf '%s\n' "unexpected gh invocation: $*" >&2
    exit 2
    ;;
esac
STUB_EOF
chmod +x "$bindir/gh"

release() { # id tag draft assets
  printf '{"id":%s,"tag_name":"%s","draft":%s,"assets":%s}\n' "$1" "$2" "$3" "$4"
}

case_no=0
failures=0
argv_out=""
output=""
summary_out=""

# run_case <name> <cli-tag> <want-exit> <want-deleted-ids> <want-output-fragment> [VAR=value ...]
run_case() {
  local name="$1" cli_tag="$2" want_status="$3" want_deleted="$4" want_fragment="$5"
  shift 5
  case_no=$((case_no + 1))
  argv_out="$tmp/argv-$case_no"
  summary_out="$tmp/summary-$case_no"
  : >"$argv_out"
  : >"$summary_out"
  local status=0
  output="$(env PATH="$bindir:$PATH" GH_ARGV_OUT="$argv_out" GH_TOKEN=stub \
    GITHUB_REPOSITORY=layervai/qurl-integrations GITHUB_STEP_SUMMARY="$summary_out" \
    CLI_TAG="$cli_tag" "$@" "$pruner" 2>&1)" || status=$?
  local deleted
  deleted="$(sed -n 's|^api --method DELETE repos/layervai/qurl-integrations/releases/||p' "$argv_out" | tr '\n' ' ')"
  deleted="${deleted% }"
  local ok=1
  [[ "$status" == "$want_status" ]] || ok=0
  [[ "$deleted" == "$want_deleted" ]] || ok=0
  [[ "$output" == *"$want_fragment"* ]] || ok=0
  if [[ "$ok" == 1 ]]; then
    echo "ok   $name"
  else
    failures=$((failures + 1))
    echo "FAIL $name: exit $status (want $want_status); deleted '$deleted' (want '$want_deleted'); want output containing '$want_fragment'"
    printf '%s\n' "$output" | sed 's/^/     | /'
  fi
}

expect_summary() { # name fragment
  if ! grep -qF -- "$2" "$summary_out"; then
    failures=$((failures + 1))
    echo "FAIL $1: step summary does not contain '$2'"
  fi
}

expect_no_list() { # name — the refusal came before the release list was read
  if grep -q '^api --paginate' "$argv_out"; then
    failures=$((failures + 1))
    echo "FAIL $1: the release list was read after a refusal"
  fi
}

# --- the shape this exists for: an empty draft below the published version,
# among releases of every other kind that must survive.
mixed="$(
  release 10 v3.5.0 false 20
  release 11 v3.4.1 false 20
  release 12 v3.4.0 true 0
  release 13 slack-v0.6.1 false 0
  release 14 v2.5.2 true 0
)"
run_case superseded-drafts v3.5.0 0 '12 14' \
  'Deleted the empty draft release v3.4.0 (id 12), superseded by v3.5.0; the tag v3.4.0 is kept.' \
  GH_STUB_RELEASES="$mixed"
expect_summary superseded-drafts "- \`v3.4.0\`"
expect_summary superseded-drafts "- \`v2.5.2\`"

# The published release is read by tag through the draft-aware route, and
# deletion goes by release ID: no call may name a tag for deletion.
grep -qxF 'release view v3.5.0 --repo layervai/qurl-integrations --json isDraft --jq .isDraft' "$argv_out" || {
  failures=$((failures + 1))
  echo "FAIL superseded-drafts: the published release was not read by its exact tag"
}
if grep -qE 'release delete|cleanup-tag|git/refs' "$argv_out"; then
  failures=$((failures + 1))
  echo "FAIL superseded-drafts: a call could delete a tag"
fi

# --- each selection rule alone: one release that fails exactly one rule.
run_case draft-with-assets-kept v3.5.0 0 '' 'No draft CLI release is superseded by v3.5.0.' \
  GH_STUB_RELEASES="$(release 20 v3.4.0 true 1)"
run_case published-lower-kept v3.5.0 0 '' 'No draft CLI release is superseded' \
  GH_STUB_RELEASES="$(release 21 v3.4.0 false 0)"
run_case higher-draft-kept v3.5.0 0 '' 'No draft CLI release is superseded' \
  GH_STUB_RELEASES="$(release 22 v3.6.0 true 0)"
run_case same-version-draft-kept v3.5.0 0 '' 'No draft CLI release is superseded' \
  GH_STUB_RELEASES="$(release 23 v3.5.0 true 0)"
run_case component-draft-kept v3.5.0 0 '' 'No draft CLI release is superseded' \
  GH_STUB_RELEASES="$(release 24 slack-v0.1.0 true 0)"
run_case prerelease-draft-kept v3.5.0 0 '' 'No draft CLI release is superseded' \
  GH_STUB_RELEASES="$(release 25 v3.4.0-rc.1 true 0)"
run_case four-field-draft-kept v3.5.0 0 '' 'No draft CLI release is superseded' \
  GH_STUB_RELEASES="$(release 26 v3.4.0.1 true 0)"
run_case no-releases v3.5.0 0 '' 'No draft CLI release is superseded' GH_STUB_RELEASES=
[[ ! -s "$summary_out" ]] || {
  failures=$((failures + 1))
  echo "FAIL no-releases: a step summary was written with nothing deleted"
}

# --- versions compare numerically per field, never as text: "10" sorts
# before "9" as a string, in each of the three positions.
run_case numeric-minor-lower v3.10.0 0 '30' 'Deleted the empty draft release v3.9.0' \
  GH_STUB_RELEASES="$(release 30 v3.9.0 true 0)"
run_case numeric-minor-higher v3.9.0 0 '' 'No draft CLI release is superseded' \
  GH_STUB_RELEASES="$(release 31 v3.10.0 true 0)"
run_case numeric-major-higher v9.0.0 0 '' 'No draft CLI release is superseded' \
  GH_STUB_RELEASES="$(release 32 v10.0.0 true 0)"
run_case numeric-patch-lower v3.5.10 0 '33' 'Deleted the empty draft release v3.5.9' \
  GH_STUB_RELEASES="$(release 33 v3.5.9 true 0)"
run_case numeric-patch-higher v3.5.9 0 '' 'No draft CLI release is superseded' \
  GH_STUB_RELEASES="$(release 34 v3.5.10 true 0)"

# --- refusals: nothing is deleted.
run_case published-still-draft v3.5.0 1 '' 'the v3.5.0 release is not public (isDraft=true)' \
  GH_STUB_PUBLISHED_DRAFT=true GH_STUB_RELEASES="$mixed"
expect_no_list published-still-draft
run_case published-state-empty v3.5.0 1 '' 'the v3.5.0 release is not public (isDraft=<empty>)' \
  GH_STUB_PUBLISHED_DRAFT= GH_STUB_RELEASES="$mixed"
run_case published-unreadable v3.5.0 1 '' 'the v3.5.0 release could not be read' \
  GH_STUB_VIEW_STATUS=1 GH_STUB_RELEASES="$mixed"
run_case list-unreadable v3.5.0 1 '' 'the release list could not be read' \
  GH_STUB_LIST_STATUS=1 GH_STUB_RELEASES="$mixed"
for bad_tag in cli-v3.5.0 v3.5 v3.5.0-rc.1 3.5.0 v03.5.0 'v3.5.0 '; do
  run_case "tag-refused[$bad_tag]" "$bad_tag" 1 '' 'is not a bare v<x>.<y>.<z> CLI tag' \
    GH_STUB_RELEASES="$mixed"
  [[ ! -s "$argv_out" ]] || {
    failures=$((failures + 1))
    echo "FAIL tag-refused[$bad_tag]: gh was called for a refused tag"
  }
done

# One inexact record refuses the whole list, including the deletable draft
# that precedes it: selection finishes before the first deletion.
for bad_record in \
  '{"id":"41","tag_name":"v3.3.0","draft":true,"assets":0}' \
  '{"id":41.5,"tag_name":"v3.3.0","draft":true,"assets":0}' \
  '{"id":0,"tag_name":"v3.3.0","draft":true,"assets":0}' \
  '{"id":41,"tag_name":null,"draft":true,"assets":0}' \
  '{"id":41,"tag_name":"v3.3.0","draft":"true","assets":0}' \
  '{"id":41,"tag_name":"v3.3.0","draft":true,"assets":null}' \
  '{"id":41,"tag_name":"v3.3.0","draft":true}' \
  'not json'; do
  run_case "inexact-record[$bad_record]" v3.5.0 1 '' 'the release list holds a record that is not exact' \
    GH_STUB_RELEASES="$(release 40 v3.4.0 true 0; printf '%s\n' "$bad_record")"
done

# A failed deletion stops the run red. The draft before it is already gone
# and stays on record in the step summary; the one after it is not attempted.
three="$(
  release 50 v3.4.0 true 0
  release 51 v3.3.0 true 0
  release 52 v3.2.0 true 0
)"
run_case delete-fails v3.5.0 1 '50 51' 'draft release v3.3.0 (id 51) could not be deleted' \
  GH_STUB_RELEASES="$three" GH_STUB_DELETE_FAILS=51
expect_summary delete-fails "- \`v3.4.0\`"
if grep -qF -e 'v3.3.0' -e 'v3.2.0' "$summary_out"; then
  failures=$((failures + 1))
  echo "FAIL delete-fails: the step summary records a draft that was not deleted"
fi

# When the first deletion fails nothing was deleted, and the summary must not
# say otherwise.
run_case first-delete-fails v3.5.0 1 '50' 'draft release v3.4.0 (id 50) could not be deleted' \
  GH_STUB_RELEASES="$three" GH_STUB_DELETE_FAILS=50
[[ ! -s "$summary_out" ]] || {
  failures=$((failures + 1))
  echo "FAIL first-delete-fails: a step summary was written with nothing deleted"
}

# Missing required environment is a refusal, not an empty success.
run_case missing-tag '' 1 '' 'CLI_TAG must be set' GH_STUB_RELEASES="$mixed"
run_case missing-repository v3.5.0 1 '' 'GITHUB_REPOSITORY must be set' \
  GITHUB_REPOSITORY= GH_STUB_RELEASES="$mixed"

if [[ "$failures" != 0 ]]; then
  echo "$failures of $case_no prune-superseded-cli-drafts cases failed" >&2
  exit 1
fi
echo "all $case_no prune-superseded-cli-drafts cases passed"
