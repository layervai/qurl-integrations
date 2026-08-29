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

// loginCmd validates a qURL account API key and consumes it to enroll this
// machine. The
// key is never accepted as a command-line argument or flag: argv leaks into
// shell history and process lists. It is read from a pipe or typed at a
// hidden prompt.
//
// Order matters and is pinned by tests: the key is validated first, then an
// X25519 device identity is registered through NHP, then the resulting device
// REST credential is checked against the same account. Only after that full
// transition succeeds are any legacy stored account-key copies removed.
func loginCmd(opts *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Enroll this machine with a qURL account API key",
		Long: `Enroll this machine for future qURL commands.

The key is read from standard input when piped, or typed at a hidden prompt
— never passed as an argument, so it stays out of shell history. Keys look
like lv_live_... (production) or lv_test_... (test).

The account API key is checked against the qURL service, then used once to
enroll a device identity. qurl stores the device identity and its restricted
credential in the owner-only native state directory. It does not store the
account API key.

In scripts and CI, set QURL_API_KEY for the same bootstrap. After enrollment,
ordinary commands use the stored device identity and do not read the account
key.`,
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
			accountClient, err := opts.apiClient(key)
			if err != nil {
				return err
			}
			// Validate before native enrollment: a rejected key cannot create or
			// replace the machine identity.
			accountIdentity, err := accountClient.Me(cmd.Context())
			if err != nil {
				return err
			}
			client, deviceIdentity, err := opts.openRegisteredClient(cmd.Context(), accountClient, key, accountIdentity)
			if err != nil {
				return err
			}
			if client == nil || deviceIdentity == nil {
				return errors.New("registered-device enrollment returned no client identity")
			}
			if _, err := opts.credentialStore().RemoveAll(); err != nil {
				// Enrollment and the device-owner binding are already durable. A legacy-key
				// cleanup failure must not tell the user that enrollment failed.
				opts.printer().Warnf("machine enrollment succeeded, but qurl could not remove a legacy stored account key: %v", err)
			}
			return opts.printer().Login(deviceIdentity)
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
