package state

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// legacyConnectorResourcesMaxItems is the per-map cap this file shipped with
// before the 1000-share work. A live proof run hit it after 683 publishes:
// 340 retired bindings from earlier runs still occupied slots, and every
// further publish failed at commit. Tests below use it as the boundary a
// load-shaped run must cross to prove the cap is no longer the limit.
const legacyConnectorResourcesMaxItems = 1024

// TestConnectorResourceCapsFitRegistryAndChurn pins the ordering the compile
// time constants in resource.go also enforce, in a form that names each cap:
// the resource-state map must hold a live binding for every share registry
// row plus the whole retired memory, or deleting shares could still crowd out
// live ones below the registry cap that #1326 raised for the same goal.
func TestConnectorResourceCapsFitRegistryAndChurn(t *testing.T) {
	t.Parallel()
	if connectorResourcesMaxItems < LocalSharesMaxItems {
		t.Fatalf("connectorResourcesMaxItems = %d is below LocalSharesMaxItems = %d: the resource state would refuse a binding the registry accepts", connectorResourcesMaxItems, LocalSharesMaxItems)
	}
	if connectorResourcesMaxItems < LocalSharesMaxItems+connectorResourcesMaxRetired {
		t.Fatalf("connectorResourcesMaxItems = %d cannot hold LocalSharesMaxItems = %d live bindings plus connectorResourcesMaxRetired = %d retired ones", connectorResourcesMaxItems, LocalSharesMaxItems, connectorResourcesMaxRetired)
	}
	if connectorResourcesMaxPending > connectorResourcesMaxItems {
		t.Fatalf("connectorResourcesMaxPending = %d exceeds the hard cap %d it is pruned toward", connectorResourcesMaxPending, connectorResourcesMaxItems)
	}
	if connectorResourcesMaxItems <= legacyConnectorResourcesMaxItems {
		t.Fatalf("connectorResourcesMaxItems = %d does not exceed the legacy cap %d that a 1000-share run already hit", connectorResourcesMaxItems, legacyConnectorResourcesMaxItems)
	}
}

