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

	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

// ErrResourceGone marks a permanent resource-local denial.
var ErrResourceGone = errors.New("local share resource is permanently unavailable")

const (
	diagnosticStateStarting = "starting"
	diagnosticStateRetrying = "retrying"
	diagnosticStateServing  = "serving"
	diagnosticStateFailed   = "failed"
	diagnosticStateStopped  = "stopped"

	diagnosticFailureAssignment          = "assignment"
	diagnosticFailureIdentity            = "identity"
	diagnosticFailureLocalState          = "local_state"
	diagnosticFailureNetwork             = "network"
	diagnosticFailurePlatformDenied      = "platform_denied"
	diagnosticFailureResourceUnavailable = "resource_unavailable"
	diagnosticFailureUnknown             = "unknown"
)

// Registry is the durable desired-state surface consumed by the daemon.
type Registry interface {
	List(context.Context) ([]connectorstate.LocalShare, error)
	DisableTerminal(context.Context, string, uint64) (*connectorstate.LocalShare, error)
}

// Session is one independently managed resource route.
type Session interface {
	Done() <-chan struct{}
	Err() error
	// Stop must honor ctx. Manager enforces a per-resource deadline so one
	// wedged route cannot block reconciliation of healthy siblings.
	Stop(context.Context) error
}

// SessionFactory starts resource-local background sessions.
type SessionFactory interface {
	// Start must only construct and launch the resource's background runner;
	// network admission happens inside Session. A slow or unavailable resource
	// therefore cannot block reconciliation of healthy siblings.
	Start(context.Context, *connectorstate.LocalShare) (Session, error)
}

// Manager reconciles durable local intent into independent resource sessions.
type Manager struct {
	registry Registry
	factory  SessionFactory

	mu                  sync.Mutex
	sessions            map[string]*managedSession
	failures            map[string]int
	retrying            map[string]bool
	retryGeneration     map[string]uint64
	diagnostics         map[string]ResourceDiagnostic
	nextRetryGeneration uint64
	trigger             chan struct{}

	resourceStopTimeout        time.Duration
	resourceGonePersistTimeout time.Duration
	retryDelay                 func(int) time.Duration
}

type managedSession struct {
	share   connectorstate.LocalShare
	session Session
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

type diagnosticSession interface {
	Diagnostic() ResourceDiagnostic
}

// NewManager builds a resource-isolated share reconciler.
func NewManager(registry Registry, factory SessionFactory) (*Manager, error) {
	if registry == nil || factory == nil {
		return nil, errors.New("share daemon requires a registry and session factory")
	}
	return &Manager{
		registry: registry, factory: factory,
		sessions: map[string]*managedSession{}, failures: map[string]int{}, retrying: map[string]bool{},
		retryGeneration: map[string]uint64{}, diagnostics: map[string]ResourceDiagnostic{}, trigger: make(chan struct{}, 1),
		resourceStopTimeout: 10 * time.Second, resourceGonePersistTimeout: 5 * time.Second, retryDelay: daemonRetryDelay,
	}, nil
}

// Trigger requests a coalesced asynchronous reconciliation.
func (m *Manager) Trigger() {
	select {
	case m.trigger <- struct{}{}:
	default:
	}
}

// Run reconciles until ctx ends. Every exit path stops all active sessions so
// a registry or reconciliation failure cannot bypass exact receipt retirement.
func (m *Manager) Run(ctx context.Context) (retErr error) {
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		retErr = errors.Join(retErr, m.StopAll(stopCtx))
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

// Reconcile applies one crash-safe desired-state snapshot.
func (m *Manager) Reconcile(ctx context.Context) error {
	shares, err := m.registry.List(ctx)
	if err != nil {
		return fmt.Errorf("list desired local shares: %w", err)
	}
	desired := desiredShareSet(shares)
	m.pruneRetryState(desired)
	toStop := m.detachReplacedSessions(desired)
	if err := m.stopReplacedSessions(ctx, desired, toStop); err != nil {
		return err
	}
	return m.startDesiredSessions(ctx, desired)
}

// pruneRetryState forgets backoff only after a reconciliation observes that a
// resource is no longer desired. If the same resource ID is published later,
// it starts as a new lifecycle instead of inheriting an old failure delay.
// An already-scheduled timer sees that its generation was removed and exits;
// it cannot clear or trigger a later retry for a republished resource.
func (m *Manager) pruneRetryState(desired map[string]*connectorstate.LocalShare) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for resourceID := range m.failures {
		if _, present := desired[resourceID]; !present {
			delete(m.failures, resourceID)
			delete(m.retrying, resourceID)
			delete(m.retryGeneration, resourceID)
			delete(m.diagnostics, resourceID)
		}
	}
}

func desiredShareSet(shares []connectorstate.LocalShare) map[string]*connectorstate.LocalShare {
	sort.Slice(shares, func(i, j int) bool { return shares[i].ResourceID < shares[j].ResourceID })
	desired := make(map[string]*connectorstate.LocalShare, len(shares))
	for index := range shares {
		share := &shares[index]
		if share.DesiredState == "on" {
			desired[share.ResourceID] = share
		}
	}
	return desired
}

func (m *Manager) detachReplacedSessions(desired map[string]*connectorstate.LocalShare) []*managedSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	toStop := make([]*managedSession, 0)
	for resourceID, current := range m.sessions {
		want, keep := desired[resourceID]
		if keep && sameSessionDefinition(&current.share, want) {
			delete(desired, resourceID)
			continue
		}
		toStop = append(toStop, current)
		delete(m.sessions, resourceID)
	}
	return toStop
}

