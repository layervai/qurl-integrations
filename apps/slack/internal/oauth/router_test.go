package oauth

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// noopAdminStore satisfies AdminStore with no behavior — just enough
// for the Validate() pairing check.
type noopAdminStore struct{}

func (*noopAdminStore) BindWorkspace(_ context.Context, _ *WorkspaceMapping, _ string) error {
	return nil
}

// TestConfigValidateRejectsAdminStoreWithoutClassifier fences the
// AdminStore ↔ BindClassifyError pairing that handleBindError's
// switch relies on. Without a classifier, every bind conflict —
// including idempotent same-caller re-entries — would route to the
// default 500 arm, silently downgrading "key rotated" to "500".
// RegisterRoutes calls Validate() and panics on this misconfiguration
// so callers see the boot-time error instead of mysterious 500s
// after the first user runs /qurl setup.
func TestConfigValidateRejectsAdminStoreWithoutClassifier(t *testing.T) {
	cfg := Config{
		AdminStore:        &noopAdminStore{},
		BindClassifyError: nil,
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate must reject AdminStore wired without BindClassifyError")
	}
	if !strings.Contains(err.Error(), "BindClassifyError") {
		t.Errorf("Validate error should mention BindClassifyError; got %q", err.Error())
	}
}

// TestConfigValidateAcceptsPairedAdminStore mirrors the happy-path
// shape: AdminStore + BindClassifyError both wired → Validate passes.
func TestConfigValidateAcceptsPairedAdminStore(t *testing.T) {
	cfg := Config{
		AdminStore:        &noopAdminStore{},
		BindClassifyError: func(_ error) BindConflictCode { return "" },
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate rejected a paired config: %v", err)
	}
}

// TestConfigValidateAcceptsSandboxConfig fences the sandbox / no-DDB
// posture: AdminStore=nil means the callback skips the bind, so a
// classifier is irrelevant. Validate must not reject this combination.
func TestConfigValidateAcceptsSandboxConfig(t *testing.T) {
	cfg := Config{
		AdminStore:        nil,
		BindClassifyError: nil,
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate rejected the sandbox config: %v", err)
	}
}

// TestRegisterRoutesPanicsOnInvalidConfig fences that RegisterRoutes
// calls Validate() and panics on failure rather than silently
// proceeding with a config that would surface idempotent re-entries
// as 500s in production.
func TestRegisterRoutesPanicsOnInvalidConfig(t *testing.T) {
	cfg := Config{AdminStore: &noopAdminStore{}}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("RegisterRoutes must panic on AdminStore-without-classifier; got no panic")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "BindClassifyError") {
			t.Errorf("panic message should mention BindClassifyError; got %v", r)
		}
	}()
	RegisterRoutes(http.NewServeMux(), cfg)
}
