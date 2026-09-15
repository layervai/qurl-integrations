package slackaudit

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
)

func TestLogDependencyAuthFailureShape(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	LogDependencyAuthFailure(log,
		slog.String("route", "qurl_get"),
		slog.String("method", http.MethodPost),
		slog.String("path", "/v1/resources/:id/qurls"),
		slog.Int("status", http.StatusUnauthorized),
		slog.String("code", "invalid_token"),
		slog.String("request_id", "req_123"),
	)

	var got struct {
		Level string         `json:"level"`
		Audit map[string]any `json:"audit"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal audit log: %v\n%s", err, buf.String())
	}
	if got.Level != "WARN" {
		t.Fatalf("level = %q, want WARN; log=%s", got.Level, buf.String())
	}

	for k, want := range map[string]any{
		"event":      DependencyAuthFailure,
		"agent":      AgentSlack,
		"dependency": DependencyQURLService,
		"route":      "qurl_get",
		"method":     http.MethodPost,
		"path":       "/v1/resources/:id/qurls",
		"code":       "invalid_token",
		"request_id": "req_123",
	} {
		if got.Audit[k] != want {
			t.Fatalf("audit[%s] = %#v, want %#v; full audit=%#v", k, got.Audit[k], want, got.Audit)
		}
	}
	if got.Audit["status"] != float64(http.StatusUnauthorized) {
		t.Fatalf("audit[status] = %#v, want %d", got.Audit["status"], http.StatusUnauthorized)
	}
}
