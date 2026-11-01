package internal

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSlashSSLCheck(t *testing.T) {
	const body = "ssl_check=1"
	for _, tc := range []struct {
		name, signedBody string
		wantStatus       int
	}{
		{"signed probe", body, http.StatusOK},
		{"unsigned probe", "", http.StatusUnauthorized},
		{"tampered probe", "ssl_check=0", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, hits := countingQURLServer(t)
			h := newTestHandler(t, server)
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			response := httptest.NewRecorder()
			h.ServeHTTP(response, newSignedRequest(t, pathSlackCommands, body, tc.signedBody))
			if response.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, tc.wantStatus, response.Body.String())
			}
			if tc.wantStatus == http.StatusOK && strings.TrimSpace(response.Body.String()) != `{"ok":"true"}` {
				t.Errorf("probe response = %s", response.Body.String())
			}
			if hits.Load() != 0 {
				t.Errorf("probe made %d upstream calls", hits.Load())
			}
			if strings.Contains(logs.String(), "slash command") {
				t.Errorf("probe logged a command: %s", logs.String())
			}
		})
	}
}
