package internal

import (
	"errors"
	"testing"
)

// TestParse_HappyPaths fences the recognized grammar of every subcommand.
// One row per verb so a regression that drops or relabels a verb is the
// failure that reaches review, not a behavioral diff in PR-3c.3+.
func TestParse_HappyPaths(t *testing.T) {
	cases := []struct {
		name        string
		text        string
		wantSub     Subcommand
		wantAdmin   AdminAction
		wantAlias   string
		wantTarget  string
		wantChannel string
		wantFlags   map[string]string
	}{
		{name: "empty -> help", text: "", wantSub: SubcmdHelp, wantFlags: map[string]string{}},
		{name: "help literal", text: "help", wantSub: SubcmdHelp, wantFlags: map[string]string{}},
		{name: "get alias", text: "get $prod-db", wantSub: SubcmdGet, wantAlias: "prod-db", wantFlags: map[string]string{}},
		{name: "get with dm flag", text: "get $prod-db dm:true", wantSub: SubcmdGet, wantAlias: "prod-db", wantFlags: map[string]string{"dm": "true"}},
		{name: "get with reason flag", text: `get $prod-db reason:"on call"`, wantSub: SubcmdGet, wantAlias: "prod-db", wantFlags: map[string]string{"reason": "on call"}},
		{name: "get with both flags", text: `get $prod-db dm:true reason:"audit"`, wantSub: SubcmdGet, wantAlias: "prod-db", wantFlags: map[string]string{"dm": "true", "reason": "audit"}},
		{name: "setalias url", text: "setalias $prod-db https://internal.example.com", wantSub: SubcmdSetAlias, wantAlias: "prod-db", wantTarget: "https://internal.example.com", wantFlags: map[string]string{}},
		{name: "setalias resource_id", text: "setalias $prod-db r_abc123", wantSub: SubcmdSetAlias, wantAlias: "prod-db", wantTarget: "r_abc123", wantFlags: map[string]string{}},
		{name: "unsetalias", text: "unsetalias $prod-db", wantSub: SubcmdUnsetAlias, wantAlias: "prod-db", wantFlags: map[string]string{}},
		{name: "aliases", text: "aliases", wantSub: SubcmdAliases, wantFlags: map[string]string{}},
		{name: "admin claim no args", text: "admin claim", wantSub: SubcmdAdmin, wantAdmin: AdminClaim, wantFlags: map[string]string{}},
		{name: "admin allow channel + alias", text: "admin allow <#C12345|ops> $prod-db", wantSub: SubcmdAdmin, wantAdmin: AdminAllow, wantAlias: "prod-db", wantChannel: "C12345", wantFlags: map[string]string{}},
		{name: "admin allow alias + channel (reversed)", text: "admin allow $prod-db <#C12345|ops>", wantSub: SubcmdAdmin, wantAdmin: AdminAllow, wantAlias: "prod-db", wantChannel: "C12345", wantFlags: map[string]string{}},
		{name: "admin disallow", text: "admin disallow <#C99999|qa> $stage-db", wantSub: SubcmdAdmin, wantAdmin: AdminDisallow, wantAlias: "stage-db", wantChannel: "C99999", wantFlags: map[string]string{}},
		{name: "admin policies", text: "admin policies", wantSub: SubcmdAdmin, wantAdmin: AdminPolicies, wantFlags: map[string]string{}},
		{name: "admin status", text: "admin status", wantSub: SubcmdAdmin, wantAdmin: AdminStatus, wantFlags: map[string]string{}},
		{name: "admin revoke alias", text: "admin revoke $prod-db", wantSub: SubcmdAdmin, wantAdmin: AdminRevoke, wantAlias: "prod-db", wantFlags: map[string]string{}},
		{name: "create url legacy", text: "create https://example.com", wantSub: SubcmdCreate, wantTarget: "https://example.com", wantFlags: map[string]string{}},
		{name: "list", text: "list", wantSub: SubcmdList, wantFlags: map[string]string{}},
		{name: "channel ref without name", text: "admin allow <#C00001> $alias-name", wantSub: SubcmdAdmin, wantAdmin: AdminAllow, wantAlias: "alias-name", wantChannel: "C00001", wantFlags: map[string]string{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd, err := Parse(tc.text)
			if err != nil {
				t.Fatalf("Parse(%q) error = %v", tc.text, err)
			}
			if cmd.Subcommand != tc.wantSub {
				t.Errorf("Subcommand = %q, want %q", cmd.Subcommand, tc.wantSub)
			}
			if cmd.AdminAction != tc.wantAdmin {
				t.Errorf("AdminAction = %q, want %q", cmd.AdminAction, tc.wantAdmin)
			}
			if cmd.Alias != tc.wantAlias {
				t.Errorf("Alias = %q, want %q", cmd.Alias, tc.wantAlias)
			}
			if cmd.Target != tc.wantTarget {
				t.Errorf("Target = %q, want %q", cmd.Target, tc.wantTarget)
			}
			if cmd.ChannelID != tc.wantChannel {
				t.Errorf("ChannelID = %q, want %q", cmd.ChannelID, tc.wantChannel)
			}
			if len(cmd.Flags) != len(tc.wantFlags) {
				t.Errorf("Flags = %v, want %v", cmd.Flags, tc.wantFlags)
			}
			for k, v := range tc.wantFlags {
				if cmd.Flags[k] != v {
					t.Errorf("Flags[%q] = %q, want %q", k, cmd.Flags[k], v)
				}
			}
		})
	}
}

