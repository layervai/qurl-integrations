package internal

import (
	"errors"
	"strings"
	"testing"
)

// TestParse_HappyPaths fences the recognized grammar of every subcommand.
// One row per verb so a regression that drops or relabels a verb is the
// failure that reaches review, not a behavioral diff in PR-3c.3+.
func TestParse_HappyPaths(t *testing.T) {
	t.Parallel()
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
		{name: "setalias with quoted target strips outer quotes", text: `setalias $prod-db "https://internal.example.com"`, wantSub: SubcmdSetAlias, wantAlias: "prod-db", wantTarget: "https://internal.example.com", wantFlags: map[string]string{}},
		{name: "create with quoted target strips outer quotes", text: `create "https://x.example/with space"`, wantSub: SubcmdCreate, wantTarget: "https://x.example/with space", wantFlags: map[string]string{}},
		// Unbalanced quotes: tokenize tolerates (does not reject)
		// odd-count `"` runs. The opening quote stays literal in
		// Target and downstream URL validation surfaces the error.
		// Pinned here so a future refactor of tokenize can't
		// silently change the tolerance contract.
		{name: "setalias with unbalanced opening quote tolerated", text: `setalias $prod-db "https://x.example`, wantSub: SubcmdSetAlias, wantAlias: "prod-db", wantTarget: `"https://x.example`, wantFlags: map[string]string{}},
		{name: "uppercase flag key normalized", text: "get $prod-db DM:true", wantSub: SubcmdGet, wantAlias: "prod-db", wantFlags: map[string]string{"dm": "true"}},
		{name: "mixed-case flag key normalized, value preserved", text: `get $prod-db Reason:"On Call"`, wantSub: SubcmdGet, wantAlias: "prod-db", wantFlags: map[string]string{"reason": "On Call"}},
		{name: "single-char alias accepted", text: "get $a", wantSub: SubcmdGet, wantAlias: "a", wantFlags: map[string]string{}},
		{name: "single-digit alias accepted", text: "get $1", wantSub: SubcmdGet, wantAlias: "1", wantFlags: map[string]string{}},
		// Internal `--` runs are intentionally accepted — qurl-service
		// is the authoritative validator and allows them; the parser
		// only enforces leading/trailing-hyphen rejection.
		{name: "alias with internal double hyphen accepted", text: "get $foo--bar", wantSub: SubcmdGet, wantAlias: "foo--bar", wantFlags: map[string]string{}},
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
	t.Parallel()
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
		{name: "admin no verb", text: "admin", wantErr: ErrMissingAdminAction},
		{name: "admin unknown verb", text: "admin frobnicate", wantErr: ErrUnknownAdminAction},
		{name: "admin allow without channel", text: "admin allow $prod-db", wantErr: ErrMissingChannel},
		{name: "admin allow without alias", text: "admin allow <#C123|ops>", wantErr: ErrEmptyResource},
		{name: "admin allow garbage positional", text: "admin allow notachannel notalias", wantErr: ErrMissingSigil},
		{name: "admin revoke missing alias", text: "admin revoke", wantErr: ErrEmptyResource},
		{name: "create without target", text: "create", wantErr: ErrMissingTarget},
		{name: "alias with uppercase rejected", text: "get $ProdDB", wantErr: ErrInvalidAlias},
		{name: "alias with leading hyphen rejected", text: "get $-foo", wantErr: ErrInvalidAlias},
		{name: "alias with space (quoted) rejected", text: `get "$prod db"`, wantErr: ErrInvalidAlias},
		{name: "alias with equals rejected", text: "setalias $a=b https://x.example", wantErr: ErrInvalidAlias},
		{name: "admin policies with extra arg rejected", text: "admin policies extra-junk", wantErr: ErrUnexpectedArgument},
		{name: "admin status with extra arg rejected", text: "admin status oops", wantErr: ErrUnexpectedArgument},
		{name: "admin claim with positional rejected", text: "admin claim boot-code", wantErr: ErrUnexpectedArgument},
		{name: "admin revoke with extra trailing arg rejected", text: "admin revoke $alias extra", wantErr: ErrUnexpectedArgument},
		{name: "admin allow with duplicate channel rejected", text: "admin allow <#C1|a> <#C2|b> $alias", wantErr: ErrUnexpectedArgument},
		{name: "admin allow with duplicate alias rejected", text: "admin allow <#C1|a> $foo $bar", wantErr: ErrUnexpectedArgument},
		{name: "alias with trailing hyphen rejected", text: "get $prod-", wantErr: ErrInvalidAlias},
		{name: "alias single hyphen rejected", text: "get $-", wantErr: ErrInvalidAlias},
		{name: "alias with double trailing hyphens rejected", text: "get $foo--", wantErr: ErrInvalidAlias},
		// Strict-posture: once channel + alias slots are both taken,
		// any further positional is an ErrUnexpectedArgument (matches
		// the posture parseAdmin takes for verbs like `admin policies`).
		// Previously this fell into the missing-sigil branch with a
		// misleading "alias must start with $" message.
		{name: "admin allow with extra arg after both slots filled", text: "admin allow <#C1|a> $alias garbage", wantErr: ErrUnexpectedArgument},
		{name: "admin disallow with extra arg after both slots filled", text: "admin disallow <#C1|a> $alias garbage", wantErr: ErrUnexpectedArgument},
		// Strict-posture on `setalias`: a stray flag-shaped token
		// after the target must reject, not silently get glued
		// into Target via space-join. Quoted multi-word targets
		// survive as a single token through [tokenize].
		{name: "setalias with extra trailing token rejected", text: "setalias $prod-db https://x.example dm:true", wantErr: ErrUnexpectedArgument},
		{name: "setalias with extra trailing positional rejected", text: "setalias $prod-db https://x.example extra-garbage", wantErr: ErrUnexpectedArgument},
		// Empty-quoted target would otherwise tokenize to a present-
		// but-empty Target string and bypass ErrMissingTarget.
		// tokenize's post-strip empty-token drop ensures the verb
		// hits the missing-target branch — matches the strict posture.
		{name: "setalias with empty quoted target rejected", text: `setalias $prod-db ""`, wantErr: ErrMissingTarget},
		// Strict-posture on no-arg verbs `aliases` and `list`: extra
		// positionals reject just like `admin policies extra` does.
		// Carve-out is `help` only (friendly default).
		{name: "aliases with extra positional rejected", text: "aliases junk", wantErr: ErrUnexpectedArgument},
		{name: "list with extra positional rejected", text: "list extra-garbage", wantErr: ErrUnexpectedArgument},
		// Non-flag-shaped trailing token on `get`: surface as
		// ErrUnexpectedArgument rather than the misleading
		// "invalid flag" message applyFlag would otherwise return.
		{name: "get with non-flag trailing positional rejected", text: "get $prod-db junk", wantErr: ErrUnexpectedArgument},
		// A typo-class URL paste (`get $alias https://x.example:8080`)
		// contains `:` but is plainly not a flag; the
		// http://-and-https://-aware looksLikeFlag check routes it to
		// ErrUnexpectedArgument rather than the misleading
		// `unknown flag: "https"` applyFlag would otherwise produce.
		{name: "get with URL-shaped trailing positional rejected", text: "get $prod-db https://x.example:8080", wantErr: ErrUnexpectedArgument},
		{name: "get with http URL-shaped trailing positional rejected", text: "get $prod-db http://x.example:8080", wantErr: ErrUnexpectedArgument},
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
// no-op into ephemeral-channel post. Empty values (`key:` or `key:""`)
// reject so the handler in PR-3c.3+ can rely on absence-in-map to
// mean "flag unset" (no third "set-to-empty" state to distinguish).
// `wantSubstr` fences the user-visible message so both empty-value
// shapes (`key:` and `key:""`) report a consistent "empty value"
// reason instead of one of them landing in the generic
// "expected key:value" bucket.
func TestParse_GetFlagErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		text       string
		wantSubstr string
	}{
		{name: "unknown flag key", text: "get $prod-db whatever:true", wantSubstr: "unknown flag"},
		// A trailing token with no colon hits the parseGet
		// strict-posture check before applyFlag; it surfaces as
		// "unexpected argument" rather than "expected key:value"
		// because the user almost certainly didn't intend a flag.
		{name: "malformed flag (no colon) surfaces as unexpected arg", text: "get $prod-db reasontruly", wantSubstr: "unexpected argument"},
		{name: "empty bare value", text: "get $prod-db reason:", wantSubstr: "empty value"},
		{name: "empty quoted value", text: `get $prod-db reason:""`, wantSubstr: "empty value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(tc.text)
			if err == nil {
				t.Fatalf("Parse(%q) error = nil, want non-nil", tc.text)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("Parse(%q) error = %q, want substring %q", tc.text, err.Error(), tc.wantSubstr)
			}
		})
	}
}

