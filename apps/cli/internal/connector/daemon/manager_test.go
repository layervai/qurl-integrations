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

func (r *memoryRegistry) DisableTerminal(ctx context.Context, id string, epoch uint64) (*connectorstate.LocalShare, error) {
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
}

func (f *fakeFactory) Start(_ context.Context, share *connectorstate.LocalShare) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.err[share.ResourceID]; err != nil {
		return nil, err
	}
	session := newFakeSession()
	f.started = append(f.started, *share)
	f.sessions[share.ResourceID] = append(f.sessions[share.ResourceID], session)
	return session, nil
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
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- owner-only state directory.
		t.Fatal(err)
	}
	registry, err := connectorstate.OpenLocalShareRegistry(dir)
	if err != nil {
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
	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if a.stopped || manager.Running()["b"] == "" {
		t.Fatalf("recovered resource did not join healthy sibling: a stopped=%v running=%v", a.stopped, manager.Running())
	}
}

func TestManagerTransientStopFailureKeepsSiblingsAndPreventsReplacementOverlap(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": daemonShare("a", 1, "on"),
		"b": daemonShare("b", 1, "on"),
	}}
	factory := &fakeFactory{sessions: map[string][]*fakeSession{}, err: map[string]error{}}
	manager, _ := NewManager(registry, factory)
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
