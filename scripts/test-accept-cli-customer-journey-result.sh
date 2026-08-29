#!/usr/bin/env bash
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
subject="$root/scripts/accept-cli-customer-journey-result.sh"
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
method=GET
input=
path=
while (($#)); do
  case "$1" in
    api) shift ;;
    --method) method=$2; shift 2 ;;
    --input) input=$2; shift 2 ;;
    *) path=$1; shift ;;
  esac
done
printf '%s\t%s\n' "$method" "$path" >>"$FAKE_GH_CALLS"
call_count=$(wc -l <"$FAKE_GH_CALLS" | tr -d '[:space:]')
if ((call_count <= ${FAKE_FAIL_FIRST_CALLS:-0})); then
  exit 1
fi
if [[ "$method" == POST && "${FAKE_FAIL_POST:-0}" == 1 ]]; then
  exit 1
fi
case "$path" in
  repos/layervai/qurl-integrations/actions/runs/555/attempts/2)
    jq -n \
      --arg status "${FAKE_RUN_STATUS:-in_progress}" \
      --arg conclusion "${FAKE_RUN_CONCLUSION:-}" \
      --arg event "${FAKE_RUN_EVENT:-pull_request}" \
      --arg repository "${FAKE_RUN_REPOSITORY:-layervai/qurl-integrations}" \
      --arg head_repository "${FAKE_RUN_HEAD_REPOSITORY:-layervai/qurl-integrations}" \
      --arg sha "${FAKE_RUN_SHA:-17d077fbc5a50d54894d5521be623fe03420de14}" \
      --arg path "${FAKE_RUN_PATH:-.github/workflows/cli.yml}" \
      --argjson attempt "${FAKE_RUN_ATTEMPT:-2}" \
      '{repository:{full_name:$repository},head_repository:{full_name:$head_repository},head_sha:$sha,path:$path,event:$event,run_attempt:$attempt,status:$status,conclusion:(if $conclusion == "" then null else $conclusion end)}'
    ;;
  repos/layervai/qurl-integrations/actions/runs/555/attempts/2/jobs\?per_page=100)
    jq -n \
      --arg name "${FAKE_JOB_NAME:-cli / sandbox matched-cohort artifacts}" \
      --arg status "${FAKE_JOB_STATUS:-completed}" \
      --arg conclusion "${FAKE_JOB_CONCLUSION:-success}" \
      --arg step_conclusion "${FAKE_JOB_STEP_CONCLUSION:-success}" \
      --argjson copies "${FAKE_JOB_COPIES:-1}" \
      --argjson total "${FAKE_JOB_TOTAL_COUNT:-${FAKE_JOB_COPIES:-1}}" '
      {
        total_count:$total,
        jobs:[range(0;$copies) | {
          name:$name,status:$status,conclusion:$conclusion,
          steps:[
            {name:"Build exact sandbox customer artifacts",conclusion:$step_conclusion},
            {name:"Upload exact sandbox customer binaries",conclusion:"success"},
            {name:"Upload exact sandbox customer source receipt",conclusion:"success"}
          ]
        }]
      }'
    ;;
  repos/layervai/qurl-integrations/pulls/1279)
    jq -n \
      --arg sha "${FAKE_PR_SHA:-17d077fbc5a50d54894d5521be623fe03420de14}" \
      --arg state "${FAKE_PR_STATE:-open}" \
      --arg head_repository "${FAKE_PR_HEAD_REPOSITORY:-layervai/qurl-integrations}" \
      --arg base_repository "${FAKE_PR_BASE_REPOSITORY:-layervai/qurl-integrations}" \
      '{number:1279,state:$state,head:{repo:{full_name:$head_repository},sha:$sha},base:{repo:{full_name:$base_repository}}}'
    ;;
  repos/layervai/qurl-integrations/git/ref/heads/main)
    jq -n --arg sha "${FAKE_MAIN_SHA:-17d077fbc5a50d54894d5521be623fe03420de14}" \
      '{ref:"refs/heads/main",object:{type:"commit",sha:$sha}}'
    ;;
  repos/layervai/qurl-integrations/check-runs)
    [[ "$method" == POST && -f "$input" ]]
    cp "$input" "$FAKE_CHECK_REQUEST"
    jq -n '{id:9001}'
    ;;
  *)
    echo "unexpected gh call: $method $path" >&2
    exit 1
    ;;
esac
SH
chmod +x "$work/bin/gh" "$work/bin/sleep"

