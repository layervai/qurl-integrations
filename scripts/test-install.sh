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
# flag and by hyphen, VERSION= override skips the API entirely. Fixtures
# default to the API's real pretty-printed shape (one field per line);
# case 4 covers compact JSON.
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
installer="$repo_root/scripts/install.sh"
tmp_parent="$(mktemp -d)"
trap 'rm -rf "$tmp_parent"' EXIT

case_no=0
fixdir=""

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

# Stub curl. install.sh invokes exactly two shapes, both carrying the
# value-flags --retry N and --connect-timeout N:
#   curl -fsSL --retry 3 --connect-timeout 15 <url>            (API, stdout)
#   curl -fsSL --retry 3 --connect-timeout 15 <url> -o <path>  (downloads)
make_curl_stub() {
  local stub_dir="$1"
  cat > "$stub_dir/curl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
url="" out=""
while (( $# )); do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    --retry|--connect-timeout) shift 2 ;;
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
  local dir="$1" version="$2"
  local build="$dir/build"
  mkdir -p "$build" "$dir/assets"
  printf '#!/bin/sh\necho "qurl-fixture %s"\n' "$version" > "$build/qurl"
  local archive="qurl_${version}_${os}_${arch}.tar.gz"
  tar -czf "$dir/assets/$archive" -C "$build" qurl
  printf '%s  %s\n' "$(sha256_of "$dir/assets/$archive")" "$archive" \
    > "$dir/assets/checksums.txt"
}

# new_fixdir <name> — prepares the next case's fixture dir and assigns the
# global $fixdir consumed by run_case (assigning directly instead of echoing
# keeps the path derived exactly once).
new_fixdir() {
  fixdir="$tmp_parent/$((case_no + 1))-$1"
  mkdir -p "$fixdir/assets" "$fixdir/bin"
  make_curl_stub "$fixdir"
}

# run_case <name> <expected_status> <expected_output_substring> [version] [path]
# Runs the installer against the $fixdir prepared by new_fixdir. The optional
# 5th arg replaces PATH entirely (for cases that must hide host tools); the
# default prepends FIXDIR so its stubs win.
run_case() {
  local name="$1" expected_status="$2" expected_output="$3" version="${4:-}"
  case_no=$((case_no + 1))
  local run_path="${5:-$fixdir:$PATH}"

  set +e
  local output
  output="$(cd "$fixdir" \
    && FIXDIR="$fixdir" PATH="$run_path" INSTALL_DIR="$fixdir/bin" \
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
  local version="$1"
  local bin="$fixdir/bin/qurl"
  [[ -x "$bin" ]] || { echo "$fixdir: expected executable $bin" >&2; exit 1; }
  [[ "$("$bin")" == "qurl-fixture $version" ]] \
    || { echo "$fixdir: installed binary is not the $version fixture" >&2; exit 1; }
  grep -q "/releases/download/v$version/qurl_${version}_${os}_${arch}.tar.gz" "$fixdir/curl.log" \
    || { echo "$fixdir: download URL did not use tag v$version" >&2; exit 1; }
}

assert_no_api_call() {
  if grep -q "releases?per_page" "$fixdir/curl.log" 2>/dev/null; then
    echo "$fixdir: VERSION override must not query the releases API" >&2
    exit 1
  fi
}

# Fixture releases default to the API's real pretty-printed shape — one field
# per line — and mirror its field order (author object before tag_name;
# draft/prerelease after it; assets with an uploader object), which the
# installer's collapse-then-chunk parse depends on.
release_json() {
  local tag="$1" prerelease="$2" name="${3:-$1}"
  cat <<EOF
  {
    "url": "https://api.github.com/x",
    "author": {
      "login": "github-actions[bot]",
      "id": 41898282
    },
    "node_id": "RE_x",
    "tag_name": "$tag",
    "target_commitish": "main",
    "name": "$name",
    "draft": false,
    "prerelease": $prerelease,
    "created_at": "2026-07-01T00:00:00Z",
    "assets": [
      {
        "name": "a.txt",
        "uploader": {
          "login": "bot",
          "id": 1
        }
      }
    ],
    "body": "notes"
  }
EOF
}

json_list() {
  local sep="" item
  printf '[\n'
  for item in "$@"; do
    printf '%s%s' "$sep" "$item"
    sep=$',\n'
  done
  printf '\n]\n'
}

# --- Case 1: highest bare tag wins; prefixed component tags never match.
new_fixdir picks-newest-bare-tag
json_list "$(release_json slack-v0.9.9 false)" \
          "$(release_json chrome-extension-v1.0.2 false)" \
          "$(release_json v0.2.0 false)" \
          "$(release_json v0.1.0 false)" > "$fixdir/releases.json"
make_release_assets "$fixdir" 0.2.0
run_case picks-newest-bare-tag 0 "Installed qurl v0.2.0"
assert_installed 0.2.0

# --- Case 1b: a backport created after a newer version must not win — the
# highest version is selected numerically per x.y.z field, not by API
# (creation) order, including across multi-digit components.
new_fixdir backport-does-not-downgrade
json_list "$(release_json v0.2.1 false)" \
          "$(release_json v0.10.0 false)" \
          "$(release_json v0.9.9 false)" > "$fixdir/releases.json"
make_release_assets "$fixdir" 0.10.0
run_case backport-does-not-downgrade 0 "Installed qurl v0.10.0"
assert_installed 0.10.0

# --- Case 2: prerelease-flagged releases are skipped.
new_fixdir skips-prerelease-flag
json_list "$(release_json v0.3.0 true)" \
          "$(release_json v0.2.0 false)" > "$fixdir/releases.json"
make_release_assets "$fixdir" 0.2.0
run_case skips-prerelease-flag 0 "Installed qurl v0.2.0"
assert_installed 0.2.0

# --- Case 3: hyphenated (semver prerelease) tags are skipped even when the
# prerelease flag is (wrongly) false.
new_fixdir skips-hyphenated-tag
json_list "$(release_json v0.3.0-rc.1 false)" \
          "$(release_json v0.2.0 false)" > "$fixdir/releases.json"
make_release_assets "$fixdir" 0.2.0
run_case skips-hyphenated-tag 0 "Installed qurl v0.2.0"
assert_installed 0.2.0

# --- Case 4: compact single-line JSON parses identically to the default
# pretty-printed fixtures.
new_fixdir compact-json
json_list "$(release_json slack-v1.0.0 false)" \
          "$(release_json v0.2.0 false)" | tr -d ' \n' > "$fixdir/releases.json"
make_release_assets "$fixdir" 0.2.0
run_case compact-json 0 "Installed qurl v0.2.0"
assert_installed 0.2.0

# --- Case 5: only prefixed tags -> explicit no-CLI-release error.
new_fixdir no-cli-release
json_list "$(release_json slack-v0.9.9 false)" \
          "$(release_json discord-v0.3.0 false)" > "$fixdir/releases.json"
run_case no-cli-release 1 "Could not find a CLI release"

# --- Case 6: releases API failure -> network error, not "no release".
# new_fixdir writes no releases.json, so the stub fails the API call.
new_fixdir api-failure
run_case api-failure 1 "Failed to query GitHub releases"

# --- Case 7: VERSION override installs without touching the releases API.
new_fixdir version-override
make_release_assets "$fixdir" 0.2.0
run_case version-override 0 "Installed qurl v0.2.0" 0.2.0
assert_installed 0.2.0
assert_no_api_call

# --- Case 8: VERSION override tolerates a leading v.
new_fixdir version-override-v
make_release_assets "$fixdir" 0.2.0
run_case version-override-v 0 "Installed qurl v0.2.0" v0.2.0
assert_installed 0.2.0
assert_no_api_call

# --- Case 9: checksum mismatch is fatal.
new_fixdir checksum-mismatch
make_release_assets "$fixdir" 0.2.0
printf '%s  %s\n' "0000000000000000000000000000000000000000000000000000000000000000" \
  "qurl_0.2.0_${os}_${arch}.tar.gz" > "$fixdir/assets/checksums.txt"
run_case checksum-mismatch 1 "Checksum verification failed" 0.2.0

# --- Case 10: archive absent from checksums.txt is fatal.
new_fixdir missing-from-checksums
make_release_assets "$fixdir" 0.2.0
archive="qurl_0.2.0_${os}_${arch}.tar.gz"
printf '%s  %s\n' "$(sha256_of "$fixdir/assets/$archive")" "some-other-file.tar.gz" \
  > "$fixdir/assets/checksums.txt"
run_case missing-from-checksums 1 "not found in checksums.txt" 0.2.0

# --- Case 11: unsupported architecture is rejected before any download.
new_fixdir unsupported-arch
cat > "$fixdir/uname" <<'STUB'
#!/usr/bin/env bash
case "${1:-}" in
  -m) echo riscv64 ;;
  *) echo Linux ;;
