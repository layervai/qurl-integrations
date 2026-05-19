package internal

import (
	"errors"
	"strings"
	"testing"
)

// dmRejectSubstr is the load-bearing substring in `applyFlag`'s
// non-boolean-dm rejection message. Pinned as a const so the three
// rejection-row test cases share one source of truth — if the user-
// facing wording changes, this is the single edit point.
const dmRejectSubstr = "use dm:true"

// TestParse_HappyPaths fences the recognized grammar of every subcommand.
// One row per verb so a regression that drops or relabels a verb is the
// failure that reaches review, not a behavioral diff in PR-3c.3+.
func TestParse_HappyPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		text       string
		wantSub    Subcommand
		wantAdmin  AdminAction
		wantAlias  string
		wantTarget string
		wantUserID string
		wantFlags  map[string]string
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
		{name: "admin revoke qurl_id", text: "admin revoke q_01HXYZ8ABCDEF0123456789AB", wantSub: SubcmdAdmin, wantAdmin: AdminRevoke, wantTarget: "q_01HXYZ8ABCDEF0123456789AB", wantFlags: map[string]string{}},
		{name: "admin add mention", text: "admin add <@U12345>", wantSub: SubcmdAdmin, wantAdmin: AdminAdd, wantUserID: "U12345", wantFlags: map[string]string{}},
		{name: "admin add mention with display name", text: "admin add <@U12345|kevin>", wantSub: SubcmdAdmin, wantAdmin: AdminAdd, wantUserID: "U12345", wantFlags: map[string]string{}},
		{name: "admin remove mention", text: "admin remove <@U67890>", wantSub: SubcmdAdmin, wantAdmin: AdminRemove, wantUserID: "U67890", wantFlags: map[string]string{}},
		{name: testAdminListCmd, text: testAdminListCmd, wantSub: SubcmdAdmin, wantAdmin: AdminList, wantFlags: map[string]string{}},
		{name: "list", text: "list", wantSub: SubcmdList, wantFlags: map[string]string{}},
		{name: "setalias with quoted target strips outer quotes", text: `setalias $prod-db "https://internal.example.com"`, wantSub: SubcmdSetAlias, wantAlias: "prod-db", wantTarget: "https://internal.example.com", wantFlags: map[string]string{}},
		{name: "get url form", text: "get https://example.com", wantSub: SubcmdGet, wantTarget: "https://example.com", wantFlags: map[string]string{}},
		{name: "get url form with reason", text: `get https://example.com reason:"on-call"`, wantSub: SubcmdGet, wantTarget: "https://example.com", wantFlags: map[string]string{"reason": "on-call"}},
		// Unbalanced quotes: tokenize tolerates (does not reject)
		// odd-count `"` runs. The opening quote stays literal in
		// Target and downstream URL validation surfaces the error.
		// Pinned here so a future refactor of tokenize can't
		// silently change the tolerance contract.
		{name: "setalias with unbalanced opening quote tolerated", text: `setalias $prod-db "https://x.example`, wantSub: SubcmdSetAlias, wantAlias: "prod-db", wantTarget: `"https://x.example`, wantFlags: map[string]string{}},
		{name: "uppercase flag key normalized", text: "get $prod-db DM:true", wantSub: SubcmdGet, wantAlias: "prod-db", wantFlags: map[string]string{"dm": "true"}},
		{name: "mixed-case flag key normalized, value preserved", text: `get $prod-db Reason:"On Call"`, wantSub: SubcmdGet, wantAlias: "prod-db", wantFlags: map[string]string{"reason": "On Call"}},
		// dm:false is explicitly accepted (the strict-boolean gate
		// allows both `true` and `false`). Command.DM() returns
		// false for any non-"true" value, so dm:false has the same
		// runtime effect as omitting the flag — but accepting it
		// here lets users opt out explicitly without seeing an
		// "unknown flag" error.
		{name: "dm:false accepted", text: "get $prod-db dm:false", wantSub: SubcmdGet, wantAlias: "prod-db", wantFlags: map[string]string{"dm": "false"}},
		{name: "dm:FALSE case-folded", text: "get $prod-db dm:FALSE", wantSub: SubcmdGet, wantAlias: "prod-db", wantFlags: map[string]string{"dm": "FALSE"}},
		// Admin verb is lowercased before the AdminAction switch. Pinned
		// so a future refactor that drops `strings.ToLower(verb)` can't
		// silently regress mobile-client / auto-capitalize inputs.
		{name: "uppercase admin verb normalized", text: "admin CLAIM", wantSub: SubcmdAdmin, wantAdmin: AdminClaim, wantFlags: map[string]string{}},
		{name: "mixed-case admin verb normalized", text: "admin List", wantSub: SubcmdAdmin, wantAdmin: AdminList, wantFlags: map[string]string{}},
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
			if cmd.UserID != tc.wantUserID {
				t.Errorf("UserID = %q, want %q", cmd.UserID, tc.wantUserID)
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
		{name: "admin revoke missing qurl_id", text: "admin revoke", wantErr: ErrMissingTarget},
		{name: "admin revoke with $alias rejected", text: "admin revoke $prod-db", wantErr: ErrUnexpectedArgument},
		{name: "admin revoke with malformed id rejected", text: "admin revoke ;rm-rf", wantErr: ErrInvalidQURLID},
		{name: "admin revoke with non-q-prefix id rejected", text: "admin revoke r_resource_id", wantErr: ErrInvalidQURLID},
		{name: "admin revoke with oversize id rejected", text: "admin revoke q_" + strings.Repeat("A", 100), wantErr: ErrInvalidQURLID},
		{name: "admin add without mention", text: "admin add", wantErr: ErrMissingUserMention},
		{name: "admin add with bare @user", text: "admin add @alice", wantErr: ErrInvalidUserMention},
		{name: "admin add with non-mention positional", text: "admin add alice", wantErr: ErrInvalidUserMention},
		{name: "admin add with lowercase user-id", text: "admin add <@u12345>", wantErr: ErrInvalidUserMention},
		{name: "admin add with extra arg rejected", text: "admin add <@U12345> extra", wantErr: ErrUnexpectedArgument},
		{name: "admin remove without mention", text: "admin remove", wantErr: ErrMissingUserMention},
		{name: "admin remove with non-mention positional", text: "admin remove alice", wantErr: ErrInvalidUserMention},
		{name: "admin remove with extra arg rejected", text: "admin remove <@U12345> extra", wantErr: ErrUnexpectedArgument},
		{name: "admin list with extra arg rejected", text: "admin list extra", wantErr: ErrUnexpectedArgument},
		{name: "alias with uppercase rejected", text: "get $ProdDB", wantErr: ErrInvalidAlias},
		{name: "alias with leading hyphen rejected", text: "get $-foo", wantErr: ErrInvalidAlias},
		{name: "alias with space (quoted) rejected", text: `get "$prod db"`, wantErr: ErrInvalidAlias},
		{name: "alias with equals rejected", text: "setalias $a=b https://x.example", wantErr: ErrInvalidAlias},
		{name: "admin claim with positional rejected", text: "admin claim boot-code", wantErr: ErrUnexpectedArgument},
		{name: "admin revoke with extra trailing arg rejected", text: "admin revoke q_01HXYZ8ABCDEF0123456789AB extra", wantErr: ErrUnexpectedArgument},
		{name: "alias with trailing hyphen rejected", text: "get $prod-", wantErr: ErrInvalidAlias},
		{name: "alias single hyphen rejected", text: "get $-", wantErr: ErrInvalidAlias},
		{name: "alias with double trailing hyphens rejected", text: "get $foo--", wantErr: ErrInvalidAlias},
		// Length cap: parseAliasToken mirrors handler_alias.go's
		// aliasMaxLen=64. A 65-char alias rejects with a length-specific
		// message (substring-checked below); the 64-char boundary is
		// covered by the happy-path TestParse_AliasLengthBoundary.
		{name: "alias over 64 chars rejected", text: "get $" + strings.Repeat("a", 65), wantErr: ErrInvalidAlias},
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
		// Resource-ID-shape fences on the `get` verb. `requireResourceToken`
		// tries [resourceIDPattern] first, then [aliasCharsetPattern];
		// shapes that match neither must reject with ErrInvalidAlias
		// (the joint sentinel) — exact behavior matters because the
		// fallback shape would otherwise be the only path that minted
		// a malformed token.
		{name: "resource-id too short rejected", text: "get $r_short", wantErr: ErrInvalidAlias},
		{name: "resource-id uppercase rejected", text: "get $r_TOOMANYCHARS", wantErr: ErrInvalidAlias},
		// Exact-boundary fences for [resourceIDPattern]'s `{11}` cap:
		// 10 chars (one under) and 12 chars (one over). The pattern is
		// the joint shape `^r_[a-z0-9_-]{11}$` — a single char delta on
		// either side flips the match. Without these, a future regex
		// loosening to `{10,12}` would silently mint malformed tokens
		// that qurl-service then 404s on.
		{name: "resource-id exactly 10 chars rejected", text: "get $r_aaaaaaaaaa", wantErr: ErrInvalidAlias},
		{name: "resource-id exactly 12 chars rejected", text: "get $r_aaaaaaaaaaaa", wantErr: ErrInvalidAlias},
		// Invalid byte ($) anywhere in the body — neither resource-ID
		// nor alias shape accepts $ in the tail.
		{name: "resource-id invalid byte rejected", text: "get $r_aaa$bbbccc", wantErr: ErrInvalidAlias},
		// A typo-class URL paste (`get $alias https://x.example:8080`)
		// contains `:` but is plainly not a flag; the
		// http://-and-https://-aware looksLikeFlag check routes it to
		// ErrUnexpectedArgument rather than the misleading
		// `unknown flag: "https"` applyFlag would otherwise produce.
		{name: "get with URL-shaped trailing positional rejected", text: "get $prod-db https://x.example:8080", wantErr: ErrUnexpectedArgument},
		{name: "get with http URL-shaped trailing positional rejected", text: "get $prod-db http://x.example:8080", wantErr: ErrUnexpectedArgument},
		// Case-insensitive scheme match: `HTTPS://` paste from a
		// clipboard that uppercased the scheme should still route
		// through the URL-typo carve-out (ErrUnexpectedArgument)
		// rather than applyFlag's misleading `unknown flag: "HTTPS"`.
		{name: "get with HTTPS-uppercase URL-shaped trailing positional rejected", text: "get $prod-db HTTPS://x.example:8080", wantErr: ErrUnexpectedArgument},
		{name: "get with HTTP-uppercase URL-shaped trailing positional rejected", text: "get $prod-db HTTP://x.example:8080", wantErr: ErrUnexpectedArgument},
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
//
// Two checks per row: the sentinel ([ErrInvalidFlag] for applyFlag-side
// rejections, [ErrUnexpectedArgument] for the parseGet strict-posture
// branch) AND a user-visible substring. The sentinel pins the contract
// PR-3c.3+ handler dispatch will rely on via `errors.Is`; the substring
// fence keeps the user-visible reason consistent (e.g. both empty-value
// shapes report "empty value" instead of one of them landing in the
// generic "expected key:value" bucket).
func TestParse_GetFlagErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		text       string
		wantSentnl error
		wantSubstr string
	}{
		{name: "unknown flag key", text: "get $prod-db whatever:true", wantSentnl: ErrInvalidFlag, wantSubstr: "unknown flag"},
		// A trailing token with no colon hits the parseGet
		// strict-posture check before applyFlag; it surfaces as
		// "unexpected argument" rather than "expected key:value"
		// because the user almost certainly didn't intend a flag.
		{name: "malformed flag (no colon) surfaces as unexpected arg", text: "get $prod-db reasontruly", wantSentnl: ErrUnexpectedArgument, wantSubstr: "unexpected argument"},
		{name: "empty bare value", text: "get $prod-db reason:", wantSentnl: ErrInvalidFlag, wantSubstr: "empty value"},
		{name: "empty quoted value", text: `get $prod-db reason:""`, wantSentnl: ErrInvalidFlag, wantSubstr: "empty value"},
		// Strict-posture on dm: only `true` / `false` (case-folded)
		// accepted. Without these rejections, `dm:yes` / `dm:1`
		// would parse fine and then silently return false on
		// Command.DM() — the same silent-no-op failure mode the
		// parser rejects for typo'd flag keys. Pin each truthy-
		// looking-but-rejected value individually so a future
		// loosening (e.g., accepting `1`/`yes`/`on`) requires
		// touching this test deliberately.
		{name: "dm:1 rejected (not true/false)", text: "get $prod-db dm:1", wantSentnl: ErrInvalidFlag, wantSubstr: dmRejectSubstr},
		{name: "dm:yes rejected (not true/false)", text: "get $prod-db dm:yes", wantSentnl: ErrInvalidFlag, wantSubstr: dmRejectSubstr},
		{name: "dm:please rejected (not true/false)", text: "get $prod-db dm:please", wantSentnl: ErrInvalidFlag, wantSubstr: dmRejectSubstr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(tc.text)
			if err == nil {
				t.Fatalf("Parse(%q) error = nil, want non-nil", tc.text)
			}
			if !errors.Is(err, tc.wantSentnl) {
				t.Errorf("Parse(%q) error = %v, want errors.Is(_, %v)", tc.text, err, tc.wantSentnl)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("Parse(%q) error = %q, want substring %q", tc.text, err.Error(), tc.wantSubstr)
			}
		})
	}
}

