package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func quotaCmd(opts *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:     "quota",
		Short:   "Show usage quota and plan info",
		Example: "  qurl quota",
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := opts.newClient()
			if err != nil {
				return err
			}

			result, err := c.GetQuota(cmd.Context())
			if err != nil {
				return fmt.Errorf("get quota: %w", err)
			}

			return opts.formatter().FormatQuota(cmd.OutOrStdout(), result)
		},
	}
}
