package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/shared/client"
)

func extendCmd(opts *globalOpts) *cobra.Command {
	var extendBy string

	cmd := &cobra.Command{
		Use:     "extend <resource-id>",
		Short:   "Extend QURL expiration",
		Example: "  qurl extend r_k8xqp9h2sj9 --by 24h",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := opts.newClient()
			if err != nil {
				return err
			}

			qurl, err := c.Extend(cmd.Context(), args[0], client.ExtendInput{
				ExtendBy: extendBy,
			})
			if err != nil {
				return fmt.Errorf("extend QURL: %w", err)
			}

			return opts.formatter().FormatQURL(cmd.OutOrStdout(), qurl)
		},
	}

	cmd.Flags().StringVarP(&extendBy, "by", "b", "24h", "Duration to extend by (e.g., 1h, 24h, 7d)")

	return cmd
}