esac
STUB
chmod +x "$fixdir/uname"
run_case unsupported-arch 1 "Unsupported architecture: riscv64"

# --- Case 12: no sha256 tool on PATH -> refuse to install. A toolbox PATH
# holds only the tools the installer needs, minus sha256sum/shasum.
# sh/bash: the replaced PATH is used to locate the installer's interpreter
# and the stub's env-resolved bash; gzip: GNU tar execs it for -z; cat/cp:
# used by the curl stub; rm: the installer's EXIT trap.
new_fixdir no-sha-tool
make_release_assets "$fixdir" 0.2.0
toolbox="$fixdir/toolbox"
mkdir -p "$toolbox"
for tool in sh bash gzip uname tr grep sed head mktemp tar awk chmod mv cat cp rm; do
  ln -s "$(command -v "$tool")" "$toolbox/$tool"
done
ln -s "$fixdir/curl" "$toolbox/curl"
run_case no-sha-tool 1 "refusing to install unverified binaries" 0.2.0 "$toolbox"

# --- Case 13: a release *name* containing '}' splits that release's chunk;
# the release is skipped (documented parse limitation), not misparsed.
new_fixdir brace-name-skipped
json_list "$(release_json v0.3.0 false 'v0.3.0 } hotfix')" \
          "$(release_json v0.2.0 false)" > "$fixdir/releases.json"
make_release_assets "$fixdir" 0.2.0
run_case brace-name-skipped 0 "Installed qurl v0.2.0"
assert_installed 0.2.0

echo "install.sh tests passed (${case_no} cases)"
