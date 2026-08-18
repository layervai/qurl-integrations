// Package slacksmoke holds the Slack Web API hardening that the operator-triggered
// smoke commands under apps/slack/cmd share.
//
// Everything here sits on the path a bot token travels, which is why it is one package
// rather than a copy per command. NormalizeBaseURL is what keeps the token from being
// sent to a plaintext or attacker-named host, IsLoopbackHost is that rule's only http
// escape hatch, CleanToken and ContainsHTTPHeaderControl are the header-injection
// guard, and NewHTTPClient's redirect policy is what stops the token being replayed
// down a redirect chain. apps/slack/docs/operating.md makes operators that promise for
// every command built on this package.
//
// Each of these existed once per command before this package. Nothing in CI could see
// the two copies drift: dupl runs per package, and package main binaries in separate
// directories cannot import each other, so a fix to one copy silently reached one
// binary. The commands are operator-triggered and there is no CI run to catch the
// difference at runtime either.
//
// Callers keep their own response-size ceiling and pass it to DrainResponseBody; see
// that function for why the limit is a parameter rather than a constant here.
package slacksmoke

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
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
		return "", fmt.Errorf("invalid -base-url: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("invalid -base-url")
	}
	if parsed.User != nil {
		return "", ErrBaseURLUserinfo
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", ErrBaseURLQueryFragment
	}
	if parsed.Scheme == "https" || (parsed.Scheme == "http" && IsLoopbackHost(parsed.Hostname())) {
		parsed.Path = strings.TrimRight(parsed.Path, "/")
		return parsed.String(), nil
	}
	return "", ErrBaseURLRequiresHTTPS
}

// IsLoopbackHost reports whether host names the local machine, and so is the one place
// a plaintext http base URL is allowed.
//
// It trims and lowercases before deciding, so a caller that has not already normalized
// its input still gets the same answer.
func IsLoopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

// DrainResponseBody reads up to limit+1 bytes from body and discards them, a
// best-effort attempt at connection reuse for a moderately oversized response. Close
// tears down the response if bytes still remain.
//
// limit is a parameter rather than a package constant because the callers' ceilings
// differ by two orders of magnitude — slack-dm-smoke reads small chat.postMessage
// envelopes, while slack-history-upload-smoke reads whole conversations.history pages.
// When this function was duplicated per command, the same body silently drained a 64x
// different budget depending on which file it sat in.
func DrainResponseBody(body io.Reader, limit int) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, int64(limit)+1))
}

// IsEnvVarName reports whether name is a POSIX environment variable name: a leading
// letter or underscore, then letters, digits or underscores.
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
// the ordering of the checks in ValidateTimeoutBudget reads as one rule rather than as
// separate flag validations.
type TimeoutBudget struct {
	// Overall bounds the whole run; Request bounds each individual Slack call.
	Overall time.Duration
	Request time.Duration
}

// ValidateTimeoutBudget writes the first failing check to stderr and returns the
// process exit code: 2 for a bad invocation, 0 when the budget is usable.
//
// minFactor is how many whole request timeouts the overall budget must cover — the
// smallest useful run for that command, which varies with the calls it makes.
func ValidateTimeoutBudget(stderr io.Writer, budget TimeoutBudget, minFactor int) int {
	switch {
	case budget.Overall <= 0:
		_, _ = fmt.Fprintln(stderr, "-timeout must be greater than 0")
	case budget.Request <= 0:
		_, _ = fmt.Fprintln(stderr, "-request-timeout must be greater than 0")
	// Explicit before the factor guard so equal or exceeding values get the direct
	// operator-facing error rather than the multiplier one.
	case budget.Request >= budget.Overall:
		_, _ = fmt.Fprintln(stderr, "-request-timeout must be less than -timeout")
	case budget.Overall < time.Duration(minFactor)*budget.Request:
		_, _ = fmt.Fprintf(stderr, "-timeout must be at least %dx -request-timeout\n", minFactor)
	default:
		return 0
	}
	return 2
}
