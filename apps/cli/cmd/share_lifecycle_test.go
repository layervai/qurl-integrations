package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	qurl "github.com/layervai/qurl-go/qurl"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

type recordingShareDaemon struct {
	ensures   int
	reloads   int
	ensureErr error
	reloadErr error
}

type diagnosticDaemonRegistry struct{ share connectorstate.LocalShare }

func (r *diagnosticDaemonRegistry) List(context.Context) ([]connectorstate.LocalShare, error) {
	return []connectorstate.LocalShare{r.share}, nil
}

func (r *diagnosticDaemonRegistry) DisableAtCurrentEpoch(context.Context, string, uint64) (*connectorstate.LocalShare, error) {
	return &r.share, nil
}

type diagnosticDaemonSession struct {
	done               chan struct{}
	stop               sync.Once
	mu                 sync.Mutex
	diagnostic         connectordaemon.ResourceDiagnostic
	diagnosticSequence []connectordaemon.ResourceDiagnostic
	diagnosticCalls    int
}

func (s *diagnosticDaemonSession) Done() <-chan struct{} { return s.done }
func (*diagnosticDaemonSession) Err() error              { return nil }
func (s *diagnosticDaemonSession) Stop(context.Context) error {
	s.stop.Do(func() { close(s.done) })
	return nil
}
func (s *diagnosticDaemonSession) Diagnostic() connectordaemon.ResourceDiagnostic {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.diagnosticSequence) > 0 {
		index := min(s.diagnosticCalls, len(s.diagnosticSequence)-1)
		s.diagnosticCalls++
		return s.diagnosticSequence[index]
	}
	return s.diagnostic
}

func (s *diagnosticDaemonSession) setDiagnosticSequence(sequence ...connectordaemon.ResourceDiagnostic) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.diagnosticSequence = slices.Clone(sequence)
	s.diagnosticCalls = 0
}

func (s *diagnosticDaemonSession) diagnosticCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.diagnosticCalls
}

type diagnosticDaemonFactory struct{ session *diagnosticDaemonSession }

func (f diagnosticDaemonFactory) Start(context.Context, *connectorstate.LocalShare) (connectordaemon.Session, error) {
	return f.session, nil
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
	return true, d.reloadErr
}

type sharingErrorClient struct {
	qurlapi.Client
	err error
}

func (c sharingErrorClient) Sharing(ctx context.Context, _ string) (*qurlapi.Sharing, error) {
	<-ctx.Done()
	return nil, c.err
}

type restartReconcileClient struct {
	qurlapi.Client
	restartErr error
	restarted  *qurlapi.Sharing
	current    *qurlapi.Sharing
	sharingErr error
}

func (c restartReconcileClient) RestartSharing(context.Context, string) (*qurlapi.Sharing, error) {
	return c.restarted, c.restartErr
}

func (c restartReconcileClient) Sharing(context.Context, string) (*qurlapi.Sharing, error) {
	return c.current, c.sharingErr
}

type setSharingClient struct {
	qurlapi.Client
	off         *qurlapi.Sharing
	requestedID string
}

func (c *setSharingClient) SetSharing(_ context.Context, id string, _ qurlapi.DesiredState) (*qurlapi.Sharing, error) {
	c.requestedID = id
	return c.off, nil
}

func TestCompensateShareChangeUsesOnlyTrustedLocalIdentity(t *testing.T) {
	for _, test := range []struct {
		name    string
		sharing *qurlapi.Sharing
	}{
		{name: "nil rejected response"},
		{name: "mismatched rejected response", sharing: &qurlapi.Sharing{ResourceID: "wrong-resource", CRID: "wrong-crid"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			registry, err := openOwnedTestShareRegistry(connectorStateTestDir(t))
			if err != nil {
				t.Fatal(err)
			}
			local := localShareFixture(srv)
			local.DesiredState = "on"
			local.ServingEpoch = 4
			if err := registry.Put(context.Background(), &local); err != nil {
				t.Fatal(err)
			}
			path := "/v1/resources/" + srv.Key.CRID + "/sharing"
			srv.Script(http.MethodPut, path, sharingResponse(t, srv, "off", 5, "stopped"))
			client, err := qurlapi.New(&qurlapi.Config{BaseURL: srv.URL, APIKey: testAPIKey, Version: "compensation-test"})
			if err != nil {
				t.Fatal(err)
			}
			cause := errors.New("rejected lifecycle response")
			err = compensateShareChange(cause, true, client, registry, &local, test.sharing)
			if !errors.Is(err, cause) {
				t.Fatalf("compensation error = %v, want original cause", err)
			}
			requests := srv.Requests()
			if len(requests) != 1 || requests[0].Method != http.MethodPut || requests[0].Path != path {
				t.Fatalf("compensation requests = %#v, want trusted local path %s", requests, path)
			}
			stored, err := registry.Get(context.Background(), local.ResourceID)
			if err != nil || stored.DesiredState != "off" || stored.ServingEpoch != 5 {
				t.Fatalf("compensated local state = %+v, %v", stored, err)
			}
		})
	}
}

func TestCompensateShareChangeRejectsUntrustedOffEpoch(t *testing.T) {
	srv := apitest.NewServer(t)
	registry, err := openOwnedTestShareRegistry(connectorStateTestDir(t))
	if err != nil {
		t.Fatal(err)
	}
	local := localShareFixture(srv)
	local.DesiredState = "on"
	local.ServingEpoch = 4
	if err := registry.Put(context.Background(), &local); err != nil {
		t.Fatal(err)
	}
	other := apitest.GenerateResourceKey(t)
	client := &setSharingClient{off: &qurlapi.Sharing{
		ResourceID: other.ResourceID, CRID: other.CRID, DesiredState: qurlapi.DesiredStateOff,
		ServingEpoch: 999, ConnectionState: qurlapi.ConnectionStopped,
	}}
	cause := errors.New("rejected lifecycle response")
	err = compensateShareChange(cause, true, client, registry, &local, nil)
	if client.requestedID != local.CRID {
		t.Fatalf("compensation requested %q, want trusted local CRID %q", client.requestedID, local.CRID)
	}
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "response identity does not match") {
		t.Fatalf("compensation error = %v, want cause plus identity rejection", err)
	}
	stored, err := registry.Get(context.Background(), local.ResourceID)
	if err != nil || stored.DesiredState != "on" || stored.ServingEpoch != 4 {
		t.Fatalf("untrusted compensation changed local state = %+v, %v", stored, err)
	}
}

func TestRestartReconciliationRejectsEmptyAuthoritativeState(t *testing.T) {
	restartErr := errors.New("restart response lost")
	_, err := restartSharingReconciled(context.Background(), restartReconcileClient{restartErr: restartErr}, "trusted-crid", &qurlapi.Sharing{
		ResourceID: "trusted-resource", CRID: "trusted-crid", DesiredState: qurlapi.DesiredStateOn, ServingEpoch: 4,
	})
	if !errors.Is(err, restartErr) || !strings.Contains(err.Error(), "authoritative state did not advance") ||
		!strings.Contains(err.Error(), "resulting sharing state is empty") {
		t.Fatalf("restart reconciliation error = %v, want safe ambiguity error", err)
	}
}

