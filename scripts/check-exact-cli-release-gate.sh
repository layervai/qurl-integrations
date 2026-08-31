#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <source-sha>" >&2
  exit 2
fi

source_sha=$1
repository=${GITHUB_REPOSITORY:-}
[[ "$repository" == "layervai/qurl-integrations" ]] || {
  echo "::error::CLI release gates require the canonical repository" >&2
  exit 1
}
[[ -n "${GH_TOKEN:-}" && "$source_sha" =~ ^[0-9a-f]{40}$ ]] || {
  echo "::error::CLI release gate input is incomplete" >&2
  exit 1
}

runs=$(gh api --method GET "repos/$repository/actions/workflows/cli.yml/runs" \
  -f "head_sha=$source_sha" -f event=push -f per_page=100)
run=$(jq -c --arg repository "$repository" --arg sha "$source_sha" '
  if (.total_count | type) != "number" or .total_count < 0 or .total_count > 100 or
    (.workflow_runs | type) != "array" or (.workflow_runs | length) != .total_count
  then error("CLI run data is malformed or truncated")
  else [.workflow_runs[] | select(
    .repository.full_name == $repository and
    .head_repository.full_name == $repository and
    .head_branch == "main" and .head_sha == $sha and
    .path == ".github/workflows/cli.yml" and .event == "push" and
    (.id | type) == "number" and .id > 0 and
    (.run_attempt | type) == "number" and .run_attempt > 0
  )] | sort_by(.id) | last // null
  end
' <<<"$runs")

if [[ "$run" == null ]]; then
  jq -cn '{ready:false,reason:"cli_main_incomplete"}'
  exit 0
fi

run_id=$(jq -er '.id' <<<"$run")
run_attempt=$(jq -er '.run_attempt' <<<"$run")
run_url=$(jq -r '.html_url // ""' <<<"$run")
jobs=$(gh api --method GET \
  "repos/$repository/actions/runs/$run_id/attempts/$run_attempt/jobs" -f per_page=100)
required=$(jq -c '
  if (.total_count | type) != "number" or .total_count < 0 or .total_count > 100 or
    (.jobs | type) != "array" or (.jobs | length) != .total_count
  then error("CLI job data is malformed or truncated")
  else [.jobs[] | select(.name == "cli / required")]
    | if length > 1 then error("multiple cli / required jobs") else .[0] // null end
  end
' <<<"$jobs")

if [[ "$required" == null || $(jq -r '.status // ""' <<<"$required") != completed ]]; then
  jq -cn --argjson run_id "$run_id" --argjson run_attempt "$run_attempt" \
    '{ready:false,reason:"cli_main_incomplete",run_id:$run_id,run_attempt:$run_attempt}'
  exit 0
fi

conclusion=$(jq -er '.conclusion | select(type == "string")' <<<"$required")
[[ "$conclusion" == success ]] || {
  echo "::error::Exact cli / required concluded $conclusion: $run_url" >&2
  exit 1
}

jq -cn --argjson run_id "$run_id" --argjson run_attempt "$run_attempt" --arg url "$run_url" \
  '{ready:true,run_id:$run_id,run_attempt:$run_attempt,url:$url,journey_url:$url}'
