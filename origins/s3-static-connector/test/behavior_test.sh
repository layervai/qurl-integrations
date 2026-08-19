#!/usr/bin/env bash
# Behavior test: build the image and run it against a local stub S3, asserting
# the runtime contract — clean URLs, the exact security headers (on
# 200/404/405/5xx), query-string-tolerant caching, the error mapping that masks
# every upstream status but a 2xx object hit, the scrubbing of the x-amz-*/Envoy
# headers that would fingerprint the origin, Content-Type/Cache-Control
# passthrough, Range + HEAD, and that Envoy attaches a SigV4 Authorization
# header over the correctly-canonicalized path.
#
# The stub does not verify signatures (no shared secret); real SigV4 crypto is
# validated against S3 during the staging soak. See test/sigv4_fixtures.txt.
set -uo pipefail
DIR="$(cd "$(dirname "$0")/.." && pwd)"
IMG="${IMG:-s3-static-connector:test}"
REQUESTED_IMG="$IMG"
S3_ORIGIN_SECURITY_WAIVER_IMAGE="${S3_ORIGIN_SECURITY_WAIVER_IMAGE:-}"
NET="s3-static-connector-testnet"
STUB="s3-static-connector-stub"
ORIGIN="s3-static-connector-app"
# python:3.12.13-slim-trixie multi-arch index, used only for the local stub.
STUB_IMG="python:3.12-slim@sha256:d764629ce0ddd8c71fd371e9901efb324a95789d2315a47db7e4d27e78f1b0e9"
arch="$(uname -m)"; case "$arch" in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) ;; esac
PLATFORM="${PLATFORM:-linux/$arch}"
waive_security_contract=false
if [ -n "$S3_ORIGIN_SECURITY_WAIVER_IMAGE" ]; then
  if [[ ! "$S3_ORIGIN_SECURITY_WAIVER_IMAGE" =~ ^ghcr\.io/layervai/qurl-integrations/s3-static-connector@sha256:[0-9a-f]{64}$ ]]; then
    printf 'FAIL S3_ORIGIN_SECURITY_WAIVER_IMAGE must be the canonical released S3 origin digest\n' >&2
    exit 1
  fi
  if [ "$REQUESTED_IMG" != "$S3_ORIGIN_SECURITY_WAIVER_IMAGE" ]; then
    printf 'FAIL security waiver digest does not match the image under test\n' >&2
    exit 1
  fi
  waive_security_contract=true
fi

TMPROOT="${TMPDIR:-/tmp}"
H="$(mktemp "$TMPROOT/qns_h.XXXXXX")"
B="$(mktemp "$TMPROOT/qns_b.XXXXXX")"

cleanup() {
  rm -f "$H" "$B"
  docker rm -f "$STUB" "$ORIGIN" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
}
trap cleanup EXIT
cleanup

origin_base_url() {
  port="$(docker inspect -f '{{(index (index .NetworkSettings.Ports "8080/tcp") 0).HostPort}}' "$ORIGIN")"
  printf 'http://localhost:%s\n' "$port"
}

set -e
if [ "${SKIP_BUILD:-false}" = "true" ]; then
  echo "==> using existing $IMG ($PLATFORM)"
  # Digest references are remote release contracts and may be exercised for
  # more than one architecture in the same Docker daemon. Docker cannot bind
  # one digest reference to two platform-specific image IDs, so load each
  # requested platform through a uniquely tagged, otherwise-empty derivative.
  # CI-built local tags are already loaded and must not be resolved against a
  # registry.
  case "$IMG" in
    *@sha256:*)
      source_image="$IMG"
      digest="${IMG##*@sha256:}"
      platform_tag="s3-static-connector-contract:${digest:0:12}-${PLATFORM//\//-}"
      docker build --pull --platform "$PLATFORM" --build-arg SOURCE_IMAGE="$source_image" \
        -t "$platform_tag" -f - "$DIR" >/dev/null <<'EOF'
ARG SOURCE_IMAGE=scratch
FROM ${SOURCE_IMAGE}
EOF
      IMG="$platform_tag"
      ;;
  esac
else
  echo "==> building $IMG ($PLATFORM)"
  docker build --platform "$PLATFORM" -t "$IMG" "$DIR"
fi

