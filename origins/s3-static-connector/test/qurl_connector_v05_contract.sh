#!/usr/bin/env bash
# Proves the immutable production Connector v0.5 image accepts its canonical
# S3-origin route through the strict YAML decoder. This intentionally checks
# the v0.5 route schema, not Slack's v0.6 identity-pinned install schema:
# production v0.5 predates connector_routing_id.
set -euo pipefail

IMG="${IMG:-ghcr.io/layervai/qurl-connector@sha256:ef343d29dcf349b63b4dc4986789ed6691f86b7e9f98424d4ec8c5f54e37f07e}"
PLATFORM="${PLATFORM:-linux/amd64}"

version_output="$(docker run --rm --platform "$PLATFORM" \
  --entrypoint /usr/local/bin/qurl-connector \
  "$IMG" version 2>&1)"
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
      "$IMG" --config /dev/stdin list --json 2>&1)"

fail() {
  printf 'FAIL Connector v0.5 S3-origin contract for %s (%s): %s\n' "$IMG" "$PLATFORM" "$1" >&2
  printf '%s\n' "$output" >&2
  exit 1
}

case "$version_output" in
  *"qurl-connector v0.5.0 "*) ;;
  *) fail "immutable image did not identify itself as v0.5.0" ;;
esac
if ! jq -e '
  length == 1 and
  .[0].id == "s3-static-v05-contract" and
  .[0].type == "http" and
  .[0].local_ip == "127.0.0.1" and
  .[0].local_port == 8080
' >/dev/null <<<"$output"; then
  fail "strict YAML parsing did not return the canonical loopback route"
fi

printf 'Connector v0.5 S3-origin route contract passed for %s (%s)\n' "$IMG" "$PLATFORM"
