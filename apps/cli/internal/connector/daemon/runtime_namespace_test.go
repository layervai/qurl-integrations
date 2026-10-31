//go:build !windows

package daemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	connectorservice "github.com/layervai/qurl-connector/pkg/service"

	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

func TestRuntimeRecoveryAndNamespaceIsolation(t *testing.T) {
	root := shortTempRoot(t)
	stateDir := filepath.Join(root, "owner")
	runtimeDir := filepath.Join(root, "runtime")
	lookup := lookupEnvFrom(map[string]string{RuntimeDirEnv: runtimeDir})
	path, err := SocketPathForStateDir(stateDir, lookup)
	if err != nil {
		t.Fatal(err)
	}
	version, err := JobVersion("2.4.0", GroupModeSingle, "")
	if err != nil {
		t.Fatal(err)
	}
	manager := emptyManager(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- (&IPCServer{StateDir: stateDir, SocketPath: path, Manager: manager, JobVersion: version}).Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("daemon exit: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	owner := IPCClient{StateDir: stateDir, SocketPath: path}
	ready, stopReady := context.WithTimeout(t.Context(), 3*time.Second)
	defer stopReady()
	if err := owner.WaitReady(ready); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	aliasClient := IPCClient{StateDir: filepath.Join(alias, "owner"), SocketPath: path}
	if err := aliasClient.WaitReady(ready); err != nil {
		t.Fatalf("alias readiness: %v", err)
	}
	if _, err := aliasClient.ReloadIfRunning(ready); err != nil {
		t.Fatalf("alias reload: %v", err)
	}
	// A plain terminal recovers Desktop's bound address without its environment.
	controller, err := NewJobController(stateDir, filepath.Join(root, "logs"), "2.4.0", "https://api.sandbox.layerv.xyz", GroupModeSingle, connectorstate.RuntimeSupervisionNative, testHubResolver, nil)
	if err != nil {
		t.Fatal(err)
	}
	jobs := &recordingJobManager{}
	controller.Manager = jobs
	if controller.IPC.SocketPath != path {
		t.Fatal("terminal lost installed runtime directory")
	}
	if err := controller.Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(jobs.jobs)+len(jobs.replaced)+jobs.statusCalls != 0 {
		t.Fatal("terminal replaced compatible Desktop owner")
	}
	otherState := filepath.Join(root, "other")
	foreign := IPCClient{StateDir: otherState, SocketPath: path}
	if err := foreign.WaitReady(t.Context()); !errors.Is(err, ErrStateNamespaceMismatch) {
		t.Fatalf("foreign readiness = %v", err)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dialDaemonIPC(ctx, path) }}
	t.Cleanup(transport.CloseIdleConnections)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://qurl.local/reload", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(ipcNamespaceHeader, ipcNamespace(otherState))
	response, err := (&http.Client{Transport: transport, Timeout: time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("raw foreign reload = %d", response.StatusCode)
	}
	if _, running, err := foreign.Status(t.Context()); !running || !errors.Is(err, ErrStateNamespaceMismatch) {
		t.Fatalf("foreign status = %v, %v", running, err)
	}
	if _, err := foreign.ReloadIfRunning(t.Context()); !errors.Is(err, ErrStateNamespaceMismatch) {
		t.Fatalf("foreign reload = %v", err)
	}
	if _, err := foreign.SetOverlay(t.Context(), map[string]map[string]string{"connector": {"X-Test": "foreign"}}); !errors.Is(err, ErrStateNamespaceMismatch) {
		t.Fatalf("foreign overlay = %v", err)
	}
	if len(storedOverlay(manager)) != 0 {
		t.Fatal("foreign request changed owner overlay")
	}
	for _, mode := range []connectorstate.RuntimeSupervision{connectorstate.RuntimeSupervisionNative, connectorstate.RuntimeSupervisionExternal} {
		other, err := NewJobController(otherState, filepath.Join(root, "logs"), "2.4.0", "https://api.sandbox.layerv.xyz", GroupModeSingle, mode, testHubResolver, lookup)
		if err != nil {
			t.Fatal(err)
		}
		other.Manager = jobs
		if err := other.Ensure(t.Context()); !errors.Is(err, ErrStateNamespaceMismatch) {
			t.Fatalf("foreign Ensure (%s) = %v", mode, err)
		}
	}
	if len(jobs.jobs)+len(jobs.replaced)+jobs.statusCalls != 0 {
		t.Fatal("foreign command touched native ownership")
	}
	if _, err := os.Stat(otherState); !os.IsNotExist(err) {
		t.Fatalf("foreign command created state: %v", err)
	}
	// Old control clients and the strict status JSON shape stay compatible.
	if _, running, err := (IPCClient{SocketPath: path}).Status(t.Context()); err != nil || !running {
		t.Fatalf("legacy status = %v, %v", running, err)
	}
}

func TestLegacyPinnedOwnerCannotBeEvicted(t *testing.T) {
	root := shortTempRoot(t)
	stateDir := filepath.Join(root, "owner")
	if err := connectorstate.EnsureDirMode(stateDir); err != nil {
		t.Fatal(err)
	}
	lookup := lookupEnvFrom(map[string]string{RuntimeDirEnv: filepath.Join(root, "runtime")})
	path, err := SocketPathForStateDir(stateDir, lookup)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	manager := emptyManager(t)
	// An old daemon has no namespace handshake or persistent runtime hint.
	go func() { done <- (&IPCServer{SocketPath: path, Manager: manager, JobVersion: "6/2.4.0"}).Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("legacy daemon did not stop")
		}
	})
	ready, stopReady := context.WithTimeout(t.Context(), 3*time.Second)
	defer stopReady()
	if err := (IPCClient{SocketPath: path}).WaitReady(ready); err != nil {
		t.Fatal(err)
	}
	for _, env := range []func(string) (string, bool){nil, lookup} {
		controller, err := NewJobController(stateDir, filepath.Join(root, "logs"), "2.4.0", "https://api.sandbox.layerv.xyz", GroupModeSingle, connectorstate.RuntimeSupervisionNative, testHubResolver, env)
		if err != nil {
			t.Fatal(err)
		}
		jobs := &recordingJobManager{}
		controller.Manager = jobs
		if err := controller.Ensure(t.Context()); err == nil {
			t.Fatal("replaced or reloaded an unverified legacy owner")
		}
		if len(jobs.jobs)+len(jobs.replaced) != 0 {
			t.Fatal("legacy owner was evicted")
		}
	}
	client := IPCClient{StateDir: stateDir, SocketPath: path}
	if err := client.WaitReady(t.Context()); !errors.Is(err, ErrStateNamespaceUnverified) {
		t.Fatalf("legacy readiness = %v", err)
	}
	if _, err := client.ReloadIfRunning(t.Context()); !errors.Is(err, ErrStateNamespaceUnverified) {
		t.Fatalf("legacy reload = %v", err)
	}
	if _, err := client.SetOverlay(t.Context(), map[string]map[string]string{"connector": {"X-Test": "foreign"}}); !errors.Is(err, ErrStateNamespaceUnverified) {
		t.Fatalf("legacy overlay = %v", err)
	}
	if len(storedOverlay(manager)) != 0 {
		t.Fatal("legacy owner overlay changed")
	}
}

