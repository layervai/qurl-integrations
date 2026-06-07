package internal

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/layervai/qurl-integrations/shared/client"
)

// listResourcesScanLimit is the per-page size for the `/v1/resources` fetches
// backing `/qurl list`, `/qurl aliases`, the URL resource-alias fallback in
// `/qurl get`, and the `/qurl-admin protect-url` picker.
//
//   - `/qurl list` pages through resources until every resource in the channel
//     allow-set has been seen (see [Handler.fetchAllowedResources]) — tunnels
//     and URL resources alike — so the listing is complete regardless of how the
//     owner's full resource set sorts (#590). The page size only affects how many
//     requests that walk takes, not which resources render.
//   - `/qurl aliases`, the `/qurl get` URL resource-alias fallback, and the
//     `/qurl-admin protect-url` picker each scan a SINGLE page. A bound/aliased
//     resource_id past this page surfaces the triage warning in
//     [Handler.processAliases] (aliases) or is missed by the get fallback /
//     duplicate-alias ambiguity check (#590). A server-side type filter (tracked
//     in #531) would let those single-page surfaces drop to a normal size.
const listResourcesScanLimit = 100

const (
	// listResourcesEmptyMessage is the friendly empty-state copy when no
	// protected resource is available in THIS channel. It avoids admin-only
	// tunnel setup terminology for ordinary users.
	listResourcesEmptyMessage = ":mag: No protected resources are available in this channel yet. Ask a Slack admin to set one up or make one available here."

	// listResourcesEmptyAdminMessage is shown only after a successful admin
	// check, so it can name the admin-only setup command and Edit recovery path.
	listResourcesEmptyAdminMessage = ":mag: No protected resources are available in this channel yet. Install one here with `/qurl-admin protect-connector <id>`, or make an existing resource available here from `/qurl list` → *Edit* in a channel where it already appears."
)

// listCreateButtonLabel is the text on the per-row "Create qURL" button.
// Clicking it creates a qURL for that row's resource — the same work
// as typing `/qurl get $<slug>`. (Brand spelling: lowercase q, uppercase URL.)
const listCreateButtonLabel = "Create qURL"

// listHeaderBlockText titles the interactive /qurl list. It rides on a `header`
// block, whose text object is plain_text — so the `:lock:` shortcode renders as
// an icon but there is no mrkdwn bold. The bold "*Protected Resources:*"
// form still leads the plain-text fallback `body`.
const listHeaderBlockText = ":lock: Protected Resources"

// listCreateButtonMaxRows caps how many tunnel rows /qurl list renders as
// interactive section+button blocks. Slack rejects a message with more
// than 50 blocks; the rendered shape is header (1) + N row sections +
// footer (1) + optional has-more note (1), so 45 rows tops out at 48 —
// 2 blocks of deliberate headroom against a future non-row block. A
// workspace with more tunnels than this degrades to the plain-text
// listing (every tunnel still visible — the text path has no block
// ceiling — just without the per-row button) rather than a message Slack
// would refuse to render. Tunnels are created deliberately (via `/qurl
// tunnel install`), so a real workspace is far below this; see
// [listResourcesScanLimit].
//
// NOTE: [listEditButtonMaxRows] is derived as this/2 to keep admin (2-block)
// rows under the same ceiling, so bumping this to an odd value or adding
// another always-on block erodes the edit path's 3-block margin — keep the
// 2*listEditButtonMaxRows+3 <= 50 invariant in mind when changing it.
const listCreateButtonMaxRows = 45

// listEditButtonLabel is the text on the admin-only "Edit" button rendered
// alongside "Create qURL" on each `/qurl list` row for qURL admins.
// Clicking it opens the TunnelEditModal pre-filled with the tunnel's Display
// Name and channel aliases.
const listEditButtonLabel = "Edit"

// listRevokeButtonLabel is the text on the admin-only red "Revoke" button
// rendered beside "Edit" on each `/qurl list` row. Clicking it (after the
// confirm dialog) revokes the row's resource and all of its qURLs.
const listRevokeButtonLabel = "Revoke"

// listEditButtonMaxRows caps how many rows may carry the per-row Edit button.
// An admin row renders TWO blocks (a section line + an actions block carrying
// Create qURL + Edit) versus one for the Create-only path, so it halves
// [listCreateButtonMaxRows]'s row budget — derived from it (not a separate
// magic number) so the two can't drift. At 22 rows the message is header (1) +
// 2N + footer (1) + optional has-more (1) = 47 blocks, under Slack's 50-block
// ceiling (invariant: 2*listEditButtonMaxRows + 3 <= 50). Past this, the per-row
// Edit button is dropped but Create qURL buttons still render (one block per
// row, like any caller's list); only past [listCreateButtonMaxRows] does the
// listing degrade to plain text.
const listEditButtonMaxRows = listCreateButtonMaxRows / 2

// slackButtonValueMaxBytes is Slack's documented cap on a button element's
// `value`. The Edit button carries a [tunnelEditButtonValue] JSON snapshot; if
// it would exceed this, the row falls back to a Create-only button (no Edit).
// The guard compares byte length, which is intentionally conservative against
// Slack's character cap (bytes >= runes) — so a multibyte Display Name in the
// snapshot can never slip past it. Don't "fix" this to rune-counting; that
// would loosen the guard.
const slackButtonValueMaxBytes = 2000

const (
	commonListResourcesFailedPrefix = "Failed to list qURL resources"
	listResourcesFailedLogMessage   = "list: list resources failed"
)

// tunnelEditButtonValue is the JSON snapshot carried on a `/qurl list` Edit
// button's `value`, so opening the edit modal needs no extra upstream read
// (keeping it inside Slack's ~3s trigger_id window). Short JSON keys keep it
// under [slackButtonValueMaxBytes]. Aliases is the EXTRA-alias set — the
// channel aliases bound to this tunnel other than the row's primary Token — so
// the modal never offers to unbind the tunnel's own canonical name.
type tunnelEditButtonValue struct {
	ResourceID  string   `json:"r"`
	Token       string   `json:"t"`
	DisplayName string   `json:"d,omitempty"`
	Aliases     []string `json:"a,omitempty"`
}

