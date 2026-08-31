#!/usr/bin/env bash
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
subject="$root/scripts/check-exact-cli-release-gate.sh"
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
mkdir -p "$work/bin"

cat >"$work/bin/gh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail

if [[ "$*" == *"/check-runs"* ]]; then
  status=completed
  conclusion=success
  [[ "$SCENARIO" != journey_pending ]] || { status=in_progress; conclusion=null; }
  [[ "$SCENARIO" != journey_failed ]] || conclusion=failure
  jq -n --arg sha "$EXPECTED_SHA" --arg status "$status" --arg conclusion "$conclusion" '{
    total_count:1,
    check_runs:[{
      name:"qURL Customer Journey / exact CLI artifact",
      head_sha:$sha,
      external_id:("layerv.qurl-cli-customer-journey.v1:700:2:" + $sha),
      status:$status,
      conclusion:(if $conclusion == "null" then null else $conclusion end),
      details_url:"https://example.test/journey",
      app:{slug:"github-actions"}
    }]
  }'
elif [[ "$*" == *"/attempts/2/jobs"* ]]; then
  jq -n '{total_count:2,jobs:[
    {name:"cli / customer journey artifacts",status:"completed",conclusion:"success"},
    {name:"cli / required",status:"completed",conclusion:"success"}
  ]}'
else
  status=completed
  conclusion=success
  [[ "$SCENARIO" != cli_absent ]] || { jq -n '{total_count:0,workflow_runs:[]}'; exit 0; }
  [[ "$SCENARIO" != cli_pending ]] || { status=in_progress; conclusion=null; }
  [[ "$SCENARIO" != cli_failed ]] || conclusion=failure
  jq -n --arg sha "$EXPECTED_SHA" --arg status "$status" --arg conclusion "$conclusion" '{
    total_count:1,
    workflow_runs:[{
      id:700,
      run_attempt:2,
      status:$status,
      conclusion:(if $conclusion == "null" then null else $conclusion end),
      html_url:"https://example.test/cli",
      head_sha:$sha,
      path:".github/workflows/cli.yml",
      event:"push",
      repository:{full_name:"layervai/qurl-integrations"},
      head_repository:{full_name:"layervai/qurl-integrations"}
    }]
  }'
fi
SH
chmod +x "$work/bin/gh"

sha=0123456789abcdef0123456789abcdef01234567
run_case() {
  local scenario=$1 want_status=$2 want_text=$3
  local output status=0
  output=$(PATH="$work/bin:$PATH" SCENARIO="$scenario" EXPECTED_SHA="$sha" \
    GITHUB_REPOSITORY=layervai/qurl-integrations GH_TOKEN=test "$subject" "$sha" 2>&1) || status=$?
  [[ "$status" -eq "$want_status" ]] || {
    echo "$scenario: status $status, want $want_status: $output" >&2
    exit 1
  }
  grep -Fq "$want_text" <<<"$output" || {
    echo "$scenario: missing $want_text: $output" >&2
    exit 1
  }
}

run_case success 0 '"ready":true'
run_case cli_absent 0 '"reason":"cli_main_incomplete"'
run_case cli_pending 0 '"reason":"cli_main_incomplete"'
run_case journey_pending 0 '"reason":"customer_journey_incomplete"'
run_case cli_failed 1 'Exact CLI main workflow concluded failure'
run_case journey_failed 1 'Exact packaged CLI customer journey concluded failure'

echo "check-exact-cli-release-gate tests passed"
