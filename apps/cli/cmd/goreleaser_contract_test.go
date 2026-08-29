package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra/doc"
	"golang.org/x/crypto/curve25519"
	"gopkg.in/yaml.v3"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
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
	Builds []struct {
		ID      string   `yaml:"id"`
		Main    string   `yaml:"main"`
		Binary  string   `yaml:"binary"`
		GOOS    []string `yaml:"goos"`
		GOARCH  []string `yaml:"goarch"`
		LDFlags []string `yaml:"ldflags"`
	} `yaml:"builds"`
	Archives []struct {
		Files           []string `yaml:"files"`
		Formats         []string `yaml:"formats"`
		FormatOverrides []struct {
			GOOS    string   `yaml:"goos"`
			Formats []string `yaml:"formats"`
		} `yaml:"format_overrides"`
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
	if n := len(cfg.Builds); n != 1 {
		t.Fatalf("expected exactly 1 customer build in .goreleaser.yml, got %d", n)
	}
	return cfg
}

func TestReleaseBuildsOnlyQURL(t *testing.T) {
	cfg := loadGoreleaserConfig(t)
	build := cfg.Builds[0]
	if build.ID != "qurl" || build.Main != "./apps/cli/cmd/" || build.Binary != "qurl" {
		t.Fatalf("release build = %+v, want the qurl CLI only", build)
	}
}

func TestReleaseBuildEmbedsProductionHubPin(t *testing.T) {
	cfg := loadGoreleaserConfig(t)
	const pinAssignment = `-X github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub.defaultServerPublicKeyB64={{ index .Env "QURL_RELEASE_HUB_PUBLIC_KEY_B64" }}`
	if count := strings.Count(strings.Join(cfg.Builds[0].LDFlags, "\n"), pinAssignment); count != 1 {
		t.Fatalf("release ldflags contain production Hub-pin assignment %d time(s), want one", count)
	}
}

func TestReleaseLinkerTargetProducesRunnableNativeTrustPin(t *testing.T) {
	scalar := bytes.Repeat([]byte{0x42}, curve25519.ScalarSize)
	public, err := curve25519.X25519(scalar, curve25519.Basepoint)
	if err != nil {
		t.Fatal(err)
	}
	keyB64 := base64.StdEncoding.EncodeToString(public)
	wantFingerprint := hub.FingerprintSHA256Hex(public)

	binary := filepath.Join(t.TempDir(), "qurl")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	root, err := filepath.Abs(cliRepoRoot)
	if err != nil {
		t.Fatal(err)
	}
	const linkerTarget = "github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub.defaultServerPublicKeyB64"
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// #nosec G204 -- every argument is a test-owned constant or a value generated above in this process.
	build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-mod=readonly", "-ldflags", "-X "+linkerTarget+"="+keyB64, "-o", binary, "./apps/cli/cmd/")
	build.Dir = root
	build.Env = append(os.Environ(), "GOWORK=off")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build release-pin executable: %v\n%s", err, output)
	}

	// #nosec G204 -- binary is the exact test-owned output path built above.
	verify := exec.CommandContext(ctx, binary, "version", "--verify-release-native-trust")
	output, err := verify.CombinedOutput()
	if err != nil {
		t.Fatalf("run release-pin executable: %v\n%s", err, output)
	}
	if got := strings.TrimSpace(string(output)); got != wantFingerprint {
		t.Fatalf("release-pin executable fingerprint = %q, want %q", got, wantFingerprint)
	}
}

func TestReleaseBuildsSupportedCLIPlatforms(t *testing.T) {
	cfg := loadGoreleaserConfig(t)
	if got, want := cfg.Builds[0].GOOS, []string{"linux", "darwin", "windows"}; !slices.Equal(got, want) {
		t.Fatalf("release platforms = %q, want %q", got, want)
	}
	if got, want := cfg.Builds[0].GOARCH, []string{"amd64", "arm64"}; !slices.Equal(got, want) {
		t.Fatalf("release architectures = %q, want %q", got, want)
	}
	archive := cfg.Archives[0]
	if !slices.Equal(archive.Formats, []string{"tar.gz"}) || len(archive.FormatOverrides) != 1 ||
		archive.FormatOverrides[0].GOOS != "windows" || !slices.Equal(archive.FormatOverrides[0].Formats, []string{"zip"}) {
		t.Fatalf("release archive formats = %+v, want tar.gz with a Windows zip override", archive)
	}
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
	for _, want := range []string{"LICENSE", "manpages/*", "completions/*"} {
		if !slices.Contains(files, want) {
			t.Fatalf("archives[0].files = %q is missing %q", files, want)
		}
	}
}
