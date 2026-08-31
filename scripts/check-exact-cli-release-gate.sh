#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <source-sha>" >&2
  exit 2
fi

source_sha=$1
repository=${GITHUB_REPOSITORY:-}

[[ "$repository" == "layervai/qurl-integrations" ]] || {
  echo "::error::CLI release gates can be read only in the canonical repository" >&2
  exit 1
}
[[ -n "${GH_TOKEN:-}" ]] || { echo "::error::GH_TOKEN is required" >&2; exit 1; }
[[ "$source_sha" =~ ^[0-9a-f]{40}$ ]] || {
  echo "::error::source SHA must be exact lowercase 40-hex" >&2
  exit 1
}

runs=$(gh api --method GET "repos/${repository}/actions/workflows/cli.yml/runs" \
  -f "head_sha=${source_sha}" -f event=push -f per_page=100)
run=$(jq -c --arg repository "$repository" --arg sha "$source_sha" '
  if ((.total_count | type) != "number") or .total_count < 0 or .total_count > 100 or
     ((.workflow_runs | type) != "array") or ((.workflow_runs | length) != .total_count)
  then error("truncated or malformed exact CLI main-run data")
  else .
  end |
  [(.workflow_runs // [])[] | select(
    .repository.full_name == $repository and
    .head_repository.full_name == $repository and
    .head_sha == $sha and
    .path == ".github/workflows/cli.yml" and
    .event == "push" and
    (.id | type == "number" and . > 0) and
    (.run_attempt | type == "number" and . > 0)
  )] |
  sort_by(.id) | last // null
' <<<"$runs") || {
  echo "::error::GitHub returned malformed or truncated CLI main-run data" >&2
  exit 1
}

if [[ "$run" == null ]] || [[ $(jq -r '.status // ""' <<<"$run") != completed ]]; then
  jq -cn '{ready:false,reason:"cli_main_incomplete"}'
  exit 0
fi

conclusion=$(jq -er '.conclusion | select(type == "string")' <<<"$run")
run_url=$(jq -r '.html_url // ""' <<<"$run")
[[ "$conclusion" == success ]] || {
  echo "::error::Exact CLI main workflow concluded ${conclusion}: ${run_url}" >&2
  exit 1
}

run_id=$(jq -er '.id | select(type == "number" and . > 0)' <<<"$run")
run_attempt=$(jq -er '.run_attempt | select(type == "number" and . > 0)' <<<"$run")
jobs=$(gh api --method GET \
  "repos/${repository}/actions/runs/${run_id}/attempts/${run_attempt}/jobs" -f per_page=100)
jq -e '
  if ((.total_count | type) != "number") or .total_count < 0 or .total_count > 100 or
     ((.jobs | type) != "array") or ((.jobs | length) != .total_count)
  then error("truncated or malformed CLI main jobs")
  else .
  end |
  . as $response |
  ["cli / customer journey artifacts", "cli / required"] as $required |
  all($required[]; . as $name |
    ([$response.jobs[] | select(.name == $name and .status == "completed" and .conclusion == "success")] | length) == 1 and
    ([$response.jobs[] | select(.name == $name)] | length) == 1
  )
' <<<"$jobs" >/dev/null || {
  echo "::error::Exact CLI main workflow did not complete every required CLI job successfully: ${run_url}" >&2
  exit 1
}

check_name="qURL Customer Journey / exact CLI artifact"
external_id="layerv.qurl-cli-customer-journey.v1:${run_id}:${run_attempt}:${source_sha}"
checks=$(gh api --method GET "repos/${repository}/commits/${source_sha}/check-runs" \
  -f "check_name=${check_name}" -f filter=latest -f per_page=100)
check=$(jq -c --arg name "$check_name" --arg sha "$source_sha" --arg external "$external_id" '
  if ((.total_count | type) != "number") or .total_count < 0 or .total_count > 100 or
     ((.check_runs | type) != "array") or ((.check_runs | length) != .total_count)
  then error("truncated or malformed customer-journey checks")
  else .
  end |
  [.check_runs[] | select(
    .name == $name and .head_sha == $sha and .external_id == $external and
    .app.slug == "github-actions"
  )] | if length > 1 then error("multiple exact customer-journey checks") else .[0] // null end
' <<<"$checks") || {
  echo "::error::GitHub returned malformed or truncated customer-journey check data" >&2
  exit 1
}

if [[ "$check" == null ]] || [[ $(jq -r '.status // ""' <<<"$check") != completed ]]; then
  jq -cn --argjson run_id "$run_id" --argjson run_attempt "$run_attempt" \
    '{ready:false,reason:"customer_journey_incomplete",run_id:$run_id,run_attempt:$run_attempt}'
  exit 0
fi

check_conclusion=$(jq -er '.conclusion | select(type == "string")' <<<"$check")
check_url=$(jq -r '.details_url // ""' <<<"$check")
[[ "$check_conclusion" == success ]] || {
  echo "::error::Exact packaged CLI customer journey concluded ${check_conclusion}: ${check_url}" >&2
  exit 1
}

jq -cn --argjson run_id "$run_id" --argjson run_attempt "$run_attempt" \
  --arg url "$run_url" --arg journey_url "$check_url" \
  '{ready:true,run_id:$run_id,run_attempt:$run_attempt,url:$url,journey_url:$journey_url}'
