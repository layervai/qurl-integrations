package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

const (
	// msgOriginHeaderPrompt asks for the value at a hidden prompt; the
	// operand is the header name, already checked to be a plain token.
	msgOriginHeaderPrompt = "Value for %s (input hidden): "
	// msgNoOriginHeaderValue is the empty-input error.
	msgNoOriginHeaderValue = "no origin header value provided; pipe it on standard input or type it at the prompt"
	// msgOriginHeaderLocal refuses the flag for a local app, which has no
	// remote target URL for the header to be sent to.
	msgOriginHeaderLocal = "--origin-header applies only when publishing a remote URL"
)

// originHeaderHelp is the part of the help text that set and the group share.
const originHeaderHelp = `An origin header is a header qURL adds to every request it forwards to the
target URL of a remote URL resource. Your origin can then refuse every request
that does not carry it, so it serves only people who came through qURL. For
example, a Vercel deployment with Deployment Protection admits requests that
carry its bypass secret in x-vercel-protection-bypass. An Authorization header
works for an origin that uses basic or bearer authentication; it replaces the
visitor's own.

The value is a secret. It is never an argument, so it stays out of shell
history and process lists: pipe it on standard input, or type it at the hidden
prompt. qURL never shows it again; commands show only the header name.

To rotate the secret without an outage, add the new secret at your origin
first, then set it here, then remove the old secret from your origin a few
minutes later: qURL may keep sending the old value for about a minute.`

func originHeaderCmd(opts *globalOpts) *cobra.Command {
	root := &cobra.Command{
		Use:   "origin-header",
		Short: "Set or remove the secret header qURL sends to a remote URL's origin",
		Long:  originHeaderHelp,
		Example: `  printf '%s' "$VERCEL_BYPASS_SECRET" | qurl origin-header set <CRID> x-vercel-protection-bypass
  qurl origin-header clear <CRID>`,
	}
	set := &cobra.Command{
		Use:   "set <CRID> <name>",
		Short: "Set or rotate the origin header of a remote URL resource",
		Long: originHeaderHelp + `

Setting a header replaces the one the resource has. "qurl status <CRID>" shows
its name.`,
		Example: `  printf '%s' "$VERCEL_BYPASS_SECRET" | qurl origin-header set <CRID> x-vercel-protection-bypass
  qurl origin-header set <CRID> Authorization`,
		Args: exactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			header, err := readOriginHeader(opts, args[1])
			if err != nil {
				return err
			}
			client, err := opts.newClient(cmd.Context())
			if err != nil {
				return err
			}
			resource, err := client.SetOriginHeader(cmd.Context(), args[0], header)
			if err != nil {
				return err
			}
			return opts.printer().OriginHeader(resource)
		},
	}
	clearCmd := &cobra.Command{
		Use:   "clear <CRID>",
		Short: "Remove the origin header of a remote URL resource",
		Long: `Remove the origin header, so qURL no longer adds it to requests it forwards
to the target. Remove the secret from your origin's checks first if the origin
should keep serving people who come through qURL.`,
		Example: "  qurl origin-header clear <CRID>",
		Args:    exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := opts.newClient(cmd.Context())
			if err != nil {
				return err
			}
			resource, err := client.SetOriginHeader(cmd.Context(), args[0], nil)
			if err != nil {
				return err
			}
			return opts.printer().OriginHeader(resource)
		},
	}
	root.AddCommand(set, clearCmd)
	return root
}

// readOriginHeader checks the header name, then reads the value from piped
// stdin or a hidden prompt and checks it, before any request. The value is
// kept exactly as given, less its line ending: it is compared byte for byte
// at the origin, so a value with surrounding space is refused, never trimmed.
// No error names the value.
func readOriginHeader(opts *globalOpts, name string) (*qurlapi.OriginHeader, error) {
	if err := qurlapi.ValidateOriginHeaderName(name); err != nil {
		return nil, exitcode.UsageError(err)
	}
	value, err := readSecret(opts, secretInput{
		prompt:  fmt.Sprintf(msgOriginHeaderPrompt, name),
		missing: msgNoOriginHeaderValue,
		exact:   true,
	})
	if err != nil {
		return nil, err
	}
	if err := qurlapi.ValidateOriginHeaderValue(value); err != nil {
		return nil, exitcode.UsageError(err)
	}
	return &qurlapi.OriginHeader{Name: name, Value: value}, nil
}

// errOriginHeaderLocal is the refusal of --origin-header for a local app.
var errOriginHeaderLocal = errors.New(msgOriginHeaderLocal)
