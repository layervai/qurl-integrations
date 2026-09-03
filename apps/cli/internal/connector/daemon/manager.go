package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"

	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

// ErrResourceGone marks a permanent resource-local denial. It mirrors
// connectorshare.ErrResourceGone so a fake group factory in tests can signal a
// permanently unavailable route without importing the connector sentinel.
// TODO(upstream-contract): keep in step with connectorshare.ErrResourceGone.
var ErrResourceGone = errors.New("local share resource is permanently unavailable")

// The durable registry may hold exactly as many shares as one admission
// carries routes. This pins that equality at compile time — the daemon package
// is the one place that imports both sides — so the two caps cannot drift.
// Both expressions must be non-negative, which only holds when they are equal.
// TODO(upstream-contract): connectorstate.LocalSharesMaxItems == connectorshare.MaxGroupRoutes.
const (
	_ = uint(connectorshare.MaxGroupRoutes - connectorstate.LocalSharesMaxItems)
	_ = uint(connectorstate.LocalSharesMaxItems - connectorshare.MaxGroupRoutes)
)

// desiredStateOn is the desired-state value a share carries while it should be
// served. It mirrors the state package's durable encoding.
const desiredStateOn = "on"

const (
	diagnosticStateStarting = "starting"
	diagnosticStateRetrying = "retrying"
	diagnosticStateServing  = "serving"
	diagnosticStateFailed   = "failed"
	// diagnosticStateStopped is no longer emitted by this daemon (the group
	// runner has no per-session stopped diagnostic); it is retained only so the
	// IPC decoder still accepts it from an older daemon across the socket.
	diagnosticStateStopped = "stopped"

	diagnosticFailureAssignment          = "assignment"
	diagnosticFailureEnrollment          = "enrollment"
	diagnosticFailureIdentity            = "identity"
	diagnosticFailureLocalState          = "local_state"
	diagnosticFailureNetwork             = "network"
	diagnosticFailurePeerTimeout         = "peer_timeout"
	diagnosticFailurePlatformDenied      = "platform_denied"
	diagnosticFailureResourceUnavailable = "resource_unavailable"
	diagnosticFailureUnknown             = "unknown"
	diagnosticFailureVerification        = "verification"
)

// Registry is the durable desired-state surface consumed by the daemon.
type Registry interface {
	List(context.Context) ([]connectorstate.LocalShare, error)
	DisableAtCurrentEpoch(context.Context, string, uint64) (*connectorstate.LocalShare, error)
}

// GroupEvents wires one session-group runner's callbacks into the manager so
// per-resource diagnostics and durable resource-gone persistence stay driven
// by route phases rather than by a per-session Done() signal.
type GroupEvents struct {
	// OnRouteServing reports that routeID reached FRP's running phase.
	OnRouteServing func(routeID string)
	// OnRouteFailed reports a permanent (ErrResourceGone) or transient
	// (ErrRouteNotServing) route failure. The whole group is not affected.
	OnRouteFailed func(routeID string, err error)
	// OnRetry reports a group-wide admission or connection retry and the
	// bounded delay before the next attempt. No route is serving during it.
	OnRetry func(err error, wait time.Duration)
	// OnRotationLeadCapped reports that the admission window is too short for
	// the lead the current route count needs.
	OnRotationLeadCapped func(routes int, need, lead time.Duration)
}

// GroupConfig is the group identity, initial route set, and event sink the
// manager hands the factory when it builds a runner.
type GroupConfig struct {
	KnockResourceID string
	ResourceID      string
	Routes          []connectorshare.LocalHTTPRoute
	Events          GroupEvents
}

// GroupRunner is the subset of connectorshare.SessionGroupRunner the
// manager drives. Every desired-on share is one route on this single runner.
type GroupRunner interface {
	Run(context.Context) error
	SetRoutes(context.Context, []connectorshare.LocalHTTPRoute) error
	RestartRoute(context.Context, string) error
	RouteStates() map[string]connectorshare.RouteState
}

// GroupFactory builds the one route-group runner a daemon runs. Construction
// may open the native runtime and admitter lazily (a transient open failure is
// returned and retried with backoff), but it performs no NHP admission itself:
// a permanent denial for the group's protected resource surfaces later from the
// runner's Run as an error wrapping connectorshare.ErrResourceGone, which the
// manager treats as a non-benign exit and retries with bounded backoff.
type GroupFactory interface {
	NewGroupRunner(context.Context, *GroupConfig) (GroupRunner, error)
}

