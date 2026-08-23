package knock

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	qurl "github.com/layervai/qurl-go/qurl"
)

func testNativeKnockResult(opts qurl.NativeKnockOptions, token, host string) *qurl.NativeKnockResult {
	receipt := testNativeReceipt(opts.RunID, opts.RunAttempt)
	return &qurl.NativeKnockResult{
		ACToken: token, ResourceHost: host, SessionID: receipt.SessionID,
		SessionReceipt: receipt,
	}
}

func testNativeReceipt(runID string, runAttempt uint64) qurl.NativeSessionReceipt {
	return qurl.NativeSessionReceipt{
		CellID: "cell0", SessionID: 1000 + runAttempt, SessionIssuedAtMillis: 1_800_000_000_000,
		RunID: runID, RunAttempt: runAttempt,
	}
}

func testNativeReceiptPtr(runID string, runAttempt uint64) *qurl.NativeSessionReceipt {
	receipt := testNativeReceipt(runID, runAttempt)
	return &receipt
}

func sameTestNativeReceipt(got, want *qurl.NativeSessionReceipt) bool {
	if got == nil || want == nil {
		return got == want
	}
	return got.CellID == want.CellID && got.SessionID == want.SessionID &&
		got.SessionIssuedAtMillis == want.SessionIssuedAtMillis && got.RunID == want.RunID &&
		got.RunAttempt == want.RunAttempt
}

// logRecord captures one slog emit for classification assertions.
type logRecord struct {
	msg   string
	attrs map[string]string
}

// captureHandler is a minimal concurrency-safe slog.Handler that records
// every emitted entry's message and stringified attributes.
type captureHandler struct {
	mu      sync.Mutex
	records []logRecord
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error { //nolint:gocritic // hugeParam: slog.Handler pins this signature.
	rec := logRecord{msg: r.Message, attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, rec)
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) snapshot() []logRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]logRecord(nil), h.records...)
}

func captureLogger() (*slog.Logger, *captureHandler) {
	h := &captureHandler{}
	return slog.New(h), h
}

func TestNewNativeFailsClosedOnInvalidRuntimeInputs(t *testing.T) {
	t.Parallel()
	t.Run("nil binding", func(t *testing.T) {
		t.Parallel()
		knocker, err := NewNative(nil, "resource-public-key")
		if knocker != nil || err == nil || !strings.Contains(err.Error(), "native NHP runtime binding is nil") {
			t.Fatalf("NewNative = (%v, %v), want nil-binding rejection", knocker, err)
		}
	})
	for name, resourceID := range map[string]string{"empty resource": "", "whitespace resource": " \t "} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			knocker, err := NewNative(&qurl.AgentRuntimeBinding{}, resourceID)
			if knocker != nil || err == nil || !strings.Contains(err.Error(), "native NHP knock resource is empty") {
				t.Fatalf("NewNative = (%v, %v), want empty-resource rejection", knocker, err)
			}
		})
	}
	// A binding that never held a runtime key yields a zero-length key from
	// TakeDeviceStaticPrivateKey. Longer-but-short keys are unreachable from
	// outside qurl-go (its lifecycle validates 32 bytes before binding
	// construction), so the constructor's key-length gate is only observable
	// through this shape.
	t.Run("missing runtime key", func(t *testing.T) {
		t.Parallel()
		knocker, err := NewNative(&qurl.AgentRuntimeBinding{
			AgentID:        "agent-a",
			NHPUDPEndpoint: qurl.NHPUDPEndpoint{Host: "hub.test.nhp.layerv.ai", Port: 443},
		}, "resource-public-key")
		if knocker != nil || err == nil || !strings.Contains(err.Error(), "native NHP runtime key is 0 bytes, want 32") {
			t.Fatalf("NewNative = (%v, %v), want short-key rejection", knocker, err)
		}
	})
}

func TestNativeTranslatesAuthenticatedAdmission(t *testing.T) {
	t.Parallel()
	binding := &qurl.AgentRuntimeBinding{}
	privateKey := bytes.Repeat([]byte{0x5a}, 32)
	transportOpt := qurl.WithAgentRuntimeUDPBounds(2*time.Second, 1)
	knocker := &Native{
		binding: binding, privateKey: privateKey, resourceID: "resource-public-key",
		runID: "01abcdef23456789", target: "hub.test.nhp.layerv.ai:443",
		udpOpts: []qurl.AgentRuntimeUDPOption{transportOpt},
	}
	defer knocker.Close()
	knocker.knock = func(ctx context.Context, gotBinding *qurl.AgentRuntimeBinding, gotKey []byte, resourceID string, opts qurl.NativeKnockOptions, transportOpts ...qurl.AgentRuntimeUDPOption) (*qurl.NativeKnockResult, error) {
		if ctx == nil || gotBinding != binding || !bytes.Equal(gotKey, privateKey) {
			t.Fatalf("adapter forwarded context/binding/key incorrectly")
		}
		if resourceID != knocker.resourceID || opts.RunID != knocker.runID || opts.RunAttempt != 1 || len(transportOpts) != 1 {
			t.Fatalf("adapter call = resource %q, RunID %q, attempt %d, transport opts %d", resourceID, opts.RunID, opts.RunAttempt, len(transportOpts))
		}
		return testNativeKnockResult(opts, "admission-token", "tunnel.test.layerv.ai:7000"), nil
	}

	got, err := knocker.Knock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.ACTokens[knocker.resourceID] != "admission-token" || got.ResourceHost[knocker.resourceID] != "tunnel.test.layerv.ai:7000" {
		t.Fatalf("Knock() = %#v, want resource-keyed token and host", got)
	}
}

