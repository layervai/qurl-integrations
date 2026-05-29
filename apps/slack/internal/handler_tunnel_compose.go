package internal

import (
	"fmt"
	"strings"
)

func renderDockerComposeTunnelInstructions(args *tunnelInstallArgs, image string) (string, error) {
	webService := shellSingleQuote("YOUR_COMPOSE_SERVICE_NAME")
	if args.WebRef != "" {
		webService = shellSingleQuote(args.WebRef)
	}
	tunnelServiceName := "qurl-tunnel-" + args.Slug
	tunnelService := shellSingleQuote(tunnelServiceName)
	// Quote the generated service key even though the current name shape does
	// not require it. It keeps future slug/service-name widening local to the
	// YAML quoting helper instead of this heredoc.
	quotedTunnelServiceName, err := yamlSingleQuoted(tunnelServiceName)
	if err != nil {
		return "", err
	}
	quotedImage, err := yamlSingleQuoted(image)
	if err != nil {
		return "", err
	}
	configYAML, err := renderTunnelConfigYAML(args)
	if err != nil {
		return "", err
	}
	// SECURITY: The Compose heredoc below is intentionally unquoted so it can
	// expand WEB_SERVICE, QURL_TUNNEL_SLUG, AGENT_STATE_DIR, and SECRET_DIR
	// into the generated file. Trust assumptions: WEB_SERVICE comes from
	// dockerComposeServicePattern plus the runtime case guard below; the slug
	// matches tunnelSlugPattern; state/secret dirs derive only from that slug.
	// Keep dockerComposeServicePattern narrow: it rejects shell metacharacters
	// such as '$', backticks, quotes, slashes, and whitespace.
	compose := fmt.Sprintf(`set -eu
%s

%s

# Run from the directory with your existing Compose file.
APP_COMPOSE_FILE=${APP_COMPOSE_FILE:-compose.yaml}
WEB_SERVICE=%s
%s

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
%s

# This heredoc is intentionally unquoted so it expands the validated variables
# now and writes a static per-slug Compose fragment. Future compose commands
# do not need WEB_SERVICE exported unless you regenerate the fragment.
# If you edit this generated file by hand later, rerun the install instead of
# adding new shell variables here.
cat > "$QURL_COMPOSE_FILE" <<QURL_COMPOSE_YAML_EOF
services:
  %s:
    image: %s
    restart: on-failure:5
    network_mode: "service:${WEB_SERVICE}"
    depends_on:
      ${WEB_SERVICE}:
        condition: service_started
    volumes:
      - ${AGENT_STATE_DIR}:/var/lib/layerv/agent
      - ${SECRET_DIR}:/run/secrets/qurl-tunnel:ro
      - ./qurl-proxy-${QURL_TUNNEL_SLUG}.yaml:/work/qurl-proxy.yaml:ro
    environment:
      QURL_API_KEY_FILE: /run/secrets/qurl-tunnel/api_key
      QURL_TUNNEL_SLUG: ${QURL_TUNNEL_SLUG}
QURL_COMPOSE_YAML_EOF

docker compose -f "$APP_COMPOSE_FILE" -f "$QURL_COMPOSE_FILE" up -d "$TUNNEL_SERVICE"`, renderPortablePipefailShell(), renderSudoDetectionShell(), webService, renderRequiredShellNameGuard("WEB_SERVICE", "YOUR_COMPOSE_SERVICE_NAME", "the Compose service name for your local HTTP server", "A-Za-z0-9_-", "letters, numbers, underscores, and hyphens"), shellSingleQuote(args.Slug), tunnelService, configYAML, renderBootstrapKeyPromptShell(), renderBootstrapKeyFileInstallShell(`"$SECRET_DIR/api_key"`), quotedTunnelServiceName, quotedImage)

	block, err := slackCodeBlock(compose)
	if err != nil {
		return "", err
	}
	introParts := []string{
		"Run this from your Docker Compose project directory on the Linux Docker host.",
		"It prompts for the bootstrap key so the secret does not land in shell history; use a trusted host and shell because local administrators can inspect process state during setup. If your terminal echoes pasted input, stop and use a platform secret manager instead.",
	}
	if args.WebRef == "" {
		introParts = append(introParts, "Replace `YOUR_COMPOSE_SERVICE_NAME` in the block first, fill the Docker service/container field, or use `service:<name>` / `web_container:<name>` in the typed command.")
	}
	introParts = append(introParts,
		"If your app file is not compose.yaml, set `APP_COMPOSE_FILE` before running it.",
		"Re-run this install to regenerate the same tunnel's Compose fragment when the port or service changes; do not hand-edit the generated fragment because the next install replaces it.",
		"If Compose recreates the web service container, bring the tunnel service up again too.",
	)
	intro := strings.Join(introParts, " ")
	return intro + "\n\n" + block + "\n\nVerify with `docker compose -f compose.yaml -f qurl-tunnel-" + args.Slug + ".compose.yaml logs -f qurl-tunnel-" + args.Slug + "`; if you changed `APP_COMPOSE_FILE`, use that file there too. After the tunnel connects, delete the bootstrap key file.", nil
}
