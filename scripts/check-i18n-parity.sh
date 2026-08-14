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
#   2. Every key outside SANCTIONED_DELTAS is structurally identical in both
#      catalogs.
#   3. Every key inside SANCTIONED_DELTAS has a genuinely different `message`.
#      Without this the allowlist silently becomes a hole: setting Edge's
#      ext_name to Chrome's value passes rules 2 and 4 (it is exempt from 2, and
#      "qURL Agent" names no browser for 4), and Edge ships to the Edge Add-ons
#      store under the Chrome extension's name. Note this compares `message`
#      rather than the whole entry, and rule 2 above exempts only `message` —
#      otherwise a copied message could hide behind a diverging description:
#      the entries would be unequal, so a whole-entry rule 3 would pass.
#   4. Neither catalog names the other browser, in any string at any depth. This
#      needs no allowlist: today the only browser names in either catalog are
#      each copy's own, inside the two sanctioned keys. It is what catches a
#      sanctioned delta that was copied across without being retargeted. Matched
#      case-sensitively, as in the sibling lockstep script, so that the lowercase
#      `chrome.*` API namespace never reads as prose.
#   5. Every __MSG_key__ reference in manifest.json resolves in that app's
#      catalog. i18n-coverage.test.js does not scan manifest.json, and neither
#      of its regexes matches the __MSG_*__ syntax, so an unresolved key there
#      makes the extension fail to load or list with a blank name, CI green.
#   6. Each popup.html's two hard-coded ext_name mirrors (the static <title> and
#      the header .title div, which render before chrome.i18n resolves) match
#      that app's own ext_name. popup.html is not a lockstep file precisely
#      because of these, so nothing else pins them.
#
# Three accepted limits, all in the sanctioned keys, which are the only place a
# browser name lives:
#   - Rule 4 rejects the OTHER fork's name, so a mistarget to a THIRD browser
#     ("Safari will show…" in Edge's copy) clears every rule. Asserting each
#     copy names its own browser instead would false-positive on ext_name, whose
#     Chrome value ("qURL Agent") names no browser at all.
#   - Rule 4 is case-sensitive (see rule 4 below), so a lowercase browser name
#     in a user-facing message ("open in edge") slips past it. Accepted: browser
#     names are proper nouns and every catalog string capitalizes them, and
#     matching case-insensitively would diverge from the sibling script.
#   - Rule 2 exempts a sanctioned key on `message` only, so its `description`
#     and placeholders must stay identical across the two catalogs. That is
#     deliberate; a legitimately per-browser `description` would need its own
#     carve-out here and in CLAUDE.md.
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

# Both extensions currently ship English only. Extend this checker when another
# locale is added so catalog parity cannot silently remain English-only.
CATALOG = "_locales/en/messages.json"

# The other browser's name must never appear in a catalog. Values are the
# regexes applied to the OTHER app's catalog.
# This reaches every string in the entry, `description` included — not just the
# user-visible `message`. Combined with rule 2 (descriptions must be identical
# across catalogs) it means a description can never name EITHER browser: an
# identical "…in Chrome" description on both sides still trips this on the Edge
# copy. That is deliberate and slightly stronger than "pin what a user sees" —
# descriptions are developer-facing and lockstep-identical anyway, so a
# browser-specific one is a fork mistake rather than a legitimate need.
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

# The character class deliberately excludes `@`, which skips Chrome's predefined
# placeholders (__MSG_@@ui_locale__, __MSG_@@bidi_dir__ and friends). Those are
# supplied by the browser and never appear in a catalog, so widening this to
# match them would report every one as missing.
MANIFEST_KEY = re.compile(r"__MSG_([A-Za-z0-9_]+)__")

# Markup nested inside a popup mirror, stripped before the text is compared.
CHILD_TAG = re.compile(r"<[^>]*>")

