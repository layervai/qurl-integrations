package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"

	"github.com/layervai/qurl-go/qurl"
)

func TestAnonymousBootstrapNeedsNoAccountCredential(t *testing.T) {
	opts := &globalOpts{lookupEnv: func(string) (string, bool) { return "", false }}
	bootstrap := newRegisteredAccountBootstrap(opts, nil, "", nil)
	request := qurl.AgentEnrollmentCredentialRequest{AgentID: "anonymous-device", PublicKeyB64: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32)))}
	got, err := bootstrap.enrollmentCredential(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	want, err := qurl.AnonymousEnrollmentCredential(context.Background(), request)
	if err != nil || got != want || bootstrap.client != nil {
		t.Fatal("anonymous enrollment unexpectedly needed an account client")
	}
}

func TestBrowserRecoveryCannotReturnEmptyCredential(t *testing.T) {
	account, err := qurlapi.New(&qurlapi.Config{BaseURL: "https://api.example.test", APIKey: "browser-token"})
	if err != nil {
		t.Fatal(err)
	}
	b := newRegisteredAccountBootstrap(&globalOpts{}, account, "", &qurlapi.Identity{OwnerID: "device:test"})
	key, err := b.recoveryCredential(context.Background())
	if key != "" || !errors.Is(err, auth.ErrAccountRecoveryState) {
		t.Fatalf("recovery = %q, %v", key, err)
	}
	var rendered bytes.Buffer
	output.RenderError(&rendered, fmt.Errorf("runtime recovery: %w", err), false)
	if !strings.Contains(rendered.String(), "QURL_CONNECTOR_STATE_DIR") || strings.Contains(rendered.String(), "qurl login") {
		t.Fatalf("browser recovery guidance = %q", rendered.String())
	}
	b = newRegisteredAccountBootstrap(&globalOpts{lookupEnv: func(string) (string, bool) { return "", false }}, nil, "", nil)
	_, err = b.recoveryCredential(context.Background())
	rendered.Reset()
	output.RenderError(&rendered, fmt.Errorf("runtime recovery: %w", err), false)
	if !errors.Is(err, auth.ErrAnonymousRecovery) || !strings.Contains(rendered.String(), "saved copy") || strings.Contains(rendered.String(), "qurl login") {
		t.Fatalf("anonymous recovery guidance = %q", rendered.String())
	}
}

func TestSelectAccountOwner(t *testing.T) {
	for _, tc := range []struct {
		owners          []string
		requested, want string
	}{
		{[]string{"auth0|one"}, "", "auth0|one"},
		{[]string{"auth0|one", "auth0|two"}, "", ""},
		{[]string{"auth0|one", "device:one"}, "", "device:one"},
		{[]string{"auth0|one", "device:one", "device:two"}, "", ""},
		{[]string{"auth0|one", "device:one", "device:two"}, "device:two", "device:two"},
		{[]string{"auth0|one"}, "device:foreign", ""},
	} {
		got, err := selectAccountOwner(tc.owners, tc.requested)
		if err != nil && exitcode.FromError(err) != exitcode.Usage {
			t.Fatalf("owner selection exit = %d", exitcode.FromError(err))
		}
		if got != tc.want || (err != nil) != (tc.want == "") {
			t.Fatalf("select %v / %q = %q, %v", tc.owners, tc.requested, got, err)
		}
	}
}

