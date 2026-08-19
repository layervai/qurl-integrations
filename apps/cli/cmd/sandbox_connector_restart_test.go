//go:build clisandbox

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
)

// Live sandbox regression smoke for the reported restart defect: stopping a
// Connector and starting it again under the same --id left it unable to serve
// for about a minute while saying nothing an operator could act on. The FRP
// client owns its own post-admission reconnect loop and retries there forever
// (keepControllerWorking passes firstLoginExit=false, hard-coded), so before
// the reconnect watchdog the supervisor never regained control: no budget, no
// classification, no message, no exit.
//
// What this pins is the INVARIANT, not a mechanism. The dial failures in that
// window are multiplexer transport errors carrying no server-supplied reason,
// so this test deliberately does not assert why the restart is refused — only
// that the second start either serves or explains itself, and that it never
// goes quiet for longer than the watchdog's window.
//
// Arming and credentials are the same contract as TestSandboxConnectorServeSmoke
// in sandbox_connector_test.go; see its header. This one additionally needs a
// state dir that survives between the two starts, because the second start must
// present the SAME enrolled identity — a fresh temp dir would enroll a new one
// and test nothing. A one-shot enrollment token cannot satisfy that (it burns
// on the first start), so this test requires QURL_CLI_SANDBOX_CONNECTOR_STATE_DIR.
func TestSandboxConnectorRestartIsBoundedAndExplained(t *testing.T) {
	if os.Getenv("QURL_CLI_SANDBOX_CONNECTOR") != "enabled" {
		t.Skip("SKIPPED LOUDLY: live sandbox Connector restart smoke is disarmed — QURL_CLI_SANDBOX_CONNECTOR != enabled. " +
			"Arm it with QURL_CLI_SANDBOX_CONNECTOR=enabled plus QURL_CLI_SANDBOX_ENDPOINT, QURL_CLI_SANDBOX_CONNECTOR_SLUG, " +
			"the QURL_CONNECTOR_HUB_* triple, and QURL_CLI_SANDBOX_CONNECTOR_STATE_DIR holding an already-enrolled identity.")
	}
	missing := []string{}
	for _, name := range []string{
		"QURL_CLI_SANDBOX_ENDPOINT", "QURL_CLI_SANDBOX_CONNECTOR_SLUG",
		"QURL_CLI_SANDBOX_CONNECTOR_STATE_DIR",
		hub.EnvHost, hub.EnvPort, hub.EnvServerPublicKey,
	} {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Skipf("SKIPPED LOUDLY: live sandbox Connector restart smoke armed but incomplete — missing %v", missing)
	}
	stateDir := strings.TrimSpace(os.Getenv("QURL_CLI_SANDBOX_CONNECTOR_STATE_DIR"))

	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "sandbox-connector-restart-smoke")
	}))
	defer echo.Close()
	echoURL, err := url.Parse(echo.URL)
	if err != nil {
		t.Fatal(err)
	}

	serve := func(label string, budget time.Duration) *runResult {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			time.Sleep(budget)
			cancel()
		}()
		res := runCLI(t, &runOpts{
			ctx: ctx,
			args: []string{
				"--endpoint", os.Getenv("QURL_CLI_SANDBOX_ENDPOINT"), "connector", "run",
				"--id", os.Getenv("QURL_CLI_SANDBOX_CONNECTOR_SLUG"),
				"--target", ":" + echoURL.Port(),
				"--state-dir", stateDir,
			},
			env:         map[string]string{},
			syncStreams: true,
			realSleep:   true,
		})
		t.Logf("%s: exit=%d\n%s", label, res.code, res.stderr.String())
		return res
	}

	// First start: reach a real admission, then stop. This is the state the
	// report starts from — a Connector that served and was then stopped.
	first := serve("first start", 45*time.Second)
	if !strings.Contains(first.stderr.String(), "login_success") {
		t.Fatalf("first start never reached tunnel admission in 45s; the sandbox is not in a testable state\nstderr: %s", first.stderr.String())
	}

	// Second start, immediately: the reported defect. Give it more than the
	// watchdog's window so a stalled reconnect has room to be reported AND
	// acted on inside this run.
	second := serve("immediate restart", 150*time.Second)
	stderr := second.stderr.String()

	// Outcome 1 — it served. Nothing more to prove.
	if strings.Contains(stderr, "login_success") {
		return
	}

	// Outcome 2 — it could not serve, in which case it MUST have explained
	// itself and MUST have bounded the wait. Silence is the defect.
	explained := false
	for _, marker := range []string{
		"reconnect_retrying", // the watchdog's operator notice
		"reconnect_stalled",  // the watchdog took the cycle back
		"login_error",        // the supervisor classified a failed login
		"login_deny",         // the server refused the knock token
		"too_many_knock_failures",
	} {
		if strings.Contains(stderr, marker) {
			explained = true
			break
		}
	}
	if !explained {
		t.Fatalf("restart neither served nor explained itself in 150s — this is the reported defect: the operator sees only transport noise and no actionable event\nstderr: %s", stderr)
	}

	// Bounded: a run that ends on its own must carry a real exit code, not
	// hang until the harness cancels it. 130 means the harness's own cancel
	// won the race (still bounded); 11 is the retry-budget exit.
	if second.code != 11 && second.code != 130 {
		t.Fatalf("restart exit = %d, want 11 (retry budget) or 130 (canceled); an unbounded loop is the defect\nstderr: %s", second.code, stderr)
	}

	// And it must never have gone quiet for longer than the watchdog window.
	if gap, ok := longestSilentGap(stderr); ok && gap > 2*time.Minute {
		t.Fatalf("restart went %s without any operator-facing line; the watchdog window is 90s\nstderr: %s", gap, stderr)
	}
}

// slogTime matches the RFC3339 stamp slog's text handler puts on every line,
// which is how this test measures operator-visible silence.
var slogTime = regexp.MustCompile(`time=(\S+)`)

// longestSilentGap returns the longest interval between consecutive stderr
// lines carrying a slog timestamp. FRP's own logger uses a different format
// and is skipped on purpose: its transport noise is exactly what does NOT
// count as the Connector explaining itself.
func longestSilentGap(stderr string) (time.Duration, bool) {
	var prev time.Time
	var longest time.Duration
	seen := false
	for _, line := range strings.Split(stderr, "\n") {
		m := slogTime.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ts, err := time.Parse(time.RFC3339, strings.Trim(m[1], `"`))
		if err != nil {
			continue
		}
		if seen {
			if gap := ts.Sub(prev); gap > longest {
				longest = gap
			}
		}
		prev, seen = ts, true
	}
	return longest, seen
}
