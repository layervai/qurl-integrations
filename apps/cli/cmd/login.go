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

// loginCmd validates a qURL API key and will store it once the secure
// storage backend lands (a later step). The key is never accepted as a
// command-line argument or flag: argv leaks into shell history and process
// lists. It is read from a pipe or typed at a hidden prompt.
func loginCmd(opts *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Save your qURL API key on this machine",
		Long: `Save your qURL API key for future commands.

The key is read from standard input when piped, or typed at a hidden prompt
— never passed as an argument, so it stays out of shell history. Keys look
like lv_live_... (production) or lv_test_... (test).

In scripts and CI, skip login entirely and set QURL_API_KEY; when that
variable is set the CLI uses it and touches nothing on disk.`,
		Example: `  qurl login
  op read op://team/qurl/key | qurl login`,
		Args: noArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			key, err := readSecret(opts, "qURL API key (input hidden): ")
			if err != nil {
				return err
			}
			if err := auth.ValidateKeyShape(key); err != nil {
				return err
			}
			// Storage arrives with the secure credential-store step; validate
			// now, refuse loudly rather than silently dropping the key.
			return exitcode.NotImplemented(msgLoginStorage)
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
