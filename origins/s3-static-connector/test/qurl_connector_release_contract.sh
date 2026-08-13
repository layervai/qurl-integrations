#!/usr/bin/env bash
# Proves the immutable Connector release used by customer instructions accepts
# the canonical S3-origin route through its strict YAML decoder.
set -euo pipefail

IMG="${IMG:-ghcr.io/layervai/qurl-connector@sha256:801502353b82e1932c6d22ace172de591abdbeabbcd9d1ea0503513b4de1be96}"
PLATFORM="${PLATFORM:-linux/amd64}"
EXPECTED_VERSION="${EXPECTED_VERSION:-v0.7.1}"

# The customer-facing Connector release image cannot be published until the
# underlying platform reaches production, so IMG may not resolve yet. Preflight
# the ref rather than letting `docker run` fail with an opaque exit 125.
# CONNECTOR_RELEASE_IMAGE_REQUIRED="true" makes an unresolvable image fail
# closed; the default tolerates it with a loud warning and leans on the
# emitter-side golden fence in slack / test. This lives here, not in the
# workflow, so a local run behaves exactly like CI. Mirrors the
# *_PRODUCER_FIXTURE_REQUIRED knobs in layervai/qurl-connector's
# cross-repo-contracts.yml.
REQUIRE_IMAGE="${CONNECTOR_RELEASE_IMAGE_REQUIRED:-false}"
if ! docker buildx imagetools inspect "$IMG" >/dev/null 2>&1; then
  if [ "$REQUIRE_IMAGE" = "true" ]; then
    echo "::error::pinned Connector release image $IMG does not resolve and CONNECTOR_RELEASE_IMAGE_REQUIRED=true" >&2
    exit 1
  fi
  echo "::warning title=Connector release image pending::$IMG does not resolve. The customer-facing Connector release cannot publish until the underlying platform reaches production, so this run falls back to the emitter-side fence in slack / test (TestS3WebsiteReleaseContractRouteMatchesRenderedConfig). Flip CONNECTOR_RELEASE_IMAGE_REQUIRED to true once a public release exists." >&2
  exit 0
fi

version_output="$(docker run --rm --platform "$PLATFORM" \
  --entrypoint /usr/local/bin/qurl-connector \
  "$IMG" version --short 2>&1)"
# The route fed to the decoder is the golden regenerated from
# renderS3WebsiteConnectorConfigYAML by the Go test named below, so this proves
# the released Connector accepts the exact config the Slack flow emits. A strict
# decoder rejects unknown fields, so a hand-copied route here would prove
# nothing about what customers actually run.
#   apps/slack/internal/handler_s3_website_test.go
#   TestS3WebsiteReleaseContractRouteMatchesRenderedConfig (UPDATE_GOLDEN=1)
ROUTE_GOLDEN="${ROUTE_GOLDEN:-$(dirname "$0")/golden/s3-website-route.yaml}"
if [ ! -r "$ROUTE_GOLDEN" ]; then
  printf 'FAIL Connector S3-origin contract: route golden %s is missing\n' "$ROUTE_GOLDEN" >&2
  exit 1
fi
output="$(docker run --rm -i --platform "$PLATFORM" \
      --entrypoint /usr/local/bin/qurl-connector \
      "$IMG" --config /dev/stdin list --json 2>&1 < "$ROUTE_GOLDEN")"

fail() {
  printf 'FAIL Connector S3-origin contract for %s (%s): %s\n' "$IMG" "$PLATFORM" "$1" >&2
  printf '%s\n' "$output" >&2
  exit 1
}

case "$version_output" in
  *"qurl-connector $EXPECTED_VERSION"*) ;;
  *) fail "immutable image did not identify itself as $EXPECTED_VERSION" ;;
esac
# Expected values come from the golden itself: the decoder must echo back what
# it was fed, so the assertion cannot drift from the input the way a hand-kept
# literal would. The golden is a flat one-route document, so a line match is
# sufficient here and keeps this dependency-free.
route_field() {
  sed -nE "s/^[[:space:]]*(- )?$1: '?(.*[^'])'?[[:space:]]*\$/\2/p" "$ROUTE_GOLDEN" | head -1
}
for field in id type local_ip local_port resource_id connector_routing_id; do
  if [ -z "$(route_field "$field")" ]; then
    fail "route golden $ROUTE_GOLDEN is missing $field"
  fi
done
if ! jq -e \
  --arg id "$(route_field id)" \
  --arg type "$(route_field type)" \
  --arg local_ip "$(route_field local_ip)" \
  --argjson local_port "$(route_field local_port)" \
  --arg resource_id "$(route_field resource_id)" \
  --arg connector_routing_id "$(route_field connector_routing_id)" '
  length == 1 and
  .[0].id == $id and
  .[0].type == $type and
  .[0].local_ip == $local_ip and
  .[0].local_port == $local_port and
  .[0].resource_id == $resource_id and
  .[0].connector_routing_id == $connector_routing_id
' >/dev/null <<<"$output"; then
  fail "strict YAML parsing did not return the canonical loopback route"
fi

printf 'Connector S3-origin route contract passed for %s (%s)\n' "$IMG" "$PLATFORM"
