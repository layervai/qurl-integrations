#!/usr/bin/env bash
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
subject="$root/scripts/wait-for-cli-customer-journey-check.sh"
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
  success)
    status=completed conclusion=success external_id=$EXPECTED_EXTERNAL app=github-actions id=2
    ;;
  failure)
    status=completed conclusion=failure external_id=$EXPECTED_EXTERNAL app=github-actions id=2
    ;;
  pending_then_success)
    status=in_progress conclusion=null external_id=$EXPECTED_EXTERNAL app=github-actions id=2
    if ((count >= 2)); then status=completed; conclusion=success; fi
    ;;
  wrong_external)
    status=completed conclusion=success external_id=wrong app=github-actions id=2
    ;;
  wrong_app)
    status=completed conclusion=success external_id=$EXPECTED_EXTERNAL id=2 app=unexpected-checks-owner
    ;;
  duplicate)
    jq -n --arg external "$EXPECTED_EXTERNAL" '{total_count:2,check_runs:[
      {id:1,name:"qURL Customer Journey / exact CLI artifact",external_id:$external,app:{slug:"github-actions"},status:"completed",conclusion:"failure",details_url:"https://example.invalid/old"},
      {id:2,name:"qURL Customer Journey / exact CLI artifact",external_id:$external,app:{slug:"github-actions"},status:"completed",conclusion:"success",details_url:"https://example.invalid/new"}
    ]}'
    exit 0
    ;;
  malformed)
    printf '{not-json\n'
    exit 0
    ;;
  api_failure)
    exit 1
    ;;
  *)
    echo "unknown scenario" >&2
    exit 2
    ;;
esac

jq -n \
  --arg status "$status" --arg conclusion "$conclusion" \
  --arg external "$external_id" --arg app "$app" --argjson id "$id" '
  {total_count:1,check_runs:[{
    id:$id,name:"qURL Customer Journey / exact CLI artifact",
    external_id:$external,app:{slug:$app},status:$status,
    conclusion:(if $conclusion == "null" then null else $conclusion end),
    details_url:"https://example.invalid/result"
  }]}
'
SH
chmod +x "$work/bin/gh" "$work/bin/sleep"

sha=17d077fbc5a50d54894d5521be623fe03420de14
expected="layerv.qurl-cli-customer-journey.v1:${sha}:555:2"

run_case() {
  local scenario=$1 expected_status=$2
  printf '0\n' >"$work/calls"
  set +e
  output=$(env \
    PATH="$work/bin:$PATH" \
    GH_TOKEN=test-token \
    GITHUB_REPOSITORY=layervai/qurl-integrations \
    QURL_CUSTOMER_JOURNEY_WAIT_SECONDS=1 \
    QURL_CUSTOMER_JOURNEY_POLL_SECONDS=1 \
    FAKE_CALL_COUNT="$work/calls" \
    EXPECTED_EXTERNAL="$expected" \
    SCENARIO="$scenario" \
    "$subject" "$sha" 555 2 2>&1)
  status=$?
  set -e
  if [[ "$status" != "$expected_status" ]]; then
    echo "$scenario returned $status, expected $expected_status: $output" >&2
    exit 1
  fi
}

run_case success 0
run_case failure 1
run_case pending_then_success 0
[[ $(cat "$work/calls") == 2 ]]
run_case wrong_external 1
[[ $(cat "$work/calls") == 2 ]]
run_case wrong_app 1
run_case duplicate 0
run_case malformed 1
run_case api_failure 1

echo "wait-for-cli-customer-journey-check tests passed"
