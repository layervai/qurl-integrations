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
((wait_seconds <= 14400 && poll_seconds <= 60 && poll_seconds <= wait_seconds)) || {
  echo "::error::CLI main wait or poll duration is outside its bound" >&2
  exit 1
}

max_polls=$((wait_seconds / poll_seconds + 1))
for ((poll_index = 0; poll_index < max_polls; poll_index++)); do
  response=$(gh api --method GET "repos/${repository}/actions/workflows/cli.yml/runs" \
    -f "head_sha=${source_sha}" -f event=push -f per_page=100)
  run=$(jq -c --arg repository "$repository" --arg sha "$source_sha" '
    [(.workflow_runs // [])[] | select(
      .repository.full_name == $repository and
      .head_repository.full_name == $repository and
      .head_sha == $sha and
      .path == ".github/workflows/cli.yml" and
      .event == "push" and
      (.id | type == "number" and . > 0) and
      (.run_attempt | type == "number" and . > 0)
    )] |
    if length == 0 then null
    elif length == 1 then .[0]
    else error("ambiguous exact CLI main runs")
    end
  ' <<<"$response") || {
    echo "::error::GitHub returned malformed or ambiguous CLI main-run data" >&2
    exit 1
  }

  if [[ "$run" != null ]]; then
    status=$(jq -er '.status | select(type == "string")' <<<"$run")
    if [[ "$status" == completed ]]; then
      conclusion=$(jq -er '.conclusion | select(type == "string")' <<<"$run")
      url=$(jq -r '.html_url // ""' <<<"$run")
      if [[ "$conclusion" == success ]]; then
        jq -cn --argjson run_id "$(jq -r .id <<<"$run")" \
          --argjson run_attempt "$(jq -r .run_attempt <<<"$run")" \
          --arg url "$url" '{run_id:$run_id,run_attempt:$run_attempt,url:$url}'
        exit 0
      fi
      echo "::error::Exact CLI main workflow concluded ${conclusion}: ${url}" >&2
      exit 1
    fi
  fi

  if ((poll_index + 1 == max_polls)); then
    break
  fi
  if ((poll_index == 0 || (poll_index + 1) % 20 == 0)); then
    echo "Waiting for exact CLI main CI at ${source_sha}..." >&2
  fi
  sleep "$poll_seconds"
done

echo "::error::Timed out waiting for exact CLI main CI at ${source_sha}" >&2
exit 1
