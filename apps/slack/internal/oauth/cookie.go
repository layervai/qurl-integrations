package oauth

import (
	"net/http"
)

// cookieName is the double-submit CSRF cookie. /start sets it carrying
// the same state token threaded through Auth0; /callback re-checks
// cookie == query.state via crypto/hmac.Equal. Same browser flows pass;
// leaked-URL replays in a different browser fail.
//
// Cookie posture mirrors the Discord side (utils/oauth-cookies.js):
//
//	HttpOnly  — JS can't read it (XSS won't ex-filtrate)
//	Secure    — only sent over HTTPS (the prod ALB terminates TLS)
//	SameSite=Lax — survives the Auth0 redirect (Lax allows top-level
//	             GETs across origin) while still blocking cross-site
//	             POST replays
//	Path=/oauth — scopes to the OAuth surface so the cookie isn't
//	            sent on /slack/* requests
const (
	cookieName       = "qurl_oauth_state"
	cookiePath       = "/oauth"
	cookieMaxAgeSecs = 300 // 5 minutes; mirrors stateMaxAge
)

// setStateCookie writes the cookie. Caller is responsible for setting
// it before any 302 — once headers are committed Set-Cookie is a no-op.
func setStateCookie(w http.ResponseWriter, state string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    state,
		Path:     cookiePath,
		MaxAge:   cookieMaxAgeSecs,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearStateCookie expires the cookie. Called from /callback after the
// double-submit check passes so a refreshed callback URL can't re-bind.
func clearStateCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     cookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// readStateCookie returns the cookie value, or "" if absent.
func readStateCookie(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	return c.Value
}
