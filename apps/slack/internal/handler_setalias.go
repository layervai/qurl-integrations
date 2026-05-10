package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/aws/aws-lambda-go/events"
)

// modalSubmissionType is the `type` field Slack sends on a view
// submission interaction payload. Lifted because the same string
// appears in both the dispatcher and tests.
const modalSubmissionType = "view_submission"

// rebindPrivateMetadata is the typed payload threaded through the
// rebind modal's `private_metadata` field. JSON-encoded so the
// view-submission handler can `json.Unmarshal` into a known struct
// rather than parsing an ad-hoc query-string. Mirrors the shape of
// [SetAliasRebindMetadata] in views.go but adds the two extra fields
// the submission handler needs to perform the rebind without
// re-resolving anything: the user-picked new target and the existing
// resource ID we already located. Slack caps `private_metadata` at
// 3000 chars; this struct fits with comfortable margin.
type rebindPrivateMetadata struct {
	Alias      string `json:"alias"`
	Target     string `json:"target"`
	ResourceID string `json:"rid,omitempty"`
}

// targetIsResourceIDPrefix is the wire prefix on every qurl-service
// resource ID. Discriminates URL targets from resource_id targets.
const targetIsResourceIDPrefix = "r_"

// errMsgAliasUpdateFailed is the catch-all user-facing message for
// alias-update failures without a code-specific friendly mapping.
const errMsgAliasUpdateFailed = "Failed to update alias. Please try again."

// resourceTypeURL is the OpenAPI-spec value for URL-typed resources.
// `tunnel` is the other recognized type today; the slack bot only
// creates URL-typed resources because the parser routes raw
// `r_…` IDs through the PATCH path.
const resourceTypeURL = "url"

// Slack response-envelope JSON keys. Lifted to constants so the
// `ephemeralOK` / `marshalEphemeralOK` helpers and the legacy
// `respondSlack` (handler.go) path can share one source of truth.
const (
	respKeyResponseType  = "response_type"
	respKeyText          = "text"
	respKeyReplaceOrigin = "replace_original"
)

// errAdminCheckFailed returns when the /internal/v1/admin/check
// endpoint reports the user is not a workspace admin. Sentinel kept
// here (rather than admin_client.go) because only the setalias /
// unsetalias handlers gate on it; the admin client is shared and
// callers route the boolean themselves.
var errAdminCheckFailed = errors.New("not a workspace admin")

// resourceClientFactory builds a [*ResourceClient] for a given API
// key. Indirected through the handler config so tests can swap in
// a stub against an httptest server. Mirrors `Config.NewClient`'s
// pattern for the customer-API client.
type resourceClientFactory func(apiKey string) *ResourceClient

// setAliasDeps is the indirection seam for setalias/unsetalias unit
// tests. The production wiring builds these from `Handler.cfg`; tests
// inject stubs against httptest fixtures. `wired` is the explicit
// override sentinel — see [Handler.setAliasDeps].
type setAliasDeps struct {
	wired             bool
	NewResourceClient resourceClientFactory
	NewAdminClient    func() *AdminClient
	OpenView          func(ctx context.Context, triggerID string, viewJSON []byte) error
	PostResponseURL   func(ctx context.Context, responseURL string, payload []byte) error
}

