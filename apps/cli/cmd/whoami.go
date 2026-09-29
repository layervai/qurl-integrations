package main

import (
	"errors"

	"github.com/spf13/cobra"
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
device public key (plain --quiet prints only the owner id). The private key
and the device API key never leave local state.

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
				switch {
				case errors.Is(keyErr, errInvalidDevicePublicKey):
					printer.Warnf(msgDevicePublicKeyInvalid, keyErr)
				case keyErr != nil:
					printer.Warnf(msgDevicePublicKeyUnreadable, keyErr)
				}
			}
			return printer.WhoAmI(id, deviceKey)
		},
	}
}