# Slack's Docker, Compose, ECS, and Kubernetes renderers deliberately share a
# network namespace and numeric nonroot identity between the Connector and this
# origin. Fail before behavior testing if a base-image change silently breaks
# either part of that deployment contract. Keep the remediation text explicit:
# this check also runs against the exact digest pinned by the Slack renderer.
image_config="$(docker image inspect --format '{{json .Config}}' "$IMG")"
runtime_contract_fail() {
  printf 'FAIL runtime image contract for %s (%s): %s\n' "$IMG" "$PLATFORM" "$1" >&2
  printf 'Update defaultS3StaticConnectorImage and its generated deployment renderers only after testing a compatible digest.\n' >&2
  exit 1
}

if ! jq -e '.User == "65532:65532"' >/dev/null <<<"$image_config"; then
  runtime_contract_fail "expected Config.User=65532:65532"
fi
if ! jq -e '.ExposedPorts | has("8080/tcp")' >/dev/null <<<"$image_config"; then
  runtime_contract_fail "expected Config.ExposedPorts to contain 8080/tcp"
fi
if ! jq -e '[.Env[] | select(. == "LISTEN_ADDR=127.0.0.1:8080")] | length == 1' >/dev/null <<<"$image_config"; then
  runtime_contract_fail "expected exactly one LISTEN_ADDR=127.0.0.1:8080 default"
fi

# The live stub path below renders plaintext S3 so it can run locally without
# AWS. Validate the real TLS render path separately so Envoy schema changes
# (including host_rewrite/SAN verification) are caught on every supported
# architecture and on pinned Envoy base bumps.
docker run --rm --platform "$PLATFORM" --entrypoint sh \
  -e S3_BUCKET=example-bucket -e AWS_REGION=us-east-1 \
  "$IMG" -c 'set -eu; envoy --version >/dev/null; RENDER_DIR=/tmp/rendered render.sh; envoy --mode validate -c /tmp/rendered/envoy.yaml >/dev/null'

docker network create "$NET" >/dev/null

docker run -d --name "$STUB" --network "$NET" \
  -v "$DIR/test/stub-s3/stub.py:/stub.py:ro" \
  "$STUB_IMG" python /stub.py >/dev/null

docker run -d --platform "$PLATFORM" --name "$ORIGIN" --network "$NET" -p 127.0.0.1::8080 \
  -e S3_BUCKET=example-bucket -e AWS_REGION=us-east-1 \
  -e LISTEN_ADDR=0.0.0.0:8080 -e ALLOW_NON_LOOPBACK_LISTEN=true \
  -e ALLOW_PLAINTEXT_S3=true \
  -e CACHE_CONNECTOR_ID=stats-connector -e CACHE_REPLICA_ID=origin-a \
  -e S3_TLS=false -e S3_ENDPOINT_ADDR="$STUB" -e S3_ENDPOINT_PORT=9000 \
  -e AWS_ACCESS_KEY_ID=test -e AWS_SECRET_ACCESS_KEY=test \
  "$IMG" >/dev/null
base="$(origin_base_url)"
# Assertion commands below need to collect and report failures instead of
# aborting on the first mismatch.
set +e

ready=0
for _ in $(seq 1 40); do
  if [ "$(curl -s -o /dev/null -w '%{http_code}' "$base/")" = "200" ]; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != "1" ]; then
  echo "ORIGIN never became ready; logs:"; docker logs "$ORIGIN" 2>&1 | tail -40; exit 1
fi

