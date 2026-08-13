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
# The two copies are byte-identical except for the host browser's name in prose
# and comments, so this normalizes the prose token `Edge` -> `Chrome` in the Edge
# copy and then demands an exact match. The `chrome.*` extension API namespace is
# spelled the same in both browsers and is deliberately NOT normalized, so a real
# change to an API call can never be masked by the normalization.
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
    "content/gmail-compose.js",
    "background.js",
]

# Prose/comment mentions of the host browser are the only sanctioned delta.
# Word-boundary anchored so the `chrome.*` API namespace is left untouched.
BROWSER_PROSE = re.compile(r"\bEdge\b")

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

    chrome_text = chrome_path.read_text()
    edge_text = edge_path.read_text()
    normalized = BROWSER_PROSE.sub("Chrome", edge_text)

    if normalized == chrome_text:
        continue

    diff = "".join(
        difflib.unified_diff(
            chrome_text.splitlines(keepends=True),
            normalized.splitlines(keepends=True),
            fromfile=str(chrome_path),
            tofile=f"{edge_path} (normalized: Edge -> Chrome)",
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
