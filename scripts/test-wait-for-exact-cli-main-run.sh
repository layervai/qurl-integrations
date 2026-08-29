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
count=$(cat "$FAKE_CALL_COUNT")
count=$((count + 1))
printf '%s\n' "$count" >"$FAKE_CALL_COUNT"

case "$SCENARIO" in
  success) status=completed; conclusion=success ;;
  failure) status=completed; conclusion=failure ;;
  pending_then_success)
    status=in_progress; conclusion=null
    if ((count >= 2)); then status=completed; conclusion=success; fi
    ;;
  absent)
    jq -n '{total_count:0,workflow_runs:[]}'
    exit 0
    ;;
  ambiguous)
    copies=2; status=completed; conclusion=success
    ;;
  wrong_sha) status=completed; conclusion=success; head=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ;;
  malformed) printf '{not-json\n'; exit 0 ;;
  api_failure) exit 1 ;;
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
  printf '0\n' >"$work/calls"
  set +e
  output=$(env \
    PATH="$work/bin:$PATH" \
    GH_TOKEN=test-token \
    GITHUB_REPOSITORY=layervai/qurl-integrations \
    QURL_CLI_MAIN_WAIT_SECONDS=1 \
    QURL_CLI_MAIN_POLL_SECONDS=1 \
    FAKE_CALL_COUNT="$work/calls" \
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
    jq -e '.run_id == 700 and .run_attempt == 2' <<<"$output" >/dev/null
  fi
}

run_case success 0
run_case failure 1
run_case pending_then_success 0
[[ $(cat "$work/calls") == 2 ]]
run_case absent 1
run_case ambiguous 1
run_case wrong_sha 1
run_case malformed 1
run_case api_failure 1

echo "wait-for-exact-cli-main-run tests passed"
