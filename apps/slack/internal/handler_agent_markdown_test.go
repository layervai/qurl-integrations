package internal

import (
	"strings"
	"testing"
)

const (
	testNestedBillingMarkdownLink = "[billing](https://evil.example/login)"
	testNestedClickMarkdownLink   = "[click](https://evil.example/phish)"
	testSafeURLPrefix             = "https://safe.example/"
	testEscapedBracketRealLink    = `Literal \[safe label](https://example.invalid) but real [link](https://evil.example).`
	testUnclosedCodeMarkdownLink  = "` then [click me](https://evil.example/phish)"
)

func TestHardenAgentMarkdown_RevealsMaskedLinks(t *testing.T) {
	t.Parallel()
	in := "Read [the setup guide](https://docs.example/setup) before clicking [go](<https://evil.example/path>). See [title](https://evil.example/t \"tool)tip\")."
	want := "Read the setup guide (https://docs.example/setup) before clicking go (https://evil.example/path). See title (https://evil.example/t)."
	if got := hardenAgentMarkdown(in); got != want {
		t.Fatalf("hardened markdown = %q, want %q", got, want)
	}
}

func TestHardenAgentMarkdown_HardensNestedLinksInLabels(t *testing.T) {
	t.Parallel()
	in := "Use [outer [inner](https://evil.example) text](https://safe.example) and [outer ![shot](https://evil.example/i.png)](https://safe.example/img)."
	want := "Use outer inner (https://evil.example) text (https://safe.example) and outer shot (https://evil.example/i.png) (https://safe.example/img)."
	if got := hardenAgentMarkdown(in); got != want {
		t.Fatalf("hardened markdown = %q, want %q", got, want)
	}
}

func TestHardenAgentMarkdown_EscapesReferenceDefinitions(t *testing.T) {
	t.Parallel()
	in := "Use [the billing link][1].\n\n[1]: https://evil.example/login\nDone."
	want := "Use [the billing link][1].\n\n\\[1]: https://evil.example/login\nDone."
	if got := hardenAgentMarkdown(in); got != want {
		t.Fatalf("hardened markdown = %q, want %q", got, want)
	}
}

func TestHardenAgentMarkdown_EscapesMultilineReferenceDefinitions(t *testing.T) {
	t.Parallel()
	in := "Use [the billing link][click\nhere].\n\n[click\nhere]: https://evil.example/login\nDone."
	want := "Use [the billing link][click\nhere].\n\n\\[click\nhere]: https://evil.example/login\nDone."
	if got := hardenAgentMarkdown(in); got != want {
		t.Fatalf("hardened markdown = %q, want %q", got, want)
	}
}

func TestHardenAgentMarkdown_InlineLinkAtLineStartStillRevealsDestination(t *testing.T) {
	t.Parallel()
	in := "[Open billing](https://evil.example/login) now."
	want := "Open billing (https://evil.example/login) now."
	if got := hardenAgentMarkdown(in); got != want {
		t.Fatalf("hardened markdown = %q, want %q", got, want)
	}
}

func TestHardenAgentMarkdown_RevealsImageDestinations(t *testing.T) {
	t.Parallel()
	in := "Here is ![the screenshot](https://evil.example/screen.png)."
	want := "Here is the screenshot (https://evil.example/screen.png)."
	if got := hardenAgentMarkdown(in); got != want {
		t.Fatalf("hardened markdown = %q, want %q", got, want)
	}
}

func TestHardenAgentMarkdown_PreservesCodeSpans(t *testing.T) {
	t.Parallel()
	in := "Literal `[safe label](https://example.invalid)` but real [link](https://evil.example)."
	want := "Literal `[safe label](https://example.invalid)` but real link (https://evil.example)."
	if got := hardenAgentMarkdown(in); got != want {
		t.Fatalf("hardened markdown = %q, want %q", got, want)
	}
}

func TestHardenAgentMarkdown_UnclosedCodeSpanHardensFollowingLinks(t *testing.T) {
	t.Parallel()
	in := testUnclosedCodeMarkdownLink
	want := "` then click me (https://evil.example/phish)"
	if got := hardenAgentMarkdown(in); got != want {
		t.Fatalf("hardened markdown = %q, want %q", got, want)
	}
}

