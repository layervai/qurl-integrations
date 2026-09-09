//go:build !windows

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	factory := newFakeGroupFactory()
	manager, _ := NewManager(registry, factory)
	dir, err := os.MkdirTemp("/tmp", "qurl-daemon-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("TMPDIR", dir)
	// Exercise the derived runtime path, not only its string contract: this
	// state namespace is intentionally too long for sockaddr_un.
	path, err := SocketPathForStateDir(filepath.Join(dir, strings.Repeat("state-segment-", 8)), nil)
	if err != nil {
		t.Fatal(err)
	}
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

func TestSocketPathBoundsLongUnixStateDirectories(t *testing.T) {
	root := shortTempRoot(t)
	shortState := filepath.Join(root, "qurl-short-state")
	if got, err := SocketPathForStateDir(shortState, nil); err != nil || got != filepath.Join(shortState, SocketFile) {
		t.Fatalf("short state socket = %q, %v; want %q", got, err, filepath.Join(shortState, SocketFile))
	}

	longState := filepath.Join(root, strings.Repeat("long-state-segment-", 8))
	first, err := SocketPathForStateDir(longState, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := SocketPathForStateDir(longState, nil); err != nil || again != first {
		t.Fatal("long state socket path is not deterministic")
	}
	if !filepath.IsAbs(first) || len(first) > maxUnixSocketPathBytes || filepath.Dir(first) == longState {
		t.Fatalf("long state socket = %q, want bounded absolute derived path", first)
	}
	if other, err := SocketPathForStateDir(longState+"-other", nil); err != nil || other == first {
		t.Fatal("different long state namespaces share one socket path")
	}

	longRelative := strings.Repeat("relative-state-", 8)
	if got, err := SocketPathForStateDir(longRelative, nil); err == nil || got != "" {
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

func TestIPCClientRefusesInsecureSocketDirectoryWithoutChangingIt(t *testing.T) {
	base := shortTempDir(t)
	dir := filepath.Join(base, "permissive")
	if err := os.Mkdir(dir, 0o755); err != nil { // #nosec G301 -- test creates an intentionally insecure directory.
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { // #nosec G302 -- test pins the intentionally insecure mode despite umask.
		t.Fatal(err)
	}
	err := (IPCClient{SocketPath: filepath.Join(dir, SocketFile)}).WaitReady(context.Background())
	if err == nil || !strings.Contains(err.Error(), "owner-owned non-symlink directory with mode 0700") {
		t.Fatalf("client accepted insecure socket directory: %v", err)
	}
	info, statErr := os.Lstat(dir)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("client changed insecure socket directory mode to %#o", info.Mode().Perm())
	}
}

func TestIPCClientRefusesSymlinkSocketDirectory(t *testing.T) {
	base := shortTempDir(t)
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "state")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	err := (IPCClient{SocketPath: filepath.Join(link, SocketFile)}).WaitReady(context.Background())
	if err == nil || !strings.Contains(err.Error(), "owner-owned non-symlink directory with mode 0700") {
		t.Fatalf("client accepted symlink socket directory: %v", err)
	}
}

func TestIPCClientRefusesPermissiveSocket(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, SocketFile)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(path)
	})
	if err := os.Chmod(path, 0o666); err != nil { // #nosec G302 -- test creates an intentionally insecure socket.
		t.Fatal(err)
	}
	conn, err := dialDaemonIPC(context.Background(), path)
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "owner-owned non-symlink socket with mode 0600") {
		t.Fatalf("client accepted permissive socket: %v", err)
	}
}

