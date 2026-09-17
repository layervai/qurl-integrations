// Package slacksmoke holds the Slack Web API hardening that the operator-triggered
// smoke commands under apps/slack/cmd share.
//
// Everything here sits on the path a bot token travels, which is why it is one package
// rather than a copy per command. NormalizeBaseURL is what keeps the token from being
// sent to a plaintext or attacker-named host, CleanToken and ContainsHTTPHeaderControl
// are the header-injection guard, and NewHTTPClient's redirect policy is what stops the
// token being replayed down a redirect chain. The https-unless-loopback rule is the one
// apps/slack/docs/operating.md promises operators by name; the redirect policy is a
// property of this code that the runbook does not restate.
//
// The loopback predicate itself lives in the nethost leaf package, because the request
// handlers and the startup config check share it and must not depend on a package named
// for the smoke commands.
//
// Each of these existed once per command before this package. Nothing in CI could see
// the two copies drift: dupl runs per package, and package main binaries in separate
// directories cannot import each other, so a fix to one copy silently reached one
// binary. The commands are operator-triggered and there is no CI run to catch the
// difference at runtime either.
//
// The bounded response-body read these commands share with the production Lambda lives
// in the httpbody leaf, for the same reason nethost does.
package slacksmoke

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/nethost"
)

// DefaultAPIBaseURL is Slack's Web API root, used when a command's -base-url is empty.
const DefaultAPIBaseURL = "https://slack.com/api"

var (
	// ErrMissingBotToken reports an absent or blank token. The commands render it
	// naming the environment variable they read, so the message itself stays generic.
	ErrMissingBotToken = errors.New("missing Slack bot token")
	// ErrBotTokenControlCharacters reports a token that cannot be written into an
	// Authorization header without risking request splitting.
	ErrBotTokenControlCharacters = errors.New("invalid Slack bot token: contains control characters")
	// ErrBaseURLInvalid reports a -base-url that is not a usable absolute URL. Wrapped
	// around url.Parse's own error when there is one.
	ErrBaseURLInvalid = errors.New("invalid -base-url")
	// ErrBaseURLRequiresHTTPS reports a -base-url that would put a bearer token on the
	// wire in plaintext.
	ErrBaseURLRequiresHTTPS = errors.New("-base-url must use https unless host is localhost or loopback")
	// ErrBaseURLQueryFragment reports a -base-url carrying a query or fragment, which
	// would survive into every method URL built from it.
	ErrBaseURLQueryFragment = errors.New("-base-url must not include query or fragment")
	// ErrBaseURLUserinfo reports a -base-url with embedded credentials.
	ErrBaseURLUserinfo = errors.New("-base-url must not include userinfo")
	// ErrUserAgentControlCharacters reports a -user-agent that cannot be written into a
	// request header safely.
	ErrUserAgentControlCharacters = errors.New("-user-agent contains control characters")
	// ErrTokenEnvName reports a -token-env that is not a POSIX environment variable
	// name. See IsEnvVarName for why the flag is validated rather than trusted.
	ErrTokenEnvName = errors.New("-token-env must be a POSIX environment variable name")

	// ErrOverallTimeoutNotPositive reports a non-positive -timeout. This and the two
	// below carry their operator-facing text verbatim, because commands print them
	// as-is; TimeoutBudget.Validate's factor failure has no sentinel because its
	// message is parameterized by the caller's minimum.
	ErrOverallTimeoutNotPositive = errors.New("-timeout must be greater than 0")
	// ErrRequestTimeoutNotPositive reports a non-positive -request-timeout.
	ErrRequestTimeoutNotPositive = errors.New("-request-timeout must be greater than 0")
	// ErrRequestTimeoutNotLess reports a -request-timeout that does not leave the
	// overall budget room for more than one call.
	ErrRequestTimeoutNotLess = errors.New("-request-timeout must be less than -timeout")
)

// NormalizeBaseURL trims and validates a Slack Web API base URL, returning it without
// a trailing slash. An empty raw yields DefaultAPIBaseURL.
//
// The scheme check is the security-relevant one: a bearer token rides on every request
// built from this URL, so http is refused unless the host is loopback, where there is
// no network to observe it on. Userinfo, query and fragment are refused because each
// would otherwise be carried silently into every method URL derived from the base.
func NormalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = DefaultAPIBaseURL
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrBaseURLInvalid, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", ErrBaseURLInvalid
	}
	if parsed.User != nil {
		return "", ErrBaseURLUserinfo
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", ErrBaseURLQueryFragment
	}
	if parsed.Scheme == "https" || (parsed.Scheme == "http" && nethost.IsLoopback(parsed.Hostname())) {
		parsed.Path = strings.TrimRight(parsed.Path, "/")
		return parsed.String(), nil
	}
	return "", ErrBaseURLRequiresHTTPS
}

// CleanToken trims a Slack bot token and rejects one that cannot be written into an
// Authorization header. It returns ErrMissingBotToken for an empty value so the caller
// can name the environment variable it read.
func CleanToken(token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", ErrMissingBotToken
	}
	if ContainsHTTPHeaderControl(token) {
		return "", ErrBotTokenControlCharacters
	}
	return token, nil
}

// ContainsHTTPHeaderControl reports whether s carries a byte that must never reach a
// request header. Checked on the token and User-Agent because both are written into
// headers verbatim.
func ContainsHTTPHeaderControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return r < ' ' || r == 0x7f
	})
}

// NewHTTPClient returns the client every smoke command issues Slack calls through.
//
// The redirect policy is the point: returning http.ErrUseLastResponse surfaces a 3xx
// to the caller instead of following it, so the Authorization header is never replayed
// against whatever host the response points at. Commands report the Location instead.
func NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// IsEnvVarName reports whether name is a POSIX environment variable name: a leading
// letter or underscore, then letters, digits or underscores. The empty string is not
// one.
//
// Commands validate -token-env with this rather than trusting it, because the flag
// value is echoed back in the "not set" error an operator hits first, and an
// unconstrained value there puts whatever it contains — newlines included — into the
// command's own diagnostics.
func IsEnvVarName(name string) bool {
	for i, r := range name {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return name != ""
}

// TimeoutBudget pairs the two durations a smoke command runs under, kept together so
// the ordering of the checks in Validate reads as one rule rather than as separate
// flag validations.
type TimeoutBudget struct {
	// Overall bounds the whole run; Request bounds each individual Slack call.
	Overall time.Duration
	Request time.Duration
}

// Validate returns the first failing check, or nil when the budget is usable. Callers
// print it and exit; the package itself does no printing and knows no exit codes,
// which is the same division the token and base-URL sentinels above keep.
//
// minFactor is how many whole request timeouts the overall budget must cover — the
// smallest useful run for that command, which varies with the calls it makes.
func (b TimeoutBudget) Validate(minFactor int) error {
	switch {
	case b.Overall <= 0:
		return ErrOverallTimeoutNotPositive
	case b.Request <= 0:
		return ErrRequestTimeoutNotPositive
	// Explicit before the factor guard so equal or exceeding values get the direct
	// operator-facing error rather than the multiplier one.
	case b.Request >= b.Overall:
		return ErrRequestTimeoutNotLess
	case b.Overall < time.Duration(minFactor)*b.Request:
		return fmt.Errorf("-timeout must be at least %dx -request-timeout", minFactor)
	}
	return nil
}
