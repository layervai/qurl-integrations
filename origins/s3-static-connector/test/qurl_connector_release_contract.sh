#!/usr/bin/env bash
# Proves the immutable Connector release used by customer instructions accepts
# the canonical S3-origin route through its strict YAML decoder.
set -euo pipefail

IMG="${IMG:-ghcr.io/layervai/qurl-connector@sha256:801502353b82e1932c6d22ace172de591abdbeabbcd9d1ea0503513b4de1be96}"
PLATFORM="${PLATFORM:-linux/amd64}"
EXPECTED_VERSION="${EXPECTED_VERSION:-v0.7.1}"

version_output="$(docker run --rm --platform "$PLATFORM" \
  --entrypoint /usr/local/bin/qurl-connector \
  "$IMG" version --short 2>&1)"
# Keep this route's field set identical to the one the Slack flow generates in
# renderS3WebsiteConnectorConfigYAML. A strict decoder rejects unknown fields,
# so testing a thinner route would prove nothing about the config customers
# actually run. TestS3WebsiteReleaseContractRouteMatchesRenderedConfig fences
# the two against drift.
output="$(printf '%s\n' \
  'routes:' \
  '  - id: s3-static-release-contract' \
  '    type: http' \
  '    local_ip: 127.0.0.1' \
  '    local_port: 8080' \
  '    resource_id: MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE2cTVv5_3eeYCcLLq5ROYCqcmY50HiKZ9ATglIkPnCji1E_S63UMtXba1moR8-Q6EV7oM6zwwh9_j2CDujzXvLA' \
  '    connector_routing_id: c-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' \
  | docker run --rm -i --platform "$PLATFORM" \
      --entrypoint /usr/local/bin/qurl-connector \
      "$IMG" --config /dev/stdin list --json 2>&1)"

fail() {
  printf 'FAIL Connector S3-origin contract for %s (%s): %s\n' "$IMG" "$PLATFORM" "$1" >&2
  printf '%s\n' "$output" >&2
  exit 1
}

case "$version_output" in
  *"qurl-connector $EXPECTED_VERSION"*) ;;
  *) fail "immutable image did not identify itself as $EXPECTED_VERSION" ;;
esac
if ! jq -e '
  length == 1 and
  .[0].id == "s3-static-release-contract" and
  .[0].type == "http" and
  .[0].local_ip == "127.0.0.1" and
  .[0].local_port == 8080 and
  (.[0].resource_id | type == "string" and length > 0) and
  (.[0].connector_routing_id | startswith("c-"))
' >/dev/null <<<"$output"; then
  fail "strict YAML parsing did not return the canonical loopback route"
fi

printf 'Connector S3-origin route contract passed for %s (%s)\n' "$IMG" "$PLATFORM"
