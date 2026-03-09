package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func getCmd(apiKey, endpoint, format *string) *cobra.Command {
	return &cobra.Command{
		Use:   "get <resource-id>",
		Short: "Get QURL details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(apiKey, endpoint)
			if err != nil {
				return err
			}

			qurl, err := c.Get(cmd.Context(), args[0])
			if err != nil {
				return fmt.Errorf("get QURL: %w", err)
			}

			f := getFormatter(format)
			return f.FormatQURL(cmd.OutOrStdout(), qurl)
		},
	}
}
