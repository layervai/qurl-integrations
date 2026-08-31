package daemon

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

type memoryRegistry struct {
	mu           sync.Mutex
	shares       map[string]connectorstate.LocalShare
	listFailures []error
	setFailures  []error
	setCalls     int
	setDeadline  bool
}

func (r *memoryRegistry) List(context.Context) ([]connectorstate.LocalShare, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.listFailures) > 0 {
		err := r.listFailures[0]
		r.listFailures = r.listFailures[1:]
		if err != nil {
			return nil, err
		}
	}
	result := make([]connectorstate.LocalShare, 0, len(r.shares))
	for resourceID := range r.shares {
		result = append(result, r.shares[resourceID])
	}
	return result, nil
}

func TestManagerStopsActiveSessionsWhenReconciliationFails(t *testing.T) {
	reconcileErr := errors.New("registry unavailable")
	registry := &memoryRegistry{
		shares:       map[string]connectorstate.LocalShare{"a": daemonShare("a", 1, "on")},
		listFailures: []error{nil, reconcileErr},
	}
	factory := &fakeFactory{sessions: map[string][]*fakeSession{}, err: map[string]error{}}
	manager, err := NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- manager.Run(context.Background()) }()

	deadline := time.Now().Add(time.Second)
	for len(manager.Running()) != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(manager.Running()) != 1 {
		t.Fatalf("initial route did not start: %v", manager.Running())
	}
	session := factory.sessions["a"][0]
	manager.Trigger()
	select {
	case err := <-runDone:
		if !errors.Is(err, reconcileErr) {
			t.Fatalf("Run error = %v, want registry failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after registry failure")
	}
	if !session.stopped || len(manager.Running()) != 0 {
		t.Fatalf("failed reconciliation left active route: stopped=%v running=%v", session.stopped, manager.Running())
	}
}

func (r *memoryRegistry) DisableAtCurrentEpoch(ctx context.Context, id string, epoch uint64) (*connectorstate.LocalShare, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.setCalls++
	_, r.setDeadline = ctx.Deadline()
	if len(r.setFailures) > 0 {
		err := r.setFailures[0]
		r.setFailures = r.setFailures[1:]
		if err != nil {
			return nil, err
		}
	}
	share, ok := r.shares[id]
	if !ok {
		return nil, errors.New("missing")
	}
	share.DesiredState = "off"
	share.ServingEpoch = epoch
	r.shares[id] = share
	return &share, nil
}

type fakeSession struct {
	done    chan struct{}
	stopOne sync.Once
	stopped bool
	err     error
	stopErr error
	block   bool
}

type fakeDiagnosticSession struct {
	*fakeSession
	diagnostic ResourceDiagnostic
}

func (s *fakeDiagnosticSession) Diagnostic() ResourceDiagnostic { return s.diagnostic }

func newFakeSession() *fakeSession           { return &fakeSession{done: make(chan struct{})} }
func (s *fakeSession) Done() <-chan struct{} { return s.done }
func (s *fakeSession) Err() error            { return s.err }
func (s *fakeSession) Stop(ctx context.Context) error {
	if s.block {
		<-ctx.Done()
		return ctx.Err()
	}
	if s.stopErr != nil {
		return s.stopErr
	}
	s.stopOne.Do(func() { s.stopped = true; close(s.done) })
	return nil
}

type fakeFactory struct {
	mu       sync.Mutex
	started  []connectorstate.LocalShare
	sessions map[string][]*fakeSession
	err      map[string]error
	attempts map[string]int
}

func (f *fakeFactory) Start(_ context.Context, share *connectorstate.LocalShare) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attempts == nil {
		f.attempts = map[string]int{}
	}
	f.attempts[share.ResourceID]++
	if err := f.err[share.ResourceID]; err != nil {
		return nil, err
	}
	session := newFakeSession()
	f.started = append(f.started, *share)
	f.sessions[share.ResourceID] = append(f.sessions[share.ResourceID], session)
	return session, nil
}

func (f *fakeFactory) attemptCount(resourceID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts[resourceID]
}

