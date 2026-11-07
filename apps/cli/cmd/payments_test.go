package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

// stripeAccountServer answers one account read and one disconnect; a second
// read is unscripted and does not get this account.
func stripeAccountServer(t *testing.T, data map[string]any, deletes *int) *apitest.Server {
	t.Helper()
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/payments/account", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, data, nil)
	})
	srv.Script(http.MethodDelete, "/v1/payments/account", func(w http.ResponseWriter, _ *http.Request) {
		*deletes++
		w.WriteHeader(http.StatusNoContent)
	})
	return srv
}

func notReadyAccount() map[string]any {
	return map[string]any{"account_id": "acct_publisher", "layerv_fee_percent": 3, "charges_enabled": true, "payouts_enabled": false, "setup_url": "https://dashboard.stripe.com/", "renewing_subscriptions": 2}
}

func TestPaymentsAccountPrintsStatusAndSetupLink(t *testing.T) {
	var deletes int
	srv := stripeAccountServer(t, notReadyAccount(), &deletes)
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "payments", "account"}})
	if result.code != 0 {
		t.Fatalf("exit=%d error=%s", result.code, result.stderr.String())
	}
	for _, want := range []string{"acct_publisher", "Charges: enabled", "Payouts: not enabled", "Renewing subscriptions: 2", "Finish setup in Stripe: https://dashboard.stripe.com/"} {
		if !strings.Contains(result.stdout.String(), want) {
			t.Fatalf("missing %q in %s", want, result.stdout.String())
		}
	}
	srv = stripeAccountServer(t, notReadyAccount(), &deletes)
	result = runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "payments", "account", "-o", "json"}})
	if result.code != 0 || !strings.Contains(result.stdout.String(), `"payouts_enabled": false`) || !strings.Contains(result.stdout.String(), `"renewing_subscriptions": 2`) {
		t.Fatalf("missing status fields: %s", result.stdout.String())
	}
}

func TestPaymentsAccountRefusesASetupLinkOutsideStripe(t *testing.T) {
	var deletes int
	data := notReadyAccount()
	data["setup_url"] = "https://attacker.test/"
	srv := stripeAccountServer(t, data, &deletes)
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "payments", "account"}})
	if result.code == 0 || strings.Contains(result.stdout.String(), "attacker.test") || !strings.Contains(result.stderr.String(), "invalid Stripe account") {
		t.Fatalf("exit=%d stdout=%s", result.code, result.stdout.String())
	}
}

func TestPaymentsDisconnectConfirmsFirst(t *testing.T) {
	for _, tc := range []struct {
		name    string
		run     runOpts
		code    int
		deletes int
	}{
		{"pipeline without --yes", runOpts{}, 2, 0},
		{"answered no", runOpts{inTTY: true, stdin: strings.NewReader("n\n")}, 0, 0},
		{"answered yes", runOpts{inTTY: true, stdin: strings.NewReader("y\n")}, 0, 1},
		{"--yes", runOpts{args: []string{"--yes"}}, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var deletes int
			srv := stripeAccountServer(t, notReadyAccount(), &deletes)
			run := tc.run
			run.args = append([]string{"--endpoint", srv.URL, "payments", "disconnect"}, run.args...)
			result := runCLI(t, &run)
			if result.code != tc.code || deletes != tc.deletes {
				t.Fatalf("exit=%d deletes=%d stderr=%s", result.code, deletes, result.stderr.String())
			}
			if tc.run.inTTY && !strings.Contains(result.stderr.String(), "2 subscriptions will stop renewing") {
				t.Fatalf("prompt does not say what stops: %s", result.stderr.String())
			}
			if tc.deletes == 1 && !strings.Contains(result.stdout.String(), "Stripe is disconnected.") {
				t.Fatalf("stdout=%s", result.stdout.String())
			}
		})
	}
}

func TestPaymentsDisconnectWithNothingConnectedSaysSo(t *testing.T) {
	var deletes int
	srv := stripeAccountServer(t, map[string]any{"account_id": "", "layerv_fee_percent": 3, "charges_enabled": false, "payouts_enabled": false, "renewing_subscriptions": 0}, &deletes)
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "payments", "disconnect"}})
	if result.code != 0 || deletes != 0 || !strings.Contains(result.stdout.String(), "No Stripe account is connected.") {
		t.Fatalf("exit=%d deletes=%d stdout=%s", result.code, deletes, result.stdout.String())
	}
}

