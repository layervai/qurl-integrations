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

// TestConnectorResourceStateRoundTripsMaxItemsUnderByteCap fills the maps to
// the hard per-map cap with maximal entries (64-char Connector and knock
// identities; resource identities and CRIDs are fixed-length) in the two
// shapes the loader accepts, and proves each round-trips well under the byte
// cap with room to spare. "assertions" fills bindings and pending with every
// request asserting its binding's identity: retiring an ID would displace one
// of those assertions with a smaller retired entry and a smaller fresh
// request, so this is the byte maximum. "retired" fills all three maps, which
// forces every pending request onto a fresh ID with no assertion. Every field
// is already at its largest legal size, so the measurement moves only if the
// schema does, and then the cap must be re-measured anyway.
func TestConnectorResourceStateRoundTripsMaxItemsUnderByteCap(t *testing.T) {
	t.Parallel()
	// One set of keypairs serves both shapes; the subtests only read it.
	bindings := make([]ConnectorResourceBinding, 0, connectorResourcesMaxItems)
	for i := 0; i < connectorResourcesMaxItems; i++ {
		binding := testResourceBinding(t, fmt.Sprintf("c%063d", i))
		binding.KnockResourceID = strings.Repeat("k", 64)
		bindings = append(bindings, binding)
	}
	for _, shape := range []struct {
		name    string
		retired bool
	}{{name: "assertions", retired: false}, {name: "retired", retired: true}} {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()
			dir := secureStateTestDir(t)
			state := emptyConnectorResourcesState()
			for i, binding := range bindings {
				state.Bindings[binding.ConnectorID] = binding
				if !shape.retired {
					state.Pending[binding.ConnectorID] = PendingConnectorResourceRequest{
						ConnectorID: binding.ConnectorID, RequestNonce: testRequestNonce(t), ExpectedResourceID: binding.ResourceID,
					}
					continue
				}
				state.Retired[binding.ConnectorID] = true
				fresh := fmt.Sprintf("p%063d", i)
				state.Pending[fresh] = PendingConnectorResourceRequest{ConnectorID: fresh, RequestNonce: testRequestNonce(t)}
			}
			if err := writeConnectorResources(dir, state); err != nil {
				t.Fatalf("write %d-entry state: %v", connectorResourcesMaxItems, err)
			}
			info, err := os.Stat(filepath.Join(dir, ConnectorResourcesFile))
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s shape: %d bindings, %d pending, %d retired: %d bytes (cap %d)", shape.name, len(state.Bindings), len(state.Pending), len(state.Retired), info.Size(), connectorResourcesMaxBytes)
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
			if len(loaded.Bindings) != len(state.Bindings) || len(loaded.Pending) != len(state.Pending) || len(loaded.Retired) != len(state.Retired) {
				t.Fatalf("reloaded %d/%d/%d bindings/pending/retired, want %d/%d/%d", len(loaded.Bindings), len(loaded.Pending), len(loaded.Retired), len(state.Bindings), len(state.Pending), len(state.Retired))
			}
		})
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

// testRetiredChain seeds links retired default-ID links from root, each
// derived from its predecessor exactly as the publish command derives them,
// and returns the link IDs in walk order plus the ID the walk reaches after
// the last link; the caller decides whether that tail is live or absent.
func testRetiredChain(t *testing.T, state *connectorResourcesState, root string, links int) (chain []string, tail string) {
	t.Helper()
	id := root
	for i := 0; i < links; i++ {
		binding := testResourceBinding(t, id)
		state.Bindings[id] = binding
		state.Retired[id] = true
		chain = append(chain, id)
		next, err := ReplacementConnectorID(id, binding.ResourceID)
		if err != nil {
			t.Fatal(err)
		}
		id = next
	}
	return chain, id
}

