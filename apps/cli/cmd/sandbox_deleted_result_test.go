//go:build clisandbox

package main

import (
	"fmt"
	"net/url"
	"regexp"
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

// sandboxSharePublisherNotice is the exact shape of the one line a piped
// `qurl share` writes to stderr: the publisher notice, always UNVERIFIED
// because no publisher can be verified, with an optional creation date. The
// name is the publisher's quoted, escaped text; a sandbox running a service
// that predates publisher metadata reports no name and no date.
var sandboxSharePublisherNotice = regexp.MustCompile(
	`^Warning: UNVERIFIED publisher(?:, no name provided \(not confirmed by LayerV\)| "(?:[^"\\[:cntrl:]]|\\.)*" \(self-declared name, not confirmed by LayerV\))\.(?: Created [0-9]{4}-[0-9]{2}-[0-9]{2}\.)?\n$`,
)

// validateSandboxShareCommandResult holds the piped share contract: stdout is
// exactly one HTTPS link, and stderr is exactly the publisher notice. Any
// other stderr - an environment-guard warning, a clamp note, a second line -
// still fails. Like the deleted-result validator, the diagnostic reports only
// lengths and fixed booleans.
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
	if !sandboxSharePublisherNotice.MatchString(stderr) {
		return "", fmt.Errorf("%s wrote %d stderr bytes that are not exactly the UNVERIFIED publisher notice (contains UNVERIFIED: %t); private details withheld",
			name, len(stderr), strings.Contains(stderr, "UNVERIFIED"))
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
	const (
		validLink   = "https://access.invalid/open#qv3.valid-secret"
		namedNotice = "Warning: UNVERIFIED publisher \"Acme Docs\" (self-declared name, not confirmed by LayerV). Created 2026-03-01.\n"
	)
	for name, notice := range map[string]string{
		"named with date":      namedNotice,
		"named without date":   "Warning: UNVERIFIED publisher \"Acme Docs\" (self-declared name, not confirmed by LayerV).\n",
		"unnamed with date":    "Warning: UNVERIFIED publisher, no name provided (not confirmed by LayerV). Created 2026-03-01.\n",
		"unnamed without date": "Warning: UNVERIFIED publisher, no name provided (not confirmed by LayerV).\n",
		"escaped name":         "Warning: UNVERIFIED publisher \"Acme \\\"Docs\\\" \\u202e\" (self-declared name, not confirmed by LayerV).\n",
	} {
		link, err := validateSandboxShareCommandResult("share", 0, validLink+"\n", notice)
		if err != nil || link != validLink {
			t.Fatalf("%s: valid share = link length %d, error %v", name, len(link), err)
		}
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
		// The notice is required: a piped share that says nothing about the
		// publisher has lost the UNVERIFIED warning.
		"missing notice": {stdout: validLink + "\n"},
		"second line": {
			stdout: validLink + "\n",
			stderr: namedNotice + "Warning: stderr-secret\n",
		},
		"leading line": {
			stdout: validLink + "\n",
			stderr: "Warning: stderr-secret\n" + namedNotice,
		},
		"unterminated": {
			stdout: validLink + "\n",
			stderr: strings.TrimSuffix(namedNotice, "\n"),
		},
		"lowercase status": {
			stdout: validLink + "\n",
			stderr: "Warning: unverified publisher \"stderr-secret\" (self-declared name, not confirmed by LayerV).\n",
		},
		"verified publisher": {
			stdout: validLink + "\n",
			stderr: "Publisher \"stderr-secret\" (verified by LayerV).\n",
		},
		"name breaks out of its quotes": {
			stdout: validLink + "\n",
			stderr: "Warning: UNVERIFIED publisher \"Acme\" stderr-secret \"x\" (self-declared name, not confirmed by LayerV).\n",
		},
		"raw control character in the name": {
			stdout: validLink + "\n",
			stderr: "Warning: UNVERIFIED publisher \"stderr-secret\x1b[0m\" (self-declared name, not confirmed by LayerV).\n",
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
