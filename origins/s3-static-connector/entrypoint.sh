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

# Credential preflight. Envoy signs the S3 hop with the AWS default credential
# provider chain; when that chain comes up empty (a non-EC2 Docker host has no
# instance role) or the credential has expired, S3 rejects every signed request
# and nginx deliberately masks that to a plain 404 so viewers cannot tell a
# denied key from a missing one. The origin then looks healthy while serving
# nothing and the operator debugs the bucket key instead of the credential.
# Probe the index object through the signer once, before nginx starts, and
# refuse to serve when S3 rejects the signature: that class never self-heals.
# A missing object, a throttle, an upstream 5xx and an unreachable bucket stay
# warnings — they either resolve on their own or already reach viewers as a 502,
# and failing the boot on those would burn the restart budget on a transient S3
# blip. README "AWS credentials" documents both outcomes.
preflight_key="${S3_PREFIX_NORMALIZED}/${INDEX_DOCUMENT}"
preflight_remediation="Give the origin AWS credentials for s3://${S3_BUCKET}${preflight_key} (EC2 instance role with IMDSv2 hop-limit 2, ECS task role, EKS pod identity, or AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY + AWS_SESSION_TOKEN in the container environment) and grant that identity s3:GetObject on the served keys plus s3:ListBucket on the bucket."

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
preflight_deadline=$((SECONDS + 15))
while :; do
  preflight_rc=0
  preflight_line="$(probe_signer)" || preflight_rc=$?
  [ "$preflight_rc" -eq 0 ] && break
  preflight_line=""
  # Only "signer not listening yet" is worth retrying; the read timeout below
  # already bounds a signer that accepts the connection and then stalls.
  if [ "$preflight_rc" -ne 2 ] ||
    ! kill -0 "$envoy_pid" 2>/dev/null ||
    [ "$SECONDS" -ge "$preflight_deadline" ]; then
    break
  fi
  sleep 0.5
done

preflight_status="${preflight_line#* }"
preflight_status="${preflight_status%% *}"
case "$preflight_status" in
  [0-9][0-9][0-9]) ;;
  *) preflight_status="" ;;
esac

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
    preflight_log preflight_ok "Signed S3 request succeeded."
    ;;
  400|401|403)
    # 403 is AccessDenied/InvalidAccessKeyId/SignatureDoesNotMatch; 400 is the
    # ExpiredToken/InvalidToken class. Both mean the signature was rejected: with
    # the required s3:ListBucket grant a genuinely missing object comes back 404
    # instead, so neither status can be an absent key. Without that grant a
    # missing object is a 403 too, which this correctly reports as a policy fix.
    preflight_log preflight_auth_failed "S3 rejected the signed request. $preflight_remediation" >&2
    term
    wait_children
    exit 1
    ;;
  404)
    preflight_log preflight_object_missing "Credentials work; this object is not in the bucket yet. Serving anyway." >&2
    ;;
  "")
    preflight_log preflight_no_response "Could not reach the signer or S3 before the deadline. Serving anyway; requests will fail 502 while it stays unreachable." >&2
    ;;
  *)
    preflight_log preflight_upstream_error "Unexpected status from the S3 hop; 503 means the signer could not reach the bucket endpoint. Serving anyway; requests fail 502 while it lasts." >&2
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
