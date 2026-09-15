package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidateFeedbackWebhookURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		raw      string
		wantHost string
		wantErr  bool
	}{
		{name: "slack hooks host", raw: "https://hooks.slack.com/services/T0/B0/xxxx", wantHost: slackIncomingWebhookHost},
		{name: "other https host allowed (caller warns)", raw: "https://relay.example.com/feedback", wantHost: "relay.example.com"},
		{name: "http rejected", raw: "http://hooks.slack.com/x", wantErr: true},
		{name: "userinfo rejected", raw: "https://user@hooks.slack.com/x", wantErr: true},
		{name: "empty rejected", raw: "", wantErr: true},
		{name: "missing host rejected", raw: "https:///just-a-path", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			host, err := validateFeedbackWebhookURL(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.raw, err)
			}
			if host != tc.wantHost {
				t.Errorf("host = %q, want %q", host, tc.wantHost)
			}
		})
	}
}

func TestFeedbackWebhookPoster(t *testing.T) {
	t.Parallel()

	t.Run("success forwards the payload", func(t *testing.T) {
		t.Parallel()
		var gotBody string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			_, _ = w.Write([]byte("ok"))
		}))
		defer srv.Close()
		post := newFeedbackWebhookPoster(srv.URL, "qurl-slack/test", srv.Client())
		if err := post(context.Background(), []byte(`{"text":"hello"}`)); err != nil {
			t.Fatalf("post: %v", err)
		}
		if !strings.Contains(gotBody, "hello") {
			t.Errorf("webhook received %q, want it to carry the payload", gotBody)
		}
	})

	t.Run("non-200 surfaces an error", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("no_service"))
		}))
		defer srv.Close()
		post := newFeedbackWebhookPoster(srv.URL, "", srv.Client())
		err := post(context.Background(), []byte(`{}`))
		if err == nil || !strings.Contains(err.Error(), "500") {
			t.Fatalf("want an error mentioning HTTP 500, got %v", err)
		}
	})
}
