#!/bin/sh
# Verify .github/workflows/main-ci-notifications.yml can actually build its
# Slack payload. That workflow runs only on `workflow_run` completion from the
# default branch's copy of the file, so no PR check ever executes it: a broken
# payload reaches main and then fails open, silently swallowing every main-CI
# failure alert. That is not hypothetical — a bare `+` string concatenation in
# jq object-construction position (a restricted production on jq <= 1.7, which
# ubuntu-latest ships) shipped green and broke the notifier until #1021.
#
# The version skew is the trap: jq 1.8 accepts the bare form, so a developer
# checking locally on a newer jq sees the broken filter compile fine. Only the
# CI runner's jq makes this check authoritative; see the warning below.
#
# Rather than pattern-matching the jq source (brittle, and would need updating
# on every reword), this extracts the step's shell body and runs it verbatim
# against a stub `curl`, then asserts on the payload it actually produced.
set -eu

cd "$(git rev-parse --show-toplevel)"

command -v python3 >/dev/null 2>&1 || {
    echo "Error: python3 is required; install python3 and retry" >&2
    exit 1
}
command -v jq >/dev/null 2>&1 || {
    echo "Error: jq is required; install jq and retry" >&2
    exit 1
}
command -v bash >/dev/null 2>&1 || {
    echo "Error: bash is required (the step body uses bash syntax)" >&2
    exit 1
}

python3 - <<'EOF'
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile

WORKFLOW = ".github/workflows/main-ci-notifications.yml"
STEP_NAME = "Post Slack notification"
# Emitted by the `*)` arm of the workflow's impact case statement.
FALLBACK_IMPACT_PREFIX = "Main CI failed before the repository reached"

# Stand-in for the workflow_run context the step reads from its env: block.
STUB = {
    "SLACK_WEBHOOK_URL": "https://hooks.example.invalid/stub",
    "REPOSITORY": "layervai/qurl-integrations",
    "REPOSITORY_URL": "https://github.com/layervai/qurl-integrations",
    "EVENT": "push",
    "CONCLUSION": "failure",
    "HEAD_SHA": "8d0bf8686e2904c3dcdef76a077c226070a52ea1",
    "RUN_NUMBER": "3837",
    "RUN_URL": "https://github.com/layervai/qurl-integrations/actions/runs/1",
    "DEFAULT_BRANCH": "main",
    "ACTOR": "octocat",
}

def die(msg):
    raise SystemExit("%s: %s" % (WORKFLOW, msg))

with open(WORKFLOW) as fh:
    lines = fh.read().split("\n")

# --- structural extraction (stdlib only; PyYAML is not guaranteed on runners)

def extract_block(header_pattern, body_indent):
    """Return the dedented block scalar following the first matching header."""
    for i, line in enumerate(lines):
        if re.match(header_pattern, line):
            body, pad = [], " " * body_indent
            for nxt in lines[i + 1:]:
                if nxt.strip() and not nxt.startswith(pad):
                    break
                body.append(nxt[body_indent:] if nxt.startswith(pad) else "")
            return "\n".join(body)
    return None

run_script = extract_block(r"^ {8}run: \|\s*$", 10)
if not run_script or "jq -n" not in run_script:
    die("could not extract the %r step's run: block -- if the workflow was "
        "restructured, update this check rather than deleting it" % STEP_NAME)

triggers = []
for i, line in enumerate(lines):
    if re.match(r"^ {4}workflows:\s*$", line):
        for nxt in lines[i + 1:]:
            m = re.match(r'^ {6}- ([\'"]?)(.+?)\1\s*$', nxt)
            if not m:
                break
            triggers.append(m.group(2))
        break
if not triggers:
    die("could not extract on.workflow_run.workflows")

# --- 1. every trigger names a workflow that still exists (a missed rename
#        means the notifier silently never fires -- no red run at all)

names = set()
for entry in sorted(os.listdir(".github/workflows")):
    if not entry.endswith((".yml", ".yaml")):
        continue
    with open(os.path.join(".github/workflows", entry)) as fh:
        for line in fh:
            m = re.match(r'^name: *([\'"]?)(.+?)\1\s*$', line)
            if m:
                names.add(m.group(2))
                break

missing = [t for t in triggers if t not in names]
if missing:
    die("on.workflow_run.workflows names no live workflow: %s (a renamed "
        "upstream workflow makes this notifier silently never fire)" % missing)

# --- 2. run the real step body against a stub curl, for every trigger

