package internal

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Subcommand is a recognized verb after the `/qurl` slash command.
//
// The grammar is intentionally tiny — a slash-command line in Slack has
// constrained shape (no quoting beyond what the user types into the box),
// so a regex tokenizer is a better fit than a full CLI library like Cobra.
type Subcommand string

// Recognized subcommands. PR-3c.1 only defines the grammar; behaviors land
// in PR-3c.3+. Admin verbs use the second positional word ("claim",
// "allow", etc.) as the actual operation; the parser preserves that
// distinction in [Command.AdminAction].
const (
	// SubcmdHelp covers both `/qurl` (empty text) and `/qurl help`.
	SubcmdHelp Subcommand = "help"
	// SubcmdGet mints a one-time access link for a tunnel `$slug` or a
	// channel-scoped `$alias`. Raw URLs and `$r_<id>` resource IDs are
	// rejected — get is slug/alias-only.
	SubcmdGet Subcommand = "get"
	// SubcmdSetAlias binds an alias to a target. The parser accepts a
	// URL, resource ID, or tunnel slug shape, but the handler
	// ([Handler.handleSetAlias] / parseAliasArgs) now enforces
	// tunnels-only — only a `$slug` target is bound; URL/`r_<id>` are
	// rejected.
	SubcmdSetAlias Subcommand = "setalias"
	// SubcmdUnsetAlias clears the alias on the resource it points at.
	SubcmdUnsetAlias Subcommand = "unsetalias"
	// SubcmdAliases lists owner-scoped aliases visible in the channel.
	SubcmdAliases Subcommand = "aliases"
	// SubcmdAdmin groups the bot-admin membership verbs; see
	// [Command.AdminAction] for the specific verb. No longer a literal
	// leading word — the flat `/qurl-admin add|remove|admins` verbs map onto
	// it (the legacy `admin <verb>` prefix is redirected at dispatch).
	SubcmdAdmin Subcommand = "admin"
	// SubcmdRevoke revokes a protected resource (and all its qURLs) by the
	// `$<id|alias>` the bot shows; the handler resolves the token to a
	// resource_id via resolveTokenForGet. Replaces the former `admin revoke
	// <qurl_id>` per-link kill.
	SubcmdRevoke Subcommand = "revoke"
	// SubcmdList is the legacy listing of recent qURLs.
	SubcmdList Subcommand = "list"
)

// AdminAction names a bot-admin membership verb. The flat `/qurl-admin
// add|remove|admins` verbs each map onto one of these (the handlers switch on
// it); resource revoke is its own [SubcmdRevoke], not an AdminAction.
type AdminAction string

// Recognized admin actions.
const (
	// AdminAdd promotes a Slack user to bot admin. The argument is the
	// Slack `<@U12345>` mention syntax; the parsed user ID lands on
	// [Command.UserID].
	AdminAdd AdminAction = "add"
	// AdminRemove demotes a Slack user from bot admin. Same mention-
	// argument shape as [AdminAdd].
	AdminRemove AdminAction = "remove"
	// AdminList lists the workspace owner and current bot admins.
	// No positional arguments.
	AdminList AdminAction = "list"
)

// Admin-gate audit labels. Unlike the parser-enumerated actions above
// (which come from parsing `admin <action>`), these name the verb being
// gated when a handler calls requireAdminSync / requireAliasAdminGate.
// They surface only as the `action` telemetry field on the admin-gate log
// lines; naming them keeps a typo from becoming a silent mislabel in the
// audit trail.
const (
	AdminActionSetAlias   AdminAction = "set_alias"
	AdminActionUnsetAlias AdminAction = "unset_alias"
	// AdminActionExpose is the gate-audit label for the `/qurl-admin expose`
	// chooser (the two-button connector/URL picker).
	AdminActionExpose AdminAction = "expose"
	// AdminActionExposeConnector / AdminActionExposeURL are the gate-audit labels
	// for the single-word connector/URL verbs `/qurl-admin expose-connector` and
	// `/qurl-admin expose-url` — both the bare guided-modal entry and the typed
	// power-user form. Distinct from AdminActionExpose (the two-button picker) so
	// the audit trail names the exact verb.
	AdminActionExposeConnector  AdminAction = "expose_connector"
	AdminActionExposeURL        AdminAction = "expose_url"
	AdminActionSetDisplayName   AdminAction = "set_display_name"
	AdminActionUnsetDisplayName AdminAction = "unset_display_name"
	AdminActionRevoke           AdminAction = "revoke"
)

