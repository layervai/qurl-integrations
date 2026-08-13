#!/usr/bin/env bash
# Self-test for scripts/check-i18n-parity.sh.
#
# The checker's whole value is that it fails on drift the rest of CI cannot see,
# so a green run proves nothing unless the failure paths are exercised too. Each
# case builds a valid two-extension fixture, applies exactly one mutation, and
# asserts the checker's exit status and the failure text that names the cause.
# The unmutated baseline runs first, so a checker that fails on everything is
# caught rather than reading as thorough.
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
checker="$repo_root/scripts/check-i18n-parity.sh"
tmp_parent="$(mktemp -d)"

export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_NOSYSTEM=1

trap 'rm -rf "$tmp_parent"' EXIT

case_no=0

write_file() {
  local path="$1"
  local content="$2"
  mkdir -p "$(dirname "$path")"
  printf '%s\n' "$content" > "$path"
}

# A minimal but structurally faithful pair of extensions: the two sanctioned
# deltas (ext_name, permission_request_confirm), one shared key, the ext_desc
# the manifests reference, and each popup's two pre-i18n ext_name mirrors.
seed_valid() {
  local dir="$1"

  local chrome_catalog='{
  "ext_name": { "message": "qURL Agent", "description": "Extension name" },
  "ext_desc": { "message": "Upload files to qURL.", "description": "Extension description" },
  "copy_btn": { "message": "Copy the qURL link", "description": "Copy button" },
  "permission_request_confirm": { "message": "Allow access? Chrome will show a prompt next.", "description": "Permission confirm" }
}'
  local edge_catalog='{
  "ext_name": { "message": "qURL File Upload for Edge", "description": "Extension name" },
  "ext_desc": { "message": "Upload files to qURL.", "description": "Extension description" },
  "copy_btn": { "message": "Copy the qURL link", "description": "Copy button" },
  "permission_request_confirm": { "message": "Allow access? Edge will show a prompt next.", "description": "Permission confirm" }
}'
  local manifest='{
  "manifest_version": 3,
  "name": "__MSG_ext_name__",
  "default_locale": "en",
  "description": "__MSG_ext_desc__"
}'

  write_file "$dir/apps/chrome-extension/_locales/en/messages.json" "$chrome_catalog"
  write_file "$dir/apps/edge-extension/_locales/en/messages.json" "$edge_catalog"
  write_file "$dir/apps/chrome-extension/manifest.json" "$manifest"
  write_file "$dir/apps/edge-extension/manifest.json" "$manifest"
  write_file "$dir/apps/chrome-extension/popup/popup.html" '<html>
  <title>qURL Agent</title>
  <div class="title" data-i18n="ext_name">qURL Agent</div>
</html>'
  write_file "$dir/apps/edge-extension/popup/popup.html" '<html>
  <title>qURL File Upload for Edge</title>
  <div class="title" data-i18n="ext_name">qURL File Upload for Edge</div>
</html>'
}