// handleSetAlias routes the `/qurl setalias $<alias> <target>`
// command. Admin-only. Performs:
//  1. Admin gate via /internal/v1/admin/check.
//  2. Alias resolution. If alias is already bound to a different
//     resource → opens a rebind confirmation modal (views.open).
//  3. Otherwise, set/create the alias on the target resource.
//
// The reply is ephemeral via response_url (under PR-3c.0 Fargate +
// async-defer; for now the synchronous response payload mirrors the
// same shape).
func (h *Handler) handleSetAlias(ctx context.Context, cmd *Command, values url.Values) (events.APIGatewayProxyResponse, error) {
	teamID := values.Get(formFieldTeamID)
	userID := values.Get(formFieldUserID)
	triggerID := values.Get(formFieldTriggerID)
	responseURL := values.Get(formFieldResponseURL)

	deps := h.setAliasDeps()

	// Admin gate. Friendly ephemeral error on failure rather than a
	// raw 401 — Slack users should see a sentence, not a stack trace.
	if err := h.requireAdmin(ctx, deps, teamID, userID); err != nil {
		return ephemeralWarn(fmt.Sprintf("Only workspace admins can set aliases. (%s)", classifyAdminError(err)))
	}

	apiKey, err := h.cfg.AuthProvider.APIKey(ctx, teamID)
	if err != nil {
		slog.Error("setalias: API key", "error", err)
		return ephemeralWarn(authFailureMessage)
	}
	rc := deps.NewResourceClient(apiKey)

	// TODO(PR-3c.5): submit Idempotency-Key on CreateResource. The
	// helper exists (IdempotencyKey, parser.go) but ResourceClient
	// doesn't accept the header yet.

	// Resolve current binding.
	existing, lookupErr := rc.GetResourceByAlias(ctx, cmd.Alias)
	switch {
	case lookupErr == nil:
		// Alias exists. If it points at a different target, ask for
		// confirmation via the rebind modal — silently overwriting
		// would be a footgun in multi-admin workspaces.
		if rebindNeedsConfirm(existing, cmd.Target) {
			return h.openSetAliasRebindModal(ctx, deps, triggerID, cmd.Alias, existing, cmd.Target)
		}
		// Same target → no-op, but tell the user so they don't think
		// the command silently ate input.
		return ephemeralOK(fmt.Sprintf("Alias `$%s` already points there. No change.", cmd.Alias))
	case isResourceNotFound(lookupErr):
		// Fresh alias → fall through to set-or-create below.
	default:
		return mapResourceErrorToSlack(cmd.Alias, lookupErr)
	}

	// No existing alias: set on target.
	if err := h.applySetAlias(ctx, rc, cmd.Alias, cmd.Target); err != nil {
		return mapResourceErrorToSlack(cmd.Alias, err)
	}

	// PR-3c.0 swap-out: in Fargate land we'd POST to response_url
	// here. Today (Lambda), the synchronous response carries the
	// same payload. Both shapes use the ephemeral envelope so the
	// user-visible text is identical.
	if responseURL != "" && deps.PostResponseURL != nil {
		if err := deps.PostResponseURL(ctx, responseURL, marshalEphemeralOK(fmt.Sprintf("Alias `$%s` set.", cmd.Alias))); err != nil {
			slog.Warn("setalias: response_url POST failed", "error", err)
		}
	}
	return ephemeralOK(fmt.Sprintf("Alias `$%s` set.", cmd.Alias))
}

// rebindNeedsConfirm decides whether a rebind needs a confirmation
// modal. Only true when the alias was previously bound to a
// distinguishably-different target. We compare conservatively — a
// trailing slash difference is treated as identical so the user
// isn't asked to confirm a no-op.
//
// Normalization deliberately stops at trailing-slash. Case
// differences in scheme/host (`HTTPS://x` vs `https://x`) trip
// through as distinct, which forces a confirmation modal — the
// safer side of the trade-off in a multi-admin workspace. Upstream
// input is normalized by the parser (lowercased scheme), so this
// case is rare in practice.
func rebindNeedsConfirm(existing *Resource, newTarget string) bool {
	if existing == nil {
		return false
	}
	// Resource-ID target: same id → no rebind.
	if strings.HasPrefix(newTarget, targetIsResourceIDPrefix) {
		return existing.ResourceID != newTarget
	}
	// URL target: compare normalized.
	cur := strings.TrimRight(existing.TargetURL, "/")
	nxt := strings.TrimRight(newTarget, "/")
	return cur != nxt && cur != ""
}

// openSetAliasRebindModal opens the rebind confirmation modal. The
// new target and the resolved resource_id are threaded through
// `private_metadata` (JSON-encoded) because Slack doesn't preserve
// arbitrary state across the open-modal/submit-modal hop.
func (h *Handler) openSetAliasRebindModal(
	ctx context.Context,
	deps setAliasDeps,
	triggerID, alias string,
	existing *Resource,
	newTarget string,
) (events.APIGatewayProxyResponse, error) {
	oldTargetDisplay := existing.TargetURL
	if oldTargetDisplay == "" {
		oldTargetDisplay = existing.ResourceID
	}
	view, err := SetAliasRebindModal(alias, oldTargetDisplay, newTarget)
	if err != nil {
		return ephemeralWarn(errMsgAliasUpdateFailed)
	}
	// The views.go template only carries `{"alias":<name>}` in
	// private_metadata; we widen it here with the target + resource_id
	// so the submission handler can rebind without re-resolving.
	view, err = withRebindMetadata(view, rebindPrivateMetadata{
		Alias:      alias,
		Target:     newTarget,
		ResourceID: existing.ResourceID,
	})
	if err != nil {
		return ephemeralWarn(errMsgAliasUpdateFailed)
	}
	if deps.OpenView == nil {
		return ephemeralWarn("Modal cannot be opened: Slack web API not configured.")
	}
	if err := deps.OpenView(ctx, triggerID, view); err != nil {
		slog.Error("views.open failed", "error", err)
		return ephemeralWarn(errMsgAliasUpdateFailed)
	}
	// Ack the slash command with an empty 200 — the modal is the UX.
	return events.APIGatewayProxyResponse{
		StatusCode: http.StatusOK,
		Body:       "",
	}, nil
}

