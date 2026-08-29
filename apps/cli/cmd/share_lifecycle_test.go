//go:build !windows

package main

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	qurl "github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

type recordingShareDaemon struct {
	ensures   int
	reloads   int
	ensureErr error
}

func (d *recordingShareDaemon) Ensure(context.Context) error {
	d.ensures++
	return d.ensureErr
}

type failPutRegistry struct {
	localShareRegistry
	err error
}

func (r *failPutRegistry) Put(context.Context, *connectorstate.LocalShare) error { return r.err }

type failNextSetDesiredRegistry struct {
	localShareRegistry
	err   error
	calls int
}

func (r *failNextSetDesiredRegistry) SetDesired(ctx context.Context, id, desired string, epoch uint64) (*connectorstate.LocalShare, error) {
	r.calls++
	if r.calls == 1 {
		return nil, r.err
	}
	return r.localShareRegistry.SetDesired(ctx, id, desired, epoch)
}

type journeyAdmitter struct {
	host     string
	openTime time.Duration
	serving  chan struct{}
	once     sync.Once
	mu       sync.Mutex
	next     uint64
}

func (a *journeyAdmitter) Admit(_ context.Context, knockResourceID, resourceID string) (connectorshare.Admission, error) {
	runID, err := qurl.NewCycleRunID()
	if err != nil {
		return connectorshare.Admission{}, err
	}
	a.mu.Lock()
	a.next++
	sessionID := a.next
	a.mu.Unlock()
	openTime := a.openTime
	if openTime <= 0 {
		openTime = time.Hour
	}
	return connectorshare.Admission{
		KnockResourceID: knockResourceID, ResourceID: resourceID,
		RunID: runID, RunAttempt: 1, Token: "ac-hermetic", ResourceHost: a.host,
		SessionID: sessionID,
		SessionReceipt: qurl.NativeSessionReceipt{
			CellID: "cell0", SessionID: sessionID, SessionIssuedAtMillis: time.Now().UnixMilli(),
			RunID: runID, RunAttempt: 1,
		},
		OpenTime: openTime,
	}, nil
}

func (a *journeyAdmitter) admissions() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.next
}

func (*journeyAdmitter) Retire(context.Context, connectorshare.Admission) error { return nil }
func (a *journeyAdmitter) MarkServingHealthy() error {
	a.once.Do(func() { close(a.serving) })
	return nil
}
func (*journeyAdmitter) Close() error { return nil }

type journeyDaemon struct {
	registry *connectorstate.LocalShareRegistry
	admitter *journeyAdmitter
	version  string

	mu      sync.Mutex
	manager *connectordaemon.Manager
	cancel  context.CancelFunc
	done    chan error
}

func (d *journeyDaemon) Ensure(ctx context.Context) error {
	d.mu.Lock()
	if d.manager == nil {
		common, err := connectordaemon.DefaultFRPCommon(2, 5)
		if err != nil {
			d.mu.Unlock()
			return err
		}
		factory, err := connectordaemon.NewNativeSessionFactory(d.admitter, common, d.version)
		if err != nil {
			d.mu.Unlock()
			return err
		}
		manager, err := connectordaemon.NewManager(d.registry, factory)
		if err != nil {
			d.mu.Unlock()
			return err
		}
		runCtx, cancel := context.WithCancel(context.Background())
		d.manager, d.cancel, d.done = manager, cancel, make(chan error, 1)
		go func() { d.done <- manager.Run(runCtx) }()
	}
	d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (d *journeyDaemon) ReloadIfRunning(context.Context) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.manager == nil {
		return false, nil
	}
	d.manager.Trigger()
	return true, nil
}

func (d *journeyDaemon) close() {
	d.mu.Lock()
	cancel, done := d.cancel, d.done
	d.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (d *recordingShareDaemon) ReloadIfRunning(context.Context) (bool, error) {
	d.reloads++
	return true, nil
}

func TestShareLifecycleCommandsConvergeCloudRegistryAndDaemon(t *testing.T) {
	tests := []struct {
		name            string
		command         string
		method          string
		desired         string
		epoch           uint64
		connection      string
		wantEnsures     int
		wantReloads     int
		followupServing bool
	}{
		{name: "start", command: "start", method: http.MethodPut, desired: "on", epoch: 5, connection: "serving", wantEnsures: 1, followupServing: true},
		{name: "stop", command: "stop", method: http.MethodPut, desired: "off", epoch: 5, connection: "stopped", wantReloads: 1},
		{name: "restart", command: "restart", method: http.MethodPost, desired: "on", epoch: 5, connection: "serving", wantEnsures: 1, followupServing: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			stateDir := t.TempDir()
			registry, err := connectorstate.OpenLocalShareRegistry(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			seed := localShareFixture(srv)
			if err := registry.Put(context.Background(), &seed); err != nil {
				t.Fatal(err)
			}
			sharingPath := "/v1/resources/" + srv.Key.CRID + "/sharing"
			path := sharingPath
			if test.command == "restart" {
				path += "/restart"
			}
			reply := func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
					"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
					"desired_state": test.desired, "serving_epoch": test.epoch,
					"connection_state": test.connection,
				}, nil)
			}
			if test.command != "stop" {
				srv.Script(http.MethodGet, sharingPath, func(w http.ResponseWriter, _ *http.Request) {
					apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
						"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
						"desired_state": "off", "serving_epoch": 4,
						"connection_state": "stopped",
					}, nil)
				})
			}
			srv.Script(test.method, path, reply)
			if test.followupServing {
				srv.Script(http.MethodGet, sharingPath, reply)
			}
			daemon := &recordingShareDaemon{}
			res := runCLI(t, &runOpts{
				args: []string{"--endpoint", srv.URL, test.command, srv.Key.CRID},
				env: map[string]string{
					"QURL_API_KEY": testAPIKey, "QURL_CONNECTOR_STATE_DIR": stateDir,
				},
				shareRegistry: registry, shareDaemon: daemon,
				shareStateDir:   stateDir,
				preflightTarget: func(context.Context, string, int) error { return nil },
			})
			if res.code != 0 {
				t.Fatalf("exit=%d stderr=%s", res.code, res.stderr.String())
			}
			if daemon.ensures != test.wantEnsures || daemon.reloads != test.wantReloads {
				t.Fatalf("daemon ensures/reloads = %d/%d, want %d/%d", daemon.ensures, daemon.reloads, test.wantEnsures, test.wantReloads)
			}
			updated, err := registry.Get(context.Background(), srv.Key.CRID)
			if err != nil {
				t.Fatal(err)
			}
			if updated.DesiredState != test.desired || updated.ServingEpoch != test.epoch {
				t.Fatalf("local state = %+v", updated)
			}
			for _, want := range []string{srv.Key.CRID, seed.TargetURL, test.desired, test.connection} {
				if !strings.Contains(res.stdout.String(), want) {
					t.Errorf("stdout missing %q:\n%s", want, res.stdout.String())
				}
			}
		})
	}
}

