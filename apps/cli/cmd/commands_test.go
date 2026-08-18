package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

// TestStubsReturnCleanNotAvailableExit1 pins the stub contract: commands
// whose platform calls land in later steps exist with full surfaces and
// fail fast with the uniform message, exit 1, nothing on stdout, no hang.
func TestStubsReturnCleanNotAvailableExit1(t *testing.T) {
	cases := [][]string{
		{"get", "aeh2" + strings.Repeat("a", 56)},
	}
	for _, args := range cases {
		t.Run(args[0], func(t *testing.T) {
			res := runCLI(t, &runOpts{args: args})
			if res.code != 1 {
				t.Fatalf("exit = %d, want 1; stderr: %s", res.code, res.stderr.String())
			}
			mustEmptyStdout(t, res)
			if !strings.Contains(res.stderr.String(), "isn't available in this build") {
				t.Errorf("expected the uniform stub message, got %q", res.stderr.String())
			}
		})
	}
}

func TestLoginRejectsImplausibleKey(t *testing.T) {
	res := runCLI(t, &runOpts{
		args:  []string{"login"},
		stdin: strings.NewReader("not-a-key\n"),
	})
	if res.code != 4 {
		t.Fatalf("exit = %d, want 4; stderr: %s", res.code, res.stderr.String())
	}
}

func TestLoginEmptyPipeIsUsageErrorNotAHang(t *testing.T) {
	res := runCLI(t, &runOpts{args: []string{"login"}, stdin: strings.NewReader("")})
	if res.code != 2 {
		t.Fatalf("exit = %d, want 2; stderr: %s", res.code, res.stderr.String())
	}
}

func TestVersionOutputShape(t *testing.T) {
	res := runCLI(t, &runOpts{args: []string{"version"}})
	if res.code != 0 {
		t.Fatalf("exit = %d", res.code)
	}
	// The Homebrew formula asserts on this line's shape; keep it stable.
	if !strings.HasPrefix(res.stdout.String(), "qurl version test (") {
		t.Errorf("version output = %q, want the qurl version prefix", res.stdout.String())
	}
}

func TestCompletionGeneratesScripts(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		t.Run(shell, func(t *testing.T) {
			res := runCLI(t, &runOpts{args: []string{"completion", shell}})
			if res.code != 0 || res.stdout.Len() == 0 {
				t.Fatalf("completion %s: exit=%d, %d bytes", shell, res.code, res.stdout.Len())
			}
		})
	}
	res := runCLI(t, &runOpts{args: []string{"completion", "tcsh"}})
	if res.code != 2 {
		t.Errorf("unsupported shell should be a usage error, got exit %d", res.code)
	}
}

