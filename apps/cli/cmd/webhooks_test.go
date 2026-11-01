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
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

const webhookCommandID = "wh_0123456789abcdef"
const webhookCommandSecret = "whsec_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"

func TestWebhookCommandsUseAccountAuthorityAndSafeOutput(t *testing.T) {
	for _, operation := range []string{"list", "get", "create", "update", "delete", "rotate-secret", "events", "deliveries"} {
		for _, format := range []string{"text", "json", "quiet"} {
			t.Run(operation+"/"+format, func(t *testing.T) {
				signedIn, identified, performed := 0, 0, 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer fixture-browser-token" || r.Header.Get("X-QURL-Owner") != "" || r.Header.Get("Idempotency-Key") != "" {
						t.Error("wrong browser authority, device owner, or retry header")
					}
					w.Header().Set("Content-Type", "application/json")
					if r.URL.Path == "/v1/me" {
						identified++
						_, _ = w.Write([]byte(`{"data":{"owner_id":"auth0|fixture","auth_type":"jwt"}}`))
						return
					}
					performed++
					item := qurlapi.Webhook{ID: webhookCommandID, OwnerID: "auth0|fixture", URL: "https://receiver.example/hook", Events: []string{"qurl.created"}, Status: "active", Description: "receiver"}
					switch operation {
					case "list":
						if r.Method != http.MethodGet || r.URL.Path != "/v1/webhooks" || r.URL.Query().Get("limit") != "2" || r.URL.Query().Get("cursor") != "opaque&+/#?" {
							t.Error("wrong list route or query")
						}
						metadata, _ := json.Marshal(item)
						var unsafe map[string]any
						_ = json.Unmarshal(metadata, &unsafe)
						unsafe["secret"] = webhookCommandSecret
						unsafe["description"] = "receiver\n\x1b[31m"
						_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{unsafe}, "meta": map[string]any{"has_more": true, "next_cursor": "opaque-next"}})
					case "get":
						if r.Method != http.MethodGet || r.URL.Path != "/v1/webhooks/"+webhookCommandID {
							t.Error("wrong get route")
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"data": qurlapi.WebhookSecret{Webhook: item, Secret: webhookCommandSecret}})
					case "create":
						if r.Method != http.MethodPost || r.URL.Path != "/v1/webhooks" {
							t.Error("wrong create route")
						}
						var body map[string]any
						if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 3 || body["url"] != item.URL || body["description"] != item.Description {
							t.Error("wrong create body")
						}
						w.WriteHeader(http.StatusCreated)
						_ = json.NewEncoder(w).Encode(map[string]any{"data": qurlapi.WebhookSecret{Webhook: item, Secret: webhookCommandSecret}})
					case "update":
						if r.Method != http.MethodPatch || r.URL.Path != "/v1/webhooks/"+webhookCommandID {
							t.Error("wrong update route")
						}
						var body map[string]any
						if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 2 || body["description"] != "" || body["status"] != "disabled" {
							t.Error("update did not preserve explicit empty description or changed omitted fields")
						}
						item.Description, item.Status = "", "disabled"
						_ = json.NewEncoder(w).Encode(map[string]any{"data": qurlapi.WebhookSecret{Webhook: item, Secret: webhookCommandSecret}})
					case "delete":
						if r.Method != http.MethodDelete || r.URL.Path != "/v1/webhooks/"+webhookCommandID {
							t.Error("wrong delete route")
						}
						w.WriteHeader(http.StatusNoContent)
					case "rotate-secret":
						if r.Method != http.MethodPost || r.URL.Path != "/v1/webhooks/"+webhookCommandID+"/secret" {
							t.Error("wrong rotate route")
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"data": qurlapi.WebhookSecret{Webhook: item, Secret: webhookCommandSecret}})
					case "events":
						if r.Method != http.MethodGet || r.URL.Path != "/v1/webhooks/events" {
							t.Error("wrong catalog route")
						}
						_, _ = w.Write([]byte(`{"data":[{"type":"resource.closed","category":"resource","description":"Not yet delivered.\n\u001b[31m"}]}`))
					case "deliveries":
						if r.Method != http.MethodGet || r.URL.Path != "/v1/webhooks/"+webhookCommandID+"/deliveries" || r.URL.Query().Get("limit") != "2" || r.URL.Query().Get("cursor") != "opaque&+/#?" {
							t.Error("wrong deliveries route or query")
						}
						_, _ = fmt.Fprintf(w, `{"data":[{"delivery_id":"wd_0123456789abcdef","webhook_id":%q,"event_type":"qurl.created","status":"failed","response_code":500,"response_body":%q,"error_message":%q}],"meta":{"has_more":false}}`, webhookCommandID, webhookCommandSecret, webhookCommandSecret)
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
						t.Error("opened device authority")
						return nil, errors.New("device forbidden")
					}
					g.signInAdmin = func(_ context.Context, cfg *qurlapi.Config, _ func(context.Context, string) error, scopes []string) (string, error) {
						signedIn++
						expected := []string{accountAdminReadScope}
						if slices.Contains([]string{"create", "update", "delete", "rotate-secret"}, operation) {
							expected = append(expected, accountAdminWriteScope)
						}
						if !slices.Equal(scopes, expected) || cfg.APIKey != "" || cfg.OwnerID != "" {
							t.Error("wrong browser scope request or persisted authority")
						}
						return "fixture-browser-token", nil
					}
				})
				args := []string{"--endpoint", server.URL, "webhooks", operation}
				switch operation {
				case "list":
					args = append(args, "--limit", "2", "--cursor", "opaque&+/#?")
				case "get", "delete", "rotate-secret":
					args = append(args, webhookCommandID)
				case "create":
					args = append(args, "--url", "https://receiver.example/hook", "--events", "qurl.created", "--description", "receiver")
				case "update":
					args = append(args, webhookCommandID, "--description", "", "--status", "disabled")
				case "deliveries":
					args = append(args, webhookCommandID, "--limit", "2", "--cursor", "opaque&+/#?")
				}
				if format == "json" {
					args = append(args, "--output", "json")
				}
				if format == "quiet" {
					args = append(args, "--quiet")
				}
				root.SetArgs(args)
				if err := root.Execute(); err != nil {
					t.Fatal(err)
				}
				if signedIn != 1 || identified != 1 || performed != 1 {
					t.Fatalf("sign-ins=%d identities=%d operations=%d", signedIn, identified, performed)
				}
				if strings.Contains(stderr.String(), webhookCommandSecret) || strings.Contains(stdout.String(), "\x1b") {
					t.Fatal("secret in diagnostics or terminal control in data")
				}
				secretOutput := operation == "create" || operation == "rotate-secret"
				if strings.Contains(stdout.String(), webhookCommandSecret) != secretOutput {
					t.Fatal("wrong one-time secret boundary")
				}
				if format == "json" && !json.Valid(stdout.Bytes()) {
					t.Fatal("invalid JSON output")
				}
				if format == "quiet" && secretOutput && stdout.String() != webhookCommandSecret+"\n" {
					t.Fatal("quiet secret output changed")
				}
				files, err := os.ReadDir(configDir)
				if err != nil || len(files) != 0 {
					t.Fatal("account authority persisted")
				}
			})
		}
	}
}

