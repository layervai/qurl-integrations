//go:build windows

package daemon

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

func TestWindowsIPCServerReadinessReloadSecondDaemonAndShutdown(t *testing.T) {
	dir := filepath.Join(shortTempDir(t), "state")
	path := filepath.Join(dir, SocketFile)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (&IPCServer{SocketPath: path, Manager: windowsEmptyManager(t), JobVersion: "1/windows-test"}).Run(ctx)
	}()

	client := IPCClient{SocketPath: path}
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer readyCancel()
	if err := client.WaitReady(readyCtx); err != nil {
		t.Fatal(err)
	}
	conn, err := dialDaemonIPC(readyCtx, path)
	if err != nil {
		t.Fatalf("dial owner-verified Windows daemon pipe: %v", err)
	}
	if err := validateWindowsDaemonPipeServer(conn); err != nil {
		_ = conn.Close()
		t.Fatalf("validate current-user Windows daemon pipe: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	status, running, err := client.Status(context.Background())
	if err != nil || !running || status.JobVersion != "1/windows-test" {
		t.Fatalf("status = %+v running=%v err=%v", status, running, err)
	}
	if err := (&IPCServer{SocketPath: path, Manager: windowsEmptyManager(t)}).Run(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Windows daemon Run() = %v, want ErrAlreadyRunning", err)
	}
	if running, err := client.ReloadIfRunning(context.Background()); err != nil || !running {
		t.Fatalf("reload running=%v err=%v", running, err)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("server shutdown = %v, want context cancellation", err)
	}
	if running, err := client.ReloadIfRunning(context.Background()); err != nil || running {
		t.Fatalf("post-shutdown reload running=%v err=%v", running, err)
	}
}

func TestWindowsDaemonPipeValidationFailsClosedWithoutHandle(t *testing.T) {
	left, right := net.Pipe()
	defer func() { _ = left.Close() }()
	defer func() { _ = right.Close() }()
	if err := validateWindowsDaemonPipeServer(left); err == nil || !strings.Contains(err.Error(), "valid Windows handle") {
		t.Fatalf("handle-free connection validation = %v, want fail-closed", err)
	}
}

func TestWindowsDaemonPipeNameIsCaseInsensitiveAndPathScoped(t *testing.T) {
	path := `C:\Users\Builder\AppData\Local\qurl\connector-v2\` + SocketFile
	if first, second := windowsDaemonPipeName(path), windowsDaemonPipeName(strings.ToUpper(path)); first != second {
		t.Fatalf("case-equivalent Windows paths mapped to different pipes: %q != %q", first, second)
	}
	if first, second := windowsDaemonPipeName(path), windowsDaemonPipeName(path+"-other"); first == second {
		t.Fatalf("different Windows paths mapped to the same pipe: %q", first)
	}
}

func TestWindowsIPCSecurityDescriptorAndUnavailableClassification(t *testing.T) {
	descriptor, err := currentWindowsIPCSecurityDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(descriptor, "D:P") || !strings.Contains(descriptor, ";;;SY)") || !strings.Contains(descriptor, ";;;BA)") {
		t.Fatalf("Windows IPC descriptor does not include the protected user/system/admin contract: %q", descriptor)
	}
	if _, err := winio.SddlToSecurityDescriptor(descriptor); err != nil {
		t.Fatalf("Windows IPC descriptor is invalid: %v", err)
	}
	if !isUnavailableIPCError(windows.ERROR_FILE_NOT_FOUND) || !isUnavailableIPCError(windows.ERROR_PATH_NOT_FOUND) {
		t.Fatal("missing Windows named-pipe errors were not classified unavailable")
	}
	if isUnavailableIPCError(windows.ERROR_ACCESS_DENIED) || isUnavailableIPCError(context.DeadlineExceeded) {
		t.Fatal("ambiguous Windows named-pipe failure was classified unavailable")
	}
}

func TestWindowsNamedPipeCollisionDoesNotHideAccessDenied(t *testing.T) {
	for _, collision := range []error{windows.STATUS_OBJECT_NAME_COLLISION, windows.ERROR_ALREADY_EXISTS, windows.ERROR_PIPE_BUSY} {
		if !windowsNamedPipeCollision(collision) {
			t.Fatalf("Windows named-pipe collision %v was not classified", collision)
		}
	}
	if windowsNamedPipeCollision(windows.ERROR_ACCESS_DENIED) {
		t.Fatal("Windows named-pipe access denial was hidden as an already-running daemon")
	}
}

func windowsEmptyManager(t *testing.T) *Manager {
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
