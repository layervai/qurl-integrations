package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"

	"github.com/layervai/qurl-integrations/apps/cli/internal/config"
)

// TestRuntimeSupervisionValuesMatchConfigVocabulary pins the supervision
// modes to the daemon_supervision vocabulary the config layer validates files
// against; state imports config for this test, never the reverse.
func TestRuntimeSupervisionValuesMatchConfigVocabulary(t *testing.T) {
	if want := config.DaemonSupervisions(); !slices.Equal(RuntimeSupervisionValues(), want) {
		t.Errorf("RuntimeSupervisionValues() = %v, want config's %v", RuntimeSupervisionValues(), want)
	}
	values := RuntimeSupervisionValues()
	if len(values) != 2 || values[0] != string(DefaultRuntimeSupervision) {
		t.Fatalf("RuntimeSupervisionValues() = %v, want the default %q first", values, DefaultRuntimeSupervision)
	}
}

func TestParseRuntimeSupervisionAcceptsOnlyCanonicalSpellings(t *testing.T) {
	for value, want := range map[string]RuntimeSupervision{"native": RuntimeSupervisionNative, "external": RuntimeSupervisionExternal} {
		got, err := ParseRuntimeSupervision(value)
		if err != nil || got != want {
			t.Errorf("ParseRuntimeSupervision(%q) = (%q, %v), want %q", value, got, err, want)
		}
	}
	// The mode decides whether a command may install a background job, so a
	// near-miss is refused rather than normalized.
	for _, value := range []string{"", " native", "Native", "EXTERNAL", "desktop", "external "} {
		got, err := ParseRuntimeSupervision(value)
		if err == nil || got != "" || !strings.Contains(err.Error(), "native or external") {
			t.Errorf("ParseRuntimeSupervision(%q) = (%q, %v), want a rejection naming both modes", value, got, err)
		}
	}
}