func TestPaymentsDisconnectReportsAnUnfinishedDisconnect(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/payments/account", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, notReadyAccount(), nil)
	})
	srv.Script(http.MethodDelete, "/v1/payments/account", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"service_unavailable","detail":"The Stripe disconnect did not finish, so LayerV is still connected."}}`))
	})
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "payments", "disconnect", "--yes"}})
	if result.code == 0 || strings.Contains(result.stdout.String(), "Stripe is disconnected.") || !strings.Contains(result.stderr.String(), "LayerV is still connected") {
		t.Fatalf("exit=%d stdout=%s stderr=%s", result.code, result.stdout.String(), result.stderr.String())
	}
}

func TestPaymentsDisconnectYesNeedsNoStatusRead(t *testing.T) {
	srv := apitest.NewServer(t)
	var deletes int
	srv.Script(http.MethodDelete, "/v1/payments/account", func(w http.ResponseWriter, _ *http.Request) {
		deletes++
		w.WriteHeader(http.StatusNoContent)
	})
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "payments", "disconnect", "--yes"}})
	if result.code != 0 || deletes != 1 {
		t.Fatalf("exit=%d deletes=%d stderr=%s", result.code, deletes, result.stderr.String())
	}
}

func TestPaymentsUnfinishedDisconnectFinishesWithoutAskingAgain(t *testing.T) {
	var deletes int
	data := notReadyAccount()
	data["disconnecting"] = true
	srv := stripeAccountServer(t, data, &deletes)
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "payments", "account"}})
	if !strings.Contains(result.stdout.String(), "Run qurl payments disconnect to finish disconnecting.") || strings.Contains(result.stdout.String(), "Charges:") {
		t.Fatalf("stdout=%s", result.stdout.String())
	}
	srv = stripeAccountServer(t, data, &deletes)
	result = runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "payments", "disconnect"}})
	if result.code != 0 || deletes != 1 {
		t.Fatalf("exit=%d deletes=%d stderr=%s", result.code, deletes, result.stderr.String())
	}
}

func TestPaymentsAccountTellsToCancelBillingAfterAStripeSideRevoke(t *testing.T) {
	var deletes int
	data := notReadyAccount()
	data["revoked"] = true
	srv := stripeAccountServer(t, data, &deletes)
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "payments", "account"}})
	if !strings.Contains(result.stdout.String(), "Cancel or refund remaining subscriptions in Stripe") {
		t.Fatalf("stdout=%s", result.stdout.String())
	}
}

func TestPaymentsYesIsOnlyForDisconnect(t *testing.T) {
	srv := apitest.NewServer(t)
	for _, operation := range []string{"account", "connect"} {
		result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "payments", operation, "--yes"}})
		if result.code != 2 {
			t.Fatalf("%s --yes: exit=%d", operation, result.code)
		}
	}
}

func TestPaymentsDisconnectOfARevokedAccountSaysToCancelBillingInStripe(t *testing.T) {
	var deletes int
	data := notReadyAccount()
	data["revoked"] = true
	srv := stripeAccountServer(t, data, &deletes)
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "payments", "disconnect"}})
	if result.code != 0 || deletes != 1 || !strings.Contains(result.stderr.String(), "Cancel or refund remaining subscriptions there.") || !strings.Contains(result.stdout.String(), "LayerV is released") || strings.Contains(result.stdout.String(), "Buyers keep access") {
		t.Fatalf("exit=%d deletes=%d stderr=%s", result.code, deletes, result.stderr.String())
	}
}

func TestPaymentsDisconnectJSON(t *testing.T) {
	var deletes int
	srv := stripeAccountServer(t, notReadyAccount(), &deletes)
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "payments", "disconnect", "--yes", "-o", "json"}})
	if result.code != 0 || !strings.Contains(result.stdout.String(), `"disconnected": true`) {
		t.Fatalf("exit=%d stdout=%s", result.code, result.stdout.String())
	}
}

func TestPaymentsDisconnectPointsToYesWhenTheStatusCannotBeRead(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/payments/account", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"service_unavailable","detail":"retry later"}}`))
	})
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "payments", "disconnect"}})
	if result.code == 0 || !strings.Contains(result.stderr.String(), "re-run with --yes") {
		t.Fatalf("exit=%d stderr=%s", result.code, result.stderr.String())
	}
}