func (m *Manager) stopReplacedSessions(ctx context.Context, desired map[string]*connectorstate.LocalShare, toStop []*managedSession) error {
	for _, current := range toStop {
		stopCtx, cancel := context.WithTimeout(ctx, m.resourceStopTimeout)
		err := stopSession(stopCtx, current.session)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// A resource-local stop timeout must not terminate the daemon and
			// tear down healthy siblings. Keep a session that may still be live
			// under management, suppress its replacement to avoid overlapping
			// target/epoch definitions, and retry only this resource. The
			// existing watcher remains valid because it compares session identity.
			select {
			case <-current.session.Done():
				// The route is already gone despite its terminal Stop error, so a
				// desired-on replacement can start below without overlap.
			default:
				m.mu.Lock()
				if _, replaced := m.sessions[current.share.ResourceID]; !replaced {
					m.sessions[current.share.ResourceID] = current
				}
				m.scheduleRetryLocked(ctx, current.share.ResourceID, err)
				m.mu.Unlock()
				delete(desired, current.share.ResourceID)
			}
		}
	}
	return nil
}

func (m *Manager) startDesiredSessions(ctx context.Context, desired map[string]*connectorstate.LocalShare) error {
	for _, share := range sortedDesired(desired) {
		session, err := m.factory.Start(ctx, share)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, ErrResourceGone) {
				if !m.persistResourceGone(ctx, share) {
					m.mu.Lock()
					m.scheduleRetryLocked(ctx, share.ResourceID, ErrResourceGone)
					m.mu.Unlock()
				}
				continue
			}
			m.mu.Lock()
			m.scheduleRetryLocked(ctx, share.ResourceID, err)
			m.mu.Unlock()
			continue
		}
		m.mu.Lock()
		delete(m.failures, share.ResourceID)
		delete(m.retrying, share.ResourceID)
		delete(m.retryGeneration, share.ResourceID)
		m.sessions[share.ResourceID] = &managedSession{share: *share, session: session}
		if diagnostic, ok := session.(diagnosticSession); ok {
			m.diagnostics[share.ResourceID] = diagnostic.Diagnostic()
		} else {
			m.diagnostics[share.ResourceID] = ResourceDiagnostic{State: diagnosticStateStarting, LastTransition: time.Now().UTC()}
		}
		m.mu.Unlock()
		go m.watch(ctx, share.ResourceID, session)
	}
	return nil
}

func (m *Manager) watch(ctx context.Context, resourceID string, session Session) {
	<-session.Done()
	m.mu.Lock()
	current, ok := m.sessions[resourceID]
	if !ok || current.session != session {
		m.mu.Unlock()
		return
	}
	delete(m.sessions, resourceID)
	m.mu.Unlock()
	if errors.Is(session.Err(), ErrResourceGone) {
		if !m.persistResourceGone(ctx, &current.share) {
			m.mu.Lock()
			m.scheduleRetryLocked(ctx, resourceID, session.Err())
			m.mu.Unlock()
		}
		return
	}
	m.mu.Lock()
	m.scheduleRetryLocked(ctx, resourceID, session.Err())
	m.mu.Unlock()
}