func TestRestartSuccessfulResponseRequiresExactEpochAdvance(t *testing.T) {
	prior := &qurlapi.Sharing{
		ResourceID: "trusted-resource", CRID: "trusted-crid",
		DesiredState: qurlapi.DesiredStateOn, ServingEpoch: 4,
	}
	tests := []struct {
		name    string
		result  *qurlapi.Sharing
		wantErr bool
	}{
		{name: "same epoch", result: &qurlapi.Sharing{
			ResourceID: prior.ResourceID, CRID: prior.CRID,
			DesiredState: qurlapi.DesiredStateOn, ServingEpoch: 4,
		}, wantErr: true},
		{name: "lower epoch", result: &qurlapi.Sharing{
			ResourceID: prior.ResourceID, CRID: prior.CRID,
			DesiredState: qurlapi.DesiredStateOn, ServingEpoch: 3,
		}, wantErr: true},
		{name: "identity mismatch", result: &qurlapi.Sharing{
			ResourceID: "other-resource", CRID: prior.CRID,
			DesiredState: qurlapi.DesiredStateOn, ServingEpoch: 5,
		}, wantErr: true},
		{name: "CRID mismatch", result: &qurlapi.Sharing{
			ResourceID: prior.ResourceID, CRID: "other-crid",
			DesiredState: qurlapi.DesiredStateOn, ServingEpoch: 5,
		}, wantErr: true},
		{name: "wrong desired state", result: &qurlapi.Sharing{
			ResourceID: prior.ResourceID, CRID: prior.CRID,
			DesiredState: qurlapi.DesiredStateOff, ServingEpoch: 5,
		}, wantErr: true},
		{name: "advanced", result: &qurlapi.Sharing{
			ResourceID: prior.ResourceID, CRID: prior.CRID,
			DesiredState: qurlapi.DesiredStateOn, ServingEpoch: 5,
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := restartSharingReconciled(context.Background(), restartReconcileClient{restarted: test.result}, prior.CRID, prior)
			if test.wantErr {
				if !errors.Is(err, qurl.ErrInvalidAPIResponse) || got != nil {
					t.Fatalf("restart result = %+v, %v; want nil and ErrInvalidAPIResponse", got, err)
				}
				return
			}
			if err != nil || got != test.result {
				t.Fatalf("restart result = %+v, %v; want accepted advance", got, err)
			}
		})
	}
}

