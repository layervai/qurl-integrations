package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	qurl "github.com/layervai/qurl-go/qurl"
	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

// rootCmd builds the full command tree with real process defaults. The
// goreleaser contract test uses it to mirror release-time docs generation,
// which reads command metadata only and never runs a command.
func rootCmd(version string) *cobra.Command {
	root, _ := newRoot(version, output.Detect())
	return root
}

// testAPIKey is a shape-valid test credential for the harness environment:
// the pinned 51-character wire format (prefix + 43 URL-safe base-64 chars).
const testAPIKey = "lv_test_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"

// connectorStateTestDir creates a state namespace through the same
// owner-only setup path that a real CLI invocation uses. Windows temp
// directories inherit a broad ACL, so passing t.TempDir() itself would test
// the intentional fail-closed path instead of a normal installation.
func connectorStateTestDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "connector-state")
	if err := connectorstate.EnsureDirMode(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// fixedNow is the harness clock: one day after the mock server's canned
// created_at timestamps, so relative times render deterministically.
var fixedNow = time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)

// fakeBrowser is the harness's browser-launcher stand-in: it records every
// link the CLI tried to open and never starts a process. The seam is always
// injected, so no test run can ever launch a real browser.
type fakeBrowser struct {
	opened []string
	err    error
}

func (f *fakeBrowser) open(_ context.Context, link string) error {
	f.opened = append(f.opened, link)
	return f.err
}

// runOpts configures one CLI invocation under test.
type runOpts struct {
	args  []string
	env   map[string]string // full environment; nil means just QURL_API_KEY
	stdin io.Reader
	inTTY bool
	tty   bool // stdout+stderr TTY-ness

	configDir string
	sleeps    *[]time.Duration
	// realSleep leaves the production sleep path in place instead of
	// injecting a test double, so the API transport waits out its bounded
	// 429 retries (Retry-After included) on its own context-aware timer.
	// Only the clisandbox-tagged live sandbox suite sets it; hermetic tests
	// never wall-clock sleep.
	realSleep bool
	// browser is the injected launcher recorder; nil means a fresh recorder
	// (pass one to assert on what was — or was not — opened).
	browser *fakeBrowser
	// enterPortal is the injected platform access opener; nil means a
	// refusing fake, so no hermetic test can ever send a real access
	// request (the clisandbox journey uses the production wiring via
	// realOpener instead).
	enterPortal func(ctx context.Context, link string) (string, error)
	// realOpener keeps the production access opener in place instead of the
	// refusing fake. Only the clisandbox-tagged live suite sets it.
	realOpener bool

	// ctx, when non-nil, replaces context.Background() so a test can cancel a
	// foreground daemon or another long-running command.
	ctx                  context.Context
	localShares          []connectorstate.LocalShare
	localSharesErr       error
	localSharesLoads     *int
	shareRegistry        localShareRegistry
	shareDaemon          shareDaemonController
	shareRegistryFactory func(string) (localShareRegistry, error)
	shareDaemonFactory   func(string, string) shareDaemonController
	preflightTarget      func(context.Context, string, int) error
	shareStateDir        string
	shareStateDirErr     error
	localResource        localResourceResolver
	foregroundDaemon     func(context.Context, *globalOpts, string, string) error
	sharingWaitLimit     time.Duration
	// platformGOOS overrides the hermetic default of darwin for tests that
	// exercise the production platform fence.
	platformGOOS string
	// syncStreams serializes writes to the captured stdout/stderr buffers.
	// The connector serve test needs it: the linked FRP client and the
	// in-process test server log from their own goroutines through the
	// redirected process-global logger while the command goroutine writes
	// too.
	syncStreams bool
	// openRegisteredClient overrides login's native enrollment boundary.
	// The default returns the already-validated account mock as a registered
	// client so command tests stay HTTP-only; dedicated API and connector tests
	// cover the real registered-state seams.
	openRegisteredClient func(context.Context, qurlapi.AccountClient, string, *qurlapi.Identity) (qurlapi.Client, *qurlapi.Identity, error)
	openAPIClient        func(context.Context) (qurlapi.Client, error)
}

// runResult captures one invocation's streams and exit code.
type runResult struct {
	stdout bytes.Buffer
	stderr bytes.Buffer
	code   int
}

