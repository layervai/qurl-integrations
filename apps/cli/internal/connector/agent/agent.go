// Package agent orchestrates the CLI qURL Connector's native agent
// lifecycle around the qurl-go SDK: first-time enrollment with a one-shot
// token, warm re-open of the persisted device identity, and the
// operator-gated assignment refresh that self-heals a stale Hub binding.
//
// The package is glue, not protocol: registration, assignment, warm open,
// and refresh transactions (and all cryptography) live in qurl-go. What lives
// here is the ordering and the fail-closed policy between those calls — which
// credential may be spent when, when a refresh is allowed to consume its
// one-per-episode budget, and how a stalled first registration is explained
// to the operator.
//
// Credential model: the enrollment credential consumed here is the ONE-SHOT
// Connector enrollment token, deliberately distinct from the CLI's durable
// qURL API key (internal/auth). The two never share an env var or a file, so
// a durable key can never be spent as an enrollment token or vice versa.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	qurl "github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

// Config carries the caller-owned inputs for opening the Connector runtime.
// The command layer resolves flags and passes them here; this package reads
// environment variables only for the documented operator overrides.
type Config struct {
	// APIBaseURL is the versioned qURL API base URL (the CLI's configured
	// endpoint). The SDK origin is derived from it; see ResourceSDKOrigin.
	APIBaseURL string

	// EnrollmentToken is an explicit one-shot enrollment credential for
	// embedders and tests. The CLI command deliberately never binds it — the
	// token must never travel through argv, so the customer surface is
	// QURL_CONNECTOR_TOKEN_FILE / QURL_CONNECTOR_TOKEN only, which apply
	// whenever this is empty. It is only consulted (and only spent) on a
	// first registration; warm opens and refreshes use the persisted device
	// identity.
	EnrollmentToken string

	// EnrollmentTokenProvider lazily supplies a one-shot credential for a
	// first registration. It takes precedence over the legacy environment/file
	// enrollment surface, allowing `qurl publish` to mint an exact Connector-
	// bound token from the logged-in credential without exposing it in argv,
	// environment, or durable state. `qurl connector run` leaves this nil and
	// retains its existing explicit token behavior.
	EnrollmentTokenProvider EnrollmentTokenProvider

	// StateDir overrides state-directory resolution (flag-first). Empty
	// falls through to the state package's env/XDG chain.
	StateDir string

	// RefreshMode is the flag-first override for the operator's assignment
	// refresh gate (manual|auto|disabled). Empty falls through to the
	// LAYERV_AGENT_REGISTRATION_REFRESH_MODE environment contract, then to
	// manual; see ResolveRefreshMode.
	RefreshMode string

	// AgentID optionally pins the persisted native agent identity,
	// flag-first. Empty falls back to the LAYERV_AGENT_ID override, then to
	// the identity qurl-go generates and persists.
	AgentID string

	// Version is the CLI build version stamped into registration metadata.
	Version string

	// Logger receives orchestration events; nil uses slog.Default().
	Logger *slog.Logger
}

// EnrollmentTokenRequest is the non-secret durable identity context supplied
// to a lazy enrollment token provider.
type EnrollmentTokenRequest = qurl.AgentEnrollmentCredentialRequest

// EnrollmentTokenProvider lazily mints a one-shot enrollment credential from
// inside qurl-go's serialized native lifecycle setup.
type EnrollmentTokenProvider = qurl.AgentEnrollmentCredentialProvider

// Runtime is the opened Connector runtime: the SDK's explicit-management
// client handle, its native runtime binding, and the owning state store.
// Resource setup and admission use Binding directly and never Client. The
// caller owns Close; hand the Binding to the knocker (which then owns it) and
// set Binding to nil before Close to transfer ownership.
type Runtime struct {
	Client  *qurl.Client
	Binding *qurl.AgentRuntimeBinding
	Store   *state.Store
	Hub     qurl.HubBootstrap
	AgentID string
}

