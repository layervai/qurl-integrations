//go:build windows

package state

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	qurl "github.com/layervai/qurl-go/qurl"
)

// EnsureDirMode delegates directory creation and ACL validation to qurl-go's
// retained Windows state capability. It does not create an inherited-DACL
// directory before qurl-go can establish the protected owner-only namespace.
func EnsureDirMode(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return errors.New("state directory path is empty")
	}
	store, err := qurl.OpenFileAgentState(filepath.Join(dir, AgentStateFile))
	if err != nil {
		return fmt.Errorf("secure Windows state directory: %w", err)
	}
	if err := store.Close(); err != nil {
		return fmt.Errorf("close Windows state directory capability: %w", err)
	}
	return nil
}