// Command is the parsed shape of a `/qurl …` slash command.
type Command struct {
	// Subcommand is the first word (or [SubcmdHelp] when text is empty).
	Subcommand Subcommand
	// AdminAction is the second word when [Subcommand] is [SubcmdAdmin];
	// empty otherwise.
	AdminAction AdminAction
	// Alias is the `$<slug>` or `$<alias>` argument (sigil stripped) for
	// `get`, `setalias`, and `unsetalias`. `get` resolves it as a tunnel
	// slug or a channel alias; the alias-mutating verbs treat it as the
	// alias name.
	Alias string
	// Target is the trailing positional arg used by `setalias` (parser
	// accepts a URL, raw resource_id, or `$slug` shape — the handler then
	// enforces tunnels-only). `revoke` carries its `$<id|alias>` in Alias
	// (via parseAliasToken), not here.
	Target string
	// UserID is the parsed Slack user ID from a `<@U12345>` mention
	// argument used by `admin add` / `admin remove`. Distinct from the
	// `user_id` form-field of the slash command itself (which is the
	// *caller* — the user who typed the command); this field holds the
	// *target* of the verb (the user being added or removed).
	UserID string
	// Flags holds optional `key:value` flags. Only `dm` and `reason`
	// are recognized today (on `get`). One-time use is unconditional
	// for `get` — there is no `once` flag (the link always burns on
	// first redemption), so it isn't a flag here.
	Flags map[string]string
	// Raw is the original trimmed text, kept for diagnostics.
	Raw string
}

// DM returns the parsed value of the `dm:true` flag on `get`.
func (c *Command) DM() bool {
	if c == nil {
		return false
	}
	v, ok := c.Flags["dm"]
	if !ok {
		return false
	}
	return strings.EqualFold(v, "true")
}

// Reason returns the parsed `reason:"..."` flag on `get`, empty when unset.
func (c *Command) Reason() string {
	if c == nil {
		return ""
	}
	return c.Flags["reason"]
}

// ErrEmptyResource is returned when a subcommand expects a `$alias`
// argument and the user either omitted it entirely OR supplied a bare
// `$` with no name after the sigil. The handler in PR-3c.3+ renders
// the same friendly "you forgot the alias" message in both cases.
var ErrEmptyResource = errors.New("missing or empty $alias argument")

// ErrMissingSigil is returned when a token in the resource position does
// not start with `$`. We require the sigil to keep aliases visually
// distinct from raw URLs / resource_ids in the slash-command grammar.
var ErrMissingSigil = errors.New("alias must start with $")

// ErrUnknownSubcommand is returned for any first word not in the grammar.
var ErrUnknownSubcommand = errors.New("unknown subcommand")

// ErrMissingTarget is returned when `setalias` is invoked without its
// trailing target positional argument.
var ErrMissingTarget = errors.New("missing target argument")

// ErrURLNotSupportedGet is returned when `/qurl get` is handed a raw
// URL. The Slack bot only mints links for tunnel resources now, reached
// by their `$slug` or a channel `$alias` — never an arbitrary URL.
//
// The text is a terse sentinel like the other parser errors; the
// rich, user-facing copy that names the fix lives in the handler
// ([Handler.handleGet] maps this sentinel via errors.Is), matching the
// repo convention that keeps multi-sentence prose out of `error`-typed
// values (see the parseAliasArgs doc comment).
var ErrURLNotSupportedGet = errors.New("raw URL not supported by get")

// ErrResourceIDNotSupportedGet is returned when `/qurl get` is handed a
// `$r_<id>` resource-id token. The resource-id get form is gone (slug/alias
// only), but pre-tunnels-only `/qurl list` surfaced `$r_<id>` tokens, so a
// user mid-migration may still paste one. Distinct from [ErrInvalidAlias] so
// the handler's copy can name resource IDs and redirect to the `$slug`,
// rather than reporting the generic alias-charset rule the `_` would trip.
// Same terse-sentinel / rich-handler-copy split as [ErrURLNotSupportedGet].
var ErrResourceIDNotSupportedGet = errors.New("resource id not supported by get")

// ErrMissingUserMention is returned when `/qurl-admin add` / `remove`
// are invoked without a `<@U…>` Slack user mention.
var ErrMissingUserMention = errors.New("missing @user mention")

// ErrInvalidUserMention is returned when the `/qurl-admin add` / `remove`
// argument doesn't match the Slack mention encoding
// `<@U12345>` / `<@U12345|name>`.
var ErrInvalidUserMention = errors.New("invalid @user mention")

