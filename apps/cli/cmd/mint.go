package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/shared/client"
)

func mintCmd(opts *globalOpts) *cobra.Command {
	var (
		expiresIn       string
		expiresAt       string
		label           string
		oneTimeUse      bool
		maxSessions     int
		sessionDuration string
	)

	cmd := &cobra.Command{
		Use:   "mint <resource-id>",
		Short: "Mint a new access link for a qURL",
		Long: `Creates a new access token and link for an existing qURL resource.
Useful for multi-use qURLs where you want to generate additional access links.

Note: AccessPolicy overrides (geo-fencing, IP restrictions) are only available
via the SDK or API; the CLI exposes the common per-link flags only.`,
		Example: `  qurl mint r_k8xqp9h2sj9
  qurl mint r_k8xqp9h2sj9 --expires-in 1h --one-time
  LINK=$(qurl mint r_k8xqp9h2sj9 -q)`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: resourceIDCompletion(opts),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateResourceID(args[0]); err != nil {
				return err
			}
			if err := validateDuration(expiresIn); err != nil {
				return err
			}
			if err := validateDuration(sessionDuration); err != nil {
				return err
			}

			c, err := opts.newClient()
			if err != nil {
				return err
			}

			var input *client.MintLinkInput
			// Only build a MintLinkInput when the caller explicitly set at least one flag.
			// Keeping input nil causes MintLink to send http.NoBody, which is a meaningful
			// wire-level difference: the server uses a bodiless POST to mean "mint with
			// the QURL's own defaults". Always building the struct (even with all zero
			// values and omitempty fields) would send an empty JSON object instead.
			hasInput := cmd.Flags().Changed("expires-in") ||
				cmd.Flags().Changed("expires-at") ||
				cmd.Flags().Changed("label") ||
				cmd.Flags().Changed("one-time") ||
				cmd.Flags().Changed("max-sessions") ||
				cmd.Flags().Changed("session-duration")

			if hasInput {
				// String and duration fields are assigned unconditionally: omitempty strips
				// empty strings cleanly. Bool/int fields use pointer types (*bool/*int) so
				// an explicit --one-time=false or --max-sessions=0 can be sent to the server
				// as an explicit override rather than being silently dropped. Only build the
				// pointer when the flag was actually set; a nil pointer omits the field.
				input = &client.MintLinkInput{
					ExpiresIn:       expiresIn,
					Label:           label,
					SessionDuration: sessionDuration,
				}
				if cmd.Flags().Changed("one-time") {
					input.OneTimeUse = &oneTimeUse
				}
				if cmd.Flags().Changed("max-sessions") {
					input.MaxSessions = &maxSessions
				}
				if cmd.Flags().Changed("expires-at") {
					t, parseErr := time.Parse(time.RFC3339, expiresAt)
					if parseErr != nil {
						return fmt.Errorf("invalid --expires-at value: %w", parseErr)
					}
					input.ExpiresAt = &t
				}
			}

			result, err := c.MintLink(cmd.Context(), args[0], input)
			if err != nil {
				return fmt.Errorf("mint link: %w", err)
			}

			if opts.quiet {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), result.QURLLink)
				return err
			}

			return opts.formatter().FormatMint(cmd.OutOrStdout(), result)
		},
	}

	cmd.Flags().StringVar(&expiresIn, "expires-in", "", "Link expiration duration (e.g., 1h, 24h)")
	cmd.Flags().StringVar(&expiresAt, "expires-at", "", "Link expiration time (RFC3339)")
	cmd.Flags().StringVar(&label, "label", "", "Label for the minted link")
	cmd.Flags().BoolVar(&oneTimeUse, "one-time", false, "Single-use link")
	cmd.Flags().IntVar(&maxSessions, "max-sessions", 0, "Maximum concurrent sessions")
	cmd.Flags().StringVar(&sessionDuration, "session-duration", "", "Session duration (e.g., 30m, 1h)")

	return cmd
}
