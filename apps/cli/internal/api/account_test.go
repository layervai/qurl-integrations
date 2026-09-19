package qurlapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAccountOwnerSelectionAndLinkProof(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer device-token" || r.Header.Get("X-QURL-Owner") != "device:owner" {
			t.Error("credential or selected owner was not preserved")
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/account/link" {
			t.Error("wrong account route")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["account_token"] != "verified-account-token" {
			t.Error("separate account proof was not carried in the body")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"owner_id":"device:owner","account_id":"auth0|account"}`))
	}))
	defer server.Close()
	client, err := New(&Config{BaseURL: server.URL, APIKey: "device-token", OwnerID: "device:owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.LinkAccount(context.Background(), "verified-account-token"); err != nil {
		t.Fatal(err)
	}
}
