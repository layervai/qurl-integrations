package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func getCmd(opts *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:     "get <resource-id>",
		Short:   "Get QURL details",
		Example: "  qurl get r_k8xqp9h2sj9",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := opts.newClient()
			if err != nil {
				return err
			}

			qurl, err := c.Get(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("get QURL: %w", err)
			}

			return opts.formatter().FormatQURL(cmd.OutOrStdout(), qurl)
		},
	}
}
