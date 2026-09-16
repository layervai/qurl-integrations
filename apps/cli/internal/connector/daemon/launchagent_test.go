package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	connectorservice "github.com/layervai/qurl-connector/pkg/service"
	qurl "github.com/layervai/qurl-go/qurl"

	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

const testHubKey = "CQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

func testHubResolver() (qurl.HubBootstrap, error) {
	return qurl.HubBootstrap{Host: "hub.sandbox.layerv.xyz", Port: 443, ServerPublicKeyB64: testHubKey}, nil
}

type recordingJobManager struct {
	jobs        []connectorservice.UserJob
	replaced    []connectorservice.UserJob
	status      *connectorservice.ServiceStatus
	statusErr   error
	statusCalls int
}

func (m *recordingJobManager) Ensure(job connectorservice.UserJob) error { //nolint:gocritic // interface requires a value.
	m.jobs = append(m.jobs, job)
	return nil
}
func (m *recordingJobManager) Replace(job connectorservice.UserJob) error { //nolint:gocritic // interface requires a value.
	m.replaced = append(m.replaced, job)
	return nil
}
func (*recordingJobManager) Remove(string) error { return nil }
func (m *recordingJobManager) Status(string) (connectorservice.ServiceStatus, error) {
	m.statusCalls++
	if m.statusErr != nil {
		return connectorservice.ServiceStatus{}, m.statusErr
	}
	if m.status != nil {
		return *m.status, nil
	}
	return connectorservice.ServiceStatus{Installed: true, Running: true}, nil
}

func TestJobControllerAbsentOwnerPersistsStableInstalledCommandPath(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "bin", "qurl")
	manager := &recordingJobManager{}
	controller := newTestJobController(t, filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.4.0", "https://api.sandbox.layerv.xyz", GroupModeSingle, testHubResolver)
	controller.Manager = manager
	controller.InvocationPath = "qurl"
	controller.LookPath = func(name string) (string, error) {
		if name != "qurl" {
			return "", errors.New("unexpected lookup")
		}
		return binaryPath, nil
	}
	controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) {
		return IPCStatus{}, false, nil
	}
	reloads := 0
	controller.Reload = func(context.Context) (bool, error) { reloads++; return true, nil }
	if err := controller.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(manager.jobs) != 1 || len(manager.replaced) != 0 || manager.statusCalls != 0 || reloads != 0 {
		t.Fatalf("absent owner manager ensure/replace/status/reload = %d/%d/%d/%d, want 1/0/0/0", len(manager.jobs), len(manager.replaced), manager.statusCalls, reloads)
	}
	for _, job := range manager.jobs {
		if job.BinaryPath != binaryPath {
			t.Fatalf("binary path = %q, want stable installed command %q", job.BinaryPath, binaryPath)
		}
		wantArguments := make([]string, 0, 20)
		wantArguments = append(wantArguments,
			"--endpoint", "https://api.sandbox.layerv.xyz", "daemon", "run", "--state-dir", filepath.Join(dir, "state"),
			"--runtime-dir", filepath.Dir(controller.IPC.SocketPath), "--job-version", "5/2.4.0", "--share-group-mode", "single",
			"--hub-host", "hub.sandbox.layerv.xyz", "--hub-port", "443",
			"--hub-server-public-key-b64", testHubKey,
			"--supervision", "native",
		)
		wantArguments = append(wantArguments, daemonJobLogArguments(
			filepath.Join(dir, "logs", "share-daemon.log"), filepath.Join(dir, "logs", "share-daemon.err.log"))...)
		if got := job.Arguments; !slices.Equal(got, wantArguments) {
			t.Fatalf("ProgramArguments = %#v", got)
		}
		if job.Umask != 0o077 {
			t.Fatalf("launchd Umask = %#o, want 0077", job.Umask)
		}
	}
}

