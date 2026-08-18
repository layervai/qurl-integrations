// Command slack-history-upload-smoke checks, against a live Slack workspace, that a
// message read back through the Web API still describes its upload the way
// internal.SlackMessageHasUpload assumes it does.
//
// It exists because that classifier's TODO(upstream-contract) names a rot path
// nothing else observes. The thread-history seam annotates a rebuilt caption with
// "an attachment was here" from the message's `files` array alone, and if Slack stops
// populating that array every unit test stays green: they supply the field directly
// and never read Slack. The measurement the comment cites was one workspace on one
// day, run by hand. This is that measurement as a command, so the next person re-runs
// it instead of re-deriving it — and it reads conversations.replies as well as
// conversations.history, which is the "not separately measured" caveat the comment
// leaves open.
//
// Operator-triggered, like slack-dm-smoke beside it, and for the same reason: it needs
// a real bot token and a workspace whose conversations actually contain uploads, so
// there is nothing for CI to run it against. Exit 0 means the contract still holds, 1
// means it does not, 2 means the invocation was wrong.
//
// Read-only and content-free. Every call is a GET against conversations.list,
// conversations.history and conversations.replies; nothing is posted, and the output
// carries counts, conversation IDs and message timestamps only. File names, message
// text, user names and mimetypes are user content and never leave the process — the
// same field discipline claimMediaNotice keeps on the event path.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultSlackAPIBaseURL = "https://slack.com/api"
	defaultUserAgent       = "qurl-slack-history-upload-smoke"
	defaultOverallTimeout  = 5 * time.Minute
	defaultRequestTimeout  = 15 * time.Second

	// minTimeoutFactor keeps the overall budget wide enough for the smallest useful
	// scan: a conversations.list page, one conversations.history page, and one
	// conversations.replies page.
	minTimeoutFactor = 3
)

var (
	errMissingSlackBotToken           = errors.New("missing Slack bot token")
	errSlackBotTokenControlCharacters = errors.New("invalid Slack bot token: contains control characters")
	errBaseURLRequiresHTTPS           = errors.New("-base-url must use https unless host is localhost or loopback")
	errBaseURLQueryFragment           = errors.New("-base-url must not include query or fragment")
	errBaseURLUserinfo                = errors.New("-base-url must not include userinfo")
	errUserAgentControlCharacters     = errors.New("-user-agent contains control characters")
	errTokenEnvName                   = errors.New("-token-env must be a POSIX environment variable name")
)

func main() {
	os.Exit(run(context.Background(), os.Stdout, os.Stderr, os.Args[1:], os.Getenv, time.Now))
}

