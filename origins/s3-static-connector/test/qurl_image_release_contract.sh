#!/usr/bin/env bash
# Validates the immutable qurl release image against the exact headless share
# configuration emitted by the guided S3-origin flow.
set -euo pipefail

IMG="${QURL_IMAGE:-}"
REQUIRE_IMAGE="${QURL_RELEASE_IMAGE_REQUIRED:-false}"
IFS=' ' read -r -a PLATFORMS <<<"${PLATFORM:-linux/amd64 linux/arm64}"
# Absolute: docker -v rejects relative host paths.
SHARE_GOLDEN="${SHARE_GOLDEN:-$(cd "$(dirname "$0")" && pwd)/golden/s3-website-share.yaml}"

fail() {
  printf 'FAIL qurl S3-origin image contract for %s (%s): %s\n' "${IMG:-unset}" "${platform:-n/a}" "$1" >&2
  if [ -n "${output:-}" ]; then
    printf '%s\n' "$output" >&2
  fi
  exit 1
}

if [ -z "$IMG" ]; then
  if [ "$REQUIRE_IMAGE" = "true" ]; then
    fail 'QURL_IMAGE is required and must be the release digest'
  fi
  echo '::warning title=qurl release image pending::QURL_IMAGE is unset; the public release image contract is covered by CLI image smoke and the renderer fixture until a released digest is pinned.' >&2
  exit 0
fi
case "$IMG" in
  ghcr.io/layervai/qurl@sha256:[0-9a-f][0-9a-f]*) ;;
  *) fail 'QURL_IMAGE must be ghcr.io/layervai/qurl@sha256:<lowercase digest>' ;;
esac
digest="${IMG##*@sha256:}"
if [ "${#digest}" -ne 64 ] || printf '%s' "$digest" | grep -q '[^0-9a-f]'; then
  fail 'QURL_IMAGE digest must contain exactly 64 lowercase hexadecimal characters'
fi
resolves=false
for attempt in 1 2 3; do
  if docker buildx imagetools inspect "$IMG" >/dev/null 2>&1; then resolves=true; break; fi
  sleep $((attempt * 5))
done
if [ "$resolves" != true ]; then
  if [ "$REQUIRE_IMAGE" = "true" ]; then
    fail 'immutable qurl image does not resolve'
  fi
  echo "::warning title=qurl release image pending::$IMG does not resolve; set QURL_RELEASE_IMAGE_REQUIRED=true after the first public release." >&2
  exit 0
fi
if [ ! -r "$SHARE_GOLDEN" ]; then
  fail "headless share fixture $SHARE_GOLDEN is missing"
fi
# The daemon refuses a config writable by group/other; CI checkouts are often
# group-writable, so mount a private 0444 copy instead of the tracked file.
# Next to the golden (inside the checkout) rather than /tmp: a docker daemon
# that only shares the workspace would otherwise mount an empty directory.
GOLDEN_MOUNT_DIR="$(mktemp -d "$(cd "$(dirname "$0")" && pwd)/.share-mount-XXXXXX")"
trap 'rm -rf "$GOLDEN_MOUNT_DIR"' EXIT
GOLDEN_MOUNT="$GOLDEN_MOUNT_DIR/share.yaml"
# Private 0755 dir + 0444 file: the daemon rejects a config (or its directory)
# writable by group/other, and CI checkouts default to group-writable.
chmod 0755 "$GOLDEN_MOUNT_DIR" && cp "$SHARE_GOLDEN" "$GOLDEN_MOUNT" && chmod 0444 "$GOLDEN_MOUNT"

for platform in "${PLATFORMS[@]}"; do
  output=""
  # Pull first (retried): a pull hiccup under emulation otherwise surfaces as
  # "failed to report its version" with only progress lines as evidence.
  # A digest reference can hold only one platform locally; drop the previous
  # platform's copy or the next pull/run fails with "cannot overwrite digest".
  docker image rm -f "$IMG" >/dev/null 2>&1 || true
  pulled=false
  for attempt in 1 2 3; do
    if docker pull --quiet --platform "$platform" "$IMG" >/dev/null 2>&1; then pulled=true; break; fi
    sleep $((attempt * 5))
  done
  if [ "$pulled" != true ]; then
    output="$(docker pull --platform "$platform" "$IMG" 2>&1 | tail -5)"
    fail "could not pull the image for $platform"
  fi
  if ! version_output="$(docker run --rm --platform "$platform" "$IMG" version 2>&1)"; then
    output="$version_output
binfmt: $(ls /proc/sys/fs/binfmt_misc 2>/dev/null | tr '\n' ' ')
docker: $(docker version --format '{{.Server.Version}} {{.Server.Os}}/{{.Server.Arch}}' 2>/dev/null)"
    fail 'image failed to report its qurl version'
  fi
  # Line-anchored: a cold runner prefixes the merged output with pull progress.
  if ! printf '%s\n' "$version_output" | grep -q '^qurl version '; then
    output="$version_output"; fail 'image did not identify itself as qurl'
  fi

  # A valid first-boot config reaches the credential gate without attempting
  # network enrollment. Unknown/old config fields would fail earlier with a
  # decoder error, so this exercises the released image's strict YAML surface.
  # The image ships no writable /tmp for its non-root user; the rendered
  # installs mount state, so give the gate a tmpfs for --state-dir.
  # Run as the invoking uid: the daemon rejects a config owned by another
  # non-root user, and a Linux bind mount keeps the host owner.
  if output="$(docker run --rm --platform "$platform" \
      --user "$(id -u):$(id -g)" \
      --tmpfs /tmp:rw,nosuid,nodev \
      -v "$GOLDEN_MOUNT:/etc/qurl/share.yaml:ro" \
      "$IMG" daemon run --state-dir /tmp/qurl-state \
      --headless-config /etc/qurl/share.yaml 2>&1)"; then
    fail 'first bootstrap unexpectedly ran without an enrollment token file'
  fi
  case "$output" in
    *'--enrollment-token-file is required for first headless bootstrap'*) ;;
    *)
      ls -ln "$GOLDEN_MOUNT_DIR" "$GOLDEN_MOUNT" >&2 || true
      fail 'released qurl rejected the guided headless config before the expected credential gate' ;;
  esac

  printf 'qurl S3-origin headless contract passed for %s (%s)\n' "$IMG" "$platform"
done
