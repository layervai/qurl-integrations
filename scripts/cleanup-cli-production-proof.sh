#!/usr/bin/env bash
set -euo pipefail
[[ $# == 2 && $1 =~ ^[1-9][0-9]{0,17}$ && $2 =~ ^[1-9][0-9]?$ ]] && (( $2 <= 30 )) || exit 2
[[ -n "${AUTOMATION_API_KEY:-}" && -n "${QURL_PRODUCTION_OWNER_ID:-}" ]] || {
  echo '::error::production cleanup credentials are missing' >&2
  exit 1
}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
(umask 077; printf %s "$AUTOMATION_API_KEY" >"$work/key")
unset AUTOMATION_API_KEY
specs=()
for ((attempt = 1; attempt <= $2; attempt++)); do
  for lane in linux macos windows; do
    case "$lane" in linux) suffix=1 ;; macos) suffix=2 ;; windows) suffix=3 ;; esac
    specs+=(--run-spec "$1$suffix:$attempt:$lane:host:production")
  done
done
status=0
# The existing controller accepts at most twelve run identities per inventory.
for ((offset = 0; offset < ${#specs[@]}; offset += 24)); do
  python3 scripts/qurl-cli-ci-credentials.py reconcile-batch \
    --qurl-endpoint https://api.layerv.ai --api-key-file "$work/key" \
    --owner-id "$QURL_PRODUCTION_OWNER_ID" --operation-budget-seconds 900 \
    "${specs[@]:offset:24}" || status=1
done
exit "$status"