// buildTunnelEditButtonValue marshals a row's edit snapshot. boundAliases is
// the full channel-alias set for the resource; the primary token is excluded
// (the modal manages only the extra aliases). Returns ("", false) when the
// marshaled value would exceed Slack's button-value cap, so the caller renders
// a Create-only button instead.
func buildTunnelEditButtonValue(resourceID, token, displayName string, boundAliases []string) (string, bool) {
	v := tunnelEditButtonValue{
		ResourceID:  resourceID,
		Token:       token,
		DisplayName: displayName,
		Aliases:     aliasesExcluding(boundAliases, token),
	}
	b, err := json.Marshal(v)
	if err != nil || len(b) > slackButtonValueMaxBytes {
		return "", false
	}
	return string(b), true
}

// tunnelRevokeButtonValue is the JSON snapshot on a `/qurl list` Revoke
// button's `value`: the resolved resource_id (what DELETE /v1/resources/{id}
// needs — the list already resolved the slug, so the click handler doesn't
// re-resolve) and the row's `$<token>` (for the reply + confirm copy). Far
// smaller than the Edit snapshot, so it never approaches the value cap.
type tunnelRevokeButtonValue struct {
	ResourceID string `json:"r"`
	Token      string `json:"t"`
}

// buildTunnelRevokeButtonValue marshals a row's revoke snapshot. Returns
// ("", false) if it would exceed Slack's button-value cap (unreachable in
// practice — two short fields — but mirrors buildTunnelEditButtonValue so the
// caller can drop the button rather than emit an oversized one).
func buildTunnelRevokeButtonValue(resourceID, token string) (string, bool) {
	b, err := json.Marshal(tunnelRevokeButtonValue{ResourceID: resourceID, Token: token})
	if err != nil || len(b) > slackButtonValueMaxBytes {
		return "", false
	}
	return string(b), true
}

// parseTunnelRevokeButtonValue is the inverse of buildTunnelRevokeButtonValue,
// used by handleListRevokeClick to recover the resource_id + token from a
// clicked button. Mirrors parseTunnelEditButtonValue.
func parseTunnelRevokeButtonValue(value string) (tunnelRevokeButtonValue, error) {
	var v tunnelRevokeButtonValue
	if err := json.Unmarshal([]byte(value), &v); err != nil {
		return tunnelRevokeButtonValue{}, err
	}
	return v, nil
}

// aliasesExcluding returns boundAliases without the primary token, preserving
// order. The token's binding is the tunnel's canonical channel name and is
// never managed through the edit modal.
func aliasesExcluding(boundAliases []string, token string) []string {
	out := make([]string, 0, len(boundAliases))
	for _, a := range boundAliases {
		if a != token {
			out = append(out, a)
		}
	}
	return out
}

// listCallerCanEdit reports whether the `/qurl list` caller should see the
// admin-only Edit button. It requires the full edit wiring — the modal opener
// (OpenView), the alias store (to reconcile bindings on submit), and the admin
// store (to gate) — plus the caller being a qURL admin. A CheckAdmin error
// hides the button (fail-closed for the affordance) WITHOUT failing the
// listing: the list still renders with Create qURL buttons. Runs on the async
// worker ctx, bounded by adminGateBudget like the other admin gates.
func (h *Handler) listCallerCanEdit(ctx context.Context, log *slog.Logger, teamID, userID string) bool {
	if h.cfg.OpenView == nil || h.aliasStore == nil || h.cfg.AdminStore == nil {
		return false
	}
	if teamID == "" || userID == "" {
		return false
	}
	gateCtx, cancel := context.WithTimeout(ctx, adminGateBudget)
	defer cancel()
	isAdmin, _, err := h.cfg.AdminStore.CheckAdmin(gateCtx, teamID, userID)
	if err != nil {
		log.Debug("list: admin check for Edit button failed — hiding Edit", "error", err, "team_id", teamID)
		return false
	}
	return isAdmin
}

// listResourcesEmptyMessageForCaller returns the empty-state copy for /qurl
// list. Non-admins never see the admin-only tunnel setup command. Admins get a
// direct setup hint, but only on a successful CheckAdmin read; failures degrade
// to the ordinary user copy so the list path stays fail-soft. This read happens
// only on the zero-resource path, where the extra hint changes the otherwise
// empty response without adding a per-row gate.
func (h *Handler) listResourcesEmptyMessageForCaller(ctx context.Context, log *slog.Logger, teamID, userID string) string {
	if h.cfg.AdminStore == nil || teamID == "" || userID == "" {
		return listResourcesEmptyMessage
	}
	gateCtx, cancel := context.WithTimeout(ctx, adminGateBudget)
	defer cancel()
	isAdmin, _, err := h.cfg.AdminStore.CheckAdmin(gateCtx, teamID, userID)
	if err != nil {
		log.Debug("list: admin check for empty-state hint failed — hiding admin setup hint", "error", err, "team_id", teamID)
		return listResourcesEmptyMessage
	}
	if !isAdmin {
		return listResourcesEmptyMessage
	}
	return listResourcesEmptyAdminMessage
}

// listFooterText is the guidance line under /qurl list when rendered as
// plain text — both the Block Kit fallback (`text`) and the visible
// message when the tunnel set is too large for per-row buttons (see
// [listCreateButtonMaxRows]). It names the typed path and the
// one-time-use default; the button path is named only in
// [listFooterButtons], shown when the buttons are actually present.
const listFooterText = "Rows that start with a `$...` token can be used with `/qurl get`; the `(alias: …)` entries are alternate names for the same resource in this channel. Run `/qurl get $token` to create a qURL link — it opens access once, then expires."