// applySetAlias writes the alias→target binding. Two paths:
//
//   - Target is a `r_…` resource ID → PATCH the resource with the
//     alias set.
//   - Target is a URL → CreateResource (find-or-create per the
//     qurl-service idempotent-create contract) carrying the alias.
func (h *Handler) applySetAlias(ctx context.Context, rc *ResourceClient, alias, target string) error {
	if strings.HasPrefix(target, targetIsResourceIDPrefix) {
		_, err := rc.UpdateResource(ctx, target, UpdateResourceInput{Alias: alias})
		return err
	}
	_, err := rc.CreateResource(ctx, CreateResourceInput{
		Type:      resourceTypeURL,
		TargetURL: target,
		Alias:     alias,
	})
	return err
}

// handleUnsetAlias routes `/qurl unsetalias $<alias>`.
//  1. Admin gate (before resolution — non-admins must not be able to
//     probe alias existence by reading the difference between a 404
//     and an admin-rejection message).
//  2. Resolve alias → resource. 404 → friendly "no such alias".
//  3. PATCH with ClearAlias=true.
func (h *Handler) handleUnsetAlias(ctx context.Context, cmd *Command, values url.Values) (events.APIGatewayProxyResponse, error) {
	teamID := values.Get(formFieldTeamID)
	userID := values.Get(formFieldUserID)
	responseURL := values.Get(formFieldResponseURL)

	deps := h.setAliasDeps()

	if err := h.requireAdmin(ctx, deps, teamID, userID); err != nil {
		return ephemeralWarn(fmt.Sprintf("Only workspace admins can unset aliases. (%s)", classifyAdminError(err)))
	}

	apiKey, err := h.cfg.AuthProvider.APIKey(ctx, teamID)
	if err != nil {
		slog.Error("unsetalias: API key", "error", err)
		return ephemeralWarn(authFailureMessage)
	}
	rc := deps.NewResourceClient(apiKey)

	res, err := rc.GetResourceByAlias(ctx, cmd.Alias)
	if err != nil {
		if isResourceNotFound(err) {
			return ephemeralWarn(fmt.Sprintf("No resource has alias `$%s`.", cmd.Alias))
		}
		return mapResourceErrorToSlack(cmd.Alias, err)
	}

	if _, err := rc.UpdateResource(ctx, res.ResourceID, UpdateResourceInput{ClearAlias: true}); err != nil {
		return mapResourceErrorToSlack(cmd.Alias, err)
	}

	if responseURL != "" && deps.PostResponseURL != nil {
		if err := deps.PostResponseURL(ctx, responseURL, marshalEphemeralOK(fmt.Sprintf("Alias `$%s` cleared.", cmd.Alias))); err != nil {
			slog.Warn("unsetalias: response_url POST failed", "error", err)
		}
	}
	return ephemeralOK(fmt.Sprintf("Alias `$%s` cleared.", cmd.Alias))
}

