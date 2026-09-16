#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
watchdog=$script_dir/qurl-cli-soak-watchdog.sh
fixture=$(mktemp -d)
cleanup() { rm -rf -- "$fixture"; }
trap cleanup EXIT
mkdir "$fixture/jobs"

now=1787313600
expected_after=1787293020
active_grace=14400
expected_journeys=4

cat >"$fixture/completed.json" <<'JSON'
{"workflow_runs":[
  {"id":101,"run_attempt":1,"event":"schedule","status":"completed","conclusion":"failure","created_at":"2026-08-21T06:17:01.123456789Z"},
  {"id":100,"run_attempt":1,"event":"workflow_dispatch","status":"completed","conclusion":"success","created_at":"2026-08-21T07:00:00Z"}
]}
JSON
cat >"$fixture/jobs/101-1.json" <<'JSON'
{"total_count":7,"jobs":[
  {"name":"cli / required","conclusion":"success"},
  {"name":"cli / customer journey cleanup","conclusion":"success"},
  {"name":"cli / customer journey (linux, 1, ubuntu-latest, TestSandboxLinuxDefaultDaemonLifecycle, 35, false)","conclusion":"success"},
  {"name":"cli / customer journey (macos, 2, macos-latest, TestSandboxMacOSDefaultDaemonLifecycle, 35, false)","conclusion":"success"},
  {"name":"cli / customer journey (windows, 3, windows-latest, TestSandboxWindowsDefaultDaemonFullCustomerLifecycle, 35, false)","conclusion":"success"},
  {"name":"cli / customer journey (linux, 4, ubuntu-latest, TestSandboxLocalPublishSoak, 110, true)","conclusion":"success"},
  {"name":"cli / notify soak success","conclusion":"failure"}
]}
JSON
QURL_CLI_WATCHDOG_NOW_EPOCH=$now "$watchdog" \
  "$fixture/completed.json" "$fixture/jobs" "$expected_after" "$active_grace" "$expected_journeys" >/dev/null

cat >"$fixture/count-mismatch.json" <<'JSON'
{"workflow_runs":[
  {"id":108,"run_attempt":1,"event":"schedule","status":"completed","conclusion":"success","created_at":"2026-08-21T06:17:01Z"}
]}
JSON
jq '.jobs |= map(select(.name != "cli / customer journey (linux, 4, ubuntu-latest, TestSandboxLocalPublishSoak, 110, true)")) |
  .total_count = (.jobs | length)' "$fixture/jobs/101-1.json" >"$fixture/jobs/108-1.json"
if QURL_CLI_WATCHDOG_NOW_EPOCH=$now "$watchdog" \
  "$fixture/count-mismatch.json" "$fixture/jobs" "$expected_after" "$active_grace" "$expected_journeys" >/dev/null 2>&1; then
  echo "watchdog accepted fewer customer journeys than the workflow contract requires" >&2
  exit 1
fi

cat >"$fixture/failed-lane.json" <<'JSON'
{"workflow_runs":[
  {"id":109,"run_attempt":1,"event":"schedule","status":"completed","conclusion":"success","created_at":"2026-08-21T06:17:01Z"}
]}
JSON
jq '(.jobs[] | select(.name == "cli / customer journey (windows, 3, windows-latest, TestSandboxWindowsDefaultDaemonFullCustomerLifecycle, 35, false)").conclusion) = "failure"' \
  "$fixture/jobs/101-1.json" >"$fixture/jobs/109-1.json"
if QURL_CLI_WATCHDOG_NOW_EPOCH=$now "$watchdog" \
  "$fixture/failed-lane.json" "$fixture/jobs" "$expected_after" "$active_grace" "$expected_journeys" >/dev/null 2>&1; then
  echo "watchdog accepted one failed customer journey in a successful workflow run" >&2
  exit 1
fi

# A fully paginated job result can exceed one API page and must still pass.
jq '.jobs += [range(0; 94) | {name:("unrelated " + tostring), conclusion:"success"}] |
  .total_count = (.jobs | length)' "$fixture/jobs/101-1.json" >"$fixture/jobs/101-1-large.json"
mv "$fixture/jobs/101-1-large.json" "$fixture/jobs/101-1.json"
QURL_CLI_WATCHDOG_NOW_EPOCH=$now "$watchdog" \
  "$fixture/completed.json" "$fixture/jobs" "$expected_after" "$active_grace" "$expected_journeys" >/dev/null

cat >"$fixture/active.json" <<'JSON'
{"workflow_runs":[
  {"id":102,"run_attempt":1,"event":"schedule","status":"in_progress","created_at":"2026-08-21T08:30:00Z"}
]}
JSON
QURL_CLI_WATCHDOG_NOW_EPOCH=$now "$watchdog" \
  "$fixture/active.json" "$fixture/jobs" "$expected_after" "$active_grace" "$expected_journeys" >/dev/null

# GitHub preserves created_at across re-runs. A current attempt must get its
# grace period from run_started_at so a late operator recovery is not paged as
# stale while it is still running.
cat >"$fixture/active-rerun.json" <<'JSON'
{"workflow_runs":[
  {"id":110,"run_attempt":2,"event":"schedule","status":"in_progress","created_at":"2026-08-21T06:17:01Z","run_started_at":"2026-08-21T11:00:00Z"}
]}
JSON
QURL_CLI_WATCHDOG_NOW_EPOCH=$now "$watchdog" \
  "$fixture/active-rerun.json" "$fixture/jobs" "$expected_after" "$active_grace" "$expected_journeys" >/dev/null

