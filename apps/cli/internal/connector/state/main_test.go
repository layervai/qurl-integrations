package state

import (
	"os"
	"testing"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
)

// TestMain keeps every test in this package off the host's TPM: with
// LAYERV_KEY_PROVIDER unset, a fresh namespace is sealed to a usable TPM, so
// tests that open a new store would depend on the machine running them. Tests
// that need another provider set it with t.Setenv (or clearStateEnv and
// unsetKeyProvider), which restore this.
func TestMain(m *testing.M) {
	// Unconditional: an ambient value (tpm on a box testing this feature, or
	// a whitespace-only value, which also counts as unset) must not decide.
	_ = os.Setenv(connectoragentstate.EnvKeyProvider, connectoragentstate.KeyProviderFile)
	os.Exit(m.Run())
}
