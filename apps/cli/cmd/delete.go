package main

import (
	"bufio"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

func deleteCmd(opts *globalOpts) *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "delete <CRID>",
		Short: "Delete a published resource",
		Long: `Delete a published resource by its CRID.

Deletion cannot be undone: the CRID stops resolving, and republishing the
same target later mints a different CRID. Interactive runs confirm first;
scripts and pipelines must pass --yes.`,
		Example: "  qurl delete " + exampleCRID + "\n" +
			"  qurl delete " + exampleCRID + " --yes",
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

			if !yes {
				confirmed, err := confirmDelete(opts, assessment.Input)
				if err != nil {
					return err
				}
				if !confirmed {
					printer.Notef(msgDeleteCanceled)
					return nil
				}
			}

			client, err := opts.newClient()
			if err != nil {
				return err
			}
			result, err := client.Delete(cmd.Context(), assessment.Input)
			if err != nil {
				return err
			}
			if result.AlreadyGone {
				// Idempotent delete: already-gone is the requested outcome.
				printer.Notef(msgAlreadyGone)
			}
			return printer.Delete(assessment.Input, result.AlreadyGone)
		},
	}

	cmd.Flags().BoolVar(&yes, "yes", false, "proceed without confirmation, including sending a test CRID to production")

	return cmd
}

// confirmDelete asks on the terminal. Without a terminal it refuses instead
// of hanging a pipeline: non-interactive deletion requires --yes.
func confirmDelete(opts *globalOpts, id string) (bool, error) {
	if !opts.streams.InIsTTY {
		return false, exitcode.UsageError(errors.New(msgNeedsYes))
	}
	// The prompt is conversation, not data: it goes to stderr.
	if _, err := fmt.Fprintf(opts.streams.Err, "Delete %s? This cannot be undone. [y/N] ", id); err != nil {
		return false, err
	}
	scanner := bufio.NewScanner(opts.streams.In)
	if !scanner.Scan() {
		return false, nil
	}
	answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
	return answer == "y" || answer == "yes", nil
}
