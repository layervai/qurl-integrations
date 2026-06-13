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

	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	// Production deploys should set QURL_CONNECTOR_IMAGE to an immutable
	// release tag or digest. The floating fallback is for dev/sandbox
	// onboarding where the latest sidecar build is intentional.
	defaultTunnelImage            = "ghcr.io/layervai/qurl-connector:latest"
	defaultTunnelLocalPort        = 8080
	tunnelBootstrapTTL            = "1h"
	tunnelBootstrapSkew           = 2 * time.Minute
	tunnelBootstrapCleanupTimeout = 5 * time.Second
	// Slack response_url values are valid for roughly 30 minutes; keep modal
	// submissions inside that window so async install errors can still reach
	// the admin after Slack accepts the view submission. This is intentionally
	// shorter than tunnelBootstrapTTL so any submitted modal still leaves setup
	// headroom after the one-time bootstrap key is minted; a modal submitted at
	// the end of this window still leaves roughly 35 minutes on the bootstrap
	// key for the operator to start the sidecar.
	tunnelInstallModalTTL = 25 * time.Minute
	// Slack trigger_ids expire after roughly three seconds. The slash-command
	// ack now happens before views.open; the call budget below leaves room for
	// the admin check while reducing false expiry on normal Slack Web API tail
	// latency.
	slackTriggerMaxAge = 3 * time.Second
	// slackTriggerOpenViewBudget is the per-call cap inside slackTriggerMaxAge.
	// Keep adminGateBudget + this value below slackTriggerMaxAge so guided setup
	// has room for the admin re-check and the views.open RPC.
	slackTriggerOpenViewBudget = 1500 * time.Millisecond
	slackRetryAfterDisplayCap  = 5 * time.Minute
	tunnelScopeAgent           = "qurl:agent"
	tunnelScopeWrite           = "qurl:write"
	tunnelEnvAPIKey            = "QURL_API_KEY"
	kubernetesNameMaxLen       = 63
	// Hex chars appended to truncated Kubernetes object names. Twelve hex
	// chars is 48 bits (2^48 ~= 3e14), keeping collision risk negligible for
	// expected workspace slug volume.
	kubernetesNameHashHexLen = 12
)

var tunnelSlugPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}[a-z0-9]$`)

// Docker does not publish a tight practical length limit for container names;
// keep Slack input bounded so an accidental paste cannot dominate the rendered
// install snippets. Compose service names reject dots even though raw Docker
// container refs allow them.
var dockerContainerRefPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
var dockerComposeServicePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

type tunnelInstallEnvironment string
type tunnelInstallWebRefKind string

const (
	tunnelEnvDocker     tunnelInstallEnvironment = "docker"
	tunnelEnvCompose    tunnelInstallEnvironment = "docker-compose"
	tunnelEnvECSFargate tunnelInstallEnvironment = "ecs-fargate"
	tunnelEnvKubernetes tunnelInstallEnvironment = "kubernetes"
	// Input-only shorthand; parseTunnelEnvironment normalizes this spelling to
	// tunnelEnvCompose so renderers never receive a second Compose value.
	tunnelEnvComposeAlt string = "compose"

	tunnelWebRefKindNone      tunnelInstallWebRefKind = ""
	tunnelWebRefKindContainer tunnelInstallWebRefKind = "container"
	tunnelWebRefKindService   tunnelInstallWebRefKind = "service"
)

const (
	// Connector reasons round-trip through Slack private_metadata, unlike the
	// confirm-card audit path; keep this cap path-specific so metadata stays
	// bounded without changing other agent audit reasons.
	agentConnectorAuditReasonMaxRunes                           = 240
	agentConnectorAuditWriteTimeout                             = 5 * time.Second
	agentProtectConnectorAuditOutcome                           = "qURL Connector setup generated."
	agentProtectConnectorAuditBootstrapDMDeliveryFailedOutcome  = "qURL Connector setup generated, but Slack could not deliver the bootstrap-key DM and the bootstrap key was revoked."
	agentProtectConnectorAuditInstructionsDeliveryFailedOutcome = "qURL Connector setup generated, but Slack could not confirm install-instructions delivery and the bootstrap key was revoked."
	agentProtectConnectorAuditBuildFailedOutcome                = "qURL Connector setup failed before install instructions were delivered."
	agentProtectConnectorAuditAdminRejectedOutcome              = "qURL Connector setup was not started because the modal submitter was not verified as a qURL admin."
)

type tunnelInstallArgs struct {
	Slug        string
	Alias       string
	LocalPort   int
	Environment tunnelInstallEnvironment
	WebRef      string
	// WebRefKind is parse-time grammar metadata for cross-field validation.
	// Renderers intentionally consume only WebRef after validation succeeds.
	WebRefKind tunnelInstallWebRefKind
}

type tunnelInstallRequest struct {
	teamID       string
	enterpriseID string
	channelID    string
	userID       string
	responseURL  string
	args         *tunnelInstallArgs
	attemptID    string
	agentAudit   *tunnelInstallAgentAudit
}

type tunnelInstallAgentAudit struct {
	target string
	reason string
}

// parseTunnelInstall parses the typed (power-user) form of the connector verb:
// `/qurl-admin protect-connector <id> [env:…] [port:…] [alias:…] [container:|service:…]`.
// The verb is a single hyphenated word — there is no `install` sub-word — so the
// id is the first positional token after the verb and options follow. Bare
// `protect-connector` (no positional) is the guided modal, routed before this by
// tunnelInstallWizardRequest, so a missing id here is a usage error.
func parseTunnelInstall(text string) (args *tunnelInstallArgs, userMsg string) {
	matched, rest := slashVerb(strings.TrimSpace(text), adminVerbProtectConnector)
	if !matched {
		return nil, tunnelInstallUsage()
	}
	fields := strings.Fields(rest)
	if len(fields) < 1 {
		return nil, tunnelInstallUsage()
	}
	slug := strings.TrimPrefix(fields[0], "$")
	args = &tunnelInstallArgs{
		Slug:        slug,
		Alias:       slug,
		LocalPort:   defaultTunnelLocalPort,
		Environment: tunnelEnvDocker,
	}
	if !tunnelSlugPattern.MatchString(args.Slug) {
		return nil, "qURL Connector ID must be 3-64 chars, lowercase letters/numbers/hyphens, start with a letter, and end with a letter or number.\n\n" + tunnelInstallUsage()
	}
	for _, token := range fields[1:] {
		if msg := parseTunnelInstallOption(args, token); msg != "" {
			return nil, msg
		}
	}
	if msg := validateTunnelInstallArgs(args); msg != "" {
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
		"• `/qurl-admin protect-connector` for guided setup",
		"Guided setup is exactly `/qurl-admin protect-connector` with no arguments; add arguments only when using typed setup.",
		"• Docker: `/qurl-admin protect-connector <id|$id> [port:8080] [alias:$alias] [env:docker] [container:<name>|web_container:<name>]`",
		"• Compose: `/qurl-admin protect-connector <id|$id> env:docker-compose [port:8080] [alias:$alias] [service:<name>]`",
		"• ECS/Fargate or Kubernetes: `/qurl-admin protect-connector <id|$id> env:ecs-fargate|kubernetes [port:8080] [alias:$alias]`",
		"`env:compose` is accepted as shorthand for `env:docker-compose`.",
		"Example: `/qurl-admin protect-connector prod-dashboard port:8080`",
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

// handleExposeConnector routes the connector verb `/qurl-admin protect-connector`:
// bare (no arguments) opens the guided connector modal; `protect-connector <id>
// [opts]` is the typed power-user form that skips the modal. This is the
// single-word rename of the former two-word `tunnel install` (the resource type
// is still a "tunnel" internally — see client.ResourceTypeTunnel).
func (h *Handler) handleExposeConnector(w http.ResponseWriter, values url.Values) {
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
		respondSlack(w, "Channel alias storage is not configured on this Secure Access Agent deployment. Contact the operator.")
		return
	}
	teamID := strings.TrimSpace(values.Get(fieldTeamID))
	enterpriseID := strings.TrimSpace(values.Get(fieldEnterpriseID))
	userID := strings.TrimSpace(values.Get(fieldUserID))
	channelID := strings.TrimSpace(values.Get(fieldChannelID))
	if channelID == "" {
		respondSlack(w, ":warning: missing channel_id in slash command payload")
		return
	}
	if !h.requireAdminSync(w, teamID, userID, AdminActionExposeConnector) {
		return
	}

	setupStartedAt := h.now()
	attemptID := tunnelBootstrapTypedAttemptID(values.Get(fieldTriggerID), setupStartedAt)
	h.runAsync(w, "tunnel_install", values, func(ctx context.Context, log *slog.Logger) {
		h.processTunnelInstall(ctx, log, &tunnelInstallRequest{
			teamID:       teamID,
			enterpriseID: enterpriseID,
			channelID:    channelID,
			userID:       userID,
			responseURL:  values.Get(fieldResponseURL),
			args:         args,
			attemptID:    attemptID,
		})
	})
}

