package internal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	resourceExposeUsage       = "Usage:\n• `/qurl-admin protect-url` for the guided picker\n• `/qurl-admin protect-url $<resource-alias> [as:$channel-alias]`\n• `/qurl-admin protect-url url:<target-url> as:$channel-alias`"
	resourceExposeSchemeHTTP  = "http"
	resourceExposeSchemeHTTPS = "https"
	// exposeURLResourceFailedMsg is the generic failure reply shared by the
	// typed `resource protect` path and the expose-URL modal, kept as one const
	// so the copy stays in lockstep across both surfaces.
	exposeURLResourceFailedMsg = "Failed to protect URL resource. Please try again."
)

type resourceExposeArgs struct {
	ResourceAlias string
	TargetURL     string
	ChannelAlias  string
}

// parseResourceExposeArgs parses the typed (power-user) form of the URL verb:
// `/qurl-admin protect-url <target> [as:$channel-alias]`, where `text` is the
// command body with the `protect-url` verb already stripped. The verb is a single
// hyphenated word — there is no `expose` sub-word — so the target is the first
// positional token and an optional `as:` flag follows. Bare `protect-url` (no
// positional) is the guided modal, routed before this by handleExposeURL, so a
// missing target here is a usage error.
func parseResourceExposeArgs(text string) (parsed *resourceExposeArgs, userMsg string) {
	tokens := strings.Fields(text)
	if len(tokens) < 1 || len(tokens) > 2 {
		return nil, resourceExposeUsage
	}

	args := &resourceExposeArgs{}
	if len(tokens) == 2 {
		if !strings.HasPrefix(tokens[1], "as:") {
			return nil, resourceExposeUsage
		}
		alias, reason := validateAliasTokenForNoun(strings.TrimPrefix(tokens[1], "as:"), "Channel alias", "channel alias")
		if reason != "" {
			return nil, reason + "\n\n" + resourceExposeUsage
		}
		args.ChannelAlias = alias
	}

	target := tokens[0]
	if strings.HasPrefix(target, "$") {
		alias, reason := validateAliasTokenForNoun(target, "Resource alias", "resource alias")
		if reason != "" {
			return nil, reason + "\n\n" + resourceExposeUsage
		}
		args.ResourceAlias = alias
		if args.ChannelAlias == "" {
			args.ChannelAlias = alias
		}
		return args, ""
	}

	if strings.HasPrefix(target, "url:") {
		targetURL := strings.TrimSpace(strings.TrimPrefix(target, "url:"))
		if targetURL == "" {
			return nil, "Missing URL after `url:`.\n\n" + resourceExposeUsage
		}
		parsed, err := url.Parse(targetURL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != resourceExposeSchemeHTTP && parsed.Scheme != resourceExposeSchemeHTTPS) {
			return nil, "URL target must be an absolute http or https URL.\n\n" + resourceExposeUsage
		}
		if args.ChannelAlias == "" {
			return nil, "`url:<target-url>` requires `as:$channel-alias` so Slack users have a friendly name.\n\n" + resourceExposeUsage
		}
		args.TargetURL = targetURL
		return args, ""
	}

	return nil, "Target must be a resource alias like `$docs`, or `url:<target-url>` with `as:$channel-alias`.\n\n" + resourceExposeUsage
}

// handleExposeURL routes the URL verb `/qurl-admin protect-url`: bare (no
// arguments) opens the guided URL-resource picker modal; `protect-url <target>
// [as:$channel-alias]` is the typed power-user form that skips the modal. This
// is the single-word URL protection verb.
//
// Either way the effect is the same: make an existing URL resource available in
// THIS Slack channel by binding a channel alias to its resource_id. The dashboard
// stays Slack-blind, and Slack users never type opaque `r_...` resource IDs.
func (h *Handler) handleExposeURL(w http.ResponseWriter, values url.Values) {
	text := strings.TrimSpace(values.Get(fieldText))
	_, rest := slashVerb(text, adminVerbProtectURL)
	if strings.TrimSpace(rest) == "" {
		// Bare verb → guided picker modal (the no-arguments path).
		h.handleExposeURLWizard(w, values)
		return
	}

	args, userMsg := parseResourceExposeArgs(rest)
	if userMsg != "" {
		respondSlack(w, userMsg)
		return
	}

	teamID, channelID, ok := h.aliasValidate(w, values, "protect-url")
	if !ok {
		return
	}
	if !h.requireAliasAdminGate(w, teamID, values, AdminActionExposeURL) {
		return
	}

	h.runAsync(w, "expose_url", values, func(ctx context.Context, log *slog.Logger) {
		msg := h.exposeURLResourceInChannel(ctx, log, teamID, channelID, args)
		_ = h.postResponse(log, values.Get(fieldResponseURL), msg)
	})
}

