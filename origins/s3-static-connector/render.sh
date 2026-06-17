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
ALLOW_PLAINTEXT_S3="${ALLOW_PLAINTEXT_S3:-false}"
INDEX_DOCUMENT="${INDEX_DOCUMENT:-index.html}"
CACHE_MAX_SIZE="${CACHE_MAX_SIZE:-1g}"
# Unset → defer to the object's Cache-Control / nginx default (cache only what
# S3 marks cacheable), per the env contract. Set it to force a fallback TTL.
CACHE_DEFAULT_TTL="${CACHE_DEFAULT_TTL:-}"
: "${AWS_REGION:?AWS_REGION is required (set AWS_REGION, or set AWS_DEFAULT_REGION before entrypoint runs)}"

if [ "${S3_BUCKET#*.}" != "$S3_BUCKET" ]; then
  echo "S3_BUCKET must not contain dots; this image uses virtual-hosted-style S3 TLS/SNI, which is incompatible with dotted bucket names" >&2
  exit 1
fi
case "$S3_BUCKET" in
  *[!a-z0-9-]*|-*|*-)
    echo "S3_BUCKET must use 3-63 lowercase letters, numbers, and hyphens, without leading or trailing hyphens" >&2
    exit 1
    ;;
esac
if [ "${#S3_BUCKET}" -lt 3 ] || [ "${#S3_BUCKET}" -gt 63 ]; then
  echo "S3_BUCKET must use 3-63 lowercase letters, numbers, and hyphens, without leading or trailing hyphens" >&2
  exit 1
fi
case "$AWS_REGION" in
  *[!a-z0-9-]*|-*|*-|*--*)
    echo "AWS_REGION must use lowercase letters, numbers, and single hyphen separators" >&2
    exit 1
    ;;
esac

case "$S3_PREFIX" in
  *[!A-Za-z0-9._/-]*)
    echo "S3_PREFIX must contain only letters, numbers, dots, underscores, hyphens, and slashes" >&2
    exit 1
    ;;
