package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

func TestConsumerAccessConfiguresFirstPolicyWithVersionZero(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Expected int64 `json:"expected_consumer_access_version"`
			Policy   struct {
				Mode    string `json:"mode"`
				Version int64  `json:"version"`
			} `json:"consumer_access"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Expected != 0 || body.Policy.Mode != "review" || body.Policy.Version != 1 {
			t.Fatalf("first policy request=%+v error=%v", body, err)
		}
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
			"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
			"private": true, "type": "url", "status": "active", "consumer_access": body.Policy,
		}, nil)
	})
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "access", "configure", srv.Key.CRID, "--mode", "review", "--version", "0", "-o", "json"}})
	if result.code != 0 {
		t.Fatalf("exit=%d error=%s", result.code, result.stderr.String())
	}
}

func TestConsumerAccessInvitationFiltersSecrets(t *testing.T) {
	srv := apitest.NewServer(t)
	id := strings.Repeat("a", 64)
	srv.Script(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/invitations", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Email string `json:"email"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Email != "ana@example.com" {
			t.Fatalf("email request=%+v error=%v", body, err)
		}
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{"id": id, "email": body.Email, "status": "open", "delivery": "accepted", "version": 1, "expires_at": "2026-10-12T00:00:00Z", "invitation_token": "must-not-leak"}, nil)
	})
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "access", "invite", srv.Key.CRID, "--email", "ana@example.com", "-o", "json"}})
	if result.code != 0 {
		t.Fatalf("exit=%d error=%s", result.code, result.stderr.String())
	}
	if strings.Contains(result.stdout.String(), "must-not-leak") {
		t.Fatal("invitation secret leaked")
	}
	var envelope struct {
		Invitation struct {
			ID string `json:"id"`
		} `json:"invitation"`
	}
	if err := json.Unmarshal(result.stdout.Bytes(), &envelope); err != nil || envelope.Invitation.ID != id {
		t.Fatalf("receipt=%s error=%v", result.stdout.String(), err)
	}
}

func TestConsumerAccessRevocationDoesNotClaimPendingCleanupComplete(t *testing.T) {
	srv := apitest.NewServer(t)
	id := strings.Repeat("b", 64)
	srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID+"/consumer-grants/"+id, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("expected_version") != "1" {
			t.Fatal("missing revocation version")
		}
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{"id": id, "status": "revoked", "version": 2, "session_cleanup": "pending"}, nil)
	})
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "access", "revoke_grant", srv.Key.CRID, id, "--version", "1"}})
	if result.code != 0 || !strings.Contains(result.stdout.String(), "Connection cleanup: pending") {
		t.Fatalf("exit=%d output=%s error=%s", result.code, result.stdout.String(), result.stderr.String())
	}
}

func TestConsumerAccessRejectsBadDecisionsBeforeRequest(t *testing.T) {
	srv := apitest.NewServer(t)
	for _, args := range [][]string{
		{"invite", srv.Key.CRID},
		{"invite", srv.Key.CRID, "--email", "ana@example.com", "--version", "1"},
		{"approve", srv.Key.CRID, "not-a-listed-id", "--version", "1"},
		{"revoke_grant", srv.Key.CRID, strings.Repeat("a", 64)},
		{"configure", srv.Key.CRID, "--mode", "review", "--contract", "unrelated", "--version", "1"},
	} {
		result := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL, "access"}, args...)})
		if result.code != 2 {
			t.Fatalf("args=%v exit=%d error=%s", args, result.code, result.stderr.String())
		}
	}
	if len(srv.Requests()) != 0 {
		t.Fatal("invalid decision reached API")
	}
}
