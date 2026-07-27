package internal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/layervai/qurl-integrations/apps/teams/internal/oauth"
	"github.com/layervai/qurl-integrations/apps/teams/internal/teamsdata"
	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

const teamsBodyLimit = 1 << 20
const teamsAsyncMessageTimeout = 1 * time.Minute
const teamsBootstrapCleanupTimeout = 5 * time.Second
const teamsTunnelScopeAgent = "qurl:agent"
const teamsTunnelScopeWrite = "qurl:write"
const teamsGetResourceLinkExpiry = "1m"
const teamsGetResourceSessionDuration = "1h"
const teamsGetResourceMaxSessions = 1

// HandlerConfig wires the Teams bot handler to qURL, auth, and storage dependencies.
type HandlerConfig struct {
	BaseContext  context.Context
	QURLEndpoint string
	AuthProvider auth.Provider
	AdminStore   *teamsdata.Store
	Messages     MessagePoster
	TokenAuth    TokenValidator
	Setup        oauth.SetupConfig
	Feedback     FeedbackPoster
	TunnelImage  string
	SkipBotAuth  bool
	UserAgent    string
}

// Handler serves the Teams bot message endpoint.
type Handler struct {
	cfg           HandlerConfig
	wg            sync.WaitGroup
	activeWorkers atomic.Int64
}

// NewHandler constructs the Teams message handler.
func NewHandler(cfg *HandlerConfig) *Handler {
	if cfg == nil {
		cfg = &HandlerConfig{}
	}
	nextCfg := *cfg
	if nextCfg.BaseContext == nil {
		nextCfg.BaseContext = context.Background()
	}
	return &Handler{cfg: nextCfg}
}

