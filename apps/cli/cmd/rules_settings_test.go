package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/accessrules"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

func TestRulesCreateTakesSettingsForItsModeOnly(t *testing.T) {
	dir := connectorStateTestDir(t)
	for _, tc := range []struct {
		args []string
		want accessrules.Definition
	}{
		{[]string{"Quick look", "--mode", "public", "--expires-in", "1h", "--one-browser", "--max-sessions", "3", "--session-length", "30m"},
			accessrules.Definition{Mode: "public", LinkLimits: &accessrules.LinkLimits{ExpiresIn: "1h", OneTimeUse: new(true), MaxSessions: new(3), SessionDuration: new("30m")}}},
		{[]string{"Reviewers", "--mode", "review", "--approvals-last", "7d", "--allow-download"}, accessrules.Definition{Mode: "review", ApprovalExpiresIn: new("7d"), AllowDownload: new(true)}},
		{[]string{"Laptops", "--mode", "keys", "--approvals-last", "never"}, accessrules.Definition{Mode: "keys"}},
	} {
		result := runCLI(t, &runOpts{args: append([]string{"rules", "create", "-o", "json"}, tc.args...), env: map[string]string{}, shareStateDir: dir})
		var got accessrules.Definition
		if result.code != 0 || json.Unmarshal(result.stdout.Bytes(), &got) != nil {
			t.Fatalf("%v: %d %s", tc.args, result.code, result.stderr.String())
		}
		tc.want.ID, tc.want.Name = got.ID, tc.args[0]
		if !accessrules.EqualItems([]accessrules.Definition{got}, []accessrules.Definition{tc.want}) {
			t.Errorf("%v: created %+v", tc.args, got)
		}
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--mode", "review", "--expires-in", "1h"}, "apply only to --mode public"},
		{[]string{"--mode", "public", "--one-browser"}, "need --expires-in"},
		{[]string{"--mode", "public", "--expires-in", "45d"}, "--expires-in must be from 1m to 30d"},
		{[]string{"--mode", "public", "--expires-in", "1h", "--max-sessions", "1001"}, "--max-sessions must be from 0 to 1000"},
		{[]string{"--mode", "public", "--expires-in", "1h", "--session-length", "2d"}, "--session-length must be whole seconds from 1s to 24h"},
		{[]string{"--mode", "public", "--approvals-last", "7d"}, "--approvals-last applies only to --mode review or keys"},
		{[]string{"--mode", "review", "--approvals-last", "2d"}, "--approvals-last must be one of"},
	} {
		result := runCLI(t, &runOpts{args: append([]string{"rules", "create", "Bad"}, tc.args...), env: map[string]string{}, shareStateDir: dir})
		if result.code != 2 || !strings.Contains(result.stderr.String(), tc.want) {
			t.Errorf("%v: exit %d %s", tc.args, result.code, result.stderr.String())
		}
	}
	listed := runCLI(t, &runOpts{args: []string{"rules", "list"}, env: map[string]string{}, shareStateDir: dir})
	for _, want := range []string{"Anyone with the link · expires in 1 hour · one browser · up to 3 sessions · sessions last 30 minutes", "People you approve · approvals last 7 days · downloads allowed"} {
		if !strings.Contains(listed.stdout.String(), want) {
			t.Errorf("list lacks %q:\n%s", want, listed.stdout.String())
		}
	}
	if strings.Contains(listed.stdout.String(), "Bad") {
		t.Error("an invalid rule was saved")
	}
}

