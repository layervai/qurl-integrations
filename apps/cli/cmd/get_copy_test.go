package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// TestGetCopyStatesWhatOpensWithOnlyACRID pins what get's help and the README
// say about fetching a public resource with only its CRID.
//
// Both say it once, in the same words: it needs no account and no setup, and
// it works only where the deployment offers it. Both then say that the
// deployment this release ships does not offer it yet. That sentence is true
// for as long as TestRequestCRIDLinkUnderTheShippedDeploymentIsNotMade
// passes. A release that ships a deployment which offers the request drops
// it, together with the interim limitation
// TestSharingCopyStatesTheDeviceAccessRule pins in publish help.
//
// Both name the three most common answers when no link is given, with the
// exit codes of the table in internal/exitcode. Both say that these are the
// most common answers and not all of them: get has more, such as a publisher
// that is offline or a resource that was closed.
//
// And neither sends the reader to a login, an account or an API key first:
// fetching a public resource by its CRID has no such step.
func TestGetCopyStatesWhatOpensWithOnlyACRID(t *testing.T) {
	collapse := func(text string) string { return strings.Join(strings.Fields(text), " ") }
	res := runCLI(t, &runOpts{args: []string{"get", "--help"}})
	if res.code != 0 {
		t.Fatalf("qurl get --help exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	help := collapse(res.stdout.String())
	// Markdown marks in the README's copy are dropped, so one set of
	// sentences is compared with both.
	readme := collapse(strings.NewReplacer("`", "", "|", " ").Replace(readCLIREADME(t)))

	const (
		promise = "a public resource can also be fetched on any machine with only its CRID: no account and no setup."
		notYet  = "The deployment this release ships does not offer it yet."
		noNew   = "a machine with no device identity creates none"
		// common introduces the list of answers. allOfThem is the earlier
		// wording, which claimed that the list was complete.
		common    = "The three most common answers when no link is given:"
		allOfThem = "Three answers mean no link was given"
	)
	answers := []struct {
		answer, meaning string
		code            int
	}{
		{"not found", "the CRID is mistyped, the resource was removed, or it is not open to this machine.", exitcode.NotFound},
		{"can't give a link right now", "nothing is wrong with the CRID. Try again later.", exitcode.Unavailable},
		{"too many requests", "wait, then try again.", exitcode.RateLimited},
	}

	for _, surface := range []struct {
		name, text string
		// answerLayout is how the surface lays out one answer: the answer,
		// its exit code, and what it means.
		answerLayout string
	}{
		{"qurl get --help", help, `"%s" (exit code %d): %s`},
		{"README", readme, "%s %d %s"},
	} {
		for _, want := range []string{"Where the deployment offers it, " + promise + " " + notYet, noNew + ". " + common} {
			if got := strings.Count(surface.text, want); got != 1 {
				t.Errorf("%s states %q %d times, want exactly once", surface.name, want, got)
			}
		}
		if strings.Contains(surface.text, allOfThem) {
			t.Errorf("%s says %q, but get has more answers than the three it lists", surface.name, allOfThem)
		}
		lower := strings.ToLower(surface.text)
		for _, a := range answers {
			if want := strings.ToLower(fmt.Sprintf(surface.answerLayout, a.answer, a.code, a.meaning)); !strings.Contains(lower, want) {
				t.Errorf("%s does not explain the answer: missing %q", surface.name, want)
			}
		}
	}

	// The whole of get's help, and the README's paragraph with its table,
	// which ends where the table of get's flags begins.
	start := strings.Index(readme, "Where the deployment offers it, "+promise)
	if start < 0 {
		t.Fatal("the README has no paragraph about fetching with only a CRID")
	}
	length := strings.Index(readme[start:], "Flag Description")
	if length < 0 {
		t.Fatal("the README's paragraph about fetching with only a CRID is not followed by the table of get's flags")
	}
	for where, text := range map[string]string{"qurl get --help": help, "README": readme[start : start+length]} {
		lower := strings.ToLower(text)
		for _, step := range []string{"qurl login", "log in", "sign in", "sign up", "api key", "qurl_api_key", "account setup", "create an account"} {
			if strings.Contains(lower, step) {
				t.Errorf("%s sends the reader to %q before fetching a public resource; that path has no such step", where, step)
			}
		}
	}
}