func TestRestartInvalidSameEpochResponseCompensatesOffBeforeDaemonHandoff(t *testing.T) {
	for _, priorDesired := range []string{"off", "on"} {
		t.Run("prior "+priorDesired, func(t *testing.T) {
			srv := apitest.NewServer(t)
			stateDir := connectorStateTestDir(t)
			registry, err := openOwnedTestShareRegistry(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			local := localShareFixture(srv)
			local.DesiredState = priorDesired
			if err := registry.Put(context.Background(), &local); err != nil {
				t.Fatal(err)
			}
			path := "/v1/resources/" + srv.Key.CRID + "/sharing"
			srv.Script(http.MethodGet, path, sharingResponse(t, srv, priorDesired, local.ServingEpoch, map[string]string{
				"off": "stopped", "on": "serving",
			}[priorDesired]))
			srv.Script(http.MethodPost, path+"/restart", sharingResponse(t, srv, "on", local.ServingEpoch, "connecting"))
			srv.Script(http.MethodPut, path, func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					DesiredState string `json:"desired_state"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode compensation request: %v", err)
				}
				if body.DesiredState != "off" {
					t.Errorf("compensation desired_state = %q, want off", body.DesiredState)
				}
				sharingResponse(t, srv, "off", local.ServingEpoch, "stopped")(w, r)
			})
			daemon := &recordingShareDaemon{}
			res := runCLI(t, &runOpts{
				args: []string{"--endpoint", srv.URL, "restart", srv.Key.CRID},
				env: map[string]string{
					"QURL_API_KEY": testAPIKey, "QURL_CONNECTOR_STATE_DIR": stateDir,
				},
				shareRegistry: registry, shareDaemon: daemon, shareStateDir: stateDir,
				preflightTarget: func(context.Context, string, int) error { return nil },
			})
			if res.code == 0 || !strings.Contains(res.stderr.String(), "serving epoch") {
				t.Fatalf("invalid restart result code=%d stdout=%s stderr=%s", res.code, res.stdout.String(), res.stderr.String())
			}
			requests := srv.Requests()
			if len(requests) != 3 || requests[0].Method != http.MethodGet || requests[1].Method != http.MethodPost || requests[2].Method != http.MethodPut {
				t.Fatalf("restart compensation requests = %#v, want GET, POST, PUT", requests)
			}
			stored, err := registry.Get(context.Background(), local.ResourceID)
			if err != nil || stored.DesiredState != "off" || stored.ServingEpoch != local.ServingEpoch {
				t.Fatalf("restart compensation local state = %+v, %v", stored, err)
			}
			if daemon.ensures != 0 || daemon.reloads != 0 {
				t.Fatalf("invalid restart reached daemon handoff: %+v", daemon)
			}
		})
	}
}

func TestWaitForSharingReportsLastObservedStateWhenRequestUsesDeadline(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/resources/"+srv.Key.CRID+"/sharing", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
			"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "desired_state": "on",
			"connection_state": "connecting", "serving_epoch": 7,
		}, nil)
	})
	srv.Script(http.MethodGet, "/v1/resources/"+srv.Key.CRID+"/sharing", func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	client, err := qurlapi.New(&qurlapi.Config{BaseURL: srv.URL, APIKey: testAPIKey, Version: "wait-timeout-test"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = waitForSharing(context.Background(), client, &connectorstate.LocalShare{
		ResourceID: srv.Key.ResourceID, CRID: srv.Key.CRID,
	}, 7, 250*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait error = %v, want context deadline classification", err)
	}
	for _, want := range []string{"did not start serving within 250ms", "desired=on", "connection=connecting", "serving_epoch=7"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("wait error = %q, want %q", err, want)
		}
	}
}

func TestWaitForSharingIncludesRedactedDaemonRootCause(t *testing.T) {
	stateDir := connectorStateTestDir(t)
	now := time.Now().UTC()
	next := now.Add(time.Second)
	local := connectorstate.LocalShare{
		ResourceID: "resource-a", CRID: "crid-a", DesiredState: "on", ServingEpoch: 1,
	}
	session := &diagnosticDaemonSession{done: make(chan struct{}), diagnostic: connectordaemon.ResourceDiagnostic{
		State: "retrying", LastTransition: now, FailureCategory: "platform_denied",
		FailureCode: "52005", RetryAttempt: 3, NextRetryAt: &next,
	}}
	manager, err := connectordaemon.NewManager(&diagnosticDaemonRegistry{share: local}, diagnosticDaemonFactory{session: session})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	server := &connectordaemon.IPCServer{
		SocketPath: connectordaemon.StateSocketPath(stateDir), Manager: manager, JobVersion: "1/test",
	}
	go func() { done <- server.Run(ctx) }()
	readyCtx, readyCancel := context.WithTimeout(context.Background(), time.Second)
	if err := (connectordaemon.IPCClient{SocketPath: connectordaemon.StateSocketPath(stateDir)}).WaitReady(readyCtx); err != nil {
		readyCancel()
		cancel()
		t.Fatal(err)
	}
	readyCancel()
	_, err = waitForSharingWithDiagnostics(context.Background(), sharingErrorClient{err: errors.New("temporary poll failure")},
		&local, stateDir, 1, 10*time.Millisecond)
	for _, want := range []string{"failure category platform_denied", "failure code 52005", "retry attempt 3"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			cancel()
			t.Fatalf("diagnosed wait error = %v, want %q", err, want)
		}
	}
	cancel()
	if serverErr := <-done; !errors.Is(serverErr, context.Canceled) {
		t.Fatalf("daemon shutdown = %v", serverErr)
	}
}

func TestWaitForSharingSettlesStartingDiagnosticIntoRetryCause(t *testing.T) {
	stateDir := connectorStateTestDir(t)
	now := time.Now().UTC()
	next := now.Add(time.Second)
	local := connectorstate.LocalShare{
		ResourceID: "resource-a", CRID: "crid-a", DesiredState: "on", ServingEpoch: 1,
	}
	session := &diagnosticDaemonSession{done: make(chan struct{}), diagnostic: connectordaemon.ResourceDiagnostic{
		State: "starting", LastTransition: now,
	}}
	manager, err := connectordaemon.NewManager(&diagnosticDaemonRegistry{share: local}, diagnosticDaemonFactory{session: session})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	server := &connectordaemon.IPCServer{
		SocketPath: connectordaemon.StateSocketPath(stateDir), Manager: manager, JobVersion: "1/test",
	}
	go func() { done <- server.Run(ctx) }()
	readyCtx, readyCancel := context.WithTimeout(context.Background(), time.Second)
	if err := (connectordaemon.IPCClient{SocketPath: connectordaemon.StateSocketPath(stateDir)}).WaitReady(readyCtx); err != nil {
		readyCancel()
		cancel()
		t.Fatal(err)
	}
	readyCancel()
	session.setDiagnosticSequence(
		connectordaemon.ResourceDiagnostic{State: "starting", LastTransition: now},
		connectordaemon.ResourceDiagnostic{
			State: "retrying", LastTransition: now.Add(time.Millisecond), FailureCategory: "platform_denied",
			FailureCode: "52029", RetryAttempt: 1, NextRetryAt: &next,
		},
	)
	_, err = waitForSharingWithDiagnostics(context.Background(), sharingErrorClient{err: errors.New("temporary poll failure")},
		&local, stateDir, 1, 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "failure category platform_denied") ||
		!strings.Contains(err.Error(), "failure code 52029") || !strings.Contains(err.Error(), "retry attempt 1") {
		cancel()
		t.Fatalf("settled diagnostic error = %v, want the retry cause", err)
	}
	if calls := session.diagnosticCallCount(); calls < 2 {
		cancel()
		t.Fatalf("diagnostic samples = %d, want starting plus one bounded handoff sample", calls)
	}
	cancel()
	if serverErr := <-done; !errors.Is(serverErr, context.Canceled) {
		t.Fatalf("daemon shutdown = %v", serverErr)
	}
}

func TestWaitForSharingReportsMissingDaemonResourceDiagnostic(t *testing.T) {
	stateDir := connectorStateTestDir(t)
	manager, err := connectordaemon.NewManager(emptyForegroundRegistry{}, emptyForegroundFactory{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	server := &connectordaemon.IPCServer{
		SocketPath: connectordaemon.StateSocketPath(stateDir), Manager: manager, JobVersion: "1/test",
	}
	go func() { done <- server.Run(ctx) }()
	readyCtx, readyCancel := context.WithTimeout(context.Background(), time.Second)
	if err := (connectordaemon.IPCClient{SocketPath: connectordaemon.StateSocketPath(stateDir)}).WaitReady(readyCtx); err != nil {
		readyCancel()
		cancel()
		t.Fatal(err)
	}
	readyCancel()
	local := &connectorstate.LocalShare{ResourceID: "resource-a", CRID: "crid-a", ServingEpoch: 1}
	_, err = waitForSharingWithDiagnostics(context.Background(), sharingErrorClient{err: errors.New("temporary poll failure")},
		local, stateDir, 1, 10*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "daemon running, resource diagnostic absent") ||
		!strings.Contains(err.Error(), "temporary poll failure") {
		cancel()
		t.Fatalf("diagnosed wait error = %v, want redacted missing-resource root cause", err)
	}
	cancel()
	if serverErr := <-done; !errors.Is(serverErr, context.Canceled) {
		t.Fatalf("daemon shutdown = %v", serverErr)
	}
}

func TestWaitForSharingPreservesCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client, err := qurlapi.New(&qurlapi.Config{BaseURL: "https://cancel.invalid", APIKey: testAPIKey, Version: "wait-cancel-test"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = waitForSharing(ctx, client, &connectorstate.LocalShare{ResourceID: "resource-a", CRID: "crid-a"}, 1, time.Minute)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "did not start serving") {
		t.Fatalf("canceled wait error = %v, want unmodified caller cancellation", err)
	}
}

func TestWaitForSharingWithDiagnosticsPreservesCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pollErr := errors.New("poll interrupted")
	_, err := waitForSharingWithDiagnostics(ctx, sharingErrorClient{err: pollErr}, &connectorstate.LocalShare{
		ResourceID: "resource-a", CRID: "crid-a",
	}, connectorStateTestDir(t), 1, time.Minute)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, pollErr) || strings.Contains(err.Error(), "daemon state") {
		t.Fatalf("canceled diagnosed wait error = %v, want the unmodified caller cancellation", err)
	}
}

func TestWaitForSharingPreservesFinalPollErrorAtDeadline(t *testing.T) {
	pollErr := errors.New("final sharing poll failed")
	_, err := waitForSharing(
		context.Background(),
		sharingErrorClient{err: pollErr},
		&connectorstate.LocalShare{ResourceID: "resource-a", CRID: "crid-a"},
		1,
		10*time.Millisecond,
	)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, pollErr) {
		t.Fatalf("wait error = %v, want deadline and final poll causes", err)
	}
}

func TestWaitForSharingRetriesTransientReadButNotAuthenticationFailure(t *testing.T) {
	t.Run("service unavailable then serving", func(t *testing.T) {
		srv := apitest.NewServer(t)
		path := "/v1/resources/" + srv.Key.CRID + "/sharing"
		srv.Script(http.MethodGet, path,
			func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			},
			sharingResponse(t, srv, "on", 7, "serving"),
		)
		client, err := qurlapi.New(&qurlapi.Config{BaseURL: srv.URL, APIKey: testAPIKey, Version: "wait-retry-test"})
		if err != nil {
			t.Fatal(err)
		}
		sharing, err := waitForSharing(context.Background(), client, &connectorstate.LocalShare{
			ResourceID: srv.Key.ResourceID, CRID: srv.Key.CRID,
		}, 7, time.Second)
		if err != nil || sharing == nil || sharing.ConnectionState != qurlapi.ConnectionServing {
			t.Fatalf("wait result = %+v, %v", sharing, err)
		}
		if got := len(srv.Requests()); got != 2 {
			t.Fatalf("sharing requests = %d, want 2", got)
		}
	})

	t.Run("unauthorized fails immediately", func(t *testing.T) {
		srv := apitest.NewServer(t)
		path := "/v1/resources/" + srv.Key.CRID + "/sharing"
		srv.Script(http.MethodGet, path, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		})
		client, err := qurlapi.New(&qurlapi.Config{BaseURL: srv.URL, APIKey: testAPIKey, Version: "wait-auth-test"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = waitForSharing(context.Background(), client, &connectorstate.LocalShare{
			ResourceID: srv.Key.ResourceID, CRID: srv.Key.CRID,
		}, 7, time.Second)
		var apiErr *qurlapi.Error
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
			t.Fatalf("wait error = %v, want HTTP 401", err)
		}
		if got := len(srv.Requests()); got != 1 {
			t.Fatalf("sharing requests = %d, want 1", got)
		}
	})

	t.Run("invalid success response fails immediately", func(t *testing.T) {
		srv := apitest.NewServer(t)
		path := "/v1/resources/" + srv.Key.CRID + "/sharing"
		srv.Script(http.MethodGet, path, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":`))
		})
		client, err := qurlapi.New(&qurlapi.Config{BaseURL: srv.URL, APIKey: testAPIKey, Version: "wait-invalid-test"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = waitForSharing(context.Background(), client, &connectorstate.LocalShare{
			ResourceID: srv.Key.ResourceID, CRID: srv.Key.CRID,
		}, 7, time.Second)
		if !errors.Is(err, qurl.ErrInvalidAPIResponse) {
			t.Fatalf("wait error = %v, want invalid API response", err)
		}
		if got := len(srv.Requests()); got != 1 {
			t.Fatalf("sharing requests = %d, want 1", got)
		}
	})
}

func TestSharingPollDoesNotRetryPermanentTransportFailures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
	}{
		{
			name: "DNS name not found",
			err:  &net.DNSError{Err: "no such host", Name: "missing.invalid", IsNotFound: true},
		},
		{
			name: "TLS certificate rejected",
			err:  &tls.CertificateVerificationError{Err: errors.New("unknown certificate authority")},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			wrapped := fmt.Errorf("sharing request failed: %w", test.err)
			if retryableSharingPollError(wrapped) {
				t.Fatalf("retryableSharingPollError(%v) = true, want false", wrapped)
			}
		})
	}
}

