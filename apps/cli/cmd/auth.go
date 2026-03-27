package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
	"github.com/layervai/qurl-integrations/apps/cli/internal/config"
	"github.com/layervai/qurl-integrations/shared/client"
)

var allScopes = []string{"qurl:read", "qurl:write", "qurl:resolve"}

func authCmd(opts *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Authenticate with the QURL API",
		Long: `Manage authentication for the QURL CLI.

Use "auth login" to authenticate via your browser using the OAuth device flow.
Use "auth status" to check your current authentication.
Use "auth logout" to remove stored credentials.`,
	}
	cmd.AddCommand(
		authLoginCmd(opts),
		authLogoutCmd(opts),
		authStatusCmd(opts),
	)
	return cmd
}

func authLoginCmd(opts *globalOpts) *cobra.Command {
	var (
		keyName   string
		scopes    []string
		noBrowser bool
	)

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in to QURL via browser-based authentication",
		Long: `Authenticate with QURL using the OAuth 2.0 Device Authorization flow.

This opens your browser to complete authentication, then creates an API key
and stores it in your local config. The API key persists across sessions until
you run "auth logout" or revoke it in the portal.`,
		Example: `  qurl auth login
  qurl auth login --key-name "my-laptop"
  qurl auth login --scopes qurl:read,qurl:write
  qurl auth login --no-browser`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAuthLogin(cmd, opts, keyName, scopes, noBrowser)
		},
	}

	cmd.Flags().StringVar(&keyName, "key-name", "", `Name for the API key (default: "CLI (<hostname>)")`)
	cmd.Flags().StringSliceVar(&scopes, "scopes", nil, "API key scopes (default: all)")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "Don't open the browser automatically")

	return cmd
}

func runAuthLogin(cmd *cobra.Command, opts *globalOpts, keyName string, scopes []string, noBrowser bool) error {
	ctx := cmd.Context()
	w := &statusWriter{w: cmd.ErrOrStderr()}
	stdout := cmd.OutOrStdout()

	clientID, domain, audience := resolveAuth0Config()
	if clientID == "" {
		return errors.New("Auth0 client ID not configured: set QURL_AUTH0_CLIENT_ID environment variable")
	}

	if len(scopes) == 0 {
		scopes = allScopes
	}

	flowCfg := &auth.DeviceFlowConfig{
		Domain:   domain,
		ClientID: clientID,
		Audience: audience,
		Scopes:   scopes,
		BaseURL:  os.Getenv("QURL_AUTH0_URL"), // Override for testing.
	}
	flow := auth.NewDeviceFlow(flowCfg)

	dcr, err := flow.RequestDeviceCode(ctx)
	if err != nil {
		return fmt.Errorf("request device code: %w", err)
	}

	bold := color.New(color.Bold)
	faint := color.New(color.Faint)

	w.ln()
	w.printf("  Your one-time code: %s\n", bold.Sprint(dcr.UserCode))
	w.ln()

	verifyURL := dcr.VerificationURIComplete
	if verifyURL == "" {
		verifyURL = dcr.VerificationURI
	}

	if !noBrowser {
		w.printf("  Opening browser to %s\n", faint.Sprint(verifyURL))
		if browserErr := auth.OpenBrowser(verifyURL); browserErr != nil {
			w.printf("  Could not open browser. Visit: %s\n", verifyURL)
		}
	} else {
		w.printf("  Visit: %s\n", verifyURL)
	}

	w.ln()
	w.printf("  Waiting for authentication...")

	token, err := flow.PollForToken(ctx, dcr.DeviceCode, dcr.Interval)
	if err != nil {
		w.ln()
		return fmt.Errorf("authentication failed: %w", err)
	}
	w.msg(" done")

	w.printf("  Creating API key...")

	name := resolveKeyName(keyName)
	endpoint := resolveEndpoint(opts)

	keyResp, err := auth.CreateAPIKey(ctx, nil, endpoint, token.AccessToken, auth.CreateKeyRequest{
		Name:   name,
		Scopes: scopes,
	})
	if err != nil {
		w.ln()
		return fmt.Errorf("create API key: %w", err)
	}
	w.msg(" done")

	profile := resolveProfile(opts)
	if saveErr := saveAuthConfig(profile, keyResp.APIKey, keyResp.KeyID); saveErr != nil {
		// Key was created on the server — show it so user doesn't lose it.
		w.ln()
		w.printf("  Warning: could not save config: %v\n", saveErr)
		w.printf("  Your API key (save manually): %s\n", keyResp.APIKey)
		return saveErr
	}

	printLoginSuccess(stdout, w, faint, keyResp, scopes, profile)
	return nil
}

func authLogoutCmd(opts *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:     "logout",
		Short:   "Remove stored authentication credentials",
		Example: "  qurl auth logout\n  qurl auth logout --profile staging",
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := &statusWriter{w: cmd.ErrOrStderr()}
			profile := resolveProfile(opts)

			cfg, _ := config.LoadProfile(profile)
			if cfg == nil || cfg.APIKey == "" {
				w.msg("Not logged in.")
				return nil
			}

			cfg.APIKey = ""
			cfg.KeyID = ""

			if saveErr := config.SaveProfile(profile, cfg); saveErr != nil {
				return fmt.Errorf("save config: %w", saveErr)
			}

			faint := color.New(color.Faint)
			w.msg("Logged out. Credentials removed from local config.")
			w.printf("To revoke the API key on the server, visit %s\n",
				faint.Sprint("https://portal.layerv.ai/keys"))
			return nil
		},
	}
}

