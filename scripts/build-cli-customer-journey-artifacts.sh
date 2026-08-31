#!/usr/bin/env bash
set -euo pipefail
umask 077

if [[ $# -ne 5 ]]; then
  echo "usage: $0 OUTPUT_DIR REPOSITORY HEAD_SHA RUN_ID RUN_ATTEMPT" >&2
  exit 64
fi

output_dir=$1
repository=$2
head_sha=$3
run_id=$4
run_attempt=$5

[[ "$repository" == "layervai/qurl-integrations" ]] || {
  echo "unsupported repository" >&2
  exit 65
}
[[ "$head_sha" =~ ^[0-9a-f]{40}$ ]] || {
  echo "head SHA must be 40 lowercase hex" >&2
  exit 65
}
[[ "$run_id" =~ ^[1-9][0-9]*$ ]] || {
  echo "run ID must be a positive integer" >&2
  exit 65
}
[[ "$run_attempt" =~ ^[1-9][0-9]*$ ]] || {
  echo "run attempt must be a positive integer" >&2
  exit 65
}
[[ "$output_dir" == /* && "$output_dir" != / ]] || {
  echo "output directory must be an absolute non-root path" >&2
  exit 65
}
[[ "$(git rev-parse --verify HEAD)" == "$head_sha" ]] || {
  echo "checked-out HEAD does not match the requested source" >&2
  exit 65
}
[[ -z "$(git status --porcelain --untracked-files=normal)" ]] || {
  echo "artifact producer requires a clean source tree" >&2
  exit 65
}

qurl_go_version=$(GOWORK=off GOFLAGS=-mod=readonly go list -m -f '{{.Version}}' github.com/layervai/qurl-go)
connector_version=$(GOWORK=off GOFLAGS=-mod=readonly go list -m -f '{{.Version}}' github.com/layervai/qurl-connector)
for selection in "$qurl_go_version" "$connector_version"; do
  [[ "$selection" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
    echo "customer binaries require tagged module dependencies" >&2
    exit 65
  }
done

mkdir -m 700 "$output_dir"
version="v0.0.0-ci.${head_sha:0:12}"

build_target() {
  local target_os=$1
  local target_arch=$2
  local suffix=$3
  local executable_suffix=$4
  local qurl_path="$output_dir/qurl-${suffix}${executable_suffix}"

  CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" GOWORK=off GOFLAGS=-mod=readonly \
    go build -trimpath -buildvcs=true -ldflags="-buildid= -X main.version=${version}" \
      -o "$qurl_path" ./apps/cli/cmd
  chmod 0500 "$qurl_path"
}

build_target linux amd64 linux-amd64 ""
build_target darwin amd64 darwin-amd64 ""
build_target darwin arm64 darwin-arm64 ""
build_target windows amd64 windows-amd64 .exe

for binary in "$output_dir"/qurl-*; do
  [[ -f "$binary" && ! -L "$binary" ]] || {
    echo "build output is not one regular file" >&2
    exit 73
  }
done

manifest="$output_dir/manifest.json"
python3 - "$output_dir" "$repository" "$head_sha" "$run_id" "$run_attempt" "$version" "$qurl_go_version" "$connector_version" <<'PY'
import hashlib
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
files = {}
for path in sorted(root.rglob("*")):
    if path.is_file() and path.name != "manifest.json":
        files[path.relative_to(root).as_posix()] = hashlib.sha256(path.read_bytes()).hexdigest()
document = {
    "schema": "layerv.qurl-cli-customer-journey.v1",
    "repository": sys.argv[2],
    "head_sha": sys.argv[3],
    "run_id": int(sys.argv[4]),
    "run_attempt": int(sys.argv[5]),
    "version": sys.argv[6],
    "modules": {
        "github.com/layervai/qurl-go": sys.argv[7],
        "github.com/layervai/qurl-connector": sys.argv[8],
    },
    "files": files,
}
(root / "manifest.json").write_text(
    json.dumps(document, separators=(",", ":"), sort_keys=True) + "\n",
    encoding="utf-8",
)
PY
chmod 0400 "$manifest"

python3 - "$output_dir" "$head_sha" <<'PY'
import json
import pathlib
import subprocess
import sys

root = pathlib.Path(sys.argv[1])
head = sys.argv[2]
manifest = json.loads((root / "manifest.json").read_text(encoding="utf-8"))
actual = {
    path.relative_to(root).as_posix()
    for path in root.rglob("*")
    if path.is_file() and path.name != "manifest.json"
}
if set(manifest["files"]) != actual:
    raise SystemExit("manifest file set is incomplete")
for name in manifest["files"]:
    metadata = subprocess.check_output(["go", "version", "-m", str(root / name)], text=True)
    if f"vcs.revision={head}" not in metadata:
        raise SystemExit("customer binary source metadata is not exact")
PY
