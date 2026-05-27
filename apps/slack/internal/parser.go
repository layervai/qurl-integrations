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
	// SubcmdGet mints an access link for a raw URL, a channel-scoped
	// `$alias` configured by an admin, or a raw `$r_<id>` resource
	// token copy-pasted from `/qurl list`.
	SubcmdGet Subcommand = "get"
	// SubcmdSetAlias binds an alias to a target URL, resource ID, or tunnel slug.
	SubcmdSetAlias Subcommand = "setalias"
	// SubcmdUnsetAlias clears the alias on the resource it points at.
	SubcmdUnsetAlias Subcommand = "unsetalias"
	// SubcmdAliases lists owner-scoped aliases visible in the channel.
	SubcmdAliases Subcommand = "aliases"
	// SubcmdAdmin is the umbrella for admin-only operations; see
	// [Command.AdminAction] for the specific verb.
	SubcmdAdmin Subcommand = "admin"
	// SubcmdList is the legacy listing of recent qURLs.
	SubcmdList Subcommand = "list"
)

// AdminAction is the second word after `admin` (e.g. `admin revoke`).
type AdminAction string

// Recognized admin actions.
const (
	// AdminRevoke revokes a single previously minted qURL by its
	// `qurl_id` (no `$` sigil — operators paste the ID directly out
	// of an audit trail or a previous mint reply).
	AdminRevoke AdminAction = "revoke"
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

// ResourceTokenKind discriminates between the two shapes a `$<token>`
// argument can take after the sigil: a human-readable alias name or a
// raw `r_*` resource ID. Lifted to its own typed string so handlers
// can branch on the parsed shape without re-running regex matches.
type ResourceTokenKind string

// Recognized resource-token kinds.
const (
	// ResourceTokenAlias is a human-readable `$<alias>` like `$prod-db`.
	// The bare value matches [aliasCharsetPattern].
	ResourceTokenAlias ResourceTokenKind = "alias"
	// ResourceTokenResourceID is a raw `$r_<11chars>` shape like
	// `$r_k8xqp9h2sj9`. The bare value matches [resourceIDPattern]
	// (including the `r_` prefix — handlers consume the full ID).
	ResourceTokenResourceID ResourceTokenKind = "resource_id"
)

// ParsedResourceToken is the shape of a successfully-parsed `$<token>`
// argument. Returned by [requireResourceToken] so handlers can decide
// whether to call the alias-resolution path or the by-ID-lookup path
// without inspecting the value themselves.
type ParsedResourceToken struct {
	// Kind is the discriminator — alias vs resource_id.
	Kind ResourceTokenKind
	// Value is the bare token. For aliases it's the name without the
	// `$` sigil; for resource IDs it's the full ID including the `r_`
	// prefix (handlers pass it directly to a by-ID resource lookup).
	Value string
}

// Command is the parsed shape of a `/qurl …` slash command.
type Command struct {
	// Subcommand is the first word (or [SubcmdHelp] when text is empty).
	Subcommand Subcommand
	// AdminAction is the second word when [Subcommand] is [SubcmdAdmin];
	// empty otherwise.
	AdminAction AdminAction
	// Alias is the `$alias` argument (sigil stripped) when the parsed
	// resource-token is an alias. Empty when the user supplied a raw
	// resource ID instead — see [Command.Resource] for the kind-aware
	// view. Kept populated for the alias path so verbs that only accept
	// aliases (`setalias`, `unsetalias`) keep their existing read site.
	Alias string
	// Resource is the kind-aware shape of the `$<token>` argument when
	// the verb accepts both alias and resource-ID forms (currently
	// `/qurl get`). Zero value when the verb only takes aliases —
	// callers that want the legacy single-string view should read
	// [Command.Alias] instead.
	Resource ParsedResourceToken
	// Target is the trailing positional arg used by `setalias` (a URL, raw
	// resource_id, or `$slug`) and the legacy `create <url>`.
	Target string
	// UserID is the parsed Slack user ID from a `<@U12345>` mention
	// argument used by `admin add` / `admin remove`. Distinct from the
	// `user_id` form-field of the slash command itself (which is the
	// *caller* — the user who typed the command); this field holds the
	// *target* of the verb (the user being added or removed).
	UserID string
	// Flags holds optional `key:value` flags. Only `dm`, `once`, and
	// `reason` are recognized today (on `get`).
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

// Once returns the parsed value of the `once:true` flag on `get`.
func (c *Command) Once() bool {
	if c == nil {
		return false
	}
	v, ok := c.Flags[flagKeyOnce]
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

// ErrUnknownAdminAction is returned for any `admin <verb>` where verb is
// not in the recognized [AdminAction] set.
var ErrUnknownAdminAction = errors.New("unknown admin action")

// ErrMissingAdminAction is returned when bare `admin` is invoked with
// no verb at all. Separate from [ErrUnknownAdminAction] so an
// `errors.Is` caller can distinguish "user forgot to type a verb"
// from "user typed something we don't recognize" — the right
// user-facing message differs ("which admin command?" vs "frobnicate
// isn't a thing, try `admin list` / `admin revoke` / …").
var ErrMissingAdminAction = errors.New("missing admin action")

// ErrMissingTarget is returned when `setalias` or `create` are invoked
// without the trailing target/URL argument.
var ErrMissingTarget = errors.New("missing target argument")

// ErrMissingUserMention is returned when `admin add` / `admin remove`
// are invoked without a `<@U…>` Slack user mention.
var ErrMissingUserMention = errors.New("missing @user mention")

// ErrInvalidUserMention is returned when the `admin add` / `admin
// remove` argument doesn't match the Slack mention encoding
// `<@U12345>` / `<@U12345|name>`.
var ErrInvalidUserMention = errors.New("invalid @user mention")

// ErrInvalidAlias is returned when an alias name (the part after `$`)
// contains characters outside the recognized set. Catching this in
// the parser surfaces a friendly slash-command error instead of
// punting an obviously-bogus alias to qurl-service.
var ErrInvalidAlias = errors.New("invalid alias")

// ErrInvalidQURLID is returned when `admin revoke <id>` receives a
// token that isn't shaped like a qurl_id (`q_<alnum>`).
var ErrInvalidQURLID = errors.New("invalid qurl_id")

// ErrUnexpectedArgument is returned when a verb that takes no
// positional arguments receives one (e.g. `admin policies extra`).
// Catches user-facing typos earlier than the handler dispatch.
var ErrUnexpectedArgument = errors.New("unexpected argument")

// flagKeyOnce is the canonical key for the `once:` strict-boolean
// flag. Constant rather than literal because goconst flags `"once"`
// at its 3-occurrence threshold on `--new-from-rev`.
const flagKeyOnce = "once"

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
// `{,63}` ceiling mirrors qurlIDPattern's posture so a pathological
// paste surfaces as a parser error rather than propagating to DDB.
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

// resourceIDPattern matches qurl-service's resource-ID shape: `r_`
// + 11 base64url chars (lowercased per generateRandomID's
// DNS-compatibility lowercase). Anchored at both ends so a partial
// substring like `r_short` or `r_abc...extra` is rejected. Mirrored
// from `qurl-service/internal/domain/qurl.go::resourceIDPattern` —
// when that schema changes, this regex changes in lockstep.
//
// The `_` in the `r_` prefix is what disambiguates this shape from
// [aliasCharsetPattern] at parse time: aliases are constrained to
// `[a-z0-9-]` (no underscore), so any `r_<…>` token is a resource
// ID by construction. The "resource-ID first" ordering in
// [requireResourceToken] is harmless rather than load-bearing — the
// two patterns can't both match the same bare value.
//
// TODO(upstream-rebrand): cross-repo lockstep with
// `qurl-service/internal/domain/qurl.go::resourceIDPattern`. If the
// upstream regex (charset, length, anchors) changes, mirror it here.
var resourceIDPattern = regexp.MustCompile(`^r_[a-z0-9_-]{11}$`)

// qurlIDPattern is the shape of a qurl_id passed to `admin revoke`.
// qurl-service emits `q_<UPPERCASE_ALPHANUMERIC>` (a ULID-style
// 26-char suffix; ULIDs are uppercase by spec); the regex is
// conservative — anything that doesn't match this gets surfaced as a
// parser error rather than letting `client.Delete` produce an opaque
// 404 from the backend. Operators paste these IDs out of an audit
// trail or a previous mint reply, so a fat-fingered space or an
// injected character is the most common failure mode this catches.
//
// The {16,64} length floor rejects obviously-truncated IDs (`q_abc`
// can't reach a real qURL) at parse time so the user gets a parser
// hint instead of an opaque 404. The ceiling fails a fat-paste at
// parse time rather than shipping a kilobyte URL path to qurl-service.
// Current qurl-service IDs are 26-char ULIDs so the {16,64} range is
// generous enough to absorb a one-off shape change without an SDK
// pin; widen further if the suffix grammar shifts.
//
// TODO(upstream-rebrand): the [A-Z0-9]-only character class will
// refuse legitimate IDs if qurl-service ever emits lowercase,
// hyphens, or underscores. The qurl-service ID grammar lives in the
// resource-mint path; widen this set in lockstep with that contract.
var qurlIDPattern = regexp.MustCompile(`^q_[A-Z0-9]{16,64}$`)

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
	case SubcmdAdmin:
		cmd.Subcommand = SubcmdAdmin
		return parseAdmin(cmd, rest)
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

// parseGet extracts the positional argument (a raw URL, a `$<alias>`,
// or a `$r_<id>` resource token) and the optional `dm:` / `reason:`
// flags. The first positional is treated as a URL when it has an
// `http://` or `https://` prefix; otherwise it must start with `$`
// and is routed through [requireResourceToken] so handlers can branch
// on alias vs resource-ID without re-parsing. Surplus positional args
// after the first are an error.
//
// `get` is the only verb in the grammar that accepts both alias and
// resource-ID shapes — the alias-mutating verbs (`setalias`,
// `unsetalias`) intentionally stay alias-only because their semantics
// are alias-scoped. `admin revoke` takes a raw `q_<id>` qurl_id (no
// sigil) so it doesn't go through this token shape. Letting `get`
// take a raw ID closes the gap between `/qurl list` (which surfaces
// IDs for un-aliased resources) and `/qurl get` (which previously
// only minted from aliases or URLs) so a list line can be
// copy-pasted into the next command without a manual lookup step.
func parseGet(cmd *Command, rest []string) (*Command, error) {
	if len(rest) == 0 {
		return nil, ErrEmptyResource
	}
	if hasASCIIPrefixFold(rest[0], "https://") || hasASCIIPrefixFold(rest[0], "http://") {
		cmd.Target = rest[0]
	} else {
		tok, err := requireResourceToken(rest[0])
		if err != nil {
			return nil, err
		}
		cmd.Resource = tok
		if tok.Kind == ResourceTokenAlias {
			// Keep [Command.Alias] populated so legacy read-sites in
			// tests or future shared helpers don't have to
			// special-case the get verb. The resource-ID branch
			// leaves Alias empty — handlers that route on Kind use
			// [Command.Resource] directly.
			cmd.Alias = tok.Value
		}
	}
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

// parseSetAlias extracts `$alias <target>`. Target may be a URL, raw
// resource_id, or `$slug`. Strict-posture like the
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

// parseAdmin dispatches on the second word (the [AdminAction]).
// `revoke` takes a `q_<id>` qurl_id; `add` / `remove` take a `<@U…>`
// mention; `list` takes no args. Verbs that take no args fail
// [ErrUnexpectedArgument] when given any — surfacing a typo like
// `admin list extra` early instead of silently routing it.
//
// `admin claim` was retired when /qurl setup absorbed the seed-admin
// step into the OAuth callback; the parser surfaces `claim` as
// ErrUnknownAdminAction now, matching `admin frobnicate`.
func parseAdmin(cmd *Command, rest []string) (*Command, error) {
	if len(rest) == 0 {
		return nil, ErrMissingAdminAction
	}
	verb := rest[0]
	action := AdminAction(strings.ToLower(verb))
	cmd.AdminAction = action
	tail := rest[1:]
	switch action {
	case AdminRevoke:
		// `revoke` takes a single positional `q_<id>` qurl_id (no
		// `$` sigil). A `$alias` token here surfaces an
		// [ErrUnexpectedArgument] with a hint to use the single-id
		// form — the alias-scoped revoke-all verb was cut in v1
		// because a per-link kill is enough for the beta scope.
		if len(tail) == 0 {
			return nil, ErrMissingTarget
		}
		if strings.HasPrefix(tail[0], "$") {
			return nil, fmt.Errorf("%w: `%s` (admin revoke takes a `q_<id>` qurl_id, not an `$alias`)", ErrUnexpectedArgument, truncateForError(tail[0]))
		}
		if !qurlIDPattern.MatchString(tail[0]) {
			return nil, fmt.Errorf("%w: `%s` (expected `q_<id>`)", ErrInvalidQURLID, truncateForError(tail[0]))
		}
		cmd.Target = tail[0]
		if len(tail) > 1 {
			return nil, fmt.Errorf("%w: `%s`", ErrUnexpectedArgument, truncateForError(tail[1]))
		}
		return cmd, nil
	case AdminAdd, AdminRemove:
		// `admin add @user` / `admin remove @user` — promote or demote
		// a Slack user on the bot's admin set. The argument arrives as
		// Slack's encoded mention syntax (`<@U12345>` or
		// `<@U12345|name>`); [userMentionPattern] strips the sigil
		// wrapper and yields the bare user ID for the handler.
		if len(tail) == 0 {
			return nil, ErrMissingUserMention
		}
		uid, ok := matchUserMention(tail[0])
		if !ok {
			return nil, fmt.Errorf("%w: `%s` (expected a Slack @user mention like `<@U12345>`)", ErrInvalidUserMention, truncateForError(tail[0]))
		}
		cmd.UserID = uid
		if len(tail) > 1 {
			return nil, fmt.Errorf("%w: `%s`", ErrUnexpectedArgument, truncateForError(tail[1]))
		}
		return cmd, nil
	case AdminList:
		if len(tail) > 0 {
			return nil, fmt.Errorf("%w: `%s`", ErrUnexpectedArgument, truncateForError(tail[0]))
		}
		return cmd, nil
	default:
		return nil, fmt.Errorf("%w: `%s`", ErrUnknownAdminAction, truncateForError(verb))
	}
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

// requireResourceToken enforces the `$` sigil, strips it, and matches
// the remaining text against EITHER [resourceIDPattern] (a raw `r_…`
// resource ID) OR [aliasCharsetPattern] (a human-readable alias).
// Returns a [ParsedResourceToken] tagged with the kind so handlers
// can route to the right lookup path without re-running the regex.
//
// The resource-ID pattern is checked first so a literal `r_<11chars>`
// token always wins the disambiguation. Empty after the sigil is an
// [ErrEmptyResource]; anything matching neither pattern is
// [ErrInvalidAlias] with a message naming both accepted shapes.
//
// Used by verbs that accept both alias and resource-ID forms
// (currently `/qurl get`). Alias-only verbs (`setalias`, `unsetalias`)
// use [parseAliasToken] instead.
func requireResourceToken(tok string) (ParsedResourceToken, error) {
	if !strings.HasPrefix(tok, "$") {
		return ParsedResourceToken{}, fmt.Errorf("%w: got `%s`", ErrMissingSigil, truncateForError(tok))
	}
	bare := strings.TrimPrefix(tok, "$")
	if bare == "" {
		return ParsedResourceToken{}, ErrEmptyResource
	}
	if resourceIDPattern.MatchString(bare) {
		return ParsedResourceToken{Kind: ResourceTokenResourceID, Value: bare}, nil
	}
	// Length cap precedes the charset check on the alias branch so an
	// over-cap token (>aliasMaxLen runes) reports the cap-specific
	// error rather than the joint charset-or-resource-id message, which
	// would be confusing for an otherwise-valid-shape-but-too-long
	// alias. Mirrors parseAliasToken's order; pinned by parser_test.go's
	// `alias over 64 chars rejected` case.
	if len(bare) > aliasMaxLen {
		return ParsedResourceToken{}, fmt.Errorf("%w: `%s` is longer than %d characters", ErrInvalidAlias, truncateForError(bare), aliasMaxLen)
	}
	if aliasCharsetPattern.MatchString(bare) {
		return ParsedResourceToken{Kind: ResourceTokenAlias, Value: bare}, nil
	}
	return ParsedResourceToken{}, fmt.Errorf(
		"%w: `%s` — token must be an alias (e.g. `$dev-dashboard`, lowercase a-z/0-9/hyphen, no leading/trailing hyphen) or a resource ID (e.g. `$r_abc123def01`)",
		ErrInvalidAlias, truncateForError(bare),
	)
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
// the recognized keys (`dm`, `once`, `reason`) survive — unknown keys
// are an error so a typo doesn't silently no-op. The key half is
// case-folded so `DM:true` and `Reason:foo` work the way users on
// mobile clients (with auto-capitalize) expect; the value half is
// preserved verbatim because reasons are user-facing prose. An
// empty value (either `key:` or `key:""`) is rejected — the handler
// in PR-3c.3+ should be able to distinguish "flag unset" from "flag
// set to empty" by absence in [Command.Flags] alone.
//
// Per-key validation is strict-posture: `dm` and `once` accept only
// the boolean strings `true` / `false` (case-folded), so a user
// typing `dm:yes` or `once:1` sees a friendly error rather than the
// silent-falsey behavior the unvalidated form would have produced
// ([Command.DM] / [Command.Once] case-equal against "true", so any
// non-"true" value silently returns false — exactly the typo class
// the rest of the parser rejects). `reason` accepts any non-empty
// prose because the handler in PR-3c.3+ uses it for audit text where
// the user's exact wording is the point.
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
	case flagKeyOnce:
		// Strict-boolean gate — same shape as `dm` above.
		if !strings.EqualFold(val, "true") && !strings.EqualFold(val, "false") {
			return fmt.Errorf("%w: once:%q (use once:true or omit the flag)", ErrInvalidFlag, val)
		}
		cmd.Flags[key] = val
		return nil
	case "reason":
		cmd.Flags[key] = val
		return nil
	default:
		return fmt.Errorf("%w: unknown flag %q", ErrInvalidFlag, key)
	}
}
