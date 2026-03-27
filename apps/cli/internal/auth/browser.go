package auth

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
)

// OpenBrowser attempts to open the given URL in the user's default browser.
// Only HTTPS URLs are accepted to prevent command injection via malicious URIs.
// Returns an error if the URL is invalid or the browser could not be launched.
func OpenBrowser(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("refusing to open non-HTTPS URL: %s", u.Scheme)
	}

	validated := u.String()
	ctx := context.Background()

	switch runtime.GOOS {
	case "darwin":
		return exec.CommandContext(ctx, "open", validated).Start() //nolint:gosec // validated as HTTPS URL from Auth0 server response
	case "windows":
		return exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", validated).Start() //nolint:gosec // validated as HTTPS URL from Auth0 server response
	default:
		return exec.CommandContext(ctx, "xdg-open", validated).Start() //nolint:gosec // validated as HTTPS URL from Auth0 server response
	}
}
