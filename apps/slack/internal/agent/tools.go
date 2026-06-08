package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Tool names. Read tools execute inline; propose_* tools only describe a
// mutation and stop the loop for human confirmation.
const (
	toolListResources = "list_resources"
	toolListAliases   = "list_aliases"
	toolResolveToken  = "resolve_token"
	toolGetQuota      = "get_quota"

	// proposeToolPrefix marks the mutation tools — calls with this prefix stop
	// the loop for human confirmation rather than executing.
	proposeToolPrefix = "propose_"

	toolProposeGet              = proposeToolPrefix + "get"
	toolProposeRevoke           = proposeToolPrefix + "revoke"
	toolProposeSetAlias         = proposeToolPrefix + "set_alias"
	toolProposeUnsetAlias       = proposeToolPrefix + "unset_alias"
	toolProposeProtectConnector = proposeToolPrefix + "protect_connector"
	toolProposeProtectURL       = proposeToolPrefix + "protect_url"
)

// Tool-input field names. Shared between the JSON schemas and the input
// decoders so the wire keys are defined exactly once.
const (
	fieldToken  = "token"
	fieldReason = "reason"
	fieldAlias  = "alias"
	fieldTarget = "target"
	fieldURL    = "url"
	fieldEnv    = "env"
	fieldPort   = "port"
)

// stringProp is a small helper for a JSON-Schema string property.
func stringProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// toolSpecs returns the full tool surface offered to the model. Read tools come
// first; the propose_* tools each map to one deterministic slash operation.
func toolSpecs() []ToolSpec {
	return []ToolSpec{
		{
			Name:        toolListResources,
			Description: "List the protected resources (connectors and URLs) reachable from this channel. Use this to discover what exists before answering or proposing an action. Read-only.",
			Schema:      map[string]any{},
		},
		{
			Name:        toolListAliases,
			Description: "List the channel aliases visible here and the resources they point to. Read-only.",
			Schema:      map[string]any{},
		},
		{
			Name:        toolResolveToken,
			Description: "Resolve a single $slug or $alias to its resource identity, scoped to this channel. Read-only: it does NOT mint a link or grant access. Use it to confirm a token the user named actually resolves before proposing an action on it.",
			Schema:      map[string]any{fieldToken: stringProp("The $slug or $alias to resolve (with or without the leading $).")},
			Required:    []string{fieldToken},
		},
		{
			Name:        toolGetQuota,
			Description: "Report this workspace's plan and current usage. Read-only.",
			Schema:      map[string]any{},
		},
		{
			Name:        toolProposeGet,
			Description: "Propose minting a one-time access link for a tunnel $slug or channel $alias (a 'get' / grant access). Does NOT execute — the user must confirm. Use the reason field to capture the natural-language intent for the audit trail.",
			Schema: map[string]any{
				fieldToken:  stringProp("The $slug or $alias to mint a link for."),
				fieldReason: stringProp("Short reason distilled from the request, for the audit trail (e.g. 'incident #412')."),
			},
			Required: []string{fieldToken},
		},
		{
			Name:        toolProposeRevoke,
			Description: "Propose revoking a protected resource and ALL its qURLs, in every channel it's protected in. Destructive and admin-gated. Does NOT execute — the user must confirm.",
			Schema:      map[string]any{fieldToken: stringProp("The $slug or $alias of the resource to revoke.")},
			Required:    []string{fieldToken},
		},
		{
			Name:        toolProposeSetAlias,
			Description: "Propose binding a channel alias to a tunnel slug. Admin-gated. Does NOT execute — the user must confirm.",
			Schema: map[string]any{
				fieldAlias:  stringProp("The alias name to bind (no leading $)."),
				fieldTarget: stringProp("The tunnel $slug the alias should point to."),
			},
			Required: []string{fieldAlias, fieldTarget},
		},
		{
			Name:        toolProposeUnsetAlias,
			Description: "Propose clearing a channel alias. Admin-gated. Does NOT execute — the user must confirm.",
			Schema:      map[string]any{fieldAlias: stringProp("The alias name to clear (no leading $).")},
			Required:    []string{fieldAlias},
		},
		{
			Name:        toolProposeProtectConnector,
			Description: "Propose protecting a connector (an FRP-backed reverse tunnel for something running in your own environment, e.g. a Docker container or Kubernetes service). Admin-gated. Does NOT execute — the user must confirm. Ask the user for any missing environment or port first.",
			Schema: map[string]any{
				fieldAlias: stringProp("Suggested channel alias for the new connector (often derived from the channel name)."),
				fieldEnv:   stringProp("Target environment, e.g. 'docker', 'ecs', or 'kubernetes'."),
				fieldPort:  stringProp("The local port the connector should expose."),
			},
		},
		{
			Name:        toolProposeProtectURL,
			Description: "Propose protecting an existing URL (a reachable HTTP endpoint). Admin-gated. Does NOT execute — the user must confirm.",
			Schema: map[string]any{
				fieldURL:   stringProp("The target URL to protect."),
				fieldAlias: stringProp("Suggested channel alias for the protected URL."),
			},
			Required: []string{fieldURL},
		},
	}
}