// listFooterButtons is the guidance line beneath the interactive /qurl
// list (the version with a per-row Create qURL button). It names BOTH
// ways to mint — tapping the button and the typed command — and the
// one-time-use default.
const listFooterButtons = "Tap *Create qURL* on any row with a button, or copy a `$...` token or alias and run `/qurl get`, to create a qURL link — it opens access once, then expires."

// handleListResources implements `/qurl list`. It lists the resources available
// in THIS channel: tunnel resources and URL resources. Each row with a usable
// token is copy-paste-ready for `/qurl get $<token>` without a manual lookup.
// Tunnel rows prefer their stable slug; URL rows prefer their resource alias,
// then a channel-bound alias when the resource has no alias.
//
// The listing is CHANNEL-SCOPED: a member sees only the tunnels in this
// channel's [slackdata.Store.AllowedResourceIDsForChannel] set (the union of
// its `allowed_resource_ids` and `alias_bindings` values) — the same definition
// `/qurl get` mints against and `/qurl aliases` lists. A resource available in
// another channel does not appear here until an admin makes it available here
// via the Edit modal or a channel alias binding. This restores #234's
// per-channel disclosure (reverted in #459), but with the recoverable path
// #459 lacked:
// the empty state names the Edit-modal recovery flow, and the gate applies to
// admins too (an admin who wants a tunnel here grants channel access, rather than seeing
// every workspace tunnel from every channel). List, alias, and mint now share
// one channel-scoped definition, closing the former list/mint asymmetry
// (TODO(#460)).
func (h *Handler) handleListResources(w http.ResponseWriter, values url.Values) {
	h.runAsync(w, "list", values, func(ctx context.Context, log *slog.Logger) {
		h.processListResources(ctx, log, values)
	})
}

// listChannelScope resolves the channel allow-set that scopes /qurl list — the
// union of the channel's allowed_resource_ids and alias_bindings values, the
// SAME set `/qurl get` mints against. It handles every fail-closed
// short-circuit by posting the right message and returning proceed=false:
//
//   - empty channel_id (synthetic payload): refuse rather than fan out
//     workspace-wide — the disclosure this scoping closes. Mirrors /qurl aliases.
//   - AdminStore nil (no-DDB sandbox): the scope can't be computed, so fail
//     closed rather than disclose everything — same posture as aliases/get.
//   - allow-set read error: fail CLOSED (never fall back to an unscoped list).
//   - nothing protected here: the channel empty state, WITHOUT the upstream
//     ListResources call (the common case for a channel with no tunnels).
//
// On success it returns the non-empty allow-set and true.
func (h *Handler) listChannelScope(ctx context.Context, log *slog.Logger, responseURL, teamID, channelID, userID string) (map[string]struct{}, bool) {
	if channelID == "" {
		log.Warn("list: empty channel_id; refusing workspace-wide list")
		_ = h.postResponse(log, responseURL, ":warning: "+channelRequiredMessage)
		return nil, false
	}
	if h.cfg.AdminStore == nil {
		log.Warn("list: AdminStore is nil; cannot scope listing to channel")
		_ = h.postResponse(log, responseURL, ":warning: Admin features are not configured for this deployment.")
		return nil, false
	}
	allowed, err := h.cfg.AdminStore.AllowedResourceIDsForChannel(ctx, teamID, channelID)
	if err != nil {
		log.Warn("list: channel allow-set fetch failed — failing closed", "error", err, "team_id", teamID, "channel_id", channelID)
		_ = h.postResponse(log, responseURL, ":warning: "+serviceUnreachableMessage)
		return nil, false
	}
	if len(allowed) == 0 {
		_ = h.postResponse(log, responseURL, h.listResourcesEmptyMessageForCaller(ctx, log, teamID, userID))
		return nil, false
	}
	return allowed, true
}