write_event() {
  local output=$1
  jq -n '
    {
      action:"qurl-cli-customer-journey-result",
      repository:{full_name:"layervai/qurl-integrations"},
      sender:{login:"ops-routines-reader[bot]",id:277190418,type:"Bot"},
      client_payload:{
        schema:"layerv.qurl-cli-customer-journey-result.v1",
        source_sha:"17d077fbc5a50d54894d5521be623fe03420de14",
        source_kind:"pull_request",
        pull_request_number:1279,
        producer_run_id:555,
        producer_run_attempt:2,
        orchestrator_run_id:777,
        orchestrator_run_attempt:1,
        conclusion:"success"
      }
    }
  ' >"$output"
}

run_subject() {
  env \
    PATH="$work/bin:$PATH" \
    GH_TOKEN=test-token \
    GITHUB_REPOSITORY=layervai/qurl-integrations \
    GITHUB_SERVER_URL=https://github.com \
    GITHUB_RUN_ID=888 \
    GITHUB_RUN_ATTEMPT=3 \
    FAKE_GH_CALLS="$work/gh-calls" \
    FAKE_CHECK_REQUEST="$work/check-request" \
    "$@" \
    "$subject" "$work/event.json"
}

write_event "$work/event.json"
: >"$work/gh-calls"
run_subject
jq -e '
  .name == "qURL Customer Journey / exact CLI artifact" and
  .head_sha == "17d077fbc5a50d54894d5521be623fe03420de14" and
  .external_id == "layerv.qurl-cli-customer-journey.v1:17d077fbc5a50d54894d5521be623fe03420de14:555:2" and
  .details_url == "https://github.com/layervai/qurl-integrations/actions/runs/888/attempts/3" and
  .status == "completed" and .conclusion == "success" and
  (.output.summary | contains("producer run 555, attempt 2"))
' "$work/check-request" >/dev/null

write_event "$work/event.json"
jq '.client_payload.conclusion = "failure"' "$work/event.json" >"$work/failure-event.json"
mv "$work/failure-event.json" "$work/event.json"
: >"$work/gh-calls"
run_subject >/dev/null
jq -e '
  .conclusion == "failure" and
  (.output.title | contains("failed")) and
  (.output.summary | contains("failed"))
' "$work/check-request" >/dev/null

expect_envelope_rejected() {
  local label=$1 mutation=$2
  write_event "$work/event.json"
  jq "$mutation" "$work/event.json" >"$work/mutated-event.json"
  mv "$work/mutated-event.json" "$work/event.json"
  : >"$work/gh-calls"
  if run_subject </dev/null >/dev/null 2>&1; then
    echo "$label was accepted" >&2
    exit 1
  fi
  [[ ! -s "$work/gh-calls" ]] || {
    echo "$label reached GitHub API calls" >&2
    exit 1
  }
}

while IFS=$'\t' read -r label mutation; do
  expect_envelope_rejected "$label" "$mutation"
done <<'CASES'
forged sender login	.sender.login = "attacker"
forged sender ID	.sender.id = 0
non-bot sender	.sender.type = "User"
wrong action	.action = "wrong"
wrong schema	.client_payload.schema = "wrong"
wrong envelope repository	.repository.full_name = "example/wrong"
extra payload field	.client_payload.extra = true
missing payload field	del(.client_payload.schema)
invalid source SHA	.client_payload.source_sha = "abc"
main source with PR number	.client_payload.source_kind = "main" | .client_payload.pull_request_number = 1279
PR source without PR number	.client_payload.pull_request_number = 0
invalid conclusion	.client_payload.conclusion = "neutral"
zero producer run	.client_payload.producer_run_id = 0
string producer attempt	.client_payload.producer_run_attempt = "2"
negative orchestrator run	.client_payload.orchestrator_run_id = -1
zero orchestrator attempt	.client_payload.orchestrator_run_attempt = 0
CASES

write_event "$work/event.json"
: >"$work/gh-calls"
if run_subject GITHUB_REPOSITORY=example/not-the-repository >/dev/null 2>&1; then
  echo "non-canonical repository was accepted" >&2
  exit 1
fi
[[ ! -s "$work/gh-calls" ]] || { echo "repository guard reached GitHub API calls" >&2; exit 1; }

