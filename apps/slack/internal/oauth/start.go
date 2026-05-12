package oauth

import (
	"errors"
	"log/slog"
	"net/http"
)

// Start returns the http.HandlerFunc for GET /oauth/qurl/start.
//
// Contract:
//   - Requires `?state=<signed_state>` minted by the /qurl setup
//     slash-command handler (which is Slack-signature-verified).
//     Workspace identity comes from the verified HMAC payload — never
//     from an unsigned query param.
//   - Sets the state token as the double-submit cookie via setStateCookie.
//   - 302s to Auth0 /authorize.
//
//nolint:gocritic // hugeParam: see Callback — Config is value-passed at startup.
func Start(cfg Config) http.HandlerFunc {
	now := cfg.now()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if len(cfg.OAuthStateSecret) < StateMinSecret {
			slog.Error("oauth/start refused: OAUTH_STATE_SECRET unset or shorter than 32 bytes")
			http.Error(w, "oauth not configured", http.StatusServiceUnavailable)
			return
		}
		stateParam := r.URL.Query().Get("state")
		if stateParam == "" {
			slog.Warn("oauth/start rejected: missing state")
			http.Error(w, "missing state parameter — start setup from the /qurl setup slash command", http.StatusBadRequest)
			return
		}
		if _, err := VerifyState(cfg.OAuthStateSecret, stateParam, now()); err != nil {
			reason := "invalid"
			switch {
			case errors.Is(err, errStateExpired):
				reason = "expired"
			case errors.Is(err, errStateBadHMAC):
				reason = "hmac_mismatch"
			case errors.Is(err, errStateMalformed):
				reason = "malformed"
			case errors.Is(err, errStateFuture):
				reason = "future_timestamp"
			}
			slog.Warn("oauth/start rejected invalid state", "reason", reason)
			http.Error(w, "invalid or expired setup link — run /qurl setup again", http.StatusBadRequest)
			return
		}
		// Cookie MUST be set before the 302 — once we write the Location
		// header the response is committed and a deferred Set-Cookie is
		// silently dropped.
		setStateCookie(w, stateParam)
		http.Redirect(w, r, authorizeURL(cfg, stateParam), http.StatusFound)
	}
}
