package internal

import (
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/shared/client"
)

// Block Kit type/key literals, hoisted so the assertions don't trip goconst.
const (
	blockTypeSection              = "section"
	blockTypeRichText             = "rich_text"
	blockTypeRichTextPreformatted = "rich_text_preformatted"
	blockKeyType                  = "type"
	blockKeyText                  = "text"
	blockKeyElements              = "elements"
)

// renderInstallForEnv renders a full install message for one environment via
// the same helper the production path drives, so installMessageBlocks is
// exercised against real installer output rather than a hand-built fixture.
func renderInstallForEnv(t *testing.T, env tunnelInstallEnvironment) string {
	t.Helper()
	now := fixedNow
	expiresAt := now.Add(time.Hour)
	h := NewHandler(Config{TunnelImage: testTunnelImageRef})
	freezeTunnelBootstrapNow(t, h, now)
	args := testPinnedTunnelInstallArgs()
	args.Environment = env
	msg, err := h.renderTunnelInstallMessage(args, &client.APIKey{APIKey: testTunnelAPIKey, ExpiresAt: &expiresAt}, "qURL alias `$prod-dashboard` is ready in this channel.")
	if err != nil {
		t.Fatalf("renderTunnelInstallMessage(%v): %v", env, err)
	}
	return msg
}

// richTextCodeTexts extracts the code string from every
// rich_text_preformatted block, asserting the nested shape matches what
// richTextPreformattedBlock produces.
func richTextCodeTexts(t *testing.T, blocks []any) []string {
	t.Helper()
	var out []string
	for _, b := range blocks {
		bm, ok := b.(map[string]any)
		if !ok || bm[blockKeyType] != blockTypeRichText {
			continue
		}
		elems, ok := bm[blockKeyElements].([]any)
		if !ok || len(elems) != 1 {
			t.Fatalf("rich_text block has unexpected elements: %#v", bm)
		}
		pre, ok := elems[0].(map[string]any)
		if !ok || pre[blockKeyType] != blockTypeRichTextPreformatted {
			t.Fatalf("rich_text element is not preformatted: %#v", elems[0])
		}
		inner, ok := pre[blockKeyElements].([]any)
		if !ok || len(inner) != 1 {
			t.Fatalf("preformatted has unexpected elements: %#v", pre)
		}
		txt, ok := inner[0].(map[string]any)
		if !ok || txt[blockKeyType] != blockKeyText {
			t.Fatalf("preformatted inner element is not text: %#v", inner[0])
		}
		s, _ := txt[blockKeyText].(string)
		out = append(out, s)
	}
	return out
}

// sectionTexts extracts the mrkdwn body from every section block.
func sectionTexts(t *testing.T, blocks []any) []string {
	t.Helper()
	var out []string
	for _, b := range blocks {
		bm, ok := b.(map[string]any)
		if !ok || bm[blockKeyType] != blockTypeSection {
			continue
		}
		txtObj, ok := bm[blockKeyText].(map[string]any)
		if !ok {
			t.Fatalf("section block text is not a composition object: %#v", bm)
		}
		s, _ := txtObj[blockKeyText].(string)
		out = append(out, s)
	}
	return out
}

// TestInstallMessageBlocks_AllEnvironments fences that every real install
// message renders as Block Kit (no plain-text fallback), never carries the
// plaintext bootstrap key, and keeps every code segment within the per-block
// cap. This doubles as the size measurement: an oversize real snippet would
// flip ok to false and fail here, surfacing the need to bump
// slackRichTextMaxBytes.
func TestInstallMessageBlocks_AllEnvironments(t *testing.T) {
	for _, env := range []tunnelInstallEnvironment{
		tunnelEnvDocker,
		tunnelEnvCompose,
		tunnelEnvECSFargate,
		tunnelEnvKubernetes,
	} {
		t.Run(string(env), func(t *testing.T) {
			msg := renderInstallForEnv(t, env)
			blocks, ok := installMessageBlocks(msg)
			if !ok {
				t.Fatalf("installMessageBlocks ok=false for a real %s install (largest segment over cap %d?); msg len=%d", env, slackRichTextMaxBytes, len(msg))
			}

			codes := richTextCodeTexts(t, blocks)
			sections := sectionTexts(t, blocks)
			if len(codes) == 0 || len(sections) == 0 {
				t.Fatalf("%s: want both section and rich_text blocks, got sections=%d code=%d", env, len(sections), len(codes))
			}

			for _, c := range codes {
				if strings.Contains(c, testTunnelAPIKey) {
					t.Errorf("%s: bootstrap key leaked into an install code block", env)
				}
				if len(c) > slackRichTextMaxBytes {
					t.Errorf("%s: code block exceeds cap (%d > %d)", env, len(c), slackRichTextMaxBytes)
				}
			}
			for _, s := range sections {
				if strings.Contains(s, testTunnelAPIKey) {
					t.Errorf("%s: bootstrap key leaked into a prose section block", env)
				}
			}
		})
	}
}

