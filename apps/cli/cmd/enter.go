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

// Friendly, jargon-free messages surfaced to customers. These are the ONLY
// strings the `qurl enter` command may put in front of a user on error. They are
// defined once here and referenced by both the error mapping and the tests so the
// two never drift.
const (
	enterMsgNotConfigured = "Opening qURL links from the command line isn't available yet. Please check back soon."
	enterMsgOverloaded    = "The qURL service is busy right now. Please try again in a moment."
	enterMsgGeneric       = "Couldn't open this qURL link. Please try again — if it keeps failing, ask whoever shared it for a new one."
)

// enterError wraps an underlying error with a friendly, customer-facing message.
// Error() returns only the friendly text (no jargon leaks), while Unwrap() keeps
// the original error reachable so errors.Is still works against qurl-go sentinels.
type enterError struct {
	msg string
	err error
}

func (e *enterError) Error() string { return e.msg }
func (e *enterError) Unwrap() error { return e.err }

// friendlyEnterError maps a qurl-go EnterPortal failure onto a customer-facing
// message. The raw error text is never surfaced; it is preserved via Unwrap so
// errors.Is keeps working for callers/tests.
func friendlyEnterError(err error) error {
	switch {
	case errors.Is(err, qurl.ErrNotConfigured):
		return &enterError{msg: enterMsgNotConfigured, err: err}
	case errors.Is(err, qurl.ErrServerOverloaded):
		return &enterError{msg: enterMsgOverloaded, err: err}
	default:
		return &enterError{msg: enterMsgGeneric, err: err}
	}
}

func enterCmd(opts *globalOpts) *cobra.Command {
	var (
		issuerKeys []string
		relays     []string
	)

	cmd := &cobra.Command{
		Use:   "enter [link]",
		Short: "Enter a qURL link to reach what it points to",
		Long: `Enter a qURL link to securely reach the resource it points to.

Provide your qURL link as an argument, pipe it in, or run "qurl enter" and paste it when prompted:
  qurl enter '<link>'
  echo '<link>' | qurl enter
  qurl enter

qURL links are short-lived. If a link no longer works, ask whoever shared it for a new one.`,
		Example: `  qurl enter 'https://qurl.link/#...'
  echo "$LINK" | qurl enter
  qurl enter -o json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// readToken handles arg/stdin/interactive. Interactive input is masked (like
			// resolve): a qURL link carries the per-qURL secret in its fragment, so it must
			// not be echoed. We also do NOT call validateAccessToken: a qURL link is not an
			// at_ token.
			link, err := readToken(cmd, args, "qURL link (input hidden): ")
			if err != nil {
				return err
			}

			handle, err := enterPortal(cmd.Context(), link, issuerKeys, relays)
			if err != nil {
				return err
			}

			return opts.formatter().FormatEnter(cmd.OutOrStdout(), handle)
		},
	}

	cmd.Flags().StringArrayVar(&issuerKeys, "issuer-key", nil, "Issuer trust anchor as <kid>=<base64-DER P-256 SPKI> (repeatable)")
	cmd.Flags().StringArrayVar(&relays, "relay", nil, "Allowed relay origin host[:port] (repeatable)")
	// These flags are developer/diagnostic plumbing for the not-yet-deployed qv2
	// admission path. Hide them so they never appear on the customer help surface,
	// but keep them fully functional for development and tests.
	_ = cmd.Flags().MarkHidden("issuer-key")
	_ = cmd.Flags().MarkHidden("relay")

	return cmd
}

// enterPortal drives qurl-go's EnterPortal. The branch is selected purely on
// trust-flag presence: when any --issuer-key / --relay flag is set it routes to
// EnterPortalWith with a static trust config built from those flags; otherwise it
// falls through to the one-arg EnterPortal, which resolves the process-wide default
// provider and fails closed (ErrNotConfigured) until the qv2 path is configured.
//
// staticTrustConfig errors (developer-only, hidden-flag input validation) are
// returned raw so dev/diagnostic feedback stays precise; only the qurl-go
// EnterPortal/EnterPortalWith results are mapped to friendly customer messages.
func enterPortal(ctx context.Context, link string, issuerKeys, relays []string) (*qurl.ResourceHandle, error) {
	if len(issuerKeys) == 0 && len(relays) == 0 {
		handle, err := qurl.EnterPortal(ctx, link)
		if err != nil {
			return nil, friendlyEnterError(err)
		}
		return handle, nil
	}

	cfg, err := staticTrustConfig(issuerKeys, relays)
	if err != nil {
		return nil, err
	}
	handle, err := qurl.EnterPortalWith(ctx, link, cfg)
	if err != nil {
		return nil, friendlyEnterError(err)
	}
	return handle, nil
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
		if _, dup := derByKID[kid]; dup {
			return qurl.Config{}, fmt.Errorf("duplicate --issuer-key for kid %q", kid)
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

	// qv2.NewRelayAllowlist already trims+lowercases each entry and drops empties
	// (qv2/relay.go), so a padded value like " relay.host " is normalized and still
	// matches — passing the raw slice here is safe and no CLI-side trimming is needed.
	// We reject a fully empty/whitespace entry explicitly only to surface a clear CLI
	// error instead of the allowlist silently dropping it (which would mask a typo'd flag).
	for _, relay := range relays {
		if strings.TrimSpace(relay) == "" {
			return qurl.Config{}, errors.New("invalid --relay: entry must not be empty")
		}
	}

	return qurl.Config{
		TrustStore:     ts,
		RelayAllowlist: qv2.NewRelayAllowlist(relays),
	}, nil
}