func TestSharingPollDelayRampsWithBoundedCRIDJitter(t *testing.T) {
	if got := sharingPollDelay("qexample", 0); got != sharingPollInitialDelay {
		t.Fatalf("initial poll delay = %v, want %v", got, sharingPollInitialDelay)
	}
	previousFloor := sharingPollInitialDelay
	for attempt := 1; attempt <= 12; attempt++ {
		base := sharingPollInitialDelay
		for range attempt {
			if base >= sharingPollMaximumDelay/2 {
				base = sharingPollMaximumDelay
				break
			}
			base *= 2
		}
		got := sharingPollDelay("qexample", attempt)
		if got < base*80/100 || got > base || got < previousFloor*80/100 || got > sharingPollMaximumDelay {
			t.Fatalf("poll delay attempt %d = %v, base %v previous floor %v", attempt, got, base, previousFloor)
		}
		previousFloor = base
	}
	if sharingPollDelay("qexample", 5) == sharingPollDelay("qanother", 5) {
		t.Fatal("poll jitter did not vary across CRIDs")
	}
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
			stateDir := connectorStateTestDir(t)
			registry, err := openOwnedTestShareRegistry(stateDir)
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

func TestStopConvergesAnAuthoritativeIdempotentOffResponseAtTheCurrentEpoch(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	local := localShareFixture(srv)
	local.DesiredState = "on"
	if err := registry.Put(context.Background(), &local); err != nil {
		t.Fatal(err)
	}
	path := "/v1/resources/" + srv.Key.CRID + "/sharing"
	srv.Script(http.MethodPut, path, sharingResponse(t, srv, "off", local.ServingEpoch, "stopped"))
	daemon := &recordingShareDaemon{}
	res := runCLI(t, &runOpts{
		args: []string{"--endpoint", srv.URL, "stop", srv.Key.CRID},
		env: map[string]string{
			"QURL_API_KEY": testAPIKey, "QURL_CONNECTOR_STATE_DIR": stateDir,
		},
		shareRegistry: registry, shareDaemon: daemon, shareStateDir: stateDir,
	})
	if res.code != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", res.code, res.stdout.String(), res.stderr.String())
	}
	if strings.Contains(res.stderr.String(), "local sharing cleanup did not finish") {
		t.Fatalf("idempotent stop reported incomplete local cleanup: %s", res.stderr.String())
	}
	updated, err := registry.Get(context.Background(), local.ResourceID)
	if err != nil || updated.DesiredState != "off" || updated.ServingEpoch != local.ServingEpoch {
		t.Fatalf("idempotent local stop = %+v, %v", updated, err)
	}
	if daemon.reloads != 1 || daemon.ensures != 0 {
		t.Fatalf("idempotent stop daemon reconciliation = %+v, want one reload and no install", daemon)
	}
}

func TestStopReportsCommittedSuccessWhenDaemonReloadFails(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	local := localShareFixture(srv)
	if err := registry.Put(context.Background(), &local); err != nil {
		t.Fatal(err)
	}
	path := "/v1/resources/" + srv.Key.CRID + "/sharing"
	srv.Script(http.MethodPut, path, sharingResponse(t, srv, "off", local.ServingEpoch+1, "stopped"))
	reloadErr := errors.New("daemon IPC unavailable")
	daemon := &recordingShareDaemon{reloadErr: reloadErr}
	res := runCLI(t, &runOpts{
		args: []string{"--endpoint", srv.URL, "stop", srv.Key.CRID},
		env: map[string]string{
			"QURL_API_KEY": testAPIKey, "QURL_CONNECTOR_STATE_DIR": stateDir,
		},
		shareRegistry: registry, shareDaemon: daemon, shareStateDir: stateDir,
	})
	if res.code != 0 {
		t.Fatalf("exit=%d stderr=%s", res.code, res.stderr.String())
	}
	for _, want := range []string{srv.Key.CRID, "Desired:", "off", "Observed:", "stopped"} {
		if !strings.Contains(res.stdout.String(), want) {
			t.Fatalf("stop stdout=%q, want %q", res.stdout.String(), want)
		}
	}
	for _, want := range []string{"resource is stopped", "local sharing cleanup did not finish", "local session can remain", reloadErr.Error()} {
		if !strings.Contains(res.stderr.String(), want) {
			t.Fatalf("stop stderr=%q, want %q", res.stderr.String(), want)
		}
	}
	updated, err := registry.Get(context.Background(), local.ResourceID)
	if err != nil || updated.DesiredState != "off" || updated.ServingEpoch != local.ServingEpoch+1 {
		t.Fatalf("committed local stop = %+v, %v", updated, err)
	}
	if daemon.reloads != 1 || daemon.ensures != 0 {
		t.Fatalf("stop daemon reconciliation = %+v, want one reload and no install", daemon)
	}
}

func TestStopReportsCommittedSuccessWhenLocalRegistryUpdateFails(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	local := localShareFixture(srv)
	if err := registry.Put(context.Background(), &local); err != nil {
		t.Fatal(err)
	}
	path := "/v1/resources/" + srv.Key.CRID + "/sharing"
	srv.Script(http.MethodPut, path, sharingResponse(t, srv, "off", local.ServingEpoch+1, "stopped"))
	updateErr := errors.New("local registry unavailable")
	res := runCLI(t, &runOpts{
		args: []string{"--endpoint", srv.URL, "stop", srv.Key.CRID},
		env: map[string]string{
			"QURL_API_KEY": testAPIKey, "QURL_CONNECTOR_STATE_DIR": stateDir,
		},
		shareRegistry: &failNextSetDesiredRegistry{localShareRegistry: registry, err: updateErr},
		shareDaemon:   &recordingShareDaemon{}, shareStateDir: stateDir,
	})
	if res.code != 0 {
		t.Fatalf("exit=%d stderr=%s", res.code, res.stderr.String())
	}
	for _, want := range []string{srv.Key.CRID, local.TargetURL, "Desired:", "off"} {
		if !strings.Contains(res.stdout.String(), want) {
			t.Fatalf("stop stdout=%q, want %q", res.stdout.String(), want)
		}
	}
	for _, want := range []string{"resource is stopped", "local session can remain", updateErr.Error()} {
		if !strings.Contains(res.stderr.String(), want) {
			t.Fatalf("stop stderr=%q, want %q", res.stderr.String(), want)
		}
	}
	unchanged, err := registry.Get(context.Background(), local.ResourceID)
	if err != nil || unchanged.DesiredState != local.DesiredState || unchanged.ServingEpoch != local.ServingEpoch {
		t.Fatalf("failed local convergence mutated registry = %+v, %v", unchanged, err)
	}
}

func TestStopRejectsSharingResponseThatMismatchesLocalIdentity(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	local := localShareFixture(srv)
	if err := registry.Put(context.Background(), &local); err != nil {
		t.Fatal(err)
	}
	other := apitest.GenerateResourceKey(t)
	client := &setSharingClient{off: &qurlapi.Sharing{
		ResourceID: other.ResourceID, CRID: other.CRID, DesiredState: qurlapi.DesiredStateOff,
		ServingEpoch: local.ServingEpoch + 1, ConnectionState: qurlapi.ConnectionStopped,
	}}
	daemon := &recordingShareDaemon{}
	res := runCLI(t, &runOpts{
		args: []string{"--endpoint", srv.URL, "stop", srv.Key.CRID},
		env: map[string]string{
			"QURL_API_KEY": testAPIKey, "QURL_CONNECTOR_STATE_DIR": stateDir,
		},
		shareRegistry: registry, shareDaemon: daemon, shareStateDir: stateDir,
		openAPIClient: func(context.Context) (qurlapi.Client, error) {
			return client, nil
		},
	})
	if res.code == 0 || !strings.Contains(res.stderr.String(), "resource is stopped remotely") ||
		!strings.Contains(res.stderr.String(), "response identity does not match local share") {
		t.Fatalf("identity mismatch exit=%d stdout=%q stderr=%q", res.code, res.stdout.String(), res.stderr.String())
	}
	if res.stdout.Len() != 0 {
		t.Fatalf("identity mismatch reported untrusted success: stdout=%q stderr=%q", res.stdout.String(), res.stderr.String())
	}
	unchanged, err := registry.Get(context.Background(), local.ResourceID)
	if err != nil || unchanged.DesiredState != local.DesiredState || unchanged.ServingEpoch != local.ServingEpoch {
		t.Fatalf("identity mismatch mutated local state = %+v, %v", unchanged, err)
	}
	if daemon.reloads != 0 || daemon.ensures != 0 {
		t.Fatalf("identity mismatch reached daemon: %+v", daemon)
	}
}

