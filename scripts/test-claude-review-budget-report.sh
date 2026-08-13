#!/usr/bin/env bash
# Exercise the "Report unfinished Claude review" step of
# .github/workflows/claude-code-review.yml.
#
# That step runs only after the review has already failed, so a defect in it
# ships green and is discovered exactly when it is needed — the same argument
# check-main-ci-notification-payload.sh makes for its own existence. The step
# is also the only thing that tells a maintainer a review is missing, so the
# assertion with teeth is that *every* branch annotates: nothing here may
# regress to a bare echo, which would restore the silent-pass this workflow
# change exists to remove.
#
# The real run: block is extracted and executed rather than pattern-matched,
# so rewording the messages does not require editing this test.
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
workflow="$repo_root/.github/workflows/claude-code-review.yml"
step_name="Report unfinished Claude review"
tmp="$(mktemp -d)"

trap 'rm -rf "$tmp"' EXIT

script="$tmp/report.sh"

# Anchored on the step name rather than "first run: block in the file", so
# inserting a step ahead of this one cannot silently point the test at the
# wrong script. PyYAML is not guaranteed on runners, so this stays stdlib-only.
python3 - "$workflow" "$step_name" > "$script" <<'PY'
import re
import sys

workflow, step_name = sys.argv[1], sys.argv[2]
with open(workflow) as fh:
    lines = fh.read().split("\n")

start = None
for i, line in enumerate(lines):
    if re.match(r"^ {6}- name: %s\s*$" % re.escape(step_name), line):
        start = i
        break
if start is None:
    sys.exit(
        "could not find the %r step -- if the workflow was restructured, "
        "update this test rather than deleting it" % step_name
    )

for i in range(start + 1, len(lines)):
    if re.match(r"^ {6}- ", lines[i]):
        break  # next step; this one has no run: block
    if re.match(r"^ {8}run: \|\s*$", lines[i]):
        body, pad = [], " " * 10
        for nxt in lines[i + 1:]:
            if nxt.strip() and not nxt.startswith(pad):
                break
            body.append(nxt[10:] if nxt.startswith(pad) else "")
        print("\n".join(body))
        sys.exit(0)

sys.exit("the %r step has no run: | block" % step_name)
PY

if [ ! -s "$script" ]; then
  echo "FAIL: extracted an empty run: block for '$step_name'" >&2
  exit 1
fi

failures=0

note_failure() {
  echo "FAIL: $1" >&2
  failures=$((failures + 1))
}

# The budget is read from the workflow rather than hardcoded, so this test
# tracks the real value instead of drifting from it.
budget="$(sed -n 's/^      CLAUDE_REVIEW_BUDGET_MINUTES: \([0-9]\{1,\}\)$/\1/p' "$workflow")"
if [ -z "$budget" ]; then
  note_failure "no job-level CLAUDE_REVIEW_BUDGET_MINUTES found in $workflow"
  exit 1
fi

# The single-source-of-truth property: the step cap and the annotation text
# must read the same variable, or the message can claim a budget that never
# fired.
# shellcheck disable=SC2016  # the ${{ }} is the workflow expression being
# asserted on, so it must stay literal rather than expand as a shell parameter.
if ! grep -qF 'timeout-minutes: ${{ fromJSON(env.CLAUDE_REVIEW_BUDGET_MINUTES) }}' "$workflow"; then
  note_failure "the review step no longer derives timeout-minutes from CLAUDE_REVIEW_BUDGET_MINUTES"
fi
job_cap="$(sed -n 's/^    timeout-minutes: \([0-9]\{1,\}\)$/\1/p' "$workflow")"
if [ -z "$job_cap" ]; then
  note_failure "no job-level timeout-minutes found in $workflow"
elif [ "$((job_cap - budget))" -lt 3 ]; then
  # Not just "greater than": the cap also has to absorb setup, checkout (plus
  # its internal retry), origin preparation and the verify step's two
  # `timeout 30s` gh calls. Too thin a margin and the job is cancelled around
  # the review instead of the review step failing, which reports nothing.
  note_failure "job cap ${job_cap}m leaves under 3m over the ${budget}m review budget; too thin for setup, checkout retry and verify"
fi

budget_seconds=$((budget * 60))

# Every case asserts the same three invariants: exit 0 (the review step's own
# failure already reds the job; a second red step would bury the annotation),
# exactly one annotation, and the expected title.
expect_case() {
  local desc="$1" want_title="$2" started_at="$3" budget_minutes="$4"
  local out status=0 error_lines

  out="$(STARTED_AT="$started_at" BUDGET_MINUTES="$budget_minutes" bash "$script" 2>&1)" || status=$?

  if [ "$status" -ne 0 ]; then
    note_failure "$desc: expected exit 0, got $status"
    return
  fi

  error_lines="$(printf '%s\n' "$out" | grep -c '^::error' || true)"
  if [ "$error_lines" -ne 1 ]; then
    note_failure "$desc: expected exactly 1 ::error annotation, got $error_lines -- output: $out"
    return
  fi

  if ! printf '%s\n' "$out" | grep -qF "::error title=$want_title::"; then
    note_failure "$desc: expected title '$want_title' -- output: $out"
    return
  fi

  echo "ok: $desc"
}

now="$(date +%s)"

# Overrun: the runner kills the step at the budget, so elapsed is always at or
# just past it by the time this step measures.
expect_case "over budget is reported as a timeout" \
  "Claude review ran out of time" "$((now - budget_seconds - 20))" "$budget"
expect_case "exactly at budget is reported as a timeout" \
  "Claude review ran out of time" "$((now - budget_seconds))" "$budget"

# Inside the budget the step cannot have been killed, so this was a real
# failure and must not be blamed on the clock.
expect_case "one second under budget is reported as a failure" \
  "Claude review failed" "$((now - budget_seconds + 1))" "$budget"
expect_case "an early failure is reported as a failure" \
  "Claude review failed" "$((now - 5))" "$budget"

# Unmeasurable: a timeout cannot be ruled out, so this must still annotate
# rather than pass quietly.
expect_case "an empty clock is reported as unfinished" \
  "Claude review did not finish" "" "$budget"
expect_case "a non-numeric clock is reported as unfinished" \
  "Claude review did not finish" "not-a-number" "$budget"
expect_case "an injected clock expression is reported as unfinished" \
  "Claude review did not finish" '1+1' "$budget"
expect_case "a non-numeric budget is reported as unfinished" \
  "Claude review did not finish" "$now" "thirteen"

if [ "$failures" -ne 0 ]; then
  echo "$failures check(s) failed" >&2
  exit 1
fi

echo "All Claude review budget report checks passed (budget ${budget}m, job cap ${job_cap}m)."