pass=0; fail=0
fetch() { curl -s -D "$H" -o "$B" "$@"; }   # populates $H (headers) + $B (body)
hval() { tr -d '\r' < "$H" | awk -F': ' -v k="$1" 'tolower($1)==tolower(k){print $2}'; }
status_code() { tr -d '\r' < "$H" | awk 'NR==1{print $2}'; }
cache_entries() {
  docker exec "$ORIGIN" qurl-origin-cachectl status \
    | sed -n 's/.*"entries":\([0-9][0-9]*\).*/\1/p'
}
cache_removed() {
  sed -n 's/.*"entries_removed":\([0-9][0-9]*\).*/\1/p'
}
cache_json_str() {
  sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p"
}
stub_log_mark() {
  docker logs "$STUB" 2>&1 | wc -l | tr -d ' '
}
stub_get_count_since() {
  mark="$1"
  pattern="$2"
  docker logs "$STUB" 2>&1 | tail -n +"$((mark + 1))" | grep -c "$pattern"
}
ok() { pass=$((pass+1)); printf '  ok  %s\n' "$1"; }
no() { fail=$((fail+1)); printf 'FAIL  %s\n' "$1"; }
expect_eq() { if [ "$2" = "$3" ]; then ok "$1"; else no "$1 (got '$2', want '$3')"; fi; }
expect_contains() {
  case "$2" in
    *"$3"*) ok "$1" ;;
    *) no "$1 (missing substring '$3' in '$2')" ;;
  esac
}
expect_security_headers() {
  label="$1"
  expect_eq "HSTS ($label)" "$(hval Strict-Transport-Security)" "max-age=31536000; includeSubDomains"
  expect_eq "X-Frame-Options ($label)" "$(hval X-Frame-Options)" "DENY"
  expect_eq "X-Content-Type-Options ($label)" "$(hval X-Content-Type-Options)" "nosniff"
  expect_eq "Referrer-Policy ($label)" "$(hval Referrer-Policy)" "no-referrer"
  expect_eq "X-Robots-Tag ($label)" "$(hval X-Robots-Tag)" "noindex, nofollow, noarchive, nosnippet, noimageindex"
}
expect_stub_gets_since() {
  label="$1"
  mark="$2"
  pattern="$3"
  want="$4"
  got=""
  for _ in $(seq 1 10); do
    got="$(stub_get_count_since "$mark" "$pattern")"
    [ "$got" = "$want" ] && break
    sleep 0.1
  done
  expect_eq "$label" "$got" "$want"
}

# 0. Runtime cachectl JSON escaping matches the host-side unit contract. This
# catches awk implementation drift between CI hosts and the container image.
special_replica="$(printf 'origin-a\tline\nnext\bback\fpage\abel\rcr')"
status_json=$(docker exec \
  -e CACHE_CONNECTOR_ID='stats"quoted\slash' \
  -e CACHE_REPLICA_ID="$special_replica" \
  "$ORIGIN" qurl-origin-cachectl status)
expect_contains "runtime status escapes connector metadata" "$status_json" '"connector_id":"stats\"quoted\\slash"'
expect_contains "runtime status escapes replica metadata" "$status_json" '"replica_id":"origin-a\tline\nnext\bback\fpage\u0007bel\rcr"'

# 1. root -> index
code=$(curl -s -o "$B" -w '%{http_code}' "$base/"); expect_eq "GET / status" "$code" 200
expect_eq "GET / body" "$(cat "$B")" "index"
docker exec "$ORIGIN" qurl-origin-cachectl purge /index.html >/dev/null
mark="$(stub_log_mark)"
curl -s -o /dev/null "$base/"
expect_stub_gets_since "targeted purge /index.html evicts root clean URL" "$mark" 'GET /index.html ' 1

# 2. extensionless -> /path/index.html, and the signer saw that exact path + auth
fetch -X GET "$base/website"
expect_eq "GET /website body" "$(cat "$B")" "website"
expect_eq "GET /website signed path" "$(hval X-Stub-Path)" "/website/index.html"
expect_eq "GET /website auth attached" "$(hval X-Stub-Authorization)" "present"
expect_eq "GET /website host rewritten" "$(hval X-Stub-Host)" "example-bucket.s3.us-east-1.amazonaws.com"
expect_eq "GET /website unsigned-payload" "$(hval X-Stub-Amz-Content-Sha256)" "UNSIGNED-PAYLOAD"
fetch -H "x-amz-meta-client: should-not-pass" "$base/styles/app.css"
expect_eq "client x-amz header stripped before signer" "$(hval X-Stub-Client-Amz-Meta)" "absent"

# 3. trailing slash -> index
fetch "$base/website/"; expect_eq "GET /website/ body" "$(cat "$B")" "website"

# 4. dot anywhere suppresses the rewrite
fetch "$base/v1.2/docs"
expect_eq "GET /v1.2/docs body" "$(cat "$B")" "docs"
expect_eq "GET /v1.2/docs signed path" "$(hval X-Stub-Path)" "/v1.2/docs"
fetch "$base/about."
expect_eq "GET /about. body" "$(cat "$B")" "trailing-dot"
expect_eq "GET /about. signed path" "$(hval X-Stub-Path)" "/about."