// persistResourceGone durably disables one permanently denied row without
// letting a wedged filesystem operation stall the daemon. Failure is logged
// and reported to the caller so it can schedule resource-local reconciliation;
// healthy siblings remain untouched. A concurrently deleted row is already in
// the desired terminal state.
func (m *Manager) persistResourceGone(parent context.Context, share *connectorstate.LocalShare) bool {
	if share == nil {
		return true
	}
	if parent == nil {
		parent = context.Background()
	}
	persistCtx, cancel := context.WithTimeout(parent, m.resourceGonePersistTimeout)
	defer cancel()
	_, err := m.registry.DisableTerminal(persistCtx, share.ResourceID, share.ServingEpoch)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return true
	}
	slog.ErrorContext(parent, "share daemon could not persist permanently unavailable resource as off; retrying resource", "resource_id", share.ResourceID, "err", err)
	return false
}

// scheduleRetryLocked records a resource-local retry without failing the
// daemon or disturbing healthy siblings. m.mu must be held by the caller.
func (m *Manager) scheduleRetryLocked(ctx context.Context, resourceID string, cause error) {
	if m.retrying[resourceID] {
		return
	}
	m.failures[resourceID]++
	attempt := m.failures[resourceID]
	delay := m.retryDelay(attempt)
	now := time.Now().UTC()
	next := now.Add(delay)
	category, code := classifyShareFailure(cause)
	m.diagnostics[resourceID] = ResourceDiagnostic{
		State: diagnosticStateRetrying, LastTransition: now, FailureCategory: category,
		FailureCode: code, RetryAttempt: attempt, NextRetryAt: &next,
	}
	m.retrying[resourceID] = true
	m.nextRetryGeneration++
	generation := m.nextRetryGeneration
	m.retryGeneration[resourceID] = generation
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		fire := false
		select {
		case <-ctx.Done():
		case <-timer.C:
			fire = true
		}
		m.mu.Lock()
		if m.retryGeneration[resourceID] != generation {
			m.mu.Unlock()
			return
		}
		delete(m.retrying, resourceID)
		delete(m.retryGeneration, resourceID)
		m.mu.Unlock()
		if fire {
			m.Trigger()
		}
	}()
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

// StopAll detaches sessions under the lock and stops them without the lock held.
func (m *Manager) StopAll(ctx context.Context) error {
	m.mu.Lock()
	toStop := make([]Session, 0, len(m.sessions))
	for resourceID, current := range m.sessions {
		toStop = append(toStop, current.session)
		delete(m.sessions, resourceID)
	}
	m.mu.Unlock()
	return stopSessions(ctx, toStop)
}

// stopSessions gives every independent resource the same bounded shutdown
// window. A single route that consumes the deadline cannot prevent siblings
// from attempting exact receipt retirement.
func stopSessions(ctx context.Context, sessions []Session) error {
	results := make(chan error, len(sessions))
	for _, session := range sessions {
		go func() { results <- stopSession(ctx, session) }()
	}
	var joined error
	for range sessions {
		joined = errors.Join(joined, <-results)
	}
	return joined
}

// Running returns a snapshot of active public resource IDs and CRIDs.
func (m *Manager) Running() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]string, len(m.sessions))
	for resourceID, current := range m.sessions {
		result[resourceID] = current.share.CRID
	}
	return result
}

// Diagnostics returns one redacted snapshot per managed or retrying resource.
func (m *Manager) Diagnostics() map[string]ResourceDiagnostic {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]ResourceDiagnostic, len(m.diagnostics))
	for resourceID, diagnostic := range m.diagnostics {
		result[resourceID] = diagnostic
	}
	for resourceID, current := range m.sessions {
		if diagnostic, ok := current.session.(diagnosticSession); ok {
			result[resourceID] = diagnostic.Diagnostic()
		}
	}
	return result
}

func stopSession(ctx context.Context, session Session) error {
	if session == nil {
		return nil
	}
	return session.Stop(ctx)
}

func sameSessionDefinition(a, b *connectorstate.LocalShare) bool {
	return a.ResourceID == b.ResourceID && a.CRID == b.CRID && a.ConnectorRoutingID == b.ConnectorRoutingID &&
		a.KnockResourceID == b.KnockResourceID && a.TargetURL == b.TargetURL && a.LocalIP == b.LocalIP &&
		a.LocalPort == b.LocalPort && a.ServingEpoch == b.ServingEpoch
}

func sortedDesired(shares map[string]*connectorstate.LocalShare) []*connectorstate.LocalShare {
	result := make([]*connectorstate.LocalShare, 0, len(shares))
	for _, share := range shares {
		result = append(result, share)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ResourceID < result[j].ResourceID })
	return result
}
