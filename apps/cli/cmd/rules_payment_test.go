package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/accessrules"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

const paidRule = `{"id":"11111111-1111-4111-8111-111111111111","name":"Buyers","mode":"contract","contract_id":"buyers","payment":{"price_id":"price_1","access_seconds":3600}}`

// scriptStripeAccount answers qurl payments account's request with account,
// or with 503, and every retry of it, when unavailable.
func scriptStripeAccount(t *testing.T, srv *apitest.Server, unavailable bool, account string) {
	t.Helper()
	if unavailable {
		srv.ScriptRepeat(http.MethodGet, "/v1/payments/account", 5, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"status":503,"code":"service_unavailable","title":"Service Unavailable"}}`))
		})
		return
	}
	srv.Script(http.MethodGet, "/v1/payments/account", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{"account_id": account, "layerv_fee_percent": 3}, nil)
	})
}

func TestRulesCreateSavesAPaidContractRule(t *testing.T) {
	dir := connectorStateTestDir(t)
	result := runCLI(t, &runOpts{args: []string{"rules", "create", "Buyers", "--mode", "contract", "--contract", "buyers", "--stripe-price", "price_1", "--payment-access-seconds", "3600", "-o", "json"}, env: map[string]string{}, shareStateDir: dir})
	var got accessrules.Definition
	if result.code != 0 || json.Unmarshal(result.stdout.Bytes(), &got) != nil || got.Payment == nil || *got.Payment != (accessrules.Payment{PriceID: "price_1", AccessSeconds: 3600}) {
		t.Fatalf("exit %d %s %s", result.code, result.stdout.String(), result.stderr.String())
	}
	if listed := runCLI(t, &runOpts{args: []string{"rules", "list"}, env: map[string]string{}, shareStateDir: dir}).stdout.String(); !strings.Contains(listed, "People who pay · access lasts 1 hour") {
		t.Errorf("list lacks the paid rule:\n%s", listed)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--mode", "review", "--stripe-price", "price_1"}, "apply only to --mode contract"},
		{[]string{"--mode", "contract", "--contract", "c", "--payment-access-seconds", "60"}, "--payment-access-seconds needs --stripe-price"},
		{[]string{"--mode", "contract", "--contract", "c", "--stripe-price", "prod_1"}, "--stripe-price must be a Stripe price ID"},
		{[]string{"--mode", "contract", "--contract", "c", "--stripe-price", "price_1", "--payment-access-seconds", "315360001"}, "--payment-access-seconds from 0 to 315360000"},
	} {
		result := runCLI(t, &runOpts{args: append([]string{"rules", "create", "Bad"}, tc.args...), env: map[string]string{}, shareStateDir: dir})
		if result.code != exitcode.Usage || !strings.Contains(result.stderr.String(), tc.want) {
			t.Errorf("%v: exit %d %s", tc.args, result.code, result.stderr.String())
		}
	}
}

func TestRulesSaveAcceptsAPaidRule(t *testing.T) {
	dir := connectorStateTestDir(t)
	result := runCLI(t, &runOpts{args: []string{"rules", "save", "--file", "-", "--version", "0", "-o", "json"}, stdin: strings.NewReader(paidRule), env: map[string]string{}, shareStateDir: dir})
	if result.code != 0 || !strings.Contains(result.stdout.String(), `"price_id": "price_1"`) {
		t.Fatalf("exit %d %s %s", result.code, result.stdout.String(), result.stderr.String())
	}
}

func TestPublishWithAPaidRuleUsesTheConnectedStripeAccount(t *testing.T) {
	srv := apitest.NewServer(t)
	scriptStripeAccount(t, srv, false, "acct_publisher")
	sent := map[string]any{}
	scriptPublish(t, srv, true, false, nil, sent)
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", "https://api.example.com", "--rule-file", "-", "-o", "json"}, stdin: strings.NewReader(paidRule), shareStateDir: connectorStateTestDir(t)})
	payment, _ := sent["payment"].(map[string]any)
	if result.code != 0 || sent["mode"] != "contract" || payment["account_id"] != "acct_publisher" || payment["price_id"] != "price_1" || payment["access_seconds"] != float64(3600) {
		t.Fatalf("exit %d sent %v %s", result.code, sent, result.stderr.String())
	}
}

func TestPaidRuleNeedsConnectedPaymentsBeforeAnyChange(t *testing.T) {
	commands := map[string]func(*apitest.Server) []string{
		"publish": func(*apitest.Server) []string { return []string{"publish", "https://api.example.com"} },
		"configure": func(srv *apitest.Server) []string {
			return []string{"access", "configure", srv.Key.CRID, "--version", "0"}
		},
	}
	for _, tc := range []struct {
		name        string
		unavailable bool
		code        int
		want        string
	}{
		{"not connected", false, exitcode.InvalidInput, msgRulesStripeNotConnected},
		{"payments off", true, exitcode.Unavailable, msgRulesPaymentsUnavailable},
	} {
		for name, command := range commands {
			t.Run(tc.name+" "+name, func(t *testing.T) {
				srv := apitest.NewServer(t)
				scriptStripeAccount(t, srv, tc.unavailable, "")
				// Any publish or configure request is unscripted and would fail with another code.
				args := append(append([]string{"--endpoint", srv.URL}, command(srv)...), "--rule-file", "-")
				result := runCLI(t, &runOpts{args: args, stdin: strings.NewReader(paidRule), shareStateDir: connectorStateTestDir(t)})
				if result.code != tc.code || !strings.Contains(result.stderr.String(), tc.want) {
					t.Fatalf("exit %d %s", result.code, result.stderr.String())
				}
			})
		}
	}
}

func TestLocalPublishWithAPaidRuleChecksPaymentsBeforeTheResource(t *testing.T) {
	srv := apitest.NewServer(t)
	scriptStripeAccount(t, srv, false, "")
	result := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "publish", "http://127.0.0.1:3000", "--rule-file", "-"},
		stdin:         strings.NewReader(paidRule),
		env:           map[string]string{"QURL_API_KEY": testAPIKey},
		shareRegistry: &ownerOnlyTestShareRegistry{}, shareStateDir: connectorStateTestDir(t),
		preflightTarget: func(context.Context, string, int) error { return nil },
		localResource: func(context.Context, *connectorshare.NativeRuntimeConfig, func(string) (string, error)) (*agent.ResolvedResource, error) {
			return nil, errors.New("resource prepared without a connected Stripe account")
		},
	})
	if result.code != exitcode.InvalidInput || !strings.Contains(result.stderr.String(), msgRulesStripeNotConnected) {
		t.Fatalf("exit %d %s", result.code, result.stderr.String())
	}
}

func TestConfigureWithAPaidRuleUsesTheConnectedStripeAccount(t *testing.T) {
	srv := apitest.NewServer(t)
	scriptStripeAccount(t, srv, false, "acct_publisher")
	srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Policy qurl.ConsumerAccessPolicy `json:"consumer_access"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Policy.Payment == nil || *body.Policy.Payment != (qurl.CRIDPaymentPolicy{AccountID: "acct_publisher", PriceID: "price_1", AccessSeconds: 3600}) {
			t.Fatalf("policy %+v %v", body.Policy, err)
		}
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": true, "consumer_access": body.Policy}, nil)
	})
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "access", "configure", srv.Key.CRID, "--rule-file", "-", "--version", "0", "-o", "json"}, stdin: strings.NewReader(paidRule)})
	if result.code != 0 || !strings.Contains(result.stdout.String(), `"account_id": "acct_publisher"`) {
		t.Fatalf("exit %d %s %s", result.code, result.stdout.String(), result.stderr.String())
	}
}

