package oauth

import (
	"log/slog"
	"net/http"
	"regexp"
	"time"
)

// teamIDPattern is the wire shape Slack assigns to workspace IDs —
// uppercase letters and digits, leading 'T'. The 8+ length floor is
// looser than Slack's current 9 because the platform has historically
// extended ID widths; we don't want a hardcoded length to be the thing
// that breaks the bot.
var teamIDPattern = regexp.MustCompile(`^T[A-Z0-9]{8,}$`)

// Start returns the http.HandlerFunc for GET /oauth/qurl/start.
//
// Contract:
//   - Validates the `team` query param against teamIDPattern.
//   - Mints an HMAC-bound state token via mintState and sets it as the
//     double-submit cookie via setStateCookie.
//   - 302s to Auth0's /authorize.
//
//nolint:gocritic // hugeParam: see Callback — value-passed at startup.
func Start(cfg Config) http.HandlerFunc {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		teamID := r.URL.Query().Get("team")
		if !teamIDPattern.MatchString(teamID) {
			slog.Warn("oauth/start rejected invalid team", "reason", "team_format")
			http.Error(w, "invalid team parameter", http.StatusBadRequest)
			return
		}
		if len(cfg.OAuthStateSecret) == 0 {
			slog.Error("oauth/start refused: OAUTH_STATE_SECRET not configured")
			http.Error(w, "oauth not configured", http.StatusServiceUnavailable)
			return
		}
		state, err := mintState(cfg.OAuthStateSecret, teamID, now())
		if err != nil {
			slog.Error("oauth/start: mintState failed", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// Cookie MUST be set before the 302 — once we write the Location
		// header the response is committed and a deferred Set-Cookie is
		// silently dropped.
		setStateCookie(w, state)
		http.Redirect(w, r, authorizeURL(cfg, state), http.StatusFound)
	}
}
