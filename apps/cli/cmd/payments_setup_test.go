package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/layervai/qurl-go/qurl"
	"github.com/stretchr/testify/require"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

type stripeConsentFixture struct {
	deviceStripePayments
	port          int
	state         string
	completions   int
	completedCode string
}

func (f *stripeConsentFixture) ConnectPublisherStripeForDevice(_ context.Context, port int) (link, state string, err error) {
	f.port = port
	f.state = strings.Repeat("a", 43) + "." + strconv.Itoa(port)
	return "https://connect.stripe.com/oauth/authorize?state=" + url.QueryEscape(f.state), f.state, nil
}

func (f *stripeConsentFixture) CompletePublisherStripe(_ context.Context, state, code string) error {
	if state == f.state {
		f.completions++
		f.completedCode = code
	}
	return nil
}

func TestDeviceStripeConsentRejectsWrongStateAndHandlesCancel(t *testing.T) {
	for _, code := range []string{"code_fixture", ""} {
		t.Run("code="+code, func(t *testing.T) {
			f := &stripeConsentFixture{}
			err := connectStripeOnDevice(t.Context(), f, func(ctx context.Context, link string) error {
				require.Equal(t, "https://connect.stripe.com/oauth/authorize?state="+url.QueryEscape(f.state), link)
				local := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(f.port)) + "/stripe/return?"
				prefetch := url.Values{"state": {f.state}, "code": {code}}
				head, err := http.NewRequestWithContext(ctx, http.MethodHead, local+prefetch.Encode(), http.NoBody)
				require.NoError(t, err)
				headResponse, err := http.DefaultClient.Do(head)
				require.NoError(t, err)
				require.NoError(t, headResponse.Body.Close())
				require.Equal(t, http.StatusMethodNotAllowed, headResponse.StatusCode)
				for _, state := range []string{"wrong", f.state} {
					query := url.Values{"state": {state}, "code": {code}}
					req, err := http.NewRequestWithContext(ctx, http.MethodGet, local+query.Encode(), http.NoBody)
					require.NoError(t, err)
					res, err := http.DefaultClient.Do(req)
					require.NoError(t, err)
					require.NoError(t, res.Body.Close())
					require.Equal(t, "no-store", res.Header.Get("Cache-Control"))
					if state != f.state || code == "" {
						require.Equal(t, http.StatusBadRequest, res.StatusCode)
					} else {
						require.Equal(t, http.StatusOK, res.StatusCode)
					}
				}
				return nil
			})
			if code == "" {
				require.ErrorContains(t, err, "canceled")
				require.Zero(t, f.completions)
			} else {
				require.NoError(t, err)
				require.Equal(t, 1, f.completions)
				require.Equal(t, code, f.completedCode)
			}
		})
	}
}

func TestPaidAccessCents(t *testing.T) {
	for value, want := range map[string]int64{"5": 500, "0.50": 50, "5.1": 510, "5.01": 501, "999999.99": 99999999} {
		got, err := paidAccessCents(value)
		require.NoError(t, err, value)
		require.Equal(t, want, got, value)
	}
	for _, value := range []string{"", "0.49", "-5", "1e3", "1000000", "5.", "5.001", "5.2.0", " $5.00"} {
		_, err := paidAccessCents(value)
		require.Error(t, err, value)
	}
}

func TestPaymentsSetupAttachesTheCreatedPrice(t *testing.T) {
	srv := apitest.NewServer(t)
	resource := map[string]any{
		"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
		"private": false, "type": "url", "status": "active",
	}
	path := "/v1/resources/" + srv.Key.CRID
	srv.ScriptRepeat(http.MethodGet, path, 3, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{"resource": resource}, nil)
	})
	srv.ScriptRepeat(http.MethodGet, path+"/preview", 3, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"name": "An app", "revision": 1}))
	})
	srv.ScriptRepeat(http.MethodGet, "/v1/payments/account", 1, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{"account_id": "acct_owner", "layerv_fee_percent": 3}, nil)
	})
	srv.ScriptRepeat(http.MethodPost, "/v1/payments/prices", 3, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name            string `json:"name"`
			Amount          int64  `json:"amount"`
			Interval        string `json:"interval"`
			ExpectedPriceID string `json:"expected_price_id"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "An app", body.Name)
		require.Equal(t, "month", body.Interval)
		if _, retry := resource["consumer_access"]; retry {
			require.Equal(t, "price_new", body.ExpectedPriceID)
		} else {
			require.Empty(t, body.ExpectedPriceID)
		}
		if body.Amount == 601 {
			apitest.WriteProblem(t, w, http.StatusConflict, "payment_price_mismatch", "Stripe Price Does Not Match", "The current Stripe price does not match the requested terms.")
			return
		}
		require.Equal(t, int64(501), body.Amount)
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{"account_id": "acct_owner", "price_id": "price_new"}, nil)
	})
	srv.ScriptRepeat(http.MethodPatch, path, 2, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Expected int64                     `json:"expected_consumer_access_version"`
			Policy   qurl.ConsumerAccessPolicy `json:"consumer_access"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Zero(t, body.Expected)
		require.Equal(t, "contract", body.Policy.Mode)
		require.Equal(t, int64(1), body.Policy.Version)
		require.Equal(t, &qurl.CRIDPaymentPolicy{AccountID: "acct_owner", PriceID: "price_new"}, body.Policy.Payment)
		resource["consumer_access"] = body.Policy
		resource["private"] = true
		apitest.WriteEnvelope(t, w, http.StatusOK, resource, nil)
	})
	args := []string{"--endpoint", srv.URL, "payments", "setup", srv.Key.CRID, "--price", "5.01", "--interval", "month", "-o", "json"}
	for range 2 {
		result := runCLI(t, &runOpts{args: args})
		require.Zero(t, result.code, result.stderr.String())
		require.JSONEq(t, `{"crid":`+strconv.Quote(srv.Key.CRID)+`,"version":1}`, result.stdout.String())
	}
	args[6] = "6.01"
	conflict := runCLI(t, &runOpts{args: args})
	require.NotZero(t, conflict.code)
	require.Contains(t, conflict.stderr.String(), "replacing it with --version 1")
	require.Empty(t, conflict.stdout.String())
}
