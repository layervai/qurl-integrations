package main

import (
	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// logoutCmd removes the stored key once login can store one (a later step).
func logoutCmd(opts *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove the qURL API key saved on this machine",
		Long: `Remove the qURL API key that "qurl login" saved on this machine.

logout only touches stored keys: if QURL_API_KEY is set in your
environment, commands keep using it until you unset the variable.`,
		Example: `  qurl logout
  unset QURL_API_KEY`,
		Args: noArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			_ = opts
			return exitcode.NotImplemented(msgNotImplemented)
		},
	}
}
