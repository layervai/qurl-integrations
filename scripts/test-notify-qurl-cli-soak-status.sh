#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
subject=$script_dir/notify-qurl-cli-soak-status.sh
fixture=$(mktemp -d)
cleanup() { rm -rf -- "$fixture"; }
trap cleanup EXIT

mkdir "$fixture/bin"
cat >"$fixture/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$@" >"$CURL_ARGS_FILE"
cat >"$CURL_CONFIG_FILE"
payload=
while (($#)); do
  if [[ "$1" == --data ]]; then
    payload=$2
    shift 2
    continue
  fi
  shift
done
printf '%s' "$payload" >"$CAPTURED_PAYLOAD"
count=0
if [[ -f "$CURL_COUNT_FILE" ]]; then
  count=$(<"$CURL_COUNT_FILE")
fi
count=$((count + 1))
printf '%s\n' "$count" >"$CURL_COUNT_FILE"
if ((count <= CURL_FAILS)); then
  echo "simulated Slack delivery failure" >&2
  exit 22
fi
EOF
chmod 0700 "$fixture/bin/curl"
cat >"$fixture/bin/sleep" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$1" >>"$CAPTURED_SLEEPS"
EOF
chmod 0700 "$fixture/bin/sleep"

export CAPTURED_PAYLOAD=$fixture/payload.json
export CAPTURED_SLEEPS=$fixture/sleeps
export CURL_ARGS_FILE=$fixture/curl-args
export CURL_COUNT_FILE=$fixture/curl-count
export CURL_CONFIG_FILE=$fixture/curl-config
export CURL_FAILS=0
export HEAD_SHA=0123456789abcdef0123456789abcdef01234567
export PATH=$fixture/bin:$PATH
export REPOSITORY_URL=https://github.com/layervai/qurl-integrations
export RUN_URL=https://github.com/layervai/qurl-integrations/actions/runs/123
export SLACK_WEBHOOK_URL=https://hooks.slack.com/services/test
export SOAK_DURATION=80m
export SOAK_STATUS=success
export TRIGGER=workflow_dispatch

"$subject"
[[ "$(<"$CURL_COUNT_FILE")" == 1 ]]
jq -e '
  .text == ":white_check_mark: qURL CLI 80-minute soak passed (0123456)" and
  .attachments[0].color == "#28a745" and
  .attachments[0].blocks[0].type == "section" and
  ([.attachments[0].blocks[] | .. | strings] | join("\n") |
    contains("one-hour authorization boundary") and
    contains("Protected cleanup also passed") and
    contains("actions/runs/123"))
' "$CAPTURED_PAYLOAD" >/dev/null
for argument in --fail-with-body --max-time 30 --config -; do
  grep -Fx -- "$argument" "$CURL_ARGS_FILE" >/dev/null
done
if grep -F -- "$SLACK_WEBHOOK_URL" "$CURL_ARGS_FILE" >/dev/null; then
  echo "notification exposed the Slack webhook in curl arguments" >&2
  exit 1
fi
[[ "$(<"$CURL_CONFIG_FILE")" == "url = \"$SLACK_WEBHOOK_URL\"" ]]

rm -f "$CURL_COUNT_FILE"
export SOAK_STATUS=stale
export TRIGGER=schedule
"$subject"
jq -e '
  .text == ":rotating_light: qURL CLI scheduled soak is stale (0123456)" and
  .attachments[0].color == "#dc3545" and
  ([.attachments[0].blocks[] | .. | strings] | join("\n") |
    contains("could not confirm today") and
    contains("did not produce every required passing job") and
    (contains("status lookup") | not) and
    contains("actions/runs/123"))
' "$CAPTURED_PAYLOAD" >/dev/null

rm -f "$CURL_COUNT_FILE"
export SOAK_STATUS=failure
export TRIGGER=workflow_dispatch
"$subject"
jq -e '
  .text == ":x: Manual qURL CLI validation failed (0123456)" and
  .attachments[0].color == "#dc3545" and
  ([.attachments[0].blocks[] | .. | strings] | join("\n") |
    contains("did not pass all required package, journey, cleanup, and result-delivery gates") and
    contains("actions/runs/123"))
' "$CAPTURED_PAYLOAD" >/dev/null

if env -u SLACK_WEBHOOK_URL "$subject" 2>"$fixture/missing-secret"; then
  echo "notification accepted a missing Slack webhook" >&2
  exit 1
fi
grep -F 'SLACK_WEBHOOK_URL is required' "$fixture/missing-secret" >/dev/null

for invalid_webhook in \
  http://hooks.slack.com/services/test \
  https://hooks.slack.invalid/services/test \
  'https://hooks.slack.com/services/test bad'; do
  if SLACK_WEBHOOK_URL=$invalid_webhook "$subject" 2>"$fixture/invalid-webhook"; then
    echo "notification accepted an invalid Slack webhook" >&2
    exit 1
  fi
  grep -F 'SLACK_WEBHOOK_URL must be one Slack HTTPS webhook URL' "$fixture/invalid-webhook" >/dev/null
done

for name in REPOSITORY_URL RUN_URL; do
  if env "$name=http://github.invalid" "$subject" 2>"$fixture/invalid-url"; then
    echo "notification accepted invalid $name" >&2
    exit 1
  fi
  grep -F "$name must be one HTTPS URL" "$fixture/invalid-url" >/dev/null
done

for invalid_sha in 0123456 z123456789abcdef0123456789abcdef01234567; do
  if HEAD_SHA=$invalid_sha "$subject" 2>"$fixture/invalid-sha"; then
    echo "notification accepted an invalid Git SHA" >&2
    exit 1
  fi
  grep -F 'HEAD_SHA must be one full Git SHA' "$fixture/invalid-sha" >/dev/null
done

for invalid_duration in 74m 85m 91m 80minutes; do
  if SOAK_DURATION=$invalid_duration "$subject" 2>"$fixture/invalid-duration"; then
    echo "notification accepted an invalid soak duration" >&2
    exit 1
  fi
  grep -F 'SOAK_DURATION must be exactly 80m' "$fixture/invalid-duration" >/dev/null
done

for status_trigger in success:push stale:workflow_dispatch failure:schedule failed:schedule; do
  if SOAK_STATUS=${status_trigger%%:*} TRIGGER=${status_trigger#*:} "$subject" 2>"$fixture/invalid-status"; then
    echo "notification accepted unsupported status and trigger values" >&2
    exit 1
  fi
  grep -F 'SOAK_STATUS and TRIGGER do not name a supported notification' "$fixture/invalid-status" >/dev/null
done

rm -f "$CURL_COUNT_FILE" "$CAPTURED_SLEEPS"
export CURL_FAILS=2 SOAK_STATUS=success TRIGGER=workflow_dispatch
"$subject" 2>"$fixture/retry-errors"
[[ "$(<"$CURL_COUNT_FILE")" == 3 ]]
[[ "$(<"$CAPTURED_SLEEPS")" == $'2\n4' ]]
[[ "$(grep -cF 'simulated Slack delivery failure' "$fixture/retry-errors")" == 2 ]]

rm -f "$CURL_COUNT_FILE" "$CAPTURED_SLEEPS"
export CURL_FAILS=3
if "$subject" 2>"$fixture/exhausted-errors"; then
  echo "notification accepted three failed Slack deliveries" >&2
  exit 1
fi
[[ "$(<"$CURL_COUNT_FILE")" == 3 ]]
[[ "$(<"$CAPTURED_SLEEPS")" == $'2\n4' ]]
grep -F 'Slack notification failed after 3 attempts' "$fixture/exhausted-errors" >/dev/null

echo "qURL CLI soak status notification tests passed"
