#!/usr/bin/env bash
# Config-render golden test: render nginx.conf + envoy.yaml for representative
# env sets and diff against committed goldens. Locks the security header set,
# the clean-URL rules, cache size/ttl, the SigV4 filter knobs, and the bucket
# vhost host/SNI. Run with UPDATE=1 to regenerate goldens after an intended
# change.
set -euo pipefail
DIR="$(cd "$(dirname "$0")/.." && pwd)"
GOLDEN="$DIR/test/golden"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

render() {
  name="$1"; shift
  env "$@" TEMPLATE_DIR="$DIR/templates" RENDER_DIR="$TMP/$name" sh "$DIR/render.sh"
}

render default S3_BUCKET=example-bucket AWS_REGION=us-east-1
render prefix  S3_BUCKET=example-bucket AWS_REGION=us-west-2 S3_PREFIX=site/ INDEX_DOCUMENT=home.htm CACHE_DEFAULT_TTL=60s

if render invalid-ttl S3_BUCKET=example-bucket AWS_REGION=us-east-1 CACHE_DEFAULT_TTL='60s; include /etc/passwd' 2>"$TMP/invalid-ttl.err"; then
  echo "MISMATCH: invalid CACHE_DEFAULT_TTL rendered successfully" >&2
  exit 1
fi
grep -q "CACHE_DEFAULT_TTL must be an nginx time literal" "$TMP/invalid-ttl.err"

if render invalid-cache-size S3_BUCKET=example-bucket AWS_REGION=us-east-1 CACHE_MAX_SIZE='1g; include /etc/passwd' 2>"$TMP/invalid-cache-size.err"; then
  echo "MISMATCH: invalid CACHE_MAX_SIZE rendered successfully" >&2
  exit 1
fi
grep -q "CACHE_MAX_SIZE must be an nginx size literal" "$TMP/invalid-cache-size.err"

if render zero-cache-size S3_BUCKET=example-bucket AWS_REGION=us-east-1 CACHE_MAX_SIZE=0 2>"$TMP/zero-cache-size.err"; then
  echo "MISMATCH: zero CACHE_MAX_SIZE rendered successfully" >&2
  exit 1
fi
grep -q "CACHE_MAX_SIZE must be greater than zero" "$TMP/zero-cache-size.err"

if render invalid-index S3_BUCKET=example-bucket AWS_REGION=us-east-1 INDEX_DOCUMENT='index.html; include /etc/passwd' 2>"$TMP/invalid-index.err"; then
  echo "MISMATCH: invalid INDEX_DOCUMENT rendered successfully" >&2
  exit 1
fi
grep -q "INDEX_DOCUMENT must contain only" "$TMP/invalid-index.err"

if render invalid-prefix S3_BUCKET=example-bucket AWS_REGION=us-east-1 S3_PREFIX='site"; include /etc/passwd' 2>"$TMP/invalid-prefix.err"; then
  echo "MISMATCH: invalid S3_PREFIX rendered successfully" >&2
  exit 1
fi
grep -q "S3_PREFIX must contain only" "$TMP/invalid-prefix.err"

if render zero-ttl S3_BUCKET=example-bucket AWS_REGION=us-east-1 CACHE_DEFAULT_TTL=0s 2>"$TMP/zero-ttl.err"; then
  echo "MISMATCH: zero CACHE_DEFAULT_TTL rendered successfully" >&2
  exit 1
fi
grep -q "CACHE_DEFAULT_TTL must be greater than zero" "$TMP/zero-ttl.err"

if render dotted-bucket S3_BUCKET=my.static.site AWS_REGION=us-east-1 2>"$TMP/dotted-bucket.err"; then
  echo "MISMATCH: dotted S3_BUCKET rendered successfully" >&2
  exit 1
fi
grep -q "S3_BUCKET must not contain dots" "$TMP/dotted-bucket.err"

if render invalid-bucket S3_BUCKET='Example_Bucket' AWS_REGION=us-east-1 2>"$TMP/invalid-bucket.err"; then
  echo "MISMATCH: invalid S3_BUCKET rendered successfully" >&2
  exit 1
fi
grep -q "S3_BUCKET must use 3-63 lowercase" "$TMP/invalid-bucket.err"

if render injected-bucket S3_BUCKET=$'example-bucket\ninclude /etc/passwd' AWS_REGION=us-east-1 2>"$TMP/injected-bucket.err"; then
  echo "MISMATCH: injected S3_BUCKET rendered successfully" >&2
  exit 1
fi
grep -q "S3_BUCKET must use 3-63 lowercase" "$TMP/injected-bucket.err"

if render invalid-region S3_BUCKET=example-bucket AWS_REGION='us-east-1;include' 2>"$TMP/invalid-region.err"; then
  echo "MISMATCH: invalid AWS_REGION rendered successfully" >&2
  exit 1
