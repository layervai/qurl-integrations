//go:build !windows

package daemon

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

func TestIPCServerReadinessReloadAndShutdown(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{}}
	factory := &fakeFactory{sessions: map[string][]*fakeSession{}, err: map[string]error{}}
	manager, _ := NewManager(registry, factory)
	dir, err := os.MkdirTemp("/tmp", "qurl-daemon-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	// Exercise the derived runtime path, not only its string contract: this
	// state namespace is intentionally too long for sockaddr_un.
	path := StateSocketPath(filepath.Join(dir, strings.Repeat("state-segment-", 8)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- (&IPCServer{SocketPath: path, Manager: manager, JobVersion: "1/test"}).Run(ctx) }()
	client := IPCClient{SocketPath: path}
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer readyCancel()
	if err := client.WaitReady(readyCtx); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("derived IPC directory mode = %v, %v; want owner-only 0700", info, err)
	}
	status, running, err := client.Status(context.Background())
	if err != nil || !running || status.JobVersion != "1/test" {
		t.Fatalf("status = %+v running=%v err=%v", status, running, err)
	}
	if running, err := client.ReloadIfRunning(context.Background()); err != nil || !running {
		t.Fatalf("reload running=%v err=%v", running, err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("server error = %v, want context cancellation", err)
	}
	if running, err := client.ReloadIfRunning(context.Background()); err != nil || running {
		t.Fatalf("post-shutdown reload running=%v err=%v", running, err)
	}
}

func TestStateSocketPathBoundsLongUnixStateDirectories(t *testing.T) {
	shortState := filepath.Join(unixIPCRuntimeRoot, "qurl-short-state")
	if got, want := StateSocketPath(shortState), filepath.Join(shortState, SocketFile); got != want {
		t.Fatalf("short state socket = %q, want %q", got, want)
	}

	longState := filepath.Join(unixIPCRuntimeRoot, strings.Repeat("long-state-segment-", 8))
	first := StateSocketPath(longState)
	if first != StateSocketPath(longState) {
		t.Fatal("long state socket path is not deterministic")
	}
	if !filepath.IsAbs(first) || len(first) > maxUnixSocketPathBytes || filepath.Base(first) == SocketFile {
		t.Fatalf("long state socket = %q, want bounded absolute derived path", first)
	}
	if first == StateSocketPath(longState+"-other") {
		t.Fatal("different long state namespaces share one socket path")
	}

	longRelative := strings.Repeat("relative-state-", 8)
	if got := StateSocketPath(longRelative); filepath.IsAbs(got) {
		t.Fatalf("invalid relative state path became valid IPC path %q", got)
	}
}

func TestIPCServerSecuresPermissiveSocketDirectory(t *testing.T) {
	dir := filepath.Join(shortTempDir(t), "state")
	if err := os.Mkdir(dir, 0o755); err != nil { // #nosec G301 -- test verifies permissive directories are tightened.
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // #nosec G302 -- test verifies permissive directories are tightened.
		t.Fatal(err)
	}
	manager := emptyManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (&IPCServer{SocketPath: filepath.Join(dir, SocketFile), Manager: manager}).Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want canceled after setup", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("socket directory mode = %#o, want 0700", info.Mode().Perm())
	}
}

func TestIPCServerRefusesSymlinkSocketDirectory(t *testing.T) {
	base := shortTempDir(t)
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "state")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	err := (&IPCServer{SocketPath: filepath.Join(link, SocketFile), Manager: emptyManager(t)}).Run(context.Background())
	if err == nil {
		t.Fatal("symlink socket directory was accepted")
	}
}

func TestIPCServerRefusesRegularFileAtSocketPath(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, SocketFile)
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := (&IPCServer{SocketPath: path, Manager: emptyManager(t)}).Run(context.Background())
	if err == nil {
		t.Fatal("regular file at socket path was removed or accepted")
	}
}

func TestIPCServerRefusesSecondLiveDaemon(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, SocketFile)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- (&IPCServer{SocketPath: path, Manager: emptyManager(t)}).Run(ctx) }()
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readyCancel()
	if err := (IPCClient{SocketPath: path}).WaitReady(readyCtx); err != nil {
		t.Fatal(err)
	}
	if err := (&IPCServer{SocketPath: path, Manager: emptyManager(t)}).Run(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Run() = %v, want ErrAlreadyRunning", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("first daemon shutdown = %v", err)
	}
}

