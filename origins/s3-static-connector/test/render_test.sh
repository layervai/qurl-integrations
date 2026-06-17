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

if render zero-ttl S3_BUCKET=example-bucket AWS_REGION=us-east-1 CACHE_DEFAULT_TTL=0s 2>"$TMP/zero-ttl.err"; then
  echo "MISMATCH: zero CACHE_DEFAULT_TTL rendered successfully" >&2
  exit 1
fi
grep -q "CACHE_DEFAULT_TTL must be greater than zero" "$TMP/zero-ttl.err"

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
