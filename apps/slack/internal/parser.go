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
	// SubcmdGet mints an access link for an alias-bound resource.
	SubcmdGet Subcommand = "get"
	// SubcmdSetAlias binds an alias to a target URL or resource ID.
	SubcmdSetAlias Subcommand = "setalias"
	// SubcmdUnsetAlias clears the alias on the resource it points at.
	SubcmdUnsetAlias Subcommand = "unsetalias"
	// SubcmdAliases lists owner-scoped aliases visible in the channel.
	SubcmdAliases Subcommand = "aliases"
	// SubcmdAdmin is the umbrella for admin-only operations; see
	// [Command.AdminAction] for the specific verb.
	SubcmdAdmin Subcommand = "admin"
	// SubcmdCreate is the legacy free-form-URL mint (pre-alias world).
	SubcmdCreate Subcommand = "create"
	// SubcmdList is the legacy listing of recent qURLs.
	SubcmdList Subcommand = "list"
)

// AdminAction is the second word after `admin` (e.g. `admin claim`).
type AdminAction string

// Recognized admin actions.
const (
	// AdminClaim opens the bootstrap-code modal. The code is NEVER passed
	// as slash-command text — it would land in Slack's audit log in
	// plaintext. The argument-less form opens a `views.open` modal
	// (see [AdminClaimModal]) whose `plain_text_input` block accepts
	// the code; the bot's logging middleware redacts the block on
	// submission so it never reaches diagnostics either.
	AdminClaim AdminAction = "claim"
	// AdminAllow whitelists an alias for use in a specific channel.
	AdminAllow AdminAction = "allow"
	// AdminDisallow removes an alias from a channel's allowed set.
	AdminDisallow AdminAction = "disallow"
	// AdminPolicies lists channel/alias policy rows.
	AdminPolicies AdminAction = "policies"
	// AdminStatus reports per-workspace bot health/admin info.
	AdminStatus AdminAction = "status"
	// AdminRevoke revokes a previously minted access link by alias.
	AdminRevoke AdminAction = "revoke"
)

// Command is the parsed shape of a `/qurl …` slash command.
type Command struct {
	// Subcommand is the first word (or [SubcmdHelp] when text is empty).
	Subcommand Subcommand
	// AdminAction is the second word when [Subcommand] is [SubcmdAdmin];
	// empty otherwise.
	AdminAction AdminAction
	// Alias is the `$alias` argument (sigil stripped) when present.
	Alias string
	// Target is the trailing positional arg used by `setalias` (a URL or
	// raw resource_id) and the legacy `create <url>`.
	Target string
	// ChannelID is the parsed channel from `<#C12345|name>` form when
	// present (used by `admin allow` / `admin disallow`).
	ChannelID string
	// Flags holds optional `key:value` flags. Only `dm` and `reason` are
	// recognized today (on `get`).
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

// ErrUnknownAdminAction is returned for any `admin <verb>` where verb is
// not in the recognized [AdminAction] set.
var ErrUnknownAdminAction = errors.New("unknown admin action")

// ErrMissingChannel is returned when `admin allow` / `admin disallow` are
// invoked without a `<#C…|…>` channel reference.
var ErrMissingChannel = errors.New("missing #channel argument")

// ErrMissingTarget is returned when `setalias` or `create` are invoked
// without the trailing target/URL argument.
var ErrMissingTarget = errors.New("missing target argument")

// ErrInvalidAlias is returned when an alias name (the part after `$`)
// contains characters outside the recognized set. Catching this in
// the parser surfaces a friendly slash-command error instead of
// punting an obviously-bogus alias to qurl-service.
var ErrInvalidAlias = errors.New("invalid alias")

// ErrUnexpectedArgument is returned when a verb that takes no
// positional arguments receives one (e.g. `admin policies extra`).
// Catches user-facing typos earlier than the handler dispatch.
var ErrUnexpectedArgument = errors.New("unexpected argument")

// channelRefPattern matches Slack's encoded channel-mention form
// `<#C12345|channel-name>`. The trailing `|name` is optional (Slack's UI
// always includes it but the wire shape allows omission).
var channelRefPattern = regexp.MustCompile(`^<#([A-Z0-9]+)(?:\|[^>]*)?>$`)

// flagPattern matches `key:value` and `key:"quoted value"` shapes.
//
// The pattern is intentionally permissive on the value side — Slack's
// slash-command form-encoding has already done one round of decoding by
// the time we see the body, so we don't try to handle further escapes
// here. Quoted values let users put spaces in `reason:"…"`.
var flagPattern = regexp.MustCompile(`^([a-z][a-z0-9_]*):(?:"([^"]*)"|(\S+))$`)

// aliasCharsetPattern is the alias-name shape qurl-service accepts:
// lowercase alphanumeric with hyphens, no leading or trailing hyphen.
// Surfacing the rejection here gives a friendlier slash-command error
// than punting an obviously-bogus alias all the way to the API.
//
// The pattern is anchored both ends and uses a non-capturing group
// to require a trailing alnum: `[a-z0-9]` (start) then optionally
// `[a-z0-9-]*[a-z0-9]` (middle plus required trailing alnum). A
// single-character alias (`$a`, `$1`) is allowed by the optional
// non-capturing group.
//
// Internal `--` runs (e.g. `foo--bar`) are intentionally permitted —
// qurl-service's own alias validator is the authoritative gate and
// accepts them. The parser only enforces the leading/trailing rule
// because that's what surfaces with a friendlier error than punting
// to the service.
var aliasCharsetPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

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
		return cmd, nil
	case SubcmdAdmin:
		cmd.Subcommand = SubcmdAdmin
		return parseAdmin(cmd, rest)
	case SubcmdCreate:
		cmd.Subcommand = SubcmdCreate
		return parseCreate(cmd, rest)
	case SubcmdList:
		cmd.Subcommand = SubcmdList
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

// parseGet extracts the `$alias` argument and the optional `dm:` /
// `reason:` flags. Surplus positional args after the alias are an error.
func parseGet(cmd *Command, rest []string) (*Command, error) {
	if len(rest) == 0 {
		return nil, ErrEmptyResource
	}
	alias, err := requireAlias(rest[0])
	if err != nil {
		return nil, err
	}
	cmd.Alias = alias
	for _, tok := range rest[1:] {
		if err := applyFlag(cmd, tok); err != nil {
			return nil, err
		}
	}
	return cmd, nil
}

// parseSetAlias extracts `$alias <target>`. Strict-posture like the
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
	alias, err := requireAlias(rest[0])
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
	alias, err := requireAlias(rest[0])
	if err != nil {
		return nil, err
	}
	cmd.Alias = alias
	return cmd, nil
}

