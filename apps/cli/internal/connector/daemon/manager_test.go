package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"

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

func (r *memoryRegistry) setShare(share *connectorstate.LocalShare) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shares[share.ResourceID] = *share
}

func (r *memoryRegistry) share(resourceID string) connectorstate.LocalShare {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.shares[resourceID]
}

// fakeGroupRunner models a live session group. On Run it reports every initial
// route serving and blocks until its context ends. SetRoutes/RestartRoute
// record their calls and report added or restarted routes serving, so a test
// can prove exactly which proxies changed.
type fakeGroupRunner struct {
	cfg GroupConfig

	mu              sync.Mutex
	routes          map[string]connectorshare.RouteState
	setCalls        [][]string
	restarts        []string
	restartFailures int
	runStart        chan struct{}
	runOnce         sync.Once
	autoServe       bool
	runErr          error
	blockSetRoutes  bool
}

func newFakeGroupRunner(cfg *GroupConfig, autoServe bool, runErr error, restartFailures int, blockSetRoutes bool) *fakeGroupRunner {
	r := &fakeGroupRunner{
		cfg: *cfg, routes: map[string]connectorshare.RouteState{},
		runStart: make(chan struct{}), autoServe: autoServe, runErr: runErr, restartFailures: restartFailures,
		blockSetRoutes: blockSetRoutes,
	}
	for _, route := range cfg.Routes {
		r.routes[route.RouteID] = connectorshare.RouteState{
			Route: connectorshare.GroupRoute{LocalHTTPRoute: route}, ProxyName: proxyName(route.RouteID, 0),
			Phase: connectorshare.RoutePending,
		}
	}
	return r
}

func proxyName(routeID string, generation uint64) string {
	if generation == 0 {
		return routeID + "-nhp1"
	}
	return routeID + "-nhp1-r" + itoa36(generation)
}

func itoa36(v uint64) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	if v == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = digits[v%36]
		v /= 36
	}
	return string(buf[i:])
}

func (r *fakeGroupRunner) Run(ctx context.Context) error {
	r.runOnce.Do(func() { close(r.runStart) })
	if r.runErr != nil {
		return r.runErr
	}
	if r.autoServe {
		for _, route := range r.cfg.Routes {
			r.serve(route.RouteID)
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

func (r *fakeGroupRunner) SetRoutes(ctx context.Context, routes []connectorshare.LocalHTTPRoute) error {
	if r.blockSetRoutes {
		<-ctx.Done()
		return ctx.Err()
	}
	ids := make([]string, 0, len(routes))
	next := make(map[string]connectorshare.RouteState, len(routes))
	r.mu.Lock()
	for _, route := range routes {
		ids = append(ids, route.RouteID)
		current, existed := r.routes[route.RouteID]
		if existed && current.Route.LocalHTTPRoute == route {
			next[route.RouteID] = current
			continue
		}
		next[route.RouteID] = connectorshare.RouteState{
			Route: connectorshare.GroupRoute{LocalHTTPRoute: route}, ProxyName: proxyName(route.RouteID, 0),
			Phase: connectorshare.RoutePending,
		}
	}
	sort.Strings(ids)
	r.setCalls = append(r.setCalls, ids)
	r.routes = next
	added := make([]string, 0)
	for _, route := range routes {
		if r.routes[route.RouteID].Phase == connectorshare.RoutePending {
			added = append(added, route.RouteID)
		}
	}
	r.mu.Unlock()
	if r.autoServe {
		for _, id := range added {
			r.serve(id)
		}
	}
	return nil
}

func (r *fakeGroupRunner) RestartRoute(_ context.Context, routeID string) error {
	r.mu.Lock()
	r.restarts = append(r.restarts, routeID)
	if r.restartFailures > 0 {
		r.restartFailures--
		r.mu.Unlock()
		return errors.New("fake restart failure")
	}
	if state, ok := r.routes[routeID]; ok {
		state.Route.Generation++
		state.ProxyName = proxyName(routeID, state.Route.Generation)
		state.Phase = connectorshare.RoutePending
		r.routes[routeID] = state
	}
	r.mu.Unlock()
	if r.autoServe {
		r.serve(routeID)
	}
	return nil
}

func (r *fakeGroupRunner) RouteStates() map[string]connectorshare.RouteState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]connectorshare.RouteState, len(r.routes))
	for id := range r.routes {
		out[id] = r.routes[id]
	}
	return out
}

func (r *fakeGroupRunner) serve(routeID string) {
	r.mu.Lock()
	if state, ok := r.routes[routeID]; ok {
		state.Phase = connectorshare.RouteServing
		r.routes[routeID] = state
	}
	r.mu.Unlock()
	if r.cfg.Events.OnRouteServing != nil {
		r.cfg.Events.OnRouteServing(routeID)
	}
}