func TestJobControllerPersistsCurrentArtifactInsteadOfOldQURLOnPath(t *testing.T) {
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	absoluteBinary := filepath.Join(dir, "candidate", "qurl")
	relativeInvocation := filepath.Join("test-artifacts", "candidate", "qurl")
	oldBinary := filepath.Join(dir, "old-on-path", "qurl")

	for _, test := range []struct {
		name       string
		invocation string
		want       string
	}{
		{name: "absolute artifact", invocation: absoluteBinary, want: absoluteBinary},
		{name: "relative artifact", invocation: relativeInvocation, want: filepath.Join(workingDir, relativeInvocation)},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := &recordingJobManager{}
			controller := newTestJobController(t, filepath.Join(dir, "state", test.name), filepath.Join(dir, "logs", test.name),
				"2.5.0", "https://api.sandbox.layerv.xyz", GroupModeSingle, testHubResolver)
			controller.Manager = manager
			controller.InvocationPath = test.invocation
			controller.LookPath = func(name string) (string, error) {
				if name == "qurl" {
					return oldBinary, nil
				}
				t.Fatalf("path-bearing invocation unexpectedly searched PATH for %q", name)
				return "", nil
			}
			controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) {
				return IPCStatus{}, false, nil
			}
			controller.Reload = func(context.Context) (bool, error) {
				t.Fatal("reload ran without a live daemon")
				return false, nil
			}

			if err := controller.Ensure(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(manager.jobs) != 1 || manager.jobs[0].BinaryPath != test.want {
				t.Fatalf("installed binary = %#v, want current artifact %q", manager.jobs, test.want)
			}
			if manager.jobs[0].BinaryPath == oldBinary {
				t.Fatalf("installed old PATH binary %q", oldBinary)
			}
		})
	}
}

func TestJobControllerResolvesExactBareInvocationName(t *testing.T) {
	dir := t.TempDir()
	currentBinary := filepath.Join(dir, "candidate", "qurl-candidate")
	manager := &recordingJobManager{}
	controller := newTestJobController(t, filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.5.0", "https://api.sandbox.layerv.xyz", GroupModeSingle, testHubResolver)
	controller.Manager = manager
	controller.InvocationPath = "qurl-candidate"
	controller.LookPath = func(name string) (string, error) {
		if name != "qurl-candidate" {
			t.Fatalf("looked up %q instead of the current invocation name", name)
		}
		return currentBinary, nil
	}
	controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) { return IPCStatus{}, false, nil }
	controller.Reload = func(context.Context) (bool, error) { t.Fatal("unexpected reload"); return false, nil }

	if err := controller.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(manager.jobs) != 1 || manager.jobs[0].BinaryPath != currentBinary {
		t.Fatalf("installed jobs = %#v, want exact invoked command %q", manager.jobs, currentBinary)
	}
}

func TestJobControllerRejectsNonCanonicalInvocationPath(t *testing.T) {
	dir := t.TempDir()
	manager := &recordingJobManager{}
	controller := newTestJobController(t, filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.5.0", "https://api.sandbox.layerv.xyz", GroupModeSingle, testHubResolver)
	controller.Manager = manager
	controller.InvocationPath = " qurl"
	controller.LookPath = func(string) (string, error) {
		t.Fatal("non-canonical invocation searched PATH")
		return "", nil
	}
	controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) { return IPCStatus{}, false, nil }
	controller.Reload = func(context.Context) (bool, error) { t.Fatal("unexpected reload"); return false, nil }

	err := controller.Ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invocation path is empty or non-canonical") {
		t.Fatalf("Ensure error = %v, want non-canonical invocation rejection", err)
	}
	if len(manager.jobs) != 0 || len(manager.replaced) != 0 {
		t.Fatalf("invalid invocation installed jobs: ensure=%d replace=%d", len(manager.jobs), len(manager.replaced))
	}
}

func TestJobControllerCompatibleForegroundOwnerReloadsWithoutNativeManager(t *testing.T) {
	dir := t.TempDir()
	manager := &recordingJobManager{}
	controller := newTestJobController(t, filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.4.0", "https://api.sandbox.layerv.xyz", GroupModeSingle, func() (qurl.HubBootstrap, error) {
		t.Fatal("Hub resolution ran for a compatible live owner")
		return qurl.HubBootstrap{}, nil
	})
	controller.Manager = manager
	controller.LookPath = func(string) (string, error) {
		t.Fatal("qurl path lookup ran for a compatible live owner")
		return "", nil
	}
	controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) {
		return IPCStatus{JobVersion: "5/2.4.0"}, true, nil
	}
	reloads := 0
	controller.Reload = func(context.Context) (bool, error) {
		reloads++
		return true, nil
	}
	if err := controller.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(manager.jobs) != 0 || len(manager.replaced) != 0 || manager.statusCalls != 0 || reloads != 1 {
		t.Fatalf("compatible owner manager ensure/replace/status/reload = %d/%d/%d/%d, want 0/0/0/1", len(manager.jobs), len(manager.replaced), manager.statusCalls, reloads)
	}
}

