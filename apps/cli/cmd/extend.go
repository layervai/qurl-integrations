package main

import (
	"errors"

	"github.com/spf13/cobra"
)

func extendCmd(apiKey, endpoint, format *string) *cobra.Command {
	var extendBy string

	cmd := &cobra.Command{
		Use:    "extend <resource-id>",
		Short:  "Extend QURL expiration",
		Args:   cobra.ExactArgs(1),
		Hidden: true, // Not yet implemented — hidden until backend Extend() is available
		RunE: func(_ *cobra.Command, _ []string) error {
			_, _ = apiKey, endpoint
			_, _ = format, extendBy
			return errors.New("extend not yet implemented")
		},
	}

	cmd.Flags().StringVarP(&extendBy, "by", "b", "24h", "Duration to extend by")

	return cmd
}
