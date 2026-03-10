package main

import (
	"bufio"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func deleteCmd(opts *globalOpts) *cobra.Command {
	var (
		yes    bool
		dryRun bool
	)

	cmd := &cobra.Command{
		Use:               "delete <resource-id>",
		Short:             "Revoke/delete a QURL",
		Example:           "  qurl delete r_k8xqp9h2sj9 --yes",
		Args:              cobra.ExactArgs(1),
		Aliases:           []string{"revoke"},
		ValidArgsFunction: resourceIDCompletion(opts),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]

			if err := validateResourceID(id); err != nil {
				return err
			}

			if dryRun {
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "Would revoke QURL %s (dry run, no changes made)\n", id)
				return err
			}

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

			c, err := opts.newClient()
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
	cmd.Flags().BoolVar(&yes, "force", false, "Skip confirmation prompt (alias for --yes)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be done without making changes")

	return cmd
}