// ValidateContinuity proves the runtime's state store still resolves to its
// retained directory capability.
func (r *Runtime) ValidateContinuity() error {
	if r == nil || r.Store == nil {
		return fmt.Errorf("%w: Connector runtime has no state store", qurl.ErrAgentStateContinuity)
	}
	return r.Store.ValidateContinuity()
}

// Close destroys the runtime binding (when still owned) and closes the state
// store. Idempotent and nil-safe.
func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}
	if r.Binding != nil {
		r.Binding.Destroy()
		r.Binding = nil
	}
	var err error
	if r.Store != nil {
		err = r.Store.Close()
		r.Store = nil
	}
	r.Client = nil
	return err
}

// Test-only injection seams for qurl-go's native UDP lifecycle entry points.
// Production must keep these bound to the package functions.
//
// Warm open uses ConnectAgentRuntime's offline option before the enrolling
// call. That preserves this package's operator-gated refresh ladder (manual by
// default, one refresh per failure episode) while allowing the SDK to invoke a
// lazy enrollment provider inside its setup lock on first registration.
var (
	registerAgentRuntime = func(ctx context.Context, credential string, store qurl.AgentStateStore, opts ...qurl.AgentRuntimeRegistrationOption) (*qurl.Client, *qurl.AgentRuntimeBinding, error) {
		if credential != "" {
			opts = append(opts, qurl.WithAgentRuntimeEnrollmentCredential(credential))
		}
		return qurl.ConnectAgentRuntime(ctx, store, opts...)
	}
	openRegisteredAgentRuntime = qurl.ConnectAgentRuntime
	refreshAgentRuntime        = qurl.RefreshAgentRuntime
)

// allowedRegistrationKeyKinds is the closed headless enrollment policy:
// pre-issued key kinds only, never account/OTP credentials.
var allowedRegistrationKeyKinds = []qurl.RegistrationKeyKind{
	qurl.RegistrationKeyKindConnectorBootstrap,
	qurl.RegistrationKeyKindBootstrap,
	qurl.RegistrationKeyKindAgent,
}

// The customer-surfaced failure identities of the Connector lifecycle ladder.
// Exported deliberately: every exported Err* under apps/cli enters the CLI's
// exit-code contract through internal/exitcode's AST tripwire, which forces a
// decided exit-code row (and a rendering decision) for each. The wrapped
// message carries the operator detail; internal/output owns the
// customer-language rendering.
var (
	// ErrEnrollmentTokenRequired refuses a first registration when this
	// machine has no persisted Connector identity and no enrollment token is
	// configured. Raised BEFORE any network I/O: an empty credential can
	// never register, so spending a network attempt on it would only move
	// this refusal several layers away from its remedy.
	ErrEnrollmentTokenRequired = errors.New("enrollment token required for the first registration")

	// ErrIdentityConflict refuses a runtime whose persisted native identity
	// disagrees with the operator-configured one. Proceeding under either
	// identity would silently serve as the other.
	ErrIdentityConflict = errors.New("configured agent identity conflicts with the persisted native agent identity")

	// ErrRefreshApprovalRequired is the manual-mode gate: a required
	// assignment refresh waits for an explicitly approved start instead of
	// consuming the episode's one refresh on an unattended restart.
	ErrRefreshApprovalRequired = errors.New("native assignment refresh requires explicit operator approval")

	// ErrRefreshDisabled is the disabled-mode gate: the operator's standing
	// configuration forbids the assignment refresh this state requires.
	ErrRefreshDisabled = errors.New("native assignment refresh is disabled while a refresh is required")

	// ErrRefreshAlreadyAttempted reports that this failure episode's one
	// assignment refresh was already consumed and the Connector still cannot
	// open its persisted assignment.
	ErrRefreshAlreadyAttempted = errors.New("native assignment refresh already attempted")
)

