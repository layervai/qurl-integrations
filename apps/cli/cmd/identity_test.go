package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

func TestSignedIdentityEnableDisable(t *testing.T) {
	for _, action := range []string{"enable", "disable"} {
		t.Run(action, func(t *testing.T) {
			srv := apitest.NewServer(t)
			want := action == "enable"
			srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, r *http.Request) {
				var body map[string]bool
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 1 || body["signed_identity"] != want {
					t.Errorf("unexpected body: %v, %v", body, err)
				}
				if r.Header.Get("Idempotency-Key") != "" {
					t.Error("identity change must not replay")
				}
				apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "type": "url", "status": "active", "signed_identity": want}, nil)
			})
			res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "identity", action, srv.Key.CRID, "-o", "json"}})
			if res.code != 0 {
				t.Fatalf("exit=%d: %s", res.code, res.stderr.String())
			}
			var out struct {
				Enabled *bool `json:"signed_identity"`
			}
			if err := json.Unmarshal(res.stdout.Bytes(), &out); err != nil || out.Enabled == nil || *out.Enabled != want {
				t.Fatalf("unconfirmed output: %s", res.stdout.String())
			}
		})
	}
}

func TestSignedIdentityRefusesMissingOrWrongConfirmation(t *testing.T) {
	for _, kind := range []string{"missing", "wrong", "wrong resource", "unavailable"} {
		t.Run(kind, func(t *testing.T) {
			srv := apitest.NewServer(t)
			other := apitest.GenerateResourceKey(t)
			calls := 0
			srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if kind == "unavailable" {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				row := map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "type": "url", "status": "active"}
				if kind != "missing" {
					row["signed_identity"] = kind != "wrong"
				}
				if kind == "wrong resource" {
					row["crid"] = other.CRID
					row["resource_id"] = other.ResourceID
				}
				apitest.WriteEnvelope(t, w, http.StatusOK, row, nil)
			})
			res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "identity", "enable", srv.Key.CRID}})
			if calls != 1 {
				t.Fatalf("requests=%d; want one", calls)
			}
			if res.code == 0 || res.stdout.Len() != 0 {
				t.Fatalf("claimed success: %s", res.stdout.String())
			}
		})
	}
}

func TestSignedIdentityStatus(t *testing.T) {
	for _, kind := range []string{"enabled", "disabled", "old service"} {
		t.Run(kind, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.Script(http.MethodGet, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
				row := map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "type": "url", "status": "active"}
				if kind != "old service" {
					row["signed_identity"] = kind == "enabled"
				}
				apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{"resource": row}, nil)
			})
			res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "identity", "status", srv.Key.CRID, "-o", "json"}})
			if kind == "old service" {
				if res.code == 0 || res.stdout.Len() != 0 {
					t.Fatal("claimed support on old service")
				}
				return
			}
			if res.code != 0 {
				t.Fatalf("exit=%d: %s", res.code, res.stderr.String())
			}
			var out struct {
				Enabled *bool `json:"signed_identity"`
			}
			if err := json.Unmarshal(res.stdout.Bytes(), &out); err != nil || out.Enabled == nil || *out.Enabled != (kind == "enabled") {
				t.Fatalf("unconfirmed output: %s", res.stdout.String())
			}
		})
	}
}
