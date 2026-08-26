package daemon

import (
	"context"
	"errors"
	"path/filepath"
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
	jobs     []connectorservice.UserJob
	replaced []connectorservice.UserJob
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
func (*recordingJobManager) Status(string) (connectorservice.ServiceStatus, error) {
	return connectorservice.ServiceStatus{Installed: true, Running: true}, nil
}

func TestJobControllerPersistsStableHomebrewShim(t *testing.T) {
	dir := t.TempDir()
	manager := &recordingJobManager{}
	controller := NewJobController(filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.4.0", "https://api.sandbox.layerv.xyz", testHubResolver)
	controller.Manager = manager
	controller.LookPath = func(name string) (string, error) {
		if name != "qurl" {
			return "", errors.New("unexpected lookup")
		}
		return "/opt/homebrew/bin/qurl", nil
	}
	probes := 0
	controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) {
		probes++
		if probes == 1 {
			return IPCStatus{}, false, nil
		}
		return IPCStatus{JobVersion: "1/2.4.0"}, true, nil
	}
	reloads := 0
	controller.Reload = func(context.Context) (bool, error) { reloads++; return true, nil }
	if err := controller.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A Homebrew Cellar target change is intentionally invisible because the
	// persisted ProgramArguments keep the PATH-resolved shim.
	if err := controller.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(manager.jobs) != 2 || reloads != 1 {
		t.Fatalf("LaunchAgent definition checks/reloads = %d/%d, want two idempotent definition checks and one IPC reload", len(manager.jobs), reloads)
	}
	for _, job := range manager.jobs {
		if job.BinaryPath != "/opt/homebrew/bin/qurl" {
			t.Fatalf("binary path = %q, want stable Homebrew shim", job.BinaryPath)
		}
		if got := job.Arguments; len(got) != 14 || got[0] != "--endpoint" || got[1] != "https://api.sandbox.layerv.xyz" || got[2] != "daemon" || got[3] != "run" || got[4] != "--state-dir" || got[6] != "--job-version" || got[7] != "1/2.4.0" || got[8] != "--hub-host" || got[9] != "hub.sandbox.layerv.xyz" || got[10] != "--hub-port" || got[11] != "443" || got[12] != "--hub-server-public-key-b64" || got[13] != testHubKey {
			t.Fatalf("ProgramArguments = %#v", got)
		}
		if job.Umask != 0o077 {
			t.Fatalf("launchd Umask = %#o, want 0077", job.Umask)
		}
	}
}

func TestJobControllerVersionChangeReloadsDefinitionInsteadOfLiveIPC(t *testing.T) {
	dir := t.TempDir()
	manager := &recordingJobManager{}
	controller := NewJobController(filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.5.0", "https://api.sandbox.layerv.xyz", testHubResolver)
	controller.Manager = manager
	controller.LookPath = func(string) (string, error) { return "/opt/homebrew/bin/qurl", nil }
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
	if got := manager.replaced[0].Arguments[7]; got != "1/2.5.0" {
		t.Fatalf("job version argument = %q, want 1/2.5.0", got)
	}
}

func TestJobControllerTreatsLoadedJobAsOwnershipBeforeIPCInitialization(t *testing.T) {
	dir := t.TempDir()
	manager := &recordingJobManager{}
	controller := NewJobController(filepath.Join(dir, "state"), filepath.Join(dir, "logs"), "2.4.0", "https://api.sandbox.layerv.xyz", testHubResolver)
	controller.Manager = manager
	controller.LookPath = func(string) (string, error) { return "/opt/homebrew/bin/qurl", nil }
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
			controller.ProbeStatus = func(context.Context) (IPCStatus, bool, error) {
				t.Fatal("IPC probe ran before deployment state validation")
				return IPCStatus{}, false, nil
			}
			err := controller.Ensure(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Ensure error = %v, want substring %q", err, test.want)
			}
			if len(manager.jobs) != 0 || len(manager.replaced) != 0 {
				t.Fatalf("invalid deployment state installed jobs: ensure=%d replace=%d", len(manager.jobs), len(manager.replaced))
			}
		})
	}
}