// handleExposeURLWizard opens the guided URL-resource picker for a bare
// `/qurl-admin protect-url`. Like the connector wizard (handleTunnelInstallWizard)
// it acks fast and does the admin re-check + resource fetch + views.open on the
// async worker inside Slack's short trigger window, so the picker's first-page
// resource scan never blocks the slash ack. The picker's button-driven sibling
// (the `expose` chooser → handleExposeURLClick) opens the identical modal from a
// fresh button trigger; this is the direct slash entry. OpenView must be wired —
// without it the bare verb declines and points at the typed form.
func (h *Handler) handleExposeURLWizard(w http.ResponseWriter, values url.Values) {
	if !h.requireAdminStoreSync(w) {
		return
	}
	if h.aliasStore == nil {
		respondSlack(w, "Channel alias storage is not configured on this Slack bot deployment. Contact the operator.")
		return
	}
	if h.cfg.OpenView == nil {
		respondSlack(w, "Guided setup is not configured on this Slack bot deployment. Use `/qurl-admin protect-url $<alias>` instead.")
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
		respondSlack(w, "Slack did not include a trigger_id, so guided setup could not open. Use `/qurl-admin protect-url $<alias>` instead.")
		return
	}
	log := slog.With(
		"command", "expose_url_wizard",
		"team_id", teamID,
		"enterprise_id", enterpriseID,
		"channel_id", channelID,
		"user_id", userID,
		"trigger_id", triggerID,
	)
	triggerReceivedAt := h.now()
	responseURL := values.Get(fieldResponseURL)
	if !h.startAsyncWorker(log, func(ctx context.Context, log *slog.Logger) {
		h.openExposeURLWizard(ctx, log, teamID, enterpriseID, channelID, userID, triggerID, responseURL, triggerReceivedAt)
	}) {
		respondSlack(w, ackBusy)
		return
	}
	// Ack before the async admin check so Slack's short trigger_id window is
	// preserved for views.open; denials and open failures come back via
	// response_url. Mirrors handleTunnelInstallWizard.
	respondSlack(w, ackWorkingOnIt)
}

