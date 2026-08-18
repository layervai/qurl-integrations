package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// loginCmd validates a qURL API key against the platform and stores it. The
// key is never accepted as a command-line argument or flag: argv leaks into
// shell history and process lists. It is read from a pipe or typed at a
// hidden prompt.
//
// Order matters and is pinned by tests: the key is validated first — shape
// locally, then for real against the platform's identity endpoint — and
// stored only after the platform accepted it. A mistyped key is rejected
// loudly instead of being saved and breaking every later command.
func loginCmd(opts *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Save your qURL API key on this machine",
		Long: `Save your qURL API key for future commands.

The key is read from standard input when piped, or typed at a hidden prompt
— never passed as an argument, so it stays out of shell history. Keys look
like lv_live_... (production) or lv_test_... (test).

The key is checked against the qURL service first and stored only once the
service confirms it. It goes into your OS keyring; on systems without one,
it falls back to a file only your user can read, and the CLI says so.

In scripts and CI, skip login entirely and set QURL_API_KEY; when that
variable is set the CLI uses it and touches nothing on disk.`,
		Example: `  qurl login
  op read op://team/qurl/key | qurl login`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			key, err := readSecret(opts, "qURL API key (input hidden): ")
			if err != nil {
				return err
			}
			if err := auth.ValidateKeyShape(key); err != nil {
				return err
			}
			client, err := opts.apiClient(key)
			if err != nil {
				return err
			}
			// Validate before anything touches storage: a key the platform
			// rejects is never stored.
			id, err := client.Me(cmd.Context())
			if err != nil {
				return err
			}
			backend, err := opts.credentialStore().SaveTo(key)
			if err != nil {
				return fmt.Errorf("save the key: %w", err)
			}
			if backend == auth.BackendFile {
				opts.printer().Warnf("%s", msgKeyringUnavailable)
			}
			return opts.printer().Login(id, backend)
		},
	}
	return cmd
}

// readSecret reads a secret from piped stdin or an interactive hidden
// prompt. It never echoes and never hangs: piped-but-empty input is an
// error, not a wait.
func readSecret(opts *globalOpts, prompt string) (string, error) {
	if !opts.streams.InIsTTY {
		scanner := bufio.NewScanner(opts.streams.In)
		if scanner.Scan() {
			if secret := strings.TrimSpace(scanner.Text()); secret != "" {
				return secret, nil
			}
		}
		if err := scanner.Err(); err != nil {
			return "", err
		}
		return "", exitcode.UsageError(errors.New(msgNoKeyProvided))
	}

	if _, err := fmt.Fprint(opts.streams.Err, prompt); err != nil {
		return "", err
	}
	// Deliberate seam gap: term.ReadPassword needs a real terminal fd, so the
	// hidden-prompt read bypasses the injected opts.streams.In that selected
	// this branch (via InIsTTY). The piped path above stays fully injectable;
	// this branch is only reachable on a real TTY, where os.Stdin is the
	// stream the injector wrapped anyway.
	secretBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	if _, printErr := fmt.Fprintln(opts.streams.Err); printErr != nil {
		return "", printErr
	}
	if err != nil {
		return "", fmt.Errorf("read key: %w", err)
	}
	secret := strings.TrimSpace(string(secretBytes))
	if secret == "" {
		return "", exitcode.UsageError(errors.New(msgNoKeyProvided))
	}
	return secret, nil
}
