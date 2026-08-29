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
  echo "::error::customer-journey results are accepted only by the canonical repository" >&2
  exit 1
}
[[ -n "${GH_TOKEN:-}" ]] || { echo "::error::GH_TOKEN is required" >&2; exit 1; }
[[ -f "$event_path" && ! -L "$event_path" ]] || { echo "::error::event payload must be a regular file" >&2; exit 1; }
[[ "${GITHUB_RUN_ID:-}" =~ ^[1-9][0-9]*$ && "${GITHUB_RUN_ATTEMPT:-}" =~ ^[1-9][0-9]*$ ]] || {
  echo "::error::callback workflow run identity is invalid" >&2
  exit 1
}

github_api() {
  local description=$1
  shift
  local attempt output status=1
  for attempt in 1 2 3; do
    if output=$(gh api "$@"); then
      printf '%s\n' "$output"
      return 0
    else
      status=$?
    fi
    if ((attempt < 3)); then
      sleep $((attempt * 2))
    fi
  done
  echo "::error::GitHub API failed to ${description} after 3 attempts" >&2
  return "$status"
}

# GitHub authenticates repository_dispatch senders. Accept only the dedicated
# operations App identity and one closed payload schema. The payload contains
# no endpoint, credential, account, region, or private-repository detail.
# TODO(upstream-contract): Keep the sender identity and result schema in
# lockstep with the protected result producer. The schema uses nine of
# repository_dispatch's ten allowed top-level client_payload fields.
jq -e --arg repository "$repository" '
  .action == "qurl-cli-customer-journey-result" and
  .repository.full_name == $repository and
  .sender.login == "ops-routines-reader[bot]" and
  # Public bot-account user ID from the webhook sender, not a GitHub App ID.
  .sender.id == 277190418 and
  .sender.type == "Bot" and
  (.client_payload | type == "object") and
  ((.client_payload | keys) == [
    "conclusion", "orchestrator_run_attempt", "orchestrator_run_id",
    "producer_run_attempt", "producer_run_id", "pull_request_number",
    "schema", "source_kind", "source_sha"
  ]) and
  .client_payload.schema == "layerv.qurl-cli-customer-journey-result.v1" and
  (.client_payload.source_sha | type == "string" and length == 40 and test("^[0-9a-f]{40}$")) and
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
  echo "::error::repository-dispatch customer-journey result is not trusted or is malformed" >&2
  exit 1
}

mapfile -t fields < <(jq -r '
  .client_payload |
  .source_sha, .source_kind, (.pull_request_number | floor | tostring),
  (.producer_run_id | floor | tostring), (.producer_run_attempt | floor | tostring),
  (.orchestrator_run_id | floor | tostring), (.orchestrator_run_attempt | floor | tostring),
  .conclusion
' "$event_path")
if ((${#fields[@]} != 8)); then
  echo "::error::customer-journey result fields are incomplete" >&2
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

if [[ "$source_kind" == pull_request ]]; then
  pull_request=$(github_api "read the named pull request" --method GET \
    "repos/${repository}/pulls/${pull_request_number}")
  jq -e --arg repository "$repository" --argjson number "$pull_request_number" '
    .number == $number and
    .head.repo.full_name == $repository and
    .base.repo.full_name == $repository
  ' <<<"$pull_request" >/dev/null || {
    echo "::error::customer-journey result does not bind a same-repository pull request" >&2
    exit 1
  }
  jq -e --arg sha "$source_sha" '.state == "open" and .head.sha == $sha' \
    <<<"$pull_request" >/dev/null || {
    echo "::notice::Ignoring a customer-journey result for a superseded or closed pull request head"
    exit 0
  }
else
  main_ref=$(github_api "read current main" --method GET \
    "repos/${repository}/git/ref/heads/main")
  jq -e --arg sha "$source_sha" '.ref == "refs/heads/main" and .object.type == "commit" and .object.sha == $sha' \
    <<<"$main_ref" >/dev/null || {
    echo "::notice::Ignoring a customer-journey result for superseded main"
    # Only the current tip receives a check. A later main push owns its own
    # exact artifact and journey; accepting an ancestor result would let an
    # older callback satisfy a newer release decision.
    exit 0
  }
fi

producer_run=$(github_api "read the exact CLI producer attempt" --method GET \
  "repos/${repository}/actions/runs/${producer_run_id}/attempts/${producer_run_attempt}")
expected_event=push
if [[ "$source_kind" == pull_request ]]; then
  expected_event=pull_request
fi
# The CLI workflow dispatches this journey only after every local quality gate
# has passed. A first delivery therefore belongs to an in-progress run. A
# completed successful run is accepted for an explicit callback replay; a
# failed, cancelled, queued, or unrelated run cannot mint a journey check.
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
  echo "::error::customer-journey result does not bind an eligible exact CLI producer run" >&2
  exit 1
}

# TODO(upstream-contract): Keep the exact producer job and step names in
# lockstep with .github/workflows/cli.yml and the protected artifact consumer.
producer_jobs=$(github_api "read the exact CLI producer jobs" --method GET \
  "repos/${repository}/actions/runs/${producer_run_id}/attempts/${producer_run_attempt}/jobs?per_page=100")
jq -e '.total_count <= 100 and (.jobs | length) == .total_count' <<<"$producer_jobs" >/dev/null || {
  echo "::error::exact CLI producer job response is truncated or inconsistent" >&2
  exit 1
}
jq -e '
  ([.jobs[] | select(.name == "cli / sandbox matched-cohort artifacts")]) as $matches |
  ($matches | length) == 1 and
  ($matches[0] as $job |
    $job.status == "completed" and $job.conclusion == "success" and
    (["Build exact sandbox customer artifacts", "Upload exact sandbox customer binaries",
      "Upload exact sandbox customer source receipt"] |
     all(. as $step | [$job.steps[] | select(.name == $step and .conclusion == "success")] | length == 1)))
' <<<"$producer_jobs" >/dev/null || {
  echo "::error::customer-journey result does not bind the exact successful CLI artifact producer" >&2
  exit 1
}

# TODO(upstream-contract): Keep this check name and external-ID schema in
# lockstep with the polling gate. Same-repository pull-request workflows can
# create checks under the same GitHub Actions App identity; required review of
# workflow permission changes is part of this trusted-insider boundary. A
# replay can create another check with the same key; the gate selects newest.
# Payload fields also form the workflow concurrency key before this script can
# authenticate them. A sender that can exploit that is already inside this
# repository-write trusted boundary; the closed envelope still rejects it here.
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

# Check creation is not idempotent. Do not use the read retry wrapper: if the
# server creates the check but the response is lost, a retry would create a
# duplicate from one callback run. A replay is safe because it has a distinct
# workflow run and the exact waiter selects the newest matching external ID.
if ! gh api --method POST "repos/${repository}/check-runs" --input "$request" >/dev/null; then
  echo "::error::GitHub API failed to record the exact customer-journey check" >&2
  exit 1
fi
echo "Accepted the exact CLI artifact customer-journey result for ${source_sha}."