tmp = tempfile.mkdtemp()
try:
    script = os.path.join(tmp, "step.sh")
    with open(script, "w") as fh:
        fh.write(run_script)

    bindir = os.path.join(tmp, "bin")
    os.mkdir(bindir)
    stub = os.path.join(bindir, "curl")
    with open(stub, "w") as fh:
        fh.write(
            '#!/bin/sh\nprev=""\nfor a in "$@"; do\n'
            '  [ "$prev" = "--data" ] && printf \'%s\' "$a" > "$PAYLOAD_OUT"\n'
            '  prev="$a"\ndone\nexit 0\n'
        )
    os.chmod(stub, 0o755)
    out = os.path.join(tmp, "payload.json")

    def run(env_overrides):
        env = dict(os.environ)
        env.update(STUB)
        env.update({
            "PATH": bindir + os.pathsep + os.environ.get("PATH", ""),
            "PAYLOAD_OUT": out,
        })
        env.update(env_overrides)
        if os.path.exists(out):
            os.remove(out)
        proc = subprocess.run(
            ["bash", script], env=env, capture_output=True, text=True
        )
        return proc, (open(out).read() if os.path.exists(out) else None)

    # Every trigger, plus an unlisted name to prove the fallback arm works.
    cases = [(w, {}) for w in triggers + ["Some Unlisted Workflow"]]
    # Two branches unreachable by varying WORKFLOW_NAME alone: a scheduled run
    # relabels the actor, and a missing actor must fall back rather than render
    # an empty field.
    cases.append((triggers[0], {"EVENT": "schedule"}))
    cases.append((triggers[0], {"ACTOR": ""}))

    for workflow, extra in cases:
        listed = workflow in triggers
        env = {"WORKFLOW_NAME": workflow}
        env.update(extra)
        label = "%s%s" % (workflow, (" " + str(extra)) if extra else "")
        proc, payload = run(env)
        if proc.returncode != 0:
            die("step failed for %s (exit %d)\n%s"
                % (label, proc.returncode, proc.stderr.strip()))
        if payload is None:
            die("step posted nothing for %s" % label)
        try:
            obj = json.loads(payload)
        except ValueError as exc:
            die("invalid JSON payload for %s: %s" % (label, exc))

        if set(obj) != {"text", "blocks"}:
            die("payload keys changed for %s: %s" % (label, sorted(obj)))
        if len(obj["blocks"]) != 4:
            die("expected 4 Slack blocks for %s, got %d"
                % (label, len(obj["blocks"])))
        fields = obj["blocks"][1]["fields"]
        if len(fields) != 4:
            die("expected 4 fields for %s" % label)

        # Compare each field in full. A substring probe is not enough: the
        # repository slug also occurs inside the repository URL, so wiring the
        # label to the wrong --arg still contains the expected text. Exact
        # equality is the payload contract -- an intentional reword updates
        # this list, and the mismatch message shows both sides.
        ctx = dict(STUB)
        ctx.update(extra)
        actor = ("schedule" if ctx["EVENT"] == "schedule"
                 else (ctx["ACTOR"] or "unknown"))
        expected = [
            "*Repository*\n<%s|%s>" % (ctx["REPOSITORY_URL"], ctx["REPOSITORY"]),
            "*Commit*\n<%s/commit/%s|`%s`>"
            % (ctx["REPOSITORY_URL"], ctx["HEAD_SHA"], ctx["HEAD_SHA"][:7]),
            "*Triggered by*\n%s" % actor,
            "*Run*\n<%s|#%s>" % (ctx["RUN_URL"], ctx["RUN_NUMBER"]),
        ]
        for i, want in enumerate(expected):
            if fields[i]["text"] != want:
                die("field %d rendered wrong for %s\n  expected: %r\n"
                    "  actual:   %r" % (i, label, want, fields[i]["text"]))

        for block in obj["blocks"]:
            for text in ([block["text"]["text"]] if "text" in block else []) + \
                        [f["text"] for f in block.get("fields", [])] + \
                        [e["text"] for e in block.get("elements", [])]:
                if text.strip() in ("", "*Impact*"):
                    die("empty text rendered for %s: %r" % (label, text))

        impact = obj["blocks"][2]["text"]["text"]
        generic = FALLBACK_IMPACT_PREFIX in impact
        # A listed trigger falling through to the generic arm means the case
        # statement and the trigger list drifted apart.
        if listed and generic:
            die("%r is triggered but has no impact case arm (trigger list and "
                "case statement drifted)" % workflow)
        if not listed and not generic:
            die("unlisted %r did not hit the fallback arm" % workflow)

    # --- 3. a missing webhook must fail loudly, not no-op
    proc, payload = run({
        "WORKFLOW_NAME": triggers[0], "SLACK_WEBHOOK_URL": ""
    })
    if proc.returncode == 0:
        die("empty SLACK_WEBHOOK_URL must fail the step, but it exited 0")
    if "::error::" not in proc.stdout + proc.stderr:
        die("empty SLACK_WEBHOOK_URL must emit an ::error:: annotation")
finally:
    shutil.rmtree(tmp, ignore_errors=True)

# --- authority warning: the compile half is version-dependent

version = subprocess.run(
    ["jq", "--version"], capture_output=True, text=True
).stdout.strip()
digits = re.search(r"(\d+)\.(\d+)", version)
if digits and (int(digits.group(1)), int(digits.group(2))) >= (1, 8):
    sys.stderr.write(
        "warning: %s relaxed the object-construction grammar, so this run "
        "cannot catch a jq <= 1.7 parse regression. Only CI (ubuntu-latest, "
        "jq 1.7) enforces that half.\n" % version
    )

print("main CI notification payload builds on %s for all %d triggers "
      "(+ fallback, schedule, missing actor); webhook guard fails loudly"
      % (version, len(triggers)))
EOF
