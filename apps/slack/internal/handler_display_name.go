package internal

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/layervai/qurl-integrations/shared/client"
)

// displayNameMaxLen caps the Display Name length at the qurl-service
// resource `description` field's bound (the field these verbs reuse for
// storage). Parsing rejects above-cap names so the user sees a friendlier
// error than the eventual upstream rejection.
const displayNameMaxLen = 500

// defaultTunnelDisplayName is the Display Name a tunnel gets at install
// time and the value `unset-display-name` reverts to. The Display Name
// reuses the resource description field (there is no separate field), so a
// tunnel always has one: install seeds this default, admins refine it with
// set-display-name, and unset restores it. Install (processTunnelInstall)
// and unset (resolveAndResetTunnelDisplayName) MUST construct the string
// the same way so unset matches what a fresh install would have produced —
// hence this single constructor.
func defaultTunnelDisplayName(slug string) string {
	return "Slack tunnel install for " + slug
}

// displayNameUsage is the help-text body returned when set-display-name /
// unset-display-name is invoked with an obvious typo. Centralized so the
// missing-arg path and the validation-rejection path share one copy.
const displayNameUsage = "Usage:\n• `/qurl-admin set-display-name <id> <display name>`\n• `/qurl-admin unset-display-name <id>`\n\nThe id is the tunnel token shown by `/qurl list` (without the `$`). The Display Name is free text up to 500 characters."

// parseSetDisplayNameArgs splits a `set-display-name <id> <display name>`
// body into the tunnel id (first whitespace-delimited token) and the
// Display Name (the rest of the line). The Display Name is trimmed and may
// contain spaces; surrounding single or double quotes are tolerated and
// stripped. Returns (id, name, "") on success or ("", "", userMsg) with the
// ephemeral copy to surface when the grammar doesn't match.
//
// Returns a plain-string user message rather than an error for the same
// reason parseAliasArgs does: this is surface copy carrying sentence
// punctuation that ST1005 rejects on error-typed values.
func parseSetDisplayNameArgs(text string) (id, name, userMsg string) {
	text = strings.TrimSpace(text)
	idTok, rest, found := strings.Cut(text, " ")
	if idTok == "" {
		return "", "", "Missing tunnel id.\n\n" + displayNameUsage
	}
	if !tunnelSlugPattern.MatchString(idTok) {
		return "", "", fmt.Sprintf("`%s` isn't a valid tunnel id. Run `/qurl list` to see your tunnel ids, then retry.\n\n%s", echoText(idTok), displayNameUsage)
	}
	if !found {
		return "", "", "Missing Display Name.\n\n" + displayNameUsage
	}
	name = strings.TrimSpace(stripSurroundingQuotes(strings.TrimSpace(rest)))
	if name == "" {
		return "", "", "Missing Display Name.\n\n" + displayNameUsage
	}
	if len([]rune(name)) > displayNameMaxLen {
		return "", "", fmt.Sprintf("Display Name is too long (max %d characters).\n\n%s", displayNameMaxLen, displayNameUsage)
	}
	// Reject control bytes: the Display Name is echoed into a Slack reply
	// and rendered next to the id in `/qurl list`; a control byte would
	// garble both. Printable text (including spaces and punctuation) is
	// fine — this is free-text copy, not a token.
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return "", "", "Display Name contains an unsupported character. Use plain text only.\n\n" + displayNameUsage
		}
	}
	return idTok, name, ""
}

// parseUnsetDisplayNameArgs validates a `unset-display-name <id>` body:
// exactly one token, a valid tunnel id. Returns (id, "") or ("", userMsg).
func parseUnsetDisplayNameArgs(text string) (id, userMsg string) {
	tokens := strings.Fields(text)
	if len(tokens) != 1 {
		return "", "Provide exactly one tunnel id.\n\n" + displayNameUsage
	}
	if !tunnelSlugPattern.MatchString(tokens[0]) {
		return "", fmt.Sprintf("`%s` isn't a valid tunnel id. Run `/qurl list` to see your tunnel ids, then retry.\n\n%s", echoText(tokens[0]), displayNameUsage)
	}
	return tokens[0], ""
}