func TestManagerUnrelatedReconcileDoesNotBypassResourceBackoff(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": daemonShare("a", 1, "on"),
		"b": daemonShare("b", 1, "on"),
	}}
	factory := &fakeFactory{sessions: map[string][]*fakeSession{}, err: map[string]error{
		"a": errors.New("a unavailable"),
		"b": errors.New("b unavailable"),
	}}
	manager, err := NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	manager.retryDelay = func(int) time.Duration { return time.Hour }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if factory.attemptCount("a") != 1 || factory.attemptCount("b") != 1 {
		t.Fatalf("unrelated reconcile bypassed backoff: attempts=%v", factory.attempts)
	}
	manager.mu.Lock()
	retryDefinition := manager.retryDefinitions["a"]
	retryAttempt := manager.diagnostics["a"].RetryAttempt
	manager.mu.Unlock()
	if retryDefinition.ServingEpoch != 1 || retryAttempt != 1 {
		t.Fatalf("same-definition retry state changed: definition=%+v attempt=%d", retryDefinition, retryAttempt)
	}

	manager.mu.Lock()
	delete(manager.retrying, "a")
	delete(manager.retryGeneration, "a")
	manager.mu.Unlock()
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if factory.attemptCount("a") != 2 || factory.attemptCount("b") != 1 {
		t.Fatalf("resource-scoped retry disturbed sibling backoff: attempts=%v", factory.attempts)
	}
}

func TestManagerNewDefinitionBypassesFailedStartBackoffImmediately(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{"a": daemonShare("a", 1, "on")}}
	factory := &fakeFactory{sessions: map[string][]*fakeSession{}, err: map[string]error{
		"a": errors.New("old lifecycle unavailable"),
	}}
	manager, err := NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	manager.retryDelay = func(int) time.Duration { return time.Hour }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	factory.mu.Lock()
	delete(factory.err, "a")
	factory.mu.Unlock()
	registry.mu.Lock()
	registry.shares["a"] = daemonShare("a", 2, "on")
	registry.mu.Unlock()
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if factory.attemptCount("a") != 2 || len(factory.sessions["a"]) != 1 || factory.started[0].ServingEpoch != 2 {
		t.Fatalf("new lifecycle did not start immediately: attempts=%d sessions=%d starts=%+v",
			factory.attemptCount("a"), len(factory.sessions["a"]), factory.started)
	}
	manager.mu.Lock()
	_, retrying := manager.retrying["a"]
	_, hasRetryDefinition := manager.retryDefinitions["a"]
	_, hasFailures := manager.failures["a"]
	diagnostic := manager.diagnostics["a"]
	manager.mu.Unlock()
	if retrying || hasRetryDefinition || hasFailures || diagnostic.State != diagnosticStateStarting || diagnostic.RetryAttempt != 0 {
		t.Fatalf("new lifecycle retained old backoff or diagnostics: retrying=%t definition=%t failures=%t diagnostic=%+v",
			retrying, hasRetryDefinition, hasFailures, diagnostic)
	}
}

func TestManagerNewDefinitionResetsBackoffAndOldTimerCannotDisturbIt(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{"a": daemonShare("a", 1, "on")}}
	factory := &fakeFactory{sessions: map[string][]*fakeSession{}, err: map[string]error{
		"a": errors.New("lifecycle unavailable"),
	}}
	manager, err := NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	delayCalls := 0
	manager.retryDelay = func(attempt int) time.Duration {
		if attempt != 1 {
			t.Fatalf("new lifecycle inherited retry attempt %d", attempt)
		}
		delayCalls++
		if delayCalls == 1 {
			return 50 * time.Millisecond
		}
		return time.Hour
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	firstGeneration := manager.retryGeneration["a"]
	manager.mu.Unlock()
	registry.mu.Lock()
	registry.shares["a"] = daemonShare("a", 2, "on")
	registry.mu.Unlock()
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	secondGeneration := manager.retryGeneration["a"]
	definition := manager.retryDefinitions["a"]
	diagnostic := manager.diagnostics["a"]
	manager.mu.Unlock()
	if secondGeneration == firstGeneration || definition.ServingEpoch != 2 || diagnostic.RetryAttempt != 1 {
		t.Fatalf("new lifecycle retry was not reset: first=%d second=%d definition=%+v diagnostic=%+v",
			firstGeneration, secondGeneration, definition, diagnostic)
	}
	time.Sleep(100 * time.Millisecond)
	manager.mu.Lock()
	stillRetrying := manager.retrying["a"]
	currentGeneration := manager.retryGeneration["a"]
	currentDefinition := manager.retryDefinitions["a"]
	manager.mu.Unlock()
	if !stillRetrying || currentGeneration != secondGeneration || currentDefinition.ServingEpoch != 2 || len(manager.trigger) != 0 {
		t.Fatalf("old timer disturbed new lifecycle retry: retrying=%t generation=%d definition=%+v triggers=%d",
			stillRetrying, currentGeneration, currentDefinition, len(manager.trigger))
	}
}

