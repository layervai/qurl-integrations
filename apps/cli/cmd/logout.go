package main

import (
	"github.com/spf13/cobra"
)

// logoutCmd removes the stored key from every storage backend that holds it.
func logoutCmd(opts *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove the qURL API key saved on this machine",
		Long: `Remove the qURL API key that "qurl login" saved on this machine.

Every place a key may sit is cleared: the OS keyring and the fallback
credential file. Running logout when nothing is stored is fine — it simply
says so.

logout only touches stored keys: if QURL_API_KEY is set in your
environment, commands keep using it until you unset the variable.`,
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
