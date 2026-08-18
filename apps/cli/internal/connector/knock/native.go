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
	target     string
	udpOpts    []qurl.AgentRuntimeUDPOption
	knock      nativeKnockFunc
	exit       nativeExitFunc
	logger     *slog.Logger
}

var _ CycleKnocker = (*Native)(nil)

type nativeKnockFunc func(context.Context, *qurl.AgentRuntimeBinding, []byte, string, qurl.NativeKnockOptions, ...qurl.AgentRuntimeUDPOption) (*qurl.NativeKnockResult, error)
type nativeExitFunc func(context.Context, *qurl.AgentRuntimeBinding, []byte, string, qurl.NativeKnockOptions, ...qurl.AgentRuntimeUDPOption) error

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
		exit:       qurl.ExitRegisteredAgentSession,
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

	start := time.Now()
	result, err := k.knock(
		ctx,
		k.binding,
		k.privateKey,
		k.resourceID,
		qurl.NativeKnockOptions{RunID: k.runID},
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

// EndCycle consumes the current cycle RunID and sends the best-effort native
// session exit for it. Replays after the RunID is consumed are no-ops; the
// RunID is consumed even when the runtime turns out to be closed, so an
// invalid-state return can never resurrect a spent cycle.
func (k *Native) EndCycle(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.runID == "" {
		return nil
	}
	runID := k.runID
	k.runID = ""
	if !k.aliveLocked() {
		return errors.New("native NHP runtime is closed")
	}
	return k.exit(
		ctx,
		k.binding,
		k.privateKey,
		k.resourceID,
		qurl.NativeKnockOptions{RunID: runID},
		k.udpOpts...,
	)
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
	k.mu.Unlock()
	if binding != nil {
		binding.Destroy()
	}
}