fi
grep -q "AWS_REGION must use lowercase" "$TMP/invalid-region.err"

if render injected-region S3_BUCKET=example-bucket AWS_REGION=$'us-east-1\ninclude' 2>"$TMP/injected-region.err"; then
  echo "MISMATCH: injected AWS_REGION rendered successfully" >&2
  exit 1
fi
grep -q "AWS_REGION must use lowercase" "$TMP/injected-region.err"

if render invalid-endpoint-addr S3_BUCKET=example-bucket AWS_REGION=us-east-1 S3_ENDPOINT_ADDR=$'stub\ninclude' 2>"$TMP/invalid-endpoint-addr.err"; then
  echo "MISMATCH: invalid S3_ENDPOINT_ADDR rendered successfully" >&2
  exit 1
fi
grep -q "S3_ENDPOINT_ADDR must be" "$TMP/invalid-endpoint-addr.err"

if render invalid-endpoint-port S3_BUCKET=example-bucket AWS_REGION=us-east-1 S3_ENDPOINT_PORT='9000;include' 2>"$TMP/invalid-endpoint-port.err"; then
  echo "MISMATCH: invalid S3_ENDPOINT_PORT rendered successfully" >&2
  exit 1
fi
grep -q "S3_ENDPOINT_PORT must be numeric" "$TMP/invalid-endpoint-port.err"

if render invalid-s3-tls S3_BUCKET=example-bucket AWS_REGION=us-east-1 S3_TLS=maybe 2>"$TMP/invalid-s3-tls.err"; then
  echo "MISMATCH: invalid S3_TLS rendered successfully" >&2
  exit 1
fi
grep -q "S3_TLS must be true or false" "$TMP/invalid-s3-tls.err"

if render plaintext-s3-no-escape S3_BUCKET=example-bucket AWS_REGION=us-east-1 S3_TLS=false S3_ENDPOINT_ADDR=stub S3_ENDPOINT_PORT=9000 2>"$TMP/plaintext-s3-no-escape.err"; then
  echo "MISMATCH: plaintext S3 endpoint rendered without escape hatch" >&2
  exit 1
fi
grep -q "S3_TLS=false is only allowed" "$TMP/plaintext-s3-no-escape.err"

if render public-listen S3_BUCKET=example-bucket AWS_REGION=us-east-1 LISTEN_ADDR=0.0.0.0:8080 2>"$TMP/public-listen.err"; then
  echo "MISMATCH: non-loopback LISTEN_ADDR rendered successfully" >&2
  exit 1
fi
grep -q "LISTEN_ADDR must bind loopback" "$TMP/public-listen.err"

if render invalid-listen S3_BUCKET=example-bucket AWS_REGION=us-east-1 LISTEN_ADDR='127.0.0.1:8080;include' 2>"$TMP/invalid-listen.err"; then
  echo "MISMATCH: invalid LISTEN_ADDR rendered successfully" >&2
  exit 1
fi
grep -q "LISTEN_ADDR port must be numeric" "$TMP/invalid-listen.err"

render ipv6-loopback S3_BUCKET=example-bucket AWS_REGION=us-east-1 LISTEN_ADDR='[::1]:8080' ENVOY_LISTEN_ADDR='[::1]:9090'
grep -q 'server \[::1\]:9090;' "$TMP/ipv6-loopback/nginx.conf"
grep -q 'socket_address: { address: "::1", port_value: 9090 }' "$TMP/ipv6-loopback/envoy.yaml"
render public-listen-allowed S3_BUCKET=example-bucket AWS_REGION=us-east-1 LISTEN_ADDR=0.0.0.0:8080 ALLOW_NON_LOOPBACK_LISTEN=true
render plaintext-s3-allowed S3_BUCKET=example-bucket AWS_REGION=us-east-1 S3_TLS=false S3_ENDPOINT_ADDR=stub S3_ENDPOINT_PORT=9000 ALLOW_NON_LOOPBACK_LISTEN=true

map_golden() {
  case "$1" in
    default/nginx.conf) echo nginx.default.conf ;;
    default/envoy.yaml) echo envoy.default.yaml ;;
    prefix/nginx.conf)  echo nginx.prefix.conf ;;
    prefix/envoy.yaml)  echo envoy.prefix.yaml ;;
  esac
}

rc=0
for f in default/nginx.conf default/envoy.yaml prefix/nginx.conf prefix/envoy.yaml; do
  g="$GOLDEN/$(map_golden "$f")"
  if [ "${UPDATE:-0}" = "1" ]; then
    mkdir -p "$GOLDEN"; cp "$TMP/$f" "$g"; echo "updated $g"
  elif ! diff -u "$g" "$TMP/$f"; then
    echo "MISMATCH: rendered $f differs from $g" >&2; rc=1
  fi
done

[ "$rc" -eq 0 ] && [ "${UPDATE:-0}" != "1" ] && echo "render goldens OK"
exit "$rc"
