#!/usr/bin/env bash
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
fixture=$(mktemp -d)
trap 'rm -rf -- "$fixture"' EXIT
sha=0123456789abcdef0123456789abcdef01234567

cat >"$fixture/gh" <<'GH'
#!/usr/bin/env bash
set -euo pipefail
if [[ -n "${GH_CALL_CAPTURE:-}" ]]; then
  printf '%s\n' "$*" >>"$GH_CALL_CAPTURE"
fi
if [[ "$SCENARIO" == http_404 ]]; then
  printf 'HTTP/2.0 404 Not Found\n'
  exit 1
fi
if [[ "$SCENARIO" == unavailable ]]; then
  exit 1
fi
if [[ "$SCENARIO" == jobs_unavailable && "$*" == *attempts* ]]; then
  exit 1
fi
if [[ "$SCENARIO" == stderr_warning ]]; then
  echo "gh: simulated warning" >&2
fi
if [[ "$*" == *"--include"* ]]; then
  printf 'HTTP/2.0 200 OK\r\nContent-Type: application/json\r\n\r\n'
fi
if [[ "$*" == *"commits/v1.2.3"* ]]; then
  jq -n --arg sha "$SOURCE_SHA" '{sha:$sha}'
  exit 0
fi
if [[ "$*" == *"/compare/"* ]]; then
  if [[ "$SCENARIO" == mismatch ]]; then
    jq -n '{status:"diverged",merge_base_commit:{sha:"ffffffffffffffffffffffffffffffffffffffff"}}'
  else
    jq -n --arg sha "$SOURCE_SHA" '{status:"ahead",merge_base_commit:{sha:$sha}}'
  fi
  exit 0
fi
if [[ "$*" == *actions/runs/700 && "$*" != *attempts* ]]; then
  run_sha=$SOURCE_SHA
  display_title="CLI release gate $SOURCE_SHA v1.2.3"
  head_branch=main
  if [[ "$SCENARIO" == mismatch ]]; then
    run_sha=ffffffffffffffffffffffffffffffffffffffff
  fi
  if [[ "$SCENARIO" == operator ]]; then
    display_title="Operator CLI soak"
  fi
  if [[ "$SCENARIO" == wrong_tag ]]; then
    display_title="CLI release gate $SOURCE_SHA v1.2.4"
  fi
  jq -n --arg sha "$run_sha" --arg display_title "$display_title" --arg head_branch "$head_branch" '{
    id:700,run_attempt:2,event:"workflow_dispatch",head_branch:$head_branch,head_sha:$sha,
    display_title:$display_title,
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
attempt=2
if [[ "$*" =~ /attempts/([0-9]+)/jobs ]]; then
  attempt=${BASH_REMATCH[1]}
fi
case "$SCENARIO" in
  incomplete) journey_status=in_progress; cleanup_status=queued ;;
  failed) journey_conclusion=failure ;;
  required_failed) required_conclusion=failure ;;
  cleanup_failed) cleanup_conclusion=failure ;;
  missing) journey_count=3 ;;
  partial_rerun)
    if [[ "$attempt" != 1 ]]; then
      journey_count=1
    fi
    ;;
esac
journey_count=${journey_count:-4}
jq -n --arg required_status "$required_status" --arg journey_status "$journey_status" \
  --arg cleanup_status "$cleanup_status" --arg required_conclusion "$required_conclusion" \
  --arg journey_conclusion "$journey_conclusion" --arg cleanup_conclusion "$cleanup_conclusion" \
  --arg scenario "$SCENARIO" --arg attempt "$attempt" \
  --argjson journey_count "$journey_count" '{
    jobs: (
      [{name:"cli / required",status:$required_status,conclusion:$required_conclusion}] +
      [range(0; $journey_count) | {
        name:("cli / customer journey (lane-" + tostring + ")"),
        status:$journey_status,
        conclusion:(if $scenario == "partial_rerun" and $attempt == "1" and . == 0
          then "failure" else $journey_conclusion end)
      }] +
      [{name:"cli / customer journey cleanup",status:$cleanup_status,conclusion:$cleanup_conclusion}]
    )
  } |
  if $scenario == "missing_cleanup" then
    .jobs |= map(select(.name != "cli / customer journey cleanup"))
  elif $scenario == "duplicate" then
    .jobs += [{name:"cli / required",status:$required_status,conclusion:$required_conclusion}]
  else . end |
  .total_count = (.jobs | length) |
  if $scenario == "malformed" then .total_count += 1 else . end
