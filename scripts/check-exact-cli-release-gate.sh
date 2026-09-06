#!/usr/bin/env bash
set -euo pipefail

# Cleanup is part of the release gate by design. A product journey that leaves
# live customer or device credentials is a failed security journey, even when
# the CLI assertions passed. The fallback workflow still retries that cleanup.

if [[ $# -ne 5 ]]; then
  echo "usage: $0 <source-sha> <source-tag> <run-id> <run-attempt> <journey-count>" >&2
  exit 2
fi

source_sha=$1
source_tag=$2
run_id=$3
run_attempt=$4
journey_count=$5
repository=${GITHUB_REPOSITORY:-}
[[ "$repository" == "layervai/qurl-integrations" ]] || {
  echo "::error::CLI release gates require the canonical repository" >&2
  exit 1
}
if [[ -z "${GH_TOKEN:-}" || ! "$source_sha" =~ ^[0-9a-f]{40}$ ||
  ! "$source_tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ||
  ! "$run_id" =~ ^[1-9][0-9]{0,18}$ || ! "$run_attempt" =~ ^[1-9][0-9]?$ ||
  ! "$journey_count" =~ ^[1-9][0-9]?$ ]] ||
  (( 10#$run_attempt > 30 || 10#$journey_count > 20 )); then
  echo "::error::CLI release gate input is incomplete" >&2
  exit 1
fi

gh_stderr=$(mktemp)
trap 'rm -f "$gh_stderr"' EXIT

gh_json() {
  local path=$1 response attempt status
  for attempt in 1 2 3; do
    : >"$gh_stderr"
    if response=$(gh api --include --method GET "$path" 2>"$gh_stderr"); then
      if [[ -s "$gh_stderr" ]]; then
        cat "$gh_stderr" >&2
      fi
      if [[ "$response" == HTTP/* ]]; then
        printf '%s\n' "$response" | awk 'body { print } /^[[:space:]]*$/ { body=1 }'
      else
        printf '%s\n' "$response"
      fi
      return 0
    fi
    status=$(sed -nE 's/^HTTP\/[^ ]+ ([0-9]{3}).*/\1/p' <<<"$response" | tail -1)
    if [[ "$status" =~ ^4[0-9]{2}$ && "$status" != 408 && "$status" != 429 ]]; then
      echo "::error::GitHub API rejected $path with HTTP $status" >&2
      return 2
    fi
    if ((attempt < 3)); then
      sleep "$attempt"
    fi
  done
  return 1
}

if ! run=$(gh_json "repos/$repository/actions/runs/$run_id"); then
  echo "::error::CLI release run lookup failed" >&2
  jq -cn --arg run_id "$run_id" --arg run_attempt "$run_attempt" \
    '{reason:"cli_release_run_unavailable",run_id:$run_id,run_attempt:$run_attempt}' >&2
  exit 1
fi
jq -e --arg repository "$repository" --arg sha "$source_sha" --arg tag "$source_tag" \
  --arg run_id "$run_id" --arg run_attempt "$run_attempt" '
  .repository.full_name == $repository and
  .head_repository.full_name == $repository and
  (.head_sha | test("^[0-9a-f]{40}$")) and
  .head_branch == "main" and
  .path == ".github/workflows/cli.yml" and .event == "workflow_dispatch" and
  .display_title == ("CLI release gate " + $sha + " " + $tag) and
  (.id | tostring) == $run_id and (.run_attempt | tostring) == $run_attempt
' <<<"$run" >/dev/null || {
  echo "::error::CLI release gate run does not match the exact handoff" >&2
  exit 1
}
run_head_sha=$(jq -r '.head_sha' <<<"$run")
if ! tag_commit=$(gh_json "repos/$repository/commits/$source_tag") ||
  ! jq -e --arg sha "$source_sha" '.sha == $sha' <<<"$tag_commit" >/dev/null; then
  echo "::error::CLI release tag no longer names the exact tested source" >&2
  exit 1
fi
if [[ "$run_head_sha" != "$source_sha" ]]; then
  if ! comparison=$(gh_json "repos/$repository/compare/$source_sha...$run_head_sha") ||
    ! jq -e --arg source "$source_sha" '
      .status == "ahead" and .merge_base_commit.sha == $source
    ' <<<"$comparison" >/dev/null; then
    echo "::error::CLI release source is not an ancestor of the trusted main workflow" >&2
    exit 1
  fi
fi
run_url=$(jq -r '.html_url // ""' <<<"$run")
latest_jobs='{}'
for ((attempt = 10#$run_attempt; attempt >= 1; attempt--)); do
  if ! jobs=$(gh_json \
    "repos/$repository/actions/runs/$run_id/attempts/$attempt/jobs?per_page=100"); then
    echo "::error::CLI release job lookup failed" >&2
    jq -cn --arg run_id "$run_id" --arg run_attempt "$run_attempt" \
      --arg job_attempt "$attempt" \
      '{reason:"cli_release_jobs_unavailable",run_id:$run_id,run_attempt:$run_attempt,job_attempt:$job_attempt}' >&2
    exit 1
  fi
  if ! current_gates=$(jq -c --argjson attempt "$attempt" '
    if (.total_count | type) != "number" or .total_count < 0 or .total_count > 100 or
      (.jobs | type) != "array" or (.jobs | length) != .total_count
    then error("CLI job data is malformed or truncated")
    else [.jobs[] | select(
      .name == "cli / required" or
      .name == "cli / customer journey cleanup" or
      .name == "cli / customer journey" or
      (.name | startswith("cli / customer journey ("))) |
      . + {qurl_run_attempt:$attempt}]
    end |
    if length != (unique_by(.name) | length)
    then error("CLI gate job names are ambiguous within one attempt")
    else .
    end
  ' <<<"$jobs"); then
    echo "::error::CLI release job data is malformed or ambiguous" >&2
    exit 1
  fi
  latest_jobs=$(jq -cn --argjson prior "$latest_jobs" --argjson current "$current_gates" '
    reduce $current[] as $job ($prior;
      if has($job.name) then . else . + {($job.name): $job} end)
  ')
  complete=$(jq -r --argjson journey_count "$journey_count" '
    [. | to_entries[].value] as $jobs |
    ([$jobs[] | select(.name == "cli / required")] | length) == 1 and
    ([$jobs[] | select(
      .name == "cli / customer journey" or
      (.name | startswith("cli / customer journey (")))] | length) == $journey_count and
    ([$jobs[] | select(.name == "cli / customer journey cleanup")] | length) == 1
  ' <<<"$latest_jobs")
  if [[ "$complete" == true ]]; then
    break
  fi
done
gates=$(jq -cn --argjson latest "$latest_jobs" '
  [$latest | to_entries[].value] as $jobs |
  {
    required: [$jobs[] | select(.name == "cli / required")],
    journeys: [$jobs[] | select(
      .name == "cli / customer journey" or
      (.name | startswith("cli / customer journey (")))],
    cleanup: [$jobs[] | select(.name == "cli / customer journey cleanup")]
  }
')

gate_shape=$(jq -r --argjson journey_count "$journey_count" '
  (.required | length) == 1 and
  (.journeys | length) == $journey_count and
  (.cleanup | length) == 1
' <<<"$gates")
[[ "$gate_shape" == true ]] || {
  echo "::error::Exact CLI customer-journey gate set is missing or ambiguous: $run_url" >&2
  exit 1
}

if ! jq -e 'all(.required[], .journeys[], .cleanup[]; .status == "completed")' \
  <<<"$gates" >/dev/null; then
  echo "::error::Exact CLI customer-journey jobs were incomplete after the tested run signaled release" >&2
  jq -cn --arg run_id "$run_id" --arg run_attempt "$run_attempt" \
    '{reason:"cli_release_journey_incomplete",run_id:$run_id,run_attempt:$run_attempt}' >&2
  exit 1
fi

if ! jq -e 'all(.required[], .journeys[], .cleanup[]; .conclusion == "success")' \
  <<<"$gates" >/dev/null; then
  failed=$(jq -r '
    [.required[], .journeys[], .cleanup[] |
      select(.conclusion != "success") |
      (.name + "=" + (.conclusion // "<empty>"))] | join(", ")
  ' <<<"$gates")
  echo "::error::Exact CLI customer-journey gate failed ($failed): $run_url" >&2
  exit 1
fi

jq -cn --arg run_id "$run_id" --arg run_attempt "$run_attempt" --arg url "$run_url" \
  '{run_id:$run_id,run_attempt:$run_attempt,url:$url,journey_url:$url}'
