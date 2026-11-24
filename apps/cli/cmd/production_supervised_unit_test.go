//go:build clisandbox && cliproduction

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
)

func TestProductionProofEnrollmentMint(t *testing.T) {
	const name = "qurl CLI journey v2 1231/2/linux/enrollment"
	key := "lv_test_" + strings.Repeat("a", 43)
	token := "lv_test_" + strings.Repeat("b", 43)
	kind := "enrollment_token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/api-keys" || r.Header.Get("Authorization") != "Bearer "+key || len(r.Header.Get("Idempotency-Key")) < 32 || body["name"] != name || body["kind"] != "enrollment_token" || body["target"] != "agent" || body["expires_in"] != "1h" || len(body) != 4 {
			t.Error("supervised mint changed its scoped, run-named one-shot request")
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"api_key": token, "name": name, "kind": kind, "target": "agent", "status": "active", "scopes": []string{"qurl:agent"}, "expires_at": time.Now().Add(time.Hour),
		}})
	}))
	defer server.Close()
	got, err := productionMintEnrollment(context.Background(), server.URL, key, name)
	if err != nil || got != token {
		t.Fatal("scoped enrollment mint did not return its one-shot token")
	}
	kind = "api_key"
	got, err = productionMintEnrollment(context.Background(), server.URL, key, name)
	if err == nil || got != "" || strings.Contains(err.Error(), token) {
		t.Fatal("mint accepted an ordinary key or disclosed credential material")
	}
}

func TestProductionProofSupervisedProcesses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("supervised proof requires Node.js")
	}
	// Exercise the production coordinator with a local executable, including
	// hostile output and a cold process that never persisted an identity.
	const fixture = `const fs=require('node:fs'),path=require('node:path');
const state=process.env.QURL_CONNECTOR_STATE_DIR, token=path.join(state,'enrollment-token'), sealed=path.join(state,'agent_state.sealed.json');
if(process.env.QURL_API_KEY||process.env.QURL_API_KEY_FILE||process.env.AUTOMATION_API_KEY||process.env.LAYERV_KEY_PROVIDER!=='local-key'||process.env.LAYERV_LOCAL_KEY_FD!=='3')process.exit(8);
if(process.env.PROOF_MODE==='reject'){console.error('SECRET-CANARY');process.exit(8)}
if(process.env.PROOF_MODE==='leak-token')console.error(fs.readFileSync(token,'utf8'));
if(!fs.existsSync(sealed)){if(!fs.existsSync(token))process.exit(9);if(process.env.PROOF_MODE!=='no-state')fs.writeFileSync(sealed,'opaque-sealed-fixture')}
else if(fs.existsSync(token)||process.env.PROOF_MODE==='warm-failure')process.exit(10);
console.log(JSON.stringify({owner_id:process.env.PROOF_MODE==='wrong-owner'?'other-owner':'proof-owner',device_enrolled:true}));`
	for _, mode := range []string{"success", "reject", "no-state", "warm-failure", "wrong-owner", "leak-token"} {
		t.Run(mode, func(t *testing.T) {
			state, err := filepath.EvalSymlinks(productionStateDir(t))
			if err != nil {
				t.Fatal(err)
			}
			tokenPath := filepath.Join(state, "enrollment-token")
			productionWriteEnrollment(t, tokenPath, "one-shot-fixture")
			if token, err := auth.ReadExternalEnrollmentTokenFile(tokenPath); err != nil || token != "one-shot-fixture" {
				t.Fatal("production token file does not pass the customer permission reader")
			}
			payload, err := json.Marshal(map[string]any{"binary": node, "args": []string{"-e", fixture}, "state": state, "tokenPath": tokenPath, "owner": "proof-owner"})
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("QURL_API_KEY", "must-not-reach-candidate")
			raw, code := productionCommand(t, node, map[string]string{"PROOF_MODE": mode, "QURL_CONNECTOR_STATE_DIR": state}, string(payload), "-e", productionSupervisedScript)
			if (code == 0) != (mode == "success") || strings.Contains(string(raw), "SECRET-CANARY") || strings.Contains(string(raw), "one-shot-fixture") {
				t.Fatalf("supervised coordinator accepted failed evidence or leaked output (exit %d)", code)
			}
			if mode == "success" {
				if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
					t.Fatal("warm supervised process retained the enrollment token")
				}
			}
		})
	}
}