func run(ctx context.Context, stdout, stderr io.Writer, args []string, getenv func(string) string, now func() time.Time) int {
	// A nil config is the stop signal, not the exit code: -h parses fine and exits 0
	// while having nothing to run, so keying off the code alone runs the scan on a nil
	// config.
	cfg, tokenEnv, budget, code := parseFlags(stderr, args)
	if cfg == nil {
		return code
	}
	cfg.Token = getenv(tokenEnv)
	cfg.StartedAt = now().UTC()
	cfg.HTTPClient = newSlackHTTPClient(budget.request)

	if err := prepareScanConfig(cfg); err != nil {
		writeConfigValidationError(stderr, tokenEnv, err)
		return 2
	}

	runCtx, cancel := context.WithTimeout(ctx, budget.overall)
	defer cancel()

	result, err := runScan(runCtx, cfg)
	if encErr := json.NewEncoder(stdout).Encode(result); encErr != nil {
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
		}
		_, _ = fmt.Fprintf(stderr, "write result: %v\n", encErr)
		return 1
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// timeoutBudget is the pair of durations run applies, kept together so the ordering
// of the two validations below reads as one rule rather than two flag checks.
type timeoutBudget struct {
	overall time.Duration
	request time.Duration
}

// parseFlags returns the config, the env var naming the token, the timeout budget, and
// the exit code. A nil config means run should stop and return that code — which is 2
// for a bad invocation and 0 for -h, where the flag package has already printed usage.
// It deliberately does not read the environment or the clock; run owns both so tests
// can supply them.
func parseFlags(stderr io.Writer, args []string) (cfg *scanConfig, tokenEnv string, budget timeoutBudget, code int) {
	fs := flag.NewFlagSet("slack-history-upload-smoke", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var channels string
	cfg = &scanConfig{}
	fs.StringVar(&tokenEnv, "token-env", "SLACK_BOT_TOKEN", "environment variable containing the Slack bot token")
	fs.StringVar(&cfg.BaseURL, "base-url", defaultSlackAPIBaseURL, "Slack Web API base URL")
	fs.StringVar(&cfg.UserAgent, "user-agent", defaultUserAgent, "HTTP User-Agent")
	fs.StringVar(&channels, "channels", "", "comma-separated conversation IDs to scan; empty discovers them with conversations.list")
	fs.StringVar(&cfg.ConversationTypes, "conversation-types", defaultConversationTypes, "conversations.list types filter, used only when -channels is empty")
	fs.IntVar(&cfg.MaxConversations, "max-conversations", defaultMaxConversations, "stop after scanning this many conversations")
	fs.IntVar(&cfg.MaxPages, "max-pages", defaultMaxPages, "conversations.history pages to read per conversation; also caps conversations.list paging")
	fs.IntVar(&cfg.PageLimit, "page-limit", defaultPageLimit, "messages requested per page")
	fs.IntVar(&cfg.MaxThreads, "max-threads", defaultMaxThreads, "threads sampled per conversation for the conversations.replies surface")
	fs.BoolVar(&cfg.SkipReplies, "skip-replies", false, "scan conversations.history only, leaving the replies surface unmeasured")
	fs.IntVar(&cfg.MinUploads, "min-uploads", 1, "fail unless the scan classifies at least this many uploads")
	fs.Var(&cfg.ExpectUploads, "expect-upload", "CHANNEL:TIMESTAMP of a message known to carry an upload; repeatable")
	fs.BoolVar(&cfg.StrictUncountable, "strict-uncountable", false, "make an unrecognized files shape fail the command rather than only report")
	fs.StringVar(&cfg.WorkspaceShape, "workspace-shape", "", "operator note, for example Enterprise Grid org install")
	fs.StringVar(&cfg.TokenOwner, "token-owner", "", "operator note for the token owner, for example workspace or enterprise")
	fs.StringVar(&cfg.Scopes, "scopes", "", "operator note for Slack scopes on the tested app")
	fs.DurationVar(&budget.overall, "timeout", defaultOverallTimeout, "overall scan timeout")
	fs.DurationVar(&budget.request, "request-timeout", defaultRequestTimeout, "timeout for each Slack Web API request")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, "", budget, 0
		}
		return nil, "", budget, 2
	}

	tokenEnv = strings.TrimSpace(tokenEnv)
	if tokenEnv == "" {
		_, _ = fmt.Fprintln(stderr, "-token-env is required")
		return nil, "", budget, 2
	}
	// Validated rather than trusted because it is echoed back in the "not set" error an
	// operator hits first, and an unconstrained flag value there puts whatever it
	// contains — newlines included — into the command's own diagnostics.
	if !isEnvVarName(tokenEnv) {
		_, _ = fmt.Fprintln(stderr, errTokenEnvName.Error())
		return nil, "", budget, 2
	}
	cfg.Channels = splitConversationIDs(channels)
	if failure := validateBudget(stderr, budget); failure != 0 {
		return nil, "", budget, failure
	}
	if failure := validateBounds(stderr, cfg); failure != 0 {
		return nil, "", budget, failure
	}
	return cfg, tokenEnv, budget, 0
}

func validateBudget(stderr io.Writer, budget timeoutBudget) int {
	switch {
	case budget.overall <= 0:
		_, _ = fmt.Fprintln(stderr, "-timeout must be greater than 0")
	case budget.request <= 0:
		_, _ = fmt.Fprintln(stderr, "-request-timeout must be greater than 0")
	// Explicit before the factor guard so equal or exceeding values get the direct
	// operator-facing error rather than the multiplier one.
	case budget.request >= budget.overall:
		_, _ = fmt.Fprintln(stderr, "-request-timeout must be less than -timeout")
	case budget.overall < minTimeoutFactor*budget.request:
		_, _ = fmt.Fprintf(stderr, "-timeout must be at least %dx -request-timeout\n", minTimeoutFactor)
	default:
		return 0
	}
	return 2
}