// Manager reconciles durable local intent into one Connector session group.
// Every desired-on share is one route on a single SessionGroupRunner, so the
// whole set costs one knock, one login, one authorization, and one heartbeat
// stream instead of one of each per share.
type Manager struct {
	registry Registry
	factory  GroupFactory

	mu          sync.Mutex
	tracked     map[string]trackedShare       // resource ID -> applied definition
	routeToRes  map[string]string             // group route ID -> resource ID
	diagnostics map[string]ResourceDiagnostic // resource ID -> redacted state
	retry       map[string]int                // resource ID -> transient failure count
	persisting  map[string]struct{}           // resource ID -> resource-gone persistence in flight

	runner        GroupRunner
	runnerCancel  context.CancelFunc
	runnerDone    chan struct{}
	runnerRunning bool
	// runnerStopRequested records that the manager (not a runner failure) asked
	// the current runner to stop, so finishRunner classifies the exit by intent
	// rather than by whether the error happens to wrap a context cancellation.
	runnerStopRequested bool
	groupFailures       int
	groupRetryAt        time.Time

	// lifetime bounds background work (resource-gone persistence retries) to
	// the daemon's Run; it is set when Run starts.
	lifetime context.Context

	trigger chan struct{}

	resourceGonePersistTimeout time.Duration
	runnerStopTimeout          time.Duration
	retryDelay                 func(int) time.Duration
}

type trackedShare struct {
	share connectorstate.LocalShare
	route connectorshare.LocalHTTPRoute
}

// ResourceDiagnostic is the redacted, resource-local daemon state exposed by
// owner-only IPC. It contains no endpoint, credential, receipt, or topology.
type ResourceDiagnostic struct {
	State           string     `json:"state"`
	LastTransition  time.Time  `json:"last_transition"`
	FailureCategory string     `json:"failure_category,omitempty"`
	FailureCode     string     `json:"failure_code,omitempty"`
	RetryAttempt    int        `json:"retry_attempt"`
	NextRetryAt     *time.Time `json:"next_retry_at,omitempty"`
}

// NewManager builds a session-group share reconciler.
func NewManager(registry Registry, factory GroupFactory) (*Manager, error) {
	if registry == nil || factory == nil {
		return nil, errors.New("share daemon requires a registry and group factory")
	}
	return &Manager{
		registry: registry, factory: factory,
		tracked: map[string]trackedShare{}, routeToRes: map[string]string{},
		diagnostics: map[string]ResourceDiagnostic{}, retry: map[string]int{},
		persisting:                 map[string]struct{}{},
		trigger:                    make(chan struct{}, 1),
		resourceGonePersistTimeout: 5 * time.Second,
		runnerStopTimeout:          10 * time.Second,
		retryDelay:                 daemonRetryDelay,
	}, nil
}

// Trigger requests a coalesced asynchronous reconciliation.
func (m *Manager) Trigger() {
	select {
	case m.trigger <- struct{}{}:
	default:
	}
}

// Run reconciles until ctx ends. Every exit path stops the session group so a
// registry or reconciliation failure cannot bypass exact admission retirement.
func (m *Manager) Run(ctx context.Context) (retErr error) {
	m.mu.Lock()
	m.lifetime = ctx
	m.mu.Unlock()
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), m.runnerStopTimeout)
		defer cancel()
		retErr = errors.Join(retErr, m.stopRunner(stopCtx))
	}()
	if err := m.Reconcile(ctx); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.trigger:
			if err := m.Reconcile(ctx); err != nil {
				return err
			}
		}
	}
}