func TestNamespaceCanonicalizesAncestorAliasesBeforeCreation(t *testing.T) {
	root := shortTempRoot(t)
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	realState := filepath.Join(root, "owner")
	aliasState := filepath.Join(alias, "owner")
	before := ipcNamespace(aliasState)
	if err := connectorstate.EnsureDirMode(realState); err != nil {
		t.Fatal(err)
	}
	if before != ipcNamespace(realState) || before != ipcNamespace(aliasState) {
		t.Fatal("namespace changed after creation or through alias")
	}
}

func TestDaemonRepairsInvalidHintButClientFailsClosed(t *testing.T) {
	dir := shortTempRoot(t)
	marker := filepath.Join(dir, "daemon-runtime-dir.json")
	if err := os.WriteFile(marker, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SocketPathForStateDir(dir, nil); err == nil {
		t.Fatal("client silently fell back")
	}
	path, err := SocketPathForDaemon(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- RunRefusedJob(ctx, dir, path) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("repair daemon did not stop")
		}
	})
	ready, stop := context.WithTimeout(t.Context(), 3*time.Second)
	defer stop()
	if err := (IPCClient{StateDir: dir, SocketPath: path}).WaitReady(ready); !errors.Is(err, ErrJobRefused) {
		t.Fatal(err)
	}
	if recovered, err := SocketPathForStateDir(dir, nil); err != nil || recovered != path {
		t.Fatalf("repaired address = %q, %v", recovered, err)
	}
}

