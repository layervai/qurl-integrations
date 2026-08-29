#!/usr/bin/env bash
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
subject="$root/scripts/accept-cli-customer-journey-result.sh"
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
mkdir -p "$work/bin"

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
case "$path" in
  repos/layervai/qurl-integrations/actions/runs/555)
    jq -n \
      --arg status "${FAKE_RUN_STATUS:-in_progress}" \
      --arg conclusion "${FAKE_RUN_CONCLUSION:-}" \
      --arg event "${FAKE_RUN_EVENT:-pull_request}" \
      '{repository:{full_name:"layervai/qurl-integrations"},head_repository:{full_name:"layervai/qurl-integrations"},head_sha:"17d077fbc5a50d54894d5521be623fe03420de14",path:".github/workflows/cli.yml",event:$event,run_attempt:2,status:$status,conclusion:(if $conclusion == "" then null else $conclusion end)}'
    ;;
  repos/layervai/qurl-integrations/pulls/1279)
    jq -n --arg sha "${FAKE_PR_SHA:-17d077fbc5a50d54894d5521be623fe03420de14}" \
      '{number:1279,state:"open",head:{repo:{full_name:"layervai/qurl-integrations"},sha:$sha},base:{repo:{full_name:"layervai/qurl-integrations"}}}'
    ;;
  repos/layervai/qurl-integrations/git/ref/heads/main)
    jq -n '{ref:"refs/heads/main",object:{type:"commit",sha:"17d077fbc5a50d54894d5521be623fe03420de14"}}'
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
chmod +x "$work/bin/gh"

write_event() {
  local output=$1 sender=${2:-ops-routines-reader[bot]}
  jq -n --arg sender "$sender" '
    {
      action:"qurl-cli-customer-journey-result",
      repository:{full_name:"layervai/qurl-integrations"},
      sender:{login:$sender,id:277190418,type:"Bot"},
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

write_event "$work/event.json" attacker
: >"$work/gh-calls"
if run_subject >/dev/null 2>&1; then
  echo "forged dispatch sender was accepted" >&2
  exit 1
fi
[[ ! -s "$work/gh-calls" ]] || { echo "forged sender reached GitHub API calls" >&2; exit 1; }

write_event "$work/event.json"
: >"$work/gh-calls"
if run_subject FAKE_PR_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa >/dev/null 2>&1; then
  echo "stale pull-request head was accepted" >&2
  exit 1
fi
[[ $(wc -l <"$work/gh-calls" | tr -d '[:space:]') == 2 ]] || {
  echo "stale head did not fail before check creation" >&2
  exit 1
}

: >"$work/gh-calls"
if run_subject FAKE_RUN_STATUS=completed FAKE_RUN_CONCLUSION=failure >/dev/null 2>&1; then
  echo "failed CLI producer run was accepted" >&2
  exit 1
fi
[[ $(wc -l <"$work/gh-calls" | tr -d '[:space:]') == 1 ]] || {
  echo "failed producer did not fail at producer validation" >&2
  exit 1
}

jq '.client_payload.source_kind = "main" | .client_payload.pull_request_number = 0' \
  "$work/event.json" >"$work/main-event.json"
mv "$work/main-event.json" "$work/event.json"
: >"$work/gh-calls"
run_subject FAKE_RUN_EVENT=push >/dev/null
[[ $(wc -l <"$work/gh-calls" | tr -d '[:space:]') == 3 ]] || {
  echo "current-main result did not validate producer, main ref, and check creation" >&2
  exit 1
}

echo "accept-cli-customer-journey-result tests passed"
