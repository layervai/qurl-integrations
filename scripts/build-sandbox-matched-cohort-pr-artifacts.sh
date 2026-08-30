#!/usr/bin/env bash
set -euo pipefail
umask 077

usage() {
  echo "usage: $0 OUTPUT_DIR REPOSITORY HEAD_SHA RUN_ID RUN_ATTEMPT QURL_GO_SOURCE_SHA" >&2
  exit 64
}

[ "$#" -eq 6 ] || usage

output_dir=$1
repository=$2
head_sha=$3
run_id=$4
run_attempt=$5
qurl_go_source_sha=$6

case "$repository" in
  layervai/qurl-integrations) ;;
  *) echo "unsupported repository: $repository" >&2; exit 65 ;;
esac
[[ "$head_sha" =~ ^[0-9a-f]{40}$ ]] || { echo "head SHA must be 40 lowercase hex" >&2; exit 65; }
[[ "$run_id" =~ ^[1-9][0-9]*$ ]] || { echo "run ID must be positive decimal" >&2; exit 65; }
[[ "$run_attempt" =~ ^[1-9][0-9]*$ ]] || { echo "run attempt must be positive decimal" >&2; exit 65; }
[[ "$qurl_go_source_sha" =~ ^[0-9a-f]{40}$ ]] || {
  echo "qurl-go source SHA must be 40 lowercase hex" >&2
  exit 65
}
((run_id <= 9007199254740991 && run_attempt <= 9007199254740991)) || {
  echo "run identity exceeds the exact JSON integer range" >&2
  exit 65
}
case "$output_dir" in
  /*) ;;
  *) echo "output directory must be absolute" >&2; exit 65 ;;
esac
[ "$output_dir" != "/" ] || { echo "output directory must not be the filesystem root" >&2; exit 65; }

for command in awk git go jq shasum; do
  command -v "$command" >/dev/null 2>&1 || { echo "required command is unavailable: $command" >&2; exit 69; }
done

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

actual_head=$(git rev-parse --verify HEAD)
[ "$actual_head" = "$head_sha" ] || {
  echo "checked-out HEAD does not match the caller head SHA" >&2
  exit 65
}

qurl_go_module_version=$(GOWORK=off GOFLAGS=-mod=readonly \
  go list -m -f '{{.Version}}' github.com/layervai/qurl-go)
if ! [[ "$qurl_go_module_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+-0\.[0-9]{14}-([0-9a-f]{12})$ ]] ||
  [ "${BASH_REMATCH[1]}" != "${qurl_go_source_sha:0:12}" ]; then
  echo "qurl-go module version does not bind the required source SHA" >&2
  exit 65
fi

# The caller uploads this tree as release authority. Refuse an existing path,
# including a symlink, so stale files or a hostile parent step cannot survive a
# retry and become part of the uploaded artifact.
if ! mkdir -m 700 "$output_dir"; then
  echo "output directory must be a new path" >&2
  exit 73
fi
mkdir -m 700 \
  "$output_dir/lifecycle-artifact" \
  "$output_dir/lifecycle-artifact/bin" \
  "$output_dir/source-receipt-artifact"

build_binary() {
  local package=$1
  local destination=$2
  local target_os=${3:-linux}
  CGO_ENABLED=0 GOOS="$target_os" GOARCH=amd64 GOWORK=off GOFLAGS=-mod=readonly \
    go build -trimpath -buildvcs=true -ldflags='-buildid=' -o "$destination" "$package"
  chmod 500 "$destination"
}

build_binary ./apps/cli/cmd/sandbox-matched-cohort-lifecycle \
  "$output_dir/lifecycle-artifact/bin/sandbox-matched-cohort-lifecycle"
build_binary ./apps/cli/cmd/sandbox-matched-cohort-authority \
  "$output_dir/lifecycle-artifact/bin/sandbox-matched-cohort-authority"
build_binary ./apps/cli/cmd \
  "$output_dir/lifecycle-artifact/bin/qurl"
build_binary ./apps/cli/cmd \
  "$output_dir/lifecycle-artifact/bin/qurl-windows-amd64.exe" windows

for binary in \
  "$output_dir/lifecycle-artifact/bin/sandbox-matched-cohort-lifecycle" \
  "$output_dir/lifecycle-artifact/bin/sandbox-matched-cohort-authority" \
  "$output_dir/lifecycle-artifact/bin/qurl" \
  "$output_dir/lifecycle-artifact/bin/qurl-windows-amd64.exe"; do
  if [[ ! -f "$binary" || -L "$binary" ]]; then
    echo "build output is not a regular file: $binary" >&2
    exit 73
  fi
done

verify_binary_metadata() {
  local binary=$1
  local metadata
  metadata=$(go version -m "$binary")
  awk -F '\t' -v head="$head_sha" -v module_version="$qurl_go_module_version" '
    $2 == "build" && $3 == "vcs.revision=" head { revisions++ }
    $2 == "dep" && $3 == "github.com/layervai/qurl-go" && $4 == module_version { modules++ }
    END { exit !(revisions == 1 && modules == 1) }
  ' <<<"$metadata" || {
    echo "built binary metadata does not match caller or qurl-go source authority: $binary" >&2
    exit 65
  }
}

verify_binary_metadata "$output_dir/lifecycle-artifact/bin/sandbox-matched-cohort-lifecycle"
verify_binary_metadata "$output_dir/lifecycle-artifact/bin/sandbox-matched-cohort-authority"
verify_binary_metadata "$output_dir/lifecycle-artifact/bin/qurl"
verify_binary_metadata "$output_dir/lifecycle-artifact/bin/qurl-windows-amd64.exe"

lifecycle_sha=$(shasum -a 256 "$output_dir/lifecycle-artifact/bin/sandbox-matched-cohort-lifecycle" | awk '{print $1}')
authority_sha=$(shasum -a 256 "$output_dir/lifecycle-artifact/bin/sandbox-matched-cohort-authority" | awk '{print $1}')
qurl_sha=$(shasum -a 256 "$output_dir/lifecycle-artifact/bin/qurl" | awk '{print $1}')
qurl_windows_amd64_sha=$(shasum -a 256 "$output_dir/lifecycle-artifact/bin/qurl-windows-amd64.exe" | awk '{print $1}')

jq -cnS \
  --arg repository "$repository" \
  --arg head_sha "$head_sha" \
  --argjson run_id "$run_id" \
  --argjson run_attempt "$run_attempt" \
  --arg qurl_go_source_sha "$qurl_go_source_sha" \
  --arg qurl_go_module_version "$qurl_go_module_version" \
  --arg lifecycle_sha "$lifecycle_sha" \
  --arg authority_sha "$authority_sha" \
  --arg qurl_sha "$qurl_sha" \
  --arg qurl_windows_amd64_sha "$qurl_windows_amd64_sha" \
  '{
    schema_version: 2,
    repository: $repository,
    head_sha: $head_sha,
    run_id: $run_id,
    run_attempt: $run_attempt,
    qurl_go_source_sha: $qurl_go_source_sha,
    qurl_go_module_version: $qurl_go_module_version,
    binaries: {
      lifecycle: {
        path: "bin/sandbox-matched-cohort-lifecycle",
        sha256: $lifecycle_sha
      },
      authority: {
        path: "bin/sandbox-matched-cohort-authority",
        sha256: $authority_sha
      },
      qurl: {
        path: "bin/qurl",
        sha256: $qurl_sha
      },
      qurl_windows_amd64: {
        path: "bin/qurl-windows-amd64.exe",
        sha256: $qurl_windows_amd64_sha
      }
    }
  }' >"$output_dir/source-receipt-artifact/sandbox-matched-cohort-source-receipt.json"

chmod 400 "$output_dir/source-receipt-artifact/sandbox-matched-cohort-source-receipt.json"

source_receipt_sha=$(shasum -a 256 \
  "$output_dir/source-receipt-artifact/sandbox-matched-cohort-source-receipt.json" | awk '{print $1}')

printf '%s\n' "$qurl_go_source_sha" >"$output_dir/qurl-go-source-sha.txt"
printf '%s\n' "$source_receipt_sha" >"$output_dir/source-receipt-sha256.txt"
printf '%s\n' "$lifecycle_sha" >"$output_dir/lifecycle-sha256.txt"
printf '%s\n' "$authority_sha" >"$output_dir/authority-sha256.txt"
printf '%s\n' "$qurl_sha" >"$output_dir/qurl-sha256.txt"
printf '%s\n' "$qurl_windows_amd64_sha" >"$output_dir/qurl-windows-amd64-sha256.txt"
chmod 400 \
  "$output_dir/qurl-go-source-sha.txt" \
  "$output_dir/source-receipt-sha256.txt" \
  "$output_dir/lifecycle-sha256.txt" \
  "$output_dir/authority-sha256.txt" \
  "$output_dir/qurl-sha256.txt" \
  "$output_dir/qurl-windows-amd64-sha256.txt"
