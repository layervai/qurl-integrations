package matchedcohort

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	qurl "github.com/layervai/qurl-go/qurl"
	"github.com/layervai/qurl-go/relayknock/nativeudp"
)

func TestPrepareClosurePersistsIntentAndFourOperationsBeforeNetwork(t *testing.T) {
	consumer, authority, runtime := closureFixture(t)
	prepared, err := consumer.PrepareClosure(context.Background(), authority, closureInput())
	if err != nil {
		t.Fatalf("PrepareClosure: %v", err)
	}
	if runtime.prepares != 4 || runtime.admits != 0 || runtime.recovers != 0 {
		t.Fatalf("runtime before network = %#v", runtime)
	}
	if err := consumer.ValidatePreparedClosure(context.Background(), prepared); err != nil {
		t.Fatalf("ValidatePreparedClosure: %v", err)
	}
	for index, key := range prepared.OperationKeys {
		record, _, loadErr := loadOperation(context.Background(), consumer.Blobs, key)
		if loadErr != nil || record.Status != OperationPrepared || record.Label != closureLabels[index] ||
			record.Operation.RunID != prepared.Intent.Operations[index].RunID || runtime.intentVisible[index] != prepared.IntentReference.Key {
			t.Fatalf("operation %d = %#v visible=%q err=%v", index, record, runtime.intentVisible[index], loadErr)
		}
	}
	// An exact partial offline replay reuses the immutable intent and operations.
	replayed, err := consumer.PrepareClosure(context.Background(), authority, closureInput())
	if err != nil || replayed.IntentReference != prepared.IntentReference || replayed.OperationKeys != prepared.OperationKeys {
		t.Fatalf("replay = %#v, %v", replayed, err)
	}
}

func TestRunPreparedClosureRequiresFourCanceledAndExactSelectorSet(t *testing.T) {
	consumer, authority, runtime := closureFixture(t)
	prepared, err := consumer.PrepareClosure(context.Background(), authority, closureInput())
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := consumer.RunPreparedClosure(context.Background(), authority, prepared, time.Second, 2*time.Second)
	if err != nil {
		t.Fatalf("RunPreparedClosure: %v", err)
	}
	if outcome.AdmissionSucceeded || len(outcome.AttemptedLabels) != 4 || len(outcome.CanceledOperationKeys) != 4 ||
		strings.Join(outcome.UniqueFRPSSelectors, ",") != "qurl-tunnel-server-a,qurl-tunnel-server-b,qurl-tunnel-server-c" {
		t.Fatalf("outcome = %#v", outcome)
	}
	if runtime.admits != 4 || runtime.recovers != 4 || runtime.retires != 0 {
		t.Fatalf("runtime = %#v", runtime)
	}
	if runtime.evidenceBeforeRecover != 4 {
		t.Fatalf("evidence persisted after recovery: %#v", runtime)
	}
	for _, key := range prepared.OperationKeys {
		record, _, loadErr := loadOperation(context.Background(), consumer.Blobs, key)
		if loadErr != nil || record.Status != OperationCanceled || record.Terminal == nil || record.Terminal.State != OperationCanceled {
			t.Fatalf("terminal %q = %#v, %v", key, record, loadErr)
		}
	}
}

func TestRunPreparedClosureFailsAndClosesUnexpectedAdmission(t *testing.T) {
	consumer, authority, runtime := closureFixture(t)
	runtime.admitIndex = 2
	prepared, err := consumer.PrepareClosure(context.Background(), authority, closureInput())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := consumer.RunPreparedClosure(context.Background(), authority, prepared, time.Second, 2*time.Second); err == nil {
		t.Fatal("unexpected admission was accepted as closed")
	}
	if runtime.retires != 1 {
		t.Fatalf("unexpected admitted operation was not retired: %#v", runtime)
	}
	closed := 0
	for _, key := range prepared.OperationKeys {
		record, _, loadErr := loadOperation(context.Background(), consumer.Blobs, key)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if record.Status == OperationClosed {
			closed++
		}
	}
	if closed != 1 {
		t.Fatalf("unexpected admitted operation count = %d", closed)
	}
}

