package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
	connectorshare "github.com/layervai/qurl-connector/pkg/share"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

// TestLocalKeyStorageDescribesOnlyAnExistingEnvelope pins that whoami reports
// the provider of the envelope on disk and stays silent, without probing the
// TPM, when there is none.
func TestLocalKeyStorageDescribesOnlyAnExistingEnvelope(t *testing.T) {
	t.Setenv(connectoragentstate.EnvKeyProvider, "")
	for name, tc := range map[string]struct {
		file, body, want string
	}{
		"no state":  {},
		"plaintext": {file: connectorstate.AgentStateFile, body: "{}", want: connectoragentstate.KeyProviderFile},
		"tpm":       {file: connectoragentstate.SealedAgentStateFile, body: `{"provider_id":"tpm"}`, want: connectoragentstate.KeyProviderTPM},
		"corrupt":   {file: connectoragentstate.SealedAgentStateFile, body: `{}`},
		"both":      {file: "both"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			switch tc.file {
			case "":
			case "both":
				// Two envelopes: the connector refuses, so nothing is described.
				for name, body := range map[string]string{connectorstate.AgentStateFile: "{}", connectoragentstate.SealedAgentStateFile: `{"provider_id":"tpm"}`} {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			default:
				if err := os.WriteFile(filepath.Join(dir, tc.file), []byte(tc.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			opts := &globalOpts{nativeStateDir: dir}
			if got := localKeyStorage(opts).Provider; got != tc.want {
				t.Fatalf("localKeyStorage = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLocalKeyStorageDescribesProvidersForPeople(t *testing.T) {
	t.Setenv(connectoragentstate.EnvKeyProvider, "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, connectoragentstate.SealedAgentStateFile), []byte(`{"provider_id":"tpm"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := localKeyStorage(&globalOpts{nativeStateDir: dir})
	if got.Provider != connectoragentstate.KeyProviderTPM || got.Description != msgKeyStorageTPM {
		t.Fatalf("localKeyStorage = %+v", got)
	}
	if got := localKeyStorage(&globalOpts{}); got != (output.KeyStorage{}) {
		t.Fatalf("localKeyStorage with no opened state directory = %+v, want nothing", got)
	}
}

// TestLocalKeyStorageDescribesNothingForAConflictingEnvelope covers the file
// path: an unknown id in the envelope conflicts with the selected provider,
// the connector refuses it, and nothing is described.
func TestLocalKeyStorageDescribesNothingForAConflictingEnvelope(t *testing.T) {
	t.Setenv(connectoragentstate.EnvKeyProvider, connectoragentstate.KeyProviderTPM)
	dir := t.TempDir()
	// With the variable set, the connector checks it against the envelope; an
	// unknown provider id in the file is refused, so nothing is described.
	if err := os.WriteFile(filepath.Join(dir, connectoragentstate.SealedAgentStateFile), []byte("{\"provider_id\":\"\x1b[31mevil\"}"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := localKeyStorage(&globalOpts{nativeStateDir: dir})
	if got != (output.KeyStorage{}) {
		t.Fatalf("localKeyStorage for a conflicting envelope = %+v, want nothing described", got)
	}
}

// TestLocalKeyStorageNeverEchoesAnUnknownProvider drives the mapping's
// default arm directly: even an id that survived resolution is described with
// fixed text, and JSON carries no synthetic id.
func TestLocalKeyStorageNeverEchoesAnUnknownProvider(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, connectoragentstate.SealedAgentStateFile), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	original := connectorstate.ResolveKeyProvider
	connectorstate.ResolveKeyProvider = func(string) (string, error) { return "\x1b[31mevil", nil }
	t.Cleanup(func() { connectorstate.ResolveKeyProvider = original })
	got := localKeyStorage(&globalOpts{nativeStateDir: dir})
	if got != (output.KeyStorage{Description: msgKeyStorageUnrecognizedRow}) {
		t.Fatalf("localKeyStorage with an unknown provider = %+v, want only the fixed description", got)
	}
}

func TestLocalKeyStoragePassesTheConnectorsOtherProvidersThrough(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, connectoragentstate.SealedAgentStateFile), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	original := connectorstate.ResolveKeyProvider
	t.Cleanup(func() { connectorstate.ResolveKeyProvider = original })
	for _, provider := range []string{
		connectoragentstate.KeyProviderLocalKey, connectoragentstate.KeyProviderAWSKMS, connectoragentstate.KeyProviderGCPKMS,
		connectoragentstate.KeyProviderAWSNitro, connectoragentstate.KeyProviderGCPConfidentialSpace,
	} {
		connectorstate.ResolveKeyProvider = func(string) (string, error) { return provider, nil }
		got := localKeyStorage(&globalOpts{nativeStateDir: dir})
		if got != (output.KeyStorage{Provider: provider, Description: provider}) {
			t.Errorf("localKeyStorage for %q = %+v", provider, got)
		}
	}
}

func TestTPMSealingNoticeWordingByPlatform(t *testing.T) {
	for goos, want := range map[string]string{
		"linux":   msgTPMSealedLinuxDevice,
		"windows": msgTPMSealedDevice,
		"darwin":  msgTPMSealedDevice,
		"":        "",
	} {
		got, ok := tpmSealingNotice(goos)
		if got != want || ok != (want != "") {
			t.Errorf("tpmSealingNotice(%q) = %q, %v; want %q", goos, got, ok, want)
		}
	}
}

// TestSealingNoticeFiresOnlyBeforeStateExists drives the one new
// customer-visible notice through its real entry point: printed under native
// supervision when the resolver assigns a namespace with no envelope to the
// TPM, and silent once any envelope exists (qurl-go persists the agent ID
// before enrollment, so a later call site would never fire).
func TestSealingNoticeFiresOnlyBeforeStateExists(t *testing.T) {
	originalResolve := connectorstate.ResolveKeyProvider
	connectorstate.ResolveKeyProvider = func(string) (string, error) { return connectoragentstate.KeyProviderTPM, nil }
	t.Cleanup(func() { connectorstate.ResolveKeyProvider = originalResolve })

	note := func(dir, goos string, supervision connectorstate.RuntimeSupervision) string {
		var stderr bytes.Buffer
		opts := &globalOpts{
			streams:              &output.Streams{In: strings.NewReader(""), Out: io.Discard, Err: &stderr},
			resolvedSupervision:  supervision,
			keyStorageNoticeGOOS: goos,
		}
		opts.noteTPMSealing(dir)
		return stderr.String()
	}
	if got := note(t.TempDir(), "linux", connectorstate.RuntimeSupervisionNative); !strings.Contains(got, "tss group") || !strings.Contains(got, "move the state directory aside") || !strings.Contains(got, "LAYERV_KEY_PROVIDER=file") {
		t.Fatalf("linux notice = %q, want the tss clause and the opt-out", got)
	}
	// A genuine first run: nothing has created the state directory yet. The
	// connector's resolver treats a missing directory like an empty one
	// (pinned upstream by TestResolveKeyProviderMissingDirectoryResolvesLikeEmptyWithoutCreatingIt).
	if got := note(filepath.Join(t.TempDir(), "absent"), "linux", connectorstate.RuntimeSupervisionNative); !strings.Contains(got, "sealed to this machine's TPM") {
		t.Fatalf("first-run notice for an absent directory = %q, want it printed", got)
	}
	if got := note(t.TempDir(), "windows", connectorstate.RuntimeSupervisionNative); !strings.Contains(got, "open only on this machine") || strings.Contains(got, "tss") {
		t.Fatalf("windows notice = %q, want the permanence notice without tss", got)
	}
	if got := note(t.TempDir(), "linux", connectorstate.RuntimeSupervisionExternal); got != "" {
		t.Fatalf("external supervision printed %q, want nothing", got)
	}
	existing := t.TempDir()
	if err := os.WriteFile(filepath.Join(existing, connectoragentstate.SealedAgentStateFile), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := note(existing, "linux", connectorstate.RuntimeSupervisionNative); got != "" {
		t.Fatalf("existing state printed %q, want nothing", got)
	}
}

// TestWhoAmIWiresTheSealingNoticeAndKeyStorage drives both new call sites
// through the real command and openNativeRegisteredClient: a fresh namespace
// the resolver assigns to the TPM prints the sealing notice, and a namespace
// that already holds a sealed envelope renders Key storage without it.
func TestWhoAmIWiresTheSealingNoticeAndKeyStorage(t *testing.T) {
	original := connectorstate.ResolveKeyProvider
	connectorstate.ResolveKeyProvider = func(string) (string, error) { return connectoragentstate.KeyProviderTPM, nil }
	t.Cleanup(func() { connectorstate.ResolveKeyProvider = original })

	run := func(dir string) *runResult {
		srv := apitest.NewServer(t)
		state := bootstrapRegisteredState(t)
		return runCLI(t, &runOpts{
			args:          []string{"--endpoint", srv.URL, "whoami"},
			env:           map[string]string{},
			nativeClient:  true,
			shareStateDir: dir,
			openNativeRuntime: func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
				return &bootstrapNativeRuntime{store: &bootstrapAgentStateStore{state: state}}, nil
			},
		})
	}

	fresh := run(connectorStateTestDir(t))
	if fresh.code != 0 || !strings.Contains(fresh.stderr.String(), "sealed to this machine's TPM") {
		t.Fatalf("fresh namespace: exit=%d stderr=%q, want the sealing notice", fresh.code, fresh.stderr.String())
	}

	sealed := connectorStateTestDir(t)
	if err := os.WriteFile(filepath.Join(sealed, connectoragentstate.SealedAgentStateFile), []byte(`{"provider_id":"tpm"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	existing := run(sealed)
	if existing.code != 0 || strings.Contains(existing.stderr.String(), "sealed to this machine's TPM") {
		t.Fatalf("existing namespace: exit=%d stderr=%q, want no sealing notice", existing.code, existing.stderr.String())
	}
	if !strings.Contains(existing.stdout.String(), "Key storage:") || !strings.Contains(existing.stdout.String(), msgKeyStorageTPM) {
		t.Fatalf("existing namespace stdout = %q, want the TPM key storage row", existing.stdout.String())
	}
}
