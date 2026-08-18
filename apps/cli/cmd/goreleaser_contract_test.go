package main

import (
	"maps"
	"os"
	"path/filepath"
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
	cfg := loadGoreleaserConfig(t)

	// scripts/cli-completions.sh writes exactly these files into the archive.
	want := map[string]string{
		"bash": "completions/qurl.bash",
		"zsh":  "completions/qurl.zsh",
		"fish": "completions/qurl.fish",
	}
	if got := cfg.HomebrewCasks[0].Completions; !maps.Equal(got, want) {
		t.Fatalf("homebrew_casks[0].completions = %v, want %v", got, want)
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
