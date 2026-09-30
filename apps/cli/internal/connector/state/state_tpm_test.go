package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
)

// TestOpenReopensATPMNamespaceWithoutTheEnvironment pins the seam the native
// background job depends on: launchd, systemd, and Task Scheduler start
// `qurl daemon run` with no LAYERV_KEY_PROVIDER, so a namespace sealed to the
// TPM must still open sealed rather than be refused or forked into plaintext.
//
// It passes on a TPM-less runner because the connector's NewSDKStore defers
// all TPM contact to the first unseal; if that ever becomes eager, this test
// fails on every hosted runner rather than on the change that caused it.
func TestOpenReopensATPMNamespaceWithoutTheEnvironment(t *testing.T) {
	clearStateEnv(t)
	dir := secureStateTestDir(t)
	envelope := filepath.Join(dir, connectoragentstate.SealedAgentStateFile)
	if err := os.WriteFile(envelope, []byte(`{"provider_id":"tpm"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open TPM namespace without the environment = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if store.envelope != connectoragentstate.SealedAgentStateFile {
		t.Fatalf("Open chose envelope %q, want the sealed envelope", store.envelope)
	}
	if _, err := os.Lstat(filepath.Join(dir, AgentStateFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plaintext envelope created beside the TPM one: %v", err)
	}
}

// TestOpenRefusesTPMOverAnExistingPlaintextNamespace pins that the
// TPM never migrates state in place: naming it over plaintext is refused with
// the configuration exit code.
func TestOpenRefusesTPMOverAnExistingPlaintextNamespace(t *testing.T) {
	clearStateEnv(t)
	dir := secureStateTestDir(t)
	// Pin the plaintext precondition explicitly, so the refusal is asserted
	// even on a host whose TPM would otherwise seal a fresh namespace.
	t.Setenv(connectoragentstate.EnvKeyProvider, connectoragentstate.KeyProviderFile)
	first, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.envelope != AgentStateFile {
		t.Fatalf("Open with the file provider chose %q", first.envelope)
	}
	if err := os.WriteFile(filepath.Join(dir, AgentStateFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(connectoragentstate.EnvKeyProvider, connectoragentstate.KeyProviderTPM)
	store, err := Open(dir)
	if err == nil {
		_ = store.Close()
		t.Fatal("Open accepted LAYERV_KEY_PROVIDER=tpm over plaintext state")
	}
	// TODO(upstream-contract): "not an in-place migration" is qurl-connector
	// pkg/agentstate's refusal text for a provider change.
	if !errors.Is(err, ErrAgentStateEnvelope) || !strings.Contains(err.Error(), "not an in-place migration") {
		t.Fatalf("Open error = %v, want an ErrAgentStateEnvelope migration refusal", err)
	}
}

// TestRequireRuntimeSupervisionAcceptsTheTPMUnderNative pins that only
// providers whose key lives in the environment force external supervision.
func TestRequireRuntimeSupervisionAcceptsTheTPMUnderNative(t *testing.T) {
	for provider, refused := range map[string]bool{
		connectoragentstate.KeyProviderTPM:      false,
		"TPM":                                   false,
		connectoragentstate.KeyProviderFile:     false,
		connectoragentstate.KeyProviderLocalKey: true,
		connectoragentstate.KeyProviderAWSKMS:   true,
		"not-a-provider":                        true,
	} {
		t.Run(provider, func(t *testing.T) {
			clearStateEnv(t)
			dir := secureStateTestDir(t)
			t.Setenv(connectoragentstate.EnvKeyProvider, provider)
			err := RequireRuntimeSupervision(dir, RuntimeSupervisionNative)
			if got := errors.Is(err, ErrAgentStateEnvelope); got != refused {
				t.Fatalf("RequireRuntimeSupervision(native) with %q = %v, want refused=%v", provider, err, refused)
			}
		})
	}
}
