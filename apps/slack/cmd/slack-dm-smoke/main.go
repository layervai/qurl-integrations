// Command slack-dm-smoke runs an operator-triggered Slack DM delivery smoke.
package main

import (
	"bytes"
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
	"unicode/utf8"
)

const (
	defaultSlackAPIBaseURL = "https://slack.com/api"
	defaultUserAgent       = "qurl-slack-dm-smoke"
	defaultOverallTimeout  = 90 * time.Second
	defaultRequestTimeout  = 10 * time.Second
	minRequiredCallFactor  = 3
	minDirectProbeFactor   = 4
	maxSlackResponseBytes  = 64 * 1024
	// maxSmokeTextBytes is a conservative local cap for non-secret smoke text,
	// not Slack's chat.postMessage ceiling.
	maxSmokeTextBytes        = 4000
	directUserProbeSuffix    = " (direct-user probe)"
	apiErrorRequestFailed    = "request_failed"
	apiErrorRequestTimeout   = "request_timeout"
	apiErrorRequestCanceled  = "request_canceled"
	apiErrorBudgetExhausted  = "budget_exhausted"
	apiErrorResponseTooLarge = "response_too_large"
	messageKindNonSecret     = "non_secret_smoke"
)

var (
	errMissingSlackBotToken           = errors.New("missing Slack bot token")
	errSlackBotTokenControlCharacters = errors.New("invalid Slack bot token: contains control characters")
	errMissingSlackUserID             = errors.New("missing Slack user ID")
	errSlackUserIDSeparatorControl    = errors.New("invalid Slack user ID: contains comma, ASCII whitespace, or ASCII control characters")
	errStrictDirectProbeRequiresProbe = errors.New("-strict-direct-user-probe requires -direct-user-probe")
	errBaseURLRequiresHTTPS           = errors.New("-base-url must use https unless host is localhost or loopback")
	errBaseURLQueryFragment           = errors.New("-base-url must not include query or fragment")
	errBaseURLUserinfo                = errors.New("-base-url must not include userinfo")
	errSmokeTextTooLong               = errors.New("smoke text is too long")
	errUserAgentControlCharacters     = errors.New("-user-agent contains control characters")
)

type smokeConfig struct {
	Token             string
	UserID            string
	Text              string
	BaseURL           string
	UserAgent         string
	WorkspaceShape    string
	TokenOwner        string
	Scopes            string
	DirectUserProbe   bool
	ForceDirectStrict bool
	HTTPClient        *http.Client
	StartedAt         time.Time
}

type smokeResult struct {
	StartedAt       string          `json:"started_at"`
	WorkspaceShape  string          `json:"workspace_shape,omitempty"`
	TokenOwner      string          `json:"token_owner,omitempty"`
	Scopes          string          `json:"scopes,omitempty"`
	UserID          string          `json:"user_id"`
	MessageKind     string          `json:"message_kind"`
	Auth            *apiCallResult  `json:"auth_test,omitempty"`
	ProductionPath  []apiCallResult `json:"production_path"`
	DirectUserProbe *apiCallResult  `json:"direct_user_probe,omitempty"`
}

// apiCallResult is the evidence union for auth.test, conversations.open, and
// chat.postMessage calls. PostedChannel records what the smoke sent, while
// ChannelID records Slack's response when a method returns a channel.
type apiCallResult struct {
	Method        string `json:"method"`
	OK            bool   `json:"ok"`
	StatusCode    int    `json:"status_code"`
	Error         string `json:"error,omitempty"`
	Needed        string `json:"needed,omitempty"`
	Provided      string `json:"provided,omitempty"`
	ChannelID     string `json:"channel_id,omitempty"`
	PostedChannel string `json:"posted_channel,omitempty"`
	Timestamp     string `json:"ts,omitempty"`
	RetryAfter    string `json:"retry_after,omitempty"`
	TeamID        string `json:"team_id,omitempty"`
	EnterpriseID  string `json:"enterprise_id,omitempty"`
	BotID         string `json:"bot_id,omitempty"`
	TokenUserID   string `json:"token_user_id,omitempty"`
}

type slackAPIResponse struct {
	OK           bool           `json:"ok"`
	Error        string         `json:"error"`
	Needed       string         `json:"needed"`
	Provided     string         `json:"provided"`
	TS           string         `json:"ts"`
	Channel      slackChannelID `json:"channel"`
	TeamID       string         `json:"team_id"`
	EnterpriseID string         `json:"enterprise_id"`
	BotID        string         `json:"bot_id"`
	UserID       string         `json:"user_id"`
}

type slackChannelID string

func (c *slackChannelID) UnmarshalJSON(raw []byte) error {
	var id string
	if err := json.Unmarshal(raw, &id); err == nil {
		*c = slackChannelID(id)
		return nil
	}
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return err
	}
	*c = slackChannelID(obj.ID)
	return nil
}