func (r *fakeGroupRunner) failRoute(routeID string, err error) {
	r.mu.Lock()
	if state, ok := r.routes[routeID]; ok {
		state.Phase, state.Err = connectorshare.RouteFailed, err
		r.routes[routeID] = state
	}
	r.mu.Unlock()
	if r.cfg.Events.OnRouteFailed != nil {
		r.cfg.Events.OnRouteFailed(routeID, err)
	}
}

func (r *fakeGroupRunner) groupRetry(err error, wait time.Duration) {
	if r.cfg.Events.OnRetry != nil {
		r.cfg.Events.OnRetry(err, wait)
	}
}

// setRouteCalls returns every route set pushed so far, each sorted.
func (r *fakeGroupRunner) setRouteCalls() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, 0, len(r.setCalls))
	for _, call := range r.setCalls {
		out = append(out, append([]string(nil), call...))
	}
	return out
}

// dropRoutes models the live session dying: nothing is served any more.
func (r *fakeGroupRunner) dropRoutes() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes = map[string]connectorshare.RouteState{}
}

func (r *fakeGroupRunner) lastSetRoutes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.setCalls) == 0 {
		return nil
	}
	return append([]string(nil), r.setCalls[len(r.setCalls)-1]...)
}

func (r *fakeGroupRunner) restartedRoutes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.restarts...)
}

type fakeGroupFactory struct {
	mu        sync.Mutex
	starts    int
	runners   []*fakeGroupRunner
	configs   []GroupConfig
	errs      []error
	autoServe bool
	runErr    error
	// runErrByResource overrides runErr for the group signed for that resource.
	runErrByResource map[string]error
	restartFailures  int
	blockSetRoutes   bool
}

func newFakeGroupFactory() *fakeGroupFactory { return &fakeGroupFactory{autoServe: true} }

func (f *fakeGroupFactory) NewGroupRunner(_ context.Context, cfg *GroupConfig) (GroupRunner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	f.configs = append(f.configs, *cfg)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return nil, err
		}
	}
	runErr := f.runErr
	if err, ok := f.runErrByResource[cfg.ResourceID]; ok {
		runErr = err
	}
	runner := newFakeGroupRunner(cfg, f.autoServe, runErr, f.restartFailures, f.blockSetRoutes)
	f.runners = append(f.runners, runner)
	return runner, nil
}

func (f *fakeGroupFactory) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts
}

func (f *fakeGroupFactory) runner(index int) *fakeGroupRunner {
	f.mu.Lock()
	defer f.mu.Unlock()
	if index < 1 || index > len(f.runners) {
		return nil
	}
	return f.runners[index-1]
}

func (f *fakeGroupFactory) lastConfig() GroupConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.configs[len(f.configs)-1]
}

func daemonShare(id string, epoch uint64, desired string) connectorstate.LocalShare {
	return connectorstate.LocalShare{
		ResourceID: id, CRID: "crid-" + id, ConnectorID: "connector-" + id,
		ConnectorRoutingID: "routing-" + id, KnockResourceID: "q_catalog_key",
		TargetURL: "http://127.0.0.1:3000", LocalIP: "127.0.0.1", LocalPort: 3000,
		DesiredState: desired, ServingEpoch: epoch,
	}
}

func newRunningManager(t *testing.T, registry *memoryRegistry, factory *fakeGroupFactory) (*Manager, context.CancelFunc) {
	t.Helper()
	manager, err := NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("manager did not stop")
		}
	})
	return manager, cancel
}

func waitManagerCondition(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitServing(t *testing.T, manager *Manager, resourceID string) {
	t.Helper()
	waitManagerCondition(t, func() bool {
		return manager.Diagnostics()[resourceID].State == diagnosticStateServing
	}, "resource "+resourceID+" serving")
}

func TestManagerServesEveryDesiredShareOnOneGroup(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": daemonShare("a", 1, "on"),
		"b": daemonShare("b", 1, "on"),
		"c": daemonShare("c", 1, "on"),
	}}
	factory := newFakeGroupFactory()
	manager, _ := newRunningManager(t, registry, factory)
	for _, id := range []string{"a", "b", "c"} {
		waitServing(t, manager, id)
	}
	if got := factory.startCount(); got != 1 {
		t.Fatalf("group starts = %d, want one admission for three shares", got)
	}
	if got := len(factory.lastConfig().Routes); got != 3 {
		t.Fatalf("initial routes = %d, want 3", got)
	}
	if got := manager.Running(); len(got) != 3 || got["a"] != "crid-a" {
		t.Fatalf("running set = %v, want three resources", got)
	}
}