// Reconcile applies one crash-safe desired-state snapshot. It computes the
// desired route set from every desired-on share, then pushes it to the live
// session group (adding, removing, and restarting individual routes) or starts
// a fresh group when none is running.
//
// ctx bounds this reconcile only. When Reconcile starts a group it detaches the
// runner from ctx (see launchRunner), so a caller must not pass a short-lived
// request context expecting it to scope the session group — Run's context is
// what owns the group's lifetime. Production drives Reconcile solely from Run's
// long-lived context; only tests call it directly.
func (m *Manager) Reconcile(ctx context.Context) error {
	shares, err := m.registry.List(ctx)
	if err != nil {
		return fmt.Errorf("list desired local shares: %w", err)
	}
	desired := desiredShares(shares)
	m.pruneUndesired(desired)
	if len(desired) == 0 {
		return m.stopRunnerForEmptyGroup(ctx)
	}
	if m.groupIsRunning() {
		return m.applyDesired(ctx, desired)
	}
	return m.startGroup(ctx, desired)
}

// desiredShares returns the desired-on shares keyed and ordered by resource ID.
func desiredShares(shares []connectorstate.LocalShare) []connectorstate.LocalShare {
	desired := make([]connectorstate.LocalShare, 0, len(shares))
	for i := range shares {
		if shares[i].DesiredState == desiredStateOn {
			desired = append(desired, shares[i])
		}
	}
	sort.Slice(desired, func(i, j int) bool { return desired[i].ResourceID < desired[j].ResourceID })
	return desired
}

func shareRoute(share *connectorstate.LocalShare) connectorshare.LocalHTTPRoute {
	return connectorshare.LocalHTTPRoute{
		RouteID: share.ConnectorID, LocalIP: share.LocalIP, LocalPort: share.LocalPort,
		ResourceID: share.ResourceID, ConnectorRoutingID: share.ConnectorRoutingID,
	}
}

// pruneUndesired forgets tracked state and diagnostics for resources that are
// no longer desired-on, so a stopped or deleted share leaves no residue.
func (m *Manager) pruneUndesired(desired []connectorstate.LocalShare) {
	keep := make(map[string]struct{}, len(desired))
	for i := range desired {
		keep[desired[i].ResourceID] = struct{}{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for resourceID := range m.tracked {
		if _, ok := keep[resourceID]; ok {
			continue
		}
		delete(m.routeToRes, m.tracked[resourceID].route.RouteID)
		delete(m.tracked, resourceID)
		delete(m.diagnostics, resourceID)
		delete(m.retry, resourceID)
	}
}

// restartEntry is one route whose serving epoch advanced with an unchanged
// target. Its new definition is committed to tracked only after RestartRoute
// succeeds, so a transient restart failure is re-detected on the next
// reconcile rather than lost behind an already-advanced epoch.
type restartEntry struct {
	routeID string
	share   connectorstate.LocalShare
}

// applyDesired pushes the desired route set to the live group and restarts any
// route whose serving epoch advanced without a target change.
func (m *Manager) applyDesired(ctx context.Context, desired []connectorstate.LocalShare) error {
	routes := make([]connectorshare.LocalHTTPRoute, 0, len(desired))
	for i := range desired {
		routes = append(routes, shareRoute(&desired[i]))
	}
	restart, runner := m.recordDesired(desired)
	if runner == nil {
		return nil
	}
	if err := runner.SetRoutes(ctx, routes); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// TODO(upstream-contract): the runner records its own divergence on a
		// failed SetRoutes and re-applies the desired set under its own context
		// (qurl-connector pkg/share). We also schedule our own bounded reconcile
		// so a newly published share still converges even if that self-heal ever
		// regresses; a resource-local push failure must not fail the daemon or
		// disturb serving siblings.
		slog.WarnContext(ctx, "share daemon could not push the desired route set; will re-apply",
			"routes", len(routes), "error", redactShareError(err))
		m.scheduleGroupRetry(ctx, m.retryDelay(1))
	}
	restartFailed := false
	for i := range restart {
		entry := &restart[i]
		if err := runner.RestartRoute(ctx, entry.routeID); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Leave the tracked epoch unadvanced so the next reconcile
			// re-detects and re-attempts this restart; a single transient
			// failure must not strand the route on the stale epoch.
			restartFailed = true
			slog.WarnContext(ctx, "share daemon could not restart a route; will retry",
				"resource_id", entry.share.ResourceID, "error", redactShareError(err))
			continue
		}
		m.commitRestart(&entry.share)
	}
	if restartFailed {
		// Schedule a bounded reconcile so the deferred restart converges
		// without waiting for the next lifecycle command.
		m.scheduleGroupRetry(ctx, m.retryDelay(1))
	}
	return nil
}

