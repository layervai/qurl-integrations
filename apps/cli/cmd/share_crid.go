package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-go/qurl"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

// shareCmd is the CRID share operator: it turns a CRID into a fresh,
// short-lived share link. The command's former name, `resolve`, is gone —
// a hard cutover with no alias.
func shareCmd(opts *globalOpts) *cobra.Command {
	var (
		ttl             time.Duration
		sessionDuration time.Duration
		yes             bool
	)

	cmd := &cobra.Command{
		Use: "share <CRID>",
		// The retired verb is not an alias: cobra still rejects it with exit 2
		// and no request leaves the process. SuggestFor only makes the error
		// name the command that replaced it.
		SuggestFor: []string{"resolve"},
		Short:      "Share a CRID as a short-lived access link",
		Long: `Share a CRID as a temporary access link for the resource it names.

share uses this device's identity. It works on the resource owner's devices
and, for a private resource, on the devices the publisher allowed; any other
device gets "not found".
The link expires on its own; share again whenever you need a fresh one.

Before anything is printed, the CLI verifies that the link belongs
to the CRID you asked for — a mismatched answer is discarded and the
command exits with code 12 without printing a link.

The link opens in a browser. Passing it to a tool like curl fetches the
page that opens the link, not the content itself — to download the content
from a script, use ` + "`qurl get <CRID> --file <path>`" + `.

The CLI also shows who published the resource and when it was created. The
publisher name is self-declared and shown as UNVERIFIED: LayerV has not
confirmed who the publisher is, so treat the name as a claim, not as proof.

When stdout is not a terminal the command prints the bare link on stdout,
ready to hand out or open, and reports the publisher in one line on stderr.
--quiet prints only the link.`,
		Example: "  qurl share " + exampleCRID + "\n" +
			"  qurl get " + exampleCRID + " --file report.pdf   # download instead of linking",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			assessment, err := cridux.Assess(args[0])
			if err != nil {
				return err
			}
			printer := opts.printer()
			if err := applyCRIDGuards(printer, assessment, opts.productionEndpoint(), yes); err != nil {
				return err
			}

			// The wire carries whole seconds; anything finer would silently
			// truncate — and reportClamp would then misattribute the CLI's
			// own rounding to the service. Refuse instead: a requested
			// lifetime is never silently altered.
			if ttl != 0 && (ttl < 0 || ttl != ttl.Truncate(time.Second)) {
				return exitcode.UsageError(fmt.Errorf("--ttl %s must be a positive whole number of seconds", ttl))
			}

			if sessionDuration != 0 && (sessionDuration < 0 || sessionDuration%time.Second != 0) {
				return exitcode.UsageError(fmt.Errorf("--session-duration %s must be a positive whole number of seconds", sessionDuration))
			}

			link, err := opts.shareResource(cmd.Context(), assessment.Input, qurlapi.ShareOptions{
				TTLSeconds: int(ttl.Seconds()), SessionDurationSeconds: int(sessionDuration / time.Second),
			})

			if err := verifyShareLink(assessment, link, err); err != nil {
				return err
			}
			if err := opts.verifyLink(cmd.Context(), link.QURL, assessment.Input); err != nil {
				return err
			}
			reportClamp(printer, ttl, link)
			return printer.ShareLink(link)
		},
	}

	cmd.Flags().DurationVar(&ttl, "ttl", 0, "requested link lifetime, e.g. 5m or 1h (service may grant less)")
	cmd.Flags().DurationVar(&sessionDuration, "session-duration", 0, "session lifetime after access starts, e.g. 5m or 1h (default: service policy)")
	cmd.Flags().BoolVar(&yes, "yes", false, "proceed without confirmation, including sending a test CRID to production")

	return cmd
}

// applyCRIDGuards requires a valid CRID and applies the environment guard.
func applyCRIDGuards(printer *output.Printer, assessment *cridux.Assessment, productionEndpoint, yes bool) error {
	if err := requireCRID(assessment); err != nil {
		return err
	}
	warning, err := cridux.EnvironmentGuard(assessment.CRID.Environment(), productionEndpoint, yes)
	if err != nil {
		return err
	}
	if warning != "" {
		printer.Warnf("%s", warning)
	}
	return nil
}

// requireCRID refuses an operand that is not a CRID, with the most specific
// reason the assessment has. It is the whole guard for a command that only
// reads a resource: the environment guard is for a request that acts on one.
func requireCRID(assessment *cridux.Assessment) error {
	if assessment.Kind == cridux.KindCRID {
		return nil
	}
	if assessment.Kind == cridux.KindResourceKey {
		return exitcode.InvalidInputError("public keys are verification data; use the resource's CRID", cridux.ErrUnusableID)
	}
	if len(assessment.Warnings) > 0 {
		return exitcode.InvalidInputError(strings.Join(assessment.Warnings, " "), cridux.ErrUnusableID)
	}
	return errValidCRIDRequired()
}

// errValidCRIDRequired is the refusal for an operand that is not a CRID this
// client accepts, when there is nothing more specific to say about it.
func errValidCRIDRequired() error {
	return exitcode.InvalidInputError(msgValidCRIDRequired, cridux.ErrUnusableID)
}

// verifyShareLink rejects unverified responses before any link is used or printed.
func verifyShareLink(assessment *cridux.Assessment, link *qurlapi.ShareLink, err error) error {
	if errors.Is(err, qurl.ErrNoCRID) {
		return exitcode.VerificationError(msgVerifyMissing, err)
	}
	if errors.Is(err, qurl.ErrCRIDMismatch) {
		return exitcode.VerificationError(msgVerifyMismatch, err)
	}
	if err != nil {
		return err
	}
	if link == nil || link.CRID == "" {
		return exitcode.VerificationError(msgVerifyMissing, qurl.ErrNoCRID)
	}
	if subtle.ConstantTimeCompare([]byte(link.CRID), []byte(assessment.Input)) != 1 {
		return exitcode.VerificationError(msgVerifyMismatch, qurl.ErrCRIDMismatch)
	}
	return nil
}

// reportClamp tells the user when the service granted a shorter lifetime
// than --ttl requested (clamp-and-report, never silent).
func reportClamp(printer *output.Printer, requested time.Duration, link *qurlapi.ShareLink) {
	if requested <= 0 || link.ExpiresInSeconds <= 0 {
		return
	}
	granted := time.Duration(link.ExpiresInSeconds) * time.Second
	if granted < requested {
		printer.Notef(msgTTLClamped, granted.String(), requested.String())
	}
}

// shareResource is the one share path for share and get. It always goes
// through the registered-device client, so every share request carries this
// device's credential and none is ever sent without one: a device that
// cannot be opened fails here, before the share route is contacted, and the
// error says that sharing needed the device before it gives the cause.
//
// TODO(upstream-contract): qurl-service decides who may share. It mints a
// link on the resource owner's devices and, for a private resource, on the
// devices the publisher allowed, and it answers any other device 404. The
// help, the README, and the share not-found hint all state that rule.
func (opts *globalOpts) shareResource(ctx context.Context, id string, options qurlapi.ShareOptions) (*qurlapi.ShareLink, error) {
	client, err := opts.newClient(ctx)
	if err != nil {
		return nil, fmt.Errorf(msgShareNeedsDevice, err)
	}
	return client.Share(ctx, id, options)
}