// openExposeURLWizard is the async worker for handleExposeURLWizard: admin
// re-check, fetch the channel's exposable URL resources, then open the picker
// modal — all bounded to fit Slack's trigger window. With no URL resources to
// protect it opens a first-run create-and-protect modal instead of an empty picker.
// Mirrors openTunnelInstallWizard's gate/budget posture; the modal-render half is
// shared with handleExposeURLClick (urlResourceSelectOptions + ExposeURLModal).
func (h *Handler) openExposeURLWizard(ctx context.Context, log *slog.Logger, teamID, enterpriseID, channelID, userID, triggerID, responseURL string, triggerReceivedAt time.Time) {
	openBudget := slackTriggerOpenViewBudgetRemaining(h.now().Sub(triggerReceivedAt))
	if openBudget <= 0 {
		log.Warn("protect-url wizard trigger expired before admin check")
		_ = h.postErrorResponse(log, responseURL, "Slack's setup window expired before the modal opened. Run `/qurl-admin protect-url` again.", true)
		return
	}
	adminCtx, cancel := context.WithTimeout(ctx, adminGateBudget)
	isAdmin, _, err := h.cfg.AdminStore.CheckAdmin(adminCtx, teamID, userID)
	cancel()
	if err != nil {
		log.Error("protect-url wizard admin check failed", "error", err)
		_ = h.postErrorResponse(log, responseURL, "Could not verify admin status. Retry in a moment.", true)
		return
	}
	if !isAdmin {
		log.Warn("protect-url wizard denied: non-admin")
		_ = h.postErrorResponse(log, responseURL, "This command is admin-only.", true)
		return
	}

	fetchCtx, fetchCancel := context.WithTimeout(ctx, adminGateBudget)
	options, userMsg := h.urlResourceSelectOptions(fetchCtx, log, teamID)
	fetchCancel()
	if userMsg != "" {
		_ = h.postErrorResponse(log, responseURL, userMsg, true)
		return
	}
	if len(options) == 0 {
		view, err := ExposeURLCreateModal(ExposeURLModalMetadata{
			TeamID:      teamID,
			ChannelID:   channelID,
			UserID:      userID,
			ResponseURL: responseURL,
		})
		if err != nil {
			log.Error("protect-url wizard create modal render failed", "error", err)
			_ = h.postErrorResponse(log, responseURL, "Could not open the guided URL picker. Please retry or contact support.", true)
			return
		}
		openBudget = slackTriggerOpenViewBudgetRemaining(h.now().Sub(triggerReceivedAt))
		if openBudget <= 0 {
			log.Warn("protect-url wizard trigger expired before create modal views.open")
			_ = h.postErrorResponse(log, responseURL, "Slack's setup window expired before the modal opened. Run `/qurl-admin protect-url` again.", true)
			return
		}
		openCtx, openCancel := context.WithTimeout(ctx, openBudget)
		defer openCancel()
		if err := h.openViewWithGridFallback(openCtx, log, teamID, enterpriseID, triggerID, view); err != nil {
			log.Warn("protect-url wizard create modal views.open failed", "error", err,
				"slack_trigger_expired", errors.Is(err, ErrSlackTriggerExpired),
				"slack_rate_limited", errors.Is(err, ErrSlackRateLimited),
			)
			switch {
			case errors.Is(err, ErrSlackTriggerExpired):
				_ = h.postErrorResponse(log, responseURL, "Slack's setup window expired before the modal opened. Run `/qurl-admin protect-url` again.", true)
			case errors.Is(err, ErrSlackRateLimited):
				_ = h.postErrorResponse(log, responseURL, "Slack rate-limited the guided picker. Wait a moment, then run `/qurl-admin protect-url` again.", true)
			default:
				_ = h.postErrorResponse(log, responseURL, "Could not open the guided URL picker. Please retry or contact support.", true)
			}
			return
		}
		_ = h.deleteOriginalResponse(log, responseURL)
		return
	}

	view, err := ExposeURLModal(ExposeURLModalMetadata{
		TeamID:      teamID,
		ChannelID:   channelID,
		UserID:      userID,
		ResponseURL: responseURL,
	}, options)
	if err != nil {
		log.Error("protect-url wizard modal render failed", "error", err)
		_ = h.postErrorResponse(log, responseURL, "Could not open the guided URL picker. Please retry or contact support.", true)
		return
	}

	openBudget = slackTriggerOpenViewBudgetRemaining(h.now().Sub(triggerReceivedAt))
	if openBudget <= 0 {
		log.Warn("protect-url wizard trigger expired before views.open")
		_ = h.postErrorResponse(log, responseURL, "Slack's setup window expired before the modal opened. Run `/qurl-admin protect-url` again.", true)
		return
	}
	openCtx, openCancel := context.WithTimeout(ctx, openBudget)
	defer openCancel()
	if err := h.openViewWithGridFallback(openCtx, log, teamID, enterpriseID, triggerID, view); err != nil {
		log.Warn("protect-url wizard views.open failed", "error", err,
			"slack_trigger_expired", errors.Is(err, ErrSlackTriggerExpired),
			"slack_rate_limited", errors.Is(err, ErrSlackRateLimited),
		)
		switch {
		case errors.Is(err, ErrSlackTriggerExpired):
			_ = h.postErrorResponse(log, responseURL, "Slack's setup window expired before the modal opened. Run `/qurl-admin protect-url` again.", true)
		case errors.Is(err, ErrSlackRateLimited):
			_ = h.postErrorResponse(log, responseURL, "Slack rate-limited the guided picker. Wait a moment, then run `/qurl-admin protect-url` again.", true)
		default:
			_ = h.postErrorResponse(log, responseURL, "Could not open the guided URL picker. Please retry or contact support.", true)
		}
		return
	}
	_ = h.deleteOriginalResponse(log, responseURL)
}