func TestStartRotatesEpochAfterLocalTerminalDisable(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := t.TempDir()
	registry, err := connectorstate.OpenLocalShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	seed := localShareFixture(srv)
	seed.DesiredState = "off"
	if err := registry.Put(context.Background(), &seed); err != nil {
		t.Fatal(err)
	}
	path := "/v1/resources/" + srv.Key.CRID + "/sharing"
	srv.Script(http.MethodGet, path, sharingResponse(t, srv, "on", seed.ServingEpoch, "serving"))
	srv.Script(http.MethodPost, path+"/restart", sharingResponse(t, srv, "on", seed.ServingEpoch+1, "connecting"))
	srv.Script(http.MethodGet, path, sharingResponse(t, srv, "on", seed.ServingEpoch+1, "serving"))
	daemon := &recordingShareDaemon{}
	res := runCLI(t, &runOpts{
		args: []string{"--endpoint", srv.URL, "start", srv.Key.CRID},
		env: map[string]string{
			"QURL_API_KEY": testAPIKey, "QURL_CONNECTOR_STATE_DIR": stateDir,
		},
		shareRegistry: registry, shareDaemon: daemon, shareStateDir: stateDir,
		preflightTarget: func(context.Context, string, int) error { return nil },
	})
	if res.code != 0 {
		t.Fatalf("exit=%d stderr=%s", res.code, res.stderr.String())
	}
	requests := srv.Requests()
	var posts, puts int
	for _, request := range requests {
		if request.Method == http.MethodPost {
			posts++
		}
		if request.Method == http.MethodPut {
			puts++
		}
	}
	if posts != 1 || puts != 0 || daemon.ensures != 1 {
		t.Fatalf("terminal recovery POST/PUT/ensure=%d/%d/%d, want 1/0/1", posts, puts, daemon.ensures)
	}
	got, err := registry.Get(context.Background(), srv.Key.CRID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DesiredState != "on" || got.ServingEpoch != seed.ServingEpoch+1 {
		t.Fatalf("terminal recovery local state=%+v", got)
	}
}

func TestShareStatusDoesNotStartOrReloadDaemon(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := t.TempDir()
	t.Setenv("QURL_CONNECTOR_STATE_DIR", stateDir)
	registry, err := connectorstate.OpenLocalShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	seed := localShareFixture(srv)
	if err := registry.Put(context.Background(), &seed); err != nil {
		t.Fatal(err)
	}
	srv.Script(http.MethodGet, "/v1/resources/"+srv.Key.CRID+"/sharing", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
			"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
			"desired_state": "off", "serving_epoch": 4, "connection_state": "stopped",
		}, nil)
	})
	daemon := &recordingShareDaemon{}
	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "status", srv.Key.CRID},
		env:           map[string]string{"QURL_API_KEY": testAPIKey, "QURL_CONNECTOR_STATE_DIR": stateDir},
		shareRegistry: registry, shareDaemon: daemon,
		shareStateDir: stateDir,
	})
	if res.code != 0 {
		t.Fatalf("exit=%d stderr=%s", res.code, res.stderr.String())
	}
	if daemon.ensures != 0 || daemon.reloads != 0 {
		t.Fatalf("status touched daemon: %+v", daemon)
	}
}

func TestShareStatusReportsRemoteURLResource(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			srv := apitest.NewServer(t)
			sharingPath := "/v1/resources/" + srv.Key.CRID + "/sharing"
			srv.Script(http.MethodGet, sharingPath, func(w http.ResponseWriter, _ *http.Request) {
				// Detail is intentionally unrelated prose: status branches only on
				// the stable code, then uses the generic resource type to prove
				// whether this is a non-Connector resource.
				apitest.WriteProblem(t, w, http.StatusBadRequest, "invalid_input", "Invalid Input", "This wording may change")
			})
			srv.Script(http.MethodGet, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{"resource": map[string]any{
					"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
					"target_url": "https://aol.com", "type": "url", "status": "active",
				}}, nil)
			})
			registryOpens := 0
			daemonBuilds := 0
			args := []string{"--endpoint", srv.URL}
			if format == "json" {
				args = append(args, "--output", "json")
			}
			args = append(args, "status", srv.Key.CRID)
			res := runCLI(t, &runOpts{
				args: args,
				env:  map[string]string{"QURL_API_KEY": testAPIKey},
				shareRegistryFactory: func(string) (localShareRegistry, error) {
					registryOpens++
					return nil, errors.New("URL status constructed a local registry")
				},
				shareDaemonFactory: func(string, string) shareDaemonController {
					daemonBuilds++
					return &recordingShareDaemon{}
				},
				shareStateDirErr: connectorstate.ErrNoDefaultStateDir,
			})
			if res.code != 0 {
				t.Fatalf("exit=%d stderr=%s", res.code, res.stderr.String())
			}
			for _, want := range []string{srv.Key.CRID, "https://aol.com", "url", "active"} {
				if !strings.Contains(res.stdout.String(), want) {
					t.Errorf("stdout missing %q:\n%s", want, res.stdout.String())
				}
			}
			if registryOpens != 0 || daemonBuilds != 0 {
				t.Fatalf("URL status constructed local controls: registry=%d daemon=%d", registryOpens, daemonBuilds)
			}
			requests := srv.Requests()
			if len(requests) != 2 || requests[0].Path != sharingPath || requests[1].Path != "/v1/resources/"+srv.Key.CRID {
				t.Fatalf("URL status requests = %#v", requests)
			}
		})
	}
}

func TestShareStatusPreservesInvalidInputForConnectorResource(t *testing.T) {
	srv := apitest.NewServer(t)
	detail := "the Connector request was invalid for another reason"
	srv.Script(http.MethodGet, "/v1/resources/"+srv.Key.CRID+"/sharing", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteProblem(t, w, http.StatusBadRequest, "invalid_input", "Invalid Input", detail)
	})
	srv.Script(http.MethodGet, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{"resource": map[string]any{
			"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
			"type": "tunnel", "status": "active", "desired_state": "off", "serving_epoch": 0,
		}}, nil)
	})

	res := runCLI(t, &runOpts{
		args: []string{"--endpoint", srv.URL, "status", srv.Key.CRID},
		env:  map[string]string{"QURL_API_KEY": testAPIKey},
	})
	if res.code == 0 || !strings.Contains(res.stderr.String(), detail) {
		t.Fatalf("exit=%d stderr=%s, want original Connector invalid_input", res.code, res.stderr.String())
	}
}

func TestDeleteRemovesLocalShareWithoutStartingDaemon(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := t.TempDir()
	t.Setenv("QURL_CONNECTOR_STATE_DIR", stateDir)
	registry, err := connectorstate.OpenLocalShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	local := localShareFixture(srv)
	if err := registry.Put(context.Background(), &local); err != nil {
		t.Fatal(err)
	}
	daemon := &recordingShareDaemon{}
	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "delete", srv.Key.CRID, "--yes"},
		env:           map[string]string{"QURL_API_KEY": testAPIKey, "QURL_CONNECTOR_STATE_DIR": stateDir},
		shareRegistry: registry, shareDaemon: daemon,
		shareStateDir: stateDir,
	})
	if res.code != 0 {
		t.Fatalf("exit=%d stderr=%s", res.code, res.stderr.String())
	}
	if _, err := registry.Get(context.Background(), srv.Key.CRID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted local share still present: %v", err)
	}
	if daemon.ensures != 0 || daemon.reloads != 0 {
		t.Fatalf("delete touched LaunchAgent controller: %+v", daemon)
	}
}

func TestStopAndStatusWorkWithoutLocalRegistryRow(t *testing.T) {
	for _, command := range []string{"stop", "status"} {
		t.Run(command, func(t *testing.T) {
			srv := apitest.NewServer(t)
			stateDir := filepath.Join(t.TempDir(), "absent-connector-state")
			method := http.MethodGet
			if command == "stop" {
				method = http.MethodPut
			}
			srv.Script(method, "/v1/resources/"+srv.Key.CRID+"/sharing", func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
					"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
					"desired_state": "off", "serving_epoch": 9, "connection_state": "stopped",
				}, nil)
			})
			registryOpens := 0
			daemonBuilds := 0
			res := runCLI(t, &runOpts{
				args: []string{"--endpoint", srv.URL, command, srv.Key.CRID},
				env:  map[string]string{"QURL_API_KEY": testAPIKey},
				shareRegistryFactory: func(string) (localShareRegistry, error) {
					registryOpens++
					return nil, errors.New("remote command constructed a local registry")
				},
				shareDaemonFactory: func(string, string) shareDaemonController {
					daemonBuilds++
					return &recordingShareDaemon{}
				},
				shareStateDir: stateDir,
			})
			if res.code != 0 {
				t.Fatalf("exit=%d stderr=%s", res.code, res.stderr.String())
			}
			if registryOpens != 0 || daemonBuilds != 0 {
				t.Fatalf("remote %s constructed local controls: registry=%d daemon=%d", command, registryOpens, daemonBuilds)
			}
			if _, err := os.Lstat(stateDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("remote %s created local state path: %v", command, err)
			}
			if strings.Contains(res.stdout.String(), "Target:") {
				t.Fatalf("remote status fabricated local target:\n%s", res.stdout.String())
			}
		})
	}
}

