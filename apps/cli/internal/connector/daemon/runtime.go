package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	v1 "github.com/fatedier/frp/pkg/config/v1"
	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	qurl "github.com/layervai/qurl-go/qurl"
	"github.com/layervai/qurl-go/relayknock/nativeudp"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

// DefaultFRPCommon builds the daemon's shared immutable FRP client defaults.
func DefaultFRPCommon(dialTimeoutSeconds, keepaliveSeconds int64) (*v1.ClientCommonConfig, error) {
	loginFailExit := true
	common := &v1.ClientCommonConfig{LoginFailExit: &loginFailExit}
	common.Transport.DialServerTimeout = dialTimeoutSeconds
	common.Transport.DialServerKeepAlive = keepaliveSeconds
	if err := common.Complete(); err != nil {
		return nil, fmt.Errorf("complete qURL daemon tunnel configuration: %w", err)
	}
	return common, nil
}

// NativeSessionFactory is the daemon adapter around qurl-connector's single
// production NHP/FRP lifecycle engine. All resource runners share one
// serialized native admitter, while each resource keeps its own resource-bound
// NHP authorization and FRP control session.
type NativeSessionFactory struct {
	admitter ResourceAdmitter
	common   *v1.ClientCommonConfig
	version  string
}

// DeferredSessionFactory lets the daemon bind IPC and transfer ownership to
// launchd before a warm native open or automatic assignment recovery
// completes. Initialization runs on the manager goroutine only when at least
// one desired-on share exists; subsequent resource starts share the result.
type DeferredSessionFactory struct {
	initialize func(context.Context) (SessionFactory, error)

	mu             sync.Mutex
	delegate       SessionFactory
	delegateCancel context.CancelFunc
	initializing   bool
	ready          chan struct{}
	initCancel     context.CancelFunc
	closed         bool
	closing        bool
	closeDone      chan struct{}
	closeErr       error
	activeStarts   int
	cond           *sync.Cond
}

// NewDeferredSessionFactory delays native runtime initialization until needed.
func NewDeferredSessionFactory(initialize func(context.Context) (SessionFactory, error)) (*DeferredSessionFactory, error) {
	if initialize == nil {
		return nil, errors.New("deferred share session factory requires an initializer")
	}
	factory := &DeferredSessionFactory{initialize: initialize}
	factory.cond = sync.NewCond(&factory.mu)
	return factory, nil
}

var errDeferredFactoryClosed = errors.New("deferred share session factory is closed")

// Start serializes native initialization and caches only a successful
// delegate. A transient open failure is returned to that reconciliation; the
// manager's next retry performs a fresh initialization attempt.
func (f *DeferredSessionFactory) Start(ctx context.Context, local *connectorstate.LocalShare) (Session, error) {
	for {
		f.mu.Lock()
		if f.closed {
			f.mu.Unlock()
			return nil, errDeferredFactoryClosed
		}
		if f.delegate != nil {
			delegate := f.delegate
			f.activeStarts++
			f.mu.Unlock()
			return f.startDelegate(ctx, delegate, local)
		}
		if f.initializing {
			ready := f.ready
			f.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-ready:
				continue
			}
		}
		initialize := f.initialize
		// Initialization follows this Start call until it finishes, but a
		// successful native runtime must not retain the caller context. Close
		// owns the independent lifetime cancellation after successful handoff.
		initCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		stopCallerCancel := context.AfterFunc(ctx, cancel)
		f.initializing = true
		f.ready = make(chan struct{})
		f.initCancel = cancel
		ready := f.ready
		f.mu.Unlock()

		delegate, err := initialize(initCtx)
		settleInitializationCaller(ctx, cancel, stopCallerCancel)
		if err == nil && delegate == nil {
			err = errors.New("deferred share session factory initialized without a delegate")
		}
		if err == nil {
			err = initCtx.Err()
		}

		f.mu.Lock()
		closed := f.closed
		if err == nil && !closed {
			f.delegate = delegate
			// The successful native runtime owns this context for its full
			// lifetime. Canceling the one-shot initialization context here would
			// close the admitter before the first resource admission.
			f.delegateCancel = cancel
			f.initialize = nil
		} else {
			cancel()
		}
		f.initCancel = nil
		if closed && delegate != nil {
			// Close the just-created delegate before waking Close or any Start
			// waiter, so shutdown cannot return while native state is leaked.
			f.mu.Unlock()
			closeErr := closeSessionFactory(delegate)
			f.mu.Lock()
			f.closeErr = errors.Join(f.closeErr, closeErr)
			err = errors.Join(errDeferredFactoryClosed, err, closeErr)
		}
		f.initializing = false
		close(ready)
		f.cond.Broadcast()
		f.mu.Unlock()
		if err != nil {
			if !closed && delegate != nil {
				return nil, errors.Join(err, closeSessionFactory(delegate))
			}
			return nil, err
		}
		continue
	}
}

