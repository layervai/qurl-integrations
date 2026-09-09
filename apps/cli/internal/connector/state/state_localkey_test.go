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
	return strconv.Itoa(fd)
}

// sealedStateTestDir is secureStateTestDir with symlink components resolved:
// the connector's pinned-filesystem walk refuses them, and on macOS
// t.TempDir() lives below the /var alias.
func sealedStateTestDir(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "state")
	if err := EnsureDirMode(dir); err != nil {
		t.Fatal(err)
	}
	return dir
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
	dir := sealedStateTestDir(t)
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
	dir := sealedStateTestDir(t)
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
	dir := sealedStateTestDir(t)
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
	if !strings.Contains(err.Error(), "provider changes are not an in-place migration") {
		t.Fatalf("Open() error = %v, want the connector's envelope-conflict refusal", err)
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
	dir := sealedStateTestDir(t)
	useLocalKey(t, 9)
	first, err := Open(dir)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
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
