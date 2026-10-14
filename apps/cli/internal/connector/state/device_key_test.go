package state

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
	qurl "github.com/layervai/qurl-go/qurl"
)

// Tests for ReadDeviceStaticPrivateKey, the read-only way to the device key.
//
// Every test here checks two things: what the function returns, and that the
// state directory and the directory around it are the same afterwards. The
// function is used by a command that only fetches something, so it must
// never create, repair or lock anything.

// registeredDeviceState returns the state of a registered device with a new
// key pair, and the 32 bytes of its private key.
func registeredDeviceState(t *testing.T) (state *qurl.AgentState, privateKey []byte) {
	t.Helper()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	registeredAt := time.Now().UTC().Round(time.Second)
	return &qurl.AgentState{
		AgentID:       "agent-device-key-test",
		PrivateKeyB64: base64.StdEncoding.EncodeToString(key.Bytes()),
		PublicKeyB64:  base64.StdEncoding.EncodeToString(key.PublicKey().Bytes()),
		RegisteredAt:  &registeredAt,
	}, key.Bytes()
}

// saveStateThroughTheStore writes state into dir the way an enrollment does:
// through the CLI's own state store.
func saveStateThroughTheStore(t *testing.T, dir string, state *qurl.AgentState) {
	t.Helper()
	store, err := Open(dir)
	if err != nil {
		t.Fatalf("open the state store: %v", err)
	}
	sdkStore, err := store.Handoff()
	if err == nil {
		err = sdkStore.SaveAgentState(context.Background(), state)
	}
	if closeErr := store.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("save the device state: %v", err)
	}
}

// writeStateFile writes state as the plaintext state file of dir, with no
// check of what it holds. It is for state the store would not write.
func writeStateFile(t *testing.T, dir string, state *qurl.AgentState) {
	t.Helper()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	writeOwnerOnlyTestFile(t, dir, AgentStateFile, raw)
}