// TestParse_QURLIDLengthBoundary pins the {16,64} boundary on
// qurlIDPattern. 16 and 64 accept; 15 and 65 reject with
// ErrInvalidQURLID. The floor is the realistic shortest qurl-service
// emits (ULID suffixes are 26 chars; 16 leaves a margin) — a 15-char
// `q_abc` token is almost always a truncation paste, so failing at
// parse time gives the user a hint instead of an opaque 404.
func TestParse_QURLIDLengthBoundary(t *testing.T) {
	t.Parallel()
	at16 := strings.Repeat("A", 16)
	at15 := strings.Repeat("A", 15)
	at64 := strings.Repeat("A", 64)
	at65 := strings.Repeat("A", 65)

	for _, ok := range []string{at16, at64} {
		cmd, err := Parse("admin revoke q_" + ok)
		if err != nil {
			t.Errorf("Parse(admin revoke q_<%d chars>): unexpected error %v", len(ok), err)
			continue
		}
		if cmd.Target != "q_"+ok {
			t.Errorf("Target = %q, want %q", cmd.Target, "q_"+ok)
		}
	}

	for _, bad := range []string{at15, at65} {
		_, err := Parse("admin revoke q_" + bad)
		if err == nil {
			t.Errorf("Parse(admin revoke q_<%d chars>) returned nil error, want ErrInvalidQURLID", len(bad))
			continue
		}
		if !errors.Is(err, ErrInvalidQURLID) {
			t.Errorf("Parse(admin revoke q_<%d chars>) error = %v, want errors.Is(_, ErrInvalidQURLID)", len(bad), err)
		}
	}
}

