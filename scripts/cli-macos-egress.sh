#!/usr/bin/env bash
# Give native UDP admission and TCP traffic the same public source address.
set -euo pipefail
[[ $(uname -s) == Darwin ]] || { echo 'macOS is required' >&2; exit 1; }
config=/var/run/qurl-ci-egress/qci.conf
case ${1:-} in
  up)
    [[ -n ${QURL_JOURNEY_WIREGUARD_CONFIG:-} ]] || {
      echo '::error::protected macOS WireGuard configuration is missing' >&2
      exit 1
    }
    brew install wireguard-tools wireguard-go
    sudo install -d -m 0700 /var/run/qurl-ci-egress
    printf '%s\n' "$QURL_JOURNEY_WIREGUARD_CONFIG" | sudo tee "$config" >/dev/null
    unset QURL_JOURNEY_WIREGUARD_CONFIG
    sudo chmod 0600 "$config"
    sudo env "PATH=$PATH" wg-quick up "$config"
    # Teardown needs the routing configuration, but not the private key.
    sudo sed -i '' '/^PrivateKey = /d' "$config"
    expected=$(sudo awk '/^Endpoint = / {split($3, endpoint, ":"); print endpoint[1]}' "$config")
    actual=$(curl --fail --silent --show-error --max-time 15 https://checkip.amazonaws.com)
    [[ -n "$expected" && "$actual" == "$expected" ]] || {
      echo '::error::macOS traffic did not use the configured CI gateway' >&2
      exit 1
    }
    echo "macOS customer journey egress: $actual"
    ;;
  down)
    if sudo test -f "$config"; then
      result=0
      sudo env "PATH=$PATH" wg-quick down "$config" || result=$?
      sudo rm -f "$config"
      sudo rmdir /var/run/qurl-ci-egress
      exit "$result"
    fi
    ;;
  *) echo 'usage: cli-macos-egress.sh up|down' >&2; exit 2 ;;
esac