// TestConnectorResourceStateRoundTripsMaxItemsUnderByteCap fills bindings and
// pending to the hard per-map cap with maximal entries (64-char Connector and
// knock identities, every pending request asserting its binding's identity,
// which is the larger of the two legal pending shapes) and proves the file
// round-trips well under the byte cap with room to spare.
func TestConnectorResourceStateRoundTripsMaxItemsUnderByteCap(t *testing.T) {
	t.Parallel()
	dir := secureStateTestDir(t)
	state := emptyConnectorResourcesState()
	for i := 0; i < connectorResourcesMaxItems; i++ {
		binding := testResourceBinding(t, fmt.Sprintf("c%063d", i))
		binding.KnockResourceID = strings.Repeat("k", 64)
		state.Bindings[binding.ConnectorID] = binding
		state.Pending[binding.ConnectorID] = PendingConnectorResourceRequest{
			ConnectorID: binding.ConnectorID, RequestNonce: testRequestNonce(t), ExpectedResourceID: binding.ResourceID,
		}
	}
	if err := writeConnectorResources(dir, state); err != nil {
		t.Fatalf("write %d-entry state: %v", connectorResourcesMaxItems, err)
	}
	info, err := os.Stat(filepath.Join(dir, ConnectorResourcesFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d-binding, %d-pending state file size: %d bytes (cap %d)", connectorResourcesMaxItems, connectorResourcesMaxItems, info.Size(), connectorResourcesMaxBytes)
	if info.Size() >= connectorResourcesMaxBytes {
		t.Fatalf("state file size %d is not under the %d-byte cap", info.Size(), connectorResourcesMaxBytes)
	}
	// A generous margin: a full state must not sit within a hair of the cap.
	if info.Size() > connectorResourcesMaxBytes/2 {
		t.Fatalf("state file size %d leaves less than 2x headroom under the %d-byte cap", info.Size(), connectorResourcesMaxBytes)
	}
	loaded, err := loadConnectorResources(dir)
	if err != nil {
		t.Fatalf("reload %d-entry state: %v", connectorResourcesMaxItems, err)
	}
	if len(loaded.Bindings) != connectorResourcesMaxItems || len(loaded.Pending) != connectorResourcesMaxItems {
		t.Fatalf("reloaded %d bindings and %d pending, want %d each", len(loaded.Bindings), len(loaded.Pending), connectorResourcesMaxItems)
	}
}

// TestConnectorResourcePublishDeleteChurnNeverRefusesBinding is the
// load-shaped proof: more publish/delete cycles than the legacy cap. A
// completed cycle's only durable residue is one retired binding, so the first
// connectorResourcesMaxRetired cycles are seeded as exactly that (the
// earlier-run leftovers the live proof run found) and every cycle past the
// bound runs for real: a fresh binding committed through the store, then
// retired through the delete path. The old file refused the 1025th binding
// because every retired one still counted; now the very first live commit
// lands past the legacy cap, each retirement evicts the oldest-in-file
// leftover, and the file ends at exactly the bound. Every store operation on
// a file this full re-validates ~1024 P-256 bindings on load and again on
// write (three durable writes per cycle), which the race detector slows
// roughly tenfold, so the live cycle count is sized for CI rather than for
// drama: every live cycle past the first exercises the same steady state.
func TestConnectorResourcePublishDeleteChurnNeverRefusesBinding(t *testing.T) {
	t.Parallel()
	const seeded, live = connectorResourcesMaxRetired, 32
	const cycles = seeded + live
	if cycles <= legacyConnectorResourcesMaxItems {
		t.Fatalf("%d cycles do not cross the legacy cap %d", cycles, legacyConnectorResourcesMaxItems)
	}
	store := openTestStore(t)
	state := emptyConnectorResourcesState()
	for i := 0; i < seeded; i++ {
		binding := testResourceBinding(t, fmt.Sprintf("churn-%04d", i))
		state.Bindings[binding.ConnectorID] = binding
		state.Retired[binding.ConnectorID] = true
	}
	if err := writeConnectorResources(store.Dir(), state); err != nil {
		t.Fatalf("seed %d completed cycles: %v", seeded, err)
	}
	for i := seeded; i < cycles; i++ {
		binding := testResourceBinding(t, fmt.Sprintf("churn-%04d", i))
		tx, err := store.BeginConnectorResource(context.Background(), binding.ConnectorID)
		if err != nil {
			t.Fatalf("cycle %d: begin: %v", i, err)
		}
		if err := tx.Commit(&binding); err != nil {
			_ = tx.Close()
			t.Fatalf("cycle %d: commit refused a new binding: %v", i, err)
		}
		if err := tx.Close(); err != nil {
			t.Fatalf("cycle %d: close: %v", i, err)
		}
		retired, err := store.RetireConnectorResource(context.Background(), binding.ResourceID)
		if err != nil || !retired {
			t.Fatalf("cycle %d: RetireConnectorResource() = %t, %v, want true", i, retired, err)
		}
	}

	loaded, err := loadConnectorResources(store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Retired) != connectorResourcesMaxRetired || len(loaded.Bindings) != connectorResourcesMaxRetired || len(loaded.Pending) != 0 {
		t.Fatalf("after %d cycles: bindings=%d pending=%d retired=%d, want %d/0/%d", cycles, len(loaded.Bindings), len(loaded.Pending), len(loaded.Retired), connectorResourcesMaxRetired, connectorResourcesMaxRetired)
	}
	// The file's own key order decides what is forgotten: the first
	// cycles-connectorResourcesMaxRetired IDs are gone from both maps, and
	// everything after them is still refused.
	evicted := cycles - connectorResourcesMaxRetired
	for i := 0; i < cycles; i++ {
		id := fmt.Sprintf("churn-%04d", i)
		_, retired := loaded.Retired[id]
		_, bound := loaded.Bindings[id]
		if i < evicted && (retired || bound) {
			t.Fatalf("%s should have been forgotten: retired=%t bound=%t", id, retired, bound)
		}
		if i >= evicted && (!retired || !bound) {
			t.Fatalf("%s should still be remembered: retired=%t bound=%t", id, retired, bound)
		}
	}
	forgotten, err := store.BeginConnectorResource(context.Background(), "churn-0000")
	if err != nil {
		t.Fatalf("a forgotten retirement must accept a fresh request: %v", err)
	}
	if request := forgotten.Request(); request == nil || request.ExpectedResourceID != "" {
		t.Fatalf("forgotten retirement request = %+v, want a fresh request with no identity assertion", request)
	}
	if err := forgotten.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginConnectorResource(context.Background(), fmt.Sprintf("churn-%04d", evicted)); !errors.Is(err, ErrConnectorResourceRetired) {
		t.Fatalf("the oldest remembered retirement must still be refused, got %v", err)
	}
}

// TestConnectorResourceLegacyFullStateCommitsUnderRaisedCap replays the exact
// shape the live proof run left behind (1024 bindings, 317 pending, 340
// retired): a file the legacy cap had frozen must load and accept the next
// binding without any repair.
func TestConnectorResourceLegacyFullStateCommitsUnderRaisedCap(t *testing.T) {
	t.Parallel()
	const legacyBindings, legacyPending, legacyRetired = 1024, 317, 340
	if legacyBindings != legacyConnectorResourcesMaxItems {
		t.Fatalf("legacy fixture holds %d bindings, want the legacy cap %d so the next commit is the one the old file refused", legacyBindings, legacyConnectorResourcesMaxItems)
	}
	store := openTestStore(t)
	state := emptyConnectorResourcesState()
	for i := 0; i < legacyBindings; i++ {
		binding := testResourceBinding(t, fmt.Sprintf("proof-%04d", i))
		state.Bindings[binding.ConnectorID] = binding
		if i < legacyRetired {
			state.Retired[binding.ConnectorID] = true
		}
	}
	for i := 0; i < legacyPending; i++ {
		id := fmt.Sprintf("proof-%04d", legacyBindings+i)
		state.Pending[id] = PendingConnectorResourceRequest{ConnectorID: id, RequestNonce: testRequestNonce(t)}
	}
	if err := writeConnectorResources(store.Dir(), state); err != nil {
		t.Fatalf("seed legacy-shaped state: %v", err)
	}

	next := testResourceBinding(t, "proof-next")
	commitTestBinding(t, store, &next)
	loaded, err := loadConnectorResources(store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Bindings) != legacyBindings+1 || len(loaded.Pending) != legacyPending || len(loaded.Retired) != legacyRetired {
		t.Fatalf("after the first commit past the legacy cap: bindings=%d pending=%d retired=%d, want %d/%d/%d", len(loaded.Bindings), len(loaded.Pending), len(loaded.Retired), legacyBindings+1, legacyPending, legacyRetired)
	}
	if loaded.Bindings[next.ConnectorID] != next {
		t.Fatalf("committed binding = %+v, want %+v", loaded.Bindings[next.ConnectorID], next)
	}
}

// TestConnectorResourcePendingMemoryIsBounded seeds the pending memory at its
// bound and proves the next fresh request evicts the oldest-in-file request,
// keeps its own regardless of where it sorts, and turns the forgotten request
// into a fresh one while every remembered request still replays exactly.
func TestConnectorResourcePendingMemoryIsBounded(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		fresh   string
		evicted string
	}{
		{name: "fresh request sorts last", fresh: "zz-fresh", evicted: "stale-0000"},
		{name: "fresh request sorts first", fresh: "aa-fresh", evicted: "stale-0000"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := openTestStore(t)
			state := emptyConnectorResourcesState()
			for i := 0; i < connectorResourcesMaxPending; i++ {
				id := fmt.Sprintf("stale-%04d", i)
				state.Pending[id] = PendingConnectorResourceRequest{ConnectorID: id, RequestNonce: testRequestNonce(t)}
			}
			if err := writeConnectorResources(store.Dir(), state); err != nil {
				t.Fatalf("seed full pending memory: %v", err)
			}

			tx, err := store.BeginConnectorResource(context.Background(), test.fresh)
			if err != nil {
				t.Fatalf("a full pending memory must still accept a fresh request: %v", err)
			}
			freshNonce := tx.Request().RequestNonce
			if err := tx.Close(); err != nil {
				t.Fatal(err)
			}
			loaded, err := loadConnectorResources(store.Dir())
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded.Pending) != connectorResourcesMaxPending {
				t.Fatalf("pending memory holds %d requests, want the bound %d", len(loaded.Pending), connectorResourcesMaxPending)
			}
			if got := loaded.Pending[test.fresh].RequestNonce; got != freshNonce {
				t.Fatalf("the request being dispatched was not kept durable: got %q, want %q", got, freshNonce)
			}
			if _, remembered := loaded.Pending[test.evicted]; remembered {
				t.Fatalf("%s should have been forgotten", test.evicted)
			}
			for id, seeded := range state.Pending {
				if id == test.evicted {
					continue
				}
				if loaded.Pending[id] != seeded {
					t.Fatalf("remembered request %s changed: %+v, want %+v", id, loaded.Pending[id], seeded)
				}
			}

			// Replaying a remembered request adds nothing, so it evicts nothing.
			exact, err := store.BeginConnectorResource(context.Background(), "stale-0001")
			if err != nil {
				t.Fatal(err)
			}
			if got := exact.Request().RequestNonce; got != state.Pending["stale-0001"].RequestNonce {
				_ = exact.Close()
				t.Fatalf("a remembered request must replay exactly: got %q, want %q", got, state.Pending["stale-0001"].RequestNonce)
			}
			if err := exact.Close(); err != nil {
				t.Fatal(err)
			}
			// The forgotten request starts over with a fresh nonce; being fresh, it
			// evicts the next oldest-in-file entry and the memory stays at its bound.
			replay, err := store.BeginConnectorResource(context.Background(), test.evicted)
			if err != nil {
				t.Fatal(err)
			}
			if got := replay.Request().RequestNonce; got == state.Pending[test.evicted].RequestNonce {
				_ = replay.Close()
				t.Fatal("a forgotten request replayed its old nonce instead of starting fresh")
			}
			if err := replay.Close(); err != nil {
				t.Fatal(err)
			}
			if loaded, err = loadConnectorResources(store.Dir()); err != nil {
				t.Fatal(err)
			}
			if len(loaded.Pending) != connectorResourcesMaxPending || loaded.Pending[test.evicted].ConnectorID != test.evicted {
				t.Fatalf("after a second fresh request: %d pending, %s kept=%t; want %d and true", len(loaded.Pending), test.evicted, loaded.Pending[test.evicted].ConnectorID == test.evicted, connectorResourcesMaxPending)
			}
		})
	}
}

