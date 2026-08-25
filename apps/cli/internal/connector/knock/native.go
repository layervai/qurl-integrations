package knock

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	qurl "github.com/layervai/qurl-go/qurl"
)

// nativeKnockResourceNotFoundCode names the authenticated platform denial for
// a knock against a resource the admission controller does not know.
// qurl-go intentionally preserves an authenticated denial's wire code in
// ServerDenyError, and the qurl-conformance vectors freeze 52004 as the
// native knock resource-not-found denial; keep the log-only interpretation
// named and exact so a future, different authenticated denial stays generic.
const nativeKnockResourceNotFoundCode = "52004"

// Native owns the one process-lifetime native runtime key and one immutable
// RunID per supervisor cycle. qurl-go owns packet construction, assigned-cell
// resolution, server authentication, ACK parsing, and the knock/exit
// transactions. The supervisor sequences BeginCycle, Knock/EndCycle, and
// Close. The mutex additionally keeps Close from wiping the key or destroying
// the binding while an in-flight UDP exchange is using them, so safety does
// not rely only on call order. Consequently Close may wait for that
// exchange's bounded context deadline; callers must include the native UDP
// timeout in their shutdown budget.
type Native struct {
	mu sync.Mutex

	binding    *qurl.AgentRuntimeBinding
	closed     bool
	privateKey []byte
	resourceID string
	runID      string
	runAttempt uint64
	receipts   []qurl.NativeSessionReceipt
	target     string
	udpOpts    []qurl.AgentRuntimeUDPOption
	knock      nativeKnockFunc
	retire     nativeRetireFunc
	logger     *slog.Logger
}

var _ CycleKnocker = (*Native)(nil)

type nativeKnockFunc func(context.Context, *qurl.AgentRuntimeBinding, []byte, string, qurl.NativeKnockOptions, ...qurl.AgentRuntimeUDPOption) (*qurl.NativeKnockResult, error)
type nativeRetireFunc func(context.Context, *qurl.AgentRuntimeBinding, []byte, qurl.NativeSessionReceipt, ...qurl.AgentRuntimeUDPOption) (*qurl.NativeSessionRetirement, error)

// NewNative takes ownership of the binding's device runtime key and returns
// the process-lifetime cycle knocker for resourceID. The caller must Close
// the returned knocker; Close also destroys the binding.
func NewNative(binding *qurl.AgentRuntimeBinding, resourceID string, udpOpts ...qurl.AgentRuntimeUDPOption) (*Native, error) {
	if binding == nil {
		return nil, errors.New("native NHP runtime binding is nil")
	}
	if strings.TrimSpace(resourceID) == "" {
		return nil, errors.New("native NHP knock resource is empty")
	}
	privateKey := binding.TakeDeviceStaticPrivateKey()
	if len(privateKey) != 32 {
		clear(privateKey)
		return nil, fmt.Errorf("native NHP runtime key is %d bytes, want 32", len(privateKey))
	}
	return &Native{
		binding:    binding,
		privateKey: privateKey,
		resourceID: resourceID,
		target:     binding.NHPUDPEndpoint.Host + ":" + strconv.Itoa(binding.NHPUDPEndpoint.Port),
		udpOpts:    append([]qurl.AgentRuntimeUDPOption(nil), udpOpts...),
		knock:      qurl.KnockRegisteredAgent,
		retire:     qurl.RetireRegisteredAgentSession,
	}, nil
}

func (k *Native) log() *slog.Logger {
	if k.logger != nil {
		return k.logger
	}
	return slog.Default()
}

// aliveLocked reports whether the runtime still holds a usable binding and
// key. Callers must hold k.mu.
func (k *Native) aliveLocked() bool {
	return !k.closed && k.binding != nil && len(k.privateKey) == 32
}

// BeginCycle issues a fresh caller-owned cycle RunID. The supervisor calls it
// exactly once per outer cycle, before the cycle's first knock.
func (k *Native) BeginCycle() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.aliveLocked() {
		return errors.New("native NHP runtime is closed")
	}
	runID, err := qurl.NewCycleRunID()
	if err != nil {
		return fmt.Errorf("generate native cycle RunID: %w", err)
	}
	k.runID = runID
	k.runAttempt = 1
	k.receipts = nil
	return nil
}

// CycleRunID returns the current cycle's RunID, or "" outside a cycle.
func (k *Native) CycleRunID() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.runID
}

