#!/usr/bin/env bash
# Resolve the base SHA a default-branch push should be diffed against.
#
# GitHub keeps at most one *pending* run per concurrency group. The app
# workflows set `cancel-in-progress: false` on main, which protects a run
# that is already in progress but not one still queued: a newer push
# supersedes and cancels the older pending run. A burst of merges therefore
# leaves the intermediate commits with no run at all, and the surviving run's
# paths filter — which by default diffs only its own push — skips the app
# jobs for changes that landed in the cancelled runs. Every check reports
# green with the app CI never having executed. See #1022 for the observed
# instance (an undici bump in apps/discord reached main unvalidated).
#
# Diffing against the last SHA this workflow actually validated closes that:
# the surviving run covers the whole accumulated range. It also re-covers the
# range after a *failed* run, because a failed run never advances the base.
#
# Emits two GITHUB_OUTPUT values:
#   sha   - base SHA for dorny/paths-filter's `base:` input; empty when unknown
#   force - "true" when no trustworthy base exists, meaning the caller must
#           treat every gate as changed rather than trust a narrower diff
#
# Every unknown fails closed. A wrong "force" costs one redundant CI run; a
# wrong narrow diff is the silent-skip bug this exists to prevent.
set -euo pipefail

: "${GITHUB_OUTPUT:?GITHUB_OUTPUT must be set (this script writes step outputs)}"

emit() {
  printf 'sha=%s\n' "$1" >> "$GITHUB_OUTPUT"
  printf 'force=%s\n' "$2" >> "$GITHUB_OUTPUT"
  printf 'base=%s force=%s\n' "${1:-<none>}" "$2"
}

force_full() {
  printf '::warning::%s; validating every gate against this push instead of a narrower diff\n' "$1"
  emit '' true
  exit 0
}

repo="${GITHUB_REPOSITORY:-}"
head_sha="${GITHUB_SHA:-}"
branch="${GITHUB_REF_NAME:-}"

# GITHUB_WORKFLOW_REF is `owner/repo/.github/workflows/<file>@<ref>`, and the
# REST API's `workflow_id` path segment accepts that bare file name. Deriving
# it beats having each caller hardcode — and eventually drift from — its own
# filename. WORKFLOW_FILE overrides it for the test harness.
workflow_file="${WORKFLOW_FILE:-}"
if [[ -z "$workflow_file" && -n "${GITHUB_WORKFLOW_REF:-}" ]]; then
  workflow_path="${GITHUB_WORKFLOW_REF%%@*}"
  workflow_file="${workflow_path##*/}"
fi

[[ -n "$repo" ]] || force_full 'GITHUB_REPOSITORY is unset'
[[ -n "$branch" ]] || force_full 'GITHUB_REF_NAME is unset'
[[ "$head_sha" =~ ^[0-9a-f]{40}$ ]] || force_full "GITHUB_SHA is not a commit SHA: ${head_sha:-<empty>}"
[[ "$workflow_file" == *.yml || "$workflow_file" == *.yaml ]] ||
  force_full "could not derive a workflow file name from GITHUB_WORKFLOW_REF: ${GITHUB_WORKFLOW_REF:-<empty>}"

# `status=success` selects on conclusion, so a cancelled or failed run never
# advances the base. The in-flight run cannot match itself — it is still
# in_progress while this step executes.
#
# This relies on the endpoint's default `created_at` descending sort to make
# per_page=1 mean "most recent". A re-run keeps its original created_at, so
# re-running an old success cannot pull the base backwards past a newer one;
# any staleness here widens the diff, which is the safe direction.
api_status=0
base_sha="$(gh api \
  --method GET "repos/${repo}/actions/workflows/${workflow_file}/runs" \
  --raw-field "branch=${branch}" \
  --raw-field event=push \
  --raw-field status=success \
  --raw-field per_page=1 \
  --jq '.workflow_runs[0].head_sha // empty')" || api_status=$?

if (( api_status != 0 )); then
  force_full "querying previous ${workflow_file} runs failed (gh exited ${api_status})"
fi

base_sha="${base_sha//[[:space:]]/}"

[[ -n "$base_sha" ]] ||
  force_full "no successful ${workflow_file} push run on ${branch} to diff against"
[[ "$base_sha" =~ ^[0-9a-f]{40}$ ]] ||
  force_full "previous ${workflow_file} run reported an unusable head SHA: ${base_sha}"

# A re-run of an already-successful SHA. An empty diff would skip every gate
# — including, on main, the deploy dispatch — which is the opposite of what
# re-running a run is for.
[[ "$base_sha" != "$head_sha" ]] ||
  force_full "last validated SHA is this run's own SHA (${base_sha:0:7})"

git cat-file -e "${base_sha}^{commit}" 2>/dev/null ||
  force_full "last validated SHA ${base_sha:0:7} is not present locally (the checkout needs fetch-depth: 0)"

# Guards against a history rewrite leaving a base that no longer leads to
# HEAD, where `git diff base..HEAD` would report unrelated churn as changed.
git merge-base --is-ancestor "$base_sha" "$head_sha" 2>/dev/null ||
  force_full "last validated SHA ${base_sha:0:7} is not an ancestor of ${head_sha:0:7}"

emit "$base_sha" false
