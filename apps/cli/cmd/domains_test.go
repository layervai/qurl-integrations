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
	"strconv"
	"strings"
	"testing"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

func domainCommandFixture() qurlapi.AccountDomain {
	ready := false
	return qurlapi.AccountDomain{Domain: "secure.example.com", Status: "pending_verification", VerificationToken: "fixture-dns-token", ReadyForQURLs: &ready, DNSRecords: []qurlapi.DomainDNSRecord{{Type: "TXT", Name: "_layerv-verify.secure.example.com", Value: "fixture-dns-token\n\x1b[31m", Verified: &ready}}}
}

func TestDomainCommandsUseFreshAccountAndActualReadiness(t *testing.T) {
	for _, operation := range []string{"list", "get", "add", "delete", "verify", "regenerate-token"} {
		for _, format := range []string{"text", "json", "quiet"} {
			t.Run(operation+"/"+format, func(t *testing.T) {
				signedIn, identified, performed := 0, 0, 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer fixture-browser-token" || r.Header.Get("X-QURL-Owner") != "" || r.Header.Get("Idempotency-Key") != "" {
						t.Error("wrong account authority or replay header")
					}
					w.Header().Set("Content-Type", "application/json")
					if r.URL.Path == "/v1/me" {
						identified++
						_, _ = w.Write([]byte(`{"data":{"owner_id":"auth0|fixture","auth_type":"jwt"}}`))
						return
					}
					performed++
					item := domainCommandFixture()
					path := "/v1/domains/secure.example.com"
					switch operation {
					case "list":
						if r.Method != http.MethodGet || r.URL.Path != "/v1/domains" || r.URL.Query().Get("limit") != "2" || r.URL.Query().Get("cursor") != "opaque&+/#?" {
							t.Error("wrong list route or cursor")
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"data": []qurlapi.AccountDomain{item}, "meta": map[string]any{"has_more": true, "next_cursor": "next-page"}})
						return
					case "get":
						if r.Method != http.MethodGet || r.URL.Path != path {
							t.Error("wrong get route")
						}
					case "add":
						if r.Method != http.MethodPost || r.URL.Path != "/v1/domains" {
							t.Error("wrong register route")
						}
						var body map[string]string
						if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 1 || body["domain"] != "secure.example.com" {
							t.Error("registration did not normalize the hostname or added fields")
						}
						w.WriteHeader(http.StatusCreated)
					case "delete":
						if r.Method != http.MethodDelete || r.URL.Path != path {
							t.Error("wrong delete route")
						}
						w.WriteHeader(http.StatusNoContent)
						return
					case "verify":
						if r.Method != http.MethodPost || r.URL.Path != path+"/verify" {
							t.Error("wrong verify route")
						}
						_, _ = w.Write([]byte(`{"data":{"domain":"secure.example.com","status":"pending_verification","checks":{"txt":{"verified":false,"error":"DNS not ready\n\u001b[31m"},"acme_cname":{"verified":false},"traffic_routing":{"verified":false}}}}`))
						return
					case "regenerate-token":
						if r.Method != http.MethodPost || r.URL.Path != path+"/regenerate-token" {
							t.Error("wrong regeneration route")
						}
						item.VerificationToken = "fixture-new-dns-token"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": item})
				}))
				defer server.Close()
				var stdout, stderr bytes.Buffer
				configDir := t.TempDir()
				root, _ := newRoot("test", &output.Streams{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, func(g *globalOpts) {
					g.configDir = configDir
					g.lookupEnv = func(name string) (string, bool) {
						if name == "QURL_API_KEY" || name == "QURL_API_KEY_FILE" {
							t.Errorf("account administration consulted %s", name)
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
						if operation != "list" && operation != "get" {
							expected = append(expected, accountAdminWriteScope)
						}
						if !slices.Equal(scopes, expected) || cfg.APIKey != "" || cfg.OwnerID != "" {
							t.Error("wrong browser consent or persisted authority")
						}
						return "fixture-browser-token", nil
					}
				})
				args := []string{"--endpoint", server.URL, "domains", operation}
				if operation == "list" {
					args = append(args, "--limit", "2", "--cursor", "opaque&+/#?")
				} else {
					args = append(args, " Secure.Example.COM ")
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
				if strings.Contains(stdout.String(), "\x1b") {
					t.Fatal("DNS text reached the terminal as a control sequence")
				}
				if format == "json" && !json.Valid(stdout.Bytes()) {
					t.Fatal("invalid JSON")
				}
				if format == "quiet" {
					expected := "secure.example.com\n"
					if operation == "verify" {
						expected = "pending_verification\n"
					}
					if stdout.String() != expected {
						t.Fatal("quiet output lost the actual domain or verification status")
					}
				}
				if format == "json" && operation == "verify" && (!strings.Contains(stdout.String(), `"verified": false`) || !strings.Contains(stdout.String(), "pending_verification")) {
					t.Fatal("incomplete DNS checks were reported as success")
				}
				files, err := os.ReadDir(configDir)
				if err != nil || len(files) != 0 {
					t.Fatal("account token persisted")
				}
			})
		}
	}
}

