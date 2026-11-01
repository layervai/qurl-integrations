package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

const commandAccessCode = "ac_abcdefghijklmnopqrstuv"
const commandAccessCodeID = "acd_abcdefghijk"

func TestAccessCodeCommandsUseAccountAuthorityAndSafeOutput(t *testing.T) {
	key := apitest.FixedResourceKey(t)
	for _, operation := range []string{"list", "create", "delete"} {
		for _, format := range []string{"text", "json", "quiet"} {
			t.Run(operation+"/"+format, func(t *testing.T) {
				identified, performed, signedIn := 0, 0, 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer fixture-browser-token" || r.Header.Get("X-QURL-Owner") != "" || r.Header.Get("Idempotency-Key") != "" {
						t.Error("wrong account authority")
					}
					if r.URL.Path == "/v1/me" {
						identified++
						_, _ = w.Write([]byte(`{"data":{"owner_id":"auth0|fixture","auth_type":"jwt"}}`))
						return
					}
					performed++
					item := qurlapi.AccessCode{ID: commandAccessCodeID, ResourceID: key.ResourceID, Name: "fixture\n\x1b[31m", Status: "active", MaxUses: 2, CreatedAt: time.Now().UTC().Truncate(time.Second)}
					switch operation {
					case "list":
						if r.Method != http.MethodGet || r.URL.Path != "/v1/access-codes" || r.URL.Query().Get("limit") != "2" || r.URL.Query().Get("cursor") != "opaque&+/#?" {
							t.Error("wrong bounded list")
						}
						raw, _ := json.Marshal(item)
						var row map[string]any
						_ = json.Unmarshal(raw, &row)
						row["code"] = commandAccessCode
						_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{row}, "meta": map[string]any{"has_more": true, "next_cursor": "next"}})
					case "create":
						var input qurlapi.AccessCodeInput
						if r.Method != http.MethodPost || r.URL.Path != "/v1/access-codes" || json.NewDecoder(r.Body).Decode(&input) != nil || input.ResourceID != key.CRID || input.Name != item.Name || input.MaxUses != 2 {
							t.Error("wrong creation")
						}
						w.WriteHeader(http.StatusCreated)
						_ = json.NewEncoder(w).Encode(map[string]any{"data": qurlapi.CreatedAccessCode{AccessCode: item, Code: commandAccessCode}})
					case "delete":
						if r.Method != http.MethodDelete || r.URL.Path != "/v1/access-codes/"+commandAccessCodeID {
							t.Error("wrong revocation")
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
							t.Error("default account mode read ambient key")
						}
						return "", false
					}
					g.openAPIClient = func(context.Context) (qurlapi.Client, error) {
						t.Fatal("opened device client")
						return nil, errors.New("forbidden")
					}
					g.signInAdmin = func(_ context.Context, cfg *qurlapi.Config, _ func(context.Context, string) error, scopes []string) (string, error) {
						signedIn++
						expected := []string{accountAdminReadScope}
						if operation != "list" {
							expected = append(expected, accountAdminWriteScope)
						}
						if cfg.APIKey != "" || cfg.OwnerID != "" || !slices.Equal(scopes, expected) {
							t.Error("wrong browser scopes")
						}
						return "fixture-browser-token", nil
					}
				})
				args := []string{"--endpoint", server.URL, "access-codes", operation}
				switch operation {
				case "list":
					args = append(args, "--limit", "2", "--cursor", "opaque&+/#?")
				case "create":
					args = append(args, key.CRID, "--name", "fixture\n\x1b[31m", "--max-uses", "2")
				case "delete":
					args = append(args, commandAccessCodeID)
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
				if identified != 1 || performed != 1 || signedIn != 1 {
					t.Fatal("wrong authority or request count")
				}
				if strings.Contains(stdout.String(), "\x1b") || strings.Contains(stderr.String(), commandAccessCode) {
					t.Fatal("unsafe output")
				}
				if operation != "create" && strings.Contains(stdout.String(), commandAccessCode) {
					t.Fatal("metadata leaked secret")
				}
				if format == "json" && !json.Valid(stdout.Bytes()) {
					t.Fatal("invalid JSON")
				}
				if format == "quiet" {
					expected := commandAccessCodeID + "\n"
					if operation == "create" {
						expected = commandAccessCode + "\n"
					}
					if stdout.String() != expected {
						t.Fatal("wrong quiet output")
					}
				}
				files, err := os.ReadDir(configDir)
				if err != nil || len(files) != 0 {
					t.Fatal("account credential persisted")
				}
			})
		}
	}
}

