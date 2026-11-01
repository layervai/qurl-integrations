package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

func commandBatchResult(t *testing.T, succeeded int) *qurlapi.BatchCreateResult {
	t.Helper()
	key := apitest.FixedResourceKey(t)
	expiry := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	result := &qurlapi.BatchCreateResult{Succeeded: succeeded, Failed: 2 - succeeded}
	for i := 0; i < 2; i++ {
		item := qurlapi.BatchItemResult{Index: i, Success: i < succeeded}
		if item.Success {
			item.CRID = key.CRID
			item.ResourceID = key.ResourceID
			item.QURLLink = "https://qurl.link/#fixture"
			item.ExpiresAt = &expiry
		} else {
			item.Error = &qurlapi.BatchItemError{Code: "quota_exceeded", Message: "resolve plan\n\x1b[31m"}
		}
		result.Results = append(result.Results, item)
	}
	return result
}

func TestBatchCommandsPreservePartialResultsAndAccountAuthority(t *testing.T) {
	for _, apiKeyMode := range []bool{false, true} {
		for _, succeeded := range []int{0, 1, 2} {
			for _, format := range []string{"text", "json", "quiet"} {
				name := format + map[bool]string{false: "/browser", true: "/key"}[apiKeyMode] + string(rune('0'+succeeded))
				t.Run(name, func(t *testing.T) {
					calls, signedIn := 0, 0
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						token := "fixture-browser-token"
						if apiKeyMode {
							token = testAPIKey
						}
						if r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-QURL-Owner") != "" || r.Header.Get("Idempotency-Key") != "" {
							t.Error("wrong account authority")
						}
						if r.URL.Path == "/v1/me" {
							if apiKeyMode {
								_, _ = w.Write([]byte(`{"data":{"owner_id":"auth0|fixture","auth_type":"api_key","api_key":{"key_id":"key_Ab0123456789","kind":"api_key","scopes":["qurl:read","qurl:write"]}}}`))
							} else {
								_, _ = w.Write([]byte(`{"data":{"owner_id":"auth0|fixture","auth_type":"jwt"}}`))
							}
							return
						}
						calls++
						if r.Method != http.MethodPost || r.URL.Path != "/v1/qurls/batch" {
							t.Error("wrong batch route")
						}
						var body qurlapi.BatchCreateInput
						if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Items) != 2 {
							t.Error("lost input items")
						}
						status := http.StatusMultiStatus
						if succeeded == 0 {
							status = http.StatusBadRequest
						}
						if succeeded == 2 {
							status = http.StatusCreated
						}
						w.WriteHeader(status)
						_ = json.NewEncoder(w).Encode(map[string]any{"data": commandBatchResult(t, succeeded)})
					}))
					defer server.Close()
					var stdout, stderr bytes.Buffer
					configDir := t.TempDir()
					root, _ := newRoot("test", &output.Streams{In: strings.NewReader(`{"items":[{"target_url":"https://a.example"},{"target_url":"https://b.example"}]}`), Out: &stdout, Err: &stderr}, func(g *globalOpts) {
						g.configDir = configDir
						g.lookupEnv = func(name string) (string, bool) {
							if name == auth.EnvAPIKey || name == auth.EnvAPIKeyFile {
								if !apiKeyMode {
									t.Error("default mode read account key")
								}
								if name == auth.EnvAPIKey {
									return testAPIKey, true
								}
							}
							return "", false
						}
						g.openAPIClient = func(context.Context) (qurlapi.Client, error) {
							t.Fatal("batch opened saved device authority")
							return nil, errors.New("forbidden")
						}
						g.signInAdmin = func(_ context.Context, cfg *qurlapi.Config, _ func(context.Context, string) error, scopes []string) (string, error) {
							signedIn++
							if apiKeyMode || cfg.APIKey != "" || cfg.OwnerID != "" || !slices.Equal(scopes, []string{accountAdminReadScope, accountAdminWriteScope}) {
								t.Error("wrong browser account consent")
							}
							return "fixture-browser-token", nil
						}
					})
					args := []string{"--endpoint", server.URL, "batch", "create"}
					if apiKeyMode {
						args = append(args, "--api-key-auth")
					}
					if format == "json" {
						args = append(args, "-o", "json")
					}
					if format == "quiet" {
						args = append(args, "-q")
					}
					root.SetArgs(args)
					err := root.Execute()
					if calls != 1 || (succeeded == 2 && err != nil) || (succeeded != 2 && (err == nil || exitcode.FromError(err) == 0)) {
						t.Fatal("partial failure hidden or request replayed", calls, err)
					}
					if signedIn != map[bool]int{false: 1, true: 0}[apiKeyMode] || strings.Contains(stdout.String(), "\x1b") || strings.Contains(stderr.String(), "\x1b") {
						t.Fatal("wrong sign-in or unsafe terminal output")
					}
					if format == "json" {
						var result qurlapi.BatchCreateResult
						if json.Unmarshal(stdout.Bytes(), &result) != nil || result.Succeeded != succeeded || len(result.Results) != 2 {
							t.Fatal("JSON lost results on failure")
						}
					}
					if format == "quiet" && strings.Count(stdout.String(), "https://qurl.link/#fixture") != succeeded {
						t.Fatal("quiet output lost successful links")
					}
					files, readErr := os.ReadDir(configDir)
					if readErr != nil || len(files) != 0 {
						t.Fatal("batch persisted credential")
					}
				})
			}
		}
	}
}

