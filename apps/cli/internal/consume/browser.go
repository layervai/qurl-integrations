package consume

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Browser launching. The command is a seam twice over: the argv comes from
// an environment override before the platform default, and the exec itself
// is injectable so tests can assert "a browser was (never) launched" without
// one ever starting.

// Environment variables consulted for the browser command, most specific
// first. The value is a command plus space-separated arguments (no shell
// quoting); the link is appended as the final argument.
const (
	// EnvBrowser is the qURL-specific override.
	EnvBrowser = "QURL_BROWSER"
	// EnvBrowserGeneric is the conventional cross-tool override.
	EnvBrowserGeneric = "BROWSER"
)

// BrowserCommand resolves the launcher argv: QURL_BROWSER, then BROWSER,
// then the platform default for goos (`open` on macOS, the URL handler on
// Windows, `xdg-open` elsewhere). The returned slice never includes the
// link; Launcher.Open appends it.
func BrowserCommand(lookupEnv func(string) (string, bool), goos string) []string {
	if lookupEnv != nil {
		for _, key := range []string{EnvBrowser, EnvBrowserGeneric} {
			if v, ok := lookupEnv(key); ok && strings.TrimSpace(v) != "" {
				return strings.Fields(v)
			}
		}
	}
	switch goos {
	case "darwin":
		return []string{"open"}
	case "windows":
		return []string{"rundll32", "url.dll,FileProtocolHandler"}
	default:
		return []string{"xdg-open"}
	}
}

// Launcher opens links in the user's browser.
type Launcher struct {
	// LookupEnv resolves the override variables; nil skips them.
	LookupEnv func(string) (string, bool)
	// GOOS picks the platform default command.
	GOOS string
	// Run executes one resolved argv; nil means a real subprocess.
	Run func(ctx context.Context, argv []string) error
}

// Open launches the browser at link. The caller is responsible for only
// passing links that already passed CRID verification.
func (l *Launcher) Open(ctx context.Context, link string) error {
	argv := append(BrowserCommand(l.LookupEnv, l.GOOS), link)
	run := l.Run
	if run == nil {
		run = runBrowserCommand
	}
	if err := run(ctx, argv); err != nil {
		return fmt.Errorf("%s: %w", argv[0], err)
	}
	return nil
}

// runBrowserCommand is the real exec: platform launchers (`open`,
// `xdg-open`, rundll32) hand the URL off and exit immediately, so waiting is
// cheap and surfaces their failure status. Launcher output is discarded —
// stderr belongs to the CLI's own messages.
func runBrowserCommand(ctx context.Context, argv []string) error {
	// #nosec G204 -- argv is the platform's URL launcher or the user's own
	// QURL_BROWSER/BROWSER override; launching the user's chosen browser
	// with the verified link is this function's entire purpose.
	return exec.CommandContext(ctx, argv[0], argv[1:]...).Run()
}