# 5. metrics.json with cache-buster: 200, passthrough headers, query stripped
fetch "$base/metrics.json?_t=1718000000000"
expect_eq "GET metrics?_t status" "$(curl -s -o /dev/null -w '%{http_code}' "$base/metrics.json?_t=9")" 200
expect_eq "GET metrics body" "$(cat "$B")" '{"ok":true}'
expect_eq "GET metrics Content-Type" "$(hval Content-Type)" "application/json"
expect_eq "GET metrics Cache-Control" "$(hval Cache-Control)" "max-age=300"

# 6. query excluded from cache key: two ?_t= GETs hit the stub once
mark="$(stub_log_mark)"
curl -s -o /dev/null "$base/cacheprobe.json?_t=1"
curl -s -o /dev/null "$base/cacheprobe.json?_t=2"
expect_stub_gets_since "cacheprobe upstream GETs (query excluded + cached)" "$mark" 'GET /cacheprobe.json ' 1

# 6b. deploy automation can target one local nginx cache path without evicting
# unrelated paths.
before_entries=$(cache_entries)
purge_report=$(docker exec "$ORIGIN" qurl-origin-cachectl purge /cacheprobe.json)
removed=$(printf '%s\n' "$purge_report" | cache_removed)
after_entries=$(cache_entries)
expect_eq "targeted purge removes cacheprobe file" "$removed" 1
expect_eq "targeted purge decrements cache file count" "$after_entries" $((before_entries - 1))
mark="$(stub_log_mark)"
curl -s -o /dev/null "$base/cacheprobe.json?_t=3"
expect_stub_gets_since "cacheprobe upstream GETs after targeted cache purge" "$mark" 'GET /cacheprobe.json ' 1
mark="$(stub_log_mark)"
curl -s -o /dev/null "$base/metrics.json?_t=10"
expect_stub_gets_since "metrics upstream GETs unaffected by targeted purge" "$mark" 'GET /metrics.json ' 0
purge_report=$(docker exec "$ORIGIN" qurl-origin-cachectl purge-connector other-connector /metrics.json 2>&1)
code=$?
expect_eq "connector purge rejects wrong connector" "$code" 3
case "$purge_report" in
  *"Refusing connector-scoped purge"*) ok "connector purge wrong-connector message" ;;
  *) no "connector purge wrong-connector message (got '$purge_report')" ;;
esac
mark="$(stub_log_mark)"
curl -s -o /dev/null "$base/metrics.json?_t=11"
expect_stub_gets_since "metrics upstream GETs unchanged after rejected connector purge" "$mark" 'GET /metrics.json ' 0
purge_report=$(docker exec "$ORIGIN" qurl-origin-cachectl purge-connector stats-connector /metrics.json)
expect_eq "connector purge reports scope" "$(printf '%s\n' "$purge_report" | cache_json_str scope)" "connector"
expect_eq "connector purge reports connector id" "$(printf '%s\n' "$purge_report" | cache_json_str connector_id)" "stats-connector"
expect_eq "connector purge reports replica id" "$(printf '%s\n' "$purge_report" | cache_json_str replica_id)" "origin-a"
removed=$(printf '%s\n' "$purge_report" | cache_removed)
expect_eq "connector purge removes metrics file" "$removed" 1
mark="$(stub_log_mark)"
curl -s -o /dev/null "$base/metrics.json?_t=12"
expect_stub_gets_since "metrics upstream GETs after connector purge" "$mark" 'GET /metrics.json ' 1

# 6c. No path arguments still purge the whole cache, matching the current
# CloudFront "/*" deploy invalidation shape.
purge_report=$(docker exec "$ORIGIN" qurl-origin-cachectl purge)
removed=$(printf '%s\n' "$purge_report" | cache_removed)
after_entries=$(cache_entries)
expect_eq "full purge leaves no cache files" "$after_entries" 0
if [ "$removed" -ge 1 ]; then
  ok "full purge removes at least one cache file"
else
  no "full purge removed '$removed' files, want >= 1"
fi
mark="$(stub_log_mark)"
curl -s -o /dev/null "$base/metrics.json?_t=13"
expect_stub_gets_since "metrics upstream GETs after full cache purge" "$mark" 'GET /metrics.json ' 1

# 6d. Missing keys are not negative-cached. OSS nginx file-based cache purges
# cannot reliably invalidate intercepted 404s from the shared cache zone, so
# new S3 keys must not be hidden behind an unpurgeable local negative cache.
mark="$(stub_log_mark)"
curl -s -o /dev/null "$base/future-object.json"
curl -s -o /dev/null "$base/future-object.json"
expect_stub_gets_since "missing key is not negative-cached" "$mark" 'GET /future-object.json ' 2

