package agent

import (
	"os"
	"testing"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
)

// TestMain keeps these tests off the host's TPM: they open fresh state
// namespaces, and with LAYERV_KEY_PROVIDER unset a fresh namespace is sealed to
// a usable TPM. Tests that need another provider set it with t.Setenv.
func TestMain(m *testing.M) {
	if err := os.Setenv(connectoragentstate.EnvKeyProvider, connectoragentstate.KeyProviderFile); err != nil {
		panic("pin LAYERV_KEY_PROVIDER for hermetic tests: " + err.Error())
	}
	os.Exit(m.Run())
}