func TestRunPreparedClosureResumeNeverReadmitsButCannotInventNoReplyEvidence(t *testing.T) {
	consumer, authority, runtime := closureFixture(t)
	prepared, err := consumer.PrepareClosure(context.Background(), authority, closureInput())
	if err != nil {
		t.Fatal(err)
	}
	record, blob, err := loadOperation(context.Background(), consumer.Blobs, prepared.OperationKeys[0])
	if err != nil {
		t.Fatal(err)
	}
	record.Status = OperationDispatching
	record.DispatchToken = strings.Repeat("f", 64)
	if _, err := commitOperation(context.Background(), consumer.Blobs, prepared.OperationKeys[0], blob, record); err != nil {
		t.Fatal(err)
	}
	outcome, err := consumer.RunPreparedClosure(context.Background(), authority, prepared, time.Second, 2*time.Second)
	if err == nil || outcome.AttemptedLabels != nil || outcome.CanceledOperationKeys != nil ||
		outcome.UniqueFRPSSelectors != nil || outcome.AdmissionSucceeded {
		t.Fatalf("resumed closure invented evidence = %#v, %v", outcome, err)
	}
	if runtime.admits != 3 || runtime.recovers != 4 {
		t.Fatalf("resume emitted a second admission: %#v", runtime)
	}
}

func TestClosureRunnerLossAdvancesToPrecommittedSuccessorAttempt(t *testing.T) {
	consumer, authority, runtime := closureFixture(t)
	first, err := consumer.PrepareClosure(context.Background(), authority, closureInput())
	if err != nil {
		t.Fatal(err)
	}
	record, blob, err := loadOperation(context.Background(), consumer.Blobs, first.OperationKeys[0])
	if err != nil {
		t.Fatal(err)
	}
	record.Status = OperationDispatching
	record.DispatchToken = strings.Repeat("f", 64)
	if _, err := commitOperation(context.Background(), consumer.Blobs, first.OperationKeys[0], blob, record); err != nil {
		t.Fatal(err)
	}
	if _, err := consumer.RunPreparedClosure(context.Background(), authority, first, time.Second, 2*time.Second); err == nil {
		t.Fatal("runner-loss attempt invented no-reply evidence")
	}
	settlement, err := consumer.SettlePreparedClosure(context.Background(), first)
	if err != nil || settlement.Attempt != 1 || !settlement.RetryRequired ||
		len(settlement.OperationKeys) != 4 || len(settlement.TerminalStates) != 4 || len(settlement.EvidencePresent) != 4 ||
		settlement.EvidencePresent[0] || !settlement.EvidencePresent[1] || !settlement.EvidencePresent[2] || !settlement.EvidencePresent[3] {
		t.Fatalf("runner-loss settlement = %#v, %v", settlement, err)
	}

	secondInput := closureInput()
	secondInput.Attempt = 2
	secondInput.RunIDs = [4]string{"4123456789abcdef", "5123456789abcdef", "6123456789abcdef", "7123456789abcdef"}
	secondInput.PreparedAt = secondInput.PreparedAt.Add(time.Minute)
	secondInput.ExpiresAt = secondInput.ExpiresAt.Add(time.Minute)
	secondInput.Previous = &first
	runtime.intentKey = "releases/" + secondInput.ReleaseID + "/closure-intents/" + secondInput.Phase + "/" + secondInput.Color + "/attempt-2"
	second, err := consumer.PrepareClosure(context.Background(), authority, secondInput)
	if err != nil {
		t.Fatalf("prepare successor: %v", err)
	}
	if second.Intent.Attempt != 2 || second.Intent.Previous == nil || second.Intent.Previous.Attempt != 1 ||
		second.Intent.Previous.Intent != first.IntentReference || second.Intent.Previous.OperationKeys != first.OperationKeys {
		t.Fatalf("successor ledger = %#v", second.Intent.Previous)
	}
	for _, oldKey := range first.OperationKeys {
		for _, newKey := range second.OperationKeys {
			if oldKey == newKey {
				t.Fatalf("successor reused operation %q", oldKey)
			}
		}
	}
	outcome, err := consumer.RunPreparedClosure(context.Background(), authority, second, time.Second, 2*time.Second)
	if err != nil || len(outcome.Evidence) != 4 || len(outcome.CanceledOperationKeys) != 4 {
		t.Fatalf("successor outcome = %#v, %v", outcome, err)
	}
	if runtime.admits != 7 || runtime.recovers != 8 {
		t.Fatalf("bounded successor runtime = %#v", runtime)
	}
	for _, key := range first.OperationKeys {
		old, _, loadErr := loadOperation(context.Background(), consumer.Blobs, key)
		if loadErr != nil || old.Status != OperationCanceled {
			t.Fatalf("old terminal authority %q = %#v, %v", key, old, loadErr)
		}
	}
}

