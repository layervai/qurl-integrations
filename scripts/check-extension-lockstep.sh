#!/bin/sh
# Verify apps/edge-extension/ has not silently drifted from apps/chrome-extension/.
# The Edge extension is a platform fork of the Chrome one, so both ship their own
# copy of the same security-sensitive logic: the multipart header-injection
# sanitizers, the https-only link normalization, and the optional-host-permission
# grant/revoke handling. A fix applied to one copy and forgotten in the other is
# invisible in review — the files live in different directories, so no diff ever
# shows the divergence — and the failure mode is a security bug that only affects
# one browser's users. See the Chrome<->Edge lockstep table in CLAUDE.md.
#
# The two copies are byte-identical except for the host browser's name and each
# copy's own app directory, so this masks those tokens on BOTH sides before
# demanding an exact match. Masking both sides rather than rewriting Edge ->
# Chrome keeps an ordinary comment about an "Edge case" from reading as drift.
# The lowercase `chrome.*` extension API namespace is spelled the same in both
# browsers and is deliberately NOT masked, so a real change to an API call can
# never be hidden by the normalization.
set -eu

cd "$(git rev-parse --show-toplevel)"

command -v python3 >/dev/null 2>&1 || {
    echo "Error: python3 is required (text normalization); install python3 and retry" >&2
    exit 1
}

python3 - <<'EOF'
import difflib
import pathlib
import re
import sys

CHROME = pathlib.Path("apps/chrome-extension")
EDGE = pathlib.Path("apps/edge-extension")

# Keep in sync with the lockstep table in CLAUDE.md.
LOCKSTEP_FILES = [
    "lib/qurl-api.js",
    "lib/qurl-compose-format.js",
    "lib/qurl-config.js",
    "lib/qurl-i18n.js",
    "content/gmail-compose.js",
    "popup/popup.js",
    "background.js",
]

# Prose/comment mentions of the host browser are the only sanctioned delta.
# Word-boundary anchored and case-sensitive so the lowercase `chrome.*` API
# namespace is left untouched.
BROWSER_PROSE = re.compile(r"\b(?:Chrome|Edge)\b")

# Each copy names its own app directory when pointing at sibling files (e.g. the
# `.env` path in lib/qurl-config.js's doc comment). Narrow by construction: it
# only matches this exact directory pair, so the trade-off is that a copy-pasted
# reference to the *other* extension's directory would be masked rather than
# reported. That is a doc-comment-level mistake, and masking it is worth having
# these files checked at all rather than excluded outright.
APP_DIR = re.compile(r"apps/(?:chrome|edge)-extension")

MASKS = ((BROWSER_PROSE, "<browser>"), (APP_DIR, "apps/<browser>-extension"))


def normalize(text):
    for pattern, replacement in MASKS:
        text = pattern.sub(replacement, text)
    return text

failures = []

for rel in LOCKSTEP_FILES:
    chrome_path, edge_path = CHROME / rel, EDGE / rel

    missing = [str(p) for p in (chrome_path, edge_path) if not p.is_file()]
    if missing:
        failures.append(
            f"{rel}: lockstep file missing: {', '.join(missing)}\n"
            "  If the file was renamed or removed, update LOCKSTEP_FILES here "
            "and the table in CLAUDE.md."
        )
        continue

    chrome_text = normalize(chrome_path.read_text())
    edge_text = normalize(edge_path.read_text())

    if chrome_text == edge_text:
        continue

    diff = "".join(
        difflib.unified_diff(
            chrome_text.splitlines(keepends=True),
            edge_text.splitlines(keepends=True),
            fromfile=f"{chrome_path} (normalized)",
            tofile=f"{edge_path} (normalized)",
        )
    )
    failures.append(f"{rel}: copies have diverged:\n{diff}")

if failures:
    print("Chrome<->Edge extension lockstep drift detected.\n", file=sys.stderr)
    for failure in failures:
        print(failure, file=sys.stderr)
    print(
        "Apply the same change to both copies. If the divergence is deliberate, "
        "document it under 'Intentional differences' in CLAUDE.md and adjust this "
        "check to match.",
        file=sys.stderr,
    )
    raise SystemExit(1)

print(f"chrome-extension/edge-extension in lockstep ({len(LOCKSTEP_FILES)} shared files)")
EOF
