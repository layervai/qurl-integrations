//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

// TestRunExplainsConnectorUnsafeDirectory pins the one call site that joins
// the connector's text-only refusal to its rendering and exit code: a command
// returning the real refusal must exit Config with the chmod remedy.
func TestRunExplainsConnectorUnsafeDirectory(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o700); err != nil { //nolint:gosec // a directory, which needs its search bit.
		t.Fatal(err)
	}
	loose := filepath.Join(base, "loose")
	if err := os.Mkdir(loose, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(loose, 0o775); err != nil { //nolint:gosec // intentionally group-writable to exercise the refusal.
		t.Fatal(err)
	}
	stateDir := filepath.Join(loose, "state")
	t.Setenv("QURL_CONNECTOR_STATE_DIR", stateDir)

	// No real config directory or ambient QURL_* environment, like runCLI.
	// The state override stays in the process environment above because
	// state.ResolveDir reads it there.
	for name, apiErr := range map[string]*qurlapi.Error{
		"local refusal":                    nil,
		"unrelated API error joined on it": {StatusCode: 500, Title: "internal error"},
	} {
		t.Run(name, func(t *testing.T) { runUnsafeDirectoryCommand(t, stateDir, loose, apiErr) })
	}
}

// runUnsafeDirectoryCommand runs a command returning the real connector
// refusal, optionally joined with a failed compensating API call, and
// asserts the local remedy and exit code survive.
func runUnsafeDirectoryCommand(t *testing.T, stateDir, loose string, apiErr *qurlapi.Error) {
	t.Helper()
	var stderr bytes.Buffer
	root, opts := newRoot("test", &output.Streams{In: strings.NewReader(""), Out: &bytes.Buffer{}, Err: &stderr}, func(g *globalOpts) {
		g.configDir = t.TempDir()
		g.lookupEnv = func(key string) (string, bool) {
			if key == "QURL_CONNECTOR_STATE_DIR" {
				return stateDir, true
			}
			return "", false
		}
	})
	root.AddCommand(&cobra.Command{
		Use:    "open-state",
		Hidden: true,
		RunE: func(*cobra.Command, []string) error {
			err := connectoragentstate.ValidateSDKStoreLayout(stateDir)
			if apiErr != nil {
				err = errors.Join(err, fmt.Errorf("turn sharing off: %w", apiErr))
			}
			return err
		},
	})
	root.SetArgs([]string{"open-state"})
	if code := run(context.Background(), root, opts); code != 3 {
		t.Fatalf("exit = %d, want 3 (Config)\n%s", code, stderr.String())
	}
	for _, want := range []string{"chmod go-w " + loose, "QURL_CONNECTOR_STATE_DIR"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr lacks %q:\n%s", want, stderr.String())
		}
	}
	if strings.Contains(stderr.String(), "unsafe mode") {
		t.Fatalf("stderr kept the raw connector text:\n%s", stderr.String())
	}
}