// ServeHTTP handles incoming Teams Bot Framework activities.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Body != nil {
		defer func() { _ = r.Body.Close() }()
	}
	if !h.cfg.SkipBotAuth {
		token, err := bearerToken(r.Header.Get("Authorization"))
		if err != nil {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, teamsBodyLimit+1))
		if err != nil {
			http.Error(w, "read request body failed", http.StatusBadRequest)
			return
		}
		if len(body) > teamsBodyLimit {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		var activity Activity
		if err := json.Unmarshal(body, &activity); err != nil {
			http.Error(w, "invalid activity payload", http.StatusBadRequest)
			return
		}
		if h.cfg.TokenAuth == nil {
			// Fail closed: if bot auth is enabled but no validator is configured,
			// reject all requests rather than silently accepting unvalidated tokens.
			slog.Error("teams bot auth enabled but TokenAuth is nil — rejecting request")
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		if err := h.cfg.TokenAuth.Validate(r.Context(), token, activity.ServiceURL); err != nil {
			serviceURL := strings.TrimSpace(activity.ServiceURL)
			reason := classifyTokenValidationError(err)
			//nolint:gosec // Structured fields are reduced to constant classifications and length/presence metadata rather than raw request input.
			slog.Warn("teams auth validation failed",
				"reason", reason,
				"service_url_present", serviceURL != "",
				"service_url_len", len(serviceURL))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.handleActivity(w, r.WithContext(r.Context()), &activity)
		return
	}
	var activity Activity
	if err := json.NewDecoder(io.LimitReader(r.Body, teamsBodyLimit)).Decode(&activity); err != nil {
		http.Error(w, "invalid activity payload", http.StatusBadRequest)
		return
	}
	h.handleActivity(w, r, &activity)
}

func (h *Handler) handleActivity(w http.ResponseWriter, r *http.Request, activity *Activity) {
	switch strings.ToLower(strings.TrimSpace(activity.Type)) {
	case "message":
		w.WriteHeader(http.StatusAccepted)
		h.processMessageAsync(activity)
		return
	case "conversationupdate":
		if err := h.capturePersonalReference(r.Context(), activity); err != nil {
			slog.Warn("teams conversation update capture failed", "error", err)
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) processMessageAsync(activity *Activity) {
	h.asyncStart()
	ctx, cancel := context.WithTimeout(h.cfg.BaseContext, teamsAsyncMessageTimeout)
	go func() {
		defer h.asyncDone()
		defer cancel()
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("teams message processing panicked", "recover", rec)
			}
		}()
		if err := h.processMessage(ctx, activity); err != nil {
			//nolint:gosec // Structured fields are reduced to constant classifications rather than raw request input.
			slog.Error("teams message processing failed", "error_class", classifyTeamsCommandError(err))
		}
	}()
}

func (h *Handler) processMessage(ctx context.Context, activity *Activity) error {
	if err := h.capturePersonalReference(ctx, activity); err != nil {
		slog.Warn("teams personal conversation capture failed", "error", err)
	}
	text := normalizeActivityText(activity)
	cmd, err := ParseCommand(text)
	if err != nil {
		return h.reply(ctx, activity, "I couldn't parse that command. Send `help` to see the Teams syntax.")
	}
	scope := deriveScope(activity)
	if scope.TenantID == "" {
		return h.reply(ctx, activity, "This Teams activity did not include a tenant id, so qURL can't scope the request safely.")
	}
	replyText, err := h.execute(ctx, activity, scope, cmd)
	if err != nil {
		slog.Warn("teams command failed",
			"error_class", classifyTeamsCommandError(err),
			"verb", safeTeamsVerb(cmd.Verb),
			"tenant_id_present", strings.TrimSpace(scope.TenantID) != "",
			"tenant_id_len", len(strings.TrimSpace(scope.TenantID)))
		return h.reply(ctx, activity, teamsUserMessageForError(err))
	}
	if strings.TrimSpace(replyText) == "" {
		return nil
	}
	return h.reply(ctx, activity, replyText)
}

func (h *Handler) execute(ctx context.Context, activity *Activity, scope scopeInfo, cmd *Command) (string, error) {
	switch cmd.Verb {
	case verbHelp:
		return helpMessage(), nil
	case verbSetup:
		return h.handleSetup(ctx, scope, activity, cmd)
	case verbFeedback:
		return h.handleFeedback(ctx, scope, activity.From, cmd.Text)
	}
	if h.cfg.AdminStore == nil {
		return "", &userError{msg: "Teams admin features are not configured on this deployment."}
	}
	if reply, handled, err := h.executeTenantAdminCommand(ctx, activity, scope, cmd); handled {
		return reply, err
	}
	return h.executeChannelCommand(ctx, activity, scope, cmd)
}

func (h *Handler) executeTenantAdminCommand(ctx context.Context, activity *Activity, scope scopeInfo, cmd *Command) (reply string, handled bool, err error) {
	switch cmd.Verb {
	case verbAdmins:
		reply, err = h.handleAdmins(ctx, scope)
		return reply, true, err
	case verbAdd:
		reply, err = h.handleAddAdmin(ctx, scope, activity.From.ID, cmd.UserID)
		return reply, true, err
	case verbRemove:
		reply, err = h.handleRemoveAdmin(ctx, scope, activity.From.ID, cmd.UserID)
		return reply, true, err
	case verbUninstall:
		reply, err = h.handleUninstall(ctx, scope, activity.From.ID)
		return reply, true, err
	}
	return "", false, nil
}

func (h *Handler) executeChannelCommand(ctx context.Context, activity *Activity, scope scopeInfo, cmd *Command) (string, error) {
	if !scope.Channel {
		return "", &userError{msg: "This command must be run in a Teams channel. Personal chat is only used for setup confirmation and private delivery."}
	}
	qc, err := h.qurlClient(ctx, scope.TenantID)
	if err != nil {
		return "", err
	}
	switch cmd.Verb {
	case verbList:
		return h.handleList(ctx, qc, scope)
	case verbAliases:
		return h.handleAliases(ctx, scope)
	case verbGet:
		return h.handleGet(ctx, qc, scope, activity, cmd)
	case verbProtectURL:
		return h.handleProtectURL(ctx, qc, scope, activity.From.ID, cmd.Args)
	case verbProtectConnector:
		return h.handleProtectConnector(ctx, qc, scope, activity, cmd.Args)
	case verbSetAlias:
		return h.handleSetAlias(ctx, qc, scope, activity.From.ID, cmd)
	case verbUnsetAlias:
		return h.handleUnsetAlias(ctx, scope, activity.From.ID, cmd)
	case verbSetDisplayName:
		return h.handleSetDisplayName(ctx, qc, scope, activity.From.ID, cmd)
	case verbUnsetDisplayName:
		return h.handleUnsetDisplayName(ctx, qc, scope, activity.From.ID, cmd)
	case verbRevoke:
		return h.handleRevoke(ctx, qc, scope, activity.From.ID, cmd)
	default:
		return "", fmt.Errorf("unsupported command %q", cmd.Verb)
	}
}

func (h *Handler) handleSetup(ctx context.Context, scope scopeInfo, activity *Activity, cmd *Command) (string, error) {
	if len(h.cfg.Setup.StateSecret) < oauth.StateMinSecret || strings.TrimSpace(h.cfg.Setup.TeamsBaseURL) == "" {
		return "", &userError{msg: "Setup is not configured on this deployment."}
	}
	if cmd.SetupMode != SetupModeReuse {
		if h.cfg.AdminStore == nil {
			return "", &userError{msg: "Setup rotation is not available because Teams admin storage is not configured."}
		}
		ownerID, _, err := h.cfg.AdminStore.ListAdmins(ctx, scope.TenantID)
		if err != nil && !isTeamsStoreCode(err, teamsdata.ErrCodeWorkspaceNotBound) {
			return "", newSystemError("qURL couldn't verify the tenant owner right now. Try again.", fmt.Errorf("check tenant owner: %w", err))
		}
		if ownerID != "" && ownerID != strings.TrimSpace(activity.From.ID) {
			return "", &userError{msg: "Only the tenant owner can run `setup --rotate` or `setup --repoint`."}
		}
	}
	state, err := oauth.MintStateWithEmailMode(h.cfg.Setup.StateSecret, scope.TenantID, strings.TrimSpace(activity.From.ID), cmd.Email, oauth.SetupMode(cmd.SetupMode), time.Now().UTC())
	if err != nil {
		return "", newSystemError("qURL couldn't build the setup link right now. Try again.", fmt.Errorf("build setup link: %w", err))
	}
	return "Open this qURL setup link in your browser:\n" + h.cfg.Setup.SetupURL(state), nil
}

func (h *Handler) handleAdmins(ctx context.Context, scope scopeInfo) (string, error) {
	ownerID, adminIDs, err := h.cfg.AdminStore.ListAdmins(ctx, scope.TenantID)
	if err != nil {
		return "", err
	}
	lines := []string{"Tenant owner: " + ownerID}
	if len(adminIDs) == 0 {
		lines = append(lines, "Admins: none")
	} else {
		lines = append(lines, "Admins: "+strings.Join(adminIDs, ", "))
	}
	return strings.Join(lines, "\n"), nil
}

func (h *Handler) handleAddAdmin(ctx context.Context, scope scopeInfo, callerID, targetID string) (string, error) {
	if err := h.requireAdmin(ctx, scope.TenantID, callerID); err != nil {
		return "", err
	}
	if err := h.cfg.AdminStore.AddAdmin(ctx, scope.TenantID, targetID); err != nil {
		return "", err
	}
	return "Added Teams user `" + targetID + "` as a qURL admin for this tenant.", nil
}

func (h *Handler) handleRemoveAdmin(ctx context.Context, scope scopeInfo, callerID, targetID string) (string, error) {
	if err := h.requireAdmin(ctx, scope.TenantID, callerID); err != nil {
		return "", err
	}
	if err := h.cfg.AdminStore.RemoveAdmin(ctx, scope.TenantID, targetID); err != nil {
		return "", err
	}
	return "Removed Teams user `" + targetID + "` from qURL admins for this tenant.", nil
}

func (h *Handler) handleUninstall(ctx context.Context, scope scopeInfo, callerID string) (string, error) {
	if err := h.requireAdmin(ctx, scope.TenantID, callerID); err != nil {
		return "", err
	}
	workspaceID := teamsWorkspaceID(scope.TenantID)
	if h.cfg.AuthProvider != nil && h.cfg.AuthProvider.SupportsDeleteAPIKey() {
		if err := h.cfg.AuthProvider.DeleteAPIKey(ctx, workspaceID); err != nil && !errors.Is(err, auth.ErrWorkspaceAPIKeyDeleteUnsupported) {
			return "", newSystemError("qURL couldn't remove the stored tenant key right now. Try again.", fmt.Errorf("remove stored qURL key: %w", err))
		}
	}
	if err := h.cfg.AdminStore.DeleteWorkspace(ctx, scope.TenantID); err != nil {
		return "", err
	}
	if err := h.cfg.AdminStore.PurgeTenantScopePolicies(ctx, scope.TenantID); err != nil {
		return "", err
	}
	return "Disconnected qURL from this Teams tenant. This only removes the local Teams binding and channel policies; it does not revoke the qURL API key outside Teams.", nil
}

func (h *Handler) handleFeedback(ctx context.Context, scope scopeInfo, from ChannelAccount, message string) (string, error) {
	if h.cfg.Feedback == nil {
		return "", &userError{msg: "Feedback is not enabled on this deployment."}
	}
	if err := h.cfg.Feedback.Post(ctx, strings.TrimSpace(from.ID), scope.TenantID, message); err != nil {
		return "", newSystemError("Feedback couldn't be delivered right now. Try again later.", fmt.Errorf("send feedback: %w", err))
	}
	return "Thanks. The qURL team received your feedback.", nil
}

func (h *Handler) handleAliases(ctx context.Context, scope scopeInfo) (string, error) {
	entries, err := h.cfg.AdminStore.GetScopePolicy(ctx, scope.TenantID, scope.ScopeID)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "No aliases are configured in this channel.", nil
	}
	lines := make([]string, 0, len(entries)+1)
	lines = append(lines, "Aliases in this channel:")
	for _, entry := range entries {
		lines = append(lines, fmt.Sprintf("- `$%s` -> `$%s`", entry.Alias, entry.ResourceID))
	}
	return strings.Join(lines, "\n"), nil
}