'
GH
chmod +x "$fixture/gh"

run_case() {
  local scenario=$1 expected_status=$2 expected=$3
  local output status=0
  output=$(PATH="$fixture:$PATH" SCENARIO="$scenario" SOURCE_SHA="$sha" \
    GH_TOKEN=test GITHUB_REPOSITORY=layervai/qurl-integrations \
    "$root/scripts/check-exact-cli-release-gate.sh" "$sha" v1.2.3 700 2 4 2>&1) || status=$?
  [[ "$status" == "$expected_status" && "$output" == *"$expected"* ]] || {
    echo "$scenario: status=$status output=$output" >&2
    exit 1
  }
}

run_case success 0 '"journey_url":"https://example.invalid/run/700"'
run_case stderr_warning 0 '"journey_url":"https://example.invalid/run/700"'
run_case incomplete 1 '"reason":"cli_release_journey_incomplete"'
run_case failed 1 'Exact CLI customer-journey gate failed'
run_case required_failed 1 'cli / required=failure'
run_case cleanup_failed 1 'cli / customer journey cleanup=failure'
run_case missing 1 'gate set is missing or ambiguous'
run_case missing_cleanup 1 'gate set is missing or ambiguous'
run_case duplicate 1 '::error::CLI release job data is malformed or ambiguous'
run_case malformed 1 '::error::CLI release job data is malformed or ambiguous'
run_case mismatch 1 'release source is not an ancestor'
run_case operator 1 'run does not match the exact handoff'
run_case wrong_tag 1 'run does not match the exact handoff'
run_case partial_rerun 0 '"journey_url":"https://example.invalid/run/700"'
run_case unavailable 1 '::error::CLI release run lookup failed'
run_case jobs_unavailable 1 '::error::CLI release job lookup failed'

early_capture=$fixture/early-stop-calls
output=""
status=0
output=$(PATH="$fixture:$PATH" SCENARIO=success SOURCE_SHA="$sha" \
  GH_CALL_CAPTURE="$early_capture" GH_TOKEN=test GITHUB_REPOSITORY=layervai/qurl-integrations \
  "$root/scripts/check-exact-cli-release-gate.sh" "$sha" v1.2.3 700 2 4 2>&1) || status=$?
[[ "$status" == 0 && "$output" == *'"journey_url":"https://example.invalid/run/700"'* && \
  "$(cat "$early_capture")" == *'/attempts/2/jobs?per_page=100'* && \
  "$(cat "$early_capture")" != *'/attempts/1/jobs?per_page=100'* ]] || {
  echo "early-stop: status=$status output=$output calls=$(cat "$early_capture")" >&2
  exit 1
}

http_capture=$fixture/http-404-calls
output=""
status=0
output=$(PATH="$fixture:$PATH" SCENARIO=http_404 SOURCE_SHA="$sha" \
  GH_CALL_CAPTURE="$http_capture" GH_TOKEN=test GITHUB_REPOSITORY=layervai/qurl-integrations \
  "$root/scripts/check-exact-cli-release-gate.sh" "$sha" v1.2.3 700 2 4 2>&1) || status=$?
[[ "$status" == 1 && "$output" == *"GitHub API rejected"* && "$(wc -l <"$http_capture" | tr -d ' ')" == 1 ]] || {
  echo "http_404: status=$status output=$output" >&2
  exit 1
}

for malformed in 0 01 -1 1.0 900719925474099300000; do
  run_case_name="malformed-$malformed"
  output=""
  status=0
  output=$(PATH="$fixture:$PATH" SCENARIO=success SOURCE_SHA="$sha" \
    GH_TOKEN=test GITHUB_REPOSITORY=layervai/qurl-integrations \
    "$root/scripts/check-exact-cli-release-gate.sh" "$sha" v1.2.3 "$malformed" 2 4 2>&1) || status=$?
  [[ "$status" == 1 && "$output" == *"input is incomplete"* ]] || {
    echo "$run_case_name: status=$status output=$output" >&2
    exit 1
  }
done
echo "exact CLI release gate tests passed"