// ErrInvalidAlias is returned when an alias name (the part after `$`)
// contains characters outside the recognized set. Catching this in
// the parser surfaces a friendly slash-command error instead of
// punting an obviously-bogus alias to qurl-service.
var ErrInvalidAlias = errors.New("invalid alias")

// ErrUnexpectedArgument is returned when a verb that takes no
// positional arguments receives one (e.g. `admin policies extra`).
// Catches user-facing typos earlier than the handler dispatch.
var ErrUnexpectedArgument = errors.New("unexpected argument")

// ErrInvalidFlag is the sentinel wrapped by every [applyFlag] error
// path (unknown key, missing-key-before-colon, empty value,
// expected-key:value). Tests in [TestParse_GetFlagErrors] match on
// this sentinel via `errors.Is` rather than on the formatted
// message substring, so the user-visible copy can evolve without
// churning the test surface.
var ErrInvalidFlag = errors.New("invalid flag")

// userMentionPattern matches Slack's encoded user-mention form
// `<@U12345678>` (and `<@U12345678|display-name>` when the client
// includes the optional pipe-delimited display label). Real Slack
// user IDs start with `U` (workspace user) or `W` (Enterprise Grid
// org-level user) followed by 8+ uppercase-alphanumeric characters,
// per Slack's documented ID grammar — `{8,63}` after the prefix
// rejects toy IDs like `<@A>` at parse time, where a future
// AddAdmin would otherwise happily store a bogus user ID. The
// `{,63}` ceiling caps a pathological paste so it surfaces as a
// parser error rather than propagating to DDB.
//
// TODO(legacy-slack-ids): pre-2017 Slack workspaces may have user
// IDs shorter than 9 chars total (e.g. `U12345`). If any such
// workspace hits beta with an admin who can't be added/removed
// because their ID rejects here, relax the {8,} floor — the
// security posture only depends on the regex rejecting truly-
// malformed tokens, not on the length floor itself.
var userMentionPattern = regexp.MustCompile(`^<@([UW][A-Z0-9]{8,63})(?:\|[^>]*)?>$`)

// flagKeyCharset is the shared key-shape contract for flag-style
// tokens. Used by both [flagPattern] (full key:value parse) and
// [looksLikeFlag] (pre-applyFlag gate, case-insensitive on the input
// because the case-fold happens later in applyFlag). Keeping the two
// in lockstep prevents a future change to the key charset from
// landing in only one place and silently drifting the two checks
// apart.
const flagKeyCharset = `[a-z][a-z0-9_]*`

// flagPattern matches `key:value` and `key:"quoted value"` shapes.
//
// The pattern is intentionally permissive on the value side — Slack's
// slash-command form-encoding has already done one round of decoding by
// the time we see the body, so we don't try to handle further escapes
// here. Quoted values let users put spaces in `reason:"…"`.
var flagPattern = regexp.MustCompile(`^(` + flagKeyCharset + `):(?:"([^"]*)"|(\S+))$`)

// flagKeyShape is the key-only counterpart to [flagPattern], used by
// [looksLikeFlag] to gate on key shape BEFORE applyFlag's case-fold
// runs. Case-insensitive on the input via the `(?i)` flag so callers
// can type `DM:true` on a mobile client and have applyFlag's
// post-fold regex match accept it. Stays anchored to
// [flagKeyCharset] so any future expansion of the key charset
// (e.g., adding `-`) lands in one source.
var flagKeyShape = regexp.MustCompile(`(?i)^` + flagKeyCharset + `$`)

// The shared alias contract — `aliasCharsetPattern` (regex) and
// `aliasMaxLen` (length cap) — lives in
// [apps/slack/internal/handler_alias.go] alongside the other alias
// verbs added by PR #347. This parser reuses both halves so the
// `setalias` HTTP path and the slash-command grammar reject the
// same alias shapes with the same wording. (Pre-merge the regex
// was duplicated in both files; the dedup lives here.) See
// [aliasCharsetPattern] / [aliasMaxLen] in handler_alias.go for the
// full doc on internal-`--` deference to qurl-service and the nhp
// #1825 GSI-key sizing.

