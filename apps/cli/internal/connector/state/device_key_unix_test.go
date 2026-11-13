//go:build unix

package state

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
	"golang.org/x/sys/unix"
)

// changeStamp returns what the file system records about path beyond what a
// portable snapshot holds: the inode number and the change time. The inode
// number shows a file that was replaced by another with the same bytes. The
// change time moves on every chmod, chown, rename and write, also when the
// modification time is put back.
func changeStamp(t *testing.T, path string) string {
	t.Helper()
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fmt.Sprintf("inode=%d links=%d changed=%d.%09d", stat.Ino, stat.Nlink, stat.Ctim.Sec, stat.Ctim.Nsec)
}

// TestReadDeviceStaticPrivateKeyDoesNotRepairWhatItCannotRead covers state
// whose permissions the read-only open refuses. The command that creates
// state repairs a directory with loose permissions. This function must not:
// it gives no key and leaves every permission as it found it.
func TestReadDeviceStaticPrivateKeyDoesNotRepairWhatItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the root user passes permission checks whatever the permission bits say")
	}
	for name, loosen := range map[string]func(t *testing.T, dir string){
		"directory other users can read": func(t *testing.T, dir string) {
			if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // The loose mode is the case under test.
				t.Fatal(err)
			}
		},
		"directory other users can write": func(t *testing.T, dir string) {
			if err := os.Chmod(dir, 0o777); err != nil { //nolint:gosec // The loose mode is the case under test.
				t.Fatal(err)
			}
		},
		"state file other users can read": func(t *testing.T, dir string) {
			if err := os.Chmod(filepath.Join(dir, AgentStateFile), 0o644); err != nil { //nolint:gosec // The loose mode is the case under test.
				t.Fatal(err)
			}
		},
		"state file its owner cannot read": func(t *testing.T, dir string) {
			if err := os.Chmod(filepath.Join(dir, AgentStateFile), 0); err != nil {
				t.Fatal(err)
			}
		},
		"state file that is a symbolic link": func(t *testing.T, dir string) {
			path := filepath.Join(dir, AgentStateFile)
			moved := filepath.Join(filepath.Dir(dir), "state-moved-away.json")
			if err := os.Rename(path, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, path); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			clearStateEnv(t)
			dir := secureStateTestDir(t)
			state, _ := registeredDeviceState(t)
			saveStateThroughTheStore(t, dir, state)
			loosen(t, dir)

			mustLeaveUnchanged(t, filepath.Dir(dir), func() {
				key, why := ReadDeviceStaticPrivateKey(context.Background(), dir, RuntimeSupervisionNative)
				if want := wantNoKey(NoDeviceKeyUnreadable); key != nil || why != want {
					t.Errorf("ReadDeviceStaticPrivateKey returned a key of %d bytes and %q, want no key and %q", len(key), why, want)
				}
			})
		})
	}
}

// TestReadDeviceStaticPrivateKeyNeverUsesAKeyFromTheEnvironment covers the
// key storages that take their key from the environment of the process. The
// state here is really sealed with such a key, and the key that opens it is
// offered to the process again for the read. The function still gives no
// key, and it does not take the offered key: the descriptor that holds it is
// open and unread afterwards.
//
// The command runs for an external supervisor, because a directory with
// state of this kind is refused before that for every other command.
func TestReadDeviceStaticPrivateKeyNeverUsesAKeyFromTheEnvironment(t *testing.T) {
	clearStateEnv(t)
	dir := secureStateTestDir(t)
	if err := EstablishExternalRuntimeMode(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	wrappingKey := bytes.Repeat([]byte{0x2a}, 32)
	t.Setenv(connectoragentstate.EnvKeyProvider, connectoragentstate.KeyProviderLocalKey)
	t.Setenv(connectoragentstate.EnvLocalKeyFD, localKeyFD(t, wrappingKey))
	state, _ := registeredDeviceState(t)
	saveStateThroughTheStore(t, dir, state)
	if _, err := os.Lstat(filepath.Join(dir, connectoragentstate.SealedAgentStateFile)); err != nil {
		t.Fatalf("the fixture did not seal its state: %v", err)
	}

	// The same key again, on a new descriptor, as a supervisor would hand it
	// to the next process.
	offered := localKeyFD(t, wrappingKey)
	t.Setenv(connectoragentstate.EnvLocalKeyFD, offered)
	descriptor, err := strconv.Atoi(offered)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(descriptor) })

	for _, tc := range []struct {
		name string
		// named says that LAYERV_KEY_PROVIDER still names the provider.
		named bool
		// answer replaces the answer to "which key storage does this
		// directory use"; empty keeps the real answer.
		answer string
	}{
		{name: "the provider is named", named: true},
		{name: "no provider is named"},
		// The guard in front of that question: a provider that takes its key
		// from the environment is refused whatever the answer is. Without
		// it, an answer of "TPM" would lead to an open with the key that the
		// environment offers.
		{name: "the provider is named and the answer is the TPM", named: true, answer: connectoragentstate.KeyProviderTPM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.named {
				unsetKeyProvider(t)
			}
			if tc.answer != "" {
				original := ResolveKeyProvider
				ResolveKeyProvider = func(string) (string, error) { return tc.answer, nil }
				t.Cleanup(func() { ResolveKeyProvider = original })
			}
			mustLeaveUnchanged(t, filepath.Dir(dir), func() {
				key, why := ReadDeviceStaticPrivateKey(context.Background(), dir, RuntimeSupervisionExternal)
				if want := wantNoKey(NoDeviceKeyStorage); key != nil || why != want {
					t.Errorf("ReadDeviceStaticPrivateKey returned a key of %d bytes and %q, want no key and %q", len(key), why, want)
				}
			})
		})
	}

	// The offered key is still in its pipe: the descriptor is open, and all
	// 32 bytes can be read from it now. So nothing read them before.
	if err := unix.SetNonblock(descriptor, true); err != nil {
		t.Fatalf("the descriptor with the offered key was closed: %v", err)
	}
	left := make([]byte, len(wrappingKey)+1)
	n, err := unix.Read(descriptor, left)
	if err != nil || !bytes.Equal(left[:max(n, 0)], wrappingKey) {
		t.Errorf("read %d bytes of the offered key (error %v), want all %d: the function took the key", n, err, len(wrappingKey))
	}
}
