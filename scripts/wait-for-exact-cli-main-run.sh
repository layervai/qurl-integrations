#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <source-sha>" >&2
  exit 2
fi

source_sha=$1
repository=${GITHUB_REPOSITORY:-}
wait_seconds=${QURL_CLI_MAIN_WAIT_SECONDS:-12600}
poll_seconds=${QURL_CLI_MAIN_POLL_SECONDS:-15}
max_api_failures=${QURL_CLI_MAIN_MAX_API_FAILURES:-4}

[[ "$repository" == "layervai/qurl-integrations" ]] || {
  echo "::error::CLI main runs can be read only in the canonical repository" >&2
  exit 1
}
[[ -n "${GH_TOKEN:-}" ]] || { echo "::error::GH_TOKEN is required" >&2; exit 1; }
[[ "$source_sha" =~ ^[0-9a-f]{40}$ ]] || { echo "::error::source SHA must be exact lowercase 40-hex" >&2; exit 1; }
[[ "$wait_seconds" =~ ^[1-9][0-9]*$ && "$poll_seconds" =~ ^[1-9][0-9]*$ ]] || {
  echo "::error::CLI main wait and poll durations must be positive integers" >&2
  exit 1
}
if [[ ! "$max_api_failures" =~ ^[1-9][0-9]*$ ]] || ((max_api_failures > 10)); then
  echo "::error::CLI main maximum API failures must be between 1 and 10" >&2
  exit 1
fi
((wait_seconds <= 14400 && poll_seconds <= 60 && poll_seconds <= wait_seconds)) || {
  echo "::error::CLI main wait or poll duration is outside its bound" >&2
  exit 1
}

max_polls=$((wait_seconds / poll_seconds + 1))
deadline=$((SECONDS + wait_seconds))
consecutive_api_failures=0
source_run_id=
source_run_attempt=
source_run_url=
for ((poll_index = 0; poll_index < max_polls; poll_index++)); do
  if ! response=$(gh api --method GET "repos/${repository}/actions/workflows/cli.yml/runs" \
    -f "head_sha=${source_sha}" -f event=push -f per_page=100); then
    consecutive_api_failures=$((consecutive_api_failures + 1))
    if ((consecutive_api_failures >= max_api_failures)); then
      echo "::error::GitHub API failed ${consecutive_api_failures} consecutive CLI main polls" >&2
      exit 1
    fi
    echo "::warning::GitHub API CLI main poll failed; retrying (${consecutive_api_failures}/${max_api_failures})" >&2
    sleep "$poll_seconds"
    continue
  fi
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
  ' <<<"$response") || {
    echo "::error::GitHub returned malformed or truncated CLI main-run data" >&2
    exit 1
  }

  if [[ "$run" != null ]]; then
    status=$(jq -er '.status | select(type == "string")' <<<"$run")
    if [[ "$status" == completed ]]; then
      conclusion=$(jq -er '.conclusion | select(type == "string")' <<<"$run")
      url=$(jq -r '.html_url // ""' <<<"$run")
      if [[ "$conclusion" == success ]]; then
        if ! jobs_response=$(gh api --method GET "repos/${repository}/actions/runs/$(jq -r .id <<<"$run")/attempts/$(jq -r .run_attempt <<<"$run")/jobs" -f per_page=100); then
          consecutive_api_failures=$((consecutive_api_failures + 1))
          if ((consecutive_api_failures >= max_api_failures)); then
            echo "::error::GitHub API failed ${consecutive_api_failures} consecutive CLI main job polls" >&2
            exit 1
          fi
          echo "::warning::GitHub API CLI main job poll failed; retrying (${consecutive_api_failures}/${max_api_failures})" >&2
          sleep "$poll_seconds"
          continue
        fi
        consecutive_api_failures=0
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
        ' <<<"$jobs_response" >/dev/null || {
          echo "::error::Exact CLI main workflow did not complete every required CLI job successfully: ${url}" >&2
          exit 1
        }
        source_run_id=$(jq -r .id <<<"$run")
        source_run_attempt=$(jq -r .run_attempt <<<"$run")
        source_run_url=$url
        break
      fi
      echo "::error::Exact CLI main workflow concluded ${conclusion}: ${url}" >&2
      exit 1
    fi
  fi
  consecutive_api_failures=0

  if ((poll_index + 1 == max_polls)); then
    break
  fi
  if ((poll_index == 0 || (poll_index + 1) % 20 == 0)); then
    echo "Waiting for exact CLI main CI at ${source_sha}..." >&2
  fi
  sleep "$poll_seconds"
done

[[ -n "$source_run_id" ]] || {
  echo "::error::Timed out waiting for exact CLI main CI at ${source_sha}" >&2
  exit 1
}

check_name="qURL Customer Journey / exact CLI artifact"
external_id="layerv.qurl-cli-customer-journey.v1:${source_run_id}:${source_run_attempt}:${source_sha}"
remaining_seconds=$((deadline - SECONDS))
if ((remaining_seconds <= 0)); then
  echo "::error::Timed out waiting for exact packaged CLI customer journey at ${source_sha}" >&2
  exit 1
fi
max_check_polls=$((remaining_seconds / poll_seconds + 1))
for ((poll_index = 0; poll_index < max_check_polls; poll_index++)); do
  if ! response=$(gh api --method GET "repos/${repository}/commits/${source_sha}/check-runs" \
    -f "check_name=${check_name}" -f filter=latest -f per_page=100); then
    consecutive_api_failures=$((consecutive_api_failures + 1))
    if ((consecutive_api_failures >= max_api_failures)); then
      echo "::error::GitHub API failed ${consecutive_api_failures} consecutive customer-journey polls" >&2
      exit 1
    fi
    echo "::warning::GitHub API customer-journey poll failed; retrying (${consecutive_api_failures}/${max_api_failures})" >&2
    sleep "$poll_seconds"
    continue
  fi
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
  ' <<<"$response") || {
    echo "::error::GitHub returned malformed or truncated customer-journey check data" >&2
    exit 1
  }
  consecutive_api_failures=0
  if [[ "$check" != null ]]; then
    status=$(jq -er '.status | select(type == "string")' <<<"$check")
    if [[ "$status" == completed ]]; then
      conclusion=$(jq -er '.conclusion | select(type == "string")' <<<"$check")
      check_url=$(jq -r '.details_url // ""' <<<"$check")
      if [[ "$conclusion" != success ]]; then
        echo "::error::Exact packaged CLI customer journey concluded ${conclusion}: ${check_url}" >&2
        exit 1
      fi
      jq -cn --argjson run_id "$source_run_id" \
        --argjson run_attempt "$source_run_attempt" \
        --arg url "$source_run_url" --arg journey_url "$check_url" \
        '{run_id:$run_id,run_attempt:$run_attempt,url:$url,journey_url:$journey_url}'
      exit 0
    fi
  fi
  if ((poll_index + 1 == max_check_polls)); then
    break
  fi
  if ((poll_index == 0 || (poll_index + 1) % 20 == 0)); then
    echo "Waiting for exact packaged CLI customer journey at ${source_sha}..." >&2
  fi
  sleep "$poll_seconds"
done

echo "::error::Timed out waiting for exact packaged CLI customer journey at ${source_sha}" >&2
exit 1
