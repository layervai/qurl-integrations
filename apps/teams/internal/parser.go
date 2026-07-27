package internal

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	verbHelp             = "help"
	verbSetup            = "setup"
	verbGet              = "get"
	verbList             = "list"
	verbAliases          = "aliases"
	verbProtectURL       = "protect-url"
	verbProtectConnector = "protect-connector"
	verbSetAlias         = "set-alias"
	verbUnsetAlias       = "unset-alias"
	verbSetDisplayName   = "set-display-name"
	verbUnsetDisplayName = "unset-display-name"
	verbAdd              = "add"
	verbRemove           = "remove"
	verbAdmins           = "admins"
	verbRevoke           = "revoke"
	verbUninstall        = "uninstall"
	verbFeedback         = "feedback"
)

var (
	errUnknownCommand  = errors.New("unknown command")
	errMissingArgument = errors.New("missing required argument")
	errUnexpectedArg   = errors.New("unexpected argument")
	errInvalidAlias    = errors.New("invalid alias")
	errInvalidMention  = errors.New("invalid user mention")
)

var (
	aliasPattern        = regexp.MustCompile(`^[a-z](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	mentionTokenPattern = regexp.MustCompile(`^<@([A-Za-z0-9._:-]{1,200})>$`)
	flagPattern         = regexp.MustCompile(`^([a-z][a-z0-9_]*):(?:"([^"]*)"|(\S+))$`)
)

// SetupMode selects how Teams setup should handle an existing workspace key.
type SetupMode string

const (
	// SetupMode values accepted by the Teams setup command.
	SetupModeReuse   SetupMode = "reuse"
	SetupModeRotate  SetupMode = "rotate"
	SetupModeRepoint SetupMode = "repoint"
)

// Command is the parsed Teams bot command.
type Command struct {
	Raw            string
	Verb           string
	AdminRequested bool
	Resource       string
	Alias          string
	Target         string
	UserID         string
	Email          string
	SetupMode      SetupMode
	Text           string
	Flags          map[string]string
	Args           []string
}

// ParseCommand parses Teams bot text into a structured command.
func ParseCommand(text string) (*Command, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return &Command{Verb: verbHelp, Flags: map[string]string{}}, nil
	}
	tokens := tokenize(text)
	if len(tokens) == 0 {
		return &Command{Verb: verbHelp, Flags: map[string]string{}}, nil
	}
	cmd := &Command{Raw: text, Flags: map[string]string{}}
	switch strings.ToLower(tokens[0]) {
	case "qurl-admin", "/qurl-admin":
		cmd.AdminRequested = true
		tokens = tokens[1:]
	case "qurl", "/qurl":
		tokens = tokens[1:]
	}
	if len(tokens) == 0 {
		cmd.Verb = verbHelp
		return cmd, nil
	}
	cmd.Verb = strings.ToLower(tokens[0])
	cmd.Args = append([]string(nil), tokens[1:]...)
	switch cmd.Verb {
	case verbHelp:
		if len(cmd.Args) > 0 {
			return nil, fmt.Errorf("%w: %q", errUnexpectedArg, cmd.Args[0])
		}
	case verbList, verbAliases, verbAdmins, verbUninstall:
		if len(cmd.Args) > 0 {
			return nil, fmt.Errorf("%w: %q", errUnexpectedArg, cmd.Args[0])
		}
	case verbSetup:
		return parseSetup(cmd)
	case verbGet:
		return parseGet(cmd)
	case verbProtectURL, verbProtectConnector:
		if len(cmd.Args) == 0 {
			return cmd, nil
		}
	case verbSetAlias:
		return parseSetAlias(cmd)
	case verbUnsetAlias, verbRevoke, verbUnsetDisplayName:
		return parseSingleResource(cmd)
	case verbSetDisplayName:
		return parseSetDisplayName(cmd)
	case verbAdd, verbRemove:
		return parseMembership(cmd)
	case verbFeedback:
		cmd.Text = strings.TrimSpace(strings.Join(cmd.Args, " "))
		if cmd.Text == "" {
			return nil, fmt.Errorf("%w: feedback message", errMissingArgument)
		}
	default:
		return nil, fmt.Errorf("%w: %q", errUnknownCommand, tokens[0])
	}
	return cmd, nil
}