func TestLifecycleCommandsRejectInternalConnectorID(t *testing.T) {
	for _, command := range []string{"start", "restart", "status", "inspect", "stop"} {
		t.Run(command, func(t *testing.T) {
			srv := apitest.NewServer(t)
			stateDir := connectorStateTestDir(t)
			registry, err := openOwnedTestShareRegistry(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			local := localShareFixture(srv)
			if err := registry.Put(context.Background(), &local); err != nil {
				t.Fatal(err)
			}
			preflightCalls := 0
			localReads := 0
			res := runCLI(t, &runOpts{
				args: []string{"--endpoint", srv.URL, command, local.ConnectorID},
				env: map[string]string{
					"QURL_API_KEY": testAPIKey, "QURL_CONNECTOR_STATE_DIR": stateDir,
				},
				shareRegistry: registry, shareDaemon: &recordingShareDaemon{},
				shareStateDir: stateDir,
				readLocalShares: func(context.Context, string) ([]connectorstate.LocalShare, bool, error) {
					localReads++
					return []connectorstate.LocalShare{local}, true, nil
				},
				preflightTarget: func(context.Context, string, int) error {
					preflightCalls++
					return nil
				},
			})
			if res.code != exitcode.Usage {
				t.Fatalf("exit=%d stderr=%s, want usage error", res.code, res.stderr.String())
			}
			for _, want := range []string{"not a Connector ID", "use CRID " + local.CRID} {
				if !strings.Contains(res.stderr.String(), want) {
					t.Fatalf("stderr=%q, want %q", res.stderr.String(), want)
				}
			}
			wantLocalReads := 0
			if command == "status" || command == "inspect" || command == "stop" {
				wantLocalReads = 1
			}
			if localReads != wantLocalReads || preflightCalls != 0 || len(srv.Requests()) != 0 {
				t.Fatalf("rejected Connector ID reads/preflight/requests = %d/%d/%#v, want %d/0/none",
					localReads, preflightCalls, srv.Requests(), wantLocalReads)
			}
		})
	}
}

func TestLinuxStartRotatesEpochAfterLocalTerminalDisable(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
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
		args:         []string{"--endpoint", srv.URL, "start", srv.Key.CRID},
		platformGOOS: "linux",
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
	stateDir := connectorStateTestDir(t)
	t.Setenv("QURL_CONNECTOR_STATE_DIR", stateDir)
	registry, err := openOwnedTestShareRegistry(stateDir)
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

func TestShareInspectAddsRedactedLocalDiagnostics(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
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
			"desired_state": "on", "serving_epoch": seed.ServingEpoch, "connection_state": "connecting",
		}, nil)
	})
	res := runCLI(t, &runOpts{
		args: []string{"--endpoint", srv.URL, "--output", "json", "inspect", srv.Key.CRID},
		env: map[string]string{
			"QURL_API_KEY": testAPIKey, "QURL_CONNECTOR_STATE_DIR": stateDir,
		},
		shareRegistry: registry, shareStateDir: stateDir,
		preflightTarget: func(context.Context, string, int) error { return nil },
	})
	if res.code != 0 {
		t.Fatalf("exit=%d stderr=%s", res.code, res.stderr.String())
	}
	for _, want := range []string{
		`"daemon_state": "not_running"`, `"local_target_health": "healthy"`,
		`"last_transition":`, `"retry_attempt": 0`,
	} {
		if !strings.Contains(res.stdout.String(), want) {
			t.Fatalf("inspect output=%s, want %s", res.stdout.String(), want)
		}
	}
	for _, forbidden := range []string{"connector_routing_id", "knock_resource_id", "session_receipt", "server_public_key"} {
		if strings.Contains(res.stdout.String(), forbidden) {
			t.Fatalf("inspect output exposed %q: %s", forbidden, res.stdout.String())
		}
	}
}

func TestShareInspectKeepsAuthoritativeStoppedStateOverStaleDaemonDiagnostic(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	seed := localShareFixture(srv)
	seed.DesiredState = string(qurlapi.DesiredStateOff)
	if err := registry.Put(context.Background(), &seed); err != nil {
		t.Fatal(err)
	}
	srv.Script(http.MethodGet, "/v1/resources/"+srv.Key.CRID+"/sharing", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
			"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
			"desired_state": "off", "serving_epoch": seed.ServingEpoch + 1, "connection_state": "stopped",
		}, nil)
	})
	stale := seed
	stale.DesiredState = string(qurlapi.DesiredStateOn)
	session := &diagnosticDaemonSession{done: make(chan struct{}), diagnostic: connectordaemon.ResourceDiagnostic{
		State: "starting", LastTransition: time.Now().UTC(),
	}}
	manager, err := connectordaemon.NewManager(&diagnosticDaemonRegistry{share: stale}, diagnosticDaemonFactory{session: session})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	server := &connectordaemon.IPCServer{
		SocketPath: connectordaemon.StateSocketPath(stateDir), Manager: manager, JobVersion: "1/test",
	}
	go func() { done <- server.Run(ctx) }()
	readyCtx, readyCancel := context.WithTimeout(context.Background(), time.Second)
	if err := (connectordaemon.IPCClient{SocketPath: connectordaemon.StateSocketPath(stateDir)}).WaitReady(readyCtx); err != nil {
		readyCancel()
		cancel()
		t.Fatal(err)
	}
	readyCancel()

	res := runCLI(t, &runOpts{
		args: []string{"--endpoint", srv.URL, "--output", "json", "inspect", srv.Key.CRID},
		env: map[string]string{
			"QURL_API_KEY": testAPIKey, "QURL_CONNECTOR_STATE_DIR": stateDir,
		},
		shareRegistry: registry, shareStateDir: stateDir,
		preflightTarget: func(context.Context, string, int) error { return nil },
	})
	if res.code != 0 || !strings.Contains(res.stdout.String(), `"daemon_state": "stopped"`) ||
		strings.Contains(res.stdout.String(), `"daemon_state": "starting"`) {
		cancel()
		t.Fatalf("inspect exit=%d stdout=%s stderr=%s", res.code, res.stdout.String(), res.stderr.String())
	}
	cancel()
	if serverErr := <-done; !errors.Is(serverErr, context.Canceled) {
		t.Fatalf("daemon shutdown = %v", serverErr)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := manager.StopAll(stopCtx); err != nil {
		t.Fatal(err)
	}
}

