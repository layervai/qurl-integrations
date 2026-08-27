// Command verify-sandbox-pin gates a CLI release on the sandbox Hub public
// key matching the repository's reviewed raw-key fingerprint.
package main

import (
	"crypto/subtle"
	"fmt"
	"io"
	"os"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
)

const usage = "verify-sandbox-pin <fingerprint-file>"

func main() {
	os.Exit(run(os.Args[1:], os.LookupEnv, os.ReadFile, os.Stdout, os.Stderr))
}

func run(
	args []string,
	lookupEnv func(string) (string, bool),
	readFile func(string) ([]byte, error),
	stdout, stderr io.Writer,
) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintf(stderr, "usage: %s\n", usage)
		return 2
	}

	candidate, set := lookupEnv(hub.ReleaseEnvSandboxServerPublicKey)
	if !set || candidate == "" {
		_, _ = fmt.Fprintf(stderr, "%s repository variable must be non-empty\n", hub.ReleaseEnvSandboxServerPublicKey)
		return 1
	}
	rawKey, err := hub.DecodeServerPublicKeyB64(candidate)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s repository variable %v\n", hub.ReleaseEnvSandboxServerPublicKey, err)
		return 1
	}

	contents, err := readFile(args[0])
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "read sandbox Hub fingerprint: %v\n", err)
		return 1
	}
	want, err := hub.ParseFingerprintFile(contents)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "parse sandbox Hub fingerprint: %v\n", err)
		return 1
	}
	got := hub.FingerprintSHA256Hex(rawKey)
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		_, _ = fmt.Fprintf(stderr, "sandbox Hub public-key fingerprint mismatch: got %s, want %s\n", got, want)
		return 1
	}

	_, _ = fmt.Fprintf(stdout, "verified sandbox Hub public-key fingerprint %s\n", got)
	return 0
}
