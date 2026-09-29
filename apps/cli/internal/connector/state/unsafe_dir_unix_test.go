//go:build darwin || linux

package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
	qurl "github.com/layervai/qurl-go/qurl"
)

// realTempDir resolves macOS's /var -> /private/var alias and closes the
// directory to other users regardless of umask: the connector refuses
// symlinked or loose ancestors, which would mask the directory under test.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory, which needs its search bit.
		t.Fatal(err)
	}
	return dir
}

func looseDir(t *testing.T, parent string) string {
	t.Helper()
	dir := filepath.Join(parent, "loose")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// umask 002 (the Ubuntu and Fedora default) leaves tool-created
	// directories such as ~/.local at 0775.
	if err := os.Chmod(dir, 0o775); err != nil { //nolint:gosec // intentionally group-writable to exercise the refusal.
		t.Fatal(err)
	}
	return dir
}

// TestExplainUnsafeDirectoryMatchesConnector pins the translation to the real
// connector refusal rather than to a hand-written copy of its text, then
// proves the remedy the CLI prints is the one that clears it.
func TestExplainUnsafeDirectoryMatchesConnector(t *testing.T) {
	loose := looseDir(t, realTempDir(t))
	stateDir := filepath.Join(loose, "qurl", "connector-v2")
	t.Setenv(EnvStateDirPrimary, stateDir)

	raw := connectoragentstate.ValidateSDKStoreLayout(stateDir)
	if raw == nil {
		t.Fatal("connector accepted a state directory beneath a group-writable directory")
	}
	var explained *UnsafeDirectoryError
	if !errors.As(ExplainUnsafeDirectory(raw), &explained) {
		t.Fatalf("connector refusal was not explained; upstream wording may have changed: %v", raw)
	}
	if explained.Dir != loose || explained.Mode != 0o775 || !explained.ContainsStateDir {
		t.Fatalf("explained = {Dir:%q Mode:%04o ContainsStateDir:%v}, want {%q 0775 true}",
			explained.Dir, explained.Mode, explained.ContainsStateDir, loose)
	}
	if !errors.Is(explained, ErrUnsafeDirectory) || !errors.Is(explained, raw) {
		t.Fatal("explained error must match ErrUnsafeDirectory and keep the original chain")
	}

	// The printed remedy: chmod go-w on the named directory.
	if err := os.Chmod(loose, 0o755); err != nil { //nolint:gosec // the remedy under test.
		t.Fatal(err)
	}
	if err := connectoragentstate.ValidateSDKStoreLayout(stateDir); err != nil {
		t.Fatalf("connector still refused after chmod go-w %s: %v", loose, err)
	}
}

func TestExplainUnsafeDirectoryLeavesOtherErrorsAlone(t *testing.T) {
	root := realTempDir(t)
	loose := looseDir(t, root)
	t.Setenv(EnvStateDirPrimary, filepath.Join(root, "elsewhere"))

	sticky := filepath.Join(root, "sticky")
	if err := os.Mkdir(sticky, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sticky, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}

	refusal := func(dir string) error {
		return errors.New("validate Linux user job executable " + dir + "/qurl: directory component " + dir + " has unsafe mode 0775")
	}
	for name, err := range map[string]error{
		"unrelated":        errors.New("dial tcp 127.0.0.1:8000: connect: connection refused"),
		"already fixed":    refusal(private),
		"sticky shared":    refusal(sticky),
		"missing":          refusal(filepath.Join(root, "gone")),
		"symlink refusal":  errors.New("directory component /bin must not be a symlink"),
		"owner refusal":    errors.New("directory component " + loose + " owner uid is 7, want root or effective uid 501"),
		"relative claimed": errors.New("directory component loose has unsafe mode 0775"),
	} {
		t.Run(name, func(t *testing.T) {
			if got := ExplainUnsafeDirectory(err); got != err { //nolint:errorlint // identity is the contract: unchanged means the same value.
				t.Fatalf("ExplainUnsafeDirectory rewrote %v into %v", err, got)
			}
		})
	}
	// qurl-go classifies its own permission refusals; they must keep their
	// exit code and message even when the text also names a loose directory.
	for _, sentinel := range []error{qurl.ErrInsecureCredentialStatePermissions, qurl.ErrInsecureAgentStatePermissions} {
		err := fmt.Errorf("%w: %w", sentinel, refusal(loose))
		if got := ExplainUnsafeDirectory(err); got != err { //nolint:errorlint // identity is the contract.
			t.Fatalf("ExplainUnsafeDirectory rewrote qurl-go's %v", sentinel)
		}
	}
	if ExplainUnsafeDirectory(nil) != nil {
		t.Fatal("nil must stay nil")
	}

	// A directory outside the state path, such as the one holding the qurl
	// executable, is explained without the state-relocation remedy, and a
	// second pass leaves the explanation as it is.
	explained := ExplainUnsafeDirectory(refusal(loose))
	var unsafeDir *UnsafeDirectoryError
	if !errors.As(explained, &unsafeDir) || unsafeDir.Dir != loose || unsafeDir.ContainsStateDir {
		t.Fatalf("executable directory explanation = %#v", explained)
	}
	if again := ExplainUnsafeDirectory(explained); again != explained { //nolint:errorlint // identity is the contract.
		t.Fatal("explaining twice must not wrap again")
	}
}
