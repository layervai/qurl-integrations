package oauth

import (
	"html"
	"net/http/httptest"
	"strings"
	"testing"
)

var testSecret = []byte("hmac-secret-32-bytes-or-whatever")

const (
	testTenantID       = "tenant-123"
	testUserID         = "user-123"
	testAuth0ClientID  = "client-id"
	testAuth0Audience  = "https://api.qurl.invalid"
	testAdminEmail     = "admin@example.com"
	testNormalizedMail = "admin+setup@example.com"
	testKeyID          = "k_1"
	testKeyPrefix      = "lv_live_abcd"
	testAPIKey         = "lv_live_abcd1234"
)

func assertOAuthErrorPage(t *testing.T, rec *httptest.ResponseRecorder, heading string) {
	t.Helper()
	body := rec.Body.String()
	wantTitle := "<title>" + html.EscapeString(heading) + "</title>"
	if !strings.Contains(body, wantTitle) {
		t.Fatalf("body missing title %q: %s", wantTitle, body)
	}
	if !strings.Contains(body, html.EscapeString(heading)) {
		t.Fatalf("body missing heading %q: %s", heading, body)
	}
}