# popup.html renders the extension name twice before chrome.i18n resolves: the
# static <title>, and the header div that data-i18n later overwrites. Both must
# already hold this app's own ext_name or the popup visibly swaps names on open.
# Only the FIRST match of each pattern is checked; each appears exactly once
# today, and a second copy of either would be a markup bug of its own.
POPUP_MIRRORS = (
    ("<title>", re.compile(r"<title>(?P<text>.*?)</title>", re.DOTALL)),
    (
        'data-i18n="ext_name"',
        # Identified by its data-i18n key alone — the tag name, its other
        # attributes and their order are all free. So neither a CSS refactor
        # (class="title utility") nor a change of element (<div> to <h1>)
        # surfaces as "the scan is broken" on correct markup. The backreference
        # keeps the closing tag matched to whatever the opening one was.
        re.compile(
            r'<(?P<tag>[a-zA-Z][a-zA-Z0-9]*)\b[^>]*\bdata-i18n="ext_name"[^>]*>'
            r"(?P<text>.*?)</(?P=tag)>",
            re.DOTALL,
        ),
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


def bail(headline):
    print(headline + "\n", file=sys.stderr)
    for failure in failures:
        print(failure, file=sys.stderr)
    raise SystemExit(1)


CANNOT_RUN = "Chrome<->Edge i18n parity check could not run."

def load_text(path):
    try:
        return path.read_text()
    except FileNotFoundError:
        failures.append(f"{path}: missing")
    return None


catalogs = {name: load_json(root / CATALOG) for name, root in APPS.items()}
if any(catalog is None for catalog in catalogs.values()):
    bail(CANNOT_RUN)

chrome, edge = catalogs["chrome"], catalogs["edge"]
manifests = {name: load_json(root / "manifest.json") for name, root in APPS.items()}
popups = {name: load_text(root / "popup/popup.html") for name, root in APPS.items()}

# Input gate, before any rule runs. Every catalog must be an object whose entries
# are objects carrying a string `message`, every manifest must be an object, and
# every file loaded above must have been readable. A valid-JSON-but-wrong-shape
# input would otherwise throw out of the parity rules: a non-zero exit, but a
# traceback rather than a curated failure. A missing or unparseable input is an
# infrastructure problem, not drift, so it bails under CANNOT_RUN rather than
# being reported beneath the drift banner. Everything past this point is
# known-good.
for name, catalog in (("chrome", chrome), ("edge", edge)):
    if not isinstance(catalog, dict):
        failures.append(
            f"{APPS[name] / CATALOG}: top level is "
            f"{type(catalog).__name__}, not an object."
        )
    else:
        for key in sorted(catalog):
            entry = catalog[key]
            if not isinstance(entry, dict):
                failures.append(
                    f"{APPS[name] / CATALOG}: {key} is {type(entry).__name__}, not an "
                    "object. Every entry must be "
                    "{\"message\": ..., \"description\": ...}."
                )
            elif not isinstance(entry.get("message"), str):
                failures.append(
                    f"{APPS[name] / CATALOG}: {key} has no string `message`."
                )

    # Same gate for the manifest: rules 4 and 5 iterate it as a mapping, so a
    # valid-JSON-but-wrong-shape document (a list, say) would throw rather than
    # report. Missing and unparseable manifests are already covered by the
    # load_json failures above.
    if not isinstance(manifests[name], dict):
        failures.append(
            f"{APPS[name] / 'manifest.json'}: top level is "
            f"{type(manifests[name]).__name__}, not an object."
        )
if failures:
    bail(CANNOT_RUN)

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

# Rule 2: shared keys are identical. A sanctioned key is exempt on `message`
# ONLY — its description, placeholders and everything else still have to match,
# and rule 3 below requires the exempted message to actually differ. Exempting
# the whole entry instead would reopen the hole rule 3 exists to close: an entry
# whose message was copied across but whose description drifted is unequal, so a
# whole-entry rule 3 would see a difference and pass.
for key in sorted(set(chrome) & set(edge)):
    exempt = {"message"} if key in SANCTIONED_DELTAS else set()
    fields = sorted(
        field
        for field in (set(chrome[key]) | set(edge[key])) - exempt
        if chrome[key].get(field) != edge[key].get(field)
    )
    if not fields:
        continue
    detail = "\n".join(
        f"    {field}:\n      chrome: {chrome[key].get(field)!r}\n"
        f"      edge:   {edge[key].get(field)!r}"
        for field in fields
    )
    scope = (
        "is a sanctioned delta, but only its `message` may differ"
        if key in SANCTIONED_DELTAS
        else "is not a sanctioned delta"
    )
    failures.append(
        f"{key}: catalogs disagree and the key {scope}:\n{detail}\n"
        "  Apply the same wording to both copies. If the divergence is deliberate, "
        "add the key to SANCTIONED_DELTAS here and to 'Intentional differences' in "
        "CLAUDE.md."
    )

# Rule 3: sanctioned deltas must be real deltas, or the allowlist is a hole.
for key, reason in sorted(SANCTIONED_DELTAS.items()):
    if key not in chrome or key not in edge:
        # Rule 1 already reported the absence; do not pile on.
        continue
    # The user-visible message specifically: a whole-entry compare would let a
    # copied message hide behind a diverging description and ship silently.
    if chrome[key].get("message") != edge[key].get("message"):
        continue
    failures.append(
        f"{key}: listed in SANCTIONED_DELTAS ({reason}) but both catalogs carry "
        f"the same message: {chrome[key].get('message')!r}\n"
        "  This key is exempt from the equality check, so an accidental copy "
        "across the fork would otherwise ship silently. Either restore the "
        "per-browser wording, or drop the key from SANCTIONED_DELTAS here and "
        "from 'Intentional differences' in CLAUDE.md."
    )

# Rule 4: neither catalog names the other browser.
def walk_strings(value, path):
    """Yield (dotted path, string) for every string anywhere under `value`."""
    if isinstance(value, str):
        yield path, value
    elif isinstance(value, dict):
        for field in sorted(value):
            yield from walk_strings(value[field], f"{path}.{field}")
    elif isinstance(value, list):
        for index, item in enumerate(value):
            yield from walk_strings(item, f"{path}[{index}]")


for name, catalog in (("chrome", chrome), ("edge", edge)):
    pattern = FOREIGN_BROWSER[name]

    # manifest.json is scanned alongside the catalog: it is outside the lockstep
    # check too (the version delta), and a name/description de-localized from
    # __MSG_*__ to a literal is user-visible in the store listing. Only string
    # VALUES are walked, so the `minimum_chrome_version` KEY is not matched —
    # and the __MSG_*__ placeholders themselves carry no browser name.
    sources = (
        (APPS[name] / CATALOG, catalog),
        (APPS[name] / "manifest.json", manifests[name]),
    )

    for source_path, document in sources:
        for key in sorted(document):
            for path, value in walk_strings(document[key], key):
                if not pattern.search(value):
                    continue
                failures.append(
                    f"{source_path}: {path} names the wrong browser: {value!r}\n"
                    f"  The {name} copy must never mention the other browser. "
                    "Retarget the wording to this copy's own browser."
                )

# Rule 5: manifest __MSG_*__ references resolve.
for name, root in APPS.items():
    manifest = manifests[name]
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
    markup = popups[name]
    for label, pattern in POPUP_MIRRORS:
        match = pattern.search(markup)
        if match is None:
            failures.append(
                f"{popup}: no {label} mirror of ext_name found — the scan is broken, "
                "or the markup changed shape."
            )
            continue
        # Child markup is stripped before comparing, so adding an icon or a
        # wrapping span inside the mirror does not read as a name mismatch —
        # the same tolerance the tag-agnostic pattern above provides. Then
        # stripped on both sides: surrounding whitespace in the markup is not
        # rendered, so comparing it against a raw catalog value would report a
        # difference the user could never see.
        actual = html.unescape(CHILD_TAG.sub("", match.group("text"))).strip()
        if actual != expected.strip():
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

identical = sum(1 for key in set(chrome) & set(edge) if chrome[key] == edge[key])
print(
    f"chrome-extension/edge-extension i18n in parity "
    f"({identical} identical keys, {len(SANCTIONED_DELTAS)} sanctioned deltas)"
)
EOF