func TestManagerAddsFourthShareWithoutASecondAdmission(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": daemonShare("a", 1, "on"),
		"b": daemonShare("b", 1, "on"),
		"c": daemonShare("c", 1, "on"),
	}}
	factory := newFakeGroupFactory()
	manager, _ := newRunningManager(t, registry, factory)
	for _, id := range []string{"a", "b", "c"} {
		waitServing(t, manager, id)
	}
	fourth := daemonShare("d", 1, "on")
	registry.setShare(&fourth)
	manager.Trigger()
	waitServing(t, manager, "d")
	if got := factory.startCount(); got != 1 {
		t.Fatalf("group starts after publishing a fourth share = %d, want still one admission", got)
	}
	runner := factory.runner(1)
	if got := runner.lastSetRoutes(); len(got) != 4 {
		t.Fatalf("SetRoutes route set = %v, want four routes", got)
	}
	if got := runner.restartedRoutes(); len(got) != 0 {
		t.Fatalf("publishing a new share restarted existing routes: %v", got)
	}
}

func TestManagerStopOneRouteLeavesSiblingsUntouched(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": daemonShare("a", 1, "on"),
		"b": daemonShare("b", 1, "on"),
	}}
	factory := newFakeGroupFactory()
	manager, _ := newRunningManager(t, registry, factory)
	waitServing(t, manager, "a")
	waitServing(t, manager, "b")

	stopA := daemonShare("a", 2, "off")
	registry.setShare(&stopA)
	manager.Trigger()
	waitManagerCondition(t, func() bool {
		_, present := manager.Diagnostics()["a"]
		return !present
	}, "stopped share pruned")
	runner := factory.runner(1)
	if got := runner.lastSetRoutes(); len(got) != 1 || got[0] != "connector-b" {
		t.Fatalf("SetRoutes after stop = %v, want only connector-b", got)
	}
	if manager.Diagnostics()["b"].State != diagnosticStateServing {
		t.Fatalf("sibling b state = %q, want serving", manager.Diagnostics()["b"].State)
	}
	if got := runner.restartedRoutes(); len(got) != 0 {
		t.Fatalf("stop restarted a route: %v", got)
	}
	if factory.startCount() != 1 {
		t.Fatalf("stop opened a new admission: starts=%d", factory.startCount())
	}
}

func TestManagerRestartAdvancesOnlyThatRoute(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": daemonShare("a", 1, "on"),
		"b": daemonShare("b", 1, "on"),
	}}
	factory := newFakeGroupFactory()
	manager, _ := newRunningManager(t, registry, factory)
	waitServing(t, manager, "a")
	waitServing(t, manager, "b")

	// A restart advances only this share's serving epoch, same target.
	restartB := daemonShare("b", 2, "on")
	registry.setShare(&restartB)
	manager.Trigger()
	waitManagerCondition(t, func() bool {
		for _, id := range factory.runner(1).restartedRoutes() {
			if id == "connector-b" {
				return true
			}
		}
		return false
	}, "route b restarted")
	runner := factory.runner(1)
	if got := runner.restartedRoutes(); len(got) != 1 || got[0] != "connector-b" {
		t.Fatalf("restarted routes = %v, want only connector-b", got)
	}
	if got := runner.RouteStates()["connector-a"].Route.Generation; got != 0 {
		t.Fatalf("sibling a generation = %d, want unchanged", got)
	}
	if factory.startCount() != 1 {
		t.Fatalf("restart opened a new admission: starts=%d", factory.startCount())
	}
}

func TestManagerConvergesThousandDesiredRowsWithOneAdmission(t *testing.T) {
	shares := make(map[string]connectorstate.LocalShare, 1000)
	for i := 0; i < 1000; i++ {
		id := resourceIDf(i)
		shares[id] = connectorstate.LocalShare{
			ResourceID: id, CRID: "crid-" + id, ConnectorID: "connector-" + id,
			ConnectorRoutingID: "routing-" + id, KnockResourceID: "q_catalog_key",
			TargetURL: "http://127.0.0.1:3000", LocalIP: "127.0.0.1", LocalPort: 3000,
			DesiredState: "on", ServingEpoch: 1,
		}
	}
	registry := &memoryRegistry{shares: shares}
	factory := newFakeGroupFactory()
	manager, _ := newRunningManager(t, registry, factory)
	waitManagerCondition(t, func() bool { return len(manager.Running()) == 1000 }, "1000 rows converge")
	if got := factory.startCount(); got != 1 {
		t.Fatalf("group starts for 1000 rows = %d, want one knock", got)
	}
	if got := len(factory.lastConfig().Routes); got != 1000 {
		t.Fatalf("initial routes = %d, want 1000", got)
	}
}

func resourceIDf(i int) string {
	const digits = "0123456789"
	b := []byte("r0000")
	for pos := 4; pos > 0 && i > 0; pos-- {
		b[pos] = digits[i%10]
		i /= 10
	}
	return string(b)
}

func TestManagerStopsGroupWhenReconciliationFails(t *testing.T) {
	reconcileErr := errors.New("registry unavailable")
	registry := &memoryRegistry{
		shares:       map[string]connectorstate.LocalShare{"a": daemonShare("a", 1, "on")},
		listFailures: []error{nil, reconcileErr},
	}
	factory := newFakeGroupFactory()
	manager, err := NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- manager.Run(context.Background()) }()
	waitManagerCondition(t, func() bool { return len(manager.Running()) == 1 }, "initial group start")
	runner := factory.runner(1)
	manager.Trigger()
	select {
	case err := <-runDone:
		if !errors.Is(err, reconcileErr) {
			t.Fatalf("Run error = %v, want registry failure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after registry failure")
	}
	select {
	case <-runner.runStart:
	default:
		t.Fatal("runner never started")
	}
	if factory.startCount() != 1 {
		t.Fatalf("group starts = %d, want one", factory.startCount())
	}
}

