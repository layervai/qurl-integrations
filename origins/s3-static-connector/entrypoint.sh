#!/usr/bin/env bash
# PID 1 payload (under tini) for s3-static-connector. Renders configs from the
# environment, then runs nginx + Envoy together; if either process exits, the
# container exits non-zero so the orchestrator/systemd restart is a clean,
# observable event (the OriginRestart alarm keys on the startup marker below).
set -Eeuo pipefail

RENDER_DIR="${RENDER_DIR:-/etc/qurl/rendered}"
export RENDER_DIR

# Resolve AWS_REGION from IMDSv2 when not supplied (EC2). Envoy needs a concrete
# region for the S3 endpoint host and SigV4. Our CDK deployment always passes
# AWS_REGION; this is the convenience fallback the env contract documents.
if [ -z "${AWS_REGION:-}" ] && [ -n "${AWS_DEFAULT_REGION:-}" ]; then
  AWS_REGION="$AWS_DEFAULT_REGION"
fi
if [ -z "${AWS_REGION:-}" ]; then
  _tok="$(curl -fsS -m 2 -X PUT 'http://169.254.169.254/latest/api/token' \
    -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' 2>/dev/null || true)"
  if [ -n "$_tok" ]; then
    AWS_REGION="$(curl -fsS -m 2 -H "X-aws-ec2-metadata-token: $_tok" \
      'http://169.254.169.254/latest/meta-data/placement/region' 2>/dev/null || true)"
  fi
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
