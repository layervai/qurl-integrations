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
		if resourceID != knocker.resourceID || opts.RunID != knocker.runID || len(transportOpts) != 1 {
			t.Fatalf("adapter call = resource %q, RunID %q, transport opts %d", resourceID, opts.RunID, len(transportOpts))
		}
		return &qurl.NativeKnockResult{ACToken: "admission-token", ResourceHost: "tunnel.test.layerv.ai:7000"}, nil
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

func TestNativeLogsMissingACTokenAsDeny(t *testing.T) {
	t.Parallel()
	logger, rec := captureLogger()
	knocker := &Native{
		binding: &qurl.AgentRuntimeBinding{}, privateKey: bytes.Repeat([]byte{0x52}, 32),
		resourceID: "resource-public-key", runID: "01abcdef23456789",
		target: "hub.test.nhp.layerv.ai:443", logger: logger,
	}
	defer knocker.Close()
	knocker.knock = func(context.Context, *qurl.AgentRuntimeBinding, []byte, string, qurl.NativeKnockOptions, ...qurl.AgentRuntimeUDPOption) (*qurl.NativeKnockResult, error) {
		return &qurl.NativeKnockResult{ResourceHost: "tunnel.test.layerv.ai:7000"}, nil
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
	knocker.knock = func(context.Context, *qurl.AgentRuntimeBinding, []byte, string, qurl.NativeKnockOptions, ...qurl.AgentRuntimeUDPOption) (*qurl.NativeKnockResult, error) {
		return &qurl.NativeKnockResult{ACToken: "admission-token"}, nil
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

func TestNativeEndCycleForwardsIdentityAndSkipsEmptyRunID(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("EXT reply unavailable")
	called := 0
	knocker := &Native{binding: &qurl.AgentRuntimeBinding{}, privateKey: bytes.Repeat([]byte{0x33}, 32), resourceID: "resource-public-key"}
	defer knocker.Close()
	wantRunID := "01abcdef23456789"
	knocker.exit = func(_ context.Context, binding *qurl.AgentRuntimeBinding, key []byte, resourceID string, opts qurl.NativeKnockOptions, transportOpts ...qurl.AgentRuntimeUDPOption) error {
		called++
		if binding != knocker.binding || !bytes.Equal(key, knocker.privateKey) || resourceID != knocker.resourceID || opts.RunID != wantRunID || len(transportOpts) != 0 {
			t.Fatalf("EndCycle adapter call did not preserve binding/key/resource/RunID/options")
		}
		return wantErr
	}
	if err := knocker.EndCycle(context.Background()); err != nil || called != 0 {
		t.Fatalf("EndCycle() without RunID = %v, calls %d; want nil, 0", err, called)
	}

	knocker.runID = wantRunID
	if err := knocker.EndCycle(context.Background()); !errors.Is(err, wantErr) || called != 1 {
		t.Fatalf("EndCycle() = %v, calls %d; want transport error, 1", err, called)
	}
	if knocker.CycleRunID() != "" {
		t.Fatalf("EndCycle retained consumed RunID %q", knocker.CycleRunID())
	}
	if err := knocker.EndCycle(context.Background()); err != nil || called != 1 {
		t.Fatalf("second EndCycle() = %v, calls %d; want nil and no replay", err, called)
	}
}

func TestNativeRejectsUseAfterClose(t *testing.T) {
	t.Parallel()
	knockCalls := 0
	knocker := &Native{
		binding: &qurl.AgentRuntimeBinding{}, privateKey: bytes.Repeat([]byte{0x61}, 32),
		resourceID: "resource-public-key", runID: "01abcdef23456789",
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
	knocker.knock = func(context.Context, *qurl.AgentRuntimeBinding, []byte, string, qurl.NativeKnockOptions, ...qurl.AgentRuntimeUDPOption) (*qurl.NativeKnockResult, error) {
		close(entered)
		<-release
		return &qurl.NativeKnockResult{ACToken: "token", ResourceHost: "tunnel.test.layerv.ai:7000"}, nil
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
	var exitRunIDs []string
	knocker.knock = func(_ context.Context, _ *qurl.AgentRuntimeBinding, _ []byte, _ string, opts qurl.NativeKnockOptions, _ ...qurl.AgentRuntimeUDPOption) (*qurl.NativeKnockResult, error) {
		close(entered)
		<-release
		// The in-flight exchange must keep this cycle's RunID for its whole
		// duration; a concurrent EndCycle may not consume it out from under us.
		if opts.RunID != runID {
			t.Errorf("in-flight knock RunID = %q, want %q", opts.RunID, runID)
		}
		return &qurl.NativeKnockResult{ACToken: "token", ResourceHost: "tunnel.test.layerv.ai:7000"}, nil
	}
	knocker.exit = func(_ context.Context, _ *qurl.AgentRuntimeBinding, _ []byte, _ string, opts qurl.NativeKnockOptions, _ ...qurl.AgentRuntimeUDPOption) error {
		exitRunIDs = append(exitRunIDs, opts.RunID)
		return nil
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
	if len(exitRunIDs) != 1 || exitRunIDs[0] != runID {
		t.Fatalf("EXT RunIDs = %v, want exactly one consume of %q", exitRunIDs, runID)
	}
	if got := knocker.CycleRunID(); got != "" {
		t.Fatalf("EndCycle retained consumed RunID %q", got)
	}
	// A consumed RunID is never reused: EndCycle replays are no-ops and a
	// fresh Knock fails closed until BeginCycle issues a new one.
	if err := knocker.EndCycle(context.Background()); err != nil || len(exitRunIDs) != 1 {
		t.Fatalf("EndCycle replay = %v, EXT calls %d; want nil and still 1", err, len(exitRunIDs))
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
	}
	knocker.exit = func(_ context.Context, binding *qurl.AgentRuntimeBinding, key []byte, _ string, opts qurl.NativeKnockOptions, _ ...qurl.AgentRuntimeUDPOption) error {
		close(entered)
		<-release
		// Close must not wipe the key or destroy the binding while this EXT
		// exchange is still using them.
		if binding == nil || !bytes.Equal(key, wantKey) {
			t.Error("Close wiped runtime state under an in-flight EXT")
		}
		if opts.RunID != runID {
			t.Errorf("in-flight EXT RunID = %q, want %q", opts.RunID, runID)
		}
		return nil
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
		return &qurl.NativeKnockResult{ACToken: "token", ResourceHost: "tunnel.test.layerv.ai:7000"}, nil
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
// observe is never torn, and the EXT log asserts no consumed RunID is ever
// replayed.
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
		return &qurl.NativeKnockResult{ACToken: "token", ResourceHost: "tunnel.test.layerv.ai:7000"}, nil
	}
	knocker.exit = func(_ context.Context, binding *qurl.AgentRuntimeBinding, key []byte, _ string, opts qurl.NativeKnockOptions, _ ...qurl.AgentRuntimeUDPOption) error {
		if binding == nil || !bytes.Equal(key, wantKey) || opts.RunID == "" {
			t.Error("in-flight EXT observed torn runtime state")
		}
		integrity.Lock()
		consumed[opts.RunID]++
		integrity.Unlock()
		return nil
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
			t.Errorf("cycle RunID %q reached EXT %d times, want exactly once", runID, replays)
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