// resolveDefault runs the publish command's own default-ID walk.
func resolveDefault(t *testing.T, store *Store, root string) string {
	t.Helper()
	id, _, err := store.ResolveDefaultConnectorID(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestConnectorResourceRetiredEvictionKeepsLiveTailedChains proves what
// eviction does to default-ID chains, asserting through the same walk the
// publish command uses. Links of a chain that still ends in a live share are
// never cut while anything else can be forgotten; a chain whose tail was
// itself deleted is cut wherever key order says, restarts there, and its
// links beyond the cut decay as ordinary leftovers; and when every remembered
// retirement leads to a live share (one long chain, or many recycled targets
// each holding one link), the forced cut lands on the link just before a live
// share, the walk restarts there, and the rest of that chain is unprotected
// from then on.
func TestConnectorResourceRetiredEvictionKeepsLiveTailedChains(t *testing.T) {
	t.Parallel()
	const root = "chain-root"
	fill := func(t *testing.T, state *connectorResourcesState, count int) {
		t.Helper()
		for i := 0; i < count; i++ {
			binding := testResourceBinding(t, fmt.Sprintf("old-%04d", i))
			state.Bindings[binding.ConnectorID] = binding
			state.Retired[binding.ConnectorID] = true
		}
	}
	retireLive := func(t *testing.T, store *Store, id string) connectorResourcesState {
		t.Helper()
		live := testResourceBinding(t, id)
		commitTestBinding(t, store, &live)
		retired, err := store.RetireConnectorResource(context.Background(), live.CRID)
		if err != nil || !retired {
			t.Fatalf("RetireConnectorResource(%s) = %t, %v, want true", id, retired, err)
		}
		loaded, err := loadConnectorResources(store.Dir())
		if err != nil {
			t.Fatal(err)
		}
		if len(loaded.Retired) != connectorResourcesMaxRetired || !loaded.Retired[id] {
			t.Fatalf("after retiring %s: %d retired (want %d), kept=%t", id, len(loaded.Retired), connectorResourcesMaxRetired, loaded.Retired[id])
		}
		return loaded
	}
	missingLinks := func(loaded connectorResourcesState, chain []string) (missing []string) {
		for _, link := range chain {
			if _, bound := loaded.Bindings[link]; !bound {
				missing = append(missing, link)
			}
		}
		return missing
	}

	t.Run("live tail is never cut", func(t *testing.T) {
		t.Parallel()
		store := openTestStore(t)
		state := emptyConnectorResourcesState()
		chain, tail := testRetiredChain(t, &state, root, 2)
		live := testResourceBinding(t, tail)
		state.Bindings[tail] = live
		fill(t, &state, connectorResourcesMaxRetired-len(chain))
		if err := writeConnectorResources(store.Dir(), state); err != nil {
			t.Fatal(err)
		}
		loaded := retireLive(t, store, "zz-extra")
		if missing := missingLinks(loaded, chain); len(missing) != 0 {
			t.Fatalf("chain links %v were cut", missing)
		}
		if loaded.Bindings[tail] != live || loaded.Retired[tail] {
			t.Fatalf("live tail changed: %+v retired=%t", loaded.Bindings[tail], loaded.Retired[tail])
		}
		if _, bound := loaded.Bindings["old-0000"]; bound {
			t.Fatal("the oldest-in-file leftover should have been forgotten instead")
		}
		got, advanced, err := store.ResolveDefaultConnectorID(context.Background(), root)
		if err != nil || got != tail || advanced != len(chain) {
			t.Fatalf("default walk = %q after %d links, %v; want the live tail %q after %d", got, advanced, err, tail, len(chain))
		}
	})

	t.Run("deleted tail restarts at the cut and decays", func(t *testing.T) {
		t.Parallel()
		store := openTestStore(t)
		state := emptyConnectorResourcesState()
		chain, tail := testRetiredChain(t, &state, root, 3)
		fill(t, &state, connectorResourcesMaxRetired-len(chain))
		if err := writeConnectorResources(store.Dir(), state); err != nil {
			t.Fatal(err)
		}
		// Nothing leads to a live share, so key order decides: the root sorts
		// before every "local-" link and every "old-" leftover.
		loaded := retireLive(t, store, "zz-extra")
		if missing := missingLinks(loaded, chain); len(missing) != 1 || missing[0] != root {
			t.Fatalf("cut links = %v, want just the root", missing)
		}
		if got := resolveDefault(t, store, root); got != root {
			t.Fatalf("default walk resolved %q, want a restart at the cut root %q", got, root)
		}
		if _, bound := loaded.Bindings[tail]; bound {
			t.Fatalf("deleted tail %s should never have been bound", tail)
		}
		// The orphaned links beyond the cut are ordinary leftovers now: the next
		// eviction takes one of them (every "local-" link sorts ahead of every
		// "old-" entry; which link is key order among the links themselves).
		loaded = retireLive(t, store, "zz-extra-2")
		if missing := missingLinks(loaded, chain); len(missing) != 2 {
			t.Fatalf("cut links after the next eviction = %v, want the root plus one orphaned link", missing)
		}
		if _, bound := loaded.Bindings["old-0000"]; !bound {
			t.Fatal("an old leftover was evicted ahead of an orphaned link")
		}
	})

	t.Run("forced cut takes the link before the live share", func(t *testing.T) {
		t.Parallel()
		store := openTestStore(t)
		state := emptyConnectorResourcesState()
		chain, tail := testRetiredChain(t, &state, root, connectorResourcesMaxRetired)
		live := testResourceBinding(t, tail)
		state.Bindings[tail] = live
		if err := writeConnectorResources(store.Dir(), state); err != nil {
			t.Fatal(err)
		}
		loaded := retireLive(t, store, "zz-extra")
		last := chain[len(chain)-1]
		if missing := missingLinks(loaded, chain); len(missing) != 1 || missing[0] != last {
			t.Fatalf("forced cut removed %v, want only the link before the live share %q", missing, last)
		}
		if loaded.Bindings[tail] != live || loaded.Retired[tail] {
			t.Fatalf("live tail changed: %+v retired=%t", loaded.Bindings[tail], loaded.Retired[tail])
		}
		if got := resolveDefault(t, store, root); got != last {
			t.Fatalf("default walk resolved %q, want a restart at the cut link %q", got, last)
		}
		// The rest of that chain no longer ends in a live share, so the next
		// eviction is an ordinary leftover from it, not another forced cut.
		loaded = retireLive(t, store, "zz-extra-2")
		if missing := missingLinks(loaded, chain); len(missing) != 2 {
			t.Fatalf("after the next eviction %d links are gone, want 2", len(missing))
		}
		if loaded.Bindings[tail] != live || !loaded.Retired["zz-extra"] {
			t.Fatal("the next eviction touched the live share or the previous retirement instead of the unprotected chain")
		}
	})

	t.Run("saturated by recycled targets cuts exactly one", func(t *testing.T) {
		t.Parallel()
		store := openTestStore(t)
		state := emptyConnectorResourcesState()
		lives := make(map[string]string, connectorResourcesMaxRetired)
		for i := 0; i < connectorResourcesMaxRetired; i++ {
			chain, tail := testRetiredChain(t, &state, fmt.Sprintf("tgt-%04d", i), 1)
			state.Bindings[tail] = testResourceBinding(t, tail)
			lives[chain[0]] = tail
		}
		if err := writeConnectorResources(store.Dir(), state); err != nil {
			t.Fatal(err)
		}
		loaded := retireLive(t, store, "zz-extra")
		cut := 0
		for target, tail := range lives {
			if _, bound := loaded.Bindings[tail]; !bound || loaded.Retired[tail] {
				t.Fatalf("live share %s of %s was touched", tail, target)
			}
			if _, bound := loaded.Bindings[target]; bound {
				continue
			}
			cut++
			if target != "tgt-0000" {
				t.Fatalf("forced cut took %s, want the first in key order", target)
			}
			if got := resolveDefault(t, store, target); got != target {
				t.Fatalf("cut target %s resolves to %q, want a restart at itself", target, got)
			}
		}
		if cut != 1 {
			t.Fatalf("forced cut removed %d links, want exactly one", cut)
		}
		if got := resolveDefault(t, store, "tgt-0001"); got != lives["tgt-0001"] {
			t.Fatalf("intact target resolves to %q, want its live share %q", got, lives["tgt-0001"])
		}
	})
}

func TestReplacementConnectorID(t *testing.T) {
	t.Parallel()
	first, err := ReplacementConnectorID("local-a234567890123456", strings.Repeat("a", 91))
	if err != nil {
		t.Fatal(err)
	}
	again, _ := ReplacementConnectorID("local-a234567890123456", strings.Repeat("a", 91))
	other, _ := ReplacementConnectorID("local-a234567890123456", strings.Repeat("b", 91))
	if first != again || first == other || !strings.HasPrefix(first, "local-") || len(first) != len("local-")+16 {
		t.Fatalf("replacement IDs first=%q again=%q other=%q", first, again, other)
	}
	if err := validateConnectorID(first); err != nil {
		t.Fatalf("replacement ID invalid: %v", err)
	}
	if _, err := ReplacementConnectorID("", "resource"); err == nil {
		t.Fatal("empty predecessor identity was accepted")
	}
	if _, err := ReplacementConnectorID("local-a234567890123456", " "); err == nil {
		t.Fatal("blank resource identity was accepted")
	}
}

// TestReplacementConnectorIDGolden pins the derivation to the value the
// publish command produced before it moved here, so a drift in the domain
// string or encoding cannot silently change every existing user's next
// default Connector ID.
func TestReplacementConnectorIDGolden(t *testing.T) {
	t.Parallel()
	got, err := ReplacementConnectorID("local-a234567890123456", strings.Repeat("a", 91))
	if err != nil || got != "local-knu7h5msm6nxb3nf" {
		t.Fatalf("ReplacementConnectorID() = %q, %v, want %q", got, err, "local-knu7h5msm6nxb3nf")
	}
}