func TestStopAndStatusWorkWithoutDefaultLocalNamespace(t *testing.T) {
	for _, command := range []string{"stop", "status"} {
		t.Run(command, func(t *testing.T) {
			srv := apitest.NewServer(t)
			method := http.MethodGet
			if command == "stop" {
				method = http.MethodPut
			}
			srv.Script(method, "/v1/resources/"+srv.Key.CRID+"/sharing", func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
					"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
					"desired_state": "off", "serving_epoch": 9, "connection_state": "stopped",
				}, nil)
			})
			registryOpens := 0
			daemonBuilds := 0
			res := runCLI(t, &runOpts{
				args:             []string{"--endpoint", srv.URL, command, srv.Key.CRID},
				env:              map[string]string{"QURL_API_KEY": testAPIKey},
				shareStateDirErr: connectorstate.ErrNoDefaultStateDir,
				shareRegistryFactory: func(string) (localShareRegistry, error) {
					registryOpens++
					return nil, errors.New("remote command constructed a local registry")
				},
				shareDaemonFactory: func(string, string) shareDaemonController {
					daemonBuilds++
					return &recordingShareDaemon{}
				},
			})
			if res.code != 0 {
				t.Fatalf("exit=%d stderr=%s", res.code, res.stderr.String())
			}
			if registryOpens != 0 || daemonBuilds != 0 {
				t.Fatalf("remote %s constructed local controls: registry=%d daemon=%d", command, registryOpens, daemonBuilds)
			}
			if strings.Contains(res.stdout.String(), "Target:") {
				t.Fatalf("remote %s fabricated local target:\n%s", command, res.stdout.String())
			}
		})
	}
}

func TestStopAndStatusReportCorruptExistingLocalRegistry(t *testing.T) {
	for _, command := range []string{"stop", "status"} {
		t.Run(command, func(t *testing.T) {
			srv := apitest.NewServer(t)
			stateDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(stateDir, connectorstate.LocalSharesFile), []byte("{not-json"), 0o600); err != nil {
				t.Fatal(err)
			}
			method := http.MethodGet
			if command == "stop" {
				method = http.MethodPut
			}
			srv.Script(method, "/v1/resources/"+srv.Key.CRID+"/sharing", func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
					"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
					"desired_state": "off", "serving_epoch": 9, "connection_state": "stopped",
				}, nil)
			})
			res := runCLI(t, &runOpts{
				args: []string{"--endpoint", srv.URL, command, srv.Key.CRID},
				env:  map[string]string{"QURL_API_KEY": testAPIKey}, shareStateDir: stateDir,
			})
			if res.code == 0 || !strings.Contains(res.stderr.String(), "local share registry") {
				t.Fatalf("result code=%d stderr=%s", res.code, res.stderr.String())
			}
			if requests := srv.Requests(); len(requests) != 1 || requests[0].Method != method {
				t.Fatalf("management requests = %#v, want exactly the authoritative %s", requests, method)
			}
		})
	}
}

func TestStartFailsImmediatelyWhenServingEpochAdvances(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := t.TempDir()
	registry, err := connectorstate.OpenLocalShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	local := localShareFixture(srv)
	if err := registry.Put(context.Background(), &local); err != nil {
		t.Fatal(err)
	}
	path := "/v1/resources/" + srv.Key.CRID + "/sharing"
	write := func(epoch uint64, state string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
				"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
				"desired_state": "on", "serving_epoch": epoch, "connection_state": state,
			}, nil)
		}
	}
	srv.Script(http.MethodGet, path, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
			"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
			"desired_state": "off", "serving_epoch": 4, "connection_state": "stopped",
		}, nil)
	})
	srv.Script(http.MethodPut, path, write(5, "connecting"))
	srv.Script(http.MethodGet, path, write(6, "serving"))
	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "start", srv.Key.CRID},
		env:           map[string]string{"QURL_API_KEY": testAPIKey},
		shareRegistry: registry, shareDaemon: &recordingShareDaemon{}, shareStateDir: stateDir,
		preflightTarget: func(context.Context, string, int) error { return nil },
	})
	if res.code == 0 || !strings.Contains(res.stderr.String(), "advanced to serving epoch 6") {
		t.Fatalf("result code=%d stderr=%s", res.code, res.stderr.String())
	}
}

func TestLocalPublishCompensatesSetupFailureBeforeDaemonOwnership(t *testing.T) {
	for _, stage := range []string{"registry", "daemon"} {
		t.Run(stage, func(t *testing.T) {
			srv := apitest.NewServer(t)
			stateDir := t.TempDir()
			baseRegistry, err := connectorstate.OpenLocalShareRegistry(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			var registry localShareRegistry = baseRegistry
			setupErr := fmt.Errorf("%s setup failed", stage)
			if stage == "registry" {
				registry = &failPutRegistry{localShareRegistry: baseRegistry, err: setupErr}
			}
			daemon := &recordingShareDaemon{}
			if stage == "daemon" {
				daemon.ensureErr = setupErr
			}
			path := "/v1/resources/" + srv.Key.CRID + "/sharing"
			reply := func(desired string, epoch uint64, connection string) http.HandlerFunc {
				return func(w http.ResponseWriter, _ *http.Request) {
					apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
						"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
						"desired_state": desired, "serving_epoch": epoch, "connection_state": connection,
					}, nil)
				}
			}
			srv.Script(http.MethodGet, path, reply("off", 4, "stopped"))
			srv.Script(http.MethodPut, path, reply("on", 5, "connecting"), reply("off", 6, "stopped"))
			resolver := func(_ context.Context, _ *connectorshare.NativeRuntimeConfig, resolveID func(string) (string, error)) (*agent.ResolvedResource, error) {
				id, err := resolveID("agent-one")
				if err != nil {
					return nil, err
				}
				found := false
				return &agent.ResolvedResource{Resource: &qurl.ConnectorResource{
					ResourceID: srv.Key.ResourceID, CRID: srv.Key.CRID, Slug: id,
					ConnectorRoutingID: "c-" + strings.Repeat("a", 52), KnockResourceID: "q_catalog_key",
				}, FoundExisting: &found}, nil
			}
			res := runCLI(t, &runOpts{
				args:          []string{"--endpoint", srv.URL, "publish", "http://127.0.0.1:3000"},
				env:           map[string]string{"QURL_API_KEY": testAPIKey},
				shareRegistry: registry, shareDaemon: daemon, shareStateDir: stateDir,
				localResource:   resolver,
				preflightTarget: func(context.Context, string, int) error { return nil },
			})
			if res.code == 0 || !strings.Contains(res.stderr.String(), setupErr.Error()) {
				t.Fatalf("result code=%d stderr=%s", res.code, res.stderr.String())
			}
			requests := srv.Requests()
			if len(requests) != 3 || requests[0].Method != http.MethodGet || requests[1].Method != http.MethodPut || requests[2].Method != http.MethodPut {
				t.Fatalf("lifecycle requests = %#v, want on then compensating off", requests)
			}
			local, getErr := baseRegistry.Get(context.Background(), srv.Key.CRID)
			if stage == "registry" {
				if !errors.Is(getErr, os.ErrNotExist) {
					t.Fatalf("failed registry write left row: %+v err=%v", local, getErr)
				}
			} else if getErr != nil || local.DesiredState != "off" || local.ServingEpoch != 6 {
				t.Fatalf("daemon failure compensation = %+v err=%v", local, getErr)
			}
		})
	}
}

