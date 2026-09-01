package main

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/sessionrelay"
)

// versionCmd prints the version line. The output shape is a distribution
// contract: the Homebrew formula's install test asserts on `qurl version`
// output, so keep the format stable.
func versionCmd(version string) *cobra.Command {
	var verifyReleaseNativeTrust bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Long:  "Print the qURL CLI version and the platform it was built for.",
		Example: `  qurl version
  qurl version | awk '{print $3}'`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if verifyReleaseNativeTrust {
				if _, err := sessionrelay.EmbeddedProductionURL(); err != nil {
					return err
				}
				fingerprint, err := hub.EmbeddedProductionPinFingerprint()
				if err != nil {
					return err
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), fingerprint)
				return err
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "qurl version %s (%s/%s)\n",
				version, runtime.GOOS, runtime.GOARCH)
			return err
		},
	}
	cmd.Flags().BoolVar(&verifyReleaseNativeTrust, "verify-release-native-trust", false, "verify the embedded native connection settings")
	if err := cmd.Flags().MarkHidden("verify-release-native-trust"); err != nil {
		panic(err)
	}
	return cmd
}