func TestPublishWithLinkLimitsSharesAMintedLink(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": false}, nil)
	})
	srv.Script(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/qurls", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["expires_in"] != "1h" || body["one_time_use"] != false || body["max_sessions"] != float64(0) || body["session_duration"] != "30m" {
			t.Fatalf("limits lost: %v", body)
		}
		apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "qurl_link": "https://qurl.link/at_limited", "expires_at": "2026-10-30T13:00:00Z"}, nil)
	})
	rule := `{"id":"11111111-1111-4111-8111-111111111111","name":"Hour","mode":"public","link_limits":{"expires_in":"1h","session_duration":"30m"}}`
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", "https://api.example.com", "--rule-file", "-", "-o", "json"}, stdin: strings.NewReader(rule), shareStateDir: connectorStateTestDir(t)})
	var out struct {
		Link struct {
			QURL      string `json:"qurl"`
			ExpiresAt string `json:"expires_at"`
		} `json:"link"`
	}
	if result.code != 0 || json.Unmarshal(result.stdout.Bytes(), &out) != nil || out.Link.QURL != "https://qurl.link/at_limited" || out.Link.ExpiresAt == "" {
		t.Fatalf("publish %d %s %s", result.code, result.stdout.String(), result.stderr.String())
	}
}

func TestApprovalsLastAsLongAsTheSharesRule(t *testing.T) {
	srv := apitest.NewServer(t)
	dir := connectorStateTestDir(t)
	srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Policy map[string]any `json:"consumer_access"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": true, "consumer_access": body.Policy}, nil)
	})
	var got *time.Time
	rule := `{"id":"11111111-1111-4111-8111-111111111111","name":"Week","mode":"keys","approval_expires_in":"7d"}`
	if result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", "https://api.example.com", "--rule-file", "-", "-o", "json"}, stdin: strings.NewReader(rule), shareStateDir: dir}); result.code != 0 {
		t.Fatalf("publish %d %s", result.code, result.stderr.String())
	}
	approve := func(extra ...string) {
		t.Helper()
		got = nil
		srv.Script(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/consumer-grants/new/approve", func(w http.ResponseWriter, r *http.Request) {
			var body qurl.ConsumerGrantApproval
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			got = body.ExpiresAt
			apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{"id": strings.Repeat("a", 64), "status": "active", "version": 1}, nil)
		})
		args := append([]string{"--endpoint", srv.URL, "access", "approve", srv.Key.CRID, "new", "--version", "0", "--credential-kind", "device", "--credential-id", "agent-consumer01", "--credential-key", "public-key", "-o", "json"}, extra...)
		if result := runCLI(t, &runOpts{args: args, shareStateDir: dir}); result.code != 0 {
			t.Fatalf("approve %d %s", result.code, result.stderr.String())
		}
	}
	approve()
	if want := fixedNow.Add(7 * 24 * time.Hour); got == nil || !got.Equal(want) {
		t.Fatalf("expires_at %v, want %v", got, want)
	}
	override := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	approve("--expires-at", override.Format(time.RFC3339))
	if got == nil || !got.Equal(override) {
		t.Fatalf("--expires-at override lost: %v", got)
	}
	// A policy set without a rule replaces the share's rule settings.
	srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Policy map[string]any `json:"consumer_access"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": true, "consumer_access": body.Policy}, nil)
	})
	if result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "access", "configure", srv.Key.CRID, "--mode", "keys", "--version", "1", "-o", "json"}, shareStateDir: dir}); result.code != 0 {
		t.Fatalf("configure %d %s", result.code, result.stderr.String())
	}
	approve()
	if got != nil {
		t.Fatalf("cleared rule still set expires_at %v", got)
	}
}

func TestApprovalFailsClosedOnDamagedShareRules(t *testing.T) {
	srv := apitest.NewServer(t)
	dir := connectorStateTestDir(t)
	if err := os.WriteFile(filepath.Join(dir, "share_rules.json"), []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "access", "approve", srv.Key.CRID, strings.Repeat("a", 64), "--version", "1"}, shareStateDir: dir})
	if result.code != 3 || !strings.Contains(result.stderr.String(), "share_rules.json") || len(srv.Requests()) != 0 {
		t.Fatalf("approval without its lifetime: %d %s", result.code, result.stderr.String())
	}
}