func TestStartCompensatesCloudOnWhenLocalSetupDoesNotReachDaemonOwnership(t *testing.T) {
	for _, stage := range []string{"registry", "daemon"} {
		t.Run(stage, func(t *testing.T) {
			srv := apitest.NewServer(t)
			stateDir := t.TempDir()
			baseRegistry, err := connectorstate.OpenLocalShareRegistry(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			seed := localShareFixture(srv)
			seed.DesiredState = "off"
			seed.ServingEpoch = 4
			if err := baseRegistry.Put(context.Background(), &seed); err != nil {
				t.Fatal(err)
			}
			setupErr := fmt.Errorf("%s setup failed", stage)
			var registry localShareRegistry = baseRegistry
			if stage == "registry" {
				registry = &failNextSetDesiredRegistry{localShareRegistry: baseRegistry, err: setupErr}
			}
			daemon := &recordingShareDaemon{}
			if stage == "daemon" {
				daemon.ensureErr = setupErr
			}
			path := "/v1/resources/" + srv.Key.CRID + "/sharing"
			reply := func(desired string, epoch uint64, connection string) http.HandlerFunc {
				return func(w http.ResponseWriter, _ *http.Request) {
					apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
						"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
						"desired_state": desired, "serving_epoch": epoch, "connection_state": connection,
					}, nil)
				}
			}
			srv.Script(http.MethodGet, path, reply("off", 4, "stopped"))
			srv.Script(http.MethodPut, path, reply("on", 5, "connecting"), reply("off", 6, "stopped"))
			res := runCLI(t, &runOpts{
				args:          []string{"--endpoint", srv.URL, "start", srv.Key.CRID},
				env:           map[string]string{"QURL_API_KEY": testAPIKey},
				shareRegistry: registry, shareDaemon: daemon, shareStateDir: stateDir,
				preflightTarget: func(context.Context, string, int) error { return nil },
			})
			if res.code == 0 || !strings.Contains(res.stderr.String(), setupErr.Error()) {
				t.Fatalf("result code=%d stderr=%s", res.code, res.stderr.String())
			}
			requests := srv.Requests()
			if len(requests) != 3 || requests[0].Method != http.MethodGet || requests[1].Method != http.MethodPut || requests[2].Method != http.MethodPut {
				t.Fatalf("lifecycle requests = %#v, want on then compensating off", requests)
			}
			local, err := baseRegistry.Get(context.Background(), srv.Key.CRID)
			if err != nil || local.DesiredState != "off" || local.ServingEpoch != 6 {
				t.Fatalf("compensated local state = %+v err=%v", local, err)
			}
		})
	}
}

func TestStartPriorOnSetupFailureDoesNotDisableHealthyShare(t *testing.T) {
	for _, stage := range []string{"registry", "daemon"} {
		t.Run(stage, func(t *testing.T) {
			srv := apitest.NewServer(t)
			stateDir := t.TempDir()
			baseRegistry, err := connectorstate.OpenLocalShareRegistry(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			seed := localShareFixture(srv)
			seed.DesiredState = "on"
			if err := baseRegistry.Put(context.Background(), &seed); err != nil {
				t.Fatal(err)
			}
			setupErr := fmt.Errorf("%s setup failed", stage)
			var registry localShareRegistry = baseRegistry
			if stage == "registry" {
				registry = &failNextSetDesiredRegistry{localShareRegistry: baseRegistry, err: setupErr}
			}
			daemon := &recordingShareDaemon{}
			if stage == "daemon" {
				daemon.ensureErr = setupErr
			}
			path := "/v1/resources/" + srv.Key.CRID + "/sharing"
			reply := func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
					"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
					"desired_state": "on", "serving_epoch": 4, "connection_state": "serving",
				}, nil)
			}
			srv.Script(http.MethodGet, path, reply)
			srv.Script(http.MethodPut, path, reply)
			res := runCLI(t, &runOpts{
				args:          []string{"--endpoint", srv.URL, "start", srv.Key.CRID},
				env:           map[string]string{"QURL_API_KEY": testAPIKey},
				shareRegistry: registry, shareDaemon: daemon, shareStateDir: stateDir,
				preflightTarget: func(context.Context, string, int) error { return nil },
			})
			if res.code == 0 || !strings.Contains(res.stderr.String(), setupErr.Error()) {
				t.Fatalf("result code=%d stderr=%s", res.code, res.stderr.String())
			}
			requests := srv.Requests()
			if len(requests) != 2 || requests[0].Method != http.MethodGet || requests[1].Method != http.MethodPut {
				t.Fatalf("lifecycle requests = %#v, want GET then idempotent PUT with no compensating off", requests)
			}
			local, err := baseRegistry.Get(context.Background(), srv.Key.CRID)
			if err != nil || local.DesiredState != "on" || local.ServingEpoch != 4 {
				t.Fatalf("healthy prior-on local state changed: %+v err=%v", local, err)
			}
		})
	}
}

func TestRepublishPriorOnSetupFailureDoesNotDisableHealthyShare(t *testing.T) {
	for _, stage := range []string{"registry", "daemon"} {
		t.Run(stage, func(t *testing.T) {
			srv := apitest.NewServer(t)
			stateDir := t.TempDir()
			baseRegistry, err := connectorstate.OpenLocalShareRegistry(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			seed := localShareFixture(srv)
			seed.DesiredState = "on"
			if err := baseRegistry.Put(context.Background(), &seed); err != nil {
				t.Fatal(err)
			}
			setupErr := fmt.Errorf("%s setup failed", stage)
			var registry localShareRegistry = baseRegistry
			if stage == "registry" {
				registry = &failPutRegistry{localShareRegistry: baseRegistry, err: setupErr}
			}
			daemon := &recordingShareDaemon{}
			if stage == "daemon" {
				daemon.ensureErr = setupErr
			}
			path := "/v1/resources/" + srv.Key.CRID + "/sharing"
			reply := func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
					"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
					"desired_state": "on", "serving_epoch": 4, "connection_state": "serving",
				}, nil)
			}
			srv.Script(http.MethodGet, path, reply)
			srv.Script(http.MethodPut, path, reply)
			found := true
			res := runCLI(t, &runOpts{
				args:          []string{"--endpoint", srv.URL, "publish", seed.TargetURL},
				env:           map[string]string{"QURL_API_KEY": testAPIKey},
				shareRegistry: registry, shareDaemon: daemon, shareStateDir: stateDir,
				preflightTarget: func(context.Context, string, int) error { return nil },
				localResource: func(context.Context, *connectorshare.NativeRuntimeConfig, func(string) (string, error)) (*agent.ResolvedResource, error) {
					return &agent.ResolvedResource{Resource: &qurl.ConnectorResource{
						ResourceID: srv.Key.ResourceID, CRID: srv.Key.CRID, Slug: seed.ConnectorID,
						ConnectorRoutingID: seed.ConnectorRoutingID, KnockResourceID: seed.KnockResourceID,
					}, FoundExisting: &found}, nil
				},
			})
			if res.code == 0 || !strings.Contains(res.stderr.String(), setupErr.Error()) {
				t.Fatalf("result code=%d stderr=%s", res.code, res.stderr.String())
			}
			requests := srv.Requests()
			if len(requests) != 2 || requests[0].Method != http.MethodGet || requests[1].Method != http.MethodPut {
				t.Fatalf("lifecycle requests = %#v, want GET then idempotent PUT with no compensating off", requests)
			}
			local, err := baseRegistry.Get(context.Background(), srv.Key.CRID)
			if err != nil || local.DesiredState != "on" || local.ServingEpoch != 4 {
				t.Fatalf("healthy prior-on local state changed: %+v err=%v", local, err)
			}
		})
	}
}

