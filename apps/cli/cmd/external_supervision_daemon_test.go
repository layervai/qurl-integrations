//go:build !windows

package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/fatedier/frp/pkg/config/v1"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

// daemonRunHubArgs pins the daemon's Hub through the same hidden override the
// per-user job definition uses, so an in-process `daemon run` never consults
// the host environment. The key is a shape-valid test fixture.
var daemonRunHubArgs = []string{
	"--hub-host", "hub.nhp.layerv.ai", "--hub-port", "443",
	"--hub-server-public-key-b64", "CQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
}

// runDaemonUntilReady runs `daemon run` with args in-process, waits until the
// daemon answers /status, hands that status back, and stops the daemon the
// way a supervisor would: by canceling it.
func runDaemonUntilReady(t *testing.T, stateDir string, args ...string) (*runResult, connectordaemon.IPCStatus) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socketPath, err := connectordaemon.SocketPathForStateDir(stateDir, nil)
	if err != nil {
		t.Fatalf("resolve daemon socket: %v", err)
	}
	client := connectordaemon.IPCClient{SocketPath: socketPath}
	statuses := make(chan connectordaemon.IPCStatus, 1)
	go func() {
		defer cancel()
		readyCtx, cancelReady := context.WithTimeout(ctx, 5*time.Second)
		defer cancelReady()
		if err := client.WaitReady(readyCtx); err != nil {
			return
		}
		status, running, err := client.Status(readyCtx)
		if err != nil || !running {
			return
		}
		statuses <- status
	}()
	res := runCLI(t, &runOpts{args: append(append([]string{"daemon", "run"}, daemonRunHubArgs...), args...), ctx: ctx, shareStateDir: stateDir})
	select {
	case status := <-statuses:
		return res, status
	default:
		t.Fatalf("daemon never answered /status: exit %d stderr %s", res.code, res.stderr.String())
		return nil, connectordaemon.IPCStatus{}
	}
}

// TestDaemonRunExternalEstablishesMarker pins the external start-up contract
// on a fresh namespace: the first `daemon run --supervision external` commits
// the immutable policy marker before anything else, and every later start
// accepts it, serves /status with this process's pid, and stops on its
// supervisor's cancellation.
func TestDaemonRunExternalEstablishesMarker(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "external")
	markerPath := filepath.Join(stateDir, connectorstate.RuntimeModeFile)

	first := runCLI(t, &runOpts{
		args:          append([]string{"daemon", "run", "--supervision", "external", "--state-dir", stateDir}, daemonRunHubArgs...),
		shareStateDir: stateDir,
	})
	data, err := os.ReadFile(markerPath) // #nosec G304 -- test-owned state directory.
	if err != nil {
		t.Fatalf("fresh external start left no policy marker (exit %d stderr %s): %v", first.code, first.stderr.String(), err)
	}
	if got, want := string(data), `{"schema_version":1,"supervision":"external"}`; got != want {
		t.Fatalf("runtime policy = %q, want exact %q", got, want)
	}
	// Enrollment (a later task's `login --supervision external`) is what binds
	// the owner; without it the daemon has nothing to serve and says so.
	if first.code != 1 || !strings.Contains(first.stderr.String(), "no durable account owner") {
		t.Fatalf("fresh external daemon = exit %d stderr %q, want the missing-owner refusal", first.code, first.stderr.String())
	}
	before, err := os.Stat(markerPath)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := openOwnedTestShareRegistry(stateDir); err != nil {
		t.Fatal(err)
	}
	second, status := runDaemonUntilReady(t, stateDir, "--supervision", "external", "--state-dir", stateDir)
	if second.code != 130 {
		t.Fatalf("supervised daemon stop = exit %d stderr %s, want the cancellation exit", second.code, second.stderr.String())
	}
	wantJobVersion, err := connectordaemon.JobVersion("test", connectordaemon.GroupModeSingle)
	if err != nil {
		t.Fatal(err)
	}
	if status.JobVersion != wantJobVersion || status.Pid != os.Getpid() {
		t.Fatalf("external daemon status = %+v, want job version %s and pid %d", status, wantJobVersion, os.Getpid())
	}
	after, err := os.Stat(markerPath)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("a repeated external start replaced the immutable policy marker: %v", err)
	}
	if mode, err := connectorstate.ReadRuntimeSupervision(stateDir); err != nil || mode != connectorstate.RuntimeSupervisionExternal {
		t.Fatalf("namespace policy after external starts = (%q, %v), want external", mode, err)
	}
}

func TestDaemonRunNativeRefusesExternalNamespace(t *testing.T) {
	stateDir := connectorStateTestDir(t)
	if err := connectorstate.EstablishExternalRuntimeMode(context.Background(), stateDir); err != nil {
		t.Fatal(err)
	}
	if _, err := openOwnedTestShareRegistry(stateDir); err != nil {
		t.Fatal(err)
	}
	res := runCLI(t, &runOpts{
		args:          append([]string{"daemon", "run", "--state-dir", stateDir}, daemonRunHubArgs...),
		shareStateDir: stateDir,
	})
	const wantMessage = `runtime supervision is "external", not "native"; run this command with --supervision external`
	if res.code != 3 || !strings.Contains(res.stderr.String(), wantMessage) {
		t.Fatalf("native daemon over an external namespace = exit %d stderr %q, want exit 3 with %q", res.code, res.stderr.String(), wantMessage)
	}
}

