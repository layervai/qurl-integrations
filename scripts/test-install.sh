#!/usr/bin/env bash
# Fixture tests for scripts/install.sh, in the style of
# test-validate-github-actions-pins.sh.
#
# All of install.sh's network I/O goes through `curl` invoked by name, so a
# PATH-stubbed curl serves fixtures from $FIXDIR: the releases-list API call
# streams $FIXDIR/releases.json (absent file = curl failure), and release
# asset downloads copy from $FIXDIR/assets/ by filename. INSTALL_DIR points
# at a per-case writable directory so the sudo branch never triggers, and
# every URL the installer requests is logged to $FIXDIR/curl.log for
# assertions. These cases pin the version-selection policy documented in
# install.sh: newest-first, bare v<digit> tags only, prereleases excluded by
# flag and by hyphen, VERSION= override skips the API entirely.
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
installer="$repo_root/scripts/install.sh"
tmp_parent="$(mktemp -d)"
trap 'rm -rf "$tmp_parent"' EXIT

case_no=0

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch_raw="$(uname -m)"
case "$arch_raw" in
  x86_64|amd64) arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  *) echo "unsupported test host arch: $arch_raw" >&2; exit 1 ;;
esac

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# Stub curl. install.sh invokes exactly two shapes:
#   curl -fsSL <url>            (releases API, to stdout)
#   curl -fsSL <url> -o <path>  (asset downloads)
make_curl_stub() {
  local stub_dir="$1"
  cat > "$stub_dir/curl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
url="" out=""
while (( $# )); do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    -*) shift ;;
    *) url="$1"; shift ;;
  esac
done
echo "$url" >> "$FIXDIR/curl.log"
case "$url" in
  *"/releases?per_page="*)
    [[ -f "$FIXDIR/releases.json" ]] || exit 22
    cat "$FIXDIR/releases.json"
    ;;
  *"/releases/download/"*)
    name="${url##*/}"
    [[ -f "$FIXDIR/assets/$name" ]] || exit 22
    cp "$FIXDIR/assets/$name" "$out"
    ;;
  *) exit 22 ;;
esac
STUB
  chmod +x "$stub_dir/curl"
}

# Build a release archive + goreleaser-style checksums.txt ("<sha>  <name>")
# for the given version under $FIXDIR/assets.
make_release_assets() {
  local fixdir="$1" version="$2"
  local build="$fixdir/build"
  mkdir -p "$build" "$fixdir/assets"
  printf '#!/bin/sh\necho "qurl-fixture %s"\n' "$version" > "$build/qurl"
  local archive="qurl_${version}_${os}_${arch}.tar.gz"
  tar -czf "$fixdir/assets/$archive" -C "$build" qurl
  printf '%s  %s\n' "$(sha256_of "$fixdir/assets/$archive")" "$archive" \
    > "$fixdir/assets/checksums.txt"
}

# run_case <name> <expected_status> <expected_output_substring> [VERSION=...]
# The caller prepares $tmp_parent/<case_no+1>-<name> as FIXDIR beforehand via
# new_fixdir.
new_fixdir() {
  local name="$1"
  local dir="$tmp_parent/$((case_no + 1))-$name"
  mkdir -p "$dir/assets" "$dir/bin"
  make_curl_stub "$dir"
  printf '%s' "$dir"
}