func TestNativeBeginCycleRegeneratesCanonicalRunID(t *testing.T) {
	t.Parallel()
	knocker := &Native{
		binding:    &qurl.AgentRuntimeBinding{},
		privateKey: bytes.Repeat([]byte{0x24}, 32),
	}
	defer knocker.Close()
	if err := knocker.BeginCycle(); err != nil {
		t.Fatal(err)
	}
	first := knocker.CycleRunID()
	if err := qurl.ValidateCycleRunID(first); err != nil {
		t.Fatalf("first CycleRunID %q is not canonical: %v", first, err)
	}
	if err := knocker.BeginCycle(); err != nil {
		t.Fatal(err)
	}
	second := knocker.CycleRunID()
	if err := qurl.ValidateCycleRunID(second); err != nil {
		t.Fatalf("second CycleRunID %q is not canonical: %v", second, err)
	}
	if second == first {
		t.Fatalf("BeginCycle reused RunID %q", first)
	}
}

func TestNativeRejectsMissingRunIDAndNilAdmission(t *testing.T) {
	t.Parallel()
	logger, _ := captureLogger()
	knocker := &Native{
		binding: &qurl.AgentRuntimeBinding{}, privateKey: bytes.Repeat([]byte{0x42}, 32),
		resourceID: "resource-public-key", logger: logger,
	}
	defer knocker.Close()
	knocker.knock = func(context.Context, *qurl.AgentRuntimeBinding, []byte, string, qurl.NativeKnockOptions, ...qurl.AgentRuntimeUDPOption) (*qurl.NativeKnockResult, error) {
		return nil, nil //nolint:nilnil // the nil-admission-without-error SDK shape is exactly the scenario under test.
	}
	if _, err := knocker.Knock(nil); err == nil || !strings.Contains(err.Error(), "context is nil") { //nolint:staticcheck // SA1012: nil is the invalid boundary under test.
		t.Fatalf("Knock() nil-context error = %v", err)
	}
	if _, err := knocker.Knock(context.Background()); err == nil || !strings.Contains(err.Error(), "no RunID") {
		t.Fatalf("Knock() missing-RunID error = %v", err)
	}

	knocker.runID = "01abcdef23456789"
	if _, err := knocker.Knock(context.Background()); err == nil || !strings.Contains(err.Error(), "no admission") {
		t.Fatalf("Knock() nil-admission error = %v", err)
	}

	wantErr := errors.New("authenticated server denial")
	knocker.knock = func(context.Context, *qurl.AgentRuntimeBinding, []byte, string, qurl.NativeKnockOptions, ...qurl.AgentRuntimeUDPOption) (*qurl.NativeKnockResult, error) {
		return nil, wantErr
	}
	if _, err := knocker.Knock(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Knock() error = %v, want SDK error", err)
	}
}

func TestNativeRejectsIncompleteExactSessionReceipt(t *testing.T) {
	t.Parallel()
	tests := map[string]func(*qurl.NativeSessionReceipt){
		"empty cell":        func(receipt *qurl.NativeSessionReceipt) { receipt.CellID = "" },
		"whitespace cell":   func(receipt *qurl.NativeSessionReceipt) { receipt.CellID = " \t " },
		"zero session":      func(receipt *qurl.NativeSessionReceipt) { receipt.SessionID = 0 },
		"wrong session":     func(receipt *qurl.NativeSessionReceipt) { receipt.SessionID++ },
		"zero issued at":    func(receipt *qurl.NativeSessionReceipt) { receipt.SessionIssuedAtMillis = 0 },
		"negative issued":   func(receipt *qurl.NativeSessionReceipt) { receipt.SessionIssuedAtMillis = -1 },
		"wrong run":         func(receipt *qurl.NativeSessionReceipt) { receipt.RunID = "01ffffffffffffff" },
		"wrong run attempt": func(receipt *qurl.NativeSessionReceipt) { receipt.RunAttempt++ },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			logger, rec := captureLogger()
			knocker := &Native{
				binding: &qurl.AgentRuntimeBinding{}, privateKey: bytes.Repeat([]byte{0x43}, 32),
				resourceID: "resource-public-key", runID: "01abcdef23456789",
				target: "hub.test.nhp.layerv.ai:443", logger: logger,
			}
			defer knocker.Close()
			knocker.knock = func(_ context.Context, _ *qurl.AgentRuntimeBinding, _ []byte, _ string, opts qurl.NativeKnockOptions, _ ...qurl.AgentRuntimeUDPOption) (*qurl.NativeKnockResult, error) {
				result := testNativeKnockResult(opts, "token", "tunnel.test.layerv.ai:7000")
				mutate(&result.SessionReceipt)
				return result, nil
			}
			if _, err := knocker.Knock(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid exact-session receipt") {
				t.Fatalf("Knock() error = %v, want exact-session receipt rejection", err)
			}
			if knocker.receipt != nil {
				t.Fatalf("invalid receipt was retained: %+v", knocker.receipt)
			}
			entries := rec.snapshot()
			if len(entries) != 1 {
				t.Fatalf("log entries = %d, want 1", len(entries))
			}
			entry := entries[0]
			if entry.attrs["event"] != eventKnockError || entry.attrs["reason"] != "knock_invalid_response" ||
				entry.attrs["resource_id"] != knocker.resourceID || entry.attrs["target"] != knocker.target ||
				entry.attrs["run_id"] != knocker.runID || entry.attrs["err"] == "" {
				t.Fatalf("invalid-receipt log = %#v, want attributed knock_error/knock_invalid_response", entry)
			}
		})
	}
}

