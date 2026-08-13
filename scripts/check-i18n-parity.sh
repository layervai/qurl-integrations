#!/bin/sh
# Verify the Chrome and Edge extensions' message catalogs have not drifted.
#
# `_locales/en/messages.json` is deliberately excluded from
# check-extension-lockstep.sh because it carries sanctioned wording deltas (the
# extension's own name, and the browser named in the permission prompt). That
# exclusion left the catalogs with no cross-app check at all: each app's
# test/i18n-coverage.test.js only proves that keys referenced in that app's
# source exist in that app's catalog and are non-empty. It never compares the
# two catalogs and never looks at message *content*, so Edge could ship under
# the Chrome extension's name with every suite green.
#
# This check cannot live in test/i18n-coverage.test.js, because that file IS a
# lockstep file: the lockstep normalization masks `\b(?:Chrome|Edge)\b` on both
# sides, so any assertion there that pins browser-specific copy is erased before
# the comparison. Pinning that copy requires a checker outside the lockstep file
# set, which is why this is a standalone script alongside its sibling.
#
# Six rules, in the order they are reported:
#   1. The two catalogs declare the same key set.
#   2. Every key outside SANCTIONED_DELTAS is byte-identical in both catalogs.
#   3. Every key inside SANCTIONED_DELTAS actually differs. Without this the
#      allowlist silently becomes a hole: setting Edge's ext_name to Chrome's
#      value passes rules 2 and 4 (it is exempt from 2, and "qURL Agent" names
#      no browser for 4), and Edge ships to the Edge Add-ons store under the
#      Chrome extension's name.
#   4. Neither catalog names the other browser, anywhere. This needs no
#      allowlist: today the only browser names in either catalog are each copy's
#      own, inside the two sanctioned keys. It is what catches a sanctioned
#      delta that was copied across without being retargeted.
#   5. Every __MSG_key__ reference in manifest.json resolves in that app's
#      catalog. i18n-coverage.test.js does not scan manifest.json, and neither
#      of its regexes matches the __MSG_*__ syntax, so an unresolved key there
#      makes the extension fail to load or list with a blank name, CI green.
#   6. Each popup.html's two hard-coded ext_name mirrors (the static <title> and
#      the header .title div, which render before chrome.i18n resolves) match
#      that app's own ext_name. popup.html is not a lockstep file precisely
#      because of these, so nothing else pins them.
#
# See the Chrome<->Edge lockstep section in CLAUDE.md.
set -eu

cd "$(git rev-parse --show-toplevel)"

command -v python3 >/dev/null 2>&1 || {
    echo "Error: python3 is required (JSON parsing); install python3 and retry" >&2
    exit 1
}

python3 - <<'EOF'
import html
import json
import pathlib
import re
import sys

APPS = {
    "chrome": pathlib.Path("apps/chrome-extension"),
    "edge": pathlib.Path("apps/edge-extension"),
}

CATALOG = "_locales/en/messages.json"

# The other browser's name must never appear in a catalog. Values are the
# regexes applied to the OTHER app's catalog.
FOREIGN_BROWSER = {
    "chrome": re.compile(r"\bEdge\b"),
    "edge": re.compile(r"\bChrome\b"),
}

# Keys allowed to differ between the two catalogs. Keep in sync with
# 'Intentional differences' in CLAUDE.md. A key listed here is exempt from rule
# 2 and REQUIRED by rule 3 to differ, so adding one that happens to match today
# fails immediately rather than quietly widening the hole.
SANCTIONED_DELTAS = {
    "ext_name": "each store lists the extension under its own name",
    "permission_request_confirm": "names the host browser that shows the prompt",
}

MANIFEST_KEY = re.compile(r"__MSG_([A-Za-z0-9_]+)__")

# popup.html renders the extension name twice before chrome.i18n resolves: the
# static <title>, and the header div that data-i18n later overwrites. Both must
# already hold this app's own ext_name or the popup visibly swaps names on open.
POPUP_MIRRORS = (
    ("<title>", re.compile(r"<title>(.*?)</title>", re.DOTALL)),
    (
        'data-i18n="ext_name"',
        re.compile(r'<div class="title" data-i18n="ext_name">(.*?)</div>', re.DOTALL),
    ),
)

failures = []


def load_json(path):
    try:
        return json.loads(path.read_text())
    except FileNotFoundError:
        failures.append(f"{path}: missing")
    except json.JSONDecodeError as exc:
        failures.append(f"{path}: invalid JSON: {exc}")
    return None


catalogs = {name: load_json(root / CATALOG) for name, root in APPS.items()}
if any(catalog is None for catalog in catalogs.values()):
    print("Chrome<->Edge i18n parity check could not run.\n", file=sys.stderr)
    for failure in failures:
        print(failure, file=sys.stderr)
    raise SystemExit(1)

chrome, edge = catalogs["chrome"], catalogs["edge"]

