package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

func TestGrantsReplaceAndClear(t *testing.T) {
	key := "cHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHBwcHA="
	for _, clearGrants := range []bool{false, true} {
		srv := apitest.NewServer(t)
		srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Keys []string `json:"allowed_device_keys"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			want := []string{key}
			if clearGrants {
				want = []string{}
			}
			if body.Keys == nil {
				t.Error("grant replacement omitted its list")
			}
			if !slices.Equal(body.Keys, want) {
				t.Errorf("incorrect replacement: %+v", body)
			}
			if r.Header.Get("Authorization") == "" {
				t.Error("grant update lacks device authority")
			}
			apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": true, "allowed_device_keys": body.Keys, "type": "url", "status": "active"}, nil)
		})
		args := []string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "-o", "json"}
		if clearGrants {
			args = append(args, "--clear")
		} else {
			args = append(args, "--allow-device-key", key)
		}
		res := runCLI(t, &runOpts{args: args})
		if res.code != 0 || !strings.Contains(res.stdout.String(), `"private": true`) {
			t.Fatalf("grant mutation failed: %s / %s", res.stdout.String(), res.stderr.String())
		}
	}
}

func TestDeviceGrantLimit(t *testing.T) {
	if err := validateAllowedDeviceKeys(make([]string, 257)); err == nil || !strings.Contains(err.Error(), "256") {
		t.Fatal("grant cap not enforced")
	}
}
