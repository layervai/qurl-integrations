#!/usr/bin/env bash
# Proves the immutable production Connector v0.5 image accepts the canonical
# S3-origin route through its strict YAML decoder. The process must advance to
# the expected missing-enrollment-credential boundary; a schema or startup
# failure before that point is a compatibility regression.
set -euo pipefail

IMG="${IMG:-ghcr.io/layervai/qurl-connector@sha256:ef343d29dcf349b63b4dc4986789ed6691f86b7e9f98424d4ec8c5f54e37f07e}"
PLATFORM="${PLATFORM:-linux/amd64}"

set +e
output="$(printf '%s\n' \
  'server:' \
  '  protocol: tcp' \
  'routes:' \
  '  - id: s3-static-v05-contract' \
  '    type: http' \
  '    local_ip: 127.0.0.1' \
  '    local_port: 8080' \
  | docker run --rm -i --platform "$PLATFORM" \
      --entrypoint /usr/local/bin/qurl-connector \
      -e LAYERV_AGENT_STATE_DIR=/tmp/agent \
      -e QURL_AUDIT_FILE=/tmp/qurl-connector-audit.log \
      "$IMG" --config /dev/stdin run 2>&1)"
status=$?
set -e

fail() {
  printf 'FAIL Connector v0.5 S3-origin contract for %s (%s): %s\n' "$IMG" "$PLATFORM" "$1" >&2
  printf '%s\n' "$output" >&2
  exit 1
}

if [ "$status" -eq 0 ]; then
  fail "Connector unexpectedly started without an enrollment credential"
fi
case "$output" in
  *"qurl-connector v0.5.0 (client)"*) ;;
  *) fail "immutable image did not identify itself as v0.5.0" ;;
esac
case "$output" in
  *"API key required for first bootstrap or state recovery"*) ;;
  *) fail "route did not reach the expected post-parse credential boundary" ;;
esac
case "$output" in
  *"parsing config file"*|*"field local_"*|*"field routes"*)
    fail "strict YAML parsing rejected the canonical loopback route"
    ;;
esac

printf 'Connector v0.5 S3-origin route contract passed for %s (%s)\n' "$IMG" "$PLATFORM"
