#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: $0 <source-sha> <run-id> <run-attempt>" >&2
  exit 2
fi

source_sha=$1
run_id=$2
run_attempt=$3
repository=${GITHUB_REPOSITORY:-}
[[ "$repository" == "layervai/qurl-integrations" ]] || {
  echo "::error::CLI release gates require the canonical repository" >&2
  exit 1
}
[[ -n "${GH_TOKEN:-}" && "$source_sha" =~ ^[0-9a-f]{40}$ &&
  "$run_id" =~ ^[1-9][0-9]{0,19}$ && "$run_attempt" =~ ^[1-9][0-9]{0,19}$ ]] || {
  echo "::error::CLI release gate input is incomplete" >&2
  exit 1
}

run=$(gh api --method GET "repos/$repository/actions/runs/$run_id")
jq -e --arg repository "$repository" --arg sha "$source_sha" \
  --argjson run_id "$run_id" --argjson run_attempt "$run_attempt" '
  .repository.full_name == $repository and
  .head_repository.full_name == $repository and
  .head_branch == "main" and .head_sha == $sha and
  .path == ".github/workflows/cli.yml" and .event == "workflow_dispatch" and
  .id == $run_id and .run_attempt == $run_attempt
' <<<"$run" >/dev/null || {
  echo "::error::CLI release gate run does not match the exact handoff" >&2
  exit 1
}
run_url=$(jq -r '.html_url // ""' <<<"$run")
jobs=$(gh api --method GET \
  "repos/$repository/actions/runs/$run_id/attempts/$run_attempt/jobs" -f per_page=100)
gates=$(jq -c '
  if (.total_count | type) != "number" or .total_count < 0 or .total_count > 100 or
    (.jobs | type) != "array" or (.jobs | length) != .total_count
  then error("CLI job data is malformed or truncated")
  else {
    required: [.jobs[] | select(.name == "cli / required")],
    journeys: [.jobs[] | select(
      .name == "cli / customer journey" or
      (.name | startswith("cli / customer journey (")))],
    cleanup: [.jobs[] | select(.name == "cli / customer journey cleanup")]
  }
  end
' <<<"$jobs")

gate_shape=$(jq -r '
  (.required | length) == 1 and
  (.journeys | length) == 4 and
  (.cleanup | length) == 1
' <<<"$gates")
[[ "$gate_shape" == true ]] || {
  echo "::error::Exact CLI customer-journey gate set is missing or ambiguous: $run_url" >&2
  exit 1
}

if ! jq -e 'all(.required[], .journeys[], .cleanup[]; .status == "completed")' \
  <<<"$gates" >/dev/null; then
  jq -cn --argjson run_id "$run_id" --argjson run_attempt "$run_attempt" \
    '{ready:false,reason:"cli_release_journey_incomplete",run_id:$run_id,run_attempt:$run_attempt}'
  exit 0
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

jq -cn --argjson run_id "$run_id" --argjson run_attempt "$run_attempt" --arg url "$run_url" \
  '{ready:true,run_id:$run_id,run_attempt:$run_attempt,url:$url,journey_url:$url}'
