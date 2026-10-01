package main

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/layervai/qurl-go/qurl"
	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

// publisherCmd shows and changes the publisher profile of this device's
// owner: the self-declared name recipients see beside every CRID that owner
// published. It uses the same registered device identity as whoami and never
// asks for an account or an API key.
func publisherCmd(opts *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "publisher",
		Short: "Show or change the publisher name shown with your CRIDs",
		Long: `Show the publisher profile people see with your CRIDs.

Anyone who requests a link for one of your CRIDs with "qurl share" or
"qurl get" is shown who published it: the name you set here, or
"no name provided" when you set none.

The name is optional and self-declared. LayerV does not check it, and no
publisher can be confirmed by LayerV yet, so every publisher is shown as
UNVERIFIED.

The same name appears on every CRID this device's owner publishes, so people
can tell those CRIDs come from the same publisher.

Plain --quiet prints only the name, and nothing when none is set.`,
		Example: `  qurl publisher
  qurl publisher set "Acme Docs"
  qurl publisher clear
  qurl publisher -o json`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := opts.newClient(cmd.Context())
			if err != nil {
				return err
			}
			profile, err := client.Publisher(cmd.Context())
			if err != nil {
				return publisherRouteError(err)
			}
			return printPublisherProfile(opts, profile, output.PublisherShown)
		},
	}
	cmd.AddCommand(publisherSetCmd(opts), publisherClearCmd(opts))
	return cmd
}

func publisherSetCmd(opts *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "set <name>",
		Short: "Set the publisher name shown with your CRIDs",
		// TODO(upstream-contract): the naming rules summarized here are
		// qurl-service's. The service is the authority and explains a refusal;
		// this text only helps someone pick a name that will be accepted.
		Long: `Set the publisher name people see with your CRIDs.

The name is self-declared: setting it does not confirm who you are, and it is
shown as UNVERIFIED to anyone who requests a link for one of your CRIDs.

A name is 1 to 64 characters: letters, digits, single spaces, and common
punctuation. It cannot spell "verified", "LayerV" or "qURL", even split by
spaces or punctuation. The qURL service checks the name and says why when it
refuses one. Quote a name that has spaces.

Setting a name replaces the previous one. The same name appears on every CRID
this device's owner publishes, including ones already published, so people
can tell those CRIDs come from the same publisher.`,
		Example: `  qurl publisher set "Acme Docs"
  qurl publisher set Acme -o json`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] == "" {
				return exitcode.UsageError(errors.New(msgPublisherSetNeedsName))
			}
			client, err := opts.newClient(cmd.Context())
			if err != nil {
				return err
			}
			profile, err := client.SetPublisherName(cmd.Context(), args[0])
			if err != nil {
				return publisherNameError(opts.printer(), err)
			}
			return printPublisherProfile(opts, profile, output.PublisherNameSet)
		},
	}
}

func publisherClearCmd(opts *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "clear",
		Short: "Remove the publisher name shown with your CRIDs",
		Long: `Remove the publisher name. People who request a link for one of your
CRIDs are then shown "no name provided", still marked UNVERIFIED.`,
		Example: "  qurl publisher clear",
		Args:    noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := opts.newClient(cmd.Context())
			if err != nil {
				return err
			}
			// The service removes the name when asked to set an empty one.
			profile, err := client.SetPublisherName(cmd.Context(), "")
			if err != nil {
				// A refused removal is the same 400 a refused name is, so it
				// gets the same rendering and exit code.
				return publisherNameError(opts.printer(), err)
			}
			return printPublisherProfile(opts, profile, output.PublisherNameCleared)
		},
	}
}

func printPublisherProfile(opts *globalOpts, profile *qurlapi.Publisher, change output.PublisherChange) error {
	if profile == nil {
		return fmt.Errorf("%w: publisher profile is empty", qurl.ErrInvalidAPIResponse)
	}
	return opts.printer().PublisherProfile(profile, change)
}

// publisherUnsupportedError replaces the generic not-found rendering, whose
// hint is about a mistyped CRID, when the endpoint has no publisher profile
// at all. The service problem stays in the chain, so the exit code is still
// the not-found row.
type publisherUnsupportedError struct{ cause error }

func (e *publisherUnsupportedError) Error() string       { return msgPublisherUnsupported }
func (e *publisherUnsupportedError) UserMessage() string { return msgPublisherUnsupported }
func (e *publisherUnsupportedError) Unwrap() error       { return e.cause }

// publisherRouteError explains a 404 from the publisher profile itself.
//
// TODO(upstream-contract): a qurl-service that predates publisher metadata
// answers 404 for /v1/me/publisher. The profile of an existing owner is never
// missing, so on these commands a 404 can only mean an older service.
func publisherRouteError(err error) error {
	var apiErr *qurlapi.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return &publisherUnsupportedError{cause: err}
	}
	return err
}

// publisherNameError turns a refused publisher name into the invalid-input
// outcome (exit 8) with the reason on stderr. The message is built from the
// service's explanation only: the argument itself is never echoed, and the
// explanation is sanitized and bounded (Printer.ServiceReason) before it can
// reach the terminal. Every other failure goes through publisherRouteError.
func publisherNameError(printer *output.Printer, err error) error {
	if !errors.Is(err, qurl.ErrInvalidPublisherName) {
		return publisherRouteError(err)
	}
	reason := printer.ServiceReason(qurlapi.PublisherNameReason(err))
	if reason == "" {
		return exitcode.InvalidInputError(msgPublisherNameRefused, err)
	}
	return exitcode.InvalidInputError(fmt.Sprintf(msgPublisherNameRefusedReason, reason), err)
}