// snapshot describes every entry below root: its name, its type and
// permission bits, its size, the time it was last changed, and for a file a
// digest of its bytes. Two snapshots are equal only when nothing was added,
// removed, replaced, rewritten or given other permissions.
func snapshot(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		digest := "-"
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path) //nolint:gosec // A path below the test's own temporary directory.
			if err != nil {
				// A file this test made unreadable on purpose.
				digest = "unreadable"
			} else {
				sum := sha256.Sum256(data)
				digest = hex.EncodeToString(sum[:])
			}
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("%s mode=%s size=%d modified=%s sha256=%s %s",
			filepath.ToSlash(rel), info.Mode(), info.Size(), info.ModTime().UTC().Format(time.RFC3339Nano), digest, changeStamp(t, path)))
		return nil
	})
	if err != nil {
		t.Fatalf("describe %s: %v", root, err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// mustLeaveUnchanged runs read and fails the test when anything below root
// is different afterwards.
func mustLeaveUnchanged(t *testing.T, root string, read func()) {
	t.Helper()
	before := snapshot(t, root)
	read()
	if after := snapshot(t, root); after != before {
		t.Errorf("the read changed the state directory or what is around it.\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// wantKey is what a read of usable state returns on this platform: the key
// where the read is supported, and "platform" where it is not.
func wantKey(key []byte) (wantedKey []byte, why NoDeviceKey) {
	if !deviceKeyReadSupported {
		return nil, NoDeviceKeyPlatform
	}
	return key, ""
}

// wantNoKey is what a read of state that gives no key returns on this
// platform. Where the read is not supported, the answer is "platform"
// whatever the state holds, because the state is never opened.
func wantNoKey(why NoDeviceKey) NoDeviceKey {
	if !deviceKeyReadSupported {
		return NoDeviceKeyPlatform
	}
	return why
}

// TestReadDeviceStaticPrivateKeyFromThePlaintextFile is the main case: state
// that an enrollment wrote through the CLI's store. The function returns the
// 32 bytes of the private key in that state, and the directory is byte for
// byte the same after the read: no new file, no lock file, no changed
// permission and no changed time stamp.
func TestReadDeviceStaticPrivateKeyFromThePlaintextFile(t *testing.T) {
	clearStateEnv(t)
	dir := secureStateTestDir(t)
	state, privateKey := registeredDeviceState(t)
	saveStateThroughTheStore(t, dir, state)

	var got []byte
	var why NoDeviceKey
	mustLeaveUnchanged(t, filepath.Dir(dir), func() {
		got, why = ReadDeviceStaticPrivateKey(context.Background(), dir, RuntimeSupervisionNative)
	})

	want, wantWhy := wantKey(privateKey)
	if why != wantWhy || !bytes.Equal(got, want) {
		t.Fatalf("ReadDeviceStaticPrivateKey returned a key of %d bytes and %q, want the %d bytes of the stored key and %q", len(got), why, len(want), wantWhy)
	}
	if !deviceKeyReadSupported {
		return
	}
	// The key is the caller's own copy: wiping it does not reach the next
	// read.
	clear(got)
	again, why := ReadDeviceStaticPrivateKey(context.Background(), dir, RuntimeSupervisionNative)
	if why != "" || !bytes.Equal(again, privateKey) {
		t.Fatalf("a second read returned a key of %d bytes and %q, want the stored key again", len(again), why)
	}
}

// TestReadDeviceStaticPrivateKeyTouchesNothingWhereThereIsNoState pins the
// first rule of the function. A directory with no state file gives no key,
// and the function does not go on to ask which key storage the directory
// would use. That question can reach the TPM of the machine.
func TestReadDeviceStaticPrivateKeyTouchesNothingWhereThereIsNoState(t *testing.T) {
	for name, prepare := range map[string]func(t *testing.T) (dir string, exists bool){
		"no directory": func(t *testing.T) (string, bool) {
			return filepath.Join(t.TempDir(), "never-created"), false
		},
		"empty directory": func(t *testing.T) (string, bool) { return secureStateTestDir(t), true },
		"other files only": func(t *testing.T) (string, bool) {
			dir := secureStateTestDir(t)
			writeOwnerOnlyTestFile(t, dir, LocalSharesFile, []byte("{}"))
			return dir, true
		},
		"blank path": func(*testing.T) (string, bool) { return "  ", false },
	} {
		t.Run(name, func(t *testing.T) {
			clearStateEnv(t)
			dir, exists := prepare(t)
			original := ResolveKeyProvider
			ResolveKeyProvider = func(string) (string, error) {
				t.Error("the function asked which key storage a directory with no state would use")
				return "", errors.New("unexpected key storage question")
			}
			t.Cleanup(func() { ResolveKeyProvider = original })

			read := func() {
				if key, why := ReadDeviceStaticPrivateKey(context.Background(), dir, RuntimeSupervisionNative); key != nil || why != NoDeviceKeyNoState {
					t.Errorf("ReadDeviceStaticPrivateKey returned a key of %d bytes and %q, want no key and %q", len(key), why, NoDeviceKeyNoState)
				}
			}
			if !exists {
				read()
				if strings.TrimSpace(dir) != "" {
					if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
						t.Errorf("the read created %s: %v", dir, err)
					}
				}
				return
			}
			mustLeaveUnchanged(t, filepath.Dir(dir), read)
		})
	}
}

// TestReadDeviceStaticPrivateKeyChecksTheState covers state that can be read
// and must still give no key, and the pinned device id. Each case is a state
// file with one thing changed. The directory is unchanged after every read.
func TestReadDeviceStaticPrivateKeyChecksTheState(t *testing.T) {
	other, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	zeroKey, err := ecdh.X25519().NewPrivateKey(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		// change edits the state of a registered device before it is written.
		change func(state *qurl.AgentState)
		// pinned is the value of QURL_CONNECTOR_AGENT_ID; empty leaves it unset.
		pinned string
		// why is the reason for no key; empty means the key is returned.
		why NoDeviceKey
	}{
		{name: "usable state", change: func(*qurl.AgentState) {}},
		{name: "the pinned device", change: func(*qurl.AgentState) {}, pinned: "agent-device-key-test"},
		{name: "another device is pinned", change: func(*qurl.AgentState) {}, pinned: "agent-somebody-else", why: NoDeviceKeyAgentID},
		{name: "registration never finished", change: func(s *qurl.AgentState) { s.RegisteredAt = nil }, why: NoDeviceKeyNotRegistered},
		{name: "registration time is zero", change: func(s *qurl.AgentState) { s.RegisteredAt = &time.Time{} }, why: NoDeviceKeyNotRegistered},
		{
			name: "public key of another key pair", why: NoDeviceKeyInvalid,
			change: func(s *qurl.AgentState) {
				s.PublicKeyB64 = base64.StdEncoding.EncodeToString(other.PublicKey().Bytes())
			},
		},
		{name: "no public key", change: func(s *qurl.AgentState) { s.PublicKeyB64 = "" }, why: NoDeviceKeyInvalid},
		{name: "no private key", change: func(s *qurl.AgentState) { s.PrivateKeyB64 = "" }, why: NoDeviceKeyInvalid},
		{
			// What a wiped key holds. The public key in the state is the one
			// that belongs to those bytes, so the pair is consistent and only
			// the check for zero bytes can refuse it.
			name: "private key of only zero bytes", why: NoDeviceKeyInvalid,
			change: func(s *qurl.AgentState) {
				s.PrivateKeyB64 = base64.StdEncoding.EncodeToString(zeroKey.Bytes())
				s.PublicKeyB64 = base64.StdEncoding.EncodeToString(zeroKey.PublicKey().Bytes())
			},
		},
		{
			name: "private key that is too short", why: NoDeviceKeyInvalid,
			change: func(s *qurl.AgentState) { s.PrivateKeyB64 = base64.StdEncoding.EncodeToString(other.Bytes()[:31]) },
		},
		{
			// The same key bytes, written without padding. qurl-go writes
			// padded base64 only, so another form is damaged state.
			name: "private key without base64 padding", why: NoDeviceKeyInvalid,
			change: func(s *qurl.AgentState) { s.PrivateKeyB64 = strings.TrimRight(s.PrivateKeyB64, "=") },
		},
		{name: "private key that is not base64", change: func(s *qurl.AgentState) { s.PrivateKeyB64 = "not a key" }, why: NoDeviceKeyInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearStateEnv(t)
			dir := secureStateTestDir(t)
			state, privateKey := registeredDeviceState(t)
			tc.change(state)
			writeStateFile(t, dir, state)
			if tc.pinned != "" {
				t.Setenv(EnvAgentID, tc.pinned)
			}

			var got []byte
			var why NoDeviceKey
			mustLeaveUnchanged(t, filepath.Dir(dir), func() {
				got, why = ReadDeviceStaticPrivateKey(context.Background(), dir, RuntimeSupervisionNative)
			})

			want, wantWhy := wantKey(privateKey)
			if tc.why != "" {
				want, wantWhy = nil, wantNoKey(tc.why)
			}
			if why != wantWhy || !bytes.Equal(got, want) {
				t.Fatalf("ReadDeviceStaticPrivateKey returned a key of %d bytes and %q, want a key of %d bytes and %q", len(got), why, len(want), wantWhy)
			}
		})
	}
}

// TestReadDeviceStaticPrivateKeyGivesNoKeyForStateItCannotUse covers a
// directory that reads as holding state and cannot be read as a registered
// device's plaintext or TPM state. Each case gives no key and one fixed
// word, never an error, and the directory is unchanged.
func TestReadDeviceStaticPrivateKeyGivesNoKeyForStateItCannotUse(t *testing.T) {
	for _, tc := range []struct {
		name string
		// prepare fills dir. supervision is the mode the command runs with.
		prepare     func(t *testing.T, dir string)
		supervision RuntimeSupervision
		why         NoDeviceKey
	}{
		{
			name: "state file that is not JSON", supervision: RuntimeSupervisionNative, why: NoDeviceKeyUnreadable,
			prepare: func(t *testing.T, dir string) {
				writeOwnerOnlyTestFile(t, dir, AgentStateFile, []byte("not state"))
			},
		},
		{
			name: "both state files", supervision: RuntimeSupervisionNative, why: NoDeviceKeyStorage,
			prepare: func(t *testing.T, dir string) {
				state, _ := registeredDeviceState(t)
				writeStateFile(t, dir, state)
				writeOwnerOnlyTestFile(t, dir, connectoragentstate.SealedAgentStateFile, []byte(`{"provider_id":"tpm"}`))
			},
		},
		{
			// A directory of another supervision mode than the command's.
			name: "directory of an external supervisor", supervision: RuntimeSupervisionNative, why: NoDeviceKeySupervision,
			prepare: func(t *testing.T, dir string) {
				if err := EstablishExternalRuntimeMode(context.Background(), dir); err != nil {
					t.Fatal(err)
				}
				state, _ := registeredDeviceState(t)
				writeStateFile(t, dir, state)
			},
		},
		{
			name: "native directory, command of an external supervisor", supervision: RuntimeSupervisionExternal, why: NoDeviceKeySupervision,
			prepare: func(t *testing.T, dir string) {
				state, _ := registeredDeviceState(t)
				writeStateFile(t, dir, state)
			},
		},
		{
			name: "supervision mode that does not exist", supervision: RuntimeSupervision("desktop"), why: NoDeviceKeySupervision,
			prepare: func(t *testing.T, dir string) {
				state, _ := registeredDeviceState(t)
				writeStateFile(t, dir, state)
			},
		},
		{
			// State sealed by a provider that takes its key from the
			// environment, opened with no provider named.
			name: "state sealed by a provider that needs its environment", supervision: RuntimeSupervisionNative, why: NoDeviceKeyStorage,
			prepare: func(t *testing.T, dir string) {
				writeOwnerOnlyTestFile(t, dir, connectoragentstate.SealedAgentStateFile, []byte(`{"provider_id":"aws-kms"}`))
				unsetKeyProvider(t)
			},
		},
		{
			// A sealed file with no usable content, in a directory whose
			// state the TPM would open. No machine can unseal it, so the
			// answer does not depend on whether this machine has a TPM.
			name: "TPM state that cannot be unsealed", supervision: RuntimeSupervisionNative, why: NoDeviceKeyUnreadable,
			prepare: func(t *testing.T, dir string) {
				writeOwnerOnlyTestFile(t, dir, connectoragentstate.SealedAgentStateFile, []byte(`{"provider_id":"tpm"}`))
				unsetKeyProvider(t)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearStateEnv(t)
			dir := secureStateTestDir(t)
			tc.prepare(t, dir)

			mustLeaveUnchanged(t, filepath.Dir(dir), func() {
				key, why := ReadDeviceStaticPrivateKey(context.Background(), dir, tc.supervision)
				if want := wantNoKey(tc.why); key != nil || why != want {
					t.Errorf("ReadDeviceStaticPrivateKey returned a key of %d bytes and %q, want no key and %q", len(key), why, want)
				}
			})
		})
	}
}

// TestReadDeviceStaticPrivateKeyReadsAnExternalSupervisorsStateForIt pins
// the other side of the supervision check: a command that runs with the mode
// the directory has gets the key.
func TestReadDeviceStaticPrivateKeyReadsAnExternalSupervisorsStateForIt(t *testing.T) {
	clearStateEnv(t)
	dir := secureStateTestDir(t)
	if err := EstablishExternalRuntimeMode(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	state, privateKey := registeredDeviceState(t)
	writeStateFile(t, dir, state)

	var got []byte
	var why NoDeviceKey
	mustLeaveUnchanged(t, filepath.Dir(dir), func() {
		got, why = ReadDeviceStaticPrivateKey(context.Background(), dir, RuntimeSupervisionExternal)
	})
	if want, wantWhy := wantKey(privateKey); why != wantWhy || !bytes.Equal(got, want) {
		t.Fatalf("ReadDeviceStaticPrivateKey returned a key of %d bytes and %q, want a key of %d bytes and %q", len(got), why, len(want), wantWhy)
	}
}

// TestReadDeviceStaticPrivateKeyReadsOnlyTheFileAndTheTPM pins which key
// storages the function reads from. The answer to "which storage does this
// directory use" is given by the seam, so the test does not depend on what
// the machine has. Only the plaintext file and the TPM lead to an open. Any
// other answer gives no key, and the state is not opened at all: a provider
// of that kind can call a service of another company to open it.
//
// The directory holds usable plaintext state in every case. So a case that
// gives no key shows that the state was not opened: an open would have
// returned the key. The TPM is not a case here, because the seam cannot make
// a directory hold TPM state.
// TestReadDeviceStaticPrivateKeyGivesNoKeyForStateItCannotUse has that case,
// with a sealed file and the real answer.
func TestReadDeviceStaticPrivateKeyReadsOnlyTheFileAndTheTPM(t *testing.T) {
	if !deviceKeyReadSupported {
		t.Skip("the read is not supported on this platform, so no key storage is ever asked for")
	}
	for provider, why := range map[string]NoDeviceKey{
		connectoragentstate.KeyProviderFile:                 "",
		connectoragentstate.KeyProviderLocalKey:             NoDeviceKeyStorage,
		connectoragentstate.KeyProviderAWSKMS:               NoDeviceKeyStorage,
		connectoragentstate.KeyProviderGCPKMS:               NoDeviceKeyStorage,
		connectoragentstate.KeyProviderAWSNitro:             NoDeviceKeyStorage,
		connectoragentstate.KeyProviderGCPConfidentialSpace: NoDeviceKeyStorage,
		"":       NoDeviceKeyStorage,
		"File":   NoDeviceKeyStorage,
		"future": NoDeviceKeyStorage,
	} {
		t.Run("provider "+provider, func(t *testing.T) {
			clearStateEnv(t)
			dir := secureStateTestDir(t)
			state, privateKey := registeredDeviceState(t)
			writeStateFile(t, dir, state)
			original := ResolveKeyProvider
			ResolveKeyProvider = func(string) (string, error) { return provider, nil }
			t.Cleanup(func() { ResolveKeyProvider = original })

			mustLeaveUnchanged(t, filepath.Dir(dir), func() {
				key, got := ReadDeviceStaticPrivateKey(context.Background(), dir, RuntimeSupervisionNative)
				if got != why || (why == "") != bytes.Equal(key, privateKey) || (why != "" && key != nil) {
					t.Errorf("ReadDeviceStaticPrivateKey returned a key of %d bytes and %q, want %q", len(key), got, why)
				}
			})
		})
	}

	t.Run("the question fails", func(t *testing.T) {
		clearStateEnv(t)
		dir := secureStateTestDir(t)
		state, _ := registeredDeviceState(t)
		writeStateFile(t, dir, state)
		original := ResolveKeyProvider
		ResolveKeyProvider = func(string) (string, error) { return "", errors.New("the TPM does not answer") }
		t.Cleanup(func() { ResolveKeyProvider = original })

		if key, why := ReadDeviceStaticPrivateKey(context.Background(), dir, RuntimeSupervisionNative); key != nil || why != NoDeviceKeyStorage {
			t.Errorf("ReadDeviceStaticPrivateKey returned a key of %d bytes and %q, want no key and %q", len(key), why, NoDeviceKeyStorage)
		}
	})
}

// TestReadDeviceStaticPrivateKeyEndsWithItsContext pins that the read obeys
// the context it is given: a context that has ended gives no key.
func TestReadDeviceStaticPrivateKeyEndsWithItsContext(t *testing.T) {
	clearStateEnv(t)
	dir := secureStateTestDir(t)
	state, _ := registeredDeviceState(t)
	writeStateFile(t, dir, state)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	mustLeaveUnchanged(t, filepath.Dir(dir), func() {
		key, why := ReadDeviceStaticPrivateKey(ctx, dir, RuntimeSupervisionNative)
		if want := wantNoKey(NoDeviceKeyUnreadable); key != nil || why != want {
			t.Errorf("ReadDeviceStaticPrivateKey returned a key of %d bytes and %q, want no key and %q", len(key), why, want)
		}
	})
}

// openLoadSaveClose does to the state in dir what a command does that opens
// the state to use it and to change it: it opens the store, loads the state,
// saves it again and closes the store. The save replaces the state file.
func openLoadSaveClose(dir string) error {
	store, err := Open(dir)
	if err != nil {
		return fmt.Errorf("open the state store: %w", err)
	}
	sdkStore, err := store.Handoff()
	if err != nil {
		return errors.Join(fmt.Errorf("hand off the state store: %w", err), store.Close())
	}
	loaded, err := sdkStore.LoadAgentState(context.Background())
	if err != nil || loaded == nil {
		return errors.Join(fmt.Errorf("load the device state: state present %t: %w", loaded != nil, err), store.Close())
	}
	if err := sdkStore.SaveAgentState(context.Background(), loaded); err != nil {
		return errors.Join(fmt.Errorf("save the device state: %w", err), store.Close())
	}
	if err := store.Close(); err != nil {
		return fmt.Errorf("close the state store: %w", err)
	}
	return nil
}

// TestReadDeviceStaticPrivateKeyNextToTheStateStore runs the read at the same
// time as the store that opens the same state to change it. `qurl get` needs
// the two to be safe together: a read of the key that is slow can still run
// when the command goes on to its share request, and that request opens the
// state through the store.
//
// The read takes no lock and writes nothing, and the store replaces the state
// file by a rename. So neither can break the other:
//
//   - The store opens, loads, saves and closes without an error, whatever a
//     read does at that moment.
//   - A read gives the key, or no key and the word "unreadable". That word is
//     a read that met a save: the file it had opened was replaced, so it does
//     not use what it read. A read never gives another key, a part of a key,
//     or another word.
func TestReadDeviceStaticPrivateKeyNextToTheStateStore(t *testing.T) {
	if !deviceKeyReadSupported {
		t.Skip("the read opens no state on this platform, so nothing can run next to it")
	}

	// The store is open and has loaded the state, as it is for the length of
	// a share request. A read at that moment gives the key and leaves the
	// directory as it is. The save after it replaces the state file, so the
	// read left nothing behind that stops a save. The read after the save
	// gives the key again.
	t.Run("a read while the store is open", func(t *testing.T) {
		clearStateEnv(t)
		dir := secureStateTestDir(t)
		state, privateKey := registeredDeviceState(t)
		saveStateThroughTheStore(t, dir, state)

		store, err := Open(dir)
		if err != nil {
			t.Fatalf("open the state store: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		sdkStore, err := store.Handoff()
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := sdkStore.LoadAgentState(context.Background())
		if err != nil || loaded == nil {
			t.Fatalf("load the device state: state present %t, error %v", loaded != nil, err)
		}

		mustLeaveUnchanged(t, filepath.Dir(dir), func() {
			if key, why := ReadDeviceStaticPrivateKey(context.Background(), dir, RuntimeSupervisionNative); why != "" || !bytes.Equal(key, privateKey) {
				t.Errorf("the read next to the open store returned a key of %d bytes and %q, want the stored key", len(key), why)
			}
		})
		if err := sdkStore.SaveAgentState(context.Background(), loaded); err != nil {
			t.Fatalf("the save after the read failed: %v", err)
		}
		if key, why := ReadDeviceStaticPrivateKey(context.Background(), dir, RuntimeSupervisionNative); why != "" || !bytes.Equal(key, privateKey) {
			t.Errorf("the read after the save returned a key of %d bytes and %q, want the stored key", len(key), why)
		}
		if err := store.Close(); err != nil {
			t.Errorf("close the state store: %v", err)
		}
	})

	// Reads and saves with no order between them. Some goroutines read the
	// key again and again. This one opens the store, loads, saves and closes,
	// several times. Every save replaces the state file under the reads.
	t.Run("reads and saves at the same time", func(t *testing.T) {
		clearStateEnv(t)
		dir := secureStateTestDir(t)
		state, privateKey := registeredDeviceState(t)
		saveStateThroughTheStore(t, dir, state)

		const readers, saves = 4, 20
		var keys, unreadable atomic.Int64
		var mu sync.Mutex
		var other []string
		stop := make(chan struct{})
		var reading sync.WaitGroup
		for range readers {
			reading.Go(func() {
				for {
					select {
					case <-stop:
						return
					default:
					}
					key, why := ReadDeviceStaticPrivateKey(context.Background(), dir, RuntimeSupervisionNative)
					switch {
					case why == "" && bytes.Equal(key, privateKey):
						keys.Add(1)
					case key == nil && why == NoDeviceKeyUnreadable:
						unreadable.Add(1)
					default:
						mu.Lock()
						other = append(other, fmt.Sprintf("a key of %d bytes and %q", len(key), why))
						mu.Unlock()
					}
				}
			})
		}
		var storeErr error
		for range saves {
			if storeErr = openLoadSaveClose(dir); storeErr != nil {
				break
			}
		}
		close(stop)
		reading.Wait()

		if storeErr != nil {
			t.Errorf("the store failed next to the reads: %v", storeErr)
		}
		if len(other) != 0 {
			t.Errorf("%d read(s) gave something else than the key or \"unreadable\", the first: %s", len(other), other[0])
		}
		if keys.Load() == 0 {
			t.Error("no read gave the key, so this run shows nothing about a read next to a save")
		}
		t.Logf("%d reads gave the key and %d met a save, next to %d saves", keys.Load(), unreadable.Load(), saves)

		// The state is whole after all of it.
		if key, why := ReadDeviceStaticPrivateKey(context.Background(), dir, RuntimeSupervisionNative); why != "" || !bytes.Equal(key, privateKey) {
			t.Errorf("the read after the last save returned a key of %d bytes and %q, want the stored key", len(key), why)
		}
	})
}