func TestStoppedLegacyPinnedOwnerCanBeReinstalled(t *testing.T) {
	root := shortTempRoot(t)
	dir := filepath.Join(root, "state")
	if err := connectorstate.EnsureDirMode(dir); err != nil {
		t.Fatal(err)
	}
	lookup := lookupEnvFrom(map[string]string{RuntimeDirEnv: filepath.Join(root, "runtime")})
	path, err := SocketPathForStateDir(dir, lookup)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	manager := emptyManager(t)
	go func() { done <- (&IPCServer{SocketPath: path, Manager: manager, JobVersion: "5/2.4.0"}).Run(ctx) }()
	stopped := false
	t.Cleanup(func() {
		cancel()
		if !stopped {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("legacy owner did not stop")
			}
		}
	})
	ready, stop := context.WithTimeout(t.Context(), 3*time.Second)
	defer stop()
	if err := (IPCClient{SocketPath: path}).WaitReady(ready); err != nil {
		t.Fatal(err)
	}
	controller, err := NewJobController(dir, filepath.Join(root, "logs"), "2.4.0", "https://api.sandbox.layerv.xyz", GroupModeSingle, connectorstate.RuntimeSupervisionNative, testHubResolver, lookup)
	if err != nil {
		t.Fatal(err)
	}
	jobs := &recordingJobManager{}
	controller.Manager = jobs
	if err := controller.Ensure(t.Context()); !errors.Is(err, ErrStateNamespaceUnverified) {
		t.Fatalf("live legacy owner: %v", err)
	}
	if len(jobs.jobs)+len(jobs.replaced)+jobs.statusCalls != 0 {
		t.Fatal("unverified legacy job was changed or inspected")
	}
	// The verified owning app/service manager stops its own process, not IPC.
	cancel()
	select {
	case <-done:
		stopped = true
	case <-time.After(3 * time.Second):
		t.Fatal("legacy owner did not stop")
	}
	jobs.status = &connectorservice.ServiceStatus{}
	if err := controller.Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(jobs.jobs) != 1 || len(jobs.replaced) != 0 || jobs.statusCalls != 1 {
		t.Fatalf("stopped legacy recovery: %+v", jobs)
	}
}

func TestPinnedMutationsNeedNamespaceButNotStatusSchema(t *testing.T) {
	root := shortTempRoot(t)
	stateDir := filepath.Join(root, "state")
	path := filepath.Join(root, "runtime", SocketFile)
	if err := EnsureIPCDir(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	listener, cleanup, err := listenDaemonIPC(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	var mutations atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"job_version":"6/future","pid":1,"running":{},"resources":{},"future_field":true}`))
			return
		}
		mutations.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	server := &http.Server{Handler: (&IPCServer{StateDir: stateDir}).controlHandler(handler), ReadHeaderTimeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = cleanup()
		select {
		case err := <-done:
			if !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("server: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("server did not stop")
		}
	})
	client := IPCClient{StateDir: stateDir, SocketPath: path}
	if _, _, err := client.Status(t.Context()); !errors.Is(err, errIPCStatusIncompatible) {
		t.Fatalf("strict status: %v", err)
	}
	if _, err := client.ReloadIfRunning(t.Context()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, err := client.SetOverlay(t.Context(), map[string]map[string]string{}); err != nil {
		t.Fatalf("overlay: %v", err)
	}
	foreign := IPCClient{StateDir: filepath.Join(root, "foreign"), SocketPath: path}
	if _, err := foreign.ReloadIfRunning(t.Context()); !errors.Is(err, ErrStateNamespaceMismatch) {
		t.Fatalf("foreign reload: %v", err)
	}
	if mutations.Load() != 2 {
		t.Fatalf("mutations = %d, want only the two owner mutations", mutations.Load())
	}
}