func TestReadLocalShareNormalizesRequestedIdentifier(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	seed := localShareFixture(srv)
	if err := registry.Put(context.Background(), &seed); err != nil {
		t.Fatal(err)
	}
	opts := &globalOpts{
		resolveShareStateDir: func(string) (string, error) { return stateDir, nil },
		readLocalShares:      connectorstate.ReadLocalSharesIfPresent,
	}

	local, gotDir, err := readLocalShareIfPresent(context.Background(), opts, " \t"+seed.CRID+"\n")
	if err != nil {
		t.Fatal(err)
	}
	if local == nil || local.ResourceID != seed.ResourceID || gotDir != stateDir {
		t.Fatalf("local share = %+v, state dir = %q", local, gotDir)
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

func TestStopRemoteURLReportsTheSupportedLifecycleCommand(t *testing.T) {
	srv := apitest.NewServer(t)
	sharingPath := "/v1/resources/" + srv.Key.CRID + "/sharing"
	srv.Script(http.MethodPut, sharingPath, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteProblem(t, w, http.StatusBadRequest, "invalid_input", "Invalid Input", "Resource is not a qURL Connector")
	})
	srv.Script(http.MethodGet, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{"resource": map[string]any{
			"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
			"target_url": "https://aol.com", "type": "url", "status": "active",
		}}, nil)
	})

	res := runCLI(t, &runOpts{
		args: []string{"--endpoint", srv.URL, "stop", srv.Key.CRID},
		env:  map[string]string{"QURL_API_KEY": testAPIKey},
	})
	if res.code != exitcode.InvalidInput {
		t.Fatalf("stop remote URL exit = %d, want %d", res.code, exitcode.InvalidInput)
	}
	for _, want := range []string{"stop applies only to a local qURL Connector", "qurl delete " + srv.Key.CRID + " --yes", "url resource"} {
		if !strings.Contains(res.stderr.String(), want) {
			t.Fatalf("stop remote URL stderr = %q, want %q", res.stderr.String(), want)
		}
	}
	requests := srv.Requests()
	if len(requests) != 2 || requests[0].Method != http.MethodPut || requests[0].Path != sharingPath ||
		requests[1].Method != http.MethodGet || requests[1].Path != "/v1/resources/"+srv.Key.CRID {
		t.Fatalf("stop remote URL requests = %#v", requests)
	}
}

func TestDeleteRemovesLocalShareWithoutStartingDaemon(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := connectorStateTestDir(t)
	t.Setenv("QURL_CONNECTOR_STATE_DIR", stateDir)
	registry, err := openOwnedTestShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	local := localShareFixture(srv)
	seedLocalConnectorResourceBinding(t, stateDir, &local)
	if err := registry.Put(context.Background(), &local); err != nil {
		t.Fatal(err)
	}
	wantLogDir, err := connectordaemon.DefaultLogDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	daemon := &recordingShareDaemon{}
	gotStateDir, gotLogDir := "", ""
	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "delete", srv.Key.CRID, "--yes"},
		env:           map[string]string{"QURL_API_KEY": testAPIKey, "QURL_CONNECTOR_STATE_DIR": stateDir},
		shareRegistry: registry,
		shareDaemonFactory: func(stateDir, logDir string) shareDaemonController {
			gotStateDir, gotLogDir = stateDir, logDir
			return daemon
		},
		shareStateDir: stateDir,
	})
	if res.code != 0 {
		t.Fatalf("exit=%d stderr=%s", res.code, res.stderr.String())
	}
	if _, err := registry.Get(context.Background(), srv.Key.CRID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted local share still present: %v", err)
	}
	if daemon.ensures != 0 || daemon.reloads != 1 {
		t.Fatalf("delete daemon reconciliation = %+v, want one reload and no install", daemon)
	}
	if gotStateDir != stateDir || gotLogDir != wantLogDir || gotLogDir == "" {
		t.Fatalf("delete daemon paths = state %q log %q, want state %q log %q", gotStateDir, gotLogDir, stateDir, wantLogDir)
	}
	assertLocalConnectorResourceRetired(t, stateDir, local.ConnectorID)
}

func TestDeleteReportsCommittedSuccessWhenDaemonReloadFails(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := connectorStateTestDir(t)
	t.Setenv("QURL_CONNECTOR_STATE_DIR", stateDir)
	registry, err := openOwnedTestShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	local := localShareFixture(srv)
	seedLocalConnectorResourceBinding(t, stateDir, &local)
	if err := registry.Put(context.Background(), &local); err != nil {
		t.Fatal(err)
	}
	reloadErr := errors.New("daemon IPC unavailable")
	daemon := &recordingShareDaemon{reloadErr: reloadErr}
	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "delete", srv.Key.CRID, "--yes"},
		env:           map[string]string{"QURL_API_KEY": testAPIKey, "QURL_CONNECTOR_STATE_DIR": stateDir},
		shareRegistry: registry,
		shareDaemonFactory: func(string, string) shareDaemonController {
			return daemon
		},
		shareStateDir: stateDir,
	})
	if res.code != 0 {
		t.Fatalf("exit=%d stderr=%s", res.code, res.stderr.String())
	}
	if _, err := registry.Get(context.Background(), srv.Key.CRID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted local share still present: %v", err)
	}
	for _, want := range []string{"Deleted " + srv.Key.CRID, "local sharing cleanup did not finish", reloadErr.Error()} {
		if !strings.Contains(res.stderr.String(), want) {
			t.Fatalf("delete stderr=%q, want %q", res.stderr.String(), want)
		}
	}
	if daemon.reloads != 1 || daemon.ensures != 0 {
		t.Fatalf("delete daemon reconciliation = %+v, want one reload and no install", daemon)
	}
	assertLocalConnectorResourceRetired(t, stateDir, local.ConnectorID)
}

func TestIdempotentDeleteRetiresBindingAfterLocalShareRowWasAlreadyRemoved(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID, apitest.HandlerNotFound404(t, "not_found"))
	stateDir := connectorStateTestDir(t)
	local := localShareFixture(srv)
	seedLocalConnectorResourceBinding(t, stateDir, &local)

	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "delete", srv.Key.CRID, "--yes"},
		env:           map[string]string{"QURL_API_KEY": testAPIKey, "QURL_CONNECTOR_STATE_DIR": stateDir},
		shareStateDir: stateDir,
	})
	if res.code != 0 || !strings.Contains(res.stderr.String(), "already deleted") {
		t.Fatalf("exit=%d stderr=%s", res.code, res.stderr.String())
	}
	assertLocalConnectorResourceRetired(t, stateDir, local.ConnectorID)
}

func TestRemoteLifecycleWorksWithoutLocalRegistryRow(t *testing.T) {
	for _, command := range []string{"stop", "status", "inspect"} {
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

func TestRemoteLifecycleWorksWithoutDefaultLocalNamespace(t *testing.T) {
	for _, command := range []string{"stop", "status", "inspect"} {
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

func TestRemoteTunnelLifecycleReadsLocalRegistryOnce(t *testing.T) {
	for _, command := range []string{"stop", "status", "inspect"} {
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
			reads := 0
			res := runCLI(t, &runOpts{
				args:          []string{"--endpoint", srv.URL, command, srv.Key.CRID},
				env:           map[string]string{"QURL_API_KEY": testAPIKey},
				shareStateDir: connectorStateTestDir(t),
				readLocalShares: func(context.Context, string) ([]connectorstate.LocalShare, bool, error) {
					reads++
					return nil, false, nil
				},
			})
			if res.code != 0 {
				t.Fatalf("exit=%d stderr=%s", res.code, res.stderr.String())
			}
			if reads != 1 {
				t.Fatalf("%s local registry reads = %d, want 1", command, reads)
			}
		})
	}
}

func TestRemoteTunnelReadIgnoresUnavailableOrUnsupportedUnrelatedLocalState(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "unavailable", err: errors.New("local registry unavailable at private-cell.example:443 via QURL_CONNECTOR_HUB_HOST")},
		{name: "unsupported v1", err: connectorstate.ErrLocalShareVersionUnsupported},
	}
	for _, command := range []string{"status", "inspect"} {
		for _, test := range tests {
			t.Run(command+"/"+test.name, func(t *testing.T) {
				srv := apitest.NewServer(t)
				srv.Script(http.MethodGet, "/v1/resources/"+srv.Key.CRID+"/sharing", func(w http.ResponseWriter, _ *http.Request) {
					apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
						"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
						"desired_state": "off", "serving_epoch": 9, "connection_state": "stopped",
					}, nil)
				})
				reads := 0
				args := []string{"--endpoint", srv.URL, command, srv.Key.CRID}
				if command == "inspect" {
					args = []string{"--endpoint", srv.URL, "--output", "json", command, srv.Key.CRID}
				}
				res := runCLI(t, &runOpts{
					args:          args,
					env:           map[string]string{"QURL_API_KEY": testAPIKey},
					shareStateDir: connectorStateTestDir(t),
					readLocalShares: func(context.Context, string) ([]connectorstate.LocalShare, bool, error) {
						reads++
						return nil, true, test.err
					},
				})
				if res.code != 0 {
					t.Fatalf("exit=%d stderr=%s", res.code, res.stderr.String())
				}
				if reads != 1 {
					t.Fatalf("local registry reads = %d, want 1", reads)
				}
				if strings.Contains(res.stdout.String(), test.err.Error()) || strings.Contains(res.stderr.String(), test.err.Error()) {
					t.Fatalf("%s exposed raw local state error %q: stdout=%s stderr=%s", command, test.err, res.stdout.String(), res.stderr.String())
				}
				if command == "inspect" && (!strings.Contains(res.stdout.String(), `"daemon_state": "unavailable"`) ||
					!strings.Contains(res.stdout.String(), `"failure_category": "local_state"`)) {
					t.Fatalf("inspect did not report redacted local-state failure: %s", res.stdout.String())
				}
				if !strings.Contains(res.stdout.String(), srv.Key.CRID) || strings.Contains(res.stdout.String(), "Target:") {
					t.Fatalf("remote tunnel output = %q", res.stdout.String())
				}
			})
		}
	}
}