func parseSetup(cmd *Command) (*Command, error) {
	if len(cmd.Args) == 0 {
		return nil, fmt.Errorf("%w: email", errMissingArgument)
	}
	cmd.Email = strings.TrimSpace(cmd.Args[0])
	cmd.SetupMode = SetupModeReuse
	for _, tok := range cmd.Args[1:] {
		switch strings.ToLower(strings.TrimSpace(tok)) {
		case "--rotate":
			if cmd.SetupMode != SetupModeReuse {
				return nil, fmt.Errorf("%w: %q", errUnexpectedArg, tok)
			}
			cmd.SetupMode = SetupModeRotate
		case "--repoint":
			if cmd.SetupMode != SetupModeReuse {
				return nil, fmt.Errorf("%w: %q", errUnexpectedArg, tok)
			}
			cmd.SetupMode = SetupModeRepoint
		default:
			return nil, fmt.Errorf("%w: %q", errUnexpectedArg, tok)
		}
	}
	return cmd, nil
}

func parseGet(cmd *Command) (*Command, error) {
	if len(cmd.Args) == 0 {
		return nil, fmt.Errorf("%w: resource token", errMissingArgument)
	}
	token, err := parseLookupToken(cmd.Args[0])
	if err != nil {
		return nil, err
	}
	cmd.Resource = token
	for _, tok := range cmd.Args[1:] {
		key, value, ok := parseFlag(tok)
		if !ok {
			return nil, fmt.Errorf("%w: %q", errUnexpectedArg, tok)
		}
		cmd.Flags[key] = value
	}
	return cmd, nil
}

func parseSetAlias(cmd *Command) (*Command, error) {
	if len(cmd.Args) < 2 {
		return nil, fmt.Errorf("%w: alias and target", errMissingArgument)
	}
	alias, err := parseAliasToken(cmd.Args[0])
	if err != nil {
		return nil, err
	}
	target, err := parseLookupToken(cmd.Args[1])
	if err != nil {
		return nil, err
	}
	if len(cmd.Args) > 2 {
		return nil, fmt.Errorf("%w: %q", errUnexpectedArg, cmd.Args[2])
	}
	cmd.Alias = alias
	cmd.Target = target
	return cmd, nil
}

func parseSingleResource(cmd *Command) (*Command, error) {
	if len(cmd.Args) == 0 {
		return nil, fmt.Errorf("%w: resource token", errMissingArgument)
	}
	token, err := parseLookupToken(cmd.Args[0])
	if err != nil {
		return nil, err
	}
	if len(cmd.Args) > 1 {
		return nil, fmt.Errorf("%w: %q", errUnexpectedArg, cmd.Args[1])
	}
	cmd.Resource = token
	return cmd, nil
}

func parseSetDisplayName(cmd *Command) (*Command, error) {
	if len(cmd.Args) < 2 {
		return nil, fmt.Errorf("%w: resource token and display name", errMissingArgument)
	}
	token, err := parseLookupToken(cmd.Args[0])
	if err != nil {
		return nil, err
	}
	cmd.Resource = token
	cmd.Text = strings.TrimSpace(strings.Join(cmd.Args[1:], " "))
	if cmd.Text == "" {
		return nil, fmt.Errorf("%w: display name", errMissingArgument)
	}
	return cmd, nil
}

func parseMembership(cmd *Command) (*Command, error) {
	if len(cmd.Args) == 0 {
		return nil, fmt.Errorf("%w: @user", errMissingArgument)
	}
	matches := mentionTokenPattern.FindStringSubmatch(strings.TrimSpace(cmd.Args[0]))
	if len(matches) != 2 {
		return nil, fmt.Errorf("%w: %q", errInvalidMention, cmd.Args[0])
	}
	cmd.UserID = matches[1]
	if len(cmd.Args) > 1 {
		return nil, fmt.Errorf("%w: %q", errUnexpectedArg, cmd.Args[1])
	}
	return cmd, nil
}

func parseLookupToken(tok string) (string, error) {
	tok = strings.TrimSpace(tok)
	tok = strings.TrimPrefix(tok, "$")
	if tok == "" {
		return "", fmt.Errorf("%w: resource token", errMissingArgument)
	}
	return tok, nil
}

func parseAliasToken(tok string) (string, error) {
	tok = strings.TrimSpace(strings.TrimPrefix(tok, "$"))
	if !aliasPattern.MatchString(tok) {
		return "", fmt.Errorf("%w: %q", errInvalidAlias, tok)
	}
	return tok, nil
}

func parseFlag(tok string) (key, value string, ok bool) {
	matches := flagPattern.FindStringSubmatch(tok)
	if len(matches) != 4 {
		return "", "", false
	}
	key = strings.ToLower(strings.TrimSpace(matches[1]))
	switch {
	case matches[2] != "":
		value = matches[2]
	default:
		value = matches[3]
	}
	if value == "" {
		return "", "", false
	}
	return key, value, true
}

func tokenize(text string) []string {
	var out []string
	var cur strings.Builder
	inQuotes := false
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		s := cur.String()
		if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
			s = s[1 : len(s)-1]
		}
		if s != "" {
			out = append(out, s)
		}
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
