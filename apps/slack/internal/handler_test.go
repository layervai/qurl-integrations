package internal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/aws/aws-lambda-go/events"

	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

func newTestHandler(t *testing.T, qurlServer *httptest.Server) *Handler {
	t.Helper()
	return NewHandler(Config{
		QURLEndpoint:       qurlServer.URL,
		AuthProvider:       &auth.EnvProvider{EnvVar: "QURL_API_KEY"},
		SlackSigningSecret: "test-secret",
		NewClient: func(apiKey string) *client.Client {
			return client.New(qurlServer.URL, apiKey)
		},
	})
}

func TestHealthEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newTestHandler(t, srv)
	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path:       "/health",
		HTTPMethod: "GET",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestSlashCommandHelp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newTestHandler(t, srv)
	body := url.Values{
		"command": {"/qurl"},
		"text":    {"help"},
		"team_id": {"T123"},
	}.Encode()

	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path:       "/slack/commands",
		HTTPMethod: methodPost,
		Body:       body,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]string
	if err := json.Unmarshal([]byte(resp.Body), &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result["text"] == "" {
		t.Error("expected non-empty help text")
	}
}

func TestSlashCommandCreate(t *testing.T) {
	qurlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(client.QURL{
			ID:        "qurl_123",
			ShortCode: "abc",
			TargetURL: "https://example.com",
			LinkURL:   "https://qurl.link.layerv.xyz/abc",
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer qurlSrv.Close()

	t.Setenv("QURL_API_KEY", "test-key")

	h := newTestHandler(t, qurlSrv)
	body := url.Values{
		"command": {"/qurl"},
		"text":    {"create https://example.com"},
		"team_id": {"T123"},
	}.Encode()

	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path:       "/slack/commands",
		HTTPMethod: methodPost,
		Body:       body,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]string
	if err := json.Unmarshal([]byte(resp.Body), &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result["response_type"] != "ephemeral" {
		t.Errorf("expected ephemeral response, got %q", result["response_type"])
	}
}

func TestURLVerificationChallenge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := newTestHandler(t, srv)
	body := `{"type":"url_verification","challenge":"test-challenge-123"}`

	resp, err := h.Handle(context.Background(), &events.APIGatewayProxyRequest{
		Path:       "/slack/events",
		HTTPMethod: methodPost,
		Body:       body,
	})
	if err != nil {
		t.Fatal(err)
	}

	var result map[string]string
	if err := json.Unmarshal([]byte(resp.Body), &result); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if result["challenge"] != "test-challenge-123" {
		t.Errorf("expected challenge echo, got %q", result["challenge"])
	}
}