func TestManagerStopsGroupWhenNoShareIsDesiredOn(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": daemonShare("a", 1, "on"),
	}}
	factory := newFakeGroupFactory()
	manager, _ := newRunningManager(t, registry, factory)
	waitServing(t, manager, "a")

	stopLast := daemonShare("a", 2, "off")
	registry.setShare(&stopLast)
	manager.Trigger()
	waitManagerCondition(t, func() bool { return len(manager.Running()) == 0 }, "group emptied")
	runner := factory.runner(1)
	waitManagerCondition(t, func() bool {
		select {
		case <-runner.runStart:
			return true
		default:
			return false
		}
	}, "runner observed")
	if factory.startCount() != 1 {
		t.Fatalf("emptying the group opened a new admission: starts=%d", factory.startCount())
	}
}

func TestManagerRebuildsGroupWithBackoffAfterTransientFactoryFailure(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{"a": daemonShare("a", 1, "on")}}
	factory := newFakeGroupFactory()
	factory.errs = []error{errors.New("native transport unavailable")}
	manager, err := NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	manager.retryDelay = func(int) time.Duration { return 10 * time.Millisecond }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitServing(t, manager, "a")
	if got := factory.startCount(); got != 2 {
		t.Fatalf("group starts = %d, want a transient failure and one recovery", got)
	}
}

func TestManagerGroupRetryStallsEveryNonServingRoute(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": daemonShare("a", 1, "on"),
		"b": daemonShare("b", 1, "on"),
	}}
	factory := &fakeGroupFactory{autoServe: false}
	manager, _ := newRunningManager(t, registry, factory)
	waitManagerCondition(t, func() bool { return factory.startCount() == 1 }, "group started")
	runner := factory.runner(1)
	runner.serve("connector-a")
	waitServing(t, manager, "a")
	runner.groupRetry(errors.New("assigned NHP cell unavailable"), 25*time.Millisecond)
	waitManagerCondition(t, func() bool {
		return manager.Diagnostics()["b"].State == diagnosticStateRetrying
	}, "non-serving route b retrying")
	if got := manager.Diagnostics()["b"]; got.NextRetryAt == nil || got.FailureCategory == "" {
		t.Fatalf("retrying diagnostic incomplete: %+v", got)
	}
	if manager.Diagnostics()["a"].State != diagnosticStateServing {
		t.Fatalf("serving route a was demoted by a group retry: %q", manager.Diagnostics()["a"].State)
	}
}

func TestManagerBacksOffAndDoesNotHotLoopWhenGroupRunCrashes(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": daemonShare("a", 1, "on"),
		"b": daemonShare("b", 1, "on"),
	}}
	factory := &fakeGroupFactory{autoServe: false, runErr: errors.New("frp login failed")}
	manager, err := NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	// A large backoff means a correctly classified crash must NOT rebuild the
	// group in the sampling window; the pre-fix "every exit is benign" bug
	// rebuilt immediately and hot-looped, so startCount would climb fast.
	manager.retryDelay = func(int) time.Duration { return time.Hour }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	waitManagerCondition(t, func() bool { return factory.startCount() >= 1 }, "group started")
	waitManagerCondition(t, func() bool {
		return manager.Diagnostics()["a"].State == diagnosticStateRetrying &&
			manager.Diagnostics()["b"].State == diagnosticStateRetrying
	}, "crashed group's routes reported retrying")
	if manager.Diagnostics()["a"].NextRetryAt == nil {
		t.Fatalf("retrying diagnostic missing next-retry time: %+v", manager.Diagnostics()["a"])
	}
	// The group is in backoff; it must not rebuild while the retry window holds.
	time.Sleep(120 * time.Millisecond)
	if got := factory.startCount(); got != 1 {
		t.Fatalf("group starts = %d during backoff, want exactly one (a benign-classified crash would hot-loop)", got)
	}
	if _, running := manager.Running()["a"]; running {
		t.Fatalf("crashed group still reports routes running: %v", manager.Running())
	}
}

