package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

const accountKeyCommandID = "key_Ab0123456789"
const accountKeyCommandSecret = "lv_test_fixture_admin_secret_123456789"

func TestAPIKeyCommandsUseFreshAccountAuthority(t *testing.T) {
	for _, operation := range []string{"list", "create", "update", "revoke"} {
		for _, format := range []string{"text", "json", "quiet"} {
			t.Run(operation+"/"+format, func(t *testing.T) {
				signedIn, discovered, performed := 0, 0, 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer fixture-browser-token" {
						t.Error("request did not use browser authority")
					}
					w.Header().Set("Content-Type", "application/json")
					if r.URL.Path == "/v1/account/owners" {
						discovered++
						if r.Header.Get("X-QURL-Owner") != "" {
							t.Error("owner discovery was already scoped")
						}
						_, _ = w.Write([]byte(`{"owners":["auth0|fixture","device:other"]}`))
						return
					}
					if r.Header.Get("X-QURL-Owner") != "auth0|fixture" {
						t.Error("wrong selected owner")
					}
					performed++
					metadata := qurlapi.APIKeyMetadata{KeyID: accountKeyCommandID, Kind: "api_key", Name: "CI reader", Status: "active", Scopes: []string{"qurl:read"}}
					switch operation {
					case "list":
						if r.Method != http.MethodGet || r.URL.Path != "/v1/api-keys" {
							t.Error("wrong list route")
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"key_id": accountKeyCommandID, "kind": "api_key", "status": "active", "name": "CI\n\x1b[31m", "api_key": accountKeyCommandSecret}}, "meta": map[string]any{"has_more": true, "next_cursor": "\x1b[31m$(unsafe)"}})
					case "create":
						if r.Method != http.MethodPost || r.Header.Get("Idempotency-Key") != strings.Repeat("a", 64) {
							t.Error("create lost replay identity")
						}
						w.WriteHeader(http.StatusCreated)
						_ = json.NewEncoder(w).Encode(map[string]any{"data": qurlapi.CreatedAPIKey{APIKeyMetadata: metadata, Secret: accountKeyCommandSecret}})
					case "update":
						if r.Method != http.MethodPatch || r.URL.Path != "/v1/api-keys/"+accountKeyCommandID {
							t.Error("wrong update route")
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"data": metadata})
					case "revoke":
						if r.Method != http.MethodDelete || r.URL.Path != "/v1/api-keys/"+accountKeyCommandID {
							t.Error("wrong revoke route")
						}
						w.WriteHeader(http.StatusNoContent)
					}
				}))
				defer server.Close()
				var stdout, stderr bytes.Buffer
				configDir := t.TempDir()
				root, _ := newRoot("test", &output.Streams{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, func(g *globalOpts) {
					g.configDir = configDir
					g.lookupEnv = func(name string) (string, bool) {
						if name == "QURL_API_KEY" || name == "QURL_API_KEY_FILE" {
							t.Errorf("account command consulted %s", name)
						}
						return "", false
					}
					g.openAPIClient = func(context.Context) (qurlapi.Client, error) {
						t.Error("account command opened device authority")
						return nil, errors.New("device auth forbidden")
					}
					g.signInAdmin = func(_ context.Context, cfg *qurlapi.Config, _ func(context.Context, string) error, scopes []string) (string, error) {
						signedIn++
						wantScopes := []string{"qurl:read"}
						if operation != "list" {
							wantScopes = append(wantScopes, "qurl:write")
						}
						if !slices.Equal(scopes, wantScopes) {
							t.Errorf("wrong admin scopes: %v", scopes)
						}
						if cfg.APIKey != "" || cfg.OwnerID != "" || cfg.BaseURL != server.URL {
							t.Error("browser sign-in carried another authority")
						}
						return "fixture-browser-token", nil
					}
				})
				args := []string{"api-keys", operation, "--endpoint", server.URL, "--owner", "auth0|fixture", "--verbose"}
				if operation == "create" {
					args = append(args, "--name", "CI reader", "--scopes", "qurl:read", "--idempotency-key", strings.Repeat("a", 64))
				}
				if operation == "update" {
					args = append(args, accountKeyCommandID, "--name", "CI reader")
				}
				if operation == "revoke" {
					args = append(args, accountKeyCommandID)
				}
				if format == "json" {
					args = append(args, "-o", "json")
				}
				if format == "quiet" {
					args = append(args, "--quiet")
				}
				root.SetArgs(args)
				if err := root.Execute(); err != nil {
					t.Fatal(err)
				}
				if signedIn != 1 || discovered != 1 || performed != 1 {
					t.Fatalf("wrong authority sequence: sign-in=%d discovery=%d operation=%d", signedIn, discovered, performed)
				}
				if strings.Contains(stderr.String(), "fixture-browser-token") || strings.Contains(stderr.String(), accountKeyCommandSecret) {
					t.Fatal("verbose diagnostics leaked a token")
				}
				if strings.Contains(stdout.String(), "\x1b") {
					t.Fatal("service content injected terminal controls")
				}
				if operation != "create" && strings.Contains(stdout.String(), accountKeyCommandSecret) {
					t.Fatal("metadata command printed a secret")
				}
				switch format {
				case "quiet":
					want := accountKeyCommandID
					if operation == "create" {
						want = accountKeyCommandSecret
					}
					if stdout.String() != want+"\n" {
						t.Fatalf("wrong quiet primary value: %q", stdout.String())
					}
				case "json":
					var document map[string]any
					if json.Unmarshal(stdout.Bytes(), &document) != nil || document["owner_id"] != "auth0|fixture" {
						t.Fatal("invalid JSON account projection")
					}
					if operation == "create" && document["api_key"] != accountKeyCommandSecret {
						t.Fatal("JSON lost new secret")
					}
				default:
					if !strings.Contains(stdout.String(), accountKeyCommandID) {
						t.Fatal("text result lost key ID")
					}
				}
				entries, err := os.ReadDir(configDir)
				if err != nil || len(entries) != 0 {
					t.Fatal("account command persisted local authority")
				}
			})
		}
	}
}

