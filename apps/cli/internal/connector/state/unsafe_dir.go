package state

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"

	qurl "github.com/layervai/qurl-go/qurl"
)

// ErrUnsafeDirectory means a directory on the path to qurl's state or to the
// qurl executable can be written by other users, so the connector's pinned
// filesystem layer refused to use anything beneath it. The check itself is
// correct; the remedy is local (tighten the directory or move the state), so
// exitcode maps it to Config.
var ErrUnsafeDirectory = errors.New("directory is writable by other users")

// UnsafeDirectoryError names the group- or other-writable directory behind a
// connector refusal. It wraps both ErrUnsafeDirectory and the original error,
// so every sentinel the original chain carried still matches.
type UnsafeDirectoryError struct {
	// Dir is the absolute directory that other users can write to.
	Dir string
	// Mode is Dir's permission bits when the CLI inspected it.
	Mode os.FileMode
	// ContainsStateDir reports that Dir is the resolved state directory or one
	// of its ancestors, so relocating the state directory also avoids it.
	ContainsStateDir bool
	Err              error
}

func (e *UnsafeDirectoryError) Error() string   { return e.Err.Error() }
func (e *UnsafeDirectoryError) Unwrap() []error { return []error{ErrUnsafeDirectory, e.Err} }

// TODO(upstream-contract): qurl-connector's pinnedfs validateTrustedDirectory
// reports this rule only as text ("directory component <path> has unsafe mode
// <octal>"); it has no sentinel. The phrase is pinned against the real
// connector by TestExplainUnsafeDirectoryMatchesConnector, so an upstream
// rewording fails that test rather than silently dropping the remedy.
var unsafeDirectoryPattern = regexp.MustCompile(`directory component (/.*?) has unsafe mode [0-7]{3,4}`)

// ExplainUnsafeDirectory returns err wrapped in an *UnsafeDirectoryError when
// it is the connector's refusal of a group- or other-writable directory, and
// err unchanged otherwise. The named directory is re-inspected so the remedy
// describes the filesystem as it is now. A sticky directory (such as /tmp) is
// left untranslated: it is shared on purpose, and `chmod go-w` is the wrong
// advice for it.
//
// qurl-go's own permission refusals keep their classification (a loose
// credential state mode exits Auth) and their message, which already names
// the chmod. Windows is excluded outright: the connector's mode rule is Unix
// only, and Go synthesizes Windows directory modes, so the re-inspection
// would prove nothing there.
func ExplainUnsafeDirectory(err error) error {
	if err == nil || !unixModeRule {
		return err
	}
	var already *UnsafeDirectoryError
	if errors.As(err, &already) ||
		errors.Is(err, qurl.ErrInsecureCredentialStatePermissions) ||
		errors.Is(err, qurl.ErrInsecureAgentStatePermissions) {
		return err
	}
	match := unsafeDirectoryPattern.FindStringSubmatch(err.Error())
	if match == nil {
		return err
	}
	dir := filepath.Clean(match[1])
	info, statErr := os.Lstat(dir)
	if statErr != nil || !info.IsDir() || info.Mode().Perm()&0o022 == 0 || info.Mode()&os.ModeSticky != 0 {
		return err
	}
	return &UnsafeDirectoryError{
		Dir:              dir,
		Mode:             info.Mode().Perm(),
		ContainsStateDir: containsStateDir(dir),
		Err:              err,
	}
}

func containsStateDir(dir string) bool {
	stateDir, err := ResolveDir("")
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(dir, stateDir)
	return err == nil && filepath.IsLocal(rel)
}