func TestIPCClientBoundsUnresponsiveDaemon(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, SocketFile)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	accepted := make(chan struct{})
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		close(accepted)
		<-release
		_ = conn.Close()
	}()
	t.Cleanup(func() {
		close(release)
		_ = listener.Close()
		_ = os.Remove(path)
	})

	client := IPCClient{SocketPath: path, requestTimeout: 25 * time.Millisecond}
	_, running, err := client.Status(context.Background())
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Status() err = %v, want bounded deadline", err)
	}
	if !running {
		t.Fatal("unresponsive verified daemon was reported absent")
	}
	select {
	case <-accepted:
	default:
		t.Fatal("timeout occurred before the client connected to the daemon")
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

func TestWaitReadyRetriesOnlyPendingSocketRestriction(t *testing.T) {
	oldProbe := probeIPCStatus
	t.Cleanup(func() { probeIPCStatus = oldProbe })
	calls := 0
	probeIPCStatus = func(IPCClient, context.Context) (*http.Response, bool, error) {
		calls++
		if calls == 1 {
			return nil, true, errIPCSocketRestrictionPending
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"job_version":"test","running":{}}`)),
		}, true, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := (IPCClient{SocketPath: "/tmp/qurl-ipc-test.sock"}).WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady() = %v, want startup restriction retry", err)
	}
	if calls != 2 {
		t.Fatalf("startup restriction probes = %d, want two", calls)
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
		"empty version":     `{"job_version":"","running":{},"resources":{}}`,
		"missing map":       `{"job_version":"1/test"}`,
		"missing resources": `{"job_version":"1/test","running":{}}`,
		"unknown field":     `{"job_version":"1/test","running":{},"resources":{},"extra":true}`,
		"trailing value":    `{"job_version":"1/test","running":{},"resources":{}} {}`,
		"blank resource id": `{"job_version":"1/test","running":{"":"crid"},"resources":{}}`,
		"blank crid":        `{"job_version":"1/test","running":{"resource":""},"resources":{}}`,
		"negative pid":      `{"job_version":"1/test","pid":-1,"running":{},"resources":{}}`,
		"unsafe category":   `{"job_version":"1/test","running":{},"resources":{"resource":{"state":"failed","last_transition":"2026-08-30T15:00:00Z","failure_category":"internal_topology","retry_attempt":0}}}`,
		"unsafe code":       `{"job_version":"1/test","running":{},"resources":{"resource":{"state":"failed","last_transition":"2026-08-30T15:00:00Z","failure_category":"platform_denied","failure_code":"secret","retry_attempt":0}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeIPCStatus(strings.NewReader(input)); err == nil {
				t.Fatalf("decodeIPCStatus(%s) succeeded", input)
			}
		})
	}
	got, err := decodeIPCStatus(strings.NewReader(`{"job_version":"1/test","pid":4242,"running":{"resource":"crid"},"resources":{"resource":{"state":"retrying","last_transition":"2026-08-30T15:00:00Z","failure_category":"platform_denied","failure_code":"52005","retry_attempt":2,"next_retry_at":"2026-08-30T15:00:02Z"}}}`))
	if err != nil || got.JobVersion != "1/test" || got.Pid != 4242 || got.Running["resource"] != "crid" ||
		got.Resources["resource"].FailureCode != "52005" {
		t.Fatalf("valid status = %+v, %v", got, err)
	}
}

func TestValidDiagnosticCategoryAcceptsPublicFailureClasses(t *testing.T) {
	for _, category := range []string{diagnosticFailureEnrollment, diagnosticFailurePeerTimeout} {
		if !validDiagnosticCategory(category) {
			t.Errorf("public diagnostic category %q was rejected", category)
		}
	}
}

// rawIPCRequest sends body verbatim and returns the daemon's status and text.
func rawIPCRequest(t *testing.T, client IPCClient, method, path string, body []byte) (status int, text string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, running, err := client.do(ctx, method, path, body)
	if err != nil || !running {
		t.Fatalf("%s %s running=%v err=%v", method, path, running, err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(raw)
}

func overlayJSON(t *testing.T, overlay map[string]map[string]string) []byte {
	t.Helper()
	body, err := json.Marshal(ipcOverlay{RouteRequestHeaders: overlay})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func storedOverlay(manager *Manager) map[string]map[string]string {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.overlay
}

// TestOverlayIPCRejectsOversizedAndUnknownFields pins the PUT /overlay
// contract: every malformed, oversized, or over-limit body is refused with 400
// and fixed text — the request's header names and values reach neither the
// response nor the daemon log — and nothing is stored; a body within every
// limit is accepted with 204 and replaces the whole overlay. Not parallel: it
// captures the process-global slog default.
func TestOverlayIPCRejectsOversizedAndUnknownFields(t *testing.T) {
	var logs lockedLogBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	manager := emptyManager(t)
	path := filepath.Join(shortTempDir(t), SocketFile)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- (&IPCServer{SocketPath: path, Manager: manager, JobVersion: "1/test"}).Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	client := IPCClient{SocketPath: path}
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readyCancel()
	if err := client.WaitReady(readyCtx); err != nil {
		t.Fatal(err)
	}

	const secretName, secretValue = "X-Sekrit-Name", "sekrit-value"
	tooMany := map[string]string{}
	for i := range 17 {
		tooMany[fmt.Sprintf("%s-%d", secretName, i)] = secretValue
	}
	var oversized strings.Builder
	oversized.WriteString(`{"route_request_headers":{`)
	for i := 0; oversized.Len() <= maxIPCOverlayBytes; i++ {
		if i > 0 {
			oversized.WriteByte(',')
		}
		fmt.Fprintf(&oversized, `"r%d":{"%s":"%s"}`, i, secretName, secretValue)
	}
	oversized.WriteString(`}}`)
	rejected := map[string][]byte{
		"too many headers":   overlayJSON(t, map[string]map[string]string{"r1": tooMany}),
		"too many bytes":     overlayJSON(t, map[string]map[string]string{"r1": {secretName: strings.Repeat("v", 1025-len(secretName))}}),
		"invalid name":       []byte(`{"route_request_headers":{"r1":{"X-Sekrit Name":"sekrit-value"}}}`),
		"reserved name":      []byte(`{"route_request_headers":{"r1":{"Host":"sekrit-value"}}}`),
		"control byte value": []byte(`{"route_request_headers":{"r1":{"X-Sekrit-Name":"sekrit\r\nvalue"}}}`),
		"duplicate name":     []byte(`{"route_request_headers":{"r1":{"X-Sekrit-Name":"a","x-sekrit-name":"b"}}}`),
		"unknown field":      []byte(`{"route_request_headers":{},"sekrit_field":"sekrit-value"}`),
		"blank connector id": []byte(`{"route_request_headers":{"":{"X-Sekrit-Name":"sekrit-value"}}}`),
		"padded connector":   []byte(`{"route_request_headers":{" r1":{"X-Sekrit-Name":"sekrit-value"}}}`),
		"non-string value":   []byte(`{"route_request_headers":{"r1":{"X-Sekrit-Name":1}}}`),
		"missing field":      []byte(`{}`),
		"null field":         []byte(`{"route_request_headers":null}`),
		"not an object":      []byte(`[]`),
		"trailing value":     []byte(`{"route_request_headers":{}} {}`),
		"oversized body":     []byte(oversized.String()),
		"non-ascii name":     []byte(`{"route_request_headers":{"r1":{"X-Sekrit-é":"sekrit-value"}}}`),
		"array route":        []byte(`{"route_request_headers":{"r1":["X-Sekrit-Name","sekrit-value"]}}`),
	}
	for name, body := range rejected {
		t.Run(name, func(t *testing.T) {
			status, text := rawIPCRequest(t, client, http.MethodPut, "/overlay", body)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", status)
			}
			if !strings.HasPrefix(text, ipcOverlayRejected) {
				t.Fatalf("rejection text = %q, want the fixed message", text)
			}
			if strings.Contains(strings.ToLower(text), "sekrit") || strings.Contains(strings.ToLower(logs.String()), "sekrit") {
				t.Fatal("rejection echoed the request in the response or the daemon log")
			}
			if got := storedOverlay(manager); len(got) != 0 {
				t.Fatalf("rejected overlay left %d routes stored", len(got))
			}
		})
	}
	if !strings.Contains(logs.String(), "rejected a runtime overlay update") {
		t.Fatalf("daemon log %q does not record the rejections", logs.String())
	}

	// The limits are inclusive: 16 headers and 1,024 aggregate bytes pass.
	atLimit := map[string]string{}
	for i := range 15 {
		atLimit[fmt.Sprintf("X-%02d", i)] = ""
	}
	atLimit["X-Last"] = strings.Repeat("v", 1024-15*4-len("X-Last"))
	if status, _ := rawIPCRequest(t, client, http.MethodPut, "/overlay", overlayJSON(t, map[string]map[string]string{"r1": atLimit})); status != http.StatusNoContent {
		t.Fatalf("overlay at the limits returned %d, want 204", status)
	}
	if got := storedOverlay(manager)["r1"]; len(got) != 16 {
		t.Fatalf("stored %d headers for r1, want the 16 at the limit", len(got))
	}
	// Shapes a supervisor may legitimately send: a null or empty route entry is
	// a headerless route and is not stored, and a route key repeated in one
	// body follows JSON's last-wins rule rather than merging the two sets.
	for name, body := range map[string][]byte{
		"null route":  []byte(`{"route_request_headers":{"r1":null}}`),
		"empty route": []byte(`{"route_request_headers":{"r1":{}}}`),
	} {
		if status, _ := rawIPCRequest(t, client, http.MethodPut, "/overlay", body); status != http.StatusNoContent {
			t.Fatalf("%s returned %d, want 204", name, status)
		}
		if got := storedOverlay(manager); len(got) != 0 {
			t.Fatalf("%s stored %d routes, want a headerless route dropped", name, len(got))
		}
	}
	if status, _ := rawIPCRequest(t, client, http.MethodPut, "/overlay", []byte(`{"route_request_headers":{"r1":{"X-First":"1"},"r1":{"X-Last":"2"}}}`)); status != http.StatusNoContent {
		t.Fatalf("duplicate route key returned %d, want 204", status)
	}
	if got := storedOverlay(manager)["r1"]; len(got) != 1 || got["X-Last"] != "2" {
		t.Fatalf("duplicate route key stored %v, want only the last entry", got)
	}
	if status, _ := rawIPCRequest(t, client, http.MethodPost, "/overlay", overlayJSON(t, nil)); status != http.StatusMethodNotAllowed {
		t.Fatalf("POST /overlay returned %d, want 405", status)
	}

	// The client wrapper: a replacement drops every route it does not name, a
	// rejection surfaces as an error without the headers, and clearing works.
	overlayCtx, overlayCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer overlayCancel()
	if running, err := client.SetOverlay(overlayCtx, map[string]map[string]string{"r2": {"X-Token": "t"}}); err != nil || !running {
		t.Fatalf("SetOverlay running=%v err=%v", running, err)
	}
	if got := storedOverlay(manager); len(got) != 1 || got["r2"]["X-Token"] != "t" {
		t.Fatalf("stored overlay names %d routes, want only r2 after the replacement", len(got))
	}
	running, err := client.SetOverlay(overlayCtx, map[string]map[string]string{"r2": {"Host": secretValue}})
	if err == nil || !running || !strings.Contains(err.Error(), "HTTP 400") || strings.Contains(err.Error(), secretValue) {
		t.Fatalf("SetOverlay with a reserved header = running %v, %v; want the daemon running with an HTTP 400 error without the value", running, err)
	}
	if got := storedOverlay(manager); len(got) != 1 || got["r2"]["X-Token"] != "t" {
		t.Fatal("a rejected replacement changed the stored overlay")
	}
	if running, err := client.SetOverlay(overlayCtx, nil); err != nil || !running {
		t.Fatalf("SetOverlay(nil) running=%v err=%v", running, err)
	}
	if got := storedOverlay(manager); len(got) != 0 {
		t.Fatalf("stored overlay after clearing names %d routes, want none", len(got))
	}
	if strings.Contains(strings.ToLower(logs.String()), "sekrit") {
		t.Fatal("the daemon log carries an overlay value")
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
		newFakeGroupFactory(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}