func TestRemoteLifecycleDoesNotBlockReadOnCorruptUnrelatedLocalRegistry(t *testing.T) {
	for _, command := range []string{"stop", "status", "inspect"} {
		t.Run(command, func(t *testing.T) {
			srv := apitest.NewServer(t)
			stateDir := connectorStateTestDir(t)
			reloadErr := errors.New("daemon reload unavailable")
			daemon := &recordingShareDaemon{}
			if command == "stop" {
				daemon.reloadErr = reloadErr
			}
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
				env:  map[string]string{"QURL_API_KEY": testAPIKey}, shareStateDir: stateDir, shareDaemon: daemon,
			})
			if command == "stop" {
				if res.code != 0 || !strings.Contains(res.stderr.String(), "local sharing cleanup did not finish") ||
					!strings.Contains(res.stderr.String(), "local share registry") || !strings.Contains(res.stderr.String(), reloadErr.Error()) {
					t.Fatalf("committed stop result code=%d stderr=%s", res.code, res.stderr.String())
				}
				if !strings.Contains(res.stdout.String(), srv.Key.CRID) || !strings.Contains(res.stdout.String(), "Desired:") ||
					!strings.Contains(res.stdout.String(), "off") {
					t.Fatalf("committed stop stdout=%s", res.stdout.String())
				}
			} else if res.code != 0 || !strings.Contains(res.stdout.String(), srv.Key.CRID) {
				t.Fatalf("remote read result code=%d stdout=%s stderr=%s", res.code, res.stdout.String(), res.stderr.String())
			}
			if requests := srv.Requests(); len(requests) != 1 || requests[0].Method != method {
				t.Fatalf("management requests = %#v, want exactly the authoritative %s", requests, method)
			}
			wantReloads := 0
			if command == "stop" {
				wantReloads = 1
			}
			if daemon.reloads != wantReloads || daemon.ensures != 0 {
				t.Fatalf("%s daemon reloads/ensures = %d/%d, want %d/0", command, daemon.reloads, daemon.ensures, wantReloads)
			}
		})
	}
}

func TestStartFailsImmediatelyWhenServingEpochAdvances(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
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
			stateDir := connectorStateTestDir(t)
			baseRegistry, err := openOwnedTestShareRegistry(stateDir)
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
			// The authoritative stop can retain the on generation's epoch. The
			// local compensation must still persist off instead of rejecting a
			// same-epoch desired-state transition through SetDesired.
			srv.Script(http.MethodPut, path, reply("on", 5, "connecting"), reply("off", 5, "stopped"))
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
			} else if getErr != nil || local.DesiredState != "off" || local.ServingEpoch != 5 {
				t.Fatalf("daemon failure compensation = %+v err=%v", local, getErr)
			}
		})
	}
}

func TestLocalPublishCompensatesAmbiguousEnableBeforeLocalHandoff(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	seed := localShareFixture(srv)
	seed.DesiredState = "off"
	seed.ServingEpoch = 4
	if err := registry.Put(context.Background(), &seed); err != nil {
		t.Fatal(err)
	}
	path := "/v1/resources/" + srv.Key.CRID + "/sharing"
	srv.Script(http.MethodGet, path, sharingResponse(t, srv, "off", 4, "stopped"))
	srv.Script(http.MethodPut, path,
		func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteProblem(t, w, http.StatusServiceUnavailable, "unavailable", "Response uncertain", "enable result is ambiguous")
		},
		sharingResponse(t, srv, "off", 4, "stopped"),
	)
	found := true
	daemon := &recordingShareDaemon{}
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
	if res.code == 0 || !strings.Contains(res.stderr.String(), "ambiguous") {
		t.Fatalf("ambiguous enable result code=%d stdout=%s stderr=%s", res.code, res.stdout.String(), res.stderr.String())
	}
	requests := srv.Requests()
	if len(requests) != 3 || requests[0].Method != http.MethodGet || requests[1].Method != http.MethodPut || requests[2].Method != http.MethodPut {
		t.Fatalf("ambiguous enable requests = %#v, want GET, enable PUT, compensating off PUT", requests)
	}
	stored, err := registry.Get(context.Background(), seed.ResourceID)
	if err != nil || stored.DesiredState != "off" || stored.ServingEpoch != 4 {
		t.Fatalf("ambiguous enable compensation = %+v, %v", stored, err)
	}
	if daemon.ensures != 0 || daemon.reloads != 0 {
		t.Fatalf("ambiguous enable reached daemon handoff: %+v", daemon)
	}
}

func TestLocalPublishCompensatesInvalidRestartBeforeLocalHandoff(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	seed := localShareFixture(srv)
	seed.DesiredState = "on"
	seed.ServingEpoch = 4
	if err := registry.Put(context.Background(), &seed); err != nil {
		t.Fatal(err)
	}
	path := "/v1/resources/" + srv.Key.CRID + "/sharing"
	srv.Script(http.MethodGet, path, sharingResponse(t, srv, "on", 4, "serving"))
	srv.Script(http.MethodPost, path+"/restart", sharingResponse(t, srv, "on", 4, "connecting"))
	srv.Script(http.MethodPut, path, sharingResponse(t, srv, "off", 4, "stopped"))
	found := true
	daemon := &recordingShareDaemon{}
	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "publish", "http://127.0.0.1:4000"},
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
	if res.code == 0 || !strings.Contains(res.stderr.String(), "serving epoch") {
		t.Fatalf("invalid restart result code=%d stdout=%s stderr=%s", res.code, res.stdout.String(), res.stderr.String())
	}
	requests := srv.Requests()
	if len(requests) != 3 || requests[0].Method != http.MethodGet || requests[1].Method != http.MethodPost || requests[2].Method != http.MethodPut {
		t.Fatalf("invalid restart requests = %#v, want GET, restart POST, compensating off PUT", requests)
	}
	stored, err := registry.Get(context.Background(), seed.ResourceID)
	if err != nil || stored.DesiredState != "off" || stored.ServingEpoch != 4 || stored.TargetURL != seed.TargetURL {
		t.Fatalf("invalid restart compensation = %+v, %v", stored, err)
	}
	if daemon.ensures != 0 || daemon.reloads != 0 {
		t.Fatalf("invalid restart reached daemon handoff: %+v", daemon)
	}
}