func TestClosureEvidenceSurvivesCrashBeforeRecoveryAndAggregateReport(t *testing.T) {
	consumer, authority, runtime := closureFixture(t)
	prepared, err := consumer.PrepareClosure(context.Background(), authority, closureInput())
	if err != nil {
		t.Fatal(err)
	}
	record, blob, err := loadOperation(context.Background(), consumer.Blobs, prepared.OperationKeys[0])
	if err != nil {
		t.Fatal(err)
	}
	record.Status = OperationDispatching
	record.DispatchToken = strings.Repeat("e", 64)
	if _, err := commitOperation(context.Background(), consumer.Blobs, prepared.OperationKeys[0], blob, record); err != nil {
		t.Fatal(err)
	}
	noReply, ok := exactClosureNoReply(closureNoReplyError(t, runtime.admissionEndpoint), runtime.admissionEndpoint)
	if !ok {
		t.Fatal("fixture no-reply observation is not exact")
	}
	if _, err := consumer.persistClosureEvidence(context.Background(), &prepared, &record, noReply); err != nil {
		t.Fatal(err)
	}
	// This is the durable state after SIGKILL between evidence persistence and
	// recovery/report. The restarted invocation must recover without a new KNK.
	outcome, err := consumer.RunPreparedClosure(context.Background(), authority, prepared, time.Second, 2*time.Second)
	if err != nil || len(outcome.Evidence) != 4 || runtime.admits != 3 || runtime.recovers != 4 {
		t.Fatalf("evidence replay outcome=%#v runtime=%#v err=%v", outcome, runtime, err)
	}
}

func TestPrepareClosureRejectsUnsettledReuseAndAttemptsPastBound(t *testing.T) {
	consumer, authority, runtime := closureFixture(t)
	first, err := consumer.PrepareClosure(context.Background(), authority, closureInput())
	if err != nil {
		t.Fatal(err)
	}
	retry := closureInput()
	retry.Attempt = 2
	retry.Previous = &first
	retry.RunIDs = [4]string{"4123456789abcdef", "5123456789abcdef", "6123456789abcdef", "7123456789abcdef"}
	runtime.intentKey = "releases/" + retry.ReleaseID + "/closure-intents/" + retry.Phase + "/" + retry.Color + "/attempt-2"
	if _, err := consumer.PrepareClosure(context.Background(), authority, retry); err == nil {
		t.Fatal("successor prepared before predecessor terminal")
	}
	retry.Attempt = MaxClosureAttempts + 1
	retry.Previous = nil
	if _, err := consumer.PrepareClosure(context.Background(), authority, retry); err == nil {
		t.Fatal("closure attempt beyond bound accepted")
	}
}

func TestRunPreparedClosureRejectsNonWireFailuresAndWrongEndpointAfterCleanup(t *testing.T) {
	input := closureInput()
	expectedEndpoint := net.JoinHostPort(input.AdmissionEndpoint.Host, "443")
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "generic local", err: errors.New("local state failed")},
		{name: "deadline before evidence", err: context.DeadlineExceeded},
		{name: "DNS", err: fmt.Errorf("lookup: %w", nativeudp.ErrResolve)},
		{name: "dial", err: fmt.Errorf("dial: %w", nativeudp.ErrTransport)},
		{name: "state", err: qurl.ErrAgentStateContinuity},
		{name: "wrong endpoint", err: closureNoReplyError(t, "other.sandbox.example:443")},
		{name: "empty elapsed", err: &qurl.EndpointNoReplyError{Endpoint: expectedEndpoint, Attempts: 1,
			Last: fmt.Errorf("%w: %w", nativeudp.ErrInitialKnockNoReply, nativeudp.ErrNoReply)}},
		{name: "fabricated public markers", err: &qurl.EndpointNoReplyError{Endpoint: expectedEndpoint, Attempts: 1,
			Elapsed: time.Millisecond, Last: fmt.Errorf("%w: %w", nativeudp.ErrInitialKnockNoReply, nativeudp.ErrNoReply)}},
		{name: "wrapped sealed result", err: &qurl.EndpointNoReplyError{Endpoint: expectedEndpoint, Attempts: 1,
			Elapsed: time.Millisecond, Last: fmt.Errorf("wrapped: %w", genuineInitialNoReply(t))}},
	} {
		t.Run(test.name, func(t *testing.T) {
			consumer, authority, runtime := closureFixture(t)
			runtime.admitErr = test.err
			prepared, err := consumer.PrepareClosure(context.Background(), authority, closureInput())
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := consumer.RunPreparedClosure(context.Background(), authority, prepared, time.Second, 2*time.Second)
			if err == nil || outcome.AttemptedLabels != nil || outcome.CanceledOperationKeys != nil ||
				outcome.UniqueFRPSSelectors != nil || outcome.AdmissionSucceeded || runtime.recovers != 4 || runtime.retires != 0 {
				t.Fatalf("non-wire failure outcome=%#v runtime=%#v err=%v", outcome, runtime, err)
			}
			for _, key := range prepared.OperationKeys {
				record, _, loadErr := loadOperation(context.Background(), consumer.Blobs, key)
				if loadErr != nil || record.Status != OperationCanceled {
					t.Fatalf("cleanup record %q = %#v, %v", key, record, loadErr)
				}
			}
		})
	}
}

