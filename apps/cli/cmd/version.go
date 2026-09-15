package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/layervai/qurl-go/qurl"
	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
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
				// A deployment override must not hide missing settings in an artifact.
				if strings.TrimSpace(os.Getenv(qurl.EnvDeploymentPath)) != "" {
					return fmt.Errorf("release verification requires %s to be unset", qurl.EnvDeploymentPath)
				}
				// An empty link reaches parsing only after default trust loads.
				// It cannot request access or fetch content.
				if _, err := qurl.EnterPortal(cmd.Context(), ""); !errors.Is(err, qurl.ErrFragment) {
					return fmt.Errorf("release deployment verification failed: %w", err)
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
	cmd.Flags().BoolVar(&verifyReleaseNativeTrust, "verify-release-native-trust", false, "verify the embedded native Hub trust root")
	if err := cmd.Flags().MarkHidden("verify-release-native-trust"); err != nil {
		panic(err)
	}
	return cmd
}