esac
case "/$S3_PREFIX/" in
  */./*|*/../*)
    echo "S3_PREFIX must not contain dot or dot-dot path segments" >&2
    exit 1
    ;;
esac

if [ -z "$INDEX_DOCUMENT" ]; then
  echo "INDEX_DOCUMENT must not be empty" >&2
  exit 1
fi
case "$INDEX_DOCUMENT" in
  *[!A-Za-z0-9._-]*)
    echo "INDEX_DOCUMENT must contain only letters, numbers, dots, underscores, and hyphens" >&2
    exit 1
    ;;
esac

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

validate_port() {
  port_label="$1"
  port_value="$2"
  port_got="$3"
  case "$port_value" in
    ""|*[!0-9]*)
      echo "$port_label must be numeric (got $port_got)" >&2
      exit 1
      ;;
  esac
  if [ "$port_value" -lt 1 ] || [ "$port_value" -gt 65535 ]; then
    echo "$port_label must be between 1 and 65535 (got $port_got)" >&2
    exit 1
  fi
}

valid_ipv4_octet() {
  case "$1" in
    ""|*[!0-9]*)
      return 1
      ;;
  esac
  [ "$1" -le 255 ]
}

is_ipv4_loopback() {
  case "$1" in
    127.*.*.*) ;;
    *) return 1 ;;
  esac
  case "$1" in
    *.*.*.*.*) return 1 ;;
  esac

  loopback_rest="${1#127.}"
  loopback_octet_b="${loopback_rest%%.*}"
  loopback_rest="${loopback_rest#*.}"
  loopback_octet_c="${loopback_rest%%.*}"
  loopback_octet_d="${loopback_rest#*.}"
  valid_ipv4_octet "$loopback_octet_b" &&
    valid_ipv4_octet "$loopback_octet_c" &&
    valid_ipv4_octet "$loopback_octet_d"
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
  case "$host" in
    *[!A-Za-z0-9_.:-]*)
      echo "$name host contains unsupported characters (got $addr)" >&2
      exit 1
      ;;
  esac
  validate_port "$name port" "$port" "$addr"
}

assert_loopback_listener() {
  name="$1"
  addr="$2"
  host="$(listener_host "$addr")"
  case "$host" in
    localhost|::1)
      return
      ;;
  esac
  if is_ipv4_loopback "$host"; then
    return
  fi
  echo "$name must bind loopback by default (got $addr); set ALLOW_NON_LOOPBACK_LISTEN=true only for local tests or diagnostics" >&2
  exit 1
}

validate_listener LISTEN_ADDR "$LISTEN_ADDR"
validate_listener ENVOY_LISTEN_ADDR "$ENVOY_LISTEN_ADDR"
if [ "$ALLOW_NON_LOOPBACK_LISTEN" != "true" ]; then
  assert_loopback_listener LISTEN_ADDR "$LISTEN_ADDR"
  assert_loopback_listener ENVOY_LISTEN_ADDR "$ENVOY_LISTEN_ADDR"
fi

case "$CACHE_MAX_SIZE" in
  ""|*[!0-9kKmMgG]*)
    echo "CACHE_MAX_SIZE must be an nginx size literal such as 128m, 1g, or 1024" >&2
    exit 1
    ;;
  *[kKmMgG])
    cache_max_size_digits="${CACHE_MAX_SIZE%?}"
    ;;
  *)
    cache_max_size_digits="$CACHE_MAX_SIZE"
    ;;
esac
case "$cache_max_size_digits" in
  ""|*[!0-9]*)
    echo "CACHE_MAX_SIZE must be an nginx size literal such as 128m, 1g, or 1024" >&2
    exit 1
    ;;
esac
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
case "$S3_PREFIX_NORMALIZED" in
  *//*)
    echo "S3_PREFIX must not contain empty path segments" >&2
    exit 1
    ;;
esac
if [ -n "$S3_PREFIX_NORMALIZED" ]; then
  S3_PREFIX_NORMALIZED="/${S3_PREFIX_NORMALIZED}"
fi

# Envoy's socket_address needs host + port split out.
ENVOY_LISTEN_HOST="$(listener_host "$ENVOY_LISTEN_ADDR")"
ENVOY_LISTEN_PORT="${ENVOY_LISTEN_ADDR##*:}"

# Upstream Envoy dials. Defaults to the real S3 vhost over TLS; the behavior
# tests override these to point at a plaintext local stub.
S3_ENDPOINT_ADDR="${S3_ENDPOINT_ADDR:-$S3_HOST}"
S3_ENDPOINT_PORT="${S3_ENDPOINT_PORT:-443}"
S3_TLS="${S3_TLS:-true}"
case "$S3_ENDPOINT_ADDR" in
  ""|*[!A-Za-z0-9_.:-]*)
    echo "S3_ENDPOINT_ADDR must be a DNS name, IPv4 address, or unbracketed IPv6 address" >&2
    exit 1
    ;;
esac
validate_port "S3_ENDPOINT_PORT" "$S3_ENDPOINT_PORT" "$S3_ENDPOINT_PORT"
case "$S3_TLS" in
  true|false) ;;
  *)
    echo "S3_TLS must be true or false" >&2
    exit 1
    ;;
esac
if [ "$S3_TLS" = "false" ] && [ "$ALLOW_PLAINTEXT_S3" != "true" ]; then
  echo "S3_TLS=false is only allowed with ALLOW_PLAINTEXT_S3=true for local tests or diagnostics" >&2
  exit 1
fi

# Forced-fallback caching is opt-in: only emit proxy_cache_valid for successful
# object responses when CACHE_DEFAULT_TTL is set. Unset = object Cache-Control.
if [ -n "$CACHE_DEFAULT_TTL" ]; then
  case "$CACHE_DEFAULT_TTL" in
    *[!0-9A-Za-z]*)
      echo "CACHE_DEFAULT_TTL must be an nginx time literal such as 60, 60s, 5m, or 1h30m; millisecond TTLs are intentionally not supported" >&2
      exit 1
      ;;
  esac
  ttl_literal_re='^([0-9]+y)?([0-9]+M)?([0-9]+w)?([0-9]+d)?([0-9]+h)?([0-9]+m)?([0-9]+s)?$|^[0-9]+$'
  if ! printf '%s\n' "$CACHE_DEFAULT_TTL" | grep -Eq "$ttl_literal_re"; then
    echo "CACHE_DEFAULT_TTL must be an nginx time literal such as 60, 60s, 5m, or 1h30m; millisecond TTLs are intentionally not supported" >&2
    exit 1
  fi
  case "$CACHE_DEFAULT_TTL" in
    *[1-9]*) ;;
    *)
      echo "CACHE_DEFAULT_TTL must be greater than zero; leave it unset to avoid a fallback TTL" >&2
      exit 1
      ;;
  esac
  CACHE_FALLBACK_DIRECTIVE="proxy_cache_valid 200 ${CACHE_DEFAULT_TTL};"
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