func TestAccessCodeRedeemMeasuresInputAndNeverLoadsAuthority(t *testing.T) {
	for _, format := range []string{"text", "json", "quiet"} {
		t.Run(format, func(t *testing.T) {
			started := time.Now()
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var body struct {
					Code    string `json:"code"`
					Elapsed int64  `json:"elapsed_ms"`
				}
				if r.URL.Path != "/v1/access-codes/redeem" || r.Header.Get("Authorization") != "" || r.Header.Get("X-QURL-Owner") != "" || json.NewDecoder(r.Body).Decode(&body) != nil || body.Code != commandAccessCode || body.Elapsed < 3000 || time.Since(started) < 3*time.Second {
					t.Error("wrong anonymous request or fabricated elapsed time")
				}
				_, _ = w.Write([]byte(`{"data":{"redirect_url":"https://qurl.link/#fixture"}}`))
			}))
			defer server.Close()
			var stdout, stderr bytes.Buffer
			root, _ := newRoot("test", &output.Streams{In: strings.NewReader(commandAccessCode + "\n"), Out: &stdout, Err: &stderr}, func(g *globalOpts) {
				g.configDir = t.TempDir()
				g.lookupEnv = func(name string) (string, bool) {
					if name == "QURL_API_KEY" || name == "QURL_API_KEY_FILE" {
						t.Error("redemption read ambient key")
					}
					return "", false
				}
				g.signInAdmin = func(context.Context, *qurlapi.Config, func(context.Context, string) error, []string) (string, error) {
					t.Fatal("redemption opened sign-in")
					return "", nil
				}
				g.openAPIClient = func(context.Context) (qurlapi.Client, error) {
					t.Fatal("redemption loaded device")
					return nil, errors.New("device forbidden")
				}
			})
			args := []string{"--endpoint", server.URL, "access-codes", "redeem"}
			if format == "json" {
				args = append(args, "-o", "json")
			}
			if format == "quiet" {
				args = append(args, "-q")
			}
			root.SetArgs(args)
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || !strings.Contains(stdout.String(), "https://qurl.link/#fixture") || strings.Contains(stderr.String(), commandAccessCode) {
				t.Fatal("redemption output boundary failed")
			}
			if format == "json" && !json.Valid(stdout.Bytes()) {
				t.Fatal("invalid JSON")
			}
		})
	}
}

func TestAccessCodeInvalidInputAndCanceledWaitDoNotSend(t *testing.T) {
	for _, args := range [][]string{{"create", "r_private"}, {"create", apitest.FixedResourceKey(t).CRID, "--max-uses", "-1"}, {"create", apitest.FixedResourceKey(t).CRID, "--expires-at", "2000-01-01T00:00:00Z"}, {"delete", commandAccessCode}, {"list", "--limit", "101"}, {"redeem", commandAccessCode}, {"redeem", "--api-key-auth"}, {"redeem"}} {
		var stdout, stderr bytes.Buffer
		root, _ := newRoot("test", &output.Streams{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, func(g *globalOpts) {
			g.configDir = t.TempDir()
			g.signInAdmin = func(context.Context, *qurlapi.Config, func(context.Context, string) error, []string) (string, error) {
				t.Fatal("invalid input opened browser")
				return "", nil
			}
		})
		root.SetArgs(append([]string{"access-codes"}, args...))
		if root.Execute() == nil {
			t.Fatal("invalid input accepted", args)
		}
	}
	var stdout, stderr bytes.Buffer
	root, _ := newRoot("test", &output.Streams{In: strings.NewReader(commandAccessCode), Out: &stdout, Err: &stderr}, func(g *globalOpts) { g.configDir = t.TempDir() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root.SetArgs([]string{"access-codes", "redeem"})
	if !errors.Is(root.ExecuteContext(ctx), context.Canceled) {
		t.Fatal("canceled input wait was ignored")
	}
}

func TestAccessCodeRecoverySurvivesFinalErrorRendering(t *testing.T) {
	for _, err := range []error{accessCodeWriteFailure(&qurlapi.Error{StatusCode: 503, Title: "Unavailable"}), accessCodeRedeemFailure(&qurlapi.Error{StatusCode: 503, Title: "Unavailable"}), &accessCodeResultError{cause: errors.New("output failure"), message: "Code created; revoke " + commandAccessCodeID}} {
		var stderr bytes.Buffer
		output.RenderError(&stderr, err, false)
		if !strings.Contains(stderr.String(), err.Error()) {
			t.Fatal("final error renderer discarded recovery guidance")
		}
	}
}

func TestAccessCodeCreateFailureKeepsRecoveryAndTierErrors(t *testing.T) {
	key := apitest.FixedResourceKey(t)
	for _, failure := range []string{"unavailable", "forbidden", "output"} {
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
						_, _ = w.Write([]byte(`{"title":"Access codes require system-tier account","code":"forbidden"}`))
					} else {
						w.WriteHeader(http.StatusServiceUnavailable)
						_, _ = w.Write([]byte(`{"title":"Unavailable"}`))
					}
					return
				}
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]any{"data": qurlapi.CreatedAccessCode{AccessCode: qurlapi.AccessCode{ID: commandAccessCodeID, ResourceID: key.ResourceID, Status: "active", CreatedAt: time.Now().UTC()}, Code: commandAccessCode}})
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
			root.SetArgs([]string{"--endpoint", server.URL, "access-codes", "create", key.CRID, "--output", "json"})
			err := root.Execute()
			if err == nil || calls != 1 {
				t.Fatal("creation failure replayed or ignored", calls, err)
			}
			var rendered bytes.Buffer
			output.RenderError(&rendered, err, false)
			switch failure {
			case "forbidden":
				if !strings.Contains(rendered.String(), "system-tier") || strings.Contains(rendered.String(), "uncertain") {
					t.Fatal("definitive tier error lost")
				}
			case "output":
				if !strings.Contains(rendered.String(), "delete "+commandAccessCodeID) || !strings.Contains(rendered.String(), "creation succeeded") {
					t.Fatal("known created ID lost")
				}
			case "unavailable":
				if !strings.Contains(rendered.String(), "qurl access-codes list") {
					t.Fatal("unknown creation recovery lost")
				}
			}
			if strings.Contains(rendered.String(), commandAccessCode) {
				t.Fatal("recovery leaked secret")
			}
		})
	}
}