// runCLI executes the real command tree with injected process context: no
// real environment, no real TTYs, a fixed clock, and recorded sleeps.
func runCLI(t *testing.T, o *runOpts) *runResult {
	t.Helper()

	res := &runResult{}
	env := o.env
	if env == nil {
		env = map[string]string{"QURL_API_KEY": testAPIKey}
	}
	stdin := o.stdin
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	configDir := o.configDir
	if configDir == "" {
		configDir = t.TempDir()
	}

	streams := &output.Streams{
		In:       stdin,
		Out:      &res.stdout,
		Err:      &res.stderr,
		InIsTTY:  o.inTTY,
		OutIsTTY: o.tty,
		ErrIsTTY: o.tty,
	}
	if o.syncStreams {
		streams.Out = &syncWriter{w: &res.stdout}
		streams.Err = &syncWriter{w: &res.stderr}
	}

	browser := o.browser
	if browser == nil {
		browser = &fakeBrowser{}
	}

	root, opts := newRoot("test", streams, func(g *globalOpts) {
		// Exercise the injected background-job/daemon boundary on every host. The
		// separate platform-contract test keeps unsupported hosts fail-closed.
		g.backgroundShareGOOS = o.platformGOOS
		if g.backgroundShareGOOS == "" {
			g.backgroundShareGOOS = "darwin"
		}
		g.lookupEnv = func(key string) (string, bool) {
			v, ok := env[key]
			return v, ok
		}
		g.configDir = configDir
		g.now = func() time.Time { return fixedNow }
		g.newRequestID = func() string { return "cli-req-fixed" }
		g.openAPIClient = func(context.Context) (qurlapi.Client, error) {
			key, err := g.apiCredential()
			if err != nil {
				return nil, err
			}
			return g.apiClient(key)
		}
		if o.openRegisteredClient != nil {
			g.openRegisteredClient = o.openRegisteredClient
		} else {
			g.openRegisteredClient = func(_ context.Context, account qurlapi.AccountClient, _ string, identity *qurlapi.Identity) (qurlapi.Client, *qurlapi.Identity, error) {
				return account, identity, nil
			}
		}
		if o.openAPIClient != nil {
			g.openAPIClient = o.openAPIClient
		}
		g.openBrowser = browser.open
		switch {
		case o.enterPortal != nil:
			g.enterPortal = o.enterPortal
		case o.realOpener:
			// nil is the production default: newRoot wires the real
			// consume.AccessOpener over this invocation's lookupEnv.
		default:
			g.enterPortal = func(_ context.Context, link string) (string, error) {
				return "", fmt.Errorf("test invoked the platform access opener without injecting one (link %d bytes)", len(link))
			}
		}
		switch {
		case o.sleeps != nil:
			g.sleep = func(d time.Duration) { *o.sleeps = append(*o.sleeps, d) }
		case o.realSleep:
			// nil is the production default: the API transport then uses its
			// own context-aware timer, so live-sandbox runs honor real
			// Retry-After waits.
			g.sleep = nil
		default:
			g.sleep = func(time.Duration) {} // tests never wall-clock sleep
		}
		g.loadLocalShares = func(context.Context) ([]connectorstate.LocalShare, error) {
			if o.localSharesLoads != nil {
				*o.localSharesLoads++
			}
			return append([]connectorstate.LocalShare(nil), o.localShares...), o.localSharesErr
		}
		if o.shareRegistryFactory != nil {
			g.openShareRegistry = o.shareRegistryFactory
		} else if o.shareRegistry != nil {
			g.openShareRegistry = func(string) (localShareRegistry, error) { return o.shareRegistry, nil }
		}
		if o.shareDaemonFactory != nil {
			g.newShareDaemon = o.shareDaemonFactory
		} else if o.shareDaemon != nil {
			g.newShareDaemon = func(string, string) shareDaemonController { return o.shareDaemon }
		}
		if o.preflightTarget != nil {
			g.preflightTarget = o.preflightTarget
		}
		resolvedShareStateDir := o.shareStateDir
		if resolvedShareStateDir == "" {
			resolvedShareStateDir = filepath.Join(configDir, "connector-state")
		}
		g.resolveShareStateDir = func(string) (string, error) { return resolvedShareStateDir, o.shareStateDirErr }
		g.resolveSessionConfig = func(ownerID string) (connectorshare.NativeSessionOperationAuthority, error) {
			return connectorshare.NativeSessionOperationAuthority{OwnerID: ownerID}, nil
		}
		if o.localResource != nil {
			g.resolveLocalResource = o.localResource
			g.resolveHubBootstrap = func() (qurl.HubBootstrap, error) { return qurl.HubBootstrap{}, nil }
		}
		if o.foregroundDaemon != nil {
			g.runForegroundDaemon = o.foregroundDaemon
		}
		if o.sharingWaitLimit > 0 {
			g.sharingWaitLimit = o.sharingWaitLimit
		}
		// The FRP global logger is pinned once for the whole test binary in
		// TestMain; a per-invocation swap would race the in-process tunnel
		// server's own log goroutines.
		g.redirectFRPLogs = func() {}
	})
	root.SetArgs(o.args)
	ctx := o.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	res.code = run(ctx, root, opts)
	return res
}

// syncWriter serializes writes from concurrent goroutines into one buffer.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// mustEmptyStdout asserts the data stream carried nothing — the fail-closed
// contract for verification failures and errors.
func mustEmptyStdout(t *testing.T, res *runResult) {
	t.Helper()
	if res.stdout.Len() != 0 {
		t.Fatalf("stdout must be empty, got %q", res.stdout.String())
	}
}