// recordDesired updates the tracked definition for every desired share except
// restart-only ones (a serving-epoch advance with an unchanged target), which
// it returns so applyDesired can advance their epoch only after RestartRoute
// succeeds. It returns the restart entries and the live runner.
func (m *Manager) recordDesired(desired []connectorstate.LocalShare) ([]restartEntry, GroupRunner) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var restart []restartEntry
	for i := range desired {
		share := desired[i]
		route := shareRoute(&share)
		previous, existed := m.tracked[share.ResourceID]
		switch {
		case !existed:
			m.seedStartingLocked(share.ResourceID)
		case previous.route == route && share.ServingEpoch > previous.share.ServingEpoch:
			// Restart: defer the epoch commit until RestartRoute succeeds.
			restart = append(restart, restartEntry{routeID: route.RouteID, share: share})
			continue
		case previous.route.RouteID != route.RouteID:
			// The route's Connector ID changed; drop its stale reverse mapping
			// so a late callback for the old route ID cannot resolve to it.
			delete(m.routeToRes, previous.route.RouteID)
		}
		m.tracked[share.ResourceID] = trackedShare{share: share, route: route}
		m.routeToRes[route.RouteID] = share.ResourceID
	}
	sort.Slice(restart, func(i, j int) bool { return restart[i].routeID < restart[j].routeID })
	return restart, m.runner
}

// commitRestart advances the tracked definition for a route after its restart
// has been accepted by the live group.
func (m *Manager) commitRestart(share *connectorstate.LocalShare) {
	route := shareRoute(share)
	m.mu.Lock()
	m.tracked[share.ResourceID] = trackedShare{share: *share, route: route}
	m.routeToRes[route.RouteID] = share.ResourceID
	m.mu.Unlock()
}

func (m *Manager) commitRestarts(restart []restartEntry) {
	for i := range restart {
		m.commitRestart(&restart[i].share)
	}
}