func settleInitializationCaller(ctx context.Context, cancel context.CancelFunc, stopCallerCancel func() bool) {
	if !stopCallerCancel() || ctx.Err() != nil {
		// Either the cancellation callback has started or cancellation won the
		// handoff before the callback ran. Complete its effect synchronously so
		// the factory cannot cache that delegate as successful.
		cancel()
	}
}

func (f *DeferredSessionFactory) startDelegate(ctx context.Context, delegate SessionFactory, local *connectorstate.LocalShare) (Session, error) {
	session, startErr := delegate.Start(ctx, local)
	f.mu.Lock()
	closed := f.closed
	if !closed {
		// Checking closed and releasing the active call are one handoff. Close
		// cannot slip between them, close the delegate, and then let Start return
		// a session from that closed delegate.
		f.activeStarts--
		f.cond.Broadcast()
		f.mu.Unlock()
		return session, startErr
	}
	f.mu.Unlock()
	var stopErr error
	if session != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		stopErr = session.Stop(stopCtx)
		cancel()
		session = nil
	}
	f.mu.Lock()
	f.closeErr = errors.Join(f.closeErr, stopErr)
	f.activeStarts--
	f.cond.Broadcast()
	f.mu.Unlock()
	return nil, errors.Join(errDeferredFactoryClosed, startErr, stopErr)
}

// Close releases an initialized native delegate.
func (f *DeferredSessionFactory) Close() error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	if f.closing {
		done := f.closeDone
		f.mu.Unlock()
		<-done
		f.mu.Lock()
		err := f.closeErr
		f.mu.Unlock()
		return err
	}
	f.closing = true
	f.closeDone = make(chan struct{})
	f.closed = true
	if f.initCancel != nil {
		f.initCancel()
	}
	for f.initializing || f.activeStarts > 0 {
		f.cond.Wait()
	}
	delegate := f.delegate
	f.delegate = nil
	delegateCancel := f.delegateCancel
	f.delegateCancel = nil
	priorErr := f.closeErr
	f.mu.Unlock()
	if delegateCancel != nil {
		delegateCancel()
	}
	closeErr := closeSessionFactory(delegate)
	f.mu.Lock()
	f.closeErr = errors.Join(priorErr, closeErr)
	finalErr := f.closeErr
	close(f.closeDone)
	f.mu.Unlock()
	return finalErr
}

func closeSessionFactory(factory SessionFactory) error {
	closer, ok := factory.(interface{ Close() error })
	if !ok || closer == nil {
		return nil
	}
	return closer.Close()
}

// ResourceAdmitter is the credential-free native session surface consumed by
// the daemon. qurl-connector's NativeAdmitter is the production implementation.
type ResourceAdmitter interface {
	connectorshare.Admitter
	MarkServingHealthy() error
	Close() error
}

// NewNativeSessionFactory adapts qurl-connector's production lifecycle engine.
func NewNativeSessionFactory(admitter ResourceAdmitter, common *v1.ClientCommonConfig, version string) (*NativeSessionFactory, error) {
	if admitter == nil || common == nil {
		return nil, errors.New("native share session factory requires an admitter and FRP common config")
	}
	return &NativeSessionFactory{admitter: admitter, common: common, version: version}, nil
}

// Start constructs one resource runner and launches its admission loop in the
// background. It deliberately performs no network I/O before returning, so a
// broken resource cannot block reconciliation of healthy siblings.
func (f *NativeSessionFactory) Start(ctx context.Context, local *connectorstate.LocalShare) (Session, error) {
	if local == nil {
		return nil, errors.New("native share session requires local state")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	frpFactory, err := connectorshare.NewFRPSessionFactory(connectorshare.FRPFactoryConfig{
		Common: f.common,
		Route: connectorshare.LocalHTTPRoute{
			RouteID: local.ConnectorID, LocalIP: local.LocalIP, LocalPort: local.LocalPort,
			ResourceID: local.ResourceID, ConnectorRoutingID: local.ConnectorRoutingID,
		},
		ClientVersion: f.version,
	})
	if err != nil {
		return nil, err
	}
	// The manager owns the returned session and stops it explicitly. Retaining
	// a one-shot Reconcile context would couple a healthy route to that call.
	runCtx, cancel := context.WithCancel(context.Background())
	session := &nativeSession{cancel: cancel, done: make(chan struct{}), diagnostic: ResourceDiagnostic{
		State: diagnosticStateStarting, LastTransition: time.Now().UTC(),
	}}
	runner, err := connectorshare.NewResourceRunner(connectorshare.ResourceConfig{
		KnockResourceID: local.KnockResourceID,
		ResourceID:      local.ResourceID,
		Admitter:        f.admitter,
		Sessions:        frpFactory,
		OnServing: func(connectorshare.Admission) {
			// A failed marker clear is deliberately retried on the next serving
			// cycle. It must not tear down a healthy route or its siblings.
			_ = f.admitter.MarkServingHealthy()
			session.recordServing()
		},
		OnRetry: func(err error, wait time.Duration) {
			session.recordRetry(err, wait)
			slog.WarnContext(runCtx, "share daemon session attempt failed; retrying",
				"crid", local.CRID, "retry_in", wait, "error", qurlapi.Redact(err.Error()))
		},
	})
	if err != nil {
		cancel()
		return nil, err
	}
	go session.run(runCtx, runner)
	return session, nil
}

// Close ends all native agent sessions after every resource runner stops.
func (f *NativeSessionFactory) Close() error {
	if f == nil || f.admitter == nil {
		return nil
	}
	return f.admitter.Close()
}

type nativeSession struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu         sync.Mutex
	err        error
	once       sync.Once
	diagnostic ResourceDiagnostic
}

