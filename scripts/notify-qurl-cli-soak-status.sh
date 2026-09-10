#!/usr/bin/env bash
set -euo pipefail

# Keep the 80-minute duration in this script's callers aligned with the soak
# matrix in cli.yml and the scheduled-impact text in main-ci-notifications.yml.
: "${SLACK_WEBHOOK_URL:?SLACK_WEBHOOK_URL is required for qURL CLI soak notifications}"
: "${REPOSITORY_URL:?REPOSITORY_URL is required}"
: "${HEAD_SHA:?HEAD_SHA is required}"
: "${RUN_URL:?RUN_URL is required}"
: "${TRIGGER:?TRIGGER is required}"
: "${SOAK_STATUS:?SOAK_STATUS is required}"
: "${SOAK_DURATION:?SOAK_DURATION is required}"

[[ "$SLACK_WEBHOOK_URL" =~ ^https://hooks\.slack\.com/services/[A-Za-z0-9/_-]+$ ]] || {
  echo "SLACK_WEBHOOK_URL must be one Slack HTTPS webhook URL" >&2
  exit 1
}
[[ "$REPOSITORY_URL" =~ ^https://[^[:space:]]+$ ]] || {
  echo "REPOSITORY_URL must be one HTTPS URL" >&2
  exit 1
}
[[ "$RUN_URL" =~ ^https://[^[:space:]]+$ ]] || {
  echo "RUN_URL must be one HTTPS URL" >&2
  exit 1
}
[[ "$HEAD_SHA" =~ ^[0-9a-f]{40}$ ]] || {
  echo "HEAD_SHA must be one full Git SHA" >&2
  exit 1
}
[[ "$SOAK_DURATION" == 80m ]] || {
  echo "SOAK_DURATION must be exactly 80m" >&2
  exit 1
}
case "$SOAK_STATUS:$TRIGGER" in
  success:schedule | success:workflow_dispatch | failure:workflow_dispatch | stale:schedule)
    ;;
  *)
    echo "SOAK_STATUS and TRIGGER do not name a supported notification" >&2
    exit 1
    ;;
esac

short_sha=${HEAD_SHA:0:7}
soak_minutes=${SOAK_DURATION%m}
commit_url="${REPOSITORY_URL}/commit/${HEAD_SHA}"
case "$SOAK_STATUS" in
  success)
    color="#28a745"
    emoji=white_check_mark
    status_text=success
    headline="qURL CLI ${soak_minutes}-minute soak passed"
    detail="The packaged CLI served the live customer path across the one-hour authorization boundary, a credential-free warm daemon restart, and an epoch restart. Protected cleanup also passed."
    ;;
  failure)
    color="#dc3545"
    emoji=x
    status_text=failure
    headline="Manual qURL CLI validation failed"
    detail="The manually requested packaged CLI workflow did not pass all required package, journey, cleanup, and result-delivery gates. Review the workflow before another run."
    ;;
  stale)
    color="#dc3545"
    emoji=rotating_light
    status_text=stale
    headline="qURL CLI scheduled soak is stale"
    detail="The watchdog could not confirm today's successful scheduled ${soak_minutes}-minute qURL CLI soak. The schedule can be missing, or today's scheduled validation did not produce every required passing job."
    ;;
esac

payload="$(jq -n \
  --arg text ":${emoji}: ${headline} (${short_sha})" \
  --arg headline ":${emoji}: *${headline}*" \
  --arg color "$color" \
  --arg status "$status_text" \
  --arg status_emoji ":${emoji}:" \
  --arg short_sha "$short_sha" \
  --arg commit_url "$commit_url" \
  --arg run_url "$RUN_URL" \
  --arg trigger "$TRIGGER" \
  --arg detail "$detail" \
  '{
    text: $text,
    attachments: [{
      color: $color,
      blocks: [
        {type: "section", text: {type: "mrkdwn", text: $headline}},
        {type: "section", fields: [
          {type: "mrkdwn", text: "*Result:*\n\($status_emoji) `\($status)`"},
          {type: "mrkdwn", text: "*Trigger:*\n\($trigger)"}
        ]},
        {type: "section", text: {type: "mrkdwn", text: $detail}},
        {type: "context", elements: [{type: "mrkdwn", text: "<\($commit_url)|\($short_sha)>  |  <\($run_url)|View workflow>"}]}
      ]
    }]
  }')"

for attempt in 1 2 3; do
  if printf 'url = "%s"\n' "$SLACK_WEBHOOK_URL" | curl -sS --fail-with-body --max-time 30 \
    -X POST -H 'Content-type: application/json' \
    --data "$payload" --config -; then
    exit 0
  fi
  if [[ "$attempt" -lt 3 ]]; then
    sleep "$((attempt * 2))"
  fi
done

echo "Slack notification failed after 3 attempts" >&2
exit 1
