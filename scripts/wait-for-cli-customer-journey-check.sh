#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: $0 <source-sha> <producer-run-id> <producer-run-attempt>" >&2
  exit 2
fi

source_sha=$1
producer_run_id=$2
producer_run_attempt=$3
repository=${GITHUB_REPOSITORY:-}
wait_seconds=${QURL_CUSTOMER_JOURNEY_WAIT_SECONDS:-9000}
poll_seconds=${QURL_CUSTOMER_JOURNEY_POLL_SECONDS:-15}
max_api_failures=${QURL_CUSTOMER_JOURNEY_MAX_API_FAILURES:-4}

[[ "$repository" == "layervai/qurl-integrations" ]] || {
  echo "::error::customer-journey checks can be read only in the canonical repository" >&2
  exit 1
}
[[ -n "${GH_TOKEN:-}" ]] || { echo "::error::GH_TOKEN is required" >&2; exit 1; }
[[ "$source_sha" =~ ^[0-9a-f]{40}$ ]] || { echo "::error::source SHA must be exact lowercase 40-hex" >&2; exit 1; }
[[ "$producer_run_id" =~ ^[1-9][0-9]*$ && "$producer_run_attempt" =~ ^[1-9][0-9]*$ ]] || {
  echo "::error::producer run ID and attempt must be positive integers" >&2
  exit 1
}
[[ "$wait_seconds" =~ ^[1-9][0-9]*$ && "$poll_seconds" =~ ^[1-9][0-9]*$ ]] || {
  echo "::error::customer-journey wait and poll durations must be positive integers" >&2
  exit 1
}
if [[ ! "$max_api_failures" =~ ^[1-9][0-9]*$ ]] || ((max_api_failures > 10)); then
  echo "::error::customer-journey maximum API failures must be between 1 and 10" >&2
  exit 1
fi
((wait_seconds <= 10800 && poll_seconds <= 60 && poll_seconds <= wait_seconds)) || {
  echo "::error::customer-journey wait or poll duration is outside its bound" >&2
  exit 1
}

check_name="qURL Customer Journey / exact CLI artifact"
external_id="layerv.qurl-cli-customer-journey.v1:${source_sha}:${producer_run_id}:${producer_run_attempt}"
max_polls=$((wait_seconds / poll_seconds + 1))
consecutive_api_failures=0

for ((poll_index = 0; poll_index < max_polls; poll_index++)); do
  if ! response=$(gh api --method GET "repos/${repository}/commits/${source_sha}/check-runs" \
    -f "check_name=${check_name}" -f filter=all -f per_page=100); then
    consecutive_api_failures=$((consecutive_api_failures + 1))
    if ((consecutive_api_failures >= max_api_failures)); then
      echo "::error::GitHub API failed ${consecutive_api_failures} consecutive customer-journey polls" >&2
      exit 1
    fi
    echo "::warning::GitHub API customer-journey poll failed; retrying (${consecutive_api_failures}/${max_api_failures})" >&2
    sleep "$poll_seconds"
    continue
  fi
  consecutive_api_failures=0
  latest=$(jq -c --arg name "$check_name" --arg external_id "$external_id" '
    (.check_runs // []) |
    map(select(
      .name == $name and
      .external_id == $external_id and
      .app.slug == "github-actions" and
      (.id | type == "number" and . > 0)
    )) |
    if length == 0 then null else max_by(.id) end
  ' <<<"$response") || {
    echo "::error::GitHub returned malformed customer-journey check data" >&2
    exit 1
  }

  if [[ "$latest" != null ]]; then
    status=$(jq -er '.status | select(type == "string")' <<<"$latest")
    if [[ "$status" == completed ]]; then
      conclusion=$(jq -er '.conclusion | select(type == "string")' <<<"$latest")
      details_url=$(jq -r '.details_url // ""' <<<"$latest")
      if [[ "$conclusion" == success ]]; then
        echo "Exact packaged CLI customer journey passed: ${details_url}"
        exit 0
      fi
      echo "::error::Exact packaged CLI customer journey concluded ${conclusion}: ${details_url}" >&2
      exit 1
    fi
  fi

  if ((poll_index + 1 == max_polls)); then
    break
  fi
  if ((poll_index == 0 || (poll_index + 1) % 20 == 0)); then
    echo "Waiting for the exact packaged CLI customer-journey result..." >&2
  fi
  sleep "$poll_seconds"
done

echo "::error::Timed out waiting for the exact packaged CLI customer-journey result" >&2
exit 1
