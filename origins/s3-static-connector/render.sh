#!/bin/sh
# Render nginx.conf + envoy.yaml from the environment contract into RENDER_DIR.
# Sourced by entrypoint.sh at container start and exercised directly by
# test/render_test.sh. POSIX sh (busybox/dash in the image, bash on CI/dev).
set -eu

TEMPLATE_DIR="${TEMPLATE_DIR:-/etc/qurl/templates}"
RENDER_DIR="${RENDER_DIR:-/etc/qurl/rendered}"

# --- environment contract (defaults mirror the Dockerfile ENV + README) ---
: "${S3_BUCKET:?S3_BUCKET is required}"
S3_PREFIX="${S3_PREFIX:-}"
LISTEN_ADDR="${LISTEN_ADDR:-127.0.0.1:8080}"
ENVOY_LISTEN_ADDR="${ENVOY_LISTEN_ADDR:-127.0.0.1:9090}"
ALLOW_NON_LOOPBACK_LISTEN="${ALLOW_NON_LOOPBACK_LISTEN:-false}"
INDEX_DOCUMENT="${INDEX_DOCUMENT:-index.html}"
CACHE_MAX_SIZE="${CACHE_MAX_SIZE:-1g}"
# Unset → defer to the object's Cache-Control / nginx default (cache only what
# S3 marks cacheable), per the env contract. Set it to force a fallback TTL.
CACHE_DEFAULT_TTL="${CACHE_DEFAULT_TTL:-}"
: "${AWS_REGION:?AWS_REGION is required (pass it, or let entrypoint resolve it from IMDS)}"

if [ "${S3_BUCKET#*.}" != "$S3_BUCKET" ]; then
  echo "S3_BUCKET must not contain dots; this image uses virtual-hosted-style S3 TLS/SNI, which is incompatible with dotted bucket names" >&2
  exit 1
fi

if ! printf '%s\n' "$S3_PREFIX" | grep -Eq '^[A-Za-z0-9._/-]*$'; then
  echo "S3_PREFIX must contain only letters, numbers, dots, underscores, hyphens, and slashes" >&2
  exit 1
fi

if [ -z "$INDEX_DOCUMENT" ]; then
  echo "INDEX_DOCUMENT must not be empty" >&2
  exit 1
fi
if ! printf '%s\n' "$INDEX_DOCUMENT" | grep -Eq '^[A-Za-z0-9._-]+$'; then
  echo "INDEX_DOCUMENT must contain only letters, numbers, dots, underscores, and hyphens" >&2
  exit 1
fi

listener_host() {
  case "$1" in
    \[*\]:*)
      host="${1%%]:*}"
      printf '%s' "${host#\[}"
      ;;
    *)
      printf '%s' "${1%:*}"
      ;;
  esac
}

validate_listener() {
  name="$1"
  addr="$2"
  case "$addr" in
    \[*\]:*|*:*) ;;
    *)
      echo "$name must be a host:port listener address (got $addr)" >&2
      exit 1
      ;;
  esac
  host="$(listener_host "$addr")"
  port="${addr##*:}"
  if [ -z "$host" ] || [ -z "$port" ]; then
    echo "$name must be a host:port listener address (got $addr)" >&2
    exit 1
  fi
  if ! printf '%s\n' "$host" | grep -Eq '^(localhost|[A-Za-z0-9_.-]+|[0-9A-Fa-f:]+)$'; then
    echo "$name host contains unsupported characters (got $addr)" >&2
    exit 1
  fi
  case "$port" in
    *[!0-9]*)
      echo "$name port must be numeric (got $addr)" >&2
      exit 1
      ;;
  esac
  if [ "$port" -lt 1 ] || [ "$port" -gt 65535 ]; then
    echo "$name port must be between 1 and 65535 (got $addr)" >&2
    exit 1
  fi
}

assert_loopback_listener() {
  name="$1"
  addr="$2"
  host="$(listener_host "$addr")"
  case "$host" in
    127.*|localhost|::1)
      return
      ;;
  esac
  echo "$name must bind loopback by default (got $addr); set ALLOW_NON_LOOPBACK_LISTEN=true only for local tests or diagnostics" >&2
  exit 1
}

validate_listener LISTEN_ADDR "$LISTEN_ADDR"
validate_listener ENVOY_LISTEN_ADDR "$ENVOY_LISTEN_ADDR"
if [ "$ALLOW_NON_LOOPBACK_LISTEN" != "true" ]; then
  assert_loopback_listener LISTEN_ADDR "$LISTEN_ADDR"
  assert_loopback_listener ENVOY_LISTEN_ADDR "$ENVOY_LISTEN_ADDR"
fi

if ! printf '%s\n' "$CACHE_MAX_SIZE" | grep -Eq '^[0-9]+[kKmMgG]?$'; then
  echo "CACHE_MAX_SIZE must be an nginx size literal such as 128m, 1g, or 1024" >&2
  exit 1