func TestHardenAgentMarkdown_EscapedBacktickDoesNotLeaveEscapeState(t *testing.T) {
	t.Parallel()
	in := `Literal \` + "`" + ` before [click me](https://evil.example/phish)`
	want := `Literal \` + "`" + ` before click me (https://evil.example/phish)`
	if got := hardenAgentMarkdown(in); got != want {
		t.Fatalf("hardened markdown = %q, want %q", got, want)
	}
}

func TestHardenAgentMarkdown_PreservesBangBeforeCodeSpan(t *testing.T) {
	t.Parallel()
	in := "Heads up!`code` done"
	if got := hardenAgentMarkdown(in); got != in {
		t.Fatalf("hardened markdown = %q, want %q", got, in)
	}
}

func TestHardenAgentMarkdown_PreservesBangBeforeUnclosedCodeSpan(t *testing.T) {
	t.Parallel()
	in := "Heads up!`code [click](https://evil.example/phish)"
	want := "Heads up!`code click (https://evil.example/phish)"
	if got := hardenAgentMarkdown(in); got != want {
		t.Fatalf("hardened markdown = %q, want %q", got, want)
	}
}

func TestHardenAgentMarkdown_PreservesEscapedBrackets(t *testing.T) {
	t.Parallel()
	in := testEscapedBracketRealLink
	want := `Literal \[safe label](https://example.invalid) but real link (https://evil.example).`
	if got := hardenAgentMarkdown(in); got != want {
		t.Fatalf("hardened markdown = %q, want %q", got, want)
	}
}

func TestHardenAgentMarkdown_EscapesRawHTMLTagStarts(t *testing.T) {
	t.Parallel()
	in := `Read <a href="https://evil.example/login">billing</a> and <img src="https://evil.example/pixel.png">.`
	want := `Read \<a href="https://evil.example/login">billing\</a> and \<img src="https://evil.example/pixel.png">.`
	if got := hardenAgentMarkdown(in); got != want {
		t.Fatalf("hardened markdown = %q, want %q", got, want)
	}
}

func TestHardenAgentMarkdown_EscapesSlackControlAngles(t *testing.T) {
	t.Parallel()
	in := `Notify <!channel>, <@U12345678>, and <#C12345678|ops>.`
	want := `Notify \<!channel>, \<@U12345678>, and \<#C12345678|ops>.`
	if got := hardenAgentMarkdown(in); got != want {
		t.Fatalf("hardened markdown = %q, want %q", got, want)
	}
}

func TestHardenAgentMarkdown_EscapesFutureSlackControlLetterPrefixes(t *testing.T) {
	t.Parallel()
	in := `Notify <@B12345678> and <#D12345678|direct>.`
	want := `Notify \<@B12345678> and \<#D12345678|direct>.`
	if got := hardenAgentMarkdown(in); got != want {
		t.Fatalf("hardened markdown = %q, want %q", got, want)
	}
}

func TestHardenAgentMarkdown_PreservesBenignAngleControlsLookalikes(t *testing.T) {
	t.Parallel()
	in := `Keep prose like 5 <# 7 and temp <@ home unchanged.`
	if got := hardenAgentMarkdown(in); got != in {
		t.Fatalf("hardened markdown = %q, want %q", got, in)
	}
}

func TestHardenAgentMarkdown_PreservesVisibleAutolinks(t *testing.T) {
	t.Parallel()
	in := `Use <https://docs.example/setup>, <MAILTO:security@example.com>, <user@example.com>, or <tel:+15551234567>.`
	if got := hardenAgentMarkdown(in); got != in {
		t.Fatalf("hardened markdown = %q, want %q", got, in)
	}
}

func TestHardenAgentMarkdown_RevealsSlackAngleLinks(t *testing.T) {
	t.Parallel()
	in := "Use <https://evil.example/login|billing portal> or <mailto:security@example.com|security>."
	want := "Use billing portal (https://evil.example/login) or security (mailto:security@example.com)."
	if got := hardenAgentMarkdown(in); got != want {
		t.Fatalf("hardened markdown = %q, want %q", got, want)
	}
}