func TestBatchInvalidInputStopsBeforeSignIn(t *testing.T) {
	for _, raw := range []string{`{}`, `{"items":[{"target_url":"file:///secret"}]}`, strings.Repeat(" ", (1<<20)+1)} {
		var stdout, stderr bytes.Buffer
		root, _ := newRoot("test", &output.Streams{In: strings.NewReader(raw), Out: &stdout, Err: &stderr}, func(g *globalOpts) {
			g.configDir = t.TempDir()
			g.signInAdmin = func(context.Context, *qurlapi.Config, func(context.Context, string) error, []string) (string, error) {
				t.Fatal("invalid batch opened browser")
				return "", nil
			}
		})
		root.SetArgs([]string{"batch", "create"})
		if err := root.Execute(); err == nil || exitcode.FromError(err) != exitcode.Usage {
			t.Fatal("invalid input accepted", err)
		}
	}
}

func TestBatchFileInputAndUnknownOrLostOutputKeepRecovery(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "batch.json")
	if err := os.WriteFile(filename, []byte(`{"items":[{"target_url":"https://a.example"},{"target_url":"https://b.example"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"unavailable", "output", "forbidden"} {
		t.Run(failure, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/me" {
					_, _ = w.Write([]byte(`{"data":{"owner_id":"auth0|fixture","auth_type":"jwt"}}`))
					return
				}
				calls++
				if failure != "output" {
					w.Header().Set("Content-Type", "application/problem+json")
					if failure == "forbidden" {
						w.WriteHeader(http.StatusForbidden)
						_, _ = w.Write([]byte(`{"title":"Account frozen","code":"forbidden"}`))
					} else {
						w.WriteHeader(http.StatusServiceUnavailable)
						_, _ = w.Write([]byte(`{"title":"Unavailable"}`))
					}
					return
				}
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]any{"data": commandBatchResult(t, 2)})
			}))
			defer server.Close()
			var stdout, stderr bytes.Buffer
			streams := &output.Streams{In: strings.NewReader("must not read stdin"), Out: &stdout, Err: &stderr}
			if failure == "output" {
				streams.Out = apiKeyBrokenOutput{}
			}
			root, _ := newRoot("test", streams, func(g *globalOpts) {
				g.configDir = t.TempDir()
				g.lookupEnv = func(string) (string, bool) { return "", false }
				g.signInAdmin = func(context.Context, *qurlapi.Config, func(context.Context, string) error, []string) (string, error) {
					return "fixture-browser-token", nil
				}
			})
			root.SetArgs([]string{"--endpoint", server.URL, "batch", "create", "--file", filename, "-o", "json"})
			err := root.Execute()
			if err == nil || calls != 1 {
				t.Fatal("failed batch ignored or replayed", err, calls)
			}
			var rendered bytes.Buffer
			output.RenderError(&rendered, err, false)
			if failure == "forbidden" {
				if !strings.Contains(rendered.String(), "Account frozen") || strings.Contains(rendered.String(), "uncertain") {
					t.Fatal("definitive denial lost")
				}
			} else if !strings.Contains(rendered.String(), "Do not repeat the whole batch") {
				t.Fatal("lost unknown-result guidance")
			}
		})
	}
}