func TestEstablishExternalRuntimeModeFreshAndIdempotent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "external-state")
	if err := EstablishExternalRuntimeMode(context.Background(), dir); err != nil {
		t.Fatal(err)
	}

	mode, err := ReadRuntimeSupervision(dir)
	if err != nil || mode != RuntimeSupervisionExternal {
		t.Fatalf("ReadRuntimeSupervision() = (%q, %v), want external", mode, err)
	}
	if err := RequireRuntimeSupervision(dir, RuntimeSupervisionExternal); err != nil {
		t.Fatalf("RequireRuntimeSupervision(external): %v", err)
	}
	err = RequireRuntimeSupervision(dir, RuntimeSupervisionNative)
	const wantMessage = `runtime supervision is "external", not "native"; run this command with --supervision external`
	if !errors.Is(err, ErrRuntimeSupervision) || err.Error() != wantMessage {
		t.Fatalf("RequireRuntimeSupervision(native) = %v, want ErrRuntimeSupervision with %q", err, wantMessage)
	}

	path := filepath.Join(dir, RuntimeModeFile)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !isWindows(t) && before.Mode().Perm() != 0o600 {
		t.Fatalf("runtime policy mode = %04o, want 0600", before.Mode().Perm())
	}
	data, err := os.ReadFile(path) // #nosec G304 -- test-owned policy path.
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), `{"schema_version":1,"supervision":"external"}`; got != want {
		t.Fatalf("runtime policy = %q, want exact %q", got, want)
	}
	if err := EstablishExternalRuntimeMode(context.Background(), dir); err != nil {
		t.Fatalf("idempotent establishment: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("idempotent establishment replaced the immutable policy file")
	}
	if _, err := os.Lstat(filepath.Join(dir, AgentStateFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("external policy created plaintext AgentState: %v", err)
	}
}

func TestReadRuntimeSupervisionTreatsAbsenceAsNative(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	mode, err := ReadRuntimeSupervision(dir)
	if err != nil || mode != RuntimeSupervisionNative {
		t.Fatalf("missing namespace mode = (%q, %v), want native", mode, err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only policy check created state directory: %v", err)
	}
	if err := RequireRuntimeSupervision(dir, RuntimeSupervisionNative); err != nil {
		t.Fatalf("RequireRuntimeSupervision(native): %v", err)
	}
	err = RequireRuntimeSupervision(dir, RuntimeSupervisionExternal)
	const wantMessage = `runtime supervision is "native", not "external"; run this command with --supervision native`
	if !errors.Is(err, ErrRuntimeSupervision) || err.Error() != wantMessage {
		t.Fatalf("absent namespace against external policy = %v, want ErrRuntimeSupervision with %q", err, wantMessage)
	}
	if err := RequireRuntimeSupervision(dir, RuntimeSupervision("desktop")); err == nil || errors.Is(err, ErrRuntimeSupervision) {
		t.Fatalf("unknown expected supervision = %v, want a parse rejection rather than a policy mismatch", err)
	}
}

func TestEstablishExternalRuntimeModeRejectsPreexistingLifecycleState(t *testing.T) {
	for _, name := range []string{
		AgentStateFile,
		connectoragentstate.SealedAgentStateFile,
		LocalSharesFile,
		ConnectorResourcesFile,
	} {
		t.Run(name, func(t *testing.T) {
			dir := secureStateTestDir(t)
			// Seed through the package's own writer so the entry carries the
			// owner-only ACL on Windows; an inherited-ACL agent_state.json is
			// refused by the directory capability before the freshness check.
			if err := replaceConnectorResources(dir, filepath.Join(dir, name), []byte("occupied")); err != nil {
				t.Fatal(err)
			}
			err := EstablishExternalRuntimeMode(context.Background(), dir)
			if err == nil || !strings.Contains(err.Error(), "not a fresh external namespace") {
				t.Fatalf("EstablishExternalRuntimeMode with %s = %v", name, err)
			}
			if _, statErr := os.Lstat(filepath.Join(dir, RuntimeModeFile)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("rejected namespace gained policy file: %v", statErr)
			}
			if mode, err := ReadRuntimeSupervision(dir); err != nil || mode != RuntimeSupervisionNative {
				t.Fatalf("rejected namespace mode = (%q, %v), want native", mode, err)
			}
		})
	}
}

func TestRuntimeModeStrictFileContract(t *testing.T) {
	cases := []struct {
		name string
		data string
		mode os.FileMode
	}{
		{name: "malformed", data: `{`, mode: 0o600},
		{name: "empty", data: ``, mode: 0o600},
		{name: "duplicate field", data: `{"schema_version":1,"schema_version":1,"supervision":"external"}`, mode: 0o600},
		{name: "unknown field", data: `{"schema_version":1,"supervision":"external","future":true}`, mode: 0o600},
		{name: "wrong schema", data: `{"schema_version":2,"supervision":"external"}`, mode: 0o600},
		{name: "wrong supervision", data: `{"schema_version":1,"supervision":"native"}`, mode: 0o600},
		{name: "trailing json", data: `{"schema_version":1,"supervision":"external"}{}`, mode: 0o600},
		{name: "group readable", data: `{"schema_version":1,"supervision":"external"}`, mode: 0o640},
		{name: "oversize", data: strings.Repeat(" ", runtimeModeMaxBytes+1), mode: 0o600},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.mode != 0o600 && isWindows(t) {
				t.Skip("POSIX mode bits are not the Windows file contract")
			}
			dir := secureStateTestDir(t)
			path := filepath.Join(dir, RuntimeModeFile)
			if err := os.WriteFile(path, []byte(tc.data), tc.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadRuntimeSupervision(dir); err == nil {
				t.Fatal("unsafe or incompatible runtime policy was accepted")
			}
			// A corrupt marker is never silently repaired or relabelled.
			if err := EstablishExternalRuntimeMode(context.Background(), dir); err == nil {
				t.Fatal("establishment over an invalid runtime policy succeeded")
			}
		})
	}

	t.Run("symlink", func(t *testing.T) {
		if isWindows(t) {
			t.Skip("symlink creation needs a privilege on Windows")
		}
		dir := secureStateTestDir(t)
		target := filepath.Join(t.TempDir(), "policy")
		if err := os.WriteFile(target, []byte(`{"schema_version":1,"supervision":"external"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, RuntimeModeFile)); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadRuntimeSupervision(dir); err == nil {
			t.Fatal("symlink runtime policy was accepted")
		}
	})

	t.Run("directory", func(t *testing.T) {
		dir := secureStateTestDir(t)
		if err := os.Mkdir(filepath.Join(dir, RuntimeModeFile), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadRuntimeSupervision(dir); err == nil {
			t.Fatal("directory runtime policy was accepted")
		}
	})
}

func TestEstablishExternalRuntimeModeSerializesConcurrentCreation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "concurrent")
	start := make(chan struct{})
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			errs <- EstablishExternalRuntimeMode(context.Background(), dir)
		}()
	}
	ready.Wait()
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent establishment: %v", err)
		}
	}
	if mode, err := ReadRuntimeSupervision(dir); err != nil || mode != RuntimeSupervisionExternal {
		t.Fatalf("concurrent policy result = (%q, %v)", mode, err)
	}
}