func TestJobControllerInstallsWhenCompatibleOwnerExitsBeforeReload(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "bin", "qurl")
	manager := &recordingJobManager{}
	controller := newTestJobController(t, filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.4.0", "https://api.sandbox.layerv.xyz", GroupModeSingle, testHubResolver)
	controller.Manager = manager
	controller.InvocationPath = "qurl"
	controller.LookPath = func(string) (string, error) { return binaryPath, nil }
	controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) {
		return IPCStatus{JobVersion: "5/2.4.0"}, true, nil
	}
	reloads := 0
	controller.Reload = func(context.Context) (bool, error) {
		reloads++
		return false, nil
	}
	if err := controller.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(manager.jobs) != 1 || len(manager.replaced) != 0 || manager.statusCalls != 0 || reloads != 1 {
		t.Fatalf("exited owner manager ensure/replace/status/reload = %d/%d/%d/%d, want 1/0/0/1", len(manager.jobs), len(manager.replaced), manager.statusCalls, reloads)
	}
}

func TestJobControllerVersionChangeReloadsDefinitionInsteadOfLiveIPC(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "bin", "qurl")
	manager := &recordingJobManager{}
	controller := newTestJobController(t, filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.5.0", "https://api.sandbox.layerv.xyz", GroupModeSingle, testHubResolver)
	controller.Manager = manager
	controller.InvocationPath = "qurl"
	controller.LookPath = func(string) (string, error) { return binaryPath, nil }
	controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) {
		return IPCStatus{JobVersion: "5/2.4.0"}, true, nil
	}
	reloads := 0
	controller.Reload = func(context.Context) (bool, error) { reloads++; return true, nil }
	if err := controller.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(manager.jobs) != 0 || len(manager.replaced) != 1 || reloads != 0 {
		t.Fatalf("definition loads/replacements/live reloads = %d/%d/%d, want forced versioned replacement", len(manager.jobs), len(manager.replaced), reloads)
	}
	if manager.statusCalls != 1 {
		t.Fatalf("native ownership status calls = %d, want 1", manager.statusCalls)
	}
	if got := manager.replaced[0].Arguments[9]; got != "5/2.5.0" {
		t.Fatalf("job version argument = %q, want 5/2.5.0", got)
	}
}

func TestJobControllerRejectsIncompatibleForegroundOwnerWithoutStartingSecondDaemon(t *testing.T) {
	dir := t.TempDir()
	foreground := connectorservice.ServiceStatus{Installed: false, Running: false}
	manager := &recordingJobManager{status: &foreground}
	controller := newTestJobController(t, filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.5.0", "https://api.sandbox.layerv.xyz", GroupModeSingle, func() (qurl.HubBootstrap, error) {
		t.Fatal("Hub resolution ran for an incompatible foreground owner")
		return qurl.HubBootstrap{}, nil
	})
	controller.Manager = manager
	controller.LookPath = func(string) (string, error) {
		t.Fatal("qurl path lookup ran for an incompatible foreground owner")
		return "", nil
	}
	controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) {
		return IPCStatus{JobVersion: "5/2.4.0"}, true, nil
	}
	controller.Reload = func(context.Context) (bool, error) {
		t.Fatal("reload ran for an incompatible foreground owner")
		return false, nil
	}
	err := controller.Ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "stop the foreground or externally managed daemon") {
		t.Fatalf("Ensure error = %v, want safe incompatible foreground-owner guidance", err)
	}
	if len(manager.jobs) != 0 || len(manager.replaced) != 0 || manager.statusCalls != 1 {
		t.Fatalf("incompatible foreground manager ensure/replace/status = %d/%d/%d, want 0/0/1", len(manager.jobs), len(manager.replaced), manager.statusCalls)
	}
}