func (h *Handler) handleList(ctx context.Context, qc *client.Client, scope scopeInfo) (string, error) {
	allowed, err := h.cfg.AdminStore.AllowedResourceIDsForScope(ctx, scope.TenantID, scope.ScopeID)
	if err != nil {
		return "", err
	}
	if len(allowed) == 0 {
		return "No protected resources are available in this channel yet.", nil
	}
	resources, err := listAllResources(ctx, qc)
	if err != nil {
		return "", err
	}
	aliases, err := h.cfg.AdminStore.GetScopePolicy(ctx, scope.TenantID, scope.ScopeID)
	if err != nil {
		return "", err
	}
	aliasesByResource := map[string][]string{}
	for _, entry := range aliases {
		aliasesByResource[entry.ResourceID] = append(aliasesByResource[entry.ResourceID], entry.Alias)
	}
	lines := []string{"Protected resources in this channel:"}
	count := 0
	for i := range resources {
		resource := &resources[i]
		if _, ok := allowed[resource.ResourceID]; !ok {
			continue
		}
		count++
		lines = append(lines, fmt.Sprintf("- `$%s`  %s", resource.ResourceID, formatResourceSummary(resource, aliasesByResource[resource.ResourceID])))
	}
	if count == 0 {
		return "No protected resources are available in this channel yet.", nil
	}
	return strings.Join(lines, "\n"), nil
}

