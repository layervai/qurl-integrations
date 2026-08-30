#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
script="$root/scripts/build-sandbox-matched-cohort-pr-artifacts.sh"
tmp=$(mktemp -d)
cleanup() {
  find "$tmp" -type f -delete 2>/dev/null || true
  find "$tmp" -type l -delete 2>/dev/null || true
  find "$tmp" -depth -type d -empty -delete 2>/dev/null || true
}
trap cleanup EXIT

cd "$root"
head_sha=$(git rev-parse --verify HEAD)
qurl_go_module_version=$(GOWORK=off GOFLAGS=-mod=readonly \
  go list -m -f '{{.Version}}' github.com/layervai/qurl-go)
if ! [[ "$qurl_go_module_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+-0\.[0-9]{14}-([0-9a-f]{12})$ ]]; then
  echo "test fixture could not resolve the qurl-go pseudo-version suffix" >&2
  exit 1
fi
qurl_go_source_sha="${BASH_REMATCH[1]}0000000000000000000000000000"

file_mode() {
  case "$(uname -s)" in
    Darwin) stat -f '%Lp' "$1" ;;
    Linux) stat -c '%a' "$1" ;;
    *) echo "unsupported test platform" >&2; return 1 ;;
  esac
}

output="$tmp/output"
"$script" "$output" layervai/qurl-integrations \
  "$head_sha" 12345 2 "$qurl_go_source_sha"

test -x "$output/lifecycle-artifact/bin/sandbox-matched-cohort-lifecycle"
test -x "$output/lifecycle-artifact/bin/sandbox-matched-cohort-authority"
test -x "$output/lifecycle-artifact/bin/qurl"
test -x "$output/lifecycle-artifact/bin/qurl-windows-amd64.exe"
test "$(file_mode "$output")" = 700
test "$(file_mode "$output/lifecycle-artifact")" = 700
test "$(file_mode "$output/lifecycle-artifact/bin")" = 700
test "$(file_mode "$output/source-receipt-artifact")" = 700
test "$(file_mode "$output/lifecycle-artifact/bin/sandbox-matched-cohort-lifecycle")" = 500
test "$(file_mode "$output/lifecycle-artifact/bin/sandbox-matched-cohort-authority")" = 500
test "$(file_mode "$output/lifecycle-artifact/bin/qurl")" = 500
test "$(file_mode "$output/lifecycle-artifact/bin/qurl-windows-amd64.exe")" = 500
test "$(file_mode "$output/source-receipt-artifact/sandbox-matched-cohort-source-receipt.json")" = 400

test "$(cd "$output/lifecycle-artifact" && find . -type f | LC_ALL=C sort)" = "./bin/qurl
./bin/qurl-windows-amd64.exe
./bin/sandbox-matched-cohort-authority
./bin/sandbox-matched-cohort-lifecycle"
test "$(cd "$output/source-receipt-artifact" && find . -type f | LC_ALL=C sort)" = "./sandbox-matched-cohort-source-receipt.json"
receipt="$output/source-receipt-artifact/sandbox-matched-cohort-source-receipt.json"
test "$(wc -l <"$receipt" | tr -d ' ')" = 1
if LC_ALL=C grep -q $'\r' "$receipt"; then
  echo "source receipt contains a carriage return" >&2
  exit 1
fi
cmp "$receipt" <(jq -cS . "$receipt")

jq -e '
  keys == ["binaries", "head_sha", "qurl_go_module_version", "qurl_go_source_sha", "repository", "run_attempt", "run_id", "schema_version"] and
  .schema_version == 2 and
  .repository == "layervai/qurl-integrations" and
  .head_sha == $head_sha and
  .run_id == 12345 and
  .run_attempt == 2 and
  .qurl_go_source_sha == $qurl_go_source_sha and
  .qurl_go_module_version == $qurl_go_module_version and
  .binaries.lifecycle.path == "bin/sandbox-matched-cohort-lifecycle" and
  .binaries.authority.path == "bin/sandbox-matched-cohort-authority" and
  .binaries.qurl.path == "bin/qurl" and
  .binaries.qurl_windows_amd64.path == "bin/qurl-windows-amd64.exe" and
  (.binaries.lifecycle.sha256 | test("^[0-9a-f]{64}$")) and
  (.binaries.authority.sha256 | test("^[0-9a-f]{64}$")) and
  (.binaries.qurl.sha256 | test("^[0-9a-f]{64}$")) and
  (.binaries.qurl_windows_amd64.sha256 | test("^[0-9a-f]{64}$"))
' --arg head_sha "$head_sha" \
  --arg qurl_go_source_sha "$qurl_go_source_sha" \
  --arg qurl_go_module_version "$qurl_go_module_version" \
  "$receipt" >/dev/null