// processListResources is the async-worker body for /qurl list.
func (h *Handler) processListResources(ctx context.Context, log *slog.Logger, values url.Values) {
	responseURL := values.Get(fieldResponseURL)
	teamID := values.Get(fieldTeamID)
	userID := strings.TrimSpace(values.Get(fieldUserID))
	channelID := values.Get(fieldChannelID)

	// Resolve the channel scope first. This handles every fail-closed
	// short-circuit (no channel, no AdminStore, read error, nothing exposed)
	// and, on the common "nothing protected here" path, avoids the upstream
	// ListResources call entirely.
	allowed, ok := h.listChannelScope(ctx, log, responseURL, teamID, channelID, userID)
	if !ok {
		return
	}

	c, err := h.authenticatedClient(ctx, teamID)
	if err != nil {
		log.Error("list: API key lookup failed", "error", err)
		_ = h.postResponse(log, responseURL, ":warning: "+authErrorMessage(err))
		return
	}

	// `/qurl list` shows the channel's mintable resources — tunnels AND URL
	// resources — scoped to the channel allow-set, so every member sees exactly
	// the resources they could `/qurl get` here, no more. The listing is driven by
	// paging the owner's resources until every allow-set member has been seen
	// (#590): a resource protected in this channel can no longer be missed for
	// sorting past a fixed scan window, because the walk doesn't stop at one. The
	// common case — every exposed resource on the first page — still costs a
	// single request.
	resources, err := h.fetchAllowedResources(ctx, log, c, allowed)
	if err != nil {
		_ = h.postResponse(log, responseURL, ":warning: "+mapListResourcesError(log, teamID, err))
		return
	}

	if len(resources) == 0 {
		_ = h.postResponse(log, responseURL, h.listResourcesEmptyMessageForCaller(ctx, log, teamID, userID))
		return
	}

	// Map each resource_id to its channel-bound `$alias` shortcuts so each row
	// can show them next to the resource's primary token. Best-effort: a fetch
	// failure renders without the channel alias extras. Built BEFORE the sort
	// because the sort keys on [resourceDisplayTokenForList], including tunnel
	// rows whose first bound alias becomes the primary token when they have no
	// intrinsic token — the sort key must match what the row renders.
	aliasMap := h.channelAliasesByResourceID(ctx, log, teamID, channelID)
	channelAliasOwners := channelAliasOwnersByAlias(aliasMap)
	sharedAliases := sharedResourceAliases(resources)
	tunnelSlugOwners := tunnelSlugOwnersBySlug(resources)

	// Precompute each row's display token once in a row model
	// rather than recomputing it inside the O(n log n) comparator below.
	rows := make([]resourceListRow, 0, len(resources))
	for i := range resources {
		aliases := aliasMap[resources[i].ResourceID]
		token, blockedAlias := resourceDisplayTokenForList(&resources[i], aliases, sharedAliases, channelAliasOwners, tunnelSlugOwners)
		rows = append(rows, resourceListRow{
			Resource:     resources[i],
			Aliases:      aliases,
			Token:        token,
			BlockedAlias: blockedAlias,
		})
	}

	// Stable order for two-call idempotency at the Slack ephemeral
	// surface — the server's pagination cursor implies an order but not a
	// stable one across re-queries. Tokenless rows (slug-less, alias-less —
	// the bare-resource_id "(no slug …)" rows) sort to the END so the
	// legible $slug/$alias tunnels lead rather than a wall of opaque
	// r_<id>s. Within each cohort, sort by the displayed token with
	// resource_id as a tiebreaker (so two rows sharing a token — a slug ==
	// another row's alias, or two tokenless rows — order deterministically
	// rather than inheriting the unstable upstream order). BEFORE formatting.
	sort.SliceStable(rows, func(i, j int) bool {
		ti, tj := rows[i].Token, rows[j].Token
		if (ti == "") != (tj == "") {
			return ti != "" // non-empty (legible) token sorts first
		}
		if ti != tj {
			return ti < tj
		}
		return rows[i].Resource.ResourceID < rows[j].Resource.ResourceID
	})

	// Render each resource as a section block carrying a "Create qURL"
	// accessory button (so a click mints the one-time link without the
	// user copy-pasting `/qurl get $token`), and in parallel build the
	// plain-text `body` Slack uses as the block fallback. useButtons is
	// false when the resource set exceeds Slack's per-message block ceiling
	// (see listCreateButtonMaxRows) — then only the text path renders.
	//
	// Admin callers (with the modal/alias/admin wiring present) also get an
	// Edit button per row. The list is ephemeral — only the caller sees it —
	// so gating the button on the caller's admin status is the whole access
	// boundary for the affordance; the mutation it opens is re-gated at
	// view_submission time (see handleTunnelEditSubmission).
	// Buttons render whenever the tunnel set fits the per-message block ceiling
	// (listCreateButtonMaxRows). An admin row carries an extra block (Edit lives
	// in its own actions block), so per-row Edit only fits under the halved
	// listEditButtonMaxRows budget. Past that an admin still gets the Create-only
	// buttons every caller gets — just no per-row Edit — rather than the whole
	// list collapsing to text (which would be a surprising admin-only regression
	// versus the same tunnel count for a non-admin).
	//
	// listCallerCanEdit costs a CheckAdmin read, so the size gate is checked
	// first (&& short-circuits): a list too large to carry Edit buttons skips the
	// read entirely, since the answer can't change the output.
	useButtons := len(rows) <= listCreateButtonMaxRows
	showEdit := len(rows) <= listEditButtonMaxRows &&
		h.listCallerCanEdit(ctx, log, teamID, userID)
	lines := make([]string, 0, len(rows))
	var blocks []any
	if useButtons {
		blockCap := len(rows) + 3
		if showEdit {
			blockCap = len(rows)*2 + 3
		}
		blocks = make([]any, 0, blockCap)
		blocks = append(blocks, headerBlock(listHeaderBlockText))
	}
	for i := range rows {
		row := &rows[i]
		line := formatResourceListLineWithToken(&row.Resource, row.Aliases, row.Token, row.BlockedAlias)
		lines = append(lines, line)
		if !useButtons {
			continue
		}
		blocks = appendResourceListBlocks(blocks, &row.Resource, row.Aliases, row.Token, row.BlockedAlias, showEdit)
	}

	body := "*Protected Resources:*\n" + strings.Join(lines, "\n") + "\n\n_" + listFooterText + "_"
	if useButtons {
		blocks = append(blocks, contextBlock(listFooterButtons))
	}

	if !useButtons {
		_ = h.postResponse(log, responseURL, body)
		return
	}
	_ = h.postResponseBlocks(log, responseURL, body, blocks)
}

type resourceListRow struct {
	Resource     client.Resource
	Aliases      []string
	Token        string
	BlockedAlias string
}

