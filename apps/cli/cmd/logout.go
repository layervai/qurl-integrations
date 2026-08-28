package main

import (
	"github.com/spf13/cobra"
)

// logoutCmd removes legacy stored account-key copies. Registered v2 device
// state is intentionally outside this compatibility-cleanup command.
func logoutCmd(opts *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove legacy stored qURL account keys",
		Long: `Remove account API key copies saved by older qURL CLI versions.

Every place a key may sit is cleared: the OS keyring and the fallback
credential file. Running logout when nothing is stored is fine — it simply
says so.

Current qURL login does not store the account key. logout does not unset
QURL_API_KEY and does not delete the registered native device identity.`,
		Example: `  qurl logout
  unset QURL_API_KEY`,
		Args: noArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			removed, err := opts.credentialStore().RemoveAll()
			if err != nil {
				return err
			}
			return opts.printer().Logout(removed)
		},
	}
}
