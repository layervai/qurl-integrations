package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"

	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// docsFormatMan and docsFormatMarkdown are the two accepted `qurl docs` output
// formats; named so the validity check, the switch, ValidArgs and the usage
// error stay in step. `qurl docs man` is a distribution contract (see docsCmd).
const (
	docsFormatMan      = "man"
	docsFormatMarkdown = "markdown"
)

// docsCmd generates man pages and markdown docs from the command tree. It is
// load-bearing for releases: .goreleaser.yml runs `qurl docs man -d manpages`
// in its before hook and ships the result in every archive, so the command
// name, modes, and -d flag are a distribution contract.
func docsCmd() *cobra.Command {
	var outDir string

	cmd := &cobra.Command{
		Use:    "docs [man|markdown]",
		Short:  "Generate documentation (man pages or markdown)",
		Hidden: true,
		Args:   exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := args[0]
			if mode != docsFormatMan && mode != docsFormatMarkdown {
				return exitcode.UsageError(fmt.Errorf("docs mode must be %q or %q, got %q", docsFormatMan, docsFormatMarkdown, mode))
			}

			dir := filepath.Clean(outDir)
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return err
			}

			root := cmd.Root()

			switch mode {
			case docsFormatMan:
				// Man-page section headers are conventionally uppercase
				// (CURL(1), GIT(1), etc.). Keep "QURL" here even though the
				// brand is "qURL" — system references follow the convention,
				// not the brand-prose rule.
				header := &doc.GenManHeader{
					Title:   "QURL",
					Section: "1",
					Source:  "LayerV",
				}
				return doc.GenManTree(root, header, dir)
			default:
				return doc.GenMarkdownTree(root, dir)
			}
		},
		ValidArgs: []string{docsFormatMan, docsFormatMarkdown},
	}

	cmd.Flags().StringVarP(&outDir, "output-dir", "d", ".", "Output directory for generated docs")

	return cmd
}
