package state

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

func openOwnedLocalShareRegistry(dir string) (*LocalShareRegistry, error) {
	registry, err := OpenLocalShareRegistry(dir)
	if err != nil {
		return nil, err
	}
	if err := registry.BindOwner(context.Background(), "owner-test"); err != nil {
		return nil, err
	}
	return registry, nil
}

func TestLocalShareRegistryBindsOwnerBeforeShares(t *testing.T) {
	dir := secureStateTestDir(t)
	registry, err := OpenLocalShareRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	owner, present, err := registry.OwnerID(context.Background())
	if err != nil || present || owner != "" {
		t.Fatalf("empty owner = %q, %v, %v", owner, present, err)
	}
	binding := testResourceBinding(t, "owner-binding")
	binding.CRID = testBindingCRID(t, &binding, apitest.VersionTest)
	share := LocalShare{
		CRID: binding.CRID, ResourceID: binding.ResourceID,
		ConnectorID: binding.ConnectorID, ConnectorRoutingID: binding.ConnectorRoutingID,
		KnockResourceID: binding.KnockResourceID,
		TargetURL:       "http://127.0.0.1:3000", LocalIP: "127.0.0.1", LocalPort: 3000,
		DesiredState: "on", ServingEpoch: 1,
	}
	if err := registry.Put(context.Background(), &share); err == nil {
		t.Fatal("share was stored before its account owner was bound")
	}
	if err := registry.BindOwner(context.Background(), "owner-one"); err != nil {
		t.Fatal(err)
	}
	beforeIdempotentBind, err := os.Stat(filepath.Join(dir, LocalSharesFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.BindOwner(context.Background(), "owner-one"); err != nil {
		t.Fatalf("idempotent owner binding: %v", err)
	}
	afterIdempotentBind, err := os.Stat(filepath.Join(dir, LocalSharesFile))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(beforeIdempotentBind, afterIdempotentBind) {
		t.Fatal("idempotent owner binding replaced the durable registry file")
	}
	if err := registry.BindOwner(context.Background(), "owner-two"); err == nil {
		t.Fatal("account owner drift was accepted")
	}
	if err := registry.Put(context.Background(), &share); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenLocalShareRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	owner, present, err = reopened.OwnerID(context.Background())
	if err != nil || !present || owner != "owner-one" {
		t.Fatalf("durable owner = %q, %v, %v", owner, present, err)
	}
}

func TestLocalShareRegistryRejectsOldVersionWithSafeRecovery(t *testing.T) {
	dir := secureStateTestDir(t)
	path := filepath.Join(dir, LocalSharesFile)
	if err := os.WriteFile(path, []byte(`{"version":1,"owner_id":"owner-old","shares":{}}`), connectorResourceFileMode); err != nil {
		t.Fatal(err)
	}
	secureConnectorStateFixtureFile(t, path)
	_, _, err := ReadLocalSharesIfPresent(context.Background(), dir)
	if !errors.Is(err, ErrLocalShareVersionUnsupported) {
		t.Fatalf("old registry error = %v, want version sentinel", err)
	}
	for _, want := range []string{strconv.Quote(path), "does not migrate old state", "revoke any device key", "move or remove the complete state directory", "qurl login"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("old registry error = %q, want %q", err, want)
		}
	}
}

func TestLocalShareRegistryJourney(t *testing.T) {
	dir := secureStateTestDir(t)
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- owner-only directory mode, not a file mode.
		t.Fatal(err)
	}
	registry, err := openOwnedLocalShareRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	binding := testResourceBinding(t, "local-app")
	binding.CRID = testBindingCRID(t, &binding, apitest.VersionTest)
	share := LocalShare{
		CRID: binding.CRID, ResourceID: binding.ResourceID,
		ConnectorID: binding.ConnectorID, ConnectorRoutingID: binding.ConnectorRoutingID,
		KnockResourceID: binding.KnockResourceID,
		TargetURL:       "http://127.0.0.1:3000", LocalIP: "127.0.0.1", LocalPort: 3000,
		DesiredState: "off", ServingEpoch: 1,
	}
	if err := registry.Put(context.Background(), &share); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, LocalSharesFile)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || (!isWindows(t) && info.Mode().Perm() != 0o600) {
		t.Fatalf("registry mode = %v", info.Mode())
	}
	updated, err := registry.SetDesired(context.Background(), binding.CRID, "on", 2)
	if err != nil {
		t.Fatal(err)
	}
	if updated.DesiredState != "on" || updated.ServingEpoch != 2 {
		t.Fatalf("updated = %+v", updated)
	}
	beforeIdempotentSet, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	updated, err = registry.SetDesired(context.Background(), binding.CRID, "on", 2)
	if err != nil || updated.DesiredState != "on" || updated.ServingEpoch != 2 {
		t.Fatalf("idempotent desired-state update = %+v, %v", updated, err)
	}
	afterIdempotentSet, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(beforeIdempotentSet, afterIdempotentSet) {
		t.Fatal("idempotent desired-state update replaced the durable registry file")
	}
	got, err := registry.Get(context.Background(), binding.ConnectorID)
	if err != nil || got.ResourceID != binding.ResourceID || got.TargetURL != share.TargetURL {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	items, err := registry.List(context.Background())
	if err != nil || len(items) != 1 {
		t.Fatalf("List = %+v, %v", items, err)
	}
	if err := registry.Delete(context.Background(), binding.ResourceID); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Get(context.Background(), binding.CRID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Get after delete = %v", err)
	}
	beforeAbsentDelete, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Delete(context.Background(), binding.ResourceID); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
	afterAbsentDelete, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(beforeAbsentDelete, afterAbsentDelete) {
		t.Fatal("absent delete replaced the durable registry file")
	}
}