func (s *nativeSession) run(ctx context.Context, runner *connectorshare.ResourceRunner) {
	err := runner.Run(ctx)
	if errors.Is(err, connectorshare.ErrResourceGone) {
		err = errors.Join(ErrResourceGone, err)
	} else if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		err = nil
	}
	s.mu.Lock()
	s.err = err
	s.diagnostic.State = diagnosticStateStopped
	if err != nil {
		s.diagnostic.State = diagnosticStateFailed
		s.diagnostic.FailureCategory, s.diagnostic.FailureCode = classifyShareFailure(err)
	}
	s.diagnostic.LastTransition = time.Now().UTC()
	s.diagnostic.NextRetryAt = nil
	s.mu.Unlock()
	close(s.done)
}

// Done closes after the resource runner exits.
func (s *nativeSession) Done() <-chan struct{} { return s.done }

// Err returns the resource runner's terminal result.
func (s *nativeSession) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Diagnostic returns the current redacted state for owner-only IPC.
func (s *nativeSession) Diagnostic() ResourceDiagnostic {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.diagnostic
}

func (s *nativeSession) recordServing() {
	s.mu.Lock()
	s.diagnostic = ResourceDiagnostic{State: diagnosticStateServing, LastTransition: time.Now().UTC()}
	s.mu.Unlock()
}

func (s *nativeSession) recordRetry(err error, wait time.Duration) {
	now := time.Now().UTC()
	next := now.Add(wait)
	category, code := classifyShareFailure(err)
	s.mu.Lock()
	s.diagnostic.State = diagnosticStateRetrying
	s.diagnostic.LastTransition = now
	s.diagnostic.FailureCategory = category
	s.diagnostic.FailureCode = code
	s.diagnostic.RetryAttempt++
	s.diagnostic.NextRetryAt = &next
	s.mu.Unlock()
}

func classifyShareFailure(err error) (category, code string) {
	if err == nil {
		return diagnosticFailureUnknown, ""
	}
	var deny *qurl.ServerDenyError
	if errors.As(err, &deny) {
		return diagnosticFailurePlatformDenied, deny.ErrCode
	}
	var assignment *qurl.AssignmentError
	if errors.As(err, &assignment) {
		return diagnosticFailureAssignment, assignment.Code
	}
	for _, sentinel := range []error{
		qurl.ErrAssignmentIdentityRejected, qurl.ErrAssignmentKeyRejected,
		qurl.ErrInvalidAgentState, qurl.ErrInsecureAgentStatePermissions,
		qurl.ErrRegistrationInvalidInput, qurl.ErrKeyRejected,
	} {
		if errors.Is(err, sentinel) {
			return diagnosticFailureIdentity, ""
		}
	}
	for _, sentinel := range []error{
		qurl.ErrAssignmentUnavailable, qurl.ErrAssignmentRecoveryRequired,
		qurl.ErrAssignmentReassignmentRequired, qurl.ErrAssignmentRateLimited,
		qurl.ErrAssignmentLeaseExpired, qurl.ErrNativeSessionOperationLeaseMargin,
	} {
		if errors.Is(err, sentinel) {
			return diagnosticFailureAssignment, ""
		}
	}
	for _, sentinel := range []error{
		qurl.ErrEndpointNoReply, nativeudp.ErrResolve, nativeudp.ErrTransport,
		nativeudp.ErrNoReply, context.DeadlineExceeded,
	} {
		if errors.Is(err, sentinel) {
			return diagnosticFailureNetwork, ""
		}
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) || errors.Is(err, os.ErrPermission) {
		return diagnosticFailureLocalState, ""
	}
	if errors.Is(err, ErrResourceGone) || errors.Is(err, connectorshare.ErrResourceGone) {
		return diagnosticFailureResourceUnavailable, ""
	}
	return diagnosticFailureUnknown, ""
}

// Stop cancels and joins the resource runner.
func (s *nativeSession) Stop(ctx context.Context) error {
	s.once.Do(s.cancel)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return s.Err()
	}
}

var _ SessionFactory = (*NativeSessionFactory)(nil)
var _ SessionFactory = (*DeferredSessionFactory)(nil)
var _ Session = (*nativeSession)(nil)
