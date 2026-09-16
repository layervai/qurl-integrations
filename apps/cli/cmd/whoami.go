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
read an account API key on a warm start. If this machine is not enrolled, run
"qurl login" or set QURL_API_KEY for one-time bootstrap.

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
			return opts.printer().WhoAmI(id)
		},
	}
}