func appendResourceListBlocks(blocks []any, resource *client.Resource, aliases []string, token, blockedAlias string, showEdit bool) []any {
	// The block path renders a richer, multi-line section than the plain-text
	// fallback line: the `$id` bold on its own row, resource detail beneath it,
	// and a faint aliases line when present. It takes the precomputed display
	// token so the section can never name a different token than the row's button
	// mints against.
	sectionText := formatResourceListSectionWithToken(resource, aliases, token, blockedAlias)
	// Button values stay as the same token a user would paste into `/qurl get`.
	// We deliberately re-run get resolution on click instead of snapshotting a
	// resource_id here, so channel policy and URL alias ambiguity are checked at
	// click time too.
	// token is "" only for a resource with no `$<token>` that `/qurl get` (or a
	// button) could mint against — so that row gets no button.
	if token == "" {
		return append(blocks, sectionBlock(sectionText))
	}
	// Create qURL is the row's headline action. It renders as the primary
	// (filled) button ONLY when an Edit button sits beside it — admin tunnel
	// rows, where primary expresses the Create-over-Edit hierarchy. A
	// create-only row has nothing to outrank, and a whole column of lone
	// primaries reads as noise (Slack advises using `primary` sparingly), so it
	// gets a default-style button. Admin tunnel rows pair the two in an actions
	// block; the Edit button carries the row's edit snapshot so opening the modal
	// needs no extra read. A snapshot too large for a button value falls through
	// to the Create-only accessory path below.
	if showEdit && isTunnelResource(resource) {
		if editVal, ok := buildTunnelEditButtonValue(resource.ResourceID, token, resource.Description, aliases); ok {
			row := []map[string]any{
				primaryButtonElement(listCreateButtonLabel, listCreateQurlActionID, token),
				buttonElement(listEditButtonLabel, listEditTunnelActionID, editVal),
			}
			// Red "Revoke" beside Edit; the confirm dialog gates the destructive
			// action and the value carries the resolved resource_id so the click
			// handler needs no slug re-resolve.
			if revokeVal, ok := buildTunnelRevokeButtonValue(resource.ResourceID, token); ok {
				row = append(row, withConfirmDialog(
					dangerButtonElement(listRevokeButtonLabel, listRevokeTunnelActionID, revokeVal),
					"Revoke $"+escapeMrkdwnCode(token)+"?",
					revokeConfirmText,
					"Revoke",
				))
			}
			return append(blocks, sectionBlock(sectionText), actionsBlock(row...))
		}
	}
	return append(blocks, sectionWithAccessory(sectionText, buttonElement(listCreateButtonLabel, listCreateQurlActionID, token)))
}

// filterListableResources returns only live resources Slack can mint from the
// fetched page: tunnels and URL resources. Unknown/future resource types are
// dropped until the Slack get/list contract knows how to resolve and describe
// them.
//
// Revoked resources are dropped too. The list endpoint is status-visible for
// both active AND revoked rows (a revoke is a soft delete; the row is
// hard-deleted only after a retention window), so without this guard a revoked
// resource would keep appearing — with a "Create qURL" button that can't mint
// against it. A missing/empty status is treated as live, so the guard can only
// ever hide an explicitly revoked row.
func filterListableResources(resources []client.Resource) []client.Resource {
	out := make([]client.Resource, 0, len(resources))
	for i := range resources {
		if resourceTypeMintableFromSlack(&resources[i]) && resources[i].Status != client.StatusRevoked {
			out = append(out, resources[i])
		}
	}
	return out
}

func resourceTypeMintableFromSlack(r *client.Resource) bool {
	return isTunnelResource(r) || isURLResource(r)
}

func isTunnelResource(r *client.Resource) bool {
	return r.Type == client.ResourceTypeTunnel
}

// isURLResource treats empty-type target_url rows as URL resources to preserve
// legacy qurl-service responses until Type is guaranteed on every URL row.
func isURLResource(r *client.Resource) bool {
	return r.Type == client.ResourceTypeURL || (r.Type == "" && r.TargetURL != "")
}

// listMaxResourcePages bounds how many `/v1/resources` pages
// [Handler.fetchAllowedResources] will walk. It is a safety backstop, not an
// expected limit: the walk normally stops as soon as every allow-set member is
// found (the common case is the first page). The cap only bites when an
// allow-set id is never found — e.g. a stale channel binding to a resource the
// owner has since deleted — which would otherwise scan to the end of the
// owner's resource list on every call. At listResourcesScanLimit (100) rows
// per page this covers far more resources than any real workspace holds, so a
// capped walk can only ever drop an id the owner's list no longer contains
// (fail-safe under-disclosure, never a live listed resource).
const listMaxResourcePages = 50