// tunnelInstallWizardRequest reports whether the text is a bare
// `/qurl-admin protect-connector` (no positional id, no options) — the guided
// modal entry. Any trailing token routes to the typed parser instead.
func tunnelInstallWizardRequest(text string) bool {
	matched, rest := slashVerb(strings.TrimSpace(text), adminVerbProtectConnector)
	if !matched {
		return false
	}
	return strings.TrimSpace(rest) == ""
}

func (h *Handler) handleTunnelInstallWizard(w http.ResponseWriter, values url.Values) {
	if !h.requireAdminStoreSync(w) {
		return
	}
	if h.aliasStore == nil {
		respondSlack(w, "Channel alias storage is not configured on this Secure Access Agent deployment. Contact the operator.")
		return
	}
	if h.cfg.OpenView == nil {
		respondSlack(w, "Guided qURL Connector setup is not configured on this Secure Access Agent deployment. Use `/qurl-admin protect-connector <id> [port:8080]` instead.")
		return
	}
	teamID := strings.TrimSpace(values.Get(fieldTeamID))
	enterpriseID := strings.TrimSpace(values.Get(fieldEnterpriseID))
	userID := strings.TrimSpace(values.Get(fieldUserID))
	channelID := strings.TrimSpace(values.Get(fieldChannelID))
	if channelID == "" {
		respondSlack(w, ":warning: missing channel_id in slash command payload")
		return
	}
	triggerID := strings.TrimSpace(values.Get(fieldTriggerID))
	if triggerID == "" {
		respondSlack(w, "Slack did not include a trigger_id, so guided setup could not open. Use `/qurl-admin protect-connector <id> [port:8080]` instead.")
		return
	}
	log := slog.With(
		"command", "tunnel_install_wizard",
		"team_id", teamID,
		"enterprise_id", enterpriseID,
		"channel_id", channelID,
		"user_id", userID,
		"trigger_id", triggerID,
	)
	triggerReceivedAt := h.now()
	if !h.startAsyncWorker(log, func(ctx context.Context, log *slog.Logger) {
		h.openTunnelInstallWizard(ctx, log, teamID, enterpriseID, channelID, userID, triggerID, values.Get(fieldResponseURL), triggerReceivedAt)
	}) {
		respondSlack(w, ackBusy)
		return
	}
	// Acknowledge before the async admin check so Slack's short trigger_id
	// window is preserved for views.open. Denials and open failures are sent
	// back through response_url by openTunnelInstallWizard. This intentionally
	// differs from typed tunnel installs: the guided path may briefly show
	// neutral progress copy to a non-admin so admin-gate latency does not spend
	// the trigger_id before the modal can open.
	respondSlack(w, ackWorkingOnIt)
}

// tunnelInstallWizardOpenedMsg replaces the "Working on it…" ack once the guided
// qURL Connector modal opens. Slack can't delete a slash command's ephemeral
// ack, so the wizard replaces it in place (see replaceOriginalResponse).
const tunnelInstallWizardOpenedMsg = ":white_check_mark: Opened guided qURL Connector setup — complete the form to finish."

