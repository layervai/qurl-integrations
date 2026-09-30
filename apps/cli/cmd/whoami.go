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
			if opts.nativeStateStore != nil && printer.WhoAmIRendersDeviceKey() {
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
			var keyStorage output.KeyStorage
			if opts.nativeStateStore != nil && printer.WhoAmIRendersDeviceKey() {
				// Same condition as the device key above: both rows describe the
				// local state this command actually opened.
				keyStorage = localKeyStorage(opts)
			}
			return printer.WhoAmI(id, deviceKey, keyStorage)
		},
	}
}

// localKeyStorage describes the key provider protecting this device's local
// state, or the zero value when there is no envelope to describe. It reports
// only an existing envelope, so it never probes the TPM, and it is best
// effort: a failure here must not turn an identity answer into an error.
func localKeyStorage(opts *globalOpts) output.KeyStorage {
	stateDir, err := opts.resolveShareStateDir("")
	if err != nil {
		return output.KeyStorage{}
	}
	present := false
	for _, name := range []string{connectorstate.AgentStateFile, connectoragentstate.SealedAgentStateFile} {
		if _, err := os.Lstat(filepath.Join(stateDir, name)); err == nil {
			present = true
		}
	}
	if !present {
		return output.KeyStorage{}
	}
	provider, err := connectoragentstate.ResolveKeyProvider(stateDir)
	if err != nil {
		return output.KeyStorage{}
	}
	description := provider
	switch provider {
	case connectoragentstate.KeyProviderTPM:
		description = "TPM (sealed to this machine)"
	case connectoragentstate.KeyProviderFile:
		description = "file (owner-only, not encrypted)"
	}
	return output.KeyStorage{Provider: provider, Description: description}
}
