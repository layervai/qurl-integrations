package main

import (
	"errors"

	"github.com/spf13/cobra"

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
device public key. The private key and the device API key never leave local
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
			// Plain --quiet prints only the owner id, so it skips the state read.
			// JSON wins over --quiet in the printer, so JSON still reads it.
			rendersDeviceKey := opts.resolvedFormat == output.FormatJSON || !opts.quiet
			if opts.nativeRuntime != nil && rendersDeviceKey {
				// Copy so the cached registeredIdentity stays a pure /v1/me echo.
				// The shallow copy is safe only because attach writes a string
				// field; revisit if it ever writes a slice or pointer.
				shown := *id
				if err := attachDevicePublicKey(cmd.Context(), opts.nativeRuntime, &shown); err != nil {
					// The identity is already complete. A missing key row must not
					// turn the diagnostic command into a failure.
					opts.printer().Warnf(msgDevicePublicKeyUnreadable, err)
				} else {
					id = &shown
				}
			}
			return opts.printer().WhoAmI(id)
		},
	}
}
