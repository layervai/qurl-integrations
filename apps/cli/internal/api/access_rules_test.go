package qurlapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/accessrules"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

func TestAccessRulesUsesWholeListVersionWithoutReplay(t *testing.T) {
	srv := apitest.NewServer(t)
	items := []accessrules.Definition{{ID: "11111111-1111-4111-8111-111111111111", Name: "Team", Mode: "email_domain", EmailDomain: "example.com"}}
	srv.Script(http.MethodPut, "/v1/me/access-rules", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Version int64                    `json:"version"`
			Items   []accessrules.Definition `json:"items"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Version != 3 || !accessrules.EqualItems(body.Items, items) || r.Header.Get("Idempotency-Key") != "" {
			t.Fatalf("wrong CAS body %+v", body)
		}
		apitest.WriteEnvelope(t, w, http.StatusOK, accessrules.AccountLibrary{AccountID: "one", Version: 4, Items: items}, nil)
	})
	result, err := newTestClient(t, srv, nil).AccessRules().Replace(context.Background(), 3, items)
	if err != nil || result.Version != 4 {
		t.Fatalf("result %+v %v", result, err)
	}
}
func TestAccessRulesDoesNotTreatAuthOrMalformedAsEmpty(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusUnauthorized, http.StatusConflict, http.StatusServiceUnavailable, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.Script(http.MethodGet, "/v1/me/access-rules", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"secret":"do not expose"}`))
			})
			_, err := newTestClient(t, srv, nil).AccessRules().Load(t.Context())
			if err == nil {
				t.Fatal("invalid account accepted")
			}
			if status == http.StatusForbidden && !errors.Is(err, accessrules.ErrUnlinked) {
				t.Fatal(err)
			}
			if err.Error() == "do not expose" {
				t.Fatal("raw error leaked")
			}
		})
	}
}

func TestAccessRulesIgnoresEnvelopeAdditionsButNotRuleFields(t *testing.T) {
	rule := `{"id":"11111111-1111-4111-8111-111111111111","name":"Team","mode":"keys"}`
	for body, ok := range map[string]bool{
		`{"data":{"account_id":"one","version":2,"items":[` + rule + `],"built_ins":[{"id":"builtin.new","name":"Renamed","mode":"keys","future":true}],"next":1},"meta":{"request_id":"r"},"links":{}}`: true,
		`{"data":{"account_id":"one","version":2,"items":[{"id":"11111111-1111-4111-8111-111111111111","name":"Team","mode":"keys","future":1}]}}`:                                                       false,
		`{"data":{"account_id":"one","version":null,"items":[]}}`: false,
	} {
		srv := apitest.NewServer(t)
		srv.Script(http.MethodGet, "/v1/me/access-rules", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		})
		library, err := newTestClient(t, srv, nil).AccessRules().Load(t.Context())
		if ok != (err == nil) || (ok && (library.Version != 2 || len(library.Items) != 1)) {
			t.Errorf("%s: %+v %v", body, library, err)
		}
	}
}