# 7. exact security header set on 200 and on 404
for path in "/" "/definitely-missing"; do
  fetch "$base$path"
  expect_security_headers "$path"
done

# 8. missing key -> clean 404 (no S3 XML leak)
fetch "$base/definitely-missing"
expect_eq "missing status" "$(curl -s -o /dev/null -w '%{http_code}' "$base/definitely-missing")" 404
expect_eq "missing body" "$(cat "$B")" "Not Found"

# 9. upstream 403 -> client 404 (no leak), but logged as upstream_status 403
code=$(curl -s -o /dev/null -w '%{http_code}' "$base/forbidden.json")
expect_eq "forbidden client status" "$code" 404
if docker logs "$ORIGIN" 2>&1 | grep -q '"upstream_status":"403"'; then ok "forbidden logged upstream_status 403"; else no "forbidden not logged as upstream 403"; fi

# 9b. other S3-side 4xx responses are also masked; clients must never see XML
# error bodies or distinguish malformed/denied/missing object states.
fetch "$base/badrequest.json"
expect_eq "badrequest client status" "$(status_code)" 404
expect_eq "badrequest body" "$(cat "$B")" "Not Found"
if docker logs "$ORIGIN" 2>&1 | grep -q '"upstream_status":"400"'; then ok "badrequest logged upstream_status 400"; else no "badrequest not logged as upstream 400"; fi

# 9c. throttle-class responses are retryable upstream failures, not missing
# objects, and still never leak S3 XML.
fetch "$base/throttle.json"
expect_eq "throttle status" "$(status_code)" 502
expect_eq "throttle body" "$(cat "$B")" "Bad Gateway"
if docker logs "$ORIGIN" 2>&1 | grep -q '"upstream_status":"429"'; then ok "throttle logged upstream_status 429"; else no "throttle not logged as upstream 429"; fi

# 9d. Every other upstream status is masked too, not just the handful the
# intercept list used to enumerate. The pinned pre-fix digest passes S3's 3xx
# and 501 responses straight through, so these ride the same waiver as the
# control-char contract below.
if [ "$waive_security_contract" != "true" ]; then
  # Slack only validates the shape of the region an admin types, so a mistyped
  # region reaches S3 as a wrong-region request and S3 answers 301
  # PermanentRedirect — a body naming the private bucket and its true region.
  fetch "$base/wrongregion.html"
  expect_eq "wrong-region 301 status" "$(status_code)" 404
  expect_eq "wrong-region 301 body" "$(cat "$B")" "Not Found"
  case "$(cat "$B")" in
    *example-private-site*|*eu-west-1*|*PermanentRedirect*)
      no "wrong-region 301 body discloses bucket or region" ;;
    *) ok "wrong-region 301 body discloses no bucket or region" ;;
  esac
  expect_eq "wrong-region 301 hides x-amz-bucket-region" "$(hval x-amz-bucket-region)" ""
  if docker logs "$ORIGIN" 2>&1 | grep -q '"upstream_status":"301"'; then ok "wrong-region logged upstream_status 301"; else no "wrong-region not logged as upstream 301"; fi

  # 501 sits outside every status family the intercept list used to name.
  fetch "$base/notimplemented.html"
  expect_eq "notimplemented status" "$(status_code)" 502
  expect_eq "notimplemented body" "$(cat "$B")" "Bad Gateway"
  if docker logs "$ORIGIN" 2>&1 | grep -q '"upstream_status":"501"'; then ok "notimplemented logged upstream_status 501"; else no "notimplemented not logged as upstream 501"; fi

  # 9e. A real object hit must not fingerprint the origin as S3 behind Envoy.
  # X-Stub-Path is the control: upstream headers do reach the viewer unless
  # they are explicitly hidden.
  fetch "$base/metrics.json"
  expect_eq "object hit forwards unhidden upstream headers" "$(hval X-Stub-Path)" "/metrics.json"
  for header in x-amz-request-id x-amz-id-2 x-amz-server-side-encryption \
                x-amz-version-id x-envoy-upstream-service-time; do
    expect_eq "object hit hides $header" "$(hval "$header")" ""
  done

  # The method guard answers with its own body, which is what keeps it out of
  # the intercept list; masking it to 404 would hide the 405 contract.
  fetch -X POST "$base/"
  expect_eq "POST / body is the origin's own text" "$(cat "$B")" "Method Not Allowed"