// handleSetAliasSubmit routes the `view_submission` payload from the
// rebind confirmation modal. Reads alias + target out of
// `private_metadata`, then performs the `UpdateResource` PATCH. Slack
// requires a 200 (with empty body) to dismiss the modal.
//
// Failure surface: the rebind modal has no input blocks (only
// section/context), so a `response_action=errors` keyed on a fake
// block_id silently dismisses the modal — Slack only renders error
// strings tied to a real input block in the current view. Instead,
// we close the modal cleanly (`response_action=clear`) and surface
// the failure via slog (operator-side) plus the same TODO PR-3c.0
// will wire — a `chat.postEphemeral` follow-up to the original
// channel keyed off `private_metadata.channel_id`. Until that lands,
// slog is the operator signal and the user sees the modal close.
func (h *Handler) handleSetAliasSubmit(ctx context.Context, payload *interactionPayload) (events.APIGatewayProxyResponse, error) {
	teamID := payload.Team.ID
	userID := payload.User.ID
	deps := h.setAliasDeps()

	if err := h.requireAdmin(ctx, deps, teamID, userID); err != nil {
		slog.Warn("setalias submit: admin gate rejected", "team_id", teamID, "user_id", userID, "error", err)
		return modalClearResponse()
	}

	meta, err := parseRebindMetadata(payload.View.PrivateMetadata)
	if err != nil {
		slog.Error("setalias submit: malformed private_metadata", "error", err)
		return modalClearResponse()
	}
	alias := meta.Alias
	target := meta.Target
	resourceID := meta.ResourceID
	if alias == "" || target == "" {
		slog.Error("setalias submit: missing alias or target in private_metadata", "alias_empty", alias == "", "target_empty", target == "")
		return modalClearResponse()
	}

	apiKey, err := h.cfg.AuthProvider.APIKey(ctx, teamID)
	if err != nil {
		slog.Error("setalias submit: API key", "error", err, "team_id", teamID)
		return modalClearResponse()
	}
	rc := deps.NewResourceClient(apiKey)

	if err := h.rebindAlias(ctx, rc, alias, target, resourceID); err != nil {
		slog.Error("setalias submit: rebind failed", "alias", alias, "error", err, "friendly_message", friendlyResourceMessage(alias, err))
		return modalClearResponse()
	}

	// Slack expects a 200 with empty body to close the modal cleanly.
	return events.APIGatewayProxyResponse{StatusCode: http.StatusOK, Body: ""}, nil
}

// rebindAlias is the shared resource-API choreography for the rebind
// modal-submission path. Three cases:
//
//   - `resourceID == ""` (alias was unbound at slash-command time):
//     defer to [Handler.applySetAlias] for the clean-set path.
//   - `resourceID != ""` and target is a different resource_id:
//     clear alias from old + attach to new (two PATCHes).
//   - `resourceID != ""` and target is a URL: clear alias from old +
//     create-or-get the URL resource carrying the alias.
//
// `resourceID == target` is a no-op (the user picked the same target
// they were already bound to).
//
// Deliberate TOCTOU: the resource_id baked into private_metadata at
// modal-open time is trusted at submit time. If another admin
// rebinds the same alias between open and submit, the clear-alias
// PATCH lands on a stale resource. Multi-admin races are rare
// enough that re-resolving (and the extra GET-by-alias round-trip)
// isn't worth the complexity; the qurl-service authz layer is the
// last line of defense.
func (h *Handler) rebindAlias(ctx context.Context, rc *ResourceClient, alias, target, resourceID string) error {
	if resourceID == "" {
		return h.applySetAlias(ctx, rc, alias, target)
	}
	targetIsRID := strings.HasPrefix(target, targetIsResourceIDPrefix)
	switch {
	case targetIsRID && target == resourceID:
		// Same resource — no-op.
		return nil
	case targetIsRID:
		if _, err := rc.UpdateResource(ctx, resourceID, UpdateResourceInput{ClearAlias: true}); err != nil {
			return err
		}
		_, err := rc.UpdateResource(ctx, target, UpdateResourceInput{Alias: alias})
		return err
	default:
		// URL target: clear-then-create.
		if _, err := rc.UpdateResource(ctx, resourceID, UpdateResourceInput{ClearAlias: true}); err != nil {
			return err
		}
		_, err := rc.CreateResource(ctx, CreateResourceInput{
			Type:      resourceTypeURL,
			TargetURL: target,
			Alias:     alias,
		})
		return err
	}
}

// requireAdmin runs the /internal/v1/admin/check call and returns
// `errAdminCheckFailed` if the user is not flagged admin. Wraps the
// network error so the handler's error mapping can distinguish
// admin-rejection from transport failures.
func (h *Handler) requireAdmin(ctx context.Context, deps setAliasDeps, teamID, userID string) error {
	if deps.NewAdminClient == nil {
		return errors.New("admin client not configured")
	}
	ac := deps.NewAdminClient()
	isAdmin, _, err := ac.CheckAdmin(ctx, teamID, userID)
	if err != nil {
		return fmt.Errorf("admin check: %w", err)
	}
	if !isAdmin {
		return errAdminCheckFailed
	}
	return nil
}