for preflight_override in GH_TOKEN= GITHUB_RUN_ID=not-a-number GITHUB_RUN_ATTEMPT=0; do
  write_event "$work/event.json"
  : >"$work/gh-calls"
  if run_subject "$preflight_override" >/dev/null 2>&1; then
    echo "preflight accepted $preflight_override" >&2
    exit 1
  fi
  [[ ! -s "$work/gh-calls" ]] || { echo "preflight guard reached GitHub API calls" >&2; exit 1; }
done

write_event "$work/event.json"
mv "$work/event.json" "$work/missing-event.json"
: >"$work/gh-calls"
if run_subject >/dev/null 2>&1; then
  echo "missing event file was accepted" >&2
  exit 1
fi
mv "$work/missing-event.json" "$work/event.json"
[[ ! -s "$work/gh-calls" ]] || { echo "missing-file guard reached GitHub API calls" >&2; exit 1; }

mv "$work/event.json" "$work/real-event.json"
ln -s "$work/real-event.json" "$work/event.json"
: >"$work/gh-calls"
if run_subject >/dev/null 2>&1; then
  echo "symlinked event file was accepted" >&2
  exit 1
fi
rm "$work/event.json"
mv "$work/real-event.json" "$work/event.json"
[[ ! -s "$work/gh-calls" ]] || { echo "symlink guard reached GitHub API calls" >&2; exit 1; }

expect_stale_pr_ignored() {
  local label=$1 override=$2
  write_event "$work/event.json"
  rm -f "$work/check-request"
  : >"$work/gh-calls"
  if ! run_subject "$override" >/dev/null; then
    echo "$label produced a callback failure" >&2
    exit 1
  fi
  [[ $(wc -l <"$work/gh-calls" | tr -d '[:space:]') == 1 && ! -e "$work/check-request" ]] || {
    echo "$label was not ignored before check creation" >&2
    exit 1
  }
}

expect_stale_pr_ignored "stale pull-request head" \
  FAKE_PR_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
expect_stale_pr_ignored "closed pull request" FAKE_PR_STATE=closed
expect_stale_pr_ignored "fork pull-request head" FAKE_PR_HEAD_REPOSITORY=attacker/qurl-integrations
expect_stale_pr_ignored "crossed pull-request base" FAKE_PR_BASE_REPOSITORY=attacker/qurl-integrations

write_event "$work/event.json"
: >"$work/gh-calls"
run_subject FAKE_FAIL_FIRST_CALLS=2 >/dev/null
[[ $(wc -l <"$work/gh-calls" | tr -d '[:space:]') == 6 ]] || {
  echo "transient GitHub API failures were not retried exactly" >&2
  exit 1
}

write_event "$work/event.json"
: >"$work/gh-calls"
if run_subject FAKE_FAIL_FIRST_CALLS=99 >/dev/null 2>&1; then
  echo "persistent GitHub API failure was accepted" >&2
  exit 1
fi
[[ $(wc -l <"$work/gh-calls" | tr -d '[:space:]') == 3 ]] || {
  echo "persistent GitHub API failure did not stop after 3 attempts" >&2
  exit 1
}

write_event "$work/event.json"
: >"$work/gh-calls"
if run_subject FAKE_RUN_STATUS=completed FAKE_RUN_CONCLUSION=failure >/dev/null 2>&1; then
  echo "failed CLI producer run was accepted" >&2
  exit 1
fi
[[ $(wc -l <"$work/gh-calls" | tr -d '[:space:]') == 2 ]] || {
  echo "failed producer did not fail at producer validation" >&2
  exit 1
}

write_event "$work/event.json"
: >"$work/gh-calls"
if run_subject FAKE_RUN_STATUS=completed FAKE_RUN_CONCLUSION=cancelled >/dev/null 2>&1; then
  echo "cancelled CLI producer run was accepted" >&2
  exit 1
fi
[[ $(wc -l <"$work/gh-calls" | tr -d '[:space:]') == 2 ]] || {
  echo "cancelled producer did not fail at producer validation" >&2
  exit 1
}

