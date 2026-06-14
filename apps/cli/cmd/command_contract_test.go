package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"

	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	testFieldExpiresIn   = "expires_in"
	testFieldExtendBy    = "extend_by"
	testFieldLabel       = "label"
	testFieldMaxSessions = "max_sessions"
	testFieldOneTimeUse  = "one_time_use"
)

func runCmdWithInput(t *testing.T, srv *httptest.Server, input string, args ...string) (string, error) {
	t.Helper()
	isolateCLIEnv(t, testAPIKey)

	cmd := rootCmd("test")
	var buf bytes.Buffer
	cmd.SetIn(strings.NewReader(input))
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(append([]string{testEndpointFlag, srv.URL}, args...))

	err := cmd.Execute()
	return buf.String(), err
}

func TestCreateCommandSendsContractBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != testRouteQURLs {
			t.Errorf("path = %s, want %s", r.URL.Path, testRouteQURLs)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testAPIKey {
			t.Errorf("Authorization = %q", got)
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		assertJSONField(t, body, testFieldTarget, testExampleURL)
		assertJSONField(t, body, testFieldLabel, "Admin access")
		assertJSONField(t, body, testFieldExpiresIn, "24h")
		assertJSONField(t, body, testFieldOneTimeUse, true)
		assertJSONNumber(t, body, testFieldMaxSessions, 3)
		if _, ok := body["description"]; ok {
			t.Fatalf("deprecated description field must not be sent: %#v", body)
		}

		apiEnvelope(t, w, map[string]any{
			testFieldResource: "r_contract",
			testFieldQURLLink: "https://qurl.link/at_contract",
			"qurl_site":       "https://r_contract.qurl.site",
		})
	}))
	defer srv.Close()

	out := runCmd(t, srv, "create", testExampleURL,
		"--description", "legacy label",
		"--label", "Admin access",
		"--expires", "24h",
		"--one-time",
		"--max-sessions", "3",
	)
	if !strings.Contains(out, "r_contract") {
		t.Errorf("expected created resource in output:\n%s", out)
	}
}

func TestCreateCommandDescriptionAliasFallsBackToLabel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		assertJSONField(t, body, testFieldLabel, "Legacy label")
		if _, ok := body["description"]; ok {
			t.Fatalf("deprecated description field must not be sent: %#v", body)
		}
		apiEnvelope(t, w, map[string]any{
			testFieldResource: "r_legacy",
			testFieldQURLLink: "https://qurl.link/at_legacy",
		})
	}))
	defer srv.Close()

	out := runCmd(t, srv, "create", testExampleURL, "--description", "Legacy label")
	if !strings.Contains(out, "r_legacy") {
		t.Errorf("expected legacy create output:\n%s", out)
	}
}

func TestListCommandSendsQueryContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != testRouteQURLs {
			t.Errorf("path = %s, want %s", r.URL.Path, testRouteQURLs)
		}
		q := r.URL.Query()
		want := map[string]string{
			"limit":  "7",
			"cursor": "page=2&sig=yes",
			"status": client.StatusRevoked,
			"q":      "dashboard",
			"sort":   "created_at:asc",
		}
		for key, value := range want {
			if got := q.Get(key); got != value {
				t.Errorf("query %s = %q, want %q", key, got, value)
			}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			testFieldData: []map[string]any{{
				testFieldResource: "r_revoked",
				testFieldTarget:   testExampleURL,
				testFieldStatus:   client.StatusRevoked,
				testFieldCreated:  testCreatedAt,
			}},
			testFieldMeta: map[string]any{
				testFieldRequest: testRequestID,
			},
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	out := runCmd(t, srv, "list",
		"--limit", "7",
		"--cursor", "page=2&sig=yes",
		"--status", client.StatusRevoked,
		"--query", "dashboard",
		"--sort", "created_at:asc",
	)
	if !strings.Contains(out, "r_revoked") {
		t.Errorf("expected list output:\n%s", out)
	}
}