# Rule 1: same key set.
for name, other, missing in (
    ("edge", "chrome", sorted(set(chrome) - set(edge))),
    ("chrome", "edge", sorted(set(edge) - set(chrome))),
):
    if missing:
        failures.append(
            f"{APPS[name] / CATALOG}: missing {len(missing)} key(s) present in the "
            f"{other} catalog: {', '.join(missing)}\n"
            "  Every key must exist in both catalogs. A key missing here falls back "
            "to the hard-coded English literal in getMessage's second argument."
        )

# Rule 2: shared keys are identical unless sanctioned.
for key in sorted(set(chrome) & set(edge)):
    if key in SANCTIONED_DELTAS or chrome[key] == edge[key]:
        continue
    fields = sorted(
        field
        for field in set(chrome[key]) | set(edge[key])
        if chrome[key].get(field) != edge[key].get(field)
    )
    detail = "\n".join(
        f"    {field}:\n      chrome: {chrome[key].get(field)!r}\n"
        f"      edge:   {edge[key].get(field)!r}"
        for field in fields
    )
    failures.append(
        f"{key}: catalogs disagree and the key is not a sanctioned delta:\n{detail}\n"
        "  Apply the same wording to both copies. If the divergence is deliberate, "
        "add the key to SANCTIONED_DELTAS here and to 'Intentional differences' in "
        "CLAUDE.md."
    )

# Rule 3: sanctioned deltas must be real deltas, or the allowlist is a hole.
for key, reason in sorted(SANCTIONED_DELTAS.items()):
    if key not in chrome or key not in edge:
        # Rule 1 already reported the absence; do not pile on.
        continue
    if chrome[key] != edge[key]:
        continue
    failures.append(
        f"{key}: listed in SANCTIONED_DELTAS ({reason}) but the two catalogs "
        f"agree: {chrome[key].get('message')!r}\n"
        "  This key is exempt from the equality check, so an accidental copy "
        "across the fork would otherwise ship silently. Either restore the "
        "per-browser wording, or drop the key from SANCTIONED_DELTAS here and "
        "from 'Intentional differences' in CLAUDE.md."
    )

# Rule 4: neither catalog names the other browser.
for name, catalog in (("chrome", chrome), ("edge", edge)):
    pattern = FOREIGN_BROWSER[name]
    for key in sorted(catalog):
        entry = catalog[key]
        if not isinstance(entry, dict):
            continue
        for field in sorted(entry):
            value = entry[field]
            if not isinstance(value, str) or not pattern.search(value):
                continue
            failures.append(
                f"{APPS[name] / CATALOG}: {key}.{field} names the wrong browser: "
                f"{value!r}\n"
                f"  The {name} catalog must never mention the other browser. "
                "Retarget the wording to this copy's own browser."
            )

# Rule 5: manifest __MSG_*__ references resolve.
for name, root in APPS.items():
    manifest = load_json(root / "manifest.json")
    if manifest is None:
        continue
    referenced = sorted(set(MANIFEST_KEY.findall(json.dumps(manifest))))
    if not referenced:
        failures.append(
            f"{root / 'manifest.json'}: no __MSG_*__ references found — the scan is "
            "broken, or the manifest stopped localizing its name and description."
        )
        continue
    missing = [key for key in referenced if key not in catalogs[name]]
    if missing:
        failures.append(
            f"{root / 'manifest.json'}: references {len(missing)} key(s) absent from "
            f"{root / CATALOG}: {', '.join(missing)}\n"
            "  Chrome and Edge do not fall back for manifest placeholders: the "
            "extension fails to load, or lists with a blank name."
        )

# Rule 6: popup.html's pre-i18n mirrors match this app's own ext_name.
for name, root in APPS.items():
    expected = catalogs[name].get("ext_name", {}).get("message")
    if expected is None:
        continue  # rule 1/5 already covers a missing ext_name
    popup = root / "popup/popup.html"
    try:
        markup = popup.read_text()
    except FileNotFoundError:
        failures.append(f"{popup}: missing")
        continue
    for label, pattern in POPUP_MIRRORS:
        match = pattern.search(markup)
        if match is None:
            failures.append(
                f"{popup}: no {label} mirror of ext_name found — the scan is broken, "
                "or the markup changed shape."
            )
            continue
        actual = html.unescape(match.group(1)).strip()
        if actual != expected:
            failures.append(
                f"{popup}: {label} reads {actual!r} but this app's ext_name is "
                f"{expected!r}\n"
                "  Both mirrors render before chrome.i18n resolves, so a mismatch "
                "makes the popup visibly swap names on open."
            )

if failures:
    print("Chrome<->Edge i18n parity drift detected.\n", file=sys.stderr)
    for failure in failures:
        print(failure, file=sys.stderr)
    print(
        "\nThese catalogs are outside check-extension-lockstep.sh by design; this "
        "script is the only thing comparing them. See the Chrome<->Edge lockstep "
        "section in CLAUDE.md.",
        file=sys.stderr,
    )
    raise SystemExit(1)

shared = len(set(chrome) & set(edge)) - len(SANCTIONED_DELTAS)
print(
    f"chrome-extension/edge-extension i18n in parity "
    f"({shared} identical keys, {len(SANCTIONED_DELTAS)} sanctioned deltas)"
)
EOF