for producer_case in \
  'wrong producer repository:FAKE_RUN_REPOSITORY=example/wrong' \
  'fork producer repository:FAKE_RUN_HEAD_REPOSITORY=attacker/qurl-integrations' \
  'wrong producer SHA:FAKE_RUN_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' \
  'wrong producer workflow:FAKE_RUN_PATH=.github/workflows/not-cli.yml' \
  'wrong producer attempt:FAKE_RUN_ATTEMPT=3' \
  'wrong producer event:FAKE_RUN_EVENT=push'; do
  label=${producer_case%%:*}
  override=${producer_case#*:}
  write_event "$work/event.json"
  : >"$work/gh-calls"
  if run_subject "$override" >/dev/null 2>&1; then
    echo "$label was accepted" >&2
    exit 1
  fi
  [[ $(wc -l <"$work/gh-calls" | tr -d '[:space:]') == 2 ]] || {
    echo "$label did not fail at producer validation" >&2
    exit 1
  }
done

write_event "$work/event.json"
: >"$work/gh-calls"
if run_subject FAKE_RUN_STATUS=queued >/dev/null 2>&1; then
  echo "queued CLI producer run was accepted" >&2
  exit 1
fi
[[ $(wc -l <"$work/gh-calls" | tr -d '[:space:]') == 2 ]] || {
  echo "queued producer did not fail at producer validation" >&2
  exit 1
}

for artifact_case in \
  'missing artifact job:FAKE_JOB_NAME=cli / wrong artifact job' \
  'running artifact job:FAKE_JOB_STATUS=in_progress' \
  'failed artifact job:FAKE_JOB_CONCLUSION=failure' \
  'failed artifact step:FAKE_JOB_STEP_CONCLUSION=failure' \
  'truncated artifact jobs:FAKE_JOB_TOTAL_COUNT=2' \
  'duplicate artifact jobs:FAKE_JOB_COPIES=2'; do
  label=${artifact_case%%:*}
  override=${artifact_case#*:}
  write_event "$work/event.json"
  : >"$work/gh-calls"
  if run_subject "$override" >/dev/null 2>&1; then
    echo "$label was accepted" >&2
    exit 1
  fi
  [[ $(wc -l <"$work/gh-calls" | tr -d '[:space:]') == 3 ]] || {
    echo "$label did not fail at artifact producer validation" >&2
    exit 1
  }
done

write_event "$work/event.json"
jq '.client_payload.source_kind = "main" | .client_payload.pull_request_number = 0' \
  "$work/event.json" >"$work/main-event.json"
mv "$work/main-event.json" "$work/event.json"
: >"$work/gh-calls"
run_subject FAKE_RUN_EVENT=push >/dev/null
[[ $(wc -l <"$work/gh-calls" | tr -d '[:space:]') == 4 ]] || {
  echo "current-main result did not validate producer, artifact job, main ref, and check creation" >&2
  exit 1
}
jq -e '
  .head_sha == "17d077fbc5a50d54894d5521be623fe03420de14" and
  .external_id == "layerv.qurl-cli-customer-journey.v1:17d077fbc5a50d54894d5521be623fe03420de14:555:2" and
  .details_url == "https://github.com/layervai/qurl-integrations/actions/runs/888/attempts/3"
' "$work/check-request" >/dev/null

write_event "$work/event.json"
jq '.client_payload.source_kind = "main" | .client_payload.pull_request_number = 0' \
  "$work/event.json" >"$work/main-event.json"
mv "$work/main-event.json" "$work/event.json"
: >"$work/gh-calls"
if run_subject >/dev/null 2>&1; then
  echo "main result bound to a pull-request producer was accepted" >&2
  exit 1
fi
[[ $(wc -l <"$work/gh-calls" | tr -d '[:space:]') == 2 ]] || {
  echo "crossed main producer did not fail at producer validation" >&2
  exit 1
}

write_event "$work/event.json"
jq '.client_payload.source_kind = "main" | .client_payload.pull_request_number = 0' \
  "$work/event.json" >"$work/main-event.json"
mv "$work/main-event.json" "$work/event.json"
rm -f "$work/check-request"
: >"$work/gh-calls"
if ! run_subject FAKE_RUN_EVENT=push FAKE_MAIN_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa >/dev/null; then
  echo "stale main result produced a callback failure" >&2
  exit 1
fi
[[ $(wc -l <"$work/gh-calls" | tr -d '[:space:]') == 1 && ! -e "$work/check-request" ]] || {
  echo "stale main was not ignored before check creation" >&2
  exit 1
}

write_event "$work/event.json"
: >"$work/gh-calls"
if run_subject FAKE_FAIL_POST=1 >/dev/null 2>&1; then
  echo "failed check creation was accepted" >&2
  exit 1
fi
[[ $(wc -l <"$work/gh-calls" | tr -d '[:space:]') == 4 ]] || {
  echo "non-idempotent check creation was retried" >&2
  exit 1
}

echo "accept-cli-customer-journey-result tests passed"
