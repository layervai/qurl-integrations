package main

import (
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
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
			if mode != "man" && mode != "markdown" {
				return cmd.Usage()
			}

			dir := filepath.Clean(outDir)
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return err
			}

			root := cmd.Root()

			switch mode {
			case "man":
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
		ValidArgs: []string{"man", "markdown"},
	}

	cmd.Flags().StringVarP(&outDir, "output-dir", "d", ".", "Output directory for generated docs")

	return cmd
}