// classifyAdminError reduces an admin-check failure to a user-facing
// reason. We don't surface internal codes — just a coarse hint so a
// confused user knows whether to retry (transport) or stop trying
// (not admin).
func classifyAdminError(err error) string {
	if errors.Is(err, errAdminCheckFailed) {
		return "you are not flagged as an admin"
	}
	return "admin check failed"
}

// mapResourceErrorToSlack converts a [*ResourceError] into the
// canonical ephemeral Slack response. Only known codes get bespoke
// messages; everything else falls through to a generic "failed"
// line so the bot never echoes raw server output to end users.
func mapResourceErrorToSlack(alias string, err error) (events.APIGatewayProxyResponse, error) {
	return ephemeralWarn(friendlyResourceMessage(alias, err))
}

// friendlyResourceMessage maps a resource-API error to the user-
// visible string. Lifted to a helper so the modal-submit path
// (modalErrorResponse) and the slash-command path (ephemeralWarn)
// share one source of truth for the wording.
func friendlyResourceMessage(alias string, err error) string {
	var rerr *ResourceError
	if errors.As(err, &rerr) {
		switch rerr.Code {
		case errCodeAliasInUse:
			return fmt.Sprintf("Alias `$%s` is already used by another resource.", alias)
		case errCodeAliasReserved:
			return fmt.Sprintf("`$%s` is a reserved word.", alias)
		case errCodeAliasInvalidFmt:
			return "Alias must be 3-64 chars, lowercase, dash-separated."
		case errCodeTunnelDisabled:
			return "Tunnel resources are disabled in this environment. Use a URL target instead."
		}
		switch rerr.StatusCode {
		case http.StatusConflict:
			return fmt.Sprintf("Alias `$%s` is already used by another resource.", alias)
		case http.StatusUnprocessableEntity:
			return "Invalid alias or target."
		case http.StatusForbidden:
			return "Permission denied for this alias."
		}
	}
	return errMsgAliasUpdateFailed
}

// isResourceNotFound returns true for 404s from the resource API.
// Used as a discriminator on `GetResourceByAlias` failure modes —
// 404 is the "alias unbound" path, which is non-fatal to setalias
// and a friendly error to unsetalias.
func isResourceNotFound(err error) bool {
	var rerr *ResourceError
	if errors.As(err, &rerr) {
		return rerr.StatusCode == http.StatusNotFound || rerr.Code == errCodeAliasNotFound
	}
	return false
}

// withRebindMetadata replaces a Slack modal payload's
// `private_metadata` field with the JSON-encoded [rebindPrivateMetadata].
// The view template emits `{"alias":<name>}` only; this overrides
// the field with the wider struct so the submit handler has
// everything it needs without a second alias→resource lookup.
func withRebindMetadata(viewJSON []byte, meta rebindPrivateMetadata) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(viewJSON, &doc); err != nil {
		return nil, fmt.Errorf("unmarshal modal: %w", err)
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("marshal private_metadata: %w", err)
	}
	doc["private_metadata"] = string(encoded)
	return json.Marshal(doc)
}

// parseRebindMetadata is the inverse of [withRebindMetadata]. Slack
// hands us the value back as-is on `view_submission`; we unmarshal
// into the typed struct so the handler reads named fields rather
// than untyped map[string]string accesses.
func parseRebindMetadata(blob string) (*rebindPrivateMetadata, error) {
	if blob == "" {
		return nil, errors.New("private_metadata is empty")
	}
	var meta rebindPrivateMetadata
	if err := json.Unmarshal([]byte(blob), &meta); err != nil {
		return nil, fmt.Errorf("unmarshal private_metadata: %w", err)
	}
	return &meta, nil
}

// modalClearResponse renders the `response_action=clear` shape Slack
// expects when a view submission needs to dismiss the modal stack
// without surfacing a field-level error. Used for the rebind
// modal's failure paths because the modal has no input blocks to
// key an `errors` payload against (the older field-error path
// silently dropped the message). Operator-side visibility comes
// from the slog records the callers emit before invoking this.
func modalClearResponse() (events.APIGatewayProxyResponse, error) {
	body, err := json.Marshal(map[string]any{
		"response_action": "clear",
	})
	if err != nil {
		return events.APIGatewayProxyResponse{StatusCode: http.StatusInternalServerError}, nil //nolint:nilerr // wire-shape failure surfaces via 500 only
	}
	return events.APIGatewayProxyResponse{
		StatusCode: http.StatusOK,
		Headers:    map[string]string{headerContentType: contentTypeJSON},
		Body:       string(body),
	}, nil
}

