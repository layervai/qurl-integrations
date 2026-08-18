package main

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/spf13/cobra/doc"
	"gopkg.in/yaml.v3"
)

// .goreleaser.yml's Homebrew cask enumerates man pages and completion files
// by exact path: casks can neither glob the way the old formula's
// Dir["manpages/*.1"] did nor generate completions at install time. That
// makes drift silent in both directions — a new command's man page would
// quietly be missing from the cask, and a removed command would leave the
// cask referencing a path absent from the release archive, which breaks
// `brew install` for every user at the next release. These tests pin the
// cask's lists, and the archive globs that ship the files, to the real
// command tree.

type goreleaserConfig struct {
	Archives []struct {
		Files []string `yaml:"files"`
	} `yaml:"archives"`
	HomebrewCasks []struct {
		Manpages    []string          `yaml:"manpages"`
		Completions map[string]string `yaml:"completions"`
	} `yaml:"homebrew_casks"`
}

func loadGoreleaserConfig(t *testing.T) goreleaserConfig {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("..", "..", "..", ".goreleaser.yml"))
	if err != nil {
		t.Fatalf("read .goreleaser.yml: %v", err)
	}

	var cfg goreleaserConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse .goreleaser.yml: %v", err)
	}
	if n := len(cfg.HomebrewCasks); n != 1 {
		t.Fatalf("expected exactly 1 homebrew cask in .goreleaser.yml, got %d", n)
	}
	if n := len(cfg.Archives); n != 1 {
		t.Fatalf("expected exactly 1 archive in .goreleaser.yml, got %d", n)
	}
	return cfg
}

func TestCaskManpagesMatchGeneratedManTree(t *testing.T) {
	cfg := loadGoreleaserConfig(t)

	// Mirror release-time generation (`qurl-gendocs docs man -d manpages` in
	// .goreleaser.yml's before hooks): same header, same tree walk.
	dir := t.TempDir()
	header := &doc.GenManHeader{Title: "QURL", Section: "1", Source: "LayerV"}
	if err := doc.GenManTree(rootCmd("test"), header, dir); err != nil {
		t.Fatalf("generate man tree: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read generated man tree: %v", err)
	}
	generated := make([]string, 0, len(entries))
	for _, entry := range entries {
		generated = append(generated, "manpages/"+entry.Name())
	}
	slices.Sort(generated)

	declared := slices.Sorted(slices.Values(cfg.HomebrewCasks[0].Manpages))

	if !slices.Equal(generated, declared) {
		t.Fatalf("homebrew_casks[0].manpages is out of sync with the generated man tree\ngenerated: %q\ndeclared:  %q", generated, declared)
	}
}

func TestCaskCompletionsMatchGeneratedFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runs the sh completion script; the release before hooks never run on windows")
	}
	cfg := loadGoreleaserConfig(t)

	// Run the real script (with a stub gendocs — only the file names matter
	// here) so the cask's completions map is pinned to what the script
	// actually writes, not to a second hardcoded copy of the same names.
	stub := filepath.Join(t.TempDir(), "gendocs")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\necho \"stub $*\"\n"), 0o755); err != nil { //nolint:gosec // G306: the stub must be executable for the script to invoke it; it lives in t.TempDir().
		t.Fatalf("write stub gendocs: %v", err)
	}
	outdir := filepath.Join(t.TempDir(), "completions")
	script := filepath.Join("..", "..", "..", "scripts", "cli-completions.sh")
	if out, err := exec.CommandContext(t.Context(), script, stub, outdir).CombinedOutput(); err != nil { //nolint:gosec // G204: script path and args are test-built literals and TempDir paths, not external input.
		t.Fatalf("run %s: %v\n%s", script, err, out)
	}

	entries, err := os.ReadDir(outdir)
	if err != nil {
		t.Fatalf("read script output dir: %v", err)
	}
	written := make([]string, 0, len(entries))
	for _, entry := range entries {
		written = append(written, "completions/"+entry.Name())
	}
	slices.Sort(written)

	declared := slices.Sorted(maps.Values(cfg.HomebrewCasks[0].Completions))

	if !slices.Equal(written, declared) {
		t.Fatalf("homebrew_casks[0].completions out of sync with scripts/cli-completions.sh output\nscript wrote: %q\ndeclared:     %q", written, declared)
	}
	// Pin the shell→file pairing too: the value-set check above is
	// satisfiable by a swapped mapping (bash: qurl.zsh / zsh: qurl.bash),
	// which Homebrew would install as the wrong shell's completion. The
	// three keys are the shells Homebrew has completion artifacts for.
	for _, shell := range []string{"bash", "fish", "zsh"} {
		if got, want := cfg.HomebrewCasks[0].Completions[shell], "completions/qurl."+shell; got != want {
			t.Fatalf("homebrew_casks[0].completions[%q] = %q, want %q", shell, got, want)
		}
	}
	if n := len(cfg.HomebrewCasks[0].Completions); n != 3 {
		t.Fatalf("homebrew_casks[0].completions has %d entries, want 3", n)
	}
}

func TestArchiveShipsCaskSourcePaths(t *testing.T) {
	cfg := loadGoreleaserConfig(t)

	// The cask installs manpages and completions from inside the archive;
	// dropping either glob from the archive's files would break
	// `brew install` without failing any build.
	files := cfg.Archives[0].Files
	for _, want := range []string{"manpages/*", "completions/*"} {
		if !slices.Contains(files, want) {
			t.Fatalf("archives[0].files = %q is missing %q", files, want)
		}
	}
}