// startGroup builds and launches one session group from the desired shares.
// The group knocks once for the whole set; each route carries its own public
// resource identity in its FRP proxy metadata.
func (m *Manager) startGroup(ctx context.Context, desired []connectorstate.LocalShare) error {
	// Record desired state before the backoff gate so a share published while
	// the group is backing off still gets a seeded diagnostic (otherwise
	// waitForSharingWithDiagnostics has no daemon cause to surface). A fresh
	// group start subsumes any per-route restart accumulated during downtime —
	// the new admission already retires the stale session — so commit those
	// epochs now rather than issuing a spurious RestartRoute on a later cycle.
	restart, _ := m.recordDesired(desired)
	m.commitRestarts(restart)
	if wait := m.groupRestartWait(); wait > 0 {
		m.scheduleGroupRetry(ctx, wait)
		return nil
	}
	routes := make([]connectorshare.LocalHTTPRoute, 0, len(desired))
	for i := range desired {
		routes = append(routes, shareRoute(&desired[i]))
	}
	// A daemon serves exactly one Connector, so every desired share is a proxy
	// of that Connector and shares its one knock resource. The representative's
	// public resource identity is the group's protected resource; the server
	// still authorizes each proxy by its own metadata.
	representative := desired[0]
	warnHeterogeneousKnock(ctx, desired, &representative)
	runner, err := m.factory.NewGroupRunner(ctx, &GroupConfig{
		KnockResourceID: representative.KnockResourceID,
		ResourceID:      representative.ResourceID,
		Routes:          routes,
		Events:          m.groupEvents(),
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		m.recordGroupFailure(ctx, err)
		return nil
	}
	m.launchRunner(ctx, runner)
	return nil
}

// warnHeterogeneousKnock loudly reports a desired set whose shares do not all
// carry the representative's knock resource. A daemon serves one Connector, so
// every row should share one knock resource; the group knocks once for the
// representative's and registers every route under it. Divergence cannot occur
// for a real single-Connector machine, so this is a data-integrity alarm.
func warnHeterogeneousKnock(ctx context.Context, desired []connectorstate.LocalShare, representative *connectorstate.LocalShare) {
	for i := range desired {
		if desired[i].KnockResourceID != representative.KnockResourceID {
			slog.WarnContext(ctx, "share daemon local shares disagree on the Connector knock resource; the whole group will knock once for the representative's",
				"representative_resource_id", representative.ResourceID)
			return
		}
	}
}

func (m *Manager) launchRunner(parent context.Context, runner GroupRunner) {
	runCtx, cancel := context.WithCancel(context.WithoutCancel(parent))
	stopOnParent := context.AfterFunc(parent, cancel)
	done := make(chan struct{})
	m.mu.Lock()
	m.runner = runner
	m.runnerCancel = cancel
	m.runnerDone = done
	m.runnerRunning = true
	m.runnerStopRequested = false
	// The group is launching; a scheduled retry window, if any, is now spent.
	// groupFailures is deliberately not reset here — it is reset on the first
	// route serving so a start-then-crash cycle keeps escalating its backoff
	// toward the ceiling rather than pinning at the first step.
	m.groupRetryAt = time.Time{}
	m.mu.Unlock()
	go func() {
		err := runner.Run(runCtx)
		stopOnParent()
		// Sample the parent before the goroutine's own cancel so a daemon
		// shutdown is recognized as an intentional stop, not a failure.
		parentCanceled := parent.Err() != nil
		cancel()
		m.finishRunner(parent, runner, err, parentCanceled)
		close(done)
	}()
}

// finishRunner records a runner exit. parent is the daemon-lifetime context,
// not the runner's own (already-canceled) run context. Classification is by
// intent, not by whether the error wraps a context cancellation: an emptied
// group, a daemon shutdown (parentCanceled), or a manager-driven stop
// (runnerStopRequested) is benign and resets the backoff. A whole-group
// permanent denial (ErrResourceGone from the shared admission) converges every
// share to off instead of re-knocking forever. Any other exit schedules a
// backed-off reconciliation so a failed group is rebuilt without a hot
// re-knock loop.
func (m *Manager) finishRunner(parent context.Context, runner GroupRunner, err error, parentCanceled bool) {
	m.mu.Lock()
	if m.runner != runner {
		m.mu.Unlock()
		return
	}
	m.runner = nil
	m.runnerCancel = nil
	m.runnerRunning = false
	stopRequested := m.runnerStopRequested
	m.runnerStopRequested = false
	if err == nil || errors.Is(err, connectorshare.ErrGroupEmpty) || parentCanceled || stopRequested {
		m.groupFailures = 0
		m.groupRetryAt = time.Time{}
		m.mu.Unlock()
		m.Trigger()
		return
	}
	if errors.Is(err, connectorshare.ErrResourceGone) {
		// The whole group's Connector knock resource is permanently gone, so
		// every share is unservable. Converge each to a durable off rather than
		// leaving an infinite quiet re-knock loop. Gate any rebuild behind a
		// backoff window so the pending persists land and the desired set
		// empties instead of the group re-knocking for still-on rows in
		// between; a persist failure still lets the backed-off retry re-drive it.
		category, code := classifyShareFailure(err)
		gone := make([]connectorstate.LocalShare, 0, len(m.tracked))
		for resourceID := range m.tracked {
			m.diagnostics[resourceID] = ResourceDiagnostic{
				State: diagnosticStateFailed, LastTransition: time.Now().UTC(),
				FailureCategory: category, FailureCode: code,
			}
			if _, inFlight := m.persisting[resourceID]; inFlight {
				continue
			}
			m.persisting[resourceID] = struct{}{}
			gone = append(gone, m.tracked[resourceID].share)
		}
		m.groupFailures++
		delay := m.retryDelay(m.groupFailures)
		m.groupRetryAt = time.Now().Add(delay)
		m.mu.Unlock()
		for i := range gone {
			go m.persistResourceGone(&gone[i])
		}
		m.scheduleGroupRetry(parent, delay)
		return
	}
	m.groupFailures++
	delay := m.retryDelay(m.groupFailures)
	m.groupRetryAt = time.Now().Add(delay)
	m.markTrackedRetryingLocked(err, delay)
	m.mu.Unlock()
	m.scheduleGroupRetry(parent, delay)
}

func (m *Manager) recordGroupFailure(ctx context.Context, err error) {
	m.mu.Lock()
	m.groupFailures++
	delay := m.retryDelay(m.groupFailures)
	m.groupRetryAt = time.Now().Add(delay)
	m.markTrackedRetryingLocked(err, delay)
	m.mu.Unlock()
	m.scheduleGroupRetry(ctx, delay)
}

func (m *Manager) groupRestartWait() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.groupRetryAt.IsZero() {
		return 0
	}
	return time.Until(m.groupRetryAt)
}

