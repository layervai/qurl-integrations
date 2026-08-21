#!/usr/bin/env bash
# PID 1 payload (under tini) for s3-static-connector. Renders configs from the
# environment, then runs nginx + Envoy together; if either process exits, the
# container exits non-zero so the orchestrator/systemd restart is a clean,
# observable event (the OriginRestart alarm keys on the startup marker below).
set -Eeuo pipefail

if (( BASH_VERSINFO[0] < 5 || (BASH_VERSINFO[0] == 5 && BASH_VERSINFO[1] < 1) )); then
  echo '{"layer":"origin","msg":"unsupported_bash","required":"5.1"}' >&2
  exit 1
fi

RENDER_DIR="${RENDER_DIR:-/etc/qurl/rendered}"
export RENDER_DIR

# Envoy needs a concrete region for the S3 endpoint host and SigV4. Prefer
# AWS_REGION, but accept AWS_DEFAULT_REGION for local/provider parity.
if [ -z "${AWS_REGION:-}" ] && [ -n "${AWS_DEFAULT_REGION:-}" ]; then
  AWS_REGION="$AWS_DEFAULT_REGION"
fi
if [ -n "${AWS_REGION:-}" ] && [ -z "${AWS_DEFAULT_REGION:-}" ]; then
  AWS_DEFAULT_REGION="$AWS_REGION"
fi
if [ -n "${AWS_REGION:-}" ]; then
  export AWS_REGION
else
  unset AWS_REGION
fi
if [ -n "${AWS_DEFAULT_REGION:-}" ]; then
  export AWS_DEFAULT_REGION
else
  unset AWS_DEFAULT_REGION
fi

mkdir -p /tmp/s3cache /tmp/client_body /tmp/proxy_temp /tmp/fastcgi_temp /tmp/uwsgi_temp /tmp/scgi_temp

# Render nginx.conf + envoy.yaml (fails fast on missing S3_BUCKET / AWS_REGION).
. /usr/local/bin/render.sh

# Startup marker for the OriginRestart metric filter ($.msg == "origin_started").
printf '{"layer":"origin","msg":"origin_started"}\n'

envoy_pid=""
nginx_pid=""
term() {
  [ -n "$envoy_pid" ] && kill -TERM "$envoy_pid" 2>/dev/null || true
  [ -n "$nginx_pid" ] && kill -TERM "$nginx_pid" 2>/dev/null || true
}
wait_children() {
  [ -n "$envoy_pid" ] && wait "$envoy_pid" 2>/dev/null || true
  [ -n "$nginx_pid" ] && wait "$nginx_pid" 2>/dev/null || true
}
shutdown() {
  term
  wait_children
  exit 143
}
trap shutdown TERM INT

/usr/local/bin/envoy -c "${RENDER_DIR}/envoy.yaml" \
  --log-format '{"layer":"envoy","level":"%l","name":"%n","message":"%j"}' &
envoy_pid=$!

# Request preflight. Envoy signs the S3 hop with the AWS default credential
# provider chain. S3 can reject that request because of credentials, IAM,
# region/endpoint selection, or other signed-request configuration, while nginx
# deliberately masks the rejection to a plain 404 so viewers cannot distinguish
# it from a missing key. The origin would then look healthy while serving
# nothing and the operator would debug the object path instead of the request.
# Probe the index object through the signer once, before nginx starts, and
# refuse to serve when S3 rejects the request: that class needs operator action.
# A missing object, a throttle, an upstream 5xx and an unreachable bucket stay
# warnings — they either resolve on their own or already reach viewers as a 502,
# and failing the boot on those would burn the restart budget on a transient S3
# blip. README "AWS credentials" documents both outcomes.
preflight_key="${S3_PREFIX_NORMALIZED}/${INDEX_DOCUMENT}"
preflight_remediation="Verify the credentials/provider chain, IAM permissions (s3:GetObject on the served keys and s3:ListBucket on the bucket), AWS_REGION, bucket/endpoint, and signed-request configuration for s3://${S3_BUCKET}${preflight_key}."

# Status line of a HEAD sent through the signer; exit 2 while it is not
# listening yet, exit 1 once it is but the S3 hop produced no response.
probe_signer() {
  (
    exec 3<>"/dev/tcp/${ENVOY_LISTEN_HOST}/${ENVOY_LISTEN_PORT}" || exit 2
    printf 'HEAD %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n' \
      "$preflight_key" "$S3_HOST" >&3 || exit 1
    IFS= read -r -t 10 status_line <&3 || exit 1
    printf '%s\n' "$status_line"
  ) 2>/dev/null
}

