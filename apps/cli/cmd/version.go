package main

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// versionCmd prints the version line. The output shape is a distribution
// contract: the Homebrew formula's install test asserts on `qurl version`
// output, so keep the format stable.
func versionCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Long:  "Print the qURL CLI version and the platform it was built for.",
		Example: `  qurl version
  qurl version | awk '{print $3}'`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "qurl version %s (%s/%s)\n",
				version, runtime.GOOS, runtime.GOARCH)
			return err
		},
	}
}
