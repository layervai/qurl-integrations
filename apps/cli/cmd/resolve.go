package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/layervai/qurl-integrations/shared/client"
)

func resolveCmd(opts *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:   "resolve [access-token]",
		Short: "Resolve a QURL access token (headless)",
		Long: `Resolve a QURL access token to get the target URL and open firewall access.
After resolution, the target URL is accessible from your IP for the duration
specified in the access grant.

The access token can be provided as an argument, via stdin, or interactively:
  qurl resolve at_abc123           Argument (visible in shell history)
  echo $TOKEN | qurl resolve       Stdin (safer)
  qurl resolve                     Interactive prompt (hidden input)`,
		Example: `  qurl resolve at_k8xqp9h2sj9lx7r4a
  echo "$TOKEN" | qurl resolve
  qurl resolve -o json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			token, err := readToken(cmd, args)
			if err != nil {
				return err
			}

			if err := validateAccessToken(token); err != nil {
				return err
			}

			c, err := opts.newClient()
			if err != nil {
				return err
			}

			result, err := c.Resolve(cmd.Context(), client.ResolveInput{
				AccessToken: token,
			})
			if err != nil {
				return fmt.Errorf("resolve QURL: %w", err)
			}

			return opts.formatter().FormatResolve(cmd.OutOrStdout(), result)
		},
	}
}

// readToken reads the access token from args, stdin, or interactive prompt.
func readToken(cmd *cobra.Command, args []string) (string, error) {
	// From argument
	if len(args) > 0 {
		return args[0], nil
	}

	// From stdin (piped)
	stat, _ := os.Stdin.Stat()
	if stat != nil && (stat.Mode()&os.ModeCharDevice) == 0 {
		scanner := bufio.NewScanner(cmd.InOrStdin())
		if scanner.Scan() {
			token := strings.TrimSpace(scanner.Text())
			if token != "" {
				return token, nil
			}
		}
		return "", errors.New("no token provided via stdin")
	}

	// Interactive prompt with hidden input
	if _, err := fmt.Fprint(cmd.ErrOrStderr(), "Access token: "); err != nil {
		return "", err
	}
	tokenBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	if _, printErr := fmt.Fprintln(cmd.ErrOrStderr()); printErr != nil {
		return "", printErr
	}
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}

	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return "", errors.New("no token provided")
	}
	return token, nil
}
