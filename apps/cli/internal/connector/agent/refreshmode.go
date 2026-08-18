package agent

import (
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

// RefreshMode resolves the operator's refresh-mode gate. Empty defaults to
// manual; anything but manual/auto/disabled is rejected.
func RefreshMode() (string, error) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv(EnvRefreshMode)))
	if mode == "" {
		mode = RefreshModeManual
	}
	switch mode {
	case RefreshModeManual, RefreshModeAuto, RefreshModeDisabled:
		return mode, nil
	default:
		return "", fmt.Errorf("%s must be manual, auto, or disabled; got %q", EnvRefreshMode, mode)
	}
}
