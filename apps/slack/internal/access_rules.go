package internal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strconv"
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

// privateRuleModes decide who can open the resource; catalog.v1.json "modes"
// minus public (TestDefaultAccessRulesMatchCatalog checks it).
var privateRuleModes = []string{"keys", "invite_only", "email_capture", "email_domain", "review", "contract"}

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
const accessRuleChoicesHint = "Slack can share with *Anyone with the link*, *One-time link*, or a rule saved on this workspace's qURL account that lets anyone with the link open it."

var errAccessRuleAmbiguous = errors.New("more than one access rule matches")

// findAccessRule returns the Default or saved rule whose ID or name is
// selector, ignoring case and the name's surrounding space; nil if none.
// Matches on different rules, an ID and a name included, are ambiguous. IDs
// are compared untrimmed: they are UUIDs or the catalog's fixed IDs.
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

// refuseAccessRule returns the user-facing reason Slack cannot share with r,
// or nil when r is a public rule holding only link limits.
func refuseAccessRule(r *qurl.AccessRule) error {
	name := escapeMrkdwnText(r.Name)
	if r.HasUnknownFields() {
		return &userError{msg: fmt.Sprintf("*%s* has settings from a newer qURL app, so Slack can't share with it.", name)}
	}
	if r.Payment != nil {
		return &userError{msg: fmt.Sprintf("*%s* is a paid rule, which Slack can't set up. Share with it from qURL Desktop or the qURL CLI. %s", name, accessRuleChoicesHint)}
	}
	if r.Mode != accessRuleModePublic && !slices.Contains(privateRuleModes, r.Mode) {
		return &userError{msg: fmt.Sprintf("Slack can't share with *%s*'s kind of rule yet. Share with it from qURL Desktop or the qURL CLI. %s", name, accessRuleChoicesHint)}
	}
	if !onlyLinkLimits(r) {
		return &userError{msg: fmt.Sprintf("*%s* decides who can open the resource itself, which Slack can't set. Share with it from qURL Desktop or the qURL CLI. %s", name, accessRuleChoicesHint)}
	}
	return nil
}

// applyAccessRule replaces input's four default limits with a public rule's
// link limits; refuseAccessRule must have accepted r.
func applyAccessRule(input *client.CreateInput, r *qurl.AccessRule) {
	var limits qurl.AccessRuleLinkLimits
	if r.LinkLimits != nil {
		limits = *r.LinkLimits
	}
	input.MaxSessions = 0
	input.OneTimeUse = limits.OneTimeUse != nil && *limits.OneTimeUse
	input.ExpiresIn = limits.ExpiresIn
	input.SessionDuration = limits.SessionDuration
}

// onlyLinkLimits reports whether r is a public rule whose only settings are
// the three link limits Slack applies. It allows fields rather than listing
// private ones, so a field a newer SDK models is refused, never dropped. It
// relies on the SDK leaving unset fields nil (not empty) and adding no
// unexported state, which TestOnlyLinkLimitsDecodedRule checks.
// TODO(upstream-contract): qurl-service's AccessRule schema has
// additionalProperties: false and no metadata fields.
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
// workspace account's saved rules, and returns it if Slack can apply it. When
// the saved rules can't be read, only a Default rule's exact ID applies.
func resolveAccessRule(ctx context.Context, log *slog.Logger, c *client.Client, selector string) (*qurl.AccessRule, error) {
	selector = strings.TrimSpace(selector)
	saved, err := c.AccessRules(ctx)
	if err != nil {
		return defaultRuleByID(log, selector, err)
	}
	rule, err := findAccessRule(selector, saved)
	switch {
	case err != nil:
		log.Info("get: access rule is ambiguous")
		msg := fmt.Sprintf("*%s* matches more than one access rule. Rename the saved rule in qURL Desktop or the qURL CLI", escapeMrkdwnText(truncateRunes(selector, 40)))
		if d := defaultRuleWithID(selector); d != nil {
			msg += `, or pick the Default access rule by name: rule:` + strconv.Quote(d.Name)
		} else {
			msg += ", or pick a Default access rule by ID (`rule:builtin.public`, `rule:once`)"
		}
		return nil, &userError{msg: msg + "."}
	case rule == nil:
		log.Info("get: access rule not found", "saved_rules", len(saved))
		return nil, &userError{msg: fmt.Sprintf("No access rule is named *%s*. %s", escapeMrkdwnText(truncateRunes(selector, 40)), accessRuleChoicesHint)}
	}
	if err := refuseAccessRule(rule); err != nil {
		return nil, err
	}
	return rule, nil
}

// defaultRuleByID handles a failed saved-rules read: any name could be an
// unread saved rule's, so only a Default rule's exact ID applies. A 403 means
// the account has no saved rules this key may read (no linked library).
// TODO(upstream-contract): qurl-service documents account_frozen and
// device_not_linked for that 403.
func defaultRuleByID(log *slog.Logger, selector string, readErr error) (*qurl.AccessRule, error) {
	byID := "Use a Default access rule by ID: `rule:builtin.public` (Anyone with the link) or `rule:once` (One-time link)."
	msg := "Couldn't read this workspace's saved access rules. Please try again, or " + strings.ToLower(byID[:1]) + byID[1:]
	var apiErr *client.APIError
	if errors.As(readErr, &apiErr) && apiErr.StatusCode == http.StatusForbidden {
		log.Info("get: saved access rules unavailable", withRequestIDAttr(apiErr.RequestID, "code", apiErr.Code)...)
		msg = "Saved rules can't be read for the qURL account connected to this workspace. " + byID
	} else {
		log.Warn("get: saved access rules read failed", "error", readErr)
	}
	r := defaultRuleWithID(selector)
	if r == nil {
		return nil, &userError{msg: msg}
	}
	if err := refuseAccessRule(r); err != nil {
		return nil, err
	}
	return r, nil
}

// defaultRuleWithID returns the Default access rule whose ID is selector, or nil.
func defaultRuleWithID(selector string) *qurl.AccessRule {
	for i := range defaultAccessRules {
		if strings.EqualFold(defaultAccessRules[i].ID, selector) {
			return &defaultAccessRules[i]
		}
	}
	return nil
}

// ruleLinkNotice describes a rule-shared link for the success reply, with
// the expiry the service applied when it reports one.
func ruleLinkNotice(input *client.CreateInput, expiresAt *time.Time) string {
	notice := "anyone with the link can open it"
	if input.OneTimeUse {
		notice = "one-time use"
	}
	switch {
	case expiresAt != nil:
		// Slack renders the date in the reader's time zone.
		notice += fmt.Sprintf(" · link expires <!date^%d^{date_short_pretty} at {time}|%s>", expiresAt.Unix(), expiresAt.UTC().Format("2006-01-02 15:04 UTC"))
	case input.ExpiresIn != "":
		notice += " · link expires in " + escapeMrkdwnText(input.ExpiresIn)
	}
	return notice
}
