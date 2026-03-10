package main

import (
	"errors"
	"fmt"
	"log"
	"os"
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
  1. --api-key flag
  2. QURL_API_KEY environment variable
  3. ~/.config/qurl/config.yaml (or --profile <name>)

Get started:
  qurl create https://example.com        Create a QURL
  qurl list                              List active QURLs
  qurl resolve <access-token>            Resolve a token (headless)
  qurl quota                             Check your usage
  qurl completion bash                   Generate shell completions`,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.PersistentFlags().StringVar(&opts.apiKey, "api-key", "", "API key (prefer env var or config file to avoid exposure in process list)")
	cmd.PersistentFlags().StringVar(&opts.endpoint, "endpoint", "", "API endpoint (default: https://api.layerv.ai)")
	cmd.PersistentFlags().StringVarP(&opts.format, "output", "o", "table", "Output format: table or json")
	cmd.PersistentFlags().BoolVarP(&opts.quiet, "quiet", "q", false, "Minimal output (just the essential value)")
	cmd.PersistentFlags().BoolVarP(&opts.verbose, "verbose", "v", false, "Show HTTP request/response details")
	cmd.PersistentFlags().StringVar(&opts.profile, "profile", "", "Config profile name (reads ~/.config/qurl/profiles/<name>.yaml)")

	cmd.AddCommand(
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
	cfg, _ := config.LoadProfile(profile)

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
	if o.format == "json" {
		return output.JSONFormatter{}
	}
	return output.NewTableFormatter()
}

var errMissingAPIKey = &cliError{msg: "API key required: set QURL_API_KEY, use --api-key, or run `qurl config set api_key <key>`"}

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
		return err.Error()
	}

	red := color.New(color.FgRed, color.Bold).SprintFunc()
	dim := color.New(color.Faint).SprintFunc()

	msg := fmt.Sprintf("%s %s (%d)", red("Error:"), apiErr.Title, apiErr.StatusCode)
	if apiErr.Detail != "" {
		msg += "\n\n  " + apiErr.Detail
	}

	if len(apiErr.InvalidFields) > 0 {
		var fields []string
		for field, reason := range apiErr.InvalidFields {
			fields = append(fields, fmt.Sprintf("    %s: %s", field, reason))
		}
		msg += "\n\n  Invalid fields:\n" + strings.Join(fields, "\n")
	}

	// Actionable hints
	switch {
	case apiErr.StatusCode == 401:
		msg += "\n\n  " + dim("Hint: Check your API key — set QURL_API_KEY or run `qurl config set api_key <key>`")
	case apiErr.StatusCode == 429 && apiErr.RetryAfter > 0:
		msg += fmt.Sprintf("\n\n  "+dim("Retry after %ds"), apiErr.RetryAfter)
	case apiErr.Code == "quota_exceeded":
		msg += "\n\n  " + dim("Hint: Upgrade your plan at https://layerv.ai/pricing")
	}

	if apiErr.RequestID != "" {
		msg += "\n  " + dim("Request ID: "+apiErr.RequestID)
	}

	return msg
}
