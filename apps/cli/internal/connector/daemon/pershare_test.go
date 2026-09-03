package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"testing"
	"time"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"

	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

// perShareRow is a desired-on share whose knock resource is its own, so a test
// can prove each group knocked for its own row rather than a representative's.
func perShareRow(id string, epoch uint64, desired string) connectorstate.LocalShare {
	share := daemonShare(id, epoch, desired)
	share.KnockResourceID = "knock-" + id
	return share
}

func newRunningPerShareManager(t *testing.T, registry Registry, factory GroupFactory) *PerShareManager {
	t.Helper()
	manager, err := NewPerShareManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	manager.configure = func(group *Manager) {
		group.retryDelay = func(int) time.Duration { return 10 * time.Millisecond }
		group.refusalDelay = func(int) time.Duration { return 150 * time.Millisecond }
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("per-share manager Run = %v, want the cancellation", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("per-share manager did not stop")
		}
	})
	return manager
}

// runnersFor returns every runner the fake factory built for resourceID's
// group, in start order.
func runnersFor(factory *fakeGroupFactory, resourceID string) []*fakeGroupRunner {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	var runners []*fakeGroupRunner
	for _, runner := range factory.runners {
		if runner.cfg.ResourceID == resourceID {
			runners = append(runners, runner)
		}
	}
	return runners
}

func runnerFor(t *testing.T, factory *fakeGroupFactory, resourceID string) *fakeGroupRunner {
	t.Helper()
	runners := runnersFor(factory, resourceID)
	if len(runners) == 0 {
		t.Fatalf("no group was built for resource %s", resourceID)
	}
	return runners[len(runners)-1]
}

func groupConfigs(factory *fakeGroupFactory) []GroupConfig {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	return append([]GroupConfig(nil), factory.configs...)
}

func waitPerShareServing(t *testing.T, manager ShareManager, ids ...string) {
	t.Helper()
	for _, id := range ids {
		waitManagerCondition(t, func() bool {
			return manager.Diagnostics()[id].State == diagnosticStateServing
		}, "resource "+id+" serving")
	}
}

func TestPerShareManagerServesEachShareOnItsOwnGroup(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": perShareRow("a", 1, "on"),
		"b": perShareRow("b", 1, "on"),
		"c": perShareRow("c", 1, "on"),
	}}
	factory := newFakeGroupFactory()
	manager := newRunningPerShareManager(t, registry, factory)
	waitPerShareServing(t, manager, "a", "b", "c")
	if got := factory.startCount(); got != 3 {
		t.Fatalf("group starts = %d, want one group per share", got)
	}
	signed := make([]string, 0, 3)
	for _, cfg := range groupConfigs(factory) {
		signed = append(signed, cfg.ResourceID)
		if len(cfg.Routes) != 1 || cfg.Routes[0].ResourceID != cfg.ResourceID {
			t.Fatalf("group for %s carries routes %v, want exactly its own", cfg.ResourceID, cfg.Routes)
		}
		if cfg.KnockResourceID != "knock-"+cfg.ResourceID {
			t.Fatalf("group for %s knocks for %q, want its own row's knock resource", cfg.ResourceID, cfg.KnockResourceID)
		}
	}
	sort.Strings(signed)
	if got := signed; len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("groups signed for %v, want one per share", got)
	}
	if got := manager.Running(); len(got) != 3 || got["a"] != "crid-a" || got["c"] != "crid-c" {
		t.Fatalf("running set = %v, want every share with its CRID", got)
	}
}

