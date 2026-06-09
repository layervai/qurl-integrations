package agent

import (
	"strings"
	"testing"
)

func TestSystemPrompt_Invariants(t *testing.T) {
	p := systemPrompt(&TurnContext{ChannelID: "C1", ChannelName: testChannel, UserID: "U1", CallerIsAdmin: true})

	// Brand + terminology rules from CLAUDE.md.
	if !strings.Contains(p, "qURL") {
		t.Error("prompt must use the qURL brand")
	}
	if !strings.Contains(p, "Secure Access Agent") {
		t.Error("prompt must identify as the Secure Access Agent")
	}
	// "firewall" may appear EXACTLY ONCE, and only inside the negative
	// instruction — never as a description of the product. The exact phrase
	// "never call qURL a firewall" is load-bearing for this guard; rewording it
	// will (correctly) trip this test.
	if lower := strings.ToLower(p); strings.Contains(lower, "firewall") {
		if strings.Count(lower, "firewall") != 1 {
			t.Error("prompt mentions 'firewall' more than once; it must appear only in the negative instruction")
		}
		if !strings.Contains(lower, "never call qurl a firewall") {
			t.Error("prompt must not describe qURL as a firewall")
		}
	}

	// Core safety invariant must be stated.
	for _, want := range []string{"confirm", "never", "admin"} {
		if !strings.Contains(strings.ToLower(p), want) {
			t.Errorf("prompt missing safety language %q", want)
		}
	}

	// Hardening invariants: tool output is untrusted (prompt-injection via
	// alias/description), and the agent must not invent resources on a zero match.
	for _, want := range []string{"untrusted", "invent"} {
		if !strings.Contains(strings.ToLower(p), want) {
			t.Errorf("prompt missing hardening language %q", want)
		}
	}

	// Per-turn context is injected.
	if !strings.Contains(p, testChannel) || !strings.Contains(p, "U1") {
		t.Error("prompt must inject the channel and user context")
	}
}

func TestSystemPrompt_AdminVsNonAdmin(t *testing.T) {
	admin := systemPrompt(&TurnContext{ChannelID: "C1", UserID: "U1", CallerIsAdmin: true})
	if !strings.Contains(admin, "is a workspace admin") {
		t.Error("admin prompt should state the caller is an admin")
	}
	nonAdmin := systemPrompt(&TurnContext{ChannelID: "C1", UserID: "U1", CallerIsAdmin: false})
	if !strings.Contains(nonAdmin, "NOT a workspace admin") {
		t.Error("non-admin prompt should state the caller is not an admin")
	}
}

func TestDescribeChannel(t *testing.T) {
	cases := []struct {
		tc   TurnContext
		want string
	}{
		{TurnContext{ChannelName: testChannel, ChannelID: "C1"}, "#" + testChannel + " (C1)"},
		{TurnContext{ChannelID: "C1"}, "C1"},
		{TurnContext{}, unknownLabel},
	}
	for _, c := range cases {
		tc := c.tc
		if got := describeChannel(&tc); got != c.want {
			t.Errorf("describeChannel(%+v) = %q, want %q", c.tc, got, c.want)
		}
	}
}