func TestRestartSetupFailureAlwaysCompensatesAdvancedEpoch(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := t.TempDir()
	registry, err := connectorstate.OpenLocalShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	seed := localShareFixture(srv)
	seed.DesiredState = "on"
	if err := registry.Put(context.Background(), &seed); err != nil {
		t.Fatal(err)
	}
	path := "/v1/resources/" + srv.Key.CRID + "/sharing"
	srv.Script(http.MethodGet, path, sharingResponse(t, srv, "on", 4, "serving"))
	srv.Script(http.MethodPost, path+"/restart", sharingResponse(t, srv, "on", 5, "connecting"))
	srv.Script(http.MethodPut, path, sharingResponse(t, srv, "off", 6, "stopped"))
	want := errors.New("daemon setup failed")
	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "restart", srv.Key.CRID},
		env:           map[string]string{"QURL_API_KEY": testAPIKey},
		shareRegistry: registry, shareDaemon: &recordingShareDaemon{ensureErr: want}, shareStateDir: stateDir,
		preflightTarget: func(context.Context, string, int) error { return nil },
	})
	if res.code == 0 || !strings.Contains(res.stderr.String(), want.Error()) {
		t.Fatalf("result code=%d stderr=%s", res.code, res.stderr.String())
	}
	requests := srv.Requests()
	if len(requests) != 3 || requests[0].Method != http.MethodGet || requests[1].Method != http.MethodPost || requests[2].Method != http.MethodPut {
		t.Fatalf("lifecycle requests = %#v, want GET, one restart POST, compensating off PUT", requests)
	}
	local, err := registry.Get(context.Background(), srv.Key.CRID)
	if err != nil || local.DesiredState != "off" || local.ServingEpoch != 6 {
		t.Fatalf("restart compensation = %+v err=%v", local, err)
	}
}

func TestRestartReconcilesAmbiguousAppliedPostWithoutReplay(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := t.TempDir()
	registry, err := connectorstate.OpenLocalShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	seed := localShareFixture(srv)
	seed.DesiredState = "on"
	if err := registry.Put(context.Background(), &seed); err != nil {
		t.Fatal(err)
	}
	path := "/v1/resources/" + srv.Key.CRID + "/sharing"
	srv.Script(http.MethodGet, path,
		sharingResponse(t, srv, "on", 4, "serving"),
		sharingResponse(t, srv, "on", 5, "connecting"),
		sharingResponse(t, srv, "on", 5, "serving"),
	)
	srv.Script(http.MethodPost, path+"/restart", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteProblem(t, w, http.StatusServiceUnavailable, "unavailable", "Response uncertain", "restart result is ambiguous")
	})
	daemon := &recordingShareDaemon{}
	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "restart", srv.Key.CRID},
		env:           map[string]string{"QURL_API_KEY": testAPIKey},
		shareRegistry: registry, shareDaemon: daemon, shareStateDir: stateDir,
		preflightTarget: func(context.Context, string, int) error { return nil },
	})
	if res.code != 0 {
		t.Fatalf("result code=%d stderr=%s", res.code, res.stderr.String())
	}
	posts := 0
	gets := 0
	for _, request := range srv.Requests() {
		switch request.Method {
		case http.MethodPost:
			posts++
		case http.MethodGet:
			gets++
		}
	}
	if posts != 1 || gets != 3 || daemon.ensures != 1 {
		t.Fatalf("restart post/get/ensure=%d/%d/%d, want 1/3/1", posts, gets, daemon.ensures)
	}
	local, err := registry.Get(context.Background(), srv.Key.CRID)
	if err != nil || local.ServingEpoch != 5 || local.DesiredState != "on" {
		t.Fatalf("reconciled local state = %+v err=%v", local, err)
	}
}

func TestRestartRejectsAmbiguousStateWithoutNewOnEpoch(t *testing.T) {
	for _, current := range []struct {
		name, desired, connection string
		epoch                     uint64
	}{
		{name: "unchanged", desired: "on", epoch: 4, connection: "serving"},
		{name: "stopped concurrently", desired: "off", epoch: 5, connection: "stopped"},
	} {
		t.Run(current.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			stateDir := t.TempDir()
			registry, err := connectorstate.OpenLocalShareRegistry(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			seed := localShareFixture(srv)
			seed.DesiredState = "on"
			if err := registry.Put(context.Background(), &seed); err != nil {
				t.Fatal(err)
			}
			path := "/v1/resources/" + srv.Key.CRID + "/sharing"
			srv.Script(http.MethodGet, path,
				sharingResponse(t, srv, "on", 4, "serving"),
				sharingResponse(t, srv, current.desired, current.epoch, current.connection),
			)
			srv.Script(http.MethodPost, path+"/restart", func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteProblem(t, w, http.StatusServiceUnavailable, "unavailable", "Response uncertain", "restart result is ambiguous")
			})
			daemon := &recordingShareDaemon{}
			res := runCLI(t, &runOpts{
				args:          []string{"--endpoint", srv.URL, "restart", srv.Key.CRID},
				env:           map[string]string{"QURL_API_KEY": testAPIKey},
				shareRegistry: registry, shareDaemon: daemon, shareStateDir: stateDir,
				preflightTarget: func(context.Context, string, int) error { return nil },
			})
			if res.code == 0 || !strings.Contains(res.stderr.String(), "ambiguous") {
				t.Fatalf("result code=%d stderr=%s", res.code, res.stderr.String())
			}
			if requests := srv.Requests(); len(requests) != 3 || requests[1].Method != http.MethodPost {
				t.Fatalf("requests = %#v, want prior GET, one POST, reconcile GET", requests)
			}
			if daemon.ensures != 0 {
				t.Fatalf("ambiguous state started daemon %d times", daemon.ensures)
			}
		})
	}
}

func TestPublishNewMachineTakeoverRotatesEpochOnce(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := t.TempDir()
	registry, err := connectorstate.OpenLocalShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/resources/" + srv.Key.CRID + "/sharing"
	srv.Script(http.MethodGet, path, sharingResponse(t, srv, "on", 4, "serving"))
	srv.Script(http.MethodPost, path+"/restart", sharingResponse(t, srv, "on", 5, "connecting"))
	srv.Script(http.MethodGet, path, sharingResponse(t, srv, "on", 5, "serving"))
	daemon := &recordingShareDaemon{}
	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "publish", "http://127.0.0.1:3000"},
		env:           map[string]string{"QURL_API_KEY": testAPIKey},
		shareRegistry: registry, shareDaemon: daemon, shareStateDir: stateDir,
		preflightTarget: func(context.Context, string, int) error { return nil },
		localResource:   resolvedLocalResource(srv, true),
	})
	if res.code != 0 {
		t.Fatalf("result code=%d stderr=%s", res.code, res.stderr.String())
	}
	posts, puts := 0, 0
	for _, request := range srv.Requests() {
		if request.Method == http.MethodPost {
			posts++
		}
		if request.Method == http.MethodPut {
			puts++
		}
	}
	if posts != 1 || puts != 0 {
		t.Fatalf("takeover POST/PUT=%d/%d, want one epoch-fencing restart only", posts, puts)
	}
	local, err := registry.Get(context.Background(), srv.Key.CRID)
	if err != nil || local.ServingEpoch != 5 || local.TargetURL != "http://127.0.0.1:3000" {
		t.Fatalf("takeover local state = %+v err=%v", local, err)
	}
}

func TestPublishRotatesEpochAfterLocalTerminalDisable(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := t.TempDir()
	registry, err := connectorstate.OpenLocalShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	seed := localShareFixture(srv)
	seed.DesiredState = "off"
	if err := registry.Put(context.Background(), &seed); err != nil {
		t.Fatal(err)
	}
	path := "/v1/resources/" + srv.Key.CRID + "/sharing"
	srv.Script(http.MethodGet, path, sharingResponse(t, srv, "on", seed.ServingEpoch, "serving"))
	srv.Script(http.MethodPost, path+"/restart", sharingResponse(t, srv, "on", seed.ServingEpoch+1, "connecting"))
	srv.Script(http.MethodGet, path, sharingResponse(t, srv, "on", seed.ServingEpoch+1, "serving"))
	daemon := &recordingShareDaemon{}
	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "publish", "http://127.0.0.1:3000"},
		env:           map[string]string{"QURL_API_KEY": testAPIKey},
		shareRegistry: registry, shareDaemon: daemon, shareStateDir: stateDir,
		preflightTarget: func(context.Context, string, int) error { return nil },
		localResource:   resolvedLocalResource(srv, true),
	})
	if res.code != 0 {
		t.Fatalf("result code=%d stderr=%s", res.code, res.stderr.String())
	}
	posts, puts := 0, 0
	for _, request := range srv.Requests() {
		if request.Method == http.MethodPost {
			posts++
		}
		if request.Method == http.MethodPut {
			puts++
		}
	}
	if posts != 1 || puts != 0 || daemon.ensures != 1 {
		t.Fatalf("terminal recovery POST/PUT/ensure=%d/%d/%d, want 1/0/1", posts, puts, daemon.ensures)
	}
	local, err := registry.Get(context.Background(), srv.Key.CRID)
	if err != nil {
		t.Fatal(err)
	}
	if local.DesiredState != "on" || local.ServingEpoch != seed.ServingEpoch+1 || local.TargetURL != seed.TargetURL {
		t.Fatalf("terminal recovery local state = %+v", local)
	}
}

