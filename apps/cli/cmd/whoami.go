package main

import (
	"github.com/spf13/cobra"
)

// whoamiCmd reports the account and key identity behind the configured
// credential, via the platform's identity echo. Deliberately cheap: it shows
// identity only — no plan or usage data — so it is safe to call from scripts
// and shell prompts.
func whoamiCmd(opts *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show which qURL account your key belongs to",
		Long: `Show who the configured qURL API key is: the account it belongs to and
the key's own identity (id, kind, scopes, expiry).

The key is resolved the same way every command resolves it — QURL_API_KEY
first, then the key "qurl login" stored — and checked against the qURL
service, so whoami also confirms the key still works. It shows identity
only: no plan or usage details.

Useful for checking which account a script will act as before it publishes
anything.`,
		Example: `  qurl whoami
  qurl whoami -o json
  qurl whoami -q`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := opts.newClient()
			if err != nil {
				return err
			}
			id, err := client.Me(cmd.Context())
			if err != nil {
				return err
			}
			return opts.printer().WhoAmI(id)
		},
	}
}