// parseAdmin dispatches on the second word (the [AdminAction]). `claim`
// takes no positional args (the code is collected via modal — see
// Blocker #3 in the plan and [AdminClaimModal]); `allow`/`disallow`
// take a `#channel` and a `$alias`; `policies`/`status` take no
// args; `revoke` takes a `$alias`. Verbs that take no args fail
// [ErrUnexpectedArgument] when given any — surfacing a typo like
// `admin policies extra` early instead of silently routing it.
func parseAdmin(cmd *Command, rest []string) (*Command, error) {
	if len(rest) == 0 {
		return nil, fmt.Errorf("%w: admin requires a verb", ErrUnknownAdminAction)
	}
	verb := rest[0]
	action := AdminAction(strings.ToLower(verb))
	cmd.AdminAction = action
	tail := rest[1:]
	switch action {
	case AdminClaim:
		// Code never appears as text — modal-only flow.
		if len(tail) > 0 {
			return nil, fmt.Errorf("%w: %q (use the modal to enter the code)", ErrUnexpectedArgument, tail[0])
		}
		return cmd, nil
	case AdminAllow, AdminDisallow:
		return parseAdminChannelAlias(cmd, tail)
	case AdminPolicies, AdminStatus:
		if len(tail) > 0 {
			return nil, fmt.Errorf("%w: %q", ErrUnexpectedArgument, tail[0])
		}
		return cmd, nil
	case AdminRevoke:
		if len(tail) == 0 {
			return nil, ErrEmptyResource
		}
		alias, err := requireAlias(tail[0])
		if err != nil {
			return nil, err
		}
		cmd.Alias = alias
		if len(tail) > 1 {
			return nil, fmt.Errorf("%w: %q", ErrUnexpectedArgument, tail[1])
		}
		return cmd, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownAdminAction, verb)
	}
}