func TestManagerNewDefinitionBypassesWatcherBackoffWithoutLiveSession(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{"a": daemonShare("a", 1, "on")}}
	factory := &fakeFactory{sessions: map[string][]*fakeSession{}, err: map[string]error{}}
	manager, err := NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	manager.retryDelay = func(int) time.Duration { return time.Hour }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	old := factory.sessions["a"][0]
	old.err = errors.New("old lifecycle ended")
	close(old.done)
	waitManagerCondition(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return manager.retrying["a"]
	}, "watcher retry")
	registry.mu.Lock()
	registry.shares["a"] = daemonShare("a", 2, "on")
	registry.mu.Unlock()
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if factory.attemptCount("a") != 2 || len(factory.sessions["a"]) != 2 || factory.started[1].ServingEpoch != 2 {
		t.Fatalf("watcher backoff suppressed new lifecycle: attempts=%d sessions=%d starts=%+v",
			factory.attemptCount("a"), len(factory.sessions["a"]), factory.started)
	}
}

func TestManagerLiveOldSessionKeepsRetryGateAcrossNewDefinition(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{"a": daemonShare("a", 1, "on")}}
	factory := &fakeFactory{sessions: map[string][]*fakeSession{}, err: map[string]error{}}
	manager, err := NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	manager.retryDelay = func(int) time.Duration { return time.Hour }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	old := factory.sessions["a"][0]
	old.stopErr = errors.New("old session stop not confirmed")
	registry.mu.Lock()
	registry.shares["a"] = daemonShare("a", 2, "on")
	registry.mu.Unlock()
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	retrying := manager.retrying["a"]
	retryDefinition := manager.retryDefinitions["a"]
	retryAttempt := manager.diagnostics["a"].RetryAttempt
	managed := manager.sessions["a"]
	manager.mu.Unlock()
	if !retrying || retryDefinition.ServingEpoch != 1 || retryAttempt != 1 || managed == nil || managed.session != old ||
		factory.attemptCount("a") != 1 || len(factory.sessions["a"]) != 1 {
		t.Fatalf("live old session lost its overlap gate: retrying=%t definition=%+v attempt=%d managed=%+v starts=%d sessions=%d",
			retrying, retryDefinition, retryAttempt, managed, factory.attemptCount("a"), len(factory.sessions["a"]))
	}
}

func TestManagerDiagnosticsPreferScheduledRetryOverLiveSession(t *testing.T) {
	manager, err := NewManager(&memoryRegistry{shares: map[string]connectorstate.LocalShare{}},
		&fakeFactory{sessions: map[string][]*fakeSession{}, err: map[string]error{}})
	if err != nil {
		t.Fatal(err)
	}
	manager.sessions["a"] = &managedSession{share: daemonShare("a", 1, "on"), session: &fakeDiagnosticSession{
		fakeSession: newFakeSession(), diagnostic: ResourceDiagnostic{State: diagnosticStateServing},
	}}
	manager.retrying["a"] = true
	manager.diagnostics["a"] = ResourceDiagnostic{State: diagnosticStateRetrying, RetryAttempt: 2}
	got := manager.Diagnostics()["a"]
	if got.State != diagnosticStateRetrying || got.RetryAttempt != 2 {
		t.Fatalf("scheduled retry diagnostic was hidden by live session: %+v", got)
	}
}

func daemonShare(id string, epoch uint64, desired string) connectorstate.LocalShare {
	return connectorstate.LocalShare{
		ResourceID: id, CRID: "crid-" + id, ConnectorID: "connector-" + id,
		ConnectorRoutingID: "routing-" + id, KnockResourceID: "knock-" + id,
		TargetURL: "http://127.0.0.1:3000", LocalIP: "127.0.0.1", LocalPort: 3000,
		DesiredState: desired, ServingEpoch: epoch,
	}
}

