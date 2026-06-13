package internal

import (
	"strings"
	"testing"
)

func TestHardenAgentMarkdown_RevealsMaskedLinks(t *testing.T) {
	t.Parallel()
	in := "Read [the setup guide](https://docs.example/setup) before clicking [go](<https://evil.example/path>). See [title](https://evil.example/t \"tool)tip\")."
	want := "Read the setup guide (https://docs.example/setup) before clicking go (https://evil.example/path). See title (https://evil.example/t)."
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

func TestHardenAgentMarkdown_PreservesEscapedBrackets(t *testing.T) {
	t.Parallel()
	in := `Literal \[safe label](https://example.invalid) but real [link](https://evil.example).`
	want := `Literal \[safe label](https://example.invalid) but real link (https://evil.example).`
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
