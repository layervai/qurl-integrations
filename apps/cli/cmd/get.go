package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func getCmd(opts *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:               "get <resource-id>",
		Short:             "Get QURL details",
		Example:           "  qurl get r_k8xqp9h2sj9",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: resourceIDCompletion(opts),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateResourceID(args[0]); err != nil {
				return err
			}

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

	return cmd
}