func (h *Handler) openTunnelInstallWizard(ctx context.Context, log *slog.Logger, teamID, enterpriseID, channelID, userID, triggerID, responseURL string, triggerReceivedAt time.Time) {
	triggerElapsed := h.now().Sub(triggerReceivedAt)
	if triggerElapsed < 0 {
		triggerElapsed = 0
	}
	openBudget := slackTriggerOpenViewBudgetRemaining(triggerElapsed)
	log = log.With(
		"slack_trigger_elapsed_ms", triggerElapsed.Milliseconds(),
		"slack_trigger_max_age_ms", slackTriggerMaxAge.Milliseconds(),
		"slack_views_open_budget_ms", slackTriggerOpenViewBudget.Milliseconds(),
		"slack_views_open_budget_remaining_ms", openBudget.Milliseconds(),
		"admin_gate_budget_ms", adminGateBudget.Milliseconds(),
	)
	if openBudget <= 0 {
		log.Warn("tunnel install wizard trigger expired before admin check")
		_ = h.postErrorResponse(log, responseURL, "Slack's setup window expired before the modal opened. Run `/qurl-admin protect-connector` again.", true)
		return
	}
	// adminGateBudget + slackTriggerOpenViewBudget intentionally fit inside
	// slackTriggerMaxAge. The admin store is the dominant tail-latency risk
	// before views.open; expiry is converted into a retry prompt instead of
	// wasting a Slack RPC or leaving the admin with a silent modal miss.
	// startAsyncWorker already derives ctx from h.baseCtx, so shutdown cancels
	// this check coherently with the rest of the async slash-command work.
	adminCtx, cancel := context.WithTimeout(ctx, adminGateBudget)
	isAdmin, _, err := h.cfg.AdminStore.CheckAdmin(adminCtx, teamID, userID)
	cancel()
	if err != nil {
		log.Error("tunnel install wizard admin check failed", "error", err)
		_ = h.postErrorResponse(log, responseURL, "Could not verify admin status. Retry in a moment.", true)
		return
	}
	if !isAdmin {
		log.Warn("tunnel install wizard denied: non-admin")
		_ = h.postErrorResponse(log, responseURL, "This command is admin-only.", true)
		return
	}
	view, err := TunnelInstallModal(&TunnelInstallModalMetadata{
		TeamID:        teamID,
		EnterpriseID:  enterpriseID,
		ChannelID:     channelID,
		UserID:        userID,
		ResponseURL:   responseURL,
		CreatedAtUnix: h.now().Unix(),
	})
	if err != nil {
		log.Error("tunnel install wizard modal render failed", "error", err)
		_ = h.postErrorResponse(log, responseURL, "Could not open guided qURL Connector setup. Please retry or contact support.", true)
		return
	}
	triggerElapsed = h.now().Sub(triggerReceivedAt)
	if triggerElapsed < 0 {
		triggerElapsed = 0
	}
	openBudget = slackTriggerOpenViewBudgetRemaining(triggerElapsed)
	if openBudget <= 0 {
		log.Warn("tunnel install wizard trigger expired before views.open", "slack_trigger_elapsed_ms", triggerElapsed.Milliseconds())
		_ = h.postErrorResponse(log, responseURL, "Slack's setup window expired before the modal opened. Run `/qurl-admin protect-connector` again.", true)
		return
	}
	openCtx, openCancel := context.WithTimeout(ctx, openBudget)
	defer openCancel()
	if err := h.openViewWithGridFallback(openCtx, log, teamID, enterpriseID, triggerID, view); err != nil {
		log.Error("tunnel install wizard views.open failed",
			"error", err,
			"slack_trigger_expired", errors.Is(err, ErrSlackTriggerExpired),
			"slack_views_open_deadline_exceeded", errors.Is(err, context.DeadlineExceeded),
			"slack_rate_limited", errors.Is(err, ErrSlackRateLimited),
			"slack_bot_token_not_configured", errors.Is(err, auth.ErrSlackBotTokenNotConfigured),
		)
		switch {
		case errors.Is(err, ErrSlackTriggerExpired):
			_ = h.postErrorResponse(log, responseURL, "Slack's setup window expired before the modal opened. Run `/qurl-admin protect-connector` again.", true)
		case errors.Is(err, context.DeadlineExceeded):
			_ = h.postErrorResponse(log, responseURL, "Slack did not respond before the setup window expired. Run `/qurl-admin protect-connector` again.", true)
		case errors.Is(err, ErrSlackRateLimited):
			_ = h.postErrorResponse(log, responseURL, tunnelInstallRateLimitMessage(err), true)
		case errors.Is(err, auth.ErrSlackBotTokenNotConfigured):
			_ = h.postErrorResponse(log, responseURL, h.guidedTunnelSlackAppInstallMessage(), true)
		default:
			_ = h.postErrorResponse(log, responseURL, "Could not open guided qURL Connector setup. Please retry or contact support.", true)
		}
		return
	}
	_ = h.replaceOriginalResponse(log, responseURL, tunnelInstallWizardOpenedMsg)
}

// openViewWithGridFallback opens a modal with the workspace bot token and, on a
// missing-token error, retries with the Enterprise Grid org install token. The
// retry only fires for ErrSlackBotTokenNotConfigured and only when the
// enterprise ID is a distinct token owner — every other error returns
// unchanged. Shared by the guided tunnel installer and `/qurl feedback`.
func (h *Handler) openViewWithGridFallback(ctx context.Context, log *slog.Logger, teamID, enterpriseID, triggerID string, view []byte) error {
	err := h.cfg.OpenView(ctx, teamID, triggerID, view)
	if err == nil || !errors.Is(err, auth.ErrSlackBotTokenNotConfigured) {
		return err
	}
	if enterpriseID == "" || enterpriseID == teamID {
		return err
	}
	log.Warn("workspace Slack bot token missing; retrying modal with Enterprise Grid install token",
		"team_id", teamID,
		"enterprise_id", enterpriseID,
	)
	return h.cfg.OpenView(ctx, enterpriseID, triggerID, view)
}

func (h *Handler) guidedTunnelSlackAppInstallMessage() string {
	return h.latestSlackAppInstallMessage("Guided qURL Connector setup", "run `/qurl-admin protect-connector` again")
}

func (h *Handler) tunnelBootstrapDMSlackAppInstallMessage() string {
	return h.latestSlackAppInstallMessage("qURL Connector bootstrap-key DM delivery", "run `/qurl-admin protect-connector` again")
}

func (h *Handler) latestSlackAppInstallMessage(subject, retryInstruction string) string {
	subject = strings.TrimSpace(subject)
	retryInstruction = strings.TrimSpace(retryInstruction)
	installURL := strings.TrimSpace(h.cfg.SlackInstallURL)
	if installURL == "" || strings.ContainsAny(installURL, "<>|") {
		return subject + " needs the latest qURL Slack app install. Ask a workspace admin to open the qURL Slack install link your operator provided, then " + retryInstruction + "."
	}
	return subject + " needs the latest qURL Slack app install. Ask a workspace admin to open <" + installURL + "|the qURL Slack install link>, then " + retryInstruction + "."
}

func slackTriggerOpenViewBudgetRemaining(triggerElapsed time.Duration) time.Duration {
	if triggerElapsed < 0 {
		triggerElapsed = 0
	}
	remaining := slackTriggerMaxAge - triggerElapsed
	if remaining <= 0 {
		return 0
	}
	if remaining > slackTriggerOpenViewBudget {
		return slackTriggerOpenViewBudget
	}
	return remaining
}

// errMissingBootstrapPlaintext is returned by [Handler.buildTunnelInstall] when
// the qURL API accepted the key create but omitted the plaintext key.
var errMissingBootstrapPlaintext = errors.New("bootstrap key response missing plaintext")

// tunnelInstallBuild is the successful result of [Handler.buildTunnelInstall]:
// the created resource, the minted bootstrap key, key-free install instructions,
// and the secret-bearing DM body. client is retained so the caller can revoke key
// if either delivery step is never confirmed.
type tunnelInstallBuild struct {
	client        *client.Client
	resource      *client.Resource
	key           *client.APIKey
	message       string
	secretMessage string
}