// Open opens the Connector runtime: it resolves the state directory and Hub
// bootstrap, then follows the lifecycle ladder —
//
//  1. an armed refresh marker is first correlated with a persisted identity;
//     an orphan marker is cleared because there is no assignment to refresh;
//  2. a real armed, unattempted marker forces the operator-gated assignment
//     refresh path;
//  3. otherwise the persisted identity is warm-opened offline, so an expired
//     assignment lease surfaces as a refresh request here instead of the SDK
//     silently renewing behind the operator's refresh-mode gate;
//  4. with no persisted state, first-time registration spends the one-shot
//     enrollment token.
//
// On success the caller owns the returned Runtime and must Close it.
func Open(ctx context.Context, cfg *Config) (_ *Runtime, retErr error) {
	logger := cfg.logger()
	dir, err := state.ResolveDir(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	store, err := state.Open(dir)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, store.Close())
		}
	}()

	hubBootstrap, err := hub.Bootstrap()
	if err != nil {
		return nil, err
	}
	origin, err := ResourceSDKOrigin(cfg.APIBaseURL)
	if err != nil {
		return nil, err
	}
	clientOpts := []qurl.AgentResourceClientOption{qurl.WithAgentClientBaseURL(origin)}
	agentID := cfg.agentID()
	mode, err := ResolveRefreshMode(cfg.RefreshMode)
	if err != nil {
		return nil, err
	}

	marker, markerPresent, err := loadRefreshMarkerFailSafe(ctx, logger, store)
	if err != nil {
		return nil, fmt.Errorf("load assignment refresh marker: %w", err)
	}
	marker, markerPresent, err = clearOrphanedRefreshMarker(ctx, logger, store, marker, markerPresent)
	if err != nil {
		return nil, err
	}
	if markerPresent && !marker.Attempted {
		return refreshRuntime(ctx, logger, store, hubBootstrap, dir, agentID, marker, mode, clientOpts)
	}

	stateStore, err := store.Handoff()
	if err != nil {
		return nil, fmt.Errorf("validate state before native warm open: %w", err)
	}
	// ConnectAgentRuntime with the closed offline-open option is the warm-open
	// probe. It cannot enroll or renew, so the CLI's operator-gated refresh
	// ladder below remains authoritative.
	openOpts := make([]qurl.AgentRuntimeRegistrationOption, 0, len(clientOpts)+1)
	for _, option := range clientOpts {
		openOpts = append(openOpts, option)
	}
	// Open offline so an expired assignment lease surfaces as
	// ErrAssignmentLeaseExpired (handled just below) instead of triggering
	// qurl-go's built-in auto-renewal. That built-in renewal resolves its Hub
	// trust root only from the shipped deployment — it cannot see the
	// QURL_CONNECTOR_HUB_* triple — so without this a warm open of an expired
	// lease fails "no Hub trust root is configured" even with the triple set,
	// never reaching this package's own gated refresh (which does thread that
	// Hub into RefreshAgentRuntime). Opening offline also keeps the
	// operator's refresh-mode gate (auto/manual/disabled) authoritative
	// rather than letting the SDK renew behind it.
	openOpts = append(openOpts, qurl.WithAgentRuntimeOfflineOpen())
	client, binding, openErr := openRegisteredAgentRuntime(ctx, stateStore, openOpts...)
	if openErr == nil {
		return assembleRuntime(client, binding, store, hubBootstrap, agentID)
	}
	if markerPresent && marker.Attempted {
		return nil, refreshAlreadyAttemptedError(openErr)
	}
	if errors.Is(openErr, qurl.ErrAssignmentLeaseExpired) {
		if err := store.RequestRefresh("assigned NHP cell lease expired"); err != nil {
			return nil, fmt.Errorf("record assignment refresh request: %w", err)
		}
		marker, markerPresent, err = loadRefreshMarkerFailSafe(ctx, logger, store)
		if err != nil {
			return nil, fmt.Errorf("reload assignment refresh marker: %w", err)
		}
		if !markerPresent {
			return nil, errors.New("assignment refresh marker missing after recording lease-expiry request")
		}
		return refreshRuntime(ctx, logger, store, hubBootstrap, dir, agentID, marker, mode, clientOpts)
	}

	return registerRuntime(ctx, logger, cfg, store, hubBootstrap, agentID, clientOpts, openErr)
}