func TestAPIKeyInvalidInvocationsDoNotOpenBrowser(t *testing.T) {
	cases := [][]string{{"list", "--limit", "0"}, {"list", "--status", "hidden"}, {"create"}, {"create", "--name", "CI", "--scopes", "admin:all"}, {"create", "--name", "CI", "--scopes", "qurl:read", "--idempotency-key", "bad"}, {"update", "bad", "--name", "CI"}, {"update", accountKeyCommandID}, {"revoke", "bad"}, {"revoke"}}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			root, _ := newRoot("test", &output.Streams{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, func(g *globalOpts) {
				g.configDir = t.TempDir()
				g.lookupEnv = func(string) (string, bool) { return "", false }
				g.signInAdmin = func(_ context.Context, _ *qurlapi.Config, _ func(context.Context, string) error, _ []string) (string, error) {
					t.Error("invalid invocation opened browser")
					return "", errors.New("unexpected browser")
				}
			})
			root.SetArgs(append([]string{"api-keys"}, args...))
			if err := root.Execute(); exitcode.FromError(err) != 2 {
				t.Fatalf("want usage exit 2, got %v", err)
			}
		})
	}
}

func TestAPIKeyOwnerSelectionPrecedesMutation(t *testing.T) {
	for _, owner := range []string{"", "auth0|outsider"} {
		t.Run(owner, func(t *testing.T) {
			mutations := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/account/owners" {
					mutations++
					t.Error("unselected owner reached credential operation")
				}
				_, _ = w.Write([]byte(`{"owners":["auth0|fixture","device:other"]}`))
			}))
			defer server.Close()
			var stdout, stderr bytes.Buffer
			root, _ := newRoot("test", &output.Streams{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, func(g *globalOpts) {
				g.configDir = t.TempDir()
				g.lookupEnv = func(string) (string, bool) { return "", false }
				g.signInAdmin = func(_ context.Context, _ *qurlapi.Config, _ func(context.Context, string) error, _ []string) (string, error) {
					return "fixture-browser-token", nil
				}
			})
			args := []string{"api-keys", "revoke", accountKeyCommandID, "--endpoint", server.URL}
			if owner != "" {
				args = append(args, "--owner", owner)
			}
			root.SetArgs(args)
			if err := root.Execute(); err == nil {
				t.Fatal("unselected owner accepted")
			}
			if mutations != 0 {
				t.Fatal("mutation occurred before owner check")
			}
		})
	}
}

