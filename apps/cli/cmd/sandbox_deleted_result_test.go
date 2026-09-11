//go:build clisandbox

package main

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// validateSandboxDeletedCommandResult deliberately reports only lengths and
// fixed booleans. Share stdout can contain live access authority when the
// deletion fence regresses, so no child output may enter the diagnostic.
func validateSandboxDeletedCommandResult(name string, gotCode int, stdout, stderr string) error {
	hasDeletedDiagnostic := strings.Contains(strings.ToLower(stderr), "deleted")
	if gotCode == exitcode.NotFound && stdout == "" && hasDeletedDiagnostic {
		return nil
	}
	return fmt.Errorf(
		"%s after Connector delete = exit code %d, stdout %d bytes, stderr %d bytes, deleted diagnostic %t; want owner-truthful deleted response",
		name,
		gotCode,
		len(stdout),
		len(stderr),
		hasDeletedDiagnostic,
	)
}

func validateSandboxShareCommandResult(name string, gotCode int, stdout, stderr string) (string, error) {
	if gotCode != 0 {
		return "", fmt.Errorf("%s = exit code %d, stdout %d bytes, stderr %d bytes; private details withheld", name, gotCode, len(stdout), len(stderr))
	}
	link := strings.TrimSuffix(stdout, "\n")
	if link == "" || link+"\n" != stdout || strings.ContainsAny(link, " \r\n\t") {
		return "", fmt.Errorf("%s returned %d stdout bytes with an invalid single-link shape; private details withheld", name, len(stdout))
	}
	parsed, err := url.Parse(link)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return "", fmt.Errorf("%s returned a non-HTTPS access link; private details withheld", name)
	}
	if stderr != "" {
		return "", fmt.Errorf("%s wrote %d stderr bytes; private details withheld", name, len(stderr))
	}
	return link, nil
}

func TestSandboxDeletedCommandDiagnosticWithholdsChildOutput(t *testing.T) {
	t.Parallel()
	if err := validateSandboxDeletedCommandResult("share", exitcode.NotFound, "", "resource was deleted"); err != nil {
		t.Fatalf("valid deleted result = %v", err)
	}
	const (
		stdoutAuthority = "https://access.invalid/#qv3.stdout-secret"
		stderrAuthority = "stderr-secret"
	)
	err := validateSandboxDeletedCommandResult("share", 0, stdoutAuthority, stderrAuthority)
	if err == nil {
		t.Fatal("unexpected successful share result was accepted")
	}
	for _, secret := range []string{stdoutAuthority, stderrAuthority} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("deleted-result diagnostic exposed child output")
		}
	}
}

func TestSandboxShareCommandDiagnosticWithholdsChildOutput(t *testing.T) {
	const validLink = "https://access.invalid/open#qv3.valid-secret"
	link, err := validateSandboxShareCommandResult("share", 0, validLink+"\n", "")
	if err != nil || link != validLink {
		t.Fatalf("valid share = link length %d, error %v", len(link), err)
	}
	for name, test := range map[string]struct {
		code   int
		stdout string
		stderr string
	}{
		"failed command": {code: 1, stdout: validLink + "\n", stderr: "stderr-secret"},
		"bad shape":      {stdout: validLink},
		"bad URL":        {stdout: "not-a-url#qv3.bad-url-secret\n"},
		"unexpected stderr": {
			stdout: validLink + "\n",
			stderr: "stderr-secret",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := validateSandboxShareCommandResult("share", test.code, test.stdout, test.stderr)
			if err == nil {
				t.Fatal("invalid share result was accepted")
			}
			for _, secret := range []string{test.stdout, test.stderr} {
				if secret != "" && strings.Contains(err.Error(), secret) {
					t.Fatal("share diagnostic exposed child output")
				}
			}
		})
	}
}
