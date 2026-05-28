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
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	// Production deploys should set QURL_TUNNEL_IMAGE to an immutable
	// release tag or digest. The floating fallback is for dev/sandbox
	// onboarding where the latest sidecar build is intentional.
	defaultTunnelImage            = "ghcr.io/layervai/qurl-reverse-tunnel-client:latest"
	defaultTunnelLocalPort        = 8080
	tunnelBootstrapTTL            = "1h"
	tunnelBootstrapSkew           = 2 * time.Minute
	tunnelBootstrapCleanupTimeout = 5 * time.Second
	// Slack response_url values are valid for roughly 30 minutes; keep modal
	// submissions inside that window so async install errors can still reach
	// the admin after Slack accepts the view submission. This is intentionally
	// shorter than tunnelBootstrapTTL so any submitted modal still leaves setup
	// headroom after the one-time bootstrap key is minted.
	tunnelInstallModalTTL = 25 * time.Minute
	// Slack trigger_ids expire after roughly three seconds. The slash-command
	// ack now happens before views.open; this bound leaves room for the admin
	// check while reducing false expiry on normal Slack Web API tail latency.
	slackTriggerOpenViewBudget = 1500 * time.Millisecond
	slackRetryAfterDisplayCap  = 5 * time.Minute
	tunnelScopeAgent           = "qurl:agent"
	tunnelScopeWrite           = "qurl:write"
	tunnelEnvAPIKey            = "QURL_API_KEY"
	kubernetesNameMaxLen       = 63
	// Hex chars appended to truncated Kubernetes object names. Twelve hex
	// chars keeps collision risk negligible for expected workspace slug volume.
	kubernetesNameHashHexLen = 12
)

var tunnelSlugPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}[a-z0-9]$`)

// Docker does not publish a tight practical length limit for container names;
// keep Slack input bounded so an accidental paste cannot dominate the rendered
// install snippets. Compose service names reject dots even though raw Docker
// container refs allow them.
var dockerContainerRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
var dockerComposeServicePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
var tunnelBootstrapNow = time.Now

type tunnelInstallEnvironment string
type tunnelInstallWebRefKind string

const (
	tunnelEnvDocker     tunnelInstallEnvironment = "docker"
	tunnelEnvCompose    tunnelInstallEnvironment = "docker-compose"
	tunnelEnvECSFargate tunnelInstallEnvironment = "ecs-fargate"
	tunnelEnvKubernetes tunnelInstallEnvironment = "kubernetes"
	// Input-only shorthand; parseTunnelEnvironment normalizes this spelling to
	// tunnelEnvCompose so renderers never receive a second Compose value.
	tunnelEnvComposeAlt = "compose"

	tunnelWebRefKindNone      tunnelInstallWebRefKind = ""
	tunnelWebRefKindContainer tunnelInstallWebRefKind = "container"
	tunnelWebRefKindService   tunnelInstallWebRefKind = "service"
)

type tunnelInstallArgs struct {
	Slug        string
	Alias       string
	LocalPort   int
	Environment tunnelInstallEnvironment
	WebRef      string
	WebRefKind  tunnelInstallWebRefKind
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
		Environment: tunnelEnvDocker,
	}
	if !tunnelSlugPattern.MatchString(args.Slug) {
		return nil, "Tunnel slug must be 3-64 chars, lowercase letters/numbers/hyphens, start with a letter, and end with a letter or number.\n\n" + tunnelInstallUsage()
	}
	for _, token := range fields[2:] {
		if msg := parseTunnelInstallOption(args, token); msg != "" {
			return nil, msg
		}
	}
	if msg := tunnelWebRefValidationMessage(args.Environment, args.WebRef); msg != "" {
		return nil, msg + "\n\n" + tunnelInstallUsage()
	}
	if msg := tunnelWebRefKindValidationMessage(args.Environment, args.WebRefKind); msg != "" {
		return nil, msg + "\n\n" + tunnelInstallUsage()
	}
	return args, ""
}

func parseTunnelInstallOption(args *tunnelInstallArgs, token string) string {
	switch {
	case strings.HasPrefix(token, "port:"):
		port, err := strconv.Atoi(strings.TrimPrefix(token, "port:"))
		if err != nil || port < 1 || port > 65535 {
			return "port must be a TCP port from 1 to 65535.\n\n" + tunnelInstallUsage()
		}
		args.LocalPort = port
	case strings.HasPrefix(token, "alias:"):
		aliasToken := strings.TrimPrefix(token, "alias:")
		if aliasToken != "" && !strings.HasPrefix(aliasToken, "$") {
			aliasToken = "$" + aliasToken
		}
		alias, msg := requireAlias(aliasToken)
		if msg != "" {
			return msg
		}
		args.Alias = alias
	case strings.HasPrefix(token, "env:"):
		env, msg := parseTunnelEnvironment(strings.TrimPrefix(token, "env:"))
		if msg != "" {
			return msg + "\n\n" + tunnelInstallUsage()
		}
		args.Environment = env
	case strings.HasPrefix(token, "service:"):
		_, value, _ := strings.Cut(token, ":")
		if !dockerComposeServicePattern.MatchString(value) {
			return "service must use letters, numbers, underscores, or hyphens.\n\n" + tunnelInstallUsage()
		}
		args.WebRef = value
		args.WebRefKind = tunnelWebRefKindService
	case strings.HasPrefix(token, "container:"), strings.HasPrefix(token, "web_container:"):
		_, value, _ := strings.Cut(token, ":")
		if !dockerContainerRefPattern.MatchString(value) {
			return "container/web_container must use letters, numbers, dots, underscores, or hyphens.\n\n" + tunnelInstallUsage()
		}
		args.WebRef = value
		args.WebRefKind = tunnelWebRefKindContainer
	default:
		return tunnelInstallUsage()
	}
	return ""
}

func tunnelInstallUsage() string {
	return strings.Join([]string{
		"Usage:",
		"• `/qurl tunnel install` for guided setup",
		"• `/qurl tunnel install <slug|$slug> [port:8080] [alias:$alias] [env:docker|docker-compose|ecs-fargate|kubernetes] [container:<name>|service:<name>|web_container:<name>]`",
		"`env:compose` is accepted as shorthand for `env:docker-compose`.",
		"Example: `/qurl tunnel install prod-dashboard port:8080`",
	}, "\n")
}

func parseTunnelEnvironment(raw string) (env tunnelInstallEnvironment, userMsg string) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(tunnelEnvDocker):
		return tunnelEnvDocker, ""
	case string(tunnelEnvCompose), tunnelEnvComposeAlt:
		return tunnelEnvCompose, ""
	case string(tunnelEnvECSFargate):
		return tunnelEnvECSFargate, ""
	case string(tunnelEnvKubernetes):
		return tunnelEnvKubernetes, ""
	default:
		return "", "env must be one of docker, docker-compose, ecs-fargate, or kubernetes; compose is accepted as shorthand for docker-compose."
	}
}

func (e tunnelInstallEnvironment) label() (string, error) {
	switch e {
	case tunnelEnvDocker:
		return "Docker sidecar", nil
	case tunnelEnvCompose:
		return "Docker Compose", nil
	case tunnelEnvECSFargate:
		return "AWS ECS/Fargate", nil
	case tunnelEnvKubernetes:
		return "Kubernetes", nil
	default:
		return "", fmt.Errorf("unreachable tunnel install environment: %s", e)
	}
}

// handleTunnel routes `/qurl tunnel install` and `/qurl tunnel install <slug>`.
func (h *Handler) handleTunnel(w http.ResponseWriter, values url.Values) {
	text := strings.TrimSpace(values.Get(fieldText))
	if tunnelInstallWizardRequest(text) {
		h.handleTunnelInstallWizard(w, values)
		return
	}

	args, userMsg := parseTunnelInstall(text)
	if userMsg != "" {
		respondSlack(w, userMsg)
		return
	}
	if !h.requireAdminStoreSync(w) {
		return
	}
	if h.aliasStore == nil {
		respondSlack(w, "Channel shortcut storage is not configured on this Slack bot deployment. Contact the operator.")
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

	setupStartedAt := tunnelBootstrapNow()
	h.runAsync(w, "tunnel_install", values, func(ctx context.Context, log *slog.Logger) {
		h.processTunnelInstall(ctx, log, teamID, channelID, userID, values.Get(fieldResponseURL), args, setupStartedAt)
	})
}

func tunnelInstallWizardRequest(text string) bool {
	matched, rest := slashVerb(strings.TrimSpace(text), "tunnel")
	if !matched {
		return false
	}
	fields := strings.Fields(rest)
	if len(fields) != 1 || fields[0] != "install" {
		return false
	}
	return true
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
	triggerReceivedAt := tunnelBootstrapNow()
	if !h.startAsyncWorker(log, func(ctx context.Context, log *slog.Logger) {
		h.openTunnelInstallWizard(ctx, log, teamID, channelID, userID, triggerID, values.Get(fieldResponseURL), triggerReceivedAt)
	}) {
		respondSlack(w, ackBusy)
		return
	}
	// Acknowledge before the async admin check so Slack's short trigger_id
	// window is preserved for views.open. Denials and open failures are sent
	// back through response_url by openTunnelInstallWizard. This intentionally
	// differs from typed tunnel installs: the guided path may briefly show
	// "checking" to a non-admin so the admin-gate latency does not spend the
	// trigger_id before the modal can open.
	respondSlack(w, "Checking admin permissions, then opening guided tunnel setup…")
}

func (h *Handler) openTunnelInstallWizard(ctx context.Context, log *slog.Logger, teamID, channelID, userID, triggerID, responseURL string, triggerReceivedAt time.Time) {
	triggerElapsed := tunnelBootstrapNow().Sub(triggerReceivedAt)
	if triggerElapsed < 0 {
		triggerElapsed = 0
	}
	log = log.With(
		"slack_trigger_elapsed_ms", triggerElapsed.Milliseconds(),
		"slack_views_open_budget_ms", slackTriggerOpenViewBudget.Milliseconds(),
		"admin_gate_budget_ms", adminGateBudget.Milliseconds(),
	)
	// adminGateBudget + slackTriggerOpenViewBudget intentionally fit inside
	// Slack's roughly three-second trigger_id window. The admin store is the
	// dominant tail-latency risk before views.open; expiry is converted into a
	// retry prompt instead of leaving the admin with a silent modal miss.
	// startAsyncWorker already derives ctx from h.baseCtx, so shutdown cancels
	// this check coherently with the rest of the async slash-command work.
	adminCtx, cancel := context.WithTimeout(ctx, adminGateBudget)
	isAdmin, _, err := h.cfg.AdminStore.CheckAdmin(adminCtx, teamID, userID)
	cancel()
	if err != nil {
		log.Error("tunnel install wizard admin check failed", "error", err)
		h.postErrorResponse(log, responseURL, "Could not verify admin status. Retry in a moment.", true)
		return
	}
	if !isAdmin {
		log.Warn("tunnel install wizard denied: non-admin")
		h.postErrorResponse(log, responseURL, "This command is admin-only.", true)
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
		h.postErrorResponse(log, responseURL, "Could not open guided tunnel setup. Please retry or contact support.", true)
		return
	}
	openCtx, openCancel := context.WithTimeout(ctx, slackTriggerOpenViewBudget)
	defer openCancel()
	if err := h.cfg.OpenView(openCtx, teamID, triggerID, view); err != nil {
		log.Error("tunnel install wizard views.open failed",
			"error", err,
			"slack_trigger_expired", errors.Is(err, ErrSlackTriggerExpired),
			"slack_views_open_deadline_exceeded", errors.Is(err, context.DeadlineExceeded),
			"slack_rate_limited", errors.Is(err, ErrSlackRateLimited),
		)
		switch {
		case errors.Is(err, ErrSlackTriggerExpired):
			h.postErrorResponse(log, responseURL, "Slack's setup window expired before the modal opened. Run `/qurl tunnel install` again.", true)
		case errors.Is(err, context.DeadlineExceeded):
			h.postErrorResponse(log, responseURL, "Slack did not respond before the setup window expired. Run `/qurl tunnel install` again.", true)
		case errors.Is(err, ErrSlackRateLimited):
			h.postErrorResponse(log, responseURL, tunnelInstallRateLimitMessage(err), true)
		default:
			h.postErrorResponse(log, responseURL, "Could not open guided tunnel setup. Please retry or contact support.", true)
		}
		return
	}
	h.deleteOriginalResponse(log, responseURL)
}

func (h *Handler) processTunnelInstall(ctx context.Context, log *slog.Logger, teamID, channelID, userID, responseURL string, args *tunnelInstallArgs, setupStartedAt time.Time) {
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

	preparedMessage, err := h.prepareTunnelInstallMessage(args)
	if err != nil {
		log.Error("tunnel install: render preflight failed", "error", err, "slug", args.Slug, "resource_id", resource.ResourceID)
		h.postResponse(log, responseURL, "Tunnel setup could not render the install instructions. No bootstrap key was minted. Please retry or contact support.")
		return
	}

	key, err := c.CreateAPIKey(ctx, &client.CreateAPIKeyInput{
		Name:           "Slack tunnel bootstrap " + args.Slug,
		Scopes:         []string{tunnelScopeAgent, tunnelScopeWrite},
		Purpose:        client.APIKeyPurposeTunnelBootstrap,
		TunnelSlug:     args.Slug,
		ExpiresIn:      tunnelBootstrapTTL,
		IdempotencyKey: tunnelBootstrapIdempotencyKey(teamID, channelID, userID, args.Slug, setupStartedAt),
	})
	if err != nil {
		log.Error("tunnel install: bootstrap key mint failed", "error", err, "slug", args.Slug, "resource_id", resource.ResourceID)
		h.postResponse(log, responseURL, sanitizeAPIError(err, "Failed to mint a tunnel bootstrap key"))
		return
	}
	if key.APIKey == "" {
		log.Error("tunnel install: create api key response missing plaintext", "slug", args.Slug, "resource_id", resource.ResourceID, "key_id", key.KeyID)
		revokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, c, key, "missing_plaintext")
		h.postResponse(log, responseURL, "The qURL API did not return a bootstrap key. Please retry or contact support.")
		return
	}
	if err := validateBootstrapAPIKeyForShell(key.APIKey); err != nil {
		log.Error("tunnel install: create api key response was not shell-renderable", "error", err, "slug", args.Slug, "resource_id", resource.ResourceID, "key_id", key.KeyID)
		revokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, c, key, "shell_validation_failed")
		h.postResponse(log, responseURL, "The qURL API returned a bootstrap key in an unexpected format. Please retry or contact support.")
		return
	}

	msg, err := preparedMessage.render(args, key, aliasStatus)
	if err != nil {
		log.Error("tunnel install: render failed after bootstrap key mint", "error", err, "slug", args.Slug, "resource_id", resource.ResourceID, "key_id", key.KeyID)
		revokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, c, key, "message_render_failed")
		h.postResponse(log, responseURL, "Tunnel setup could not render the install instructions. The temporary bootstrap key was revoked. Please retry or contact support.")
		return
	}
	log.Info("tunnel install succeeded", "slug", args.Slug, "shortcut", args.Alias, "environment", args.Environment, "resource_id", resource.ResourceID)
	if !h.postResponse(log, responseURL, msg) {
		// This second post is best-effort too: if Slack never accepts either
		// response_url call, the admin may see neither the install nor the
		// revoke notice. The key is still revoked because delivery was not
		// confirmed, and the structured logs retain the resource/key IDs.
		log.Error("tunnel install: Slack follow-up delivery failed after bootstrap key mint; revoking key because delivery confirmation was not received", "slug", args.Slug, "resource_id", resource.ResourceID, "key_id", key.KeyID, "slack_delivery_confirmed", false, "slack_delivery_may_have_persisted", true)
		revokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, c, key, "response_url_delivery_failed")
		h.postResponse(log, responseURL, "Slack did not confirm delivery of the tunnel install instructions, so the bootstrap key was revoked. Run `/qurl tunnel install` again.")
	}
}

func revokeBootstrapKeyAfterInstallFailure(parent context.Context, log *slog.Logger, c *client.Client, key *client.APIKey, reason string) {
	if key == nil || strings.TrimSpace(key.KeyID) == "" {
		log.Warn("tunnel install: cannot revoke bootstrap key after install failure; missing key_id", "event", "tunnel_bootstrap_cleanup_skipped", "reason", reason)
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	// Use the handler base context instead of the request context so a canceled
	// Slack request cannot strand a freshly minted bootstrap key, while process
	// shutdown can still cancel cleanup. Keep this synchronous while install
	// work runs behind a bounded semaphore: it trades up to a few seconds of
	// worker occupancy on rare render failures for deterministic cleanup before
	// the user sees a retry prompt. If the cleanup endpoint stalls under
	// saturation, back-pressure is visible through the existing async-worker
	// pool instead of spawning unbounded cleanup work.
	ctx, cancel := context.WithTimeout(parent, tunnelBootstrapCleanupTimeout)
	defer cancel()
	if err := c.RevokeAPIKey(ctx, key.KeyID); err != nil {
		log.Error("tunnel install: bootstrap key cleanup failed after install failure", "error", err, "event", "tunnel_bootstrap_cleanup_failed", "key_id", key.KeyID, "reason", reason)
		return
	}
	log.Info("tunnel install: revoked bootstrap key after install failure", "event", "tunnel_bootstrap_cleanup_succeeded", "key_id", key.KeyID, "reason", reason)
}

func tunnelBootstrapIdempotencyKey(teamID, channelID, userID, slug string, now time.Time) string {
	// Bucket every install path on the modal TTL window, not the API key TTL.
	// Modal submissions pass the modal creation timestamp so duplicate submits
	// for one still-valid modal cannot shift buckets just because the async
	// worker runs later. Typed slash-command retries pass the command time; if a
	// typed retry crosses the same 25-minute bucket boundary it may mint a fresh
	// key, which is acceptable because no Slack modal replay contract exists for
	// that path.
	windowSeconds := int64(tunnelInstallModalTTL / time.Second)
	if windowSeconds <= 0 {
		windowSeconds = 1
	}
	bucket := now.UTC().Unix() / windowSeconds
	return IdempotencyKey(teamID, channelID, userID, fmt.Sprintf("tunnel-bootstrap:%s:%d", slug, bucket))
}

func (h *Handler) ensureTunnelAlias(ctx context.Context, teamID, channelID, alias, resourceID string) (string, error) {
	existing, found, err := h.cfg.AdminStore.LookupChannelAlias(ctx, teamID, channelID, alias)
	if err != nil {
		return ":warning: failed to check the existing channel alias; no bootstrap key was minted.", err
	}
	if found {
		if existing == resourceID {
			return fmt.Sprintf("qURL shortcut `$%s` is ready in this channel.", alias), nil
		}
		return fmt.Sprintf("qURL shortcut `$%s` is already used in this channel. Run `/qurl unset-alias $%s` first, or pick a different shortcut.", alias, alias), slackdata.ErrAliasAlreadyBound
	}
	if err := h.aliasStore.BindChannelAlias(ctx, teamID, channelID, alias, resourceID); err != nil {
		if errors.Is(err, slackdata.ErrAliasAlreadyBound) {
			// A concurrent retry may have created the same binding after our
			// optimistic read. Confirm that benign race before surfacing a
			// conflict to the admin.
			existing, found, lookupErr := h.cfg.AdminStore.LookupChannelAlias(ctx, teamID, channelID, alias)
			if lookupErr == nil && found && existing == resourceID {
				return fmt.Sprintf("qURL shortcut `$%s` is ready in this channel.", alias), nil
			}
		}
		return ":warning: failed to bind the channel shortcut; no bootstrap key was minted.", err
	}
	return fmt.Sprintf("qURL shortcut `$%s` is ready in this channel.", alias), nil
}

type preparedTunnelInstallMessage struct {
	imageNote        string
	imageLine        string
	environmentLabel string
	instructions     string
}

func (h *Handler) prepareTunnelInstallMessage(args *tunnelInstallArgs) (preparedTunnelInstallMessage, error) {
	image := strings.TrimSpace(h.cfg.TunnelImage)
	usingDefaultImage := image == ""
	if image == "" {
		image = defaultTunnelImage
	}
	environmentLabel, err := args.Environment.label()
	if err != nil {
		return preparedTunnelInstallMessage{}, err
	}
	instructions, err := h.renderTunnelInstallInstructions(args, image)
	if err != nil {
		return preparedTunnelInstallMessage{}, err
	}
	imageNote := tunnelImageNote(usingDefaultImage)
	if imageNote != "" {
		imageNote = "\n\n" + imageNote
	}
	return preparedTunnelInstallMessage{
		imageNote:        imageNote,
		imageLine:        fmt.Sprintf("Sidecar image: `%s`.", image),
		environmentLabel: environmentLabel,
		instructions:     instructions,
	}, nil
}

func (p preparedTunnelInstallMessage) render(args *tunnelInstallArgs, key *client.APIKey, aliasStatus string) (string, error) {
	if key == nil {
		return "", errors.New("bootstrap api key is missing")
	}
	if err := validateBootstrapAPIKeyForShell(key.APIKey); err != nil {
		return "", err
	}
	keyBlock, err := slackCodeBlock(key.APIKey)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Tunnel `%s` is ready to install.\n", args.Slug)
	b.WriteString(aliasStatus)
	fmt.Fprintf(&b, "\n\nBootstrap key %s. The shell block below prompts for it; do not add the key to the shell text itself. Paste it only when prompted or into your secret manager. If a terminal echoes pasted input, stop and use a platform secret manager instead.\n\n", tunnelBootstrapExpiryLabel(key))
	b.WriteString(keyBlock)
	b.WriteString(p.imageNote)
	b.WriteString("\n\n")
	b.WriteString(p.imageLine)
	fmt.Fprintf(&b, "\nTarget environment: %s.\n\n", p.environmentLabel)
	b.WriteString(p.instructions)
	b.WriteString("\n\nTreat this ephemeral Slack message as a secret until the sidecar connects. After the first successful start, remove the mounted bootstrap key from the runtime. Keep the qURL agent-state directory, volume, or PVC; it stores the sidecar identity used on future restarts.\n\n")
	fmt.Fprintf(&b, "Then users can run `/qurl get $%s`.", args.Alias)
	return b.String(), nil
}

func (h *Handler) renderTunnelInstallMessage(args *tunnelInstallArgs, key *client.APIKey, aliasStatus string) (string, error) {
	// Convenience wrapper for focused tests; production uses
	// prepareTunnelInstallMessage(...).render(...) so render failures before
	// CreateAPIKey cannot strand a bootstrap key.
	prepared, err := h.prepareTunnelInstallMessage(args)
	if err != nil {
		return "", err
	}
	return prepared.render(args, key, aliasStatus)
}

func tunnelImageNote(usingDefaultImage bool) string {
	if !usingDefaultImage {
		return ""
	}
	return ":warning: Image: using the dev/sandbox fallback `" + defaultTunnelImage + "`. Set `QURL_TUNNEL_IMAGE` to an immutable release tag or digest before production rollout, for example `ghcr.io/layervai/qurl-reverse-tunnel-client@sha256:<digest>`."
}

func tunnelInstallRateLimitMessage(err error) string {
	retryAfter := slackRetryAfterLabel(SlackRateLimitRetryAfter(err))
	if retryAfter == "" {
		return "Slack rate-limited guided tunnel setup. Wait a moment, then run `/qurl tunnel install` again."
	}
	return "Slack rate-limited guided tunnel setup. Wait " + retryAfter + ", then run `/qurl tunnel install` again."
}

func slackRetryAfterLabel(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		// Slack documents Retry-After as integer seconds. Treat any other
		// shape as untrusted display text and fall back to generic retry copy.
		return ""
	}
	if time.Duration(seconds)*time.Second > slackRetryAfterDisplayCap {
		return "at least " + humanSlackRetryAfterDuration(slackRetryAfterDisplayCap)
	}
	if seconds >= 60 {
		minutes := seconds / 60
		remainingSeconds := seconds % 60
		minuteLabel := "minutes"
		if minutes == 1 {
			minuteLabel = "minute"
		}
		if remainingSeconds == 0 {
			return fmt.Sprintf("%d %s", minutes, minuteLabel)
		}
		secondLabel := "seconds"
		if remainingSeconds == 1 {
			secondLabel = "second"
		}
		return fmt.Sprintf("%d %s %d %s", minutes, minuteLabel, remainingSeconds, secondLabel)
	}
	if seconds == 1 {
		return "1 second"
	}
	return fmt.Sprintf("%d seconds", seconds)
}

func (h *Handler) renderTunnelInstallInstructions(args *tunnelInstallArgs, image string) (string, error) {
	// Instructions deliberately do not receive the plaintext bootstrap key:
	// prepareTunnelInstallMessage can preflight all environment-specific
	// rendering before CreateAPIKey, and the final message adds the secret in
	// one audited code block after the key shape is validated.
	switch args.Environment {
	case tunnelEnvECSFargate:
		return renderECSFargateTunnelInstructions(args, image)
	case tunnelEnvKubernetes:
		return renderKubernetesTunnelInstructions(args, image)
	case tunnelEnvCompose:
		return renderDockerComposeTunnelInstructions(args, image)
	case tunnelEnvDocker:
		return renderDockerTunnelInstructions(args, image)
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

func renderPortablePipefailShell() string {
	return `if (set -o pipefail) 2>/dev/null; then
  set -o pipefail