func TestHardenAgentMarkdown_HardensNestedLinksInSlackAngleLabels(t *testing.T) {
	t.Parallel()
	in := "Use <https://safe.example|[billing](https://evil.example)> now."
	want := "Use billing (https://evil.example) (https://safe.example) now."
	if got := hardenAgentMarkdown(in); got != want {
		t.Fatalf("hardened markdown = %q, want %q", got, want)
	}
}

func TestHardenAgentMarkdown_HardensMaskedSyntaxInLinkDestinations(t *testing.T) {
	t.Parallel()
	nested := testNestedClickMarkdownLink
	got := hardenAgentMarkdown("Check [billing](" + testSafeURLPrefix + nested + ") now.")
	if containsUnescapedMarkdownToken(got, nested) {
		t.Fatalf("link destination should not expose raw nested masked syntax, got %q", got)
	}
	if !strings.Contains(got, testSafeURLPrefix+`\[click](https://evil.example/phish)`) {
		t.Fatalf("link destination should keep the visible destination with literal nested label, got %q", got)
	}
}

func TestHardenAgentMarkdown_HardensMaskedSyntaxInSlackAngleURLs(t *testing.T) {
	t.Parallel()
	nested := testNestedClickMarkdownLink
	got := hardenAgentMarkdown("Use <" + testSafeURLPrefix + nested + "|see details>.")
	if containsUnescapedMarkdownToken(got, nested) {
		t.Fatalf("angle URL should not expose raw nested masked syntax, got %q", got)
	}
	if !strings.Contains(got, testSafeURLPrefix+`\[click](https://evil.example/phish)`) {
		t.Fatalf("angle URL should keep the visible destination with literal nested label, got %q", got)
	}
}

func TestHardenAgentMarkdown_HardensNoPipeSlackAngleURLOriginals(t *testing.T) {
	t.Parallel()
	nested := testNestedClickMarkdownLink
	got := hardenAgentMarkdown("Use <" + testSafeURLPrefix + " " + nested + ">.")
	if containsUnescapedMarkdownToken(got, nested) {
		t.Fatalf("no-pipe angle original should not expose raw nested masked syntax, got %q", got)
	}
	if !strings.Contains(got, `\<`+testSafeURLPrefix+` \[click](https://evil.example/phish)>`) {
		t.Fatalf("no-pipe angle original should keep the destination literal, got %q", got)
	}
}

func TestHardenAgentMarkdown_IsIdempotent(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"Read [the setup guide](https://docs.example/setup).",
		"Use <https://safe.example/[click](https://evil.example/phish)|see details>.",
		"Use <https://safe.example/ [click](https://evil.example/phish)>.",
		testEscapedBracketRealLink,
		testUnclosedCodeMarkdownLink,
		"Email <user@example.com> and <a href=\"https://evil.example/login\">billing</a>.",
		"Notify <!channel>, <@U12345678>, and <#C12345678|ops>.",
		"[1]: https://evil.example/login",
	} {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			once := hardenAgentMarkdown(in)
			if twice := hardenAgentMarkdown(once); twice != once {
				t.Fatalf("hardenAgentMarkdown not idempotent:\ninput: %q\nonce: %q\ntwice: %q", in, once, twice)
			}
			assertNoVisibleMaskedLinkSyntax(t, once)
		})
	}
}

func FuzzHardenAgentMarkdown(f *testing.F) {
	for _, seed := range []string{
		"",
		"plain text",
		"Read [the setup guide](https://docs.example/setup).",
		"Use <https://safe.example/[click](https://evil.example/phish)|see details>.",
		"Use <https://safe.example/ [click](https://evil.example/phish)>.",
		testEscapedBracketRealLink,
		testUnclosedCodeMarkdownLink,
		"Email <user@example.com> and <a href=\"https://evil.example/login\">billing</a>.",
		"Notify <!channel>, <@U12345678>, and <#C12345678|ops>.",
		"[1]: https://evil.example/login",
		"Heads up!`code` done",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		once := hardenAgentMarkdown(in)
		if twice := hardenAgentMarkdown(once); twice != once {
			t.Fatalf("hardenAgentMarkdown not idempotent:\ninput: %q\nonce: %q\ntwice: %q", in, once, twice)
		}
		assertNoVisibleMaskedLinkSyntax(t, once)
	})
}

