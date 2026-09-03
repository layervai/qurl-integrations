package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	v1 "github.com/fatedier/frp/pkg/config/v1"
	"github.com/layervai/qurl-connector/pkg/agentstate"
	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	qurl "github.com/layervai/qurl-go/qurl"
	"github.com/layervai/qurl-go/relayknock/nativeudp"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

// ErrDirectEgressRequired reports an environment proxy configuration that
// would split the source address used for admission from the Connector route.
var ErrDirectEgressRequired = errors.New("qURL local sharing requires direct egress")

// DefaultFRPCommon builds the daemon's shared immutable FRP client defaults.
func DefaultFRPCommon(dialTimeoutSeconds, keepaliveSeconds int64) (*v1.ClientCommonConfig, error) {
	loginFailExit := true
	common := &v1.ClientCommonConfig{LoginFailExit: &loginFailExit}
	common.Transport.DialServerTimeout = dialTimeoutSeconds
	common.Transport.DialServerKeepAlive = keepaliveSeconds
	if err := common.Complete(); err != nil {
		return nil, fmt.Errorf("complete qURL daemon tunnel configuration: %w", err)
	}
	// TODO(upstream-contract): FRP derives ProxyURL from lowercase http_proxy during Complete and repeats
	// that derivation inside NewService. Native UDP session admission is
	// source-IP-bound, so reject a proxy that would split the tunnel's egress
	// before the tunnel performs network I/O.
	if common.Transport.ProxyURL != "" {
		return nil, fmt.Errorf("%w: lowercase http_proxy is set", ErrDirectEgressRequired)
	}
	return common, nil
}

// ResourceAdmitter is the credential-free native session surface consumed by
// the daemon. qurl-connector's NativeAdmitter is the production implementation.
type ResourceAdmitter interface {
	connectorshare.Admitter
	MarkServingHealthy() error
	Close() error
}

// NativeGroupFactory is the daemon adapter around qurl-connector's single
// production NHP/FRP session-group engine. One native admitter and one FRP
// session-group factory back every group runner the manager builds, so the
// whole route set shares one knock, one login, and one heartbeat stream.
type NativeGroupFactory struct {
	admitter ResourceAdmitter
	sessions *connectorshare.FRPSessionGroupFactory
}

// NewNativeGroupFactory adapts qurl-connector's production session-group engine.
func NewNativeGroupFactory(admitter ResourceAdmitter, common *v1.ClientCommonConfig, version string) (*NativeGroupFactory, error) {
	if admitter == nil || common == nil {
		return nil, errors.New("native group factory requires an admitter and FRP common config")
	}
	sessions, err := connectorshare.NewFRPSessionGroupFactory(connectorshare.FRPGroupFactoryConfig{
		Common: common, ClientVersion: version,
	})
	if err != nil {
		return nil, err
	}
	return &NativeGroupFactory{admitter: admitter, sessions: sessions}, nil
}

// NewGroupRunner builds one SessionGroupRunner bound to the shared native
// admitter. It performs no network I/O before returning; the runner's Run
// method owns admission and FRP session lifetime.
func (f *NativeGroupFactory) NewGroupRunner(ctx context.Context, cfg *GroupConfig) (GroupRunner, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return connectorshare.NewSessionGroupRunner(connectorshare.SessionGroupConfig{
		KnockResourceID: cfg.KnockResourceID,
		ResourceID:      cfg.ResourceID,
		Routes:          cfg.Routes,
		Admitter:        f.admitter,
		Sessions:        f.sessions,
		OnServing: func(connectorshare.Admission) {
			// A failed marker clear is retried on the next serving cycle; it
			// must never tear down a healthy route or its siblings.
			_ = f.admitter.MarkServingHealthy()
		},
		OnRouteServing: func(routeID string, _ connectorshare.Admission) {
			if cfg.Events.OnRouteServing != nil {
				cfg.Events.OnRouteServing(routeID)
			}
		},
		OnRouteFailed:        cfg.Events.OnRouteFailed,
		OnRetry:              cfg.Events.OnRetry,
		OnRotationLeadCapped: cfg.Events.OnRotationLeadCapped,
	})
}

