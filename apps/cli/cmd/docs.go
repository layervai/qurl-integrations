package main

import (
	"os"
	"path/filepath"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
)

func docsCmd() *cobra.Command {
	var outDir string

	cmd := &cobra.Command{
		Use:    "docs [man|markdown]",
		Short:  "Generate documentation (man pages or markdown)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := args[0]
			if mode != "man" && mode != "markdown" {
				return cmd.Usage()
			}

			// Disable color in generated docs and restore after.
			origNoColor := color.NoColor
			color.NoColor = true
			defer func() { color.NoColor = origNoColor }()

			dir := filepath.Clean(outDir)
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return err
			}

			root := cmd.Root()

			switch mode {
			case "man":
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
