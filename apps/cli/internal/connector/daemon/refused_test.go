package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

func TestRefusedJobCanBeReplacedWithoutServingResources(t *testing.T) {
	dir := shortTempDir(t)
	path, err := SocketPathForStateDir(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- RunRefusedJob(ctx, dir, path) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("exit: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("refused job did not stop")
		}
	})
	client := IPCClient{StateDir: dir, SocketPath: path}
	ready, stop := context.WithTimeout(t.Context(), 3*time.Second)
	defer stop()
	if err := client.WaitReady(ready); !errors.Is(err, ErrJobRefused) {
		t.Fatalf("readiness: %v", err)
	}
	status, running, err := client.Status(t.Context())
	if err != nil || !running || status.JobVersion != "0/refused" || status.Pid <= 0 || len(status.Running)+len(status.Resources) != 0 {
		t.Fatalf("status = %+v, %v, %v", status, running, err)
	}
	if _, err := client.ReloadIfRunning(t.Context()); !errors.Is(err, ErrJobRefused) {
		t.Fatalf("reload: %v", err)
	}
	if _, err := client.SetOverlay(t.Context(), map[string]map[string]string{}); !errors.Is(err, ErrJobRefused) {
		t.Fatalf("overlay: %v", err)
	}
	controller, err := NewJobController(dir, filepath.Join(dir, "logs"), "2.4.0", "https://api.sandbox.layerv.xyz", GroupModeSingle, connectorstate.RuntimeSupervisionNative, testHubResolver, nil)
	if err != nil {
		t.Fatal(err)
	}
	jobs := &recordingJobManager{}
	controller.Manager = jobs
	if err := controller.Ensure(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(jobs.replaced) != 1 || len(jobs.jobs) != 0 || jobs.statusCalls != 1 {
		t.Fatalf("job actions: %+v", jobs)
	}
	if runtime.GOOS == "windows" {
		if ipcNamespace(strings.ToUpper(dir)) != ipcNamespace(strings.ToLower(dir)) {
			t.Fatal("Windows namespace changed with case")
		}
		if hint, err := connectorstate.ReadDaemonRuntimeDir(dir); err != nil || hint != "" {
			t.Fatalf("Windows marker: %q, %v", hint, err)
		}
	}
}
