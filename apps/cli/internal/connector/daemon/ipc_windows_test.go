//go:build windows

package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

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

// TODO(upstream-contract): Desktop mirrors Go Unicode 15 simple lowercasing.
// Keep these wire hashes in sync with qurl-desktop cli-paths.test.ts; a Go
// Unicode upgrade must not silently change an existing profile's pipe name.
func TestWindowsDaemonPipeNameUnicodeWireContract(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		digest string
	}{
		{"TurkishAndGreek", `C:\Users\İpek\ΟΣ\state\daemon.sock`, "ff6fffc0cdd17367ee83b0e1d00ac7eec2d89519becdfa44666fac9b2829117b"},
		// These newer letters intentionally remain uncased under Go Unicode 15.
		{"NewerUnicodeLetters", `C:\Users\ᲉꟋꟌ꟎꟒꟔ꟚꟜ𐵐𖺠\state\daemon.sock`, "7efd5756e27a12a2e7e021cd9d71168692f408ed5ccba17aea161822650b9c25"},
		{"SharpSKelvinAndDeseret", `C:\Users\ẞK𐐀\state\daemon.sock`, "860493d1a89cc91ebbd7cd22f96be188cd6d1d109b322cea34268c13a8acb65d"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := `\\.\pipe\layerv-qurl-share-daemon-` + tc.digest
			if got := windowsDaemonPipeName(tc.path); got != want {
				t.Fatalf("Windows Unicode pipe contract changed: got %q, want %q", got, want)
			}
		})
	}
}

func TestWindowsDaemonPipeUnicodeScalarContract(t *testing.T) {
	digest := sha256.New()
	for code := rune(0); code <= unicode.MaxRune; code++ {
		if code >= 0xd800 && code <= 0xdfff {
			continue
		}
		_, _ = digest.Write([]byte(string(unicode.ToLower(code))))
	}
	// Mirrors Desktop's exhaustive scalar test, not the current Go table.
	const want = "137590953b837f1ec8b7c02b3a0425d0789df789a59ad256e7a63416f9fc4c11"
	if got := hex.EncodeToString(digest.Sum(nil)); got != want {
		t.Fatalf("Windows pipe Unicode casing contract changed: got %s, want %s", got, want)
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

func TestWindowsExistingPipeRequiresVerifiedCurrentUserDaemon(t *testing.T) {
	for _, listenErr := range []error{windows.ERROR_ACCESS_DENIED, windows.STATUS_OBJECT_NAME_COLLISION} {
		t.Run(listenErr.Error(), func(t *testing.T) {
			left, right := net.Pipe()
			defer func() { _ = right.Close() }()
			err := classifyWindowsExistingPipe(
				context.Background(),
				`C:\Users\Builder\qurl\`+SocketFile,
				listenErr,
				func(context.Context, string) (net.Conn, error) { return left, nil },
			)
			if !errors.Is(err, ErrAlreadyRunning) {
				t.Fatalf("verified current-user pipe = %v, want ErrAlreadyRunning", err)
			}

			verifyErr := errors.New("pipe owner could not be verified")
			err = classifyWindowsExistingPipe(
				context.Background(),
				`C:\Users\Builder\qurl\`+SocketFile,
				listenErr,
				func(context.Context, string) (net.Conn, error) { return nil, verifyErr },
			)
			if errors.Is(err, ErrAlreadyRunning) {
				t.Fatalf("unverified existing pipe was classified as already running: %v", err)
			}
			if !errors.Is(err, listenErr) || !errors.Is(err, verifyErr) {
				t.Fatalf("unverified existing pipe did not preserve both causes: %v", err)
			}
		})
	}
}

func windowsEmptyManager(t *testing.T) *Manager {
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

func TestWindowsSocketPathIgnoresRuntimeDirAndRejectsRelativeStateDir(t *testing.T) {
	stateDir := `C:\Users\Builder\AppData\Local\qurl\connector-v2`
	got, err := SocketPathForStateDir(stateDir, lookupEnvFrom(map[string]string{RuntimeDirEnv: `C:\rt`}))
	if err != nil || got != filepath.Join(stateDir, SocketFile) {
		t.Fatalf("got %q err %v, want the state-directory pipe path", got, err)
	}
	if got, err := SocketPathForStateDir(`relative\state`, nil); err == nil || got != "" {
		t.Fatalf("relative state dir resolved to %q, want an error", got)
	}
}

// TestWindowsSocketPathIgnoresAnUnusableRuntimeDir pins that Windows does not
// fail a command over a setting its named-pipe address provably discards. A
// relative or root QURL_CONNECTOR_RUNTIME_DIR left in an environment is a
// unix concern only.
func TestWindowsSocketPathIgnoresAnUnusableRuntimeDir(t *testing.T) {
	stateDir := `C:\ProgramData\qurl\state`
	want := filepath.Join(stateDir, SocketFile)
	for _, raw := range []string{"rt", `\`, "/", `relative\runtime`} {
		got, err := SocketPathForStateDir(stateDir, lookupEnvFrom(map[string]string{RuntimeDirEnv: raw}))
		if err != nil || got != want {
			t.Fatalf("%s=%q resolved to (%q, %v), want (%q, nil)", RuntimeDirEnv, raw, got, err, want)
		}
	}
}
