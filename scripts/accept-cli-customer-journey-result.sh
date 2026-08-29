#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <repository-dispatch-event.json>" >&2
  exit 2
fi

event_path=$1
repository=${GITHUB_REPOSITORY:-}
server_url=${GITHUB_SERVER_URL:-https://github.com}

[[ "$repository" == "layervai/qurl-integrations" ]] || {
  echo "customer-journey results are accepted only by the canonical repository" >&2
  exit 1
}
[[ -n "${GH_TOKEN:-}" ]] || { echo "GH_TOKEN is required" >&2; exit 1; }
[[ -f "$event_path" && ! -L "$event_path" ]] || { echo "event payload must be a regular file" >&2; exit 1; }
[[ "${GITHUB_RUN_ID:-}" =~ ^[1-9][0-9]*$ && "${GITHUB_RUN_ATTEMPT:-}" =~ ^[1-9][0-9]*$ ]] || {
  echo "callback workflow run identity is invalid" >&2
  exit 1
}

# GitHub authenticates repository_dispatch senders. Accept only the dedicated
# operations App identity and one closed payload schema. The payload contains
# no endpoint, credential, account, region, or private-repository detail.
jq -e '
  .action == "qurl-cli-customer-journey-result" and
  .repository.full_name == "layervai/qurl-integrations" and
  .sender.login == "ops-routines-reader[bot]" and
  .sender.id == 277190418 and
  .sender.type == "Bot" and
  (.client_payload | type == "object") and
  ((.client_payload | keys | sort) == ([
    "conclusion", "orchestrator_run_attempt", "orchestrator_run_id",
    "producer_run_attempt", "producer_run_id", "pull_request_number",
    "schema", "source_kind", "source_sha"
  ] | sort)) and
  .client_payload.schema == "layerv.qurl-cli-customer-journey-result.v1" and
  (.client_payload.source_sha | type == "string" and test("^[0-9a-f]{40}$")) and
  (.client_payload.source_kind == "main" or .client_payload.source_kind == "pull_request") and
  (.client_payload.pull_request_number | type == "number" and floor == . and . >= 0) and
  (.client_payload.producer_run_id | type == "number" and floor == . and . > 0) and
  (.client_payload.producer_run_attempt | type == "number" and floor == . and . > 0) and
  (.client_payload.orchestrator_run_id | type == "number" and floor == . and . > 0) and
  (.client_payload.orchestrator_run_attempt | type == "number" and floor == . and . > 0) and
  (.client_payload.conclusion == "success" or .client_payload.conclusion == "failure") and
  (if .client_payload.source_kind == "main"
   then .client_payload.pull_request_number == 0
   else .client_payload.pull_request_number > 0
   end)
' "$event_path" >/dev/null || {
  echo "repository-dispatch customer-journey result is not trusted or is malformed" >&2
  exit 1
}

mapfile -t fields < <(jq -r '
  .client_payload |
  .source_sha, .source_kind, (.pull_request_number | tostring),
  (.producer_run_id | tostring), (.producer_run_attempt | tostring),
  (.orchestrator_run_id | tostring), (.orchestrator_run_attempt | tostring),
  .conclusion
' "$event_path")
if ((${#fields[@]} != 8)); then
  echo "customer-journey result fields are incomplete" >&2
  exit 1
fi
source_sha=${fields[0]}
source_kind=${fields[1]}
pull_request_number=${fields[2]}
producer_run_id=${fields[3]}
producer_run_attempt=${fields[4]}
orchestrator_run_id=${fields[5]}
orchestrator_run_attempt=${fields[6]}
conclusion=${fields[7]}

producer_run=$(gh api --method GET "repos/${repository}/actions/runs/${producer_run_id}")
expected_event=push
if [[ "$source_kind" == pull_request ]]; then
  expected_event=pull_request
fi
jq -e --arg repository "$repository" --arg sha "$source_sha" --arg event "$expected_event" \
  --argjson attempt "$producer_run_attempt" '
  .repository.full_name == $repository and
  .head_repository.full_name == $repository and
  .head_sha == $sha and
  .path == ".github/workflows/cli.yml" and
  .event == $event and
  .run_attempt == $attempt and
  (
    .status == "in_progress" or
    (.status == "completed" and .conclusion == "success")
  )
' <<<"$producer_run" >/dev/null || {
  echo "customer-journey result does not bind an eligible exact CLI producer run" >&2
  exit 1
}

if [[ "$source_kind" == pull_request ]]; then
  pull_request=$(gh api --method GET "repos/${repository}/pulls/${pull_request_number}")
  jq -e --arg repository "$repository" --arg sha "$source_sha" --argjson number "$pull_request_number" '
    .number == $number and .state == "open" and
    .head.repo.full_name == $repository and .head.sha == $sha and
    .base.repo.full_name == $repository
  ' <<<"$pull_request" >/dev/null || {
    echo "customer-journey result is not for the current head of the named open pull request" >&2
    exit 1
  }
else
  main_ref=$(gh api --method GET "repos/${repository}/git/ref/heads/main")
  jq -e --arg sha "$source_sha" '.ref == "refs/heads/main" and .object.type == "commit" and .object.sha == $sha' \
    <<<"$main_ref" >/dev/null || {
    echo "customer-journey result is not for current main" >&2
    exit 1
  }
fi

external_id="layerv.qurl-cli-customer-journey.v1:${source_sha}:${producer_run_id}:${producer_run_attempt}"
details_url="${server_url}/${repository}/actions/runs/${GITHUB_RUN_ID}/attempts/${GITHUB_RUN_ATTEMPT}"
title="Exact CLI artifact customer journey passed"
summary="The protected customer journey passed for the exact CLI artifact from producer run ${producer_run_id}, attempt ${producer_run_attempt}."
if [[ "$conclusion" == failure ]]; then
  title="Exact CLI artifact customer journey failed"
  summary="The protected customer journey failed for the exact CLI artifact from producer run ${producer_run_id}, attempt ${producer_run_attempt}."
fi
summary+=" Result authority: run ${orchestrator_run_id}, attempt ${orchestrator_run_attempt}."

request=$(mktemp)
trap 'rm -f -- "$request"' EXIT
jq -n \
  --arg name "qURL Customer Journey / exact CLI artifact" \
  --arg head_sha "$source_sha" \
  --arg external_id "$external_id" \
  --arg details_url "$details_url" \
  --arg conclusion "$conclusion" \
  --arg title "$title" \
  --arg summary "$summary" '
  {
    name: $name,
    head_sha: $head_sha,
    external_id: $external_id,
    details_url: $details_url,
    status: "completed",
    conclusion: $conclusion,
    output: {title: $title, summary: $summary}
  }
' >"$request"

gh api --method POST "repos/${repository}/check-runs" --input "$request" >/dev/null
echo "Accepted the exact CLI artifact customer-journey result for ${source_sha}."
