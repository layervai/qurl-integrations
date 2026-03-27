package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/apps/cli/internal/config"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
	"github.com/layervai/qurl-integrations/shared/client"
)

// globalOpts holds flags and config shared across all subcommands.
type globalOpts struct {
	apiKey   string
	endpoint string
	format   string
	quiet    bool
	verbose  bool
	profile  string
	version  string
}

func rootCmd(version string) *cobra.Command {
	opts := &globalOpts{version: version}

	// Respect NO_COLOR convention (https://no-color.org/).
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		color.NoColor = true
	}

	cmd := &cobra.Command{
		Use:   "qurl",
		Short: "QURL CLI - manage secure links from the command line",
		Long: `QURL CLI creates, resolves, and manages QURL secure links.

Authentication (in order of precedence):
  1. --api-key flag (visible in process list — prefer env var)
  2. QURL_API_KEY environment variable (recommended)
  3. ~/.config/qurl/config.yaml (or --profile <name>)

Get started:
  qurl auth login                        Authenticate via browser
  qurl create https://example.com        Create a QURL
  qurl list                              List active QURLs
  qurl resolve <access-token>            Resolve a token (headless)
  qurl quota                             Check your usage
  qurl completion bash                   Generate shell completions`,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if opts.format != output.FormatTable && opts.format != output.FormatJSON && opts.format != "" {
				return fmt.Errorf("invalid output format %q: must be %q or %q", opts.format, output.FormatTable, output.FormatJSON)
			}
			return nil
		},
	}

	cmd.PersistentFlags().StringVar(&opts.apiKey, "api-key", "", "API key (prefer env var or config file to avoid exposure in process list)")
	cmd.PersistentFlags().StringVar(&opts.endpoint, "endpoint", "", "API endpoint (default: https://api.layerv.ai)")
	cmd.PersistentFlags().StringVarP(&opts.format, "output", "o", output.FormatTable, "Output format: table or json")
	cmd.PersistentFlags().BoolVarP(&opts.quiet, "quiet", "q", false, "Minimal output (just the essential value)")
	cmd.PersistentFlags().BoolVarP(&opts.verbose, "verbose", "v", false, "Show HTTP request/response details")
	cmd.PersistentFlags().StringVar(&opts.profile, "profile", "", "Config profile name (reads ~/.config/qurl/profiles/<name>.yaml)")

	cmd.AddCommand(
		authCmd(opts),
		createCmd(opts),
		resolveCmd(opts),
		listCmd(opts),
		getCmd(opts),
		deleteCmd(opts),
		updateCmd(opts),
		extendCmd(opts),
		mintCmd(opts),
		quotaCmd(opts),
		configCmd(),
		completionCmd(),
		docsCmd(),
		versionCmd(version),
	)

	return cmd
}

// defaultEndpoint is the production API endpoint.
const defaultEndpoint = "https://api.layerv.ai"

func (o *globalOpts) newClient() (*client.Client, error) {
	// Resolve profile: flag > env > default.
	profile := o.profile
	if profile == "" {
		profile = os.Getenv("QURL_PROFILE")
	}

	// Load config from profile or default location.
	cfg, err := config.LoadProfile(profile)
	if err != nil && profile != "" {
		return nil, fmt.Errorf("load profile %q: %w", profile, err)
	}

	key := resolveValue("QURL_API_KEY", &o.apiKey, "api_key", cfg)
	if key == "" {
		return nil, errMissingAPIKey
	}

	ep := resolveValue("QURL_ENDPOINT", &o.endpoint, "endpoint", cfg)
	if ep == "" {
		ep = defaultEndpoint
	}

	opts := []client.Option{
		client.WithUserAgent("qurl-cli/" + o.version),
	}
	if o.verbose {
		opts = append(opts, client.WithLogger(log.New(os.Stderr, "[debug] ", 0)))
	}

	return client.New(ep, key, opts...), nil
}

func (o *globalOpts) formatter() output.Formatter {
	if o.format == output.FormatJSON {
		return output.JSONFormatter{}
	}
	return output.NewTableFormatter()
}

var errMissingAPIKey = &cliError{msg: "API key required: run `qurl auth login`, set QURL_API_KEY, or run `qurl config set api_key <key>`"}

type cliError struct {
	msg string
}

func (e *cliError) Error() string { return e.msg }

// resolveValue returns the first non-empty value from: flag, env var, config file.
func resolveValue(envKey string, flag *string, configKey string, cfg *config.Config) string {
	if flag != nil && *flag != "" {
		return *flag
	}
	if v, ok := os.LookupEnv(envKey); ok && v != "" {
		return v
	}
	if cfg != nil {
		if v := cfg.Get(configKey); v != "" {
			return v
		}
	}
	return ""
}

// formatError renders an APIError with color and actionable hints.
func formatError(err error) string {
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		// Unwrap to show the root cause while preserving context prefix.
		return err.Error()
	}

	// Preserve wrapping context (e.g., "create QURL: ...").
	prefix := ""
	if wrapper := err.Error(); wrapper != apiErr.Error() {
		// Extract the prefix before the APIError message.
		if idx := strings.Index(wrapper, apiErr.Error()); idx > 0 {
			prefix = strings.TrimRight(wrapper[:idx], ": ") + ": "
		}
	}

	red := color.New(color.FgRed, color.Bold).SprintFunc()
	dim := color.New(color.Faint).SprintFunc()

	msg := fmt.Sprintf("%s %s%s (%d)", red("Error:"), prefix, apiErr.Title, apiErr.StatusCode)
	if apiErr.Detail != "" {
		msg += "\n\n  " + apiErr.Detail
	}

	if len(apiErr.InvalidFields) > 0 {
		keys := make([]string, 0, len(apiErr.InvalidFields))
		for k := range apiErr.InvalidFields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fields := make([]string, 0, len(keys))
		for _, field := range keys {
			fields = append(fields, fmt.Sprintf("    %s: %s", field, apiErr.InvalidFields[field]))
		}
		msg += "\n\n  Invalid fields:\n" + strings.Join(fields, "\n")
	}

	// Actionable hints
	switch {
	case apiErr.StatusCode == http.StatusUnauthorized:
		msg += "\n\n  " + dim("Hint: Check your API key — run `qurl auth login` or set QURL_API_KEY")
	case apiErr.StatusCode == http.StatusTooManyRequests && apiErr.RetryAfter > 0:
		msg += fmt.Sprintf("\n\n  "+dim("Retry after %ds"), apiErr.RetryAfter)
	case apiErr.Code == "quota_exceeded":
		msg += "\n\n  " + dim("Hint: Upgrade your plan at https://layerv.ai/pricing")
	}

	if apiErr.RequestID != "" {
		msg += "\n  " + dim("Request ID: "+apiErr.RequestID)
	}

	return msg
}
