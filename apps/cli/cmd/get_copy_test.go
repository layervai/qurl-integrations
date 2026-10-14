package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

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
// it, together with the same sentence in publish help and in the README's
// section about private CRIDs (TestSharingCopyStatesTheDeviceAccessRule) and
// in the share not-found hint.
//
// Both say in the same words which request comes first where it is offered:
// get asks for a link this way first, and a device that has an identity asks
// as this device. A machine that has an account key and no device identity
// yet asks first too, with the CRID alone: that is intended. These two mint
// a share link only when no link is given. On a machine with no device
// identity and no account key the answer is final and no identity is
// created.
//
// The README also says what asking this way costs. The service limits these
// requests for each source address, and a device that has an identity can
// send two of them for one CRID. A machine that has an account key sends one
// request before it mints a share link. Both wait for the answer before they
// mint a share link, for as long as cridLinkTimeoutBeforeShare says; when no
// answer came in that time and no share link can be minted, get asks once
// more. A device that reaches the limit mints a share link, which gives a
// link only when the device may share the resource. The README gives no
// number for the limit: the public documents of the SDK give none.
//
// Both name the three most common answers when no link is given, with the
// exit codes of the table in internal/exitcode. Both say that these are the
// most common answers and not all of them: get has more, such as a publisher
// that is offline or a resource that was closed.
//
// And neither sends the reader to a login, an account or an API key first:
// fetching a public resource by its CRID has no such step. Both name an
// account key only to say what a machine that already has one does.
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
		noNew   = "that machine creates no device identity"
		// order says which request comes first, and how each kind of machine
		// asks.
		order = "Where it is offered, get asks for a link this way first. " +
			"On Linux and macOS a device that has an identity asks as this device, so it can also get a link this way for a private resource it is allowed to open. " +
			"On other systems it asks with the CRID alone. " +
			"A machine that has an account key in its environment and no device identity yet asks first too, with the CRID alone. " +
			"Only when no link is given do these mint a share link. " +
			"On a machine with no device identity and no account key the answer is final, and " + noNew + "."
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
		for _, want := range []string{"Where the deployment offers it, " + promise + " " + notYet, noNew + ". " + common, notYet + " " + order + " " + common} {
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

	// What asking this way costs, in the README only. The time a machine
	// waits before it mints a share link is the limit in the code.
	if cridLinkTimeoutBeforeShare%time.Second != 0 {
		t.Fatalf("cridLinkTimeoutBeforeShare = %s; the README states it in whole seconds", cridLinkTimeoutBeforeShare)
	}
	for _, want := range []string{
		"The service limits these requests for each source address, so machines that reach the service from one shared address use the same limit.",
		"A machine with no device identity and no account key sends one request for each link.",
		`A device that has an identity sends one request for a public resource. For any other CRID it sends up to two on Linux and macOS: when the first answer is "not found", it asks once more as this device. On other systems it sends one.`,
		"A machine that has an account key and no device identity yet sends one request for each link before it mints a share link, and that request counts in the limit. " +
			"For a resource it may share, that is one request more than it sent before get asked this way first.",
		fmt.Sprintf("Asking first also costs time when the service does not answer. "+
			"A device that has an identity, and a machine that has an account key, wait up to %d seconds for the answer before they mint a share link.",
			cridLinkTimeoutBeforeShare/time.Second),
		`If no answer came in that time, and the attempt to mint a share link ends in "not found", get asks once more with the CRID alone and waits longer for that answer, as it did before it asked this way first. ` +
			"So a slow service does not cost a link. That request counts in the limit too.",
		"When the limit is reached, a device that has an identity mints a share link, as it does for every answer that is not a link, and so does a machine that has an account key. " +
			"That gives a link when this device may share the resource.",
		`When it may not, the answer is "too many requests", as it is on a machine with no device identity and no account key. Wait, then try again.`,
		"With --session-duration, get mints the share link first, as it did before, because only a share link can carry that lifetime.",
	} {
		if got := strings.Count(readme, want); got != 1 {
			t.Errorf("the README states %q %d times, want exactly once", want, got)
		}
	}
	// The earlier wording promised a link at the limit to every device that
	// has an identity. A device that may not share the resource gets none.
	if stillGets := "still gets its link"; strings.Contains(readme, stillGets) {
		t.Errorf("the README says %q, but a device that reaches the limit and may not share the resource gets no link", stillGets)
	}

	// The whole of get's help, and the README's paragraphs with the table of
	// answers and the cost of asking, which end where the table of get's
	// flags begins.
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
