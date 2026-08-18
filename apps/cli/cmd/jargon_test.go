package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/consume"
	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

// forbiddenWords is the customer-surface jargon contract for the entire v2
// CLI: none of these may appear (case-insensitive) in any non-hidden
// command's Short / Long / Example, any non-hidden flag usage string, or any
// fixed customer-facing message constant anywhere in the CLI. Entries are
// pre-lowercased so substring checks against a lowercased haystack actually
// match. Protocol and implementation vocabulary stays in code and docs for
// developers; customers get plain language. "firewall" is banned by the
// brand rule (qURL is not described in firewall terms), and the typo-warning
// copy must speak of typos, not of the checksum mechanics behind them.
var forbiddenWords = []string{
	"qv2",
	"relay",
	"trust",
	"issuer",
	"signature",
	"admission",
	"proof-of-possession",
	"proof of possession",
	"knock",
	"fail closed",
	"fails closed",
	"errnotconfigured",
	"at_",
	"access token",
	"device key",
	"spki",
	"der",
	"base64",
	"base32",
	"checksum",
	"crc",
	"allowlist",
	"nhp",
	"serverid",
	"cell",
	"firewall",
}

// isAlnumToken reports whether w is only [a-z0-9] (already-lowercased).
func isAlnumToken(w string) bool {
	if w == "" {
		return false
	}
	for _, r := range w {
		isLower := r >= 'a' && r <= 'z'
		isDigit := r >= '0' && r <= '9'
		if !isLower && !isDigit {
			return false
		}
	}
	return true
}

// findForbiddenJargon returns the first forbiddenWords entry present in s.
// Alphanumeric tokens (der, cell, qv2, ...) match on word boundaries so
// innocuous copy like "consider"/"excellent" can't trip them; tokens with
// non-word characters (at_, "access token") match as substrings.
func findForbiddenJargon(s string) (string, bool) {
	lower := strings.ToLower(s)
	for _, w := range forbiddenWords {
		if isAlnumToken(w) {
			if regexp.MustCompile(`\b` + w + `\b`).MatchString(lower) {
				return w, true
			}
		} else if strings.Contains(lower, w) {
			return w, true
		}
	}
	return "", false
}

// visibleSurfaces walks the real command tree and collects every
// customer-visible help surface: Short, Long, Example, and each non-hidden
// flag's usage, for every non-hidden command, recursively. Hidden commands
// (docs, cobra's __complete) are developer surface and exempt.
func visibleSurfaces(cmd *cobra.Command) map[string]string {
	surfaces := map[string]string{}
	var walk func(c *cobra.Command, path string)
	walk = func(c *cobra.Command, path string) {
		if c.Hidden {
			return
		}
		surfaces[path+" short"] = c.Short
		surfaces[path+" long"] = c.Long
		surfaces[path+" example"] = c.Example
		c.Flags().VisitAll(func(f *pflag.Flag) {
			if f.Hidden {
				return
			}
			surfaces[path+" --"+f.Name] = f.Usage
		})
		for _, sub := range c.Commands() {
			walk(sub, path+" "+sub.Name())
		}
	}
	walk(cmd, cmd.Name())
	return surfaces
}

// TestNoJargonOnHelpSurfaces asserts the whole v2 help surface is free of
// forbidden jargon.
func TestNoJargonOnHelpSurfaces(t *testing.T) {
	root, _ := newRoot("test", discardStreams())
	for where, text := range visibleSurfaces(root) {
		if bad, found := findForbiddenJargon(text); found {
			t.Errorf("help surface %q leaked jargon %q:\n%s", where, bad, text)
		}
	}
}

// TestNoJargonInCustomerMessages asserts every fixed customer-facing message
// constant in the CLI — command messages, CRID warnings, API error framing,
// error-rendering hints — is free of forbidden jargon.
func TestNoJargonInCustomerMessages(t *testing.T) {
	all := make([]string, 0, 64)
	all = append(all, customerMessages()...)
	all = append(all, consume.CustomerMessages()...)
	all = append(all, cridux.Messages()...)
	all = append(all, qurlapi.CustomerMessages()...)
	all = append(all, output.CustomerMessages()...)

	if len(all) == 0 {
		t.Fatal("no customer messages collected; the gate would be vacuous")
	}
	for _, msg := range all {
		if msg == "" {
			t.Error("empty customer message registered")
			continue
		}
		if bad, found := findForbiddenJargon(msg); found {
			t.Errorf("customer message leaked jargon %q: %q", bad, msg)
		}
	}
}

// TestFindForbiddenJargon pins the detector both ways: real jargon is caught
// (the gates above aren't vacuous) and innocuous copy that merely contains a
// short token as a substring does not false-positive.
func TestFindForbiddenJargon(t *testing.T) {
	for _, bad := range []string{
		"this uses a relay",
		"verify the issuer signature",
		"a qv2 link",
		"base64 blob",
		"crc mismatch",
		"opens the firewall",
	} {
		if _, found := findForbiddenJargon(bad); !found {
			t.Errorf("expected jargon detected in %q", bad)
		}
	}
	for _, ok := range []string{
		"please consider this",
		"an excellent result",
		"entrust the folder",
		"under the hood",
		"a miscellaneous provider",
		"created just now",
	} {
		if w, found := findForbiddenJargon(ok); found {
			t.Errorf("false positive: %q flagged on %q", w, ok)
		}
	}
}

// TestForbiddenWordsAreLowercase guards the gate itself: a mixed-case entry
// would never match the lowercased haystack and silently pass.
func TestForbiddenWordsAreLowercase(t *testing.T) {
	for _, w := range forbiddenWords {
		if w != strings.ToLower(w) {
			t.Errorf("forbidden word %q is not pre-lowercased", w)
		}
	}
}

func discardStreams() *output.Streams {
	return &output.Streams{
		In:  strings.NewReader(""),
		Out: nopWriter{},
		Err: nopWriter{},
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
