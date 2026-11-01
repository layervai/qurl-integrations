package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

func TestAccountAPIKeyModeIsExplicitAndScoped(t *testing.T) {
	for _, group := range []string{"webhooks", "domains"} {
		for _, write := range []bool{false, true} {
			t.Run(group+map[bool]string{false: "/read", true: "/write"}[write], func(t *testing.T) {
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer "+testAPIKey || r.Header.Get("X-QURL-Owner") != "" {
						t.Error("wrong explicit authority")
					}
					if r.URL.Path == "/v1/me" {
						_, _ = w.Write([]byte(`{"data":{"owner_id":"auth0|fixture","auth_type":"api_key","api_key":{"key_id":"key_Ab0123456789","kind":"api_key","scopes":["qurl:read","qurl:write"]}}}`))
						return
					}
					calls++
					if write {
						if r.Method != http.MethodDelete {
							t.Error("wrong mutation")
						}
						w.WriteHeader(http.StatusNoContent)
						return
					}
					if r.Method != http.MethodGet || r.URL.Path != "/v1/"+group {
						t.Error("wrong read")
					}
					_, _ = w.Write([]byte(`{"data":[],"meta":{"has_more":false}}`))
				}))
				defer server.Close()
				var stdout, stderr bytes.Buffer
				configDir := t.TempDir()
				root, _ := newRoot("test", &output.Streams{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, func(g *globalOpts) {
					g.configDir = configDir
					g.lookupEnv = func(name string) (string, bool) {
						if name == auth.EnvAPIKey {
							return testAPIKey, true
						}
						return "", false
					}
					g.signInAdmin = func(context.Context, *qurlapi.Config, func(context.Context, string) error, []string) (string, error) {
						t.Fatal("explicit key mode opened browser")
						return "", nil
					}
					g.openAPIClient = func(context.Context) (qurlapi.Client, error) {
						t.Fatal("explicit account authority opened device client")
						return nil, errors.New("device authority forbidden")
					}
				})
				args := []string{"--endpoint", server.URL, "--output", "json", group, "--api-key-auth", "list"}
				if write {
					id := "secure.example.com"
					if group == "webhooks" {
						id = webhookCommandID
					}
					args = []string{"--endpoint", server.URL, "--output", "json", group, "--api-key-auth", "delete", id}
				}
				root.SetArgs(args)
				if err := root.Execute(); err != nil {
					t.Fatal(err)
				}
				files, readErr := os.ReadDir(configDir)
				if readErr != nil || len(files) != 0 {
					t.Fatal("explicit account key persisted or opened device state")
				}
				if calls != 1 || !json.Valid(stdout.Bytes()) || strings.Contains(stdout.String(), testAPIKey) || strings.Contains(stderr.String(), testAPIKey) {
					t.Fatal("lost operation or exposed account key")
				}
			})
		}
	}
}

func TestAccountAPIKeyModeRejectsAmbiguousAuthority(t *testing.T) {
	tests := []struct {
		name, identity string
		write          bool
	}{
		{"device", `{"auth_type":"api_key","api_key":{"key_id":"key_fixture","kind":"device","scopes":["qurl:read","qurl:write"]}}`, false},
		{"bootstrap", `{"auth_type":"api_key","api_key":{"key_id":"key_fixture","kind":"tunnel_bootstrap","scopes":["qurl:read","qurl:write"]}}`, false},
		{"future", `{"auth_type":"api_key","api_key":{"key_id":"key_fixture","kind":"future_kind","scopes":["qurl:read","qurl:write"]}}`, false},
		{"jwt", `{"auth_type":"jwt"}`, false},
		{"missing-key", `{"auth_type":"api_key"}`, false},
		{"missing-id", `{"auth_type":"api_key","api_key":{"kind":"api_key","scopes":["qurl:read","qurl:write"]}}`, false},
		{"missing-read", `{"auth_type":"api_key","api_key":{"key_id":"key_fixture","kind":"api_key","scopes":["qurl:write"]}}`, false},
		{"missing-write", `{"auth_type":"api_key","api_key":{"key_id":"key_fixture","kind":"api_key","scopes":["qurl:read"]}}`, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/me" {
					calls++
					t.Error("invalid authority reached account operation")
				}
				var identity map[string]any
				if err := json.Unmarshal([]byte(test.identity), &identity); err != nil {
					t.Fatal(err)
				}
				identity["owner_id"] = "auth0|fixture"
				_ = json.NewEncoder(w).Encode(map[string]any{"data": identity})
			}))
			defer server.Close()
			var stdout, stderr bytes.Buffer
			root, _ := newRoot("test", &output.Streams{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, func(g *globalOpts) {
				g.configDir = t.TempDir()
				g.lookupEnv = func(name string) (string, bool) {
					if name == auth.EnvAPIKey {
						return testAPIKey, true
					}
					return "", false
				}
				g.signInAdmin = func(context.Context, *qurlapi.Config, func(context.Context, string) error, []string) (string, error) {
					t.Fatal("invalid key fell back to browser")
					return "", nil
				}
			})
			args := []string{"--endpoint", server.URL, "webhooks", "--api-key-auth", "list"}
			if test.write {
				args = []string{"--endpoint", server.URL, "webhooks", "--api-key-auth", "delete", webhookCommandID}
			}
			root.SetArgs(args)
			err := root.Execute()
			if !errors.Is(err, auth.ErrAccountManagementDenied) || exitcode.FromError(err) != exitcode.Forbidden || calls != 0 {
				t.Fatalf("authority accepted or misclassified: %v", err)
			}
		})
	}
}

func TestAccountAPIKeyModeNeverFallsBackOnCredentialErrors(t *testing.T) {
	for _, values := range []map[string]string{
		{}, {auth.EnvAPIKey: "not-a-key"}, {auth.EnvAPIKey: testAPIKey, auth.EnvAPIKeyFile: "/missing/fixture-key"}, {auth.EnvAPIKeyFile: "/missing/fixture-key"},
	} {
		var stdout, stderr bytes.Buffer
		root, _ := newRoot("test", &output.Streams{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, func(g *globalOpts) {
			g.configDir = t.TempDir()
			g.lookupEnv = func(name string) (string, bool) { v, ok := values[name]; return v, ok }
			g.signInAdmin = func(context.Context, *qurlapi.Config, func(context.Context, string) error, []string) (string, error) {
				t.Fatal("invalid explicit credential opened browser")
				return "", nil
			}
		})
		root.SetArgs([]string{"webhooks", "--api-key-auth", "list"})
		err := root.Execute()
		if err == nil || strings.Contains(err.Error(), testAPIKey) {
			t.Fatal("invalid explicit credential accepted or printed")
		}
	}
}