func TestPrepareSocketOnlyRemovesExplicitlyRefusedSocket(t *testing.T) {
	oldDial := dialUnixSocket
	t.Cleanup(func() { dialUnixSocket = oldDial })
	makeSocket := func() string {
		t.Helper()
		path := filepath.Join(shortTempDir(t), SocketFile)
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		listener.SetUnlinkOnClose(false)
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		return path
	}

	refused := makeSocket()
	dialUnixSocket = func(string, time.Duration) (net.Conn, error) { return nil, syscall.ECONNREFUSED }
	if err := prepareSocket(refused); err != nil {
		t.Fatalf("refused stale socket: %v", err)
	}
	if _, err := os.Lstat(refused); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused stale socket still exists: %v", err)
	}

	for name, dialErr := range map[string]error{
		"timeout":    context.DeadlineExceeded,
		"permission": os.ErrPermission,
	} {
		t.Run(name, func(t *testing.T) {
			path := makeSocket()
			dialUnixSocket = func(string, time.Duration) (net.Conn, error) { return nil, dialErr }
			if err := prepareSocket(path); !errors.Is(err, ErrAlreadyRunning) {
				t.Fatalf("prepareSocket() = %v, want fail-closed ErrAlreadyRunning", err)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("ambiguous socket was unlinked: %v", err)
			}
		})
	}
}

func TestUnavailableSocketClassificationFailsClosedOnAmbiguity(t *testing.T) {
	for name, err := range map[string]error{
		"refused": syscall.ECONNREFUSED,
		"missing": syscall.ENOENT,
	} {
		t.Run(name, func(t *testing.T) {
			if !isUnavailableIPCError(&net.OpError{Op: "dial", Net: "unix", Err: err}) {
				t.Fatalf("%v was not classified unavailable", err)
			}
		})
	}
	for name, err := range map[string]error{
		"timeout":    context.DeadlineExceeded,
		"permission": os.ErrPermission,
	} {
		t.Run(name, func(t *testing.T) {
			if isUnavailableIPCError(&net.OpError{Op: "dial", Net: "unix", Err: err}) {
				t.Fatalf("ambiguous %v was classified as daemon absent", err)
			}
		})
	}
}

func TestWaitReadyReturnsAmbiguousProbeErrorImmediately(t *testing.T) {
	oldProbe := probeIPCStatus
	t.Cleanup(func() { probeIPCStatus = oldProbe })
	want := &net.OpError{Op: "dial", Net: "unix", Err: os.ErrPermission}
	calls := 0
	probeIPCStatus = func(IPCClient, context.Context) (*http.Response, bool, error) {
		calls++
		return nil, true, want
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := (IPCClient{SocketPath: "/tmp/qurl-ipc-test.sock"}).WaitReady(ctx); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("WaitReady() = %v, want permission error", err)
	}
	if calls != 1 {
		t.Fatalf("ambiguous IPC probes = %d, want one", calls)
	}
}

func TestWaitReadyClosesNonSuccessProbeBodies(t *testing.T) {
	oldProbe := probeIPCStatus
	t.Cleanup(func() { probeIPCStatus = oldProbe })
	closed := make(chan struct{})
	probeIPCStatus = func(IPCClient, context.Context) (*http.Response, bool, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       closeTracker{Reader: strings.NewReader("starting"), onClose: func() { close(closed) }},
		}, true, nil
	}
	err := (IPCClient{SocketPath: "/tmp/qurl-ipc-test.sock"}).WaitReady(context.Background())
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("WaitReady() = %v, want immediate HTTP 503 error", err)
	}
	select {
	case <-closed:
	default:
		t.Fatal("WaitReady did not close non-success response body")
	}
}

func TestDecodeIPCStatusRejectsAmbiguousShapes(t *testing.T) {
	for name, input := range map[string]string{
		"empty version":     `{"job_version":"","running":{}}`,
		"missing map":       `{"job_version":"1/test"}`,
		"unknown field":     `{"job_version":"1/test","running":{},"extra":true}`,
		"trailing value":    `{"job_version":"1/test","running":{}} {}`,
		"blank resource id": `{"job_version":"1/test","running":{"":"crid"}}`,
		"blank crid":        `{"job_version":"1/test","running":{"resource":""}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeIPCStatus(strings.NewReader(input)); err == nil {
				t.Fatalf("decodeIPCStatus(%s) succeeded", input)
			}
		})
	}
	got, err := decodeIPCStatus(strings.NewReader(`{"job_version":"1/test","running":{"resource":"crid"}}`))
	if err != nil || got.JobVersion != "1/test" || got.Running["resource"] != "crid" {
		t.Fatalf("valid status = %+v, %v", got, err)
	}
}

type closeTracker struct {
	io.Reader
	onClose func()
}

func (c closeTracker) Close() error {
	c.onClose()
	return nil
}

func emptyManager(t *testing.T) *Manager {
	t.Helper()
	manager, err := NewManager(
		&memoryRegistry{shares: map[string]connectorstate.LocalShare{}},
		&fakeFactory{sessions: map[string][]*fakeSession{}, err: map[string]error{}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}