// buildTunnelInstall is the qURL Connector mutation core, decoupled from how the
// result is delivered: create-or-find the tunnel resource, bind the channel
// alias, mint + validate a short-lived bootstrap key, and render the install
// instructions. Both the `/qurl-admin protect-connector` slash path
// ([Handler.processTunnelInstall]) and the conversation-mode confirm path drive
// it, so the create/mint/render logic lives in exactly one place.
//
// It does NOT gate admin — callers gate first (requireAdminSync on the slash
// path; a CheckAdmin re-check on the confirm path). On failure it returns the
// user-facing message to post plus the error, revoking any bootstrap key it
// minted if key validation or the final render fails (so a key whose install
// block never rendered doesn't stay live). On success the caller delivers
// build.message and, if delivery is not confirmed, revokes build.key via
// [revokeBootstrapKeyAfterInstallFailure].
func (h *Handler) buildTunnelInstall(ctx context.Context, log *slog.Logger, teamID, channelID, userID string, args *tunnelInstallArgs, attemptID string) (*tunnelInstallBuild, string, error) {
	c, err := h.authenticatedClient(ctx, teamID)
	if err != nil {
		log.Error("tunnel install: failed to get API key", "error", err)
		return nil, authErrorMessage(err), err
	}

	// The description doubles as the tunnel's user-facing Display Name
	// (see handleSetDisplayName — there's no separate field). Install
	// seeds it with a sensible default so every qURL Connector has a Display Name
	// from the moment it exists; admins refine it with
	// `/qurl-admin set-display-name` and revert to this default with
	// `/qurl-admin unset-display-name`. find_or_create only applies the
	// description on first create, so re-installing an admin-renamed
	// tunnel keeps the admin's Display Name.
	resource, err := c.CreateResource(ctx, &client.CreateResourceInput{
		Type:         client.ResourceTypeTunnel,
		Slug:         args.Slug,
		FindOrCreate: true,
		Description:  defaultTunnelDisplayName(args.Slug),
	})
	if err != nil {
		log.Error("tunnel install: create/find resource failed", "error", err, "slug", args.Slug)
		return nil, sanitizeAPIError(err, "Failed to create or find the qURL Connector resource"), err
	}

	// Bind/verify the channel shortcut before minting the bootstrap key so an
	// alias conflict fails without creating a secret. After the resource exists,
	// the binding is intentionally durable across later key/render/delivery
	// failures: rerunning the same install reuses the same slug+shortcut and
	// mints a fresh short-lived key.
	aliasStatus, err := h.ensureTunnelAlias(ctx, teamID, channelID, args.Alias, resource.ResourceID)
	if err != nil {
		log.Error("tunnel install: channel shortcut bind failed", "error", err, "shortcut", args.Alias, "resource_id", resource.ResourceID)
		return nil, aliasStatus, err
	}

	preparedMessage, err := h.prepareTunnelInstallMessage(args)
	if err != nil {
		log.Error("tunnel install: render preflight failed", "error", err, "slug", args.Slug, "resource_id", resource.ResourceID)
		return nil, "qURL Connector setup could not render the install instructions. No bootstrap key was minted. Please retry or contact support.", err
	}

	key, err := c.CreateAPIKey(ctx, &client.CreateAPIKeyInput{
		Name:           "Slack qURL Connector bootstrap " + args.Slug,
		Scopes:         []string{tunnelScopeAgent, tunnelScopeWrite},
		Purpose:        client.APIKeyPurposeTunnelBootstrap,
		TunnelSlug:     args.Slug,
		ExpiresIn:      tunnelBootstrapTTL,
		IdempotencyKey: tunnelBootstrapIdempotencyKey(teamID, channelID, userID, args.Slug, attemptID),
	})
	if err != nil {
		log.Error("tunnel install: bootstrap key mint failed", "error", err, "slug", args.Slug, "resource_id", resource.ResourceID)
		return nil, sanitizeAPIError(err, "Failed to mint a qURL Connector bootstrap key"), err
	}
	if key.APIKey == "" {
		log.Error("tunnel install: create api key response missing plaintext", "slug", args.Slug, "resource_id", resource.ResourceID, "key_id", key.KeyID)
		revokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, c, key, "missing_plaintext")
		return nil, "The qURL API did not return a bootstrap key. Please retry or contact support.", errMissingBootstrapPlaintext
	}
	if err := validateBootstrapAPIKeyForShell(key.APIKey); err != nil {
		log.Error("tunnel install: create api key response was not shell-renderable", "error", err, "slug", args.Slug, "resource_id", resource.ResourceID, "key_id", key.KeyID)
		revokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, c, key, "shell_validation_failed")
		return nil, "The qURL API returned a bootstrap key in an unexpected format. Please retry or contact support.", err
	}

	msg, err := preparedMessage.render(args, key, aliasStatus, resource.Description, h.now())
	if err != nil {
		log.Error("tunnel install: render failed after bootstrap key mint", "error", err, "slug", args.Slug, "resource_id", resource.ResourceID, "key_id", key.KeyID)
		revokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, c, key, "message_render_failed")
		return nil, "qURL Connector setup could not render the install instructions. The temporary bootstrap key was revoked. Please retry or contact support.", err
	}
	secretMsg, err := renderTunnelBootstrapSecretMessage(args, key, h.now())
	if err != nil {
		log.Error("tunnel install: secret message render failed after bootstrap key mint", "error", err, "slug", args.Slug, "resource_id", resource.ResourceID, "key_id", key.KeyID)
		revokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, c, key, "secret_message_render_failed")
		return nil, "qURL Connector setup could not render the bootstrap-key DM. The temporary bootstrap key was revoked. Please retry or contact support.", err
	}

	return &tunnelInstallBuild{client: c, resource: resource, key: key, message: msg, secretMessage: secretMsg}, "", nil
}