// Parse tokenizes the trimmed `text` field of a Slack slash command into a
// [Command]. Empty or `help` text returns a [Command] with Subcommand =
// [SubcmdHelp] and no error. Behavior of each subcommand is implemented
// elsewhere (PR-3c.3+) — this function only validates grammar.
func Parse(text string) (*Command, error) {
	text = strings.TrimSpace(text)
	if text == "" || strings.EqualFold(text, "help") {
		return &Command{Subcommand: SubcmdHelp, Raw: text, Flags: map[string]string{}}, nil
	}

	tokens := tokenize(text)
	if len(tokens) == 0 {
		return &Command{Subcommand: SubcmdHelp, Raw: text, Flags: map[string]string{}}, nil
	}

	// Invariant: Flags is initialized here so every sub-parser can
	// call applyFlag without nil-check. Adding a new sub-parser that
	// builds its own *Command must preserve this — applyFlag writes
	// to cmd.Flags directly and will panic on a nil map.
	cmd := &Command{Raw: text, Flags: map[string]string{}}
	first := strings.ToLower(tokens[0])
	rest := tokens[1:]

	switch Subcommand(first) {
	case SubcmdHelp:
		// Help is the friendly default; trailing tokens are
		// intentionally ignored so a user fumbling `help me` still
		// gets help instead of an `ErrUnexpectedArgument`. This is
		// the one deliberate exception to the strict-posture rule.
		cmd.Subcommand = SubcmdHelp
		return cmd, nil
	case SubcmdGet:
		cmd.Subcommand = SubcmdGet
		return parseGet(cmd, rest)
	case SubcmdSetAlias:
		cmd.Subcommand = SubcmdSetAlias
		return parseSetAlias(cmd, rest)
	case SubcmdUnsetAlias:
		cmd.Subcommand = SubcmdUnsetAlias
		return parseAliasOnly(cmd, rest)
	case SubcmdAliases:
		cmd.Subcommand = SubcmdAliases
		if len(rest) > 0 {
			return nil, fmt.Errorf("%w: %q", ErrUnexpectedArgument, rest[0])
		}
		return cmd, nil
	case SubcmdRevoke:
		cmd.Subcommand = SubcmdRevoke
		return parseRevoke(cmd, rest)
	case "add":
		// Flat bot-admin membership verbs map onto SubcmdAdmin + an
		// AdminAction so the membership handlers (handleAdminAdd / Remove /
		// List) stay unchanged. The legacy `admin <verb>` prefix is redirected
		// at dispatch (dispatchAdminCommand), so it never reaches Parse. Keep
		// these spellings in sync with adminVerbs + dispatchAdminCommand.
		cmd.Subcommand = SubcmdAdmin
		cmd.AdminAction = AdminAdd
		return parseAdminMention(cmd, rest)
	case "remove":
		cmd.Subcommand = SubcmdAdmin
		cmd.AdminAction = AdminRemove
		return parseAdminMention(cmd, rest)
	case "admins":
		cmd.Subcommand = SubcmdAdmin
		cmd.AdminAction = AdminList
		if len(rest) > 0 {
			// truncateForError (backtick code-span + mrkdwn neutralize) to
			// match the other admin verbs' echo posture, not the bare `%q`
			// the user-surface list/aliases verbs use.
			return nil, fmt.Errorf("%w: `%s`", ErrUnexpectedArgument, truncateForError(rest[0]))
		}
		return cmd, nil
	case SubcmdAdmin:
		// SubcmdAdmin is the value the flat add/remove/admins verbs map onto
		// (above); it has no literal leading word of its own. The deprecated
		// `admin <verb>` prefix is redirected at dispatch before Parse runs, so
		// a literal `admin` reaching here is just an unknown verb. This explicit
		// arm keeps `exhaustive` enforced for the Subcommand enum (the repo runs
		// it without default-signifies-exhaustive).
		return nil, fmt.Errorf("%w: %q", ErrUnknownSubcommand, tokens[0])
	case SubcmdList:
		cmd.Subcommand = SubcmdList
		if len(rest) > 0 {
			return nil, fmt.Errorf("%w: %q", ErrUnexpectedArgument, rest[0])
		}
		return cmd, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownSubcommand, tokens[0])
	}
}

