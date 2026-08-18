package main

import (
	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// whoamiCmd will report the account behind the configured key once the
// identity endpoint is available (a later step).
func whoamiCmd(opts *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show which qURL account your key belongs to",
		Long: `Show the account and plan behind the configured qURL API key.

Useful for checking which environment a script will talk to before it
publishes anything.`,
		Example: `  qurl whoami
  qurl whoami -o json`,
		Args: noArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			_ = opts
			return exitcode.NotImplemented(msgNotImplemented)
		},
	}
}