test "$(shasum -a 256 "$output/lifecycle-artifact/bin/sandbox-matched-cohort-lifecycle" | awk '{print $1}')" = \
  "$(jq -r .binaries.lifecycle.sha256 "$receipt")"
test "$(shasum -a 256 "$output/lifecycle-artifact/bin/sandbox-matched-cohort-authority" | awk '{print $1}')" = \
  "$(jq -r .binaries.authority.sha256 "$receipt")"
test "$(shasum -a 256 "$output/lifecycle-artifact/bin/qurl" | awk '{print $1}')" = \
  "$(jq -r .binaries.qurl.sha256 "$receipt")"
test "$(shasum -a 256 "$output/lifecycle-artifact/bin/qurl-windows-amd64.exe" | awk '{print $1}')" = \
  "$(jq -r .binaries.qurl_windows_amd64.sha256 "$receipt")"
test "$(shasum -a 256 "$receipt" | awk '{print $1}')" = \
  "$(<"$output/source-receipt-sha256.txt")"

if "$script" "$tmp/bad-repo" attacker/qurl-integrations \
  "$head_sha" 12345 2 "$qurl_go_source_sha" >/dev/null 2>&1; then
  echo "wrong repository was accepted" >&2
  exit 1
fi
if "$script" "$tmp/bad-sha" layervai/qurl-integrations deadbeef 12345 2 \
  "$qurl_go_source_sha" >/dev/null 2>&1; then
  echo "short head SHA was accepted" >&2
  exit 1
fi
if "$script" "$tmp/bad-run" layervai/qurl-integrations \
  "$head_sha" 0 2 "$qurl_go_source_sha" >/dev/null 2>&1; then
  echo "zero run ID was accepted" >&2
  exit 1
fi
if "$script" "$tmp/oversized-run" layervai/qurl-integrations \
  "$head_sha" 9007199254740992 2 "$qurl_go_source_sha" >/dev/null 2>&1; then
  echo "an inexact JSON run ID was accepted" >&2
  exit 1
fi

wrong_head=0000000000000000000000000000000000000000
if [ "$head_sha" = "$wrong_head" ]; then
  wrong_head=1111111111111111111111111111111111111111
fi
if "$script" "$tmp/wrong-head" layervai/qurl-integrations \
  "$wrong_head" 12345 2 "$qurl_go_source_sha" >/dev/null 2>&1; then
  echo "a different full-length caller head was accepted" >&2
  exit 1
fi

mkdir "$tmp/existing"
printf 'sentinel\n' >"$tmp/existing/keep"
if "$script" "$tmp/existing" layervai/qurl-integrations \
  "$head_sha" 12345 2 "$qurl_go_source_sha" >/dev/null 2>&1; then
  echo "an existing output directory was accepted" >&2
  exit 1
fi
test "$(cat "$tmp/existing/keep")" = sentinel
test "$(find "$tmp/existing" -mindepth 1 -maxdepth 1 | wc -l | tr -d ' ')" = 1

mkdir "$tmp/symlink-target"
ln -s "$tmp/symlink-target" "$tmp/output-link"
if "$script" "$tmp/output-link" layervai/qurl-integrations \
  "$head_sha" 12345 2 "$qurl_go_source_sha" >/dev/null 2>&1; then
  echo "a symlink output directory was accepted" >&2
  exit 1
fi
test -z "$(find "$tmp/symlink-target" -mindepth 1 -print -quit)"

if "$script" relative-output layervai/qurl-integrations \
  "$head_sha" 12345 2 "$qurl_go_source_sha" >/dev/null 2>&1; then
  echo "a relative output directory was accepted" >&2
  exit 1
fi

wrong_qurl_go_source_sha=0000000000000000000000000000000000000000
if [ "$qurl_go_source_sha" = "$wrong_qurl_go_source_sha" ]; then
  wrong_qurl_go_source_sha=1111111111111111111111111111111111111111
fi
if "$script" "$tmp/wrong-qurl-go-source" layervai/qurl-integrations \
  "$head_sha" 12345 2 "$wrong_qurl_go_source_sha" >/dev/null 2>&1; then
  echo "a qurl-go source SHA that does not match the module suffix was accepted" >&2
  exit 1
fi
if "$script" "$tmp/short-qurl-go-source" layervai/qurl-integrations \
  "$head_sha" 12345 2 "${qurl_go_source_sha:0:12}" >/dev/null 2>&1; then
  echo "a short qurl-go source SHA was accepted" >&2
  exit 1
fi

echo "sandbox matched-cohort PR artifact tests passed"
