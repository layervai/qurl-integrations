package internal

import "testing"

func TestHardenAgentMarkdown_RevealsMaskedLinks(t *testing.T) {
	t.Parallel()
	in := "Read [the setup guide](https://docs.example/setup) before clicking [go](<https://evil.example/path>)."
	want := "Read the setup guide (https://docs.example/setup) before clicking go (https://evil.example/path)."
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

func TestAgentMarkdownLinkHarden_IncompleteLinkFlushesOriginal(t *testing.T) {
	t.Parallel()
	var h agentMarkdownLinkHarden
	got := h.write("This is [not a link]") + h.flush()
	if got != "This is [not a link]" {
		t.Fatalf("stream-hardened markdown = %q", got)
	}
}
