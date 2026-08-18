package agent

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// The one-shot Connector enrollment credential surface. Deliberately DISTINCT
// from the CLI's durable API key (QURL_API_KEY, internal/auth): the
// enrollment token is spent once at first registration and never stored,
// while the API key is the durable customer credential. The two never share
// an env var, and this package never reads the CLI's credential store or its
// keyring-less token file, so one can never be spent as the other.
const (
	// EnvEnrollmentToken carries the one-shot enrollment token inline.
	EnvEnrollmentToken = "QURL_CONNECTOR_TOKEN"
	// EnvEnrollmentTokenFile points at a file containing the enrollment
	// token. Standard _FILE convention: preferred over the inline variant
	// for container deployments because it keeps the secret out of the
	// process environment and container inspection output.
	EnvEnrollmentTokenFile = "QURL_CONNECTOR_TOKEN_FILE"
)

// enrollmentTokenFileMaxBytes caps the token-file read. Enrollment tokens are
// comfortably under 4 KiB; the cap exists so a misconfigured mount pointing
// at /dev/zero or a log file cannot load arbitrary data into memory.
const enrollmentTokenFileMaxBytes = 4 * 1024

// errSecretFileTooLarge is returned when the token file exceeds the cap.
// Strict-no-silent-truncate: callers surface this as a misconfiguration
// rather than handing a truncated blob to the platform (which would reject it
// with a generic invalid-token error several layers away from the cause).
var errSecretFileTooLarge = fmt.Errorf("file exceeds %d byte cap (Connector enrollment tokens fit well under this)", enrollmentTokenFileMaxBytes)

// resolveEnrollmentToken returns the one-shot enrollment credential.
// Resolution order:
//
//  1. the explicit value (the command's --token flag), highest precedence
//  2. QURL_CONNECTOR_TOKEN_FILE — the trimmed contents of that file
//  3. QURL_CONNECTOR_TOKEN
//
// All paths trim surrounding whitespace: a token never legitimately carries
// it, and trimming uniformly avoids a "works via file but not via env"
// footgun when a value is pasted with a trailing newline.
//
// When QURL_CONNECTOR_TOKEN_FILE is set there is NO fallback to the inline
// env var on a read error or empty file: the _FILE variant exists to keep the
// secret out of the process environment, and silently degrading would defeat
// that and mask the misconfiguration — both failure modes are returned as
// errors naming the path.
//
// An empty result with a nil error means "no token configured", which is the
// normal warm-open case; first registration fails with a token-required hint
// only when it actually needs one.
func resolveEnrollmentToken(explicit string) (string, error) {
	if t := strings.TrimSpace(explicit); t != "" {
		return t, nil
	}
	if path := strings.TrimSpace(os.Getenv(EnvEnrollmentTokenFile)); path != "" {
		val, err := readSecretFile(path)
		if err != nil {
			return "", fmt.Errorf("%s (%q) read failed: %w", EnvEnrollmentTokenFile, path, err)
		}
		if val == "" {
			return "", fmt.Errorf("%s set to %q but the file is empty or whitespace-only", EnvEnrollmentTokenFile, path)
		}
		return val, nil
	}
	return strings.TrimSpace(os.Getenv(EnvEnrollmentToken)), nil
}

// readSecretFile reads a path with a size cap and trims surrounding
// whitespace. It reads one byte past the cap and reports
// errSecretFileTooLarge when the file exceeds it, so a misconfigured mount
// fails loudly rather than silently truncating.
func readSecretFile(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the operator-supplied path is the whole point of the _FILE indirection.
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, enrollmentTokenFileMaxBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > enrollmentTokenFileMaxBytes {
		return "", errSecretFileTooLarge
	}
	return strings.TrimSpace(string(data)), nil
}
