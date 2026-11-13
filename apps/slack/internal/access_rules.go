package internal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"time"

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

// defaultAccessRules are the Default access rules, then the other ready-made
// choices, by the IDs and names of
// the shared catalog (TestDefaultAccessRulesMatchCatalog checks them).
// TODO(upstream-contract): keep aligned with qurl-service's catalog.v1.json.
var defaultAccessRules = []qurl.AccessRule{
	{ID: "builtin.public", Name: "Anyone with the link", Mode: accessRuleModePublic},
	{ID: "once", Name: "One-time link", Mode: accessRuleModePublic, LinkLimits: &qurl.AccessRuleLinkLimits{ExpiresIn: "30d", OneTimeUse: new(true), SessionDuration: "24h"}},
	{ID: "builtin.invite_only", Name: "People you invite", Mode: "invite_only"},
	{ID: "builtin.review", Name: "People you approve", Mode: "review"},
	// The catalog's other ready-made choices, known only so they are refused
	// by name rather than reported as missing.
	{ID: "builtin.email_capture", Name: "Anyone with a verified email", Mode: "email_capture"},
	{ID: "builtin.keys", Name: "Devices you approve", Mode: "keys"},
	// A non-nil Payment marks it paid; it is never applied, so it holds no price.
	{ID: "paid", Name: "People who pay", Mode: "contract", Payment: &qurl.AccessRulePayment{}},
}

// accessRuleChoicesHint names what a Slack link can be shared with.
const accessRuleChoicesHint = "Slack can share with *Anyone with the link*, *One-time link*, or one of your saved rules that lets anyone with the link open it."

var errAccessRuleAmbiguous = errors.New("more than one access rule matches")

// findAccessRule returns the Default or saved rule whose ID or name is
// selector, ignoring case and the name's surrounding space; nil if none.
// Matches on different rules, an ID and a name included, are ambiguous.
func findAccessRule(selector string, saved []qurl.AccessRule) (*qurl.AccessRule, error) {
	var match *qurl.AccessRule
	for _, list := range [][]qurl.AccessRule{defaultAccessRules, saved} {
		for i := range list {
			r := &list[i]
			if !strings.EqualFold(r.ID, selector) && !strings.EqualFold(strings.TrimSpace(r.Name), selector) {
				continue
			}
			if match != nil {
				return nil, errAccessRuleAmbiguous
			}
			match = r
		}
	}
	return match, nil
}

// applyAccessRule sets input's link limits from a public rule, or returns
// the user-facing reason Slack cannot share with it.
func applyAccessRule(input *client.CreateInput, r *qurl.AccessRule) error {
	name := escapeMrkdwnText(r.Name)
	if r.HasUnknownFields() {
		return &userError{msg: fmt.Sprintf("*%s* has settings from a newer qURL app, so Slack can't share with it.", name)}
	}
	if !onlyLinkLimits(r) {
		return &userError{msg: fmt.Sprintf("*%s* decides who can open the resource itself, which Slack can't set. Share with it from qURL Desktop or the qURL CLI. %s", name, accessRuleChoicesHint)}
	}
	var limits qurl.AccessRuleLinkLimits
	if r.LinkLimits != nil {
		limits = *r.LinkLimits
	}
	input.MaxSessions = 0
	input.OneTimeUse = limits.OneTimeUse != nil && *limits.OneTimeUse
	input.ExpiresIn = limits.ExpiresIn
	input.SessionDuration = limits.SessionDuration
	return nil
}

// onlyLinkLimits reports whether r is a public rule whose only settings are
// the three link limits Slack applies. It allows fields rather than listing
// private ones, so a field a newer SDK models is refused, never dropped. It
// relies on the SDK leaving unset fields nil (not empty), which
// TestHandleGet_RuleSaved checks end to end.
func onlyLinkLimits(r *qurl.AccessRule) bool {
	rest := *r
	rest.ID, rest.Name, rest.Mode, rest.LinkLimits = "", "", "", nil
	if r.Mode != accessRuleModePublic || !reflect.DeepEqual(rest, qurl.AccessRule{}) {
		return false
	}
	if r.LinkLimits == nil {
		return true
	}
	limits := *r.LinkLimits
	limits.ExpiresIn, limits.OneTimeUse, limits.SessionDuration = "", nil, ""
	return reflect.DeepEqual(limits, qurl.AccessRuleLinkLimits{})
}

// resolveAccessRule finds selector among the Default access rules and the
// workspace account's saved rules and applies it to input. When the account
// has no saved rules this key may read (403), the Default access rules still
// work; when the read fails, only a Default rule's ID does.
func resolveAccessRule(ctx context.Context, log *slog.Logger, c *client.Client, input *client.CreateInput, selector string) (*qurl.AccessRule, error) {
	selector = strings.TrimSpace(selector)
	saved, loadErr := c.AccessRules(ctx)
	var apiErr *client.APIError
	if errors.As(loadErr, &apiErr) && apiErr.StatusCode == http.StatusForbidden {
		// The account has no saved rules this key may read. TODO(upstream-contract):
		// qurl-service documents account_frozen and device_not_linked here; a
		// frozen account's mint fails on its own, so every 403 reads as none.
		log.Info("get: saved access rules unavailable", withRequestIDAttr(apiErr.RequestID, "code", apiErr.Code)...)
		saved, loadErr = nil, nil
	}
	rule, err := findAccessRule(selector, saved)
	switch {
	case err != nil:
		return nil, &userError{msg: fmt.Sprintf("*%s* matches more than one access rule. Rename your saved rule in qURL Desktop or the qURL CLI.", escapeMrkdwnText(truncateForError(selector)))}
	case loadErr != nil && (rule == nil || !strings.EqualFold(rule.ID, selector)):
		// With no saved rules read, only a Default rule's exact ID is unambiguous.
		log.Warn("get: saved access rules read failed", "error", loadErr)
		return nil, &userError{msg: "Couldn't read your saved access rules. Please try again. " + accessRuleChoicesHint}
	case rule == nil:
		return nil, &userError{msg: fmt.Sprintf("No access rule is named *%s*. %s", escapeMrkdwnText(truncateForError(selector)), accessRuleChoicesHint)}
	}
	return rule, applyAccessRule(input, rule)
}

// ruleLinkNotice describes a rule-shared link for the success reply; the
// service's expiry stands in when the rule sets none.
func ruleLinkNotice(input *client.CreateInput, expiresAt *time.Time) string {
	notice := "anyone with the link can open it"
	if input.OneTimeUse {
		notice = "one-time use"
	}
	switch {
	case input.ExpiresIn != "":
		notice += " · link expires in " + escapeMrkdwnText(input.ExpiresIn)
	case expiresAt != nil:
		// Slack renders the date in the reader's time zone.
		notice += fmt.Sprintf(" · link expires <!date^%d^{date_short_pretty} at {time}|%s>", expiresAt.Unix(), expiresAt.UTC().Format("2006-01-02 15:04 UTC"))
	}
	return notice
}
