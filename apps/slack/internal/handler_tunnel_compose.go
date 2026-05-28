package internal

import (
	"fmt"

	"github.com/layervai/qurl-integrations/shared/client"
)

func renderDockerComposeTunnelInstructions(args *tunnelInstallArgs, _ *client.APIKey, image string) string {
	webService := shellSingleQuote("YOUR_COMPOSE_SERVICE_NAME")
	if args.WebRef != "" {
		webService = shellSingleQuote(args.WebRef)
	}
	tunnelServiceName := "qurl-tunnel-" + args.Slug
	tunnelService := shellSingleQuote(tunnelServiceName)
	// SECURITY: The Compose heredoc below is intentionally unquoted so it can
	// write concrete paths and service names into the generated file. Keep
	// dockerComposeServicePattern narrow: it rejects shell metacharacters such
	// as '$', backticks, quotes, slashes, and whitespace.
	compose := fmt.Sprintf(`set -eu

if [ "$(id -u)" -eq 0 ]; then
  SUDO=""
elif command -v sudo >/dev/null 2>&1; then
  SUDO="sudo"
else
  echo "Run as root or install sudo so the state and secret directories can be owned by UID 65532." >&2
  exit 1
fi

# Run from the directory with your existing Compose file.
APP_COMPOSE_FILE=${APP_COMPOSE_FILE:-compose.yaml}
WEB_SERVICE=%s
if [ "$WEB_SERVICE" = "YOUR_COMPOSE_SERVICE_NAME" ] || [ -z "$WEB_SERVICE" ]; then
  echo "Set WEB_SERVICE to the Compose service name for your local HTTP server." >&2
  exit 1
fi

QURL_TUNNEL_SLUG=%s
TUNNEL_SERVICE=%s
SECRET_DIR="/run/secrets/qurl-tunnel/${QURL_TUNNEL_SLUG}"
AGENT_STATE_DIR="/var/lib/layerv/qurl-tunnel/${QURL_TUNNEL_SLUG}/agent"
CONFIG_FILE="$PWD/qurl-proxy-${QURL_TUNNEL_SLUG}.yaml"
QURL_COMPOSE_FILE="$PWD/qurl-tunnel-${QURL_TUNNEL_SLUG}.compose.yaml"

cat > "$CONFIG_FILE" <<'QURL_PROXY_YAML_EOF'
%s
QURL_PROXY_YAML_EOF

$SUDO install -d -m 0700 -o 65532 -g 65532 "$SECRET_DIR"
$SUDO install -d -m 0700 -o 65532 -g 65532 "$AGENT_STATE_DIR"
%s
printf '%%s' "$QURL_BOOTSTRAP_KEY" | $SUDO install -m 0400 -o 65532 -g 65532 /dev/stdin "$SECRET_DIR/api_key"
unset QURL_BOOTSTRAP_KEY

# This heredoc is intentionally unquoted so it writes concrete values into
# the per-slug Compose fragment; future docker compose commands should not need
# the install script's temporary environment variables.
cat > "$QURL_COMPOSE_FILE" <<QURL_COMPOSE_YAML_EOF
services:
  %s:
    image: %s
    restart: on-failure:5
    network_mode: "service:${WEB_SERVICE}"
    depends_on:
      - ${WEB_SERVICE}
    volumes:
      - ${AGENT_STATE_DIR}:/var/lib/layerv/agent
      - ${SECRET_DIR}:/run/secrets/qurl-tunnel:ro
      - ./qurl-proxy-${QURL_TUNNEL_SLUG}.yaml:/work/qurl-proxy.yaml:ro
    environment:
      QURL_API_KEY_FILE: /run/secrets/qurl-tunnel/api_key
      QURL_TUNNEL_SLUG: ${QURL_TUNNEL_SLUG}
QURL_COMPOSE_YAML_EOF

docker compose -f "$APP_COMPOSE_FILE" -f "$QURL_COMPOSE_FILE" up -d "$TUNNEL_SERVICE"`, webService, shellSingleQuote(args.Slug), tunnelService, renderTunnelConfigYAML(args), renderBootstrapKeyPromptShell(), tunnelServiceName, yamlSingleQuoted(image))

	intro := "Run this from your Docker Compose project directory on the Linux Docker host. It prompts for the bootstrap key so the secret does not land in shell history."
	if args.WebRef == "" {
		intro += " Replace `YOUR_COMPOSE_SERVICE_NAME` in the block first, fill the Docker service/container field, or use `service:<name>` / `web_container:<name>` in the typed command."
	}
	intro += " If your app file is not compose.yaml, set `APP_COMPOSE_FILE` before running it. Re-run this install to regenerate the per-slug Compose fragment when the slug, port, or service changes."
	return intro + "\n\n" + slackCodeBlock(compose) + "\n\nVerify with `docker compose -f compose.yaml -f qurl-tunnel-" + args.Slug + ".compose.yaml logs -f qurl-tunnel-" + args.Slug + "`; if you changed `APP_COMPOSE_FILE`, use that file there too. After the tunnel connects, delete the bootstrap key file."
}