func (h *Handler) handleGet(ctx context.Context, qc *client.Client, scope scopeInfo, activity *Activity, cmd *Command) (string, error) {
	resource, err := h.resolveScopedResource(ctx, qc, scope.TenantID, scope.ScopeID, cmd.Resource)
	if err != nil {
		return "", err
	}
	var ref *teamsdata.PersonalConversationRef
	if strings.EqualFold(cmd.Flags["dm"], "true") {
		if h.cfg.Messages == nil {
			return "", &userError{msg: "Private delivery is not available on this deployment."}
		}
		var found bool
		ref, found, err = h.cfg.AdminStore.PersonalConversationRef(ctx, scope.TenantID, strings.TrimSpace(activity.From.ID))
		if err != nil {
			return "", err
		}
		if !found {
			return "", &userError{msg: "Private delivery isn't ready for this Teams user yet. Open a personal chat with the bot once, then retry `get ... dm:true`."}
		}
	}
	out, err := qc.Create(ctx, client.CreateInput{
		ResourceID:      resource.ResourceID,
		ExpiresIn:       teamsGetResourceLinkExpiry,
		OneTimeUse:      true,
		MaxSessions:     teamsGetResourceMaxSessions,
		SessionDuration: teamsGetResourceSessionDuration,
		Reason:          cmd.Flags["reason"],
		IdempotencyKey:  getQURLIdempotencyKey(scope.TenantID, scope.ScopeID, strings.TrimSpace(activity.From.ID), resource.ResourceID, activity),
	})
	if err != nil {
		return "", mapClientError("mint qURL", err)
	}
	linkMessage := "qURL for `$" + cmd.Resource + "`: " + out.QURLLink
	if out.ExpiresAt != nil {
		linkMessage += "\nExpires: " + out.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if ref != nil {
		if err := h.cfg.Messages.SendText(ctx, ref.ServiceURL, ref.ConversationID, linkMessage); err != nil {
			return "", newSystemError("Private delivery failed. Open a personal chat with the bot and retry `get ... dm:true`.", fmt.Errorf("send personal message: %w", err))
		}
		return "Sent the one-time qURL to your personal Teams chat.", nil
	}
	return linkMessage, nil
}

func (h *Handler) handleProtectURL(ctx context.Context, qc *client.Client, scope scopeInfo, callerID string, args []string) (string, error) {
	if err := h.requireAdmin(ctx, scope.TenantID, callerID); err != nil {
		return "", err
	}
	if len(args) == 0 {
		return "Usage:\n- `protect-url url:https://internal.example.com as:$docs`\n- `protect-url $resource-id as:$docs`\n- `protect-url $docs`", nil
	}
	alias, err := parseProtectURLAliasArgs(args[1:])
	if err != nil {
		return "", err
	}
	resource, alias, err := h.resolveProtectURLResource(ctx, qc, args[0], alias)
	if err != nil {
		return "", err
	}
	if resource.Type != "" && resource.Type != client.ResourceTypeURL {
		return "", errors.New("protect-url only works with URL resources")
	}
	if err := h.cfg.AdminStore.ExposeResourceToScope(ctx, scope.TenantID, scope.ScopeID, resource.ResourceID); err != nil {
		return "", err
	}
	if alias != "" {
		if err := h.upsertScopeAlias(ctx, scope.TenantID, scope.ScopeID, alias, resource.ResourceID); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("URL resource `$%s` is now available in this channel as `$%s`.", resource.ResourceID, alias), nil
}

func parseProtectURLAliasArgs(args []string) (string, error) {
	var alias string
	for _, tok := range args {
		tok = strings.TrimSpace(tok)
		if len(tok) < len("as:") || !strings.EqualFold(tok[:len("as:")], "as:") {
			return "", fmt.Errorf("unexpected argument %q", tok)
		}
		if alias != "" {
			return "", fmt.Errorf("unexpected argument %q", tok)
		}
		parsedAlias, err := parseAliasToken(strings.TrimSpace(tok[len("as:"):]))
		if err != nil {
			return "", err
		}
		alias = parsedAlias
	}
	return alias, nil
}

func (h *Handler) resolveProtectURLResource(ctx context.Context, qc *client.Client, firstArg, alias string) (*client.Resource, string, error) {
	if strings.HasPrefix(strings.ToLower(firstArg), "url:") {
		return createProtectURLResource(ctx, qc, firstArg, alias)
	}
	return h.resolveExistingProtectURLResource(ctx, qc, firstArg, alias)
}

func createProtectURLResource(ctx context.Context, qc *client.Client, firstArg, alias string) (*client.Resource, string, error) {
	targetURL := strings.TrimSpace(firstArg[len("url:"):])
	if targetURL == "" {
		return nil, "", errors.New("protect-url requires a non-empty url: value")
	}
	if alias == "" {
		return nil, "", errors.New("protect-url url:<target> requires `as:$alias` in Teams")
	}
	resource, err := qc.CreateResource(ctx, &client.CreateResourceInput{
		TargetURL:    targetURL,
		Type:         client.ResourceTypeURL,
		FindOrCreate: true,
	})
	if err != nil {
		return nil, "", mapClientError("protect url resource", err)
	}
	return resource, alias, nil
}

func (h *Handler) resolveExistingProtectURLResource(ctx context.Context, qc *client.Client, firstArg, alias string) (*client.Resource, string, error) {
	token, err := parseLookupToken(firstArg)
	if err != nil {
		return nil, "", err
	}
	resource, err := h.resolveTenantResource(ctx, qc, token)
	if err != nil {
		return nil, "", err
	}
	if alias == "" {
		alias, err = defaultProtectURLAlias(resource)
		if err != nil {
			return nil, "", err
		}
	}
	return resource, alias, nil
}

func defaultProtectURLAlias(resource *client.Resource) (string, error) {
	switch {
	case resource == nil:
		return "", errors.New("protect-url requires a valid URL resource")
	case resource.Alias != "":
		return resource.Alias, nil
	case resource.Slug != "":
		return resource.Slug, nil
	default:
		return "", errors.New("protect-url needs `as:$alias` when the resource has no reusable alias")
	}
}

func (h *Handler) handleProtectConnector(ctx context.Context, qc *client.Client, scope scopeInfo, activity *Activity, args []string) (string, error) {
	if err := h.requireAdmin(ctx, scope.TenantID, activity.From.ID); err != nil {
		return "", err
	}
	if len(args) == 0 {
		return "Usage:\n- `protect-connector prod-dashboard [alias:$dash] [env:docker|compose|docker-compose|ecs-fargate|kubernetes] [port:8080] [service:web]`\nRun it in a channel. The bootstrap key is delivered privately in personal Teams chat.", nil
	}
	parsed, err := parseTunnelArgs(args)
	if err != nil {
		return "", err
	}
	if h.cfg.Messages == nil {
		return "", &userError{msg: "Private delivery is not available on this deployment."}
	}
	ref, found, err := h.cfg.AdminStore.PersonalConversationRef(ctx, scope.TenantID, strings.TrimSpace(activity.From.ID))
	if err != nil {
		return "", err
	}
	if !found {
		return "", &userError{msg: "protect-connector needs personal Teams chat delivery for the bootstrap key. Message the bot once in personal chat, then retry in the channel."}
	}
	resource, err := qc.CreateResource(ctx, &client.CreateResourceInput{
		Type:         client.ResourceTypeTunnel,
		Slug:         parsed.Slug,
		FindOrCreate: true,
	})
	if err != nil {
		return "", mapClientError("protect connector", err)
	}
	if err := h.cfg.AdminStore.ExposeResourceToScope(ctx, scope.TenantID, scope.ScopeID, resource.ResourceID); err != nil {
		return "", err
	}
	if err := h.upsertScopeAlias(ctx, scope.TenantID, scope.ScopeID, parsed.Alias, resource.ResourceID); err != nil {
		return "", err
	}
	key, err := qc.CreateAPIKey(ctx, &client.CreateAPIKeyInput{
		Name:           "Teams connector " + parsed.Slug,
		Scopes:         []string{teamsTunnelScopeAgent, teamsTunnelScopeWrite},
		ExpiresIn:      "15m",
		KeyType:        client.APIKeyTypeTunnelBootstrap,
		TunnelSlug:     parsed.Slug,
		IdempotencyKey: tunnelBootstrapIdempotencyKey(scope.TenantID, scope.ScopeID, strings.TrimSpace(activity.From.ID), parsed.Slug, tunnelBootstrapAttemptID(activity)),
	})
	if err != nil {
		return "", mapClientError("create bootstrap key", err)
	}
	message, err := renderTunnelInstallMessage(&TunnelInstallArgs{
		Slug:         parsed.Slug,
		Alias:        parsed.Alias,
		Environment:  parsed.Environment,
		Port:         parsed.Port,
		Service:      parsed.Service,
		TunnelImage:  h.cfg.TunnelImage,
		BootstrapKey: key.APIKey,
	})
	if err != nil {
		if cleanupErr := revokeBootstrapKeyAfterInstallFailure(qc, key, "message_render_failed"); cleanupErr != nil {
			slog.Error("teams protect-connector bootstrap key cleanup failed after render failure", "error", cleanupErr, "slug", parsed.Slug, "resource_id", resource.ResourceID, "key_id", key.KeyID)
			return "", newSystemError("qURL Connector setup couldn't be completed and the temporary bootstrap key could not be revoked automatically. Ask an operator to check qURL before retrying.", fmt.Errorf("render tunnel install message: %w", err))
		}
		return "", &userError{msg: "qURL Connector setup couldn't render the install instructions. The temporary bootstrap key was revoked. Retry `protect-connector`."}
	}
	if err := h.cfg.Messages.SendText(ctx, ref.ServiceURL, ref.ConversationID, message); err != nil {
		if cleanupErr := revokeBootstrapKeyAfterInstallFailure(qc, key, "personal_dm_delivery_failed"); cleanupErr != nil {
			slog.Error("teams protect-connector bootstrap key cleanup failed after DM delivery failure", "error", cleanupErr, "slug", parsed.Slug, "resource_id", resource.ResourceID, "key_id", key.KeyID)
			return "", newSystemError("I couldn't deliver the bootstrap key to your personal Teams chat, and the temporary key could not be revoked automatically. Ask an operator to check qURL before retrying.", fmt.Errorf("send bootstrap key to personal chat: %w", err))
		}
		return "", &userError{msg: "I couldn't deliver the bootstrap key to your personal Teams chat, so the temporary key was revoked. Open the bot chat and retry `protect-connector`."}
	}
	return fmt.Sprintf("Protected connector `$%s` and bound `$%s` in this channel. I sent the bootstrap key and install instructions to your personal Teams chat.", resource.ResourceID, parsed.Alias), nil
}

func (h *Handler) handleSetAlias(ctx context.Context, qc *client.Client, scope scopeInfo, callerID string, cmd *Command) (string, error) {
	if err := h.requireAdmin(ctx, scope.TenantID, callerID); err != nil {
		return "", err
	}
	resource, err := h.resolveTenantResource(ctx, qc, cmd.Target)
	if err != nil {
		return "", err
	}
	if err := h.cfg.AdminStore.ExposeResourceToScope(ctx, scope.TenantID, scope.ScopeID, resource.ResourceID); err != nil {
		return "", err
	}
	if err := h.upsertScopeAlias(ctx, scope.TenantID, scope.ScopeID, cmd.Alias, resource.ResourceID); err != nil {
		return "", err
	}
	return fmt.Sprintf("Alias `$%s` now points to `$%s` in this channel.", cmd.Alias, resource.ResourceID), nil
}

func (h *Handler) handleUnsetAlias(ctx context.Context, scope scopeInfo, callerID string, cmd *Command) (string, error) {
	if err := h.requireAdmin(ctx, scope.TenantID, callerID); err != nil {
		return "", err
	}
	if err := h.cfg.AdminStore.UnbindScopeAlias(ctx, scope.TenantID, scope.ScopeID, cmd.Resource); err != nil {
		return "", err
	}
	return "Removed alias `$" + cmd.Resource + "` from this channel.", nil
}

func (h *Handler) handleSetDisplayName(ctx context.Context, qc *client.Client, scope scopeInfo, callerID string, cmd *Command) (string, error) {
	if err := h.requireAdmin(ctx, scope.TenantID, callerID); err != nil {
		return "", err
	}
	resource, err := h.resolveTenantResource(ctx, qc, cmd.Resource)
	if err != nil {
		return "", err
	}
	if _, err := qc.UpdateResource(ctx, resource.ResourceID, &client.UpdateResourceInput{Description: &cmd.Text}); err != nil {
		return "", mapClientError("set display name", err)
	}
	return fmt.Sprintf("Updated display name for `$%s`.", resource.ResourceID), nil
}

func (h *Handler) handleUnsetDisplayName(ctx context.Context, qc *client.Client, scope scopeInfo, callerID string, cmd *Command) (string, error) {
	if err := h.requireAdmin(ctx, scope.TenantID, callerID); err != nil {
		return "", err
	}
	resource, err := h.resolveTenantResource(ctx, qc, cmd.Resource)
	if err != nil {
		return "", err
	}
	reset := ""
	if _, err := qc.UpdateResource(ctx, resource.ResourceID, &client.UpdateResourceInput{Description: &reset}); err != nil {
		return "", mapClientError("unset display name", err)
	}
	return fmt.Sprintf("Reset display name for `$%s`.", resource.ResourceID), nil
}

func (h *Handler) handleRevoke(ctx context.Context, qc *client.Client, scope scopeInfo, callerID string, cmd *Command) (string, error) {
	if err := h.requireAdmin(ctx, scope.TenantID, callerID); err != nil {
		return "", err
	}
	resource, err := h.resolveScopedResource(ctx, qc, scope.TenantID, scope.ScopeID, cmd.Resource)
	if err != nil {
		resource, err = h.resolveTenantResource(ctx, qc, cmd.Resource)
		if err != nil {
			return "", err
		}
	}
	if err := qc.DeleteResource(ctx, resource.ResourceID); err != nil {
		return "", mapClientError("revoke resource", err)
	}
	scopes, err := h.cfg.AdminStore.ScopesForResource(ctx, scope.TenantID, resource.ResourceID)
	if err != nil {
		return "", err
	}
	for _, scopeID := range scopes {
		if _, err := h.cfg.AdminStore.PurgeResourceFromScope(ctx, scope.TenantID, scopeID, resource.ResourceID); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("Revoked resource `$%s` and removed it from Teams channel policies in this tenant.", resource.ResourceID), nil
}

func (h *Handler) capturePersonalReference(ctx context.Context, activity *Activity) error {
	scope := deriveScope(activity)
	if !scope.Personal || h.cfg.AdminStore == nil {
		return nil
	}
	if strings.TrimSpace(scope.TenantID) == "" || strings.TrimSpace(activity.From.ID) == "" {
		return nil
	}
	return h.cfg.AdminStore.SavePersonalConversationRef(ctx, scope.TenantID, activity.From.ID, &teamsdata.PersonalConversationRef{
		ServiceURL:     strings.TrimSpace(activity.ServiceURL),
		ConversationID: strings.TrimSpace(activity.Conversation.ID),
	})
}

func (h *Handler) qurlClient(ctx context.Context, tenantID string) (*client.Client, error) {
	if h.cfg.AuthProvider == nil {
		return nil, newSystemError("qURL authentication is not configured on this deployment.", errors.New("qURL auth provider is not configured"))
	}
	key, err := h.cfg.AuthProvider.APIKey(ctx, teamsWorkspaceID(tenantID))
	if err != nil {
		if errors.Is(err, auth.ErrWorkspaceNotConfigured) {
			return nil, &userError{msg: "qURL isn't connected to this Teams tenant yet. Run `setup <email>` first."}
		}
		return nil, newSystemError("qURL couldn't load the tenant configuration right now. Try again.", fmt.Errorf("load tenant qURL API key: %w", err))
	}
	return client.New(h.cfg.QURLEndpoint, key, client.WithUserAgent(teamsUserAgent(h.cfg.UserAgent))), nil
}

func teamsWorkspaceID(tenantID string) string {
	return "teams:" + strings.TrimSpace(tenantID)
}

func (h *Handler) requireAdmin(ctx context.Context, tenantID, actorID string) error {
	ok, _, err := h.cfg.AdminStore.CheckAdmin(ctx, tenantID, strings.TrimSpace(actorID))
	if err != nil {
		return err
	}
	if !ok {
		return &userError{msg: "This command is limited to the tenant owner and qURL admins."}
	}
	return nil
}

func (h *Handler) resolveScopedResource(ctx context.Context, qc *client.Client, tenantID, scopeID, token string) (*client.Resource, error) {
	if resourceID, found, err := h.cfg.AdminStore.LookupScopeAlias(ctx, tenantID, scopeID, token); err != nil {
		return nil, err
	} else if found {
		return h.resolveResourceByID(ctx, qc, resourceID)
	}
	allowed, err := h.cfg.AdminStore.AllowedResourceIDsForScope(ctx, tenantID, scopeID)
	if err != nil {
		return nil, err
	}
	if len(allowed) == 0 {
		return nil, errors.New("this channel has no protected qURL resources yet")
	}
	resources, err := listAllResources(ctx, qc)
	if err != nil {
		return nil, err
	}
	var matches []*client.Resource
	for i := range resources {
		resource := &resources[i]
		if _, ok := allowed[resource.ResourceID]; !ok {
			continue
		}
		if resourceMatchesToken(resource, token) {
			matches = append(matches, resource)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no resource named `%s` is available in this channel", token)
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("`%s` matches multiple allowed resources in this channel; use the resource id shown by `list`", token)
	}
}

func (h *Handler) resolveTenantResource(ctx context.Context, qc *client.Client, token string) (*client.Resource, error) {
	resources, err := listAllResources(ctx, qc)
	if err != nil {
		return nil, err
	}
	var matches []*client.Resource
	for i := range resources {
		resource := &resources[i]
		if resourceMatchesToken(resource, token) {
			matches = append(matches, resource)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no resource named `%s` was found for this tenant", token)
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("`%s` matches multiple tenant resources; use the exact resource id", token)
	}
}

func (h *Handler) resolveResourceByID(ctx context.Context, qc *client.Client, resourceID string) (*client.Resource, error) {
	resources, err := listAllResources(ctx, qc)
	if err != nil {
		return nil, err
	}
	for i := range resources {
		if resources[i].ResourceID == resourceID {
			return &resources[i], nil
		}
	}
	return nil, fmt.Errorf("resource `%s` was not found", resourceID)
}

func (h *Handler) upsertScopeAlias(ctx context.Context, tenantID, scopeID, alias, resourceID string) error {
	if err := h.cfg.AdminStore.UnbindScopeAlias(ctx, tenantID, scopeID, alias); err != nil && !errors.Is(err, teamsdata.ErrAliasNotFound) {
		return err
	}
	return h.cfg.AdminStore.BindScopeAlias(ctx, tenantID, scopeID, alias, resourceID)
}

func resourceMatchesToken(resource *client.Resource, token string) bool {
	if resource == nil {
		return false
	}
	token = strings.TrimSpace(token)
	return resource.ResourceID == token || resource.Slug == token || resource.Alias == token
}

func listAllResources(ctx context.Context, qc *client.Client) ([]client.Resource, error) {
	var (
		out    []client.Resource
		cursor string
	)
	for {
		page, err := qc.ListResources(ctx, client.ListResourcesInput{
			Limit:  100,
			Cursor: cursor,
		})
		if err != nil {
			return nil, mapClientError("list resources", err)
		}
		for i := range page.Resources {
			if !isLiveResource(&page.Resources[i]) {
				continue
			}
			out = append(out, page.Resources[i])
		}
		if !page.HasMore || page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

func formatResourceSummary(resource *client.Resource, aliases []string) string {
	if resource == nil {
		return ""
	}
	name := resource.Description
	if name == "" {
		switch {
		case resource.Slug != "":
			name = resource.Slug
		case resource.Alias != "":
			name = resource.Alias
		case resource.TargetURL != "":
			name = resource.TargetURL
		default:
			name = resource.ResourceID
		}
	}
	parts := []string{name}
	if resource.Type != "" {
		parts = append(parts, "type="+resource.Type)
	}
	if len(aliases) > 0 {
		parts = append(parts, "aliases=$"+strings.Join(aliases, ", $"))
	}
	return strings.Join(parts, "  ")
}

func helpMessage() string {
	return strings.Join([]string{
		"qURL for Teams",
		"",
		"User commands:",
		"- `setup <email> [--rotate|--repoint]`",
		"- `get $<id|alias> [dm:true] [reason:\"...\"]`",
		"- `list`",
		"- `aliases`",
		"- `feedback <message>`",
		"",
		"Admin commands:",
		"- `protect-url url:https://internal.example.com as:$docs`",
		"- `protect-url $resource-id as:$docs`",
		"- `protect-connector prod-dashboard [alias:$dash] [env:docker|compose|docker-compose|ecs-fargate|kubernetes] [port:8080]`",
		"- `set-alias $alias $resource-id`",
		"- `unset-alias $alias`",
		"- `set-display-name $resource-id Friendly name`",
		"- `unset-display-name $resource-id`",
		"- `revoke $resource-id`",
		"- `add @user` / `remove @user` / `admins`",
		"- `uninstall`",
		"",
		"Teams notes:",
		"- Run channel-scoped commands in a Teams channel.",
		"- For `dm:true` and connector bootstrap delivery, message the bot once in personal chat so it can store your personal conversation reference.",
	}, "\n")
}

func parseTunnelArgs(args []string) (*TunnelInstallArgs, error) {
	if len(args) == 0 {
		return nil, errors.New("missing connector id")
	}
	slug, err := parseLookupToken(args[0])
	if err != nil {
		return nil, err
	}
	if err := validateTunnelSlug(slug); err != nil {
		return nil, err
	}
	out := &TunnelInstallArgs{
		Slug:        slug,
		Alias:       slug,
		Environment: tunnelEnvDocker,
		Port:        defaultTunnelPort,
	}
	for _, tok := range args[1:] {
		lowerTok := strings.ToLower(tok)
		switch {
		case strings.HasPrefix(lowerTok, "alias:"):
			out.Alias, err = parseAliasToken(tok[len("alias:"):])
		case strings.HasPrefix(lowerTok, "env:"):
			out.Environment, err = normalizeTunnelEnvironment(tok[len("env:"):])
		case strings.HasPrefix(lowerTok, "port:"):
			out.Port, err = strconv.Atoi(strings.TrimSpace(tok[len("port:"):]))
			if err == nil && (out.Port <= 0 || out.Port > 65535) {
				err = errors.New("port must be a TCP port from 1 to 65535")
			}
		case strings.HasPrefix(lowerTok, "service:"):
			out.Service = strings.TrimSpace(tok[len("service:"):])
		default:
			err = fmt.Errorf("unexpected connector option %q", tok)
		}
		if err != nil {
			return nil, err
		}
	}
	if err := validateTunnelService(out.Service); err != nil {
		return nil, err
	}
	if out.Service != "" && out.Environment != tunnelEnvCompose {
		return nil, errors.New("service is only supported with env:compose or env:docker-compose")
	}
	return out, nil
}

func bearerToken(header string) (string, error) {
	parts := strings.Fields(strings.TrimSpace(header))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", errors.New("invalid authorization header")
	}
	return parts[1], nil
}

func (h *Handler) reply(ctx context.Context, activity *Activity, text string) error {
	if h.cfg.Messages == nil {
		return nil
	}
	return h.cfg.Messages.Reply(ctx, activity, text)
}

func (h *Handler) asyncStart() {
	h.activeWorkers.Add(1)
	h.wg.Add(1)
}

func (h *Handler) asyncDone() {
	h.wg.Done()
	h.activeWorkers.Add(-1)
}

// WaitTimeout waits for async workers to drain until the timeout expires.
func (h *Handler) WaitTimeout(d time.Duration) bool {
	if d <= 0 {
		return h.activeWorkers.Load() == 0
	}

	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func isTeamsStoreCode(err error, code string) bool {
	var terr *teamsdata.Error
	return errors.As(err, &terr) && terr.Code == code
}

type userError struct {
	msg string
}

func (e *userError) Error() string {
	return e.msg
}

type systemError struct {
	userMsg string
	err     error
}

func (e *systemError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *systemError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func newSystemError(userMsg string, err error) error {
	if err == nil {
		return nil
	}
	return &systemError{userMsg: userMsg, err: err}
}

func teamsUserMessageForError(err error) string {
	if err == nil {
		return ""
	}
	var ue *userError
	if errors.As(err, &ue) {
		return ue.msg
	}
	var se *systemError
	if errors.As(err, &se) && strings.TrimSpace(se.userMsg) != "" {
		return se.userMsg
	}
	var terr *teamsdata.Error
	if errors.As(err, &terr) {
		return teamsStoreUserMessage(terr)
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return appendTeamsReference("qURL request failed", apiErr.RequestID) + "."
	}
	if errors.Is(err, auth.ErrWorkspaceNotConfigured) {
		return "qURL isn't connected to this Teams tenant yet. Run `setup <email>` first."
	}
	return err.Error()
}

func teamsStoreUserMessage(err *teamsdata.Error) string {
	if err == nil {
		return "The Teams admin data request couldn't be completed right now. Try again."
	}
	switch err.Code {
	case teamsdata.ErrCodeWorkspaceNotBound:
		return "qURL isn't connected to this Teams tenant yet. Run `setup <email>` first."
	case teamsdata.ErrCodeAdminAlreadyExists:
		return "That Teams user is already a qURL admin for this tenant."
	case teamsdata.ErrCodeAdminNotFound:
		return "That Teams user is not a qURL admin for this tenant."
	case teamsdata.ErrCodeCannotRemoveOwner:
		return "The tenant owner can't be removed from qURL admins."
	case teamsdata.ErrCodeWorkspaceAlreadyBoundToCaller:
		return "This Teams tenant is already connected under your account."
	case teamsdata.ErrCodeWorkspaceAlreadyBound:
		return "This Teams tenant is already connected by another owner."
	default:
		if err.StatusCode >= http.StatusInternalServerError {
			return "The Teams admin data request couldn't be completed right now. Try again."
		}
		return "The Teams admin data request couldn't be completed."
	}
}

func clientOperationUserMessage(operation string) string {
	switch operation {
	case "mint qURL":
		return "Couldn't create the qURL"
	case "list resources":
		return "Couldn't list protected resources"
	case "protect url resource":
		return "Couldn't protect the URL resource"
	case "protect connector":
		return "Couldn't create or find the qURL Connector"
	case "create bootstrap key":
		return "Couldn't mint the qURL Connector bootstrap key"
	case "set display name":
		return "Couldn't update the display name"
	case "unset display name":
		return "Couldn't reset the display name"
	case "revoke resource":
		return "Couldn't revoke the resource"
	default:
		return "qURL request failed"
	}
}

func appendTeamsReference(message, requestID string) string {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return message
	}
	return fmt.Sprintf("%s (Reference: `%s`)", message, requestID)
}

func tunnelBootstrapAttemptID(activity *Activity) string {
	if activity == nil {
		return "activity:unknown"
	}
	if activityID := strings.TrimSpace(activity.ID); activityID != "" {
		return "activity:" + activityID
	}
	raw, err := json.Marshal(activity)
	if err == nil && len(raw) > 0 {
		sum := sha256.Sum256(raw)
		return "activity-body:" + hex.EncodeToString(sum[:])
	}
	return "activity:unknown"
}

func getQURLIdempotencyKey(tenantID, scopeID, userID, resourceID string, activity *Activity) string {
	return hashIdempotencyFields(
		tenantID,
		scopeID,
		userID,
		"get",
		strings.TrimSpace(resourceID),
		tunnelBootstrapAttemptID(activity),
	)
}

func tunnelBootstrapIdempotencyKey(tenantID, scopeID, userID, slug, attemptID string) string {
	attemptID = strings.TrimSpace(attemptID)
	if attemptID == "" {
		attemptID = "attempt:unknown"
	}
	return IdempotencyKey(tenantID, scopeID, userID, fmt.Sprintf("tunnel-bootstrap:%s:%s", slug, attemptID))
}

func revokeBootstrapKeyAfterInstallFailure(qc *client.Client, key *client.APIKey, reason string) error {
	if qc == nil {
		return errors.New("missing qURL client for bootstrap key cleanup")
	}
	if key == nil || strings.TrimSpace(key.KeyID) == "" {
		return errors.New("missing bootstrap key_id for cleanup")
	}
	ctx, cancel := context.WithTimeout(context.Background(), teamsBootstrapCleanupTimeout)
	defer cancel()
	if err := qc.RevokeAPIKey(ctx, key.KeyID); err != nil {
		var apiErr *client.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			slog.Info("teams protect-connector bootstrap key already absent after install failure", "key_id", key.KeyID, "reason", reason)
			return nil
		}
		return fmt.Errorf("revoke bootstrap key %s after %s: %w", key.KeyID, reason, err)
	}
	slog.Info("teams protect-connector revoked bootstrap key after install failure", "key_id", key.KeyID, "reason", reason)
	return nil
}

func isLiveResource(resource *client.Resource) bool {
	return resource != nil && resource.Status != client.StatusRevoked
}

func mapClientError(operation string, err error) error {
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		return newSystemError(clientOperationUserMessage(operation)+".", fmt.Errorf("%s: %w", operation, err))
	}
	return &userError{msg: appendTeamsReference(clientOperationUserMessage(operation), apiErr.RequestID) + "."}
}

func teamsUserAgent(userAgent string) string {
	userAgent = strings.TrimSpace(userAgent)
	if userAgent == "" {
		return "qurl-teams/unknown"
	}
	return userAgent
}

func classifyTokenValidationError(err error) string {
	if err == nil {
		return "none"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "missing bearer token"):
		return "missing_token"
	case strings.Contains(msg, "serviceUrl claim mismatch"):
		return "service_url_mismatch"
	case strings.Contains(msg, "metadata"):
		return "metadata_fetch"
	case strings.Contains(msg, "jwks"):
		return "jwks"
	case strings.Contains(msg, "verify bot connector token"):
		return "token_verify"
	default:
		return "other"
	}
}

func classifyTeamsCommandError(err error) string {
	if err == nil {
		return "none"
	}
	var userErr *userError
	if errors.As(err, &userErr) {
		return "user"
	}
	var systemErr *systemError
	if errors.As(err, &systemErr) {
		return "system"
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return "client_api"
	}
	var storeErr *teamsdata.Error
	if errors.As(err, &storeErr) {
		return "teams_store"
	}
	return "other"
}

func safeTeamsVerb(verb string) string {
	switch verb {
	case verbHelp,
		verbSetup,
		verbGet,
		verbList,
		verbAliases,
		verbProtectURL,
		verbProtectConnector,
		verbSetAlias,
		verbUnsetAlias,
		verbSetDisplayName,
		verbUnsetDisplayName,
		verbAdd,
		verbRemove,
		verbAdmins,
		verbRevoke,
		verbUninstall,
		verbFeedback:
		return verb
	default:
		return "unknown"
	}
}
