package main

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

func versionCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "qurl version %s (%s/%s)\n",
				version, runtime.GOOS, runtime.GOARCH)
			return err
		},
	}
}