func TestLocalShareRegistryRejectsStaleEpochAndUnsafeTarget(t *testing.T) {
	dir := secureStateTestDir(t)
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- owner-only directory mode, not a file mode.
		t.Fatal(err)
	}
	registry, err := openOwnedLocalShareRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	binding := testResourceBinding(t, "safe-app")
	binding.CRID = testBindingCRID(t, &binding, apitest.VersionTest)
	share := LocalShare{
		CRID: binding.CRID, ResourceID: binding.ResourceID,
		ConnectorID: binding.ConnectorID, ConnectorRoutingID: binding.ConnectorRoutingID,
		KnockResourceID: binding.KnockResourceID,
		TargetURL:       "http://127.0.0.1:3000", LocalIP: "127.0.0.1", LocalPort: 3000,
		DesiredState: "on", ServingEpoch: 7,
	}
	if err := registry.Put(context.Background(), &share); err != nil {
		t.Fatal(err)
	}
	if updated, err := registry.SetDesired(context.Background(), share.CRID, "off", 6); err == nil || updated != nil {
		t.Fatalf("stale epoch result = %+v, %v; want nil row and an error", updated, err)
	}
	share.TargetURL = "http://192.0.2.1:3000"
	share.LocalIP = "192.0.2.1"
	if err := registry.Put(context.Background(), &share); err == nil {
		t.Fatal("non-loopback target was accepted")
	}
}