func TestAgentMarkdownLinkHarden_HandlesChunkSplitLinks(t *testing.T) {
	t.Parallel()
	var h agentMarkdownLinkHarden
	got := h.write("Use [Click") +
		h.write(" here](https://evil.example") +
		h.write("/login) now") +
		h.flush()
	want := "Use Click here (https://evil.example/login) now"
	if got != want {
		t.Fatalf("stream-hardened markdown = %q, want %q", got, want)
	}
}

func TestAgentMarkdownLinkHarden_HandlesChunkSplitReferenceDefinitions(t *testing.T) {
	t.Parallel()
	var h agentMarkdownLinkHarden
	got := h.write("Use [the billing link][1].\n\n[") +
		h.write("1]: https://evil.example/login") +
		h.write("\nDone.") +
		h.flush()
	want := "Use [the billing link][1].\n\n\\[1]: https://evil.example/login\nDone."
	if got != want {
		t.Fatalf("stream-hardened markdown = %q, want %q", got, want)
	}
}

func TestAgentMarkdownLinkHarden_EscapesReferenceDefinitionAtChunkBoundary(t *testing.T) {
	t.Parallel()
	var h agentMarkdownLinkHarden
	got := h.write("Use [the billing link][evil]. ") +
		h.write("[evil]: https://evil.example/login") +
		h.flush()
	want := "Use [the billing link][evil]. \\[evil]: https://evil.example/login"
	if got != want {
		t.Fatalf("stream-hardened markdown = %q, want %q", got, want)
	}
}

func TestAgentMarkdownLinkHarden_EscapesChunkSplitRawHTMLTagStarts(t *testing.T) {
	t.Parallel()
	var h agentMarkdownLinkHarden
	got := h.write("Read <") +
		h.write(`a href="https://evil.example/login">billing</a>`) +
		h.flush()
	want := `Read \<a href="https://evil.example/login">billing\</a>`
	if got != want {
		t.Fatalf("stream-hardened markdown = %q, want %q", got, want)
	}
}

func TestAgentMarkdownLinkHarden_HandlesSubSchemeChunkSplitAngleLinks(t *testing.T) {
	t.Parallel()
	var h agentMarkdownLinkHarden
	got := h.write("Use <htt") +
		h.write("ps://evil.example/login|billing> now") +
		h.flush()
	want := "Use billing (https://evil.example/login) now"
	if got != want {
		t.Fatalf("stream-hardened markdown = %q, want %q", got, want)
	}
}

func TestAgentMarkdownLinkHarden_HandlesChunkSplitEmailAutolinks(t *testing.T) {
	t.Parallel()
	var h agentMarkdownLinkHarden
	got := h.write("Email <u") +
		h.write("ser@example.com> now") +
		h.flush()
	want := "Email <user@example.com> now"
	if got != want {
		t.Fatalf("stream-hardened markdown = %q, want %q", got, want)
	}
}

func TestAgentMarkdownLinkHarden_HandlesChunkSplitSlackAngleLinks(t *testing.T) {
	t.Parallel()
	var h agentMarkdownLinkHarden
	got := h.write("Use <https://evil.example") +
		h.write("/login|billing portal> now") +
		h.flush()
	want := "Use billing portal (https://evil.example/login) now"
	if got != want {
		t.Fatalf("stream-hardened markdown = %q, want %q", got, want)
	}
}

func TestAgentMarkdownLinkHarden_HandlesChunkSplitSlackControls(t *testing.T) {
	t.Parallel()
	var h agentMarkdownLinkHarden
	got := h.write("Notify <") +
		h.write("@U12345678> and <!channel") +
		h.write("> now") +
		h.flush()
	want := `Notify \<@U12345678> and \<!channel> now`
	if got != want {
		t.Fatalf("stream-hardened markdown = %q, want %q", got, want)
	}
}