// decodeFields unmarshals a tool input object into a flat string map. Every
// tool parameter is declared as a string in its schema, so this is a faithful
// decode; a non-object or non-string value yields an error the caller surfaces
// back to the model.
func decodeFields(raw json.RawMessage) (map[string]string, error) {
	fields := map[string]string{}
	if len(raw) == 0 {
		return fields, nil
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("decode tool input: %w", err)
	}
	return fields, nil
}

// executeRead runs a read tool and returns its model-readable result plus
// whether it was an error. Read tools are scoped to the caller's channel by the
// Backend; errors are surfaced to the model as tool_result content so it can
// adapt rather than aborting the turn.
func (a *Agent) executeRead(ctx context.Context, tc *TurnContext, call ToolCall) (content string, isErr bool) {
	var (
		out string
		err error
	)
	switch call.Name {
	case toolListResources:
		out, err = a.backend.ListResources(ctx, tc)
	case toolListAliases:
		out, err = a.backend.ListAliases(ctx, tc)
	case toolGetQuota:
		out, err = a.backend.Quota(ctx, tc)
	case toolResolveToken:
		fields, derr := decodeFields(call.Input)
		if derr != nil {
			return "Invalid input for resolve_token.", true
		}
		token := normalizeToken(fields[fieldToken])
		if token == "" {
			return "resolve_token requires a non-empty token.", true
		}
		out, err = a.backend.ResolveToken(ctx, tc, token)
	default:
		return fmt.Sprintf("Unknown tool %q.", call.Name), true
	}
	if err != nil {
		// Keep the model's context clean: a stable, non-leaky message.
		return "That lookup didn't succeed. Try a different approach or ask the user for clarification.", true
	}
	return out, false
}

// normalizeToken strips a single leading $ and surrounding whitespace so the
// agent accepts both "$staging" and "staging" from the model.
func normalizeToken(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), "$")
}

// parseProposal converts a propose_* tool call into a [Proposal]. It returns
// (nil, false, nil) when the call is not a proposal tool. A non-nil error means
// the proposal tool was called with invalid input — the caller surfaces it back
// to the model.
func parseProposal(call ToolCall) (*Proposal, bool, error) {
	if !strings.HasPrefix(call.Name, proposeToolPrefix) {
		return nil, false, nil
	}
	fields, err := decodeFields(call.Input)
	if err != nil {
		return nil, true, fmt.Errorf("%s: %w", call.Name, err)
	}

	// Each builder returns just (*Proposal, error); the "is a proposal" bool is
	// true for every recognized propose_* name, so it's supplied once here.
	var p *Proposal
	switch call.Name {
	case toolProposeGet:
		p, err = proposalGet(fields)
	case toolProposeRevoke:
		p, err = proposalRevoke(fields)
	case toolProposeSetAlias:
		p, err = proposalSetAlias(fields)
	case toolProposeUnsetAlias:
		p, err = proposalUnsetAlias(fields)
	case toolProposeProtectConnector:
		p, err = proposalProtectConnector(fields)
	case toolProposeProtectURL:
		p, err = proposalProtectURL(fields)
	default:
		return nil, false, nil // a propose_*-prefixed name we don't recognize
	}
	return p, true, err
}

