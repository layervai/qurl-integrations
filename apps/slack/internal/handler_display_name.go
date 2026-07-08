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
	return "Slack qURL Connector install for " + slug
}

// displayNameUsage is the help-text body returned when set-display-name /
// unset-display-name is invoked with an obvious typo. Centralized so the
// missing-arg path and the validation-rejection path share one copy.
const displayNameUsage = "Usage:\n• `/qurl-admin set-display-name <id> <display name>`\n• `/qurl-admin unset-display-name <id>`\n\nThe id is the qURL Connector token shown by `/qurl list` (the leading `$` is optional). The Display Name is free text up to 500 characters."

// parseDisplayNameID strips an optional leading `$` from a tunnel-id token
// and validates it against tunnelSlugPattern. Shared by both display-name
// parsers so set/unset accept the `$<id>` form `/qurl list` prints and reject
// identically. The sigil is optional here, whereas `/qurl get`/`set-alias`
// require it (missing → ErrMissingSigil) — these verbs are the lenient
// superset. Stripping beats widening tunnelSlugPattern (shared with install),
// and the invalid-id echo shows the stripped id, matching parseAliasToken on
// the value (the Slack escaper differs). Returns (id, "") on success or
// ("", userMsg).
func parseDisplayNameID(tok string) (id, userMsg string) {
	id = strings.TrimPrefix(tok, "$")
	if id == "" {
		return "", "Missing qURL Connector id.\n\n" + displayNameUsage
	}
	if !tunnelSlugPattern.MatchString(id) {
		return "", fmt.Sprintf("`%s` isn't a valid qURL Connector id. Run `/qurl list` to see your qURL Connector ids, then retry.\n\n%s", echoText(id), displayNameUsage)
	}
	return id, ""
}

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
	// Split the id off the first run of whitespace (any kind), matching
	// parseUnsetDisplayNameArgs' strings.Fields tokenization — so a tab- or
	// newline-separated `<id> <name>` parses the same as a space-separated
	// one, rather than folding the whitespace into the id and failing the
	// slug check with a confusing error.
	idTok, rest := text, ""
	found := false
	if i := strings.IndexFunc(text, unicode.IsSpace); i >= 0 {
		idTok, rest = text[:i], strings.TrimLeftFunc(text[i:], unicode.IsSpace)
		found = true
	}
	// Strip the optional `$` and validate the id (shared with the unset
	// parser via parseDisplayNameID).
	id, userMsg = parseDisplayNameID(idTok)
	if userMsg != "" {
		return "", "", userMsg
	}
	if !found {
		return "", "", "Missing Display Name.\n\n" + displayNameUsage
	}
	name = normalizeDisplayNameInput(rest)
	if name == "" {
		return "", "", "Missing Display Name.\n\n" + displayNameUsage
	}
	if msg := validateDisplayNameChars(name); msg != "" {
		return "", "", msg + "\n\n" + displayNameUsage
	}
	return id, name, ""
}

// validateDisplayNameChars rejects a Display Name that is too long or carries
// characters unsafe in the shared `/qurl list` / `/qurl aliases` surfaces: a
// backtick breaks the inline-code fence the id is rendered in, mrkdwn `<…>` is
// an injection vector (`<https://evil|Prod>` disguised links, `<!channel>` /
// `<!here>` pings), and a control byte garbles the line. The Display Name is
// stored in `description` and shown to EVERY workspace member, so this fence is
// applied wherever a name is set — the set-display-name verb here AND the
// `/qurl list` Edit modal (handler_edit.go). Returns "" when ok, else the
// user-facing reason WITHOUT a usage dump (callers append their own context).
func validateDisplayNameChars(name string) string {
	if len([]rune(name)) > displayNameMaxLen {
		return fmt.Sprintf("Display Name is too long (max %d characters).", displayNameMaxLen)
	}
	for _, r := range name {
		if r == '`' || r == '<' || r == '>' || !unicode.IsPrint(r) {
			return "Display Name can't contain backticks, angle brackets, or control characters. Use plain text only."
		}
	}
	return ""
}

