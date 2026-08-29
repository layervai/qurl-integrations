//go:build windows

package main

import (
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

func TestWindowsDaemonJobOutputUsesExactLogFiles(t *testing.T) {
	for _, precreate := range []bool{true, false} {
		t.Run(fmt.Sprintf("precreate=%t", precreate), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "logs with spaces")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			stdoutPath := filepath.Join(dir, "stdout.log")
			stderrPath := filepath.Join(dir, "stderr.log")
			if precreate {
				createProtectedWindowsDaemonLogForTest(t, stdoutPath)
				createProtectedWindowsDaemonLogForTest(t, stderrPath)
			}
			command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestWindowsDaemonJobOutputHelper$", "--", stdoutPath, stderrPath) //nolint:gosec // Exact test binary and test-owned paths.
			command.Env = append(os.Environ(), "QURL_WINDOWS_DAEMON_OUTPUT_HELPER=1")
			if raw, err := command.CombinedOutput(); err != nil {
				t.Fatalf("run Windows daemon-output helper: %v: %s", err, raw)
			}
			stdout, err := os.ReadFile(stdoutPath) //nolint:gosec // Test-owned exact path.
			if err != nil {
				t.Fatal(err)
			}
			stderr, err := os.ReadFile(stderrPath) //nolint:gosec // Test-owned exact path.
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(stdout), "daemon stdout\n") ||
				!strings.Contains(string(stderr), "daemon standard log") ||
				!strings.Contains(string(stderr), `msg="daemon structured log"`) {
				t.Fatalf("Windows daemon logs = stdout %q stderr %q", stdout, stderr)
			}
		})
	}
}

func createProtectedWindowsDaemonLogForTest(t *testing.T, path string) {
	t.Helper()
	token := windows.GetCurrentProcessToken()
	var err error
	user, err := token.GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		t.Fatalf("read current Windows test user: %v", err)
	}
	sid := user.User.Sid.String()
	createWindowsDaemonLogWithSDDLForTest(t, path, fmt.Sprintf(
		"O:%sG:%sD:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)", sid, sid, sid))
}

func createWindowsDaemonLogWithSDDLForTest(t *testing.T, path, sddl string) {
	t.Helper()
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	security := &windows.SecurityAttributes{
		Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor,
	}
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(path16, windows.GENERIC_WRITE|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		security, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsDaemonJobOutputRejectsUntrustedLogACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "untrusted.log")
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		t.Fatalf("read current Windows test user: %v", err)
	}
	sid := user.User.Sid.String()
	createWindowsDaemonLogWithSDDLForTest(t, path, fmt.Sprintf("O:%sG:%sD:P(A;;FA;;;WD)", sid, sid))
	file, err := openProtectedWindowsDaemonLog(path)
	if file != nil {
		_ = file.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "grants another principal access") {
		t.Fatalf("untrusted Windows daemon log ACL error = %v", err)
	}
}

func TestWindowsDaemonJobOutputHelper(t *testing.T) {
	if os.Getenv("QURL_WINDOWS_DAEMON_OUTPUT_HELPER") != "1" {
		t.Skip("subprocess helper; driven by TestWindowsDaemonJobOutputUsesExactLogFiles")
	}
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+2 >= len(os.Args) {
		t.Fatal("Windows daemon-output helper arguments are incomplete")
	}
	streams := &output.Streams{}
	if err := redirectDaemonJobOutput(os.Args[separator+1], os.Args[separator+2], streams); err != nil {
		t.Fatal(err)
	}
	stdoutHandle, stdoutErr := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE)
	stderrHandle, stderrErr := windows.GetStdHandle(windows.STD_ERROR_HANDLE)
	if stdoutErr != nil || stderrErr != nil || stdoutHandle != windows.Handle(os.Stdout.Fd()) || stderrHandle != windows.Handle(os.Stderr.Fd()) {
		t.Fatalf("Windows standard handles were not redirected: stdout=%v/%v stderr=%v/%v", stdoutHandle, stdoutErr, stderrHandle, stderrErr)
	}
	_, _ = fmt.Fprintln(streams.Out, "daemon stdout")
	log.Print("daemon standard log")
	slog.Error("daemon structured log")
}

func TestWindowsDaemonJobOutputRejectsPartialOrAliasedPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	for _, paths := range [][2]string{{path, ""}, {"", path}, {path, path}, {"relative", path}} {
		if err := redirectDaemonJobOutput(paths[0], paths[1], &output.Streams{}); err == nil ||
			!strings.Contains(err.Error(), "distinct exact absolute") {
			t.Fatalf("redirectDaemonJobOutput(%q, %q) = %v", paths[0], paths[1], err)
		}
	}
}
