package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// loginCmd validates a qURL account API key and consumes it to enroll this
// machine. The key is never accepted as a command-line argument or flag: argv
// leaks into shell history and process lists. It is read from a pipe or typed
// at a hidden prompt.
//
// Order matters and is pinned by tests: the key is validated first, then an
// X25519 device identity is registered through NHP, then the resulting device
// REST credential is checked against the same account.
//
// The --enrollment-token-file form is for a supervising app that mints the
// one-time enrollment token itself: no account key is read from anywhere, and
// the namespace is labeled externally supervised as part of the enrollment.
func loginCmd(opts *globalOpts) *cobra.Command {
	var enrollmentTokenFile string
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
key.

A supervising app that already holds a signed-in session can mint a one-time
enrollment token itself and pass it with --enrollment-token-file under
--supervision external. qurl then reads the token file once, only while
enrolling, and reads no account key from the environment or standard input.
The state directory must be sealed: LAYERV_KEY_PROVIDER=local-key with the
wrapping key on the inherited LAYERV_LOCAL_KEY_FD descriptor.`,
		Example: `  qurl login
  op read op://team/qurl/key | qurl login
  qurl login --enrollment-token-file /path/to/enrollment-token --supervision external`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// An explicit token file, even an empty value, selects the
			// external form: it must never fall through to the key prompt.
			if cmd.Flags().Changed("enrollment-token-file") {
				return runExternalLogin(cmd.Context(), opts, enrollmentTokenFile)
			}
			// Enrollment writes the namespace's device identity: refuse the
			// wrong lifecycle contract before the key is even read.
			if err := opts.requireRuntimeSupervisionIfNamespace(); err != nil {
				return err
			}
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
			opts.registeredClient = client
			opts.registeredIdentity = deviceIdentity
			return opts.printer().Login(deviceIdentity)
		},
	}
	cmd.Flags().StringVar(&enrollmentTokenFile, "enrollment-token-file", "", "enroll from a one-time enrollment token file written by a supervising app (requires --supervision external)")
	return cmd
}

// runExternalLogin enrolls this machine from a supervisor's one-time token
// file. Every precondition is checked on public metadata before the namespace
// is touched; the token itself is read only by the runtime's enrollment
// provider, so a warm namespace never opens the file.
func runExternalLogin(ctx context.Context, opts *globalOpts, tokenPath string) error {
	if opts.resolvedSupervision != connectorstate.RuntimeSupervisionExternal {
		return exitcode.UsageError(errors.New("--enrollment-token-file requires --supervision external (or " + connectorstate.EnvRuntimeSupervision + "=external)"))
	}
	if err := auth.ValidateExternalEnrollmentTokenPath(tokenPath); err != nil {
		return exitcode.UsageError(fmt.Errorf("--enrollment-token-file: %w", err))
	}
	if accountKeyConfigured(opts.lookupEnv) {
		return exitcode.UsageError(fmt.Errorf("--enrollment-token-file cannot be combined with %s or %s", auth.EnvAPIKey, auth.EnvAPIKeyFile))
	}
	if err := requireLocalKeyProvider(opts.lookupEnv); err != nil {
		return exitcode.UsageError(err)
	}
	stateDir, err := opts.resolveShareStateDir("")
	if err != nil {
		return err
	}
	client, deviceIdentity, err := opts.openNativeExternalRegisteredClient(ctx, tokenPath, stateDir)
	if err != nil {
		return err
	}
	opts.registeredClient = client
	opts.registeredIdentity = deviceIdentity
	return opts.printer().Login(deviceIdentity)
}

// accountKeyConfigured reports whether the environment carries account-key
// authority. External enrollment refuses it rather than ignoring it: a
// supervisor that sets both has crossed its own credential boundary.
func accountKeyConfigured(lookup func(string) (string, bool)) bool {
	for _, name := range []string{auth.EnvAPIKey, auth.EnvAPIKeyFile} {
		if value, present := lookup(name); present && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

// requireLocalKeyProvider checks the sealed-state contract by public metadata
// only: the provider name and a plausible inherited descriptor number. The
// key bytes are read by the connector's provider, never here, and the
// descriptor value is never echoed.
func requireLocalKeyProvider(lookup func(string) (string, bool)) error {
	provider, _ := lookup(connectoragentstate.EnvKeyProvider)
	if strings.ToLower(strings.TrimSpace(provider)) != connectoragentstate.KeyProviderLocalKey {
		return fmt.Errorf("--enrollment-token-file requires %s=%s", connectoragentstate.EnvKeyProvider, connectoragentstate.KeyProviderLocalKey)
	}
	if fd, _ := lookup(connectoragentstate.EnvLocalKeyFD); !validLocalKeyDescriptor(fd) {
		return fmt.Errorf("--enrollment-token-file requires %s to name an inherited descriptor (3 or higher)", connectoragentstate.EnvLocalKeyFD)
	}
	return nil
}

// validLocalKeyDescriptor accepts only a bare decimal descriptor number at or
// above 3: stdio can never carry the wrapping key.
func validLocalKeyDescriptor(raw string) bool {
	if raw == "" || strings.IndexFunc(raw, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return false
	}
	fd, err := strconv.Atoi(raw)
	return err == nil && fd >= 3
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
