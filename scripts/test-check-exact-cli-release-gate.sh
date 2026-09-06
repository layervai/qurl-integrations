#!/usr/bin/env bash
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
fixture=$(mktemp -d)
trap 'rm -rf -- "$fixture"' EXIT
sha=0123456789abcdef0123456789abcdef01234567

cat >"$fixture/gh" <<'GH'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == *actions/runs/700 && "$*" != *attempts* ]]; then
  run_sha=$SOURCE_SHA
  if [[ "$SCENARIO" == mismatch ]]; then
    run_sha=ffffffffffffffffffffffffffffffffffffffff
  fi
  jq -n --arg sha "$run_sha" '{
    id:700,run_attempt:2,event:"workflow_dispatch",head_branch:"main",head_sha:$sha,
    path:".github/workflows/cli.yml",html_url:"https://example.invalid/run/700",
    repository:{full_name:"layervai/qurl-integrations"},
    head_repository:{full_name:"layervai/qurl-integrations"}
  }'
  exit 0
fi
required_status=completed
journey_status=completed
cleanup_status=completed
required_conclusion=success
journey_conclusion=success
cleanup_conclusion=success
case "$SCENARIO" in
  incomplete) journey_status=in_progress; cleanup_status=queued ;;
  failed) journey_conclusion=failure ;;
  required_failed) required_conclusion=failure ;;
  cleanup_failed) cleanup_conclusion=failure ;;
  missing) journey_count=3 ;;
esac
journey_count=${journey_count:-4}
jq -n --arg required_status "$required_status" --arg journey_status "$journey_status" \
  --arg cleanup_status "$cleanup_status" --arg required_conclusion "$required_conclusion" \
  --arg journey_conclusion "$journey_conclusion" --arg cleanup_conclusion "$cleanup_conclusion" \
  --argjson journey_count "$journey_count" '{
    jobs: (
      [{name:"cli / required",status:$required_status,conclusion:$required_conclusion}] +
      [range(0; $journey_count) | {
        name:("cli / customer journey (lane-" + tostring + ")"),
        status:$journey_status,
        conclusion:$journey_conclusion
      }] +
      [{name:"cli / customer journey cleanup",status:$cleanup_status,conclusion:$cleanup_conclusion}]
    )
  } | .total_count = (.jobs | length)
'
GH
chmod +x "$fixture/gh"

run_case() {
  local scenario=$1 expected_status=$2 expected=$3
  local output status=0
  output=$(PATH="$fixture:$PATH" SCENARIO="$scenario" SOURCE_SHA="$sha" \
    GH_TOKEN=test GITHUB_REPOSITORY=layervai/qurl-integrations \
    "$root/scripts/check-exact-cli-release-gate.sh" "$sha" 700 2 2>&1) || status=$?
  [[ "$status" == "$expected_status" && "$output" == *"$expected"* ]] || {
    echo "$scenario: status=$status output=$output" >&2
    exit 1
  }
}

run_case success 0 '"ready":true'
run_case incomplete 0 '"reason":"cli_release_journey_incomplete"'
run_case failed 1 'Exact CLI customer-journey gate failed'
run_case required_failed 1 'cli / required=failure'
run_case cleanup_failed 1 'cli / customer journey cleanup=failure'
run_case missing 1 'gate set is missing or ambiguous'
run_case mismatch 1 'run does not match the exact handoff'
echo "exact CLI release gate tests passed"