func TestManagerRetriesRestartWhenTheFirstAttemptFails(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{"a": daemonShare("a", 1, "on")}}
	factory := &fakeGroupFactory{autoServe: true, restartFailures: 1}
	manager, err := NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	manager.retryDelay = func(int) time.Duration { return 10 * time.Millisecond }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitServing(t, manager, "a")

	restartA := daemonShare("a", 2, "on")
	registry.setShare(&restartA)
	manager.Trigger()
	// The first RestartRoute fails, so the tracked epoch must not advance and a
	// later reconcile must re-detect and re-attempt the restart until it lands.
	waitManagerCondition(t, func() bool {
		got := 0
		for _, id := range factory.runner(1).restartedRoutes() {
			if id == "connector-a" {
				got++
			}
		}
		return got >= 2
	}, "restart re-attempted after a transient failure")
	// Once the restart succeeds, the advanced epoch is committed, so further
	// reconciles do not keep restarting.
	manager.Trigger()
	manager.Trigger()
	time.Sleep(50 * time.Millisecond)
	got := 0
	for _, id := range factory.runner(1).restartedRoutes() {
		if id == "connector-a" {
			got++
		}
	}
	if got != 2 {
		t.Fatalf("restart attempts = %d, want exactly two (one failed, one succeeded, then committed)", got)
	}
}

func TestManagerJoinedCancellationWithFailureIsNotBenign(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{"a": daemonShare("a", 1, "on")}}
	// A real failure joined with a context cancellation must NOT be classified
	// benign: the manager never asked to stop, so this is a crash that must
	// back off, not reset-and-rebuild into a hot loop.
	factory := &fakeGroupFactory{autoServe: false, runErr: errors.Join(context.Canceled, errors.New("frp login failed"))}
	manager, err := NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	manager.retryDelay = func(int) time.Duration { return time.Hour }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	waitManagerCondition(t, func() bool {
		return manager.Diagnostics()["a"].State == diagnosticStateRetrying
	}, "joined-cancellation crash reported retrying")
	time.Sleep(120 * time.Millisecond)
	if got := factory.startCount(); got != 1 {
		t.Fatalf("group starts = %d, want one; a joined context.Canceled must not be read as an intentional stop", got)
	}
}

func TestManagerGroupResourceGoneConvergesEveryShareOff(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": daemonShare("a", 3, "on"),
		"b": daemonShare("b", 5, "on"),
	}}
	factory := &fakeGroupFactory{autoServe: false, runErr: errors.Join(connectorshare.ErrResourceGone, errors.New("knock resource gone"))}
	manager, err := NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	manager.retryDelay = func(int) time.Duration { return 5 * time.Millisecond }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	// A permanent whole-group denial converges every share to a durable off
	// (each at its own epoch) rather than re-knocking forever.
	waitManagerCondition(t, func() bool {
		return registry.share("a").DesiredState == "off" && registry.share("b").DesiredState == "off"
	}, "every share persisted off on a group-level resource-gone")
	if got := registry.share("a").ServingEpoch; got != 3 {
		t.Fatalf("share a persisted epoch = %d, want 3", got)
	}
	if got := registry.share("b").ServingEpoch; got != 5 {
		t.Fatalf("share b persisted epoch = %d, want 5", got)
	}
	// Once both are off the desired set is empty, so the group is not rebuilt.
	time.Sleep(60 * time.Millisecond)
	if got := factory.startCount(); got != 1 {
		t.Fatalf("group starts = %d, want one; a converged gone group must not re-knock", got)
	}
}

func TestManagerRouteRefusedByPlatformStaysOnRetriesAndKeepsSiblingsServing(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": daemonShare("a", 7, "on"),
		"b": daemonShare("b", 4, "on"),
		"c": daemonShare("c", 2, "on"),
	}}
	factory := newFakeGroupFactory()
	manager, err := NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	manager.retryDelay = func(int) time.Duration { return 15 * time.Millisecond }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	for _, id := range []string{"a", "b", "c"} {
		waitServing(t, manager, id)
	}
	runner := factory.runner(1)
	pushesBefore := len(runner.setRouteCalls())
	// The platform refuses b's proxy registration; the group withdraws it and
	// reports it gone. That is never a reason to turn the share off.
	runner.failRoute("connector-b", errors.Join(connectorshare.ErrResourceGone,
		errors.New("resource_not_found: resource does not match signed NHP session")))
	waitManagerCondition(t, func() bool {
		return manager.Diagnostics()["b"].State == diagnosticStateRetrying
	}, "refused route reported retrying")
	got := manager.Diagnostics()["b"]
	if got.FailureCategory != diagnosticFailurePlatformDenied || got.NextRetryAt == nil || got.RetryAttempt != 1 {
		t.Fatalf("refused route diagnostic = %+v, want retrying/platform_denied with a next retry", got)
	}
	if registry.share("b").DesiredState != "on" {
		t.Fatal("platform refusal of one proxy persisted the share off")
	}
	registry.mu.Lock()
	persisted := registry.setCalls
	registry.mu.Unlock()
	if persisted != 0 {
		t.Fatalf("registry disable calls = %d, want none for a refused proxy", persisted)
	}
	// After its backoff the route is re-added to the live group (no knock).
	waitManagerCondition(t, func() bool {
		calls := runner.setRouteCalls()
		if len(calls) <= pushesBefore {
			return false
		}
		last := calls[len(calls)-1]
		return len(last) == 3 && last[1] == "connector-b"
	}, "refused route re-added after backoff")
	for _, id := range []string{"a", "c"} {
		if manager.Diagnostics()[id].State != diagnosticStateServing {
			t.Fatalf("sibling %s state = %q, want serving", id, manager.Diagnostics()[id].State)
		}
	}
	if factory.startCount() != 1 {
		t.Fatalf("a refused route opened a new admission: starts=%d", factory.startCount())
	}
}