fi

# 10. upstream 5xx -> 502 Bad Gateway
fetch "$base/boom.json"
expect_eq "boom status" "$(status_code)" 502
expect_eq "boom body" "$(cat "$B")" "Bad Gateway"
expect_security_headers "/boom.json"

# 11. method not allowed
fetch -X POST "$base/"
expect_eq "POST / -> 405" "$(status_code)" 405
expect_security_headers "POST /"

# 12. HEAD -> headers, no body
hb=$(curl -s -I "$base/metrics.json" | tr -d '\r')
expect_eq "HEAD metrics status" "$(printf '%s' "$hb" | awk 'NR==1{print $2}')" 200
expect_eq "HEAD metrics Content-Type" "$(printf '%s\n' "$hb" | awk -F': ' 'tolower($1)=="content-type"{print $2}')" "application/json"

# 12b. The origin still answers a viewer's conditional GET itself: 304 is
# deliberately absent from the intercept list, so client caching keeps working.
expect_eq "conditional GET -> 304" \
  "$(curl -s -o /dev/null -w '%{http_code}' -H 'If-Modified-Since: Wed, 21 Oct 2026 07:28:00 GMT' "$base/metrics.json")" 304

# 13. Range. Viewer Range is not forwarded to S3, so nginx serves 206 while
# fetching the full 200 from S3 once; subsequent ranges are served from cache.
mark="$(stub_log_mark)"
code=$(curl -s -o "$B" -w '%{http_code}' -H 'Range: bytes=0-3' "$base/range.bin")
expect_eq "Range status (cold cache)" "$code" 206
expect_eq "Range body (cold cache)" "$(cat "$B")" "0123"
expect_stub_gets_since "Range upstream GETs after cold range" "$mark" 'GET /range.bin ' 1
mark="$(stub_log_mark)"
code=$(curl -s -o "$B" -w '%{http_code}' -H 'Range: bytes=4-6' "$base/range.bin")
expect_eq "Range status (cached different slice)" "$code" 206
expect_eq "Range body (cached different slice)" "$(cat "$B")" "456"
expect_stub_gets_since "Range upstream GETs after cached range" "$mark" 'GET /range.bin ' 0
# 416 is nginx's own range-filter response — S3 never sees the viewer's Range —
# so it is deliberately absent from the intercept list and must stay a 416.
expect_eq "unsatisfiable Range -> 416" \
  "$(curl -s -o /dev/null -w '%{http_code}' -H 'Range: bytes=9999-' "$base/range.bin")" 416

# 14. S3_PREFIX is joined with the clean-URL path at runtime.
docker rm -f "$ORIGIN" >/dev/null 2>&1
docker run -d --name "$ORIGIN" --network "$NET" -p 127.0.0.1::8080 \
  -e S3_BUCKET=example-bucket -e AWS_REGION=us-east-1 -e S3_PREFIX=site \
  -e CACHE_DEFAULT_TTL=60s \
  -e LISTEN_ADDR=0.0.0.0:8080 -e ALLOW_NON_LOOPBACK_LISTEN=true \
  -e ALLOW_PLAINTEXT_S3=true \
  -e CACHE_CONNECTOR_ID=stats-connector -e CACHE_REPLICA_ID=origin-a \
  -e S3_TLS=false -e S3_ENDPOINT_ADDR="$STUB" -e S3_ENDPOINT_PORT=9000 \
  -e AWS_ACCESS_KEY_ID=test -e AWS_SECRET_ACCESS_KEY=test \
  "$IMG" >/dev/null
base="$(origin_base_url)"

ready=0
for _ in $(seq 1 40); do
  if [ "$(curl -s -o /dev/null -w '%{http_code}' "$base/")" = "200" ]; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != "1" ]; then
  echo "PREFIX ORIGIN never became ready; logs:"; docker logs "$ORIGIN" 2>&1 | tail -40; exit 1
fi