// TestPerShareManagerSpendsOneAdmissionAndOneSessionPerShare drives the real
// SessionGroupRunner through the native group factory: N desired-on shares
// cost N knocks on the one shared admitter and N sessions, each signed for and
// carrying exactly its own share, and /status reports the union unchanged.
func TestPerShareManagerSpendsOneAdmissionAndOneSessionPerShare(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": perShareRow("a", 1, "on"),
		"b": perShareRow("b", 1, "on"),
		"c": perShareRow("c", 1, "on"),
	}}
	admitter := &fakeAdmitter{}
	sessions := &fakeSessionGroupFactory{}
	manager, err := NewPerShareManager(registry, &NativeGroupFactory{admitter: admitter, sessions: sessions})
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(shortTempDir(t), SocketFile)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (&IPCServer{SocketPath: socket, Manager: manager, JobVersion: "3/test/per-share"}).Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	waitPerShareServing(t, manager, "a", "b", "c")
	if got := admitter.admissions(); got != 3 {
		t.Fatalf("admissions = %d, want one knock per share", got)
	}
	if got := sessions.startCount(); got != 3 {
		t.Fatalf("sessions = %d, want one session per share", got)
	}
	signed := make([]string, 0, 3)
	for i := 1; i <= 3; i++ {
		session := sessions.session(i)
		states := session.RouteStates()
		if len(states) != 1 {
			t.Fatalf("session %d carries %d routes, want exactly one", i, len(states))
		}
		for _, state := range states {
			if state.Route.ResourceID != session.admission.ResourceID {
				t.Fatalf("session %d signed for %q but serves %q", i, session.admission.ResourceID, state.Route.ResourceID)
			}
		}
		if session.admission.KnockResourceID != "knock-"+session.admission.ResourceID {
			t.Fatalf("session %d knocked for %q, want its own row's knock resource", i, session.admission.KnockResourceID)
		}
		signed = append(signed, session.admission.ResourceID)
	}
	sort.Strings(signed)
	if signed[0] != "a" || signed[1] != "b" || signed[2] != "c" {
		t.Fatalf("sessions signed for %v, want a, b, c", signed)
	}
	client := IPCClient{SocketPath: socket}
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readyCancel()
	if err := client.WaitReady(readyCtx); err != nil {
		t.Fatal(err)
	}
	status := ipcStatus(t, client)
	if status.JobVersion != "3/test/per-share" || len(status.Running) != 3 || len(status.Resources) != 3 {
		t.Fatalf("/status = %+v, want the job version and every share once", status)
	}
	for _, id := range []string{"a", "b", "c"} {
		if status.Running[id] != "crid-"+id || status.Resources[id].State != diagnosticStateServing {
			t.Fatalf("/status for %s = running %q diagnostic %+v", id, status.Running[id], status.Resources[id])
		}
	}
}

func TestNewShareManagerSingleModeStillSpendsOneAdmissionForEveryShare(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": daemonShare("a", 1, "on"),
		"b": daemonShare("b", 1, "on"),
		"c": daemonShare("c", 1, "on"),
	}}
	admitter := &fakeAdmitter{}
	sessions := &fakeSessionGroupFactory{}
	manager, err := NewShareManager(registry, &NativeGroupFactory{admitter: admitter, sessions: sessions}, GroupModeSingle)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.(*Manager); !ok {
		t.Fatalf("single mode built %T, want the one-group Manager", manager)
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
	waitPerShareServing(t, manager, "a", "b", "c")
	if admitter.admissions() != 1 || sessions.startCount() != 1 || len(sessions.session(1).RouteStates()) != 3 {
		t.Fatalf("single mode admissions/sessions/routes = %d/%d/%d, want 1/1/3",
			admitter.admissions(), sessions.startCount(), len(sessions.session(1).RouteStates()))
	}
}

func TestNewShareManagerBuildsPerShareAndRejectsUnknownModes(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{}}
	manager, err := NewShareManager(registry, newFakeGroupFactory(), GroupModePerShare)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.(*PerShareManager); !ok {
		t.Fatalf("per-share mode built %T, want PerShareManager", manager)
	}
	if _, err := NewShareManager(registry, newFakeGroupFactory(), GroupMode("both")); err == nil {
		t.Fatal("unknown mode built a manager")
	}
	if _, err := NewShareManager(nil, newFakeGroupFactory(), GroupModePerShare); err == nil {
		t.Fatal("nil registry built a manager")
	}
}

func TestPerShareManagerPublishAddsAGroupWithoutTouchingSiblings(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{"a": perShareRow("a", 1, "on")}}
	factory := newFakeGroupFactory()
	manager := newRunningPerShareManager(t, registry, factory)
	waitPerShareServing(t, manager, "a")
	second := perShareRow("b", 1, "on")
	registry.setShare(&second)
	manager.Trigger()
	waitPerShareServing(t, manager, "b")
	if got := factory.startCount(); got != 2 {
		t.Fatalf("group starts after publishing a second share = %d, want its own admission", got)
	}
	if cfg := groupConfigs(factory)[1]; cfg.ResourceID != "b" || len(cfg.Routes) != 1 {
		t.Fatalf("second group = %+v, want signed for b with only b", cfg)
	}
	a := runnerFor(t, factory, "a")
	if got := a.restartedRoutes(); len(got) != 0 {
		t.Fatalf("publishing b restarted a's route: %v", got)
	}
	for _, call := range a.setRouteCalls() {
		if len(call) != 1 || call[0] != "connector-a" {
			t.Fatalf("a's group was pushed %v, want only its own route", call)
		}
	}
}