func validateBounds(stderr io.Writer, cfg *scanConfig) int {
	switch {
	case cfg.MaxConversations <= 0:
		_, _ = fmt.Fprintln(stderr, "-max-conversations must be greater than 0")
	case cfg.MaxPages <= 0:
		_, _ = fmt.Fprintln(stderr, "-max-pages must be greater than 0")
	case cfg.PageLimit <= 0 || cfg.PageLimit > maxPageLimit:
		_, _ = fmt.Fprintf(stderr, "-page-limit must be between 1 and %d\n", maxPageLimit)
	case cfg.MaxThreads < 0:
		_, _ = fmt.Fprintln(stderr, "-max-threads must not be negative")
	case cfg.MinUploads < 0:
		_, _ = fmt.Fprintln(stderr, "-min-uploads must not be negative")
	case cfg.MaxThreads > maxThreadsCeiling:
		_, _ = fmt.Fprintf(stderr, "-max-threads must be at most %d\n", maxThreadsCeiling)
	// Refused rather than truncated. -channels is what an operator names when they know
	// which conversations carry uploads, so silently dropping the tail would answer a
	// different question than the one asked.
	case len(cfg.Channels) > cfg.MaxConversations:
		_, _ = fmt.Fprintf(stderr, "-channels names %d conversations but -max-conversations is %d; raise it or name fewer\n",
			len(cfg.Channels), cfg.MaxConversations)
	default:
		return 0
	}
	return 2
}

// prepareScanConfig normalizes and validates everything that does not depend on the
// network, so a bad invocation costs no Slack calls.
func prepareScanConfig(cfg *scanConfig) error {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = newSlackHTTPClient(defaultRequestTimeout)
	}
	if cfg.StartedAt.IsZero() {
		cfg.StartedAt = time.Now().UTC()
	}
	if strings.TrimSpace(cfg.UserAgent) == "" {
		cfg.UserAgent = defaultUserAgent
	}
	if strings.TrimSpace(cfg.ConversationTypes) == "" {
		cfg.ConversationTypes = defaultConversationTypes
	}

	var err error
	if cfg.Token, err = cleanSlackToken(cfg.Token); err != nil {
		return err
	}
	if containsHTTPHeaderControl(cfg.UserAgent) {
		return errUserAgentControlCharacters
	}
	if cfg.BaseURL, err = normalizeSlackBaseURL(cfg.BaseURL); err != nil {
		return err
	}
	for _, id := range cfg.Channels {
		if err := validateConversationID(id); err != nil {
			return err
		}
	}
	return nil
}

// isEnvVarName reports whether name is a POSIX environment variable name: a leading
// letter or underscore, then letters, digits or underscores.
func isEnvVarName(name string) bool {
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

// writeConfigValidationError names the environment variable it looked at, which is the
// difference between an actionable error and a guessing game once -token-env has been
// pointed somewhere custom.
//
// The two nolints below are gosec's taint analysis reaching a flag value that reaches
// an io.Writer. There is no XSS sink here — stderr on a CLI is not a browser — and
// isEnvVarName has already constrained the value to a POSIX environment variable name
// before it gets this far, so it cannot carry a control character either.
func writeConfigValidationError(stderr io.Writer, tokenEnv string, err error) {
	switch {
	case errors.Is(err, errMissingSlackBotToken):
		_, _ = fmt.Fprintf(stderr, "%s is not set or is empty\n", tokenEnv) //nolint:gosec // G705: stderr on a CLI is not an XSS sink; isEnvVarName constrains tokenEnv.
	case errors.Is(err, errSlackBotTokenControlCharacters):
		_, _ = fmt.Fprintf(stderr, "%s contains control characters\n", tokenEnv) //nolint:gosec // G705: stderr on a CLI is not an XSS sink; isEnvVarName constrains tokenEnv.
	default:
		_, _ = fmt.Fprintln(stderr, err)
	}
}

func normalizeSlackBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = defaultSlackAPIBaseURL
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid -base-url: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("invalid -base-url")
	}
	if parsed.User != nil {
		return "", errBaseURLUserinfo
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errBaseURLQueryFragment
	}
	if parsed.Scheme == "https" || (parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		parsed.Path = strings.TrimRight(parsed.Path, "/")
		return parsed.String(), nil
	}
	return "", errBaseURLRequiresHTTPS
}

func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func cleanSlackToken(token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errMissingSlackBotToken
	}
	if containsHTTPHeaderControl(token) {
		return "", errSlackBotTokenControlCharacters
	}
	return token, nil
}

// containsHTTPHeaderControl reports whether s carries a byte that must never reach a
// request header. Checked on the token and User-Agent because both are written into
// headers verbatim.
func containsHTTPHeaderControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return r < ' ' || r == 0x7f
	})
}

func newSlackHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
