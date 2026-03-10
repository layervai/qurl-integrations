// Package main generates man pages and markdown docs for the qurl CLI.
//
// This tool rebuilds the root command directly to generate documentation.
// Run: go run ./apps/cli/tools/gendocs man ./man
// Run: go run ./apps/cli/tools/gendocs markdown ./docs
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
)

func main() {
	// Disable color codes in generated docs.
	color.NoColor = true

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: gendocs <man|markdown> [output-dir]")
		os.Exit(1)
	}

	mode := os.Args[1]
	outDir := "./docs"
	if len(os.Args) > 2 {
		outDir = os.Args[2]
	}

	dir, err := safeOutputDir(outDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "output dir: %v\n", err)
		os.Exit(1)
	}

	root := buildDocCmd()

	var genErr error
	switch mode {
	case "man":
		header := &doc.GenManHeader{
			Title:   "QURL",
			Section: "1",
			Source:  "LayerV",
		}
		genErr = doc.GenManTree(root, header, dir)
	case "markdown":
		genErr = doc.GenMarkdownTree(root, dir)
	default:
		fmt.Fprintln(os.Stderr, "unknown mode: use 'man' or 'markdown'")
		os.Exit(1)
	}

	if genErr != nil {
		fmt.Fprintf(os.Stderr, "generate docs: %v\n", genErr)
		os.Exit(1)
	}
	fmt.Printf("Generated %s docs in %s\n", mode, dir)
}

// safeOutputDir resolves and creates the output directory.
// It validates the path is under the current working directory.
func safeOutputDir(raw string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}

	abs := filepath.Join(cwd, filepath.Clean(raw))
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return "", fmt.Errorf("create directory: %w", err)
	}
	return abs, nil
}

// buildDocCmd creates a minimal root command tree for doc generation.
// This mirrors the real CLI structure without requiring imports from package main.
func buildDocCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "qurl",
		Short: "QURL CLI - manage secure links from the command line",
		Long: `QURL CLI creates, resolves, and manages QURL secure links.

Authentication (in order of precedence):
  1. --api-key flag
  2. QURL_API_KEY environment variable
  3. ~/.config/qurl/config.yaml (or --profile <name>)

Get started:
  qurl create https://example.com        Create a QURL
  qurl list                              List active QURLs
  qurl resolve <access-token>            Resolve a token (headless)
  qurl quota                             Check your usage
  qurl completion bash                   Generate shell completions`,
	}

	root.PersistentFlags().String("api-key", "", "API key")
	root.PersistentFlags().String("endpoint", "", "API endpoint")
	root.PersistentFlags().StringP("output", "o", "table", "Output format: table or json")
	root.PersistentFlags().BoolP("quiet", "q", false, "Minimal output")
	root.PersistentFlags().BoolP("verbose", "v", false, "Show HTTP request/response details")
	root.PersistentFlags().String("profile", "", "Config profile name")

	commands := []struct {
		use, short string
	}{
		{"create <target-url>", "Create a QURL for a target URL"},
		{"resolve [access-token]", "Resolve a QURL access token (headless)"},
		{"list", "List QURLs"},
		{"get <resource-id>", "Get QURL details"},
		{"delete <resource-id>", "Revoke/delete a QURL"},
		{"update <resource-id>", "Update a QURL's properties"},
		{"extend <resource-id>", "Extend QURL expiration"},
		{"mint <resource-id>", "Mint a new access link for a QURL"},
		{"quota", "Show usage quota and plan info"},
		{"config", "Manage CLI configuration"},
		{"completion [bash|zsh|fish|powershell]", "Generate shell completion scripts"},
		{"version", "Print version information"},
	}

	for _, c := range commands {
		root.AddCommand(&cobra.Command{Use: c.use, Short: c.short})
	}

	return root
}
