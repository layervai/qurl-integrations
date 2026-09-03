package daemon

import (
	"fmt"
	"strings"
)

// GroupMode selects how the daemon maps its desired-on local shares onto
// Connector session groups.
type GroupMode string

const (
	// GroupModeSingle serves every desired-on share as one route on one session
	// group, so the whole set costs one admission, one login, and one heartbeat
	// stream. It is the default and the target model.
	GroupModeSingle GroupMode = "single"
	// GroupModePerShare serves each desired-on share on a session group of its
	// own: one admission and one login per share, rotated per share. It is the
	// compatibility mode for a platform that admits a session's proxies only for
	// the one resource that session was signed for.
	GroupModePerShare GroupMode = "per-share"
)

// DefaultGroupMode is the mode a daemon runs in when nothing selects one.
const DefaultGroupMode = GroupModeSingle

// GroupModeEnv is the environment variable that selects the mode. The flag
// (`qurl daemon run --share-group-mode`) and the config key (`share_group_mode`)
// resolve through the CLI's ordinary settings precedence.
const GroupModeEnv = "QURL_SHARE_GROUP_MODE"

// GroupModeValues lists every accepted mode, default first.
func GroupModeValues() []string {
	return []string{string(GroupModeSingle), string(GroupModePerShare)}
}

// ParseGroupMode validates an operator-supplied mode. Only the canonical
// spellings are accepted: the mode is written into the durable job definition
// and folded into the job version, so it is never normalized silently.
func ParseGroupMode(value string) (GroupMode, error) {
	switch mode := GroupMode(value); mode {
	case GroupModeSingle, GroupModePerShare:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid share group mode %q: must be %s", value, strings.Join(GroupModeValues(), " or "))
	}
}