func authStatusCmd(opts *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:     "status",
		Short:   "Show current authentication status",
		Example: "  qurl auth status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			stdout := cmd.OutOrStdout()
			apiKey, source := resolveAPIKeyWithSource(opts)

			if apiKey == "" {
				_, _ = color.New(color.FgRed).Fprintln(stdout, "Not authenticated")
				_, _ = fmt.Fprintln(stdout)
				_, err := fmt.Fprintln(stdout, "Run 'qurl auth login' to authenticate.")
				return err
			}

			prefix := apiKey
			if len(prefix) > 12 {
				prefix = prefix[:12]
			}

			bold := color.New(color.Bold)
			faint := color.New(color.Faint)

			_, _ = fmt.Fprintln(stdout)
			_, _ = color.New(color.FgGreen).Fprintln(stdout, "  Authenticated")
			_, _ = fmt.Fprintln(stdout)
			_, _ = fmt.Fprintf(stdout, "  API Key: %s\n", faint.Sprintf("%s...", prefix))
			_, _ = fmt.Fprintf(stdout, "  Source:  %s\n", source)

			ep := resolveEndpoint(opts)
			c := client.New(ep, apiKey, client.WithUserAgent("qurl-cli/"+opts.version))

			statusCtx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()

			quota, qErr := c.GetQuota(statusCtx)
			if qErr == nil {
				_, _ = fmt.Fprintf(stdout, "  Plan:    %s\n", bold.Sprint(strings.ToUpper(quota.Plan)))
			}

			_, err := fmt.Fprintln(stdout)
			return err
		},
	}
}

// resolveAuth0Config returns the Auth0 client ID, domain, and audience
// from environment variables, falling back to defaults.
func resolveAuth0Config() (clientID, domain, audience string) {
	clientID = os.Getenv("QURL_AUTH0_CLIENT_ID")
	if clientID == "" {
		clientID = auth.DefaultClientID
	}

	domain = os.Getenv("QURL_AUTH0_DOMAIN")
	if domain == "" {
		domain = auth.DefaultDomain
	}

	audience = os.Getenv("QURL_AUTH0_AUDIENCE")
	if audience == "" {
		audience = auth.DefaultAudience
	}
	return clientID, domain, audience
}

// resolveAPIKeyWithSource returns the API key and its source label.
func resolveAPIKeyWithSource(opts *globalOpts) (key, source string) {
	if opts.apiKey != "" {
		return opts.apiKey, "--api-key flag"
	}
	if v := os.Getenv("QURL_API_KEY"); v != "" {
		return v, "QURL_API_KEY environment variable"
	}

	profile := resolveProfile(opts)
	cfg, _ := config.LoadProfile(profile)
	if cfg != nil && cfg.APIKey != "" {
		if profile != "" {
			return cfg.APIKey, fmt.Sprintf("profile %q", profile)
		}
		return cfg.APIKey, "config file"
	}
	return "", ""
}

// resolveProfile returns the active profile name from flag or env var.
func resolveProfile(opts *globalOpts) string {
	if opts.profile != "" {
		return opts.profile
	}
	return os.Getenv("QURL_PROFILE")
}

// resolveEndpoint returns the API endpoint from flag, env, or default.
func resolveEndpoint(opts *globalOpts) string {
	ep := resolveValue("QURL_ENDPOINT", &opts.endpoint, "endpoint", nil)
	if ep == "" {
		ep = defaultEndpoint
	}
	return ep
}

// resolveKeyName returns the key name from flag or generates a default.
func resolveKeyName(name string) string {
	if name != "" {
		return name
	}
	hostname, _ := os.Hostname()
	if hostname != "" {
		return "CLI (" + hostname + ")"
	}
	return "CLI"
}

// saveAuthConfig stores the API key and key ID in the config file.
func saveAuthConfig(profile, apiKey, keyID string) error {
	cfg, _ := config.LoadProfile(profile)
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.APIKey = apiKey
	cfg.KeyID = keyID
	return config.SaveProfile(profile, cfg)
}

// statusWriter wraps an io.Writer for progress messages where write errors
// are not actionable (e.g., status messages to stderr during auth flow).
type statusWriter struct {
	w io.Writer
}

func (s *statusWriter) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(s.w, format, args...)
}

func (s *statusWriter) msg(a string) {
	_, _ = fmt.Fprintln(s.w, a)
}

func (s *statusWriter) ln() {
	_, _ = fmt.Fprintln(s.w)
}

func printLoginSuccess(stdout io.Writer, w *statusWriter, faint *color.Color, keyResp *auth.CreateKeyResponse, scopes []string, profile string) {
	w.ln()
	_, _ = color.New(color.FgGreen, color.Bold).Fprintln(w.w, "  Logged in successfully!")
	w.ln()
	_, _ = fmt.Fprintf(stdout, "  API Key: %s\n", faint.Sprint(keyResp.KeyPrefix+"..."))
	_, _ = fmt.Fprintf(stdout, "  Key ID:  %s\n", keyResp.KeyID)
	_, _ = fmt.Fprintf(stdout, "  Scopes:  %s\n", strings.Join(scopes, ", "))
	if profile != "" {
		_, _ = fmt.Fprintf(stdout, "  Profile: %s\n", profile)
	}

	configPath := config.Path()
	if profile != "" {
		configPath, _ = config.ProfilePath(profile)
	}
	_, _ = fmt.Fprintf(stdout, "  Saved:   %s\n", faint.Sprint(configPath))
	w.ln()
}