func TestDomainInvalidInputStopsBeforeBrowser(t *testing.T) {
	for _, args := range [][]string{
		{"list", "--limit", "101"}, {"get", "https://secure.example.com"}, {"delete", "../secret"},
		{"add", "*.example.com"}, {"verify", "example.com:443"}, {"regenerate-token", "127.0.0.1"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			root, _ := newRoot("test", &output.Streams{In: strings.NewReader(""), Out: &stdout, Err: &stderr}, func(g *globalOpts) {
				g.configDir = t.TempDir()
				g.signInAdmin = func(context.Context, *qurlapi.Config, func(context.Context, string) error, []string) (string, error) {
					t.Fatal("invalid domain opened browser")
					return "", nil
				}
			})
			root.SetArgs(append([]string{"domains"}, args...))
			if err := root.Execute(); err == nil {
				t.Fatal("invalid domain input accepted")
			}
		})
	}
}

func TestDomainWriteRecoveryAndEntitlementErrors(t *testing.T) {
	for _, operation := range []string{"add", "delete", "verify", "regenerate-token"} {
		for _, failure := range []string{"unavailable", "forbidden", "output"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/v1/me" {
						_, _ = w.Write([]byte(`{"data":{"owner_id":"auth0|fixture","auth_type":"jwt"}}`))
						return
					}
					calls++
					if failure == "unavailable" {
						w.WriteHeader(http.StatusServiceUnavailable)
						_, _ = w.Write([]byte(`{"title":"Fixture unavailable","status":503}`))
						return
					}
					if failure == "forbidden" {
						w.WriteHeader(http.StatusForbidden)
						_, _ = w.Write([]byte(`{"title":"Enterprise plan required","status":403}`))
						return
					}
					if operation == "delete" {
						w.WriteHeader(http.StatusNoContent)
						return
					}
					if operation == "verify" {
						_, _ = w.Write([]byte(`{"data":{"domain":"secure.example.com","status":"pending_verification","checks":{"txt":{"verified":false},"acme_cname":{"verified":false},"traffic_routing":{"verified":false}}}}`))
						return
					}
					if operation == "add" {
						w.WriteHeader(http.StatusCreated)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": domainCommandFixture()})
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
				root.SetArgs([]string{"--endpoint", server.URL, "--output", "json", "domains", operation, "secure.example.com"})
				err := root.Execute()
				if err == nil || calls != 1 {
					t.Fatalf("calls=%d error=%v", calls, err)
				}
				var rendered bytes.Buffer
				output.RenderError(&rendered, err, false)
				if failure == "forbidden" {
					if !strings.Contains(rendered.String(), "Enterprise plan required") || strings.Contains(rendered.String(), "outcome is uncertain") {
						t.Fatal("definitive entitlement error lost")
					}
				} else if !strings.Contains(rendered.String(), "qurl domains get secure.example.com") {
					t.Fatal("lost current-state recovery instructions")
				}
			})
		}
	}
}

func TestDomainMissingReadinessStaysUnknown(t *testing.T) {
	item := domainCommandFixture()
	item.ReadyForQURLs = nil
	var stdout, stderr bytes.Buffer
	printer := output.New(&output.Streams{Out: &stdout, Err: &stderr}, output.FormatText, false, false, true, nil)
	if err := printer.Domain(&item); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "unknown") {
		t.Fatal("missing readiness became false")
	}
	if !strings.Contains(stdout.String(), strconv.Quote(item.DNSRecords[0].Value)) {
		t.Fatal("DNS instruction text changed")
	}
}