func TestJobControllerTreatsLoadedJobAsOwnershipBeforeIPCInitialization(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "bin", "qurl")
	manager := &recordingJobManager{}
	controller := newTestJobController(t, filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.4.0", "https://api.sandbox.layerv.xyz", GroupModeSingle, testHubResolver)
	controller.Manager = manager
	controller.InvocationPath = "qurl"
	controller.LookPath = func(string) (string, error) { return binaryPath, nil }
	controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) { return IPCStatus{}, false, nil }
	controller.Reload = func(context.Context) (bool, error) { t.Fatal("reload called without IPC"); return false, nil }
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := controller.Ensure(ctx); err != nil {
		t.Fatalf("Ensure waited for initializing daemon IPC: %v", err)
	}
	if len(manager.jobs) != 1 {
		t.Fatalf("LaunchAgent Ensure calls = %d, want 1", len(manager.jobs))
	}
}

func TestJobControllerRejectsSecretBearingOrMalformedDeploymentState(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		resolve  func() (qurl.HubBootstrap, error)
		want     string
	}{
		{name: "empty endpoint", endpoint: "", resolve: testHubResolver, want: "endpoint is empty or non-canonical"},
		{name: "endpoint userinfo", endpoint: "https://user:secret@api.sandbox.layerv.xyz", resolve: testHubResolver, want: "must not contain userinfo"},
		{name: "endpoint query", endpoint: "https://api.sandbox.layerv.xyz?token=secret", resolve: testHubResolver, want: "must not contain userinfo, query, or fragment"},
		{name: "resolver error", endpoint: "https://api.sandbox.layerv.xyz", resolve: func() (qurl.HubBootstrap, error) {
			return qurl.HubBootstrap{}, errors.New("Hub lookup failed")
		}, want: "Hub lookup failed"},
		{name: "untrusted Hub", endpoint: "https://api.sandbox.layerv.xyz", resolve: func() (qurl.HubBootstrap, error) {
			return qurl.HubBootstrap{Host: "127.0.0.1", Port: 443, ServerPublicKeyB64: testHubKey}, nil
		}, want: "canonical lowercase DNS name"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			manager := &recordingJobManager{}
			controller := newTestJobController(t, filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.4.0", test.endpoint, GroupModeSingle, test.resolve)
			controller.Manager = manager
			controller.LookPath = func(string) (string, error) {
				t.Fatal("qurl path lookup ran before deployment state validation")
				return "", nil
			}
			probes := 0
			controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) {
				probes++
				return IPCStatus{}, false, nil
			}
			err := controller.Ensure(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Ensure error = %v, want substring %q", err, test.want)
			}
			if len(manager.jobs) != 0 || len(manager.replaced) != 0 {
				t.Fatalf("invalid deployment state installed jobs: ensure=%d replace=%d", len(manager.jobs), len(manager.replaced))
			}
			if probes != 1 {
				t.Fatalf("IPC probes = %d, want 1 before deployment validation", probes)
			}
		})
	}
}

func TestJobVersionFoldsOnlyANonDefaultModeIntoTheDefinition(t *testing.T) {
	// single is the pre-mode string byte for byte, so an unchanged single-mode
	// daemon is still compatible with its job across this change.
	if got, err := JobVersion("2.4.0", GroupModeSingle); err != nil || got != "5/2.4.0" {
		t.Fatalf("single-mode job version = (%q, %v), want 5/2.4.0", got, err)
	}
	if got, err := JobVersion("2.4.0", GroupModePerShare); err != nil || got != "5/2.4.0/per-share" {
		t.Fatalf("per-share job version = (%q, %v), want 5/2.4.0/per-share", got, err)
	}
	if _, err := JobVersion("2.4.0", GroupMode("")); err == nil || !strings.Contains(err.Error(), "invalid share group mode") {
		t.Fatalf("empty mode error = %v, want an invalid-mode rejection", err)
	}
	if _, err := JobVersion("", GroupModeSingle); err == nil {
		t.Fatal("empty binary version was accepted")
	}
}

