package daemon

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	qurl "github.com/layervai/qurl-go/qurl"
)

// fakeAdmitter mints valid admissions for the real SessionGroupRunner without
// any NHP traffic, counting how many knocks the group spent.
type fakeAdmitter struct {
	mu   sync.Mutex
	next uint64
}

func (a *fakeAdmitter) Admit(_ context.Context, knockResourceID, resourceID string) (connectorshare.Admission, error) {
	runID, err := qurl.NewCycleRunID()
	if err != nil {
		return connectorshare.Admission{}, err
	}
	a.mu.Lock()
	a.next++
	sessionID := a.next
	a.mu.Unlock()
	return connectorshare.Admission{
		KnockResourceID: knockResourceID, ResourceID: resourceID,
		RunID: runID, RunAttempt: 1, Token: "ac-hermetic", ResourceHost: "127.0.0.1:7000",
		SessionID: sessionID,
		SessionReceipt: qurl.NativeSessionReceipt{
			CellID: "cell0", SessionID: sessionID, SessionIssuedAtMillis: time.Now().UnixMilli(),
			RunID: runID, RunAttempt: 1,
		},
		OpenTime: time.Hour,
	}, nil
}

func (*fakeAdmitter) Retire(context.Context, connectorshare.Admission) error { return nil }

func (*fakeAdmitter) MarkServingHealthy() error { return nil }
func (*fakeAdmitter) Close() error              { return nil }

func (a *fakeAdmitter) admissions() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.next
}

// fakeSessionGroupFactory stands in for the FRP session-group factory: every
// route registers immediately unless refuse says the platform rejects that
// registration, in which case the route fails exactly as a resource_not_found
// NewProxy answer does (RouteFailed wrapping ErrResourceGone).
type fakeSessionGroupFactory struct {
	mu       sync.Mutex
	sessions []*fakeGroupSession
	refuse   func(route *connectorshare.GroupRoute) bool
}

// Start's value-typed admission is fixed by connectorshare.SessionGroupFactory.
func (f *fakeSessionGroupFactory) Start(_ context.Context, admission connectorshare.Admission, routes []connectorshare.GroupRoute) (connectorshare.GroupServingSession, error) { //nolint:gocritic // hugeParam: signature is dictated by the connectorshare.SessionGroupFactory interface.
	session := &fakeGroupSession{
		admission: admission, refuse: f.refuse,
		routes: map[string]connectorshare.RouteState{}, registrations: map[string][]string{},
		ready: make(chan struct{}), done: make(chan struct{}), changes: make(chan struct{}, 1),
	}
	session.install(routes)
	f.mu.Lock()
	f.sessions = append(f.sessions, session)
	f.mu.Unlock()
	return session, nil
}

func (f *fakeSessionGroupFactory) session(index int) *fakeGroupSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	if index < 1 || index > len(f.sessions) {
		return nil
	}
	return f.sessions[index-1]
}

func (f *fakeSessionGroupFactory) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sessions)
}

type fakeGroupSession struct {
	admission connectorshare.Admission
	refuse    func(route *connectorshare.GroupRoute) bool

	mu     sync.Mutex
	routes map[string]connectorshare.RouteState
	// updates records every route set pushed to this session.
	updates [][]connectorshare.GroupRoute
	// registrations records every proxy name each route registered under.
	registrations map[string][]string
	err           error
	stopped       bool

	// ready is never closed: the runner drives off Changes/RouteStates, and
	// make-before-break rotation is out of scope for this fake.
	ready    chan struct{}
	done     chan struct{}
	changes  chan struct{}
	stopOnce sync.Once
}

func fakeProxyName(route *connectorshare.GroupRoute, sessionID uint64) string {
	name := route.RouteID + "-nhp" + strconv.FormatUint(sessionID, 36)
	if route.Generation > 0 {
		name += "-r" + strconv.FormatUint(route.Generation, 36)
	}
	return name
}

func (s *fakeGroupSession) install(routes []connectorshare.GroupRoute) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]connectorshare.RouteState, len(routes))
	for i := range routes {
		route := &routes[i]
		name := fakeProxyName(route, s.admission.SessionID)
		if current, ok := s.routes[route.RouteID]; ok && current.ProxyName == name && current.Route == *route {
			next[route.RouteID] = current
			continue
		}
		s.registrations[route.RouteID] = append(s.registrations[route.RouteID], name)
		state := connectorshare.RouteState{Route: *route, ProxyName: name, Phase: connectorshare.RouteServing}
		if s.refuse != nil && s.refuse(route) {
			state.Phase = connectorshare.RouteFailed
			state.Err = fmt.Errorf("%w: resource_not_found: resource does not match signed NHP session", connectorshare.ErrResourceGone)
		}
		next[route.RouteID] = state
	}
	s.routes = next
	s.notifyLocked()
}

func (s *fakeGroupSession) notifyLocked() {
	select {
	case s.changes <- struct{}{}:
	default:
	}
}

func (s *fakeGroupSession) Update(ctx context.Context, routes []connectorshare.GroupRoute) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return connectorshare.ErrSessionGroupEnded
	}
	s.updates = append(s.updates, append([]connectorshare.GroupRoute(nil), routes...))
	s.mu.Unlock()
	s.install(routes)
	return nil
}

func (s *fakeGroupSession) RouteStates() map[string]connectorshare.RouteState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]connectorshare.RouteState, len(s.routes))
	for id := range s.routes {
		out[id] = s.routes[id]
	}
	return out
}

func (s *fakeGroupSession) Changes() <-chan struct{} { return s.changes }
func (s *fakeGroupSession) Ready() <-chan struct{}   { return s.ready }
func (s *fakeGroupSession) Done() <-chan struct{}    { return s.done }
func (s *fakeGroupSession) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *fakeGroupSession) Stop(context.Context) error {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.err = errors.New("fake session stopped")
		s.notifyLocked()
		s.mu.Unlock()
		close(s.done)
	})
	return nil
}

// updateSizes returns the route count of every pushed set, in order.
func (s *fakeGroupSession) updateSizes() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	sizes := make([]int, 0, len(s.updates))
	for _, set := range s.updates {
		sizes = append(sizes, len(set))
	}
	return sizes
}

// proxyNames returns every proxy name routeID registered under, in order.
func (s *fakeGroupSession) proxyNames(routeID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.registrations[routeID]...)
}

var (
	_ ResourceAdmitter                   = (*fakeAdmitter)(nil)
	_ connectorshare.SessionGroupFactory = (*fakeSessionGroupFactory)(nil)
	_ connectorshare.GroupServingSession = (*fakeGroupSession)(nil)
)
