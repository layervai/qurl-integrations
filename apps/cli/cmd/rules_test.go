package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/accessrules"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

func TestRulesOfflineBridgePersistsAndRejectsStaleUI(t *testing.T) {
	dir := connectorStateTestDir(t)
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
			dir := connectorStateTestDir(t)
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
	dir := connectorStateTestDir(t)
	result := runCLI(t, &runOpts{args: []string{"rules", "create", " Team ", "--mode", "email_domain", "--email-domain", " Example.COM ", "--emails-file", "-", "-o", "json"}, stdin: strings.NewReader(`[" Partner@Outside.EXAMPLE "]`), env: map[string]string{}, shareStateDir: dir})
	var created accessrules.Definition
	if result.code != 0 || json.Unmarshal(result.stdout.Bytes(), &created) != nil || created.Validate(false) != nil || created.Name != "Team" ||
		created.EmailDomain != "example.com" || len(created.AllowedEmails) != 1 || created.AllowedEmails[0] != "Partner@outside.example" {
		t.Fatalf("create %d %s %s", result.code, result.stdout.String(), result.stderr.String())
	}
	quiet := runCLI(t, &runOpts{args: []string{"rules", "create", "Reviewers", "--mode", "review", "-q"}, env: map[string]string{}, shareStateDir: dir})
	id := strings.TrimSpace(quiet.stdout.String())
	if quiet.code != 0 || !regexp.MustCompile(`^[0-9a-f-]{36}$`).MatchString(id) {
		t.Fatalf("quiet create printed %q: %s", quiet.stdout.String(), quiet.stderr.String())
	}
	text := runCLI(t, &runOpts{args: []string{"rules", "create", "Partners", "--mode", "keys"}, env: map[string]string{}, shareStateDir: dir})
	if text.code != 0 || strings.Count(text.stdout.String(), "\n") != 1 || !strings.Contains(text.stdout.String(), `"Partners"`) {
		t.Fatalf("create printed more than the new rule: %q", text.stdout.String())
	}
	listed := runCLI(t, &runOpts{args: []string{"rules", "list", "-q"}, env: map[string]string{}, shareStateDir: dir})
	if lines := strings.Split(strings.TrimSpace(listed.stdout.String()), "\n"); listed.code != 0 || len(lines) != 7 || !slices.Contains(lines, id) || strings.Contains(listed.stdout.String(), "Version") {
		t.Fatalf("quiet list %q", listed.stdout.String())
	}
}

func TestRulesHandTypedDeleteAndResolveTakeNamesWithoutVersion(t *testing.T) {
	dir := connectorStateTestDir(t)
	run := func(args ...string) *runResult {
		return runCLI(t, &runOpts{args: append([]string{"rules"}, args...), env: map[string]string{}, shareStateDir: dir})
	}
	if created := run("create", "Team", "--mode", "keys"); created.code != 0 {
		t.Fatalf("create %d %s", created.code, created.stderr.String())
	}
	if stale := run("delete", "Team", "--version", "0"); stale.code != 7 {
		t.Fatalf("stale delete %d %s", stale.code, stale.stderr.String())
	}
	if deleted := run("delete", "Team"); deleted.code != 0 || strings.Contains(deleted.stdout.String(), `"Team"`) {
		t.Fatalf("delete by name %d %s %s", deleted.code, deleted.stdout.String(), deleted.stderr.String())
	}
	for _, args := range [][]string{{"delete", "Team"}, {"resolve", "Team", "--use", "local"}} {
		if missing := run(args...); missing.code != 5 || !strings.Contains(missing.stderr.String(), "qurl rules list") {
			t.Fatalf("%v: %d %s", args, missing.code, missing.stderr.String())
		}
	}
}

func TestRulesErrorsNameTheCause(t *testing.T) {
	dir := connectorStateTestDir(t)
	run := func(stdin string, args ...string) *runResult {
		return runCLI(t, &runOpts{args: append([]string{"rules"}, args...), stdin: strings.NewReader(stdin), env: map[string]string{}, shareStateDir: dir})
	}
	if created := run("", "create", "Team", "--mode", "keys"); created.code != 0 {
		t.Fatalf("create %d %s", created.code, created.stderr.String())
	}
	for _, tc := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"create", " team", "--mode", "review"}, 8, "already exists"},
		{[]string{"create", "PEOPLE YOU INVITE", "--mode", "review"}, 8, "already exists"},
		{[]string{"create", "Other"}, 2, "--mode must be one of"},
		{[]string{"create", "Other", "--mode", "email_domain", "--email-domain", "not a domain"}, 2, "--email-domain must be a domain"},
		{[]string{"create", "Other", "--mode", "keys", "--email-domain", "example.com"}, 2, "apply only to --mode email_domain"},
		{[]string{"save", "--version", "1", "--file", "rule.json"}, 2, "use --file -"},
	} {
		result := run("", tc.args...)
		if result.code != tc.code || !strings.Contains(result.stderr.String(), tc.want) || strings.Contains(result.stderr.String(), "--rule-file") {
			t.Errorf("%v: exit %d %q, want %d %q", tc.args, result.code, result.stderr.String(), tc.code, tc.want)
		}
	}
	other := runCLI(t, &runOpts{args: []string{"--endpoint", "https://other.example.test", "rules", "list"}, env: map[string]string{}, shareStateDir: dir})
	if other.code != 3 || !strings.Contains(other.stderr.String(), "another account or deployment") {
		t.Fatalf("other deployment %d %s", other.code, other.stderr.String())
	}
	path := filepath.Join(dir, "access_rules.json")
	if err := os.WriteFile(path, []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	damaged := run("", "list")
	if damaged.code != 3 || !strings.Contains(damaged.stderr.String(), path) || !strings.Contains(damaged.stderr.String(), "Move that file aside") {
		t.Fatalf("damaged file %d %s", damaged.code, damaged.stderr.String())
	}
}