// TestJobControllerModeChangeReplacesResidentDaemonLikeAVersionChange pins
// that switching the session group mode is a job-definition change: the live
// daemon is not reloaded over IPC, it is replaced with a job that carries the
// new mode explicitly and the mode-bearing job version.
func TestJobControllerModeChangeReplacesResidentDaemonLikeAVersionChange(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "bin", "qurl")
	manager := &recordingJobManager{}
	controller := newTestJobController(t, filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.4.0", "https://api.sandbox.layerv.xyz", GroupModePerShare, testHubResolver)
	controller.Manager = manager
	controller.InvocationPath = "qurl"
	controller.LookPath = func(string) (string, error) { return binaryPath, nil }
	controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) {
		// Same binary, resident in the default mode.
		return IPCStatus{JobVersion: "5/2.4.0"}, true, nil
	}
	reloads := 0
	controller.Reload = func(context.Context) (bool, error) { reloads++; return true, nil }
	if err := controller.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(manager.jobs) != 0 || len(manager.replaced) != 1 || reloads != 0 || manager.statusCalls != 1 {
		t.Fatalf("definition loads/replacements/live reloads/status = %d/%d/%d/%d, want a forced replacement", len(manager.jobs), len(manager.replaced), reloads, manager.statusCalls)
	}
	arguments := manager.replaced[0].Arguments
	if got := arguments[9]; got != "5/2.4.0/per-share" {
		t.Fatalf("job version argument = %q, want 5/2.4.0/per-share", got)
	}
	if arguments[10] != "--share-group-mode" || arguments[11] != "per-share" {
		t.Fatalf("job arguments = %#v, want an explicit --share-group-mode per-share", arguments)
	}

	// Switching back is the same definition change in the other direction.
	back := newTestJobController(t, filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.4.0", "https://api.sandbox.layerv.xyz", GroupModeSingle, testHubResolver)
	back.Manager = manager
	back.InvocationPath = "qurl"
	back.LookPath = func(string) (string, error) { return binaryPath, nil }
	back.ProbeStatus = func(context.Context) (IPCStatus, bool, error) {
		return IPCStatus{JobVersion: "5/2.4.0/per-share"}, true, nil
	}
	back.Reload = func(context.Context) (bool, error) { reloads++; return true, nil }
	if err := back.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(manager.replaced) != 2 || reloads != 0 {
		t.Fatalf("switching back replaced/reloaded = %d/%d, want 2/0", len(manager.replaced), reloads)
	}
	if got := manager.replaced[1].Arguments; got[9] != "5/2.4.0" || got[10] != "--share-group-mode" || got[11] != "single" {
		t.Fatalf("single-mode job arguments = %#v", got)
	}
}

func TestJobControllerSameModeResidentDaemonReloadsLive(t *testing.T) {
	dir := t.TempDir()
	manager := &recordingJobManager{}
	controller := newTestJobController(t, filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.4.0", "https://api.sandbox.layerv.xyz", GroupModePerShare, func() (qurl.HubBootstrap, error) {
		t.Fatal("Hub resolution ran for a compatible live owner")
		return qurl.HubBootstrap{}, nil
	})
	controller.Manager = manager
	controller.LookPath = func(string) (string, error) {
		t.Fatal("qurl path lookup ran for a compatible live owner")
		return "", nil
	}
	controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) {
		return IPCStatus{JobVersion: "5/2.4.0/per-share"}, true, nil
	}
	reloads := 0
	controller.Reload = func(context.Context) (bool, error) { reloads++; return true, nil }
	if err := controller.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(manager.jobs) != 0 || len(manager.replaced) != 0 || reloads != 1 {
		t.Fatalf("same-mode owner ensure/replace/reload = %d/%d/%d, want 0/0/1", len(manager.jobs), len(manager.replaced), reloads)
	}
}

func TestJobControllerRefusesToInstallAnUnknownMode(t *testing.T) {
	dir := t.TempDir()
	manager := &recordingJobManager{}
	controller := newTestJobController(t, filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.4.0", "https://api.sandbox.layerv.xyz", GroupMode("both"), testHubResolver)
	controller.Manager = manager
	controller.LookPath = func(string) (string, error) { return filepath.Join(dir, "bin", "qurl"), nil }
	controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) { return IPCStatus{}, false, nil }
	controller.Reload = func(context.Context) (bool, error) { t.Fatal("unexpected reload"); return false, nil }
	err := controller.Ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid share group mode") {
		t.Fatalf("Ensure error = %v, want invalid-mode rejection", err)
	}
	if len(manager.jobs) != 0 || len(manager.replaced) != 0 {
		t.Fatalf("unknown mode installed jobs: ensure=%d replace=%d", len(manager.jobs), len(manager.replaced))
	}
}