// processTunnelInstall is the async-worker body for qURL Connector setup. It
// runs the shared mutation core and delivers the result to Slack. The secret DM
// is delivered first so DM failure can stop before publishing install
// instructions that reference an unavailable secret; if the later
// install-instructions delivery fails, the key is revoked and the admin gets a
// best-effort discard notice. agentAudit is nil for slash-command setup because
// that path is not an agent action.
func (h *Handler) processTunnelInstall(ctx context.Context, log *slog.Logger, req *tunnelInstallRequest) {
	args := req.args
	if h.cfg.PostDM == nil {
		log.Error("tunnel install: bootstrap-key DM delivery is not configured; refusing to mint", "slug", args.Slug)
		_ = h.postResponse(log, req.responseURL, "qURL Connector setup needs Slack DM delivery for the temporary bootstrap key. No bootstrap key was minted. Ask the operator to update the qURL Slack app, then run `/qurl-admin protect-connector` again.")
		h.recordTunnelInstallAgentAudit(log, req, agentProtectConnectorAuditBuildFailedOutcome, false)
		return
	}
	build, failMsg, err := h.buildTunnelInstall(ctx, log, req.teamID, req.channelID, req.userID, args, req.attemptID)
	if err != nil {
		_ = h.postResponse(log, req.responseURL, failMsg)
		// Once the modal submit has passed validation/admin gates and reached
		// this worker, auth/resource/key/render failures are terminal agent
		// setup attempts too. buildTunnelInstall revokes any minted key before
		// returning.
		h.recordTunnelInstallAgentAudit(log, req, agentProtectConnectorAuditBuildFailedOutcome, false)
		return
	}

	log.Info("tunnel install succeeded", "slug", args.Slug, "shortcut", args.Alias, "environment", args.Environment, "resource_id", build.resource.ResourceID)
	if err := h.postTunnelInstallDM(ctx, req.teamID, req.enterpriseID, req.userID, build.secretMessage); err != nil {
		log.Error("tunnel install: Slack DM delivery failed after bootstrap key mint; revoking key before posting install instructions", "error", err, "slug", args.Slug, "resource_id", build.resource.ResourceID, "key_id", build.key.KeyID, "slack_delivery_confirmed", false)
		revokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, build.client, build.key, "dm_delivery_failed")
		message := "Slack could not deliver the qURL Connector bootstrap key by DM, so the temporary key was revoked and the install instructions were not posted."
		if errors.Is(err, ErrSlackMissingScope) {
			message += " " + h.tunnelBootstrapDMSlackAppInstallMessage()
		} else {
			message += " Re-run `/qurl-admin protect-connector` after DM delivery is available."
		}
		_ = h.postResponse(log, req.responseURL, message)
		h.recordTunnelInstallAgentAudit(log, req, agentProtectConnectorAuditBootstrapDMDeliveryFailedOutcome, false)
		return
	}
	delivered := h.postInstallInstructions(log, req.responseURL, build.message)
	if !delivered {
		// This second post is best-effort too: if Slack never accepts either
		// response_url call, the admin may see neither the install nor the
		// revoke notice. The key is still revoked because delivery was not
		// confirmed, and the structured logs retain the resource/key IDs for
		// operators investigating a disappeared install attempt.
		log.Error("tunnel install: Slack follow-up delivery failed after bootstrap key mint; revoking key because delivery confirmation was not received", "slug", args.Slug, "resource_id", build.resource.ResourceID, "key_id", build.key.KeyID, "slack_delivery_confirmed", false, "slack_delivery_may_have_persisted", true)
		revokeBootstrapKeyAfterInstallFailure(h.baseCtx, log, build.client, build.key, "response_url_delivery_failed")
		// Intentionally notify both places: the DM reaches admins who saw the key
		// first, while response_url covers the command surface if DM delivery fails.
		if err := h.postTunnelInstallDM(h.baseCtx, req.teamID, req.enterpriseID, req.userID, "The qURL Connector install instructions were not delivered, so the temporary bootstrap key from the previous DM was revoked. Discard that key and run `/qurl-admin protect-connector` again."); err != nil {
			log.Error("tunnel install: Slack discard DM delivery failed after bootstrap key revoke", "error", err, "slug", args.Slug, "resource_id", build.resource.ResourceID, "key_id", build.key.KeyID, "event", "tunnel_bootstrap_discard_dm_delivery_failed")
		}
		if !h.postResponse(log, req.responseURL, "Slack did not confirm delivery of the qURL Connector install instructions, so the bootstrap key was revoked. If the install block from this attempt appears later, discard it because its key is no longer valid. Run `/qurl-admin protect-connector` again.") {
			log.Error("tunnel install: Slack discard notice delivery failed after bootstrap key revoke", "slug", args.Slug, "resource_id", build.resource.ResourceID, "key_id", build.key.KeyID, "event", "tunnel_bootstrap_discard_notice_delivery_failed")
		}
	}

	outcome := agentProtectConnectorAuditOutcome
	if !delivered {
		outcome = agentProtectConnectorAuditInstructionsDeliveryFailedOutcome
	}
	h.recordTunnelInstallAgentAudit(log, req, outcome, delivered)
}

func tunnelInstallAgentAuditFromMetadata(meta *TunnelInstallModalMetadata, args *tunnelInstallArgs) *tunnelInstallAgentAudit {
	if meta == nil || meta.Agent == nil || meta.Agent.Action != string(agent.ActionProtectConnector) || args == nil {
		return nil
	}
	// Submit-side half of the protect-connector provenance carry-through; the
	// confirm-side half is tunnelInstallAgentMetadata.
	// Reason is untrusted proposal provenance; target is the submitted/enforced
	// connector identity from the modal, so an edited slug is recorded as the
	// setup the approver actually submitted.
	return &tunnelInstallAgentAudit{
		target: args.Slug,
		reason: normalizeTunnelInstallAgentReason(meta.Agent.Reason),
	}
}

func normalizeTunnelInstallAgentReason(reason string) string {
	return truncateRunes(strings.TrimSpace(reason), agentConnectorAuditReasonMaxRunes)
}

func (h *Handler) recordTunnelInstallAgentAudit(log *slog.Logger, req *tunnelInstallRequest, outcome string, success bool) {
	audit := req.agentAudit
	if audit == nil {
		return
	}
	baseCtx := h.baseCtx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	// Modal-submit audits intentionally do not inherit the worker ctx: setup,
	// Slack delivery, and possible revoke may have spent or canceled that ctx by
	// the time we write the review row. Give the best-effort audit its own short
	// handler-scoped budget instead.
	auditCtx, cancel := context.WithTimeout(baseCtx, agentConnectorAuditWriteTimeout)
	defer cancel()
	resultSuccess := success
	// Modal-submit audits have no public confirm card, so the legacy Outcome
	// and structured App Home Result intentionally share the same neutral text.
	// success=false on delivery failure reflects that the setup did not reach a
	// usable user-visible state; the Result text preserves that the resource was
	// generated before the bootstrap key was revoked.
	h.recordAgentAuditEntry(auditCtx, log, &agentAuditEntry{
		teamID:        req.teamID,
		actorID:       req.userID,
		action:        string(agent.ActionProtectConnector),
		target:        audit.target,
		channelID:     req.channelID,
		reason:        audit.reason,
		outcome:       outcome,
		result:        outcome,
		resultSuccess: &resultSuccess,
	})
}

