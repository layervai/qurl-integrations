#!/usr/bin/env bash
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
subject="$root/scripts/wait-for-exact-cli-main-run.sh"
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
mkdir -p "$work/bin"

cat >"$work/bin/sleep" <<'SH'
#!/usr/bin/env bash
exit 0
SH

cat >"$work/bin/gh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail

if [[ "$*" == *"/check-runs"* ]]; then
  count=$(cat "$FAKE_CHECK_CALL_COUNT")
  count=$((count + 1))
  printf '%s\n' "$count" >"$FAKE_CHECK_CALL_COUNT"
  case "$SCENARIO" in
    check_api_failure) exit 1 ;;
    check_api_failure_then_success) if ((count == 1)); then exit 1; fi ;;
    check_malformed) printf '{not-json\n'; exit 0 ;;
    check_absent)
      jq -n '{total_count:0,check_runs:[]}'
      exit 0
      ;;
    check_truncated)
      jq -n '{total_count:101,check_runs:[]}'
      exit 0
      ;;
  esac
  status=completed
  conclusion=success
  source_run_id=700
  [[ "$SCENARIO" != multiple ]] || source_run_id=701
  [[ "$SCENARIO" != check_pending_then_success || "$count" -ge 2 ]] || { status=in_progress; conclusion=null; }
  [[ "$SCENARIO" != check_failure ]] || conclusion=failure
  external="layerv.qurl-cli-customer-journey.v1:${source_run_id}:2:${EXPECTED_SHA}"
  [[ "$SCENARIO" != check_wrong_external ]] || external="layerv.qurl-cli-customer-journey.v1:999:2:${EXPECTED_SHA}"
  jq -n --arg status "$status" --arg conclusion "$conclusion" --arg head "$EXPECTED_SHA" --arg external "$external" '{
    total_count:1,
    check_runs:[{
      name:"qURL Customer Journey / exact CLI artifact",head_sha:$head,external_id:$external,
      app:{slug:"github-actions"},status:$status,
      conclusion:(if $conclusion == "null" then null else $conclusion end),
      details_url:"https://example.invalid/journey"
    }]
  }'
  exit 0
fi

if [[ "$*" == *"/attempts/"*"/jobs"* ]]; then
  count=$(cat "$FAKE_JOB_CALL_COUNT")
  count=$((count + 1))
  printf '%s\n' "$count" >"$FAKE_JOB_CALL_COUNT"
  case "$SCENARIO" in
    jobs_api_failure) exit 1 ;;
    jobs_api_failure_then_success) if ((count == 1)); then exit 1; fi ;;
    jobs_malformed) printf '{not-json\n'; exit 0 ;;
  esac
  case "$SCENARIO" in
    jobs_missing)
      jq -n '{total_count:1,jobs:[{name:"cli / customer journey artifacts",status:"completed",conclusion:"success"}]}'
      ;;
    jobs_skipped)
      jq -n '{total_count:2,jobs:[
        {name:"cli / customer journey artifacts",status:"completed",conclusion:"skipped"},
        {name:"cli / required",status:"completed",conclusion:"success"}
      ]}'
      ;;
    jobs_required_failed)
      jq -n '{total_count:2,jobs:[
        {name:"cli / customer journey artifacts",status:"completed",conclusion:"success"},
        {name:"cli / required",status:"completed",conclusion:"failure"}
      ]}'
      ;;
    jobs_truncated)
      jq -n '{total_count:101,jobs:[
        {name:"cli / customer journey artifacts",status:"completed",conclusion:"success"},
        {name:"cli / required",status:"completed",conclusion:"success"}
      ]}'
      ;;
    *)
      jq -n '{total_count:2,jobs:[
        {name:"cli / customer journey artifacts",status:"completed",conclusion:"success"},
        {name:"cli / required",status:"completed",conclusion:"success"}
      ]}'
      ;;
  esac
  exit 0
fi

count=$(cat "$FAKE_RUN_CALL_COUNT")
count=$((count + 1))
printf '%s\n' "$count" >"$FAKE_RUN_CALL_COUNT"