// scheduleGroupRetry triggers a reconciliation after delay so a failed group
// rebuild is retried with bounded backoff.
func (m *Manager) scheduleGroupRetry(ctx context.Context, delay time.Duration) {
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
			m.Trigger()
		}
	}()
}

func (m *Manager) groupEvents() GroupEvents {
	return GroupEvents{
		OnRouteServing:       m.onRouteServing,
		OnRouteFailed:        m.onRouteFailed,
		OnRetry:              m.onGroupRetry,
		OnRotationLeadCapped: m.onRotationLeadCapped,
	}
}

func (m *Manager) onRouteServing(routeID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	resourceID, ok := m.routeToRes[routeID]
	if !ok {
		return
	}
	delete(m.retry, resourceID)
	// A route reaching serving means the group's admission and login succeeded,
	// so the group is healthy: reset the whole-group start backoff toward its
	// first step for the next time a rebuild is ever needed.
	m.groupFailures = 0
	m.groupRetryAt = time.Time{}
	m.diagnostics[resourceID] = ResourceDiagnostic{State: diagnosticStateServing, LastTransition: time.Now().UTC()}
}

func (m *Manager) onRouteFailed(routeID string, cause error) {
	m.mu.Lock()
	resourceID, ok := m.routeToRes[routeID]
	if !ok {
		m.mu.Unlock()
		return
	}
	gone := errors.Is(cause, ErrResourceGone) || errors.Is(cause, connectorshare.ErrResourceGone)
	if !gone {
		m.markRetryingLocked(resourceID, cause, m.retryDelay(m.retry[resourceID]+1))
		m.retry[resourceID]++
		m.mu.Unlock()
		return
	}
	category, code := classifyShareFailure(cause)
	m.diagnostics[resourceID] = ResourceDiagnostic{
		State: diagnosticStateFailed, LastTransition: time.Now().UTC(),
		FailureCategory: category, FailureCode: code,
	}
	share := m.tracked[resourceID].share
	if _, inFlight := m.persisting[resourceID]; inFlight {
		// A persistence retry loop for this resource is already running; a
		// re-emitted gone signal must not spawn a second one that races for the
		// cross-process registry lock and rewrites the whole file again.
		m.mu.Unlock()
		return
	}
	m.persisting[resourceID] = struct{}{}
	m.mu.Unlock()
	go m.persistResourceGone(&share)
}

func (m *Manager) onGroupRetry(cause error, wait time.Duration) {
	slog.WarnContext(m.lifetimeContext(), "share daemon session group attempt failed; retrying",
		"retry_in", wait, "error", redactShareError(cause))
	m.mu.Lock()
	defer m.mu.Unlock()
	m.markTrackedRetryingLocked(cause, wait)
}

func (m *Manager) onRotationLeadCapped(routes int, need, lead time.Duration) {
	slog.WarnContext(m.lifetimeContext(), "share daemon session group rotation lead is capped below the route count's need",
		"routes", routes, "needed", need, "lead", lead)
}

// markTrackedRetryingLocked marks every route that is not currently serving as
// retrying. A group-wide admission or connection retry stalls the whole set.
func (m *Manager) markTrackedRetryingLocked(cause error, wait time.Duration) {
	for resourceID := range m.tracked {
		if m.diagnostics[resourceID].State == diagnosticStateServing {
			continue
		}
		m.markRetryingLocked(resourceID, cause, wait)
		m.retry[resourceID]++
	}
}

func (m *Manager) markRetryingLocked(resourceID string, cause error, wait time.Duration) {
	category, code := classifyShareFailure(cause)
	now := time.Now().UTC()
	next := now.Add(wait)
	attempt := m.retry[resourceID] + 1
	m.diagnostics[resourceID] = ResourceDiagnostic{
		State: diagnosticStateRetrying, LastTransition: now, FailureCategory: category,
		FailureCode: code, RetryAttempt: attempt, NextRetryAt: &next,
	}
}

