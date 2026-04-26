package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/shared/client"
)

func createCmd(opts *globalOpts) *cobra.Command {
	var (
		description string
		expiresIn   string
		oneTimeUse  bool
		maxSessions int
	)

	cmd := &cobra.Command{
		Use:   "create <target-url>",
		Short: "Create a qURL for a target URL",
		Example: `  qurl create https://api.example.com/data
  qurl create https://internal.example.com --expires 1h --one-time
  qurl create https://dashboard.example.com -d "Admin access" -e 7d`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateURL(args[0]); err != nil {
				return err
			}
			if err := validateDuration(expiresIn); err != nil {
				return err
			}

			c, err := opts.newClient()
			if err != nil {
				return err
			}

			input := client.CreateInput{
				TargetURL:   args[0],
				Description: description,
				ExpiresIn:   expiresIn,
				OneTimeUse:  oneTimeUse,
				MaxSessions: maxSessions,
			}

			result, err := c.Create(cmd.Context(), input)
			if err != nil {
				return fmt.Errorf("create qURL: %w", err)
			}

			if opts.quiet {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), result.QURLLink)
				return err
			}

			return opts.formatter().FormatCreate(cmd.OutOrStdout(), result)
		},
	}

	cmd.Flags().StringVarP(&description, "description", "d", "", "Description")
	cmd.Flags().StringVarP(&expiresIn, "expires", "e", "", "Expiration duration (e.g., 1h, 24h, 7d)")
	cmd.Flags().BoolVar(&oneTimeUse, "one-time", false, "Single-use token (consumed after first access)")
	cmd.Flags().IntVar(&maxSessions, "max-sessions", 0, "Maximum concurrent sessions (0 = unlimited)")

	return cmd
}