func TestAgentMarkdownLinkHarden_HandlesTwoByteChunkSplitSlackControls(t *testing.T) {
	t.Parallel()
	var h agentMarkdownLinkHarden
	got := h.write("Notify <@") +
		h.write("U12345678> and <#") +
		h.write("C12345678|ops> now") +
		h.flush()
	want := `Notify \<@U12345678> and \<#C12345678|ops> now`
	if got != want {
		t.Fatalf("stream-hardened markdown = %q, want %q", got, want)
	}
}

func TestAgentMarkdownLinkHarden_PreservesTwoByteChunkSplitSlackLookalikes(t *testing.T) {
	t.Parallel()
	var h agentMarkdownLinkHarden
	got := h.write("Keep temp <@") +
		h.write(" home and 5 <#") +
		h.write(" 7 unchanged") +
		h.flush()
	want := `Keep temp <@ home and 5 <# 7 unchanged`
	if got != want {
		t.Fatalf("stream-hardened markdown = %q, want %q", got, want)
	}
}

func TestAgentMarkdownLinkHarden_EscapesDeferredSlackControlPrefixesOnFlush(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"<@", "<#", "<!"} {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			var h agentMarkdownLinkHarden
			got := h.write("prefix "+in) + h.flush()
			want := `prefix \` + in
			if got != want {
				t.Fatalf("stream-hardened markdown = %q, want %q", got, want)
			}
		})
	}
}

func TestAgentMarkdownLinkHarden_EscapesUnclosedSlackAngleLinks(t *testing.T) {
	t.Parallel()
	var h agentMarkdownLinkHarden
	got := h.write("Use <https://evil.example/login|billing portal") + h.flush()
	want := `Use \<https://evil.example/login|billing portal`
	if got != want {
		t.Fatalf("stream-hardened markdown = %q, want %q", got, want)
	}
}

func TestAgentMarkdownLinkHarden_HardensNestedLinksInUnclosedSlackAngleLinks(t *testing.T) {
	t.Parallel()
	nested := testNestedBillingMarkdownLink
	var h agentMarkdownLinkHarden
	got := h.write("Use <https://safe.example|"+nested) + h.flush()
	if strings.Contains(got, nested) {
		t.Fatalf("unclosed angle link should harden nested masked links, got %q", got)
	}
	if !strings.Contains(got, "billing (https://evil.example/login)") {
		t.Fatalf("unclosed angle link should expose nested destination, got %q", got)
	}
}

func TestAgentMarkdownLinkHarden_HardensMaskedSyntaxInUnclosedSlackAngleURLs(t *testing.T) {
	t.Parallel()
	nested := testNestedClickMarkdownLink
	var h agentMarkdownLinkHarden
	got := h.write("Use <"+testSafeURLPrefix+nested+"|see details") + h.flush()
	if containsUnescapedMarkdownToken(got, nested) {
		t.Fatalf("unclosed angle URL should not expose raw nested masked syntax, got %q", got)
	}
	if !strings.Contains(got, testSafeURLPrefix+`\[click](https://evil.example/phish)`) {
		t.Fatalf("unclosed angle URL should keep the visible destination with literal nested label, got %q", got)
	}
}

func TestAgentMarkdownLinkHarden_HandlesChunkSplitMultilineReferenceDefinitions(t *testing.T) {
	t.Parallel()
	var h agentMarkdownLinkHarden
	got := h.write("Use [the billing link][click\nhere].\n\n[click") +
		h.write("\nhere]: https://evil.example/login") +
		h.write("\nDone.") +
		h.flush()
	want := "Use [the billing link][click\nhere].\n\n\\[click\nhere]: https://evil.example/login\nDone."
	if got != want {
		t.Fatalf("stream-hardened markdown = %q, want %q", got, want)
	}
}

func TestAgentMarkdownLinkHarden_EscapesOversizedBufferedLinks(t *testing.T) {
	t.Parallel()
	label := strings.Repeat("a", maxAgentMarkdownLinkBytes+1)
	got := hardenAgentMarkdown("[" + label + "](https://evil.example/login)")
	if !strings.HasPrefix(got, `\[`) {
		t.Fatalf("oversized link should be escaped, got prefix %q", got[:2])
	}
	if !strings.Contains(got, "https://evil.example/login") {
		t.Fatalf("oversized link should still expose destination, got %q", got)
	}
}

