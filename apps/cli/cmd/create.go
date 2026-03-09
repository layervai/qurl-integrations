package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/shared/client"
)

func createCmd(apiKey, endpoint, format *string) *cobra.Command {
	var (
		description string
		expiresIn   string
	)

	cmd := &cobra.Command{
		Use:   "create <target-url>",
		Short: "Create a QURL for a target URL",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(apiKey, endpoint)
			if err != nil {
				return err
			}

			input := client.CreateInput{
				TargetURL: args[0],
			}
			if description != "" {
				input.Description = description
			}
			if expiresIn != "" {
				input.ExpiresIn = expiresIn
			}

			qurl, err := c.Create(cmd.Context(), input)
			if err != nil {
				return fmt.Errorf("create QURL: %w", err)
			}

			f := getFormatter(format)
			return f.FormatQURL(cmd.OutOrStdout(), qurl)
		},
	}

	cmd.Flags().StringVarP(&description, "description", "d", "", "Description")
	cmd.Flags().StringVarP(&expiresIn, "expires", "e", "", "Expiration duration (e.g., 1h, 24h, 168h)")

	return cmd
}