func TestBootstrapSecretMessageBlocks_CarriesKeyInCodeOnly(t *testing.T) {
	now := fixedNow
	expiresAt := now.Add(time.Hour)
	msg, err := renderTunnelBootstrapSecretMessage(&tunnelInstallArgs{
		Slug:        testTunnelSlug,
		Alias:       testTunnelSlug,
		LocalPort:   defaultTunnelLocalPort,
		Environment: tunnelEnvDocker,
	}, &client.APIKey{APIKey: testTunnelAPIKey, ExpiresAt: &expiresAt}, now)
	if err != nil {
		t.Fatalf("renderTunnelBootstrapSecretMessage: %v", err)
	}
	blocks, ok := installMessageBlocks(msg)
	if !ok {
		t.Fatalf("installMessageBlocks ok=false for bootstrap secret DM; msg len=%d", len(msg))
	}
	codes := richTextCodeTexts(t, blocks)
	sections := sectionTexts(t, blocks)
	if len(codes) != 1 || !strings.Contains(codes[0], testTunnelAPIKey) {
		t.Fatalf("bootstrap secret code blocks = %#v, want one containing key", codes)
	}
	for _, s := range sections {
		if strings.Contains(s, testTunnelAPIKey) {
			t.Fatalf("bootstrap key leaked into DM prose section: %q", s)
		}
	}
}

// TestInstallMessageBlocks_PreservesOrderAndProse fences the segmentation:
// prose and code alternate in source order, prose becomes sections, fences
// become rich_text, and the captured code is verbatim (fence markers stripped).
func TestInstallMessageBlocks_PreservesOrderAndProse(t *testing.T) {
	msg := "Step one.\n\n```\ncode-A\n```\n\nStep two.\n\n```\ncode-B\n```"
	blocks, ok := installMessageBlocks(msg)
	if !ok {
		t.Fatal("ok=false for a well-formed prose/code message")
	}
	wantTypes := []string{blockTypeSection, blockTypeRichText, blockTypeSection, blockTypeRichText}
	if len(blocks) != len(wantTypes) {
		t.Fatalf("block count = %d, want %d: %#v", len(blocks), len(wantTypes), blocks)
	}
	for i, want := range wantTypes {
		if got := blocks[i].(map[string]any)[blockKeyType]; got != want {
			t.Errorf("block[%d] type = %v, want %s", i, got, want)
		}
	}
	if codes := richTextCodeTexts(t, blocks); len(codes) != 2 || codes[0] != "code-A" || codes[1] != "code-B" {
		t.Errorf("codes = %#v, want [code-A code-B]", codes)
	}
}

// TestInstallMessageBlocks_FallbackPaths fences every (nil,false) signal that
// routes the caller to the always-safe plain-text post: no code fence to
// enrich, an empty fence, an oversize prose run, an oversize code segment, and
// a block count over the per-message cap.
func TestInstallMessageBlocks_FallbackPaths(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		{"no fence", "just prose, no code fence at all"},
		{"empty fence", "intro\n\n```\n\n```\n\nfooter"},
		{
			"oversize prose",
			strings.Repeat("p", slackSectionTextMaxBytes+1) + "\n\n```\ncode\n```",
		},
		{
			"oversize code",
			"intro\n\n```\n" + strings.Repeat("x", slackRichTextMaxBytes+1) + "\n```\n\nfooter",
		},
		{
			"too many blocks",
			strings.Repeat("```\nx\n```\n\n", slackMessageBlockMax+1),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := installMessageBlocks(tc.msg); ok {
				t.Errorf("want ok=false for %s (should drop to plain-text post)", tc.name)
			}
		})
	}
}