// accountRules is a fixed account library for seeding the local journal.
type accountRules struct{ library accessrules.AccountLibrary }

func (a *accountRules) Load(context.Context) (accessrules.AccountLibrary, error) {
	return a.library, nil
}
func (a *accountRules) Replace(context.Context, int64, []accessrules.Definition) (accessrules.AccountLibrary, error) {
	return a.library, nil
}

// Only local saves check NameTaken, so a rule synced from another client can
// carry a default's name; that name then picks neither.
func TestSyncedRuleNamedLikeADefaultIsAmbiguous(t *testing.T) {
	srv := apitest.NewServer(t)
	dir := connectorStateTestDir(t)
	store, err := connectorstate.OpenAccessRules(dir, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	synced := accessrules.Definition{ID: "11111111-1111-4111-8111-111111111111", Name: "People you approve", Mode: accessrules.ModeKeys}
	if _, err = store.Sync(t.Context(), &accountRules{accessrules.AccountLibrary{AccountID: "one", Items: []accessrules.Definition{synced}}}); err != nil {
		t.Fatal(err)
	}
	publish := func(selector string) *runResult {
		return runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", "https://api.example.com", "--rule", selector, "-o", "json"}, shareStateDir: dir})
	}
	if result := publish(synced.Name); result.code != exitcode.Usage || !strings.Contains(result.stderr.String(), msgRulesAmbiguous) {
		t.Fatalf("shadowed name: exit %d %s", result.code, result.stderr.String())
	}
	sent := map[string]any{}
	scriptPublish(t, srv, true, false, nil, sent)
	if result := publish(synced.ID); result.code != 0 || sent["mode"] != accessrules.ModeKeys {
		t.Fatalf("by ID: exit %d sent %v %s", result.code, sent, result.stderr.String())
	}
}

func TestRepublishPaidRuleAfterStripeChangeConflicts(t *testing.T) {
	srv := apitest.NewServer(t)
	scriptStripeAccount(t, srv, false, "acct_new")
	existing := map[string]any{"mode": "contract", "version": 1, "contract_id": "buyers", "payment": map[string]any{"account_id": "acct_old", "price_id": "price_1", "access_seconds": 3600}}
	scriptPublish(t, srv, true, true, existing, nil)
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", "https://api.example.com", "--rule-file", "-", "-o", "json"}, stdin: strings.NewReader(paidRule), shareStateDir: connectorStateTestDir(t)})
	if result.code != exitcode.Conflict {
		t.Fatalf("exit %d %s", result.code, result.stderr.String())
	}
}

func TestNewRuleIDIsAValidCustomRuleID(t *testing.T) {
	id, err := newRuleID()
	if err != nil {
		t.Fatal(err)
	}
	if d := (accessrules.Definition{ID: id, Name: "Team", Mode: accessrules.ModeKeys}); d.Validate(false) != nil {
		t.Fatalf("generated ID %q is not a valid rule ID", id)
	}
}