// parseAdminChannelAlias extracts `<#channel|name> $alias` in either order
// (Slack's autocomplete sometimes interleaves them). Both must be
// present, and each may appear at most once — duplicate channels or
// aliases are an [ErrUnexpectedArgument] (consistent with the strict
// posture parseAdmin takes for verbs like `admin policies`). Once
// both slots are filled, any further positional surfaces as
// [ErrUnexpectedArgument] too — `admin allow <#C1|a> $alias junk`
// is a typo, not a missing-sigil error.
func parseAdminChannelAlias(cmd *Command, rest []string) (*Command, error) {
	for _, tok := range rest {
		if cmd.ChannelID != "" && cmd.Alias != "" {
			// Both slots taken — any further token is a typo, surface
			// the strict-posture sentinel instead of routing through
			// the channel/alias dispatchers (which would say
			// "duplicate" or "missing sigil" — neither is right here).
			return nil, fmt.Errorf("%w: %q", ErrUnexpectedArgument, tok)
		}
		if id, ok := matchChannel(tok); ok {
			if cmd.ChannelID != "" {
				return nil, fmt.Errorf("%w: duplicate #channel %q", ErrUnexpectedArgument, tok)
			}
			cmd.ChannelID = id
			continue
		}
		if strings.HasPrefix(tok, "$") {
			alias, err := requireAlias(tok)
			if err != nil {
				return nil, err
			}
			if cmd.Alias != "" {
				return nil, fmt.Errorf("%w: duplicate $alias %q", ErrUnexpectedArgument, tok)
			}
			cmd.Alias = alias
			continue
		}
		// Unrecognized positional — surface as missing-sigil for the
		// most common mistake (forgetting the $).
		return nil, fmt.Errorf("%w: %q", ErrMissingSigil, tok)
	}
	if cmd.ChannelID == "" {
		return nil, ErrMissingChannel
	}
	if cmd.Alias == "" {
		return nil, ErrEmptyResource
	}
	return cmd, nil
}

// parseCreate keeps the legacy free-form-URL grammar working through
// PR-3c.3 (the cutover to alias-only mints). Strict-posture like
// every other verb: exactly one target token. URLs containing spaces
// must be quoted so [tokenize] keeps them as one token.
func parseCreate(cmd *Command, rest []string) (*Command, error) {
	if len(rest) == 0 {
		return nil, ErrMissingTarget
	}
	if len(rest) > 1 {
		return nil, fmt.Errorf("%w: %q (quote the target if it contains spaces)", ErrUnexpectedArgument, rest[1])
	}
	cmd.Target = rest[0]
	return cmd, nil
}

// requireAlias enforces the `$` sigil, strips it, and validates the
// remaining alias against [aliasCharsetPattern]. Empty after the
// sigil (`$`) is treated as an empty-resource error; out-of-charset
// runs as an invalid-alias error.
func requireAlias(tok string) (string, error) {
	if !strings.HasPrefix(tok, "$") {
		return "", fmt.Errorf("%w: got %q", ErrMissingSigil, tok)
	}
	alias := strings.TrimPrefix(tok, "$")
	if alias == "" {
		return "", ErrEmptyResource
	}
	if !aliasCharsetPattern.MatchString(alias) {
		return "", fmt.Errorf("%w: %q (allowed: lowercase a-z, 0-9, hyphen, no leading/trailing hyphen)", ErrInvalidAlias, alias)
	}
	return alias, nil
}

// matchChannel returns the `C…` ID and true when `tok` is a Slack
// channel-mention encoded form, else returns ("", false).
func matchChannel(tok string) (string, bool) {
	m := channelRefPattern.FindStringSubmatch(tok)
	if len(m) < 2 {
		return "", false
	}
	return m[1], true
}

// applyFlag parses a single `key:value` token into [Command.Flags]. Only
// the recognized keys (`dm`, `reason`) survive — unknown keys are an
// error so a typo doesn't silently no-op. The key half is
// case-folded so `DM:true` and `Reason:foo` work the way users on
// mobile clients (with auto-capitalize) expect; the value half is
// preserved verbatim because reasons are user-facing prose. An
// empty value (either `key:` or `key:""`) is rejected — the handler
// in PR-3c.3+ should be able to distinguish "flag unset" from "flag
// set to empty" by absence in [Command.Flags] alone.
func applyFlag(cmd *Command, tok string) error {
	colonIdx := strings.IndexByte(tok, ':')
	if colonIdx < 0 {
		return fmt.Errorf("invalid flag: %q (expected key:value)", tok)
	}
	if colonIdx == 0 {
		return fmt.Errorf("invalid flag: %q (missing key before colon)", tok)
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
		return fmt.Errorf("invalid flag: %q (empty value — use a non-empty value or omit the flag)", tok)
	}
	m := flagPattern.FindStringSubmatch(normalized)
	if len(m) == 0 {
		return fmt.Errorf("invalid flag: %q (expected key:value)", tok)
	}
	key := m[1]
	val := m[2]
	if val == "" {
		val = m[3]
	}
	if val == "" {
		return fmt.Errorf("invalid flag: %q (empty value — use a non-empty value or omit the flag)", tok)
	}
	switch key {
	case "dm", "reason":
		cmd.Flags[key] = val
		return nil
	default:
		return fmt.Errorf("unknown flag: %q", key)
	}
}
