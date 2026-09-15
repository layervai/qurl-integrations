#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 5 ]]; then
  echo "usage: $0 <workflow-runs-json> <jobs-directory> <expected-run-not-before-epoch> <active-grace-seconds> <expected-journey-count>" >&2
  exit 2
fi

runs_file=$1
jobs_dir=$2
expected_after=$3
active_grace=$4
expected_journeys=$5
if [[ ! -f "$runs_file" || -L "$runs_file" ]]; then
  echo "workflow runs must be one regular non-symlink file" >&2
  exit 2
fi
if [[ ! -d "$jobs_dir" || -L "$jobs_dir" ]]; then
  echo "jobs directory must be one real directory" >&2
  exit 2
fi
for value_name in expected_after active_grace expected_journeys; do
  value=${!value_name}
  if [[ ! "$value" =~ ^[1-9][0-9]*$ ]]; then
    echo "$value_name must be a positive integer" >&2
    exit 2
  fi
done

now_epoch=${QURL_CLI_WATCHDOG_NOW_EPOCH:-$(date +%s)}
if [[ ! "$now_epoch" =~ ^[1-9][0-9]*$ ]]; then
  echo "QURL_CLI_WATCHDOG_NOW_EPOCH must be a positive integer" >&2
  exit 2
fi
if ((expected_after > now_epoch)); then
  echo "expected run boundary cannot be in the future" >&2
  exit 2
fi

if ! jq -e '
  (.workflow_runs | type) == "array" and
  (.workflow_runs | length) <= 100 and
  all(.workflow_runs[];
    (.id | type) == "number" and .id > 0 and
    (.run_attempt | type) == "number" and .run_attempt > 0 and
    (.event | type) == "string" and
    (.status | type) == "string" and
    ((.run_started_at | type) == "string" or .run_started_at == null) and
    (.created_at | type) == "string")
' "$runs_file" >/dev/null; then
  echo "workflow run history is malformed" >&2
  exit 2
fi

timestamp_filter='def epoch: sub("\\.[0-9]+Z$"; "Z") | fromdateiso8601;'
# TODO(upstream-contract): GitHub keeps created_at fixed across re-runs and
# advances run_started_at for the current attempt. The cohort uses created_at;
# the active-attempt grace uses run_started_at.
if jq -e --argjson expected "$expected_after" --argjson now "$now_epoch" \
    --argjson grace "$active_grace" "
  $timestamp_filter
  any(.workflow_runs[];
    .event == \"schedule\" and
    (.status == \"queued\" or .status == \"in_progress\" or
      .status == \"waiting\" or .status == \"requested\" or
      .status == \"pending\") and
    (.created_at | epoch) >= \$expected and
    (.created_at | epoch) <= \$now and
    ((.run_started_at // .created_at) | epoch) <= \$now and
    (\$now - ((.run_started_at // .created_at) | epoch)) <= \$grace)
" "$runs_file" >/dev/null; then
  echo "today's scheduled qURL CLI soak attempt is still active inside its grace window"
  exit 0
fi

if ! candidates=$(jq -r --argjson expected "$expected_after" --argjson now "$now_epoch" "
  $timestamp_filter
  [.workflow_runs[] |
    select(.event == \"schedule\" and .status == \"completed\") |
    select((.created_at | epoch) >= \$expected and (.created_at | epoch) <= \$now) |
    [.id, .run_attempt, (.conclusion // \"\")]] |
  sort_by(.[0]) | reverse | .[] | @tsv
" "$runs_file"); then
  echo "workflow run timestamps are invalid" >&2
  exit 2
fi

while IFS=$'\t' read -r run_id run_attempt run_conclusion; do
  [[ -n "$run_id" ]] || continue
  jobs_file=$jobs_dir/$run_id-$run_attempt.json
  if [[ ! -f "$jobs_file" || -L "$jobs_file" ]] ||
     ! jq -e '
       (.total_count | type) == "number" and
       .total_count >= 0 and
       (.jobs | type) == "array" and
       (.jobs | length) == .total_count and
       all(.jobs[];
         (.name | type) == "string" and
         ((.conclusion | type) == "string" or .conclusion == null))
     ' "$jobs_file" >/dev/null; then
    echo "job results for scheduled run $run_id attempt $run_attempt are missing, malformed, or truncated" >&2
    exit 2
  fi
  # Check cleanup directly as defense in depth. The required aggregate also
  # owns cleanup today, but this keeps the Slack claim safe if that contract
  # changes without the watchdog changing with it.
  if jq -e --argjson expected_journeys "$expected_journeys" '
    ([.jobs[] | select(.name == "cli / required" and .conclusion == "success")] | length) == 1 and
    ([.jobs[] | select(.name == "cli / customer journey cleanup" and .conclusion == "success")] | length) == 1 and
    # The opening parenthesis keeps the matrix lanes distinct from the
    # adjacent "cli / customer journey cleanup" job.
    ([.jobs[] | select(.name | startswith("cli / customer journey ("))] | length) == $expected_journeys and
    all(.jobs[] | select(.name | startswith("cli / customer journey (")); .conclusion == "success")
  ' "$jobs_file" >/dev/null; then
    echo "today's scheduled qURL CLI validation and all $expected_journeys customer journeys passed"
    exit 0
  fi
  # TODO(upstream-contract): Keep this partition in lockstep with the GitHub
  # workflow_run conclusions admitted by main-ci-notifications.yml.
  case "$run_conclusion" in
    failure | timed_out | cancelled)
      completed_failure_notified=true
      ;;
  esac
done <<<"$candidates"

if [[ "${completed_failure_notified:-false}" == true ]]; then
  echo "today's scheduled qURL CLI run completed; the main failure notifier owns any failed validation alert"
  exit 0
fi

echo "today's scheduled qURL CLI validation did not produce one complete pass" >&2
exit 1
