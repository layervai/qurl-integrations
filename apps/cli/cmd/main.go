// Package main is the entry point for the qurl CLI.
package main

import (
	"fmt"
	"os"
)

// version is set at build time via ldflags.
var version = "dev"

func main() {
	if err := rootCmd(version).Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
