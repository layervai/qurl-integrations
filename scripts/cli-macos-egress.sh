#!/usr/bin/env bash
# Give native UDP admission and TCP traffic the same public source address.
set -euo pipefail
[[ $(uname -s) == Darwin ]] || { echo 'macOS is required' >&2; exit 1; }
config=/var/run/qurl-ci-egress/qci.conf
private_key_pattern='^[[:space:]]*[Pp][Rr][Ii][Vv][Aa][Tt][Ee][Kk][Ee][Yy][[:space:]]*='
case ${1:-} in
  up)
    [[ -n ${QURL_JOURNEY_WIREGUARD_CONFIG:-} ]] || {
      echo '::error::protected macOS WireGuard configuration is missing' >&2
      exit 1
    }
    sudo install -d -m 0700 /var/run/qurl-ci-egress
    printf '%s\n' "$QURL_JOURNEY_WIREGUARD_CONFIG" | sudo tee "$config" >/dev/null
    unset QURL_JOURNEY_WIREGUARD_CONFIG
    sudo chmod 0600 "$config"
    # Remove the private key even when package setup or tunnel startup fails.
    trap 'sudo sed -i "" -E "/$private_key_pattern/d" "$config"' EXIT
    brew install wireguard-tools wireguard-go
    sudo env "PATH=$PATH" wg-quick up "$config"
    # Teardown needs the routing configuration, but not the private key.
    sudo sed -i '' -E "/$private_key_pattern/d" "$config"
    # TODO(upstream-contract): the provisioner emits one IPv4 Endpoint with
    # full-tunnel AllowedIPs; that same single-interface EIP is the SNAT source.
    expected=$(sudo awk '/^Endpoint = / {split($3, endpoint, ":"); print endpoint[1]}' "$config")
    actual=$(curl --fail --silent --show-error --retry 2 --retry-all-errors --max-time 15 https://checkip.amazonaws.com)
    [[ -n "$expected" && "$actual" == "$expected" ]] || {
      echo '::error::macOS traffic did not use the configured CI gateway' >&2
      exit 1
    }
    echo "macOS customer journey egress: $actual"
    ;;
  down)
    if sudo test -f "$config"; then
      result=0
      # TODO(upstream-contract): Darwin wg-quick creates this interface map.
      if sudo test -f /var/run/wireguard/qci.name; then
        sudo env "PATH=$PATH" wg-quick down "$config" || result=$?
      fi
      # Retain routing information if teardown fails so cleanup can be retried.
      sudo sed -i '' -E "/$private_key_pattern/d" "$config"
      ((result == 0)) || exit "$result"
      sudo rm -f "$config"
      sudo rmdir /var/run/qurl-ci-egress
      exit "$result"
    fi
    ;;
  *) echo 'usage: cli-macos-egress.sh up|down' >&2; exit 2 ;;
esac
