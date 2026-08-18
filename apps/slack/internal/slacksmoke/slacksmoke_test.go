package slacksmoke

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testToken       = "xoxb-test-token"
	testAuthHeader  = "Authorization"
	testLoopbackURL = "http://127.0.0.1:8080"
	testHTTPSURL    = "https://slack.test"
)

func TestNormalizeBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr error
	}{
		{name: "empty defaults", raw: "", want: DefaultAPIBaseURL},
		{name: "whitespace defaults", raw: "   ", want: DefaultAPIBaseURL},
		{name: "https kept", raw: testHTTPSURL, want: testHTTPSURL},
		{name: "trailing slash trimmed", raw: testHTTPSURL + "/api/", want: testHTTPSURL + "/api"},
		{name: "default host trailing slash trimmed", raw: DefaultAPIBaseURL + "/", want: DefaultAPIBaseURL},
		{name: "custom path preserved", raw: testHTTPSURL + "/api/~smoke/", want: testHTTPSURL + "/api/~smoke"},
		{name: "http loopback ip allowed", raw: testLoopbackURL, want: testLoopbackURL},
		{name: "http loopback keeps port and trims path", raw: "http://localhost:1234/api/", want: "http://localhost:1234/api"},
		{name: "http ipv6 loopback keeps port and path", raw: "http://[::1]:1234/api", want: "http://[::1]:1234/api"},
		{name: "http localhost allowed", raw: "http://localhost:8080", want: "http://localhost:8080"},
		{name: "http ipv6 loopback allowed", raw: "http://[::1]:8080", want: "http://[::1]:8080"},
		// The reason this function exists: a bearer token must not go out in plaintext
		// to a host nobody can vouch for.
		{name: "http public host refused", raw: "http://slack.test", wantErr: ErrBaseURLRequiresHTTPS},
		{name: "http near-loopback host refused", raw: "http://localhost.evil.test", wantErr: ErrBaseURLRequiresHTTPS},
		{name: "http non-loopback ip refused", raw: "http://8.8.8.8", wantErr: ErrBaseURLRequiresHTTPS},
		{name: "userinfo refused", raw: "https://user:pass@slack.test", wantErr: ErrBaseURLUserinfo},
		{name: "query refused", raw: testHTTPSURL + "?a=b", wantErr: ErrBaseURLQueryFragment},
		{name: "fragment refused", raw: testHTTPSURL + "#f", wantErr: ErrBaseURLQueryFragment},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeBaseURL(tc.raw)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("NormalizeBaseURL(%q) error = %v, want %v", tc.raw, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeBaseURL(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeBaseURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestNormalizeBaseURLRejectsUnparseable covers the inputs that fail the scheme/host
// shape check rather than one of the named sentinels.
func TestNormalizeBaseURLRejectsUnparseable(t *testing.T) {
	for _, raw := range []string{"//slack.test", "slack.test", "://slack.test", "https://", "http://[::1"} {
		t.Run(raw, func(t *testing.T) {
			if got, err := NormalizeBaseURL(raw); err == nil {
				t.Fatalf("NormalizeBaseURL(%q) = %q, want an error", raw, got)
			}
		})
	}
}

func TestCleanToken(t *testing.T) {
	tests := []struct {
		name    string
		token   string
		want    string
		wantErr error
	}{
		{name: "trimmed", token: "  " + testToken + "\t", want: testToken},
		{name: "empty", token: "", wantErr: ErrMissingBotToken},
		{name: "whitespace only", token: "   ", wantErr: ErrMissingBotToken},
		{name: "newline", token: "xoxb-a\nb", wantErr: ErrBotTokenControlCharacters},
		{name: "carriage return", token: "xoxb-a\rb", wantErr: ErrBotTokenControlCharacters},
		{name: "del", token: "xoxb-a\x7fb", wantErr: ErrBotTokenControlCharacters},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CleanToken(tc.token)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("CleanToken(%q) error = %v, want %v", tc.token, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("CleanToken(%q): %v", tc.token, err)
			}
			if got != tc.want {
				t.Fatalf("CleanToken(%q) = %q, want %q", tc.token, got, tc.want)
			}
		})
	}
}

func TestContainsHTTPHeaderControl(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{in: "qurl-slack-dm-smoke", want: false},
		{in: "", want: false},
		{in: "with space", want: false},
		{in: "a\nb", want: true},
		{in: "a\rb", want: true},
		{in: "a\tb", want: true},
		{in: "a\x00b", want: true},
		{in: "a\x7fb", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := ContainsHTTPHeaderControl(tc.in); got != tc.want {
				t.Fatalf("ContainsHTTPHeaderControl(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestNewHTTPClientDoesNotReplayTokenOnRedirect pins the promise the redirect policy
// makes: a 3xx is surfaced, not followed, so the Authorization header never reaches
// the host the response points at.
func TestNewHTTPClientDoesNotReplayTokenOnRedirect(t *testing.T) {
	// Guarded because the assertion below reads what a server goroutine would write in
	// the failure this test exists to catch; an unsynchronized read there would race
	// under -race exactly when the test is doing its job.
	var mu sync.Mutex
	var targetAuth string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		targetAuth = r.Header.Get(testAuthHeader)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer origin.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, origin.URL, http.NoBody)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set(testAuthHeader, "Bearer "+testToken)

	resp, err := NewHTTPClient(5 * time.Second).Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d (redirect must be surfaced, not followed)", resp.StatusCode, http.StatusFound)
	}
	mu.Lock()
	defer mu.Unlock()
	if targetAuth != "" {
		t.Fatalf("redirect target saw Authorization %q, want it never sent", targetAuth)
	}
}

func TestNewHTTPClientTimeout(t *testing.T) {
	if got := NewHTTPClient(3 * time.Second).Timeout; got != 3*time.Second {
		t.Fatalf("Timeout = %v, want %v", got, 3*time.Second)
	}
}

// TestDrainResponseBodyHonoursCallerLimit is the regression guard for the hazard that
// motivated the limit parameter: when this function was duplicated per command it read
// whichever maxSlackResponseBytes sat in the same file, so the same body drained a 64x
// different budget depending on which copy ran.
func TestDrainResponseBodyHonoursCallerLimit(t *testing.T) {
	const total = 4096
	for _, limit := range []int64{0, 16, 1024} {
		src := strings.NewReader(strings.Repeat("a", total))
		DrainResponseBody(src, limit)
		if got := int64(total - src.Len()); got != limit+1 {
			t.Fatalf("DrainResponseBody(_, %d) consumed %d bytes, want %d", limit, got, limit+1)
		}
	}
	// A negative limit must drain nothing rather than be read as an EOF-immediately
	// count that silently skips the drain.
	src := strings.NewReader(strings.Repeat("a", total))
	DrainResponseBody(src, -5)
	if got := total - src.Len(); got != 1 {
		t.Fatalf("DrainResponseBody(_, -5) consumed %d bytes, want 1", got)
	}
}

func TestDrainResponseBodyStopsAtShortBody(t *testing.T) {
	src := strings.NewReader("short")
	DrainResponseBody(src, 1<<20)
	if src.Len() != 0 {
		t.Fatalf("unread = %d, want 0", src.Len())
	}
}

func TestIsEnvVarName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{name: "SLACK_BOT_TOKEN", want: true},
		{name: "_LEADING_UNDERSCORE", want: true},
		{name: "lower_case9", want: true},
		{name: "A1", want: true},
		{name: "", want: false},
		{name: "9LEADING_DIGIT", want: false},
		{name: "HAS-DASH", want: false},
		{name: "HAS SPACE", want: false},
		{name: "HAS.DOT", want: false},
		// The value is echoed into the command's own stderr diagnostics, so a name
		// carrying a newline is exactly what this rejects.
		{name: "INJECT\nLINE", want: false},
		{name: "UNICODE_É", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsEnvVarName(tc.name); got != tc.want {
				t.Fatalf("IsEnvVarName(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestTimeoutBudgetValidate(t *testing.T) {
	tests := []struct {
		name      string
		budget    TimeoutBudget
		minFactor int
		wantErr   error
		wantText  string
	}{
		{
			name:      "usable",
			budget:    TimeoutBudget{Overall: 90 * time.Second, Request: 10 * time.Second},
			minFactor: 3,
		},
		{
			name:      "exactly at factor",
			budget:    TimeoutBudget{Overall: 30 * time.Second, Request: 10 * time.Second},
			minFactor: 3,
		},
		{
			name:      "overall not positive",
			budget:    TimeoutBudget{Overall: 0, Request: 10 * time.Second},
			minFactor: 3,
			wantErr:   ErrOverallTimeoutNotPositive,
			wantText:  "-timeout must be greater than 0",
		},
		{
			name:      "request not positive",
			budget:    TimeoutBudget{Overall: 90 * time.Second, Request: 0},
			minFactor: 3,
			wantErr:   ErrRequestTimeoutNotPositive,
			wantText:  "-request-timeout must be greater than 0",
		},
		{
			// Ordering matters: an equal pair must get this message rather than the
			// multiplier one, which is why the check sits ahead of the factor guard.
			name:      "request equals overall",
			budget:    TimeoutBudget{Overall: 10 * time.Second, Request: 10 * time.Second},
			minFactor: 3,
			wantErr:   ErrRequestTimeoutNotLess,
			wantText:  "-request-timeout must be less than -timeout",
		},
		{
			name:      "request exceeds overall",
			budget:    TimeoutBudget{Overall: 5 * time.Second, Request: 10 * time.Second},
			minFactor: 3,
			wantErr:   ErrRequestTimeoutNotLess,
			wantText:  "-request-timeout must be less than -timeout",
		},
		{
			name:      "below factor",
			budget:    TimeoutBudget{Overall: 25 * time.Second, Request: 10 * time.Second},
			minFactor: 3,
			wantText:  "-timeout must be at least 3x -request-timeout",
		},
		{
			name:      "factor is caller supplied",
			budget:    TimeoutBudget{Overall: 35 * time.Second, Request: 10 * time.Second},
			minFactor: 4,
			wantText:  "-timeout must be at least 4x -request-timeout",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.budget.Validate(tc.minFactor)
			if tc.wantText == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate = nil, want %q", tc.wantText)
			}
			// The text is the operator-facing contract: commands Fprintln it verbatim.
			if err.Error() != tc.wantText {
				t.Fatalf("Validate = %q, want %q", err.Error(), tc.wantText)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate = %v, want errors.Is %v", err, tc.wantErr)
			}
		})
	}
}