func TestPerShareManagerStopRemovesExactlyOneGroup(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": perShareRow("a", 1, "on"),
		"b": perShareRow("b", 1, "on"),
	}}
	factory := newFakeGroupFactory()
	manager := newRunningPerShareManager(t, registry, factory)
	waitPerShareServing(t, manager, "a", "b")
	b := runnerFor(t, factory, "b")
	stopB := perShareRow("b", 2, "off")
	registry.setShare(&stopB)
	manager.Trigger()
	waitManagerCondition(t, func() bool {
		_, present := manager.Diagnostics()["b"]
		_, running := manager.Running()["b"]
		return !present && !running
	}, "stopped share pruned")
	// b's group retired its own session: its runner's Run has returned.
	waitManagerCondition(t, func() bool {
		select {
		case <-b.runStart:
		default:
			return false
		}
		manager.mu.Lock()
		defer manager.mu.Unlock()
		_, kept := manager.groups["b"]
		return !kept
	}, "b's group removed")
	if got := factory.startCount(); got != 2 {
		t.Fatalf("group starts = %d, want no new admission on stop", got)
	}
	a := runnerFor(t, factory, "a")
	if manager.Diagnostics()["a"].State != diagnosticStateServing {
		t.Fatalf("sibling a state = %q, want serving", manager.Diagnostics()["a"].State)
	}
	if got := a.restartedRoutes(); len(got) != 0 {
		t.Fatalf("stopping b restarted a: %v", got)
	}
	for _, call := range a.setRouteCalls() {
		if len(call) != 1 || call[0] != "connector-a" {
			t.Fatalf("a's group was pushed %v, want only its own route", call)
		}
	}
	if got := b.setRouteCalls(); len(got) != 0 {
		t.Fatalf("b's retired group was pushed a route set: %v", got)
	}
}

func TestPerShareManagerRestartRebuildsOnlyThatShareGroup(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": perShareRow("a", 1, "on"),
		"b": perShareRow("b", 1, "on"),
	}}
	factory := newFakeGroupFactory()
	manager := newRunningPerShareManager(t, registry, factory)
	waitPerShareServing(t, manager, "a", "b")
	restartB := perShareRow("b", 2, "on")
	registry.setShare(&restartB)
	manager.Trigger()
	b := runnerFor(t, factory, "b")
	waitManagerCondition(t, func() bool {
		for _, id := range b.restartedRoutes() {
			if id == "connector-b" {
				return true
			}
		}
		return false
	}, "route b restarted on its own group")
	if got := b.restartedRoutes(); len(got) != 1 || got[0] != "connector-b" {
		t.Fatalf("restarted routes on b's group = %v, want only connector-b", got)
	}
	if got := runnerFor(t, factory, "a").restartedRoutes(); len(got) != 0 {
		t.Fatalf("restarting b restarted a's route: %v", got)
	}
	if got := runnerFor(t, factory, "a").RouteStates()["connector-a"].Route.Generation; got != 0 {
		t.Fatalf("sibling a generation = %d, want unchanged", got)
	}
	if got := factory.startCount(); got != 2 {
		t.Fatalf("group starts = %d, want no new admission on restart", got)
	}
	waitPerShareServing(t, manager, "a", "b")
}

// TestPerShareManagerRefusedShareRetiresAndRebuildsOnlyItsOwnGroup pins the
// per-group reading of a platform refusal: the refused share stays desired-on
// and visible as retrying/platform_denied, its own group (its only route
// withheld) is retired and re-knocked after the backoff, and the sibling's
// group is never restarted, re-admitted, or pushed anything but its own route.
func TestPerShareManagerRefusedShareRetiresAndRebuildsOnlyItsOwnGroup(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": perShareRow("a", 1, "on"),
		"b": perShareRow("b", 1, "on"),
	}}
	factory := newFakeGroupFactory()
	manager := newRunningPerShareManager(t, registry, factory)
	waitPerShareServing(t, manager, "a", "b")
	first := runnerFor(t, factory, "b")
	first.failRoute("connector-b", errors.Join(connectorshare.ErrResourceGone,
		errors.New("resource_not_found: resource does not match signed NHP session")))
	waitManagerCondition(t, func() bool {
		return manager.Diagnostics()["b"].State == diagnosticStateRetrying
	}, "refused share reported retrying")
	if got := manager.Diagnostics()["b"]; got.FailureCategory != diagnosticFailurePlatformDenied || got.NextRetryAt == nil {
		t.Fatalf("refused share diagnostic = %+v, want retrying/platform_denied with a next retry", got)
	}
	if registry.share("b").DesiredState != "on" {
		t.Fatal("a platform refusal persisted the share off")
	}
	// A lifecycle command reconciles while b is withheld: b's group has nothing
	// to push, so it retires its session; a's group is untouched.
	manager.Trigger()
	waitManagerCondition(t, func() bool { return len(runnersFor(factory, "b")) == 2 }, "b's group rebuilt after its backoff")
	waitPerShareServing(t, manager, "b")
	if got := factory.startCount(); got != 3 {
		t.Fatalf("group starts = %d, want a's one plus b's original and rebuilt groups", got)
	}
	if cfg := groupConfigs(factory)[2]; cfg.ResourceID != "b" || len(cfg.Routes) != 1 || cfg.Routes[0].RouteID != "connector-b" {
		t.Fatalf("rebuilt group = %+v, want signed for b with only b", cfg)
	}
	a := runnerFor(t, factory, "a")
	if len(runnersFor(factory, "a")) != 1 || len(a.restartedRoutes()) != 0 || manager.Diagnostics()["a"].State != diagnosticStateServing {
		t.Fatalf("sibling a disturbed: groups=%d restarts=%v state=%q",
			len(runnersFor(factory, "a")), a.restartedRoutes(), manager.Diagnostics()["a"].State)
	}
	for _, call := range a.setRouteCalls() {
		if len(call) != 1 || call[0] != "connector-a" {
			t.Fatalf("a's group was pushed %v, want only its own route", call)
		}
	}
}

