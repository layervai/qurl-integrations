package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// apiEnvelope wraps data in the qURL API response envelope.
func apiEnvelope(t *testing.T, w http.ResponseWriter, data any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(map[string]any{
		"data": data,
		"meta": map[string]any{"request_id": "req_test"},
	}); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

// newMockServer creates a test server that handles qURL API routes.
func newMockServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/qurls":
			apiEnvelope(t, w, map[string]any{
				"qurl_id":     "q_test123",
				"resource_id": "r_test123",
				"qurl_link":   "https://qurl.link/at_abc",
				"qurl_site":   "https://r_test123.qurl.site",
			})

		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/qurls/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/qurls/")
			apiEnvelope(t, w, map[string]any{
				"resource_id": id,
				"target_url":  "https://example.com",
				"status":      "active",
				"description": "updated",
				"created_at":  "2026-03-01T00:00:00Z",
			})

		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/qurls/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/qurls/")
			apiEnvelope(t, w, map[string]any{
				"resource_id": id,
				"target_url":  "https://example.com",
				"status":      "active",
				"created_at":  "2026-03-01T00:00:00Z",
			})

		case r.Method == http.MethodGet && r.URL.Path == "/v1/qurls":
			if err := json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{
						"resource_id": "r_1",
						"target_url":  "https://example.com",
						"status":      "active",
						"created_at":  "2026-03-01T00:00:00Z",
					},
				},
				"meta": map[string]any{
					"request_id": "req_test",
					"has_more":   false,
				},
			}); err != nil {
				t.Fatalf("encode response: %v", err)
			}

		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPost && r.URL.Path == "/v1/resolve":
			apiEnvelope(t, w, map[string]any{
				"target_url":  "https://api.example.com",
				"resource_id": "r_test",
				"access_grant": map[string]any{
					"expires_in": 305,
					"src_ip":     "127.0.0.1",
				},
			})

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/mint_link"):
			apiEnvelope(t, w, map[string]any{
				"qurl_link":  "https://qurl.link/at_minted",
				"expires_at": "2026-04-01T00:00:00Z",
			})

		case r.Method == http.MethodGet && r.URL.Path == "/v1/quota":
			apiEnvelope(t, w, map[string]any{
				"plan":         "pro",
				"period_start": "2026-03-01T00:00:00Z",
				"period_end":   "2026-03-31T00:00:00Z",
				"usage": map[string]any{
					"qurls_created":        42,
					"active_qurls":         10,
					"active_qurls_percent": 20.0,
					"total_accesses":       100,
				},
			})

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

// runCmdErr executes a CLI command expecting an error, returns the error.
func runCmdErr(t *testing.T, srv *httptest.Server, args ...string) error {
	t.Helper()
	t.Setenv("QURL_API_KEY", "test-key")

	cmd := rootCmd("test")
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(append([]string{"--endpoint", srv.URL}, args...))

	return cmd.Execute()
}

func TestCreateCommand(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	out := runCmd(t, srv, "create", "https://example.com")
	if !strings.Contains(out, "r_test123") {
		t.Errorf("expected r_test123 in output:\n%s", out)
	}
}

