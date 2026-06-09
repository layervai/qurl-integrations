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
	if strings.Contains(strings.ToLower(p), "firewall") {
		// We may say "never call qURL a firewall"; assert the word only appears
		// in that negative instruction, never as a description of the product.
		if !strings.Contains(p, "never call qURL a firewall") {
			t.Error("prompt must not describe qURL as a firewall")
		}
	}

	// Core safety invariant must be stated.
	for _, want := range []string{"confirm", "never", "admin"} {
		if !strings.Contains(strings.ToLower(p), want) {
			t.Errorf("prompt missing safety language %q", want)
		}
	}

	// Channel-scope disclosure: the agent must be told to surface that its reads
	// are channel-scoped rather than implying a workspace-wide answer.
	if !strings.Contains(p, "only see what's reachable in THIS channel") {
		t.Error("prompt must instruct the agent to disclose its channel-scoped read boundary")
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
