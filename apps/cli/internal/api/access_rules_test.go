package qurlapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
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

func TestAccessRulesFrozenAccountIsNotUnlinked(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/me/access-rules", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"account_frozen","detail":"private detail"}}`))
	})
	_, err := newTestClient(t, srv, nil).AccessRules().Load(t.Context())
	var apiErr *Error
	if errors.Is(err, accessrules.ErrUnlinked) || !errors.As(err, &apiErr) || apiErr.Code != "account_frozen" || strings.Contains(err.Error(), "private detail") {
		t.Fatalf("frozen account: %v", err)
	}
}

func TestAccessRulesIgnoresEnvelopeAdditionsAndKeepsRuleFields(t *testing.T) {
	rule := `{"id":"11111111-1111-4111-8111-111111111111","name":"Team","mode":"keys"}`
	for body, ok := range map[string]bool{
		`{"data":{"account_id":"one","version":2,"items":[` + rule + `],"built_ins":[{"id":"builtin.new","name":"Renamed","mode":"keys","future":true}],"next":1},"meta":{"request_id":"r"},"links":{}}`: true,
		`{"data":{"account_id":"one","version":2,"items":[{"id":"11111111-1111-4111-8111-111111111111","name":"Team","mode":"keys","future":1}]}}`:                                                       true,
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

// A rule field this CLI doesn't model goes back to the service unchanged.
func TestAccessRulesSaveUnknownRuleFieldsBack(t *testing.T) {
	newer := `{"id":"11111111-1111-4111-8111-111111111111","name":"Buyers","mode":"contract","contract_id":"buyers","payment":{"price_id":"price_1","access_seconds":0,"trial_days":7},"region":"eu"}`
	reply := `{"data":{"account_id":"one","version":%d,"items":[` + newer + `],"built_ins":[]}}`
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/me/access-rules", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, reply, 2)
	})
	srv.Script(http.MethodPut, "/v1/me/access-rules", func(w http.ResponseWriter, r *http.Request) {
		if body, err := io.ReadAll(r.Body); err != nil || string(body) != `{"version":2,"items":[`+newer+`]}` {
			t.Errorf("save body %s %v", body, err)
		}
		_, _ = fmt.Fprintf(w, reply, 3)
	})
	rules := newTestClient(t, srv, nil).AccessRules()
	library, err := rules.Load(t.Context())
	if err != nil || len(library.Items) != 1 || !library.Items[0].HasUnknownFields() {
		t.Fatalf("load %+v %v", library, err)
	}
	if saved, err := rules.Replace(t.Context(), library.Version, library.Items); err != nil || saved.Version != 3 {
		t.Fatalf("save %+v %v", saved, err)
	}
}

// quick_share_rule_id is a Desktop setting: the service keeps it when a save
// omits it, so the CLI ignores it on read and never sends it, not even null.
func TestAccessRulesKeepQuickShareOutOfSavesAndCarryPayment(t *testing.T) {
	paid := `{"id":"11111111-1111-4111-8111-111111111111","name":"Buyers","mode":"contract","contract_id":"buyers","payment":{"price_id":"price_1","access_seconds":3600}}`
	reply := `{"data":{"account_id":"one","version":%d,"items":[` + paid + `],"built_ins":[],"quick_share_rule_id":"11111111-1111-4111-8111-111111111111"}}`
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/me/access-rules", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, reply, 2)
	})
	srv.Script(http.MethodPut, "/v1/me/access-rules", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != `{"version":2,"items":[`+paid+`]}` {
			t.Errorf("save body %s %v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, reply, 3)
	})
	rules := newTestClient(t, srv, nil).AccessRules()
	library, err := rules.Load(t.Context())
	if err != nil || len(library.Items) != 1 || library.Items[0].HasUnknownFields() || library.Items[0].Payment == nil || *library.Items[0].Payment != (accessrules.Payment{PriceID: "price_1", AccessSeconds: 3600}) {
		t.Fatalf("load %+v %v", library, err)
	}
	if saved, err := rules.Replace(t.Context(), library.Version, library.Items); err != nil || saved.Version != 3 {
		t.Fatalf("save %+v %v", saved, err)
	}
}

func TestAccessRulesReadsLargeEscapedLibrary(t *testing.T) {
	items := make([]accessrules.Definition, 14)
	for i := range items {
		items[i] = accessrules.Definition{
			ID:   fmt.Sprintf("%08d-0000-4000-8000-000000000000", i),
			Name: "Team", Mode: "email_domain", EmailDomain: "example.com",
		}
		for j := range 240 {
			items[i].AllowedEmails = append(items[i].AllowedEmails, fmt.Sprintf("%03d%s@x.co", j, strings.Repeat("&", 60)))
		}
	}
	if err := accessrules.ValidateItems(items); err != nil {
		t.Fatal(err)
	}
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, accessRulesPath, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, accessrules.AccountLibrary{AccountID: "one", Version: 1, Items: items}, nil)
	})
	got, err := newTestClient(t, srv, nil).AccessRules().Load(t.Context())
	if err != nil || !accessrules.EqualItems(got.Items, items) {
		t.Fatalf("read %d rules: %v", len(got.Items), err)
	}
}
