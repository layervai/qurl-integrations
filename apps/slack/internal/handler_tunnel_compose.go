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
	tunnelServiceName := "qurl-connector-" + args.Slug
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
	quotedAPIURL, err := yamlSingleQuoted(args.APIURL)
	if err != nil {
		return "", err
	}
	// The Compose heredoc must remain expandable for its validated runtime
	// variables. Assign the complete YAML scalar through a shell quote first;
	// parameter expansion is not recursive, so URL shell metacharacters remain
	// literal when the heredoc expands this variable.
	quotedAPIURLShell := shellSingleQuote(quotedAPIURL)
	// HubTrust is unset (zero value) unless cmd/main.go's validateHubTrustEnv
	// confirmed all three envs are set together, so empty hubTrustShellVars/
	// hubTrustEnvironmentYAML leave this block byte-identical to the
	// no-Hub-trust output. Values route through the same YAML-then-shell
	// quoting as QURL_API_URL_YAML above because, like the API URL, the Hub
	// host is an unconstrained operator-supplied string expanded into this
	// intentionally unquoted heredoc.
	hubTrustShellVars := ""
	hubTrustEnvironmentYAML := ""
	if args.HubTrust.Host != "" {
		quotedHubHost, err := yamlSingleQuoted(args.HubTrust.Host)
		if err != nil {
			return "", err
		}
		quotedHubPort, err := yamlSingleQuoted(args.HubTrust.Port)
		if err != nil {
			return "", err
		}
		quotedHubKey, err := yamlSingleQuoted(args.HubTrust.PublicKeyB64)
		if err != nil {
			return "", err
		}
		hubTrustShellVars = fmt.Sprintf("QURL_CONNECTOR_HUB_HOST_YAML=%s\nQURL_CONNECTOR_HUB_PORT_YAML=%s\nQURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64_YAML=%s\n",
			shellSingleQuote(quotedHubHost), shellSingleQuote(quotedHubPort), shellSingleQuote(quotedHubKey))
		hubTrustEnvironmentYAML = "      QURL_CONNECTOR_HUB_HOST: ${QURL_CONNECTOR_HUB_HOST_YAML}\n      QURL_CONNECTOR_HUB_PORT: ${QURL_CONNECTOR_HUB_PORT_YAML}\n      QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64: ${QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64_YAML}\n"
	}
	// SECURITY: The Compose heredoc below is intentionally unquoted so it can
	// expand WEB_SERVICE, QURL_CONNECTOR_ID, AGENT_STATE_DIR, and SECRET_DIR
	// into the generated file. Trust assumptions: WEB_SERVICE comes from
	// dockerComposeServicePattern plus the runtime case guard below; the slug
	// matches tunnelSlugPattern; state/secret dirs derive only from that slug.
	// Keep dockerComposeServicePattern narrow: it rejects shell metacharacters
	// such as '$', backticks, quotes, slashes, and whitespace. QURL_API_URL_YAML
	// is assigned through a shell quote and expanded once, so it does not share
	// those identifier-only restrictions.
	compose := fmt.Sprintf(`set -eu
%s

%s

# Run from the directory with your existing Compose file.
APP_COMPOSE_FILE=${APP_COMPOSE_FILE:-compose.yaml}
WEB_SERVICE=%s
%s

QURL_CONNECTOR_ID=%s
CONNECTOR_SERVICE=%s
%sQURL_API_URL_YAML=%s
SECRET_DIR="/run/secrets/qurl-connector/${QURL_CONNECTOR_ID}"
AGENT_STATE_DIR="/var/lib/layerv/qurl-connector/${QURL_CONNECTOR_ID}/agent"
AUDIT_DIR="/var/log/layerv/qurl-connector/${QURL_CONNECTOR_ID}"
CONFIG_FILE="$PWD/qurl-proxy-${QURL_CONNECTOR_ID}.yaml"
QURL_COMPOSE_FILE="$PWD/qurl-connector-${QURL_CONNECTOR_ID}.compose.yaml"

cat > "$CONFIG_FILE" <<'QURL_PROXY_YAML_EOF'
%s
QURL_PROXY_YAML_EOF
# The generated config contains only client-safe public/routing metadata; 0644
# lets the nonroot Connector (UID 65532) read the bind mount. Its bootstrap
# credential stays separately protected in the 0600 api_key file below.
$SUDO chmod 0644 "$CONFIG_FILE"

$SUDO install -d -m 0700 -o 65532 -g 65532 "$SECRET_DIR"
$SUDO install -d -m 0700 -o 65532 -g 65532 "$AGENT_STATE_DIR"
$SUDO install -d -m 0700 -o 65532 -g 65532 "$AUDIT_DIR"
%s
%s

# This heredoc is intentionally unquoted so it expands the validated variables
# now and writes a static per-connector Compose fragment. Future compose commands
# do not need WEB_SERVICE exported unless you regenerate the fragment.
# If you edit this generated file by hand later, rerun the install instead of
# adding new shell variables here.
cat > "$QURL_COMPOSE_FILE" <<QURL_COMPOSE_YAML_EOF
services:
  %s:
    image: %s
    user: "65532:65532"
    restart: on-failure:5
    read_only: true
    tmpfs:
      - /tmp:rw,size=64m
    pids_limit: 512
    cap_drop:
      - ALL
    security_opt:
      - 'no-new-privileges:true'
    network_mode: "service:${WEB_SERVICE}"
    depends_on:
      ${WEB_SERVICE}:
        condition: service_started
    volumes:
      - ${AGENT_STATE_DIR}:/var/lib/layerv/agent
      - ${AUDIT_DIR}:/var/log/layerv/qurl-connector
      - ${SECRET_DIR}:/run/secrets/qurl-connector:ro
      - ./qurl-proxy-${QURL_CONNECTOR_ID}.yaml:/work/qurl-proxy.yaml:ro
    environment:
      QURL_API_KEY_FILE: /run/secrets/qurl-connector/api_key
      QURL_AUDIT_FILE: /var/log/layerv/qurl-connector/audit.log
      QURL_CONNECTOR_ID: ${QURL_CONNECTOR_ID}
%s      QURL_API_URL: ${QURL_API_URL_YAML}
QURL_COMPOSE_YAML_EOF

docker compose -f "$APP_COMPOSE_FILE" -f "$QURL_COMPOSE_FILE" up -d "$CONNECTOR_SERVICE"`, renderPortablePipefailShell(), renderSudoDetectionShell(), webService, renderRequiredShellNameGuard("WEB_SERVICE", "YOUR_COMPOSE_SERVICE_NAME", "the Compose service name for your local HTTP server", "A-Za-z0-9_-", "letters, numbers, underscores, and hyphens"), shellSingleQuote(args.Slug), tunnelService, hubTrustShellVars, quotedAPIURLShell, configYAML, renderBootstrapKeyPromptShell(), renderBootstrapKeyFileInstallShell(`"$SECRET_DIR/api_key"`), quotedTunnelServiceName, quotedImage, hubTrustEnvironmentYAML)

	block, err := slackCodeBlock(compose)
	if err != nil {
		return "", err
	}
	introParts := []string{
		"Run this from your Docker Compose project directory on the Linux Docker host.",
		"It prompts for the enrollment token so the secret does not land in shell history; use a trusted host and shell because local administrators can inspect process state during setup. If your terminal echoes pasted input, stop and use a platform secret manager instead.",
	}
	if args.WebRef == "" {
		introParts = append(introParts, "Replace `YOUR_COMPOSE_SERVICE_NAME` in the block first, fill the Docker service/container field, or use `service:<name>` / `web_container:<name>` in the typed command.")
	}
	introParts = append(introParts,
		"If your app file is not compose.yaml, set `APP_COMPOSE_FILE` before running it.",
		"Re-run this install to regenerate the same qURL Connector's Compose fragment when the port or service changes; do not hand-edit the generated fragment because the next install replaces it.",
		"If Compose recreates the web service container, bring the qURL Connector service up again too.",
	)
	intro := strings.Join(introParts, " ")
	return intro + "\n\n" + block + "\n\nVerify with `docker compose -f compose.yaml -f qurl-connector-" + args.Slug + ".compose.yaml logs -f qurl-connector-" + args.Slug + "`; if you changed `APP_COMPOSE_FILE`, use that file there too. After the qURL Connector connects, delete the enrollment-token file.", nil
}
