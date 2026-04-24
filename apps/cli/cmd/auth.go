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

Use "auth login" to authenticate via your browser.
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
		Long: `Authenticate with QURL using the OAuth 2.0 Authorization Code flow with PKCE.

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
		return errors.New("Auth0 client ID not configured: this build was not compiled with a default client ID; set QURL_AUTH0_CLIENT_ID to override")
	}

	if len(scopes) == 0 {
		scopes = allScopes
	}
	if err := validateScopes(scopes); err != nil {
		return err
	}

	// Load config before starting the OAuth flow so a malformed config fails fast
	// rather than after the user completes the browser flow and a server-side key
	// has already been minted.
	profile := resolveProfile(opts)
	loginCfg, cfgLoadErr := config.LoadProfile(profile)
	if cfgLoadErr != nil {
		return fmt.Errorf("load config: %w", cfgLoadErr)
	}

	flowCfg := &auth.PKCEConfig{
		Domain:   domain,
		ClientID: clientID,
		Audience: audience,
		Scopes:   scopes,
		BaseURL:  os.Getenv("QURL_AUTH0_URL"), // Override for testing.
	}
	flow := auth.NewPKCEFlow(flowCfg)

	session, err := flow.StartLogin(ctx)
	if err != nil {
		return fmt.Errorf("start login: %w", err)
	}

	faint := color.New(color.Faint)
	w.ln()

	if !noBrowser {
		w.printf("  Opening browser to authenticate...\n")
		if browserErr := auth.OpenBrowser(session.AuthURL); browserErr != nil {
			w.printf("  Could not open browser. Visit this URL:\n")
			w.printf("  %s\n", session.AuthURL)
		}
	} else {
		w.printf("  Visit this URL to authenticate:\n")
		w.printf("  %s\n", session.AuthURL)
	}

	w.ln()
	w.printf("  Waiting for authentication...")

	// Give the user up to 10 minutes to complete the browser flow before timing out.
	loginCtx, loginCancel := context.WithTimeout(ctx, 10*time.Minute)
	defer loginCancel()
	token, err := session.WaitForToken(loginCtx)
	if err != nil {
		w.ln()
		if errors.Is(err, context.DeadlineExceeded) {
			return errors.New("authentication timed out: browser flow not completed within 10 minutes")
		}
		return fmt.Errorf("authentication failed: %w", err)
	}
	w.msg(" done")

	w.printf("  Creating API key...")

	name := resolveKeyName(keyName)
	endpoint := resolveEndpoint(opts, loginCfg)

	// Use loginCtx so key creation is bounded by the same 10-minute deadline
	// as the browser flow rather than falling through to the HTTP client's default.
	keyResp, err := auth.CreateAPIKey(loginCtx, nil, endpoint, token.AccessToken, auth.CreateKeyRequest{
		Name:   name,
		Scopes: scopes,
	})
	if err != nil {
		w.ln()
		return fmt.Errorf("create API key: %w", err)
	}
	w.msg(" done")

	if saveErr := saveAuthConfig(profile, keyResp.APIKey, keyResp.KeyID); saveErr != nil {
		w.ln()
		w.printf("  Warning: could not save config: %v\n", saveErr)
		w.printf("  Your API key (save manually): %s\n", keyResp.APIKey)
		w.printf("  Store this key securely — it will not be shown again.\n")
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

			cfg, loadErr := config.LoadProfile(profile)
			if loadErr != nil {
				return fmt.Errorf("load config: %w", loadErr)
			}
			if cfg.APIKey == "" {
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
			apiKey, source, cfgErr := resolveAPIKeyWithSource(opts)
			if cfgErr != nil {
				return cfgErr
			}

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

			statusCfg, _ := config.LoadProfile(resolveProfile(opts))
			ep := resolveEndpoint(opts, statusCfg)
			c := client.New(ep, apiKey, client.WithUserAgent("qurl-cli/"+opts.version))

			statusCtx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()

			quota, qErr := c.GetQuota(statusCtx)
			if qErr == nil {
				_, _ = fmt.Fprintf(stdout, "  Plan:    %s\n", bold.Sprint(strings.ToUpper(quota.Plan)))
			} else {
				_, _ = color.New(color.Faint).Fprintf(stdout, "  Plan:    (unavailable: %s)\n", qErr)
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

// resolveAPIKeyWithSource returns the API key, its source label, and any error
// loading the config file. A missing config file is not an error; a malformed
// one is, so callers can distinguish "not configured" from "broken config".
func resolveAPIKeyWithSource(opts *globalOpts) (key, source string, err error) {
	if opts.apiKey != "" {
		return opts.apiKey, "--api-key flag", nil
	}
	if v := os.Getenv("QURL_API_KEY"); v != "" {
		return v, "QURL_API_KEY environment variable", nil
	}

	profile := resolveProfile(opts)
	cfg, loadErr := config.LoadProfile(profile)
	if loadErr != nil {
		return "", "", fmt.Errorf("load config: %w", loadErr)
	}
	if cfg != nil && cfg.APIKey != "" {
		if profile != "" {
			return cfg.APIKey, fmt.Sprintf("profile %q", profile), nil
		}
		return cfg.APIKey, "config file", nil
	}
	return "", "", nil
}

// resolveProfile returns the active profile name from flag or env var.
func resolveProfile(opts *globalOpts) string {
	if opts.profile != "" {
		return opts.profile
	}
	return os.Getenv("QURL_PROFILE")
}

// resolveEndpoint returns the API endpoint from flag, env, config file, or default.
// cfg may be nil when no config has been loaded yet (falls back to flag/env only).
func resolveEndpoint(opts *globalOpts, cfg *config.Config) string {
	ep := resolveValue("QURL_ENDPOINT", &opts.endpoint, "endpoint", cfg)
	if ep == "" {
		ep = defaultEndpoint
	}
	return ep
}

// validateScopes checks that all provided scopes are recognized.
func validateScopes(scopes []string) error {
	valid := make(map[string]bool, len(allScopes))
	for _, s := range allScopes {
		valid[s] = true
	}
	for _, s := range scopes {
		if !valid[s] {
			return fmt.Errorf("unknown scope %q (valid: %s)", s, strings.Join(allScopes, ", "))
		}
	}
	return nil
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

// saveAuthConfig stores the API key and key ID in the config file, preserving
// any other fields (endpoint, output format, etc.) that are already present.
// A malformed existing config is an error — the call site warns the user and
// prints the key for manual recovery.
func saveAuthConfig(profile, apiKey, keyID string) error {
	// LoadProfile always returns a non-nil *Config on success (missing file
	// yields an empty config, not nil), so no nil guard is needed here.
	cfg, err := config.LoadProfile(profile)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
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

// printLoginSuccess prints the post-login success message.
//
// Output split: the animated "Logged in successfully!" banner is written to
// stderr (w.w) so that it appears on the terminal even when stdout is
// redirected. The actionable data (API key prefix, key ID, scopes, saved path)
// goes to stdout so that `qurl auth login > out.txt` captures the machine-
// readable fields. This mirrors how `auth status` works.
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
