package auth

import (
	"context"
	"os/exec"
	"runtime"
)

// OpenBrowser attempts to open the given URL in the user's default browser.
// Returns an error if the browser could not be launched.
func OpenBrowser(rawURL string) error {
	ctx := context.Background()
	switch runtime.GOOS {
	case "darwin":
		return exec.CommandContext(ctx, "open", rawURL).Start() //nolint:gosec // rawURL is an Auth0 verification URI, not user-controlled shell input
	case "windows":
		return exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", rawURL).Start() //nolint:gosec // rawURL is an Auth0 verification URI
	default:
		return exec.CommandContext(ctx, "xdg-open", rawURL).Start() //nolint:gosec // rawURL is an Auth0 verification URI
	}
}
