#!/usr/bin/env bash
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
fixture=$(mktemp -d)
trap 'rm -rf -- "$fixture"' EXIT
sha=0123456789abcdef0123456789abcdef01234567

cat >"$fixture/gh" <<'GH'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == *actions/workflows/cli.yml/runs* ]]; then
  [[ " $* " == *" -f event=workflow_dispatch "* ]]
  if [[ "$SCENARIO" == absent ]]; then
    jq -n '{total_count:0,workflow_runs:[]}'
  else
    jq -n --arg sha "$SOURCE_SHA" '{total_count:1,workflow_runs:[{
      id:700,run_attempt:2,event:"workflow_dispatch",head_branch:"main",head_sha:$sha,
      path:".github/workflows/cli.yml",html_url:"https://example.invalid/run/700",
      repository:{full_name:"layervai/qurl-integrations"},
      head_repository:{full_name:"layervai/qurl-integrations"}
    }]}'
  fi
  exit 0
fi
status=completed
journey_conclusion=success
case "$SCENARIO" in
  incomplete) status=in_progress ;;
  failed) journey_conclusion=failure ;;
  missing) journey_count=3 ;;
esac
journey_count=${journey_count:-4}
jq -n --arg status "$status" --arg journey_conclusion "$journey_conclusion" \
  --argjson journey_count "$journey_count" '{
    jobs: (
      [{name:"cli / required",status:$status,conclusion:"success"}] +
      [range(0; $journey_count) | {
        name:("cli / customer journey (lane-" + tostring + ")"),
        status:$status,
        conclusion:$journey_conclusion
      }] +
      [{name:"cli / customer journey cleanup",status:$status,conclusion:"success"}]
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
    "$root/scripts/check-exact-cli-release-gate.sh" "$sha" 2>&1) || status=$?
  [[ "$status" == "$expected_status" && "$output" == *"$expected"* ]] || {
    echo "$scenario: status=$status output=$output" >&2
    exit 1
  }
}

run_case success 0 '"ready":true'
run_case incomplete 0 '"reason":"cli_release_journey_incomplete"'
run_case absent 0 '"reason":"cli_release_journey_incomplete"'
run_case failed 1 'Exact CLI customer-journey gate failed'
run_case missing 1 'gate set is missing or ambiguous'
echo "exact CLI release gate tests passed"
