package main

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/accessrules"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

func TestRulesCreateTakesSettingsForItsModeOnly(t *testing.T) {
	dir := connectorStateTestDir(t)
	for _, tc := range []struct {
		args []string
		want accessrules.Definition
	}{
		{[]string{"Quick look", "--mode", "public", "--expires-in", "1h", "--one-browser", "--session-length", "30m"},
			accessrules.Definition{Mode: "public", LinkLimits: &accessrules.LinkLimits{ExpiresIn: "1h", OneTimeUse: new(true), SessionDuration: new("30m")}}},
		{[]string{"Stays open", "--mode", "public", "--expires-in", "1h", "--once", "--stays-open-for", "30m"},
			accessrules.Definition{Mode: "public", LinkLimits: &accessrules.LinkLimits{ExpiresIn: "1h", OneTimeUse: new(true), SessionDuration: new("30m")}}},
		{[]string{"First look", "--mode", "public", "--expires-in", "1h", "--once"},
			accessrules.Definition{Mode: "public", LinkLimits: &accessrules.LinkLimits{ExpiresIn: "1h", OneTimeUse: new(true)}}},
		{[]string{"Reviewers", "--mode", "review", "--approvals-last", "7d"}, accessrules.Definition{Mode: "review", ApprovalExpiresIn: new("7d")}},
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
		{[]string{"--mode", "public", "--expires-in", "1h", "--session-length", "2d"}, "--stays-open-for must be whole seconds from 1s to 24h"},
		{[]string{"--mode", "public", "--approvals-last", "7d"}, "--approvals-last applies only to --mode review or keys"},
		{[]string{"--mode", "review", "--approvals-last", "2d"}, "--approvals-last must be one of"},
		// Removed settings are gone, not ignored.
		{[]string{"--mode", "public", "--expires-in", "1h", "--max-sessions", "3"}, "unknown flag: --max-sessions"},
		{[]string{"--mode", "review", "--allow-download"}, "unknown flag: --allow-download"},
	} {
		result := runCLI(t, &runOpts{args: append([]string{"rules", "create", "Bad"}, tc.args...), env: map[string]string{}, shareStateDir: dir})
		if result.code != 2 || !strings.Contains(result.stderr.String(), tc.want) {
			t.Errorf("%v: exit %d %s", tc.args, result.code, result.stderr.String())
		}
	}
	// Released Desktop detects rule settings by these names in help.
	help := runCLI(t, &runOpts{args: []string{"rules", "create", "--help"}, env: map[string]string{}, shareStateDir: dir}).stdout.String()
	for _, want := range []string{"--once", "--one-browser", "--stays-open-for", "--session-length", "--expires-in", "--approvals-last"} {
		if !strings.Contains(help, want) {
			t.Errorf("help lacks %s", want)
		}
	}
	listed := runCLI(t, &runOpts{args: []string{"rules", "list"}, env: map[string]string{}, shareStateDir: dir})
	for _, want := range []string{"Anyone with the link · links expire in 1 hour · only the first person · stays open for 30 minutes", "People you approve · approvals last 7 days"} {
		if !strings.Contains(listed.stdout.String(), want) {
			t.Errorf("list lacks %q:\n%s", want, listed.stdout.String())
		}
	}
	if strings.Contains(listed.stdout.String(), "Bad") {
		t.Error("an invalid rule was saved")
	}
}