func TestPatchCommandsSendContractBodies(t *testing.T) {
	t.Run("update can clear description", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPatch {
				t.Errorf("method = %s, want PATCH", r.Method)
			}
			if r.URL.Path != testRouteQURLs+"/"+testResourceABC {
				t.Errorf("path = %s, want %s/%s", r.URL.Path, testRouteQURLs, testResourceABC)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			assertJSONField(t, body, "description", "")
			apiEnvelope(t, w, map[string]any{
				testFieldResource: testResourceABC,
				testFieldTarget:   testExampleURL,
				testFieldStatus:   testStatusActive,
				testFieldCreated:  testCreatedAt,
			})
		}))
		defer srv.Close()

		out := runCmd(t, srv, "update", testResourceABC, "--description", "")
		if !strings.Contains(out, testResourceABC) {
			t.Errorf("expected update output:\n%s", out)
		}
	})

	t.Run("extend sends extend_by", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPatch {
				t.Errorf("method = %s, want PATCH", r.Method)
			}
			if r.URL.Path != testRouteQURLs+"/"+testResourceABC {
				t.Errorf("path = %s, want %s/%s", r.URL.Path, testRouteQURLs, testResourceABC)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			assertJSONField(t, body, testFieldExtendBy, "48h")
			apiEnvelope(t, w, map[string]any{
				testFieldResource: testResourceABC,
				testFieldTarget:   testExampleURL,
				testFieldStatus:   testStatusActive,
				testFieldCreated:  testCreatedAt,
			})
		}))
		defer srv.Close()

		out := runCmd(t, srv, "extend", testResourceABC, "--by", "48h")
		if !strings.Contains(out, testResourceABC) {
			t.Errorf("expected extend output:\n%s", out)
		}
	})
}

func TestDeleteCommandLocalConfirmationPathsDoNotHitAPI(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	out := runCmd(t, srv, "delete", "--dry-run", "r_123")
	if !strings.Contains(out, "dry run") {
		t.Errorf("expected dry-run output:\n%s", out)
	}

	out, err := runCmdWithInput(t, srv, "n\n", "delete", "r_123")
	if err != nil {
		t.Fatalf("cancel delete: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Canceled") {
		t.Errorf("expected cancel output:\n%s", out)
	}
	if hits.Load() != 0 {
		t.Errorf("API should not be hit for dry-run/canceled delete; got %d hits", hits.Load())
	}
}

func TestResourceIDCompletionContract(t *testing.T) {
	longTarget := testExampleURL + "/" + strings.Repeat("x", 80)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("q"); got != "dash" {
			t.Errorf("completion query = %q, want dash", got)
		}
		if got := r.URL.Query().Get("limit"); got != "20" {
			t.Errorf("completion limit = %q, want 20", got)
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			testFieldData: []map[string]any{{
				testFieldResource: "r_complete",
				testFieldTarget:   longTarget,
				testFieldStatus:   testStatusActive,
				testFieldCreated:  testCreatedAt,
			}},
			testFieldMeta: map[string]any{testFieldRequest: testRequestID},
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer srv.Close()

	cmd := rootCmd("test")
	cmd.SetContext(context.Background())
	opts := &globalOpts{apiKey: testAPIKey, endpoint: srv.URL, version: "test"}
	got, directive := resourceIDCompletion(opts)(cmd, nil, "dash")
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want NoFileComp", directive)
	}
	if len(got) != 1 {
		t.Fatalf("completion count = %d, want 1: %v", len(got), got)
	}
	if !strings.HasPrefix(got[0], "r_complete\t"+testExampleURL) || !strings.Contains(got[0], "…") {
		t.Errorf("unexpected completion entry: %q", got[0])
	}
}

func TestResourceIDCompletionWithoutCredentialsDoesNotCompleteFiles(t *testing.T) {
	isolateCLIEnv(t)
	got, directive := resourceIDCompletion(&globalOpts{})(rootCmd("test"), nil, "r_")
	if len(got) != 0 {
		t.Errorf("expected no completions without credentials, got %v", got)
	}
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want NoFileComp", directive)
	}
}

func TestCompletionCommandBash(t *testing.T) {
	isolateCLIEnv(t)
	cmd := rootCmd("test")
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"completion", shellBash})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("completion bash: %v\n%s", err, buf.String())
	}
	if out := buf.String(); !strings.Contains(out, "__start_qurl") || !strings.Contains(out, "completion") {
		t.Errorf("unexpected bash completion output:\n%s", out)
	}
}

func assertJSONField(t *testing.T, body map[string]any, key string, want any) {
	t.Helper()
	got, ok := body[key]
	if !ok {
		t.Fatalf("missing JSON field %q in %#v", key, body)
	}
	if got != want {
		t.Fatalf("JSON field %q = %#v, want %#v", key, got, want)
	}
}

func assertJSONNumber(t *testing.T, body map[string]any, key string, want float64) {
	t.Helper()
	got, ok := body[key]
	if !ok {
		t.Fatalf("missing JSON field %q in %#v", key, body)
	}
	if got != want {
		t.Fatalf("JSON number %q = %#v, want %#v", key, got, want)
	}
}
