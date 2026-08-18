package main

import (
	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// getCmd is the Phase-1 consume command: resolve a CRID and act on the
// result — open it in the browser on a terminal, or download it to a file.
// The consume flow lands in a later step; this build carries the full flag
// surface and returns the uniform not-available error.
func getCmd(opts *globalOpts) *cobra.Command {
	var (
		file  string
		force bool
	)

	cmd := &cobra.Command{
		Use:   "get <CRID>",
		Short: "Fetch what a CRID points to",
		Long: `Fetch the content behind a CRID.

On a terminal, get opens the resource in your browser. With --file it
downloads to a path instead ("-" streams to stdout); existing files are
never overwritten unless --force is given. When output is piped, get never
opens a browser.`,
		Example: "  qurl get " + exampleCRID + "\n" +
			"  qurl get " + exampleCRID + " --file report.pdf",
		Args: exactArgs(1),
		RunE: func(_ *cobra.Command, _ []string) error {
			_ = opts
			_ = file
			_ = force
			return exitcode.NotImplemented(msgNotImplemented)
		},
	}

	cmd.Flags().StringVar(&file, "file", "", "download to this path instead of opening a browser (\"-\" = stdout)")
	cmd.Flags().BoolVar(&force, "force", false, "allow --file to replace an existing file")

	return cmd
}