# run_case <name> <expected_status> <expected_output> [mutation…]
# The mutation is run with the fixture root as cwd; omit it for the baseline.
run_case() {
  local name="$1"
  local expected_status="$2"
  local expected_output="$3"
  shift 3

  case_no=$((case_no + 1))
  local workdir="$tmp_parent/$case_no-$name"
  mkdir -p "$workdir"
  # The checker only needs a repo root; fixtures intentionally do not commit.
  git -C "$workdir" init -q
  seed_valid "$workdir"

  if (( $# )); then
    (cd "$workdir" && "$@")
  fi

  set +e
  local output
  output="$(cd "$workdir" && "$checker" 2>&1)"
  local status="$?"
  set -e

  if [[ "$status" != "$expected_status" ]]; then
    printf '%s: expected exit %s, got %s\n%s\n' "$name" "$expected_status" "$status" "$output" >&2
    exit 1
  fi

  if [[ -n "$expected_output" && "$output" != *"$expected_output"* ]]; then
    printf '%s: expected output to contain %q\n%s\n' "$name" "$expected_output" "$output" >&2
    exit 1
  fi
}

# Rewrite one JSON field in a fixture catalog or manifest.
set_json() { # <relative path> <key> <field> <value>
  python3 -c '
import json, sys
path, key, field, value = sys.argv[1:5]
data = json.load(open(path))
if field:
    data[key][field] = value
else:
    data[key] = value
json.dump(data, open(path, "w"), indent=2)
' "$@"
}

drop_json_key() { # <relative path> <key>
  python3 -c '
import json, sys
path, key = sys.argv[1:3]
data = json.load(open(path))
del data[key]
json.dump(data, open(path, "w"), indent=2)
' "$@"
}

# Give Edge Chrome's ext_name message, with a description of the caller's
# choosing, and update Edge's popup.html mirrors so rule 6 does not mask the
# result. This is the motivating mutation, parameterized by the field that
# decides whether it lands on rule 3 or rule 2.
copy_chrome_ext_name_to_edge() { # <edge description>
  python3 -c '
import json, sys
description = sys.argv[1]
chrome = json.load(open("apps/chrome-extension/_locales/en/messages.json"))
path = "apps/edge-extension/_locales/en/messages.json"
edge = json.load(open(path))
name = chrome["ext_name"]["message"]
edge["ext_name"] = {"message": name, "description": description}
json.dump(edge, open(path, "w"), indent=2)

popup = "apps/edge-extension/popup/popup.html"
markup = open(popup).read()
open(popup, "w").write(markup.replace("qURL File Upload for Edge", name))
' "$@"
}

# Give BOTH catalogs the same placeholder example naming Chrome. Identical on
# both sides, so rule 2 is satisfied and only rule 4 — reaching a nested string
# rather than a top-level field — can fire, on the Edge copy.
add_wrong_browser_placeholder() {
  python3 -c '
for app in ("chrome", "edge"):
    import json
    path = f"apps/{app}-extension/_locales/en/messages.json"
    data = json.load(open(path))
    data["permission_request_confirm"]["placeholders"] = {
        "origin": {"example": "https://files.example.com seen from Chrome"}
    }
    json.dump(data, open(path, "w"), indent=2)
'
}

CHROME_CATALOG="apps/chrome-extension/_locales/en/messages.json"
EDGE_CATALOG="apps/edge-extension/_locales/en/messages.json"

# Baseline: the unmutated fixture must pass, or every case below is vacuous.
run_case baseline-in-parity 0 "i18n in parity"

# Rule 1 — key sets must match, in both directions.
run_case key-missing-from-edge 1 "missing 1 key(s) present in the chrome catalog: copy_btn" \
  drop_json_key "$EDGE_CATALOG" copy_btn

run_case key-missing-from-chrome 1 "missing 1 key(s) present in the edge catalog: copy_btn" \
  drop_json_key "$CHROME_CATALOG" copy_btn

# Rule 2 — a shared key that quietly drifts on one side. This is the shape of
# the #1048 drift: no browser name involved, key sets intact.
run_case shared-message-drift 1 "copy_btn: catalogs disagree and the key is not a sanctioned delta" \
  set_json "$EDGE_CATALOG" copy_btn message "Copy the link"

run_case shared-description-drift 1 "copy_btn: catalogs disagree and the key is not a sanctioned delta" \
  set_json "$EDGE_CATALOG" copy_btn description "Copy button label"

# Rule 3 — the mutation that motivated this script: Edge takes Chrome's name.
# Exempt from rule 2, and "qURL Agent" names no browser for rule 4, so only the
# allowlist-non-vacuity rule stands between this and the Edge Add-ons store.
run_case edge-takes-chrome-ext-name 1 "ext_name: listed in SANCTIONED_DELTAS" \
  set_json "$EDGE_CATALOG" ext_name message "qURL Agent"

run_case sanctioned-key-collapsed 1 "permission_request_confirm: listed in SANCTIONED_DELTAS" \
  set_json "$EDGE_CATALOG" permission_request_confirm message "Allow access? Chrome will show a prompt next."

# The exemption is `message` only: a sanctioned key whose message was copied
# across still fails on its other fields. Without this, the copied message hides
# behind the diverging description — the entries are unequal, so a whole-entry
# rule 3 would see a difference and pass, and Edge ships under Chrome's name.
run_case sanctioned-message-copied-description-drifts 1 "only its \`message\` may differ" \
  copy_chrome_ext_name_to_edge "Edge extension name"

run_case sanctioned-description-drift 1 "only its \`message\` may differ" \
  set_json "$EDGE_CATALOG" ext_name description "Edge extension name"

# Rule 4 — a sanctioned delta copied across without being retargeted. It still
# differs from Chrome's, so rule 3 is satisfied and only this rule fires.
run_case edge-names-chrome 1 "permission_request_confirm.message names the wrong browser" \
  set_json "$EDGE_CATALOG" permission_request_confirm message "Allow access? Chrome shows a prompt."

run_case chrome-names-edge 1 "permission_request_confirm.message names the wrong browser" \
  set_json "$CHROME_CATALOG" permission_request_confirm message "Allow access? Edge shows a prompt."

# The rule reaches description too, not just the user-visible message.
run_case wrong-browser-in-description 1 "copy_btn.description names the wrong browser" \
  set_json "$EDGE_CATALOG" copy_btn description "Copy button, Chrome only"

# Rule 4 reaches nested strings, not just top-level fields. Both catalogs carry
# the same placeholder example here, so rule 2 is satisfied and only rule 4 can
# fire — on the Edge copy, for naming Chrome.
run_case wrong-browser-in-nested-placeholder 1 "permission_request_confirm.placeholders.origin.example names the wrong browser" \
  add_wrong_browser_placeholder

# Rule 5 — manifest placeholders have no fallback: an unresolved key makes the
# extension fail to load or list blank, and i18n-coverage.test.js never sees it.
run_case manifest-key-absent-from-catalog 1 "references 1 key(s) absent from" \
  set_json "apps/edge-extension/manifest.json" name "" "__MSG_ext_title__"

run_case manifest-stopped-localizing 1 "no __MSG_*__ references found" \
  python3 -c 'import json; json.dump({"manifest_version": 3, "name": "qURL", "description": "x"}, open("apps/edge-extension/manifest.json", "w"))'

run_case manifest-missing 1 "manifest.json: missing" \
  rm "apps/edge-extension/manifest.json"

run_case manifest-invalid-json 1 "invalid JSON" \
  python3 -c 'open("apps/chrome-extension/manifest.json", "w").write("{ not json")'

# Rule 6 — popup.html's two pre-i18n mirrors, which no lockstep file covers.
run_case popup-title-mismatch 1 "<title> reads 'qURL Agent' but this app's ext_name is" \
  python3 -c 'p = "apps/edge-extension/popup/popup.html"; s = open(p).read(); open(p, "w").write(s.replace("<title>qURL File Upload for Edge</title>", "<title>qURL Agent</title>"))'

run_case popup-header-mismatch 1 "data-i18n=\"ext_name\" reads 'qURL Agent' but this app's ext_name is" \
  python3 -c 'p = "apps/edge-extension/popup/popup.html"; s = open(p).read(); open(p, "w").write(s.replace(">qURL File Upload for Edge</div>", ">qURL Agent</div>"))'

# The header mirror is matched on its data-i18n key alone, so extra classes and
# reordered attributes still resolve — an innocent CSS refactor must not read as
# "the scan is broken".
run_case popup-header-tolerates-extra-class 0 "i18n in parity" \
  python3 -c 'p = "apps/edge-extension/popup/popup.html"; s = open(p).read(); open(p, "w").write(s.replace("<div class=\"title\" data-i18n=\"ext_name\">", "<div data-i18n=\"ext_name\" class=\"title utility\">"))'

# Rule 6 is symmetric over both apps; the cases above only mutate Edge.
run_case popup-title-mismatch-chrome-side 1 "<title> reads 'qURL File Upload for Edge' but this app's ext_name is" \
  python3 -c 'p = "apps/chrome-extension/popup/popup.html"; s = open(p).read(); open(p, "w").write(s.replace("<title>qURL Agent</title>", "<title>qURL File Upload for Edge</title>"))'

run_case popup-mirror-shape-changed 1 "no <title> mirror of ext_name found" \
  python3 -c 'p = "apps/edge-extension/popup/popup.html"; s = open(p).read(); open(p, "w").write(s.replace("<title>qURL File Upload for Edge</title>", ""))'

# Malformed or absent inputs must fail loudly rather than skipping silently.
run_case catalog-missing 1 "missing" \
  rm "$EDGE_CATALOG"

run_case catalog-invalid-json 1 "invalid JSON" \
  python3 -c 'open("apps/edge-extension/_locales/en/messages.json", "w").write("{ not json")'

# A wrong-shape-but-valid-JSON entry must produce a curated failure, not an
# AttributeError traceback out of rule 2.
run_case entry-not-an-object 1 "copy_btn is str, not an object" \
  set_json "$EDGE_CATALOG" copy_btn "" "Copy the qURL link"

run_case entry-message-not-a-string 1 "ext_name has no string \`message\`" \
  python3 -c 'import json; p = "apps/chrome-extension/_locales/en/messages.json"; d = json.load(open(p)); d["ext_name"] = {"description": "Extension name"}; json.dump(d, open(p, "w"), indent=2)'

run_case popup-missing 1 "popup.html: missing" \
  rm "apps/edge-extension/popup/popup.html"

printf 'check-i18n-parity.sh: %d cases passed\n' "$case_no"
