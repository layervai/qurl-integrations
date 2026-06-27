package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-go/qurl"
	"github.com/layervai/qurl-go/qv2"
)

func enterCmd(opts *globalOpts) *cobra.Command {
	var (
		issuerKeys []string
		relays     []string
	)

	cmd := &cobra.Command{
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

Trust configuration (required — the path fails closed without it):
  --issuer-key <kid>=<base64-DER>   Issuer trust anchor (P-256 SPKI DER, std base64). Repeatable.
  --relay <host[:port]>             Allowed relay origin. Repeatable.

Note: the qv2 admission contract is not yet deployed. Without trust anchors
configured for the qv2 path, "enter" fails closed (the portal is not configured).`,
		Example: `  qurl enter '#qv2.<claims>.<secret>.<sig>' --issuer-key k1=<base64-DER> --relay relay.qurl.link
  echo "$LINK" | qurl enter --issuer-key k1=<base64-DER> --relay relay.qurl.link
  qurl enter -o json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// readToken handles arg/stdin/interactive. We intentionally do NOT call
			// validateAccessToken here: a qv2 link is not an at_ token.
			link, err := readToken(cmd, args)
			if err != nil {
				return err
			}

			handle, err := enterPortal(cmd.Context(), link, issuerKeys, relays)
			if err != nil {
				return fmt.Errorf("enter qURL portal: %w", err)
			}

			return opts.formatter().FormatEnter(cmd.OutOrStdout(), handle)
		},
	}

	cmd.Flags().StringArrayVar(&issuerKeys, "issuer-key", nil, "Issuer trust anchor as <kid>=<base64-DER P-256 SPKI> (repeatable)")
	cmd.Flags().StringArrayVar(&relays, "relay", nil, "Allowed relay origin host[:port] (repeatable)")

	return cmd
}

// enterPortal drives qurl-go's EnterPortal. When explicit trust anchors are
// supplied (--issuer-key / --relay) it builds a Static provider Config and calls
// EnterPortalWith; otherwise it falls through to the one-arg EnterPortal, which
// resolves the process-wide default provider and fails closed (ErrNotConfigured)
// until the qv2 path is configured.
func enterPortal(ctx context.Context, link string, issuerKeys, relays []string) (*qurl.ResourceHandle, error) {
	if len(issuerKeys) == 0 && len(relays) == 0 {
		return qurl.EnterPortal(ctx, link)
	}

	cfg, err := staticTrustConfig(issuerKeys, relays)
	if err != nil {
		return nil, err
	}
	return qurl.EnterPortalWith(ctx, link, cfg)
}

// staticTrustConfig builds an EnterPortal Config from CLI-supplied issuer keys
// (repeatable "<kid>=<base64-DER>") and relay allowlist entries.
func staticTrustConfig(issuerKeys, relays []string) (qurl.Config, error) {
	if len(issuerKeys) == 0 {
		return qurl.Config{}, errors.New("at least one --issuer-key is required when configuring trust anchors")
	}
	derByKID := make(map[string][]byte, len(issuerKeys))
	for _, raw := range issuerKeys {
		kid, encoded, ok := strings.Cut(raw, "=")
		if !ok || kid == "" || encoded == "" {
			return qurl.Config{}, fmt.Errorf("invalid --issuer-key %q: expected <kid>=<base64-DER>", raw)
		}
		der, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return qurl.Config{}, fmt.Errorf("invalid --issuer-key %q: bad base64: %w", kid, err)
		}
		derByKID[kid] = der
	}
	ts, err := qv2.NewTrustStoreFromDER(derByKID)
	if err != nil {
		return qurl.Config{}, fmt.Errorf("build trust store: %w", err)
	}
	return qurl.Config{
		TrustStore:     ts,
		RelayAllowlist: qv2.NewRelayAllowlist(relays),
	}, nil
}