// fetchAllowedResources returns the live resources — tunnels AND URL resources —
// protected in the current channel, by paging `/v1/resources` until every
// resource_id in the channel allow-set has been seen (or the listing is
// exhausted / the page cap is hit). This is the channel-scoped disclosure half
// of `/qurl list`: a member sees exactly the resources they could `/qurl get`
// here.
//
// Paging until the allow-set is satisfied — rather than scanning a single fixed
// page and filtering it — is the #590 fix: a resource protected in this channel
// can no longer be omitted for sorting past the page window. Allow-set ids are
// matched as resources stream past and the walk stops as soon as the last one is
// found, so a workspace whose exposed resources all land on the first page still
// costs a single request (no regression to the common case).
//
// Allow-set membership is matched here; non-mintable / revoked rows are dropped
// by filterListableResources (tunnels + URL resources). (#596 introduced this as
// fetchAllowedTunnels; it was generalized to all listable resource types when
// #599's URL-resource support merged, so the channel-scoped #590 paging fix now
// covers URL resources too.)
func (h *Handler) fetchAllowedResources(ctx context.Context, log *slog.Logger, c *client.Client, allowed map[string]struct{}) ([]client.Resource, error) {
	pending := make(map[string]struct{}, len(allowed))
	for id := range allowed {
		pending[id] = struct{}{}
	}
	matched := make([]client.Resource, 0, len(allowed))
	cursor := ""
	for page := 0; page < listMaxResourcePages; page++ {
		out, err := c.ListResources(ctx, client.ListResourcesInput{Limit: listResourcesScanLimit, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for i := range out.Resources {
			if _, ok := pending[out.Resources[i].ResourceID]; ok {
				delete(pending, out.Resources[i].ResourceID)
				matched = append(matched, out.Resources[i])
			}
		}
		// Stop once every exposed id is found, or the listing is exhausted.
		if len(pending) == 0 || !out.HasMore || out.NextCursor == "" {
			return filterListableResources(matched), nil
		}
		cursor = out.NextCursor
	}
	// Page cap reached with allow-set ids still unresolved — the backstop in
	// listMaxResourcePages. Not an expected path; surface it for operators. The
	// unresolved ids simply don't render (fail-safe, never a leak).
	log.Warn("list: resource-page cap reached before resolving every allow-set member",
		"pages", listMaxResourcePages, "unresolved", len(pending))
	return filterListableResources(matched), nil
}

// tunnelToken returns the resource-intrinsic `$<token>` for a tunnel.
// Precedence:
//
//  1. Slug — the stable, owner-scoped tunnel handle. `/qurl-admin
//     protect-connector <slug>` binds `$<slug>` as a channel alias, so the slug
//     pastes straight into `/qurl get $<slug>`. This is the common case
//     and the identifier we want to surface (never the opaque r_<id>).
//  2. Resource-level alias — fallback when a tunnel somehow carries no
//     slug but does have an alias.
//  3. "" — a slug-less, resource-alias-less tunnel has no intrinsic
//     `$<token>` the user can `get` (get is slug/alias-only now).
//     Vanishingly rare (tunnels are created with a slug). This function
//     stops at ""; the channel-alias promotion and the bare-resource_id
//     fallback both live downstream — in [tunnelDisplayToken] and
//     [formatTunnelListLine] respectively, not here.
func tunnelToken(r *client.Resource) string {
	if r.Slug != "" {
		return r.Slug
	}
	return r.Alias // "" when the tunnel has neither a slug nor an alias
}

// tunnelDisplayToken is the `$<token>` shown for a tunnel in /qurl list AND
// the key rows sort by — both go through this one function so the sort order
// matches what each row actually renders. It extends [tunnelToken] with
// channel-alias promotion: when a tunnel has no intrinsic token (slug-less,
// resource-alias-less) but a channel `$alias` binds to it, the first
// (lexically sorted) bound alias becomes the token. Returns "" only when
// there is no token at all — formatTunnelListLine then renders the bare
// resource_id.
//
// Surprise to note: because promotion picks the lexically-first bound alias,
// a slug-less tunnel's displayed identity can change if an admin later binds
// an alphabetically-earlier alias (e.g. `$zebra` → `$aardvark`). Rare —
// installs come with a slug — but documented so the flip isn't a mystery.
func tunnelDisplayToken(r *client.Resource, boundAliases []string) string {
	if t := tunnelToken(r); t != "" {
		return t
	}
	if len(boundAliases) > 0 {
		return boundAliases[0] // sorted by channelAliasesByResourceID
	}
	return ""
}

func resourceDisplayToken(r *client.Resource, boundAliases []string) string {
	if isURLResource(r) {
		return urlDisplayToken(r, boundAliases)
	}
	return tunnelDisplayToken(r, boundAliases)
}

// resourceDisplayTokenForList mirrors /qurl get precedence so list rows never
// advertise a Create qURL token that would resolve to a different resource.
func resourceDisplayTokenForList(r *client.Resource, boundAliases []string, sharedResourceAliases map[string]struct{}, channelAliasOwners, tunnelSlugOwners map[string]string) (token, blockedAlias string) {
	if isURLResource(r) && r.Alias != "" {
		_, shared := sharedResourceAliases[r.Alias]
		owner, channelAliasExists := channelAliasOwners[r.Alias]
		channelAliasPointsHere := channelAliasExists && owner == r.ResourceID
		channelAliasPointsElsewhere := channelAliasExists && owner != r.ResourceID
		slugOwner, slugExists := tunnelSlugOwners[r.Alias]
		tunnelSlugPointsElsewhere := slugExists && slugOwner != r.ResourceID
		// Mirror /qurl get precedence: a channel alias pointing here makes the
		// token safe, a channel alias pointing elsewhere wins over this row, and
		// tunnel slugs or shared URL aliases would make the token resolve away
		// from (or ambiguously among) URL rows.
		if ((shared || tunnelSlugPointsElsewhere) && !channelAliasPointsHere) || channelAliasPointsElsewhere {
			if token := firstAliasOtherThan(boundAliases, r.Alias); token != "" {
				return token, r.Alias
			}
			return "", r.Alias
		}
	}
	return resourceDisplayToken(r, boundAliases), ""
}

func tunnelSlugOwnersBySlug(resources []client.Resource) map[string]string {
	owners := make(map[string]string)
	for i := range resources {
		if isTunnelResource(&resources[i]) && resources[i].Slug != "" {
			owners[resources[i].Slug] = resources[i].ResourceID
		}
	}
	return owners
}

func firstAliasOtherThan(aliases []string, blocked string) string {
	for _, alias := range aliases {
		if alias != blocked {
			return alias
		}
	}
	return ""
}

func channelAliasOwnersByAlias(aliasMap map[string][]string) map[string]string {
	owners := make(map[string]string)
	for resourceID, aliases := range aliasMap {
		for _, alias := range aliases {
			owners[alias] = resourceID
		}
	}
	return owners
}

func sharedResourceAliases(resources []client.Resource) map[string]struct{} {
	counts := make(map[string]int)
	for i := range resources {
		if isURLResource(&resources[i]) && resources[i].Alias != "" {
			counts[resources[i].Alias]++
		}
	}
	shared := make(map[string]struct{})
	for alias, count := range counts {
		if count > 1 {
			shared[alias] = struct{}{}
		}
	}
	return shared
}

func urlDisplayToken(r *client.Resource, boundAliases []string) string {
	if r.Alias != "" {
		return r.Alias
	}
	if len(boundAliases) > 0 {
		return boundAliases[0]
	}
	return ""
}

func formatResourceListLineWithToken(r *client.Resource, boundAliases []string, token, blockedAlias string) string {
	if isURLResource(r) {
		return formatURLListLineWithToken(r, boundAliases, token, blockedAlias)
	}
	return formatTunnelListLine(r, boundAliases)
}

// formatTunnelListLine renders one tunnel resource as a single text
// line in /qurl list output:
//
//   - Slug only:           • `$<slug>`
//   - With bound aliases:  • `$<slug>` (aliases: `$<alias>`, `$<alias2>`)
//   - With description:    • `$<slug>` → <description>
//
// The primary token is [tunnelDisplayToken] (slug-first; never the opaque
// r_<id>). `boundAliases` are the channel `$alias` names that resolve
// to this tunnel in `/qurl get` — a tunnel can have several. They render
// as "(alias: …)" / "(aliases: …)" so the user sees every name that works,
// EXCLUDING the primary token itself (the install flow binds `$<slug>` as a
// channel alias, so the slug would otherwise appear twice). The token-in-backticks
// shape lets Slack render each as inline code. There is no `(tunnel)` label
// or `[slug:...]` fragment — the whole list is tunnels and the token IS the
// slug. An em-dash joins to the tunnel's Display Name.
//
// A slug-less, resource-alias-less tunnel with a bound channel `$alias` has
// that alias promoted to the primary token (by [tunnelDisplayToken]) so the
// row shows a name the user can `get` against — rather than a bare
// resource_id labeled "(no slug set)" sitting next to an "(also `$alias`)"
// that advertises a usable token. With no bound alias either, the tunnel has
// no token `/qurl get` can accept at all (the opaque resource_id isn't an
// alias-shaped token), so the row renders the bare resource_id WITHOUT a `$`
// sigil and spells out that it's not usable from Slack until an admin sets a
// slug — keeping the "copy a token and get it" promise honest.
//
// An em-dash joins the id to the tunnel's Display Name. The Display Name
// reuses the resource description field (see handleSetDisplayName) and is
// always set — install seeds a default and admins refine it with
// `/qurl-admin set-display-name` — so it normally renders. The empty guard
// is defensive only.
func formatTunnelListLine(r *client.Resource, boundAliases []string) string {
	token := tunnelDisplayToken(r, boundAliases)
	var line string
	if token == "" {
		line = "• " + mrkdwnCodeSpan(r.ResourceID) + " (no ID — ask your Slack admin to set one)"
	} else {
		line = "• " + mrkdwnTokenSpan(token)
	}
	if extras := extraAliasTokens(boundAliases, token); len(extras) > 0 {
		line += " (" + aliasNoun(len(extras)) + ": " + strings.Join(extras, ", ") + ")"
	}
	// Show the tunnel's Display Name next to the id. The description field
	// doubles as the Display Name (see handleSetDisplayName) and is always
	// set, so this normally renders; the empty guard is defensive only (an
	// upstream returning a blank description shouldn't dangle an em-dash).
	if r.Description != "" {
		line += " — " + escapeMrkdwnText(r.Description)
	}
	return line
}

func formatURLListLineWithToken(r *client.Resource, boundAliases []string, token, blockedAlias string) string {
	var line string
	if token == "" {
		if blockedAlias != "" {
			line = "• " + mrkdwnCodeSpan(r.ResourceID) + " (alias " + mrkdwnTokenSpan(blockedAlias) + " is ambiguous here — ask your Slack admin to set a channel alias)"
		} else {
			line = "• " + mrkdwnCodeSpan(r.ResourceID) + " (no alias — ask your Slack admin to set one)"
		}
	} else {
		line = "• " + mrkdwnTokenSpan(token)
		if blockedAlias != "" {
			line += " (resource alias " + mrkdwnTokenSpan(blockedAlias) + " is shadowed here)"
		}
		if extras := extraAliasTokens(boundAliases, token); len(extras) > 0 {
			line += " (" + aliasNoun(len(extras)) + ": " + strings.Join(extras, ", ") + ")"
		}
	}
	// URL resources intentionally show their destination. /qurl list is already
	// channel-scoped to the allow-set, and the target is the user-meaningful label
	// for this resource type; raw URLs still cannot be passed to /qurl get.
	target := r.TargetURL
	if target == "" {
		target = "<empty>"
	}
	line += " → " + escapeMrkdwnURL(target)
	if r.Description != "" {
		line += " — " + escapeMrkdwnText(r.Description)
	}
	return line
}

func formatResourceListSectionWithToken(r *client.Resource, boundAliases []string, token, blockedAlias string) string {
	if isURLResource(r) {
		return formatURLListSectionWithToken(r, boundAliases, token, blockedAlias)
	}
	return formatTunnelListSection(r, boundAliases, token)
}

// formatTunnelListSection renders one tunnel as the mrkdwn body of a `section`
// block for the interactive /qurl list. It lays the row out for buttons rather
// than a plain line: the `$id` bold on its own row, the Display Name beneath
// it, and a faint "aliases:" line when extra channel aliases are bound. `token`
// is the row's precomputed display token (see [tunnelDisplayToken]) — the same
// value the row's button mints against, so the section can't name a different
// one. The plain-text fallback (and notifications) still use
// [formatTunnelListLine]; this richer form is block-only. A slug-less,
// alias-less tunnel has an empty token, so it renders the bare resource_id,
// keeps the Display Name (the only human-readable handle such a row has), and
// spells out that it can't be used until an admin sets an ID — matching the
// fallback's "(no ID …)" honesty, which also retains the Display Name.
func formatTunnelListSection(r *client.Resource, boundAliases []string, token string) string {
	var b strings.Builder
	if token == "" {
		b.WriteString("*" + mrkdwnCodeSpan(r.ResourceID) + "*")
		if r.Description != "" {
			b.WriteString("\n" + escapeMrkdwnText(r.Description))
		}
		b.WriteString("\n_No ID set — ask your Slack admin to set one._")
		return b.String()
	}
	b.WriteString("*" + mrkdwnTokenSpan(token) + "*")
	if r.Description != "" {
		b.WriteString("\n" + escapeMrkdwnText(r.Description))
	}
	if extras := extraAliasTokens(boundAliases, token); len(extras) > 0 {
		b.WriteString("\n_" + aliasNoun(len(extras)) + ":_ " + strings.Join(extras, ", "))
	}
	return b.String()
}

func formatURLListSectionWithToken(r *client.Resource, boundAliases []string, token, blockedAlias string) string {
	var b strings.Builder
	if token == "" {
		b.WriteString("*" + mrkdwnCodeSpan(r.ResourceID) + "*")
		if blockedAlias != "" {
			b.WriteString("\n_Alias " + mrkdwnTokenSpan(blockedAlias) + " is ambiguous here — ask your Slack admin to set a channel alias._")
		} else {
			b.WriteString("\n_No alias set — ask your Slack admin to set one._")
		}
	} else {
		b.WriteString("*" + mrkdwnTokenSpan(token) + "*")
		if blockedAlias != "" {
			b.WriteString("\n_Resource alias " + mrkdwnTokenSpan(blockedAlias) + " is shadowed here._")
		}
	}
	target := r.TargetURL
	if target == "" {
		target = "<empty>"
	}
	b.WriteString("\n" + escapeMrkdwnURL(target))
	if r.Description != "" {
		b.WriteString("\n" + escapeMrkdwnText(r.Description))
	}
	if token != "" {
		if extras := extraAliasTokens(boundAliases, token); len(extras) > 0 {
			b.WriteString("\n_" + aliasNoun(len(extras)) + ":_ " + strings.Join(extras, ", "))
		}
	}
	return b.String()
}

// aliasNoun returns "alias" or "aliases" to agree with n. Shared by the
// plain-text and block list formatters so the singular/plural rule lives in one
// place.
func aliasNoun(n int) string {
	if n == 1 {
		return "alias"
	}
	return "aliases"
}

// extraAliasTokens returns the channel-bound aliases other than the row's
// primary token, each wrapped as a mrkdwn `$alias` code span and preserving
// order. It layers display formatting over [aliasesExcluding] so the "exclude
// the primary token" rule has a single home. Shared by the plain-text
// [formatTunnelListLine] and the block [formatTunnelListSection] so the two
// can't drift on which aliases a row advertises.
func extraAliasTokens(boundAliases []string, token string) []string {
	// Mutates the slice in place — safe only because aliasesExcluding returns a
	// freshly make-allocated slice, never a sub-slice of boundAliases. Preserve
	// that contract if aliasesExcluding ever changes, or the caller's
	// boundAliases (and the edit-button alias snapshot) would be corrupted.
	extras := aliasesExcluding(boundAliases, token)
	for i, a := range extras {
		extras[i] = mrkdwnTokenSpan(a)
	}
	return extras
}

func mrkdwnTokenSpan(token string) string {
	return "`$" + escapeMrkdwnCode(token) + "`"
}

func mrkdwnCodeSpan(text string) string {
	return "`" + escapeMrkdwnCode(text) + "`"
}

// channelAliasesByResourceID builds resource_id → sorted channel-bound
// `$alias` shortcuts from the channel's policy entries, so /qurl list
// can show every alias that resolves to each tunnel. Best-effort:
// AdminStore-nil, empty channel, or a fetch failure yields nil (rows
// render slug-only) — the listing must still render.
//
// Cost: one GetChannelPolicy (GetItem) per /qurl list, purely for the
// cosmetic alias display — a second point read of the same channel_policies
// row the scope gate already read via AllowedResourceIDsForChannel. The two
// have different shapes (this returns alias→resource entries; the gate returns
// the membership set), so they're kept as separate reads rather than folded;
// both are cheap GetItems on a tiny row behind the dominant upstream
// ListResources call. Acceptable for the discovery surface; fold into one read
// if this path ever gets hot.
func (h *Handler) channelAliasesByResourceID(ctx context.Context, log *slog.Logger, teamID, channelID string) map[string][]string {
	if h.cfg.AdminStore == nil || channelID == "" {
		return nil
	}
	entries, err := h.cfg.AdminStore.GetChannelPolicy(ctx, teamID, channelID)
	if err != nil {
		log.Debug("list: channel-policy fetch for alias display failed — rendering slug-only",
			"error", err, "team_id", teamID, "channel_id", channelID)
		return nil
	}
	out := make(map[string][]string)
	for i := range entries {
		if entries[i].Alias != "" && entries[i].ResourceID != "" {
			out[entries[i].ResourceID] = append(out[entries[i].ResourceID], entries[i].Alias)
		}
	}
	for id := range out {
		sort.Strings(out[id])
	}
	return out
}

// mapListResourcesError surfaces a friendly user-facing error for
// /qurl list failures. Auth-class (401/403) maps to authFailureMessage;
// 429 gets specific retry-after guidance; 5xx maps to retry-friendly
// service-unreachable copy; other APIError statuses use generic
// list-failed copy so permanent-class errors don't masquerade as
// transport outages. Raw APIError text MUST NOT reach Slack — it carries
// internal codes that are operator-grade, not user-grade.
func mapListResourcesError(log *slog.Logger, teamID string, err error) string {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		log.Warn(listResourcesFailedLogMessage, withRequestIDAttr(apiErr.RequestID, "error", err, "team_id", teamID, "status", apiErr.StatusCode, "code", apiErr.Code)...)
		if apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden {
			return authFailureMessage
		}
		if apiErr.StatusCode == http.StatusTooManyRequests {
			return rateLimitMessage(time.Duration(apiErr.RetryAfter)*time.Second, apiErr.RequestID)
		}
		if apiErr.StatusCode >= 500 && apiErr.StatusCode < 600 {
			return serviceUnreachableMessageWith(apiErr)
		}
		return listResourcesFailedMessage(apiErr.RequestID)
	}
	log.Warn(listResourcesFailedLogMessage, "error", err, "team_id", teamID)
	return serviceUnreachableMessage
}

func listResourcesFailedMessage(requestID string) string {
	return appendSlackReference(commonListResourcesFailedPrefix, requestID) + "."
}