fi`
}

func renderBootstrapKeyPromptShell() string {
	return `if [ -z "${QURL_BOOTSTRAP_KEY:-}" ]; then
  if [ ! -t 0 ]; then
    echo "Set QURL_BOOTSTRAP_KEY or run this block from an interactive terminal." >&2
    exit 1
  fi
  printf 'Paste qURL bootstrap key (input hidden): ' >&2
  STTY_STATE="$(stty -g 2>/dev/null || true)"
  if [ -n "$STTY_STATE" ]; then
    stty -echo
  fi
  if ! IFS= read -r QURL_BOOTSTRAP_KEY; then
    if [ -n "$STTY_STATE" ]; then
      stty "$STTY_STATE"
    fi
    printf '\n' >&2
    echo "Bootstrap key is required." >&2
    exit 1
  fi
  if [ -n "$STTY_STATE" ]; then
    stty "$STTY_STATE"
  fi
  printf '\n' >&2
fi
if [ -z "$QURL_BOOTSTRAP_KEY" ]; then
  echo "Bootstrap key is required." >&2
  exit 1
fi`
}

func renderBootstrapKeyFileInstallShell(targetPath string) string {
	// Avoid passing the bootstrap key as a command argument: under some shells
	// printf may be external, which would briefly expose the secret in argv.
	return fmt.Sprintf(`QURL_BOOTSTRAP_KEY_LEN=${#QURL_BOOTSTRAP_KEY}
$SUDO sh -c 'set -eu
umask 077
head -c "$2" > "$1"
chown 65532:65532 "$1"
chmod 0400 "$1"
' _ %s "$QURL_BOOTSTRAP_KEY_LEN" <<QURL_BOOTSTRAP_KEY_EOF
$QURL_BOOTSTRAP_KEY
QURL_BOOTSTRAP_KEY_EOF
unset QURL_BOOTSTRAP_KEY QURL_BOOTSTRAP_KEY_LEN`, targetPath)
}

func renderBootstrapKeyPipeShell(command string) string {
	// Stream exactly the key byte count from a here-doc so the trailing heredoc
	// newline is not part of the secret and the key never appears in argv.
	return fmt.Sprintf(`QURL_BOOTSTRAP_KEY_LEN=${#QURL_BOOTSTRAP_KEY}
head -c "$QURL_BOOTSTRAP_KEY_LEN" <<QURL_BOOTSTRAP_KEY_EOF | %s
$QURL_BOOTSTRAP_KEY
QURL_BOOTSTRAP_KEY_EOF
unset QURL_BOOTSTRAP_KEY QURL_BOOTSTRAP_KEY_LEN`, command)
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
			return "expires very soon"
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
	return humanDurationCeilMinutes(d)
}

func humanSlackRetryAfterDuration(d time.Duration) string {
	return humanDurationCeilMinutes(d)
}

func humanDurationCeilMinutes(d time.Duration) string {
	if d < time.Minute {
		return "under 1 minute"
	}
	// Ceil to the next minute so near-boundary keys never display as
	// "0 minutes" or understate the operator's remaining setup window.
	minutesTotal := int((d + time.Minute - 1) / time.Minute)
	hours := minutesTotal / 60
	minutes := minutesTotal % 60
	hourUnit := "hours"
	if hours == 1 {
		hourUnit = "hour"
	}
	minuteUnit := "minutes"
	if minutes == 1 {
		minuteUnit = "minute"
	}
	switch {
	case hours > 0 && minutes > 0:
		return fmt.Sprintf("%d %s %d %s", hours, hourUnit, minutes, minuteUnit)
	case hours > 0:
		return fmt.Sprintf("%d %s", hours, hourUnit)
	default:
		return fmt.Sprintf("%d %s", minutes, minuteUnit)
	}
}

func validateBootstrapAPIKeyForShell(apiKey string) error {
	// shellSingleQuote safely quotes arbitrary text. This check is an
	// additional output-surface guard: qurl-service bootstrap keys must be
	// printable single-line ASCII tokens. ASCII keeps ${#QURL_BOOTSTRAP_KEY}
	// and head -c byte counts aligned across shells/locales. A dollar sign is
	// safe because POSIX single quotes prevent shell expansion in Docker paths;
	// the prompt-based heredoc renderers also read a fixed byte count from
	// stdin, and the substituted bytes are not reparsed by the shell.
	if apiKey == "" {
		return errors.New("empty api key")
	}
	for _, r := range apiKey {
		if r == '\'' || r == '`' || r < 0x20 || r > 0x7e {
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

func yamlSingleQuoted(s string) (string, error) {
	for _, r := range s {
		if r == '\n' || r == '\r' || r < 0x20 || r == 0x7f {
			return "", errors.New("yaml scalar contains control characters or newlines")
		}
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'", nil
}

// ValidateTunnelImageRef checks the operator-provided image reference shown in
// install snippets. Empty is valid and means the handler will use its fallback.
// This is intentionally stricter than Docker's full reference grammar: the bot
// controls the image source, and rejecting shell/YAML syntax-bearing bytes
// keeps generated Slack install blocks inspectable and boring.
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

func slackCodeBlock(body string) (string, error) {
	// Slack cannot escape a nested triple-backtick fence inside a code block.
	// Return an error instead of panicking so request-path callers can fail
	// closed if a future renderer accidentally includes unsanitized input.
	if strings.Contains(body, "```") {
		return "", errors.New("slack code block body contains nested triple-backtick fence")
	}
	return "```\n" + body + "\n```", nil
}