// TestParse_AliasLengthBoundary pins the off-by-one boundary on the
// shared 64-char alias cap. 64 chars must accept (qurl-service's
// nhp #1825 GSI key is exactly 64); 65 must reject with the
// length-specific message so the user sees "$… is longer than 64
// characters" rather than the generic invalid-alias message — that
// wording mirrors handler_alias.go's `setalias` path so the two
// entry points produce parallel copy.
func TestParse_AliasLengthBoundary(t *testing.T) {
	t.Parallel()
	at64 := strings.Repeat("a", 64)
	at65 := strings.Repeat("a", 65)

	cmd, err := Parse("get $" + at64)
	if err != nil {
		t.Fatalf("Parse(get $<64 chars>): unexpected error %v", err)
	}
	if cmd.Alias != at64 {
		t.Errorf("Alias = %q (len=%d), want %q (len=64)", cmd.Alias, len(cmd.Alias), at64)
	}

	_, err = Parse("get $" + at65)
	if err == nil {
		t.Fatal("Parse(get $<65 chars>) returned nil error, want ErrInvalidAlias")
	}
	if !errors.Is(err, ErrInvalidAlias) {
		t.Errorf("Parse(get $<65 chars>) error = %v, want errors.Is(_, ErrInvalidAlias)", err)
	}
	if !strings.Contains(err.Error(), "longer than 64 characters") {
		t.Errorf("Parse(get $<65 chars>) error = %q, want substring %q", err.Error(), "longer than 64 characters")
	}
}

