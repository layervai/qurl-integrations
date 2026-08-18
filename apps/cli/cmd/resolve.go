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

func resolveCmd(opts *globalOpts) *cobra.Command {
	var (
		ttl time.Duration
		yes bool
	)

	cmd := &cobra.Command{
		Use:   "resolve <CRID>",
		Short: "Turn a CRID into a short-lived access link",
		Long: `Resolve a CRID into a temporary access link for the resource it names.

The link expires on its own; resolve again whenever you need a fresh one.
Before anything is printed, the CLI verifies that the service's answer
matches the CRID you asked for — a mismatched answer is discarded and the
command exits with code 12 without printing a link.

When stdout is not a terminal the command prints the bare link and nothing
else, so it composes: curl "$(qurl resolve <CRID>)".`,
		Example: "  qurl resolve " + exampleCRID + "\n" +
			"  curl \"$(qurl resolve " + exampleCRID + ")\"",
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

			client, err := opts.newClient()
			if err != nil {
				return err
			}
			result, err := client.Resolve(cmd.Context(), assessment.Input, qurlapi.ResolveOptions{
				TTLSeconds: int(ttl.Seconds()),
			})
			if err != nil {
				return err
			}

			if err := verifyResolved(assessment, result); err != nil {
				return err
			}
			reportClamp(printer, ttl, result)
			return printer.Resolve(result)
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

// verifyResolved is the fail-closed client half of the CRID trust story,
// applied before anything is emitted:
//
//   - resolved by CRID: the response must echo exactly that CRID;
//   - resolved by resource key: the response CRID must derive from the key
//     (the SDK's VerifyCRID);
//   - resolved by an operand the CLI holds no anchor for (typo-warned or
//     unknown forms): nothing to verify against, so nothing is enforced.
//
// Any failure returns a verification error: stdout stays empty and the run
// exits with code 12.
func verifyResolved(assessment *cridux.Assessment, result *qurlapi.Resolved) error {
	switch assessment.Kind {
	case cridux.KindCRID:
		if result.CRID == "" {
			return exitcode.VerificationError(msgVerifyMissing, qurl.ErrNoCRID)
		}
		if subtle.ConstantTimeCompare([]byte(result.CRID), []byte(assessment.Input)) != 1 {
			return exitcode.VerificationError(msgVerifyMismatch, qurl.ErrCRIDMismatch)
		}
		return nil
	case cridux.KindResourceKey:
		if err := result.VerifyKey(assessment.KeyDER); err != nil {
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
func reportClamp(printer *output.Printer, requested time.Duration, result *qurlapi.Resolved) {
	if requested <= 0 || result.ExpiresInSeconds <= 0 {
		return
	}
	granted := time.Duration(result.ExpiresInSeconds) * time.Second
	if granted < requested {
		printer.Notef(msgTTLClamped, granted.String(), requested.String())
	}
}