func TestPublishTargetChangeReconcilesAmbiguousRestart(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := t.TempDir()
	registry, err := connectorstate.OpenLocalShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	seed := localShareFixture(srv)
	seed.DesiredState = "on"
	if err := registry.Put(context.Background(), &seed); err != nil {
		t.Fatal(err)
	}
	path := "/v1/resources/" + srv.Key.CRID + "/sharing"
	srv.Script(http.MethodGet, path,
		sharingResponse(t, srv, "on", 4, "serving"),
		sharingResponse(t, srv, "on", 5, "connecting"),
		sharingResponse(t, srv, "on", 5, "serving"),
	)
	srv.Script(http.MethodPost, path+"/restart", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteProblem(t, w, http.StatusServiceUnavailable, "unavailable", "Response uncertain", "restart result is ambiguous")
	})
	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "publish", "http://127.0.0.1:4000"},
		env:           map[string]string{"QURL_API_KEY": testAPIKey},
		shareRegistry: registry, shareDaemon: &recordingShareDaemon{}, shareStateDir: stateDir,
		preflightTarget: func(context.Context, string, int) error { return nil },
		localResource:   resolvedLocalResource(srv, true),
	})
	if res.code != 0 {
		t.Fatalf("result code=%d stderr=%s", res.code, res.stderr.String())
	}
	posts := 0
	for _, request := range srv.Requests() {
		if request.Method == http.MethodPost {
			posts++
		}
	}
	if posts != 1 {
		t.Fatalf("target-change restart POSTs=%d, want one", posts)
	}
	local, err := registry.Get(context.Background(), srv.Key.CRID)
	if err != nil || local.ServingEpoch != 5 || local.TargetURL != "http://127.0.0.1:4000" {
		t.Fatalf("target-change local state = %+v err=%v", local, err)
	}
}

func sharingResponse(t *testing.T, srv *apitest.Server, desired string, epoch uint64, connection string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
			"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
			"desired_state": desired, "serving_epoch": epoch, "connection_state": connection,
		}, nil)
	}
}

func TestLocalPublishPollingTimeoutLeavesDaemonOwnedRecoveryOn(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := t.TempDir()
	registry, err := connectorstate.OpenLocalShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/resources/" + srv.Key.CRID + "/sharing"
	connecting := func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
			"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
			"desired_state": "on", "serving_epoch": 5, "connection_state": "connecting",
		}, nil)
	}
	srv.Script(http.MethodGet, path, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
			"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
			"desired_state": "off", "serving_epoch": 4, "connection_state": "stopped",
		}, nil)
	})
	srv.Script(http.MethodPut, path, connecting)
	srv.ScriptRepeat(http.MethodGet, path, 4, connecting)
	resolver := func(_ context.Context, _ *connectorshare.NativeRuntimeConfig, resolveID func(string) (string, error)) (*agent.ResolvedResource, error) {
		id, err := resolveID("agent-one")
		if err != nil {
			return nil, err
		}
		found := false
		return &agent.ResolvedResource{Resource: &qurl.ConnectorResource{
			ResourceID: srv.Key.ResourceID, CRID: srv.Key.CRID, Slug: id,
			ConnectorRoutingID: "c-" + strings.Repeat("a", 52), KnockResourceID: "q_catalog_key",
		}, FoundExisting: &found}, nil
	}
	daemon := &recordingShareDaemon{}
	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "publish", "http://127.0.0.1:3000"},
		env:           map[string]string{"QURL_API_KEY": testAPIKey},
		shareRegistry: registry, shareDaemon: daemon, shareStateDir: stateDir,
		localResource: resolver, sharingWaitLimit: 5 * time.Millisecond,
		preflightTarget: func(context.Context, string, int) error { return nil },
	})
	if res.code == 0 || !strings.Contains(res.stderr.String(), "deadline exceeded") {
		t.Fatalf("result code=%d stderr=%s", res.code, res.stderr.String())
	}
	puts := 0
	for _, request := range srv.Requests() {
		if request.Method == http.MethodPut {
			puts++
		}
	}
	if puts != 1 {
		t.Fatalf("PUT requests = %d, want on only with no post-ownership compensation", puts)
	}
	local, err := registry.Get(context.Background(), srv.Key.CRID)
	if err != nil || local.DesiredState != "on" || daemon.ensures != 1 {
		t.Fatalf("daemon-owned recovery state = %+v err=%v ensures=%d", local, err, daemon.ensures)
	}
}

func TestLocalPublishWarmRuntimeFailureEmitsNoIdentityOrManagementMutation(t *testing.T) {
	srv := apitest.NewServer(t)
	want := errors.New("assigned NHP cell unavailable")
	providerPresent := false
	res := runCLI(t, &runOpts{
		args:            []string{"--endpoint", srv.URL, "publish", "http://127.0.0.1:3000", "--id", "warm-local"},
		env:             map[string]string{},
		preflightTarget: func(context.Context, string, int) error { return nil },
		localResource: func(_ context.Context, cfg *connectorshare.NativeRuntimeConfig, _ func(string) (string, error)) (*agent.ResolvedResource, error) {
			providerPresent = cfg.EnrollmentCredentialProvider != nil
			// A warm open deliberately does not invoke this provider.
			return nil, want
		},
	})
	if res.code == 0 || !strings.Contains(res.stderr.String(), want.Error()) {
		t.Fatalf("result code=%d stderr=%s", res.code, res.stderr.String())
	}
	if !providerPresent {
		t.Fatal("publish omitted the lazy first-enrollment provider")
	}
	if res.stdout.Len() != 0 {
		t.Fatalf("pre-admission failure emitted identity: %q", res.stdout.String())
	}
	if requests := srv.Requests(); len(requests) != 0 {
		t.Fatalf("warm runtime failure sent management requests: %#v", requests)
	}
}

