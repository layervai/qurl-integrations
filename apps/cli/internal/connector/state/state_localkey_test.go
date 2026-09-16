//go:build unix

package state

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
	qurl "github.com/layervai/qurl-go/qurl"
	"golang.org/x/sys/unix"
)

// nextLocalKeyFD is the floor for the next wrapping-key descriptor number. The
// connector caches the wrapping key per process by descriptor number and
// closes the descriptor once it has read the key, so the next pipe would
// inherit both the number and the cached key. Re-homing every pipe above each
// number used so far keeps distinct keys distinct.
//
// Package-level mutable state is safe here only because every test that
// reaches it goes through useLocalKey, which calls t.Setenv and therefore
// forbids t.Parallel. Do not parallelize these tests without making this a
// synchronized counter: the race would be on descriptor identity, not just
// on the integer.
var nextLocalKeyFD = 64

// localKeyFD writes key to an anonymous pipe and returns the read end's
// descriptor number, formatted for LAYERV_LOCAL_KEY_FD, at a number no earlier
// key in this process has used.
func localKeyFD(t *testing.T, key []byte) string {
	t.Helper()
	var fds [2]int
	if err := unix.Pipe(fds[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.Write(fds[1], key); err != nil {
		t.Fatal(err)
	}
	if err := unix.Close(fds[1]); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.FcntlInt(uintptr(fds[0]), unix.F_DUPFD_CLOEXEC, nextLocalKeyFD)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Close(fds[0]); err != nil {
		t.Fatal(err)
	}
	nextLocalKeyFD = fd + 1
	// The dup'd read end is deliberately not closed on cleanup: the connector
	// closes it once it has read the key, so a t.Cleanup close would be a
	// double close on a number the runtime may have handed out again. A test
	// whose Open is refused before the provider is constructed leaks its
	// descriptor for the life of the test binary, which nextLocalKeyFD's
	// re-homing makes harmless.
	return strconv.Itoa(fd)
}

func useLocalKey(t *testing.T, fill byte) {
	t.Helper()
	t.Setenv(connectoragentstate.EnvKeyProvider, connectoragentstate.KeyProviderLocalKey)
	t.Setenv(connectoragentstate.EnvLocalKeyFD, localKeyFD(t, bytes.Repeat([]byte{fill}, 32)))
}

func saveTestAgentState(t *testing.T, store *Store) qurl.AgentStateStore {
	t.Helper()
	sdkStore, err := store.Handoff()
	if err != nil {
		t.Fatal(err)
	}
	if err := sdkStore.SaveAgentState(context.Background(), &qurl.AgentState{AgentID: "a"}); err != nil {
		t.Fatal(err)
	}
	return sdkStore
}

func TestOpenWithLocalKeySealsState(t *testing.T) {
	clearStateEnv(t)
	dir := secureStateTestDir(t)
	useLocalKey(t, 7)

	store := openTestStoreAt(t, dir)
	sdkStore := saveTestAgentState(t, store)
	if _, ok := sdkStore.(*qurl.SealedFileAgentStateStore); !ok {
		t.Fatalf("Handoff() returned %T, want the concrete *qurl.SealedFileAgentStateStore so SDK lock contracts stay active", sdkStore)
	}
	if _, err := os.Stat(filepath.Join(dir, AgentStateFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plaintext %s written under local-key: stat err=%v", AgentStateFile, err)
	}
	if _, err := os.Stat(filepath.Join(dir, connectoragentstate.SealedAgentStateFile)); err != nil {
		t.Fatalf("sealed envelope missing: %v", err)
	}
	if present, err := store.AgentStatePresent(); err != nil || !present {
		t.Fatalf("AgentStatePresent() = (%v, %v), want true for the sealed envelope", present, err)
	}
	loaded, err := sdkStore.LoadAgentState(context.Background())
	if err != nil || loaded == nil || loaded.AgentID != "a" {
		t.Fatalf("LoadAgentState() = (%+v, %v), want the sealed state back", loaded, err)
	}
}

func TestOpenLocalKeyWrongKeyFailsClosed(t *testing.T) {
	clearStateEnv(t)
	dir := secureStateTestDir(t)
	useLocalKey(t, 1)
	sealed := openTestStoreAt(t, dir)
	saveTestAgentState(t, sealed)
	if err := sealed.Close(); err != nil {
		t.Fatal(err)
	}

	useLocalKey(t, 2)
	reopened := openTestStoreAt(t, dir)
	sdkStore, err := reopened.Handoff()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := sdkStore.LoadAgentState(context.Background())
	if err == nil || loaded != nil {
		t.Fatalf("LoadAgentState() under the wrong wrapping key = (%+v, %v), want an error", loaded, err)
	}
	if errors.Is(err, qurl.ErrAgentStateNotFound) {
		t.Fatalf("wrong wrapping key reported as absent state, which would invite re-enrollment: %v", err)
	}
}

func TestOpenLocalKeyRefusesPlaintextEnvelope(t *testing.T) {
	clearStateEnv(t)
	dir := secureStateTestDir(t)
	plaintext := openTestStoreAt(t, dir)
	saveTestAgentState(t, plaintext)
	if err := plaintext.Close(); err != nil {
		t.Fatal(err)
	}

	useLocalKey(t, 3)
	store, err := Open(dir)
	if err == nil {
		_ = store.Close()
		t.Fatal("Open() accepted local-key over an existing plaintext envelope")
	}
	// TODO(upstream-contract): this asserts on qurl-connector's own refusal
	// from validateSDKStoreLayoutInNamespace. The assertion is deliberately
	// loose - it checks only that the message names the variable and the
	// envelope, not its wording - so an upstream reword that keeps naming both
	// stays green.
	if !errors.Is(err, ErrAgentStateEnvelope) {
		t.Fatalf("Open error = %v, want ErrAgentStateEnvelope so exitcode maps it to Config in this direction too", err)
	}
	if !strings.Contains(err.Error(), connectoragentstate.EnvKeyProvider) || !strings.Contains(err.Error(), AgentStateFile) {
		t.Fatalf("Open() error = %v, want the connector's envelope-conflict refusal naming %s and %s", err, connectoragentstate.EnvKeyProvider, AgentStateFile)
	}
	if _, err := os.Stat(filepath.Join(dir, connectoragentstate.SealedAgentStateFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sealed envelope created next to the refused plaintext one: stat err=%v", err)
	}
}

func TestOpenFileProviderStaysPlaintext(t *testing.T) {
	clearStateEnv(t)
	t.Setenv(connectoragentstate.EnvKeyProvider, " File ")
	store := openTestStore(t)
	sdkStore := saveTestAgentState(t, store)
	if _, ok := sdkStore.(*qurl.FileAgentStateStore); !ok {
		t.Fatalf("Handoff() returned %T, want the plaintext *qurl.FileAgentStateStore", sdkStore)
	}
	if _, err := os.Stat(filepath.Join(store.Dir(), AgentStateFile)); err != nil {
		t.Fatalf("plaintext envelope missing under the explicit file provider: %v", err)
	}
}

// TestOpenWithLocalKeyReopensWithinOneProcess pins the descriptor contract a
// supervisor relies on: it hands every qurl process exactly one wrapping-key
// descriptor, yet one command opens the sealed envelope several times
// (publish keeps its registered runtime open while resource discovery opens
// a second runtime and this store again). The first open consumes the
// descriptor; every later open, concurrent or after a close, must read the
// same key from the connector's per-process cache.
func TestOpenWithLocalKeyReopensWithinOneProcess(t *testing.T) {
	clearStateEnv(t)
	dir := secureStateTestDir(t)
	useLocalKey(t, 9)
	first, err := Open(dir)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	// Closed explicitly below to prove a reopen works after every store is
	// gone; the cleanups keep an intermediate t.Fatal from leaking a pinned
	// namespace capability, and a second Close is a no-op.
	t.Cleanup(func() { _ = first.Close() })
	saveTestAgentState(t, first)

	loadThrough := func(store *Store) error {
		sdkStore, err := store.Handoff()
		if err != nil {
			return err
		}
		state, err := sdkStore.LoadAgentState(context.Background())
		if err != nil {
			return err
		}
		if state == nil || state.AgentID != "a" {
			return errors.New("sealed state did not round-trip")
		}
		return nil
	}
	second, err := Open(dir)
	if err != nil {
		t.Fatalf("second open beside the first: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if err := loadThrough(second); err != nil {
		t.Fatalf("second store: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store, err := Open(dir)
			if err != nil {
				errs <- err
				return
			}
			defer func() { _ = store.Close() }()
			errs <- loadThrough(store)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent open: %v", err)
		}
	}
	if err := errors.Join(second.Close(), first.Close()); err != nil {
		t.Fatal(err)
	}
	third, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen after every store closed: %v", err)
	}
	defer func() { _ = third.Close() }()
	if err := loadThrough(third); err != nil {
		t.Fatalf("third store: %v", err)
	}
}

// TestSealedStoreFailsClosedAfterClose runs the plaintext branch's
// close contract against the sealed one: Close is idempotent and every later
// use is a continuity error. Close and ValidateContinuity route through the
// stateOwner interface, so both implementations need the pin.
func TestSealedStoreFailsClosedAfterClose(t *testing.T) {
	clearStateEnv(t)
	dir := secureStateTestDir(t)
	useLocalKey(t, 'k')
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if present, err := store.AgentStatePresent(); err != nil || present {
		t.Fatalf("fresh sealed AgentStatePresent() = (%t, %v), want (false, nil)", present, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second Close() = %v, want idempotent nil", err)
	}
	if _, err := store.Handoff(); !errors.Is(err, qurl.ErrAgentStateContinuity) {
		t.Fatalf("Handoff() after Close = %v, want state-continuity error", err)
	}
	if err := store.ValidateContinuity(); !errors.Is(err, qurl.ErrAgentStateContinuity) {
		t.Fatalf("ValidateContinuity() after Close = %v, want state-continuity error", err)
	}
}

// TestOpenLocalKeyOverAPopulatedNamespace pins the second and later runs: a
// sealed namespace in service also holds qurl's own registries, and the
// connector's legacy-artifact reject list must not claim any of their names.
// The Open comment states that invariant; this test enforces it, so an
// upstream reject-list change fails in CI rather than in the field.
func TestOpenLocalKeyOverAPopulatedNamespace(t *testing.T) {
	clearStateEnv(t)
	dir := secureStateTestDir(t)
	useLocalKey(t, 'p')
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	saveTestAgentState(t, store)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{LocalSharesFile, ConnectorResourcesFile, RuntimeModeFile, connectorResourcesLock} {
		if err := replaceConnectorResources(dir, filepath.Join(dir, name), []byte("{}")); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("sealed Open over a populated namespace = %v, want the registries to be ordinary neighbors", err)
	}
	if present, err := reopened.AgentStatePresent(); err != nil || !present {
		t.Fatalf("AgentStatePresent() = (%t, %v), want the saved sealed envelope", present, err)
	}
}

// TestOpenLocalKeyPinsTheEnvelopeToTheConfiguredAgentID covers the one input
// to NewSDKStore no other sealed test varies. Open passes ConfiguredAgentID
// through and the sealed envelope is pinned to it, so a reopen under the same
// ID round-trips, and a store configured for a different ID cannot write to
// it.
func TestOpenLocalKeyPinsTheEnvelopeToTheConfiguredAgentID(t *testing.T) {
	clearStateEnv(t)
	t.Setenv(EnvAgentID, "a")
	dir := secureStateTestDir(t)
	useLocalKey(t, 'i')
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	saveTestAgentState(t, store)
	if store.envelope != connectoragentstate.SealedAgentStateFile {
		t.Fatalf("envelope = %q, want the sealed one", store.envelope)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen under the same agent ID = %v", err)
	}
	if present, err := reopened.AgentStatePresent(); err != nil || !present {
		t.Fatalf("AgentStatePresent() = (%t, %v), want the saved envelope", present, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	// The pin is enforced on the write, not on the open: a store configured
	// for a different agent may read the envelope but may not save an
	// AgentState that disagrees with its configured expectation.
	t.Setenv(EnvAgentID, "another-agent")
	useLocalKey(t, 'i')
	other, err := Open(dir)
	if err != nil {
		t.Fatalf("open under another agent ID = %v", err)
	}
	t.Cleanup(func() { _ = other.Close() })
	sdkStore, err := other.Handoff()
	if err != nil {
		t.Fatal(err)
	}
	if err := sdkStore.SaveAgentState(context.Background(), &qurl.AgentState{AgentID: "a"}); err == nil {
		t.Fatal("a store configured for another agent saved this envelope's state")
	}
}
