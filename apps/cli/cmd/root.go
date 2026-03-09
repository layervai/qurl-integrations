package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
	"github.com/layervai/qurl-integrations/shared/client"
)

func rootCmd(version string) *cobra.Command {
	var (
		apiKey   string
		endpoint string
		format   string
	)

	cmd := &cobra.Command{
		Use:   "qurl",
		Short: "QURL CLI - manage secure links from the command line",
		Long: `QURL CLI creates, resolves, and manages QURL secure links.

Set QURL_API_KEY environment variable or use --api-key flag for authentication.`,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.PersistentFlags().StringVar(&apiKey, "api-key", "", "API key (overrides QURL_API_KEY env)")
	cmd.PersistentFlags().StringVar(&endpoint, "endpoint", "", "API endpoint (overrides QURL_ENDPOINT env, default: https://api.layerv.ai)")
	cmd.PersistentFlags().StringVarP(&format, "output", "o", "table", "Output format: table or json")

	cmd.AddCommand(
		createCmd(&apiKey, &endpoint, &format),
		resolveCmd(&apiKey, &endpoint, &format),
		listCmd(&apiKey, &endpoint, &format),
		getCmd(&apiKey, &endpoint, &format),
		deleteCmd(&apiKey, &endpoint, &format),
		extendCmd(&apiKey, &endpoint, &format),
		versionCmd(version),
	)

	return cmd
}

// defaultEndpoint is the production API endpoint.
const defaultEndpoint = "https://api.layerv.ai"

func newClient(apiKey, endpoint *string) (*client.Client, error) {
	key := envOrFlag("QURL_API_KEY", apiKey)
	if key == "" {
		return nil, errMissingAPIKey
	}

	ep := envOrFlag("QURL_ENDPOINT", endpoint)
	if ep == "" {
		ep = defaultEndpoint
	}

	return client.New(ep, key), nil
}

var errMissingAPIKey = &cliError{msg: "API key required: set QURL_API_KEY or use --api-key"}

type cliError struct {
	msg string
}

func (e *cliError) Error() string { return e.msg }

func envOrFlag(envKey string, flag *string) string {
	if flag != nil && *flag != "" {
		return *flag
	}
	v, _ := os.LookupEnv(envKey)
	return v
}

func getFormatter(format *string) output.Formatter {
	if format != nil && *format == "json" {
		return output.JSONFormatter{}
	}
	return output.TableFormatter{}
}