func TestManagerKeepsSiblingServingAcrossStopRestartAndRemove(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": daemonShare("a", 1, "on"),
		"b": daemonShare("b", 1, "on"),
	}}
	factory := &fakeFactory{sessions: map[string][]*fakeSession{}, err: map[string]error{}}
	manager, err := NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	a1 := factory.sessions["a"][0]
	b1 := factory.sessions["b"][0]

	registry.shares["a"] = daemonShare("a", 1, "off")
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !a1.stopped || b1.stopped || len(factory.sessions["b"]) != 1 {
		t.Fatalf("stop one: a stopped=%v b stopped=%v b starts=%d", a1.stopped, b1.stopped, len(factory.sessions["b"]))
	}

	registry.shares["a"] = daemonShare("a", 2, "on")
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	a2 := factory.sessions["a"][1]
	registry.shares["a"] = daemonShare("a", 3, "on")
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !a2.stopped || b1.stopped || len(factory.sessions["a"]) != 3 {
		t.Fatalf("restart one lost isolation: a2 stopped=%v b stopped=%v a starts=%d", a2.stopped, b1.stopped, len(factory.sessions["a"]))
	}

	delete(registry.shares, "a")
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b1.stopped || len(manager.Running()) != 1 || manager.Running()["b"] == "" {
		t.Fatalf("remove one disrupted sibling: running=%v b stopped=%v", manager.Running(), b1.stopped)
	}
}

func TestManagerPrunesDiagnosticsForSuccessfulRemovedResource(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{"a": daemonShare("a", 1, "on")}}
	factory := &fakeFactory{sessions: map[string][]*fakeSession{}, err: map[string]error{}}
	manager, err := NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.Diagnostics()["a"]; !ok {
		t.Fatal("successful session has no diagnostic before removal")
	}

	delete(registry.shares, "a")
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if diagnostic, ok := manager.Diagnostics()["a"]; ok {
		t.Fatalf("removed successful session retained diagnostic: %+v", diagnostic)
	}
}

func TestManagerStopsRetryingPermanentMissingResource(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{"a": daemonShare("a", 4, "on")}}
	factory := &fakeFactory{sessions: map[string][]*fakeSession{}, err: map[string]error{"a": ErrResourceGone}}
	manager, _ := NewManager(registry, factory)
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := registry.shares["a"].DesiredState; got != "off" {
		t.Fatalf("desired state = %q, want off after permanent resource denial", got)
	}
	if len(manager.Running()) != 0 {
		t.Fatalf("running = %v, want none", manager.Running())
	}
}

func TestManagerPersistsTerminalDisableWithRealLocalRegistry(t *testing.T) {
	dir := shortTempDir(t)
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- owner-only state directory.
		t.Fatal(err)
	}
	registry, err := connectorstate.OpenLocalShareRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.BindOwner(context.Background(), "own_daemon_fixture"); err != nil {
		t.Fatal(err)
	}
	key := apitest.FixedResourceKey(t)
	share := connectorstate.LocalShare{
		CRID: key.CRID, ResourceID: key.ResourceID, ConnectorID: "terminal-share",
		ConnectorRoutingID: "c-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		KnockResourceID:    "qurl-tunnel-server",
		TargetURL:          "http://127.0.0.1:3000", LocalIP: "127.0.0.1", LocalPort: 3000,
		DesiredState: "on", ServingEpoch: 7,
	}
	if err := registry.Put(context.Background(), &share); err != nil {
		t.Fatal(err)
	}
	factory := &fakeFactory{sessions: map[string][]*fakeSession{}, err: map[string]error{share.ResourceID: ErrResourceGone}}
	manager, err := NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := registry.Get(context.Background(), share.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DesiredState != "off" || got.ServingEpoch != share.ServingEpoch || got.TargetURL != share.TargetURL {
		t.Fatalf("terminal persistence = %+v, want off at the same epoch with target preserved", got)
	}
}

