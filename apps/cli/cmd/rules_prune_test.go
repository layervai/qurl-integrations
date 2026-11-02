package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/accessrules"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

func TestDeletedResourcePrunesOnlyConfirmedTombstoneLimits(t *testing.T) {
	for _, command := range []string{"share", "publish", "get"} {
		for _, reply := range []struct {
			name   string
			status int
			code   string
			prune  bool
		}{
			{"tombstone", http.StatusGone, "resource_tombstoned", true},
			{"hidden or missing", http.StatusNotFound, "resource_not_found", false},
			{"forbidden", http.StatusForbidden, "forbidden", false},
			{"expired link", http.StatusGone, "expired", false},
			{"wrong status", http.StatusBadRequest, "resource_tombstoned", false},
		} {
			t.Run(command+"/"+reply.name, func(t *testing.T) {
				srv := apitest.NewServer(t)
				dir := connectorStateTestDir(t)
				store, err := connectorstate.OpenShareRules(dir)
				if err != nil {
					t.Fatal(err)
				}
				limits := &accessrules.LinkLimits{ExpiresIn: "1h"}
				if err := store.Set(t.Context(), srv.Key.CRID, limits); err != nil {
					t.Fatal(err)
				}
				args := []string{"--endpoint", srv.URL, command, srv.Key.CRID}
				route := "/v1/resources/" + srv.Key.CRID + "/qurls"
				if command == "get" {
					route = shareRoute(srv)
					args = append(args, "--file", filepath.Join(t.TempDir(), "download"))
				}
				if command == "publish" {
					scriptPublish(t, srv, false, true, nil, nil)
					args = []string{"--endpoint", srv.URL, "publish", "https://api.example.com", "--rule-file", "-"}
				}
				srv.Script(http.MethodPost, route, func(w http.ResponseWriter, _ *http.Request) {
					apitest.WriteProblem(t, w, reply.status, reply.code, "Refused", "resource request refused")
				})
				result := runCLI(t, &runOpts{args: args, stdin: strings.NewReader(hourRule), shareStateDir: dir})
				if result.code == 0 || strings.Contains(result.stdout.String(), "at_") {
					t.Fatalf("failed request became success: %d %s", result.code, result.stdout.String())
				}
				kept, err := store.Get(t.Context(), srv.Key.CRID)
				if err != nil || (kept == nil) != reply.prune {
					t.Fatalf("kept=%+v prune=%v error=%v stderr=%s", kept, reply.prune, err, result.stderr.String())
				}
			})
		}
	}
}

func TestTombstoneFreesFullLinkLimitStoreForNextPublish(t *testing.T) {
	srv := apitest.NewServer(t)
	dir := connectorStateTestDir(t)
	shares := map[string]accessrules.LinkLimits{srv.Key.CRID: {ExpiresIn: "1h"}}
	for i := 1; i < 10000; i++ {
		id := apitest.DeriveCRID(t, []byte(fmt.Sprintf("stored resource %d", i)), apitest.VersionTest)
		shares[id] = accessrules.LinkLimits{ExpiresIn: "1h"}
	}
	raw, err := json.Marshal(map[string]any{"schema_version": 1, "shares": shares})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "share_rules.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	oldID := srv.Key.CRID
	srv.Script(http.MethodPost, "/v1/resources/"+oldID+"/qurls", apitest.HandlerTombstoned410(t))
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "share", oldID}, shareStateDir: dir})
	if result.code != 5 {
		t.Fatalf("tombstone: %d %s", result.code, result.stderr.String())
	}
	srv.Key = apitest.GenerateResourceKey(t)
	scriptPublish(t, srv, false, false, nil, nil)
	var mints []map[string]any
	scriptMint(t, srv, &mints)
	result = runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", "https://api.example.com", "--rule-file", "-"}, stdin: strings.NewReader(hourRule), shareStateDir: dir})
	if result.code != 0 || len(mints) != 1 {
		t.Fatalf("next publish: %d mints=%v %s", result.code, mints, result.stderr.String())
	}

	raw, err = os.ReadFile(filepath.Join(dir, "share_rules.json")) //nolint:gosec // Test-owned profile fixture.
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Shares map[string]accessrules.LinkLimits `json:"shares"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Shares) != len(shares) {
		t.Fatalf("saved %d entries, want %d", len(saved.Shares), len(shares))
	}
	for id := range shares {
		kept, exists := saved.Shares[id]
		if exists != (id != oldID) || exists && (kept.ExpiresIn != "1h" || kept.OneTimeUse != nil || kept.SessionDuration != nil) {
			t.Fatalf("unexpected change for %s: %+v exists=%v", id, kept, exists)
		}
	}
	kept, exists := saved.Shares[srv.Key.CRID]
	if !exists || kept.ExpiresIn != "1h" || kept.SessionDuration == nil || *kept.SessionDuration != "30m" {
		t.Fatalf("new limits not saved: %+v", kept)
	}
}

func TestTombstoneCleanupFailureKeepsRequestError(t *testing.T) {
	srv := apitest.NewServer(t)
	dir := connectorStateTestDir(t)
	store, err := connectorstate.OpenShareRules(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(t.Context(), srv.Key.CRID, &accessrules.LinkLimits{ExpiresIn: "1h"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "share_rules.json")
	srv.Script(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/qurls", func(w http.ResponseWriter, r *http.Request) {
		if err := os.WriteFile(path, []byte("damaged"), 0o600); err != nil {
			t.Error(err)
		}
		apitest.HandlerTombstoned410(t)(w, r)
	})
	result := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "share", srv.Key.CRID}, shareStateDir: dir})
	if result.code != 5 || !strings.Contains(result.stderr.String(), "could not be removed") {
		t.Fatalf("cleanup obscured tombstone: %d %s", result.code, result.stderr.String())
	}
	raw, err := os.ReadFile(path) //nolint:gosec // Test-owned profile fixture.
	if err != nil || string(raw) != "damaged" {
		t.Fatalf("invalid state overwritten: %q %v", raw, err)
	}
}
