#!/usr/bin/env bash
set -euo pipefail

for command in gh go jq; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "required command is unavailable: $command" >&2
    exit 69
  }
done

module_version=$(GOWORK=off GOFLAGS=-mod=readonly \
  go list -m -f '{{.Version}}' github.com/layervai/qurl-go)
if ! [[ "$module_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+-0\.[0-9]{14}-([0-9a-f]{12})$ ]]; then
  echo "qurl-go is not pinned by one exact pseudo-version" >&2
  exit 65
fi
prefix=${BASH_REMATCH[1]}

commit=$(gh api "repos/layervai/qurl-go/commits/$prefix")
source_sha=$(jq -er '.sha | select(test("^[0-9a-f]{40}$"))' <<<"$commit")
[[ "$source_sha" == "$prefix"* ]] || {
  echo "resolved qurl-go commit does not match the module pseudo-version" >&2
  exit 65
}
jq -e '.commit.verification.verified == true and .commit.verification.reason == "valid"' \
  <<<"$commit" >/dev/null || {
  echo "resolved qurl-go commit is not signed and verified" >&2
  exit 65
}

main_ref=$(gh api repos/layervai/qurl-go/git/ref/heads/main)
main_sha=$(jq -er '.object.sha | select(test("^[0-9a-f]{40}$"))' <<<"$main_ref")
comparison=$(gh api "repos/layervai/qurl-go/compare/$source_sha...$main_sha")
jq -e \
  --arg source "$source_sha" \
  --arg main "$main_sha" \
  '.base_commit.sha == $source and
   .merge_base_commit.sha == $source and
   ((.status == "identical" and
     $source == $main and
     .head_commit == null and
     .ahead_by == 0 and
     .behind_by == 0 and
     .total_commits == 0 and
     .commits == []) or
    (.status == "ahead" and
     .head_commit.sha == $main and
     .ahead_by > 0 and
     .behind_by == 0 and
     .total_commits == .ahead_by))' \
  <<<"$comparison" >/dev/null || {
  echo "resolved qurl-go source is not an exact ancestor of current main" >&2
  exit 65
}

printf '%s\n' "$source_sha"