func TestPerShareManagerKnockDeniedShareConvergesOffWhileSiblingsServe(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": perShareRow("a", 1, "on"),
		"b": perShareRow("b", 5, "on"),
	}}
	factory := newFakeGroupFactory()
	// b's own knock is denied as resource-not-found: only b is unservable.
	factory.runErrByResource = map[string]error{"b": errors.Join(connectorshare.ErrResourceGone, errors.New("knock resource gone"))}
	manager := newRunningPerShareManager(t, registry, factory)
	waitPerShareServing(t, manager, "a")
	waitManagerCondition(t, func() bool { return registry.share("b").DesiredState == "off" }, "denied share persisted off")
	if got := registry.share("b").ServingEpoch; got != 5 {
		t.Fatalf("denied share persisted epoch = %d, want its own 5", got)
	}
	waitManagerCondition(t, func() bool {
		_, present := manager.Diagnostics()["b"]
		_, running := manager.Running()["b"]
		return !present && !running
	}, "denied share pruned")
	// The denied group is not rebuilt for a row that is now off.
	time.Sleep(60 * time.Millisecond)
	if got := factory.startCount(); got != 2 {
		t.Fatalf("group starts = %d, want no re-knock for the denied share", got)
	}
	if registry.share("a").DesiredState != "on" || manager.Diagnostics()["a"].State != diagnosticStateServing {
		t.Fatalf("sibling a = %q/%q, want on and serving", registry.share("a").DesiredState, manager.Diagnostics()["a"].State)
	}
	if got := runnerFor(t, factory, "a").restartedRoutes(); len(got) != 0 {
		t.Fatalf("b's denial restarted a: %v", got)
	}
}

func TestPerShareManagerStopsEveryGroupWhenRunEnds(t *testing.T) {
	registry := &memoryRegistry{shares: map[string]connectorstate.LocalShare{
		"a": perShareRow("a", 1, "on"),
		"b": perShareRow("b", 1, "on"),
	}}
	factory := newFakeGroupFactory()
	manager, err := NewPerShareManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	waitPerShareServing(t, manager, "a", "b")
	manager.mu.Lock()
	groups := []*shareGroup{manager.groups["a"], manager.groups["b"]}
	manager.mu.Unlock()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want the cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	for _, group := range groups {
		select {
		case <-group.done:
		default:
			t.Fatal("a group outlived Run")
		}
	}
	if got := manager.Running(); len(got) != 0 {
		t.Fatalf("running set after Run ended = %v, want empty", got)
	}
}

func TestPerShareManagerListFailureStopsEveryGroup(t *testing.T) {
	listErr := errors.New("registry unavailable")
	registry := &memoryRegistry{
		shares:       map[string]connectorstate.LocalShare{"a": perShareRow("a", 1, "on")},
		listFailures: []error{nil, listErr},
	}
	factory := newFakeGroupFactory()
	manager, err := NewPerShareManager(registry, factory)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- manager.Run(context.Background()) }()
	waitPerShareServing(t, manager, "a")
	manager.Trigger()
	select {
	case err := <-done:
		if !errors.Is(err, listErr) {
			t.Fatalf("Run = %v, want the registry failure", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the registry failure")
	}
	if got := manager.Running(); len(got) != 0 {
		t.Fatalf("running set after failure = %v, want every group stopped", got)
	}
}
