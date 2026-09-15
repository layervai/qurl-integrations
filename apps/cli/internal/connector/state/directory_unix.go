//go:build !windows

package state

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

const dirMode os.FileMode = 0o700

// EnsureDirMode creates or restricts the owner-only state directory before
// qurl-go pins and validates it.
func EnsureDirMode(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return errors.New("state directory path is empty")
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, dirMode); err != nil {
			return fmt.Errorf("create state directory %s: %w", dir, err)
		}
		info, err = os.Lstat(dir)
	}
	if err != nil {
		return fmt.Errorf("inspect state directory %s: %w", dir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || !connectorResourceOwnerOK(info) {
		return fmt.Errorf("state directory %s must be an owner-owned non-symlink directory", dir)
	}
	if err := os.Chmod(dir, dirMode); err != nil {
		return fmt.Errorf("restrict state directory %s to owner-only %#o: %w", dir, dirMode, err)
	}
	current, err := os.Lstat(dir)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !current.IsDir() ||
		!connectorResourceOwnerOK(current) || current.Mode().Perm() != dirMode || !os.SameFile(info, current) {
		return fmt.Errorf("state directory %s changed while establishing owner-only access", dir)
	}
	return nil
}
