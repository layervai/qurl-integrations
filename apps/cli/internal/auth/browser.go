package auth

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"runtime"
)

// OpenBrowser attempts to open the given URL in the user's default browser.
// HTTPS URLs and loopback HTTP URLs (http://127.0.0.1 and http://localhost)
// are accepted; all other schemes are rejected to prevent command injection.
// The loopback allowlist matches what QURL_AUTH0_URL permits, so local-dev
// flows don't need --no-browser.
// Returns an error if the URL is invalid or the browser could not be launched.
func OpenBrowser(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if !isAllowedBrowserURL(u) {
		return fmt.Errorf("refusing to open non-HTTPS URL: %s", u.Scheme)
	}

	validated := u.String()
	ctx := context.Background()

	switch runtime.GOOS {
	case "darwin":
		return exec.CommandContext(ctx, "open", validated).Start() //nolint:gosec // URL validated above (https or loopback http); exec uses argv (no shell) so no injection risk
	case "windows":
		return exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", validated).Start() //nolint:gosec // URL validated above (https or loopback http); exec uses argv (no shell) so no injection risk
	default:
		return exec.CommandContext(ctx, "xdg-open", validated).Start() //nolint:gosec // URL validated above (https or loopback http); exec uses argv (no shell) so no injection risk
	}
}

// isAllowedBrowserURL reports whether u is safe to open in a browser.
// Accepts https:// and loopback http:// (127.0.0.1 and localhost) only.
func isAllowedBrowserURL(u *url.URL) bool {
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme == "http" {
		host := u.Hostname()
		return host == "127.0.0.1" || host == "localhost"
	}
	return false
}
