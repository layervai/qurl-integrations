package internal

import (
	"strings"
	"testing"
)

const testNestedBillingMarkdownLink = "[billing](https://evil.example/login)"

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
	in := "` then [click me](https://evil.example/phish)"
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

func TestHardenAgentMarkdown_PreservesEscapedBrackets(t *testing.T) {
	t.Parallel()
	in := `Literal \[safe label](https://example.invalid) but real [link](https://evil.example).`
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