// clearOrphanedRefreshMarker correlates the non-secret episode breadcrumb
// with the credential state it is supposed to refresh. True agent-state
// absence means there is no platform assignment to refresh, so retaining the
// marker would wedge an otherwise valid first enrollment behind an impossible
// operator approval. Any present entry remains fail-closed under qurl-go's
// normal validation.
func clearOrphanedRefreshMarker(
	ctx context.Context,
	logger *slog.Logger,
	store *state.Store,
	marker state.RefreshMarker,
	present bool,
) (state.RefreshMarker, bool, error) {
	if !present {
		return marker, false, nil
	}
	statePresent, err := store.AgentStatePresent()
	if err != nil {
		return state.RefreshMarker{}, false, fmt.Errorf("correlate assignment refresh marker with Connector identity: %w", err)
	}
	if statePresent {
		return marker, true, nil
	}
	logger.WarnContext(ctx, "connector: orphaned assignment-refresh marker; clearing it before first registration",
		"event", "assignment_refresh_marker_orphaned",
		"reason", marker.Reason,
		"attempted", marker.Attempted)
	if err := store.ClearRefreshMarker(); err != nil {
		return state.RefreshMarker{}, false, fmt.Errorf("clear orphaned assignment refresh marker: %w", err)
	}
	return state.RefreshMarker{}, false, nil
}

// registerRuntime performs the first-time native registration, spending the
// one-shot enrollment credential.
func registerRuntime(
	ctx context.Context,
	logger *slog.Logger,
	cfg *Config,
	store *state.Store,
	hubBootstrap qurl.HubBootstrap,
	agentID string,
	clientOpts []qurl.AgentResourceClientOption,
	openErr error,
) (*Runtime, error) {
	var credential string
	var err error
	if cfg.EnrollmentTokenProvider == nil {
		credential, err = resolveEnrollmentToken(cfg.EnrollmentToken)
		if err != nil {
			return nil, err
		}
	}
	if credential == "" && cfg.EnrollmentTokenProvider == nil && errors.Is(openErr, qurl.ErrAgentStateNotFound) {
		// No stored identity and no token: refuse BEFORE the SDK is invoked.
		// This is the zero-network token-required path — an empty credential
		// can never register, and qurl-go would spend a bounded native
		// transaction discovering that far from the remedy.
		return nil, fmt.Errorf("%w: set %s, or point %s at a file holding one (this machine has no stored Connector identity yet)", ErrEnrollmentTokenRequired, EnvEnrollmentToken, EnvEnrollmentTokenFile)
	}
	registerOpts := []qurl.AgentRuntimeRegistrationOption{
		qurl.WithAgentRuntimeHub(hubBootstrap),
		qurl.WithAgentRuntimeMetadata(Hostname(), ClientVersionMeta(cfg.Version)),
		qurl.WithAgentRuntimeAllowedRegistrationKeyKinds(allowedRegistrationKeyKinds...),
	}
	for _, option := range clientOpts {
		registerOpts = append(registerOpts, option)
	}
	if agentID != "" {
		registerOpts = append(registerOpts, qurl.WithAgentRuntimeIdentity(agentID))
	}
	if cfg.EnrollmentTokenProvider != nil {
		registerOpts = append(registerOpts, qurl.WithAgentRuntimeEnrollmentCredentialProvider(cfg.EnrollmentTokenProvider))
	}
	stateStore, err := store.Handoff()
	if err != nil {
		return nil, fmt.Errorf("validate state before native registration: %w", err)
	}
	client, binding, err := registerAgentRuntime(ctx, credential, stateStore, registerOpts...)
	if err != nil {
		logRegistrationFailure(ctx, logger, err, credential)
		// qurl-go bounds each native leg itself, so a stalled first
		// registration surfaces here as a no-reply/recovery error after its
		// bounded budget — it does not hang indefinitely. When the parent
		// context is still live and a credential was presented, enrich that
		// transport diagnostic with the one thing qurl-go cannot name —
		// which Connector this client was registering — plus the
		// Connector-specific things to check.
		//
		// A parent cancellation (SIGINT/SIGTERM) stays the bare context
		// error: ctx.Err() != nil here means the operator interrupted and
		// must not be reinterpreted as a stall. With the parent still live,
		// a bare context.Canceled is instead qurl-go's own per-leg
		// budget/abort surfacing — the same silent stall as a deadline or
		// no-reply — so it is enriched too. An authenticated denial keeps
		// its own specific message.
		if ctx.Err() == nil && (credential != "" || cfg.EnrollmentTokenProvider != nil) &&
			(isSilentRegistrationStall(err) || errors.Is(err, context.Canceled)) {
			return nil, &registrationStalledError{cause: err}
		}
		return nil, err
	}
	runtime, err := assembleRuntime(client, binding, store, hubBootstrap, agentID)
	if err != nil {
		return nil, err
	}
	logger.InfoContext(ctx, "connector: native registration succeeded",
		"event", "native_registration_succeeded",
		"agent_id", runtime.AgentID,
		"assigned_endpoint", runtime.Binding.NHPUDPEndpoint.Host)
	return runtime, nil
}

