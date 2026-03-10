package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func mintCmd(opts *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "mint <resource-id>",
		Short: "Mint a new access link for a QURL",
		Long: `Creates a new access token and link for an existing QURL resource.
Useful for multi-use QURLs where you want to generate additional access links.`,
		Example: `  qurl mint r_k8xqp9h2sj9
  LINK=$(qurl mint r_k8xqp9h2sj9 -q)`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := opts.newClient()
			if err != nil {
				return err
			}

			result, err := c.MintLink(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("mint link: %w", err)
			}

			if opts.quiet {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), result.QURLLink)
				return err
			}

			return opts.formatter().FormatMint(cmd.OutOrStdout(), result)
		},
	}
}
