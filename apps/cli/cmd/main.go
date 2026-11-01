// Package main is the entry point for the qurl CLI.
//
// main sets timestamped diagnostics, runs the command tree, and maps its exit code.
// Command behavior lives in the commands and internal packages.
package main

import (
	"log/slog"
	"os"
)

// version is set at build time via ldflags (see .goreleaser.yml).
var version = "dev"

func main() {
	// Offset-bearing timestamps keep daemon diagnostics distinct across DST clock changes.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	os.Exit(Main(version))
}
