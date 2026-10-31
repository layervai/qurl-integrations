package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-go/qurl"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
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

func TestConsumerAccessConfigureRejectsUnconfirmedPolicy(t *testing.T) {
	for _, test := range []struct {
		name, mode string
		version    int64
	}{
		{"different_mode", "invite_only", 1},
		{"different_version", "review", 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
					"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
					"private": true, "type": "url", "status": "active",
					"consumer_access": qurl.ConsumerAccessPolicy{Mode: test.mode, Version: test.version},
				}, nil)
			})
			result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "access", "configure", srv.Key.CRID, "--mode", "review", "--version", "0", "-o", "json"}})
			if result.code == 0 || result.stdout.Len() != 0 {
				t.Fatalf("unconfirmed policy reported success: exit=%d output=%s", result.code, result.stdout.String())
			}
		})
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
		{"configure", srv.Key.CRID, "--mode", "contract", "--contract", "   ", "--version", "0"},
		{"approve", srv.Key.CRID, "new", "--version", "0"},
		{"approve", srv.Key.CRID, "new", "--version", "1", "--credential-kind", "device", "--credential-id", "agent-consumer01", "--credential-key", "public-key"},
		{"approve", srv.Key.CRID, "new", "--version", "0", "--credential-kind", "unknown", "--credential-id", "id", "--credential-key", "key"},
		{"approve", srv.Key.CRID, strings.Repeat("a", 64), "--version", "1", "--entitlement", "receipt"},
		{"approve", srv.Key.CRID, strings.Repeat("a", 64), "--version", "1", "--expires-at", "2020-01-01T00:00:00Z"},
		{"approve", srv.Key.CRID, strings.Repeat("a", 64), "--version", "1", "--expires-at", time.Now().AddDate(1, 0, 2).UTC().Format(time.RFC3339)},
		{"approve", srv.Key.CRID, strings.Repeat("a", 64), "--version", "1", "--credential-id", "id"},
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

func TestConsumerAccessApprovalInputs(t *testing.T) {
	for _, input := range []consumerAccessInput{
		{action: consumerApprove, decision: "new", kind: "device", credentialID: "agent-consumer01", key: "public-key"},
		{action: consumerApprove, decision: strings.Repeat("a", 64), entitlement: "receipt", expiry: "2027-01-01T00:00:00Z"},
		{action: consumerApprove, decision: strings.Repeat("a", 64)},
	} {
		if err := validateConsumerFields(&input); err != nil {
			t.Fatalf("valid approval rejected: %v", err)
		}
		if input.expiry != "" && input.expiresAt == nil {
			t.Fatal("contract expiry was not parsed")
		}
	}
	for _, input := range []consumerAccessInput{
		{action: consumerApprove, decision: "new", kind: "device", credentialID: "id", key: "key", entitlement: "receipt", expiry: "2027-01-01T00:00:00Z"},
		{action: consumerApprove, decision: "new", kind: "device", credentialID: strings.Repeat("a", 172), key: "key"},
		{action: consumerApprove, decision: "new", kind: "device", credentialID: "id", key: strings.Repeat("a", 257)},
		{action: consumerApprove, decision: strings.Repeat("a", 64), entitlement: "receipt", expiry: "invalid"},
	} {
		if err := validateConsumerFields(&input); err == nil {
			t.Fatal("invalid approval accepted")
		}
	}
}

func TestPublishConsumerPolicyRejectsInvalidFlags(t *testing.T) {
	for _, input := range []struct {
		mode, contract string
		public         bool
	}{
		{"", "receipt", false}, {"review", "", true}, {"contract", "   ", false}, {"contract", strings.Repeat("a", 129), false}, {"review", "   ", false},
	} {
		access := &qurlapi.PublishOptions{Public: input.public}
		if err := setPublishConsumerPolicy(access, input.mode, input.contract); err == nil || access.ConsumerAccess != nil {
			t.Fatal("invalid publish policy accepted")
		}
	}
}

func TestPublishRejectsExplicitEmptyAccessBeforeRequest(t *testing.T) {
	srv := apitest.NewServer(t)
	for _, mode := range []string{"", "   "} {
		result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", "https://example.com/report", "--access=" + mode}})
		if result.code != 2 || !strings.Contains(result.stderr.String(), "--access requires") {
			t.Fatalf("mode=%q exit=%d error=%s", mode, result.code, result.stderr.String())
		}
	}
	if len(srv.Requests()) != 0 {
		t.Fatal("empty private policy reached API")
	}
}

func TestConsumerAccessApprovalRequestBodies(t *testing.T) {
	for _, key := range []bool{false, true} {
		t.Run(map[bool]string{false: "contract", true: "key"}[key], func(t *testing.T) {
			srv := apitest.NewServer(t)
			id, version := strings.Repeat("a", 64), "1"
			flags := []string{"--entitlement", "receipt", "--expires-at", "2027-01-01T00:00:00Z"}
			if key {
				id, version = "new", "0"
				flags = []string{"--credential-kind", "device", "--credential-id", "agent-consumer01", "--credential-key", "public-key"}
			}
			srv.Script(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/consumer-grants/"+id+"/approve", func(w http.ResponseWriter, r *http.Request) {
				var body qurl.ConsumerGrantApproval
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if key {
					if body.ExpectedVersion != 0 || body.CredentialKind != "device" || body.CredentialID != "agent-consumer01" || body.CredentialPublicKey != "public-key" || body.EntitlementID != "" || body.ExpiresAt != nil {
						t.Fatalf("key body=%+v", body)
					}
				} else if body.ExpectedVersion != 1 || body.EntitlementID != "receipt" || body.ExpiresAt == nil || body.ExpiresAt.Format("2006-01-02T15:04:05Z07:00") != "2027-01-01T00:00:00Z" || body.CredentialKind != "" || body.CredentialID != "" || body.CredentialPublicKey != "" {
					t.Fatalf("contract body=%+v", body)
				}
				apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{"id": strings.Repeat("a", 64), "status": "active", "version": 2}, nil)
			})
			args := make([]string, 0, 10+len(flags))
			args = append(args, "--endpoint", srv.URL, "access", "approve", srv.Key.CRID, id, "--version", version, "-o", "json")
			result := runCLI(t, &runOpts{args: append(args, flags...)})
			if result.code != 0 || len(srv.Requests()) != 1 {
				t.Fatalf("exit=%d error=%s", result.code, result.stderr.String())
			}
		})
	}
}

func TestPublishRejectsExplicitEmptyContractBeforeRequest(t *testing.T) {
	srv := apitest.NewServer(t)
	for _, contract := range []string{"", "   "} {
		result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", "https://example.com/report", "--contract=" + contract}})
		if result.code != 2 || !strings.Contains(result.stderr.String(), "--contract requires") {
			t.Fatalf("contract=%q exit=%d error=%s", contract, result.code, result.stderr.String())
		}
	}
	if len(srv.Requests()) != 0 {
		t.Fatal("empty contract reached API")
	}
}