func TestPaymentsAccountWhenStripeCannotBeReached(t *testing.T) {
	var deletes int
	data := notReadyAccount()
	data["status_unavailable"] = true
	srv := stripeAccountServer(t, data, &deletes)
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "payments", "account"}})
	if !strings.Contains(result.stdout.String(), "could not be reached") || strings.Contains(result.stdout.String(), "Charges:") {
		t.Fatalf("stdout=%s", result.stdout.String())
	}
}

func TestPaymentsDisconnectWhenStripeCannotBeReachedStatesNoCount(t *testing.T) {
	var deletes int
	data := notReadyAccount()
	data["status_unavailable"], data["renewing_subscriptions"] = true, 0
	srv := stripeAccountServer(t, data, &deletes)
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "payments", "disconnect"}, inTTY: true, stdin: strings.NewReader("n\n")})
	if !strings.Contains(result.stderr.String(), "Every renewing subscription will stop renewing") || strings.Contains(result.stderr.String(), "0 subscriptions") {
		t.Fatalf("stderr=%s", result.stderr.String())
	}
}

func TestPaymentsDisconnectWithNothingConnectedJSON(t *testing.T) {
	var deletes int
	srv := stripeAccountServer(t, map[string]any{"account_id": "", "layerv_fee_percent": 3, "charges_enabled": false, "payouts_enabled": false, "renewing_subscriptions": 0}, &deletes)
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "payments", "disconnect", "-o", "json"}})
	if result.code != 0 || deletes != 0 || !strings.Contains(result.stdout.String(), `"disconnected": false`) {
		t.Fatalf("exit=%d deletes=%d stdout=%s", result.code, deletes, result.stdout.String())
	}
}

func TestPaymentsAccountJSONAlwaysCarriesTheDisconnectStates(t *testing.T) {
	var deletes int
	data := notReadyAccount()
	data["revoked"] = true
	srv := stripeAccountServer(t, data, &deletes)
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "payments", "account", "-o", "json"}})
	for _, want := range []string{`"revoked": true`, `"disconnecting": false`, `"status_unavailable": false`} {
		if !strings.Contains(result.stdout.String(), want) {
			t.Fatalf("missing %s in %s", want, result.stdout.String())
		}
	}
}

func TestPaymentsDisconnectExplainsAnUnexpectedAnswer(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{{http.StatusOK, "not 204"}, {http.StatusNotFound, "cannot disconnect Stripe yet"}, {http.StatusMethodNotAllowed, "cannot disconnect Stripe yet"}} {
		srv := apitest.NewServer(t)
		srv.Script(http.MethodDelete, "/v1/payments/account", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{}`))
		})
		result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "payments", "disconnect", "--yes"}})
		if result.code == 0 || !strings.Contains(result.stderr.String(), tc.want) {
			t.Fatalf("status %d: exit=%d stderr=%s", tc.status, result.code, result.stderr.String())
		}
	}
}

func TestPaymentsAccountRefusesAChangedFeeButDisconnectDoesNot(t *testing.T) {
	var deletes int
	data := notReadyAccount()
	data["layerv_fee_percent"] = 5
	srv := stripeAccountServer(t, data, &deletes)
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "payments", "account"}})
	if result.code == 0 || !strings.Contains(result.stderr.String(), "upgrade the qurl CLI") {
		t.Fatalf("exit=%d stderr=%s", result.code, result.stderr.String())
	}
	srv = stripeAccountServer(t, data, &deletes)
	result = runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "payments", "disconnect"}, inTTY: true, stdin: strings.NewReader("y\n")})
	if result.code != 0 || deletes != 1 {
		t.Fatalf("exit=%d deletes=%d stderr=%s", result.code, deletes, result.stderr.String())
	}
}