fetch "$base/website"
expect_eq "S3_PREFIX /website body" "$(cat "$B")" "prefixed"
expect_eq "S3_PREFIX /website signed path" "$(hval X-Stub-Path)" "/site/website/index.html"
fetch "$base/"
expect_eq "S3_PREFIX / body" "$(cat "$B")" "prefixed-index"
expect_eq "S3_PREFIX / signed path" "$(hval X-Stub-Path)" "/site/index.html"
mark="$(stub_log_mark)"
curl -s -o /dev/null "$base/website"
expect_stub_gets_since "CACHE_DEFAULT_TTL caches metadata-less object" "$mark" 'GET /site/website/index.html ' 0

if [ "$waive_security_contract" = "true" ]; then
  message="Known pre-fix S3 origin digest remains pinned by Slack; the control-char and upstream-response assertions are waived only for this exact immutable image. Rotate the pin after PRs #1158 and #1231 publish, then remove S3_ORIGIN_SECURITY_WAIVER_IMAGE."
  if [ "${GITHUB_ACTIONS:-false}" = "true" ]; then
    printf '::warning title=S3 origin security pin pending rotation::%s\n' "$message"
  else
    printf 'WARNING: %s\n' "$message" >&2
  fi
else
  # A viewer path carrying percent-encoded CR/LF must not splice a second request
  # past S3_PREFIX into the signer hop. styles/app.css exists only at the bucket
  # root; under S3_PREFIX=site a legit request signs /site/styles/app.css, so any
  # upstream GET for the bare /styles/app.css means the prefix boundary escaped.
  mark="$(stub_log_mark)"
  curl -s --path-as-is -o /dev/null \
    "$base/q%20HTTP/1.1%0d%0aHost:h%0d%0a%0d%0aGET%20/styles/app.css"
  expect_stub_gets_since "CRLF request-splitting cannot escape S3_PREFIX" "$mark" 'GET /styles/app.css ' 0
  expect_eq "control-char viewer path is rejected, not proxied" \
    "$(curl -s --path-as-is -o /dev/null -w '%{http_code}' "$base/a%0d%0ab.html")" 404
fi

# 15. The entrypoint supervisor exits the container if either child dies.
for child in envoy nginx; do
  docker rm -f "$ORIGIN" >/dev/null 2>&1
  docker run -d --name "$ORIGIN" --network "$NET" -p 127.0.0.1::8080 \
    -e S3_BUCKET=example-bucket -e AWS_REGION=us-east-1 \
    -e LISTEN_ADDR=0.0.0.0:8080 -e ALLOW_NON_LOOPBACK_LISTEN=true \
    -e ALLOW_PLAINTEXT_S3=true \
    -e S3_TLS=false -e S3_ENDPOINT_ADDR="$STUB" -e S3_ENDPOINT_PORT=9000 \
    -e AWS_ACCESS_KEY_ID=test -e AWS_SECRET_ACCESS_KEY=test \
    "$IMG" >/dev/null

  child_ready=0
  for _ in $(seq 1 40); do
    if [ "$(curl -s -o /dev/null -w '%{http_code}' "$(origin_base_url)/")" = "200" ]; then child_ready=1; break; fi
    sleep 1
  done
  if [ "$child_ready" != "1" ]; then
    no "supervisor $child test origin became ready"
    docker logs "$ORIGIN" 2>&1 | tail -40
    continue
  fi

  docker exec "$ORIGIN" sh -c '
  target="$1"
  found=0
  for comm in /proc/[0-9]*/comm; do
    name="$(cat "$comm" 2>/dev/null || true)"
    if [ "$name" = "$target" ]; then
      pid="${comm%/comm}"
      pid="${pid##*/}"
      kill -KILL "$pid"
      found=1
      break
    fi
  done
  [ "$found" = "1" ]
  ' sh "$child"

  stopped=0
  for _ in $(seq 1 20); do
    state="$(docker inspect -f '{{.State.Running}} {{.State.ExitCode}}' "$ORIGIN" 2>/dev/null || true)"
    case "$state" in
      false\ *) stopped=1; break ;;
    esac
    sleep 0.5
  done
  if [ "$stopped" = "1" ]; then
    ok "supervisor stops container after $child exits"
  else
    no "supervisor stops container after $child exits (state '$state')"
  fi
  exit_code="$(docker inspect -f '{{.State.ExitCode}}' "$ORIGIN" 2>/dev/null || echo 0)"
  if [ "$exit_code" != "0" ]; then
    ok "supervisor exits non-zero after $child crash"
  else
    no "supervisor exits non-zero after $child crash"
  fi
done

echo "-------------------------------------------"
echo "behavior: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