case "$SCENARIO" in
  success|jobs_*|check_*) status=completed; conclusion=success ;;
  failure) status=completed; conclusion=failure ;;
  pending_then_success)
    status=in_progress; conclusion=null
    if ((count >= 2)); then status=completed; conclusion=success; fi
    ;;
  absent)
    jq -n '{total_count:0,workflow_runs:[]}'
    exit 0
    ;;
  multiple) copies=2; status=completed; conclusion=success ;;
  newest_failure)
    jq -n --arg head "$EXPECTED_SHA" '{total_count:2,workflow_runs:[
      {id:700,run_attempt:2,repository:{full_name:"layervai/qurl-integrations"},head_repository:{full_name:"layervai/qurl-integrations"},head_sha:$head,path:".github/workflows/cli.yml",event:"push",status:"completed",conclusion:"success",html_url:"https://example.invalid/old"},
      {id:701,run_attempt:2,repository:{full_name:"layervai/qurl-integrations"},head_repository:{full_name:"layervai/qurl-integrations"},head_sha:$head,path:".github/workflows/cli.yml",event:"push",status:"completed",conclusion:"failure",html_url:"https://example.invalid/new"}
    ]}'
    exit 0
    ;;
  wrong_sha) status=completed; conclusion=success; head=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ;;
  malformed) printf '{not-json\n'; exit 0 ;;
  api_failure) exit 1 ;;
  api_failure_then_success)
    if ((count == 1)); then exit 1; fi
    status=completed; conclusion=success
    ;;
  *) echo "unknown scenario" >&2; exit 2 ;;
esac

copies=${copies:-1}
head=${head:-$EXPECTED_SHA}
jq -n --arg status "$status" --arg conclusion "$conclusion" --arg head "$head" --argjson copies "$copies" '
  {total_count:$copies,workflow_runs:[range(0;$copies) | {
    id:(700 + .),run_attempt:2,
    repository:{full_name:"layervai/qurl-integrations"},
    head_repository:{full_name:"layervai/qurl-integrations"},
    head_sha:$head,path:".github/workflows/cli.yml",event:"push",
    status:$status,conclusion:(if $conclusion == "null" then null else $conclusion end),
    html_url:"https://example.invalid/cli"
  }]}
'
SH
chmod +x "$work/bin/gh" "$work/bin/sleep"

sha=17d077fbc5a50d54894d5521be623fe03420de14
run_case() {
  local scenario=$1 expected_status=$2
  printf '0\n' >"$work/run-calls"
  printf '0\n' >"$work/job-calls"
  printf '0\n' >"$work/check-calls"
  set +e
  output=$(env \
    PATH="$work/bin:$PATH" \
    GH_TOKEN=test-token \
    GITHUB_REPOSITORY=layervai/qurl-integrations \
    QURL_CLI_MAIN_WAIT_SECONDS=5 \
    QURL_CLI_MAIN_POLL_SECONDS=1 \
    QURL_CLI_MAIN_MAX_API_FAILURES=2 \
    FAKE_RUN_CALL_COUNT="$work/run-calls" \
    FAKE_JOB_CALL_COUNT="$work/job-calls" \
    FAKE_CHECK_CALL_COUNT="$work/check-calls" \
    EXPECTED_SHA="$sha" \
    SCENARIO="$scenario" \
    "$subject" "$sha" 2>&1)
  status=$?
  set -e
  if [[ "$status" != "$expected_status" ]]; then
    echo "$scenario returned $status, expected $expected_status: $output" >&2
    exit 1
  fi
  if [[ "$scenario" == success ]]; then
    jq -e '.run_id == 700 and .run_attempt == 2 and .journey_url == "https://example.invalid/journey"' <<<"$output" >/dev/null
  fi
}

run_case success 0
[[ $(cat "$work/run-calls") == 1 && $(cat "$work/job-calls") == 1 && $(cat "$work/check-calls") == 1 ]]
run_case failure 1
[[ $(cat "$work/run-calls") == 1 && $(cat "$work/job-calls") == 0 ]]
run_case pending_then_success 0
[[ $(cat "$work/run-calls") == 2 && $(cat "$work/job-calls") == 1 ]]
run_case absent 1
run_case multiple 0
jq -e '.run_id == 701' <<<"$output" >/dev/null
run_case newest_failure 1
run_case wrong_sha 1
run_case malformed 1
run_case api_failure 1
[[ $(cat "$work/run-calls") == 2 ]]
run_case api_failure_then_success 0
[[ $(cat "$work/run-calls") == 2 && $(cat "$work/job-calls") == 1 ]]
run_case jobs_missing 1
run_case jobs_skipped 1
run_case jobs_required_failed 1
run_case jobs_truncated 1
run_case jobs_malformed 1
run_case jobs_api_failure 1
[[ $(cat "$work/run-calls") == 2 && $(cat "$work/job-calls") == 2 ]]
run_case jobs_api_failure_then_success 0
[[ $(cat "$work/run-calls") == 2 && $(cat "$work/job-calls") == 2 ]]
run_case check_absent 1
run_case check_pending_then_success 0
[[ $(cat "$work/check-calls") == 2 ]]
run_case check_failure 1
run_case check_wrong_external 1
run_case check_truncated 1
run_case check_malformed 1
run_case check_api_failure 1
[[ $(cat "$work/check-calls") == 2 ]]
run_case check_api_failure_then_success 0
[[ $(cat "$work/check-calls") == 2 ]]

echo "wait-for-exact-cli-main-run tests passed"