// stripSurroundingQuotes removes one matched pair of surrounding single or
// double quotes from s, so `set-display-name api "Prod API"` and
// `... 'Prod API'` both yield the unquoted name. A lone or mismatched quote
// is left untouched — it's part of the name.
func stripSurroundingQuotes(s string) string {
	if len(s) < 2 {
		return s
	}
	first, last := s[0], s[len(s)-1]
	if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}

// handleSetDisplayName routes `/qurl-admin set-display-name <id> <name>`.
//
// **Admin restriction:** Enforced in code via requireAdminSync (a
// CheckAdmin lookup against AdminStore) — the same gate the alias and
// membership verbs use. Slack does NOT restrict a slash command to
// workspace admins, so this code gate is the only real boundary; it runs
// before any tunnel lookup, so a non-admin is denied before any resource
// interaction.
//
// **Storage:** the Display Name is stored in the tunnel resource's
// `description` field via PATCH /v1/resources/{id} — there is no separate
// field and no qurl-service change. Resolution + update are two upstream
// calls, so this runs async (acks immediately, posts the result via
// response_url) like the slug-targeted set-alias path.
func (h *Handler) handleSetDisplayName(w http.ResponseWriter, values url.Values) {
	text := strings.TrimSpace(values.Get(fieldText))
	rest := stripSetDisplayNamePrefix(text)

	id, name, userMsg := parseSetDisplayNameArgs(rest)
	if userMsg != "" {
		respondSlack(w, userMsg)
		return
	}

	teamID, userID, ok := h.displayNameValidate(w, values, "set_display_name")
	if !ok {
		return
	}

	// Admin gate, in code. Runs after the team/user-id read but before the
	// tunnel resolve + PATCH, so a non-admin is denied before any resource
	// interaction. The parser usage hints above are surfaced before the
	// gate (the grammar is public — it's in the ungated `/qurl-admin help`),
	// matching the set-alias posture.
	if !h.requireAdminSync(w, teamID, userID, AdminActionSetDisplayName) {
		return
	}

	h.runAsync(w, "set_display_name", values, func(ctx context.Context, log *slog.Logger) {
		msg := h.resolveAndSetTunnelDisplayName(ctx, log, teamID, id, name)
		_ = h.postResponse(log, values.Get(fieldResponseURL), msg)
	})
}

// handleUnsetDisplayName routes `/qurl-admin unset-display-name <id>`.
// Same in-code admin gate as handleSetDisplayName. Reverts the Display
// Name to the install default ("Slack tunnel install for <id>") by PATCHing
// the resource description — it does NOT blank it, because a tunnel always
// has a Display Name (the description field doubles as it).
func (h *Handler) handleUnsetDisplayName(w http.ResponseWriter, values url.Values) {
	text := strings.TrimSpace(values.Get(fieldText))
	rest := stripUnsetDisplayNamePrefix(text)

	id, userMsg := parseUnsetDisplayNameArgs(rest)
	if userMsg != "" {
		respondSlack(w, userMsg)
		return
	}

	teamID, userID, ok := h.displayNameValidate(w, values, "unset_display_name")
	if !ok {
		return
	}

	if !h.requireAdminSync(w, teamID, userID, AdminActionUnsetDisplayName) {
		return
	}

	h.runAsync(w, "unset_display_name", values, func(ctx context.Context, log *slog.Logger) {
		msg := h.resolveAndResetTunnelDisplayName(ctx, log, teamID, id)
		_ = h.postResponse(log, values.Get(fieldResponseURL), msg)
	})
}

// displayNameValidate pulls team_id + user_id off the form and verifies
// AdminStore is wired (the in-code admin gate needs it). Returns
// (teamID, userID, ok); on !ok the helper has already written the
// user-facing response. No aliasStore check — these verbs don't touch the
// channel-alias store; they only read/write the tunnel resource via
// qurl-service and gate on AdminStore.
func (h *Handler) displayNameValidate(w http.ResponseWriter, values url.Values, verb string) (teamID, userID string, ok bool) {
	teamID = strings.TrimSpace(values.Get(fieldTeamID))
	userID = strings.TrimSpace(values.Get(fieldUserID))
	if teamID == "" || userID == "" {
		slog.Warn("display-name verb missing team_id or user_id", "verb", verb, "team_id_present", teamID != "", "user_id_present", userID != "")
		respondSlack(w, "Could not read your Slack workspace or user ID from the command payload.")
		return
	}
	if h.cfg.AdminStore == nil {
		// Soft-fail when AdminStore is not configured (sandbox deploys
		// without the QURL_*_TABLE env vars). Surfacing a configuration
		// error rather than silently dropping makes the bot's state
		// debuggable from the operator side.
		slog.Warn("display-name verb invoked with no AdminStore wired — refusing", "verb", verb)
		respondSlack(w, "Admin features are not configured for this deployment.")
		return
	}
	ok = true
	return
}

