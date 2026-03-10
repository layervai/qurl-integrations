package main

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func deleteCmd(apiKey, endpoint, format *string) *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "delete <resource-id>",
		Short: "Revoke/delete a QURL",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = format
			id := args[0]

			if !yes {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Revoke QURL %s? This cannot be undone. [y/N] ", id); err != nil {
					return err
				}
				scanner := bufio.NewScanner(cmd.InOrStdin())
				if !scanner.Scan() {
					return nil
				}
				answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
				if answer != "y" && answer != "yes" {
					_, err := fmt.Fprintln(cmd.OutOrStdout(), "Canceled.")
					return err
				}
			}

			c, err := newClient(apiKey, endpoint)
			if err != nil {
				return err
			}

			if err := c.Delete(cmd.Context(), id); err != nil {
				return fmt.Errorf("delete QURL: %w", err)
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "QURL %s has been revoked.\n", id)
			return err
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation prompt")

	return cmd
}
