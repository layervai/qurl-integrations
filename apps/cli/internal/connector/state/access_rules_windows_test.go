//go:build windows

package state

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/accessrules"
)

func TestWindowsAccessRulesRejectInheritedNamespaceACL(t *testing.T) {
	store, err := OpenAccessRules(t.TempDir(), "https://api.example.test")
	if store != nil || !errors.Is(err, qurl.ErrInsecureAgentStatePermissions) {
		t.Fatalf("inherited namespace = %v/%v, want constructor refusal", store, err)
	}
}

func TestWindowsAccessRulesRejectBroadJournalACL(t *testing.T) {
	store := openAccessRulesTestStore(t)
	if _, err := store.Save(t.Context(), 0, &accessrules.Definition{ID: ruleID, Name: "Private rule", Mode: "keys"}); err != nil {
		t.Fatal(err)
	}
	setWindowsConnectorTestACL(t, filepath.Join(store.dir, accessRulesFile), true)
	if _, err := store.Snapshot(t.Context()); err == nil {
		t.Fatal("broad Windows journal ACL was accepted")
	}
}