func (h *Handler) postTunnelInstallDM(ctx context.Context, teamID, enterpriseID, userID, msg string) error {
	// processTunnelInstall checks this before minting; keep the helper guard so
	// any future standalone call still fails closed instead of panicking.
	if h.cfg.PostDM == nil {
		return errors.New("tunnel install DM delivery is not configured")
	}
	return h.cfg.PostDM(ctx, teamID, enterpriseID, userID, msg)
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
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			log.Info("tunnel install: bootstrap key already absent after install failure", "event", "tunnel_bootstrap_cleanup_already_absent", "key_id", key.KeyID, "reason", reason)
			return
		}
		log.Error("tunnel install: bootstrap key cleanup failed after install failure", "error", err, "event", "tunnel_bootstrap_cleanup_failed", "key_id", key.KeyID, "reason", reason)
		return
	}
	log.Info("tunnel install: revoked bootstrap key after install failure", "event", "tunnel_bootstrap_cleanup_succeeded", "key_id", key.KeyID, "reason", reason)
}

func tunnelBootstrapIdempotencyKey(teamID, channelID, userID, slug, attemptID string) string {
	// Key on the exact Slack attempt instead of a broad modal-TTL bucket. Typed
	// commands use Slack trigger_id when available, so Slack HTTP retries dedupe
	// while a human re-run after a DM-delivery revoke gets a fresh key. Modal
	// submissions use view.id, deliberately deduping same-modal resubmissions to
	// avoid double-minting on Slack retries; processTunnelInstall revokes any key
	// whose delivery is not confirmed.
	//
	// qurl-service must replay the plaintext key for same-key idempotent
	// bootstrap creates; upstream integration coverage is tracked in
	// layervai/qurl-service#775. If Slack retries a same-modal submission after a
	// DM-failure revoke, this can replay already-revoked plaintext for that view,
	// but the key remains dead and a fresh human retry opens a new modal with a
	// fresh view.id.
	attemptID = strings.TrimSpace(attemptID)
	if attemptID == "" {
		attemptID = "attempt:unknown"
	}
	return IdempotencyKey(teamID, channelID, userID, fmt.Sprintf("tunnel-bootstrap:%s:%s", slug, attemptID))
}

func tunnelBootstrapTypedAttemptID(triggerID string, startedAt time.Time) string {
	triggerID = strings.TrimSpace(triggerID)
	if triggerID != "" {
		return "typed-trigger:" + triggerID
	}
	return tunnelBootstrapTimeAttemptID("typed-started", startedAt)
}

func tunnelBootstrapModalAttemptID(viewID string, createdAt time.Time) string {
	viewID = strings.TrimSpace(viewID)
	if viewID != "" {
		return "modal-view:" + viewID
	}
	return tunnelBootstrapTimeAttemptID("modal-created", createdAt)
}

func tunnelBootstrapTimeAttemptID(prefix string, at time.Time) string {
	return prefix + ":" + at.UTC().Format(time.RFC3339Nano)
}

