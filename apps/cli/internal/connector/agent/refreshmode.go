package agent

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// EnvRefreshMode gates the one-per-episode native assignment refresh. The
// name is the qURL Connector operator contract shared with the standalone
// Connector.
const EnvRefreshMode = "LAYERV_AGENT_REGISTRATION_REFRESH_MODE"

// Refresh modes. Manual is the default: a required refresh stops with
// operator guidance instead of consuming the episode's one refresh silently,
// so an orchestrator crash-restart loop cannot spend refreshes.
const (
	RefreshModeManual   = "manual"
	RefreshModeAuto     = "auto"
	RefreshModeDisabled = "disabled"
)

// ErrRefreshModeInvalid rejects a refresh-mode spelling outside
// manual|auto|disabled. Exported into the CLI's exit-code contract for the
// ENVIRONMENT path only: the command layer validates its --refresh-mode flag
// itself (a flag typo is a usage error), so reaching this sentinel means the
// standing LAYERV_AGENT_REGISTRATION_REFRESH_MODE configuration is wrong.
var ErrRefreshModeInvalid = errors.New("registration refresh mode must be manual, auto, or disabled")

// ResolveRefreshMode resolves the operator's refresh-mode gate, flag-first:
// a non-empty explicit value (the command's --refresh-mode flag) wins, then
// the LAYERV_AGENT_REGISTRATION_REFRESH_MODE environment contract, then the
// manual default. Case-insensitive; anything but manual/auto/disabled is
// rejected.
func ResolveRefreshMode(explicit string) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(explicit))
	source := "--refresh-mode"
	if mode == "" {
		mode = strings.ToLower(strings.TrimSpace(os.Getenv(EnvRefreshMode)))
		source = EnvRefreshMode
	}
	if mode == "" {
		mode = RefreshModeManual
	}
	switch mode {
	case RefreshModeManual, RefreshModeAuto, RefreshModeDisabled:
		return mode, nil
	default:
		return "", fmt.Errorf("%w; %s got %q", ErrRefreshModeInvalid, source, mode)
	}
}

// RefreshMode resolves the gate from the environment alone; kept for callers
// with no explicit override.
func RefreshMode() (string, error) {
	return ResolveRefreshMode("")
}
