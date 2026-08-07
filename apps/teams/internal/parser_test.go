package internal

import "testing"

const testScopeLabelChannel = "channel"

func TestNormalizeActivityTextReplacesMentionsAndStripsBotMention(t *testing.T) {
	a := &Activity{
		Text:      `<at>qurl</at> get $docs dm:true reason:"prod access" <div>ignored</div>`,
		Recipient: ChannelAccount{ID: "bot-1"},
		Entities: []Entity{
			{Type: "mention", Text: "<at>qurl</at>", Mentioned: ChannelAccount{ID: "bot-1"}},
		},
	}
	if got := normalizeActivityText(a); got != `get $docs dm:true reason:"prod access" ignored` {
		t.Fatalf("normalizeActivityText() = %q", got)
	}
}

func TestNormalizeActivityTextReplacesUserMentionWithToken(t *testing.T) {
	a := &Activity{
		Text:      `<at>Alice</at>`,
		Recipient: ChannelAccount{ID: "bot-1"},
		Entities: []Entity{
			{Type: "mention", Text: "<at>Alice</at>", Mentioned: ChannelAccount{ID: "user-1"}},
		},
	}
	if got := normalizeActivityText(a); got != "<@user-1>" {
		t.Fatalf("normalizeActivityText() = %q", got)
	}
}

func TestParseCommandSetupRotate(t *testing.T) {
	cmd, err := ParseCommand(`qurl setup user@example.com --rotate`)
	if err != nil {
		t.Fatalf("ParseCommand() error = %v", err)
	}
	if cmd.Verb != verbSetup || cmd.Email != "user@example.com" || cmd.SetupMode != SetupModeRotate {
		t.Fatalf("unexpected command: %+v", cmd)
	}
}

func TestParseCommandGetFlags(t *testing.T) {
	cmd, err := ParseCommand(`get $docs dm:true reason:"incident 123"`)
	if err != nil {
		t.Fatalf("ParseCommand() error = %v", err)
	}
	if cmd.Resource != "docs" {
		t.Fatalf("Resource = %q, want docs", cmd.Resource)
	}
	if cmd.Flags["dm"] != "true" || cmd.Flags["reason"] != "incident 123" {
		t.Fatalf("Flags = %+v", cmd.Flags)
	}
}

func TestParseCommandMembershipMention(t *testing.T) {
	cmd, err := ParseCommand(`add <@00000000-0000-0000-0000-000000000000>`)
	if err != nil {
		t.Fatalf("ParseCommand() error = %v", err)
	}
	if cmd.UserID != "00000000-0000-0000-0000-000000000000" {
		t.Fatalf("UserID = %q", cmd.UserID)
	}
}

func TestParseCommandSetAliasAndDisplayName(t *testing.T) {
	cmd, err := ParseCommand(`set-alias $docs $resource-1`)
	if err != nil {
		t.Fatalf("ParseCommand() error = %v", err)
	}
	if cmd.Alias != "docs" || cmd.Target != "resource-1" {
		t.Fatalf("unexpected set-alias command: %+v", cmd)
	}

	cmd, err = ParseCommand(`set-display-name $resource-1 Friendly name`)
	if err != nil {
		t.Fatalf("ParseCommand() error = %v", err)
	}
	if cmd.Resource != "resource-1" || cmd.Text != "Friendly name" {
		t.Fatalf("unexpected set-display-name command: %+v", cmd)
	}
}

func TestParseCommandSingleResourceVariants(t *testing.T) {
	for _, raw := range []string{`revoke $resource-1`, `unset-display-name $resource-1`, `unset-alias $docs`} {
		cmd, err := ParseCommand(raw)
		if err != nil {
			t.Fatalf("ParseCommand(%q) error = %v", raw, err)
		}
		if cmd.Resource == "" {
			t.Fatalf("ParseCommand(%q) produced empty resource: %+v", raw, cmd)
		}
	}
}

func TestParseCommandRejectsInvalidAlias(t *testing.T) {
	if _, err := ParseCommand(`set-alias $BadAlias $resource-1`); err == nil {
		t.Fatal("expected invalid alias error")
	}
}

func TestDeriveScope(t *testing.T) {
	personal := deriveScope(&Activity{
		Conversation: ConversationAccount{ID: "conv-1", ConversationType: "personal", TenantID: "tenant-1"},
	})
	if !personal.Personal || personal.ScopeLabel != "personal chat" || personal.TenantID != "tenant-1" {
		t.Fatalf("unexpected personal scope: %+v", personal)
	}

	channel := deriveScope(&Activity{
		Conversation: ConversationAccount{ID: "conv-2", ConversationType: testScopeLabelChannel},
		ChannelData: ChannelData{
			Tenant: struct {
				ID string `json:"id,omitempty"`
			}{ID: "tenant-2"},
			Channel: struct {
				ID string `json:"id,omitempty"`
			}{ID: "channel-2"},
		},
	})
	if !channel.Channel || channel.ScopeID != "channel-2" || channel.TenantID != "tenant-2" || channel.ScopeLabel != testScopeLabelChannel {
		t.Fatalf("unexpected channel scope: %+v", channel)
	}

	group := deriveScope(&Activity{
		Conversation: ConversationAccount{ID: "conv-3", ConversationType: "groupchat"},
	})
	if !group.GroupChat || group.ScopeLabel != "group chat" || group.Conversation != "conv-3" {
		t.Fatalf("unexpected group scope: %+v", group)
	}
}