func TestAPIKeyCreateReportsReplayIdentityOnFailure(t *testing.T) {
	var received string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/account/owners" {
			_, _ = w.Write([]byte(`{"owners":["auth0|fixture"]}`))
			return
		}
		received = r.Header.Get("Idempotency-Key")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	root, _ := newRoot("test", &output.Streams{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, func(g *globalOpts) {
		g.configDir = t.TempDir()
		g.lookupEnv = func(string) (string, bool) { return "", false }
		g.signInAdmin = func(_ context.Context, _ *qurlapi.Config, _ func(context.Context, string) error, _ []string) (string, error) {
			return "fixture-browser-token", nil
		}
	})
	root.SetArgs([]string{"api-keys", "create", "--name", "CI", "--scopes", "qurl:read", "--endpoint", server.URL})
	err := root.Execute()
	if err == nil || len(received) != 64 || !strings.Contains(err.Error(), "--idempotency-key "+fmt.Sprintf("%q", received)) {
		t.Fatal("failure lost its recoverable create identity")
	}
	if stdout.Len() != 0 {
		t.Fatal("failed create emitted successful data")
	}
}

func TestAPIKeyCommandTreeIsComplete(t *testing.T) {
	root := rootCmd("test")
	command, _, err := root.Find([]string{"api-keys"})
	if err != nil {
		t.Fatal(err)
	}
	commands := command.Commands()
	names := make([]string, 0, len(commands))
	for _, child := range commands {
		names = append(names, child.Name())
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"create", "list", "revoke", "update"}) {
		t.Fatalf("incomplete command tree: %v", names)
	}
}

type apiKeyBrokenOutput struct{}

func (apiKeyBrokenOutput) Write([]byte) (int, error) {
	return 0, errors.New("fixture stdout unavailable")
}

func TestAPIKeyCreateOutputFailureKeepsRecoveryIdentity(t *testing.T) {
	var replay string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/account/owners" {
			_, _ = w.Write([]byte(`{"owners":["auth0|fixture"]}`))
			return
		}
		replay = r.Header.Get("Idempotency-Key")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": qurlapi.CreatedAPIKey{APIKeyMetadata: qurlapi.APIKeyMetadata{KeyID: accountKeyCommandID, Kind: "api_key", Name: "CI", Scopes: []string{"qurl:read"}, Status: "active"}, Secret: accountKeyCommandSecret}})
	}))
	defer server.Close()
	var stderr bytes.Buffer
	root, _ := newRoot("test", &output.Streams{In: strings.NewReader(""), Out: apiKeyBrokenOutput{}, Err: &stderr}, func(g *globalOpts) {
		g.configDir = t.TempDir()
		g.lookupEnv = func(string) (string, bool) { return "", false }
		g.signInAdmin = func(_ context.Context, _ *qurlapi.Config, _ func(context.Context, string) error, _ []string) (string, error) {
			return "fixture-browser-token", nil
		}
	})
	root.SetArgs([]string{"api-keys", "create", "--name", "CI", "--scopes", "qurl:read", "--endpoint", server.URL, "-o", "json"})
	err := root.Execute()
	if err == nil || len(replay) != 64 || !strings.Contains(err.Error(), fmt.Sprintf("--idempotency-key %q", replay)) {
		t.Fatal("failed output lost the existing key's recovery identity")
	}
	if strings.Contains(err.Error(), accountKeyCommandSecret) || strings.Contains(stderr.String(), accountKeyCommandSecret) {
		t.Fatal("failed output leaked the secret into diagnostics")
	}
}

func TestAPIKeyDelegationRequestsOnlyExplicitAdditionalScopes(t *testing.T) {
	for _, requested := range []string{"qurl:read", "qurl:agent", "qurl:keys", "qurl:agent,qurl:keys"} {
		t.Run(requested, func(t *testing.T) {
			denied := errors.New("fixture sign-in stopped")
			var got []string
			var stdout, stderr bytes.Buffer
			root, _ := newRoot("test", &output.Streams{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, func(g *globalOpts) {
				g.configDir = t.TempDir()
				g.lookupEnv = func(string) (string, bool) { return "", false }
				g.signInAccount = func(context.Context, *qurlapi.Config, func(context.Context, string) error) (string, error) {
					t.Error("admin command reused device-link scopes")
					return "", denied
				}
				g.signInAdmin = func(_ context.Context, _ *qurlapi.Config, _ func(context.Context, string) error, scopes []string) (string, error) {
					got = slices.Clone(scopes)
					return "", denied
				}
			})
			root.SetArgs([]string{"api-keys", "create", "--name", "CI", "--scopes", requested})
			if err := root.Execute(); !errors.Is(err, denied) {
				t.Fatalf("unexpected result: %v", err)
			}
			want := []string{"qurl:read", "qurl:write"}
			for _, scope := range []string{"qurl:agent", "qurl:keys"} {
				if slices.Contains(strings.Split(requested, ","), scope) {
					want = append(want, scope)
				}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("requested %s: auth scopes %v, want %v", requested, got, want)
			}
		})
	}
}