func (m *Manager) seedStartingLocked(resourceID string) {
	m.diagnostics[resourceID] = ResourceDiagnostic{State: diagnosticStateStarting, LastTransition: time.Now().UTC()}
}

// persistResourceGone durably disables one permanently denied row, retrying
// with bounded backoff until it lands, without letting a wedged filesystem
// operation stall the daemon or disturb healthy siblings. It runs detached
// (the group has already withdrawn the route), bounded by the daemon lifetime,
// and each attempt carries its own deadline. It clears the in-flight guard and,
// on success, triggers a reconcile so the now-off row is pruned from tracked
// state and stops being reported by Running(). A concurrently deleted row is
// already terminal.
func (m *Manager) persistResourceGone(share *connectorstate.LocalShare) {
	if share == nil || share.ResourceID == "" {
		return
	}
	defer func() {
		m.mu.Lock()
		delete(m.persisting, share.ResourceID)
		m.mu.Unlock()
	}()
	parent := m.lifetimeContext()
	for attempt := 1; ; attempt++ {
		if parent.Err() != nil {
			return
		}
		persistCtx, cancel := context.WithTimeout(parent, m.resourceGonePersistTimeout)
		_, err := m.registry.DisableAtCurrentEpoch(persistCtx, share.ResourceID, share.ServingEpoch)
		cancel()
		if err == nil || errors.Is(err, os.ErrNotExist) {
			// The row is now off; reconcile so it is dropped from the desired
			// route set and no longer reported as running.
			m.Trigger()
			return
		}
		slog.ErrorContext(parent, "share daemon could not persist permanently unavailable resource as off; retrying resource",
			"resource_id", share.ResourceID, "error", redactShareError(err))
		timer := time.NewTimer(m.retryDelay(attempt))
		select {
		case <-parent.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (m *Manager) lifetimeContext() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lifetime == nil {
		return context.Background()
	}
	return m.lifetime
}

func (m *Manager) groupIsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runnerRunning
}

// stopRunnerForEmptyGroup cancels the session group once no share is desired-on
// so its single admission is retired instead of knocking for an empty set.
func (m *Manager) stopRunnerForEmptyGroup(ctx context.Context) error {
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.runnerStopTimeout)
	defer cancel()
	return m.stopRunner(stopCtx)
}

// stopRunner cancels the live runner and waits for it to retire its admission,
// bounded by ctx so one wedged shutdown cannot hang the daemon. It marks the
// stop as manager-requested first so finishRunner classifies the resulting
// exit as intentional rather than as a failure to back off from.
func (m *Manager) stopRunner(ctx context.Context) error {
	m.mu.Lock()
	cancel := m.runnerCancel
	done := m.runnerDone
	if cancel != nil {
		m.runnerStopRequested = true
	}
	m.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// maxIPCStatusBytes bounds the owner-only /status payload. A daemon serves at
// most MaxGroupRoutes routes; each appears once in the running map (resource ID
// plus CRID) and once in the resources map (resource ID plus a redacted
// diagnostic), together well under 1 KiB, so this bound holds a full fleet with
// generous headroom while staying finite.
// TODO(upstream-contract): sized from connectorshare.MaxGroupRoutes.
const maxIPCStatusBytes = (connectorshare.MaxGroupRoutes + 24) * 1024

// Running returns a snapshot of the active public resource IDs and CRIDs. It is
// empty while no session group is running, so a share never reports as managed
// before the native runtime has actually taken ownership of it.
func (m *Manager) Running() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.runnerRunning {
		return map[string]string{}
	}
	result := make(map[string]string, len(m.tracked))
	for resourceID := range m.tracked {
		result[resourceID] = m.tracked[resourceID].share.CRID
	}
	return result
}

// Diagnostics returns one redacted snapshot per managed resource.
func (m *Manager) Diagnostics() map[string]ResourceDiagnostic {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]ResourceDiagnostic, len(m.diagnostics))
	for resourceID, diagnostic := range m.diagnostics {
		result[resourceID] = diagnostic
	}
	return result
}

func daemonRetryDelay(attempt int) time.Duration {
	delay := time.Second
	for i := 1; i < attempt && delay < 30*time.Second; i++ {
		delay *= 2
	}
	if delay > 30*time.Second {
		return 30 * time.Second
	}
	return delay
}
