package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	// Production deploys should set QURL_TUNNEL_IMAGE to an immutable
	// release tag or digest. The floating fallback is for dev/sandbox
	// onboarding where the latest sidecar build is intentional.
	defaultTunnelImage     = "ghcr.io/layervai/qurl-reverse-tunnel-client:latest"
	defaultTunnelLocalPort = 8080
	tunnelBootstrapTTL     = "1h"
	tunnelBootstrapSkew    = 2 * time.Minute
	// Slack response_url values are valid for roughly 30 minutes; keep modal
	// submissions inside that window so async install errors can still reach
	// the admin after Slack accepts the view submission.
	tunnelInstallModalTTL = 25 * time.Minute
	// Slack trigger_ids expire after roughly three seconds. The slash-command
	// ack now happens before views.open; this tighter bound preserves enough of
	// the trigger window for one admin check plus the Slack Web API call.
	slackTriggerOpenViewBudget = time.Second
	tunnelScopeAgent           = "qurl:agent"
	tunnelScopeWrite           = "qurl:write"
	tunnelEnvAPIKey            = "QURL_API_KEY"
)

var tunnelSlugPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}[a-z0-9]$`)
var tunnelBootstrapNow = time.Now

type tunnelInstallEnvironment string

const (
	tunnelEnvDockerVM   tunnelInstallEnvironment = "docker-vm"
	tunnelEnvCompose    tunnelInstallEnvironment = "docker-compose"
	tunnelEnvECSFargate tunnelInstallEnvironment = "ecs-fargate"
	tunnelEnvKubernetes tunnelInstallEnvironment = "kubernetes"
)

type tunnelInstallArgs struct {
	Slug         string
	Alias        string
	LocalPort    int
	Environment  tunnelInstallEnvironment
	WebContainer string
}

type ecsContainerDefinition struct {
	Name             string              `json:"name"`
	Image            string              `json:"image"`
	Essential        bool                `json:"essential"`
	Environment      []ecsEnvironmentVar `json:"environment"`
	Secrets          []ecsSecret         `json:"secrets"`
	MountPoints      []ecsMountPoint     `json:"mountPoints"`
	LogConfiguration ecsLogConfiguration `json:"logConfiguration"`
}

type ecsEnvironmentVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type ecsSecret struct {
	Name      string `json:"name"`
	ValueFrom string `json:"valueFrom"`
}

type ecsMountPoint struct {
	SourceVolume  string `json:"sourceVolume"`
	ContainerPath string `json:"containerPath"`
	ReadOnly      bool   `json:"readOnly"`
}

type ecsLogConfiguration struct {
	LogDriver string            `json:"logDriver"`
	Options   map[string]string `json:"options"`
}

func parseTunnelInstall(text string) (args *tunnelInstallArgs, userMsg string) {
	matched, rest := slashVerb(strings.TrimSpace(text), "tunnel")
	if !matched {
		return nil, tunnelInstallUsage()
	}
	fields := strings.Fields(rest)
	if len(fields) < 2 || fields[0] != "install" {
		return nil, tunnelInstallUsage()
	}
	slug := strings.TrimPrefix(fields[1], "$")
	args = &tunnelInstallArgs{
		Slug:        slug,
		Alias:       slug,
		LocalPort:   defaultTunnelLocalPort,
		Environment: tunnelEnvDockerVM,
	}
	if !tunnelSlugPattern.MatchString(args.Slug) {
		return nil, "Tunnel slug must be 3-64 chars, lowercase letters/numbers/hyphens, start with a letter, and end with a letter or number.\n\n" + tunnelInstallUsage()
	}
	for _, token := range fields[2:] {
		switch {
		case strings.HasPrefix(token, "port:"):
			port, err := strconv.Atoi(strings.TrimPrefix(token, "port:"))
			if err != nil || port < 1 || port > 65535 {
				return nil, "port must be a TCP port from 1 to 65535.\n\n" + tunnelInstallUsage()
			}
			args.LocalPort = port
		case strings.HasPrefix(token, "alias:"):
			aliasToken := strings.TrimPrefix(token, "alias:")
			if aliasToken != "" && !strings.HasPrefix(aliasToken, "$") {
				aliasToken = "$" + aliasToken
			}
			alias, msg := requireAlias(aliasToken)
			if msg != "" {
				return nil, msg
			}
			args.Alias = alias
		case strings.HasPrefix(token, "env:"):
			env, msg := parseTunnelEnvironment(strings.TrimPrefix(token, "env:"))
			if msg != "" {
				return nil, msg + "\n\n" + tunnelInstallUsage()
			}
			args.Environment = env
		case strings.HasPrefix(token, "container:"), strings.HasPrefix(token, "service:"), strings.HasPrefix(token, "web_container:"):
			_, value, _ := strings.Cut(token, ":")
			if !dockerContainerRefPattern.MatchString(value) {
				return nil, "container/service/web_container must use letters, numbers, dots, underscores, or hyphens.\n\n" + tunnelInstallUsage()
			}
			args.WebContainer = value
		default:
			return nil, tunnelInstallUsage()
		}
	}
	if msg := tunnelWebContainerValidationMessage(args.Environment, args.WebContainer); msg != "" {
		return nil, msg + "\n\n" + tunnelInstallUsage()
	}
	return args, ""
}

func tunnelInstallUsage() string {
	return "Usage: `/qurl tunnel install` for guided setup, or `/qurl tunnel install <slug|$slug> [port:8080] [alias:$alias] [env:docker|docker-compose|ecs-fargate|kubernetes] [container:<name>|web_container:<name>]`\nExample: `/qurl tunnel install prod-dashboard port:8080`"
}

func parseTunnelEnvironment(raw string) (env tunnelInstallEnvironment, userMsg string) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(tunnelEnvDockerVM), "docker":
		return tunnelEnvDockerVM, ""
	case string(tunnelEnvCompose), "compose":
		return tunnelEnvCompose, ""
	case string(tunnelEnvECSFargate):
		return tunnelEnvECSFargate, ""
	case string(tunnelEnvKubernetes):
		return tunnelEnvKubernetes, ""
	default:
		return "", "env must be one of docker, docker-compose, ecs-fargate, or kubernetes"
	}
}

func (e tunnelInstallEnvironment) label() string {
	switch e {
	case tunnelEnvDockerVM:
		return "Docker sidecar"
	case tunnelEnvCompose:
		return "Docker Compose"
	case tunnelEnvECSFargate:
		return "AWS ECS/Fargate"
	case tunnelEnvKubernetes:
		return "Kubernetes"
	default:
		panic("unreachable tunnel install environment: " + string(e))
	}
}

// handleTunnel routes `/qurl tunnel install` and `/qurl tunnel install <slug>`.
func (h *Handler) handleTunnel(w http.ResponseWriter, values url.Values) {
	if tunnelInstallWizardRequest(values.Get(fieldText)) {
		h.handleTunnelInstallWizard(w, values)
		return
	}

	args, userMsg := parseTunnelInstall(values.Get(fieldText))
	if userMsg != "" {
		respondSlack(w, userMsg)
		return
	}
	if !h.requireAdminStoreSync(w) {
		return
	}
	if h.aliasStore == nil {
		respondSlack(w, "Alias storage is not configured on this Slack bot deployment. Contact the operator.")
		return
	}
	teamID := strings.TrimSpace(values.Get(fieldTeamID))
	userID := strings.TrimSpace(values.Get(fieldUserID))
	channelID := strings.TrimSpace(values.Get(fieldChannelID))
	if channelID == "" {
		respondSlack(w, ":warning: missing channel_id in slash command payload")
		return
	}
	if !h.requireAdminSync(w, teamID, userID, AdminAction("tunnel_install")) {
		return
	}

	h.runAsync(w, "tunnel_install", values, func(ctx context.Context, log *slog.Logger) {
		h.processTunnelInstall(ctx, log, teamID, channelID, userID, values.Get(fieldResponseURL), args)
	})
}

func tunnelInstallWizardRequest(text string) bool {
	matched, rest := slashVerb(strings.TrimSpace(text), "tunnel")
	return matched && rest == "install"
}

func (h *Handler) handleTunnelInstallWizard(w http.ResponseWriter, values url.Values) {
	if !h.requireAdminStoreSync(w) {
		return
	}
	if h.aliasStore == nil {
		respondSlack(w, "Channel shortcut storage is not configured on this Slack bot deployment. Contact the operator.")
		return
	}
	if h.cfg.OpenView == nil {
		respondSlack(w, "Guided tunnel setup is not configured on this Slack bot deployment. Use `/qurl tunnel install <slug> [port:8080]` instead.")
		return
	}
	teamID := strings.TrimSpace(values.Get(fieldTeamID))
	userID := strings.TrimSpace(values.Get(fieldUserID))
	channelID := strings.TrimSpace(values.Get(fieldChannelID))
	if channelID == "" {
		respondSlack(w, ":warning: missing channel_id in slash command payload")
		return
	}
	triggerID := strings.TrimSpace(values.Get(fieldTriggerID))
	if triggerID == "" {
		respondSlack(w, "Slack did not include a trigger_id, so guided setup could not open. Use `/qurl tunnel install <slug> [port:8080]` instead.")
		return
	}
	log := slog.With(
		"command", "tunnel_install_wizard",
		"team_id", teamID,
		"channel_id", channelID,
		"user_id", userID,
		"trigger_id", triggerID,
	)
	if !h.startAsyncWorker(log, func(ctx context.Context, log *slog.Logger) {
		h.openTunnelInstallWizard(ctx, log, teamID, channelID, userID, triggerID, values.Get(fieldResponseURL))
	}) {
		respondSlack(w, ackBusy)
		return
	}
	respondSlack(w, "Opening guided tunnel setup…")
}

func (h *Handler) openTunnelInstallWizard(ctx context.Context, log *slog.Logger, teamID, channelID, userID, triggerID, responseURL string) {
	adminCtx, cancel := context.WithTimeout(ctx, adminGateBudget)
	isAdmin, _, err := h.cfg.AdminStore.CheckAdmin(adminCtx, teamID, userID)
	cancel()
	if err != nil {
		log.Error("tunnel install wizard admin check failed", "error", err)
		h.postResponse(log, responseURL, "Could not verify admin status. Retry in a moment.")
		return
	}
	if !isAdmin {
		log.Warn("tunnel install wizard denied: non-admin")
		h.postResponse(log, responseURL, "This command is admin-only.")
		return
	}
	view, err := TunnelInstallModal(TunnelInstallModalMetadata{
		TeamID:        teamID,
		ChannelID:     channelID,
		UserID:        userID,
		ResponseURL:   responseURL,
		CreatedAtUnix: tunnelBootstrapNow().Unix(),
	})
	if err != nil {
		log.Error("tunnel install wizard modal render failed", "error", err)
		h.postResponse(log, responseURL, "Could not open guided tunnel setup. Please retry or contact support.")
		return
	}
	openCtx, cancel := context.WithTimeout(ctx, slackTriggerOpenViewBudget)
	defer cancel()
	if err := h.cfg.OpenView(openCtx, teamID, triggerID, view); err != nil {
		log.Error("tunnel install wizard views.open failed", "error", err, "slack_rate_limited", errors.Is(err, ErrSlackRateLimited))
		switch {
		case errors.Is(err, ErrSlackTriggerExpired):
			h.postResponse(log, responseURL, "Slack's setup window expired before the modal opened. Run `/qurl tunnel install` again.")
		case errors.Is(err, ErrSlackRateLimited):
			h.postResponse(log, responseURL, "Slack rate-limited guided tunnel setup. Wait a moment, then run `/qurl tunnel install` again.")
		default:
			h.postResponse(log, responseURL, "Could not open guided tunnel setup. Please retry or contact support.")
		}
		return
	}
}

func (h *Handler) processTunnelInstall(ctx context.Context, log *slog.Logger, teamID, channelID, userID, responseURL string, args *tunnelInstallArgs) {
	c, err := h.authenticatedClient(ctx, teamID)
	if err != nil {
		log.Error("tunnel install: failed to get API key", "error", err)
		h.postResponse(log, responseURL, authErrorMessage(err))
		return
	}

	resource, err := c.CreateResource(ctx, &client.CreateResourceInput{
		Type:         client.ResourceTypeTunnel,
		Slug:         args.Slug,
		FindOrCreate: true,
		Description:  "Slack tunnel install for " + args.Slug,
	})
	if err != nil {
		log.Error("tunnel install: create/find resource failed", "error", err, "slug", args.Slug)
		h.postResponse(log, responseURL, sanitizeAPIError(err, "Failed to create or find the tunnel resource"))
		return
	}

	aliasStatus, err := h.ensureTunnelAlias(ctx, teamID, channelID, args.Alias, resource.ResourceID)
	if err != nil {
		log.Error("tunnel install: channel shortcut bind failed", "error", err, "shortcut", args.Alias, "resource_id", resource.ResourceID)
		h.postResponse(log, responseURL, aliasStatus)
		return
	}

	key, err := c.CreateAPIKey(ctx, &client.CreateAPIKeyInput{
		Name:           "Slack tunnel bootstrap " + args.Slug,
		Scopes:         []string{tunnelScopeAgent, tunnelScopeWrite},
		Purpose:        client.APIKeyPurposeTunnelBootstrap,
		TunnelSlug:     args.Slug,
		ExpiresIn:      tunnelBootstrapTTL,
		IdempotencyKey: tunnelBootstrapIdempotencyKey(teamID, channelID, userID, args.Slug, tunnelBootstrapNow()),
	})
	if err != nil {
		log.Error("tunnel install: bootstrap key mint failed", "error", err, "slug", args.Slug, "resource_id", resource.ResourceID)
		h.postResponse(log, responseURL, sanitizeAPIError(err, "Failed to mint a tunnel bootstrap key"))
		return
	}
	if key.APIKey == "" {
		log.Error("tunnel install: create api key response missing plaintext", "slug", args.Slug, "resource_id", resource.ResourceID)
		h.postResponse(log, responseURL, "The qURL API did not return a bootstrap key. Please retry or contact support.")
		return
	}
	if err := validateBootstrapAPIKeyForShell(key.APIKey); err != nil {
		log.Error("tunnel install: create api key response was not shell-renderable", "error", err, "slug", args.Slug, "resource_id", resource.ResourceID)
		h.postResponse(log, responseURL, "The qURL API returned a bootstrap key in an unexpected format. Please retry or contact support.")
		return
	}

	msg, err := h.renderTunnelInstallMessage(args, key, aliasStatus)
	if err != nil {
		log.Error("tunnel install: render failed", "error", err, "slug", args.Slug, "resource_id", resource.ResourceID)
		h.postResponse(log, responseURL, "Tunnel setup succeeded, but Slack could not render the install instructions. Please retry or contact support.")
		return
	}
	log.Info("tunnel install succeeded", "slug", args.Slug, "shortcut", args.Alias, "environment", args.Environment, "resource_id", resource.ResourceID)
	h.postResponse(log, responseURL, msg)
}

func tunnelBootstrapIdempotencyKey(teamID, channelID, userID, slug string, now time.Time) string {
	// Hourly bucket matches the one-hour bootstrap key TTL: retries inside
	// the same setup window replay safely, while a later install gets a fresh
	// key instead of replaying an expired plaintext secret.
	bucket := now.UTC().Format("2006010215")
	return IdempotencyKey(teamID, channelID, userID, "tunnel-bootstrap:"+slug+":"+bucket)
}

func (h *Handler) ensureTunnelAlias(ctx context.Context, teamID, channelID, alias, resourceID string) (string, error) {
	existing, found, err := h.cfg.AdminStore.LookupChannelAlias(ctx, teamID, channelID, alias)
	if err != nil {
		return ":warning: failed to check the existing channel alias; no bootstrap key was minted.", err
	}
	if found {
		if existing == resourceID {
			return fmt.Sprintf("Channel shortcut `$%s` is ready.", alias), nil
		}
		return fmt.Sprintf("Channel shortcut `$%s` is already used. Run `/qurl unset-alias $%s` first, or choose `alias:$other-name`.", alias, alias), slackdata.ErrAliasAlreadyBound
	}
	if err := h.aliasStore.BindChannelAlias(ctx, teamID, channelID, alias, resourceID); err != nil {
		if errors.Is(err, slackdata.ErrAliasAlreadyBound) {
			existing, found, lookupErr := h.cfg.AdminStore.LookupChannelAlias(ctx, teamID, channelID, alias)
			if lookupErr == nil && found && existing == resourceID {
				return fmt.Sprintf("Channel shortcut `$%s` is ready.", alias), nil
			}
		}
		return ":warning: failed to bind the channel shortcut; no bootstrap key was minted.", err
	}
	return fmt.Sprintf("Channel shortcut `$%s` is ready.", alias), nil
}

func (h *Handler) renderTunnelInstallMessage(args *tunnelInstallArgs, key *client.APIKey, aliasStatus string) (string, error) {
	image := strings.TrimSpace(h.cfg.TunnelImage)
	usingDefaultImage := image == ""
	if image == "" {
		image = defaultTunnelImage
	}
	instructions, err := h.renderTunnelInstallInstructions(args, key, image)
	if err != nil {
		return "", err
	}
	imageNote := tunnelImageNote(usingDefaultImage)
	if imageNote != "" {
		imageNote = "\n\n" + imageNote
	}

	return fmt.Sprintf("Tunnel `%s` is ready.\n%s\n\nTarget environment: %s.\n\n%s%s\n\nBootstrap key %s. After the sidecar connects, delete this Slack message and remove the mounted bootstrap key from the runtime.\n\nThen users can run `/qurl get $%s`.",
		args.Slug,
		aliasStatus,
		args.Environment.label(),
		instructions,
		imageNote,
		tunnelBootstrapExpiryLabel(key),
		args.Alias,
	), nil
}

func tunnelImageNote(usingDefaultImage bool) string {
	if !usingDefaultImage {
		return ""
	}
	return "Image: using the dev/sandbox fallback. Set `QURL_TUNNEL_IMAGE` to an immutable tag or digest before production rollout."
}

func (h *Handler) renderTunnelInstallInstructions(args *tunnelInstallArgs, key *client.APIKey, image string) (string, error) {
	switch args.Environment {
	case tunnelEnvECSFargate:
		return renderECSFargateTunnelInstructions(args, key, image), nil
	case tunnelEnvKubernetes:
		return renderKubernetesTunnelInstructions(args, key, image), nil
	case tunnelEnvCompose:
		return renderDockerComposeTunnelInstructions(args, key, image), nil
	case tunnelEnvDockerVM:
		return renderDockerTunnelInstructions(args, key, image), nil
	default:
		return "", fmt.Errorf("unreachable tunnel install environment: %s", args.Environment)
	}
}

func renderTunnelConfigYAML(args *tunnelInstallArgs) string {
	return fmt.Sprintf(`routes:
  - name: %s
    type: http
    local_ip: 127.0.0.1
    local_port: %d`, args.Slug, args.LocalPort)
}

func renderDockerTunnelInstructions(args *tunnelInstallArgs, key *client.APIKey, image string) string {
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
printf '%%s' %s | $SUDO install -m 0400 -o 65532 -g 65532 /dev/stdin "$SECRET_DIR/api_key"

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
  %s`, webContainer, args.Slug, renderTunnelConfigYAML(args), shellSingleQuote(key.APIKey), shellSingleQuote(image))

	intro := "Run this whole block on the Docker host where your local HTTP server container is running."
	if args.WebContainer == "" {
		intro += " Replace `YOUR_WEB_CONTAINER_NAME` first."
	}
	intro += " It writes or overwrites a per-slug qurl-proxy config in the current directory."
	return intro + "\n\n" + slackCodeBlock(docker) + "\n\nVerify with `docker logs -f qurl-tunnel-" + args.Slug + "`; after the tunnel connects, delete the bootstrap key file."
}

