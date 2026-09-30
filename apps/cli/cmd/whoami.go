package main

import (
	"errors"
	"os"
	"path/filepath"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
	"github.com/spf13/cobra"

	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

// whoamiCmd reports the account and device identity behind the registered
// machine credential, via the platform's identity echo.
func whoamiCmd(opts *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show which qURL account this device belongs to",
		Long: `Show the qURL account and registered device identity used by this machine.

The command opens the same durable device identity as publish, list, share,
and lifecycle commands, then checks it against the qURL service. It does not
read an account API key on a warm start. A new device enrolls automatically
without an account. Use "qurl account setup" to enable account recovery.

When this machine has registered device state, the output includes its
device public key and which key storage protects that state: the TPM (sealed
to this machine) or a plaintext owner-only file (plain --quiet prints only
the owner id). The private key and the device API key never leave local
state.

Useful for checking which account a script will act as before it publishes
anything.`,
		Example: `  qurl whoami
  qurl whoami -o json
  qurl whoami -q`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := opts.newClient(cmd.Context())
			if err != nil {
				return err
			}
			id := opts.registeredIdentity
			if id == nil {
				id, err = client.Me(cmd.Context())
				if err != nil {
					return err
				}
			}
			if id == nil {
				return errors.New("qURL account identity response is empty")
			}
			printer := opts.printer()
			var deviceKey string
			var keyStorage output.KeyStorage
			// Both rows describe the local state this command actually opened,
			// under one condition so they cannot drift apart.
			if opts.nativeStateStore != nil && printer.WhoAmIRendersDeviceKey() {
				keyStorage = localKeyStorage(opts)
				var keyErr error
				deviceKey, keyErr = devicePublicKey(cmd.Context(), opts.nativeStateStore)
				// The identity is already complete. A missing key row must not
				// turn the diagnostic command into a failure.
				var invalid *invalidDevicePublicKeyError
				switch {
				case errors.As(keyErr, &invalid):
					printer.Warnf(msgDevicePublicKeyInvalid, keyErr)
				case keyErr != nil:
					printer.Warnf(msgDevicePublicKeyUnreadable, keyErr)
				}
			}
			return printer.WhoAmI(id, deviceKey, keyStorage)
		},
	}
}

// resolveLocalKeyProvider is the connector's provider resolution; tests replace
// it to reach localKeyStorage's own mapping with ids a file could not produce.
var resolveLocalKeyProvider = connectoragentstate.ResolveKeyProvider

// localKeyStorage describes the key provider protecting this device's local
// state, or the zero value when there is no envelope to describe. It is best
// effort: a failure here must not turn an identity answer into an error.
//
// TODO(upstream-contract): "never probes the TPM" rests on qurl-connector's
// ResolveKeyProvider not probing when an envelope already exists; the
// presence check here only makes the probing path unreachable locally.
func localKeyStorage(opts *globalOpts) output.KeyStorage {
	stateDir, err := opts.resolveShareStateDir("")
	if err != nil {
		return output.KeyStorage{}
	}
	present := false
	for _, name := range []string{connectorstate.AgentStateFile, connectoragentstate.SealedAgentStateFile} {
		if _, err := os.Lstat(filepath.Join(stateDir, name)); err == nil {
			present = true
			break
		}
	}
	if !present {
		return output.KeyStorage{}
	}
	provider, err := resolveLocalKeyProvider(stateDir)
	if err != nil {
		return output.KeyStorage{}
	}
	// Construct, never echo: the provider id comes from a file on disk, so
	// only the connector's own names reach the terminal.
	var description string
	switch provider {
	case connectoragentstate.KeyProviderTPM:
		description = "TPM (sealed to this machine)"
	case connectoragentstate.KeyProviderFile:
		description = "file (owner-only, not encrypted)"
	case connectoragentstate.KeyProviderLocalKey, connectoragentstate.KeyProviderAWSKMS,
		connectoragentstate.KeyProviderGCPKMS, connectoragentstate.KeyProviderAWSNitro,
		connectoragentstate.KeyProviderGCPConfidentialSpace:
		description = provider
	default:
		// No synthetic id in JSON: only the human row warns.
		return output.KeyStorage{Description: "unrecognized provider"}
	}
	return output.KeyStorage{Provider: provider, Description: description}
}