preflight_line=""
preflight_rc=0
preflight_status=""
preflight_backoff=""
preflight_deadline=$((SECONDS + 15))
while :; do
  preflight_rc=0
  preflight_line="$(probe_signer)" || preflight_rc=$?
  [ "$preflight_rc" -eq 0 ] || preflight_line=""
  preflight_status="${preflight_line#* }"
  preflight_status="${preflight_status%% *}"
  # A status line with no reason phrase ends at the code with the CR still
  # attached; strip it so a real rejection cannot fail open to "no response".
  preflight_status="${preflight_status%$'\r'}"
  case "$preflight_status" in
    [0-9][0-9][0-9]) ;;
    *) preflight_status="" ;;
  esac
  # Two outcomes earn another attempt while the deadline holds. Exit 2 is the
  # signer not listening yet; the read timeout above already bounds one that
  # accepts the connection and then stalls. The fatal class earns it because
  # credential acquisition fails as a 403 — IMDS throttled in a host boot
  # storm, an STS/web-identity hiccup, IAM or bucket-policy propagation, a
  # session token mid-refresh — and the rendered installs run this under
  # `restart: on-failure:5`, so exiting on the first one spends the whole
  # restart budget on a condition that clears itself. Genuinely wrong
  # bucket/region/IAM still fails closed, one deadline later.
  preflight_backoff=""
  case "$preflight_status" in
    "") if [ "$preflight_rc" -eq 2 ]; then preflight_backoff=0.5; fi ;;
    304|404|429) ;;
    3??|4??) preflight_backoff=5 ;;
  esac
  if [ -z "$preflight_backoff" ] ||
    ! kill -0 "$envoy_pid" 2>/dev/null ||
    [ "$SECONDS" -ge "$preflight_deadline" ]; then
    break
  fi
  sleep "$preflight_backoff"
done

# S3_BUCKET, S3_PREFIX, and INDEX_DOCUMENT are already restricted by render.sh
# to characters that need no JSON escaping, so these values interpolate as-is.
preflight_log() {
  if [ -n "$preflight_status" ]; then
    printf '{"layer":"origin","msg":"%s","status":%s,"key":"%s","detail":"%s"}\n' \
      "$1" "$preflight_status" "$preflight_key" "$2"
  else
    printf '{"layer":"origin","msg":"%s","key":"%s","detail":"%s"}\n' \
      "$1" "$preflight_key" "$2"
  fi
}

case "$preflight_status" in
  2??|304)
    preflight_log preflight_ok "S3 request succeeded."
    ;;
  404)
    preflight_log preflight_object_missing "S3 returned 404 for the requested index object. It may not be synced yet; this status does not prove which identity or permissions handled the request. Serving anyway." >&2
    ;;
  429|5??)
    preflight_log preflight_upstream_error "S3 returned a transient throttle or server error. Serving anyway; requests fail 502 while it lasts." >&2
    ;;
  3??|4??)
    # Status alone cannot distinguish credentials from IAM, wrong region,
    # endpoint, or other request configuration. It does establish that S3
    # rejected the probe, which nginx would otherwise mask to a viewer 404.
    preflight_log preflight_request_rejected "S3 rejected the request. $preflight_remediation" >&2
    term
    wait_children
    exit 1
    ;;
  "")
    preflight_log preflight_no_response "Could not reach the signer or S3 before the deadline. Serving anyway; requests will fail 502 while it stays unreachable." >&2
    ;;
  *)
    preflight_log preflight_upstream_error "Unexpected status from the S3 hop. Serving anyway; inspect the upstream status and origin logs." >&2
    ;;
esac

/usr/sbin/nginx -c "${RENDER_DIR}/nginx.conf" -g 'daemon off;' &
nginx_pid=$!

# Supervisor: `wait -n` reaps the first exited child. A `kill -0` poll loop can
# miss zombies under dash; PID-scoped `wait -n` requires bash >= 5.1 (bookworm has 5.2).
set +e
wait -n "$envoy_pid" "$nginx_pid"
exit_code=$?
set -e

if ! kill -0 "$envoy_pid" 2>/dev/null; then
  echo '{"layer":"origin","msg":"envoy_exited"}' >&2
fi
if ! kill -0 "$nginx_pid" 2>/dev/null; then
  echo '{"layer":"origin","msg":"nginx_exited"}' >&2
fi

term
wait_children
if [ "$exit_code" -eq 0 ]; then
  exit 1
fi
exit "$exit_code"