// parseUnsetDisplayNameArgs validates a `unset-display-name <id>` body:
// exactly one token, a valid tunnel id. Returns (id, "") or ("", userMsg).
func parseUnsetDisplayNameArgs(text string) (id, userMsg string) {
	tokens := strings.Fields(text)
	if len(tokens) != 1 {
		return "", "Provide exactly one qURL Connector id.\n\n" + displayNameUsage
	}
	return parseDisplayNameID(tokens[0])
}

// normalizeDisplayNameInput canonicalizes a raw Display Name: trim, drop a
// matched surrounding quote pair, trim again. Shared by the set-display-name
// verb and the `/qurl list` Edit modal so both interpret a typed/pre-filled
// name identically — the modal also diffs against this form to decide whether
// the name actually changed (see parseTunnelEditModalArgs).
func normalizeDisplayNameInput(s string) string {
	return strings.TrimSpace(stripSurroundingQuotes(strings.TrimSpace(s)))
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

	// channel_id lets the resolve step honor a `$alias` bound in THIS channel,
	// not just the connector's own slug; "" is tolerated (alias lookup skipped,
	// slug-only — see resolveTunnelByID).
	channelID := strings.TrimSpace(values.Get(fieldChannelID))

	h.runAsync(w, "set_display_name", values, func(ctx context.Context, log *slog.Logger) {
		msg := h.resolveAndSetTunnelDisplayName(ctx, log, teamID, channelID, id, name)
		_ = h.postResponse(log, values.Get(fieldResponseURL), msg)
	})
}

