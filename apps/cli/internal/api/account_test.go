package qurlapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/layervai/qurl-go/qurl"
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
	if err := client.LinkAccount(context.Background(), "verified-account-token", "device:owner"); err != nil {
		t.Fatal(err)
	}
	if err := client.LinkAccount(context.Background(), "verified-account-token", "device:other"); !errors.Is(err, qurl.ErrInvalidAPIResponse) {
		t.Fatalf("mismatched owner = %v", err)
	}
}

func TestAccountOwnersResponseBounds(t *testing.T) {
	for _, count := range []int{0, 1, 65, 66} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			owners := make([]string, count)
			for i := range owners {
				owners[i] = fmt.Sprintf("owner-%d", i)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/account/owners" || r.Header.Get("Authorization") != "Bearer account-token" {
					t.Error("wrong account request")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"owners": owners})
			}))
			defer server.Close()
			got, err := AccountOwners(context.Background(), &Config{BaseURL: server.URL, APIKey: "account-token"})
			if count == 0 || count > 65 {
				if !errors.Is(err, qurl.ErrInvalidAPIResponse) {
					t.Fatalf("owners = %v", err)
				}
			} else if err != nil || strings.Join(got, ",") != strings.Join(owners, ",") {
				t.Fatalf("owners = %v, %v", got, err)
			}
		})
	}
}