func TestManagerTransientStartFailureDoesNotStopSibling(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": daemonShare("a", 1, "on"),
		"b": daemonShare("b", 1, "on"),
	}}
	factory := &fakeFactory{
		sessions: map[string][]*fakeSession{},
		err:      map[string]error{"b": errors.New("network unavailable")},
	}
	manager, _ := NewManager(registry, factory)
	manager.retryDelay = func(int) time.Duration { return time.Millisecond }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatalf("resource-local transient escaped Reconcile: %v", err)
	}
	a := factory.sessions["a"][0]
	if a.stopped || manager.Running()["a"] == "" {
		t.Fatalf("healthy sibling was disrupted: stopped=%v running=%v", a.stopped, manager.Running())
	}
	factory.mu.Lock()
	delete(factory.err, "b")
	factory.mu.Unlock()
	waitManagerCondition(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return !manager.retrying["b"]
	}, "resource-local start retry")
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if a.stopped || manager.Running()["b"] == "" {
		t.Fatalf("recovered resource did not join healthy sibling: a stopped=%v running=%v", a.stopped, manager.Running())
	}
}

func TestManagerRemovedResourceDoesNotRetainRetryBackoff(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{"a": daemonShare("a", 1, "on")}}
	factory := &fakeFactory{
		sessions: map[string][]*fakeSession{},
		err:      map[string]error{"a": errors.New("network unavailable")},
	}
	manager, _ := NewManager(registry, factory)
	firstDelayReady := make(chan struct{})
	secondDelayReady := make(chan struct{})
	delayCalls := 0
	manager.retryDelay = func(int) time.Duration {
		delayCalls++
		if delayCalls == 1 {
			close(firstDelayReady)
			return 100 * time.Millisecond
		}
		close(secondDelayReady)
		return time.Hour
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	<-firstDelayReady
	manager.mu.Lock()
	firstFailures, firstRetrying := manager.failures["a"], manager.retrying["a"]
	manager.mu.Unlock()
	if firstFailures != 1 || !firstRetrying {
		t.Fatalf("first retry state = failures %d retrying %t, want 1/true", firstFailures, firstRetrying)
	}

	delete(registry.shares, "a")
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	_, hasFailures := manager.failures["a"]
	_, hasRetrying := manager.retrying["a"]
	manager.mu.Unlock()
	if hasFailures || hasRetrying {
		t.Fatalf("removed resource retained retry state: failures=%t retrying=%t", hasFailures, hasRetrying)
	}

	registry.shares["a"] = daemonShare("a", 2, "on")
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	<-secondDelayReady
	manager.mu.Lock()
	republishedFailures := manager.failures["a"]
	manager.mu.Unlock()
	if republishedFailures != 1 {
		t.Fatalf("republished resource failure count = %d, want fresh attempt 1", republishedFailures)
	}
	time.Sleep(150 * time.Millisecond)
	manager.mu.Lock()
	stillRetrying := manager.retrying["a"]
	_, hasGeneration := manager.retryGeneration["a"]
	manager.mu.Unlock()
	if !stillRetrying || !hasGeneration || len(manager.trigger) != 0 {
		t.Fatalf("stale timer disturbed republished retry: retrying=%t generation=%t triggers=%d", stillRetrying, hasGeneration, len(manager.trigger))
	}
}

func TestManagerTransientStopFailureKeepsSiblingsAndPreventsReplacementOverlap(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": daemonShare("a", 1, "on"),
		"b": daemonShare("b", 1, "on"),
	}}
	factory := &fakeFactory{sessions: map[string][]*fakeSession{}, err: map[string]error{}}
	manager, _ := NewManager(registry, factory)
	manager.retryDelay = func(int) time.Duration { return time.Millisecond }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	a1 := factory.sessions["a"][0]
	b1 := factory.sessions["b"][0]
	a1.stopErr = errors.New("resource-local stop timeout")
	registry.shares["a"] = daemonShare("a", 2, "on")
	registry.shares["c"] = daemonShare("c", 1, "on")

	if err := manager.Reconcile(ctx); err != nil {
		t.Fatalf("resource-local stop failure escaped Reconcile: %v", err)
	}
	if len(factory.sessions["a"]) != 1 {
		t.Fatalf("replacement overlapped session whose stop was unconfirmed: starts=%d", len(factory.sessions["a"]))
	}
	if b1.stopped || manager.Running()["b"] == "" || manager.Running()["c"] == "" {
		t.Fatalf("stop failure disrupted siblings: b stopped=%v running=%v", b1.stopped, manager.Running())
	}

	a1.stopErr = nil
	waitManagerCondition(t, func() bool {
		manager.mu.Lock()
		defer manager.mu.Unlock()
		return !manager.retrying["a"]
	}, "resource-local stop retry")
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if !a1.stopped || len(factory.sessions["a"]) != 2 {
		t.Fatalf("resource did not converge after stop recovered: stopped=%v starts=%d", a1.stopped, len(factory.sessions["a"]))
	}
	if b1.stopped || manager.Running()["b"] == "" || manager.Running()["c"] == "" {
		t.Fatalf("recovery disrupted siblings: b stopped=%v running=%v", b1.stopped, manager.Running())
	}
}