// Knock performs one native NHP knock under the current cycle RunID and
// translates the SDK admission into the resource-keyed Result the supervisor
// consumes. A nil error does not make an incomplete admission usable: an
// empty ACToken or ResourceHost is returned as-is so the consumer fails
// closed on the Result contract.
func (k *Native) Knock(ctx context.Context) (*Result, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.aliveLocked() {
		return nil, errors.New("native NHP runtime is closed")
	}
	if k.runID == "" {
		return nil, errors.New("native NHP cycle has no RunID")
	}
	if k.runAttempt != 1 {
		return nil, errors.New("native NHP cycle has no canonical RunAttempt")
	}

	start := time.Now()
	result, err := k.knock(
		ctx,
		k.binding,
		k.privateKey,
		k.resourceID,
		qurl.NativeKnockOptions{RunID: k.runID, RunAttempt: k.runAttempt},
		k.udpOpts...,
	)
	latency := latencyMS(time.Since(start))
	if err != nil {
		outcome := classifyKnockFailure(err)
		k.log().WarnContext(ctx, "connector: native NHP knock failed",
			"event", outcome.event,
			"reason", outcome.reason,
			"resource_id", k.resourceID,
			"target", k.target,
			"run_id", k.runID,
			"latency_ms", latency,
			"err", err.Error())
		return nil, err
	}
	if result == nil {
		err := errors.New("native NHP knock returned no admission")
		k.log().WarnContext(ctx, "connector: native NHP knock failed",
			"event", eventKnockError,
			"reason", "knock_invalid_response",
			"resource_id", k.resourceID,
			"target", k.target,
			"run_id", k.runID,
			"latency_ms", latency,
			"err", err.Error())
		return nil, err
	}
	// The receipt is the only authority that can retire this exact server
	// session. Retain every successful admission in case an in-cycle redial
	// produced another session. Exact retirement is replay-safe, so retaining a
	// duplicate receipt is safer than dropping a distinct receipt after a
	// response ambiguity. Suppress only a byte-identical public session
	// identity; exact replay can return that same immutable receipt.
	if !containsNativeSessionReceipt(k.receipts, &result.SessionReceipt) {
		k.receipts = append(k.receipts, result.SessionReceipt)
	}
	event, reason := eventKnockSuccess, ""
	if result.ACToken == "" {
		event, reason = eventKnockDeny, "knock_token_missing"
	} else if result.ResourceHost == "" {
		event, reason = eventKnockDeny, "resource_host_missing"
	}
	attrs := []any{
		"event", event,
		"resource_id", k.resourceID,
		"target", k.target,
		"run_id", k.runID,
		"latency_ms", latency,
	}
	if reason != "" {
		attrs = append(attrs, "reason", reason)
		k.log().WarnContext(ctx, "connector: native NHP knock admission incomplete", attrs...)
	} else {
		k.log().DebugContext(ctx, "connector: native NHP knock admitted", attrs...)
	}
	return &Result{
		ACTokens:     map[string]string{k.resourceID: result.ACToken},
		ResourceHost: map[string]string{k.resourceID: result.ResourceHost},
	}, nil
}

func containsNativeSessionReceipt(receipts []qurl.NativeSessionReceipt, candidate *qurl.NativeSessionReceipt) bool {
	for _, receipt := range receipts {
		if receipt.CellID == candidate.CellID && receipt.SessionID == candidate.SessionID &&
			receipt.SessionIssuedAtMillis == candidate.SessionIssuedAtMillis && receipt.RunID == candidate.RunID &&
			receipt.RunAttempt == candidate.RunAttempt {
			return true
		}
	}
	return false
}

// EndCycle consumes the current cycle and retires every exact authenticated
// session receipt obtained during it. Replays after the cycle is consumed are
// no-ops. A cycle with no receipt fails closed: a lost admission reply cannot
// be cleaned up truthfully from RunID alone.
func (k *Native) EndCycle(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.runID == "" {
		return nil
	}
	k.runID = ""
	k.runAttempt = 0
	receipts := k.receipts
	k.receipts = nil
	if !k.aliveLocked() {
		return errors.New("native NHP runtime is closed")
	}
	if len(receipts) == 0 {
		return errors.New("native NHP cycle has no authenticated session receipt")
	}
	var retirementErrors []error
	for _, receipt := range receipts {
		retirement, err := k.retire(ctx, k.binding, k.privateKey, receipt, k.udpOpts...)
		if err != nil {
			retirementErrors = append(retirementErrors, err)
			continue
		}
		if retirement == nil || (retirement.State != "closing" && retirement.State != "closed") {
			retirementErrors = append(retirementErrors, errors.New("native NHP session retirement returned no accepted state"))
		}
	}
	return errors.Join(retirementErrors...)
}

// Close zeroes the runtime key, clears the cycle, and destroys the owned
// binding. It is idempotent and nil-safe, and waits for an in-flight UDP
// exchange before wiping the state that exchange is using.
func (k *Native) Close() {
	if k == nil {
		return
	}
	k.mu.Lock()
	if k.closed {
		k.mu.Unlock()
		return
	}
	k.closed = true
	binding := k.binding
	k.binding = nil
	clear(k.privateKey)
	k.privateKey = nil
	k.runID = ""
	k.runAttempt = 0
	k.receipts = nil
	k.mu.Unlock()
	if binding != nil {
		binding.Destroy()
	}
}
