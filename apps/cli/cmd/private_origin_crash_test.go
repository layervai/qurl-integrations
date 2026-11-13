package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

const crashOriginToken = "private-origin-crash-test-token"

// Only cloud admission is a fixture. The daemon and origin run in separate
// processes and traffic traverses the real TLS Connector/FRPS path.
func TestPrivateOriginCrashLeavesSurvivingDaemonOffTCP(t *testing.T) {
	if testing.Short() {
		t.Skip("real FRP daemon process journey")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rootDir, err := os.MkdirTemp(privateTestTempRoot(), "qc-") // Keep the socket below macOS sun_path's limit.
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(rootDir) })
	// Create the namespace through EnsureDirMode so elevated Windows runners
	// assign its owner to the current user, not the inherited Administrators SID.
	dir := filepath.Join(rootDir, "state")
	target := privateCmdTarget(t, dir)
	if err := connectorstate.EstablishExternalRuntimeMode(ctx, dir); err != nil {
		t.Fatal(err)
	}
	registry, err := openOwnedTestShareRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := apitest.NewServer(t)
	row := localShareFixture(srv)
	row.ConnectorID, row.DesiredState = "qurl-file-crash", "on"
	row.LocalPort = reserveCmdTCPPort(t)
	row.TargetURL = "http://127.0.0.1:" + strconv.Itoa(row.LocalPort)
	if err := registry.Put(ctx, &row); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.RetargetStoppedToPrivate(ctx, "own_cli_fixture", "qurl-file-", target.URL); err != nil {
		t.Fatal(err)
	}
	before, err := registry.Get(ctx, row.CRID)
	if err != nil {
		t.Fatal(err)
	}
	frpsPort, vhostPort := reserveCmdTCPPort(t), reserveCmdTCPPort(t)
	recorder := newCmdProxyRecorder(t)
	caFile := startCmdFRPS(t, frpsPort, vhostPort, "hermetic.test", recorder.server.URL)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	start := func(role string) (*exec.Cmd, func()) {
		t.Helper()
		log, err := os.CreateTemp(t.TempDir(), role+"-*.log")
		if err != nil {
			t.Fatal(err)
		}
		child := exec.CommandContext(ctx, binary, "-test.run=^TestPrivateOriginCrashProcessHelper$") // #nosec G204 -- run this test binary as the crash fixture.
		child.Env = append(os.Environ(), "TMPDIR="+dir, "QURL_CRASH_ROLE="+role, "QURL_CRASH_STATE="+dir, "QURL_CRASH_CA="+caFile, "QURL_CRASH_FRPS="+strconv.Itoa(frpsPort))
		child.Stdout, child.Stderr = log, log
		if err := child.Start(); err != nil {
			_ = log.Close()
			t.Fatal(err)
		}
		stop := sync.OnceFunc(func() {
			_ = child.Process.Kill()
			_ = child.Wait()
			_ = log.Close()
			if t.Failed() {
				data, _ := os.ReadFile(log.Name()) // #nosec G304 -- this test's child log.
				t.Logf("%s: %s", role, data)
			}
		})
		t.Cleanup(stop)
		return child, stop
	}
	origin, crashOrigin := start("origin")
	daemon, _ := start("daemon")
	// The shared helper verifies deferred startup before the parent may reload.
	for ctx.Err() == nil {
		if _, err := os.Stat(filepath.Join(dir, "daemon-ready")); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	ipc := connectordaemon.IPCClient{SocketPath: stateSocketPath(t, dir)}
	if err := ipc.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	if running, err := ipc.SetOverlay(ctx, map[string]map[string]string{row.ConnectorID: {"X-QURL-File-Token": crashOriginToken}}); err != nil || !running {
		t.Fatalf("restore origin headers: running=%t err=%v", running, err)
	}
	client := &http.Client{Timeout: time.Second}
	request := func() (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(vhostPort), http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = row.ConnectorRoutingID + ".hermetic.test"
		req.Header.Set("X-QURL-File-Token", "forged-caller-token")
		response, err := client.Do(req)
		if err != nil {
			return 0, ""
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, string(body)
	}
	waitForOrigin := func() {
		t.Helper()
		for ctx.Err() == nil {
			if status, body := request(); status == http.StatusOK && body == "protected-file" {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("private origin never served through the daemon")
	}
	waitForOrigin()
	crashOrigin() // Forced process termination: no graceful cleanup.
	if origin.ProcessState == nil {
		t.Fatal("origin crash was not reaped")
	}
	assertPrivateOriginKilled(t, origin.ProcessState)
	var hits atomic.Int32
	occupant := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "unrelated-port-occupant")
	}))
	_ = occupant.Listener.Close()
	occupant.Listener, err = (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:"+strconv.Itoa(row.LocalPort))
	if err != nil {
		t.Fatal(err)
	}
	occupant.Start()
	t.Cleanup(occupant.Close)
	for range 5 {
		status, body := request()
		if (status >= 100 && status < 400) || strings.Contains(body, crashOriginToken) || strings.Contains(body, target.SocketPath+target.PipeName) {
			t.Fatalf("dead origin returned successful or private response: status=%d", status)
		}
	}
	if hits.Load() != 0 {
		t.Fatal("surviving daemon sent a request or origin header to the TCP occupant")
	}
	start("origin")
	waitForOrigin() // Same route and registry; no daemon reload.
	status, running, err := ipc.Status(ctx)
	if err != nil || !running || status.Pid != daemon.Process.Pid {
		t.Fatalf("daemon did not survive origin crash: pid=%d running=%t err=%v", status.Pid, running, err)
	}
	after, err := registry.Get(ctx, row.CRID)
	if err != nil || *after != *before || hits.Load() != 0 {
		t.Fatalf("origin restart changed identity or reached TCP: err=%v hits=%d", err, hits.Load())
	}
}

func TestPrivateOriginCrashProcessHelper(t *testing.T) {
	role := os.Getenv("QURL_CRASH_ROLE")
	if role == "" {
		return
	}
	dir := os.Getenv("QURL_CRASH_STATE")
	if role == "origin" {
		target := privateCmdTarget(t, dir)
		listener, err := listenPrivateTestOrigin(target)
		if err != nil {
			t.Fatal(err)
		}
		server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-QURL-File-Token") != crashOriginToken {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, "protected-file")
		})}
		t.Fatal(server.Serve(listener))
	}
	unlock, err := connectorstate.AcquireDaemonLease(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unlock() }()
	registry, err := connectorstate.OpenLocalShareRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	daemon := &journeyDaemon{registry: registry, caFile: os.Getenv("QURL_CRASH_CA"), version: "test", admitter: &journeyAdmitter{host: "localhost:" + os.Getenv("QURL_CRASH_FRPS"), serving: make(chan struct{})}}
	startExternalJourneyDaemon(t, daemon, dir, "https://unused.example.test")
	if err := os.WriteFile(filepath.Join(dir, "daemon-ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	<-time.After(time.Minute) // The parent owns and kills this process.
}
