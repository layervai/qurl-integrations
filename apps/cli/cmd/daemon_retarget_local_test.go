//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

func TestDaemonRetargetLocalNeedsNoRESTAndRefusesLiveDaemon(t *testing.T) {
	ctx := context.Background()
	dir := connectorStateTestDir(t)
	if err := connectorstate.EstablishExternalRuntimeMode(ctx, dir); err != nil {
		t.Fatal(err)
	}
	registry, err := openOwnedTestShareRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	row := localShareFixture(apitest.NewServer(t))
	row.ConnectorID = "qurl-file-test"
	if err := registry.Put(ctx, &row); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, connectorstate.LocalSharesFile)
	before, err := os.ReadFile(path) // #nosec G304 -- private test registry fixture.
	if err != nil {
		t.Fatal(err)
	}
	apiCalls := 0
	invoke := func() *runResult {
		return runCLI(t, &runOpts{
			args: []string{"daemon", "retarget-local", "--supervision", "external", "-o", "json"},
			env:  map[string]string{}, shareStateDir: dir,
			stdin: strings.NewReader(`{"owner_id":"own_cli_fixture","connector_id_prefix":"qurl-file-","target":"http+unix:///tmp/private.sock"}`),
			openAPIClient: func(context.Context) (qurlapi.Client, error) {
				apiCalls++
				return nil, errors.New("revoked credential must not block local conversion")
			},
		})
	}
	socket := stateSocketPath(t, dir)
	if err := connectordaemon.EnsureIPCDir(filepath.Dir(socket)); err != nil {
		t.Fatal(err)
	}
	var listen net.ListenConfig
	live, err := listen.Listen(ctx, "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	blocked := invoke()
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	if blocked.code == 0 {
		t.Fatal("conversion proceeded while a daemon IPC listener was live")
	}
	current, err := os.ReadFile(path) // #nosec G304 -- private test registry fixture.
	if err != nil || !bytes.Equal(current, before) {
		t.Fatal("blocked conversion changed local shares")
	}
	converted := invoke()
	var receipt struct {
		Changed int `json:"changed"`
	}
	if converted.code != 0 || json.Unmarshal(converted.stdout.Bytes(), &receipt) != nil || receipt.Changed != 1 {
		t.Fatalf("local conversion failed: exit=%d stderr=%s stdout=%s", converted.code, converted.stderr.String(), converted.stdout.String())
	}
	if apiCalls != 0 {
		t.Fatal("local conversion constructed a REST client")
	}
	after, err := registry.Get(ctx, row.CRID)
	if err != nil || after.CRID != row.CRID || after.ServingEpoch != row.ServingEpoch || after.DesiredState != row.DesiredState || after.LocalSocketPath != "/tmp/private.sock" {
		t.Fatalf("conversion did not preserve the share: %v", err)
	}
	if _, err := os.Stat(socket); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("offline conversion left a daemon socket behind")
	}
}

func TestPrivateOriginPreflightUsesOnlyUnixSocket(t *testing.T) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("/tmp", "qp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "origin.sock")
	target, err := connectorstate.ParseUnixTarget("http+unix://" + socket)
	if err != nil {
		t.Fatal(err)
	}
	var listen net.ListenConfig
	origin, err := listen.Listen(ctx, "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = origin.Close() })
	opts := &globalOpts{preflightTarget: func(context.Context, string, int) error {
		t.Fatal("Unix origin attempted TCP preflight")
		return nil
	}}
	if err := preflightShareTarget(ctx, opts, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // #nosec G302 -- deliberately unsafe fixture directory.
		t.Fatal(err)
	}
	if err := preflightShareTarget(ctx, opts, target); err == nil || strings.Contains(err.Error(), dir) {
		t.Fatal("origin outside an owner-only directory must fail without disclosing its path")
	}
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- directories need the owner execute bit.
		t.Fatal(err)
	}
	if err := origin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := preflightShareTarget(ctx, opts, target); err == nil || strings.Contains(err.Error(), dir) {
		t.Fatal("missing private origin must fail without disclosing its path")
	}
	target.IP, target.Port = "127.0.0.1", 3000
	if err := preflightShareTarget(ctx, opts, target); err == nil {
		t.Fatal("mixed Unix/TCP origin accepted")
	}
}

func TestDaemonRetargetLocalRejectsInvalidInputsAndNativeNamespace(t *testing.T) {
	for _, input := range []string{
		`{"owner_id":"own_cli_fixture","connector_id_prefix":"qurl-file-","target":"http+unix:///tmp/private.sock"}`,
		`{"target":"http://127.0.0.1:3000"}`,
		`{"target":"http+unix:///tmp/private.sock","extra":true}`,
		`{"target":"http+unix:///tmp/private.sock"} {}`,
		strings.Repeat(" ", 4097),
	} {
		dir := connectorStateTestDir(t)
		result := runCLI(t, &runOpts{args: []string{"daemon", "retarget-local", "--supervision", "external", "-o", "json"}, env: map[string]string{}, shareStateDir: dir, stdin: strings.NewReader(input)})
		if result.code == 0 {
			t.Fatal("invalid input or unmarked namespace accepted")
		}
		if _, err := os.Stat(filepath.Join(dir, connectorstate.LocalSharesFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("rejected command mutated the registry")
		}
	}
}

func TestDaemonRunLeaseContentionPreservesConflictAndCallerCancellation(t *testing.T) {
	dir := connectorStateTestDir(t)
	unlock, err := connectorstate.AcquireDaemonLease(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := unlock(); err != nil {
			t.Error(err)
		}
	}()
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		want    int
	}{
		{"another daemon", 0, exitcode.Conflict},
		{"caller deadline", time.Millisecond, exitcode.Unavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.timeout != 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.timeout)
				defer cancel()
			}
			res := runCLI(t, &runOpts{ctx: ctx, args: []string{"daemon", "run", "--supervision", "external"}, shareStateDir: dir})
			if res.code != tc.want {
				t.Fatalf("exit=%d want=%d: %s", res.code, tc.want, res.stderr.String())
			}
		})
	}
}
