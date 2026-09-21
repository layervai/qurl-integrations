#!/usr/bin/env bash
# Exercise the real helper with local command substitutes and temporary paths.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export EGRESS_TEST_ROOT="$work"
mkdir "$work/bin"
sed "s|/var/run/|$work/|g" "$root/scripts/cli-macos-egress.sh" >"$work/helper.sh"
cat >"$work/bin/sudo" <<'STUB'
#!/usr/bin/env bash
# Match BSD sed's empty backup suffix when this test runs on Linux.
if [[ $1 == sed && $(/usr/bin/uname -s) == Linux ]]; then
  shift 3
  exec sed -i "$@"
fi
exec "$@"
STUB
cat >"$work/bin/uname" <<'STUB'
#!/bin/sh
echo Darwin
STUB
cat >"$work/bin/brew" <<'STUB'
#!/bin/sh
[ -z "${QURL_JOURNEY_WIREGUARD_CONFIG:-}" ] || exit 99
exit "${BREW_EXIT:-0}"
STUB
cat >"$work/bin/curl" <<'STUB'
#!/bin/sh
echo "${OBSERVED_IP:-203.0.113.10}"
STUB
cat >"$work/bin/wg-quick" <<'STUB'
#!/bin/sh
[ -z "${QURL_JOURNEY_WIREGUARD_CONFIG:-}" ] || exit 99
if [ "$1" = up ]; then
  mkdir -p "$EGRESS_TEST_ROOT/wireguard"
  touch "$EGRESS_TEST_ROOT/wireguard/qci.name"
else
  [ "${DOWN_FAIL:-0}" = 0 ] || exit 7
  rm "$EGRESS_TEST_ROOT/wireguard/qci.name"
fi
STUB
chmod +x "$work/bin/"*
export PATH="$work/bin:$PATH"
config="$work/qurl-ci-egress/qci.conf"
for key in 'PrivateKey = fake-key' 'PrivateKey=fake-key' '  privatekey = fake-key'; do
  export QURL_JOURNEY_WIREGUARD_CONFIG="$key
Endpoint = 203.0.113.10:51820"
  bash "$work/helper.sh" up >/dev/null
  if grep -qi privatekey "$config"; then exit 1; fi
  if DOWN_FAIL=1 env -u QURL_JOURNEY_WIREGUARD_CONFIG bash "$work/helper.sh" down; then exit 1; fi
  [[ -f "$config" ]]
  env -u QURL_JOURNEY_WIREGUARD_CONFIG bash "$work/helper.sh" down
  [[ ! -e "$config" ]]
done
if BREW_EXIT=8 bash "$work/helper.sh" up; then exit 1; fi
if grep -qi privatekey "$config"; then exit 1; fi
env -u QURL_JOURNEY_WIREGUARD_CONFIG bash "$work/helper.sh" down
[[ ! -e "$config" ]]
if OBSERVED_IP=203.0.113.11 bash "$work/helper.sh" up 2>/dev/null; then exit 1; fi
if grep -qi privatekey "$config"; then exit 1; fi
env -u QURL_JOURNEY_WIREGUARD_CONFIG bash "$work/helper.sh" down
echo 'macOS egress setup, failure cleanup, and teardown retry: PASS'
