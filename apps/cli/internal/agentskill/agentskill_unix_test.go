//go:build !windows

package agentskill

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Opening a named pipe waits for a writer. A pipe where the skill should be
// must be passed over without being opened, or `qurl --version` would hang.
func TestANamedPipeWhereTheFileShouldBeIsNotOpened(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".claude", "skills", "qurl")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pipe := filepath.Join(dir, "SKILL.md")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Skipf("no named pipes here: %v", err)
	}
	done := make(chan bool, 1)
	go func() {
		_, ok := FindOutdated(home, "linux")
		done <- ok
	}()
	select {
	case ok := <-done:
		if ok {
			t.Error("a named pipe was reported as a saved copy")
		}
	case <-time.After(5 * time.Second):
		// Let the stuck open finish, so the test's folder can be removed.
		if writer, err := os.OpenFile(pipe, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = writer.Close()
		}
		t.Fatal("the lookup opened a named pipe and waited on it")
	}
}