func TestManagerRetriesResourceGonePersistenceWithoutDisruptingSibling(t *testing.T) {
	registry := &memoryRegistry{
		shares: map[string]connectorstate.LocalShare{
			"a": daemonShare("a", 7, "on"),
			"b": daemonShare("b", 1, "on"),
		},
		setFailures: []error{errors.New("temporary disk failure")},
	}
	factory := &fakeFactory{sessions: map[string][]*fakeSession{}, err: map[string]error{}}
	manager, _ := NewManager(registry, factory)
	manager.retryDelay = func(int) time.Duration { return time.Millisecond }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	a := factory.sessions["a"][0]
	b := factory.sessions["b"][0]
	factory.mu.Lock()
	factory.err["a"] = ErrResourceGone
	factory.mu.Unlock()
	a.err = ErrResourceGone
	close(a.done)

	waitManagerCondition(t, func() bool {
		registry.mu.Lock()
		defer registry.mu.Unlock()
		return registry.setCalls == 1
	}, "first bounded persistence attempt")
	registry.mu.Lock()
	if !registry.setDeadline || registry.shares["a"].DesiredState != "on" {
		t.Fatalf("failed persistence deadline=%t desired=%q, want bounded/on", registry.setDeadline, registry.shares["a"].DesiredState)
	}
	registry.mu.Unlock()

	select {
	case <-manager.trigger:
	case <-time.After(time.Second):
		t.Fatal("resource-local persistence failure did not schedule reconciliation")
	}
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatalf("resource-local persistence recovery escaped Reconcile: %v", err)
	}
	registry.mu.Lock()
	gotDesired := registry.shares["a"].DesiredState
	setCalls := registry.setCalls
	registry.mu.Unlock()
	if gotDesired != "off" || setCalls != 2 {
		t.Fatalf("recovery desired=%q persistence calls=%d, want off/2", gotDesired, setCalls)
	}
	if b.stopped || manager.Running()["b"] == "" {
		t.Fatalf("resource-local persistence failure disrupted sibling: stopped=%t running=%v", b.stopped, manager.Running())
	}
}

func TestManagerBoundsBlockingResourceStopAndContinuesReconciliation(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": daemonShare("a", 1, "on"),
		"b": daemonShare("b", 1, "on"),
	}}
	factory := &fakeFactory{sessions: map[string][]*fakeSession{}, err: map[string]error{}}
	manager, _ := NewManager(registry, factory)
	manager.resourceStopTimeout = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	a := factory.sessions["a"][0]
	b := factory.sessions["b"][0]
	a.block = true
	registry.shares["a"] = daemonShare("a", 2, "on")
	registry.shares["c"] = daemonShare("c", 1, "on")

	started := time.Now()
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatalf("bounded resource stop escaped Reconcile: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("blocking resource stop stalled reconciliation for %s", elapsed)
	}
	if b.stopped || manager.Running()["b"] == "" || manager.Running()["c"] == "" {
		t.Fatalf("blocking resource stop disrupted siblings/new resources: b stopped=%t running=%v", b.stopped, manager.Running())
	}
	if len(factory.sessions["a"]) != 1 {
		t.Fatalf("blocking old session overlapped replacement: starts=%d", len(factory.sessions["a"]))
	}
}

func TestStopSessionsDoesNotLetOneBlockedRouteStarveSiblingRetirement(t *testing.T) {
	blocked := newFakeSession()
	blocked.block = true
	sibling := newFakeSession()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := stopSessions(ctx, []Session{blocked, sibling})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stopSessions error = %v, want deadline from blocked route", err)
	}
	if !sibling.stopped {
		t.Fatal("blocked route prevented sibling retirement attempt")
	}
}

func waitManagerCondition(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(time.Millisecond)
	}
}