// ephemeralWarn returns a `:warning:`-prefixed ephemeral response
// envelope for the slash-command HTTP body.
func ephemeralWarn(message string) (events.APIGatewayProxyResponse, error) {
	body, err := ErrorResponse(message, false)
	if err != nil {
		return events.APIGatewayProxyResponse{StatusCode: http.StatusInternalServerError}, nil //nolint:nilerr // wire-shape failure surfaces via 500 only
	}
	return events.APIGatewayProxyResponse{
		StatusCode: http.StatusOK,
		Headers:    map[string]string{headerContentType: contentTypeJSON},
		Body:       string(body),
	}, nil
}

// ephemeralOK returns the plain ephemeral success envelope. Mirrors
// `respondSlack` (legacy handler.go path) but exposes the message
// directly so callers can pass arbitrary text without dragging in a
// shape-specific helper.
func ephemeralOK(message string) (events.APIGatewayProxyResponse, error) {
	return respond(http.StatusOK, map[string]string{
		respKeyResponseType: responseTypeEphemeral,
		respKeyText:         message,
	})
}

// marshalEphemeralOK is the JSON-bytes equivalent of [ephemeralOK]
// for `response_url` posts. The fixed-shape map can't fail
// json.Marshal, so the error return is suppressed. Renamed from
// `must…` to follow Go convention (must-prefix means panic-on-fail).
func marshalEphemeralOK(message string) []byte {
	body, _ := json.Marshal(map[string]any{
		respKeyResponseType:  responseTypeEphemeral,
		respKeyReplaceOrigin: true,
		respKeyText:          message,
	})
	return body
}

// interactionPayload is the subset of Slack's `view_submission`
// payload we read. Fields we don't touch are intentionally elided so
// the JSON unmarshal is forgiving to upstream additions. The rebind
// modal has no input fields, so `view.state` is not modeled —
// PR-3c.3's admin-claim modal will add it back.
type interactionPayload struct {
	Type string `json:"type"`
	Team struct {
		ID string `json:"id"`
	} `json:"team"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	View struct {
		ID              string `json:"id"`
		CallbackID      string `json:"callback_id"`
		PrivateMetadata string `json:"private_metadata"`
	} `json:"view"`
	TriggerID string `json:"trigger_id"`
}

// parseInteractionPayload decodes the `payload=` form field Slack
// sends on every interaction POST. Returns nil + error if the
// payload is missing or malformed; callers surface that as a 400.
func parseInteractionPayload(formBody string) (*interactionPayload, error) {
	v, err := url.ParseQuery(formBody)
	if err != nil {
		return nil, fmt.Errorf("parse form: %w", err)
	}
	raw := v.Get("payload")
	if raw == "" {
		return nil, errors.New("missing payload field")
	}
	var p interactionPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("unmarshal payload: %w", err)
	}
	return &p, nil
}

// setAliasDeps wires the production deps from h.cfg. Tests inject
// their own struct via [Handler.SetDeps] (which sets the explicit
// `wired` sentinel); production callers go through this method so
// the indirection seam is opaque to handler.go.
//
// We can't compare-against-zero here (the struct contains func
// fields, which are not comparable); the explicit `wired` bool
// avoids the "partial override silently falls back to production
// wiring" footgun the previous NewResourceClient-as-sentinel path
// could hit.
func (h *Handler) setAliasDeps() setAliasDeps {
	if h.deps.wired {
		return h.deps
	}
	return setAliasDeps{
		NewResourceClient: func(apiKey string) *ResourceClient {
			return NewResourceClient(h.cfg.QURLEndpoint, apiKey)
		},
		NewAdminClient: func() *AdminClient {
			return NewAdminClient(h.cfg.QURLEndpoint, h.cfg.InternalServiceToken)
		},
		// OpenView and PostResponseURL stay nil in production until
		// PR-3c.0 lands (Lambda has no goroutine-safe place to do
		// the views.open call inside the 3s budget). When the
		// handler runs on Fargate, cmd/main.go wires these.
	}
}
