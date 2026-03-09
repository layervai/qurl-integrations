package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func deleteCmd(apiKey, endpoint, format *string) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <resource-id>",
		Short: "Revoke/delete a QURL",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(apiKey, endpoint)
			if err != nil {
				return err
			}

			if err := c.Delete(cmd.Context(), args[0]); err != nil {
				return fmt.Errorf("delete QURL: %w", err)
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "QURL %s has been revoked.\n", args[0])
			return err
		},
	}
}