func TestNativeLogsMissingACTokenAsDeny(t *testing.T) {
	t.Parallel()
	logger, rec := captureLogger()
	knocker := &Native{
		binding: &qurl.AgentRuntimeBinding{}, privateKey: bytes.Repeat([]byte{0x52}, 32),
		resourceID: "resource-public-key", runID: "01abcdef23456789",
		target: "hub.test.nhp.layerv.ai:443", logger: logger,
	}
	defer knocker.Close()
	knocker.knock = func(_ context.Context, _ *qurl.AgentRuntimeBinding, _ []byte, _ string, opts qurl.NativeKnockOptions, _ ...qurl.AgentRuntimeUDPOption) (*qurl.NativeKnockResult, error) {
		return testNativeKnockResult(opts, "", "tunnel.test.layerv.ai:7000"), nil
	}

	result, err := knocker.Knock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.ACTokens[knocker.resourceID] != "" {
		t.Fatalf("ACToken = %q, want empty so the supervisor fails closed", result.ACTokens[knocker.resourceID])
	}
	entries := rec.snapshot()
	if len(entries) != 1 {
		t.Fatalf("log entries = %d, want 1", len(entries))
	}
	entry := entries[0]
	if entry.attrs["event"] != eventKnockDeny || entry.attrs["reason"] != "knock_token_missing" {
		t.Fatalf("missing-token log = %#v, want knock_deny/knock_token_missing", entry)
	}
	if entry.attrs["run_id"] != knocker.runID {
		t.Fatalf("missing-token log run_id = %q, want the cycle RunID %q", entry.attrs["run_id"], knocker.runID)
	}
}

func TestNativeLogsMissingResourceHostAsDeny(t *testing.T) {
	t.Parallel()
	logger, rec := captureLogger()
	knocker := &Native{
		binding: &qurl.AgentRuntimeBinding{}, privateKey: bytes.Repeat([]byte{0x53}, 32),
		resourceID: "resource-public-key", runID: "01abcdef23456789",
		target: "hub.test.nhp.layerv.ai:443", logger: logger,
	}
	defer knocker.Close()
	knocker.knock = func(_ context.Context, _ *qurl.AgentRuntimeBinding, _ []byte, _ string, opts qurl.NativeKnockOptions, _ ...qurl.AgentRuntimeUDPOption) (*qurl.NativeKnockResult, error) {
		return testNativeKnockResult(opts, "admission-token", ""), nil
	}

	result, err := knocker.Knock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.ResourceHost[knocker.resourceID] != "" {
		t.Fatalf("ResourceHost = %q, want empty so the supervisor fails closed", result.ResourceHost[knocker.resourceID])
	}
	entries := rec.snapshot()
	if len(entries) != 1 {
		t.Fatalf("log entries = %d, want 1", len(entries))
	}
	entry := entries[0]
	if entry.attrs["event"] != eventKnockDeny || entry.attrs["reason"] != "resource_host_missing" {
		t.Fatalf("missing-host log = %#v, want knock_deny/resource_host_missing", entry)
	}
	if entry.attrs["run_id"] != knocker.runID {
		t.Fatalf("missing-host log run_id = %q, want the cycle RunID %q", entry.attrs["run_id"], knocker.runID)
	}
}

func TestNativeEndCycleRetriesExactReceiptUntilAccepted(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("EXT reply unavailable")
	called := 0
	knocker := &Native{binding: &qurl.AgentRuntimeBinding{}, privateKey: bytes.Repeat([]byte{0x33}, 32), resourceID: "resource-public-key"}
	defer knocker.Close()
	wantRunID := "01abcdef23456789"
	wantReceipt := testNativeReceipt(wantRunID, 1)
	knocker.retire = func(_ context.Context, binding *qurl.AgentRuntimeBinding, key []byte, receipt qurl.NativeSessionReceipt, transportOpts ...qurl.AgentRuntimeUDPOption) (*qurl.NativeSessionRetirement, error) {
		called++
		if binding != knocker.binding || !bytes.Equal(key, knocker.privateKey) || !sameTestNativeReceipt(&receipt, &wantReceipt) || len(transportOpts) != 0 {
			t.Fatalf("EndCycle adapter call did not preserve binding/key/receipt/options")
		}
		if called == 1 {
			return nil, wantErr
		}
		return &qurl.NativeSessionRetirement{SessionReceipt: receipt, CloseEventID: strings.Repeat("a", 32), State: "closing"}, nil
	}
	if err := knocker.EndCycle(nil); err == nil || !strings.Contains(err.Error(), "context is nil") { //nolint:staticcheck // SA1012: nil is the invalid boundary under test.
		t.Fatalf("EndCycle() nil-context error = %v", err)
	}
	if err := knocker.EndCycle(context.Background()); err != nil || called != 0 {
		t.Fatalf("EndCycle() without receipt = %v, calls %d; want nil, 0", err, called)
	}

	knocker.runID = wantRunID
	knocker.receipt = &wantReceipt
	if err := knocker.EndCycle(context.Background()); !errors.Is(err, wantErr) || called != 1 {
		t.Fatalf("EndCycle() = %v, calls %d; want transport error, 1", err, called)
	}
	if knocker.CycleRunID() != "" {
		t.Fatalf("EndCycle retained consumed RunID %q", knocker.CycleRunID())
	}
	if knocker.receipt == nil {
		t.Fatal("EndCycle discarded failed retirement receipt")
	}
	if err := knocker.EndCycle(context.Background()); err != nil || called != 2 {
		t.Fatalf("second EndCycle() = %v, calls %d; want exact retry success", err, called)
	}
	if knocker.receipt != nil {
		t.Fatalf("successful retirement retained receipt %+v", knocker.receipt)
	}
}

