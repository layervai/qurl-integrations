package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
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

			client, err := opts.newClient(cmd.Context())
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
			// The service deletion is already committed. Local convergence cannot
			// roll it back, so report the requested outcome as success and make any
			// incomplete local cleanup explicit as a warning.
			cleanupErr := cleanupDeletedLocalShare(cmd.Context(), opts, assessment.Input)
			if err := printer.Delete(assessment.Input, result.AlreadyGone); err != nil {
				return err
			}
			if cleanupErr != nil {
				printer.Warnf("The resource was deleted, but local sharing cleanup did not finish: %v", cleanupErr)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&yes, "yes", false, "proceed without confirmation, including sending a test CRID to production")

	return cmd
}

// cleanupDeletedLocalShare converges local desired state after the service has
// committed deletion. It signals an existing daemon through IPC but never
// installs or starts one.
func cleanupDeletedLocalShare(ctx context.Context, opts *globalOpts, id string) error {
	stateDir, err := opts.resolveShareStateDir("")
	if err != nil {
		if errors.Is(err, connectorstate.ErrNoDefaultStateDir) {
			return nil
		}
		return err
	}
	_, present, err := connectorstate.ReadLocalSharesIfPresent(ctx, stateDir)
	if err != nil || !present {
		return err
	}
	registry, err := opts.openShareRegistry(stateDir)
	if err != nil {
		return err
	}
	local, err := registry.Get(ctx, id)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := registry.Delete(ctx, local.ResourceID); err != nil {
		return err
	}
	logDir, err := connectordaemon.DefaultLogDir(stateDir)
	if err != nil {
		return err
	}
	// Reload through the platform controller instead of probing for a Unix
	// socket file. On Windows the same logical address maps to a named pipe and
	// has no filesystem entry. ReloadIfRunning is side-effect free when no
	// daemon exists: it neither installs nor starts a background job.
	_, err = opts.newShareDaemon(stateDir, logDir).ReloadIfRunning(ctx)
	return err
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
		if err := scanner.Err(); err != nil {
			return false, fmt.Errorf("read delete confirmation: %w", err)
		}
		return false, nil
	}
	answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
	return answer == "y" || answer == "yes", nil
}
