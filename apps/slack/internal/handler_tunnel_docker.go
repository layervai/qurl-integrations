package internal

import (
	"fmt"

	"github.com/layervai/qurl-integrations/shared/client"
)

func renderDockerTunnelInstructions(args *tunnelInstallArgs, _ *client.APIKey, image string) string {
	webContainer := shellSingleQuote("YOUR_WEB_CONTAINER_NAME")
	if args.WebContainer != "" {
		webContainer = shellSingleQuote(args.WebContainer)
	}
	docker := fmt.Sprintf(`set -eu

if [ "$(id -u)" -eq 0 ]; then
  SUDO=""
elif command -v sudo >/dev/null 2>&1; then
  SUDO="sudo"
else
  echo "Run as root or install sudo so the state and secret directories can be owned by UID 65532." >&2
  exit 1
fi

# Keep this placeholder assignment so the block is pasteable; the guard below
# fails before writing files until the operator replaces it.
WEB_CONTAINER=%s
if [ "$WEB_CONTAINER" = "YOUR_WEB_CONTAINER_NAME" ] || [ -z "$WEB_CONTAINER" ]; then
  echo "Set WEB_CONTAINER to the Docker container name or ID for your local HTTP server." >&2
  exit 1
fi

QURL_TUNNEL_SLUG=%s
TUNNEL_CONTAINER="qurl-tunnel-${QURL_TUNNEL_SLUG}"
SECRET_DIR="/run/secrets/qurl-tunnel/${QURL_TUNNEL_SLUG}"
AGENT_STATE_DIR="/var/lib/layerv/qurl-tunnel/${QURL_TUNNEL_SLUG}/agent"
CONFIG_FILE="$PWD/qurl-proxy-${QURL_TUNNEL_SLUG}.yaml"

# This intentionally overwrites the per-slug config so rerunning the install
# refreshes the deterministic slug/port values in place.
cat > "$CONFIG_FILE" <<'QURL_PROXY_YAML_EOF'
%s
QURL_PROXY_YAML_EOF

$SUDO install -d -m 0700 -o 65532 -g 65532 "$SECRET_DIR"
$SUDO install -d -m 0700 -o 65532 -g 65532 "$AGENT_STATE_DIR"
%s
printf '%%s' "$QURL_BOOTSTRAP_KEY" | $SUDO install -m 0400 -o 65532 -g 65532 /dev/stdin "$SECRET_DIR/api_key"
unset QURL_BOOTSTRAP_KEY

if docker ps -a --format '{{.Names}}' | grep -Fxq "$TUNNEL_CONTAINER"; then
  docker rm -f "$TUNNEL_CONTAINER" >/dev/null
fi

docker run -d \
  --name "$TUNNEL_CONTAINER" \
  --network "container:${WEB_CONTAINER}" \
  --restart=on-failure:5 \
  -v "$AGENT_STATE_DIR:/var/lib/layerv/agent" \
  -v "$SECRET_DIR:$SECRET_DIR:ro" \
  -v "$CONFIG_FILE:/work/qurl-proxy.yaml:ro" \
  -e QURL_API_KEY_FILE="$SECRET_DIR/api_key" \
  -e QURL_TUNNEL_SLUG="$QURL_TUNNEL_SLUG" \
  %s`, webContainer, shellSingleQuote(args.Slug), renderTunnelConfigYAML(args), renderBootstrapKeyPromptShell(), shellSingleQuote(image))

	intro := "Run this whole block on the Linux Docker host where your local HTTP server container is running. It prompts for the bootstrap key so the secret does not land in shell history."
	if args.WebContainer == "" {
		intro += " Replace `YOUR_WEB_CONTAINER_NAME` first."
	}
	intro += " It writes or overwrites a per-slug qurl-proxy config in the current directory."
	return intro + "\n\n" + slackCodeBlock(docker) + "\n\nVerify with `docker logs -f qurl-tunnel-" + args.Slug + "`; after the tunnel connects, delete the bootstrap key file."
}