// resolveAndSetTunnelDisplayName resolves the tunnel by id, then PATCHes its
// description to the Display Name, and renders the admin-facing result.
func (h *Handler) resolveAndSetTunnelDisplayName(ctx context.Context, log *slog.Logger, teamID, id, name string) string {
	c, resource, msg := h.resolveTunnelByID(ctx, log, teamID, id)
	if msg != "" {
		return msg
	}
	if _, err := c.UpdateResource(ctx, resource.ResourceID, &client.UpdateResourceInput{Description: &name}); err != nil {
		log.Error("set-display-name update failed", "error", err, "team_id", teamID, "id", id, "resource_id", resource.ResourceID)
		return sanitizeAPIError(err, "Failed to update the Display Name")
	}
	slog.Info("tunnel display name set", "team_id", teamID, "id", id, "resource_id", resource.ResourceID)
	return fmt.Sprintf("✅ Display Name updated for `%s`: %s", id, name)
}

// resolveAndResetTunnelDisplayName resolves the tunnel by id, then PATCHes
// its description back to the install default ([defaultTunnelDisplayName]),
// and renders the admin-facing result. This reverts — it does not blank —
// because a tunnel always has a Display Name (the description field doubles
// as it). Reusing the install-default constructor keeps the reset value
// identical to what a fresh install would have produced.
func (h *Handler) resolveAndResetTunnelDisplayName(ctx context.Context, log *slog.Logger, teamID, id string) string {
	c, resource, msg := h.resolveTunnelByID(ctx, log, teamID, id)
	if msg != "" {
		return msg
	}
	reset := defaultTunnelDisplayName(id)
	if _, err := c.UpdateResource(ctx, resource.ResourceID, &client.UpdateResourceInput{Description: &reset}); err != nil {
		log.Error("unset-display-name update failed", "error", err, "team_id", teamID, "id", id, "resource_id", resource.ResourceID)
		return sanitizeAPIError(err, "Failed to reset the Display Name")
	}
	slog.Info("tunnel display name reset", "team_id", teamID, "id", id, "resource_id", resource.ResourceID)
	return fmt.Sprintf("✅ Display Name reset for `%s`.", id)
}

// resolveTunnelByID looks up the active tunnel whose id (slug) is `id` and
// returns an authenticated client plus the resolved resource. On any
// failure it returns (nil, nil, userMsg) with msg set to the ephemeral copy
// to surface; callers `if msg != "" { return msg }`.
func (h *Handler) resolveTunnelByID(ctx context.Context, log *slog.Logger, teamID, id string) (c *client.Client, resource *client.Resource, userMsg string) {
	c, err := h.authenticatedClient(ctx, teamID)
	if err != nil {
		log.Error("display-name: failed to get API key", "error", err, "team_id", teamID, "id", id)
		return nil, nil, authErrorMessage(err)
	}
	page, err := c.ListResources(ctx, client.ListResourcesInput{Slug: id})
	if err != nil {
		log.Error("display-name: tunnel id resolution failed", "error", err, "team_id", teamID, "id", id)
		return nil, nil, sanitizeAPIError(err, "Failed to look up the tunnel")
	}
	for i := range page.Resources {
		r := &page.Resources[i]
		// Defense-in-depth: re-assert type/slug/active even though the
		// server's `?slug=` filter is single-purpose, so an upstream
		// regression can't surface a non-tunnel, wrong-id, or revoked
		// resource into the PATCH target.
		if r.Type == client.ResourceTypeTunnel && r.Slug == id && r.Status == client.StatusActive {
			return c, r, ""
		}
	}
	log.Info("display-name: no active tunnel for id", "team_id", teamID, "id", id)
	return nil, nil, fmt.Sprintf("No tunnel with id `%s` was found. Run `/qurl list` to see your tunnel ids.", id)
}
