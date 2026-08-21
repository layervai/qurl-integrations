// Package main is the entry point for the qurl CLI.
//
// main is exit mapping only: run the command tree, render the error, exit
// with the one code internal/exitcode assigns. Everything else lives in the
// commands and the internal packages.
package main

import "os"

// version is set at build time via ldflags (see .goreleaser.yml).
var version = "dev"

func main() {
	os.Exit(Main(version))
}
