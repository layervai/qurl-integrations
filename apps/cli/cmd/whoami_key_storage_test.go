package main

import (
	"os"
	"path/filepath"
	"testing"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"

	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
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
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.file != "" {
				if err := os.WriteFile(filepath.Join(dir, tc.file), []byte(tc.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			opts := &globalOpts{resolveShareStateDir: func(string) (string, error) { return dir, nil }}
			if got := localKeyStorage(opts); got != tc.want {
				t.Fatalf("localKeyStorage = %q, want %q", got, tc.want)
			}
		})
	}
}