func TestNativeUsesMonotonicAttemptsAndRetiresEveryReceipt(t *testing.T) {
	t.Parallel()
	const runID = "01abcdef23456789"
	knocker := &Native{
		binding: &qurl.AgentRuntimeBinding{}, privateKey: bytes.Repeat([]byte{0x34}, 32),
		resourceID: "resource-public-key", runID: runID,
	}
	defer knocker.Close()
	var attempts []uint64
	knocker.knock = func(_ context.Context, _ *qurl.AgentRuntimeBinding, _ []byte, _ string, opts qurl.NativeKnockOptions, _ ...qurl.AgentRuntimeUDPOption) (*qurl.NativeKnockResult, error) {
		attempts = append(attempts, opts.RunAttempt)
		return testNativeKnockResult(opts, fmt.Sprintf("token-%d", opts.RunAttempt), "tunnel.test.layerv.ai:7000"), nil
	}
	var retired []qurl.NativeSessionReceipt
	knocker.retire = func(_ context.Context, _ *qurl.AgentRuntimeBinding, _ []byte, receipt qurl.NativeSessionReceipt, _ ...qurl.AgentRuntimeUDPOption) (*qurl.NativeSessionRetirement, error) {
		retired = append(retired, receipt)
		return &qurl.NativeSessionRetirement{SessionReceipt: receipt, CloseEventID: strings.Repeat("e", 32), State: "closing"}, nil
	}
	for range 2 {
		if _, err := knocker.Knock(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if err := knocker.EndCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(attempts) != "[1 2]" {
		t.Fatalf("knock attempts = %v, want [1 2]", attempts)
	}
	if len(retired) != 2 || retired[0].RunID != runID || retired[0].RunAttempt != 1 ||
		retired[1].RunID != runID || retired[1].RunAttempt != 2 {
		t.Fatalf("retired receipts = %+v, want both physical sessions in attempt order", retired)
	}
}

func TestNativeKeepsSameCycleReconnectReceiptsBounded(t *testing.T) {
	t.Parallel()
	const (
		runID      = "01abcdef23456789"
		reconnects = 512
	)
	knocker := &Native{
		binding: &qurl.AgentRuntimeBinding{}, privateKey: bytes.Repeat([]byte{0x36}, 32),
		resourceID: "resource-public-key", runID: runID,
	}
	defer knocker.Close()
	knockCalls := 0
	knocker.knock = func(_ context.Context, _ *qurl.AgentRuntimeBinding, _ []byte, _ string, opts qurl.NativeKnockOptions, _ ...qurl.AgentRuntimeUDPOption) (*qurl.NativeKnockResult, error) {
		knockCalls++
		if knocker.receipt != nil {
			t.Fatalf("replacement knock %d started with obsolete receipt %+v", knockCalls, knocker.receipt)
		}
		return testNativeKnockResult(opts, fmt.Sprintf("token-%d", opts.RunAttempt), "tunnel.test.layerv.ai:7000"), nil
	}
	retired := make([]qurl.NativeSessionReceipt, 0, reconnects)
	knocker.retire = func(_ context.Context, _ *qurl.AgentRuntimeBinding, _ []byte, receipt qurl.NativeSessionReceipt, _ ...qurl.AgentRuntimeUDPOption) (*qurl.NativeSessionRetirement, error) {
		retired = append(retired, receipt)
		return &qurl.NativeSessionRetirement{SessionReceipt: receipt, CloseEventID: strings.Repeat("1", 32), State: "closing"}, nil
	}
	for attempt := uint64(1); attempt <= reconnects; attempt++ {
		if _, err := knocker.Knock(context.Background()); err != nil {
			t.Fatalf("replacement knock %d: %v", attempt, err)
		}
		if knocker.receipt == nil || knocker.receipt.RunAttempt != attempt {
			t.Fatalf("after replacement %d receipt = %+v, want only current attempt", attempt, knocker.receipt)
		}
	}
	if err := knocker.EndCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if knockCalls != reconnects || len(retired) != reconnects {
		t.Fatalf("calls = knocks %d retired %d, want %d each", knockCalls, len(retired), reconnects)
	}
	for i, receipt := range retired {
		if want := uint64(i + 1); receipt.RunAttempt != want {
			t.Fatalf("retirement %d attempt = %d, want %d", i, receipt.RunAttempt, want)
		}
	}
}

func TestNativeRetirementFailureBlocksReplacementAdmission(t *testing.T) {
	t.Parallel()
	const runID = "01abcdef23456789"
	wantErr := errors.New("exact retirement unavailable")
	knocker := &Native{
		binding: &qurl.AgentRuntimeBinding{}, privateKey: bytes.Repeat([]byte{0x37}, 32),
		resourceID: "resource-public-key", runID: runID,
	}
	defer knocker.Close()
	knockCalls := 0
	knocker.knock = func(_ context.Context, _ *qurl.AgentRuntimeBinding, _ []byte, _ string, opts qurl.NativeKnockOptions, _ ...qurl.AgentRuntimeUDPOption) (*qurl.NativeKnockResult, error) {
		knockCalls++
		return testNativeKnockResult(opts, fmt.Sprintf("token-%d", opts.RunAttempt), "tunnel.test.layerv.ai:7000"), nil
	}
	retireCalls := 0
	knocker.retire = func(_ context.Context, _ *qurl.AgentRuntimeBinding, _ []byte, receipt qurl.NativeSessionReceipt, _ ...qurl.AgentRuntimeUDPOption) (*qurl.NativeSessionRetirement, error) {
		retireCalls++
		if retireCalls == 1 {
			return nil, wantErr
		}
		return &qurl.NativeSessionRetirement{SessionReceipt: receipt, CloseEventID: strings.Repeat("b", 32), State: "closing"}, nil
	}
	if _, err := knocker.Knock(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := knocker.Knock(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("replacement Knock() error = %v, want retirement error", err)
	}
	if knockCalls != 1 || knocker.receipt == nil || knocker.receipt.RunAttempt != 1 {
		t.Fatalf("failed replacement changed authority: knocks %d receipt %+v", knockCalls, knocker.receipt)
	}
	if _, err := knocker.Knock(context.Background()); err != nil {
		t.Fatalf("replacement after retirement recovery: %v", err)
	}
	if knockCalls != 2 || knocker.receipt == nil || knocker.receipt.RunAttempt != 2 {
		t.Fatalf("recovered replacement state: knocks %d receipt %+v", knockCalls, knocker.receipt)
	}
}

func TestNativeBlocksNewCycleAdmissionUntilPriorReceiptRetires(t *testing.T) {
	t.Parallel()
	const oldRunID = "01abcdef23456789"
	wantErr := errors.New("old exact retirement unavailable")
	knocker := &Native{
		binding: &qurl.AgentRuntimeBinding{}, privateKey: bytes.Repeat([]byte{0x35}, 32),
		resourceID: "resource-public-key", runID: oldRunID,
		receipt: testNativeReceiptPtr(oldRunID, 1),
	}
	defer knocker.Close()
	retireCalls := 0
	knocker.retire = func(_ context.Context, _ *qurl.AgentRuntimeBinding, _ []byte, receipt qurl.NativeSessionReceipt, _ ...qurl.AgentRuntimeUDPOption) (*qurl.NativeSessionRetirement, error) {
		retireCalls++
		if retireCalls <= 2 {
			return nil, wantErr
		}
		return &qurl.NativeSessionRetirement{SessionReceipt: receipt, CloseEventID: strings.Repeat("c", 32), State: "closing"}, nil
	}
	if err := knocker.EndCycle(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("old EndCycle = %v, want retained exact retirement", err)
	}
	if err := knocker.BeginCycle(); err != nil {
		t.Fatal(err)
	}
	knockCalls := 0
	knocker.knock = func(_ context.Context, _ *qurl.AgentRuntimeBinding, _ []byte, _ string, opts qurl.NativeKnockOptions, _ ...qurl.AgentRuntimeUDPOption) (*qurl.NativeKnockResult, error) {
		knockCalls++
		return testNativeKnockResult(opts, "new-token", "tunnel.test.layerv.ai:7000"), nil
	}
	if _, err := knocker.Knock(context.Background()); !errors.Is(err, wantErr) || knockCalls != 0 {
		t.Fatalf("new Knock before old retirement = (%v, calls %d), want fail closed without admission", err, knockCalls)
	}
	if _, err := knocker.Knock(context.Background()); err != nil || knockCalls != 1 {
		t.Fatalf("new Knock after old retirement = (%v, calls %d), want one admission", err, knockCalls)
	}
	if knocker.runAttempt != 1 || knocker.receipt == nil || knocker.receipt.RunID != knocker.runID {
		t.Fatalf("new-cycle state = attempt %d receipt %+v", knocker.runAttempt, knocker.receipt)
	}
}

func TestNativeLostKnockReplyDoesNotFabricateRetirement(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("reply lost")
	knocker := &Native{
		binding: &qurl.AgentRuntimeBinding{}, privateKey: bytes.Repeat([]byte{0x38}, 32),
		resourceID: "resource-public-key", runID: "01abcdef23456789",
	}
	defer knocker.Close()
	knocker.knock = func(context.Context, *qurl.AgentRuntimeBinding, []byte, string, qurl.NativeKnockOptions, ...qurl.AgentRuntimeUDPOption) (*qurl.NativeKnockResult, error) {
		return nil, wantErr
	}
	retireCalls := 0
	knocker.retire = func(context.Context, *qurl.AgentRuntimeBinding, []byte, qurl.NativeSessionReceipt, ...qurl.AgentRuntimeUDPOption) (*qurl.NativeSessionRetirement, error) {
		retireCalls++
		return nil, errors.New("unexpected retirement")
	}
	if _, err := knocker.Knock(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("Knock() = %v, want lost reply", err)
	}
	if knocker.receipt != nil {
		t.Fatalf("lost reply fabricated receipt %+v", knocker.receipt)
	}
	if err := knocker.EndCycle(context.Background()); err != nil || retireCalls != 0 {
		t.Fatalf("EndCycle after lost reply = %v, retirement calls %d; want nil, 0", err, retireCalls)
	}
}

func TestNativeLostReplyConsumesAttemptWithoutFabricatingReceipt(t *testing.T) {
	t.Parallel()
	const runID = "01abcdef23456789"
	wantErr := errors.New("reply lost")
	knocker := &Native{
		binding: &qurl.AgentRuntimeBinding{}, privateKey: bytes.Repeat([]byte{0x3a}, 32),
		resourceID: "resource-public-key", runID: runID,
	}
	defer knocker.Close()
	var attempts []uint64
	knocker.knock = func(_ context.Context, _ *qurl.AgentRuntimeBinding, _ []byte, _ string, opts qurl.NativeKnockOptions, _ ...qurl.AgentRuntimeUDPOption) (*qurl.NativeKnockResult, error) {
		attempts = append(attempts, opts.RunAttempt)
		if opts.RunAttempt == 1 {
			return nil, wantErr
		}
		return testNativeKnockResult(opts, "token-2", "tunnel.test.layerv.ai:7000"), nil
	}
	var retired []qurl.NativeSessionReceipt
	knocker.retire = func(_ context.Context, _ *qurl.AgentRuntimeBinding, _ []byte, receipt qurl.NativeSessionReceipt, _ ...qurl.AgentRuntimeUDPOption) (*qurl.NativeSessionRetirement, error) {
		retired = append(retired, receipt)
		return &qurl.NativeSessionRetirement{SessionReceipt: receipt, CloseEventID: strings.Repeat("4", 32), State: "closing"}, nil
	}
	if _, err := knocker.Knock(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("first Knock() = %v, want lost reply", err)
	}
	if knocker.receipt != nil || len(retired) != 0 {
		t.Fatalf("lost attempt created local retirement authority: receipt %+v retired %+v", knocker.receipt, retired)
	}
	if _, err := knocker.Knock(context.Background()); err != nil {
		t.Fatalf("second Knock() = %v", err)
	}
	if fmt.Sprint(attempts) != "[1 2]" {
		t.Fatalf("attempts = %v, want lost attempt 1 then admitted attempt 2", attempts)
	}
	if err := knocker.EndCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(retired) != 1 || retired[0].RunID != runID || retired[0].RunAttempt != 2 {
		t.Fatalf("retired receipts = %+v, want only authenticated attempt 2", retired)
	}
}

func TestNativeRejectsRunAttemptOverflow(t *testing.T) {
	t.Parallel()
	knocker := &Native{
		binding: &qurl.AgentRuntimeBinding{}, privateKey: bytes.Repeat([]byte{0x39}, 32),
		resourceID: "resource-public-key", runID: "01abcdef23456789", runAttempt: ^uint64(0),
	}
	defer knocker.Close()
	knockCalls := 0
	knocker.knock = func(context.Context, *qurl.AgentRuntimeBinding, []byte, string, qurl.NativeKnockOptions, ...qurl.AgentRuntimeUDPOption) (*qurl.NativeKnockResult, error) {
		knockCalls++
		return nil, nil //nolint:nilnil // no call is expected.
	}
	if _, err := knocker.Knock(context.Background()); err == nil || !strings.Contains(err.Error(), "exhausted its attempt space") {
		t.Fatalf("Knock() overflow error = %v", err)
	}
	if knockCalls != 0 {
		t.Fatalf("Knock() SDK calls = %d, want 0", knockCalls)
	}
}

func TestNativeRejectsUseAfterClose(t *testing.T) {
	t.Parallel()
	knockCalls := 0
	receipt := testNativeReceipt("01abcdef23456789", 1)
	knocker := &Native{
		binding: &qurl.AgentRuntimeBinding{}, privateKey: bytes.Repeat([]byte{0x61}, 32),
		resourceID: "resource-public-key", runID: receipt.RunID, runAttempt: receipt.RunAttempt,
		receipt: &receipt,
	}
	knocker.knock = func(context.Context, *qurl.AgentRuntimeBinding, []byte, string, qurl.NativeKnockOptions, ...qurl.AgentRuntimeUDPOption) (*qurl.NativeKnockResult, error) {
		knockCalls++
		return nil, nil //nolint:nilnil // the nil-admission-without-error SDK shape is exactly the scenario under test.
	}
	knocker.Close()

	if err := knocker.BeginCycle(); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("BeginCycle() after Close = %v, want closed error", err)
	}
	if _, err := knocker.Knock(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Knock() after Close = %v, want closed error", err)
	}
	if knockCalls != 0 {
		t.Fatalf("SDK knock calls after Close = %d, want 0", knockCalls)
	}
	if knocker.runAttempt != 0 || knocker.receipt != nil || receipt.CellID != "" || receipt.SessionID != 0 ||
		receipt.SessionIssuedAtMillis != 0 || receipt.RunID != "" || receipt.RunAttempt != 0 {
		t.Fatalf("Close retained attempt or exact receipt: attempt %d pointer %+v value %+v", knocker.runAttempt, knocker.receipt, receipt)
	}
}

func TestNativeEndCycleConsumesRunIDBeforeInvalidStateReturn(t *testing.T) {
	t.Parallel()
	knocker := &Native{
		binding: &qurl.AgentRuntimeBinding{}, privateKey: make([]byte, 31),
		resourceID: "resource-public-key", runID: "01abcdef23456789",
	}
	if err := knocker.EndCycle(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("EndCycle() = %v, want closed runtime error", err)
	}
	if got := knocker.CycleRunID(); got != "" {
		t.Fatalf("EndCycle retained invalid runtime RunID %q", got)
	}
}

func TestNativeCloseCannotRaceInFlightKnock(t *testing.T) {
	t.Parallel()
	var nilKnocker *Native
	nilKnocker.Close()

	privateKey := bytes.Repeat([]byte{0x7c}, 32)
	entered := make(chan struct{})
	release := make(chan struct{})
	knocker := &Native{
		binding: &qurl.AgentRuntimeBinding{}, privateKey: privateKey,
		resourceID: "resource-public-key", runID: "01abcdef23456789",
	}
	knocker.knock = func(_ context.Context, _ *qurl.AgentRuntimeBinding, _ []byte, _ string, opts qurl.NativeKnockOptions, _ ...qurl.AgentRuntimeUDPOption) (*qurl.NativeKnockResult, error) {
		close(entered)
		<-release
		return testNativeKnockResult(opts, "token", "tunnel.test.layerv.ai:7000"), nil
	}

	knockDone := make(chan error, 1)
	go func() {
		_, err := knocker.Knock(context.Background())
		knockDone <- err
	}()
	<-entered
	closeDone := make(chan struct{})
	go func() {
		knocker.Close()
		close(closeDone)
	}()
	close(release)
	if err := <-knockDone; err != nil {
		t.Fatal(err)
	}
	<-closeDone
	if knocker.privateKey != nil || knocker.runID != "" {
		t.Fatalf("Close() retained key or RunID")
	}
	if !bytes.Equal(privateKey, make([]byte, len(privateKey))) {
		t.Fatal("Close() did not zero the transferred key bytes")
	}
	knocker.Close() // idempotent and nil-safe even after binding destruction.
}

func TestNativeEndCycleCannotRaceInFlightKnock(t *testing.T) {
	t.Parallel()
	const runID = "01abcdef23456789"
	entered := make(chan struct{})
	release := make(chan struct{})
	knocker := &Native{
		binding: &qurl.AgentRuntimeBinding{}, privateKey: bytes.Repeat([]byte{0x71}, 32),
		resourceID: "resource-public-key", runID: runID,
	}
	defer knocker.Close()
	var retiredReceipts []qurl.NativeSessionReceipt
	knocker.knock = func(_ context.Context, _ *qurl.AgentRuntimeBinding, _ []byte, _ string, opts qurl.NativeKnockOptions, _ ...qurl.AgentRuntimeUDPOption) (*qurl.NativeKnockResult, error) {
		close(entered)
		<-release
		// The in-flight exchange must keep this cycle's RunID for its whole
		// duration; a concurrent EndCycle may not consume it out from under us.
		if opts.RunID != runID {
			t.Errorf("in-flight knock RunID = %q, want %q", opts.RunID, runID)
		}
		return testNativeKnockResult(opts, "token", "tunnel.test.layerv.ai:7000"), nil
	}
	knocker.retire = func(_ context.Context, _ *qurl.AgentRuntimeBinding, _ []byte, receipt qurl.NativeSessionReceipt, _ ...qurl.AgentRuntimeUDPOption) (*qurl.NativeSessionRetirement, error) {
		retiredReceipts = append(retiredReceipts, receipt)
		return &qurl.NativeSessionRetirement{SessionReceipt: receipt, CloseEventID: strings.Repeat("d", 32), State: "closing"}, nil
	}

	knockDone := make(chan error, 1)
	go func() {
		_, err := knocker.Knock(context.Background())
		knockDone <- err
	}()
	<-entered
	endDone := make(chan error, 1)
	go func() { endDone <- knocker.EndCycle(context.Background()) }()
	close(release)
	if err := <-knockDone; err != nil {
		t.Fatal(err)
	}
	if err := <-endDone; err != nil {
		t.Fatal(err)
	}
	if len(retiredReceipts) != 1 || retiredReceipts[0].RunID != runID || retiredReceipts[0].RunAttempt != 1 {
		t.Fatalf("retired receipts = %+v, want exactly attempt 1 of %q", retiredReceipts, runID)
	}
	if got := knocker.CycleRunID(); got != "" {
		t.Fatalf("EndCycle retained consumed RunID %q", got)
	}
	// A consumed RunID is never reused: EndCycle replays are no-ops and a
	// fresh Knock fails closed until BeginCycle issues a new one.
	if err := knocker.EndCycle(context.Background()); err != nil || len(retiredReceipts) != 1 {
		t.Fatalf("EndCycle replay = %v, retirement calls %d; want nil and still 1", err, len(retiredReceipts))
	}
	if _, err := knocker.Knock(context.Background()); err == nil || !strings.Contains(err.Error(), "no RunID") {
		t.Fatalf("Knock() after consumed cycle = %v, want missing-RunID rejection", err)
	}
}

func TestNativeCloseCannotRaceInFlightEndCycle(t *testing.T) {
	t.Parallel()
	const runID = "01abcdef23456789"
	privateKey := bytes.Repeat([]byte{0x72}, 32)
	wantKey := bytes.Clone(privateKey)
	entered := make(chan struct{})
	release := make(chan struct{})
	knocker := &Native{
		binding: &qurl.AgentRuntimeBinding{}, privateKey: privateKey,
		resourceID: "resource-public-key", runID: runID,
		receipt: testNativeReceiptPtr(runID, 1),
	}
	knocker.retire = func(_ context.Context, binding *qurl.AgentRuntimeBinding, key []byte, receipt qurl.NativeSessionReceipt, _ ...qurl.AgentRuntimeUDPOption) (*qurl.NativeSessionRetirement, error) {
		close(entered)
		<-release
		// Close must not wipe the key or destroy the binding while this EXT
		// exchange is still using them.
		if binding == nil || !bytes.Equal(key, wantKey) {
			t.Error("Close wiped runtime state under an in-flight EXT")
		}
		if receipt.RunID != runID || receipt.RunAttempt != 1 {
			t.Errorf("in-flight retirement receipt = %+v, want attempt 1 of %q", receipt, runID)
		}
		return &qurl.NativeSessionRetirement{SessionReceipt: receipt, CloseEventID: strings.Repeat("e", 32), State: "closing"}, nil
	}

	endDone := make(chan error, 1)
	go func() { endDone <- knocker.EndCycle(context.Background()) }()
	<-entered
	closeDone := make(chan struct{})
	go func() {
		knocker.Close()
		close(closeDone)
	}()
	close(release)
	if err := <-endDone; err != nil {
		t.Fatal(err)
	}
	<-closeDone
	if !bytes.Equal(privateKey, make([]byte, len(privateKey))) {
		t.Fatal("Close() did not zero the transferred key bytes")
	}
	if err := knocker.BeginCycle(); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("BeginCycle() after Close = %v, want closed error", err)
	}
	if _, err := knocker.Knock(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Knock() after Close = %v, want closed error", err)
	}
	knocker.Close() // idempotent after the racing close.
}

func TestNativeBeginCycleCannotRaceInFlightKnock(t *testing.T) {
	t.Parallel()
	const firstRunID = "01abcdef23456789"
	entered := make(chan struct{})
	release := make(chan struct{})
	knocker := &Native{
		binding: &qurl.AgentRuntimeBinding{}, privateKey: bytes.Repeat([]byte{0x73}, 32),
		resourceID: "resource-public-key", runID: firstRunID,
	}
	defer knocker.Close()
	knocker.knock = func(_ context.Context, _ *qurl.AgentRuntimeBinding, _ []byte, _ string, opts qurl.NativeKnockOptions, _ ...qurl.AgentRuntimeUDPOption) (*qurl.NativeKnockResult, error) {
		close(entered)
		<-release
		// A concurrent BeginCycle may not swap the RunID under the in-flight
		// exchange; the regeneration lands strictly after it completes.
		if opts.RunID != firstRunID {
			t.Errorf("in-flight knock RunID = %q, want %q", opts.RunID, firstRunID)
		}
		return testNativeKnockResult(opts, "token", "tunnel.test.layerv.ai:7000"), nil
	}

	knockDone := make(chan error, 1)
	go func() {
		_, err := knocker.Knock(context.Background())
		knockDone <- err
	}()
	<-entered
	beginDone := make(chan error, 1)
	go func() { beginDone <- knocker.BeginCycle() }()
	close(release)
	if err := <-knockDone; err != nil {
		t.Fatal(err)
	}
	if err := <-beginDone; err != nil {
		t.Fatal(err)
	}
	second := knocker.CycleRunID()
	if err := qurl.ValidateCycleRunID(second); err != nil {
		t.Fatalf("post-race CycleRunID %q is not canonical: %v", second, err)
	}
	if second == firstRunID {
		t.Fatalf("BeginCycle reused the in-flight cycle RunID %q", firstRunID)
	}
}

// TestNativeConcurrentLifecycleStress is the race detector for the whole
// mutex discipline: 4 goroutines hammer BeginCycle/Knock/EndCycle with Close
// landing mid-flight. The injected fakes assert the runtime state they
// observe is never torn, and the retirement log asserts no exact receipt is
// ever replayed after a successful acknowledgement.
func TestNativeConcurrentLifecycleStress(t *testing.T) {
	t.Parallel()
	logger, _ := captureLogger()
	wantKey := bytes.Repeat([]byte{0x7d}, 32)
	privateKey := bytes.Clone(wantKey)
	knocker := &Native{
		binding: &qurl.AgentRuntimeBinding{}, privateKey: privateKey,
		resourceID: "resource-public-key", logger: logger,
	}
	var integrity sync.Mutex
	consumed := make(map[string]int)
	knocker.knock = func(_ context.Context, binding *qurl.AgentRuntimeBinding, key []byte, _ string, opts qurl.NativeKnockOptions, _ ...qurl.AgentRuntimeUDPOption) (*qurl.NativeKnockResult, error) {
		if binding == nil || !bytes.Equal(key, wantKey) || opts.RunID == "" {
			t.Error("in-flight knock observed torn runtime state")
		}
		return testNativeKnockResult(opts, "token", "tunnel.test.layerv.ai:7000"), nil
	}
	knocker.retire = func(_ context.Context, binding *qurl.AgentRuntimeBinding, key []byte, receipt qurl.NativeSessionReceipt, _ ...qurl.AgentRuntimeUDPOption) (*qurl.NativeSessionRetirement, error) {
		if binding == nil || !bytes.Equal(key, wantKey) || receipt.RunID == "" || receipt.RunAttempt == 0 {
			t.Error("in-flight retirement observed torn runtime state")
		}
		integrity.Lock()
		consumed[fmt.Sprintf("%s/%d", receipt.RunID, receipt.RunAttempt)]++
		integrity.Unlock()
		return &qurl.NativeSessionRetirement{SessionReceipt: receipt, CloseEventID: strings.Repeat("f", 32), State: "closing"}, nil
	}

	const workers = 4
	const iterations = 25
	runPhase := func(closeMidway bool, allowed func(error) bool) {
		start := make(chan struct{})
		var wg sync.WaitGroup
		for worker := range workers {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				<-start
				for i := range iterations {
					if closeMidway && worker == 0 && i == 0 {
						knocker.Close()
					}
					if err := knocker.BeginCycle(); !allowed(err) {
						t.Errorf("BeginCycle() = %v", err)
					}
					if _, err := knocker.Knock(context.Background()); !allowed(err) {
						t.Errorf("Knock() = %v", err)
					}
					if err := knocker.EndCycle(context.Background()); !allowed(err) {
						t.Errorf("EndCycle() = %v", err)
					}
				}
			}(worker)
		}
		close(start)
		wg.Wait()
	}
	liveErrs := func(err error) bool {
		return err == nil || strings.Contains(err.Error(), "no RunID")
	}
	runPhase(false, liveErrs)
	runPhase(true, func(err error) bool {
		return liveErrs(err) || strings.Contains(err.Error(), "closed")
	})

	knocker.Close()
	integrity.Lock()
	defer integrity.Unlock()
	if len(consumed) == 0 {
		t.Fatal("stress consumed no cycle RunIDs; the phases exercised nothing")
	}
	for runID, replays := range consumed {
		if replays != 1 {
			t.Errorf("exact session %q reached retirement %d times, want exactly once", runID, replays)
		}
	}
	if err := knocker.BeginCycle(); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("BeginCycle() after stress Close = %v, want closed error", err)
	}
	if _, err := knocker.Knock(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Knock() after stress Close = %v, want closed error", err)
	}
	if knocker.privateKey != nil || knocker.runID != "" {
		t.Fatal("Close() retained key or RunID after stress")
	}
	if !bytes.Equal(privateKey, make([]byte, len(privateKey))) {
		t.Fatal("Close() did not zero the transferred key bytes after stress")
	}
}

func TestClassifyKnockFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		err        error
		wantEvent  string
		wantReason string
	}{
		{name: "authenticated deny", err: &qurl.ServerDenyError{ErrCode: "52001"}, wantEvent: eventKnockDeny, wantReason: "knock_denied"},
		{name: "resource not found deny", err: &qurl.ServerDenyError{ErrCode: nativeKnockResourceNotFoundCode}, wantEvent: eventKnockDeny, wantReason: "resource_not_found"},
		{name: "wrapped deny keeps classification", err: fmt.Errorf("wrap: %w", &qurl.ServerDenyError{ErrCode: "52001"}), wantEvent: eventKnockDeny, wantReason: "knock_denied"},
		{name: "invalid input", err: fmt.Errorf("wrap: %w", qurl.ErrInvalidNativeKnockInput), wantEvent: eventKnockError, wantReason: "knock_invalid_input"},
		{name: "malformed reply", err: qurl.ErrMalformedReply, wantEvent: eventKnockError, wantReason: "knock_invalid_response"},
		{name: "server overloaded", err: qurl.ErrServerOverloaded, wantEvent: eventKnockError, wantReason: "knock_server_overloaded"},
		{name: "generic transport", err: errors.New("read udp: i/o timeout"), wantEvent: eventKnockError, wantReason: "knock_transport_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := classifyKnockFailure(tt.err)
			if got.event != tt.wantEvent || got.reason != tt.wantReason {
				t.Fatalf("classifyKnockFailure(%v) = %+v, want event %q reason %q", tt.err, got, tt.wantEvent, tt.wantReason)
			}
		})
	}
}

func TestLatencyMSKeepsSubMillisecondPrecision(t *testing.T) {
	t.Parallel()
	if got := latencyMS(750 * time.Microsecond); got != 0.75 {
		t.Fatalf("latencyMS(750µs) = %v, want 0.75", got)
	}
	if got := latencyMS(2 * time.Millisecond); got != 2 {
		t.Fatalf("latencyMS(2ms) = %v, want 2", got)
	}
}