// slackClient is local to the smoke because operators supply the exact bot
// token under test. Production's Enterprise Grid fallback is a token-lookup
// fallback, not a different wire shape; keep this open-then-post path aligned
// with production while leaving lookup behavior to guided-setup tests.
type slackClient struct {
	token      string
	baseURL    string
	userAgent  string
	httpClient *http.Client
}

func main() {
	os.Exit(run(context.Background(), os.Stdout, os.Stderr, os.Args[1:], os.Getenv, time.Now))
}

func run(ctx context.Context, stdout, stderr io.Writer, args []string, getenv func(string) string, now func() time.Time) int {
	fs := flag.NewFlagSet("slack-dm-smoke", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var tokenEnv string
	var timeout time.Duration
	var requestTimeout time.Duration
	cfg := smokeConfig{}
	fs.StringVar(&tokenEnv, "token-env", "SLACK_BOT_TOKEN", "environment variable containing the Slack bot token")
	fs.StringVar(&cfg.UserID, "user", "", "Slack user ID to receive the smoke DM")
	fs.StringVar(&cfg.Text, "text", "", "non-secret smoke message text")
	fs.StringVar(&cfg.BaseURL, "base-url", defaultSlackAPIBaseURL, "Slack Web API base URL")
	fs.StringVar(&cfg.UserAgent, "user-agent", defaultUserAgent, "HTTP User-Agent")
	fs.StringVar(&cfg.WorkspaceShape, "workspace-shape", "", "operator note, for example Enterprise Grid org install")
	fs.StringVar(&cfg.TokenOwner, "token-owner", "", "operator note for the token owner, for example workspace or enterprise")
	fs.StringVar(&cfg.Scopes, "scopes", "", "operator note for Slack scopes on the tested app")
	fs.BoolVar(&cfg.DirectUserProbe, "direct-user-probe", false, "also try chat.postMessage with channel set to the user ID; may send a second non-secret DM")
	fs.BoolVar(&cfg.ForceDirectStrict, "strict-direct-user-probe", false, "make a failing direct-user probe fail the command")
	fs.DurationVar(&timeout, "timeout", defaultOverallTimeout, "overall smoke timeout")
	fs.DurationVar(&requestTimeout, "request-timeout", defaultRequestTimeout, "timeout for each Slack Web API request")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	tokenEnv = strings.TrimSpace(tokenEnv)
	if tokenEnv == "" {
		_, _ = fmt.Fprintln(stderr, "-token-env is required")
		return 2
	}
	cfg.Token = getenv(tokenEnv)
	if timeout <= 0 {
		_, _ = fmt.Fprintln(stderr, "-timeout must be greater than 0")
		return 2
	}
	if requestTimeout <= 0 {
		_, _ = fmt.Fprintln(stderr, "-request-timeout must be greater than 0")
		return 2
	}
	// Keep this explicit before the factor guard so equal/exceeding values get
	// the direct operator-facing error.
	if requestTimeout >= timeout {
		_, _ = fmt.Fprintln(stderr, "-request-timeout must be less than -timeout")
		return 2
	}
	minTimeoutFactor := minRequiredCallFactor
	if cfg.DirectUserProbe {
		minTimeoutFactor = minDirectProbeFactor
	}
	if timeout < time.Duration(minTimeoutFactor)*requestTimeout {
		_, _ = fmt.Fprintf(stderr, "-timeout must be at least %dx -request-timeout\n", minTimeoutFactor)
		return 2
	}
	cfg.StartedAt = now().UTC()
	cfg.HTTPClient = newSlackHTTPClient(requestTimeout)

	if err := prepareSmokeConfig(&cfg); err != nil {
		writeConfigValidationError(stderr, tokenEnv, err)
		return 2
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, err := runPreparedSmoke(runCtx, &cfg)
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

func runSmoke(ctx context.Context, cfg *smokeConfig) (smokeResult, error) {
	if cfg == nil {
		return smokeResult{}, errors.New("missing smoke config")
	}
	// runSmoke is only the direct test seam for unprepared configs. It depends on
	// prepareSmokeConfig staying idempotent; the CLI prepares once before
	// runPreparedSmoke. Timeout-budget validation is CLI-only because direct
	// callers provide the context and client.
	prepared := *cfg
	if err := prepareSmokeConfig(&prepared); err != nil {
		return newSmokeResult(&prepared), err
	}
	return runPreparedSmoke(ctx, &prepared)
}

// runPreparedSmoke assumes cfg has already passed prepareSmokeConfig.
func runPreparedSmoke(ctx context.Context, cfg *smokeConfig) (smokeResult, error) {
	result := newSmokeResult(cfg)
	c := slackClient{
		token:      cfg.Token,
		baseURL:    cfg.BaseURL,
		userAgent:  cfg.UserAgent,
		httpClient: cfg.HTTPClient,
	}

	// Preflight auth.test for evidence, then mirror the production open-then-post path.
	// Token lookup and Grid fallback stay in guided setup and its contract tests.
	auth, err := c.post(ctx, "auth.test", nil)
	result.Auth = &auth
	if err != nil {
		return result, err
	}

	open, err := c.post(ctx, "conversations.open", map[string]string{"users": result.UserID})
	channelID := open.ChannelID
	if err == nil && channelID == "" {
		open.OK = false
		open.Error = "missing_dm_channel_id"
		err = errors.New("conversations.open returned no channel id")
	}
	result.ProductionPath = append(result.ProductionPath, open)
	if err != nil {
		return result, err
	}

	post, err := c.post(ctx, "chat.postMessage", map[string]string{"channel": channelID, "text": cfg.Text})
	post.PostedChannel = channelID
	result.ProductionPath = append(result.ProductionPath, post)
	if err != nil {
		return result, err
	}

	if cfg.DirectUserProbe {
		// Non-strict probe errors are evidence-only unless the shared overall
		// context is done; production-path failures are always fatal.
		direct, directErr := c.post(ctx, "chat.postMessage", map[string]string{"channel": result.UserID, "text": directUserProbeText(cfg.Text)})
		direct.PostedChannel = result.UserID
		result.DirectUserProbe = &direct
		if directErr != nil {
			if cfg.ForceDirectStrict {
				return result, directErr
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return result, ctxErr
			}
		}
	}

	return result, nil
}

func prepareSmokeConfig(cfg *smokeConfig) error {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = newSlackHTTPClient(defaultRequestTimeout)
	}
	if cfg.StartedAt.IsZero() {
		cfg.StartedAt = time.Now().UTC()
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = defaultUserAgent
	}

	cleanedUserID, userIDErr := cleanSlackUserID(cfg.UserID)
	var err error
	// Token errors come first for CLI UX, but cleaned user evidence is still
	// preserved when direct callers receive the raw validation error.
	cfg.Token, err = cleanSlackToken(cfg.Token)
	if err != nil {
		cfg.UserID = cleanedUserID
		return err
	}
	cfg.UserID = cleanedUserID
	if userIDErr != nil {
		return userIDErr
	}
	if containsHTTPHeaderControl(cfg.UserAgent) {
		return errUserAgentControlCharacters
	}
	if cfg.ForceDirectStrict && !cfg.DirectUserProbe {
		return errStrictDirectProbeRequiresProbe
	}
	cfg.BaseURL, err = normalizeSlackBaseURL(cfg.BaseURL)
	if err != nil {
		return err
	}
	cfg.Text, err = cleanSmokeMessageText(cfg.Text, cfg.StartedAt)
	if err != nil {
		return err
	}
	return nil
}

func newSmokeResult(cfg *smokeConfig) smokeResult {
	return smokeResult{
		StartedAt:      cfg.StartedAt.UTC().Format(time.RFC3339),
		WorkspaceShape: cleanSlackField(cfg.WorkspaceShape),
		TokenOwner:     cleanSlackField(cfg.TokenOwner),
		Scopes:         cleanSlackField(cfg.Scopes),
		UserID:         cfg.UserID,
		MessageKind:    messageKindNonSecret,
		ProductionPath: []apiCallResult{},
	}
}

func writeConfigValidationError(stderr io.Writer, tokenEnv string, err error) {
	switch {
	case errors.Is(err, errMissingSlackUserID):
		_, _ = fmt.Fprintln(stderr, "-user is required")
	case errors.Is(err, errSlackUserIDSeparatorControl):
		_, _ = fmt.Fprintln(stderr, "-user contains comma, ASCII whitespace, or ASCII control characters")
	case errors.Is(err, errStrictDirectProbeRequiresProbe):
		_, _ = fmt.Fprintln(stderr, errStrictDirectProbeRequiresProbe.Error())
	case errors.Is(err, errMissingSlackBotToken):
		_, _ = fmt.Fprintf(stderr, "%s is not set or is empty\n", tokenEnv)
	case errors.Is(err, errSlackBotTokenControlCharacters):
		_, _ = fmt.Fprintf(stderr, "%s contains control characters\n", tokenEnv)
	case errors.Is(err, errSmokeTextTooLong):
		_, _ = fmt.Fprintf(stderr, "-text must be at most %d bytes after cleanup\n", maxSmokeTextBytes)
	case errors.Is(err, errUserAgentControlCharacters):
		_, _ = fmt.Fprintln(stderr, errUserAgentControlCharacters.Error())
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

func cleanSmokeMessageText(text string, startedAt time.Time) (string, error) {
	text = cleanSlackField(text)
	if text == "" {
		text = defaultSmokeMessage(startedAt)
	}
	if len(text) > maxSmokeTextBytes {
		return "", fmt.Errorf("%w: got %d bytes, limit %d", errSmokeTextTooLong, len(text), maxSmokeTextBytes)
	}
	return text, nil
}

func directUserProbeText(text string) string {
	for len(text)+len(directUserProbeSuffix) > maxSmokeTextBytes && text != "" {
		_, size := utf8.DecodeLastRuneInString(text)
		if size == 0 {
			break
		}
		text = text[:len(text)-size]
	}
	if text == "" {
		return strings.TrimSpace(directUserProbeSuffix)
	}
	return text + directUserProbeSuffix
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

func cleanSlackUserID(userID string) (string, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return "", errMissingSlackUserID
	}
	// Slack user IDs are single tokens. Reject comma plus ASCII whitespace and
	// controls, but leave the exact ID alphabet to Slack's authoritative validation.
	if strings.ContainsFunc(userID, func(r rune) bool {
		return r == ',' || r <= ' ' || r == 0x7f
	}) {
		return "", errSlackUserIDSeparatorControl
	}
	return userID, nil
}

func newSlackHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func defaultSmokeMessage(startedAt time.Time) string {
	return "qURL Slack DM delivery smoke " + startedAt.UTC().Format(time.RFC3339) + ". No secrets are included."
}

func (c slackClient) post(ctx context.Context, method string, body any) (apiCallResult, error) {
	result, _, err := c.postRaw(ctx, method, body)
	return result, err
}

func (c slackClient) postRaw(ctx context.Context, method string, body any) (apiCallResult, slackAPIResponse, error) {
	result := apiCallResult{Method: method}
	var out slackAPIResponse
	var requestBody io.Reader = http.NoBody
	hasBody := body != nil
	if hasBody {
		rawBody, err := json.Marshal(body)
		if err != nil {
			result.Error = "request_marshal"
			return result, out, fmt.Errorf("%s request marshal: %w", method, err)
		}
		requestBody = bytes.NewReader(rawBody)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+method, requestBody)
	if err != nil {
		result.Error = "request_build"
		return result, out, fmt.Errorf("%s request build: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		result.Error = classifyRequestError(ctx, err)
		return result, out, fmt.Errorf("%s request: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	result.StatusCode = resp.StatusCode

	if resp.StatusCode >= 300 {
		result.Error = fmt.Sprintf("http_%d", resp.StatusCode)
		if resp.StatusCode == http.StatusTooManyRequests {
			result.RetryAfter = cleanSlackField(resp.Header.Get("Retry-After"))
		}
		drainSlackResponseBody(resp.Body)
		return result, out, fmt.Errorf("%s returned HTTP %d", method, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSlackResponseBytes+1))
	if err != nil {
		result.Error = "response_read"
		return result, out, fmt.Errorf("%s response read: %w", method, err)
	}
	if len(raw) > maxSlackResponseBytes {
		drainSlackResponseBody(resp.Body)
		result.Error = apiErrorResponseTooLarge
		return result, out, fmt.Errorf("%s response exceeded %d bytes", method, maxSlackResponseBytes)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		result.Error = "response_json"
		return result, out, fmt.Errorf("%s response JSON invalid", method)
	}
	result.OK = out.OK
	result.Error = cleanSlackField(out.Error)
	result.Needed = cleanSlackField(out.Needed)
	result.Provided = cleanSlackField(out.Provided)
	result.ChannelID = cleanSlackField(string(out.Channel))
	result.Timestamp = cleanSlackField(out.TS)
	result.TeamID = cleanSlackField(out.TeamID)
	result.EnterpriseID = cleanSlackField(out.EnterpriseID)
	result.BotID = cleanSlackField(out.BotID)
	result.TokenUserID = cleanSlackField(out.UserID)
	if !out.OK {
		if result.Error == "" {
			result.Error = "not_ok"
		}
		return result, out, fmt.Errorf("%s: %s", method, result.Error)
	}
	return result, out, nil
}

func drainSlackResponseBody(body io.Reader) {
	// Best-effort connection reuse for moderately oversized bodies. Close tears
	// down the response if bytes still remain.
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxSlackResponseBytes+1))
}

func classifyRequestError(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return apiErrorRequestCanceled
	case errors.Is(err, context.DeadlineExceeded):
		// If the per-request and overall deadlines fire together, prefer the
		// outer budget label when the shared smoke context reports expiry.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return apiErrorBudgetExhausted
		}
		return apiErrorRequestTimeout
	default:
		return apiErrorRequestFailed
	}
}

func containsHTTPHeaderControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool {
		return r < ' ' || r == 0x7f
	})
}

func cleanSlackField(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case r < ' ' || r == 0x7f:
			return '?'
		default:
			return r
		}
	}, strings.TrimSpace(s))
}