func TestPrepareClosureRejectsMutationUnionBeforeRuntime(t *testing.T) {
	mutations := []func(*ClosureIntentInput){
		func(input *ClosureIntentInput) { input.RunIDs[3] = input.RunIDs[0] },
		func(input *ClosureIntentInput) { input.Color = "legacy" },
		func(input *ClosureIntentInput) { input.AdmissionEndpoint.Port = 62206 },
		func(input *ClosureIntentInput) { input.AdmissionEndpoint = input.RecoveryEndpoint },
		func(input *ClosureIntentInput) { input.RecoveryEndpoint.Port = 62206 },
		func(input *ClosureIntentInput) { input.ExpiresAt = input.PreparedAt.Add(31 * time.Minute) },
	}
	for index, mutate := range mutations {
		t.Run(string(rune('a'+index)), func(t *testing.T) {
			consumer, authority, runtime := closureFixture(t)
			input := closureInput()
			mutate(&input)
			if _, err := consumer.PrepareClosure(context.Background(), authority, input); err == nil {
				t.Fatal("mutation accepted")
			}
			if runtime.prepares != 0 || runtime.admits != 0 || runtime.recovers != 0 {
				t.Fatalf("mutation reached runtime: %#v", runtime)
			}
		})
	}
}

func TestAuthorityRejectsWrongDuplicateOrIncompleteSelectorProjection(t *testing.T) {
	mutations := []func(*Plan){
		func(plan *Plan) { plan.Identities[0].Selector.ResourceID = "qurl-tunnel-server-c" },
		func(plan *Plan) { plan.Identities[3].Selector.Port++ },
		func(plan *Plan) { plan.Identities[1].Selector.Host = "other.sandbox.layerv.xyz" },
		func(plan *Plan) { plan.Identities[2].Selector.Port = plan.Identities[1].Selector.Port },
	}
	for index, mutate := range mutations {
		plan := validPlan("9")
		mutate(&plan)
		if err := ValidatePlan(plan); err == nil {
			t.Fatalf("selector mutation %d accepted", index)
		}
	}
}

type closureRuntime struct {
	mu                    sync.Mutex
	blobs                 BlobAuthority
	intentKey             string
	prepares              int
	admits                int
	recovers              int
	retires               int
	evidenceBeforeRecover int
	admitIndex            int
	admitErr              error
	noReplyLast           error
	admissionEndpoint     string
	intentVisible         []string
}

func (r *closureRuntime) Prepare(ctx context.Context, store qurl.AgentStateStore, request PrepareOperationRequest) (*qurl.NativeSessionOperation, error) { //nolint:gocritic // Fixture observes immutable intent ordering.
	r.mu.Lock()
	r.prepares++
	r.intentVisible = append(r.intentVisible, r.intentKey)
	r.mu.Unlock()
	if _, err := r.blobs.Load(ctx, r.intentKey); err != nil {
		return nil, errors.New("closure intent was not durable before operation preparation")
	}
	return (qurlSessionRuntime{}).Prepare(ctx, store, request)
}

func (r *closureRuntime) Admit(_ context.Context, _ qurl.AgentStateStore, record OperationRecord) (*LiveSession, SessionAdmission, error) { //nolint:gocritic // Fixture selects one optional admitted operation.
	r.mu.Lock()
	r.admits++
	index := r.admits
	admitted := r.admitIndex == index
	r.mu.Unlock()
	if admitted {
		admission := SessionAdmission{CellID: record.Operation.CellID, SessionID: uint64(index),
			SessionIssuedAtMillis: record.Operation.PreparedAtMillis, RunID: record.Operation.RunID, RunAttempt: record.Operation.RunAttempt}
		return &LiveSession{ACToken: "secret", ResourceHost: "127.0.0.1:9000", OperationID: record.Operation.OperationID, value: record.Operation.OperationID}, admission, nil
	}
	if r.admitErr != nil {
		return nil, SessionAdmission{}, r.admitErr
	}
	return nil, SessionAdmission{}, &qurl.EndpointNoReplyError{Endpoint: r.admissionEndpoint, Attempts: 1,
		Elapsed: time.Millisecond, Last: r.noReplyLast}
}