// TestParse_ErrorPaths fences the friendly-error grammar — every row is
// a malformed input the parser must reject, with a stable sentinel error
// so the handler can render the right `:warning:` message in PR-3c.3+.
func TestParse_ErrorPaths(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		wantErr error
	}{
		{name: "unknown subcommand", text: "delete $foo", wantErr: ErrUnknownSubcommand},
		{name: "get without alias", text: "get", wantErr: ErrEmptyResource},
		{name: "get without sigil", text: "get prod-db", wantErr: ErrMissingSigil},
		{name: "get bare sigil", text: "get $", wantErr: ErrEmptyResource},
		{name: "setalias without alias", text: "setalias", wantErr: ErrEmptyResource},
		{name: "setalias without sigil", text: "setalias prod-db https://x.example", wantErr: ErrMissingSigil},
		{name: "setalias without target", text: "setalias $prod-db", wantErr: ErrMissingTarget},
		{name: "unsetalias without alias", text: "unsetalias", wantErr: ErrEmptyResource},
		{name: "unsetalias without sigil", text: "unsetalias prod-db", wantErr: ErrMissingSigil},
		{name: "admin no verb", text: "admin", wantErr: ErrUnknownAdminAction},
		{name: "admin unknown verb", text: "admin frobnicate", wantErr: ErrUnknownAdminAction},
		{name: "admin allow without channel", text: "admin allow $prod-db", wantErr: ErrMissingChannel},
		{name: "admin allow without alias", text: "admin allow <#C123|ops>", wantErr: ErrEmptyResource},
		{name: "admin allow garbage positional", text: "admin allow notachannel notalias", wantErr: ErrMissingSigil},
		{name: "admin revoke missing alias", text: "admin revoke", wantErr: ErrEmptyResource},
		{name: "create without target", text: "create", wantErr: ErrMissingTarget},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(tc.text)
			if err == nil {
				t.Fatalf("Parse(%q) error = nil, want %v", tc.text, tc.wantErr)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Parse(%q) error = %v, want %v", tc.text, err, tc.wantErr)
			}
		})
	}
}

// TestParse_GetFlagErrors covers the `get`-flag mini-grammar on its own.
// Unknown flags must reject so a typo (e.g. `dn:true`) doesn't silently
// no-op into ephemeral-channel post.
func TestParse_GetFlagErrors(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{name: "unknown flag key", text: "get $prod-db whatever:true"},
		{name: "malformed flag (no colon)", text: "get $prod-db reasontruly"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(tc.text)
			if err == nil {
				t.Fatalf("Parse(%q) error = nil, want non-nil", tc.text)
			}
		})
	}
}

// TestCommand_DM exercises the DM helper with all the shapes the parser
// can produce, plus the nil-receiver path.
func TestCommand_DM(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cmd  *Command
		want bool
	}{
		{name: "nil receiver", cmd: nil, want: false},
		{name: "no flags", cmd: &Command{Flags: map[string]string{}}, want: false},
		{name: "dm:true", cmd: &Command{Flags: map[string]string{"dm": "true"}}, want: true},
		{name: "dm:TRUE (case-insensitive)", cmd: &Command{Flags: map[string]string{"dm": "TRUE"}}, want: true},
		{name: "dm:false", cmd: &Command{Flags: map[string]string{"dm": "false"}}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.cmd.DM(); got != tc.want {
				t.Errorf("DM() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCommand_Reason fences the reason flag accessor including the
// nil-receiver case (callers may legitimately invoke this on a Command
// returned alongside an error).
func TestCommand_Reason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cmd  *Command
		want string
	}{
		{name: "nil", cmd: nil, want: ""},
		{name: "unset", cmd: &Command{Flags: map[string]string{}}, want: ""},
		{name: "set", cmd: &Command{Flags: map[string]string{"reason": "on call"}}, want: "on call"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.cmd.Reason(); got != tc.want {
				t.Errorf("Reason() = %q, want %q", got, tc.want)
			}
		})
	}
}