// Close ends the shared native admitter after every group runner has stopped.
func (f *NativeGroupFactory) Close() error {
	if f == nil || f.admitter == nil {
		return nil
	}
	return f.admitter.Close()
}

// DeferredGroupFactory lets the daemon bind IPC and transfer ownership to
// launchd before the native runtime opens. Initialization runs on the manager
// goroutine only when at least one desired-on share exists; every group runner
// the manager later builds shares the one initialized native delegate.
type DeferredGroupFactory struct {
	initialize func(context.Context) (GroupFactory, error)

	mu             sync.Mutex
	delegate       GroupFactory
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

// NewDeferredGroupFactory delays native runtime initialization until needed.
func NewDeferredGroupFactory(initialize func(context.Context) (GroupFactory, error)) (*DeferredGroupFactory, error) {
	if initialize == nil {
		return nil, errors.New("deferred share group factory requires an initializer")
	}
	factory := &DeferredGroupFactory{initialize: initialize}
	factory.cond = sync.NewCond(&factory.mu)
	return factory, nil
}

var errDeferredFactoryClosed = errors.New("deferred share group factory is closed")

// NewGroupRunner serializes native initialization and caches only a successful
// delegate. A transient open failure is returned to that reconciliation; the
// manager's next retry performs a fresh initialization attempt. The check that
// no initialization is in flight and the flag that starts one are a single
// critical section, so two concurrent callers never both begin a native open.
func (f *DeferredGroupFactory) NewGroupRunner(ctx context.Context, cfg *GroupConfig) (GroupRunner, error) {
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
			return f.newRunnerFromDelegate(ctx, delegate, cfg)
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
		// Initialization follows this call until it finishes, but a successful
		// native runtime must not retain the caller context. Close owns the
		// independent lifetime cancellation after successful handoff.
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
			err = errors.New("deferred share group factory initialized without a delegate")
		}
		if err == nil {
			err = initCtx.Err()
		}

		f.mu.Lock()
		closed := f.closed
		if err == nil && !closed {
			f.delegate = delegate
			// The successful native runtime owns this context for its full
			// lifetime. Canceling the one-shot initialization context here
			// would close the admitter before the first group admission.
			f.delegateCancel = cancel
			f.initialize = nil
		} else {
			cancel()
		}
		f.initCancel = nil
		if closed && delegate != nil {
			// Close the just-created delegate before waking Close or any waiter
			// so shutdown cannot return while native state is leaked.
			f.mu.Unlock()
			closeErr := closeGroupFactory(delegate)
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
				return nil, errors.Join(err, closeGroupFactory(delegate))
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

func (f *DeferredGroupFactory) newRunnerFromDelegate(ctx context.Context, delegate GroupFactory, cfg *GroupConfig) (GroupRunner, error) {
	runner, startErr := delegate.NewGroupRunner(ctx, cfg)
	f.mu.Lock()
	closed := f.closed
	f.activeStarts--
	f.cond.Broadcast()
	f.mu.Unlock()
	if !closed {
		return runner, startErr
	}
	// The factory closed while this runner was being built. The runner has not
	// been launched, so dropping it is sufficient; the delegate's admitter is
	// closed by Close.
	return nil, errors.Join(errDeferredFactoryClosed, startErr)
}

// Close releases an initialized native delegate.
func (f *DeferredGroupFactory) Close() error {
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
	closeErr := closeGroupFactory(delegate)
	f.mu.Lock()
	f.closeErr = errors.Join(priorErr, closeErr)
	finalErr := f.closeErr
	close(f.closeDone)
	f.mu.Unlock()
	return finalErr
}

func closeGroupFactory(factory GroupFactory) error {
	closer, ok := factory.(interface{ Close() error })
	if !ok || closer == nil {
		return nil
	}
	return closer.Close()
}

// redactShareError keeps credentials out of daemon logs derived from a native
// attempt error.
func redactShareError(err error) string {
	if err == nil {
		return ""
	}
	return qurlapi.Redact(err.Error())
}

func classifyShareFailure(err error) (category, code string) { //nolint:gocognit,gocyclo // Keep the closed failure taxonomy in one precedence-ordered boundary.
	if err == nil {
		return diagnosticFailureUnknown, ""
	}
	// This typed observation is more precise than the operation wrappers that
	// can contain it. It does not claim whether the peer or the path was at
	// fault, and it exposes no destination.
	if errors.Is(err, qurl.ErrEndpointNoReply) {
		return diagnosticFailurePeerTimeout, ""
	}
	// These recovery wrappers carry a cause, but their top-level state is more
	// useful than that cause. For example, a completed credential replacement
	// can wrap an assignment identity denial while it waits for the refreshed
	// assignment. That is assignment recovery, not a second identity failure.
	switch {
	case errors.Is(err, qurl.ErrCredentialRecoveryCandidatePersistence):
		return diagnosticFailureLocalState, ""
	case errors.Is(err, qurl.ErrCredentialRecoveredAssignmentRefreshRequired):
		return diagnosticFailureAssignment, ""
	case errors.Is(err, qurl.ErrCredentialRecoveryExpired):
		return diagnosticFailureIdentity, ""
	case errors.Is(err, qurl.ErrDeviceCredentialMissing),
		errors.Is(err, qurl.ErrCredentialRecoveryRequired):
		return diagnosticFailureIdentity, ""
	}
	var deny *qurl.ServerDenyError
	if errors.As(err, &deny) {
		return diagnosticFailurePlatformDenied, safeDiagnosticCode(deny.ErrCode)
	}
	// Credential recovery is an identity-stage operation. Keep its closed,
	// non-secret code so inspect can distinguish an authenticated recovery
	// refusal from an untyped daemon or network timeout.
	var recovery *qurl.CredentialRecoveryError
	if errors.As(err, &recovery) {
		switch {
		case errors.Is(err, qurl.ErrRecoveryCredentialRejected),
			errors.Is(err, qurl.ErrCredentialRecoveryIdentityRejected),
			errors.Is(err, qurl.ErrCredentialRecoveryRevokeRequired):
			return diagnosticFailureIdentity, safeDiagnosticCode(recovery.Code)
		case errors.Is(err, qurl.ErrCredentialRecoveryAssignmentRequired):
			return diagnosticFailureAssignment, safeDiagnosticCode(recovery.Code)
		default:
			// The remaining closed recovery results are authenticated
			// platform outcomes: temporary authority failure, rate limiting,
			// invalid request, rejected grant, or candidate conflict. They are
			// not proof of a bad device identity or a local network failure.
			return diagnosticFailurePlatformDenied, safeDiagnosticCode(recovery.Code)
		}
	}
	var assignment *qurl.AssignmentError
	if errors.As(err, &assignment) {
		// AssignmentError also carries the identity and enrollment-credential
		// denials. Classify those before the broad assignment family while
		// retaining the safe closed-taxonomy code.
		if errors.Is(err, qurl.ErrAssignmentIdentityRejected) ||
			errors.Is(err, qurl.ErrAssignmentKeyRejected) ||
			errors.Is(err, qurl.ErrAssignmentBootstrapConsumed) {
			return diagnosticFailureIdentity, safeDiagnosticCode(assignment.Code)
		}
		return diagnosticFailureAssignment, safeDiagnosticCode(assignment.Code)
	}
	var completion *qurl.CompletionError
	if errors.As(err, &completion) {
		category := diagnosticFailureEnrollment
		switch {
		case errors.Is(err, qurl.ErrCompletionIdentityRejected):
			category = diagnosticFailureIdentity
		case errors.Is(err, qurl.ErrDeviceKeyQuotaExceeded),
			errors.Is(err, qurl.ErrCompletionCredentialConflict):
			category = diagnosticFailurePlatformDenied
		}
		code := safeDiagnosticCode(completion.Code)
		if !strings.HasPrefix(code, "523") {
			code = ""
		}
		return category, code
	}
	for _, sentinel := range []error{
		qurl.ErrAssignmentIdentityRejected, qurl.ErrAssignmentKeyRejected,
		qurl.ErrAssignmentBootstrapConsumed, qurl.ErrRecoveryCredentialRejected,
		qurl.ErrCredentialRecoveryIdentityRejected, qurl.ErrCredentialRecoveryRevokeRequired,
		qurl.ErrInvalidAgentState, qurl.ErrInsecureAgentStatePermissions,
		qurl.ErrKeyRejected, qurl.ErrBootstrapSetupKeyConsumed,
		qurl.ErrCompletionIdentityRejected, qurl.ErrAgentIdentityConflict,
	} {
		if errors.Is(err, sentinel) {
			return diagnosticFailureIdentity, ""
		}
	}
	for _, sentinel := range []error{
		qurl.ErrAssignmentUnavailable, qurl.ErrAssignmentRecoveryRequired,
		qurl.ErrAssignmentReassignmentRequired, qurl.ErrAssignmentRateLimited,
		qurl.ErrAssignmentLeaseExpired, qurl.ErrNativeSessionOperationLeaseMargin,
		qurl.ErrCredentialRecoveryAssignmentRequired,
	} {
		if errors.Is(err, sentinel) {
			return diagnosticFailureAssignment, ""
		}
	}
	for _, sentinel := range []error{
		qurl.ErrRegistrationRecoveryRequired, qurl.ErrRegistrationRateLimited,
		qurl.ErrAssignmentTicketInvalid, qurl.ErrAssignmentTicketExpired,
		qurl.ErrRegistrationInvalidInput,
		qurl.ErrRegisterReplyMalformed, qurl.ErrRegistrationKeyKindDisallowed,
		qurl.ErrCompletionUnavailable, qurl.ErrCompletionRequestRejected,
		qurl.ErrCompletionRecoveryRequired,
	} {
		if errors.Is(err, sentinel) {
			return diagnosticFailureEnrollment, ""
		}
	}
	for _, sentinel := range []error{
		qurl.ErrInvalidRegisterConfig, qurl.ErrInvalidNativeKnockInput, qurl.ErrAgentBindingPersistence,
		qurl.ErrAgentCompletionCandidatePersistence, qurl.ErrAgentSetupLock,
		qurl.ErrInvalidNativeSessionOperation, agentstate.ErrSessionOperationConflict,
		agentstate.ErrSessionOperationJournalCorrupt,
	} {
		if errors.Is(err, sentinel) {
			return diagnosticFailureLocalState, ""
		}
	}
	for _, sentinel := range []error{
		qurl.ErrRegistrationDisabled, qurl.ErrDeviceKeyQuotaExceeded,
		qurl.ErrCompletionCredentialConflict,
	} {
		if errors.Is(err, sentinel) {
			return diagnosticFailurePlatformDenied, ""
		}
	}
	for _, sentinel := range []error{
		nativeudp.ErrServerUnauthenticated, qurl.ErrMalformedReply,
	} {
		if errors.Is(err, sentinel) {
			return diagnosticFailureVerification, ""
		}
	}
	for _, sentinel := range []error{
		nativeudp.ErrResolve, nativeudp.ErrTransport,
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
	for _, sentinel := range []error{
		qurl.ErrCredentialRecoveryUnavailable, qurl.ErrCredentialRecoveryRateLimited,
		qurl.ErrCredentialReplacementUnavailable, qurl.ErrCredentialRecoveryGrantRejected,
		qurl.ErrCredentialRecoveryRequestRejected, qurl.ErrCredentialRecoveryCandidateConflict,
		qurl.ErrCredentialRecoveryRetryRequired, qurl.ErrCredentialRecoveryInvalidResponse,
	} {
		if errors.Is(err, sentinel) {
			return diagnosticFailurePlatformDenied, ""
		}
	}
	if errors.Is(err, ErrResourceGone) || errors.Is(err, connectorshare.ErrResourceGone) ||
		errors.Is(err, connectorshare.ErrRouteNotServing) {
		return diagnosticFailureResourceUnavailable, ""
	}
	return diagnosticFailureUnknown, ""
}

// safeDiagnosticCode keeps the daemon producer inside the exact IPC contract.
// qurl-go accepts a wider authenticated decimal error grammar for some NHP
// replies, but inspect exposes only the closed five-digit public taxonomy.
func safeDiagnosticCode(code string) string {
	if !validDiagnosticCode(code) {
		return ""
	}
	return code
}

var (
	_ GroupFactory = (*NativeGroupFactory)(nil)
	_ GroupFactory = (*DeferredGroupFactory)(nil)
)