func TestManagerGroupResourceGonePersistsOffWithRealLocalRegistry(t *testing.T) {
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
	// The knock itself is denied as resource-not-found: the Connector is
	// unknown to the platform, which is the one permanent case.
	factory := &fakeGroupFactory{autoServe: false, runErr: errors.Join(connectorshare.ErrResourceGone, errors.New("knock resource gone"))}
	manager, err := NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	manager.retryDelay = func(int) time.Duration { return 5 * time.Millisecond }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitManagerCondition(t, func() bool {
		got, err := registry.Get(context.Background(), share.ResourceID)
		return err == nil && got.DesiredState == "off"
	}, "terminal disable persisted")
	got, err := registry.Get(context.Background(), share.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DesiredState != "off" || got.ServingEpoch != share.ServingEpoch || got.TargetURL != share.TargetURL {
		t.Fatalf("terminal persistence = %+v, want off at the same epoch with target preserved", got)
	}
}

func TestManagerRetriesGroupResourceGonePersistenceWithBoundedDeadline(t *testing.T) {
	registry := &memoryRegistry{
		shares:      map[string]connectorstate.LocalShare{"a": daemonShare("a", 7, "on")},
		setFailures: []error{errors.New("temporary disk failure")},
	}
	factory := &fakeGroupFactory{autoServe: false, runErr: errors.Join(connectorshare.ErrResourceGone, errors.New("knock resource gone"))}
	manager, err := NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	manager.retryDelay = func(int) time.Duration { return 5 * time.Millisecond }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitManagerCondition(t, func() bool {
		registry.mu.Lock()
		defer registry.mu.Unlock()
		return registry.setCalls >= 2
	}, "resource-gone persistence retried")
	waitManagerCondition(t, func() bool { return registry.share("a").DesiredState == "off" }, "gone share off after retry")
	registry.mu.Lock()
	deadline := registry.setDeadline
	registry.mu.Unlock()
	if !deadline {
		t.Fatal("persistence did not use a bounded deadline")
	}
}

func TestManagerBoundedRoutePushCannotStallReconcile(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{"a": daemonShare("a", 1, "on")}}
	factory := newFakeGroupFactory()
	factory.blockSetRoutes = true
	manager, err := NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	manager.routePushTimeout = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitServing(t, manager, "a")
	// Two shares arrive while every push to the group wedges; reconciliation
	// must still seed each one's diagnostic and keep turning over.
	second := daemonShare("b", 1, "on")
	registry.setShare(&second)
	manager.Trigger()
	waitManagerCondition(t, func() bool { _, ok := manager.Diagnostics()["b"]; return ok }, "second share seeded despite a wedged push")
	third := daemonShare("c", 1, "on")
	registry.setShare(&third)
	manager.Trigger()
	waitManagerCondition(t, func() bool { _, ok := manager.Diagnostics()["c"]; return ok }, "third share seeded despite a wedged push")
}

func TestManagerGroupRetryDemotesRoutesTheSessionNoLongerServes(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{"a": daemonShare("a", 1, "on")}}
	factory := newFakeGroupFactory()
	manager, _ := newRunningManager(t, registry, factory)
	waitServing(t, manager, "a")
	runner := factory.runner(1)
	// The session died: the runner no longer reports the route as serving, so
	// a group-wide retry must demote it instead of leaving a stale "serving".
	runner.dropRoutes()
	runner.groupRetry(errors.New("assigned NHP cell unavailable"), 25*time.Millisecond)
	waitManagerCondition(t, func() bool {
		return manager.Diagnostics()["a"].State == diagnosticStateRetrying
	}, "dead session route demoted to retrying")
}

