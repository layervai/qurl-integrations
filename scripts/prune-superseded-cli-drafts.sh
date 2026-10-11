#!/usr/bin/env bash
# Delete the draft CLI releases that a published CLI release has superseded.
#
# release-please tags every CLI version and creates its GitHub Release as a
# draft; the release is made public only after the customer journey passes and
# every artifact is verified. A version whose journey fails is usually not
# repaired: the fix lands on main, release-please cuts the next version, and
# that one is published instead. Nothing ever removed the earlier draft, so it
# stayed in the release list, empty, next to the version that replaced it.
#
# This runs after CLI_TAG is public and deletes a release only when ALL of
# these hold:
#   - it is a draft;
#   - its tag is a bare `v<x>.<y>.<z>` — the CLI's tag shape, which no
#     component-prefixed tag (`slack-v0.4.0`) and no prerelease
#     (`v1.2.3-rc.1`) can match;
#   - it holds no assets, so nothing GoReleaser built is ever discarded;
#   - its version is lower than CLI_TAG's, compared numerically per field;
#   - it was created more than a day ago, so the draft of a lower version
#     whose own release run may still be active is left for the next
#     publication to remove.
#
# A draft that failed after GoReleaser uploaded to it holds assets and is
# never removed here; that one needs a person to look at it.
#
# "Lower means superseded" assumes CLI versions only ever go up, which holds
# while release-please releases from main alone. A maintenance line would
# break it: the empty draft of a v3.4.2 prepared after v3.5.0 is public would
# be deleted here. Whoever adds one must narrow this rule first.
#
# The release is deleted by its ID and the tag is never touched:
# apps/cli/CHANGELOG.md links every version's compare view to its tag,
# including the versions that were never published. The notes
# release-please wrote into the draft go with it; the same text remains in
# that changelog.
#
# A deleted draft cannot be restored, so every doubt is a refusal: if CLI_TAG
# is not public yet, or one release in the list cannot be read exactly,
# nothing is deleted.
#
# Required env: GH_TOKEN (contents: write), GITHUB_REPOSITORY, CLI_TAG.
set -euo pipefail

: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY must be set}"
: "${CLI_TAG:?CLI_TAG must be set}"

fail() {
  printf '::error::Could not prune superseded CLI drafts: %s. No further release was deleted.\n' "$1" >&2
  exit 1
}

bare_tag='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
[[ "$CLI_TAG" =~ $bare_tag ]] ||
  fail "CLI_TAG '${CLI_TAG}' is not a bare v<x>.<y>.<z> CLI tag"

# A draft is superseded only by a release people can install. `gh release
# view` is used because the REST release-by-tag route returns 404 for a draft.
is_draft="$(gh release view "$CLI_TAG" --repo "$GITHUB_REPOSITORY" --json isDraft --jq .isDraft)" ||
  fail "the ${CLI_TAG} release could not be read"
[[ "$is_draft" == false ]] ||
  fail "the ${CLI_TAG} release is not public (isDraft=${is_draft:-<empty>})"

# One compact object per release, across every page: all components share this
# list, so the CLI's drafts are not guaranteed to sit on the first page.
releases="$(gh api --paginate "repos/${GITHUB_REPOSITORY}/releases?per_page=100" \
  --jq '.[] | {id, tag_name, draft, created_at, assets: (.assets | length)}')" ||
  fail "the release list could not be read"

# Select in one pass and refuse the whole list on any record that is not
# exact, before the first deletion. `tonumber` on the captured fields cannot
# fail: the pattern admits only digits. A creation time that does not parse
# is an error like any other inexact field.
min_age_seconds=86400
superseded="$(jq -r --arg published "$CLI_TAG" --arg bare "$bare_tag" \
  --argjson min_age "$min_age_seconds" '
  def version_of($tag): $tag | [match($bare).captures[].string | tonumber];
  if (.id | type) != "number" or (.id | floor) != .id or .id <= 0
    or (.tag_name | type) != "string"
    or (.draft | type) != "boolean"
    or (.assets | type) != "number"
    or (.created_at | type) != "string"
  then error("release record is not exact: \(tojson)")
  else
    select(.draft and .assets == 0 and (.tag_name | test($bare)))
    | select(version_of(.tag_name) < version_of($published))
    | select(now - (.created_at | fromdateiso8601) > $min_age)
    | "\(.id)\t\(.tag_name)"
  end' <<<"$releases")" ||
  fail "the release list holds a record that is not exact"

if [[ -z "$superseded" ]]; then
  echo "No draft CLI release is superseded by ${CLI_TAG}."
  exit 0
fi

# Each deletion is written to the step summary as it happens, not at the end:
# when a later deletion fails, the ones already made stay on record. The
# heading goes out with the first deletion that succeeds, so a summary never
# says releases were deleted above an empty list.
summary_started=false
summarize_deleted() {
  [[ -n "${GITHUB_STEP_SUMMARY:-}" ]] || return 0
  if [[ "$summary_started" == false ]]; then
    summary_started=true
    printf '%s\n' "### Superseded CLI drafts removed" "" \
      "\`${CLI_TAG}\` is public. These earlier CLI versions were tagged but never published; their empty draft releases were deleted and their tags kept:" "" \
      >>"$GITHUB_STEP_SUMMARY"
  fi
  printf -- "- \`%s\`\n" "$1" >>"$GITHUB_STEP_SUMMARY"
}

# gh reads nothing from stdin here; closing it keeps the loop's own input, the
# remaining drafts, out of reach of anything that one day does.
while IFS=$'\t' read -r id tag; do
  gh api --method DELETE "repos/${GITHUB_REPOSITORY}/releases/${id}" >/dev/null </dev/null ||
    fail "draft release ${tag} (id ${id}) could not be deleted"
  echo "Deleted the empty draft release ${tag} (id ${id}), superseded by ${CLI_TAG}; the tag ${tag} is kept."
  summarize_deleted "$tag"
done <<<"$superseded"
