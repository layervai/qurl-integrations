package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/shared/client"
)

func updateCmd(opts *globalOpts) *cobra.Command {
	var description string

	cmd := &cobra.Command{
		Use:   "update <resource-id>",
		Short: "Update a QURL's properties",
		Example: `  qurl update r_k8xqp9h2sj9 --description "Production API access"
  qurl update r_k8xqp9h2sj9 -d ""  # clear description`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: resourceIDCompletion(opts),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateResourceID(args[0]); err != nil {
				return err
			}

			if !cmd.Flags().Changed("description") {
				return errors.New("at least one flag must be set (e.g., --description)")
			}

			c, err := opts.newClient()
			if err != nil {
				return err
			}

			input := client.UpdateInput{}
			if cmd.Flags().Changed("description") {
				input.Description = &description
			}

			qurl, err := c.Update(cmd.Context(), args[0], input)
			if err != nil {
				return fmt.Errorf("update QURL: %w", err)
			}

			return opts.formatter().FormatQURL(cmd.OutOrStdout(), qurl)
		},
	}

	cmd.Flags().StringVarP(&description, "description", "d", "", "New description (use empty string to clear)")

	return cmd
}