// refreshRuntime performs the operator-gated one-per-episode assignment
// refresh.
func refreshRuntime(
	ctx context.Context,
	logger *slog.Logger,
	store *state.Store,
	hubBootstrap qurl.HubBootstrap,
	stateDir, agentID string,
	marker state.RefreshMarker,
	mode string,
	clientOpts []qurl.AgentResourceClientOption,
) (*Runtime, error) {
	switch mode {
	case RefreshModeDisabled:
		return nil, fmt.Errorf("%w (%s); deliberately clear and reprovision %s or set %s=manual|auto", ErrRefreshDisabled, marker.Reason, stateDir, EnvRefreshMode)
	case RefreshModeManual:
		return nil, fmt.Errorf("%w (%s): inspect the failure, then approve exactly one start with refresh mode auto; an orchestrator restart is not approval", ErrRefreshApprovalRequired, marker.Reason)
	case RefreshModeAuto:
	default:
		return nil, fmt.Errorf("unsupported registration refresh mode %q", mode)
	}
	if marker.Attempted {
		return nil, refreshAlreadyAttemptedError(nil)
	}
	if err := store.MarkRefreshAttempted(); err != nil {
		return nil, fmt.Errorf("mark assignment refresh attempted: %w", err)
	}
	marked, present, err := store.LoadRefreshMarker()
	if err != nil {
		return nil, fmt.Errorf("verify assignment refresh attempt marker: %w", err)
	}
	if !present || !marked.Attempted {
		return nil, errors.New("verify assignment refresh attempt marker: marker is absent or not attempted")
	}
	// Reassignment adoption is the SDK default: a refresh follows an
	// authority-directed move instead of failing closed on it, which is the
	// posture this self-heal path wants (the standalone Connector opted into
	// the same behavior when it was still an explicit option).
	refreshOpts := make([]qurl.AgentRuntimeRefreshOption, 0, len(clientOpts))
	for _, option := range clientOpts {
		refreshOpts = append(refreshOpts, option)
	}
	stateStore, err := store.Handoff()
	if err != nil {
		return nil, fmt.Errorf("validate state before native assignment refresh: %w", err)
	}
	client, binding, err := refreshAgentRuntime(ctx, hubBootstrap, stateStore, refreshOpts...)
	if err != nil {
		return nil, fmt.Errorf("refresh native assignment binding: %w", err)
	}
	runtime, err := assembleRuntime(client, binding, store, hubBootstrap, agentID)
	if err != nil {
		return nil, err
	}
	logger.InfoContext(ctx, "connector: native assignment binding refreshed",
		"event", "assignment_refresh_succeeded",
		"mode", mode,
		"assigned_endpoint", runtime.Binding.NHPUDPEndpoint.Host)
	return runtime, nil
}

