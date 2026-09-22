//go:build windows

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

func privateTestTempRoot() string { return "" }
func privateCmdTarget(t *testing.T, dir string) connectorstate.LocalTarget {
	t.Helper()
	digest := sha256.Sum256([]byte(dir))
	target, err := connectorstate.ParsePipeTarget(fmt.Sprintf("http+npipe:///layerv-qurl-file-%x-%s", digest, strings.Repeat("b", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return target
}
func listenPrivateTestOrigin(target connectorstate.LocalTarget) (net.Listener, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sid := user.User.Sid.String()
	return winio.ListenPipe(target.PipeName, &winio.PipeConfig{SecurityDescriptor: fmt.Sprintf("O:%sG:%sD:P(A;;GA;;;%s)", sid, sid, sid)})
}
func assertPrivateOriginKilled(t *testing.T, state *os.ProcessState) {
	t.Helper()
	if !state.Exited() || state.Success() {
		t.Fatalf("origin was not terminated: %s", state)
	}
}
func TestWindowsPipePreflightNeverUsesTCP(t *testing.T) {
	target := privateCmdTarget(t, t.TempDir())
	parsed, err := classifyPublishTarget(target.URL)
	if err != nil || parsed.localTarget() != target {
		t.Fatalf("publish pipe mapping failed: %v", err)
	}
	opts := &globalOpts{preflightTarget: func(context.Context, string, int) error { t.Fatal("pipe attempted TCP"); return nil }}
	listener, err := listenPrivateTestOrigin(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
		}
	}()
	if err := preflightShareTarget(context.Background(), opts, target); err != nil {
		t.Fatal(err)
	}
	_ = listener.Close()
	if err := preflightShareTarget(context.Background(), opts, target); err == nil || strings.Contains(err.Error(), target.PipeName) {
		t.Fatal("missing pipe did not fail closed with redacted error")
	}
	target.IP, target.Port = "127.0.0.1", 3000
	if err := preflightShareTarget(context.Background(), opts, target); err == nil {
		t.Fatal("mixed pipe/TCP accepted")
	}
}

func TestWindowsRetargetLocalOfflineAndDaemonExclusion(t *testing.T) {
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
	target := privateCmdTarget(t, dir)
	input, err := json.Marshal(map[string]string{"owner_id": "own_cli_fixture", "connector_id_prefix": "qurl-file-", "target": target.URL})
	if err != nil {
		t.Fatal(err)
	}
	invoke := func() *runResult {
		bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return runCLI(t, &runOpts{ctx: bounded, args: []string{"daemon", "retarget-local", "--supervision", "external", "-o", "json"}, env: map[string]string{}, shareStateDir: dir, stdin: strings.NewReader(string(input)), openAPIClient: func(context.Context) (qurlapi.Client, error) {
			t.Error("offline retarget opened REST client")
			return nil, errors.New("unexpected REST")
		}})
	}
	if err := connectordaemon.WithStoppedDaemon(ctx, stateSocketPath(t, dir), func() error {
		if invoke().code == 0 {
			t.Error("live control pipe did not exclude retarget")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	unlock, err := connectorstate.AcquireDaemonLease(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if invoke().code == 0 {
		t.Error("live daemon lease did not exclude retarget")
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	result := invoke()
	var receipt struct {
		Changed int `json:"changed"`
	}
	if result.code != 0 || json.Unmarshal(result.stdout.Bytes(), &receipt) != nil || receipt.Changed != 1 {
		t.Fatalf("offline missing-pipe conversion: %d %s %s", result.code, result.stderr.String(), result.stdout.String())
	}
	stored, err := registry.Get(ctx, row.CRID)
	if err != nil || stored.Target() != target || stored.CRID != row.CRID || stored.ServingEpoch != row.ServingEpoch || stored.DesiredState != row.DesiredState {
		t.Fatalf("conversion changed authority: %v", err)
	}
}