func TestLocalShareRegistryDisableAtCurrentEpochIsFailClosedAndExact(t *testing.T) {
	dir := secureStateTestDir(t)
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- owner-only directory mode, not a file mode.
		t.Fatal(err)
	}
	registry, err := openOwnedLocalShareRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	binding := testResourceBinding(t, "terminal-app")
	binding.CRID = testBindingCRID(t, &binding, apitest.VersionTest)
	share := LocalShare{
		CRID: binding.CRID, ResourceID: binding.ResourceID,
		ConnectorID: binding.ConnectorID, ConnectorRoutingID: binding.ConnectorRoutingID,
		KnockResourceID: binding.KnockResourceID,
		TargetURL:       "http://127.0.0.1:3000", LocalIP: "127.0.0.1", LocalPort: 3000,
		DesiredState: "on", ServingEpoch: 7,
	}
	if err := registry.Put(context.Background(), &share); err != nil {
		t.Fatal(err)
	}
	if disabled, err := registry.DisableAtCurrentEpoch(context.Background(), share.ResourceID, 6); err == nil || disabled != nil {
		t.Fatalf("older local disable result = %+v, %v; want nil row and an error", disabled, err)
	}
	if disabled, err := registry.DisableAtCurrentEpoch(context.Background(), share.ResourceID, 8); err == nil || disabled != nil {
		t.Fatalf("newer local disable result = %+v, %v; want nil row and an error", disabled, err)
	}
	disabled, err := registry.DisableAtCurrentEpoch(context.Background(), share.ResourceID, 7)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.DesiredState != "off" || disabled.ServingEpoch != 7 || disabled.TargetURL != share.TargetURL || disabled.ConnectorRoutingID != share.ConnectorRoutingID {
		t.Fatalf("local disable changed more than local intent: %+v", disabled)
	}
	transitionTime := disabled.UpdatedAt
	disabledAgain, err := registry.DisableAtCurrentEpoch(context.Background(), share.CRID, 7)
	if err != nil {
		t.Fatalf("idempotent local disable: %v", err)
	}
	if !disabledAgain.UpdatedAt.Equal(transitionTime) {
		t.Fatalf("idempotent local disable changed transition time from %s to %s", transitionTime, disabledAgain.UpdatedAt)
	}
	storedAgain, err := registry.Get(context.Background(), share.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	if !storedAgain.UpdatedAt.Equal(transitionTime) {
		t.Fatalf("idempotent local disable persisted transition time %s, want %s", storedAgain.UpdatedAt, transitionTime)
	}
	if enabled, err := registry.EnableAtCurrentEpoch(context.Background(), share.ResourceID, 6); err == nil || enabled != nil {
		t.Fatalf("older local enable result = %+v, %v; want nil row and an error", enabled, err)
	}
	if enabled, err := registry.EnableAtCurrentEpoch(context.Background(), share.ResourceID, 8); err == nil || enabled != nil {
		t.Fatalf("newer local enable result = %+v, %v; want nil row and an error", enabled, err)
	}
	enabled, err := registry.EnableAtCurrentEpoch(context.Background(), share.ResourceID, 7)
	if err != nil {
		t.Fatal(err)
	}
	if enabled.DesiredState != "on" || enabled.ServingEpoch != 7 || enabled.TargetURL != share.TargetURL || enabled.ConnectorRoutingID != share.ConnectorRoutingID {
		t.Fatalf("local enable changed more than local intent: %+v", enabled)
	}
}

func TestLocalShareRegistryRejectsOutOfOrderAndIdentityChanges(t *testing.T) {
	dir := secureStateTestDir(t)
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- owner-only directory mode, not a file mode.
		t.Fatal(err)
	}
	registry, err := openOwnedLocalShareRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	binding := testResourceBinding(t, "ordered-app")
	binding.CRID = testBindingCRID(t, &binding, apitest.VersionTest)
	base := LocalShare{
		CRID: binding.CRID, ResourceID: binding.ResourceID,
		ConnectorID: binding.ConnectorID, ConnectorRoutingID: binding.ConnectorRoutingID,
		KnockResourceID: binding.KnockResourceID,
		TargetURL:       "http://127.0.0.1:3000", LocalIP: "127.0.0.1", LocalPort: 3000,
		DesiredState: "on", ServingEpoch: 9,
	}
	if err := registry.Put(context.Background(), &base); err != nil {
		t.Fatal(err)
	}

	stale := base
	stale.ServingEpoch = 8
	stale.DesiredState = "off"
	if err := registry.Put(context.Background(), &stale); err == nil {
		t.Fatal("out-of-order lifecycle response regressed the registry")
	}
	contradiction := base
	contradiction.DesiredState = "off"
	if err := registry.Put(context.Background(), &contradiction); err == nil {
		t.Fatal("same-epoch desired-state contradiction was accepted")
	}
	changedIdentity := base
	changedIdentity.ConnectorRoutingID = testResourceBinding(t, "other-app").ConnectorRoutingID
	if err := registry.Put(context.Background(), &changedIdentity); err == nil {
		t.Fatal("immutable routing identity change was accepted")
	}
	sameEpochTarget := base
	sameEpochTarget.TargetURL = "http://127.0.0.1:4000"
	sameEpochTarget.LocalPort = 4000
	if err := registry.Put(context.Background(), &sameEpochTarget); err == nil {
		t.Fatal("same-epoch local target change was accepted")
	}

	newer := base
	newer.ServingEpoch = 10
	newer.TargetURL = "http://127.0.0.1:4000"
	newer.LocalPort = 4000
	if err := registry.Put(context.Background(), &newer); err != nil {
		t.Fatalf("newer lifecycle and target update: %v", err)
	}
	got, err := registry.Get(context.Background(), base.ResourceID)
	if err != nil || got.ServingEpoch != 10 || got.LocalPort != 4000 {
		t.Fatalf("registry after newer update = %+v, %v", got, err)
	}
}