// handleUnsetDisplayName routes `/qurl-admin unset-display-name <id>`.
// Same in-code admin gate as handleSetDisplayName. Reverts the Display
// Name to the install default ("Slack qURL Connector install for <id>") by PATCHing
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

	channelID := strings.TrimSpace(values.Get(fieldChannelID))

	h.runAsync(w, "unset_display_name", values, func(ctx context.Context, log *slog.Logger) {
		msg := h.resolveAndResetTunnelDisplayName(ctx, log, teamID, channelID, id)
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
func (h *Handler) resolveAndSetTunnelDisplayName(ctx context.Context, log *slog.Logger, teamID, channelID, id, name string) string {
	c, resource, msg := h.resolveTunnelByID(ctx, log, teamID, channelID, id)
	if msg != "" {
		return msg
	}
	if _, err := c.UpdateResource(ctx, resource.ResourceID, &client.UpdateResourceInput{Description: &name}); err != nil {
		log.Error("set-display-name update failed", "error", err, "team_id", teamID, "id", id, "resource_id", resource.ResourceID)
		return sanitizeAPIError(err, "Failed to update the Display Name")
	}
	slog.Info("tunnel display name set", "team_id", teamID, "id", id, "resource_id", resource.ResourceID)
	return fmt.Sprintf("✅ Display Name updated for `%s`: %s", id, escapeMrkdwnText(name))
}

// resolveAndResetTunnelDisplayName resolves the tunnel by id, then PATCHes
// its description back to the install default ([defaultTunnelDisplayName]),
// and renders the admin-facing result. This reverts — it does not blank —
// because a tunnel always has a Display Name (the description field doubles
// as it). Reusing the install-default constructor keeps the reset value
// identical to what a fresh install would have produced.
func (h *Handler) resolveAndResetTunnelDisplayName(ctx context.Context, log *slog.Logger, teamID, channelID, id string) string {
	c, resource, msg := h.resolveTunnelByID(ctx, log, teamID, channelID, id)
	if msg != "" {
		return msg
	}
	// Revert to the install default built from the connector's OWN slug
	// (resource.Slug) — what install wrote. Correct whether `id` was the slug
	// itself or a channel `$alias` resolving to it: the alias name and the
	// connector slug can differ, so reset off the resolved resource, not id.
	reset := defaultTunnelDisplayName(resource.Slug)
	if _, err := c.UpdateResource(ctx, resource.ResourceID, &client.UpdateResourceInput{Description: &reset}); err != nil {
		log.Error("unset-display-name update failed", "error", err, "team_id", teamID, "id", id, "resource_id", resource.ResourceID)
		return sanitizeAPIError(err, "Failed to reset the Display Name")
	}
	slog.Info("tunnel display name reset", "team_id", teamID, "id", id, "resource_id", resource.ResourceID)
	return fmt.Sprintf("✅ Display Name reset for `%s`.", id)
}

// resolveTunnelByID resolves the active tunnel the admin referenced by `id`,
// accepting EITHER the connector's own slug OR a channel `$alias` bound in
// channelID. The slug is tried first: it's a server-side `?slug=` filter, works
// from any channel, and — unlike the alias path's resource scan — isn't bounded
// by listResourcesScanLimit, so existing slug-targeted calls keep their exact
// behavior. On a slug miss it falls back to a `$alias` bound in THIS channel —
// the same alias_bindings entry `/qurl get` and `/qurl-admin revoke` resolve —
// so an admin who knows a resource only by an alias whose name differs from the
// connector slug (e.g. an alias re-attached to a re-created connector) can still
// target it.
//
// Precedence is slug-FIRST here — the reverse of `/qurl get` / `/qurl-admin
// revoke`, which resolve the alias first. Deliberate: it preserves the `?slug=`
// filter and the unbounded-by-listResourcesScanLimit behavior for existing slug
// calls. The only observable difference is a token that is BOTH a live slug AND
// a channel alias bound to a DIFFERENT resource — it resolves to the slug here
// vs the alias in `/qurl get`. Pathological in practice: install binds
// alias==slug→the same resource, so the orders agree for the common case.
//
// channelID == "" or an unwired AdminStore skips the alias fallback (slug-only).
// On any failure it returns (nil, nil, userMsg); callers
// `if msg != "" { return msg }`.
func (h *Handler) resolveTunnelByID(ctx context.Context, log *slog.Logger, teamID, channelID, id string) (c *client.Client, resource *client.Resource, userMsg string) {
	c, err := h.authenticatedClient(ctx, teamID)
	if err != nil {
		log.Error("display-name: failed to get API key", "error", err, "team_id", teamID, "id", id)
		return nil, nil, authErrorMessage(err)
	}
	page, err := c.ListResources(ctx, client.ListResourcesInput{Slug: id})
	if err != nil {
		log.Error("display-name: tunnel id resolution failed", "error", err, "team_id", teamID, "id", id)
		return nil, nil, sanitizeAPIError(err, "Failed to look up the qURL Connector")
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

	// Slug miss: try a channel `$alias` bound here. Resolves the same
	// alias_bindings entry `/qurl get` reads, then recovers the full resource so
	// the Type==Tunnel / Status==Active guard and unset's slug both still hold.
	if channelID != "" && h.cfg.AdminStore != nil {
		boundID, found, lookupErr := h.cfg.AdminStore.LookupChannelAlias(ctx, teamID, channelID, id)
		switch {
		case lookupErr != nil:
			// channel_policies is a DIFFERENT table from the workspace_mappings the
			// admin gate read, so it can be unavailable while the gate passes. Surface
			// the service-unavailable signal (matching resolveTokenForGet) rather than
			// falling through to the "no such id" copy, which would misdiagnose an
			// outage as a typo.
			log.Warn("display-name: channel alias lookup failed", "error", lookupErr, "team_id", teamID, "channel_id", channelID, "id", id)
			return nil, nil, serviceUnreachableMessage
		case found:
			// Legacy guard, mirroring resolveTokenForGet: a pre-resource set-alias
			// row stored a raw URL, not an `r_` id. It can't resolve to a tunnel, so
			// refuse with the same re-bind hint `/qurl get` gives — and skip the
			// resource scan it could never match (which, in a >first-page workspace,
			// would otherwise misreport it as a lookup limit).
			if !strings.HasPrefix(boundID, "r_") {
				log.Warn("display-name: channel alias bound to a non-resource-id target", "team_id", teamID, "channel_id", channelID, "id", id)
				return nil, nil, legacyAliasBindingMessage(id)
			}
			r, msg := h.resolveActiveTunnelByResourceID(ctx, log, c, teamID, id, boundID)
			if msg != "" {
				return nil, nil, msg
			}
			return c, r, ""
		}
	}

	log.Info("display-name: no active tunnel for id", "team_id", teamID, "id", id)
	return nil, nil, fmt.Sprintf("No qURL Connector with id `%s` was found. Run `/qurl list` to see your qURL Connector ids.", id)
}

// resolveActiveTunnelByResourceID recovers the full resource a channel alias
// pointed at and asserts it's an active tunnel before it becomes a PATCH
// target. qurl-service has no get-by-id, so it scans the first ListResources
// page (the same bounded scan `/qurl list` and protect use) and matches on
// resource_id. On no active-tunnel match it returns a friendly message, never a
// PATCH against the wrong/dead target. It distinguishes no-match cases by what
// the scan can prove: a seen non-tunnel resource gets connector-only copy, a
// seen inactive tunnel gets the unset-alias hint, and a resourceID NOT seen
// while more pages remain (HasMore) gets a non-destructive lookup-limit message
// because it may be a live alias on a later page. Callers pre-filter non-`r_`
// bindings (legacy raw-URL rows), so resourceID is always a real resource id
// here. Returns (resource, "") on success or (nil, userMsg).
func (h *Handler) resolveActiveTunnelByResourceID(ctx context.Context, log *slog.Logger, c *client.Client, teamID, id, resourceID string) (resource *client.Resource, userMsg string) {
	page, err := c.ListResources(ctx, client.ListResourcesInput{Limit: listResourcesScanLimit})
	if err != nil {
		log.Error("display-name: resource lookup by id failed", "error", err, "team_id", teamID, "id", id, "resource_id", resourceID)
		return nil, sanitizeAPIError(err, "Failed to look up the qURL Connector")
	}
	// Track whether resourceID was SEEN on this page, separately from whether it
	// matched as an active tunnel. A seen-but-dead target (revoked, or a URL
	// resource) is definitively stale even when more pages exist, so it must get
	// the unset-alias hint — only a genuine "not seen at all + more pages remain"
	// miss is the inconclusive scan-window case.
	seen := false
	for i := range page.Resources {
		r := &page.Resources[i]
		if r.ResourceID != resourceID {
			continue
		}
		seen = true
		if r.Type != client.ResourceTypeTunnel {
			log.Info("display-name: channel alias points at non-tunnel resource", "team_id", teamID, "id", id, "resource_id", resourceID, "resource_type", r.Type)
			return nil, fmt.Sprintf("`%s` is an alias in this channel, but Display Names apply only to qURL Connectors, not URL resources. Run `/qurl list` to see active connectors.", id)
		}
		if r.Status == client.StatusActive {
			return r, ""
		}
	}
	if !seen && page.HasMore {
		// Not found on the first page AND more pages exist — we CANNOT conclude the
		// alias is stale (its connector may be on a later page). Must not recommend
		// unset-alias: following that hint would unbind a possibly-live alias. Same
		// listResourcesScanLimit bound `/qurl list` carries (tracked by #590); the
		// difference is this is a mutation verb, so the copy stays non-destructive.
		log.Info("display-name: channel alias target not on first resource page; more pages exist", "team_id", teamID, "id", id, "resource_id", resourceID, "scan_limit", listResourcesScanLimit)
		return nil, fmt.Sprintf("Couldn't locate the qURL Connector bound to `%s` among the first %d resources, and your workspace has more — this is a lookup limit, not necessarily a stale alias.", id, listResourcesScanLimit)
	}
	// Either a seen-but-inactive tunnel, or a complete scan (no more pages) without
	// a match — both definitively mean the alias no longer resolves to an active
	// connector, so the unset-alias hint is correct.
	log.Info("display-name: channel alias points at no active tunnel", "team_id", teamID, "id", id, "resource_id", resourceID, "seen_on_first_page", seen)
	return nil, fmt.Sprintf("`%s` is an alias in this channel but no longer points at an active qURL Connector. Run `/qurl list` to see active connectors, or `/qurl-admin unset-alias $%s` to clear the alias.", id, id)
}