func TestDaemonRunExternalRefusesNativeNamespace(t *testing.T) {
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	local := localShareFixture(apitest.NewServer(t))
	if err := registry.Put(context.Background(), &local); err != nil {
		t.Fatal(err)
	}
	res := runCLI(t, &runOpts{
		args:          append([]string{"daemon", "run", "--supervision", "external", "--state-dir", stateDir}, daemonRunHubArgs...),
		shareStateDir: stateDir,
	})
	if res.code == 0 || res.code == 130 || !strings.Contains(res.stderr.String(), "not a fresh external namespace") {
		t.Fatalf("external daemon over a native namespace = exit %d stderr %q, want a refusal to relabel it", res.code, res.stderr.String())
	}
	if _, err := os.Lstat(filepath.Join(stateDir, connectorstate.RuntimeModeFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused external start wrote a policy marker: %v", err)
	}
}

// TestDaemonRunShutdownLeavesCloudStateUntouched pins the contract a
// supervisor's "sharing off" relies on: stopping `daemon run` is a local act.
// Unlike `publish --foreground`, which owns its share and turns it off on
// exit, the daemon makes no sharing mutation when it is canceled, and the
// durable row it was serving stays desired-on for the next start.
func TestDaemonRunShutdownLeavesCloudStateUntouched(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	local := localShareFixture(srv)
	local.DesiredState = "on"
	if err := registry.Put(context.Background(), &local); err != nil {
		t.Fatal(err)
	}
	originalBuilder := buildNativeSessionFactory
	t.Cleanup(func() { buildNativeSessionFactory = originalBuilder })
	factory := &headlessTestFactory{started: make(chan struct{})}
	buildNativeSessionFactory = func(context.Context, connectorshare.NativeRuntimeConfig, *v1.ClientCommonConfig, *qurlapi.Config, bool, *connectorstate.LocalShare) (connectordaemon.GroupFactory, error) {
		return factory, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		defer cancel()
		select {
		case <-factory.started:
		case <-time.After(5 * time.Second):
		}
	}()
	res := runCLI(t, &runOpts{
		args:          append([]string{"--endpoint", srv.URL, "daemon", "run", "--state-dir", stateDir}, daemonRunHubArgs...),
		ctx:           ctx,
		shareStateDir: stateDir,
	})
	select {
	case <-factory.started:
	default:
		t.Fatalf("daemon never served the desired-on share: exit %d stderr %s", res.code, res.stderr.String())
	}
	if res.code != 130 {
		t.Fatalf("daemon stop = exit %d stderr %s, want the cancellation exit", res.code, res.stderr.String())
	}
	for _, request := range srv.Requests() {
		if (request.Method == http.MethodPut && strings.HasSuffix(request.Path, "/sharing")) ||
			(request.Method == http.MethodPost && strings.HasSuffix(request.Path, "/sharing/restart")) {
			t.Fatalf("daemon shutdown mutated cloud sharing state: %s %s", request.Method, request.Path)
		}
	}
	stored, err := registry.Get(context.Background(), local.ResourceID)
	if err != nil || stored.DesiredState != "on" || stored.ServingEpoch != local.ServingEpoch {
		t.Fatalf("local share after daemon shutdown = %+v err=%v, want it still desired-on at epoch %d", stored, err, local.ServingEpoch)
	}
}

// TestDaemonRunExternalStopDuringDeferredFirstReconcileTouchesNothing pins
// the window between a supervisor spawning the daemon and its first reload:
// a daemon stopped while its first reconcile is still deferred exits with the
// cancellation code having served nothing, made no cloud call, and left the
// desired-on row exactly as it found it.
func TestDaemonRunExternalStopDuringDeferredFirstReconcileTouchesNothing(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := connectorStateTestDir(t)
	if err := connectorstate.EstablishExternalRuntimeMode(context.Background(), stateDir); err != nil {
		t.Fatal(err)
	}
	registry, err := openOwnedTestShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	local := localShareFixture(srv)
	local.DesiredState = "on"
	if err := registry.Put(context.Background(), &local); err != nil {
		t.Fatal(err)
	}
	originalBuilder := buildNativeSessionFactory
	t.Cleanup(func() { buildNativeSessionFactory = originalBuilder })
	factory := &headlessTestFactory{started: make(chan struct{})}
	buildNativeSessionFactory = func(context.Context, connectorshare.NativeRuntimeConfig, *v1.ClientCommonConfig, *qurlapi.Config, bool, *connectorstate.LocalShare) (connectordaemon.GroupFactory, error) {
		return factory, nil
	}
	res, status := runDaemonUntilReady(t, stateDir, "--supervision", "external", "--state-dir", stateDir, "--endpoint", srv.URL)
	if res.code != 130 {
		t.Fatalf("deferred daemon stop = exit %d stderr %s, want the cancellation exit", res.code, res.stderr.String())
	}
	select {
	case <-factory.started:
		t.Fatal("the daemon served the desired-on share before its supervisor's first reload")
	default:
	}
	if len(status.Running) != 0 || len(status.Resources) != 0 {
		t.Fatalf("deferred daemon status = %+v, want no running or diagnosed resource", status)
	}
	if requests := srv.Requests(); len(requests) != 0 {
		t.Fatalf("deferred daemon made cloud requests: %+v", requests)
	}
	stored, err := registry.Get(context.Background(), local.ResourceID)
	if err != nil || stored.DesiredState != "on" || stored.ServingEpoch != local.ServingEpoch {
		t.Fatalf("local share after the deferred stop = %+v err=%v, want it still desired-on at epoch %d", stored, err, local.ServingEpoch)
	}
}
