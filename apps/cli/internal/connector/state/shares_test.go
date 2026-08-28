//go:build !windows

package state

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

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
	dir := t.TempDir()
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
	if err := registry.BindOwner(context.Background(), "owner-one"); err != nil {
		t.Fatalf("idempotent owner binding: %v", err)
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

func TestLocalShareRegistryJourney(t *testing.T) {
	dir := t.TempDir()
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
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("registry mode = %v", info.Mode())
	}
	updated, err := registry.SetDesired(context.Background(), binding.CRID, "on", 2)
	if err != nil {
		t.Fatal(err)
	}
	if updated.DesiredState != "on" || updated.ServingEpoch != 2 {
		t.Fatalf("updated = %+v", updated)
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
}

func TestLocalShareRegistryRejectsStaleEpochAndUnsafeTarget(t *testing.T) {
	dir := t.TempDir()
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
	if _, err := registry.SetDesired(context.Background(), share.CRID, "off", 6); err == nil {
		t.Fatal("stale epoch was accepted")
	}
	share.TargetURL = "http://192.0.2.1:3000"
	share.LocalIP = "192.0.2.1"
	if err := registry.Put(context.Background(), &share); err == nil {
		t.Fatal("non-loopback target was accepted")
	}
}

func TestLocalShareRegistryTerminalDisableIsFailClosedAndEpochExact(t *testing.T) {
	dir := t.TempDir()
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
	if _, err := registry.DisableTerminal(context.Background(), share.ResourceID, 6); err == nil {
		t.Fatal("terminal disable accepted an older session epoch")
	}
	if _, err := registry.DisableTerminal(context.Background(), share.ResourceID, 8); err == nil {
		t.Fatal("terminal disable advanced the authoritative epoch")
	}
	disabled, err := registry.DisableTerminal(context.Background(), share.ResourceID, 7)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.DesiredState != "off" || disabled.ServingEpoch != 7 || disabled.TargetURL != share.TargetURL || disabled.ConnectorRoutingID != share.ConnectorRoutingID {
		t.Fatalf("terminal disable changed more than local intent: %+v", disabled)
	}
	if _, err := registry.DisableTerminal(context.Background(), share.CRID, 7); err != nil {
		t.Fatalf("idempotent terminal disable: %v", err)
	}
}

func TestLocalShareRegistryRejectsOutOfOrderAndIdentityChanges(t *testing.T) {
	dir := t.TempDir()
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
	dir := t.TempDir()
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
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- owner-only directory mode, not a file mode.
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte(`{"version":1,"shares":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, LocalSharesFile)); err != nil {
		t.Fatal(err)
	}
	registry, err := OpenLocalShareRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.List(context.Background()); err == nil {
		t.Fatal("symlink registry was accepted")
	}
}
