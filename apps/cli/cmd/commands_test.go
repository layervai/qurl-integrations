package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/shared/client"
)

// newMockServer creates a test server that handles QURL API routes.
func newMockServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/qurls":
			if err := json.NewEncoder(w).Encode(client.QURL{
				ID:        "qurl_test",
				TargetURL: "https://example.com",
				LinkURL:   "https://qurl.link/abc",
			}); err != nil {
				t.Fatalf("encode response: %v", err)
			}

		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/qurls/"):
			if err := json.NewEncoder(w).Encode(client.QURL{
				ID:        strings.TrimPrefix(r.URL.Path, "/v1/qurls/"),
				TargetURL: "https://example.com",
				LinkURL:   "https://qurl.link/abc",
			}); err != nil {
				t.Fatalf("encode response: %v", err)
			}

		case r.Method == http.MethodGet && r.URL.Path == "/v1/qurls":
			if err := json.NewEncoder(w).Encode(client.ListOutput{
				QURLs: []client.QURL{
					{ID: "qurl_1", TargetURL: "https://example.com"},
				},
			}); err != nil {
				t.Fatalf("encode response: %v", err)
			}

		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPost && r.URL.Path == "/v1/resolve":
			if err := json.NewEncoder(w).Encode(client.ResolveOutput{
				TargetURL:  "https://api.example.com",
				ResourceID: "r_test",
				AccessGrant: &client.AccessGrant{
					ExpiresIn: 305,
					SrcIP:     "127.0.0.1",
				},
			}); err != nil {
				t.Fatalf("encode response: %v", err)
			}

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// runCmd executes a CLI command with the given args and returns stdout output.
func runCmd(t *testing.T, srv *httptest.Server, args ...string) string {
	t.Helper()
	t.Setenv("QURL_API_KEY", "test-key")

	cmd := rootCmd("test")
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(append([]string{"--endpoint", srv.URL}, args...))

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute %v: %v\noutput: %s", args, err, buf.String())
	}
	return buf.String()
}

func TestCreateCommand(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	out := runCmd(t, srv, "create", "https://example.com")
	if !strings.Contains(out, "qurl_test") {
		t.Errorf("expected qurl_test in output:\n%s", out)
	}
}

func TestGetCommand(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	out := runCmd(t, srv, "get", "qurl_abc")
	if !strings.Contains(out, "qurl_abc") {
		t.Errorf("expected qurl_abc in output:\n%s", out)
	}
}

func TestListCommand(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	out := runCmd(t, srv, "list", "--limit", "5")
	if !strings.Contains(out, "qurl_1") {
		t.Errorf("expected qurl_1 in output:\n%s", out)
	}
}

func TestListCommandWithCursor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		if cursor != "page2" {
			t.Errorf("expected cursor 'page2', got %q", cursor)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(client.ListOutput{
			QURLs: []client.QURL{{ID: "qurl_page2"}},
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	out := runCmd(t, srv, "list", "--cursor", "page2")
	if !strings.Contains(out, "qurl_page2") {
		t.Errorf("expected qurl_page2 in output:\n%s", out)
	}
}

func TestResolveCommand(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	out := runCmd(t, srv, "resolve", "at_testtoken")
	if !strings.Contains(out, "https://api.example.com") {
		t.Errorf("expected target URL in output:\n%s", out)
	}
	if !strings.Contains(out, "305") {
		t.Errorf("expected expires_in 305 in output:\n%s", out)
	}
}

func TestDeleteCommand(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	t.Setenv("QURL_API_KEY", "test-key")
	cmd := rootCmd("test")
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetIn(strings.NewReader("y\n"))
	cmd.SetArgs([]string{"--endpoint", srv.URL, "delete", "qurl_123"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute delete: %v\noutput: %s", err, buf.String())
	}

	out := buf.String()
	if !strings.Contains(out, "revoked") {
		t.Errorf("expected 'revoked' in output:\n%s", out)
	}
}

func TestDeleteCommandWithYesFlag(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	out := runCmd(t, srv, "delete", "--yes", "qurl_123")
	if !strings.Contains(out, "revoked") {
		t.Errorf("expected 'revoked' in output:\n%s", out)
	}
}

func TestDeleteCommandCanceled(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	t.Setenv("QURL_API_KEY", "test-key")
	cmd := rootCmd("test")
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetIn(strings.NewReader("n\n"))
	cmd.SetArgs([]string{"--endpoint", srv.URL, "delete", "qurl_123"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute delete: %v\noutput: %s", err, buf.String())
	}

	out := buf.String()
	if !strings.Contains(out, "Canceled") {
		t.Errorf("expected 'Canceled' in output:\n%s", out)
	}
}

func TestJSONOutput(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	out := runCmd(t, srv, "-o", "json", "get", "qurl_abc")

	var parsed client.QURL
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("expected valid JSON output: %v\n%s", err, out)
	}
	if parsed.ID != "qurl_abc" {
		t.Errorf("got ID %q, want %q", parsed.ID, "qurl_abc")
	}
}

func TestMissingAPIKey(t *testing.T) {
	cmd := rootCmd("test")
	cmd.SetArgs([]string{"list"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing API key")
	}
}

func TestVersionCommand(t *testing.T) {
	t.Setenv("QURL_API_KEY", "test-key")
	cmd := rootCmd("1.2.3")
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"version"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute version: %v", err)
	}

	if !strings.Contains(buf.String(), "1.2.3") {
		t.Errorf("expected version 1.2.3 in output:\n%s", buf.String())
	}
}
