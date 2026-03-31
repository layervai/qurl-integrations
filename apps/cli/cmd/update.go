package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/shared/client"
)

func updateCmd(opts *globalOpts) *cobra.Command {
	var (
		description string
		tags        []string
		extendBy    string
		expiresAt   string
	)

	cmd := &cobra.Command{
		Use:   "update <resource-id>",
		Short: "Update a QURL's properties",
		Example: `  qurl update r_k8xqp9h2sj9 --description "Production API access"
  qurl update r_k8xqp9h2sj9 -d ""  # clear description
  qurl update r_k8xqp9h2sj9 --tags prod,api --extend-by 24h`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: resourceIDCompletion(opts),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateResourceID(args[0]); err != nil {
				return err
			}

			hasChange := cmd.Flags().Changed("description") ||
				cmd.Flags().Changed("tags") ||
				cmd.Flags().Changed("extend-by") ||
				cmd.Flags().Changed("expires-at")
			if !hasChange {
				return errors.New("at least one flag must be set (e.g., --description, --tags, --extend-by, --expires-at)")
			}

			c, err := opts.newClient()
			if err != nil {
				return err
			}

			input := client.UpdateInput{}
			if cmd.Flags().Changed("description") {
				input.Description = &description
			}
			if cmd.Flags().Changed("tags") {
				input.Tags = &tags
			}
			if cmd.Flags().Changed("extend-by") {
				input.ExtendBy = extendBy
			}
			if cmd.Flags().Changed("expires-at") {
				t, parseErr := time.Parse(time.RFC3339, expiresAt)
				if parseErr != nil {
					return fmt.Errorf("invalid --expires-at value: %w", parseErr)
				}
				input.ExpiresAt = &t
			}

			qurl, err := c.Update(cmd.Context(), args[0], input)
			if err != nil {
				return fmt.Errorf("update QURL: %w", err)
			}

			return opts.formatter().FormatQURL(cmd.OutOrStdout(), qurl)
		},
	}

	cmd.Flags().StringVarP(&description, "description", "d", "", "New description (use empty string to clear)")
	cmd.Flags().StringSliceVar(&tags, "tags", nil, "Tags (comma-separated, e.g., prod,api)")
	cmd.Flags().StringVar(&extendBy, "extend-by", "", "Extend expiration by duration (e.g., 24h, 7d)")
	cmd.Flags().StringVar(&expiresAt, "expires-at", "", "Set exact expiration time (RFC3339)")

	return cmd
}
