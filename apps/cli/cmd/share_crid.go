package main

import (
	"crypto/subtle"
	"fmt"
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
		ttl time.Duration
		yes bool
	)

	cmd := &cobra.Command{
		Use:   "share <CRID>",
		// The retired verb is not an alias: cobra still rejects it with exit 2
		// and no request leaves the process. SuggestFor only makes the error
		// name the command that replaced it.
		SuggestFor: []string{"resolve"},
		Short: "Share a CRID as a short-lived access link",
		Long: `Share a CRID as a temporary access link for the resource it names.

A CRID is safe to paste anywhere — it grants nothing by itself. The share
link is what turns it into access, so treat the link as a secret. It expires
on its own; share again whenever you need a fresh one.

Before anything is printed, the CLI verifies that the service's answer
matches the CRID you asked for — a mismatched answer is discarded and the
command exits with code 12 without printing a link.

The link opens in a browser. Passing it to a tool like curl fetches the
page that opens the link, not the content itself — to download the content
from a script, use ` + "`qurl get <CRID> --file <path>`" + `.

When stdout is not a terminal the command prints the bare link and nothing
else, ready to hand out or open.`,
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

			client, err := opts.newClient(cmd.Context())
			if err != nil {
				return err
			}
			link, err := client.Share(cmd.Context(), assessment.Input, qurlapi.ShareOptions{
				TTLSeconds: int(ttl.Seconds()),
			})
			if err != nil {
				return err
			}

			if err := verifyShareLink(assessment, link); err != nil {
				return err
			}
			reportClamp(printer, ttl, link)
			return printer.ShareLink(link)
		},
	}

	cmd.Flags().DurationVar(&ttl, "ttl", 0, "requested link lifetime, e.g. 5m or 1h (service may grant less)")
	cmd.Flags().BoolVar(&yes, "yes", false, "proceed without confirmation, including sending a test CRID to production")

	return cmd
}

// applyCRIDGuards prints local warnings and applies the environment guard
// for parsed CRIDs. Warn-only cases proceed; a test CRID aimed at the
// production endpoint refuses without --yes.
func applyCRIDGuards(printer *output.Printer, assessment *cridux.Assessment, productionEndpoint, yes bool) error {
	for _, warning := range assessment.Warnings {
		printer.Warnf("%s", warning)
	}
	if assessment.Kind != cridux.KindCRID {
		return nil
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

// verifyShareLink is the fail-closed client half of the CRID trust story,
// applied before anything is emitted:
//
//   - shared by CRID: the response must echo exactly that CRID;
//   - shared by resource key: the response CRID must derive from the key
//     (the SDK's VerifyCRID);
//   - shared by an operand the CLI holds no anchor for (typo-warned or
//     unknown forms): nothing to verify against, so nothing is enforced.
//
// Any failure returns a verification error: stdout stays empty and the run
// exits with code 12.
func verifyShareLink(assessment *cridux.Assessment, link *qurlapi.ShareLink) error {
	switch assessment.Kind {
	case cridux.KindCRID:
		if link.CRID == "" {
			return exitcode.VerificationError(msgVerifyMissing, qurl.ErrNoCRID)
		}
		if subtle.ConstantTimeCompare([]byte(link.CRID), []byte(assessment.Input)) != 1 {
			return exitcode.VerificationError(msgVerifyMismatch, qurl.ErrCRIDMismatch)
		}
		return nil
	case cridux.KindResourceKey:
		if err := link.VerifyKey(assessment.KeyDER); err != nil {
			return exitcode.VerificationError(msgVerifyMismatch, err)
		}
		return nil
	case cridux.KindCRIDTypo, cridux.KindUnknown:
		return nil
	default:
		return nil
	}
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
