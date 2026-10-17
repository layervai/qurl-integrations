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
# install.sh: highest x.y.z wins regardless of release order, bare v<digit>
# tags only, prereleases excluded by flag and by hyphen, VERSION= override
# skips the API entirely. Fixtures default to the API's real pretty-printed
# shape (one field per line); case 4 covers compact JSON.
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
#   curl ... <api-url>?per_page=100&page=N              (API, stdout)
#   curl ... <download-url> -o <path>                   (downloads)
# API pages are served from $FIXDIR/releases-pageN.json; page 1 falls back
# to $FIXDIR/releases.json (absent = curl failure) so single-page cases
# stay simple, and pages beyond the fixtures are empty lists.
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
    page="${url##*page=}"
    if [[ -f "$FIXDIR/releases-page${page}.json" ]]; then
      cat "$FIXDIR/releases-page${page}.json"
    elif [[ "$page" == "1" ]]; then
      [[ -f "$FIXDIR/releases.json" ]] || exit 22
      cat "$FIXDIR/releases.json"
    else
      printf '[]'
    fi
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
  unset RUN_INSTALL_DIR RUN_HOME RUN_SYSTEM_DIR RUN_UMASK
  fixdir="$tmp_parent/$((case_no + 1))-$1"
  mkdir -p "$fixdir/assets" "$fixdir/bin"
  make_curl_stub "$fixdir"
}