func (h *Handler) ensureTunnelAlias(ctx context.Context, teamID, channelID, alias, resourceID string) (string, error) {
	existing, found, err := h.cfg.AdminStore.LookupChannelAlias(ctx, teamID, channelID, alias)
	if err != nil {
		return ":warning: failed to check the existing channel alias; no bootstrap key was minted.", err
	}
	if found {
		if existing == resourceID {
			return fmt.Sprintf("qURL alias `$%s` is ready in this channel.", alias), nil
		}
		return fmt.Sprintf("qURL alias `$%s` is already used in this channel. Run `/qurl-admin unset-alias $%s` first, or pick a different alias.", alias, alias), slackdata.ErrAliasAlreadyBound
	}
	if err := h.aliasStore.BindChannelAlias(ctx, teamID, channelID, alias, resourceID); err != nil {
		if errors.Is(err, slackdata.ErrAliasAlreadyBound) {
			// A concurrent retry may have created the same binding after our
			// optimistic read. Confirm that benign race before surfacing a
			// conflict to the admin.
			existing, found, lookupErr := h.cfg.AdminStore.LookupChannelAlias(ctx, teamID, channelID, alias)
			if lookupErr == nil && found && existing == resourceID {
				return fmt.Sprintf("qURL alias `$%s` is ready in this channel.", alias), nil
			}
		}
		return ":warning: failed to bind the channel alias; no bootstrap key was minted.", err
	}
	return fmt.Sprintf("qURL alias `$%s` is ready in this channel.", alias), nil
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

func (p preparedTunnelInstallMessage) render(args *tunnelInstallArgs, key *client.APIKey, aliasStatus, tunnelDisplayName string, now time.Time) (string, error) {
	if key == nil {
		return "", errors.New("bootstrap api key is missing")
	}
	if err := validateBootstrapAPIKeyForShell(key.APIKey); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("qURL Connector `")
	b.WriteString(args.Slug)
	b.WriteString("`")
	// Show the tunnel's Display Name next to the id. It reuses the resource
	// description (see handleSetDisplayName) and is always set — install
	// seeds the default, admins refine it — so it normally renders. The
	// empty guard is defensive only (an upstream that ever returned a blank
	// description shouldn't dangle an em-dash).
	if tunnelDisplayName != "" {
		b.WriteString(" — ")
		b.WriteString(escapeMrkdwnText(tunnelDisplayName))
	}
	b.WriteString(" is ready to install.\n")
	b.WriteString(aliasStatus)
	b.WriteString("\n\nInstall instructions are below. The temporary bootstrap key ")
	b.WriteString(tunnelBootstrapExpiryLabel(key, now))
	b.WriteString(" and was sent separately by DM. The install instructions below either prompt for it or reference your platform secret manager; do not add the key to the instruction text itself. Paste the DM key only when prompted or into your secret manager. If a terminal echoes pasted input, stop and use a platform secret manager instead.")
	b.WriteString(p.imageNote)
	b.WriteString("\n\n")
	b.WriteString(p.imageLine)
	b.WriteString("\nTarget environment: ")
	b.WriteString(p.environmentLabel)
	b.WriteString(".\n\n")
	b.WriteString(p.instructions)
	b.WriteString("\n\nTreat the separate bootstrap-key DM as secret until the sidecar connects. After the first successful start, remove the mounted bootstrap key from the runtime. Keep the qURL agent-state directory, volume, or PVC; it stores the sidecar identity used on future restarts.\n\n")
	b.WriteString("Then users can run `/qurl get $")
	b.WriteString(args.Alias)
	b.WriteString("`.")
	return b.String(), nil
}

func renderTunnelBootstrapSecretMessage(args *tunnelInstallArgs, key *client.APIKey, now time.Time) (string, error) {
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
	b.WriteString("Temporary qURL Connector bootstrap key for `")
	b.WriteString(args.Slug)
	b.WriteString("` ")
	b.WriteString(tunnelBootstrapExpiryLabel(key, now))
	b.WriteString(".\n\nPaste this secret only when the install instructions prompt for it, or store it in the target platform's secret manager. The install instructions were sent separately and intentionally do not include this key.\n\n")
	b.WriteString(keyBlock)
	b.WriteString("\n\nAfter the qURL Connector connects, remove this bootstrap key from the runtime. Delete this DM from Slack history when your workspace retention policy allows.")
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
	// Focused-test convenience wrapper: passes an empty Display Name so
	// these tests fence the rest of the install block in isolation. The
	// production path (processTunnelInstall) passes resource.Description,
	// and the Display-Name-on-install behavior is covered by a dedicated
	// test that drives render directly.
	return prepared.render(args, key, aliasStatus, "", h.now())
}

func tunnelImageNote(usingDefaultImage bool) string {
	if !usingDefaultImage {
		return ""
	}
	return ":warning: Image: using the dev/sandbox fallback `" + defaultTunnelImage + "`. Set `QURL_CONNECTOR_IMAGE` to an immutable release tag or digest before production rollout, for example `ghcr.io/layervai/qurl-connector@sha256:<digest>`."
}

func tunnelInstallRateLimitMessage(err error) string {
	retryAfter := slackRetryAfterLabel(SlackRateLimitRetryAfter(err))
	if retryAfter == "" {
		return "Slack rate-limited guided qURL Connector setup. Wait up to " + humanDurationCeilMinutes(slackRetryAfterDisplayCap) + ", then run `/qurl-admin protect-connector` again."
	}
	return "Slack rate-limited guided qURL Connector setup. Wait " + retryAfter + ", then run `/qurl-admin protect-connector` again."
}

func (h *Handler) renderTunnelInstallInstructions(args *tunnelInstallArgs, image string) (string, error) {
	// Instructions deliberately do not receive the plaintext bootstrap key:
	// prepareTunnelInstallMessage can preflight all environment-specific
	// rendering before CreateAPIKey, and processTunnelInstall delivers the
	// validated key through a separate DM.
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

func renderTunnelConfigYAML(args *tunnelInstallArgs) (string, error) {
	quotedSlug, err := yamlSingleQuoted(args.Slug)
	if err != nil {
		return "", err
	}
	// The client calls this route token `id`; the Admin API stores and returns
	// the same verbatim value as the resource slug.
	return fmt.Sprintf(`routes:
  - id: %s
    type: http
    local_ip: 127.0.0.1
    local_port: %d`, quotedSlug, args.LocalPort), nil
}

func renderPortablePipefailShell() string {
	return `if (set -o pipefail) 2>/dev/null; then
  set -o pipefail
fi`
}

func renderSudoDetectionShell() string {
	return `if [ "$(id -u)" -eq 0 ]; then
  SUDO=""
elif command -v sudo >/dev/null 2>&1 && sudo -n true 2>/dev/null; then
  SUDO="sudo -n"
else
  echo "Run as root or configure passwordless sudo so the state and secret directories can be owned by UID 65532." >&2
  exit 1
fi`
}

func renderRequiredShellNameGuard(varName, placeholder, targetDescription, allowedCharClass, allowedDescription string) string {
	return fmt.Sprintf(`if [ "$%[1]s" = "%[2]s" ] || [ -z "$%[1]s" ]; then
  echo "Set %[1]s to %[3]s." >&2
  exit 1
fi
case "$%[1]s" in
  [A-Za-z0-9]*) ;;
  *)
    echo "%[1]s must start with a letter or number." >&2
    exit 1
    ;;
esac
case "$%[1]s" in
  *[!%[4]s]*)
    echo "%[1]s may contain only %[5]s." >&2
    exit 1
    ;;
esac`, varName, placeholder, targetDescription, allowedCharClass, allowedDescription)
}

func renderBootstrapKeyPromptShell() string {
	return `if [ -z "${QURL_BOOTSTRAP_KEY:-}" ]; then
  if [ ! -t 0 ]; then
    echo "Set QURL_BOOTSTRAP_KEY or run this block from an interactive terminal." >&2
    exit 1
  fi
  printf 'Paste qURL bootstrap key (input hidden): ' >&2
  STTY_STATE="$(stty -g 2>/dev/null | tr -d '[:space:]' || true)"
  if [ -n "$STTY_STATE" ]; then
    stty -echo
    trap 'if [ -n "$STTY_STATE" ]; then stty "$STTY_STATE" 2>/dev/null || true; fi' INT TERM EXIT
  fi
  if ! IFS= read -r QURL_BOOTSTRAP_KEY; then
    if [ -n "$STTY_STATE" ]; then
      stty "$STTY_STATE"
      trap - INT TERM EXIT
    fi
    printf '\n' >&2
    echo "Bootstrap key is required." >&2
    exit 1
  fi
  if [ -n "$STTY_STATE" ]; then
    stty "$STTY_STATE"
    trap - INT TERM EXIT
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
	// Keep this aligned with validateBootstrapAPIKeyForShell: the key is streamed
	// through an unquoted heredoc, so that validator owns heredoc-expansion safety.
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

func renderBootstrapKeyToCommandShell(command string) string {
	// Stream exactly the key byte count from a here-doc so the trailing heredoc
	// newline is not part of the secret and the key never appears in argv.
	// This heredoc-with-pipe form is intentionally limited to Linux /bin/sh
	// implementations used in our install targets: bash, dash, and BusyBox ash.
	// Keep this aligned with validateBootstrapAPIKeyForShell: the key is streamed
	// through an unquoted heredoc, so that validator owns heredoc-expansion safety.
	return fmt.Sprintf(`QURL_BOOTSTRAP_KEY_LEN=${#QURL_BOOTSTRAP_KEY}
head -c "$QURL_BOOTSTRAP_KEY_LEN" <<QURL_BOOTSTRAP_KEY_EOF | %s
$QURL_BOOTSTRAP_KEY
QURL_BOOTSTRAP_KEY_EOF
unset QURL_BOOTSTRAP_KEY QURL_BOOTSTRAP_KEY_LEN`, command)
}

func tunnelBootstrapTTLLabel() string {
	return "expires in " + humanTunnelBootstrapTTL(tunnelBootstrapTTL)
}

func tunnelBootstrapExpiryLabel(key *client.APIKey, now time.Time) string {
	if key != nil && key.ExpiresAt != nil {
		remaining := key.ExpiresAt.Sub(now)
		if remaining > 0 {
			return "expires in " + humanDurationCeilMinutes(remaining)
		}
		if remaining > -tunnelBootstrapSkew {
			return "expires very soon"
		}
		return "is expired"
	}
	return tunnelBootstrapTTLLabel()
}

func validateBootstrapAPIKeyForShell(apiKey string) error {
	// qurl-service bootstrap keys must be printable single-line ASCII tokens
	// without heredoc expansion bytes. ASCII keeps ${#QURL_BOOTSTRAP_KEY} and
	// head -c byte counts aligned across shells/locales. Dollar signs,
	// backticks, and backslashes are rejected because the generated install
	// snippets stream the prompt value through an unquoted heredoc so the key
	// never appears in argv; other shell metacharacters are not expanded there.
	if apiKey == "" {
		return errors.New("empty api key")
	}
	for _, r := range apiKey {
		if r == '\'' || r == '`' || r == '$' || r == '\\' || r < 0x20 || r > 0x7e {
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
		if r == '\n' || r == '\r' || r < 0x20 || r > 0x7e {
			return "", errors.New("yaml scalar contains non-ascii, control characters, or newlines")
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

const (
	// slackSectionTextMaxBytes guards a `section` mrkdwn run against Slack's
	// 3000-*character* per-section limit. The guard measures bytes, an
	// intentionally conservative proxy: byte length >= rune count, so a run
	// that passes is always within the character limit. At worst a multibyte
	// run (e.g. the em-dash in the prose) trips slightly early and drops the
	// whole message to the always-deliverable plain-text post — the safe
	// direction.
	slackSectionTextMaxBytes = 3000
	// slackRichTextMaxBytes caps a single rich_text_preformatted code segment.
	// rich_text `text` elements carry NO documented length limit (unlike the
	// 3000-char cap on `text`/mrkdwn composition objects), and Slack accepts
	// multi-KB code blocks, so this is a defensive ceiling rather than a hard
	// Slack bound: it sits well above the largest real install snippet (~3.8 KB,
	// the Docker Compose shell block) with headroom for installer growth, and
	// far below Slack's 40000-char message-text ceiling. An oversize segment
	// drops to the plain-text post — and [Handler.postInstallInstructions]
	// retries as text even if Slack ever rejects an over-large blocks payload —
	// so this is a first-try optimization, not the delivery safety boundary.
	slackRichTextMaxBytes = 12000
	// slackMessageBlockMax is Slack's per-message block ceiling. The install
	// message produces roughly two blocks per code segment plus prose, far
	// below this; the guard is defensive against a future renderer that fans
	// out far more segments.
	slackMessageBlockMax = 50
)

// installFencedCodeBlock matches a single ```\n<body>\n``` fence as produced by
// slackCodeBlock. Segmentation is unambiguous only because slackCodeBlock is the
// SOLE producer of ``` fences in install messages and the hand-written prose
// contains no literal ```: so every fence is one it emitted, and a body can't
// nest ``` (slackCodeBlock rejects that), letting the non-greedy capture cleanly
// split code from surrounding prose. A future renderer that emitted a literal
// ``` into prose could mis-segment — but only ever into the always-safe text
// fallback, never a key leak.
var installFencedCodeBlock = regexp.MustCompile("(?s)```\\n(.*?)\\n```")

// installMessageBlocks converts a fully-rendered install message — prose
// interleaved with ```\n…\n``` code fences (see
// [preparedTunnelInstallMessage.render]) — into Block Kit blocks: each prose
// run becomes a `section` mrkdwn block and each fenced snippet becomes a
// [richTextPreformattedBlock] (a copyable code block). It derives the blocks
// from the SAME string that is also posted as the message's plain-text
// fallback, so the two renderings cannot drift.
//
// Returns (blocks, true) on success, or (nil, false) when the message carries
// no code fence to enrich, a fence is empty, a prose segment exceeds
// [slackSectionTextMaxBytes] or a code segment exceeds [slackRichTextMaxBytes],
// or the block count would exceed [slackMessageBlockMax]. A false result is the
// caller's signal to post the plain-text message instead: the install flow
// MUST stay deliverable (an unconfirmed delivery revokes the bootstrap key), so
// blocks are strictly a best-effort enhancement over the always-safe text post.
func installMessageBlocks(msg string) ([]any, bool) {
	matches := installFencedCodeBlock.FindAllStringSubmatchIndex(msg, -1)
	if len(matches) == 0 {
		return nil, false
	}
	blocks := make([]any, 0, len(matches)*2+1)
	appendProse := func(s string) bool {
		s = strings.TrimSpace(s)
		if s == "" {
			return true
		}
		if len(s) > slackSectionTextMaxBytes {
			return false
		}
		blocks = append(blocks, sectionBlock(s))
		return true
	}
	last := 0
	for _, m := range matches {
		// m[0:2] is the full fence; m[2:4] is the captured body.
		if !appendProse(msg[last:m[0]]) {
			return nil, false
		}
		code := msg[m[2]:m[3]]
		if code == "" || len(code) > slackRichTextMaxBytes {
			// An empty fence would produce a rich_text_preformatted element
			// with an empty `text`, which Slack rejects outright; an oversize
			// one trips the defensive ceiling. Neither is reachable from
			// today's renderers, but both drop to the always-safe text post.
			return nil, false
		}
		blocks = append(blocks, richTextPreformattedBlock(code))
		last = m[1]
	}
	if !appendProse(msg[last:]) {
		return nil, false
	}
	// blocks is non-empty here: len(matches) > 0, and every match appends a
	// rich_text block, so only the upper bound can trip.
	if len(blocks) > slackMessageBlockMax {
		return nil, false
	}
	return blocks, true
}

// postInstallInstructions delivers the rendered install message, preferring the
// Block Kit rendering (copyable rich_text_preformatted snippets) and falling
// back to the plain-text post on any block-path miss. The final text delivery
// retries once before reporting failure because a false negative revokes a
// freshly minted bootstrap key. Returns whether SOME rendering was delivered,
// so the caller revokes only when delivery remains unconfirmed after the
// fallback and retry.
//
// The text post is the same single-call delivery the install flow used before
// blocks existed, so this path is never worse than that baseline: a Slack-side
// rejection of the blocks payload (non-2xx) is retried as plain text before the
// caller treats delivery as failed.
//
// If Slack actually persists the blocks post but the client reports failure
// (e.g. a timeout), the text retry posts a second copy. Neither copy carries the
// bootstrap key, and the already-DM'd key is not revoked, so this is benign: at
// worst the operator sees two ephemeral install messages, never a leaked or
// stale key.
func (h *Handler) postInstallInstructions(log *slog.Logger, responseURL, msg string) bool {
	if blocks, ok := installMessageBlocks(msg); ok {
		if h.postResponseBlocks(log, responseURL, msg, blocks) {
			return true
		}
		log.Warn("tunnel install: Block Kit follow-up delivery failed; retrying as plain text")
	}
	return h.postResponseWithRetry(log, responseURL, msg, "tunnel_install_text")
}
