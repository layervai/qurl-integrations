package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/shared/client"
)

func resolveCmd(apiKey, endpoint, format *string) *cobra.Command {
	return &cobra.Command{
		Use:   "resolve <access-token>",
		Short: "Resolve a QURL access token (headless)",
		Long: `Resolve a QURL access token to get the target URL and open firewall access.
After resolution, the target URL is accessible from your IP for the duration
specified in the access grant.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(apiKey, endpoint)
			if err != nil {
				return err
			}

			result, err := c.Resolve(cmd.Context(), client.ResolveInput{
				AccessToken: args[0],
			})
			if err != nil {
				return fmt.Errorf("resolve QURL: %w", err)
			}

			f := getFormatter(format)
			return f.FormatResolve(cmd.OutOrStdout(), result)
		},
	}
}