# run_case <expected_status> <expected_output_substring> [version] [path]
# Runs the installer against the $fixdir prepared by new_fixdir (whose
# basename names the case in failure output). The optional 4th arg replaces
# PATH entirely (for cases that must hide host tools); the default prepends
# FIXDIR so its stubs win.
run_case() {
  local name="${fixdir##*/}" expected_status="$1" expected_output="$2" version="${3:-}"
  case_no=$((case_no + 1))
  local run_path="${4:-$fixdir:$PATH}"

  # RUN_INSTALL_DIR overrides INSTALL_DIR for one case ("-" leaves it unset
  # so the installer picks its default); RUN_HOME and RUN_SYSTEM_DIR pin the
  # fallback inputs so no case can touch the host's HOME or /usr/local/bin.
  local install_dir="${RUN_INSTALL_DIR:-$fixdir/bin}"
  local -a env_args=(
    FIXDIR="$fixdir" PATH="$run_path" VERSION="$version"
    _QURL_INSTALL_SYSTEM_DIR="${RUN_SYSTEM_DIR:-$fixdir/no-system-dir}"
  )
  [[ "$install_dir" == "-" ]] || env_args+=(INSTALL_DIR="$install_dir")
  # RUN_HOME="-" leaves HOME unset.
  [[ "${RUN_HOME:-}" == "-" ]] || env_args+=(HOME="${RUN_HOME:-$fixdir/home}")

  set +e
  local output
  output="$(umask "${RUN_UMASK:-022}" && cd "$fixdir" \
    && env -u INSTALL_DIR -u HOME -u XDG_STATE_HOME "${env_args[@]}" sh "$installer" 2>&1)"
  local status="$?"
  set -e
  LAST_OUTPUT="$output"

  # "nonzero": sh implementations differ on the exit code of ${VAR:?}.
  if [[ "$expected_status" == "nonzero" && "$status" != "0" ]]; then
    :
  elif [[ "$status" != "$expected_status" ]]; then
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
  local bin="${2:-$fixdir/bin}/qurl"
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
run_case 0 "Installed qurl v0.2.0"
assert_installed 0.2.0

# --- Case 1b: a backport created after a newer version must not win — the
# highest version is selected numerically per x.y.z field, not by API
# (creation) order, including across multi-digit components.
new_fixdir backport-does-not-downgrade
json_list "$(release_json v0.2.1 false)" \
          "$(release_json v0.10.0 false)" \
          "$(release_json v0.9.9 false)" > "$fixdir/releases.json"
make_release_assets "$fixdir" 0.10.0
run_case 0 "Installed qurl v0.10.0"
assert_installed 0.10.0

# --- Case 2: prerelease-flagged releases are skipped.
new_fixdir skips-prerelease-flag
json_list "$(release_json v0.3.0 true)" \
          "$(release_json v0.2.0 false)" > "$fixdir/releases.json"
make_release_assets "$fixdir" 0.2.0
run_case 0 "Installed qurl v0.2.0"
assert_installed 0.2.0

# --- Case 3: hyphenated (semver prerelease) tags are skipped even when the
# prerelease flag is (wrongly) false.
new_fixdir skips-hyphenated-tag
json_list "$(release_json v0.3.0-rc.1 false)" \
          "$(release_json v0.2.0 false)" > "$fixdir/releases.json"
make_release_assets "$fixdir" 0.2.0
run_case 0 "Installed qurl v0.2.0"
assert_installed 0.2.0

# --- Case 4: compact single-line JSON parses identically to the default
# pretty-printed fixtures.
new_fixdir compact-json
json_list "$(release_json slack-v1.0.0 false)" \
          "$(release_json v0.2.0 false)" | tr -d ' \n' > "$fixdir/releases.json"
make_release_assets "$fixdir" 0.2.0
run_case 0 "Installed qurl v0.2.0"
assert_installed 0.2.0

# --- Case 5: only prefixed tags -> explicit no-CLI-release error.
new_fixdir no-cli-release
json_list "$(release_json slack-v0.9.9 false)" \
          "$(release_json discord-v0.3.0 false)" > "$fixdir/releases.json"
run_case 1 "Could not find a CLI release"

# --- Case 6: releases API failure -> network error, not "no release".
# new_fixdir writes no releases.json, so the stub fails the API call.
new_fixdir api-failure
run_case 1 "Failed to query GitHub releases"

# --- Case 7: VERSION override installs without touching the releases API.
new_fixdir version-override
make_release_assets "$fixdir" 0.2.0
run_case 0 "Installed qurl v0.2.0" 0.2.0
assert_installed 0.2.0
assert_no_api_call

# --- Case 8: VERSION override tolerates a leading v.
new_fixdir version-override-v
make_release_assets "$fixdir" 0.2.0
run_case 0 "Installed qurl v0.2.0" v0.2.0
assert_installed 0.2.0
assert_no_api_call

# --- Case 9: checksum mismatch is fatal.
new_fixdir checksum-mismatch
make_release_assets "$fixdir" 0.2.0
printf '%s  %s\n' "0000000000000000000000000000000000000000000000000000000000000000" \
  "qurl_0.2.0_${os}_${arch}.tar.gz" > "$fixdir/assets/checksums.txt"
run_case 1 "Checksum verification failed" 0.2.0

# --- Case 10: archive absent from checksums.txt is fatal.
new_fixdir missing-from-checksums
make_release_assets "$fixdir" 0.2.0
archive="qurl_0.2.0_${os}_${arch}.tar.gz"
printf '%s  %s\n' "$(sha256_of "$fixdir/assets/$archive")" "some-other-file.tar.gz" \
  > "$fixdir/assets/checksums.txt"
run_case 1 "not found in checksums.txt" 0.2.0

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
run_case 1 "Unsupported architecture: riscv64"

# --- Case 12: no sha256 tool on PATH -> refuse to install. A toolbox PATH
# holds only the tools the installer needs, minus sha256sum/shasum.
# sh/bash: the replaced PATH is used to locate the installer's interpreter
# and the stub's env-resolved bash; gzip: GNU tar execs it for -z; cat/cp:
# used by the curl stub; rm: the installer's EXIT trap.
new_fixdir no-sha-tool
make_release_assets "$fixdir" 0.2.0
toolbox="$fixdir/toolbox"
mkdir -p "$toolbox"
for tool in sh bash gzip uname tr grep sed mktemp tar awk chmod mv cat cp rm; do
  ln -s "$(command -v "$tool")" "$toolbox/$tool"
done
ln -s "$fixdir/curl" "$toolbox/curl"
run_case 1 "refusing to install unverified binaries" 0.2.0 "$toolbox"

# --- Case 13: a release *name* containing '}' splits that release's chunk;
# the release is skipped (documented parse limitation), not misparsed.
new_fixdir brace-name-skipped
json_list "$(release_json v0.3.0 false 'v0.3.0 } hotfix')" \
          "$(release_json v0.2.0 false)" > "$fixdir/releases.json"
make_release_assets "$fixdir" 0.2.0
run_case 0 "Installed qurl v0.2.0"
assert_installed 0.2.0

# --- Case 14: a full page of other components' releases must not hide the
# CLI tag on page 2 — pagination continues past a 100-entry page.
new_fixdir cli-tag-beyond-page-1
page1_items=()
for i in $(seq 1 100); do
  page1_items+=("$(release_json "slack-v1.${i}.0" false)")
done
json_list "${page1_items[@]}" > "$fixdir/releases-page1.json"
json_list "$(release_json v0.2.0 false)" > "$fixdir/releases-page2.json"
make_release_assets "$fixdir" 0.2.0
run_case 0 "Installed qurl v0.2.0"
assert_installed 0.2.0
grep -q 'page=2' "$fixdir/curl.log" \
  || { echo "$fixdir: expected a page-2 request" >&2; exit 1; }

# --- Install location. A sudo stub on PATH records any call; the installer
# must never escalate (an agent or CI job cannot answer a password prompt).
add_sudo_stub() {
  cat > "$fixdir/sudo" <<'STUB'
#!/usr/bin/env bash
echo "sudo $*" >> "$FIXDIR/sudo.log"
exit 1
STUB
  chmod +x "$fixdir/sudo"
}
assert_no_sudo() {
  if [[ -e "$fixdir/sudo.log" ]]; then
    echo "$fixdir: installer invoked sudo: $(cat "$fixdir/sudo.log")" >&2
    exit 1
  fi
}

# --- Case 15: no INSTALL_DIR and a writable system directory -> install there.
new_fixdir default-system-dir
add_sudo_stub
make_release_assets "$fixdir" 0.2.0
mkdir -p "$fixdir/system-bin"
RUN_INSTALL_DIR=- RUN_SYSTEM_DIR="$fixdir/system-bin" run_case 0 "Installed qurl v0.2.0" 0.2.0
assert_installed 0.2.0 "$fixdir/system-bin"
assert_no_sudo

# --- Case 16: no INSTALL_DIR and no system directory (a fresh Mac without
# Homebrew) -> ~/.local/bin, created on demand, with a PATH hint and no sudo.
# umask 002 (Ubuntu/Fedora default): the directories created must still come
# out 0755, because the CLI refuses to keep state under a writable ~/.local.
new_fixdir fallback-user-dir
add_sudo_stub
make_release_assets "$fixdir" 0.2.0
printf '#!/bin/sh\necho 1000\n' > "$fixdir/id"  # non-root, even if the suite runs as root
chmod +x "$fixdir/id"
RUN_UMASK=002 RUN_INSTALL_DIR=- run_case 0 "Installed qurl v0.2.0 to $fixdir/home/.local/bin/qurl" 0.2.0
for dir in "$fixdir/home/.local" "$fixdir/home/.local/bin"; do
  mode="$(stat -c %a "$dir" 2>/dev/null || stat -f %Lp "$dir")"
  mode="${mode: -3}"  # ignore setuid/setgid/sticky digits from the fixture tree
  [[ "$mode" == "755" ]] || { echo "$fixdir: $dir has mode $mode, want 755" >&2; exit 1; }
done
[[ "$LAST_OUTPUT" != *"writable by other users"* ]] \
  || { echo "$fixdir: unexpected unsafe-mode warning" >&2; exit 1; }
assert_installed 0.2.0 "$fixdir/home/.local/bin"
assert_no_sudo
[[ "$LAST_OUTPUT" == *"is not on your PATH"* ]] \
  || { echo "$fixdir: expected a PATH hint" >&2; exit 1; }
[[ "$LAST_OUTPUT" != *"qurl login"* ]] \
  || { echo "$fixdir: getting-started text must not ask for a login" >&2; exit 1; }

# --- Case 17: an explicit INSTALL_DIR that does not exist yet is created.
new_fixdir creates-install-dir
add_sudo_stub
make_release_assets "$fixdir" 0.2.0
RUN_INSTALL_DIR="$fixdir/new/nested/bin" run_case 0 "Installed qurl v0.2.0" 0.2.0
assert_installed 0.2.0 "$fixdir/new/nested/bin"
assert_no_sudo

# --- Case 18: an INSTALL_DIR already on PATH gets no PATH hint.
new_fixdir install-dir-on-path
make_release_assets "$fixdir" 0.2.0
run_case 0 "Installed qurl v0.2.0" 0.2.0 "$fixdir/bin:$fixdir:$PATH"
assert_installed 0.2.0
[[ "$LAST_OUTPUT" != *"is not on your PATH"* ]] \
  || { echo "$fixdir: unexpected PATH hint" >&2; exit 1; }

# --- Case 19: an unwritable explicit INSTALL_DIR fails with the fix named,
# instead of escalating. Root can write anywhere, so the case needs non-root.
if [[ "$(id -u)" != "0" ]]; then
  new_fixdir unwritable-install-dir
  add_sudo_stub
  make_release_assets "$fixdir" 0.2.0
  mkdir -p "$fixdir/locked"
  chmod 555 "$fixdir/locked"
  RUN_INSTALL_DIR="$fixdir/locked" run_case 1 "could not create or write" 0.2.0
  chmod 755 "$fixdir/locked"
  assert_no_sudo
  [[ ! -e "$fixdir/locked/qurl" ]] || { echo "$fixdir: installed into a locked dir" >&2; exit 1; }
fi

# --- Case 20: no INSTALL_DIR and a system dir that exists but is not writable
# (stock macOS: root-owned /usr/local/bin) -> ~/.local/bin, no sudo.
if [[ "$(id -u)" != "0" ]]; then
  new_fixdir unwritable-system-dir
  add_sudo_stub
  make_release_assets "$fixdir" 0.2.0
  mkdir -p "$fixdir/system-bin"
  chmod 555 "$fixdir/system-bin"
  RUN_INSTALL_DIR=- RUN_SYSTEM_DIR="$fixdir/system-bin" \
    run_case 0 "Installed qurl v0.2.0 to $fixdir/home/.local/bin/qurl" 0.2.0
  chmod 755 "$fixdir/system-bin"
  assert_installed 0.2.0 "$fixdir/home/.local/bin"
  assert_no_sudo
fi

# --- Case 21: run as root (curl ... | sudo sh) with no system dir yet -> the
# system dir is created and used. An `id` stub reports uid 0.
new_fixdir root-creates-system-dir
add_sudo_stub
make_release_assets "$fixdir" 0.2.0
printf '#!/bin/sh\necho 0\n' > "$fixdir/id"
chmod +x "$fixdir/id"
RUN_INSTALL_DIR=- RUN_SYSTEM_DIR="$fixdir/new-system-bin" \
  run_case 0 "Installed qurl v0.2.0 to $fixdir/new-system-bin/qurl" 0.2.0
assert_installed 0.2.0 "$fixdir/new-system-bin"
assert_no_sudo

# --- Case 22: a pre-existing group-writable ~/.local (pip/pipx/npm under
# umask 002) is flagged with the fix; the installer never chmods it.
new_fixdir existing-writable-local
make_release_assets "$fixdir" 0.2.0
mkdir -p "$fixdir/home/.local"
chmod 775 "$fixdir/home/.local"
RUN_INSTALL_DIR=- run_case 0 "chmod go-w" 0.2.0
[[ "$LAST_OUTPUT" == *"$fixdir/home/.local is writable by other users"* ]] \
  || { echo "$fixdir: expected the unsafe ~/.local warning" >&2; exit 1; }
mode="$(stat -c %a "$fixdir/home/.local" 2>/dev/null || stat -f %Lp "$fixdir/home/.local")"
[[ "${mode: -3}" == "775" ]] || { echo "$fixdir: installer changed ~/.local mode to $mode" >&2; exit 1; }

# --- Case 22b: a HOME containing spaces is checked as one path (no word
# splitting), and a group-writable ~/.local/state is flagged.
new_fixdir home-with-spaces
make_release_assets "$fixdir" 0.2.0
mkdir -p "$fixdir/my home/.local/state"
chmod 775 "$fixdir/my home/.local/state"
RUN_INSTALL_DIR=- RUN_HOME="$fixdir/my home" run_case 0 "Installed qurl v0.2.0" 0.2.0
[[ "$LAST_OUTPUT" == *"$fixdir/my home/.local/state is writable by other users"* ]] \
  || { echo "$fixdir: expected the ~/.local/state warning" >&2; exit 1; }
chmod 755 "$fixdir/my home/.local/state"

# --- Case 23: an older qurl earlier on PATH is reported as shadowing.
new_fixdir shadowed-by-older-install
make_release_assets "$fixdir" 0.2.0
mkdir -p "$fixdir/old-bin"
printf '#!/bin/sh\necho old\n' > "$fixdir/old-bin/qurl"
chmod +x "$fixdir/old-bin/qurl"
run_case 0 "comes first on your PATH" 0.2.0 "$fixdir/old-bin:$fixdir:$PATH"

# --- Case 24: HOME unset and no INSTALL_DIR -> clear error, nothing installed.
new_fixdir home-unset
make_release_assets "$fixdir" 0.2.0
printf '#!/bin/sh\necho 1000\n' > "$fixdir/id"
chmod +x "$fixdir/id"
RUN_INSTALL_DIR=- RUN_HOME=- run_case nonzero "HOME is not set; set INSTALL_DIR" 0.2.0

echo "install.sh tests passed (${case_no} cases)"
