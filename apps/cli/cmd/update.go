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
		Short: "Update a qURL's properties",
		Long: `Updates mutable properties of a qURL.

Note: The --description flag updates the resource-level description (maps to the API's
"description" field on UpdateQurlRequest). This is intentionally different from
"qurl create --label", which sets the qURL token label on creation.`,
		Example: `  qurl update r_k8xqp9h2sj9 --description "Production API access"
  qurl update r_k8xqp9h2sj9 -d ""  # clear description
  qurl update r_k8xqp9h2sj9 --tags prod,api --extend-by 24h`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: resourceIDCompletion(opts),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateResourceID(args[0]); err != nil {
				return err
			}
			if cmd.Flags().Changed("extend-by") {
				if err := validateDuration(extendBy); err != nil {
					return err
				}
			}
			if cmd.Flags().Changed("expires-at") {
				if err := validateRFC3339("expires-at", expiresAt); err != nil {
					return err
				}
			}

			hasChange := cmd.Flags().Changed("description") ||
				cmd.Flags().Changed("tags") ||
				cmd.Flags().Changed("extend-by") ||
				cmd.Flags().Changed("expires-at")
			if !hasChange {
				return errors.New("must set at least one of: --description, --tags, --extend-by, --expires-at")
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
				t, _ := time.Parse(time.RFC3339, expiresAt) // already validated above
				input.ExpiresAt = &t
			}

			qurl, err := c.Update(cmd.Context(), args[0], input)
			if err != nil {
				return fmt.Errorf("update qURL: %w", err)
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
