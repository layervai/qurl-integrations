package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
)

// TestOpenPlaintextRefusesSealedEnvelope pins the fail-closed rule: the default
// plaintext provider must never write agent_state.json beside a sealed
// envelope that another owner established with a key provider.
func TestOpenPlaintextRefusesSealedEnvelope(t *testing.T) {
	t.Setenv(connectoragentstate.EnvKeyProvider, "")
	t.Setenv(connectoragentstate.EnvLocalKeyFD, "")
	// The directory must already carry the owner-only ACL on Windows, otherwise
	// the directory capability refuses it before the envelope guard runs.
	dir := secureStateTestDir(t)
	sealed := filepath.Join(dir, connectoragentstate.SealedAgentStateFile)
	if err := os.WriteFile(sealed, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err == nil {
		_ = store.Close()
		t.Fatal("expected Open to refuse a directory that holds a sealed envelope")
	}
	if !strings.Contains(err.Error(), connectoragentstate.SealedAgentStateFile) || !strings.Contains(err.Error(), connectoragentstate.EnvKeyProvider) {
		t.Fatalf("error must name the sealed envelope and the provider env: %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(dir, AgentStateFile)); !os.IsNotExist(statErr) {
		t.Fatalf("plaintext envelope must not be created next to a sealed one: %v", statErr)
	}
}
