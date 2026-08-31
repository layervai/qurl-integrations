package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	connectorservice "github.com/layervai/qurl-connector/pkg/service"
	qurl "github.com/layervai/qurl-go/qurl"
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
	controller := NewJobController(filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.4.0", "https://api.sandbox.layerv.xyz", testHubResolver)
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
		wantArguments := make([]string, 0, 18)
		wantArguments = append(wantArguments,
			"--endpoint", "https://api.sandbox.layerv.xyz", "daemon", "run", "--state-dir", filepath.Join(dir, "state"),
			"--job-version", "1/2.4.0", "--hub-host", "hub.sandbox.layerv.xyz", "--hub-port", "443",
			"--hub-server-public-key-b64", testHubKey,
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
			controller := NewJobController(filepath.Join(dir, "state", test.name), filepath.Join(dir, "logs", test.name),
				"2.5.0", "https://api.sandbox.layerv.xyz", testHubResolver)
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
	controller := NewJobController(filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.5.0", "https://api.sandbox.layerv.xyz", testHubResolver)
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
	controller := NewJobController(filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.5.0", "https://api.sandbox.layerv.xyz", testHubResolver)
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
	controller := NewJobController(filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.4.0", "https://api.sandbox.layerv.xyz", func() (qurl.HubBootstrap, error) {
		t.Fatal("Hub resolution ran for a compatible live owner")
		return qurl.HubBootstrap{}, nil
	})
	controller.Manager = manager
	controller.LookPath = func(string) (string, error) {
		t.Fatal("qurl path lookup ran for a compatible live owner")
		return "", nil
	}
	controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) {
		return IPCStatus{JobVersion: "1/2.4.0"}, true, nil
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
	controller := NewJobController(filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.4.0", "https://api.sandbox.layerv.xyz", testHubResolver)
	controller.Manager = manager
	controller.InvocationPath = "qurl"
	controller.LookPath = func(string) (string, error) { return binaryPath, nil }
	controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) {
		return IPCStatus{JobVersion: "1/2.4.0"}, true, nil
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
	controller := NewJobController(filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.5.0", "https://api.sandbox.layerv.xyz", testHubResolver)
	controller.Manager = manager
	controller.InvocationPath = "qurl"
	controller.LookPath = func(string) (string, error) { return binaryPath, nil }
	controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) {
		return IPCStatus{JobVersion: "1/2.4.0"}, true, nil
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
	if got := manager.replaced[0].Arguments[7]; got != "1/2.5.0" {
		t.Fatalf("job version argument = %q, want 1/2.5.0", got)
	}
}

func TestJobControllerRejectsIncompatibleForegroundOwnerWithoutStartingSecondDaemon(t *testing.T) {
	dir := t.TempDir()
	foreground := connectorservice.ServiceStatus{Installed: false, Running: false}
	manager := &recordingJobManager{status: &foreground}
	controller := NewJobController(filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.5.0", "https://api.sandbox.layerv.xyz", func() (qurl.HubBootstrap, error) {
		t.Fatal("Hub resolution ran for an incompatible foreground owner")
		return qurl.HubBootstrap{}, nil
	})
	controller.Manager = manager
	controller.LookPath = func(string) (string, error) {
		t.Fatal("qurl path lookup ran for an incompatible foreground owner")
		return "", nil
	}
	controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) {
		return IPCStatus{JobVersion: "1/2.4.0"}, true, nil
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
	controller := NewJobController(filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.4.0", "https://api.sandbox.layerv.xyz", testHubResolver)
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
			controller := NewJobController(filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.4.0", test.endpoint, test.resolve)
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