func TestLocalShareRegistryConcurrentResponsesConvergeToHighestEpoch(t *testing.T) {
	dir := secureStateTestDir(t)
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- owner-only directory mode, not a file mode.
		t.Fatal(err)
	}
	registry, err := openOwnedLocalShareRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	binding := testResourceBinding(t, "concurrent-app")
	binding.CRID = testBindingCRID(t, &binding, apitest.VersionTest)
	base := LocalShare{
		CRID: binding.CRID, ResourceID: binding.ResourceID,
		ConnectorID: binding.ConnectorID, ConnectorRoutingID: binding.ConnectorRoutingID,
		KnockResourceID: binding.KnockResourceID,
		TargetURL:       "http://127.0.0.1:3000", LocalIP: "127.0.0.1", LocalPort: 3000,
		DesiredState: "off", ServingEpoch: 1,
	}
	if err := registry.Put(context.Background(), &base); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for epoch := uint64(2); epoch <= 20; epoch++ {
		share := base
		share.ServingEpoch = epoch
		share.DesiredState = "on"
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = registry.Put(context.Background(), &share)
		}()
	}
	close(start)
	wg.Wait()
	got, err := registry.Get(context.Background(), base.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ServingEpoch != 20 || got.DesiredState != "on" {
		t.Fatalf("concurrent final state = %+v, want epoch 20 on", got)
	}
}

