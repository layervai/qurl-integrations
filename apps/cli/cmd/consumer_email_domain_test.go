package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

func TestConsumerEmailDomainConfigure(t *testing.T) {
	for _, returnedDomain := range []string{"example.com", "other.example"} {
		t.Run(returnedDomain, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Expected int64                     `json:"expected_consumer_access_version"`
					Policy   qurl.ConsumerAccessPolicy `json:"consumer_access"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Expected != 0 || body.Policy.Mode != "email_domain" || body.Policy.EmailDomain != "example.com" || body.Policy.Version != 1 {
					t.Fatalf("request=%+v error=%v", body, err)
				}
				body.Policy.EmailDomain = returnedDomain
				apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": true, "consumer_access": body.Policy}, nil)
			})
			got := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "access", "configure", srv.Key.CRID, "--mode", "email_domain", "--email-domain", "example.com", "--version", "0", "-o", "json"}})
			if returnedDomain == "example.com" {
				if got.code != 0 {
					t.Fatalf("exit=%d: %s", got.code, got.stderr.String())
				}
				var output struct {
					Policy struct {
						EmailDomain string `json:"email_domain"`
					} `json:"consumer_access"`
				}
				if err := json.Unmarshal(got.stdout.Bytes(), &output); err != nil || output.Policy.EmailDomain != "example.com" {
					t.Fatalf("output=%s error=%v", got.stdout.String(), err)
				}
			} else if got.code == 0 || got.stdout.Len() != 0 {
				t.Fatalf("unconfirmed domain reported success: %s", got.stdout.String())
			}
		})
	}
}

func TestPublishRejectsInvalidEmailDomainsBeforeAuthentication(t *testing.T) {
	for _, domain := range []string{"", "Example.com", "*.example.com", "user@example.com", "example.com.", "example..com", "-bad.example"} {
		t.Run(domain, func(t *testing.T) {
			got := runCLI(t, &runOpts{args: []string{"publish", "http://127.0.0.1:8080", "--access", "email_domain", "--email-domain", domain}})
			if got.code != 2 {
				t.Fatalf("exit=%d: %s", got.code, got.stderr.String())
			}
		})
	}
}

func TestPublishEmailDomain(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Private *bool                     `json:"private"`
			Policy  qurl.ConsumerAccessPolicy `json:"consumer_access"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Private == nil || !*body.Private || body.Policy.Mode != "email_domain" || body.Policy.EmailDomain != "example.com" || body.Policy.Version != 1 {
			t.Fatalf("publish request=%+v error=%v", body, err)
		}
		apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": true, "consumer_access": body.Policy}, nil)
	})
	got := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", "https://example.com/site", "--name", "Example", "--access", "email_domain", "--email-domain", "example.com", "-o", "json"}})
	if got.code != 0 {
		t.Fatalf("exit=%d: %s", got.code, got.stderr.String())
	}
	var output struct {
		Policy qurl.ConsumerAccessPolicy `json:"consumer_access"`
	}
	if err := json.Unmarshal(got.stdout.Bytes(), &output); err != nil || output.Policy.EmailDomain != "example.com" {
		t.Fatalf("output=%s error=%v", got.stdout.String(), err)
	}
}