func newTestJobController(t *testing.T, stateDir, logDir, binaryVersion, endpoint string, mode GroupMode, resolveHub func() (qurl.HubBootstrap, error)) *JobController {
	t.Helper()
	controller, err := NewJobController(stateDir, logDir, binaryVersion, endpoint, mode, connectorstate.RuntimeSupervisionNative, resolveHub, nil)
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func TestJobControllerCarriesTheResolvedRuntimeDir(t *testing.T) {
	base := shortTempRoot(t)
	stateDir, runtimeDir := filepath.Join(base, "state"), filepath.Join(base, "rt")
	controller, err := NewJobController(stateDir, filepath.Join(base, "logs"), "2.4.0", "https://api.example.test",
		GroupModeSingle, connectorstate.RuntimeSupervisionNative, testHubResolver, lookupEnvFrom(map[string]string{RuntimeDirEnv: runtimeDir}))
	if err != nil {
		t.Fatal(err)
	}
	want := runtimeDir
	if runtime.GOOS == "windows" {
		// A named pipe has no path bound, so Windows ignores RuntimeDirEnv.
		want = stateDir
	}
	if controller.RuntimeDir != want || controller.IPC.SocketPath != filepath.Join(want, SocketFile) {
		t.Fatalf("runtime dir %q socket %q, want %q and %q", controller.RuntimeDir, controller.IPC.SocketPath, want, filepath.Join(want, SocketFile))
	}
	manager := &recordingJobManager{}
	controller.Manager = manager
	controller.InvocationPath = "qurl"
	controller.LookPath = func(string) (string, error) { return filepath.Join(base, "bin", "qurl"), nil }
	controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) { return IPCStatus{}, false, nil }
	controller.Reload = func(context.Context) (bool, error) { return true, nil }
	if err := controller.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(manager.jobs) != 1 {
		t.Fatalf("installed jobs = %d, want 1", len(manager.jobs))
	}
	arguments := manager.jobs[0].Arguments
	index := slices.Index(arguments, "--runtime-dir")
	if index < 0 || index+1 >= len(arguments) || arguments[index+1] != want {
		t.Fatalf("job arguments = %#v, want an explicit --runtime-dir %q", arguments, want)
	}

	_, err = NewJobController(stateDir, filepath.Join(base, "logs"), "2.4.0", "https://api.example.test",
		GroupModeSingle, connectorstate.RuntimeSupervisionNative, testHubResolver, lookupEnvFrom(map[string]string{RuntimeDirEnv: "relative"}))
	if runtime.GOOS == "windows" {
		// The pipe address discards the runtime directory, so an unusable value
		// left in the environment has no effect there rather than failing every
		// command about a setting that does nothing.
		if err != nil {
			t.Fatalf("relative runtime dir = %v, want it ignored on Windows", err)
		}
	} else if err == nil {
		t.Fatal("relative runtime dir built a job controller")
	}
}

// externalTestController is a JobController in external supervision whose
// native job manager, Hub resolver, and PATH lookup all fail the test if they
// are touched: an external daemon is its supervisor's process, never the
// CLI's.
func externalTestController(t *testing.T, manager *recordingJobManager) *JobController {
	t.Helper()
	dir := t.TempDir()
	controller, err := NewJobController(filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.5.0", "https://api.example.com",
		GroupModeSingle, connectorstate.RuntimeSupervisionExternal, func() (qurl.HubBootstrap, error) {
			t.Fatal("Hub resolution ran under external supervision")
			return qurl.HubBootstrap{}, nil
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	controller.Manager = manager
	controller.LookPath = func(string) (string, error) {
		t.Fatal("qurl path lookup ran under external supervision")
		return "", nil
	}
	return controller
}

func TestEnsureExternalReloadsMatchedDaemon(t *testing.T) {
	manager := &recordingJobManager{statusErr: errors.New("native job manager consulted under external supervision")}
	controller := externalTestController(t, manager)
	controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) {
		return IPCStatus{JobVersion: "5/2.5.0", Pid: 4242}, true, nil
	}
	reloads := 0
	controller.Reload = func(context.Context) (bool, error) { reloads++; return true, nil }
	if err := controller.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(manager.jobs) != 0 || len(manager.replaced) != 0 || manager.statusCalls != 0 || reloads != 1 {
		t.Fatalf("external matched daemon ensure/replace/status/reload = %d/%d/%d/%d, want 0/0/0/1", len(manager.jobs), len(manager.replaced), manager.statusCalls, reloads)
	}
}

func TestEnsureExternalNotRunningFailsWithoutInstall(t *testing.T) {
	manager := &recordingJobManager{statusErr: errors.New("native job manager consulted under external supervision")}
	controller := externalTestController(t, manager)
	controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) { return IPCStatus{}, false, nil }
	controller.Reload = func(context.Context) (bool, error) {
		t.Fatal("reload ran without a live daemon")
		return false, nil
	}
	err := controller.Ensure(context.Background())
	if !errors.Is(err, ErrExternalDaemonNotRunning) {
		t.Fatalf("Ensure error = %v, want ErrExternalDaemonNotRunning", err)
	}
	if !strings.Contains(err.Error(), "qurl daemon run --supervision external") {
		t.Fatalf("Ensure error = %v, want the supervisor's start command", err)
	}
	if len(manager.jobs) != 0 || len(manager.replaced) != 0 || manager.statusCalls != 0 {
		t.Fatalf("external absent daemon ensure/replace/status = %d/%d/%d, want 0/0/0", len(manager.jobs), len(manager.replaced), manager.statusCalls)
	}
}