func (h *Handler) exposeURLResourceInChannel(ctx context.Context, log *slog.Logger, teamID, channelID string, args *resourceExposeArgs) string {
	resource, err := h.resolveURLResourceForExpose(ctx, log, teamID, args)
	if err != nil {
		var userErr *userError
		if errors.As(err, &userErr) {
			return userErr.msg
		}
		log.Error("resource protect: unexpected resource lookup error", "error", err, "team_id", teamID)
		return exposeURLResourceFailedMsg
	}

	err = h.aliasStore.BindChannelAlias(ctx, teamID, channelID, args.ChannelAlias, resource.ResourceID)
	if errors.Is(err, slackdata.ErrAliasAlreadyBound) {
		return fmt.Sprintf("Alias `$%s` is already bound in this channel. Run `/qurl-admin unset-alias $%s` first, or pick a different alias.", args.ChannelAlias, args.ChannelAlias)
	}
	if err != nil {
		log.Error("resource protect: alias bind failed", "error", err, "team_id", teamID, "channel_id", channelID, "alias", args.ChannelAlias, "resource_id", resource.ResourceID)
		return exposeURLResourceFailedMsg
	}

	log.Info("URL resource protected in Slack channel", "team_id", teamID, "channel_id", channelID, "channel_alias", args.ChannelAlias, "resource_alias", resource.Alias, "resource_id", resource.ResourceID)
	if resource.Alias != "" {
		return fmt.Sprintf("URL resource `$%s` is now available as `$%s` in this channel. Run `/qurl get $%s` to create a qURL.", resource.Alias, args.ChannelAlias, args.ChannelAlias)
	}
	return fmt.Sprintf("URL resource is now available as `$%s` in this channel. Run `/qurl get $%s` to create a qURL.", args.ChannelAlias, args.ChannelAlias)
}

func (h *Handler) resolveURLResourceForExpose(ctx context.Context, log *slog.Logger, teamID string, args *resourceExposeArgs) (*client.Resource, error) {
	c, err := h.authenticatedClient(ctx, teamID)
	if err != nil {
		log.Error("resource protect: API key lookup failed", "error", err, "team_id", teamID)
		return nil, &userError{msg: "Failed to look up URL resource. Please try again."}
	}
	// Bare-minimum import path: reuse the same first-page scan as /qurl list/get
	// URL-alias fallback. A qurl-service alias lookup would remove this boundary,
	// but this keeps Slack connector ownership without a backend API change.
	page, err := c.ListResources(ctx, client.ListResourcesInput{Limit: listResourcesScanLimit})
	if err != nil {
		log.Warn("resource protect: resource lookup failed", "error", err, "team_id", teamID)
		return nil, &userError{msg: sanitizeAPIError(err, "Failed to look up URL resource")}
	}

	var matches []client.Resource
	for i := range page.Resources {
		resource := page.Resources[i]
		if resource.Status == client.StatusRevoked || !isURLResource(&resource) {
			continue
		}
		if args.ResourceAlias != "" && resource.Alias == args.ResourceAlias {
			matches = append(matches, resource)
			continue
		}
		if args.TargetURL != "" && resource.TargetURL == args.TargetURL {
			matches = append(matches, resource)
		}
	}
	if page.HasMore {
		log.Debug("resource protect: scanned first resource page only", "scan_limit", listResourcesScanLimit, "team_id", teamID)
	}
	if len(matches) == 0 {
		if args.ResourceAlias != "" {
			return nil, &userError{msg: fmt.Sprintf("No active URL resource `$%s` was found. Check the protected resource alias in the dashboard, then retry.", args.ResourceAlias)}
		}
		return nil, &userError{msg: "No active URL resource was found for that exact target URL. The `url:` value must match the dashboard URL exactly."}
	}
	if len(matches) > 1 {
		if args.ResourceAlias != "" {
			return nil, &userError{msg: fmt.Sprintf("`$%s` matches multiple active URL resources. Give the protected resource a unique alias in the dashboard, then retry.", args.ResourceAlias)}
		}
		return nil, &userError{msg: "That target URL matches multiple active URL resources. Give the protected resource a unique alias in the dashboard, then retry."}
	}
	return &matches[0], nil
}
