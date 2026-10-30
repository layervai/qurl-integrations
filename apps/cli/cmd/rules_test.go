package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/accessrules"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

func TestRulesOfflineBridgePersistsAndRejectsStaleUI(t *testing.T) {
	dir := t.TempDir()
	input := `{"id":"11111111-1111-4111-8111-111111111111","name":"Team","mode":"email_domain","email_domain":"example.com","allowed_emails":["Partner@elsewhere.com"]}`
	saved := runCLI(t, &runOpts{args: []string{"rules", "save", "--version", "0", "--file", "-", "-o", "json"}, stdin: strings.NewReader(input), env: map[string]string{}, shareStateDir: dir})
	if saved.code != 0 {
		t.Fatalf("save: %d %s", saved.code, saved.stderr.String())
	}
	var snapshot accessrules.Snapshot
	if err := json.Unmarshal(saved.stdout.Bytes(), &snapshot); err != nil || snapshot.LocalVersion != 1 || len(snapshot.Items) != 1 {
		t.Fatalf("snapshot %s %v", saved.stdout.String(), err)
	}
	listed := runCLI(t, &runOpts{args: []string{"rules", "list", "-o", "json"}, env: map[string]string{}, shareStateDir: dir})
	if listed.code != 0 || !strings.Contains(listed.stdout.String(), "Partner@elsewhere.com") {
		t.Fatalf("restart %s %s", listed.stdout.String(), listed.stderr.String())
	}
	stale := runCLI(t, &runOpts{args: []string{"rules", "delete", snapshot.Items[0].ID, "--version", "0", "-o", "json"}, env: map[string]string{}, shareStateDir: dir})
	if stale.code != 7 {
		t.Fatalf("stale exit %d: %s", stale.code, stale.stderr.String())
	}
	synced := runCLI(t, &runOpts{args: []string{"rules", "sync", "-o", "json"}, env: map[string]string{}, shareStateDir: dir})
	if synced.code != 0 || !strings.Contains(synced.stdout.String(), `"sync": "local"`) {
		t.Fatalf("unregistered sync %d %s %s", synced.code, synced.stdout.String(), synced.stderr.String())
	}
}
func TestRuleSnapshotPublishingSurvivesLibraryDeletion(t *testing.T) {
	srv := apitest.NewServer(t)
	policy := map[string]any{"mode": "email_domain", "version": 1, "email_domain": "example.com", "allowed_emails": []string{"Person@other.com"}}
	srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(body["consumer_access"])
		expected, _ := json.Marshal(policy)
		if !bytes.Equal(raw, expected) {
			t.Fatalf("snapshot lost: %s", raw)
		}
		apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": true, "consumer_access": policy}, nil)
	})
	// No saved rule remains: stdin is the definition selected earlier by Desktop.
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", "https://api.example.com", "--rule-file", "-", "-o", "json"}, stdin: strings.NewReader(`{"id":"11111111-1111-4111-8111-111111111111","name":"Deleted rule","mode":"email_domain","email_domain":"example.com","allowed_emails":["Person@other.com"]}`)})
	if result.code != 0 || !strings.Contains(result.stdout.String(), "Person@other.com") {
		t.Fatalf("publish %d %s %s", result.code, result.stdout.String(), result.stderr.String())
	}
}
func TestRuleFlagsRejectCombinationsBeforeStateOrNetwork(t *testing.T) {
	for _, flags := range [][]string{{"--rule", "builtin.keys", "--public=false"}, {"--rule", "builtin.keys", "--access", "review"}, {"--rule", "builtin.keys", "--rule-file", "-"}} {
		result := runCLI(t, &runOpts{args: append([]string{"publish", "https://api.example.com"}, flags...), env: map[string]string{}})
		if result.code != 2 {
			t.Fatalf("flags %v exit %d %s", flags, result.code, result.stderr.String())
		}
	}
}

func TestRuleSnapshotConfiguresPrivateDomainWithVersionGuard(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Policy   map[string]any `json:"consumer_access"`
			Expected int64          `json:"expected_consumer_access_version"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Expected != 3 || body.Policy["version"] != float64(4) || body.Policy["email_domain"] != "example.com" {
			t.Fatalf("snapshot/version lost %+v", body)
		}
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": true, "consumer_access": body.Policy}, nil)
	})
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "access", "configure", srv.Key.CRID, "--rule-file", "-", "--version", "3", "-o", "json"}, stdin: strings.NewReader(`{"id":"00000000-0000-0000-0000-000000000000","name":"People at a domain","mode":"email_domain","email_domain":"example.com"}`)})
	if result.code != 0 || !strings.Contains(result.stdout.String(), "example.com") {
		t.Fatalf("configure %d %s %s", result.code, result.stdout.String(), result.stderr.String())
	}
}
func TestRulesSyncAuthFailureKeepsSavedWorkAndIntegrityFailsClosed(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusUnauthorized, http.StatusServiceUnavailable, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.Script(http.MethodGet, "/v1/me/access-rules", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"secret":"private-mailbox@example.com"}`))
			})
			dir := t.TempDir()
			store, err := connectorstate.OpenAccessRules(dir, srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.Save(t.Context(), 0, &accessrules.Definition{ID: "11111111-1111-4111-8111-111111111111", Name: "Saved", Mode: "keys"}); err != nil {
				t.Fatal(err)
			}
			result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "rules", "sync", "-o", "json"}, shareStateDir: dir})
			if status == http.StatusOK {
				if result.code == 0 {
					t.Fatal("invalid response treated as empty")
				}
			} else if result.code != 0 {
				t.Fatalf("deferred sync exited %d %s", result.code, result.stderr.String())
			}
			if strings.Contains(result.stderr.String(), "private-mailbox") {
				t.Fatal("raw body leaked")
			}
			snapshot, err := store.Snapshot(t.Context())
			if err != nil || len(snapshot.Items) != 1 {
				t.Fatalf("pending work lost %+v %v", snapshot, err)
			}
		})
	}
}

func TestRulesHumanCreateNeedsNoUUIDOrVersion(t *testing.T) {
	result := runCLI(t, &runOpts{args: []string{"rules", "create", "Team", "--mode", "email_domain", "--email-domain", "example.com", "-o", "json"}, env: map[string]string{}})
	var snapshot accessrules.Snapshot
	if result.code != 0 || json.Unmarshal(result.stdout.Bytes(), &snapshot) != nil || len(snapshot.Items) != 1 || snapshot.Items[0].Validate(false) != nil || snapshot.Items[0].Name != "Team" {
		t.Fatalf("create %d %s %s", result.code, result.stdout.String(), result.stderr.String())
	}
}

func TestLocalRepublishKeepsSelectedDomainSnapshot(t *testing.T) {
	srv := apitest.NewServer(t)
	policy := map[string]any{"mode": "email_domain", "version": 4, "email_domain": "example.com", "allowed_emails": []string{"Partner@outside.com"}}
	srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["type"] != "tunnel" {
			t.Fatal("not local publish")
		}
		apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": true, "consumer_access": policy}, map[string]any{"found_existing": true})
	})
	options := servingLocalPublish(t, srv, true, "--rule-file", "-", "-o", "json")
	options.stdin = strings.NewReader(`{"id":"11111111-1111-4111-8111-111111111111","name":"Selected earlier","mode":"email_domain","email_domain":"example.com","allowed_emails":["Partner@outside.com"]}`)
	result := runCLI(t, options)
	if result.code != 0 || !strings.Contains(result.stdout.String(), "Partner@outside.com") {
		t.Fatalf("local snapshot lost %d %s %s", result.code, result.stdout.String(), result.stderr.String())
	}
}