func TestPublishBuiltInRuleNeedsNoSavedRules(t *testing.T) {
	dir := connectorStateTestDir(t)
	if err := os.WriteFile(filepath.Join(dir, "access_rules.json"), []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, rule := range [][]string{{"--rule", "builtin.keys"}, {"--rule-file", "-"}} {
		srv := apitest.NewServer(t)
		srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Policy map[string]any `json:"consumer_access"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Policy["mode"] != "keys" {
				t.Fatalf("built-in policy lost: %+v %v", body, err)
			}
			apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": true, "consumer_access": body.Policy}, nil)
		})
		// A renamed built-in snapshot still matches by ID and mode.
		stdin := strings.NewReader(`{"id":"builtin.keys","name":"A renamed choice","mode":"keys"}`)
		result := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL, "publish", "https://api.example.com", "-o", "json"}, rule...), stdin: stdin, shareStateDir: dir})
		if result.code != 0 {
			t.Errorf("%v: %d %s", rule, result.code, result.stderr.String())
		}
	}
}

func TestPublishBuiltInRuleByName(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Policy map[string]any `json:"consumer_access"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Policy["mode"] != "invite_only" {
			t.Fatalf("built-in policy lost: %+v %v", body, err)
		}
		apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": true, "consumer_access": body.Policy}, nil)
	})
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", "https://api.example.com", "--rule", "People you invite", "-o", "json"}, shareStateDir: connectorStateTestDir(t)})
	if result.code != 0 {
		t.Fatalf("%d %s", result.code, result.stderr.String())
	}
}

func TestRulesSyncTextExplainsRulesThatStayLocal(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{0, "qurl account setup"},
		{http.StatusUnauthorized, "qurl login"},
		{http.StatusForbidden, "not linked to an active account"},
		{http.StatusServiceUnavailable, "run qurl rules sync later"},
	} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.Script(http.MethodGet, "/v1/me/access-rules", func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteProblem(t, w, tc.status, "unavailable", "not available", "")
			})
			var env map[string]string
			if tc.status == 0 {
				env = map[string]string{}
			}
			dir := connectorStateTestDir(t)
			for _, format := range []string{"text", "json"} {
				result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "rules", "sync", "-o", format}, env: env, shareStateDir: dir})
				warnings := strings.Count(result.stderr.String(), "\n")
				if result.code != 0 || (format == "text") != (warnings == 1 && strings.Contains(result.stderr.String(), tc.want)) || (format == "json" && warnings != 0) {
					t.Fatalf("%s: exit %d stderr %q", format, result.code, result.stderr.String())
				}
			}
		})
	}
}

func TestRulesSyncReportsTheLibraryLimit(t *testing.T) {
	srv := apitest.NewServer(t)
	account := make([]accessrules.Definition, 60)
	for i := range account {
		account[i] = accessrules.Definition{ID: fmt.Sprintf("%08d-2222-4222-8222-222222222222", i), Name: fmt.Sprintf("Account %d", i), Mode: "keys"}
	}
	srv.Script(http.MethodGet, "/v1/me/access-rules", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, accessrules.AccountLibrary{AccountID: "account-one", Version: 3, Items: account}, nil)
	})
	dir := connectorStateTestDir(t)
	store, err := connectorstate.OpenAccessRules(dir, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 41 {
		if _, err = store.Save(t.Context(), -1, &accessrules.Definition{ID: fmt.Sprintf("%08d-1111-4111-8111-111111111111", i), Name: fmt.Sprintf("Guest %d", i), Mode: "keys"}); err != nil {
			t.Fatal(err)
		}
	}
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "rules", "sync"}, shareStateDir: dir})
	if result.code != 8 || !strings.Contains(result.stderr.String(), "limited to 100 rules") {
		t.Fatalf("sync over the limit: %d %s", result.code, result.stderr.String())
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