// TestDocsGeneratesManPagesAndMarkdown pins the release contract:
// .goreleaser.yml runs `qurl docs man -d manpages` and ships the output.
func TestDocsGeneratesManPagesAndMarkdown(t *testing.T) {
	manDir := t.TempDir()
	res := runCLI(t, &runOpts{args: []string{"docs", "man", "-d", manDir}})
	if res.code != 0 {
		t.Fatalf("docs man exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	if _, err := os.Stat(filepath.Join(manDir, "qurl.1")); err != nil {
		t.Errorf("expected qurl.1 man page: %v", err)
	}
	if _, err := os.Stat(filepath.Join(manDir, "qurl-publish.1")); err != nil {
		t.Errorf("expected qurl-publish.1 man page: %v", err)
	}

	mdDir := t.TempDir()
	res = runCLI(t, &runOpts{args: []string{"docs", "markdown", "-d", mdDir}})
	if res.code != 0 {
		t.Fatalf("docs markdown exit = %d", res.code)
	}
	if _, err := os.Stat(filepath.Join(mdDir, "qurl_resolve.md")); err != nil {
		t.Errorf("expected qurl_resolve.md: %v", err)
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	cases := map[string][]string{
		"unknown command": {"frobnicate"},
		"unknown flag":    {"list", "--no-such-flag"},
		"bad output":      {"-o", "yaml", "list"},
		"bad color":       {"--color", "sometimes", "list"},
		"missing operand": {"resolve"},
		"extra operand":   {"whoami", "extra"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			res := runCLI(t, &runOpts{args: args})
			if res.code != 2 {
				t.Fatalf("exit = %d, want 2; stderr: %s", res.code, res.stderr.String())
			}
		})
	}
}

func TestUnusableOperandExitEight(t *testing.T) {
	res := runCLI(t, &runOpts{args: []string{"resolve", "not a CRID at all!"}})
	if res.code != 8 {
		t.Fatalf("exit = %d, want 8; stderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
}

// TestEndpointPrecedence pins flag > env > profile for the endpoint setting
// end to end: each level is proven by the request actually landing on the
// mock that level names.
func TestEndpointPrecedence(t *testing.T) {
	srv := apitest.NewServer(t)

	t.Run("env", func(t *testing.T) {
		res := runCLI(t, &runOpts{
			args: []string{"list"},
			env:  map[string]string{"QURL_API_KEY": testAPIKey, "QURL_ENDPOINT": srv.URL},
		})
		if res.code != 0 {
			t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
		}
	})

	t.Run("flag beats env", func(t *testing.T) {
		res := runCLI(t, &runOpts{
			args: []string{"--endpoint", srv.URL, "list"},
			env:  map[string]string{"QURL_API_KEY": testAPIKey, "QURL_ENDPOINT": "https://unreachable.invalid"},
		})
		if res.code != 0 {
			t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
		}
	})

	t.Run("profile", func(t *testing.T) {
		dir := t.TempDir()
		profileDir := filepath.Join(dir, "profiles")
		if err := os.MkdirAll(profileDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(profileDir, "sandbox.yaml"), []byte("endpoint: "+srv.URL+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		res := runCLI(t, &runOpts{
			args:      []string{"--profile", "sandbox", "list"},
			configDir: dir,
		})
		if res.code != 0 {
			t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
		}
	})
}

func TestConfigFileWithSecretRefusedExitThree(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("api_key: lv_test_shouldnotbehere123456\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := runCLI(t, &runOpts{args: []string{"list"}, configDir: dir})
	if res.code != 3 {
		t.Fatalf("exit = %d, want 3; stderr: %s", res.code, res.stderr.String())
	}
	if !strings.Contains(res.stderr.String(), "must not contain an API key") {
		t.Errorf("expected the no-secrets refusal, got %q", res.stderr.String())
	}
}

func TestMissingCredentialExitFour(t *testing.T) {
	res := runCLI(t, &runOpts{args: []string{"list"}, env: map[string]string{}})
	if res.code != 4 {
		t.Fatalf("exit = %d, want 4; stderr: %s", res.code, res.stderr.String())
	}
	if !strings.Contains(res.stderr.String(), "QURL_API_KEY") {
		t.Errorf("expected the remedy hint, got %q", res.stderr.String())
	}
}

// TestEnumSourceDecidesExitCode pins the round-4 disposition: the same
// invalid enum value is a configuration error (exit 3) when a config FILE
// says it, and a usage error (exit 2) when the flag or environment does —
// the config layer knows the source, the resolution site does not.
func TestEnumSourceDecidesExitCode(t *testing.T) {
	fileCases := map[string]string{
		"output": "output: yaml\n",
		"color":  "color: sometimes\n",
	}
	for name, body := range fileCases {
		t.Run("file "+name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			res := runCLI(t, &runOpts{args: []string{"list"}, configDir: dir})
			if res.code != 3 {
				t.Fatalf("exit = %d, want 3; stderr: %s", res.code, res.stderr.String())
			}
			if !strings.Contains(res.stderr.String(), "config.yaml") {
				t.Errorf("the config-file error must name the file, got %q", res.stderr.String())
			}
		})
	}

	argCases := map[string][]string{
		"flag output": {"-o", "yaml", "list"},
		"flag color":  {"--color", "sometimes", "list"},
	}
	for name, args := range argCases {
		t.Run(name, func(t *testing.T) {
			res := runCLI(t, &runOpts{args: args})
			if res.code != 2 {
				t.Fatalf("exit = %d, want 2; stderr: %s", res.code, res.stderr.String())
			}
		})
	}

	envCases := map[string]map[string]string{
		"env output": {"QURL_API_KEY": testAPIKey, "QURL_OUTPUT": "yaml"},
		"env color":  {"QURL_API_KEY": testAPIKey, "QURL_COLOR": "sometimes"},
	}
	for name, env := range envCases {
		t.Run(name, func(t *testing.T) {
			res := runCLI(t, &runOpts{args: []string{"list"}, env: env})
			if res.code != 2 {
				t.Fatalf("exit = %d, want 2; stderr: %s", res.code, res.stderr.String())
			}
		})
	}
}

// TestWhoamiListedInHelp guards the command roster: every v2 command is
// registered and visible.
func TestWhoamiListedInHelp(t *testing.T) {
	res := runCLI(t, &runOpts{args: []string{"--help"}})
	if res.code != 0 {
		t.Fatalf("help exit = %d", res.code)
	}
	for _, name := range []string{"publish", "resolve", "get", "list", "delete", "login", "logout", "whoami", "version", "completion"} {
		if !strings.Contains(res.stdout.String(), name) {
			t.Errorf("help does not list %q", name)
		}
	}
}