func TestDecodeLocalSharesRequiresUpdatedAt(t *testing.T) {
	binding := testResourceBinding(t, "missing-time")
	binding.CRID = testBindingCRID(t, &binding, apitest.VersionTest)
	share := LocalShare{
		CRID: binding.CRID, ResourceID: binding.ResourceID,
		ConnectorID: binding.ConnectorID, ConnectorRoutingID: binding.ConnectorRoutingID,
		KnockResourceID: binding.KnockResourceID,
		TargetURL:       "http://127.0.0.1:3000", LocalIP: "127.0.0.1", LocalPort: 3000,
		DesiredState: "on", ServingEpoch: 1,
	}
	raw, err := json.Marshal(localSharesState{Version: localSharesVersion, Shares: map[string]LocalShare{share.ResourceID: share}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeLocalShares(raw); err == nil {
		t.Fatal("registry row with zero updated_at was accepted")
	}
}

func TestLocalShareRegistryRefusesSymlink(t *testing.T) {
	dir := secureStateTestDir(t)
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- owner-only directory mode, not a file mode.
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte(`{"version":1,"shares":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, LocalSharesFile)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	registry, err := OpenLocalShareRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.List(context.Background()); err == nil {
		t.Fatal("symlink registry was accepted")
	}
}

// TestLocalSharesRegistryRoundTripsMaxRowsUnderByteCap fills the registry to
// its item cap with maximal rows (a 64-char Connector ID, a 64-char knock
// resource, and a verbose full-form IPv6 loopback target alongside the real
// base64url resource identity and CRID) and proves the whole file round-trips
// well under the byte cap with room to spare.
func TestLocalSharesRegistryRoundTripsMaxRowsUnderByteCap(t *testing.T) {
	dir := secureStateTestDir(t)
	now := time.Now().UTC()
	state := localSharesState{
		Version: localSharesVersion,
		OwnerID: "own_" + strings.Repeat("x", 40),
		Shares:  make(map[string]LocalShare, localSharesMaxItems),
	}
	const maxTarget = "http://[0000:0000:0000:0000:0000:0000:0000:0001]:65535"
	for i := 0; i < localSharesMaxItems; i++ {
		binding := testResourceBinding(t, fmt.Sprintf("c%063d", i))
		share := LocalShare{
			CRID: binding.CRID, ResourceID: binding.ResourceID, ConnectorID: binding.ConnectorID,
			ConnectorRoutingID: binding.ConnectorRoutingID, KnockResourceID: strings.Repeat("k", 64),
			TargetURL: maxTarget, LocalIP: "0000:0000:0000:0000:0000:0000:0000:0001", LocalPort: 65535,
			DesiredState: "on", ServingEpoch: ^uint64(0), UpdatedAt: now,
		}
		state.Shares[share.ResourceID] = share
	}
	if err := writeLocalShares(dir, state); err != nil {
		t.Fatalf("write %d-row registry: %v", localSharesMaxItems, err)
	}
	info, err := os.Stat(filepath.Join(dir, LocalSharesFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d-row registry file size: %d bytes (cap %d)", localSharesMaxItems, info.Size(), localSharesMaxBytes)
	if info.Size() >= localSharesMaxBytes {
		t.Fatalf("registry file size %d is not under the %d-byte cap", info.Size(), localSharesMaxBytes)
	}
	// A generous margin: a full registry must not sit within a hair of the cap.
	if info.Size() > localSharesMaxBytes/2 {
		t.Fatalf("registry file size %d leaves less than 2x headroom under the %d-byte cap", info.Size(), localSharesMaxBytes)
	}
	loaded, err := loadLocalShares(dir)
	if err != nil {
		t.Fatalf("reload %d-row registry: %v", localSharesMaxItems, err)
	}
	if len(loaded.Shares) != localSharesMaxItems {
		t.Fatalf("reloaded %d rows, want %d", len(loaded.Shares), localSharesMaxItems)
	}
}

func TestLocalShareRegistryRetargetRequiresNewerEpochAndLoopbackTarget(t *testing.T) {
	dir := secureStateTestDir(t)
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- owner-only directory mode, not a file mode.
		t.Fatal(err)
	}
	registry, err := openOwnedLocalShareRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	binding := testResourceBinding(t, "moving-app")
	binding.CRID = testBindingCRID(t, &binding, apitest.VersionTest)
	share := LocalShare{
		CRID: binding.CRID, ResourceID: binding.ResourceID,
		ConnectorID: binding.ConnectorID, ConnectorRoutingID: binding.ConnectorRoutingID,
		KnockResourceID: binding.KnockResourceID,
		TargetURL:       "http://127.0.0.1:3000", LocalIP: "127.0.0.1", LocalPort: 3000,
		DesiredState: "on", ServingEpoch: 4,
	}
	if err := registry.Put(context.Background(), &share); err != nil {
		t.Fatal(err)
	}
	stored, err := registry.Get(context.Background(), share.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	if moved, err := registry.Retarget(context.Background(), share.ResourceID, LocalTarget{URL: "http://127.0.0.1:4000", IP: "127.0.0.1", Port: 4000}, 4); err == nil || moved != nil || !strings.Contains(err.Error(), "newer serving epoch") {
		t.Fatalf("same-epoch Retarget = %+v, %v; want nil row and a newer-epoch refusal", moved, err)
	}
	// Targets a caller outside cmd could hand in. The IP and port are the
	// caller's claim; validateLocalShare cross-checks them against the URL, so
	// a mismatch is refused even when each field is individually plausible.
	for name, test := range map[string]struct {
		target LocalTarget
		epoch  uint64
	}{
		"older epoch":   {target: LocalTarget{URL: "http://127.0.0.1:4000", IP: "127.0.0.1", Port: 4000}, epoch: 3},
		"remote host":   {target: LocalTarget{URL: "http://192.0.2.1:4000", IP: "192.0.2.1", Port: 4000}, epoch: 5},
		"https":         {target: LocalTarget{URL: "https://127.0.0.1:4000", IP: "127.0.0.1", Port: 4000}, epoch: 5},
		"path":          {target: LocalTarget{URL: "http://127.0.0.1:4000/app", IP: "127.0.0.1", Port: 4000}, epoch: 5},
		"credentials":   {target: LocalTarget{URL: "http://me:secret@127.0.0.1:4000", IP: "127.0.0.1", Port: 4000}, epoch: 5},
		"hostname":      {target: LocalTarget{URL: "http://localhost:4000", IP: "127.0.0.1", Port: 4000}, epoch: 5},
		"missing port":  {target: LocalTarget{URL: "http://127.0.0.1", IP: "127.0.0.1", Port: 4000}, epoch: 5},
		"not a url":     {target: LocalTarget{URL: "::not-a-url", IP: "127.0.0.1", Port: 4000}, epoch: 5},
		"port mismatch": {target: LocalTarget{URL: "http://127.0.0.1:4000", IP: "127.0.0.1", Port: 5000}, epoch: 5},
		"ip mismatch":   {target: LocalTarget{URL: "http://127.0.0.1:4000", IP: "127.0.0.2", Port: 4000}, epoch: 5},
	} {
		if moved, err := registry.Retarget(context.Background(), share.ResourceID, test.target, test.epoch); err == nil || moved != nil {
			t.Fatalf("%s: Retarget = %+v, %v; want nil row and an error", name, moved, err)
		}
	}
	if _, err := registry.Retarget(context.Background(), "missing", LocalTarget{URL: "http://127.0.0.1:4000", IP: "127.0.0.1", Port: 4000}, 5); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Retarget of an unknown share = %v, want os.ErrNotExist", err)
	}
	unchanged, err := registry.Get(context.Background(), share.ResourceID)
	if err != nil || unchanged.TargetURL != stored.TargetURL || unchanged.LocalPort != stored.LocalPort ||
		unchanged.ServingEpoch != stored.ServingEpoch || !unchanged.UpdatedAt.Equal(stored.UpdatedAt) {
		t.Fatalf("refused Retarget changed the row: %+v -> %+v, %v", stored, unchanged, err)
	}

	moved, err := registry.Retarget(context.Background(), share.CRID, LocalTarget{URL: "http://[::1]:4000", IP: "::1", Port: 4000}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if moved.TargetURL != "http://[::1]:4000" || moved.LocalIP != "::1" || moved.LocalPort != 4000 ||
		moved.DesiredState != "on" || moved.ServingEpoch != 5 || moved.UpdatedAt.Before(stored.UpdatedAt) ||
		moved.CRID != share.CRID || moved.ConnectorID != share.ConnectorID ||
		moved.ConnectorRoutingID != share.ConnectorRoutingID || moved.KnockResourceID != share.KnockResourceID {
		t.Fatalf("moved row = %+v", moved)
	}
	durable, err := registry.Get(context.Background(), share.ResourceID)
	if err != nil || durable.TargetURL != moved.TargetURL || durable.ServingEpoch != 5 || !durable.UpdatedAt.Equal(moved.UpdatedAt) {
		t.Fatalf("durable moved row = %+v, %v", durable, err)
	}
	// A restart is authoritative desired-on: moving a locally fail-closed
	// share at the newer epoch turns it back on with the new target.
	if _, err := registry.DisableAtCurrentEpoch(context.Background(), share.ResourceID, 5); err != nil {
		t.Fatal(err)
	}
	reenabled, err := registry.Retarget(context.Background(), share.ResourceID, LocalTarget{URL: "http://127.0.0.1:5000", IP: "127.0.0.1", Port: 5000}, 6)
	if err != nil || reenabled.DesiredState != "on" || reenabled.ServingEpoch != 6 || reenabled.LocalPort != 5000 {
		t.Fatalf("Retarget after local disable = %+v, %v", reenabled, err)
	}
}

func TestStoppedFileTargetConversionPreservesEveryRegistryRow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix origins are unsupported on Windows")
	}
	ctx := context.Background()
	dir := secureStateTestDir(t)
	if err := EstablishExternalRuntimeMode(ctx, dir); err != nil {
		t.Fatal(err)
	}
	registry, err := openOwnedLocalShareRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A freshly enrolled owner-bound profile legitimately has no file rows.
	if changed, err := registry.RetargetStoppedToUnix(ctx, "owner-test", "qurl-file-", "http+unix:///tmp/private.sock"); err != nil || changed != 0 {
		t.Fatalf("empty file set conversion = %d, %v", changed, err)
	}
	for index, id := range []string{"qurl-file-first", "qurl-file-orphan", "local-app"} {
		binding := testResourceBinding(t, id)
		binding.CRID = testBindingCRID(t, &binding, apitest.VersionTest)
		desired := "on"
		if index == 1 {
			desired = "off"
		}
		row := LocalShare{CRID: binding.CRID, ResourceID: binding.ResourceID, ConnectorID: binding.ConnectorID, ConnectorRoutingID: binding.ConnectorRoutingID, KnockResourceID: binding.KnockResourceID, TargetURL: "http://127.0.0.1:3000", LocalIP: "127.0.0.1", LocalPort: 3000, DesiredState: desired, ServingEpoch: 7}
		if err := registry.Put(ctx, &row); err != nil {
			t.Fatal(err)
		}
	}
	before, err := registry.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	converter, ok := any(registry).(interface {
		RetargetStoppedToUnix(context.Context, string, string, string) (int, error)
	})
	if !ok {
		t.Fatal("registry cannot atomically convert stopped Unix targets")
	}
	unlock, err := AcquireDaemonLease(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := converter.RetargetStoppedToUnix(ctx, "owner-test", "qurl-file-", "http+unix:///tmp/private.sock"); err == nil {
		t.Fatal("conversion bypassed a live daemon lease")
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	changed, err := converter.RetargetStoppedToUnix(ctx, "owner-test", "qurl-file-", "http+unix:///tmp/private.sock")
	if err != nil || changed != 2 {
		t.Fatalf("conversion = %d, %v", changed, err)
	}
	after, err := registry.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(before, func(a, b LocalShare) int { return cmp.Compare(a.ResourceID, b.ResourceID) })
	slices.SortFunc(after, func(a, b LocalShare) int { return cmp.Compare(a.ResourceID, b.ResourceID) })
	for i := range after {
		expected := before[i]
		if strings.HasPrefix(expected.ConnectorID, "qurl-file-") {
			expected.TargetURL, expected.LocalIP, expected.LocalPort = "http+unix:///tmp/private.sock", "", 0
			expected.LocalSocketPath = "/tmp/private.sock"
			expected.UpdatedAt = after[i].UpdatedAt
		}
		if !reflect.DeepEqual(expected, after[i]) {
			t.Fatal("conversion changed identity, preference, epoch or a sibling app")
		}
	}
	changed, err = converter.RetargetStoppedToUnix(ctx, "owner-test", "qurl-file-", "http+unix:///tmp/private.sock")
	if err != nil || changed != 0 {
		t.Fatalf("idempotent conversion = %d, %v", changed, err)
	}
	snapshot, err := os.ReadFile(filepath.Join(dir, LocalSharesFile)) // #nosec G304 -- private test registry fixture.
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][3]string{{"other-owner", "qurl-file-", "http+unix:///tmp/new.sock"}, {"owner-test", "", "http+unix:///tmp/new.sock"}, {"owner-test", "qurl-file-", "http://127.0.0.1:4000"}} {
		if _, err := converter.RetargetStoppedToUnix(ctx, args[0], args[1], args[2]); err == nil {
			t.Fatal("invalid conversion was accepted")
		}
		current, err := os.ReadFile(filepath.Join(dir, LocalSharesFile)) // #nosec G304 -- private test registry fixture.
		if err != nil || !bytes.Equal(current, snapshot) {
			t.Fatal("failed conversion mutated registry")
		}
	}
}

func TestRetargetStoppedCrashBoundaries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix origins are unsupported on Windows")
	}
	for _, beforeCommit := range []bool{true, false} {
		t.Run(fmt.Sprintf("before_commit_%t", beforeCommit), func(t *testing.T) {
			ctx := context.Background()
			dir := secureStateTestDir(t)
			if err := EstablishExternalRuntimeMode(ctx, dir); err != nil {
				t.Fatal(err)
			}
			registry, err := openOwnedLocalShareRegistry(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"qurl-file-first", "qurl-file-orphan"} {
				binding := testResourceBinding(t, id)
				binding.CRID = testBindingCRID(t, &binding, apitest.VersionTest)
				row := LocalShare{CRID: binding.CRID, ResourceID: binding.ResourceID, ConnectorID: binding.ConnectorID, ConnectorRoutingID: binding.ConnectorRoutingID, KnockResourceID: binding.KnockResourceID, TargetURL: "http://127.0.0.1:3000", LocalIP: "127.0.0.1", LocalPort: 3000, DesiredState: "on", ServingEpoch: 7}
				if err := registry.Put(ctx, &row); err != nil {
					t.Fatal(err)
				}
			}
			before, err := registry.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var release func() error
			if beforeCommit {
				release, err = acquireConnectorResourcesLock(ctx, dir)
				if err != nil {
					t.Fatal(err)
				}
			}
			childCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			testBinary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			child := exec.CommandContext(childCtx, testBinary, "-test.run=^TestRetargetStoppedCrashHelper$") // #nosec G204 -- rerun this test binary for the crash-boundary fixture.
			child.Env = append(os.Environ(), "QURL_TEST_RETARGET_STATE="+dir)
			stdout, err := child.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = child.Process.Kill() })
			if beforeCommit {
				deadline := time.Now().Add(5 * time.Second)
				owned := false
				for time.Now().Before(deadline) {
					unlock, err := AcquireDaemonLease(ctx, dir)
					if err != nil {
						owned = true
						break
					}
					if err := unlock(); err != nil {
						t.Fatal(err)
					}
					time.Sleep(10 * time.Millisecond)
				}
				if !owned {
					t.Fatal("child never acquired the stopped-daemon lease")
				}
			} else {
				line, err := bufio.NewReader(stdout).ReadString('\n')
				if err != nil || line != "converted\n" {
					t.Fatalf("child did not commit: %q %v", line, err)
				}
			}
			if err := child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := child.Wait(); err == nil {
				t.Fatal("child unexpectedly exited without being killed")
			}
			if release != nil {
				if err := release(); err != nil {
					t.Fatal(err)
				}
			}
			after, err := registry.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			slices.SortFunc(before, func(a, b LocalShare) int { return cmp.Compare(a.ResourceID, b.ResourceID) })
			slices.SortFunc(after, func(a, b LocalShare) int { return cmp.Compare(a.ResourceID, b.ResourceID) })
			for i := range after {
				expected := before[i]
				if !beforeCommit {
					expected.TargetURL, expected.LocalIP, expected.LocalPort, expected.LocalSocketPath = "http+unix:///tmp/private.sock", "", 0, "/tmp/private.sock"
					expected.UpdatedAt = after[i].UpdatedAt
				}
				if !reflect.DeepEqual(after[i], expected) {
					t.Fatal("process death left a partial conversion or changed resource identity")
				}
			}
			if _, err := registry.RetargetStoppedToUnix(ctx, "owner-test", "qurl-file-", "http+unix:///tmp/private.sock"); err != nil {
				t.Fatalf("restart could not resume: %v", err)
			}
		})
	}
}

func TestRetargetStoppedCrashHelper(t *testing.T) {
	dir := os.Getenv("QURL_TEST_RETARGET_STATE")
	if dir == "" {
		return
	}
	registry, err := OpenLocalShareRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.RetargetStoppedToUnix(context.Background(), "owner-test", "qurl-file-", "http+unix:///tmp/private.sock"); err != nil {
		t.Fatal(err)
	}
	fmt.Println("converted")
	<-time.After(time.Hour)
}