func TestStartCompensatesCloudOnWhenLocalSetupDoesNotReachDaemonOwnership(t *testing.T) {
	for _, stage := range []string{"registry", "daemon"} {
		t.Run(stage, func(t *testing.T) {
			srv := apitest.NewServer(t)
			stateDir := connectorStateTestDir(t)
			baseRegistry, err := openOwnedTestShareRegistry(stateDir)
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
			stateDir := connectorStateTestDir(t)
			baseRegistry, err := openOwnedTestShareRegistry(stateDir)
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
			stateDir := connectorStateTestDir(t)
			baseRegistry, err := openOwnedTestShareRegistry(stateDir)
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
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
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
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
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
			stateDir := connectorStateTestDir(t)
			registry, err := openOwnedTestShareRegistry(stateDir)
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
			srv.Script(http.MethodPut, path, sharingResponse(t, srv, "off", 5, "stopped"))
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
			if requests := srv.Requests(); len(requests) != 4 || requests[0].Method != http.MethodGet ||
				requests[1].Method != http.MethodPost || requests[2].Method != http.MethodGet || requests[3].Method != http.MethodPut {
				t.Fatalf("requests = %#v, want prior GET, one POST, reconcile GET, compensating PUT", requests)
			}
			stored, err := registry.Get(context.Background(), seed.ResourceID)
			if err != nil || stored.DesiredState != "off" || stored.ServingEpoch != 5 {
				t.Fatalf("ambiguous restart compensation = %+v, %v", stored, err)
			}
			if daemon.ensures != 0 || daemon.reloads != 0 {
				t.Fatalf("ambiguous state reached daemon handoff: %+v", daemon)
			}
		})
	}
}

func TestPublishNewMachineTakeoverRotatesEpochOnce(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
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
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
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
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
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
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
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
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("assigned NHP cell unavailable")
	providerPresent := false
	res := runCLI(t, &runOpts{
		args:            []string{"--endpoint", srv.URL, "publish", "http://127.0.0.1:3000", "--id", "warm-local"},
		env:             map[string]string{"QURL_API_KEY": testAPIKey},
		shareRegistry:   registry,
		shareStateDir:   stateDir,
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
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
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
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
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
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
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

func TestForegroundPublishServingTimeoutIsNotMaskedByDaemonCancellation(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := foregroundLifecycleStateDir(t, "qurl-fg-timeout-")
	registry, err := openOwnedTestShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/resources/" + srv.Key.CRID + "/sharing"
	srv.Script(http.MethodGet, path, sharingResponse(t, srv, "off", 0, "stopped"))
	srv.Script(http.MethodPut, path,
		sharingResponse(t, srv, "on", 1, "connecting"),
		sharingResponse(t, srv, "off", 2, "stopped"),
	)
	srv.Script(http.MethodGet, path, sharingResponse(t, srv, "on", 1, "connecting"))
	started := make(chan struct{})
	stopped := make(chan struct{})
	res := runCLI(t, &runOpts{
		args:          []string{"--endpoint", srv.URL, "publish", "http://127.0.0.1:3000", "--foreground"},
		env:           map[string]string{"QURL_API_KEY": testAPIKey},
		shareRegistry: registry, shareStateDir: stateDir,
		preflightTarget:  func(context.Context, string, int) error { return nil },
		localResource:    resolvedLocalResource(srv, false),
		foregroundDaemon: foregroundIPCTestDaemon(started, stopped),
		sharingWaitLimit: 5 * time.Millisecond,
	})
	if res.code == 0 || res.code == 130 ||
		!strings.Contains(res.stderr.String(), "did not start serving") ||
		!strings.Contains(res.stderr.String(), "connection=connecting") ||
		!strings.Contains(res.stderr.String(), "deadline exceeded") {
		t.Fatalf("result code=%d stderr=%s", res.code, res.stderr.String())
	}
	select {
	case <-stopped:
	default:
		t.Fatal("foreground daemon was not joined after serving timeout")
	}
	local, err := registry.Get(context.Background(), srv.Key.CRID)
	if err != nil || local.DesiredState != "off" || local.ServingEpoch != 2 {
		t.Fatalf("foreground timeout cleanup = %+v err=%v", local, err)
	}
}

func TestForegroundPublishCancellationDrainsAndStopsOwnedSharing(t *testing.T) {
	srv := apitest.NewServer(t)
	stateDir := foregroundLifecycleStateDir(t, "qurl-fg-")
	registry, err := openOwnedTestShareRegistry(stateDir)
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
		manager, err := connectordaemon.NewManager(emptyForegroundRegistry{}, emptyForegroundFactory{})
		if err != nil {
			return err
		}
		runCtx, cancelRun := context.WithCancel(ctx)
		defer cancelRun()
		path := connectordaemon.StateSocketPath(stateDir)
		done := make(chan error, 1)
		go func() {
			done <- (&connectordaemon.IPCServer{
				SocketPath: path, Manager: manager, JobVersion: jobVersion,
			}).Run(runCtx)
		}()
		readyCtx, cancelReady := context.WithTimeout(ctx, 2*time.Second)
		err = (connectordaemon.IPCClient{SocketPath: path}).WaitReady(readyCtx)
		cancelReady()
		if err != nil {
			cancelRun()
			return errors.Join(err, <-done)
		}
		close(started)
		return <-done
	}
}

type emptyForegroundRegistry struct{}

func (emptyForegroundRegistry) List(context.Context) ([]connectorstate.LocalShare, error) {
	return nil, nil
}

func (emptyForegroundRegistry) DisableAtCurrentEpoch(context.Context, string, uint64) (*connectorstate.LocalShare, error) {
	return nil, errors.New("empty foreground registry has no resource")
}

type emptyForegroundFactory struct{}

func (emptyForegroundFactory) Start(context.Context, *connectorstate.LocalShare) (connectordaemon.Session, error) {
	return nil, errors.New("empty foreground factory has no resource")
}

func foregroundLifecycleStateDir(t *testing.T, pattern string) string {
	t.Helper()
	base := ""
	if os.PathSeparator != '\\' {
		base = "/tmp"
	}
	root, err := os.MkdirTemp(base, pattern)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	dir := filepath.Join(root, "connector-state")
	if err := connectorstate.EnsureDirMode(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func localShareFixture(srv *apitest.Server) connectorstate.LocalShare {
	return connectorstate.LocalShare{
		CRID: srv.Key.CRID, ResourceID: srv.Key.ResourceID,
		ConnectorID: "local-test", ConnectorRoutingID: "c-" + strings.Repeat("a", 52),
		KnockResourceID: "q_catalog_key", TargetURL: "http://127.0.0.1:3000",
		LocalIP: "127.0.0.1", LocalPort: 3000, DesiredState: "off", ServingEpoch: 4,
	}
}

func seedLocalConnectorResourceBinding(t *testing.T, stateDir string, local *connectorstate.LocalShare) {
	t.Helper()
	store, err := connectorstate.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	tx, err := store.BeginConnectorResource(context.Background(), local.ConnectorID)
	if err != nil {
		t.Fatal(err)
	}
	binding := &connectorstate.ConnectorResourceBinding{
		ConnectorID: local.ConnectorID, ResourceID: local.ResourceID, CRID: local.CRID,
		ConnectorRoutingID: local.ConnectorRoutingID, KnockResourceID: local.KnockResourceID,
	}
	if err := tx.Commit(binding); err != nil {
		_ = tx.Close()
		t.Fatal(err)
	}
	if err := tx.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertLocalConnectorResourceRetired(t *testing.T, stateDir, connectorID string) {
	t.Helper()
	store, err := connectorstate.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	_, retired, found, err := store.ConnectorResourceBinding(context.Background(), connectorID)
	if err != nil || !found || !retired {
		t.Fatalf("deleted Connector binding found=%t retired=%t err=%v", found, retired, err)
	}
}
