package main

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
)

const (
	docModeMan      = "man"
	docModeMarkdown = "markdown"
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
			if mode != docModeMan && mode != docModeMarkdown {
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
			restoreAutoGen := setDisableAutoGen(root, true)
			defer restoreAutoGen()

			switch mode {
			case docModeMan:
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
				if err := doc.GenMarkdownTree(root, dir); err != nil {
					return err
				}
				return trimMarkdownDocs(dir)
			}
		},
		ValidArgs: []string{docModeMan, docModeMarkdown},
	}

	cmd.Flags().StringVarP(&outDir, "output-dir", "d", ".", "Output directory for generated docs")

	return cmd
}

func trimMarkdownDocs(dir string) (err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := root.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}()

	return fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}

		content, err := root.ReadFile(path)
		if err != nil {
			return err
		}
		trimmed := append(bytes.TrimRight(content, "\n"), '\n')
		if bytes.Equal(content, trimmed) {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		return root.WriteFile(path, trimmed, info.Mode())
	})
}

func setDisableAutoGen(cmd *cobra.Command, value bool) func() {
	previous := map[*cobra.Command]bool{}
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		previous[c] = c.DisableAutoGenTag
		c.DisableAutoGenTag = value
		for _, child := range c.Commands() {
			walk(child)
		}
	}
	walk(cmd)

	return func() {
		for c, v := range previous {
			c.DisableAutoGenTag = v
		}
	}
}
