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

const fixtureWebhookID = "wh_0123456789abcdef"

func TestWebhookWritesNeverRetryOrSendReplayKey(t *testing.T) {
	endpoint, description, status := "https://receiver.example/hook", "", "disabled"
	events := []string{"qurl.created"}
	for _, operation := range []string{"create", "update", "delete", "rotate"} {
		t.Run(operation, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Idempotency-Key") != "" || r.Header.Get("X-QURL-Owner") != "" {
					t.Error("unexpected replay or device-owner header")
				}
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"type":"https://api.layerv.ai/problems/unavailable","title":"Try later","status":503}`))
			}))
			defer server.Close()
			// The real transport's 503 retry path is active; write methods must bypass it.
			client, err := NewAccountManagement(&Config{BaseURL: server.URL, APIKey: "fixture-account-token"})
			if err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "create":
				_, err = client.CreateWebhook(context.Background(), WebhookInput{URL: &endpoint, Events: &events})
			case "update":
				_, err = client.UpdateWebhook(context.Background(), fixtureWebhookID, WebhookInput{Description: &description, Status: &status})
			case "delete":
				err = client.DeleteWebhook(context.Background(), fixtureWebhookID)
			case "rotate":
				_, err = client.RotateWebhookSecret(context.Background(), fixtureWebhookID)
			}
			var apiError *Error
			if calls != 1 || !errors.As(err, &apiError) || apiError.StatusCode != 503 {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
		})
	}
}

func TestWebhookMetadataRejectsMalformedReplies(t *testing.T) {
	for _, body := range []string{
		`null`, `{}`, `{"data":null,"meta":{"has_more":false}}`, `{"data":[]}`,
		`{"data":[],"meta":{}}`, `{"data":[],"meta":{"has_more":true}}`,
		`{"data":[{"webhook_id":"../secret","owner_id":"fixture","url":"https://receiver.example","status":"active"}],"meta":{"has_more":false}}`,
	} {
		t.Run(body, func(t *testing.T) {
			reply := &restReply{status: http.StatusOK, body: []byte(body)}
			page, err := decodeAccountPage[Webhook](reply, 1)
			if err == nil {
				for _, item := range page.Data {
					if !validWebhook(&item) {
						err = qurl.ErrInvalidAPIResponse
					}
				}
			}
			if !errors.Is(err, qurl.ErrInvalidAPIResponse) {
				t.Fatal("accepted incomplete metadata")
			}
		})
	}
	page, err := decodeAccountPage[Webhook](&restReply{status: http.StatusOK, body: []byte(`{"data":[],"meta":{"has_more":false}}`)}, 1)
	if err != nil || len(page.Data) != 0 || page.HasMore {
		t.Fatal("rejected explicit empty final page")
	}
}

func TestWebhookInputAndAuthorityBoundaries(t *testing.T) {
	if _, err := NewAccountManagement(nil); err == nil {
		t.Fatal("nil configuration accepted")
	}
	if _, err := NewAccountManagement(&Config{OwnerID: "device:fixture"}); err == nil {
		t.Fatal("device owner accepted")
	}
	endpoint, description, status := "https://receiver.example/hook", strings.Repeat("x", 501), "revoked"
	events := []string{"qurl.created"}
	for _, input := range []WebhookInput{
		{}, {URL: &endpoint}, {URL: &endpoint, Events: &events, Status: &status}, {URL: &endpoint, Events: &events, Description: &description},
	} {
		if err := ValidateWebhookInput(input, true); err == nil {
			t.Fatal("invalid create input accepted")
		}
	}
	if err := ValidateWebhookInput(WebhookInput{Status: &status}, false); err == nil {
		t.Fatal("invalid update status accepted")
	}
	description = strings.Repeat("😀", 500)
	if err := ValidateWebhookInput(WebhookInput{Description: &description}, false); err != nil {
		t.Fatal("description counts UTF-8 bytes instead of characters")
	}
	description += "😀"
	if err := ValidateWebhookInput(WebhookInput{Description: &description}, false); err == nil {
		t.Fatal("accepted 501-character description")
	}
	description = "line one\nline two"
	if err := ValidateWebhookInput(WebhookInput{Description: &description}, false); err != nil {
		t.Fatal("valid multiline description rejected")
	}
	description = ""
	if err := ValidateWebhookInput(WebhookInput{Description: &description}, false); err != nil {
		t.Fatal("cannot clear description")
	}
	for _, raw := range []string{"https://receiver.example/hook#fragment", "https://receiver.example/\nsecret", "http://receiver.example", "https://user:pass@receiver.example", "https:///hook", strings.Repeat("x", 2049)} {
		if err := ValidateWebhookInput(WebhookInput{URL: &raw, Events: &events}, true); err == nil {
			t.Fatal("invalid URL accepted")
		}
	}
}

func TestWebhookDiagnosticRedaction(t *testing.T) {
	secret := "whsec_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"
	for _, text := range []string{secret, "server echoed " + secret, "Bearer " + secret} {
		if strings.Contains(Redact(text), secret) {
			t.Fatal("signing secret escaped diagnostic redaction")
		}
	}
	metadata := Webhook{ID: fixtureWebhookID, OwnerID: "auth0|fixture", URL: "https://receiver.example/hook", Status: "active"}
	raw, _ := json.Marshal(metadata)
	if strings.Contains(string(raw), "secret") {
		t.Fatal("metadata contains signing-secret field")
	}
}
