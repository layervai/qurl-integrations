//go:build clisandbox

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
)

// T6-style live sandbox Connector smoke. This file carries the clisandbox
// build tag, which ARMS the cli / sandbox e2e job's placeholder step (it
// self-silences once a tagged test exists); the env gate below keeps the job
// green until the sandbox Connector credentials are provisioned.
//
// Credential contract (all required before this test runs anything):
//
//	QURL_CLI_SANDBOX_CONNECTOR       = "enabled"  — the arming switch
//	QURL_CLI_SANDBOX_ENDPOINT        — sandbox qURL API base URL
//	QURL_CLI_SANDBOX_CONNECTOR_SLUG  — a slug this tenant may serve
//	QURL_CONNECTOR_HUB_HOST/PORT/SERVER_PUBLIC_KEY_B64 — the sandbox Hub triple
//	QURL_CLI_SANDBOX_CONNECTOR_STATE_DIR — persistent state dir holding an
//	    already-enrolled identity, OR QURL_CONNECTOR_TOKEN(_FILE) carrying a
//	    fresh one-shot enrollment token for this run
//
// The smoke serves a local echo through the real sandbox platform with the
// PRODUCTION wiring (no injected seams), requires admission evidence on
// stderr, then interrupts and requires the graceful Interrupted exit.
func TestSandboxConnectorServeSmoke(t *testing.T) {
	if os.Getenv("QURL_CLI_SANDBOX_CONNECTOR") != "enabled" {
		t.Skip("SKIPPED LOUDLY: live sandbox Connector smoke is disarmed — QURL_CLI_SANDBOX_CONNECTOR != enabled. " +
			"Sandbox Connector credentials are not provisioned yet; arm this by setting QURL_CLI_SANDBOX_CONNECTOR=enabled plus " +
			"QURL_CLI_SANDBOX_ENDPOINT, QURL_CLI_SANDBOX_CONNECTOR_SLUG, the QURL_CONNECTOR_HUB_* triple, and either " +
			"QURL_CLI_SANDBOX_CONNECTOR_STATE_DIR (enrolled identity) or QURL_CONNECTOR_TOKEN(_FILE) (fresh one-shot token).")
	}
	missing := []string{}
	for _, name := range []string{
		"QURL_CLI_SANDBOX_ENDPOINT", "QURL_CLI_SANDBOX_CONNECTOR_SLUG",
		hub.EnvHost, hub.EnvPort, hub.EnvServerPublicKey,
	} {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Skipf("SKIPPED LOUDLY: live sandbox Connector smoke armed but incomplete — missing %v", missing)
	}
	stateDir := strings.TrimSpace(os.Getenv("QURL_CLI_SANDBOX_CONNECTOR_STATE_DIR"))
	if stateDir == "" {
		if os.Getenv("QURL_CONNECTOR_TOKEN") == "" && os.Getenv("QURL_CONNECTOR_TOKEN_FILE") == "" {
			t.Skip("SKIPPED LOUDLY: live sandbox Connector smoke armed but has neither an enrolled state dir " +
				"(QURL_CLI_SANDBOX_CONNECTOR_STATE_DIR) nor a one-shot enrollment token (QURL_CONNECTOR_TOKEN / _FILE)")
		}
		stateDir = t.TempDir()
	}

	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "sandbox-connector-smoke")
	}))
	defer echo.Close()
	echoURL, err := url.Parse(echo.URL)
	if err != nil {
		t.Fatal(err)
	}

	// Serve for long enough to knock, log in, and register the route against
	// the live sandbox, then interrupt.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(45 * time.Second)
		cancel()
	}()

	res := runCLI(t, &runOpts{
		ctx: ctx,
		args: []string{
			"--endpoint", os.Getenv("QURL_CLI_SANDBOX_ENDPOINT"), "connector", "run",
			"--slug", os.Getenv("QURL_CLI_SANDBOX_CONNECTOR_SLUG"),
			"--target", ":" + echoURL.Port(),
			"--state-dir", stateDir,
		},
		env:         map[string]string{},
		syncStreams: true,
	})
	if res.code != 130 {
		t.Fatalf("exit = %d, want 130 after the graceful interrupt\nstderr: %s", res.code, res.stderr.String())
	}
	stderr := res.stderr.String()
	// Admission evidence: the runner's login_success event only fires when
	// the sandbox tunnel server accepted the Login under the native RunID.
	if !strings.Contains(stderr, "login_success") {
		t.Fatalf("no tunnel admission evidence (login_success) in 45s against the sandbox:\n%s", stderr)
	}
	if !strings.Contains(stderr, "Stopped.") {
		t.Fatalf("missing the graceful-stop note:\n%s", stderr)
	}
}
