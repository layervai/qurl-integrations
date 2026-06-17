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
export AWS_REGION

mkdir -p /tmp/s3cache /tmp/client_body /tmp/proxy_temp

# Render nginx.conf + envoy.yaml (fails fast on missing S3_BUCKET / AWS_REGION).
. /usr/local/bin/render.sh

# Startup marker for the OriginRestart metric filter ($.msg == "origin_started").
printf '{"layer":"origin","msg":"origin_started"}\n'

/usr/local/bin/envoy -c "${RENDER_DIR}/envoy.yaml" \
  --log-format '{"layer":"envoy","level":"%l","name":"%n","message":"%j"}' &
envoy_pid=$!

/usr/sbin/nginx -c "${RENDER_DIR}/nginx.conf" -g 'daemon off;' &
nginx_pid=$!

term() { kill -TERM "$envoy_pid" "$nginx_pid" 2>/dev/null || true; }
shutdown() {
  term
  wait "$envoy_pid" "$nginx_pid" 2>/dev/null || true
  exit 143
}
trap shutdown TERM INT

# Supervisor: `wait -n` reaps the first exited child. A `kill -0` poll loop can
# miss zombies under dash, which would leave the surviving process running.
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
wait "$envoy_pid" "$nginx_pid" 2>/dev/null || true
if [ "$exit_code" -eq 0 ]; then
  exit 1
fi
exit "$exit_code"
