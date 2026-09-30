package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"

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
			opts := &globalOpts{resolveShareStateDir: func(string) (string, error) { return dir, nil }}
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
	got := localKeyStorage(&globalOpts{resolveShareStateDir: func(string) (string, error) { return dir, nil }})
	if got.Provider != connectoragentstate.KeyProviderTPM || got.Description != "TPM (sealed to this machine)" {
		t.Fatalf("localKeyStorage = %+v", got)
	}
	failing := &globalOpts{resolveShareStateDir: func(string) (string, error) { return "", errors.New("no state dir") }}
	if got := localKeyStorage(failing); got != (output.KeyStorage{}) {
		t.Fatalf("localKeyStorage with an unresolvable state dir = %+v, want nothing", got)
	}
}

func TestLocalKeyStorageNeverEchoesAnUnknownProvider(t *testing.T) {
	t.Setenv(connectoragentstate.EnvKeyProvider, connectoragentstate.KeyProviderTPM)
	dir := t.TempDir()
	// With the variable set, the connector checks it against the envelope; an
	// unknown provider id in the file is refused, so nothing is described.
	if err := os.WriteFile(filepath.Join(dir, connectoragentstate.SealedAgentStateFile), []byte("{\"provider_id\":\"\x1b[31mevil\"}"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := localKeyStorage(&globalOpts{resolveShareStateDir: func(string) (string, error) { return dir, nil }})
	if strings.Contains(got.Description, "\x1b") || strings.Contains(got.Provider, "\x1b") {
		t.Fatalf("localKeyStorage echoed a control sequence from disk: %+v", got)
	}
}
