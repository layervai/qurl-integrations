package main

import (
	"errors"

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
to this machine) or a plaintext owner-only file. Plain --quiet prints only
the owner id. The private key and the device API key never leave local
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
				if keyStorage.Provider == "" && keyStorage.Description != "" {
					// JSON omits an unrecognized id; the warning reaches both
					// projections without inventing one.
					printer.Warnf("%s", msgKeyStorageUnrecognized)
				}
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

// localKeyStorage describes the key provider protecting the state whoami
// opened (opts.nativeStateDir, recorded with the store), or the zero value
// when there is no envelope to describe. It is best effort: a failure here
// must not turn an identity answer into an error.
func localKeyStorage(opts *globalOpts) output.KeyStorage {
	// Described from the envelope on disk, never from the store's Go type: a
	// type another repository hands off is not evidence of whether the state
	// is encrypted.
	stateDir := opts.nativeStateDir
	if stateDir == "" || !connectorstate.EnvelopePresent(stateDir) {
		return output.KeyStorage{}
	}
	provider, err := connectorstate.ResolveKeyProvider(stateDir)
	if err != nil {
		return output.KeyStorage{}
	}
	// Construct, never echo: the provider id comes from a file on disk, so
	// only the connector's own names reach the terminal.
	//
	// TODO(upstream-contract): mirrors qurl-connector pkg/agentstate's provider
	// constants; a new provider shows as unrecognized until it is added here.
	var description string
	switch provider {
	case connectoragentstate.KeyProviderTPM:
		description = msgKeyStorageTPM
	case connectoragentstate.KeyProviderFile:
		description = msgKeyStorageFile
	case connectoragentstate.KeyProviderLocalKey, connectoragentstate.KeyProviderAWSKMS,
		connectoragentstate.KeyProviderGCPKMS, connectoragentstate.KeyProviderAWSNitro,
		connectoragentstate.KeyProviderGCPConfidentialSpace:
		// Safe to show as is: provider has just matched one of the
		// connector's own constants, and these ids are what their operators
		// set, so the id itself is the clearest description.
		description = provider
	default:
		// No synthetic id in JSON: only the human row warns.
		return output.KeyStorage{Description: msgKeyStorageUnrecognizedRow}
	}
	return output.KeyStorage{Provider: provider, Description: description}
}
