package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/accessrules"
)

const ruleID = "11111111-1111-4111-8111-111111111111"

func TestAccessRulesPersistWithoutDeviceIdentity(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenAccessRules(dir, "https://api.example.test")
	if err != nil {
		t.Fatal(err)
	}
	initial, err := store.Snapshot(t.Context())
	if err != nil || initial.LocalVersion != 0 || len(initial.BuiltIns) != 5 {
		t.Fatalf("initial %+v: %v", initial, err)
	}
	saved, err := store.Save(t.Context(), 0, &accessrules.Definition{ID: ruleID, Name: "Team", Mode: "keys"})
	if err != nil {
		t.Fatal(err)
	}
	other, _ := OpenAccessRules(dir, "https://api.example.test")
	got, err := other.Snapshot(t.Context())
	if err != nil || len(got.Items) != 1 || got.LocalVersion != saved.LocalVersion {
		t.Fatalf("restart %+v: %v", got, err)
	}
	if _, err = other.Delete(t.Context(), 0, ruleID); !errors.Is(err, accessrules.ErrConflict) {
		t.Fatalf("stale delete: %v", err)
	}
	if EnvelopePresent(dir) {
		t.Fatal("library enrolled a device")
	}
}

func TestAccessRulesConcurrentProcesses(t *testing.T) {
	if os.Getenv("QURL_RULE_TEST_CHILD") == "yes" {
		dir := os.Getenv("QURL_RULE_TEST_DIR")
		id := os.Getenv("QURL_RULE_TEST_ID")
		store, err := OpenAccessRules(dir, "https://api.example.test")
		if err != nil {
			t.Fatal(err)
		}
		for range 100 {
			snapshot, e := store.Snapshot(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			_, e = store.Save(context.Background(), snapshot.LocalVersion, &accessrules.Definition{ID: id, Name: "Process", Mode: "keys"})
			if e == nil {
				return
			}
			if !errors.Is(e, accessrules.ErrConflict) {
				t.Fatal(e)
			}
		}
		t.Fatal("concurrent save never succeeded")
	}
	dir := t.TempDir()
	commands := make([]*exec.Cmd, 6)
	for i := range commands {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAccessRulesConcurrentProcesses$") //nolint:gosec // G204: re-executes this test binary with a fixed test selector.
		cmd.Env = append(os.Environ(), "QURL_RULE_TEST_CHILD=yes", "QURL_RULE_TEST_DIR="+dir, "QURL_RULE_TEST_ID="+fmt.Sprintf("%08d-1111-4111-8111-111111111111", i))
		commands[i] = cmd
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
	}
	for _, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	store, _ := OpenAccessRules(dir, "https://api.example.test")
	snapshot, err := store.Snapshot(t.Context())
	if err != nil || len(snapshot.Items) != len(commands) {
		t.Fatalf("lost concurrent updates: %+v %v", snapshot, err)
	}
}

func TestAccessRulesRejectUnsafeJournal(t *testing.T) {
	for _, kind := range []string{"symlink", "malformed", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "access_rules.json")
			if kind == "symlink" {
				target := filepath.Join(t.TempDir(), "target")
				if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Skip(err)
				}
			} else {
				raw := `{"schema_version":77}`
				if kind == "malformed" {
					raw = `not JSON`
				}
				if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			store, _ := OpenAccessRules(dir, "https://api.example.test")
			if _, err := store.Snapshot(t.Context()); err == nil {
				t.Fatal("unsafe journal accepted")
			}
		})
	}
}

type ruleRemote struct {
	library  accessrules.AccountLibrary
	putCount int
	fail     error
	race     func()
	onPut    func()
}