// TestEnsureExternalCompatibleOwnerExitingBeforeReloadIsNotRunning pins the
// race the native path resolves by installing: under external supervision the
// same race is reported, never repaired.
func TestEnsureExternalCompatibleOwnerExitingBeforeReloadIsNotRunning(t *testing.T) {
	manager := &recordingJobManager{}
	controller := externalTestController(t, manager)
	controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) {
		return IPCStatus{JobVersion: "5/2.5.0"}, true, nil
	}
	controller.Reload = func(context.Context) (bool, error) { return false, nil }
	if err := controller.Ensure(context.Background()); !errors.Is(err, ErrExternalDaemonNotRunning) {
		t.Fatalf("Ensure error = %v, want ErrExternalDaemonNotRunning", err)
	}
	if len(manager.jobs) != 0 || len(manager.replaced) != 0 || manager.statusCalls != 0 {
		t.Fatalf("exited external owner ensure/replace/status = %d/%d/%d, want 0/0/0", len(manager.jobs), len(manager.replaced), manager.statusCalls)
	}
}

func TestEnsureExternalMismatchedDaemonIsReportedNotReplaced(t *testing.T) {
	manager := &recordingJobManager{statusErr: errors.New("native job manager consulted under external supervision")}
	controller := externalTestController(t, manager)
	controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) {
		return IPCStatus{JobVersion: "5/2.4.0"}, true, nil
	}
	controller.Reload = func(context.Context) (bool, error) {
		t.Fatal("reload ran for a daemon on another job definition")
		return false, nil
	}
	err := controller.Ensure(context.Background())
	if err == nil || errors.Is(err, ErrExternalDaemonNotRunning) || !strings.Contains(err.Error(), "restart the externally supervised daemon") {
		t.Fatalf("Ensure error = %v, want a definition mismatch that names the supervisor's restart", err)
	}
	if len(manager.jobs) != 0 || len(manager.replaced) != 0 || manager.statusCalls != 0 {
		t.Fatalf("mismatched external daemon ensure/replace/status = %d/%d/%d, want 0/0/0", len(manager.jobs), len(manager.replaced), manager.statusCalls)
	}
}

func TestJobControllerRefusesAnUnknownSupervision(t *testing.T) {
	dir := t.TempDir()
	manager := &recordingJobManager{}
	controller, err := NewJobController(filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.4.0", "https://api.example.com", GroupModeSingle, connectorstate.RuntimeSupervision(""), testHubResolver, nil)
	if err != nil {
		t.Fatal(err)
	}
	controller.Manager = manager
	controller.LookPath = func(string) (string, error) { return filepath.Join(dir, "bin", "qurl"), nil }
	controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) { return IPCStatus{}, false, nil }
	controller.Reload = func(context.Context) (bool, error) { t.Fatal("unexpected reload"); return false, nil }
	err = controller.Ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid daemon supervision") {
		t.Fatalf("Ensure error = %v, want invalid-supervision rejection", err)
	}
	if len(manager.jobs) != 0 || len(manager.replaced) != 0 {
		t.Fatalf("unknown supervision installed jobs: ensure=%d replace=%d", len(manager.jobs), len(manager.replaced))
	}
}

