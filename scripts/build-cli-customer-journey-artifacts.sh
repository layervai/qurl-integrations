#!/usr/bin/env bash
set -euo pipefail
umask 077

if [[ $# -ne 1 ]]; then
  echo "usage: $0 OUTPUT_DIR" >&2
  exit 64
fi

output_dir=$1
hub_public_key_b64=${QURL_RELEASE_HUB_PUBLIC_KEY_B64:-}
hub_public_key_sha256=${QURL_RELEASE_HUB_PUBLIC_KEY_SHA256:-}
session_relay_url=${QURL_RELEASE_SESSION_RELAY_URL:-}

[[ "$output_dir" == /* && "$output_dir" != / ]] || {
  echo "output directory must be an absolute non-root path" >&2
  exit 65
}

head_sha=$(git rev-parse --verify HEAD)
[[ "$head_sha" =~ ^[0-9a-f]{40}$ ]] || {
  echo "source revision must be 40 lowercase hex" >&2
  exit 65
}

python3 - "$hub_public_key_b64" "$hub_public_key_sha256" <<'PY'
import base64
import binascii
import hashlib
import re
import sys

try:
    key = base64.b64decode(sys.argv[1], validate=True)
except (binascii.Error, ValueError) as exc:
    raise SystemExit(f"release Hub public key is not canonical base64: {exc}") from exc
if len(key) != 32:
    raise SystemExit("release Hub public key must decode to 32 bytes")
if not re.fullmatch(r"[0-9a-f]{64}", sys.argv[2]):
    raise SystemExit("release Hub public-key SHA-256 is malformed")
if hashlib.sha256(key).hexdigest() != sys.argv[2]:
    raise SystemExit("release Hub public key does not match its SHA-256")
PY

relay_test=TestReleaseSessionRelayEnvironment
relay_pattern="^${relay_test}$"
actual_relay_test=$(GOWORK=off GOFLAGS=-mod=readonly \
  go test ./apps/cli/internal/connector/sessionrelay -list "$relay_pattern" | sed -n '/^Test/p')
[[ "$actual_relay_test" == "$relay_test" ]] || {
  echo "exact release session-relay validation test is missing or ambiguous" >&2
  exit 65
}
QURL_REQUIRE_RELEASE_SESSION_RELAY=1 \
  QURL_RELEASE_SESSION_RELAY_URL="$session_relay_url" \
  GOWORK=off GOFLAGS=-mod=readonly \
  go test ./apps/cli/internal/connector/sessionrelay -run "$relay_pattern" -count=1

mkdir -m 700 "$output_dir"
version="v0.0.0-ci.${head_sha:0:12}"

build_target() {
  local target_os=$1
  local target_arch=$2
  local suffix=$3
  local executable_suffix=$4
  local qurl_path="$output_dir/qurl-${suffix}${executable_suffix}"

  CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" GOWORK=off GOFLAGS=-mod=readonly \
    go build -trimpath -buildvcs=true \
      -ldflags="-buildid= -s -w -X main.version=${version} -X github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub.defaultServerPublicKeyB64=${hub_public_key_b64} -X github.com/layervai/qurl-integrations/apps/cli/internal/connector/sessionrelay.defaultURL=${session_relay_url}" \
      -o "$qurl_path" ./apps/cli/cmd
  chmod 0500 "$qurl_path"
}

build_target linux amd64 linux-amd64 ""
build_target darwin amd64 darwin-amd64 ""
build_target darwin arm64 darwin-arm64 ""
build_target windows amd64 windows-amd64 .exe

python3 - "$output_dir" "$head_sha" "$hub_public_key_b64" "$session_relay_url" <<'PY'
import json
import pathlib
import subprocess
import sys

root = pathlib.Path(sys.argv[1])
head_sha = sys.argv[2]
hub_key = sys.argv[3].encode("ascii")
session_relay = sys.argv[4].encode("ascii")
if not session_relay:
    raise SystemExit("release session relay must not be empty")
expected = {
    "qurl-linux-amd64": ("linux", "amd64"),
    "qurl-darwin-amd64": ("darwin", "amd64"),
    "qurl-darwin-arm64": ("darwin", "arm64"),
    "qurl-windows-amd64.exe": ("windows", "amd64"),
}
observed = {path.name for path in root.iterdir() if path.is_file()}
if observed != set(expected):
    raise SystemExit("customer artifact set is not exact")

for name, (target_os, target_arch) in expected.items():
    path = root / name
    data = path.read_bytes()
    if hub_key not in data:
        raise SystemExit(f"{name} does not contain the selected release trust root")
    if session_relay not in data:
        raise SystemExit(f"{name} does not contain the selected release session relay")
    result = subprocess.run(
        ["go", "version", "-m", "-json", str(path)],
        check=True,
        capture_output=True,
        text=True,
    )
    info = json.loads(result.stdout)
    settings = {
        row.get("Key"): row.get("Value")
        for row in info.get("Settings", [])
        if row.get("Key") in {"CGO_ENABLED", "GOARCH", "GOOS", "vcs.modified", "vcs.revision"}
    }
    wanted = {
        "CGO_ENABLED": "0",
        "GOARCH": target_arch,
        "GOOS": target_os,
        "vcs.modified": "false",
        "vcs.revision": head_sha,
    }
    if settings != wanted:
        raise SystemExit(f"{name} does not contain exact source and platform build metadata")
PY

case "$(uname -s)/$(uname -m)" in
  Linux/x86_64) native_binary="$output_dir/qurl-linux-amd64" ;;
  Darwin/x86_64) native_binary="$output_dir/qurl-darwin-amd64" ;;
  Darwin/arm64) native_binary="$output_dir/qurl-darwin-arm64" ;;
  *)
    echo "artifact builder has no native trust verifier for this host" >&2
    exit 73
    ;;
esac
actual_fingerprint=$("$native_binary" version --verify-release-native-trust)
[[ "$actual_fingerprint" == "$hub_public_key_sha256" ]] || {
  echo "customer artifacts do not carry the selected release trust root" >&2
  exit 73
}