run_case() {
  local name="$1" expected_status="$2" expected_output="$3" version="${4:-}"
  case_no=$((case_no + 1))
  local fixdir="$tmp_parent/$case_no-$name"

  set +e
  local output
  output="$(cd "$fixdir" \
    && FIXDIR="$fixdir" PATH="$fixdir:$PATH" INSTALL_DIR="$fixdir/bin" \
       VERSION="$version" sh "$installer" 2>&1)"
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

assert_installed() {
  local fixdir="$1" version="$2"
  local bin="$fixdir/bin/qurl"
  [[ -x "$bin" ]] || { echo "$fixdir: expected executable $bin" >&2; exit 1; }
  [[ "$("$bin")" == "qurl-fixture $version" ]] \
    || { echo "$fixdir: installed binary is not the $version fixture" >&2; exit 1; }
  grep -q "/releases/download/v$version/qurl_${version}_${os}_${arch}.tar.gz" "$fixdir/curl.log" \
    || { echo "$fixdir: download URL did not use tag v$version" >&2; exit 1; }
}

assert_no_api_call() {
  local fixdir="$1"
  if grep -q "releases?per_page" "$fixdir/curl.log" 2>/dev/null; then
    echo "$fixdir: VERSION override must not query the releases API" >&2
    exit 1
  fi
}

# Fixture releases mirror the API's field order (author object before
# tag_name; draft/prerelease after it; assets with an uploader object),
# which the installer's tr-chunk parse depends on.
release_json() {
  local tag="$1" prerelease="$2"
  printf '{"url":"https://api.github.com/x","author":{"login":"github-actions[bot]","id":41898282},"node_id":"RE_x","tag_name":"%s","target_commitish":"main","name":"%s","draft":false,"prerelease":%s,"created_at":"2026-07-01T00:00:00Z","assets":[{"name":"a.txt","uploader":{"login":"bot","id":1}}],"body":"notes"}' \
    "$tag" "$tag" "$prerelease"
}

# --- Case 1: newest bare tag wins; prefixed component tags never match.
fixdir="$(new_fixdir picks-newest-bare-tag)"
printf '[%s,\n%s,\n%s,\n%s]\n' \
  "$(release_json slack-v0.9.9 false)" \
  "$(release_json chrome-extension-v1.0.2 false)" \
  "$(release_json v0.2.0 false)" \
  "$(release_json v0.1.0 false)" > "$fixdir/releases.json"
make_release_assets "$fixdir" 0.2.0
run_case picks-newest-bare-tag 0 "Installed qurl v0.2.0"
assert_installed "$fixdir" 0.2.0

# --- Case 2: prerelease-flagged releases are skipped.
fixdir="$(new_fixdir skips-prerelease-flag)"
printf '[%s,\n%s]\n' \
  "$(release_json v0.3.0 true)" \
  "$(release_json v0.2.0 false)" > "$fixdir/releases.json"
make_release_assets "$fixdir" 0.2.0
run_case skips-prerelease-flag 0 "Installed qurl v0.2.0"
assert_installed "$fixdir" 0.2.0

# --- Case 3: hyphenated (semver prerelease) tags are skipped even when the
# prerelease flag is (wrongly) false.
fixdir="$(new_fixdir skips-hyphenated-tag)"
printf '[%s,\n%s]\n' \
  "$(release_json v0.3.0-rc.1 false)" \
  "$(release_json v0.2.0 false)" > "$fixdir/releases.json"
make_release_assets "$fixdir" 0.2.0
run_case skips-hyphenated-tag 0 "Installed qurl v0.2.0"
assert_installed "$fixdir" 0.2.0

# --- Case 4: compact single-line JSON (release_json emits no whitespace)
# parses identically to the API's pretty-printed form.
fixdir="$(new_fixdir compact-json)"
printf '[%s,%s]' \
  "$(release_json slack-v1.0.0 false)" \
  "$(release_json v0.2.0 false)" > "$fixdir/releases.json"
make_release_assets "$fixdir" 0.2.0
run_case compact-json 0 "Installed qurl v0.2.0"
assert_installed "$fixdir" 0.2.0

# --- Case 5: only prefixed tags -> explicit no-CLI-release error.
fixdir="$(new_fixdir no-cli-release)"
printf '[%s,\n%s]\n' \
  "$(release_json slack-v0.9.9 false)" \
  "$(release_json discord-v0.3.0 false)" > "$fixdir/releases.json"
run_case no-cli-release 1 "Could not find a CLI release"

# --- Case 6: releases API failure -> network error, not "no release".
fixdir="$(new_fixdir api-failure)"
rm -f "$fixdir/releases.json"
run_case api-failure 1 "Failed to query GitHub releases"

# --- Case 7: VERSION override installs without touching the releases API.
fixdir="$(new_fixdir version-override)"
make_release_assets "$fixdir" 0.2.0
run_case version-override 0 "Installed qurl v0.2.0" 0.2.0
assert_installed "$fixdir" 0.2.0
assert_no_api_call "$fixdir"

# --- Case 8: VERSION override tolerates a leading v.
fixdir="$(new_fixdir version-override-v)"
make_release_assets "$fixdir" 0.2.0
run_case version-override-v 0 "Installed qurl v0.2.0" v0.2.0
assert_installed "$fixdir" 0.2.0
assert_no_api_call "$fixdir"

# --- Case 9: checksum mismatch is fatal.
fixdir="$(new_fixdir checksum-mismatch)"
make_release_assets "$fixdir" 0.2.0
archive="qurl_0.2.0_${os}_${arch}.tar.gz"
printf '%s  %s\n' "0000000000000000000000000000000000000000000000000000000000000000" "$archive" \
  > "$fixdir/assets/checksums.txt"
run_case checksum-mismatch 1 "Checksum verification failed" 0.2.0

# --- Case 10: archive absent from checksums.txt is fatal.
fixdir="$(new_fixdir missing-from-checksums)"
make_release_assets "$fixdir" 0.2.0
printf '%s  %s\n' "$(sha256_of "$fixdir/assets/$archive")" "some-other-file.tar.gz" \
  > "$fixdir/assets/checksums.txt"
run_case missing-from-checksums 1 "not found in checksums.txt" 0.2.0

echo "install.sh tests passed (${case_no} cases)"
