package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
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

// testAPIKeyStored is a second shape-valid key, distinguishable from
// testAPIKey, for asserting which credential source a command actually used.
const testAPIKeyStored = "lv_test_storedstoredstoredstoredstoredstored0123456"

// fixedNow is the harness clock: one day after the mock server's canned
// created_at timestamps, so relative times render deterministically.
var fixedNow = time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)

// fakeKeyring is the harness's OS-keyring stand-in, injected so cmd tests
// never touch a developer's real keyring. It obeys the CredentialStore
// contract Chain keys on: an empty available keyring wraps ErrNoCredential,
// an unavailable one errors any other way (reads included), and deleteErr
// models a reachable keyring whose delete genuinely fails.
type fakeKeyring struct {
	key         string
	unavailable bool
	deleteErr   error
}

var errFakeKeyringDown = errors.New("no keyring daemon on this bus")

func (f *fakeKeyring) Name() string { return "OS keyring" }
func (f *fakeKeyring) Save(key string) error {
	if f.unavailable {
		return errFakeKeyringDown
	}
	f.key = key
	return nil
}
func (f *fakeKeyring) Load() (string, error) {
	if f.unavailable {
		return "", errFakeKeyringDown
	}
	if f.key == "" {
		return "", fmt.Errorf("%w: nothing stored", auth.ErrNoCredential)
	}
	return f.key, nil
}
func (f *fakeKeyring) Delete() (bool, error) {
	if f.unavailable {
		return false, errFakeKeyringDown
	}
	if f.deleteErr != nil {
		return false, f.deleteErr
	}
	if f.key == "" {
		return false, nil
	}
	f.key = ""
	return true, nil
}

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
	// keyring is the injected OS-keyring stand-in; nil means an empty,
	// available fake. The file side of the chain is always the real file
	// store rooted at configDir.
	keyring *fakeKeyring
	// browser is the injected launcher recorder; nil means a fresh recorder
	// (pass one to assert on what was — or was not — opened).
	browser *fakeBrowser
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

	kr := o.keyring
	if kr == nil {
		kr = &fakeKeyring{}
	}
	browser := o.browser
	if browser == nil {
		browser = &fakeBrowser{}
	}

	root, opts := newRoot("test", streams, func(g *globalOpts) {
		g.lookupEnv = func(key string) (string, bool) {
			v, ok := env[key]
			return v, ok
		}
		g.configDir = configDir
		g.now = func() time.Time { return fixedNow }
		g.newRequestID = func() string { return "cli-req-fixed" }
		g.newCredentialStore = func(dir string, onFileRead func()) *auth.Chain {
			return auth.NewChain(kr, auth.NewFileStore(dir), onFileRead)
		}
		g.openBrowser = browser.open
		if o.sleeps != nil {
			g.sleep = func(d time.Duration) { *o.sleeps = append(*o.sleeps, d) }
		} else {
			g.sleep = func(time.Duration) {} // tests never wall-clock sleep
		}
	})
	root.SetArgs(o.args)
	res.code = run(context.Background(), root, opts)
	return res
}

// mustEmptyStdout asserts the data stream carried nothing — the fail-closed
// contract for verification failures and errors.
func mustEmptyStdout(t *testing.T, res *runResult) {
	t.Helper()
	if res.stdout.Len() != 0 {
		t.Fatalf("stdout must be empty, got %q", res.stdout.String())
	}
}
