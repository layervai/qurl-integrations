package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-go/qurl"
)

func enterCmd(opts *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "enter [qv2-link]",
		Short: "Enter a qURL portal (qv2 link)",
		Long: `Enter a qURL portal by opening a qv2 link end to end.

Unlike "resolve" (which exchanges an at_ access token with the qURL API), "enter"
takes a self-contained qv2 link (#qv2.<claims>.<secret>.<sig>) and drives the full
client-side flow via qurl-go: verify the issuer signature locally, validate the
relay, then knock the relay so the target firewall opens for your IP.

The qv2 link can be provided as an argument, via stdin, or interactively:
  qurl enter '<qv2-link>'           Argument (visible in shell history)
  echo "$LINK" | qurl enter         Stdin (safer)
  qurl enter                        Interactive prompt (hidden input)

Note: the qv2 admission contract is not yet deployed. Without trust anchors
configured for the qv2 path, "enter" fails closed (the portal is not configured).`,
		Example: `  qurl enter '#qv2.<claims>.<secret>.<sig>'
  echo "$LINK" | qurl enter
  qurl enter -o json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// readToken handles arg/stdin/interactive. We intentionally do NOT call
			// validateAccessToken here: a qv2 link is not an at_ token.
			link, err := readToken(cmd, args)
			if err != nil {
				return err
			}

			handle, err := qurl.EnterPortal(cmd.Context(), link)
			if err != nil {
				return fmt.Errorf("enter qURL portal: %w", err)
			}

			return opts.formatter().FormatEnter(cmd.OutOrStdout(), handle)
		},
	}
}