// TestConnectorResourceRetiredMemoryIsBounded seeds the retired memory at its
// bound and proves the next deletion evicts the oldest-in-file retirement from
// both maps (freeing its bindings slot), keeps its own regardless of where it
// sorts, and lets the forgotten ID be published fresh while every remembered
// one is still refused.
func TestConnectorResourceRetiredMemoryIsBounded(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		live    string
		evicted string
	}{
		{name: "deletion sorts last", live: "zz-live", evicted: "old-0000"},
		{name: "deletion sorts first", live: "aa-live", evicted: "old-0000"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			store := openTestStore(t)
			state := emptyConnectorResourcesState()
			for i := 0; i < connectorResourcesMaxRetired; i++ {
				binding := testResourceBinding(t, fmt.Sprintf("old-%04d", i))
				state.Bindings[binding.ConnectorID] = binding
				state.Retired[binding.ConnectorID] = true
			}
			live := testResourceBinding(t, test.live)
			state.Bindings[live.ConnectorID] = live
			if err := writeConnectorResources(store.Dir(), state); err != nil {
				t.Fatalf("seed full retired memory: %v", err)
			}

			retired, err := store.RetireConnectorResource(context.Background(), live.CRID)
			if err != nil || !retired {
				t.Fatalf("RetireConnectorResource() = %t, %v, want true", retired, err)
			}
			loaded, err := loadConnectorResources(store.Dir())
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded.Retired) != connectorResourcesMaxRetired || len(loaded.Bindings) != connectorResourcesMaxRetired {
				t.Fatalf("bindings=%d retired=%d, want the bound %d for both", len(loaded.Bindings), len(loaded.Retired), connectorResourcesMaxRetired)
			}
			if !loaded.Retired[live.ConnectorID] || loaded.Bindings[live.ConnectorID] != live {
				t.Fatalf("the deletion being recorded was not kept: retired=%t binding=%+v", loaded.Retired[live.ConnectorID], loaded.Bindings[live.ConnectorID])
			}
			if _, bound := loaded.Bindings[test.evicted]; bound || loaded.Retired[test.evicted] {
				t.Fatalf("%s should have been forgotten from both maps: bound=%t retired=%t", test.evicted, bound, loaded.Retired[test.evicted])
			}

			fresh, err := store.BeginConnectorResource(context.Background(), test.evicted)
			if err != nil {
				t.Fatalf("a forgotten retirement must accept a fresh request: %v", err)
			}
			if err := fresh.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := store.BeginConnectorResource(context.Background(), "old-0001"); !errors.Is(err, ErrConnectorResourceRetired) {
				t.Fatalf("a remembered retirement must still be refused, got %v", err)
			}
			if _, err := store.BeginConnectorResource(context.Background(), live.ConnectorID); !errors.Is(err, ErrConnectorResourceRetired) {
				t.Fatalf("the deletion just recorded must be refused, got %v", err)
			}
			again, err := store.RetireConnectorResource(context.Background(), live.ResourceID)
			if err != nil || !again {
				t.Fatalf("idempotent RetireConnectorResource() = %t, %v, want true", again, err)
			}
		})
	}
}

func TestEvictionOrder(t *testing.T) {
	t.Parallel()
	entries := map[string]bool{"delta": true, "alpha": true, "charlie": true, "bravo": true}
	for _, test := range []struct {
		name   string
		excess int
		keep   string
		want   []string
	}{
		{name: "nothing over", excess: 0, keep: "alpha", want: nil},
		{name: "negative excess", excess: -3, keep: "", want: nil},
		{name: "file order", excess: 2, keep: "", want: []string{"alpha", "bravo"}},
		{name: "keep skipped", excess: 2, keep: "alpha", want: []string{"bravo", "charlie"}},
		{name: "keep absent", excess: 1, keep: "zulu", want: []string{"alpha"}},
		{name: "excess beyond entries", excess: 9, keep: "charlie", want: []string{"alpha", "bravo", "delta"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := evictionOrder(entries, test.excess, test.keep); !slices.Equal(got, test.want) {
				t.Fatalf("evictionOrder(%d, %q) = %v, want %v", test.excess, test.keep, got, test.want)
			}
		})
	}
}

func testRequestNonce(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}
