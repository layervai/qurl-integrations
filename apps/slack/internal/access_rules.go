package internal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/shared/client"
)

// Access rules for `/qurl get rule:"…"`, `/qurl crid … rule:"…"` and the
// agent's get. Slack mints with the workspace's qURL API key, so "Your rules"
// are the saved rules of the qURL account that connected the workspace.
//
// Slack only mints links on existing resources, so it applies public rules
// (their link limits) and refuses any rule that decides who may open the
// resource, a paid rule, or a rule with settings this app does not model:
// applying part of a rule could share more openly than the rule says.

const accessRuleModePublic = "public"

// defaultAccessRules are the Default access rules, by the IDs and names of
// the shared catalog (TestDefaultAccessRulesMatchCatalog checks them).
// TODO(upstream-contract): keep aligned with qurl-service's catalog.v1.json.
var defaultAccessRules = []qurl.AccessRule{
	{ID: "builtin.public", Name: "Anyone with the link", Mode: accessRuleModePublic},
	{ID: "once", Name: "One-time link", Mode: accessRuleModePublic, LinkLimits: &qurl.AccessRuleLinkLimits{ExpiresIn: "30d", OneTimeUse: new(true), SessionDuration: "24h"}},
	{ID: "builtin.invite_only", Name: "People you invite", Mode: "invite_only"},
	{ID: "builtin.review", Name: "People you approve", Mode: "review"},
}

// accessRuleChoicesHint names what a Slack link can be shared with.
const accessRuleChoicesHint = "Slack can share with *Anyone with the link*, *One-time link*, or one of your saved rules that lets anyone with the link open it."

var errAccessRuleAmbiguous = errors.New("more than one access rule matches")

// findAccessRule returns the Default or saved rule whose ID is selector, or
// whose name is selector ignoring case and surrounding space.
func findAccessRule(selector string, saved []qurl.AccessRule) (qurl.AccessRule, bool, error) {
	name := strings.TrimSpace(selector)
	var match *qurl.AccessRule
	for _, list := range [][]qurl.AccessRule{defaultAccessRules, saved} {
		for i := range list {
			r := &list[i]
			if r.ID == selector {
				return *r, true, nil
			}
			if !strings.EqualFold(strings.TrimSpace(r.Name), name) {
				continue
			}
			if match != nil && match.ID != r.ID {
				return qurl.AccessRule{}, false, errAccessRuleAmbiguous
			}
			match = r
		}
	}
	if match == nil {
		return qurl.AccessRule{}, false, nil
	}
	return *match, true, nil
}

// applyAccessRule sets input's link limits from a public rule, or returns
// the user-facing reason Slack cannot share with it.
func applyAccessRule(input *client.CreateInput, r *qurl.AccessRule) error {
	name := escapeMrkdwnText(r.Name)
	if r.HasUnknownFields() {
		return &userError{msg: fmt.Sprintf("*%s* has settings from a newer qURL app, so Slack can't share with it.", name)}
	}
	// A public rule holds only link limits; any other setting is one Slack
	// would drop.
	if r.Mode != accessRuleModePublic || r.EmailDomain != "" || len(r.AllowedEmails) > 0 || r.ContractID != "" || r.Payment != nil || r.ApprovalExpiresIn != "" {
		return &userError{msg: fmt.Sprintf("*%s* decides who can open the resource itself, which Slack can't set. Share with it from qURL Desktop or the qURL CLI. %s", name, accessRuleChoicesHint)}
	}
	input.MaxSessions = 0
	input.OneTimeUse = false
	input.ExpiresIn = ""
	input.SessionDuration = ""
	if l := r.LinkLimits; l != nil {
		input.OneTimeUse = l.OneTimeUse != nil && *l.OneTimeUse
		input.ExpiresIn = l.ExpiresIn
		input.SessionDuration = l.SessionDuration
	}
	return nil
}

// resolveAccessRule finds selector among the Default access rules and the
// workspace account's saved rules and applies it to input. The saved rules
// are read only to find a match: when they can't be read, the Default access
// rules still work.
func resolveAccessRule(ctx context.Context, log *slog.Logger, c *client.Client, input *client.CreateInput, selector string) error {
	saved, loadErr := c.AccessRules(ctx)
	var apiErr *client.APIError
	if errors.As(loadErr, &apiErr) && apiErr.StatusCode == http.StatusForbidden {
		// The account has no saved rules this key may read.
		log.Info("get: saved access rules unavailable", withRequestIDAttr(apiErr.RequestID, "code", apiErr.Code)...)
		saved, loadErr = nil, nil
	}
	rule, found, err := findAccessRule(selector, saved)
	switch {
	case err != nil:
		return &userError{msg: fmt.Sprintf("More than one access rule is named *%s*. Rename one in qURL Desktop or the qURL CLI.", escapeMrkdwnText(truncateForError(selector)))}
	case !found && loadErr != nil:
		log.Warn("get: saved access rules read failed", "error", loadErr)
		return &userError{msg: "Couldn't read your saved access rules. Please try again. " + accessRuleChoicesHint}
	case !found:
		return &userError{msg: fmt.Sprintf("No access rule is named *%s*. %s", escapeMrkdwnText(truncateForError(selector)), accessRuleChoicesHint)}
	}
	return applyAccessRule(input, &rule)
}

// ruleLinkNotice describes a rule-shared link for the success reply.
func ruleLinkNotice(input *client.CreateInput) string {
	notice := "anyone with the link can open it"
	if input.OneTimeUse {
		notice = "one-time use"
	}
	if input.ExpiresIn != "" {
		notice += " · link expires in " + escapeMrkdwnText(input.ExpiresIn)
	}
	return notice
}