// TestTokenize_UnbalancedQuoteBleedsAcrossBoundary pins the
// multi-token unbalanced-quote tolerance contract. With an opening
// quote that never closes, every subsequent space-separated token
// should fold into the same in-quotes run rather than producing
// separate tokens. This is the parity-toggle semantic documented in
// [tokenize]; a future refactor that switched to a balanced-pair
// matcher would silently change this behavior, so the test pins it.
//
// The example mirrors the doc comment's "stray-quote inputs vanishingly
// rare in practice" claim: `get $alias "reason without closing` would
// otherwise tokenize to `[get, $alias, "reason, without, closing]`
// (five tokens) and the user would see a confusing
// `unexpected argument: "without"` instead of the bleed-into-one-token
// the doc promises.
func TestTokenize_UnbalancedQuoteBleedsAcrossBoundary(t *testing.T) {
	t.Parallel()
	got := tokenize(`get $alias "reason without closing`)
	want := []string{"get", "$alias", `"reason without closing`}
	if len(got) != len(want) {
		t.Fatalf("tokenize: got %d tokens (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tokenize[%d] = %q, want %q", i, got[i], want[i])
		}
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

// FuzzParse is a panic-on-input regression fence. The grammar is
// small enough that any input either yields a non-nil [Command] with
// nil error, or an error that wraps one of the documented sentinels.
// A future refactor of [tokenize] or [Parse] that panics on
// pathological input — multi-byte runes mid-quote, deep nesting of
// `<#…|…>`, surrogate pairs in flag values — gets caught here.
//
// Seeds cover the happy-path shape (verb + alias), a flag pair, an
// admin two-positional form, a quoted target, the unbalanced-quote
// tolerance path, and a few adversarial bytes (NUL, CR, multi-byte).
// The body never asserts on `cmd` shape — only that the
// `(*Command, error)` contract holds: exactly one of the two is nil.
func FuzzParse(f *testing.F) {
	seeds := []string{
		"",
		"help",
		"get $prod-db",
		"get $prod-db dm:true reason:\"on call\"",
		"setalias $alias https://x.example",
		"setalias $alias \"https://x.example with space\"",
		"setalias $alias \"unbalanced",
		"unsetalias $alias",
		"admin allow <#C12345|ops> $alias",
		"admin disallow $alias <#C99999|qa>",
		"admin claim",
		"admin revoke $alias",
		"create https://example.com",
		"list",
		"aliases",
		"\x00",
		"get $\x00",
		"get $alias \r reason:foo",
		"setalias $alias 世界",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	knownSentinels := []error{
		ErrEmptyResource,
		ErrMissingSigil,
		ErrUnknownSubcommand,
		ErrUnknownAdminAction,
		ErrMissingAdminAction,
		ErrMissingChannel,
		ErrMissingTarget,
		ErrInvalidAlias,
		ErrUnexpectedArgument,
	}
	f.Fuzz(func(t *testing.T, in string) {
		cmd, err := Parse(in)
		if (cmd == nil) == (err == nil) {
			t.Fatalf("Parse(%q): exactly one of (cmd, err) must be non-nil; got cmd=%v err=%v", in, cmd, err)
		}
		if err != nil {
			// Every parse error must wrap one of the documented
			// sentinels — that's the contract the handler in
			// PR-3c.3+ depends on to render the right user-facing
			// `:warning:` message. An applyFlag-shaped error
			// (`unknown flag: …`, `invalid flag: …`) is also
			// acceptable since the parser surfaces those directly
			// from applyFlag, and they're carried by the
			// fmt.Errorf wrapping — accept those too via substring
			// match on the well-known prefixes.
			matched := false
			for _, sentinel := range knownSentinels {
				if errors.Is(err, sentinel) {
					matched = true
					break
				}
			}
			msg := err.Error()
			if !matched && !strings.Contains(msg, "unknown flag") && !strings.Contains(msg, "invalid flag") {
				t.Fatalf("Parse(%q) = unrecognized error %v (must wrap a documented sentinel)", in, err)
			}
		}
	})
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