func (r *closureRuntime) Retire(_ context.Context, record OperationRecord, live *LiveSession) (SessionTerminal, error) { //nolint:gocritic // Fixture exact-closes one unexpected admission.
	r.mu.Lock()
	r.retires++
	r.mu.Unlock()
	return SessionTerminal{State: OperationClosed, WasAdmitted: true, CellID: record.Operation.CellID,
		SessionID: record.Admission.SessionID, SessionIssuedAtMillis: record.Admission.SessionIssuedAtMillis,
		RunID: record.Operation.RunID, RunAttempt: record.Operation.RunAttempt, CloseEventID: "close-event"}, nil
}

func (r *closureRuntime) Recover(_ context.Context, _ qurl.AgentStateStore, record OperationRecord) (SessionTerminal, error) { //nolint:gocritic // Fixture returns the absent-operation tombstone.
	r.mu.Lock()
	r.recovers++
	evidenceKey := fmt.Sprintf("releases/%s/closure-evidence/%s/%s/attempt-%d/%s", record.ReleaseID, record.Phase,
		record.Color, record.Operation.RunAttempt, record.Label)
	if r.blobs != nil {
		if _, err := r.blobs.Load(context.Background(), evidenceKey); err == nil {
			r.evidenceBeforeRecover++
		}
	}
	r.mu.Unlock()
	return SessionTerminal{State: OperationCanceled, WasAdmitted: false}, nil
}

func closureFixture(t *testing.T) (*Consumer, Authority, *closureRuntime) {
	t.Helper()
	consumer, authority, _ := lifecycleFixture(t)
	input := closureInput()
	runtime := &closureRuntime{blobs: consumer.Blobs,
		intentKey:         "releases/" + input.ReleaseID + "/closure-intents/" + input.Phase + "/" + input.Color + "/attempt-1",
		admissionEndpoint: net.JoinHostPort(input.AdmissionEndpoint.Host, "443"), noReplyLast: genuineInitialNoReply(t)}
	consumer.Runtime = runtime
	return consumer, authority, runtime
}

func closureInput() ClosureIntentInput {
	prepared := time.Unix(1_800_000_000, 0).UTC()
	return ClosureIntentInput{ReleaseID: strings.Repeat("e", 64), Phase: "active-blue-closure", Attempt: 1, Color: ColorBlue,
		AdmissionEndpoint: qurl.NHPUDPEndpoint{Host: "blue-canonical.sandbox.example", Port: 443,
			ServerPublicKeyB64: "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXpBQkNERUY="},
		RecoveryEndpoint: qurl.NHPUDPEndpoint{Host: "blue-recovery.sandbox.example", Port: 443,
			ServerPublicKeyB64: "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXpBQkNERUY="},
		RunIDs:     [4]string{"0123456789abcdef", "1123456789abcdef", "2123456789abcdef", "3123456789abcdef"},
		PreparedAt: prepared, ExpiresAt: prepared.Add(20 * time.Minute)}
}

func closureNoReplyError(t *testing.T, endpoint string) error {
	t.Helper()
	return &qurl.EndpointNoReplyError{Endpoint: endpoint, Attempts: 1, Elapsed: time.Millisecond,
		Last: genuineInitialNoReply(t)}
}

var closureNoReplyOnce = sync.OnceValues(func() (error, error) {
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	defer func() { _ = listener.Close() }()
	deviceKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	serverKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, observed := nativeudp.KnockWithReknock(ctx, nativeudp.Endpoint{Host: "closure.sandbox.layerv.ai", Port: 443,
		ServerStaticPub: serverKey.PublicKey().Bytes()}, []byte(`{"headerType":1}`), []byte(`{"headerType":2}`),
		nativeudp.Options{DeviceStaticPriv: deviceKey.Bytes(), Resolver: closurePublicResolver{},
			Dialer: closureRedirectDialer{target: listener.LocalAddr().String()}, Timeout: 10 * time.Millisecond, MaxAddresses: 1})
	if observed == nil {
		return nil, errors.New("silent transport returned no error")
	}
	if !nativeudp.IsInitialKnockNoReply(observed) {
		return nil, fmt.Errorf("silent transport result: %w", observed)
	}
	return observed, nil
})

func genuineInitialNoReply(t *testing.T) error {
	t.Helper()
	observed, err := closureNoReplyOnce()
	if err != nil {
		t.Fatalf("create sealed no-reply observation: %v", err)
	}
	return observed
}

type closurePublicResolver struct{}

func (closurePublicResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
}

type closureRedirectDialer struct{ target string }

func (d closureRedirectDialer) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, d.target)
}
