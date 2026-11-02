package oauth

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/shared/observability"
)

// Keep sequential: this test replaces the process-wide default logger.
func TestOAuthFallbackMetricContract(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(observability.NewRedactingJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	for _, header := range []struct {
		reason, value string
		present       bool
	}{
		{reason: "missing_x_forwarded_for"},
		{reason: "empty_x_forwarded_for", present: true},
		{reason: "invalid_x_forwarded_for", present: true, value: "private-invalid-header"},
	} {
		for _, remote := range []struct{ value, fallback string }{
			{value: "192.0.2.1:1234", fallback: "remote_addr"},
			{value: "private-invalid-remote", fallback: "unknown"},
		} {
			logs.Reset()
			r := httptest.NewRequest(http.MethodGet, StartPath+"?state=private-state", http.NoBody)
			r.RemoteAddr = remote.value
			if header.present {
				r.Header.Set("X-Forwarded-For", header.value)
			}
			oauthClientIPWithFallbackLogLimiter(r, newOAuthClientIPFallbackLogLimiter(time.Now))
			var event map[string]any
			if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
				t.Fatal(err)
			}
			// This checks the emitter in isolation; infra matches named fields, not a global logger key count.
			if len(event) != 6 || event["msg"] != "oauth client ip using fallback" || event["path"] != StartPath || event["reason"] != header.reason || event["fallback"] != remote.fallback {
				t.Fatalf("fallback metric contract changed: %v", event)
			}
			for _, secret := range []string{"private-state", "private-invalid-header", "private-invalid-remote", "192.0.2.1"} {
				if strings.Contains(logs.String(), secret) {
					t.Fatal("fallback event leaked input")
				}
			}
			// Verbose output is a fixture source for the read-only AWS filter check.
			t.Logf("fallback_json=%s", logs.Bytes())
		}
	}
	logs.Reset()
	healthy := httptest.NewRequest(http.MethodGet, StartPath, http.NoBody)
	healthy.Header.Set("X-Forwarded-For", "private-invalid-header, 192.0.2.2")
	if got := oauthClientIPWithFallbackLogLimiter(healthy, newOAuthClientIPFallbackLogLimiter(time.Now)); got != "192.0.2.2" || logs.Len() != 0 {
		t.Fatal("trusted appended IP emitted a fallback signal")
	}
}