func TestWebhookInvalidInputStopsBeforeBrowser(t *testing.T) {
	for _, args := range [][]string{
		{"get", "../secret"}, {"rotate-secret", webhookCommandSecret}, {"list", "--limit", "101"},
		{"deliveries", webhookCommandID, "--limit", "0"}, {"update", webhookCommandID},
		{"create", "--url", "http://receiver.example", "--events", "qurl.created"},
		{"create", "--url", "https://user:pass@receiver.example", "--events", "qurl.created"},
		{"create", "--url", "https://receiver.example", "--events", "unknown.event"},
		{"create", "--url", "https://receiver.example", "--events", "qurl.created,qurl.created"},
		{"update", webhookCommandID, "--status", "revoked"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			root, _ := newRoot("test", &output.Streams{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, func(g *globalOpts) {
				g.configDir = t.TempDir()
				g.signInAdmin = func(context.Context, *qurlapi.Config, func(context.Context, string) error, []string) (string, error) {
					t.Fatal("invalid input opened browser")
					return "", nil
				}
			})
			root.SetArgs(append([]string{"webhooks"}, args...))
			if err := root.Execute(); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
}

func TestWebhookSecretFailureKeepsRecoveryInstructions(t *testing.T) {
	for _, operation := range []string{"create", "rotate-secret"} {
		for _, failure := range []string{"unavailable", "malformed", "wrong-owner", "output", "forbidden"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				performed := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/v1/me" {
						_, _ = w.Write([]byte(`{"data":{"owner_id":"auth0|fixture","auth_type":"jwt"}}`))
						return
					}
					performed++
					w.Header().Set("Content-Type", "application/json")
					switch failure {
					case "unavailable":
						w.WriteHeader(http.StatusServiceUnavailable)
						_, _ = w.Write([]byte(`{"title":"Fixture unavailable","status":503}`))
						return
					case "forbidden":
						w.WriteHeader(http.StatusForbidden)
						_, _ = fmt.Fprintf(w, `{"title":"Fixture permission denied","detail":%q,"status":403}`, webhookCommandSecret)
						return
					case "malformed":
						_, _ = w.Write([]byte(`{"data":{}}`))
						return
					}
					item := qurlapi.Webhook{ID: webhookCommandID, OwnerID: "auth0|fixture", URL: "https://receiver.example/hook", Events: []string{"qurl.created"}, Status: "active"}
					if failure == "wrong-owner" {
						item.OwnerID = "other-account"
					}
					if operation == "create" {
						w.WriteHeader(http.StatusCreated)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": qurlapi.WebhookSecret{Webhook: item, Secret: webhookCommandSecret}})
				}))
				defer server.Close()
				var stdout, stderr bytes.Buffer
				streams := &output.Streams{In: strings.NewReader(""), Out: &stdout, Err: &stderr}
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
				args := []string{"--endpoint", server.URL, "--output", "json", "webhooks", operation}
				if operation == "create" {
					args = append(args, "--url", "https://receiver.example/hook", "--events", "qurl.created")
				} else {
					args = append(args, webhookCommandID)
				}
				root.SetArgs(args)
				err := root.Execute()
				if err == nil || performed != 1 {
					t.Fatalf("calls=%d error=%v", performed, err)
				}
				var rendered bytes.Buffer
				output.RenderError(&rendered, err, false)
				if strings.Contains(rendered.String(), webhookCommandSecret) || strings.Contains(stderr.String(), webhookCommandSecret) {
					t.Fatal("secret escaped into error rendering")
				}
				switch {
				case failure == "forbidden":
					if !strings.Contains(rendered.String(), "Fixture permission denied") || strings.Contains(rendered.String(), "outcome is uncertain") {
						t.Fatal("definitive denial lost its actionable error")
					}
				case operation == "create" && (failure == "unavailable" || failure == "malformed"):
					if !strings.Contains(rendered.String(), "qurl webhooks list") {
						t.Fatal("unknown create lost discovery instructions")
					}
				default:
					if !strings.Contains(rendered.String(), "rotate-secret "+webhookCommandID) {
						t.Fatal("lost known webhook secret recovery instructions")
					}
				}
			})
		}
	}
}

func TestWebhookAccountIdentityMustBeBrowserJWT(t *testing.T) {
	for _, identity := range []string{`{"owner_id":"device:fixture","auth_type":"api_key"}`, `{"owner_id":"auth0|fixture","auth_type":"jwt","api_key":{"key_id":"fixture"}}`, `{"owner_id":"auth0|fixture"}`} {
		t.Run(identity, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/me" {
					t.Error("operation ran with invalid account identity")
				}
				_, _ = fmt.Fprintf(w, `{"data":%s}`, identity)
			}))
			defer server.Close()
			var stdout, stderr bytes.Buffer
			root, _ := newRoot("test", &output.Streams{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, func(g *globalOpts) {
				g.configDir = t.TempDir()
				g.lookupEnv = func(string) (string, bool) { return "", false }
				g.signInAdmin = func(context.Context, *qurlapi.Config, func(context.Context, string) error, []string) (string, error) {
					return "fixture-browser-token", nil
				}
			})
			root.SetArgs([]string{"--endpoint", server.URL, "webhooks", "list"})
			if err := root.Execute(); err == nil {
				t.Fatal("accepted non-browser account identity")
			}
		})
	}
}
