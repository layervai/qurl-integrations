package qurlapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

const accessCodeFixture = "ac_abcdefghijklmnopqrstuv"
const accessCodeIDFixture = "acd_abcdefghijk"

func TestAccessCodeCreateBindsResourceAndPolicy(t *testing.T) {
	key := apitest.FixedResourceKey(t)
	other := apitest.GenerateResourceKey(t)
	for _, variation := range []string{"valid", "wrong-resource", "wrong-name", "wrong-limit", "wrong-expiry", "used", "bad-secret", "revoked"} {
		t.Run(variation, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != "/v1/access-codes" || r.Header.Get("Idempotency-Key") != "" {
					t.Error("wrong creation request")
				}
				var input AccessCodeInput
				if json.NewDecoder(r.Body).Decode(&input) != nil || input.ResourceID != key.CRID || input.Name != "fixture" || input.MaxUses != 2 {
					t.Error("wrong creation body")
				}
				item := CreatedAccessCode{AccessCode: AccessCode{ID: accessCodeIDFixture, ResourceID: key.ResourceID, Name: input.Name, Status: "active", MaxUses: input.MaxUses, CreatedAt: time.Now().UTC()}, Code: accessCodeFixture}
				switch variation {
				case "wrong-resource":
					item.ResourceID = other.ResourceID
				case "wrong-name":
					item.Name = "other"
				case "wrong-limit":
					item.MaxUses = 0
				case "wrong-expiry":
					now := time.Now()
					item.ExpiresAt = &now
				case "used":
					item.UseCount = 1
				case "bad-secret":
					item.Code = "malformed"
				case "revoked":
					item.Status = "revoked"
				}
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(map[string]any{"data": item})
			}))
			defer server.Close()
			client, err := NewAccountManagement(&Config{BaseURL: server.URL, APIKey: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.CreateAccessCode(context.Background(), AccessCodeInput{ResourceID: key.CRID, Name: "fixture", MaxUses: 2})
			if variation == "valid" {
				if err != nil || result.Code != accessCodeFixture {
					t.Fatal("valid creation failed", err)
				}
			} else if !errors.Is(err, qurl.ErrInvalidAPIResponse) {
				t.Fatal("accepted mismatched creation", err)
			}
			if calls != 1 {
				t.Fatal("creation replayed")
			}
		})
	}
}

func TestAccessCodeListExcludesSecretsAndPreservesPage(t *testing.T) {
	key := apitest.FixedResourceKey(t)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("limit") != "1" || r.URL.Query().Get("cursor") != "opaque&+/#?" {
			t.Error("lost bounded page")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"access_code_id": accessCodeIDFixture, "resource_id": key.ResourceID, "name": "fixture", "status": "active", "max_uses": 0, "use_count": 2, "created_at": "2026-11-01T00:00:00Z", "code": accessCodeFixture, "code_hash": "never-output"}}, "meta": map[string]any{"has_more": true, "next_cursor": "next"}})
	}))
	defer server.Close()
	client, err := NewAccountManagement(&Config{BaseURL: server.URL, APIKey: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := client.ListAccessCodes(context.Background(), 1, "opaque&+/#?")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(page)
	if calls != 1 || !page.HasMore || page.NextCursor != "next" || strings.Contains(string(raw), accessCodeFixture) || strings.Contains(string(raw), "never-output") {
		t.Fatal("page traversal or secret boundary failed")
	}
}

func TestAccessCodeWritesNeverReplay(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; w.WriteHeader(status) }))
		client, err := NewAccountManagement(&Config{BaseURL: server.URL, APIKey: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = client.CreateAccessCode(context.Background(), AccessCodeInput{ResourceID: apitest.FixedResourceKey(t).ResourceID})
		_ = client.DeleteAccessCode(context.Background(), accessCodeIDFixture)
		if calls != 2 {
			t.Fatal("access code write replayed", calls)
		}
		server.Close()
	}
}

func TestAccessCodeRedemptionIsAnonymousAndSingleAttempt(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if r.Method != http.MethodPost || r.URL.Path != "/v1/access-codes/redeem" || r.Header.Get("Authorization") != "" || r.Header.Get("X-QURL-Owner") != "" || r.Header.Get("Idempotency-Key") != "" {
				t.Error("redemption used authority or wrong route")
			}
			var body struct {
				Code     string `json:"code"`
				Honeypot string `json:"honeypot"`
				Elapsed  int    `json:"elapsed_ms"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Code != accessCodeFixture || body.Honeypot != "" || body.Elapsed != 3100 {
				t.Error("wrong redemption body")
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"data":{"redirect_url":"https://qurl.link/#fixture"}}`))
		}))
		link, err := RedeemAccessCode(context.Background(), &Config{BaseURL: server.URL}, accessCodeFixture, 3100*time.Millisecond)
		if calls != 1 || (status == http.StatusOK && (err != nil || link != "https://qurl.link/#fixture")) || (status != http.StatusOK && err == nil) {
			t.Fatal("redemption failed or replayed", calls, err)
		}
		server.Close()
	}
}

func TestAccessCodeRedemptionBoundaries(t *testing.T) {
	for _, cfg := range []*Config{nil, {BaseURL: "http://api.example.com"}, {BaseURL: "https://example.com", APIKey: "fixture"}, {BaseURL: "https://example.com", OwnerID: "owner"}} {
		if _, err := RedeemAccessCode(context.Background(), cfg, accessCodeFixture, 3*time.Second); err == nil {
			t.Fatal("unsafe anonymous configuration accepted")
		}
	}
	if _, err := RedeemAccessCode(context.Background(), &Config{BaseURL: "https://example.com"}, accessCodeFixture, 0); !errors.Is(err, qurl.ErrInvalidResourceRequest) {
		t.Fatal("fabricated timing accepted")
	}
	for _, link := range []string{"https://layerv.ai", "https://qurl.link/", "javascript:alert(1)", "http://qurl.link/#fixture", "https://user:secret@qurl.link/#fixture", ""} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{"redirect_url": link}})
		}))
		_, err := RedeemAccessCode(context.Background(), &Config{BaseURL: server.URL}, accessCodeFixture, 3*time.Second)
		server.Close()
		if !errors.Is(err, qurl.ErrInvalidAPIResponse) {
			t.Fatal("invalid or fake redemption result accepted", link, err)
		}
	}
	if strings.Contains(Redact("failed "+accessCodeFixture), accessCodeFixture) || Redact(accessCodeIDFixture) != accessCodeIDFixture {
		t.Fatal("secret redaction changed public ID or exposed code")
	}
}

func TestAccessCodeInputValidation(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Second)
	key := apitest.FixedResourceKey(t)
	for _, input := range []AccessCodeInput{{ResourceID: "r_private"}, {ResourceID: key.ResourceID, MaxUses: -1}, {ResourceID: key.CRID, Name: strings.Repeat("a", 101)}, {ResourceID: key.ResourceID, ExpiresAt: &past}} {
		if ValidateAccessCodeInput(input, now) == nil {
			t.Fatal("invalid input accepted")
		}
	}
	if ValidateAccessCodeInput(AccessCodeInput{ResourceID: key.CRID, Name: strings.Repeat("界", 100)}, now) != nil {
		t.Fatal("Unicode character limit became a byte limit")
	}
	if ValidateAccessCodeID(accessCodeFixture) == nil || ValidateAccessCode(accessCodeIDFixture) == nil {
		t.Fatal("secret code and revocation ID confused")
	}
}
