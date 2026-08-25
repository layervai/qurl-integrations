#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
script="$root/scripts/resolve-sandbox-artifact-qurl-go-source.sh"
tmp=$(mktemp -d)
cleanup() {
  find "$tmp" -type f -delete 2>/dev/null || true
  find "$tmp" -depth -type d -empty -delete 2>/dev/null || true
}
trap cleanup EXIT

cd "$root"
module_version=$(GOWORK=off GOFLAGS=-mod=readonly \
  go list -m -f '{{.Version}}' github.com/layervai/qurl-go)
if ! [[ "$module_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+-0\.[0-9]{14}-([0-9a-f]{12})$ ]]; then
  echo "test fixture could not resolve the qurl-go pseudo-version suffix" >&2
  exit 1
fi
prefix=${BASH_REMATCH[1]}
source_sha="${prefix}0000000000000000000000000000"
main_sha=ffffffffffffffffffffffffffffffffffffffff

mkdir "$tmp/bin"
# These single-quoted values are the literal body of the fake executable. Its
# variables expand only when that executable runs.
# shellcheck disable=SC2016
printf '%s\n' '#!/usr/bin/env bash' \
  'set -euo pipefail' \
  'case "$2" in' \
  '  repos/layervai/qurl-go/commits/*) printf "%s\n" "$FAKE_COMMIT" ;;' \
  '  repos/layervai/qurl-go/git/ref/heads/main) printf "%s\n" "$FAKE_MAIN" ;;' \
  '  repos/layervai/qurl-go/compare/*) printf "%s\n" "$FAKE_COMPARE" ;;' \
  '  *) echo "unexpected API route: $2" >&2; exit 2 ;;' \
  'esac' >"$tmp/bin/gh"
chmod 500 "$tmp/bin/gh"

commit() {
  jq -cn --arg sha "$1" --argjson verified "$2" --arg reason "$3" \
    '{sha:$sha,commit:{verification:{verified:$verified,reason:$reason}}}'
}
main_ref() {
  jq -cn --arg sha "$1" '{object:{sha:$sha}}'
}
compare() {
  jq -cn --arg status "$1" --arg source "$2" --arg main "$3" \
    'if $status == "identical" then
       {status:$status,base_commit:{sha:$source},merge_base_commit:{sha:$source},head_commit:null,
        ahead_by:0,behind_by:0,total_commits:0,commits:[]}
     else
       {status:$status,base_commit:{sha:$source},merge_base_commit:{sha:$source},head_commit:{sha:$main},
        ahead_by:1,behind_by:0,total_commits:1,commits:[{sha:$main}]}
     end'
}
run() {
  PATH="$tmp/bin:$PATH" \
    FAKE_COMMIT="$1" FAKE_MAIN="$2" FAKE_COMPARE="$3" "$script"
}

good_commit=$(commit "$source_sha" true valid)
good_main=$(main_ref "$main_sha")
good_compare=$(compare ahead "$source_sha" "$main_sha")
test "$(run "$good_commit" "$good_main" "$good_compare")" = "$source_sha"
test "$(run "$good_commit" "$(main_ref "$source_sha")" \
  "$(compare identical "$source_sha" "$source_sha")")" = "$source_sha"

assert_rejected() {
  local label=$1
  local commit_json=$2
  local main_json=$3
  local compare_json=$4
  if run "$commit_json" "$main_json" "$compare_json" >/dev/null 2>&1; then
    echo "$label was accepted" >&2
    exit 1
  fi
}

assert_rejected "non-prefix commit" \
  "$(commit 0000000000000000000000000000000000000000 true valid)" "$good_main" "$good_compare"
assert_rejected "short commit" "$(commit "$prefix" true valid)" "$good_main" "$good_compare"
assert_rejected "unsigned commit" "$(commit "$source_sha" false unsigned)" "$good_main" "$good_compare"
assert_rejected "non-ancestor commit" "$good_commit" "$good_main" \
  "$(compare diverged "$source_sha" "$main_sha")"
assert_rejected "drifted comparison base" "$good_commit" "$good_main" \
  "$(compare ahead 1111111111111111111111111111111111111111 "$main_sha")"
assert_rejected "drifted comparison head" "$good_commit" "$good_main" \
  "$(compare ahead "$source_sha" 1111111111111111111111111111111111111111)"
fabricated_identical=$(compare identical "$source_sha" "$source_sha")
fabricated_identical=$(jq -c --arg sha "$source_sha" '.head_commit={sha:$sha}' <<<"$fabricated_identical")
assert_rejected "fabricated identical head" "$good_commit" "$(main_ref "$source_sha")" \
  "$fabricated_identical"
assert_rejected "malformed commit response" '{}' "$good_main" "$good_compare"

echo "sandbox artifact qurl-go source resolver tests passed"
