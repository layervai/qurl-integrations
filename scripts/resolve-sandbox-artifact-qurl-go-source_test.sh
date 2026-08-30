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
tag_refs=$(git ls-remote https://github.com/layervai/qurl-go.git \
  "refs/tags/$module_version" "refs/tags/$module_version^{}")
source_sha=$(awk \
  -v direct="refs/tags/$module_version" \
  -v peeled="refs/tags/$module_version^{}" '
    $2 == direct { direct_hash=$1; direct_count++ }
    $2 == peeled { peeled_hash=$1 }
    END {
      if (direct_count != 1) exit 1
      print (peeled_hash != "" ? peeled_hash : direct_hash)
    }
  ' <<<"$tag_refs")
[[ "$source_sha" =~ ^[0-9a-f]{40}$ ]]
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
       {url:("https://api.github.com/repos/layervai/qurl-go/compare/"+$source+"..."+$main),
        status:$status,base_commit:{sha:$source},merge_base_commit:{sha:$source},
        ahead_by:0,behind_by:0,total_commits:0,commits:[]}
     else
       {url:("https://api.github.com/repos/layervai/qurl-go/compare/"+$source+"..."+$main),
        status:$status,base_commit:{sha:$source},merge_base_commit:{sha:$source},
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
truncated_compare=$(jq -cn --arg source "$source_sha" --arg main "$main_sha" \
  '{url:("https://api.github.com/repos/layervai/qurl-go/compare/"+$source+"..."+$main),
    status:"ahead",base_commit:{sha:$source},merge_base_commit:{sha:$source},
    ahead_by:251,behind_by:0,total_commits:251,
    commits:([range(0;249) | {sha:"1111111111111111111111111111111111111111"}] + [{sha:$main}])}')
test "$(run "$good_commit" "$good_main" "$truncated_compare")" = "$source_sha"

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

assert_rejected "mismatched tag commit" \
  "$(commit 0000000000000000000000000000000000000000 true valid)" "$good_main" "$good_compare"
assert_rejected "short commit" "$(commit "${source_sha:0:12}" true valid)" "$good_main" "$good_compare"
assert_rejected "unsigned commit" "$(commit "$source_sha" false unsigned)" "$good_main" "$good_compare"
assert_rejected "non-ancestor commit" "$good_commit" "$good_main" \
  "$(compare diverged "$source_sha" "$main_sha")"
assert_rejected "drifted comparison base" "$good_commit" "$good_main" \
  "$(compare ahead 1111111111111111111111111111111111111111 "$main_sha")"
assert_rejected "drifted comparison head" "$good_commit" "$good_main" \
  "$(jq -c '.commits[-1].sha="1111111111111111111111111111111111111111"' <<<"$good_compare")"
assert_rejected "missing comparison head commit" "$good_commit" "$good_main" \
  "$(jq -c '.commits=[]' <<<"$good_compare")"
assert_rejected "truncated comparison with wrong final commit" "$good_commit" "$good_main" \
  "$(jq -c '.commits[-1].sha="1111111111111111111111111111111111111111"' \
    <<<"$truncated_compare")"
assert_rejected "drifted comparison URL" "$good_commit" "$good_main" \
  "$(jq -c '.url="https://api.github.com/repos/layervai/qurl-go/compare/wrong...wrong"' <<<"$good_compare")"
assert_rejected "nonpositive ahead counter" "$good_commit" "$good_main" \
  "$(jq -c '.ahead_by=0 | .total_commits=0' <<<"$good_compare")"
assert_rejected "mismatched total counter" "$good_commit" "$good_main" \
  "$(jq -c '.total_commits=2' <<<"$good_compare")"
assert_rejected "nonzero behind counter" "$good_commit" "$good_main" \
  "$(jq -c '.behind_by=1' <<<"$good_compare")"
assert_rejected "fabricated identical commit list" "$good_commit" "$(main_ref "$source_sha")" \
  "$(jq -c --arg sha "$source_sha" '.commits=[{sha:$sha}]' \
    <<<"$(compare identical "$source_sha" "$source_sha")")"
assert_rejected "malformed commit response" '{}' "$good_main" "$good_compare"

echo "sandbox artifact qurl-go source resolver tests passed"
