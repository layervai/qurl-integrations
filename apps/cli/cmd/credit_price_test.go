package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

type creditPriceTestPolicy struct {
	Mode     string `json:"mode"`
	Contract string `json:"contract_id"`
	Version  int64  `json:"version"`
	Credits  int64  `json:"credits"`
}

func TestConfigureCreditPriceUsesCurrentVersionAndRequiresConfirmation(t *testing.T) {
	for _, test := range []struct {
		name          string
		returnedPrice int64
	}{
		{name: "confirmed", returnedPrice: 2},
		{name: "ignored", returnedPrice: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			path := "/v1/resources/" + srv.Key.CRID
			resource := map[string]any{
				"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
				"private": true, "type": "url", "status": "active",
				"consumer_access_version": 3,
				"consumer_access":         creditPriceTestPolicy{Mode: "contract", Contract: "existing-contract", Version: 3, Credits: 1},
			}
			srv.Script(http.MethodGet, path, func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{"resource": resource}, nil)
			})
			srv.Script(http.MethodPatch, path, func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Expected int64                 `json:"expected_consumer_access_version"`
					Policy   creditPriceTestPolicy `json:"consumer_access"`
				}
				want := creditPriceTestPolicy{Mode: "contract", Contract: "existing-contract", Version: 4, Credits: 2}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Expected != 3 || body.Policy != want {
					t.Errorf("credit update=%+v, err=%v", body, err)
				}
				body.Policy.Credits = test.returnedPrice
				resource["consumer_access"] = body.Policy
				resource["consumer_access_version"] = body.Policy.Version
				apitest.WriteEnvelope(t, w, http.StatusOK, resource, nil)
			})
			result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "access", "configure", srv.Key.CRID, "--credit-price", "2", "-o", "json"}})
			if test.returnedPrice == 0 {
				if result.code == 0 || result.stdout.Len() != 0 {
					t.Fatalf("unconfirmed price reported success: %s", result.stdout.String())
				}
				return
			}
			if result.code != 0 || !strings.Contains(result.stdout.String(), `"credits": 2`) {
				t.Fatalf("exit=%d stdout=%s stderr=%s", result.code, result.stdout.String(), result.stderr.String())
			}
		})
	}
}

func TestPublishCreditPriceUsesPrivateContract(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Private bool                  `json:"private"`
			Policy  creditPriceTestPolicy `json:"consumer_access"`
		}
		want := creditPriceTestPolicy{Mode: "contract", Contract: "crid-credits", Version: 1, Credits: 2}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || !request.Private || request.Policy != want {
			t.Errorf("publish request=%+v, err=%v", request, err)
		}
		apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{
			"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
			"private": true, "type": "url", "status": "active", "consumer_access": request.Policy,
		}, nil)
	})
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", "https://example.com/reports", "--name", "Reports", "--credit-price", "2"}})
	if result.code != 0 || !strings.Contains(result.stdout.String(), "2 per hour") {
		t.Fatalf("exit=%d stdout=%s stderr=%s", result.code, result.stdout.String(), result.stderr.String())
	}
}