func (r *ruleRemote) Load(context.Context) (accessrules.AccountLibrary, error) {
	return r.library, r.fail
}
func (r *ruleRemote) Replace(_ context.Context, v int64, items []accessrules.Definition) (accessrules.AccountLibrary, error) {
	r.putCount++
	if r.race != nil {
		f := r.race
		r.race = nil
		f()
		return accessrules.AccountLibrary{}, accessrules.ErrConflict
	}
	if v != r.library.Version {
		return accessrules.AccountLibrary{}, accessrules.ErrConflict
	}
	r.library.Version++
	r.library.Items = items
	if r.onPut != nil {
		f := r.onPut
		r.onPut = nil
		f()
	}
	return r.library, nil
}
func TestAccessRulesSyncMergeConflictAndAccountIsolation(t *testing.T) {
	store, _ := OpenAccessRules(t.TempDir(), "https://api.example.test")
	local := accessrules.Definition{ID: ruleID, Name: "Local", Mode: "keys"}
	if _, err := store.Save(t.Context(), 0, &local); err != nil {
		t.Fatal(err)
	}
	remote := &ruleRemote{library: accessrules.AccountLibrary{AccountID: "account-one", Items: []accessrules.Definition{{ID: "22222222-2222-4222-8222-222222222222", Name: "Remote", Mode: "review"}}, BuiltIns: accessrules.BuiltIns()}}
	got, err := store.Sync(t.Context(), remote)
	if err != nil || got.Sync != "synced" || len(got.Items) != 2 {
		t.Fatalf("first sync %+v %v", got, err)
	}
	local.Name = "Local edit"
	got, err = store.Save(t.Context(), got.LocalVersion, &local)
	if err != nil {
		t.Fatal(err)
	}
	remote.library.Items[slices.IndexFunc(remote.library.Items, func(d accessrules.Definition) bool { return d.ID == ruleID })].Name = "Remote edit"
	got, err = store.Sync(t.Context(), remote)
	if err != nil || got.Sync != "conflict" || len(got.Conflicts) != 1 {
		t.Fatalf("conflict %+v %v", got, err)
	}
	got, err = store.Resolve(t.Context(), got.LocalVersion, ruleID, "account")
	if err != nil || got.Items[slices.IndexFunc(got.Items, func(d accessrules.Definition) bool { return d.ID == ruleID })].Name != "Remote edit" {
		t.Fatalf("resolve %+v %v", got, err)
	}
	count := remote.putCount
	remote.library.AccountID = "account-two"
	if _, err = store.Sync(t.Context(), remote); err != nil || remote.putCount != count {
		t.Fatalf("wrong account upload %v", err)
	}
}
func TestAccessRulesSyncRefetchesAfterCASAndPreservesPendingOnFailure(t *testing.T) {
	store, _ := OpenAccessRules(t.TempDir(), "https://api.example.test")
	_, err := store.Save(t.Context(), 0, &accessrules.Definition{ID: ruleID, Name: "Local", Mode: "keys"})
	if err != nil {
		t.Fatal(err)
	}
	remote := &ruleRemote{library: accessrules.AccountLibrary{AccountID: "account-one", Items: []accessrules.Definition{}, BuiltIns: accessrules.BuiltIns()}}
	remote.race = func() {
		remote.library.Version++
		remote.library.Items = append(remote.library.Items, accessrules.Definition{ID: "22222222-2222-4222-8222-222222222222", Name: "Race", Mode: "keys"})
	}
	got, err := store.Sync(t.Context(), remote)
	if err != nil || got.Sync != "synced" || len(got.Items) != 2 || remote.putCount != 2 {
		t.Fatalf("CAS merge %+v %v puts %s", got, err, strconv.Itoa(remote.putCount))
	}
	_, err = store.Delete(t.Context(), got.LocalVersion, ruleID)
	if err != nil {
		t.Fatal(err)
	}
	remote.fail = errors.New("offline")
	if _, err = store.Sync(t.Context(), remote); err == nil {
		t.Fatal("failure swallowed")
	}
	got, err = store.Snapshot(t.Context())
	if err != nil || len(got.Items) != 1 || got.Sync != "offline" {
		t.Fatalf("pending lost %+v %v", got, err)
	}
}

func TestAccessRulesAccountSwitchArchivesPendingAndLoadsFreshLibrary(t *testing.T) {
	store, _ := OpenAccessRules(t.TempDir(), "https://api.example.test")
	local := accessrules.Definition{ID: ruleID, Name: "Account one", Mode: "keys"}
	_, err := store.Save(t.Context(), 0, &local)
	if err != nil {
		t.Fatal(err)
	}
	remote := &ruleRemote{library: accessrules.AccountLibrary{AccountID: "one", Items: []accessrules.Definition{}, BuiltIns: accessrules.BuiltIns()}}
	snapshot, err := store.Sync(t.Context(), remote)
	if err != nil {
		t.Fatal(err)
	}
	one := remote.library
	local.Name = "Pending account one"
	if _, err = store.Save(t.Context(), snapshot.LocalVersion, &local); err != nil {
		t.Fatal(err)
	}
	remote.library = accessrules.AccountLibrary{AccountID: "two", Items: []accessrules.Definition{}, BuiltIns: accessrules.BuiltIns()}
	count := remote.putCount
	snapshot, err = store.Sync(t.Context(), remote)
	if err != nil || len(snapshot.Items) != 0 || remote.putCount != count {
		t.Fatalf("prior account cache uploaded: %+v %v", snapshot, err)
	}
	remote.library = one
	snapshot, err = store.Sync(t.Context(), remote)
	if err != nil || len(snapshot.Items) != 1 || snapshot.Items[0].Name != "Pending account one" || snapshot.Sync != "synced" {
		t.Fatalf("archived pending edit lost: %+v %v", snapshot, err)
	}
}

func TestAccessRulesSyncKeepsLocalEditMadeDuringPUT(t *testing.T) {
	store, _ := OpenAccessRules(t.TempDir(), "https://api.example.test")
	definition := accessrules.Definition{ID: ruleID, Name: "Before PUT", Mode: "keys"}
	if _, err := store.Save(t.Context(), 0, &definition); err != nil {
		t.Fatal(err)
	}
	remote := &ruleRemote{library: accessrules.AccountLibrary{AccountID: "one", Items: []accessrules.Definition{}, BuiltIns: accessrules.BuiltIns()}}
	remote.onPut = func() {
		snapshot, err := store.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		definition.Name = "Edited during PUT"
		if _, err = store.Save(t.Context(), snapshot.LocalVersion, &definition); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := store.Sync(t.Context(), remote)
	if err != nil || snapshot.Items[0].Name != "Edited during PUT" || snapshot.Sync != "pending" {
		t.Fatalf("concurrent edit lost: %+v %v", snapshot, err)
	}
}

func TestAccessRulesUnchangedSyncDoesNotInvalidateLocalRevision(t *testing.T) {
	store, _ := OpenAccessRules(t.TempDir(), "https://api.example.test")
	remote := &ruleRemote{library: accessrules.AccountLibrary{AccountID: "one", Items: []accessrules.Definition{}, BuiltIns: accessrules.BuiltIns()}}
	first, err := store.Sync(t.Context(), remote)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Sync(t.Context(), remote)
	if err != nil || second.LocalVersion != first.LocalVersion {
		t.Fatalf("unchanged sync invalidates open form: %+v %+v %v", first, second, err)
	}
}
