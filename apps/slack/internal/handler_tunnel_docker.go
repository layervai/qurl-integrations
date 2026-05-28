package internal

import (
	"fmt"
)

func renderDockerTunnelInstructions(args *tunnelInstallArgs, image string) (string, error) {
	webContainer := shellSingleQuote("YOUR_WEB_CONTAINER_NAME")
	if args.WebRef != "" {
		webContainer = shellSingleQuote(args.WebRef)
	}
	configYAML, err := renderTunnelConfigYAML(args)
	if err != nil {
		return "", err
	}
	docker := fmt.Sprintf(`set -eu
%s

%s

# Keep this placeholder assignment so the block is pasteable; the guard below
# fails before writing files until the operator replaces the quoted value.
WEB_CONTAINER=%s
%s

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
%s

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
  %s`, renderPortablePipefailShell(), renderSudoDetectionShell(), webContainer, renderRequiredShellNameGuard("WEB_CONTAINER", "YOUR_WEB_CONTAINER_NAME", "the Docker container name or ID for your local HTTP server", "A-Za-z0-9_.-", "letters, numbers, dots, underscores, and hyphens"), shellSingleQuote(args.Slug), configYAML, renderBootstrapKeyPromptShell(), renderBootstrapKeyFileInstallShell(`"$SECRET_DIR/api_key"`), shellSingleQuote(image))

	block, err := slackCodeBlock(docker)
	if err != nil {
		return "", err
	}
	intro := "Run this whole block on the Linux Docker host where your local HTTP server container is running. It prompts for the bootstrap key so the secret does not land in shell history; use a trusted host and shell because local administrators can inspect process state during setup. If your terminal echoes pasted input, stop and use a platform secret manager instead."
	if args.WebRef == "" {
		intro += " Replace the value inside `WEB_CONTAINER='YOUR_WEB_CONTAINER_NAME'` first; keep the quotes."
	}
	intro += " It writes or overwrites a per-slug qurl-proxy config in the current directory. Re-running this install briefly restarts the tunnel container if it already exists. Because the tunnel shares the web container's network namespace, restart the tunnel after replacing or recreating the web container."
	return intro + "\n\n" + block + "\n\nVerify with `docker logs -f qurl-tunnel-" + args.Slug + "`; after the tunnel connects, delete the bootstrap key file.", nil
}