func refreshAlreadyAttemptedError(cause error) error {
	result := fmt.Errorf("%w in this failure episode; investigate Hub reachability or deliberately reprovision", ErrRefreshAlreadyAttempted)
	if cause != nil {
		result = errors.Join(result, fmt.Errorf("warm-open after attempted refresh: %w", cause))
	}
	return result
}

// assembleRuntime finalizes ownership of a runtime the SDK returned: nil
// checks, the configured-identity conflict gate, and state continuity.
func assembleRuntime(client *qurl.Client, binding *qurl.AgentRuntimeBinding, store *state.Store, hubBootstrap qurl.HubBootstrap, configuredAgentID string) (*Runtime, error) {
	if client == nil || binding == nil {
		if binding != nil {
			binding.Destroy()
		}
		return nil, errors.New("qurl-go returned an incomplete native agent runtime")
	}
	if configuredAgentID = strings.TrimSpace(configuredAgentID); configuredAgentID != "" && binding.AgentID != configuredAgentID {
		binding.Destroy()
		return nil, fmt.Errorf("%w: configured %q, persisted %q; use the persisted identity or deliberately clear and reprovision this state", ErrIdentityConflict, configuredAgentID, binding.AgentID)
	}
	if store == nil {
		binding.Destroy()
		return nil, fmt.Errorf("%w: qurl-go runtime has no owning state store", qurl.ErrAgentStateContinuity)
	}
	if err := store.ValidateContinuity(); err != nil {
		binding.Destroy()
		return nil, fmt.Errorf("validate state after qurl-go lifecycle: %w", err)
	}
	return &Runtime{
		Client: client, Binding: binding, Store: store, Hub: hubBootstrap,
		AgentID: binding.AgentID,
	}, nil
}

// loadRefreshMarkerFailSafe loads the refresh marker, downgrading a corrupt
// marker to a logged warning plus removal so a torn self-heal breadcrumb
// cannot wedge startup. Real I/O faults still fail.
func loadRefreshMarkerFailSafe(ctx context.Context, logger *slog.Logger, store *state.Store) (state.RefreshMarker, bool, error) {
	if store == nil {
		return state.RefreshMarker{}, false, fmt.Errorf("%w: Connector state store is not open", qurl.ErrAgentStateContinuity)
	}
	marker, present, loadErr := store.LoadRefreshMarker()
	if loadErr == nil {
		return marker, present, nil
	}
	if !state.IsInvalidRefreshMarker(loadErr) {
		return state.RefreshMarker{}, false, loadErr
	}
	logger.WarnContext(ctx, "connector: invalid assignment-refresh marker; clearing it before ordinary native warm-open",
		"event", "assignment_refresh_marker_invalid",
		"err", loadErr.Error())
	if clearErr := store.ClearRefreshMarker(); clearErr != nil {
		return state.RefreshMarker{}, false, errors.Join(
			fmt.Errorf("read invalid assignment-refresh marker: %w", loadErr),
			fmt.Errorf("clear invalid assignment-refresh marker: %w", clearErr),
		)
	}
	return state.RefreshMarker{}, false, nil
}

func (c *Config) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

func (c *Config) agentID() string {
	if id := strings.TrimSpace(c.AgentID); id != "" {
		return id
	}
	return state.ConfiguredAgentID()
}