func TestAgentMarkdownLinkHarden_HardensNestedLinksInOversizedBufferedLinks(t *testing.T) {
	t.Parallel()
	nested := testNestedBillingMarkdownLink
	label := strings.Repeat("a", maxAgentMarkdownLinkBytes/2) + nested + strings.Repeat("b", maxAgentMarkdownLinkBytes)
	got := hardenAgentMarkdown("[" + label + "](https://safe.example)")
	if strings.Contains(got, nested) {
		t.Fatalf("oversized link should harden nested masked links, got %q", got)
	}
	if !strings.Contains(got, "billing (https://evil.example/login)") {
		t.Fatalf("oversized link should expose nested destination, got %q", got)
	}
	if !strings.Contains(got, "https://safe.example") {
		t.Fatalf("oversized link should preserve trailing destination text, got %q", got)
	}
}

func TestAgentMarkdownLinkHarden_HardensNestedLinksInUnclosedBufferedLinks(t *testing.T) {
	t.Parallel()
	nested := testNestedBillingMarkdownLink
	got := hardenAgentMarkdown("[outer " + nested + " text")
	if strings.Contains(got, nested) {
		t.Fatalf("unclosed buffered link should harden nested masked links, got %q", got)
	}
	if !strings.Contains(got, "billing (https://evil.example/login)") {
		t.Fatalf("unclosed buffered link should expose nested destination, got %q", got)
	}
}

func TestAgentMarkdownLinkHarden_HardensNestedLinksInClosedNonLinkLabels(t *testing.T) {
	t.Parallel()
	nested := testNestedBillingMarkdownLink
	got := hardenAgentMarkdown("Use [outer " + nested + "] text")
	if strings.Contains(got, nested) {
		t.Fatalf("non-link label should harden nested masked links, got %q", got)
	}
	if !strings.Contains(got, "billing (https://evil.example/login)") {
		t.Fatalf("non-link label should expose nested destination, got %q", got)
	}
}

func TestAgentMarkdownLinkHarden_EscapesOversizedReferenceDefinitions(t *testing.T) {
	t.Parallel()
	ref := strings.Repeat("a", maxAgentMarkdownLinkBytes+1)
	got := hardenAgentMarkdown("[" + ref + "]: https://evil.example/login")
	if !strings.HasPrefix(got, `\[`) {
		t.Fatalf("oversized reference definition should be escaped, got prefix %q", got[:2])
	}
	if !strings.Contains(got, "https://evil.example/login") {
		t.Fatalf("oversized reference definition should still expose destination, got %q", got)
	}
}

func TestAgentMarkdownLinkHarden_HardensNestedLinksInOversizedReferenceDefinitions(t *testing.T) {
	t.Parallel()
	nested := testNestedBillingMarkdownLink
	ref := strings.Repeat("a", maxAgentMarkdownLinkBytes/2) + nested + strings.Repeat("b", maxAgentMarkdownLinkBytes)
	got := hardenAgentMarkdown("[" + ref + "]: https://safe.example")
	if strings.Contains(got, nested) {
		t.Fatalf("oversized reference definition should harden nested masked links, got %q", got)
	}
	if !strings.Contains(got, "billing (https://evil.example/login)") {
		t.Fatalf("oversized reference definition should expose nested destination, got %q", got)
	}
	if !strings.Contains(got, "https://safe.example") {
		t.Fatalf("oversized reference definition should preserve destination text, got %q", got)
	}
}

func TestAgentMarkdownLinkHarden_HardensNestedLinksInOversizedAngleLinks(t *testing.T) {
	t.Parallel()
	nested := testNestedBillingMarkdownLink
	label := strings.Repeat("a", maxAgentMarkdownLinkBytes/2) + nested + strings.Repeat("b", maxAgentMarkdownLinkBytes)
	got := hardenAgentMarkdown("<https://safe.example|" + label)
	if strings.Contains(got, nested) {
		t.Fatalf("oversized angle link should harden nested masked links, got %q", got)
	}
	if !strings.Contains(got, "billing (https://evil.example/login)") {
		t.Fatalf("oversized angle link should expose nested destination, got %q", got)
	}
}

