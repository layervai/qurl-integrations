package internal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	defaultTunnelImage     = "ghcr.io/layervai/qurl-reverse-tunnel-client:latest"
	defaultTunnelLocalPort = 8080
	tunnelBootstrapTTL     = "24h"
)

var tunnelSlugPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}[a-z0-9]$`)

type tunnelInstallArgs struct {
	Slug      string
	Alias     string
	LocalPort int
}

func parseTunnelInstall(text string) (args *tunnelInstallArgs, userMsg string) {
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "tunnel"))
	fields := strings.Fields(rest)
	if len(fields) < 2 || fields[0] != "install" {
		return nil, tunnelInstallUsage()
	}
	args = &tunnelInstallArgs{
		Slug:      fields[1],
		Alias:     fields[1],
		LocalPort: defaultTunnelLocalPort,
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
			alias, msg := requireAlias(strings.TrimPrefix(token, "alias:"))
			if msg != "" {
				return nil, msg
			}
			args.Alias = alias
		default:
			return nil, tunnelInstallUsage()
		}
	}
	return args, ""
}

func tunnelInstallUsage() string {
	return "Usage: `/qurl tunnel install <slug> [port:8080] [alias:$alias]`\nExample: `/qurl tunnel install prod-dashboard port:8080`"
}

// handleTunnel routes `/qurl tunnel install <slug>`.
func (h *Handler) handleTunnel(w http.ResponseWriter, values url.Values) {
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
		c, err := h.authenticatedClient(ctx, teamID)
		if err != nil {
			log.Error("tunnel install: failed to get API key", "error", err)
			h.postResponse(log, values.Get(fieldResponseURL), authErrorMessage(err))
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
			h.postResponse(log, values.Get(fieldResponseURL), sanitizeAPIError(err, "Failed to create or find the tunnel resource"))
			return
		}

		aliasStatus, err := h.ensureTunnelAlias(ctx, teamID, channelID, args.Alias, resource.ResourceID)
		if err != nil {
			log.Error("tunnel install: alias bind failed", "error", err, "alias", args.Alias, "resource_id", resource.ResourceID)
			h.postResponse(log, values.Get(fieldResponseURL), aliasStatus)
			return
		}

		key, err := c.CreateAPIKey(ctx, &client.CreateAPIKeyInput{
			Name:           "Slack tunnel bootstrap " + args.Slug,
			Scopes:         []string{"qurl:agent", "qurl:write"},
			Purpose:        client.APIKeyPurposeTunnelBootstrap,
			TunnelSlug:     args.Slug,
			ExpiresIn:      tunnelBootstrapTTL,
			IdempotencyKey: IdempotencyKey(teamID, channelID, userID, values.Get(fieldTriggerID)) + "-tunnel-bootstrap",
		})
		if err != nil {
			log.Error("tunnel install: bootstrap key mint failed", "error", err, "slug", args.Slug, "resource_id", resource.ResourceID)
			h.postResponse(log, values.Get(fieldResponseURL), sanitizeAPIError(err, "Failed to mint a tunnel bootstrap key"))
			return
		}
		if key.APIKey == "" {
			log.Error("tunnel install: create api key response missing plaintext", "slug", args.Slug, "resource_id", resource.ResourceID)
			h.postResponse(log, values.Get(fieldResponseURL), "The qURL API did not return a bootstrap key. Please retry or contact support.")
			return
		}

		log.Info("tunnel install succeeded", "slug", args.Slug, "alias", args.Alias, "resource_id", resource.ResourceID)
		h.postResponse(log, values.Get(fieldResponseURL), h.renderTunnelInstallMessage(args, resource, key, aliasStatus))
	})
}

func (h *Handler) ensureTunnelAlias(ctx context.Context, teamID, channelID, alias, resourceID string) (string, error) {
	existing, found, err := h.cfg.AdminStore.LookupChannelAlias(ctx, teamID, channelID, alias)
	if err != nil {
		return ":warning: failed to check the existing channel alias; no bootstrap key was minted.", err
	}
	if found {
		if existing == resourceID {
			return fmt.Sprintf("Alias `$%s` was already bound to `%s` in this channel.", alias, resourceID), nil
		}
		return fmt.Sprintf("Alias `$%s` is already bound in this channel. Run `/qurl unsetalias $%s` first, or choose `alias:$other-name`.", alias, alias), ErrAliasAlreadyBound
	}
	if err := h.aliasStore.BindChannelAlias(ctx, teamID, channelID, alias, resourceID); err != nil {
		if errors.Is(err, ErrAliasAlreadyBound) {
			existing, found, lookupErr := h.cfg.AdminStore.LookupChannelAlias(ctx, teamID, channelID, alias)
			if lookupErr == nil && found && existing == resourceID {
				return fmt.Sprintf("Alias `$%s` was already bound to `%s` in this channel.", alias, resourceID), nil
			}
		}
		return ":warning: failed to bind the Slack alias; no bootstrap key was minted.", err
	}
	return fmt.Sprintf("Alias `$%s` now points to `%s` in this channel.", alias, resourceID), nil
}

func (h *Handler) renderTunnelInstallMessage(args *tunnelInstallArgs, resource *client.Resource, key *client.APIKey, aliasStatus string) string {
	image := strings.TrimSpace(h.cfg.TunnelImage)
	if image == "" {
		image = defaultTunnelImage
	}
	configYAML := fmt.Sprintf(`routes:
  - name: %s
    type: http
    local_ip: 127.0.0.1
    local_port: %d`, args.Slug, args.LocalPort)
	docker := fmt.Sprintf(`SECRET_DIR=/run/secrets/qurl-tunnel
install -d -m 0700 -o 65532 -g 65532 "$SECRET_DIR"
cat <<'QURL_API_KEY' | install -m 0400 -o 65532 -g 65532 /dev/stdin "$SECRET_DIR/api_key"
%s
QURL_API_KEY

docker run -d \
  --name qurl-tunnel \
  --network container:<web-container> \
  --restart=on-failure:5 \
  -v /var/lib/layerv/agent:/var/lib/layerv/agent \
  -v "$SECRET_DIR:$SECRET_DIR:ro" \
  -v "$PWD/qurl-proxy.yaml:/work/qurl-proxy.yaml:ro" \
  -e QURL_API_KEY_FILE="$SECRET_DIR/api_key" \
  -e QURL_TUNNEL_SLUG=%s \
  %s`, key.APIKey, args.Slug, image)

	return fmt.Sprintf("Tunnel `%s` is ready.\nResource: `%s`\n%s\n\nBootstrap key expires in %s. Remove the mounted key file after the first successful sidecar start.\n\n`qurl-proxy.yaml`\n%s\n\nDocker sidecar\n%s\n\nAfter it is connected, users can run `/qurl get $%s`.",
		args.Slug,
		resource.ResourceID,
		aliasStatus,
		tunnelBootstrapTTL,
		slackCodeBlock("yaml", configYAML),
		slackCodeBlock("sh", docker),
		args.Alias,
	)
}

func slackCodeBlock(lang, body string) string {
	body = strings.ReplaceAll(body, "```", "'''")
	return "```" + lang + "\n" + body + "\n```"
}
