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

	// EnrollmentToken is the one-shot enrollment credential, flag-first.
	// When empty, the QURL_CONNECTOR_TOKEN_FILE / QURL_CONNECTOR_TOKEN
	// fallbacks apply. It is only consulted (and only spent) on a first
	// registration; warm opens and refreshes use the persisted device
	// identity.
	EnrollmentToken string

	// StateDir overrides state-directory resolution (flag-first). Empty
	// falls through to the state package's env/XDG chain.
	StateDir string

	// AgentID optionally pins the persisted native agent identity,
	// flag-first. Empty falls back to the LAYERV_AGENT_ID override, then to
	// the identity qurl-go generates and persists.
	AgentID string

	// Version is the CLI build version stamped into registration metadata.
	Version string

	// Logger receives orchestration events; nil uses slog.Default().
	Logger *slog.Logger
}

// Runtime is the opened Connector runtime: the registered device's resource
// client, its native runtime binding, and the owning state store. The caller
// owns Close; hand the Binding to the knocker (which then owns it) and set
// Binding to nil before Close to transfer ownership.
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
// The register/open compatibility pair is used deliberately instead of the
// newer combined qurl.ConnectAgentRuntime: ConnectAgentRuntime renews an
// expired lease inside the SDK on every start and has no offline-open mode,
// which would bypass this package's operator-gated refresh ladder (manual by
// default, one refresh per failure episode). The pair is documented as
// remaining for compatibility and behaves identically to the combined call's
// two halves.
var (
	registerAgentRuntime       = qurl.RegisterAgentRuntime       //nolint:staticcheck // SA1019: the split call keeps enrollment spending explicit; see the seam comment above.
	openRegisteredAgentRuntime = qurl.OpenRegisteredAgentRuntime //nolint:staticcheck // SA1019: the offline warm open only exists on this call; see the seam comment above.
	refreshAgentRuntime        = qurl.RefreshAgentRuntime
)

// allowedRegistrationKeyKinds is the closed headless enrollment policy:
// pre-issued key kinds only, never account/OTP credentials.
var allowedRegistrationKeyKinds = []qurl.RegistrationKeyKind{
	qurl.RegistrationKeyKindConnectorBootstrap,
	qurl.RegistrationKeyKindBootstrap,
	qurl.RegistrationKeyKindAgent,
}

var errRefreshAlreadyAttempted = errors.New("native assignment refresh already attempted")

// Open opens the Connector runtime: it resolves the state directory and Hub
// bootstrap, then follows the lifecycle ladder —
//
//  1. an armed, unattempted refresh marker forces the operator-gated
//     assignment refresh path;
//  2. otherwise the persisted identity is warm-opened offline, so an expired
//     assignment lease surfaces as a refresh request here instead of the SDK
//     silently renewing behind the operator's refresh-mode gate;
//  3. with no persisted state, first-time registration spends the one-shot
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
	mode, err := RefreshMode()
	if err != nil {
		return nil, err
	}

	marker, markerPresent, err := loadRefreshMarkerFailSafe(ctx, logger, store)
	if err != nil {
		return nil, fmt.Errorf("load assignment refresh marker: %w", err)
	}
	if markerPresent && !marker.Attempted {
		return refreshRuntime(ctx, logger, store, hubBootstrap, dir, agentID, marker, mode, clientOpts)
	}

	stateStore, err := store.Handoff()
	if err != nil {
		return nil, fmt.Errorf("validate state before native warm open: %w", err)
	}
	// OpenRegisteredAgentRuntime takes a closed option set, so options that
	// mean nothing to a warm open are rejected by the compiler.
	// AgentResourceClientOption embeds it, so converting is all that is
	// needed here.
	openOpts := make([]qurl.AgentRuntimeOpenOption, 0, len(clientOpts)+1)
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
	credential, err := resolveEnrollmentToken(cfg.EnrollmentToken)
	if err != nil {
		return nil, err
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
	stateStore, err := store.Handoff()
	if err != nil {
		return nil, fmt.Errorf("validate state before native registration: %w", err)
	}
	client, binding, err := registerAgentRuntime(ctx, credential, stateStore, registerOpts...)
	if err != nil {
		logRegistrationFailure(ctx, logger, err, credential)
		if credential == "" && errors.Is(openErr, qurl.ErrAgentStateNotFound) {
			return nil, fmt.Errorf("enrollment token required for the first registration (pass --token, or set %s or %s): %w", EnvEnrollmentTokenFile, EnvEnrollmentToken, err)
		}
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
		if ctx.Err() == nil && credential != "" &&
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
		return nil, fmt.Errorf("native assignment refresh is disabled while a refresh is required (%s); deliberately clear and reprovision %s or set %s=manual|auto", marker.Reason, stateDir, EnvRefreshMode)
	case RefreshModeManual:
		return nil, fmt.Errorf("native assignment refresh requires explicit operator approval (%s): inspect the failure, then run exactly one start with %s=auto and restore manual after a healthy connection; an orchestrator restart is not approval", marker.Reason, EnvRefreshMode)
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
	result := fmt.Errorf("%w in this failure episode; investigate Hub reachability or deliberately reprovision", errRefreshAlreadyAttempted)
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
		return nil, fmt.Errorf("configured agent identity %q conflicts with persisted native agent identity %q; use the persisted identity or deliberately clear and reprovision this state", configuredAgentID, binding.AgentID)
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
