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
	writeOwnerOnlyTestFile(t, dir, connectoragentstate.SealedAgentStateFile, []byte(`{"provider_id":"tpm"}`))
	unsetKeyProvider(t)
	store, err := Open(dir)
	if err != nil {
		// The stub envelope may not satisfy every check the sealed store makes
		// on a real one. What this test pins is the branch: the failure must
		// come from initializing sealed state, never from resolution telling
		// the operator to set LAYERV_KEY_PROVIDER.
		if !strings.Contains(err.Error(), "initialize sealed agent state") || strings.Contains(err.Error(), connectoragentstate.EnvKeyProvider) {
			t.Fatalf("Open TPM namespace without the environment = %v, want the sealed branch", err)
		}
	} else {
		t.Cleanup(func() { _ = store.Close() })
		if store.envelope != connectoragentstate.SealedAgentStateFile {
			t.Fatalf("Open chose envelope %q, want the sealed envelope", store.envelope)
		}
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
	writeOwnerOnlyTestFile(t, dir, AgentStateFile, []byte("{}"))
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

// TestOpenRefusesAnEnvironmentSealedNamespaceWithoutItsVariables pins the
// guarantee the removed local guard in Open used to provide, now owned by the
// connector's resolver: a namespace sealed by local-key opened with no
// provider selected is refused, and the error names the variable to set.
//
// TODO(upstream-contract): the refusal and its wording belong to
// qurl-connector pkg/agentstate resolveKeyProvider.
func TestOpenRefusesAnEnvironmentSealedNamespaceWithoutItsVariables(t *testing.T) {
	clearStateEnv(t)
	dir := secureStateTestDir(t)
	writeOwnerOnlyTestFile(t, dir, connectoragentstate.SealedAgentStateFile, []byte(`{"provider_id":"local-key"}`))
	unsetKeyProvider(t)
	store, err := Open(dir)
	if err == nil {
		_ = store.Close()
		t.Fatal("Open accepted a local-key namespace with no provider selected")
	}
	if !errors.Is(err, ErrAgentStateEnvelope) || !strings.Contains(err.Error(), connectoragentstate.EnvKeyProvider+"="+connectoragentstate.KeyProviderLocalKey) {
		t.Fatalf("Open error = %v, want ErrAgentStateEnvelope naming %s=%s", err, connectoragentstate.EnvKeyProvider, connectoragentstate.KeyProviderLocalKey)
	}
	if _, statErr := os.Lstat(filepath.Join(dir, AgentStateFile)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("plaintext envelope written beside the sealed one: %v", statErr)
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

// writeOwnerOnlyTestFile writes name through the package's owner-only state
// writer, so on Windows it carries the protected ACL the stores require
// rather than the directory's inherited one.
func writeOwnerOnlyTestFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := replaceConnectorResources(dir, filepath.Join(dir, name), data); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