func TestCreateCommandInvalidURL(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	err := runCmdErr(t, srv, "create", "not-a-url")
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestCreateCommandInvalidDuration(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	err := runCmdErr(t, srv, "create", "https://example.com", "--expires", "forever")
	if err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestGetCommand(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	out := runCmd(t, srv, "get", "r_abc")
	if !strings.Contains(out, "r_abc") {
		t.Errorf("expected r_abc in output:\n%s", out)
	}
}

func TestGetCommandInvalidID(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	err := runCmdErr(t, srv, "get", "bad_id")
	if err == nil {
		t.Fatal("expected error for invalid resource ID")
	}
}

func TestListCommand(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	out := runCmd(t, srv, "list", "--limit", "5")
	if !strings.Contains(out, "r_1") {
		t.Errorf("expected r_1 in output:\n%s", out)
	}
}

func TestListCommandWithCursor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		if cursor != "page2" {
			t.Errorf("expected cursor 'page2', got %q", cursor)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{
					"resource_id": "r_page2",
					"target_url":  "https://example.com",
					"status":      "active",
					"created_at":  "2026-03-01T00:00:00Z",
				},
			},
			"meta": map[string]any{"request_id": "req_test"},
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	out := runCmd(t, srv, "list", "--cursor", "page2")
	if !strings.Contains(out, "r_page2") {
		t.Errorf("expected r_page2 in output:\n%s", out)
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

func TestResolveCommandInvalidToken(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	err := runCmdErr(t, srv, "resolve", "bad_token")
	if err == nil {
		t.Fatal("expected error for invalid access token")
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
	cmd.SetArgs([]string{"--endpoint", srv.URL, "delete", "r_123"})

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

	out := runCmd(t, srv, "delete", "--yes", "r_123")
	if !strings.Contains(out, "revoked") {
		t.Errorf("expected 'revoked' in output:\n%s", out)
	}
}

func TestDeleteCommandDryRun(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	out := runCmd(t, srv, "delete", "--dry-run", "r_123")
	if !strings.Contains(out, "dry run") {
		t.Errorf("expected 'dry run' in output:\n%s", out)
	}
	if strings.Contains(out, "revoked") {
		t.Errorf("dry-run should not revoke:\n%s", out)
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
	cmd.SetArgs([]string{"--endpoint", srv.URL, "delete", "r_123"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute delete: %v\noutput: %s", err, buf.String())
	}

	out := buf.String()
	if !strings.Contains(out, "Canceled") {
		t.Errorf("expected 'Canceled' in output:\n%s", out)
	}
}

func TestUpdateCommand(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	out := runCmd(t, srv, "update", "r_abc", "--description", "updated")
	if !strings.Contains(out, "r_abc") {
		t.Errorf("expected r_abc in output:\n%s", out)
	}
}

func TestUpdateCommandNoFlags(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	err := runCmdErr(t, srv, "update", "r_abc")
	if err == nil {
		t.Fatal("expected error when no flags set")
	}
}

func TestJSONOutput(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	out := runCmd(t, srv, "-o", "json", "get", "r_abc")

	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("expected valid JSON output: %v\n%s", err, out)
	}
	if parsed["resource_id"] != "r_abc" {
		t.Errorf("got resource_id %q, want %q", parsed["resource_id"], "r_abc")
	}
}

func TestMissingAPIKey(t *testing.T) {
	t.Setenv("QURL_API_KEY", "")
	// Isolate from any real config file on the developer's machine.
	t.Setenv("HOME", t.TempDir())
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

func TestVerboseFlag(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	// Verbose logging goes to os.Stderr (not cobra's err buffer).
	// We verify the flag is accepted and the command succeeds.
	out := runCmd(t, srv, "--verbose", "get", "r_abc")
	if !strings.Contains(out, "r_abc") {
		t.Errorf("expected r_abc in output:\n%s", out)
	}
}

func TestExtendCommand(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	out := runCmd(t, srv, "extend", "r_abc", "--by", "24h")
	if !strings.Contains(out, "r_abc") {
		t.Errorf("expected r_abc in output:\n%s", out)
	}
}

func TestExtendCommandRequiresBy(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	err := runCmdErr(t, srv, "extend", "r_abc")
	if err == nil {
		t.Fatal("expected error when --by not provided")
	}
}

func TestMintCommand(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	out := runCmd(t, srv, "mint", "r_abc")
	if !strings.Contains(out, "minted") || !strings.Contains(out, "qurl.link") {
		t.Errorf("expected mint output:\n%s", out)
	}
}

func TestQuotaCommand(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	out := runCmd(t, srv, "quota")
	if !strings.Contains(out, "PRO") && !strings.Contains(out, "pro") {
		t.Errorf("expected plan name in output:\n%s", out)
	}
}

func TestDeleteCommandNoForceFlag(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	// --force flag should no longer exist
	err := runCmdErr(t, srv, "delete", "--force", "r_123")
	if err == nil {
		t.Fatal("expected error: --force flag should not exist")
	}
}

func TestInvalidOutputFormat(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	err := runCmdErr(t, srv, "--output", "yaml", "list")
	if err == nil {
		t.Fatal("expected error for invalid output format")
	}
}

// --- Config subcommand integration tests ---

// runConfigCmd executes a config CLI command with HOME redirected to a temp dir.
func runConfigCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("QURL_API_KEY", "test-key")

	cmd := rootCmd("test")
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)

	err := cmd.Execute()
	return buf.String(), err
}

func TestConfigSetAndGet(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("QURL_API_KEY", "test-key")

	// Set a value
	setCmd := rootCmd("test")
	var setBuf bytes.Buffer
	setCmd.SetOut(&setBuf)
	setCmd.SetErr(&setBuf)
	setCmd.SetArgs([]string{"config", "set", "endpoint", "https://test.example.com"})
	if err := setCmd.Execute(); err != nil {
		t.Fatalf("config set: %v", err)
	}
	if !strings.Contains(setBuf.String(), "Set endpoint") {
		t.Errorf("expected 'Set endpoint' in output: %s", setBuf.String())
	}

	// Get the value back (same HOME dir)
	getCmd := rootCmd("test")
	var getBuf bytes.Buffer
	getCmd.SetOut(&getBuf)
	getCmd.SetErr(&getBuf)
	getCmd.SetArgs([]string{"config", "get", "endpoint"})
	if err := getCmd.Execute(); err != nil {
		t.Fatalf("config get: %v", err)
	}
	if !strings.Contains(getBuf.String(), "https://test.example.com") {
		t.Errorf("expected endpoint value in output: %s", getBuf.String())
	}
}

func TestConfigSetAPIKeyWarning(t *testing.T) {
	out, err := runConfigCmd(t, "config", "set", "api_key", "lv_live_test")
	if err != nil {
		t.Fatalf("config set: %v", err)
	}
	if !strings.Contains(out, "plaintext") {
		t.Errorf("expected plaintext warning in output: %s", out)
	}
}

func TestConfigGetInvalidKey(t *testing.T) {
	_, err := runConfigCmd(t, "config", "get", "nonexistent")
	if err == nil {
		t.Fatal("expected error for invalid key")
	}
}

func TestConfigPath(t *testing.T) {
	out, err := runConfigCmd(t, "config", "path")
	if err != nil {
		t.Fatalf("config path: %v", err)
	}
	if !strings.Contains(out, "config.yaml") {
		t.Errorf("expected config.yaml in path output: %s", out)
	}
}

func TestConfigProfiles(t *testing.T) {
	out, err := runConfigCmd(t, "config", "profiles")
	if err != nil {
		t.Fatalf("config profiles: %v", err)
	}
	if !strings.Contains(out, "No profiles configured") {
		t.Errorf("expected empty profiles message: %s", out)
	}
}

// --- Quiet flag tests ---

func TestCreateCommandQuiet(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	out := runCmd(t, srv, "--quiet", "create", "https://example.com")
	// Quiet mode should output just the link, not the full table
	if !strings.Contains(out, "qurl.link") {
		t.Errorf("expected link in quiet output: %s", out)
	}
	if strings.Contains(out, "qURL created") {
		t.Error("quiet mode should not include table header")
	}
}

func TestMintCommandQuiet(t *testing.T) {
	srv := newMockServer(t)
	defer srv.Close()

	out := runCmd(t, srv, "--quiet", "mint", "r_abc")
	if !strings.Contains(out, "qurl.link") {
		t.Errorf("expected link in quiet output: %s", out)
	}
	if strings.Contains(out, "Link minted") {
		t.Error("quiet mode should not include table header")
	}
}
