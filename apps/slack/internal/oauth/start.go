package oauth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Start returns the http.HandlerFunc for GET /oauth/qurl/start.
//
// Contract:
//   - Requires `?state=<opaque_state>` minted by the /qurl setup
//     slash-command handler (which is Slack-signature-verified).
//     Workspace identity comes from the backend state payload — never
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
			renderOAuthErrorPage(w, http.StatusMethodNotAllowed, "Use the Slack setup link",
				"This qURL™ setup start endpoint only works from the browser link opened by /qurl setup <email>.")
			return
		}
		if len(cfg.OAuthStateSecret) < StateMinSecret {
			slog.Error("oauth/start refused: OAUTH_STATE_SECRET unset or shorter than 32 bytes")
			renderOAuthErrorPage(w, http.StatusServiceUnavailable, "qURL setup is unavailable",
				"qURL™ setup is not configured for this Slack app.",
				"Contact your qURL administrator for help.")
			return
		}
		stateParam := r.URL.Query().Get("state")
		if stateParam == "" {
			slog.Warn("oauth/start rejected: missing state")
			renderOAuthErrorPage(w, http.StatusBadRequest, "Setup link is incomplete",
				"This qURL™ setup link is missing required setup details.",
				"Return to Slack and start setup from /qurl setup <email>.")
			return
		}
		verified, err := startState(r.Context(), cfg, stateParam, now())
		if err != nil {
			if !isStateValidationError(err) {
				slog.Error("oauth/start state store failed", "error", err)
				renderOAuthErrorPage(w, http.StatusServiceUnavailable, "qURL setup is temporarily unavailable",
					"qURL™ setup could not read this setup link.",
					"Return to Slack and run /qurl setup <email> again in a few minutes.")
				return
			}
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
			// Clear any stale cookie from a prior /start the user
			// abandoned. Without this, the next /callback hit would
			// surface the misleading "setup must be completed in the
			// same browser" error rather than re-running setup cleanly.
			clearStateCookie(w)
			renderOAuthErrorPage(w, http.StatusBadRequest, "Setup link is invalid or expired",
				"This qURL™ setup link is invalid or expired.",
				"Return to Slack and run /qurl setup <email> again.")
			return
		}
		// Cookie MUST be set before the 302 — once we write the Location
		// header the response is committed and a deferred Set-Cookie is
		// silently dropped.
		setStateCookie(w, stateParam)
		http.Redirect(w, r, authorizeURL(cfg, stateParam, verified), http.StatusFound)
	}
}

//nolint:gocritic // hugeParam: mirrors Start/Callback's package-wide value-pass Config posture.
func startState(ctx context.Context, cfg Config, stateParam string, now time.Time) (VerifiedState, error) {
	if cfg.StateStore != nil {
		storeCtx, cancel := context.WithTimeout(ctx, stateStoreRequestTimeout)
		verified, err := cfg.StateStore.StartState(storeCtx, stateParam, now)
		cancel()
		if err == nil {
			return verified, nil
		}
		if !errors.Is(err, errStateExpired) && !errors.Is(err, errStateMalformed) {
			return VerifiedState{}, err
		}
		if isOpaqueStateHandle(stateParam) {
			return VerifiedState{}, err
		}
		// Fall through for legacy signed states minted shortly before the
		// server-side state rollout.
	}
	return VerifyState(cfg.OAuthStateSecret, stateParam, now)
}
