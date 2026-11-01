package qurlapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/layervai/qurl-go/qurl"
)

func TestAccountDomainNormalization(t *testing.T) {
	for _, raw := range []string{"secure.example.com", " Secure.Example.COM ", "\nsecure.example.com\t"} {
		name, err := NormalizeAccountDomain(raw)
		if err != nil || name != "secure.example.com" {
			t.Fatalf("normalized=%q error=%v", name, err)
		}
	}
	for _, raw := range []string{"", "example", "example.com.", "https://example.com", "example.com/path", "example.com?secret=value", "example.com#fragment", "*.example.com", "example.com:443", "127.0.0.1", "[::1]", "-bad.example.com", "bad-.example.com", strings.Repeat("a", 64) + ".example.com", strings.Repeat("a.", 126) + "com"} {
		if _, err := NormalizeAccountDomain(raw); !errors.Is(err, qurl.ErrInvalidResourceRequest) {
			t.Fatalf("invalid hostname accepted: %q", raw)
		}
	}
}

func TestAccountDomainRejectsIncompleteOrMismatchedMetadata(t *testing.T) {
	for _, body := range []string{
		`null`, `{}`, `{"data":{}}`,
		`{"data":{"domain":"other.example.com","status":"active","verification_token":"fixture","dns_records":[{"type":"TXT","name":"fixture","value":"fixture"}]}}`,
		`{"data":{"domain":"secure.example.com","status":"active","verification_token":"fixture","dns_records":[]}}`,
		`{"data":{"domain":"secure.example.com","status":"active","dns_records":[{"type":"TXT","name":"fixture","value":"fixture"}]}}`,
		`{"data":{"domain":"secure.example.com","status":"active\n\u001b[31m","verification_token":"fixture","dns_records":[{"type":"TXT","name":"fixture","value":"fixture"}]}}`,
	} {
		t.Run(body, func(t *testing.T) {
			_, err := decodeAccountDomain(&restReply{status: http.StatusOK, body: []byte(body)}, http.StatusOK, "secure.example.com")
			if !errors.Is(err, qurl.ErrInvalidAPIResponse) {
				t.Fatal("accepted incomplete or mismatched metadata")
			}
		})
	}
	raw := []byte(`{"data":{"domain":"secure.example.com","status":"future_status","verification_token":"fixture","dns_records":[{"type":"TXT","name":"fixture","value":"fixture"}]}}`)
	item, err := decodeAccountDomain(&restReply{status: http.StatusOK, body: raw}, http.StatusOK, "secure.example.com")
	if err != nil || item.ReadyForQURLs != nil || item.DNSRecords[0].Verified != nil {
		t.Fatal("missing optional readiness became an invented value")
	}
	encoded, _ := json.Marshal(item)
	if strings.Contains(string(encoded), `"ready_for_qurls"`) {
		t.Fatal("invented JSON readiness")
	}
}

func TestDomainVerificationRequiresAllActualChecks(t *testing.T) {
	for _, body := range []string{
		`{"data":{"domain":"secure.example.com","status":"pending_verification"}}`,
		`{"data":{"domain":"secure.example.com","status":"pending_verification","checks":{"txt":{"verified":false},"acme_cname":{"verified":false}}}}`,
		`{"data":{"domain":"other.example.com","status":"active","checks":{"txt":{"verified":true},"acme_cname":{"verified":true},"traffic_routing":{"verified":true}}}}`,
		`{"data":{"domain":"secure.example.com","status":"active\n\u001b[31m","checks":{"txt":{"verified":true},"acme_cname":{"verified":true},"traffic_routing":{"verified":true}}}}`,
	} {
		t.Run(body, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			client, err := NewAccountManagement(&Config{BaseURL: server.URL, APIKey: "fixture-account-token"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.VerifyAccountDomain(context.Background(), "secure.example.com")
			if !errors.Is(err, qurl.ErrInvalidAPIResponse) || calls != 1 {
				t.Fatal("accepted missing checks or retried verification")
			}
		})
	}
}

func TestDomainListKeepsOneExplicitPage(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("limit") != "1" {
			t.Error("unbounded page")
		}
		_, _ = w.Write([]byte(`{"data":[],"meta":{"has_more":false}}`))
	}))
	defer server.Close()
	client, err := NewAccountManagement(&Config{BaseURL: server.URL, APIKey: "fixture-account-token"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := client.ListDomains(context.Background(), 1, "")
	if err != nil || calls != 1 || page.HasMore || page.Data == nil {
		t.Fatal("lost explicit empty final page")
	}
	if _, err := client.GetAccountDomain(context.Background(), "../secret"); !errors.Is(err, qurl.ErrInvalidResourceRequest) || calls != 1 {
		t.Fatal("unsafe path reached the transport")
	}
}