// newRefusingGroupHarness runs the real SessionGroupRunner through the native
// group factory against a fake admitter and a fake FRP session group, with the
// manager and IPC server up, so a test can reproduce the live sequence: one
// share serving, then a second published into the same group.
func newRefusingGroupHarness(t *testing.T, refuse func(*connectorshare.GroupRoute) bool) (*Manager, *memoryRegistry, *fakeAdmitter, *fakeSessionGroupFactory, IPCClient) {
	t.Helper()
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{"a": daemonShare("a", 1, "on")}}
	admitter := &fakeAdmitter{}
	sessions := &fakeSessionGroupFactory{refuse: refuse}
	manager, err := NewManager(registry, &NativeGroupFactory{admitter: admitter, sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	manager.retryDelay = func(int) time.Duration { return 20 * time.Millisecond }
	manager.refusalDelay = manager.retryDelay
	dir := shortTempDir(t)
	socket := filepath.Join(dir, SocketFile)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- (&IPCServer{SocketPath: socket, Manager: manager, JobVersion: "1/test"}).Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	client := IPCClient{SocketPath: socket}
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readyCancel()
	if err := client.WaitReady(readyCtx); err != nil {
		t.Fatal(err)
	}
	waitServing(t, manager, "a")
	return manager, registry, admitter, sessions, client
}

// waitIPCCondition polls an IPC-backed condition at a gentler cadence than
// waitManagerCondition, since each check opens a fresh socket connection.
func waitIPCCondition(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func ipcStatus(t *testing.T, client IPCClient) IPCStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	status, running, err := client.Status(ctx)
	if err != nil || !running {
		t.Fatalf("status running=%v err=%v", running, err)
	}
	return status
}

// TestManagerHotAddedShareRefusedByPlatformIsVisibleAndRetriedUntilAccepted
// reproduces the live defect: the tunnel edge answers the second share's
// NewProxy with resource_not_found because the group's NHP session is signed
// for the first share's resource. The daemon must hot-add the route with no
// second knock, report the refusal on /status as retrying/platform_denied
// while keeping the row desired-on, and re-register it under a fresh proxy
// name after its backoff; once the platform accepts it, it serves.
func TestManagerHotAddedShareRefusedByPlatformIsVisibleAndRetriedUntilAccepted(t *testing.T) {
	// Refuse b's first registration only (generation 0), as a platform whose
	// authorization catches up would; the retry under generation 1 is accepted.
	refuse := func(route *connectorshare.GroupRoute) bool {
		return route.RouteID == "connector-b" && route.Generation == 0
	}
	manager, registry, admitter, sessions, client := newRefusingGroupHarness(t, refuse)
	second := daemonShare("b", 1, "on")
	registry.setShare(&second)
	manager.Trigger()

	session := sessions.session(1)
	// The live session received the hot-add as one Update carrying both routes.
	waitManagerCondition(t, func() bool {
		for _, size := range session.updateSizes() {
			if size == 2 {
				return true
			}
		}
		return false
	}, "live session received an Update with two routes")
	if got := admitter.admissions(); got != 1 {
		t.Fatalf("admissions = %d, want the hot-add to spend no second knock", got)
	}
	// The refusal is visible: the row stays on and /status reports the share
	// retrying with the platform's answer as its category.
	waitIPCCondition(t, func() bool {
		diag := ipcStatus(t, client).Resources["b"]
		return diag.State == diagnosticStateRetrying && diag.FailureCategory == diagnosticFailurePlatformDenied && diag.NextRetryAt != nil
	}, "refused hot-add visible on /status as retrying/platform_denied")
	status := ipcStatus(t, client)
	if _, running := status.Running["b"]; !running {
		t.Fatalf("refused share missing from running set: %v", status.Running)
	}
	if registry.share("b").DesiredState != "on" {
		t.Fatal("refused hot-add was persisted off")
	}
	if status.Resources["a"].State != diagnosticStateServing {
		t.Fatalf("serving sibling disturbed by a refused hot-add: %+v", status.Resources["a"])
	}
	// The retry re-registers b under a fresh proxy name and, once accepted,
	// reaches serving — still on the one admission.
	waitServing(t, manager, "b")
	if names := session.proxyNames("connector-b"); len(names) != 2 || names[0] == names[1] {
		t.Fatalf("route b registrations = %v, want a refused name then a fresh one", names)
	}
	if got := admitter.admissions(); got != 1 {
		t.Fatalf("admissions after the retry = %d, want still one", got)
	}
	if registry.share("b").DesiredState != "on" {
		t.Fatal("share was turned off after being accepted")
	}
}

func TestManagerShareRefusedByPlatformForeverKeepsRetryingWithoutTurningOff(t *testing.T) {
	refuse := func(route *connectorshare.GroupRoute) bool { return route.RouteID == "connector-b" }
	manager, registry, admitter, sessions, client := newRefusingGroupHarness(t, refuse)
	second := daemonShare("b", 1, "on")
	registry.setShare(&second)
	manager.Trigger()

	session := sessions.session(1)
	waitManagerCondition(t, func() bool { return len(session.proxyNames("connector-b")) >= 3 }, "refused route re-registered with backoff")
	diag := ipcStatus(t, client).Resources["b"]
	if diag.State != diagnosticStateRetrying || diag.FailureCategory != diagnosticFailurePlatformDenied || diag.RetryAttempt < 2 {
		t.Fatalf("persistently refused route diagnostic = %+v, want retrying/platform_denied with a climbing attempt", diag)
	}
	if registry.share("b").DesiredState != "on" {
		t.Fatal("persistently refused share was persisted off")
	}
	registry.mu.Lock()
	persisted := registry.setCalls
	registry.mu.Unlock()
	if persisted != 0 {
		t.Fatalf("registry disable calls = %d, want none while the row is desired-on", persisted)
	}
	if manager.Diagnostics()["a"].State != diagnosticStateServing || admitter.admissions() != 1 || sessions.startCount() != 1 {
		t.Fatalf("refusals disturbed the group: a=%q admissions=%d sessions=%d",
			manager.Diagnostics()["a"].State, admitter.admissions(), sessions.startCount())
	}
}

func TestManagerRebuiltGroupSignsForAPushedShareNotOneInBackoff(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": daemonShare("a", 1, "on"),
		"b": daemonShare("b", 1, "on"),
	}}
	factory := newFakeGroupFactory()
	manager, err := NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	manager.retryDelay = func(int) time.Duration { return time.Hour }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitServing(t, manager, "a")
	waitServing(t, manager, "b")
	// a — the lexicographically first share, the group's current signed
	// resource — is refused and enters a long backoff.
	factory.runner(1).failRoute("connector-a", errors.Join(connectorshare.ErrResourceGone, errors.New("resource_not_found")))
	waitManagerCondition(t, func() bool {
		return manager.Diagnostics()["a"].State == diagnosticStateRetrying
	}, "a in refusal backoff")
	// Force a rebuild while a is withheld: the new group must sign for b, the
	// share whose proxy is actually pushed, or it could serve nothing.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	if err := manager.stopRunner(stopCtx); err != nil {
		t.Fatal(err)
	}
	manager.Trigger()
	waitManagerCondition(t, func() bool { return factory.startCount() == 2 }, "group rebuilt")
	cfg := factory.lastConfig()
	if cfg.ResourceID != "b" || len(cfg.Routes) != 1 || cfg.Routes[0].RouteID != "connector-b" {
		t.Fatalf("rebuilt group = resource %q routes %v, want signed for b with only b pushed", cfg.ResourceID, cfg.Routes)
	}
}