func TestAgentMarkdownLinkHarden_HardensMaskedSyntaxInOversizedAngleURLs(t *testing.T) {
	t.Parallel()
	nested := testNestedClickMarkdownLink
	label := strings.Repeat("a", maxAgentMarkdownLinkBytes)
	got := hardenAgentMarkdown("<" + testSafeURLPrefix + nested + "|" + label)
	if containsUnescapedMarkdownToken(got, nested) {
		t.Fatalf("oversized angle URL should not expose raw nested masked syntax, got %q", got)
	}
	if !strings.Contains(got, testSafeURLPrefix+`\[click](https://evil.example/phish)`) {
		t.Fatalf("oversized angle URL should keep the visible destination with literal nested label, got %q", got)
	}
}

func TestAgentMarkdownLinkHarden_HandlesChunkSplitCodeFenceTicks(t *testing.T) {
	t.Parallel()
	var h agentMarkdownLinkHarden
	got := h.write("```\n[not a link](https://example.invalid)\n``") +
		h.write("`\nThen [go](https://evil.example).") +
		h.flush()
	want := "```\n[not a link](https://example.invalid)\n```\nThen go (https://evil.example)."
	if got != want {
		t.Fatalf("stream-hardened markdown = %q, want %q", got, want)
	}
}

func TestAgentMarkdownLinkHarden_IncompleteLinkFlushesOriginal(t *testing.T) {
	t.Parallel()
	var h agentMarkdownLinkHarden
	got := h.write("This is [not a link]") + h.flush()
	if got != "This is [not a link]" {
		t.Fatalf("stream-hardened markdown = %q", got)
	}
}

func containsUnescapedMarkdownToken(s, token string) bool {
	for offset := 0; ; {
		idx := strings.Index(s[offset:], token)
		if idx < 0 {
			return false
		}
		idx += offset
		if !markdownByteEscaped(s, idx) {
			return true
		}
		offset = idx + len(token)
	}
}

func assertNoVisibleMaskedLinkSyntax(t *testing.T, markdown string) {
	t.Helper()
	if containsVisibleMaskedLinkSyntax(markdown) {
		t.Fatalf("hardened markdown still contains visible masked-link syntax: %q", markdown)
	}
}

func containsVisibleMaskedLinkSyntax(markdown string) bool {
	var inCode bool
	var codeTicks int
	for i := 0; i < len(markdown); i++ {
		if markdownByteEscaped(markdown, i) {
			continue
		}
		if markdown[i] == '`' {
			ticks := markdownBacktickRunLen(markdown[i:])
			if inCode && ticks == codeTicks {
				inCode = false
				codeTicks = 0
			} else if !inCode {
				inCode = true
				codeTicks = ticks
			}
			i += ticks - 1
			continue
		}
		if inCode {
			continue
		}
		if markdown[i] == '[' && visibleInlineLinkSyntaxStarts(markdown[i:]) {
			return true
		}
		if markdown[i] == '<' && visibleSlackAngleMaskSyntaxStarts(markdown[i:]) {
			return true
		}
	}
	return false
}

func markdownBacktickRunLen(s string) int {
	var n int
	for n < len(s) && s[n] == '`' {
		n++
	}
	return n
}

func visibleInlineLinkSyntaxStarts(s string) bool {
	for i := 1; i+1 < len(s); i++ {
		if markdownByteEscaped(s, i) {
			continue
		}
		if s[i] == '\n' {
			return false
		}
		if s[i] == ']' {
			return s[i+1] == '('
		}
	}
	return false
}

func visibleSlackAngleMaskSyntaxStarts(s string) bool {
	if len(s) < 2 || !hasVisibleAutolinkScheme(s[1:]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if markdownByteEscaped(s, i) {
			continue
		}
		switch s[i] {
		case '>':
			return false
		case '|':
			return true
		case ' ', '\t', '\n':
			return false
		}
	}
	return false
}

func markdownByteEscaped(s string, idx int) bool {
	var slashes int
	for i := idx - 1; i >= 0 && s[i] == '\\'; i-- {
		slashes++
	}
	return slashes%2 == 1
}
