package state

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func retireTestBinding(t *testing.T, store *Store, binding *ConnectorResourceBinding) {
	t.Helper()
	commitTestBinding(t, store, binding)
	if retired, err := store.RetireConnectorResource(t.Context(), binding.CRID); err != nil || !retired {
		t.Fatalf("retire test binding = %t, %v", retired, err)
	}
}

func TestPrepareConnectorResourceReusePersistsExactRequestAcrossRestart(t *testing.T) {
	store := openTestStore(t)
	old := testResourceBinding(t, "reused-api")
	retireTestBinding(t, store, &old)
	if err := store.PrepareConnectorResourceReuse(t.Context(), old.ConnectorID); err != nil {
		t.Fatal(err)
	}
	prepared, err := loadConnectorResources(store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	pending := prepared.Pending[old.ConnectorID]
	if len(prepared.Bindings) != 0 || len(prepared.Retired) != 0 || len(prepared.Pending) != 1 ||
		pending.ExpectedResourceID != "" || pending.RequestNonce == "" || pending.ConnectorID != old.ConnectorID {
		t.Fatalf("prepared state = %+v, want only a fresh pending request", prepared)
	}
	dir := store.Dir()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestStoreAt(t, dir)
	if err := reopened.PrepareConnectorResourceReuse(t.Context(), old.ConnectorID); err != nil {
		t.Fatal(err)
	}
	tx, err := reopened.BeginConnectorResource(t.Context(), old.ConnectorID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Close() }()
	request := tx.Request()
	if request.RequestNonce != pending.RequestNonce || request.ConnectorID != pending.ConnectorID || request.ExpectedResourceID != "" {
		t.Fatalf("request after restart = %+v, want exact prepared request %+v", request, pending)
	}
	// A shared cell knock target can stay the same while the public and routing
	// identities change. The replacement must become the next warm assertion.
	replacement := testResourceBinding(t, old.ConnectorID)
	if err := tx.Commit(&replacement); err != nil {
		t.Fatal(err)
	}
	if err := tx.Close(); err != nil {
		t.Fatal(err)
	}
	warm, err := reopened.BeginConnectorResource(t.Context(), old.ConnectorID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = warm.Close() }()
	if got := warm.Request(); got.ExpectedResourceID != replacement.ResourceID || got.RequestNonce == pending.RequestNonce {
		t.Fatalf("warm replacement request = %+v", got)
	}
}

func TestPrepareConnectorResourceReuseLeavesOtherStateUnchanged(t *testing.T) {
	for _, name := range []string{"absent", "live", "cold pending", "warm pending"} {
		t.Run(name, func(t *testing.T) {
			store := openTestStore(t)
			binding := testResourceBinding(t, "unchanged-api")
			if name == "live" || name == "warm pending" {
				commitTestBinding(t, store, &binding)
			}
			if strings.HasSuffix(name, "pending") {
				tx, err := store.BeginConnectorResource(t.Context(), binding.ConnectorID)
				if err != nil {
					t.Fatal(err)
				}
				if err := tx.Close(); err != nil {
					t.Fatal(err)
				}
			}
			before, beforeErr := os.ReadFile(filepath.Join(store.Dir(), ConnectorResourcesFile))
			if beforeErr != nil && !errors.Is(beforeErr, os.ErrNotExist) {
				t.Fatal(beforeErr)
			}
			if err := store.PrepareConnectorResourceReuse(t.Context(), binding.ConnectorID); err != nil {
				t.Fatal(err)
			}
			after, afterErr := os.ReadFile(filepath.Join(store.Dir(), ConnectorResourcesFile))
			if !bytes.Equal(before, after) || (beforeErr == nil) != (afterErr == nil) ||
				(afterErr != nil && !errors.Is(afterErr, os.ErrNotExist)) {
				t.Fatalf("prepare changed state: before=%s err=%v; after=%s err=%v", before, beforeErr, after, afterErr)
			}
		})
	}
}

func TestPrepareConnectorResourceReuseConcurrentRetriesKeepNonce(t *testing.T) {
	store := openTestStore(t)
	other := openTestStoreAt(t, store.Dir())
	binding := testResourceBinding(t, "concurrent-api")
	retireTestBinding(t, store, &binding)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	requests := make(chan PendingConnectorResourceRequest, 8)
	for i := range 8 {
		target := store
		if i%2 != 0 {
			target = other
		}
		wg.Go(func() {
			if err := target.PrepareConnectorResourceReuse(t.Context(), binding.ConnectorID); err != nil {
				errs <- err
				return
			}
			prepared, err := loadConnectorResources(target.Dir())
			if err != nil {
				errs <- err
				return
			}
			requests <- prepared.Pending[binding.ConnectorID]
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	close(requests)
	first := <-requests
	if first.RequestNonce == "" || first.ExpectedResourceID != "" {
		t.Fatalf("first prepared request = %+v", first)
	}
	for request := range requests {
		if request != first {
			t.Fatalf("concurrent prepare produced a different request: got %+v, want %+v", request, first)
		}
	}
	before, err := os.ReadFile(filepath.Join(store.Dir(), ConnectorResourcesFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PrepareConnectorResourceReuse(t.Context(), binding.ConnectorID); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(store.Dir(), ConnectorResourcesFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("concurrent retries changed the prepared request: before=%s after=%s", before, after)
	}
}

func TestPrepareConnectorResourceReuseBoundsPendingMemory(t *testing.T) {
	store := openTestStore(t)
	binding := testResourceBinding(t, "aaa-reused-api")
	// This ID sorts first: pruning must protect it explicitly, not by order.
	seed := emptyConnectorResourcesState()
	seed.Bindings[binding.ConnectorID] = binding
	seed.Retired[binding.ConnectorID] = true
	for i := range connectorResourcesMaxPending {
		id := fmt.Sprintf("pending-%04d", i)
		seed.Pending[id] = PendingConnectorResourceRequest{ConnectorID: id, RequestNonce: testRequestNonce(t)}
	}
	if err := writeConnectorResources(store.Dir(), seed); err != nil {
		t.Fatal(err)
	}
	if err := store.PrepareConnectorResourceReuse(t.Context(), binding.ConnectorID); err != nil {
		t.Fatal(err)
	}
	prepared, err := loadConnectorResources(store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Pending) != connectorResourcesMaxPending || prepared.Pending[binding.ConnectorID].RequestNonce == "" {
		t.Fatalf("reuse did not keep its request within pending bound: count=%d reused=%+v", len(prepared.Pending), prepared.Pending[binding.ConnectorID])
	}
}

func TestPrepareConnectorResourceReuseWaitsForActiveTransaction(t *testing.T) {
	store := openTestStore(t)
	other := openTestStoreAt(t, store.Dir())
	binding := testResourceBinding(t, "active-api")
	commitTestBinding(t, store, &binding)
	tx, err := store.BeginConnectorResource(t.Context(), binding.ConnectorID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := other.PrepareConnectorResourceReuse(ctx, binding.ConnectorID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("prepare during active transaction = %v, want deadline exceeded", err)
	}
	if err := tx.Commit(&binding); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareConnectorResourceReuseStaleDeleteCannotRetireReplacement(t *testing.T) {
	store := openTestStore(t)
	old := testResourceBinding(t, "replacement-api")
	retireTestBinding(t, store, &old)
	if err := store.PrepareConnectorResourceReuse(t.Context(), old.ConnectorID); err != nil {
		t.Fatal(err)
	}
	prepared, err := loadConnectorResources(store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, oldID := range []string{old.ResourceID, old.CRID} {
		if retired, err := store.RetireConnectorResource(t.Context(), oldID); err != nil || retired {
			t.Fatalf("stale delete affected pending reuse: retired=%t err=%v", retired, err)
		}
	}
	afterStaleDelete, err := loadConnectorResources(store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if got := afterStaleDelete.Pending[old.ConnectorID]; got != prepared.Pending[old.ConnectorID] {
		t.Fatalf("stale delete changed exact pending request: got %+v, want %+v", got, prepared.Pending[old.ConnectorID])
	}
	replacement := testResourceBinding(t, old.ConnectorID)
	commitTestBinding(t, store, &replacement)
	for _, oldID := range []string{old.ResourceID, old.CRID} {
		if retired, err := store.RetireConnectorResource(t.Context(), oldID); err != nil || retired {
			t.Fatalf("stale delete retired replacement: retired=%t err=%v", retired, err)
		}
	}
	got, retired, found, err := store.ConnectorResourceBinding(t.Context(), old.ConnectorID)
	if err != nil || !found || retired || got != replacement {
		t.Fatalf("replacement binding = %+v retired=%t found=%t err=%v", got, retired, found, err)
	}
}

func TestPrepareConnectorResourceReuseStateAccessFailurePreservesRetirement(t *testing.T) {
	store := openTestStore(t)
	binding := testResourceBinding(t, "retained-api")
	retireTestBinding(t, store, &binding)
	path := filepath.Join(store.Dir(), ConnectorResourcesFile)
	before, err := os.ReadFile(filepath.Join(store.Dir(), ConnectorResourcesFile))
	if err != nil {
		t.Fatal(err)
	}
	saved := filepath.Join(store.Dir(), "saved-resources.json")
	if err := os.Rename(path, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(saved, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := store.PrepareConnectorResourceReuse(t.Context(), binding.ConnectorID); err == nil {
		t.Fatal("unsafe state path accepted")
	}
	after, err := os.ReadFile(filepath.Join(store.Dir(), "saved-resources.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("failed prepare changed accepted state: %s err=%v", after, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginConnectorResource(t.Context(), binding.ConnectorID); !errors.Is(err, ErrConnectorResourceRetired) {
		t.Fatalf("failed prepare cleared retirement: %v", err)
	}
}

func TestPrepareConnectorResourceReuseRejectsInvalidInputs(t *testing.T) {
	store := openTestStore(t)
	if err := store.PrepareConnectorResourceReuse(nil, "valid-api"); err == nil { //nolint:staticcheck // A nil context must fail without changing state.
		t.Fatal("nil context accepted")
	}
	if err := store.PrepareConnectorResourceReuse(t.Context(), "INVALID"); err == nil {
		t.Fatal("invalid Connector ID accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := store.PrepareConnectorResourceReuse(ctx, "valid-api"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled prepare = %v", err)
	}
	var absent *Store
	if err := absent.PrepareConnectorResourceReuse(t.Context(), "valid-api"); err == nil {
		t.Fatal("nil store accepted")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.PrepareConnectorResourceReuse(t.Context(), "valid-api"); err == nil {
		t.Fatal("closed store accepted")
	}
}