cat >"$fixture/stale.json" <<'JSON'
{"workflow_runs":[
  {"id":103,"run_attempt":1,"event":"schedule","status":"completed","conclusion":"failure","created_at":"2026-08-21T07:00:00Z"},
  {"id":104,"run_attempt":1,"event":"workflow_dispatch","status":"completed","conclusion":"success","created_at":"2026-08-21T08:00:00Z"},
  {"id":105,"run_attempt":1,"event":"schedule","status":"completed","conclusion":"success","created_at":"2026-08-20T08:50:00Z"}
]}
JSON
cat >"$fixture/jobs/103-1.json" <<'JSON'
{"total_count":6,"jobs":[
  {"name":"cli / required","conclusion":"failure"},
  {"name":"cli / customer journey cleanup","conclusion":"success"},
  {"name":"cli / customer journey (linux, 1, ubuntu-latest, TestSandboxLinuxDefaultDaemonLifecycle, 35, false)","conclusion":"success"},
  {"name":"cli / customer journey (macos, 2, macos-latest, TestSandboxMacOSDefaultDaemonLifecycle, 35, false)","conclusion":"success"},
  {"name":"cli / customer journey (windows, 3, windows-latest, TestSandboxWindowsDefaultDaemonFullCustomerLifecycle, 35, false)","conclusion":"success"},
  {"name":"cli / customer journey (linux, 4, ubuntu-latest, TestSandboxLocalPublishSoak, 110, true)","conclusion":"success"}
]}
JSON
cat >"$fixture/missing.json" <<'JSON'
{"workflow_runs":[
  {"id":104,"run_attempt":1,"event":"workflow_dispatch","status":"completed","conclusion":"success","created_at":"2026-08-21T08:00:00Z"},
  {"id":105,"run_attempt":1,"event":"schedule","status":"completed","conclusion":"success","created_at":"2026-08-20T08:50:00Z"}
]}
JSON
if QURL_CLI_WATCHDOG_NOW_EPOCH=$now "$watchdog" \
  "$fixture/missing.json" "$fixture/jobs" "$expected_after" "$active_grace" "$expected_journeys" >/dev/null 2>&1; then
  echo "watchdog accepted a manual or prior-day run as today's scheduled cohort" >&2
  exit 1
fi

# A completed current run already caused main-ci-notifications.yml to report a
# failure. The freshness watchdog must not send a second alert for that run.
QURL_CLI_WATCHDOG_NOW_EPOCH=$now "$watchdog" \
  "$fixture/stale.json" "$fixture/jobs" "$expected_after" "$active_grace" "$expected_journeys" >/dev/null

cat >"$fixture/unsupported-conclusion.json" <<'JSON'
{"workflow_runs":[
  {"id":107,"run_attempt":1,"event":"schedule","status":"completed","conclusion":"startup_failure","created_at":"2026-08-21T07:00:00Z"}
]}
JSON
cat >"$fixture/jobs/107-1.json" <<'JSON'
{"total_count":0,"jobs":[]}
JSON
if QURL_CLI_WATCHDOG_NOW_EPOCH=$now "$watchdog" \
  "$fixture/unsupported-conclusion.json" "$fixture/jobs" "$expected_after" "$active_grace" "$expected_journeys" >/dev/null 2>&1; then
  echo "watchdog deferred an unsupported conclusion to a notifier that cannot report it" >&2
  exit 1
fi

cat >"$fixture/old-active.json" <<'JSON'
{"workflow_runs":[
  {"id":106,"run_attempt":1,"event":"schedule","status":"in_progress","created_at":"2026-08-21T06:20:00Z"}
]}
JSON
if QURL_CLI_WATCHDOG_NOW_EPOCH=1787319001 "$watchdog" \
  "$fixture/old-active.json" "$fixture/jobs" "$expected_after" "$active_grace" "$expected_journeys" >/dev/null 2>&1; then
  echo "watchdog accepted an active run outside its grace window" >&2
  exit 1
fi

if QURL_CLI_WATCHDOG_NOW_EPOCH=bad "$watchdog" \
  "$fixture/completed.json" "$fixture/jobs" "$expected_after" "$active_grace" "$expected_journeys" >/dev/null 2>&1; then
  echo "watchdog accepted an invalid clock" >&2
  exit 1
fi
if QURL_CLI_WATCHDOG_NOW_EPOCH=$now "$watchdog" \
  "$fixture/completed.json" "$fixture/jobs" 1787313601 "$active_grace" "$expected_journeys" >/dev/null 2>&1; then
  echo "watchdog accepted a future expected-run boundary" >&2
  exit 1
fi

cat >"$fixture/malformed.json" <<'JSON'
{"workflow_runs":[{"id":"bad","run_attempt":1,"event":"schedule","status":"completed","created_at":"bad"}]}
JSON
if QURL_CLI_WATCHDOG_NOW_EPOCH=$now "$watchdog" \
  "$fixture/malformed.json" "$fixture/jobs" "$expected_after" "$active_grace" "$expected_journeys" >/dev/null 2>&1; then
  echo "watchdog accepted malformed run history" >&2
  exit 1
fi

mv "$fixture/jobs/101-1.json" "$fixture/jobs/101-1.missing"
if QURL_CLI_WATCHDOG_NOW_EPOCH=$now "$watchdog" \
  "$fixture/completed.json" "$fixture/jobs" "$expected_after" "$active_grace" "$expected_journeys" >/dev/null 2>&1; then
  echo "watchdog accepted missing exact-attempt job results" >&2
  exit 1
fi

echo "qURL CLI soak watchdog tests passed"