// Command wiring stays independent of the browser protocol (tested with a real
// PKCE exchange in internal/api) and the native enrollment runtime tests.
func TestAccountCommandsRespectOutputAndAccountBoundaries(t *testing.T) {
	for _, command := range []string{"setup", "recover"} {
		for _, format := range []string{"text", "json", "quiet", "denied", "occupied"} {
			if command == "setup" && format == "occupied" {
				continue
			}
			t.Run(command+"/"+format, func(t *testing.T) {
				const owner = "device:command-owner"
				linked, enrolled, signedIn := false, false, false
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/v1/me":
						if r.Header.Get("Authorization") == "Bearer account-token" && r.Header.Get("X-QURL-Owner") != owner {
							t.Error("recovery identity used the wrong owner")
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"owner_id": owner, "auth_type": "api_key"}})
					case "/v1/account/owners":
						if r.Header.Get("Authorization") != "Bearer account-token" || r.Header.Get("X-QURL-Owner") != "" {
							t.Error("account discovery used device authority")
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"owners": []string{"auth0|account", "device:other", owner}})
					case "/v1/account/link":
						var body map[string]string
						if json.NewDecoder(r.Body).Decode(&body) != nil || body["account_token"] != "account-token" || r.Header.Get("Authorization") != "Bearer device-token" {
							t.Error("account linking lost one of its authorities")
						}
						linked = true
						_ = json.NewEncoder(w).Encode(map[string]string{"owner_id": owner, "account_id": "auth0|account"})
					default:
						t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				defer server.Close()
				device, err := qurlapi.New(&qurlapi.Config{BaseURL: server.URL, APIKey: "device-token"})
				if err != nil {
					t.Fatal(err)
				}
				var stdout, stderr bytes.Buffer
				root, opts := newRoot("test", &output.Streams{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, func(g *globalOpts) {
					g.lookupEnv = func(string) (string, bool) { return "", false }
					g.configDir = t.TempDir()
					stateDir := connectorStateTestDir(t)
					g.resolveShareStateDir = func(string) (string, error) { return stateDir, nil }
					g.openAPIClient = func(context.Context) (qurlapi.Client, error) { return device, nil }
					g.signInAccount = func(_ context.Context, cfg *qurlapi.Config, _ func(context.Context, string) error) (string, error) {
						signedIn = true
						if cfg.APIKey != "" || cfg.OwnerID != "" || cfg.BaseURL != server.URL {
							t.Error("browser sign-in carried device authority")
						}
						if format == "denied" {
							return "", errors.New("sign-in denied")
						}
						return "account-token", nil
					}
					g.openRegisteredClient = func(_ context.Context, account qurlapi.AccountClient, key string, identity *qurlapi.Identity) (qurlapi.Client, *qurlapi.Identity, error) {
						if account == nil || key != "" || identity == nil || identity.OwnerID != owner {
							t.Fatal("recovery did not select the requested account owner")
						}
						if format == "occupied" {
							return nil, nil, &deviceAccountConflictError{stateDir: stateDir, currentOwner: "device:existing", requestedOwner: owner}
						}
						enrolled = true
						return device, &qurlapi.Identity{OwnerID: owner, AuthType: "api_key"}, nil
					}
				})
				args := []string{"account", command, "--endpoint", server.URL}
				if command == "recover" {
					args = append(args, "--owner", owner)
				}
				if format == "json" {
					args = append(args, "--output", "json")
				}
				if format == "quiet" {
					args = append(args, "--quiet")
				}
				root.SetArgs(args)
				code := run(context.Background(), root, opts)
				if !signedIn {
					t.Fatal("account command did not invoke browser sign-in")
				}
				if format == "occupied" {
					if code != 4 || enrolled || linked || stdout.Len() != 0 || !strings.Contains(stderr.String(), "QURL_CONNECTOR_STATE_DIR") || strings.Contains(stderr.String(), "qurl login") {
						t.Fatalf("occupied recovery: exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
					}
					return
				}
				if format == "denied" {
					if code == 0 || linked || enrolled || stdout.Len() != 0 {
						t.Fatalf("denied sign-in changed account state: exit=%d linked=%v enrolled=%v stdout=%q", code, linked, enrolled, stdout.String())
					}
					return
				}
				if code != 0 || linked != (command == "setup") || enrolled != (command == "recover") {
					t.Fatalf("command exit=%d linked=%v enrolled=%v stderr=%s", code, linked, enrolled, stderr.String())
				}
				switch format {
				case "json":
					var result map[string]string
					if json.Unmarshal(stdout.Bytes(), &result) != nil || result["owner_id"] != owner {
						t.Fatalf("invalid JSON result: %s", stdout.String())
					}
					want := "linked"
					if command == "recover" {
						want = "recovered"
					}
					if result["status"] != want {
						t.Fatalf("status=%q", result["status"])
					}
				case "quiet":
					if stdout.String() != owner+"\n" || stderr.Len() != 0 {
						t.Fatalf("quiet stdout=%q stderr=%q", stdout.String(), stderr.String())
					}
				case "text":
					if stdout.Len() != 0 || !strings.Contains(stderr.String(), "Existing links are unchanged.") && !strings.Contains(stderr.String(), "Your existing links are unchanged.") {
						t.Fatalf("text stdout=%q stderr=%q", stdout.String(), stderr.String())
					}
				}
			})
		}
	}
}
