//go:build unix

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
	"golang.org/x/sys/unix"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

// Match the supervisor's inherited pipe without replacing the test process's fd3.
func localIdentityKeyFD(t *testing.T, key []byte, minimum int) string {
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
	fd, err := unix.FcntlInt(uintptr(fds[0]), unix.F_DUPFD_CLOEXEC, minimum)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Close(fds[0]); err != nil {
		t.Fatal(err)
	}
	// Connector consumes/closes this descriptor and retains its provider key.
	return strconv.Itoa(fd)
}

func TestWhoamiLocalReadsSealedNamespaceOnlyWithMatchingWrappingKey(t *testing.T) {
	for i, mode := range []string{"matching", "missing", "wrong"} {
		t.Run(mode, func(t *testing.T) {
			key := bytes.Repeat([]byte{0x61}, 32)
			fd := localIdentityKeyFD(t, key, 700+i*2)
			t.Setenv(connectoragentstate.EnvKeyProvider, connectoragentstate.KeyProviderLocalKey)
			t.Setenv(connectoragentstate.EnvLocalKeyFD, fd)
			dir, err := filepath.EvalSymlinks(connectorStateTestDir(t))
			if err != nil {
				t.Fatal(err)
			}
			store, err := connectoragentstate.NewSDKStore(dir, "")
			if err != nil {
				t.Fatal(err)
			}
			sdk, err := store.Handoff()
			if err != nil {
				t.Fatal(err)
			}
			state := bootstrapRegisteredState(t)
			if err := sdk.SaveAgentState(context.Background(), state); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, connectoragentstate.SealedAgentStateFile)
			// #nosec G304 -- fixed sealed filename inside test-owned namespace.
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			beforeInfo, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "missing" {
				t.Setenv(connectoragentstate.EnvLocalKeyFD, "")
			}
			if mode == "wrong" {
				t.Setenv(connectoragentstate.EnvLocalKeyFD, localIdentityKeyFD(t, bytes.Repeat([]byte{0x62}, 32), 701+i*2))
			}
			srv := apitest.NewServer(t)
			res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "whoami", "--local", "-o", "json"}, shareStateDir: dir})
			if mode == "matching" {
				var got map[string]any
				if res.code != 0 || json.Unmarshal(res.stdout.Bytes(), &got) != nil || got["agent_id"] != state.AgentID || got["device_key_id"] != state.DeviceAPIKeyID {
					t.Fatalf("sealed local identity failed: exit=%d", res.code)
				}
			} else if res.code == 0 || res.stdout.Len() != 0 {
				t.Fatal("invalid wrapping key exposed local identity")
			}
			// #nosec G304 -- fixed sealed filename inside test-owned namespace.
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			afterInfo, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) || !beforeInfo.ModTime().Equal(afterInfo.ModTime()) || len(srv.Requests()) != 0 {
				t.Fatal("local inspection modified state or contacted API")
			}
			if _, err := os.Stat(filepath.Join(dir, connectorstate.AgentStateFile)); !os.IsNotExist(err) {
				t.Fatal("local inspection created plaintext state")
			}
			if strings.Contains(res.stdout.String()+res.stderr.String(), state.PrivateKeyB64) || strings.Contains(res.stdout.String()+res.stderr.String(), state.DeviceAPIKey) {
				t.Fatal("local inspection disclosed secret")
			}
		})
	}
}