func proposalGet(f map[string]string) (*Proposal, error) {
	token := normalizeToken(f[fieldToken])
	if token == "" {
		return nil, errEmptyField(toolProposeGet, fieldToken)
	}
	return &Proposal{
		Action:  ActionGet,
		Token:   token,
		Reason:  strings.TrimSpace(f[fieldReason]),
		Summary: fmt.Sprintf("Mint a one-time access link for `$%s`.", token),
	}, nil
}

func proposalRevoke(f map[string]string) (*Proposal, error) {
	token := normalizeToken(f[fieldToken])
	if token == "" {
		return nil, errEmptyField(toolProposeRevoke, fieldToken)
	}
	return &Proposal{
		Action:     ActionRevoke,
		Token:      token,
		Summary:    fmt.Sprintf("Revoke `$%s` and all its qURLs, in every channel it's protected in.", token),
		AdminGated: true,
	}, nil
}

func proposalSetAlias(f map[string]string) (*Proposal, error) {
	alias := normalizeToken(f[fieldAlias])
	target := normalizeToken(f[fieldTarget])
	if alias == "" {
		return nil, errEmptyField(toolProposeSetAlias, fieldAlias)
	}
	if target == "" {
		return nil, errEmptyField(toolProposeSetAlias, fieldTarget)
	}
	return &Proposal{
		Action:     ActionSetAlias,
		Alias:      alias,
		Target:     target,
		Summary:    fmt.Sprintf("Bind alias `$%s` to `$%s`.", alias, target),
		AdminGated: true,
	}, nil
}

func proposalUnsetAlias(f map[string]string) (*Proposal, error) {
	alias := normalizeToken(f[fieldAlias])
	if alias == "" {
		return nil, errEmptyField(toolProposeUnsetAlias, fieldAlias)
	}
	return &Proposal{
		Action:     ActionUnsetAlias,
		Alias:      alias,
		Summary:    fmt.Sprintf("Clear alias `$%s`.", alias),
		AdminGated: true,
	}, nil
}

// proposalProtectConnector intentionally requires no fields: a connector
// proposal can be opened before the environment/port are pinned down, and the
// agent gathers the rest conversationally. The confirm path (the guided install
// wizard) is the enforcement point — it re-validates and collects any missing
// env/port before the bootstrap key is minted, so a sparse proposal here can
// never reach a live mutation.
func proposalProtectConnector(f map[string]string) (*Proposal, error) {
	alias := normalizeToken(f[fieldAlias])
	env := strings.TrimSpace(f[fieldEnv])
	return &Proposal{
		Action:     ActionProtectConnector,
		Alias:      alias,
		Env:        env,
		Port:       strings.TrimSpace(f[fieldPort]),
		Summary:    protectConnectorSummary(alias, env),
		AdminGated: true,
	}, nil
}

func proposalProtectURL(f map[string]string) (*Proposal, error) {
	target := strings.TrimSpace(f[fieldURL])
	if target == "" {
		return nil, errEmptyField(toolProposeProtectURL, fieldURL)
	}
	alias := normalizeToken(f[fieldAlias])
	return &Proposal{
		Action:     ActionProtectURL,
		URL:        target,
		Alias:      alias,
		Summary:    protectURLSummary(target, alias),
		AdminGated: true,
	}, nil
}

// errEmptyField builds the "required field empty" error for a proposal tool.
func errEmptyField(tool, field string) error {
	return fmt.Errorf("%s: %s is required", tool, field)
}

// protectConnectorSummary renders the confirm-card sentence for a connector
// proposal, gracefully handling missing alias/env.
func protectConnectorSummary(alias, env string) string {
	var b strings.Builder
	b.WriteString("Protect a new connector")
	if env != "" {
		b.WriteString(" (")
		b.WriteString(env)
		b.WriteString(")")
	}
	if alias != "" {
		b.WriteString(" as `$")
		b.WriteString(alias)
		b.WriteString("`")
	}
	b.WriteString(".")
	return b.String()
}

// protectURLSummary renders the confirm-card sentence for a URL proposal.
func protectURLSummary(target, alias string) string {
	if alias != "" {
		return fmt.Sprintf("Protect %s as `$%s`.", target, alias)
	}
	return fmt.Sprintf("Protect %s.", target)
}