func TestManagerStopConvergesWhileEverySurvivorIsInBackoff(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": daemonShare("a", 1, "on"),
		"b": daemonShare("b", 1, "on"),
	}}
	factory := newFakeGroupFactory()
	manager, err := NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	manager.retryDelay = func(int) time.Duration { return time.Hour }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitServing(t, manager, "a")
	waitServing(t, manager, "b")
	runner := factory.runner(1)
	runner.failRoute("connector-b", errors.Join(connectorshare.ErrResourceGone, errors.New("resource_not_found")))
	waitManagerCondition(t, func() bool {
		return manager.Diagnostics()["b"].State == diagnosticStateRetrying
	}, "b in refusal backoff")
	// The user stops a while b (the only survivor) is withheld. Nothing can be
	// pushed, so the group must be stopped rather than left serving a's proxy.
	stopA := daemonShare("a", 2, "off")
	registry.setShare(&stopA)
	manager.Trigger()
	waitManagerCondition(t, func() bool {
		select {
		case <-runner.runStart:
		default:
			return false
		}
		return !manager.groupIsRunning()
	}, "group stopped so the stopped share's proxy is withdrawn")
	if _, running := manager.Running()["a"]; running {
		t.Fatalf("stopped share still reported running: %v", manager.Running())
	}
	if factory.startCount() != 1 {
		t.Fatalf("group starts = %d, want no rebuild while every survivor is in backoff", factory.startCount())
	}
}

func TestManagerRestartOfAWithheldRouteWaitsForItsReAdd(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": daemonShare("a", 1, "on"),
		"b": daemonShare("b", 1, "on"),
	}}
	factory := newFakeGroupFactory()
	manager, err := NewManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	manager.retryDelay = func(int) time.Duration { return 10 * time.Millisecond }
	manager.refusalDelay = func(int) time.Duration { return 150 * time.Millisecond }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitServing(t, manager, "a")
	waitServing(t, manager, "b")
	runner := factory.runner(1)
	runner.failRoute("connector-b", errors.Join(connectorshare.ErrResourceGone, errors.New("resource_not_found")))
	waitManagerCondition(t, func() bool {
		return manager.Diagnostics()["b"].State == diagnosticStateRetrying
	}, "b in refusal backoff")
	// The user re-publishes b (an epoch bump with the same target) while the
	// group has withdrawn it: nothing must be restarted on a route the group
	// does not hold, and the restart must land once b is re-added.
	republish := daemonShare("b", 2, "on")
	registry.setShare(&republish)
	manager.Trigger()
	time.Sleep(60 * time.Millisecond)
	for _, id := range runner.restartedRoutes() {
		if id == "connector-b" {
			t.Fatal("RestartRoute was issued for a route the group had withdrawn")
		}
	}
	waitManagerCondition(t, func() bool {
		for _, id := range runner.restartedRoutes() {
			if id == "connector-b" {
				return true
			}
		}
		return false
	}, "restart applied after the route was re-added")
	if got := manager.Diagnostics()["a"].State; got != diagnosticStateServing {
		t.Fatalf("sibling a state = %q, want serving", got)
	}
}