// scriptPublish answers POST /v1/resources with the requested policy echoed
// and records the consumer_access the CLI sent.
func scriptPublish(t *testing.T, srv *apitest.Server, private, foundExisting bool, existing, sent map[string]any) {
	t.Helper()
	srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Policy map[string]any `json:"consumer_access"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if sent != nil {
			maps.Copy(sent, body.Policy)
		}
		policy := body.Policy
		if existing != nil {
			policy = existing
		}
		data := map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": private}
		if policy != nil {
			data["consumer_access"] = policy
		}
		apitest.WriteEnvelope(t, w, http.StatusCreated, data, map[string]any{"found_existing": foundExisting})
	})
}

// scriptMint answers up to three limited-link mints and records each request body.
func scriptMint(t *testing.T, srv *apitest.Server, bodies *[]map[string]any) {
	t.Helper()
	srv.ScriptRepeat(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/qurls", 3, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		*bodies = append(*bodies, body)
		apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "qurl_link": "https://qurl.link/at_limited", "expires_at": "2026-10-30T13:00:00Z"}, nil)
	})
}

const hourRule = `{"id":"11111111-1111-4111-8111-111111111111","name":"Hour","mode":"public","link_limits":{"expires_in":"1h","session_duration":"30m"}}`

func TestPublishWithLinkLimitsMintsAndLaterSharesUseThem(t *testing.T) {
	srv := apitest.NewServer(t)
	dir := connectorStateTestDir(t)
	var mints []map[string]any
	scriptPublish(t, srv, false, false, nil, nil)
	scriptMint(t, srv, &mints)
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", "https://api.example.com", "--rule-file", "-", "-o", "json"}, stdin: strings.NewReader(hourRule), shareStateDir: dir})
	var out struct {
		Link struct {
			QURL      string `json:"qurl"`
			ExpiresAt string `json:"expires_at"`
		} `json:"link"`
	}
	if result.code != 0 || json.Unmarshal(result.stdout.Bytes(), &out) != nil || out.Link.QURL != "https://qurl.link/at_limited" || out.Link.ExpiresAt == "" {
		t.Fatalf("publish %d %s %s", result.code, result.stdout.String(), result.stderr.String())
	}
	share := func(extra ...string) {
		t.Helper()
		args := append([]string{"--endpoint", srv.URL, "share", srv.Key.CRID}, extra...)
		if result := runCLI(t, &runOpts{args: args, shareStateDir: dir}); result.code != 0 || result.stdout.String() != "https://qurl.link/at_limited\n" {
			t.Fatalf("share %v: %d %q %s", extra, result.code, result.stdout.String(), result.stderr.String())
		}
	}
	share()
	share("--ttl", "2h", "--session-duration", "5m")
	if len(mints) != 3 {
		t.Fatalf("mints = %d, want publish + 2 shares", len(mints))
	}
	for i, want := range []struct{ expires, session string }{{"1h", "30m"}, {"1h", "30m"}, {"2h", "5m"}} {
		got := mints[i]
		if got["expires_in"] != want.expires || got["session_duration"] != want.session || got["one_time_use"] != false || got["max_sessions"] != float64(0) {
			t.Errorf("mint %d sent %v, want expires_in %s and session_duration %s", i, got, want.expires, want.session)
		}
	}
}

func TestPublishSaysASingleUseLinkWorksOnce(t *testing.T) {
	srv := apitest.NewServer(t)
	var mints []map[string]any
	scriptPublish(t, srv, false, false, nil, nil)
	scriptMint(t, srv, &mints)
	rule := `{"id":"11111111-1111-4111-8111-111111111111","name":"Once","mode":"public","link_limits":{"expires_in":"1h","one_time_use":true}}`
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "--color", "never", "publish", "https://api.example.com", "--rule-file", "-"}, stdin: strings.NewReader(rule), shareStateDir: connectorStateTestDir(t), tty: true})
	want := "Only the first person can open it. Run qurl share " + srv.Key.CRID + " for a new link."
	if result.code != 0 || !strings.Contains(result.stdout.String(), want) || len(mints) != 1 || mints[0]["one_time_use"] != true {
		t.Fatalf("publish %d mints %v\n%s%s", result.code, mints, result.stdout.String(), result.stderr.String())
	}
}

func TestShareFailsClosedOnDamagedLinkLimits(t *testing.T) {
	srv := apitest.NewServer(t)
	dir := connectorStateTestDir(t)
	if err := os.WriteFile(filepath.Join(dir, "share_rules.json"), []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "share", srv.Key.CRID}, shareStateDir: dir})
	if result.code != 3 || !strings.Contains(result.stderr.String(), "share_rules.json") || len(srv.Requests()) != 0 {
		t.Fatalf("share without its limits: %d %s", result.code, result.stderr.String())
	}
}

func TestDamagedLinkLimitsNeverFailPlainRepublishOrDelete(t *testing.T) {
	srv := apitest.NewServer(t)
	dir := connectorStateTestDir(t)
	if err := os.WriteFile(filepath.Join(dir, "share_rules.json"), []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	scriptPublish(t, srv, true, true, nil, nil)
	for _, args := range [][]string{{"publish", "https://api.example.com"}, {"delete", srv.Key.CRID, "--yes"}} {
		result := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL}, args...), shareStateDir: dir})
		if result.code != 0 || !strings.Contains(result.stderr.String(), "share_rules.json") {
			t.Fatalf("%v: %d %s", args, result.code, result.stderr.String())
		}
	}
}

func TestShareWithoutSavedLimitsLeavesStateUntouched(t *testing.T) {
	srv := apitest.NewServer(t)
	dir := filepath.Join(t.TempDir(), "never-created")
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "share", srv.Key.CRID}, shareStateDir: dir})
	if _, err := os.Lstat(dir); result.code != 0 || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("share %d created state (%v): %s", result.code, err, result.stderr.String())
	}
}

func TestPublishSendsApprovalsLastInThePolicy(t *testing.T) {
	for _, mode := range []string{"review", "keys"} {
		srv := apitest.NewServer(t)
		sent := map[string]any{}
		scriptPublish(t, srv, true, false, nil, sent)
		rule := `{"id":"11111111-1111-4111-8111-111111111111","name":"Week","mode":"` + mode + `","approval_expires_in":"7d"}`
		result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", "https://api.example.com", "--rule-file", "-", "-o", "json"}, stdin: strings.NewReader(rule), shareStateDir: connectorStateTestDir(t)})
		if result.code != 0 || sent["mode"] != mode || sent["approval_expires_in"] != "7d" {
			t.Fatalf("%s: %d sent %v %s", mode, result.code, sent, result.stderr.String())
		}
		if !strings.Contains(result.stdout.String(), `"approval_expires_in": "7d"`) {
			t.Errorf("%s: JSON lacks the policy's approval lifetime: %s", mode, result.stdout.String())
		}
	}
}

func TestRepublishWithOtherSettingsWarns(t *testing.T) {
	const warning = "This item is already shared with other settings. Use qurl access configure to change it."
	for _, tc := range []struct {
		name     string
		private  bool
		existing map[string]any
		rule     string
		differ   bool
	}{
		{"approvals", true, map[string]any{"mode": "review", "version": 1, "approval_expires_in": "30d"}, `{"id":"11111111-1111-4111-8111-111111111111","name":"Week","mode":"review","approval_expires_in":"7d"}`, true},
		{"same approvals", true, map[string]any{"mode": "review", "version": 1, "approval_expires_in": "7d"}, `{"id":"11111111-1111-4111-8111-111111111111","name":"Week","mode":"review","approval_expires_in":"7d"}`, false},
		{"link limits", false, nil, hourRule, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			var mints []map[string]any
			scriptPublish(t, srv, tc.private, true, tc.existing, nil)
			scriptMint(t, srv, &mints)
			result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", "https://api.example.com", "--rule-file", "-", "-o", "json"}, stdin: strings.NewReader(tc.rule), shareStateDir: connectorStateTestDir(t)})
			var out struct {
				SettingsDiffer bool `json:"settings_differ"`
			}
			if result.code != 0 || json.Unmarshal(result.stdout.Bytes(), &out) != nil {
				t.Fatalf("publish %d %s %s", result.code, result.stdout.String(), result.stderr.String())
			}
			if out.SettingsDiffer != tc.differ || strings.Contains(result.stderr.String(), warning) != tc.differ {
				t.Errorf("settings_differ %v, stderr %q", out.SettingsDiffer, result.stderr.String())
			}
			// The existing share keeps its settings: no link with the new rule's limits.
			if len(mints) != 0 {
				t.Errorf("minted %v for an existing share without saved limits", mints)
			}
		})
	}
}

func TestRepublishTreatsAnExplicitDefaultAsTheSameSettings(t *testing.T) {
	srv := apitest.NewServer(t)
	dir := connectorStateTestDir(t)
	var mints []map[string]any
	scriptMint(t, srv, &mints)
	srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": false}, map[string]any{"found_existing": false})
	}, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": false}, map[string]any{"found_existing": true})
	})
	for _, rule := range []string{
		`{"id":"11111111-1111-4111-8111-111111111111","name":"Hour","mode":"public","link_limits":{"expires_in":"1h","one_time_use":false}}`,
		`{"id":"22222222-2222-4222-8222-222222222222","name":"Also hour","mode":"public","link_limits":{"expires_in":"1h"}}`,
	} {
		result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", "https://api.example.com", "--rule-file", "-", "-o", "json"}, stdin: strings.NewReader(rule), shareStateDir: dir})
		if result.code != 0 || strings.Contains(result.stdout.String(), "settings_differ") || strings.Contains(result.stderr.String(), "other settings") {
			t.Fatalf("publish %d %s %s", result.code, result.stdout.String(), result.stderr.String())
		}
	}
	if len(mints) != 2 {
		t.Fatalf("mints = %d, want the first link and the existing share's link", len(mints))
	}
}

func TestPublishRuleOnceMintsAndRepublishMintsAgain(t *testing.T) {
	srv := apitest.NewServer(t)
	dir := connectorStateTestDir(t)
	var mints []map[string]any
	scriptMint(t, srv, &mints)
	for i, args := range [][]string{{"--rule", "once"}, {"--rule", "Only the first person"}, {"--public"}} {
		scriptPublish(t, srv, false, i > 0, nil, nil)
		args = append([]string{"--endpoint", srv.URL, "publish", "https://api.example.com", "-o", "json"}, args...)
		result := runCLI(t, &runOpts{args: args, shareStateDir: dir})
		var out struct {
			Link struct {
				SingleUse bool `json:"single_use"`
			} `json:"link"`
		}
		if result.code != 0 || json.Unmarshal(result.stdout.Bytes(), &out) != nil || !out.Link.SingleUse {
			t.Fatalf("publish %v: %d %s %s", args, result.code, result.stdout.String(), result.stderr.String())
		}
	}
	if len(mints) != 3 {
		t.Fatalf("mints = %d, want one per publish", len(mints))
	}
	for i, got := range mints {
		if got["expires_in"] != "1h" || got["one_time_use"] != true || got["session_duration"] != "1h" {
			t.Errorf("mint %d sent %v", i, got)
		}
	}
	saved := func() string {
		data, err := os.ReadFile(filepath.Join(dir, "share_rules.json")) //nolint:gosec // G304: test-owned temporary state directory and fixed filename.
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if !strings.Contains(saved(), srv.Key.CRID) {
		t.Fatal("publish did not save the link limits")
	}
	if result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "delete", srv.Key.CRID, "--yes"}, shareStateDir: dir}); result.code != 0 || strings.Contains(saved(), srv.Key.CRID) {
		t.Fatalf("delete %d kept the link limits: %s", result.code, result.stderr.String())
	}
}

func TestQuietPublishWithOncePrintsItsLink(t *testing.T) {
	srv := apitest.NewServer(t)
	var mints []map[string]any
	scriptMint(t, srv, &mints)
	scriptPublish(t, srv, false, false, nil, nil)
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", "https://api.example.com", "--rule", "once", "-q"}, shareStateDir: connectorStateTestDir(t)})
	lines := strings.Split(strings.TrimSpace(result.stdout.String()), "\n")
	if result.code != 0 || len(lines) != 2 || lines[0] != srv.Key.CRID || !strings.HasPrefix(lines[1], "https://") {
		t.Fatalf("quiet publish %d %q %s", result.code, result.stdout.String(), result.stderr.String())
	}
}

func TestRuleNamedOnlyTheFirstPersonIsTaken(t *testing.T) {
	for _, name := range []string{"only the first person", "once", "ONCE", "builtin.public", "Builtin.x"} {
		result := runCLI(t, &runOpts{args: []string{"rules", "create", name, "--mode", "invite_only"}, env: map[string]string{}, shareStateDir: connectorStateTestDir(t)})
		if result.code == 0 || !strings.Contains(result.stderr.String(), "already exists") {
			t.Fatalf("create %q %d %s", name, result.code, result.stderr.String())
		}
	}
}
