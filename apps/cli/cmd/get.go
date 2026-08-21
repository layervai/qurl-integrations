package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/consume"
	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

// getFlags carries get's flag values into the run helpers.
type getFlags struct {
	file  string
	force bool
	yes   bool
}

// getCmd is the Phase-1 consume command: resolve a CRID, verify the answer
// against it exactly like `qurl resolve` does, and only then act — open the
// verified link in the browser on a terminal, or download its bytes with
// --file. The action decision is local and precedes every network call.
func getCmd(opts *globalOpts) *cobra.Command {
	var flags getFlags

	cmd := &cobra.Command{
		Use:   "get <CRID>",
		Short: "Fetch what a CRID points to",
		Long: `Fetch the content behind a CRID.

The CRID is resolved into a fresh access link and the answer is verified
against the CRID you asked for — a mismatch is discarded and the command
exits with code 12 before anything happens. Only a verified link is ever
acted on:

  - On a terminal, get opens the link in your browser (set QURL_BROWSER or
    BROWSER to choose which one).
  - With --file <path> it downloads to that path instead. The download is
    atomic: bytes arrive in <path>.part, which becomes <path> only when the
    download completes. Existing files are never replaced unless --force is
    given, and an access link that expires mid-download is refreshed and
    retried once automatically.
  - With --file - the raw bytes stream to stdout, clean for piping.

When stdout is not a terminal, get never opens a browser: pass --file, or
use ` + "`qurl resolve`" + ` if you only need the link.`,
		Example: "  qurl get " + exampleCRID + "\n" +
			"  qurl get " + exampleCRID + " --file report.pdf\n" +
			"  qurl get " + exampleCRID + " --file - | shasum",
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("file") && flags.file == "" {
				return exitcode.UsageError(errors.New(msgFileNeedsPath))
			}
			if flags.file == "-" && opts.resolvedFormat == output.FormatJSON {
				// The bytes are the output; a JSON document would corrupt
				// them. Refuse loudly rather than silently ignoring either
				// flag.
				return exitcode.UsageError(errors.New(msgFileDashJSON))
			}
			if flags.file == "" && opts.resolvedFormat == output.FormatJSON {
				// JSON mode is a machine asking for data; spawning a
				// browser as a side effect would surprise it — the same
				// refuse-loudly principle as --file - above.
				return exitcode.UsageError(errors.New(msgBrowserJSON))
			}
			return runGet(cmd.Context(), opts, args[0], flags)
		},
	}

	cmd.Flags().StringVar(&flags.file, "file", "", "download to this path instead of opening a browser (\"-\" = raw bytes to stdout)")
	cmd.Flags().BoolVar(&flags.force, "force", false, "allow --file to replace an existing file")
	cmd.Flags().BoolVar(&flags.yes, "yes", false, "proceed without confirmation, including sending a test CRID to production")

	return cmd
}

// runGet applies the CRID guards, decides the action locally, and acts on a
// verified answer only.
func runGet(ctx context.Context, opts *globalOpts, operand string, flags getFlags) error {
	assessment, err := cridux.Assess(operand)
	if err != nil {
		return err
	}
	printer := opts.printer()
	if err := applyCRIDGuards(printer, assessment, opts.productionEndpoint(), flags.yes); err != nil {
		return err
	}

	action, err := consume.Decide(opts.streams.OutIsTTY, flags.file)
	if err != nil {
		return err
	}
	if action == consume.ActionSaveFile {
		// Refuse a doomed destination before touching credentials or the
		// network: no resolve is spent on a download that could never land.
		if err := consume.CheckDestination(flags.file, flags.force); err != nil {
			return err
		}
	}

	client, err := opts.newClient()
	if err != nil {
		return err
	}
	// mint resolves and verifies; every path below — the browser launch,
	// the download, and the mid-download retry — goes through it, so
	// nothing ever acts on an unverified answer.
	var resolved *qurlapi.Resolved
	mint := func(ctx context.Context) (string, error) {
		result, err := client.Resolve(ctx, assessment.Input, qurlapi.ResolveOptions{})
		if err != nil {
			return "", err
		}
		if err := verifyResolved(assessment, result); err != nil {
			return "", err
		}
		resolved = result
		return result.QURL, nil
	}
	// fetchURL is mint plus the download-only step: a link whose credential
	// rides in the URL fragment never serves its content to a plain GET
	// (HTTP clients don't transmit fragments — only the in-browser page
	// could consume it), so those links go through the platform access flow
	// and the granted content URL is what gets fetched. The browser action
	// keeps the full link: the in-browser page is exactly what a browser
	// needs. The mid-download retry re-runs all of this, fresh access
	// request included.
	fetchURL := func(ctx context.Context) (string, error) {
		link, err := mint(ctx)
		if err != nil {
			return "", err
		}
		if !consume.NeedsAccessGrant(link) {
			// No in-link credential: the URL itself serves the bytes.
			return link, nil
		}
		return opts.enterPortal(ctx, link)
	}

	switch action {
	case consume.ActionOpenBrowser:
		if _, err := mint(ctx); err != nil {
			return err
		}
		return openInBrowser(ctx, opts, printer, resolved)
	case consume.ActionStreamStdout:
		downloader := &consume.Downloader{Mint: fetchURL}
		_, err := downloader.StreamTo(ctx, opts.streams.Out)
		return err
	case consume.ActionSaveFile:
		downloader := &consume.Downloader{Mint: fetchURL}
		n, err := downloader.SaveTo(ctx, flags.file, flags.force)
		if err != nil {
			return err
		}
		return printer.Downloaded(resolved.CRID, flags.file, n)
	default:
		return fmt.Errorf("unhandled action %d", action)
	}
}

// openInBrowser prints the verified link, then launches the browser at it.
// Data first: with the link on stdout, a failed launch still leaves the
// user something to act on.
func openInBrowser(ctx context.Context, opts *globalOpts, printer *output.Printer, resolved *qurlapi.Resolved) error {
	if err := printer.Resolve(resolved); err != nil {
		return err
	}
	printer.Notef(msgOpeningBrowser)
	if err := opts.openBrowser(ctx, resolved.QURL); err != nil {
		return fmt.Errorf(msgBrowserFailed, err)
	}
	return nil
}