fi
case "$CACHE_MAX_SIZE" in
  *[1-9]*) ;;
  *)
    echo "CACHE_MAX_SIZE must be greater than zero" >&2
    exit 1
    ;;
esac

# --- derived values ---
# Virtual-hosted-style S3 endpoint: this is the Host we sign and the SNI we send.
S3_HOST="${S3_BUCKET}.s3.${AWS_REGION}.amazonaws.com"

# Normalize S3_PREFIX to "" or "/seg/seg" (leading slash, no trailing slash) so
# the final key is "${S3_PREFIX_NORMALIZED}${uri}${clean_suffix}" and $uri always
# starts with "/".
S3_PREFIX_NORMALIZED="$S3_PREFIX"
while [ "${S3_PREFIX_NORMALIZED#/}" != "$S3_PREFIX_NORMALIZED" ]; do
  S3_PREFIX_NORMALIZED="${S3_PREFIX_NORMALIZED#/}"
done
while [ "${S3_PREFIX_NORMALIZED%/}" != "$S3_PREFIX_NORMALIZED" ]; do
  S3_PREFIX_NORMALIZED="${S3_PREFIX_NORMALIZED%/}"
done
if [ -n "$S3_PREFIX_NORMALIZED" ]; then
  S3_PREFIX_NORMALIZED="/${S3_PREFIX_NORMALIZED}"
fi

# Envoy's socket_address needs host + port split out.
ENVOY_LISTEN_HOST="${ENVOY_LISTEN_ADDR%:*}"
ENVOY_LISTEN_PORT="${ENVOY_LISTEN_ADDR##*:}"

# Upstream Envoy dials. Defaults to the real S3 vhost over TLS; the behavior
# tests override these to point at a plaintext local stub.
S3_ENDPOINT_ADDR="${S3_ENDPOINT_ADDR:-$S3_HOST}"
S3_ENDPOINT_PORT="${S3_ENDPOINT_PORT:-443}"
S3_TLS="${S3_TLS:-true}"

# Forced-fallback caching is opt-in: only emit proxy_cache_valid for successful
# object responses when CACHE_DEFAULT_TTL is set. Unset = object Cache-Control.
if [ -n "$CACHE_DEFAULT_TTL" ]; then
  if ! printf '%s\n' "$CACHE_DEFAULT_TTL" | grep -Eq '^([0-9]+(ms|s|m|h|d|w|M|y)?)+$'; then
    echo "CACHE_DEFAULT_TTL must be an nginx time literal such as 60s, 5m, or 1h30m" >&2
    exit 1
  fi
  case "$CACHE_DEFAULT_TTL" in
    *[1-9]*) ;;
    *)
      echo "CACHE_DEFAULT_TTL must be greater than zero; leave it unset to avoid a fallback TTL" >&2
      exit 1
      ;;
  esac
  CACHE_FALLBACK_DIRECTIVE="proxy_cache_valid 200 206 ${CACHE_DEFAULT_TTL};"
else
  CACHE_FALLBACK_DIRECTIVE="# CACHE_DEFAULT_TTL unset; no fallback proxy_cache_valid emitted."
fi

mkdir -p "$RENDER_DIR"

export LISTEN_ADDR ENVOY_LISTEN_ADDR INDEX_DOCUMENT S3_PREFIX_NORMALIZED \
  S3_HOST CACHE_MAX_SIZE CACHE_FALLBACK_DIRECTIVE \
  ENVOY_LISTEN_HOST ENVOY_LISTEN_PORT AWS_REGION S3_ENDPOINT_ADDR S3_ENDPOINT_PORT

# nginx.conf — substitute only our own vars; nginx's own $variables pass through.
envsubst '${LISTEN_ADDR} ${ENVOY_LISTEN_ADDR} ${INDEX_DOCUMENT} ${S3_PREFIX_NORMALIZED} ${S3_HOST} ${CACHE_MAX_SIZE} ${CACHE_FALLBACK_DIRECTIVE}' \
  < "${TEMPLATE_DIR}/nginx.conf.template" > "${RENDER_DIR}/nginx.conf"

# envoy.yaml — head (listener + SigV4 filter + cluster endpoint), then the TLS
# transport_socket appended only when talking to real S3 (tests use plaintext).
envsubst '${ENVOY_LISTEN_HOST} ${ENVOY_LISTEN_PORT} ${S3_HOST} ${AWS_REGION} ${S3_ENDPOINT_ADDR} ${S3_ENDPOINT_PORT}' \
  < "${TEMPLATE_DIR}/envoy.yaml.template" > "${RENDER_DIR}/envoy.yaml"
if [ "$S3_TLS" = "true" ]; then
  envsubst '${S3_HOST}' < "${TEMPLATE_DIR}/envoy.tls.partial" >> "${RENDER_DIR}/envoy.yaml"
fi