func TestJobControllerIncompatibleStatusRequiresNativeOwnership(t *testing.T) {
	for _, tc := range []struct {
		name        string
		supervision connectorstate.RuntimeSupervision
		managed     bool
	}{
		{"native owner", connectorstate.RuntimeSupervisionNative, true},
		{"foreground owner", connectorstate.RuntimeSupervisionNative, false},
		{"external owner", connectorstate.RuntimeSupervisionExternal, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			manager := &recordingJobManager{status: &connectorservice.ServiceStatus{Installed: tc.managed, Running: tc.managed}}
			controller, err := NewJobController(filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.5.0", "https://api.sandbox.layerv.xyz", GroupModeSingle, tc.supervision, testHubResolver, nil)
			if err != nil {
				t.Fatal(err)
			}
			controller.Manager = manager
			controller.InvocationPath = "qurl"
			controller.LookPath = func(string) (string, error) { return filepath.Join(dir, "qurl"), nil }
			controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) {
				return IPCStatus{}, true, errIPCStatusIncompatible
			}
			controller.Reload = func(context.Context) (bool, error) { t.Fatal("incompatible owner reloaded"); return false, nil }
			err = controller.Ensure(context.Background())
			wantReplace := tc.managed && tc.supervision == connectorstate.RuntimeSupervisionNative
			if (err == nil) != wantReplace || (len(manager.replaced) == 1) != wantReplace || len(manager.jobs) != 0 {
				t.Fatalf("Ensure error=%v replacements=%d installs=%d, want replace=%t", err, len(manager.replaced), len(manager.jobs), wantReplace)
			}
			if !wantReplace && !errors.Is(err, errIPCStatusIncompatible) {
				t.Fatalf("Ensure error = %v, want the incompatible status cause", err)
			}
			if tc.supervision == connectorstate.RuntimeSupervisionExternal && manager.statusCalls != 0 {
				t.Fatal("external supervision queried native ownership")
			}
		})
	}
}

// TestJobControllerSecuresAPreExistingRuntimeDirBeforeProbing pins the
// client-first case: an operator creates the runtime directory with a normal
// umask, so it is 0755 when a qurl command reaches it before any daemon has
// run. Under native supervision qurl owns that directory, so Ensure secures
// it instead of failing the probe on a directory the job install would have
// fixed.
func TestJobControllerSecuresAPreExistingRuntimeDirBeforeProbing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows named pipes have no socket directory")
	}
	base := shortTempRoot(t)
	stateDir, runtimeDir := filepath.Join(base, "state"), filepath.Join(base, "rt")
	if err := os.Mkdir(runtimeDir, 0o755); err != nil { // #nosec G301 -- the point of the test is a permissive pre-existing directory.
		t.Fatal(err)
	}
	if err := os.Chmod(runtimeDir, 0o755); err != nil { // #nosec G302 -- pin the mode despite umask.
		t.Fatal(err)
	}
	controller, err := NewJobController(stateDir, filepath.Join(base, "logs"), "2.4.0", "https://api.example.test",
		GroupModeSingle, connectorstate.RuntimeSupervisionNative, testHubResolver,
		lookupEnvFrom(map[string]string{RuntimeDirEnv: runtimeDir}))
	if err != nil {
		t.Fatal(err)
	}
	manager := &recordingJobManager{}
	controller.Manager = manager
	controller.InvocationPath = "qurl"
	controller.LookPath = func(string) (string, error) { return filepath.Join(base, "bin", "qurl"), nil }
	// The production probe, so the parent-directory check actually runs.
	controller.ProbeStatus = controller.IPC.Status
	controller.Reload = controller.IPC.ReloadIfRunning
	if err := controller.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure over a 0755 runtime dir = %v, want the directory secured and the job installed", err)
	}
	info, err := os.Lstat(runtimeDir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("runtime dir mode = %v err=%v, want 0700", info.Mode().Perm(), err)
	}
	if len(manager.jobs) != 1 {
		t.Fatalf("installed jobs = %d, want the install the probe failure used to block", len(manager.jobs))
	}
}
