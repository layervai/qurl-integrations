// Package statetest holds test-only helpers for code that opens qurl's native
// agent state.
package statetest

import (
	"os"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
)

// PinFileKeyProvider sets LAYERV_KEY_PROVIDER=file for the whole test binary.
// With the variable unset, the connector seals a fresh state namespace to a
// usable host TPM, which would make any test that opens new state depend on
// the machine running it: a developer in the tss group would seal test state
// to real hardware while CI, which has no TPM, stayed green. Call it first in
// TestMain of every package whose tests can open agent state; tests that need
// another provider set it with t.Setenv, which restores this value. It
// overwrites any ambient value on purpose, including tpm or whitespace.
func PinFileKeyProvider() {
	if err := os.Setenv(connectoragentstate.EnvKeyProvider, connectoragentstate.KeyProviderFile); err != nil {
		panic("pin LAYERV_KEY_PROVIDER for hermetic tests: " + err.Error())
	}
}
