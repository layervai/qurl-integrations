package oauth

import (
	"errors"
	"log/slog"
	"net/http"
)

func Start(cfg Config) http.HandlerFunc {
	now := cfg.now()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			renderOAuthErrorPage(w, http.StatusMethodNotAllowed, "Use the Teams setup link",
				"This qURL setup entrypoint only works from the browser link opened by the Teams bot.")
			return
		}
		if len(cfg.OAuthStateSecret) < StateMinSecret {
			slog.Error("oauth/start refused: OAUTH_STATE_SECRET unset or shorter than 32 bytes")
			renderOAuthErrorPage(w, http.StatusServiceUnavailable, "qURL setup is unavailable",
				"qURL setup is not configured for this Teams deployment.",
				"Contact your qURL administrator for help.")
			return
		}
		stateParam := r.URL.Query().Get("state")
		if stateParam == "" {
			clearStateCookie(w)
			renderOAuthErrorPage(w, http.StatusBadRequest, "Setup link is incomplete",
				"This qURL setup link is missing required setup details.",
				"Return to Teams and run `setup <email>` again.")
			return
		}
		verified, err := VerifyState(cfg.OAuthStateSecret, stateParam, now())
		if err != nil {
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
			clearStateCookie(w)
			renderOAuthErrorPage(w, http.StatusBadRequest, "Setup link is invalid or expired",
				"This qURL setup link is invalid or expired.",
				"Return to Teams and run `setup <email>` again.")
			return
		}
		setStateCookie(w, stateParam)
		http.Redirect(w, r, authorizeURL(cfg, stateParam, verified), http.StatusFound)
	}
}