func renderDockerComposeTunnelInstructions(args *tunnelInstallArgs, key *client.APIKey, image string) string {
	webService := shellSingleQuote("YOUR_COMPOSE_SERVICE_NAME")
	if args.WebContainer != "" {
		webService = shellSingleQuote(args.WebContainer)
	}
	tunnelServiceName := "qurl-tunnel-" + args.Slug
	tunnelService := shellSingleQuote(tunnelServiceName)
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
printf '%%s' %s | $SUDO install -m 0400 -o 65532 -g 65532 /dev/stdin "$SECRET_DIR/api_key"

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

docker compose -f "$APP_COMPOSE_FILE" -f "$QURL_COMPOSE_FILE" up -d "$TUNNEL_SERVICE"`, webService, args.Slug, tunnelService, renderTunnelConfigYAML(args), shellSingleQuote(key.APIKey), tunnelServiceName, yamlSingleQuoted(image))

	intro := "Run this from your Docker Compose project directory."
	if args.WebContainer == "" {
		intro += " Replace `YOUR_COMPOSE_SERVICE_NAME` in the block first, fill the Docker service/container field, or use `service:<name>` / `web_container:<name>` in the typed command."
	}
	intro += " If your app file is not compose.yaml, set `APP_COMPOSE_FILE` before running it. Re-run this install to regenerate the per-slug Compose fragment when the slug, port, or service changes."
	return intro + "\n\n" + slackCodeBlock(compose) + "\n\nVerify with `docker compose -f compose.yaml -f qurl-tunnel-" + args.Slug + ".compose.yaml logs -f qurl-tunnel-" + args.Slug + "`; if you changed `APP_COMPOSE_FILE`, use that file there too. After the tunnel connects, delete the bootstrap key file."
}

func renderECSFargateTunnelInstructions(args *tunnelInstallArgs, key *client.APIKey, image string) string {
	containerJSON := renderECSSidecarContainerJSON(args, image)
	secretName := "qurl-tunnel-" + args.Slug
	// validateBootstrapAPIKeyForShell keeps this plaintext token single-line
	// and printable; slackCodeBlock rejects nested fences before Slack sees it.
	body := fmt.Sprintf(`1. Store this bootstrap key in AWS Secrets Manager, then delete this Slack message:
%s

2. Put qurl-proxy.yaml on an EFS access point mounted into the task:
%s

3. Add this sidecar container to the same task definition as the target container:
%s

4. Add durable EFS-backed volumes named qurl-agent-state and qurl-config.
Do not share qurl-agent-state across concurrently running sidecars.`, key.APIKey, renderTunnelConfigYAML(args), containerJSON)

	return "Use this as an ECS/Fargate task-definition checklist. Create the AWS Secrets Manager secret as `" + secretName + "` so the task definition's `valueFrom` ARN resolves, and replace `<region>` / `<account-id>` in the JSON placeholders before registering the task definition. The sidecar must share the target container's network namespace, so `127.0.0.1:" + strconv.Itoa(args.LocalPort) + "` reaches the local service. After the task logs show the tunnel connected, delete the bootstrap secret.\n\n" + slackCodeBlock(body)
}

func renderECSSidecarContainerJSON(args *tunnelInstallArgs, image string) string {
	container := ecsContainerDefinition{
		Name:      "qurl-tunnel",
		Image:     image,
		Essential: true,
		Environment: []ecsEnvironmentVar{
			{Name: "QURL_TUNNEL_SLUG", Value: args.Slug},
		},
		Secrets: []ecsSecret{
			{Name: tunnelEnvAPIKey, ValueFrom: "arn:aws:secretsmanager:<region>:<account-id>:secret:qurl-tunnel-" + args.Slug},
		},
		MountPoints: []ecsMountPoint{
			{SourceVolume: "qurl-agent-state", ContainerPath: "/var/lib/layerv/agent"},
			{SourceVolume: "qurl-config", ContainerPath: "/work", ReadOnly: true},
		},
		LogConfiguration: ecsLogConfiguration{
			LogDriver: "awslogs",
			Options: map[string]string{
				"awslogs-group":         "/ecs/qurl-tunnel",
				"awslogs-region":        "<region>",
				"awslogs-stream-prefix": "qurl",
			},
		},
	}
	return string(mustMarshalStaticJSON(container))
}

func mustMarshalStaticJSON(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic("marshal static Slack install JSON: " + err.Error())
	}
	return b
}

func renderKubernetesTunnelInstructions(args *tunnelInstallArgs, key *client.APIKey, image string) string {
	objects := fmt.Sprintf(`kubectl apply -f - <<'QURL_K8S_YAML_EOF'
apiVersion: v1
kind: Secret
metadata:
  name: qurl-tunnel-%s
type: Opaque
stringData:
  api_key: %s
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: qurl-proxy-%s
data:
  qurl-proxy.yaml: |
%s
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: qurl-agent-%s
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 1Gi
QURL_K8S_YAML_EOF`, args.Slug, yamlSingleQuoted(key.APIKey), args.Slug, indentLines(renderTunnelConfigYAML(args), 4), args.Slug)

	patch := fmt.Sprintf(`securityContext:
  runAsUser: 65532
  runAsGroup: 65532
  fsGroup: 65532
containers:
  - name: qurl-tunnel
    image: %s
    env:
      - name: QURL_API_KEY_FILE
        value: /run/secrets/qurl-tunnel/api_key
      - name: QURL_TUNNEL_SLUG
        value: %s
    volumeMounts:
      - name: qurl-agent-state
        mountPath: /var/lib/layerv/agent
      - name: qurl-bootstrap
        mountPath: /run/secrets/qurl-tunnel
        readOnly: true
      - name: qurl-proxy
        mountPath: /work/qurl-proxy.yaml
        subPath: qurl-proxy.yaml
        readOnly: true
volumes:
  - name: qurl-agent-state
    persistentVolumeClaim:
      claimName: qurl-agent-%s
  - name: qurl-bootstrap
    secret:
      secretName: qurl-tunnel-%s
      defaultMode: 0400
  - name: qurl-proxy
    configMap:
      name: qurl-proxy-%s`, yamlSingleQuoted(image), args.Slug, args.Slug, args.Slug, args.Slug)

	return "Run this once in the target namespace, then add the sidecar/volumes block to the same pod spec as the target container so `127.0.0.1:" + strconv.Itoa(args.LocalPort) + "` reaches the local service. Use one PVC per sidecar replica; if you scale replicas, use a StatefulSet with a volumeClaimTemplate instead of sharing this PVC. After the pod logs show the tunnel connected, delete the bootstrap Secret.\n\n" + slackCodeBlock(objects) + "\n\nPod spec additions:\n\n" + slackCodeBlock(patch)
}

func tunnelBootstrapTTLLabel() string {
	return "expires in " + humanTunnelBootstrapTTL(tunnelBootstrapTTL)
}

func tunnelBootstrapExpiryLabel(key *client.APIKey) string {
	if key != nil && key.ExpiresAt != nil {
		remaining := key.ExpiresAt.Sub(tunnelBootstrapNow())
		if remaining > 0 {
			return "expires in " + humanTunnelBootstrapDuration(remaining)
		}
		if remaining > -tunnelBootstrapSkew {
			return tunnelBootstrapTTLLabel()
		}
		return "is expired"
	}
	return tunnelBootstrapTTLLabel()
}

func humanTunnelBootstrapTTL(ttl string) string {
	d, err := time.ParseDuration(ttl)
	if err != nil {
		return "the requested " + ttl
	}
	return humanTunnelBootstrapDuration(d)
}

func humanTunnelBootstrapDuration(d time.Duration) string {
	d = d.Truncate(time.Minute)
	if d < time.Minute {
		return "less than 1 minute"
	}
	hours := int(d / time.Hour)
	minutes := int((d % time.Hour) / time.Minute)
	switch {
	case hours > 0 && minutes > 0:
		return pluralizeDuration(hours, "hour") + " " + pluralizeDuration(minutes, "minute")
	case hours > 0:
		return pluralizeDuration(hours, "hour")
	default:
		return pluralizeDuration(minutes, "minute")
	}
}

func pluralizeDuration(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func validateBootstrapAPIKeyForShell(apiKey string) error {
	// shellSingleQuote safely quotes arbitrary text. This check is an
	// additional output-surface guard: qurl-service bootstrap keys should be
	// printable single-line tokens, and refusing quote/control bytes keeps the
	// rendered install snippet easy for operators to inspect.
	if apiKey == "" {
		return errors.New("empty api key")
	}
	for _, r := range apiKey {
		if r == '\'' || r == '`' || r < 0x20 || r == 0x7f {
			return errors.New("api key contains unsupported characters")
		}
	}
	return nil
}

func shellSingleQuote(s string) string {
	// a'b -> 'a'"'"'b', the POSIX shell idiom for embedding a literal quote
	// inside a single-quoted word.
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func indentLines(s string, spaces int) string {
	prefix := strings.Repeat(" ", spaces)
	lines := strings.Split(s, "\n")
	for i := range lines {
		if lines[i] != "" {
			lines[i] = prefix + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

func yamlSingleQuoted(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// ValidateTunnelImageRef checks the operator-provided image reference shown in
// install snippets. Empty is valid and means the handler will use its fallback.
func ValidateTunnelImageRef(image string) error {
	if image == "" {
		return nil
	}
	for _, r := range image {
		if r <= ' ' || r == 0x7f || r == '\'' || r == '"' || r == '`' || r == '$' {
			return errors.New("tunnel image reference contains quotes, dollar signs, backticks, whitespace, or control characters")
		}
	}
	return nil
}

func slackCodeBlock(body string) string {
	// Slack cannot escape a nested triple-backtick fence inside a code block.
	// Current callers render static install snippets, so panic on programmer
	// error instead of silently rewriting future user-visible content.
	if strings.Contains(body, "```") {
		panic("slack code block body contains nested triple-backtick fence")
	}
	return "```\n" + body + "\n```"
}