// TestRequireResourceToken_PositiveShapes exercises the kind-tagged
// return shape of [requireResourceToken] directly so a regex change
// surfaces at the unit level rather than only through end-to-end
// fixtures. The Parse-level tests cover the integration shape; this
// pins the parser's structural contract — alias kind, resource-id
// kind, and what each Value is — without the parseGet flag-handling
// overhead.
func TestRequireResourceToken_PositiveShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		input     string
		wantKind  ResourceTokenKind
		wantValue string
	}{
		{name: "alias shape", input: "$prod-db", wantKind: ResourceTokenAlias, wantValue: "prod-db"},
		{name: "single-char alias", input: "$a", wantKind: ResourceTokenAlias, wantValue: "a"},
		{name: "resource-id shape", input: "$r_abc123def01", wantKind: ResourceTokenResourceID, wantValue: "r_abc123def01"},
		// All-zero body and mixed-charset body — both still match
		// [resourceIDPattern]'s `^r_[a-z0-9_-]{11}$` so the kind tag
		// must come back as ResourceTokenResourceID. Pinned so a
		// regex tightening (e.g., to `[a-z0-9]` only) shows up as a
		// kind-flip rather than a silent fall-through to the alias
		// shape.
		{name: "resource-id all digits body", input: "$r_00000000000", wantKind: ResourceTokenResourceID, wantValue: "r_00000000000"},
		{name: "resource-id mixed body with dashes", input: "$r_a-b-c-d-e-f", wantKind: ResourceTokenResourceID, wantValue: "r_a-b-c-d-e-f"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tok, err := requireResourceToken(tc.input)
			if err != nil {
				t.Fatalf("requireResourceToken(%q) error = %v", tc.input, err)
			}
			if tok.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", tok.Kind, tc.wantKind)
			}
			if tok.Value != tc.wantValue {
				t.Errorf("Value = %q, want %q", tok.Value, tc.wantValue)
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

// TestApplyFlag_MissingKeyBeforeColon pins the colon-at-position-0
// branch of [applyFlag]. With the round-9 [looksLikeFlag] gate in
// parseGet, a `:value`-shaped token now surfaces as
// [ErrUnexpectedArgument] from the dispatcher (looksLikeFlag
// rejects `colonIdx <= 0`), but applyFlag still carries the
// "missing key before colon" branch as defense-in-depth for any
// future caller that hands it a `:value` token directly. Pin the
// branch so the contract — "applyFlag rejects empty-key flags with
// a specific user-facing reason" — doesn't silently regress.
func TestApplyFlag_MissingKeyBeforeColon(t *testing.T) {
	t.Parallel()
	cmd := &Command{Flags: map[string]string{}}
	err := applyFlag(cmd, ":true")
	if err == nil {
		t.Fatal("applyFlag(\":true\") returned nil, want error")
	}
	if !strings.Contains(err.Error(), "missing key before colon") {
		t.Errorf("applyFlag(\":true\") error = %q, want substring %q", err.Error(), "missing key before colon")
	}
}

// TestLooksLikeFlag_EmptyString pins the empty-input path so a
// refactor of [looksLikeFlag] (e.g., adding an early `len(tok) > N`
// shortcut) can't silently break the empty case. `strings.IndexByte("",
// ':')` returns -1, which the `colonIdx <= 0` guard turns into false
// — but the contract is "empty input is not a flag," and a future
// rewrite that uses a different empty-handling approach should fail
// here rather than silently change behavior.
func TestLooksLikeFlag_EmptyString(t *testing.T) {
	t.Parallel()
	if looksLikeFlag("") {
		t.Error("looksLikeFlag(\"\") = true, want false")
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
		"admin claim",
		"admin revoke q_01HXYZ8ABCDEF0123456789AB",
		"admin add <@U12345>",
		"admin remove <@U12345|kevin>",
		testAdminListCmd,
		"get https://example.com",
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
		ErrMissingTarget,
		ErrMissingUserMention,
		ErrInvalidUserMention,
		ErrInvalidAlias,
		ErrInvalidQURLID,
		ErrUnexpectedArgument,
		ErrInvalidFlag,
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
			// `:warning:` message.
			matched := false
			for _, sentinel := range knownSentinels {
				if errors.Is(err, sentinel) {
					matched = true
					break
				}
			}
			if !matched {
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
