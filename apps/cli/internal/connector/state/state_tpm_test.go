package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
)

// TestOpenReopensATPMNamespaceWithoutTheEnvironment pins the seam the native
// background job depends on: launchd, systemd, and Task Scheduler start
// `qurl daemon run` with no LAYERV_KEY_PROVIDER, so a namespace sealed to the
// TPM must still open sealed rather than be refused or forked into plaintext.
//
// What it asserts on every host: resolution did not take the "set
// LAYERV_KEY_PROVIDER" path, and no plaintext envelope was forked beside the
// sealed one. Whether the stub envelope then initializes (it does wherever the
// connector defers TPM contact to the first unseal) is deliberately tolerated.
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

// TestOpenSealsAFreshNamespaceWhenTheResolverChoosesTheTPM pins the part of
// the headline behavior this repository owns: when the connector's resolver
// picks tpm for an empty directory with no LAYERV_KEY_PROVIDER, Open takes the
// sealed branch and writes no plaintext envelope. Whether the host has a TPM
// is the resolver's question, stubbed here.
func TestOpenSealsAFreshNamespaceWhenTheResolverChoosesTheTPM(t *testing.T) {
	clearStateEnv(t)
	dir := secureStateTestDir(t)
	original := ResolveKeyProvider
	ResolveKeyProvider = func(string) (string, error) { return connectoragentstate.KeyProviderTPM, nil }
	t.Cleanup(func() { ResolveKeyProvider = original })
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("Open fresh namespace with the resolver choosing tpm = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if store.envelope != connectoragentstate.SealedAgentStateFile {
		t.Fatalf("Open chose envelope %q, want the sealed envelope", store.envelope)
	}
	if _, err := os.Lstat(filepath.Join(dir, AgentStateFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plaintext envelope created for a TPM namespace: %v", err)
	}
}

// TestOpenKeepsTPMNotRespondingReachable pins the chain exitcode relies on to
// exit Unavailable (11) rather than Config (3): Open wraps a resolver failure
// in ErrAgentStateEnvelope, and ErrTPMNotResponding must stay reachable
// through that wrapping.
func TestOpenKeepsTPMNotRespondingReachable(t *testing.T) {
	clearStateEnv(t)
	dir := secureStateTestDir(t)
	original := ResolveKeyProvider
	ResolveKeyProvider = func(string) (string, error) {
		return "", fmt.Errorf("%w; retry, or set %s=%s", connectoragentstate.ErrTPMNotResponding, connectoragentstate.EnvKeyProvider, connectoragentstate.KeyProviderFile)
	}
	t.Cleanup(func() { ResolveKeyProvider = original })
	store, err := Open(dir)
	if err == nil {
		_ = store.Close()
		t.Fatal("Open succeeded while the TPM was not responding")
	}
	if !errors.Is(err, connectoragentstate.ErrTPMNotResponding) || !errors.Is(err, ErrAgentStateEnvelope) {
		t.Fatalf("Open error = %v, want both ErrTPMNotResponding and ErrAgentStateEnvelope reachable", err)
	}
}

// TestOpenRefusesExplicitFileOverASealedNamespace pins the opt-out's other
// edge, the guarantee the removed local guard in Open used to give: naming
// the plaintext provider over sealed state is refused, never a second
// envelope written beside it.
//
// TODO(upstream-contract): the refusal belongs to qurl-connector
// pkg/agentstate resolveKeyProvider.
func TestOpenRefusesExplicitFileOverASealedNamespace(t *testing.T) {
	clearStateEnv(t)
	dir := secureStateTestDir(t)
	writeOwnerOnlyTestFile(t, dir, connectoragentstate.SealedAgentStateFile, []byte(`{"provider_id":"tpm"}`))
	t.Setenv(connectoragentstate.EnvKeyProvider, connectoragentstate.KeyProviderFile)
	store, err := Open(dir)
	if err == nil {
		_ = store.Close()
		t.Fatal("Open accepted LAYERV_KEY_PROVIDER=file over a sealed namespace")
	}
	if !errors.Is(err, ErrAgentStateEnvelope) || !strings.Contains(err.Error(), connectoragentstate.SealedAgentStateFile) {
		t.Fatalf("Open error = %v, want ErrAgentStateEnvelope naming the sealed envelope", err)
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

func TestEnvelopePresentTreatsOnlyNotExistAsAbsent(t *testing.T) {
	if EnvelopePresent(filepath.Join(t.TempDir(), "absent")) {
		t.Fatal("a missing directory reported an envelope")
	}
	if EnvelopePresent(t.TempDir()) {
		t.Fatal("an empty directory reported an envelope")
	}
	// A path whose parent is a file (ENOTDIR, not ENOENT) is unknown, which
	// must read as present. Windows reports it as path-not-found instead.
	if runtime.GOOS == "windows" {
		return
	}
	parentFile := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parentFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !EnvelopePresent(parentFile) {
		t.Fatal("an unreadable directory reported no envelope")
	}
}

// TestOpenTakesTheSealedBranchForAnythingButExactlyFile pins the TODO on
// Open: only the exact file id is plaintext, so an empty or differently cased
// answer from the resolver can never silently open plaintext state.
func TestOpenTakesTheSealedBranchForAnythingButExactlyFile(t *testing.T) {
	for _, provider := range []string{"", "File"} {
		t.Run(provider, func(t *testing.T) {
			clearStateEnv(t)
			dir := secureStateTestDir(t)
			original := ResolveKeyProvider
			ResolveKeyProvider = func(string) (string, error) { return provider, nil }
			t.Cleanup(func() { ResolveKeyProvider = original })
			store, err := Open(dir)
			if err == nil {
				t.Cleanup(func() { _ = store.Close() })
				if store.envelope != connectoragentstate.SealedAgentStateFile {
					t.Fatalf("Open with resolver answer %q chose %q, want the sealed branch", provider, store.envelope)
				}
				return
			}
			if !strings.Contains(err.Error(), "initialize sealed agent state") {
				t.Fatalf("Open with resolver answer %q = %v, want the sealed branch", provider, err)
			}
		})
	}
}