// tokenize splits on whitespace while preserving double-quoted runs as a
// single token. We don't need shell-grade quoting (no escapes, no single
// quotes) — Slack's form already collapses surrounding control characters
// before we see the body.
//
// `inQuotes` is a parity toggle, not a balanced-pair matcher: every `"`
// flips the state. Adversarial input like `"a"b"c"` toggles four times
// and produces a single token. Slack's slash-command box can't realistically
// emit such input, but a future caller swapping in a different source
// should be aware the contract is "even count of `"` → quoted runs
// behave as expected; odd count → falls back to the unbalanced-tolerance
// path below."
//
// Balanced outer double quotes around a positional token are stripped so
// `setalias $a "https://x"` produces `Target = "https://x"` rather than
// `Target = "\"https://x\""`. Flag values strip quotes via [flagPattern]
// already; this normalizes the positional path so both surfaces behave
// the same way.
//
// Unbalanced quotes are tolerated, not rejected: input like
// `setalias $a "https://x` (no closing quote) yields a final token
// `"https://x` with the literal opening quote preserved. The caller
// (the URL/resource validator in PR-3c.3+) will reject the malformed
// target naturally. We don't error here because Slack's slash-command
// box collapses runs of whitespace before we see the body, making
// stray-quote inputs vanishingly rare in practice.
func tokenize(text string) []string {
	var out []string
	var cur strings.Builder
	inQuotes := false
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		s := cur.String()
		// Strip balanced outer quotes, e.g. `"foo"` -> `foo`. We
		// don't touch unbalanced (`"foo` or `foo"`) because the
		// caller may want to surface that as a parse error.
		// Byte-level comparison is safe here: `"` is ASCII 0x22,
		// which never appears as a continuation byte in a
		// multi-byte UTF-8 sequence (those are 0x80-0xBF). So a
		// rune like `世界` ending in a multi-byte char can't trigger
		// a spurious match on the closing quote.
		if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
			s = s[1 : len(s)-1]
		}
		// Drop tokens that are empty post-strip. A bare `""` would
		// otherwise produce an empty-string token here (the
		// cur.Len() == 0 guard above catches only the pre-write
		// state, not the post-strip state), letting `setalias $a ""`
		// slip through with Target = "" instead of ErrMissingTarget.
		// Slack collapses adjacent whitespace before we see the
		// body, so a non-quoted-pair empty token is unreachable.
		if s == "" {
			cur.Reset()
			return
		}
		out = append(out, s)
		cur.Reset()
	}
	for _, r := range text {
		switch {
		case r == '"':
			cur.WriteRune(r)
			inQuotes = !inQuotes
		case (r == ' ' || r == '\t') && !inQuotes:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// parseGet extracts the positional argument (a tunnel `$<slug>` or a
// channel `$<alias>`) and the optional `dm:` / `reason:` flags. A raw
// `http(s)://` first positional is rejected with [ErrURLNotSupportedGet];
// otherwise the token must start with `$` and validate as an alias-shaped
// name via [parseAliasToken]. Get mints from a tunnel slug or a channel
// alias only — a `$r_<id>` resource ID fails the alias charset (the `_`)
// and is rejected, same as a raw URL. The alias-mutating verbs
// (`setalias`, `unsetalias`) and `revoke` (via parseRevoke) share this same
// `$`-sigil token shape. Surplus positional args after the first are an error.
func parseGet(cmd *Command, rest []string) (*Command, error) {
	if len(rest) == 0 {
		return nil, ErrEmptyResource
	}
	if hasASCIIPrefixFold(rest[0], "https://") || hasASCIIPrefixFold(rest[0], "http://") {
		// Raw URLs are no longer mintable through Slack — get takes a
		// tunnel `$slug` or a channel `$alias` only.
		return nil, ErrURLNotSupportedGet
	}
	if strings.HasPrefix(rest[0], "$r_") {
		// A `$r_<id>` paste: the resource-id get form is gone. Redirect to
		// the `$slug` rather than falling through to the generic
		// ErrInvalidAlias (the `_` trips the alias charset), which wouldn't
		// explain that resource IDs are deliberately unsupported now. A real
		// alias can never start with `r_` — `_` isn't in the alias charset —
		// so this never shadows a valid token.
		return nil, ErrResourceIDNotSupportedGet
	}
	alias, err := parseAliasToken(rest[0])
	if err != nil {
		return nil, err
	}
	cmd.Alias = alias
	for _, tok := range rest[1:] {
		// Surface non-flag-shaped tokens as ErrUnexpectedArgument so
		// `get $alias junk` reads as a typo (matches the strict
		// posture taken on `aliases`, `list`, `admin policies`, etc.).
		// applyFlag would otherwise report "invalid flag: \"junk\"
		// (expected key:value)" — accurate to applyFlag but confusing
		// to a user who didn't intend to type a flag at all.
		if !looksLikeFlag(tok) {
			return nil, fmt.Errorf("%w: %q", ErrUnexpectedArgument, tok)
		}
		if err := applyFlag(cmd, tok); err != nil {
			return nil, err
		}
	}
	return cmd, nil
}

// parseSetAlias extracts `$alias <target>`. At the parser layer Target
// may be a URL, raw resource_id, or `$slug` shape; the tunnels-only
// policy (only `$slug` is bound) is enforced downstream in
// parseAliasArgs / [Handler.handleSetAlias]. Strict-posture like the
// other verbs: exactly one alias and exactly one target. Quoted URLs
// (e.g. `setalias $a "https://x with space"`) survive as a single
// token because [tokenize] keeps quoted runs intact, so the
// previous "join the tail" behavior was only ever reachable for
// unquoted multi-word input — which is precisely the typo class
// (`setalias $a https://x dm:true` silently swallowing a stray
// flag-shaped token) we want to reject.
func parseSetAlias(cmd *Command, rest []string) (*Command, error) {
	if len(rest) == 0 {
		return nil, ErrEmptyResource
	}
	alias, err := parseAliasToken(rest[0])
	if err != nil {
		return nil, err
	}
	cmd.Alias = alias
	if len(rest) < 2 {
		return nil, ErrMissingTarget
	}
	if len(rest) > 2 {
		return nil, fmt.Errorf("%w: %q (quote the target if it contains spaces)", ErrUnexpectedArgument, rest[2])
	}
	cmd.Target = rest[1]
	return cmd, nil
}

// parseAliasOnly is the shared parser for verbs that take just `$alias`
// (currently `unsetalias`).
func parseAliasOnly(cmd *Command, rest []string) (*Command, error) {
	if len(rest) == 0 {
		return nil, ErrEmptyResource
	}
	alias, err := parseAliasToken(rest[0])
	if err != nil {
		return nil, err
	}
	cmd.Alias = alias
	return cmd, nil
}

// parseAdminMention parses the single `<@U…>` mention argument for the
// `/qurl-admin add` / `remove` membership verbs. The leading verb and the
// AdminAction are already set by [Parse]; the argument arrives as Slack's
// encoded mention syntax (`<@U12345>` or `<@U12345|name>`) and
// [userMentionPattern] yields the bare user ID for the handler. A surplus
// positional arg is rejected so a typo like `add @u extra` surfaces early.
func parseAdminMention(cmd *Command, rest []string) (*Command, error) {
	if len(rest) == 0 {
		return nil, ErrMissingUserMention
	}
	uid, ok := matchUserMention(rest[0])
	if !ok {
		return nil, fmt.Errorf("%w: `%s` (expected a Slack @user mention like `<@U12345>`)", ErrInvalidUserMention, truncateForError(rest[0]))
	}
	cmd.UserID = uid
	if len(rest) > 1 {
		return nil, fmt.Errorf("%w: `%s`", ErrUnexpectedArgument, truncateForError(rest[1]))
	}
	return cmd, nil
}

// parseRevoke extracts the `$<id|alias>` of the resource to revoke. Same
// token shape as `/qurl get` (sigil required via [parseAliasToken]); the
// handler resolves it to a resource_id through resolveTokenForGet. Revoke is
// destructive, so a surplus positional arg is rejected rather than ignored.
func parseRevoke(cmd *Command, rest []string) (*Command, error) {
	if len(rest) == 0 {
		return nil, ErrEmptyResource
	}
	alias, err := parseAliasToken(rest[0])
	if err != nil {
		return nil, err
	}
	cmd.Alias = alias
	if len(rest) > 1 {
		return nil, fmt.Errorf("%w: `%s`", ErrUnexpectedArgument, truncateForError(rest[1]))
	}
	return cmd, nil
}

// matchUserMention returns the `U…` ID and true when `tok` is a Slack
// user-mention encoded form, else returns ("", false).
func matchUserMention(tok string) (string, bool) {
	m := userMentionPattern.FindStringSubmatch(tok)
	if len(m) < 2 {
		return "", false
	}
	return m[1], true
}

// truncateForError caps a token at 32 runes of content (plus a `…`
// truncation marker when truncation fires, so the max rendered
// length is 33 runes) and runs it through [escapeMrkdwnCode], so
// the result is safe to echo inside a Slack mrkdwn code span in an
// error message. Callers wrap the return value with a backtick
// code-span delimiter so any `<!channel>` / `<@U…>` / other mrkdwn
// token in the user's input renders as a literal code-span
// character rather than as a Slack mention.
//
// The escape table (backtick → U+02CA, line breaks → space) is owned
// by [escapeMrkdwnCode] in views.go so the same-package neighbor is
// the single source of truth — a future renderer-driven adjustment
// to the table (e.g. CR/LF handling, new break tokens) flows through
// both error and view code spans without drift.
func truncateForError(s string) string {
	const maxRunes = 32
	runes := []rune(s)
	if len(runes) > maxRunes {
		runes = append(runes[:maxRunes], '…')
	}
	return escapeMrkdwnCode(string(runes))
}

// parseAliasToken enforces the `$` sigil, strips it, and validates the
// remaining alias against the shared alias contract — both halves of
// it: the [aliasMaxLen] length cap and the [aliasCharsetPattern]
// regex declared in handler_alias.go. Empty after the sigil (`$`)
// is treated as an empty-resource error; over-cap as an invalid-alias
// error with a length-specific message; out-of-charset as an
// invalid-alias error with a charset-specific message.
//
// Order matters: length is checked before the regex because a
// 200-char alias matches the charset but blows the upstream GSI key
// length. Surfacing it as "too long" gives the user a friendlier
// error than the generic regex rejection — and matches the wording
// pattern handler_alias.go uses for its `setalias` path so the two
// entry points produce parallel copy.
func parseAliasToken(tok string) (string, error) {
	if !strings.HasPrefix(tok, "$") {
		return "", fmt.Errorf("%w: got `%s`", ErrMissingSigil, truncateForError(tok))
	}
	alias := strings.TrimPrefix(tok, "$")
	if alias == "" {
		return "", ErrEmptyResource
	}
	if len(alias) > aliasMaxLen {
		return "", fmt.Errorf("%w: `%s` is longer than %d characters", ErrInvalidAlias, truncateForError(alias), aliasMaxLen)
	}
	if !aliasCharsetPattern.MatchString(alias) {
		return "", fmt.Errorf("%w: `%s` (allowed: lowercase a-z, 0-9, hyphen, no leading/trailing hyphen)", ErrInvalidAlias, truncateForError(alias))
	}
	return alias, nil
}

// looksLikeFlag reports whether `tok` is shaped like a `key:value`
// flag rather than a stray positional. We can't just check for `:`
// because a fat-fingered URL (`get $alias https://example.com:8080`)
// would otherwise route to applyFlag and surface as the confusing
// `unknown flag: "https"`. The flag-key half is matched against
// [flagPattern]'s `[a-z][a-z0-9_]*` shape here too — the two checks
// stay in lockstep so a key shape that survives `looksLikeFlag`
// always survives `applyFlag`'s regex match (after key-half
// lowercasing in applyFlag) and vice versa.
//
// Also bails out for `http://` / `https://` specifically (matched
// case-insensitively so a `HTTPS://x:8080` clipboard paste routes
// the same way): those would match the lowercase-key shape but are
// overwhelmingly a typo-class URL paste, not a real flag attempt.
//
// Coverage is intentionally http(s) only. Other URI schemes
// (`ssh://`, `s3://`, `git://`, `mailto:`) would still surface
// `unknown flag: "<scheme>"` via applyFlag — that's acceptable
// because DDB-bound resources in this codebase are HTTP/HTTPS in
// practice (see qurl-service's URL validator). PR-3c.3+ should
// revisit if the validator there ever accepts non-HTTP schemes.
func looksLikeFlag(tok string) bool {
	// Case-insensitive http(s):// match — clipboard pastes sometimes
	// uppercase the scheme. `strings.EqualFold` on a length-bounded
	// prefix avoids allocating a full-token lowercase copy (a 2KB URL
	// token would otherwise allocate 2KB just to compare 7-8 bytes).
	if hasASCIIPrefixFold(tok, "https://") || hasASCIIPrefixFold(tok, "http://") {
		return false
	}
	// Empty `tok` is handled by accident here: strings.IndexByte("",
	// ':') returns -1, which fails the `colonIdx <= 0` guard. Pinned
	// in TestLooksLikeFlag_EmptyString below so a refactor can't
	// silently regress that path.
	colonIdx := strings.IndexByte(tok, ':')
	if colonIdx <= 0 {
		return false
	}
	// Single source of truth for key shape: [flagKeyShape] is the
	// case-insensitive form of [flagKeyCharset], the same charset
	// [flagPattern] uses post-case-fold. The two stay in lockstep so
	// a future change to the key charset lands in one place rather
	// than drifting between this gate and applyFlag's regex match.
	return flagKeyShape.MatchString(tok[:colonIdx])
}

// hasASCIIPrefixFold reports whether `s` starts with `prefix` under
// ASCII case-fold, without allocating. `strings.EqualFold` on
// `s[:len(prefix)]` would index out-of-bounds when `s` is shorter
// than `prefix`; this helper handles the short-string case
// explicitly. Used by [looksLikeFlag] for the http(s):// carve-out.
func hasASCIIPrefixFold(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	return strings.EqualFold(s[:len(prefix)], prefix)
}

// applyFlag parses a single `key:value` token into [Command.Flags]. Only
// the recognized keys (`dm`, `reason`) survive — unknown keys
// are an error so a typo doesn't silently no-op. The key half is
// case-folded so `DM:true` and `Reason:foo` work the way users on
// mobile clients (with auto-capitalize) expect; the value half is
// preserved verbatim because reasons are user-facing prose. An
// empty value (either `key:` or `key:""`) is rejected — the handler
// in PR-3c.3+ should be able to distinguish "flag unset" from "flag
// set to empty" by absence in [Command.Flags] alone.
//
// Per-key validation is strict-posture: `dm` accepts only the boolean
// strings `true` / `false` (case-folded), so a user typing `dm:yes`
// sees a friendly error rather than the silent-falsey behavior the
// unvalidated form would have produced ([Command.DM] case-equals
// against "true", so any non-"true" value silently returns false —
// exactly the typo class the rest of the parser rejects). `reason`
// accepts any non-empty prose because the handler in PR-3c.3+ uses it
// for audit text where the user's exact wording is the point.
func applyFlag(cmd *Command, tok string) error {
	colonIdx := strings.IndexByte(tok, ':')
	if colonIdx < 0 {
		return fmt.Errorf("%w: %q (expected key:value)", ErrInvalidFlag, tok)
	}
	if colonIdx == 0 {
		return fmt.Errorf("%w: %q (missing key before colon)", ErrInvalidFlag, tok)
	}
	// Lowercase only the key portion so `Reason:"On Call"` keeps
	// its mixed-case value intact.
	normalized := strings.ToLower(tok[:colonIdx]) + tok[colonIdx:]
	// Empty bare value (`reason:`) doesn't match flagPattern's
	// `(\S+)` alternation, so the regex would surface a generic
	// "expected key:value" error — but the shape IS key:value, just
	// with no value. Detect it ahead of the regex so bare-empty and
	// quoted-empty (`reason:""`) both report the same "empty value"
	// reason.
	if colonIdx == len(tok)-1 {
		return fmt.Errorf("%w: %q (empty value — use a non-empty value or omit the flag)", ErrInvalidFlag, tok)
	}
	m := flagPattern.FindStringSubmatch(normalized)
	if len(m) == 0 {
		return fmt.Errorf("%w: %q (expected key:value)", ErrInvalidFlag, tok)
	}
	key := m[1]
	val := m[2]
	if val == "" {
		val = m[3]
	}
	if val == "" {
		return fmt.Errorf("%w: %q (empty value — use a non-empty value or omit the flag)", ErrInvalidFlag, tok)
	}
	switch key {
	case "dm":
		// Strict-posture boolean: only `true` / `false` (case-folded)
		// accepted. Without this gate, `dm:yes` / `dm:1` / `dm:please`
		// would parse fine and then silently no-op because
		// [Command.DM] case-equals against "true". That silent-falsey
		// behavior is the same UX failure mode the rest of the
		// parser carefully rejects for typo'd flag keys
		// (`whatever:true` → ErrInvalidFlag), so we reject typo'd
		// values too.
		if !strings.EqualFold(val, "true") && !strings.EqualFold(val, "false") {
			return fmt.Errorf("%w: dm:%q (use dm:true or omit the flag)", ErrInvalidFlag, val)
		}
		cmd.Flags[key] = val
		return nil
	case "reason":
		cmd.Flags[key] = val
		return nil
	case "once":
		// `once` was removed — one-time use is now the unconditional
		// default for `/qurl get`. Still reject (strict-flag posture),
		// but with a transitional hint instead of the generic
		// "unknown flag", since some users have `once:true` in saved
		// slash-command recipes and deserve to know it's now redundant.
		return fmt.Errorf("%w: `once` is no longer needed — every `/qurl get` link is one-time use by default", ErrInvalidFlag)
	default:
		return fmt.Errorf("%w: unknown flag %q", ErrInvalidFlag, key)
	}
}