func TestPublishDaemonLifecycleServesRealHTTPAndStopsCleanly(t *testing.T) {
	if testing.Short() {
		t.Skip("real FRP daemon journey")
	}
	const echoBody = "daemon-lifecycle-roundtrip"
	slowStarted := make(chan struct{})
	releaseSlow := make(chan struct{})
	var slowOnce sync.Once
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			slowOnce.Do(func() { close(slowStarted) })
			<-releaseSlow
		}
		_, _ = io.WriteString(w, echoBody)
	}))
	t.Cleanup(echo.Close)

	srv := apitest.NewServer(t)
	stateDir := t.TempDir()
	registry, err := connectorstate.OpenLocalShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	routingID := "c-" + strings.Repeat("a", 52)
	frpsPort := reserveCmdTCPPort(t)
	vhostPort := reserveCmdTCPPort(t)
	recorder := newCmdProxyRecorder(t)
	startCmdFRPS(t, frpsPort, vhostPort, "hermetic.test", recorder.server.URL)
	admitter := &journeyAdmitter{
		host: "localhost:" + strconv.Itoa(frpsPort), openTime: 2 * time.Second,
		serving: make(chan struct{}),
	}
	daemon := &journeyDaemon{registry: registry, admitter: admitter, version: "test"}
	t.Cleanup(daemon.close)

	sharingPath := "/v1/resources/" + srv.Key.CRID + "/sharing"
	srv.Script(http.MethodGet, sharingPath, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
			"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
			"desired_state": "off", "serving_epoch": 0, "connection_state": "stopped",
		}, nil)
	})
	srv.Script(http.MethodPut, sharingPath, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
			"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
			"desired_state": "on", "serving_epoch": 1, "connection_state": "connecting",
		}, nil)
	})
	srv.Script(http.MethodGet, sharingPath, func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-admitter.serving:
		case <-time.After(10 * time.Second):
			t.Error("real FRP proxy never reached running")
		}
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
			"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
			"desired_state": "on", "serving_epoch": 1, "connection_state": "serving",
		}, nil)
	})
	resolver := func(_ context.Context, _ *connectorshare.NativeRuntimeConfig, resolveID func(string) (string, error)) (*agent.ResolvedResource, error) {
		id, err := resolveID("agent-journey")
		if err != nil {
			return nil, err
		}
		found := false
		return &agent.ResolvedResource{Resource: &qurl.ConnectorResource{
			ResourceID: srv.Key.ResourceID, CRID: srv.Key.CRID, Slug: id,
			ConnectorRoutingID: routingID, KnockResourceID: "q_catalog_key",
		}, FoundExisting: &found}, nil
	}
	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "--quiet", "publish", echo.URL},
		env:           map[string]string{"QURL_API_KEY": testAPIKey},
		shareRegistry: registry, shareDaemon: daemon, shareStateDir: stateDir,
		localResource: resolver, sharingWaitLimit: 10 * time.Second,
	})
	if res.code != 0 || res.stdout.String() != srv.Key.CRID+"\n" {
		t.Fatalf("publish result code=%d stdout=%q stderr=%s", res.code, res.stdout.String(), res.stderr.String())
	}
	if len(recorder.snapshot()) == 0 {
		t.Fatal("real FRPS observed no NewProxy")
	}
	publicURL := "http://127.0.0.1:" + strconv.Itoa(vhostPort)
	requestPath := func(path string, timeout time.Duration) (string, error) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, publicURL+path, http.NoBody)
		if err != nil {
			return "", err
		}
		req.Host = routingID + ".hermetic.test"
		response, requestErr := (&http.Client{Timeout: timeout}).Do(req)
		if requestErr != nil {
			return "", requestErr
		}
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		return string(body), readErr
	}
	requestRoute := func() bool {
		body, err := requestPath("/", 300*time.Millisecond)
		return err == nil && body == echoBody
	}
	deadline := time.Now().Add(5 * time.Second)
	for !requestRoute() {
		if time.Now().After(deadline) {
			t.Fatal("public qURL route did not deliver local HTTP bytes")
		}
		time.Sleep(20 * time.Millisecond)
	}
	observations := recorder.snapshot()
	if len(observations) != 1 || observations[0].op != "NewProxy" {
		t.Fatalf("expected one initial proxy before rotation, got %#v", observations)
	}
	oldProxyName := observations[0].proxyName
	type slowResult struct {
		body string
		err  error
	}
	slowDone := make(chan slowResult, 1)
	go func() {
		body, err := requestPath("/slow", 8*time.Second)
		slowDone <- slowResult{body: body, err: err}
	}()
	select {
	case <-slowStarted:
	case <-time.After(time.Second):
		t.Fatal("in-flight request never reached the old proxy")
	}

	// Wait for the replacement's authoritative NewProxy admission while the
	// original request remains in flight on the old session.
	replacementDeadline := time.Now().Add(3 * time.Second)
	for {
		newProxies := 0
		for _, observation := range recorder.snapshot() {
			if observation.op == "NewProxy" {
				newProxies++
			}
		}
		if newProxies >= 2 {
			break
		}
		if time.Now().After(replacementDeadline) {
			t.Fatalf("replacement proxy was not admitted; observations=%#v", recorder.snapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !requestRoute() {
		t.Fatal("new request did not route through the serving replacement")
	}
	close(releaseSlow)
	select {
	case result := <-slowDone:
		if result.err != nil || result.body != echoBody {
			t.Fatalf("in-flight old-session request was interrupted: body=%q err=%v", result.body, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight old-session request did not complete during drain")
	}

	// Keep real traffic flowing across two short NHP admission rotations. A
	// request may retry within one bounded in-flight TCP window; a route gap
	// beyond that fails the journey.
	requestAcrossRotation := func() bool {
		retryUntil := time.Now().Add(300 * time.Millisecond)
		for {
			if requestRoute() {
				return true
			}
			if time.Now().After(retryUntil) {
				return false
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	rotationDeadline := time.Now().Add(6 * time.Second)
	for admitter.admissions() < 3 {
		if requestAcrossRotation() {
			if time.Now().After(rotationDeadline) {
				t.Fatalf("NHP sessions did not rotate twice; admissions=%d", admitter.admissions())
			}
			time.Sleep(40 * time.Millisecond)
			continue
		}
		t.Fatalf("public route dropped across make-before-break rotation; admissions=%d", admitter.admissions())
	}
	closedOld := false
	for _, observation := range recorder.snapshot() {
		if observation.op == "CloseProxy" && observation.proxyName == oldProxyName {
			closedOld = true
			break
		}
	}
	if !closedOld {
		t.Fatalf("old proxy %q was not unregistered after drain: %#v", oldProxyName, recorder.snapshot())
	}

	srv.Script(http.MethodPut, sharingPath, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
			"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
			"desired_state": "off", "serving_epoch": 2, "connection_state": "stopped",
		}, nil)
	})
	stop := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "stop", srv.Key.CRID},
		env:           map[string]string{"QURL_API_KEY": testAPIKey},
		shareRegistry: registry, shareDaemon: daemon, shareStateDir: stateDir,
	})
	if stop.code != 0 {
		t.Fatalf("stop result code=%d stderr=%s", stop.code, stop.stderr.String())
	}
	deadline = time.Now().Add(5 * time.Second)
	for len(daemon.manager.Running()) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if running := daemon.manager.Running(); len(running) != 0 {
		t.Fatalf("stop left daemon sessions running: %#v", running)
	}
}

func TestDaemonServesTwoResourcesAndStopsOneIndependently(t *testing.T) {
	if testing.Short() {
		t.Skip("real FRP multi-resource journey")
	}
	newEcho := func(body string) (*httptest.Server, int) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
		t.Cleanup(server.Close)
		parsed, err := url.Parse(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		port, err := strconv.Atoi(parsed.Port())
		if err != nil {
			t.Fatal(err)
		}
		return server, port
	}
	echoA, portA := newEcho("resource-a")
	echoB, portB := newEcho("resource-b")
	keyA := apitest.GenerateResourceKey(t)
	keyB := apitest.GenerateResourceKey(t)
	routingA := "c-" + strings.Repeat("a", 52)
	routingBDigest := sha256.Sum256([]byte("routing-b"))
	routingB := "c-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(routingBDigest[:]))
	stateDir := t.TempDir()
	registry, err := connectorstate.OpenLocalShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, share := range []connectorstate.LocalShare{
		{CRID: keyA.CRID, ResourceID: keyA.ResourceID, ConnectorID: "local-a", ConnectorRoutingID: routingA, KnockResourceID: "q_key_a", TargetURL: echoA.URL, LocalIP: "127.0.0.1", LocalPort: portA, DesiredState: "on", ServingEpoch: 1},
		{CRID: keyB.CRID, ResourceID: keyB.ResourceID, ConnectorID: "local-b", ConnectorRoutingID: routingB, KnockResourceID: "q_key_b", TargetURL: echoB.URL, LocalIP: "127.0.0.1", LocalPort: portB, DesiredState: "on", ServingEpoch: 1},
	} {
		if err := registry.Put(context.Background(), &share); err != nil {
			t.Fatal(err)
		}
	}
	frpsPort := reserveCmdTCPPort(t)
	vhostPort := reserveCmdTCPPort(t)
	recorder := newCmdProxyRecorder(t)
	startCmdFRPS(t, frpsPort, vhostPort, "hermetic.test", recorder.server.URL)
	admitter := &journeyAdmitter{host: "localhost:" + strconv.Itoa(frpsPort), serving: make(chan struct{})}
	common, err := connectordaemon.DefaultFRPCommon(2, 5)
	if err != nil {
		t.Fatal(err)
	}
	factory, err := connectordaemon.NewNativeSessionFactory(admitter, common, "test")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := connectordaemon.NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	requestRoute := func(routing, body string) bool {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(vhostPort)+"/", http.NoBody)
		req.Host = routing + ".hermetic.test"
		response, err := (&http.Client{Timeout: 300 * time.Millisecond}).Do(req)
		if err != nil {
			return false
		}
		data, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		return err == nil && string(data) == body
	}
	waitRoute := func(routing, body string) {
		deadline := time.Now().Add(5 * time.Second)
		for !requestRoute(routing, body) {
			if time.Now().After(deadline) {
				t.Fatalf("route %s did not serve %q", routing, body)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitRoute(routingA, "resource-a")
	waitRoute(routingB, "resource-b")
	if len(recorder.snapshot()) < 2 {
		t.Fatalf("FRPS NewProxy observations = %#v, want two resources", recorder.snapshot())
	}
	if _, err := registry.SetDesired(context.Background(), keyA.ResourceID, "off", 2); err != nil {
		t.Fatal(err)
	}
	manager.Trigger()
	deadline := time.Now().Add(5 * time.Second)
	for {
		running := manager.Running()
		if len(running) == 1 && running[keyB.ResourceID] == keyB.CRID {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stop-one running set = %#v", running)
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitRoute(routingB, "resource-b")
}

func TestForegroundPublishStartupFailureStopsOwnedSharing(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := t.TempDir()
	registry, err := connectorstate.OpenLocalShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/resources/" + srv.Key.CRID + "/sharing"
	srv.Script(http.MethodGet, path, sharingResponse(t, srv, "off", 0, "stopped"))
	srv.Script(http.MethodPut, path,
		sharingResponse(t, srv, "on", 1, "connecting"),
		sharingResponse(t, srv, "off", 2, "stopped"),
	)
	want := errors.New("foreground daemon startup failed")
	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "publish", "http://127.0.0.1:3000", "--foreground"},
		env:           map[string]string{"QURL_API_KEY": testAPIKey},
		shareRegistry: registry, shareStateDir: stateDir,
		preflightTarget: func(context.Context, string, int) error { return nil },
		localResource:   resolvedLocalResource(srv, false),
		foregroundDaemon: func(context.Context, *globalOpts, string, string) error {
			return want
		},
	})
	if res.code == 0 || !strings.Contains(res.stderr.String(), want.Error()) {
		t.Fatalf("result code=%d stderr=%s", res.code, res.stderr.String())
	}
	requests := srv.Requests()
	if len(requests) != 3 || requests[0].Method != http.MethodGet || requests[1].Method != http.MethodPut || requests[2].Method != http.MethodPut {
		t.Fatalf("lifecycle requests = %#v, want GET, on PUT, foreground-owned off PUT", requests)
	}
	local, err := registry.Get(context.Background(), srv.Key.CRID)
	if err != nil || local.DesiredState != "off" || local.ServingEpoch != 2 {
		t.Fatalf("foreground startup cleanup = %+v err=%v", local, err)
	}
}

func TestForegroundPublishCancellationDrainsAndStopsOwnedSharing(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir, err := os.MkdirTemp("/tmp", "qurl-fg-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	registry, err := connectorstate.OpenLocalShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/resources/" + srv.Key.CRID + "/sharing"
	srv.Script(http.MethodGet, path, sharingResponse(t, srv, "off", 0, "stopped"))
	srv.Script(http.MethodPut, path,
		sharingResponse(t, srv, "on", 1, "connecting"),
		sharingResponse(t, srv, "off", 2, "stopped"),
	)
	srv.Script(http.MethodGet, path, sharingResponse(t, srv, "on", 1, "serving"))
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	stopped := make(chan struct{})
	done := make(chan *runResult, 1)
	go func() {
		done <- runCLI(t, &runOpts{
			ctx:           ctx,
			args:          []string{"--endpoint", srv.URL, "publish", "http://127.0.0.1:3000", "--foreground"},
			env:           map[string]string{"QURL_API_KEY": testAPIKey},
			shareRegistry: registry, shareStateDir: stateDir,
			preflightTarget:  func(context.Context, string, int) error { return nil },
			localResource:    resolvedLocalResource(srv, false),
			foregroundDaemon: foregroundIPCTestDaemon(started, stopped),
		})
	}()
	select {
	case <-started:
	case res := <-done:
		t.Fatalf("foreground publish exited before IPC bind: code=%d stdout=%s stderr=%s", res.code, res.stdout.String(), res.stderr.String())
	case <-time.After(2 * time.Second):
		t.Fatal("foreground daemon did not bind IPC")
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(srv.Requests()) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("foreground publish did not reach serving; requests=%#v", srv.Requests())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	var res *runResult
	select {
	case res = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("foreground publish did not return after cancellation")
	}
	if res.code == 0 {
		t.Fatalf("canceled foreground publish exit=0 stdout=%s stderr=%s", res.stdout.String(), res.stderr.String())
	}
	select {
	case <-stopped:
	default:
		t.Fatal("foreground daemon was not joined before command returned")
	}
	requests := srv.Requests()
	if len(requests) != 4 || requests[3].Method != http.MethodPut {
		t.Fatalf("lifecycle requests = %#v, want final foreground-owned off PUT", requests)
	}
	local, err := registry.Get(context.Background(), srv.Key.CRID)
	if err != nil || local.DesiredState != "off" || local.ServingEpoch != 2 {
		t.Fatalf("foreground cancellation cleanup = %+v err=%v", local, err)
	}
}

func resolvedLocalResource(srv *apitest.Server, found bool) localResourceResolver {
	return func(context.Context, *connectorshare.NativeRuntimeConfig, func(string) (string, error)) (*agent.ResolvedResource, error) {
		return &agent.ResolvedResource{Resource: &qurl.ConnectorResource{
			ResourceID: srv.Key.ResourceID, CRID: srv.Key.CRID, Slug: "local-test",
			ConnectorRoutingID: "c-" + strings.Repeat("a", 52), KnockResourceID: "q_catalog_key",
		}, FoundExisting: &found}, nil
	}
}

func foregroundIPCTestDaemon(started, stopped chan struct{}) func(context.Context, *globalOpts, string, string) error {
	return func(ctx context.Context, _ *globalOpts, stateDir, jobVersion string) error {
		defer close(stopped)
		socket := filepath.Join(stateDir, connectordaemon.SocketFile)
		var listenConfig net.ListenConfig
		listener, err := listenConfig.Listen(ctx, "unix", socket)
		if err != nil {
			return err
		}
		defer func() {
			_ = listener.Close()
			_ = os.Remove(socket)
		}()
		if err := os.Chmod(socket, 0o600); err != nil {
			return err
		}
		server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/status" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"job_version":%q,"running":{}}`, jobVersion)
		})}
		serveDone := make(chan error, 1)
		go func() { serveDone <- server.Serve(listener) }()
		close(started)
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			return errors.Join(ctx.Err(), server.Shutdown(shutdownCtx))
		case err := <-serveDone:
			return err
		}
	}
}

func localShareFixture(srv *apitest.Server) connectorstate.LocalShare {
	return connectorstate.LocalShare{
		CRID: srv.Key.CRID, ResourceID: srv.Key.ResourceID,
		ConnectorID: "local-test", ConnectorRoutingID: "c-" + strings.Repeat("a", 52),
		KnockResourceID: "q_catalog_key", TargetURL: "http://127.0.0.1:3000",
		LocalIP: "127.0.0.1", LocalPort: 3000, DesiredState: "off", ServingEpoch: 4,
	}
}
