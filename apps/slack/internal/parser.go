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
	// plaintext. The argument-less form opens a `views.open` modal whose
	// `private_value` field accepts the code.
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

// ErrEmptyResource is returned when a subcommand expects a `$alias` argument
// and the user omitted it entirely.
var ErrEmptyResource = errors.New("missing $alias argument")

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

	cmd := &Command{Raw: text, Flags: map[string]string{}}
	first := strings.ToLower(tokens[0])
	rest := tokens[1:]

	switch Subcommand(first) {
	case SubcmdHelp:
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
func tokenize(text string) []string {
	var out []string
	var cur strings.Builder
	inQuotes := false
	for _, r := range text {
		switch {
		case r == '"':
			cur.WriteRune(r)
			inQuotes = !inQuotes
		case (r == ' ' || r == '\t') && !inQuotes:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
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

// parseSetAlias extracts `$alias <target>`. The target is everything after
// the alias glued back together, so URLs with spaces (rare but legal in
// some forms) survive.
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
	cmd.Target = strings.Join(rest[1:], " ")
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
// Blocker #3 in the plan); `allow`/`disallow` take a `#channel` and a
// `$alias`; `policies`/`status`/`revoke` are intentionally permissive
// here so that PR-3c.5 can refine them without churning grammar tests.
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
		return cmd, nil
	case AdminAllow, AdminDisallow:
		return parseAdminChannelAlias(cmd, tail)
	case AdminPolicies, AdminStatus:
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
		return cmd, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownAdminAction, verb)
	}
}

// parseAdminChannelAlias extracts `<#channel|name> $alias` in either order
// (Slack's autocomplete sometimes interleaves them). Both must be present.
func parseAdminChannelAlias(cmd *Command, rest []string) (*Command, error) {
	for _, tok := range rest {
		if id, ok := matchChannel(tok); ok {
			cmd.ChannelID = id
			continue
		}
		if strings.HasPrefix(tok, "$") {
			alias, err := requireAlias(tok)
			if err != nil {
				return nil, err
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
// PR-3c.3 (the cutover to alias-only mints). The whole tail is treated
// as the target.
func parseCreate(cmd *Command, rest []string) (*Command, error) {
	if len(rest) == 0 {
		return nil, ErrMissingTarget
	}
	cmd.Target = strings.Join(rest, " ")
	return cmd, nil
}

// requireAlias enforces the `$` sigil and strips it. Empty after the
// sigil (`$`) is treated as an empty-resource error.
func requireAlias(tok string) (string, error) {
	if !strings.HasPrefix(tok, "$") {
		return "", fmt.Errorf("%w: got %q", ErrMissingSigil, tok)
	}
	alias := strings.TrimPrefix(tok, "$")
	if alias == "" {
		return "", ErrEmptyResource
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
// error so a typo doesn't silently no-op.
func applyFlag(cmd *Command, tok string) error {
	m := flagPattern.FindStringSubmatch(tok)
	if len(m) == 0 {
		return fmt.Errorf("invalid flag: %q (expected key:value)", tok)
	}
	key := m[1]
	val := m[2]
	if val == "" {
		val = m[3]
	}
	switch key {
	case "dm", "reason":
		cmd.Flags[key] = val
		return nil
	default:
		return fmt.Errorf("unknown flag: %q", key)
	}
}
