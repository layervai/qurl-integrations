package matchedcohort

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	qurl "github.com/layervai/qurl-go/qurl"
)

func TestConsumerPrepareCommitsExactOperationBeforeAdmission(t *testing.T) {
	ctx := context.Background()
	consumer, authority, request, runtime := sessionFixture(t)
	key, record, err := consumer.Prepare(ctx, authority, request)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if runtime.prepares != 1 || runtime.admits != 0 || record.Status != OperationPrepared || key != operationRecordKey(record) {
		t.Fatalf("prepare result key=%q record=%#v runtime=%#v", key, record, runtime)
	}
	loaded, _, err := loadOperation(ctx, consumer.Blobs, key)
	if err != nil || loaded.Operation.OperationID != record.Operation.OperationID {
		t.Fatalf("durable operation = %#v, %v", loaded, err)
	}
	if strings.Contains(string(mustJSON(t, loaded)), "device-secret") || strings.Contains(string(mustJSON(t, loaded)), "api_key") {
		t.Fatal("operation record contains credential material")
	}
}

func TestQURLSessionRuntimePreparesOfflineFromDurableState(t *testing.T) {
	ctx := context.Background()
	private, err := ecdh.X25519().GenerateKey(bytes.NewReader(bytes.Repeat([]byte{0x61}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Round(time.Second)
	publicKey := base64.StdEncoding.EncodeToString(private.PublicKey().Bytes())
	state := &qurl.AgentState{AgentID: "fixed-shared-direct-a", PrivateKeyB64: base64.StdEncoding.EncodeToString(private.Bytes()),
		PublicKeyB64: publicKey, RegisteredAt: &now, SchemaVersion: 7,
		DeviceAPIKey: "lv_live_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8", DeviceAPIKeyID: "key_AbCdEf123456",
		Assignment: &qurl.AgentAssignment{CellID: "cell-01", AssignmentGeneration: 7, EndpointRevision: 1,
			LeaseExpiresAt: now.Add(time.Hour), Endpoint: qurl.NHPUDPEndpoint{Host: "shared.sandbox.layerv.xyz", Port: 443,
				ServerPublicKeyB64: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x52}, 32))}}}
	blobs := newMemoryBlobs()
	store, _ := NewDurableAgentStateStore(blobs, "actual/offline/agent-state")
	if err := store.SaveAgentState(ctx, state); err != nil {
		t.Fatal(err)
	}
	request := PrepareOperationRequest{AWSAccountID: "111122223333", AWSRegion: "us-east-2",
		Identity: FixedIdentity{AgentID: state.AgentID, OwnerID: "auth0|canary-owner", KnockResourceID: "resource-a"},
		Cohort:   CohortPlan{CellID: "cell-01", SessionControlTable: "sandbox-session-control", QURLAgentKeysTable: "control-agent-keys"},
		RunID:    "0123456789abcdef", RunAttempt: 7, PreparedAt: now, ExpiresAt: now.Add(20 * time.Minute)}
	operation, err := (qurlSessionRuntime{}).Prepare(ctx, store, request)
	if err != nil {
		t.Fatalf("offline Prepare: %v", err)
	}
	if operation.AgentID != state.AgentID || operation.AgentPublicKeyB64 != publicKey || operation.ResourceID != "resource-a" ||
		operation.OwnerID != request.Identity.OwnerID || operation.OperationID == "" || operation.BindingSHA256 == "" {
		t.Fatalf("offline operation = %#v", operation)
	}
}

func TestConsumerHealthyAdmissionAndExactRetirement(t *testing.T) {
	ctx := context.Background()
	consumer, authority, request, runtime := sessionFixture(t)
	key, _, err := consumer.Prepare(ctx, authority, request)
	if err != nil {
		t.Fatal(err)
	}
	live, err := consumer.Admit(ctx, key)
	if err != nil || live == nil || live.ACToken == "" {
		t.Fatalf("Admit: %#v, %v", live, err)
	}
	mapped, _, err := loadOperation(ctx, consumer.Blobs, key)
	if err != nil || mapped.Status != OperationMapped || mapped.Admission == nil {
		t.Fatalf("mapped record = %#v, %v", mapped, err)
	}
	terminal, err := consumer.Retire(ctx, key, live)
	if err != nil || terminal.State != OperationClosed || !terminal.WasAdmitted {
		t.Fatalf("Retire = %#v, %v", terminal, err)
	}
	closed, _, _ := loadOperation(ctx, consumer.Blobs, key)
	if closed.Status != OperationClosed || closed.Terminal == nil || runtime.admits != 1 || runtime.retires != 1 || runtime.recovers != 0 {
		t.Fatalf("closed record/runtime = %#v %#v", closed, runtime)
	}
	if live.ACToken != "" {
		t.Fatal("retired live session retained ACToken")
	}
}

func TestConsumerRecoveryFirstCommitsCanceledReceiptAndReplaysWithoutNetwork(t *testing.T) {
	ctx := context.Background()
	consumer, authority, request, runtime := sessionFixture(t)
	receipt, err := consumer.RecoverPrepared(ctx, authority, request)
	if err != nil || receipt.Terminal.State != OperationCanceled || receipt.Terminal.WasAdmitted ||
		receipt.OperationKey == "" || receipt.Record.Key != receipt.OperationKey || receipt.Record.VersionID == "" ||
		receipt.Record.SHA256 == "" || receipt.OperationID != runtime.operation.OperationID ||
		receipt.BindingSHA256 != runtime.operation.BindingSHA256 {
		t.Fatalf("recovery-first receipt = %#v, %v", receipt, err)
	}
	if runtime.prepares != 1 || runtime.admits != 0 || runtime.retires != 0 || runtime.recovers != 1 {
		t.Fatalf("recovery-first network calls = %#v", runtime)
	}
	replayed, err := consumer.RecoverPrepared(ctx, authority, request)
	if err != nil || replayed != receipt {
		t.Fatalf("recovery-first replay = %#v, %v", replayed, err)
	}
	if runtime.prepares != 2 || runtime.admits != 0 || runtime.retires != 0 || runtime.recovers != 1 {
		t.Fatalf("recovery-first replay emitted network = %#v", runtime)
	}
}

func TestConsumerSharedRecoveryFirstRetainsCanceledAndReplaysWithoutNetwork(t *testing.T) {
	ctx := context.Background()
	consumer, authority, request, runtime := sessionFixture(t)
	receipt, err := consumer.RecoverPrepared(ctx, authority, request)
	if err != nil || receipt.Terminal.State != OperationCanceled || receipt.Terminal.WasAdmitted ||
		!strings.Contains(receipt.OperationKey, "/shared/direct-a/") {
		t.Fatalf("shared recovery-first receipt = %#v, %v", receipt, err)
	}
	record, _, loadErr := loadOperation(ctx, consumer.Blobs, receipt.OperationKey)
	if loadErr != nil || record.AuthoritySHA256 == "" || record.Status != OperationCanceled {
		t.Fatalf("shared terminal record = %#v, %v", record, loadErr)
	}
	replayed, err := consumer.RecoverPrepared(ctx, authority, request)
	if err != nil || replayed != receipt || runtime.recovers != 1 {
		t.Fatalf("shared recovery replay = %#v runtime=%#v err=%v", replayed, runtime, err)
	}
}

func TestConsumerRejectsWrongPhaseAndMovedOperationBeforeRuntime(t *testing.T) {
	consumer, authority, request, runtime := sessionFixture(t)
	request.Phase = "candidate-direct"
	if _, _, err := consumer.Prepare(context.Background(), authority, request); err == nil || runtime.prepares != 0 {
		t.Fatalf("wrong phase reached runtime: %v %#v", err, runtime)
	}
	request.Phase = "fixed_shared_recovery_first"
	key, _, err := consumer.Prepare(context.Background(), authority, request)
	if err != nil {
		t.Fatal(err)
	}
	blobs := consumer.Blobs.(*memoryBlobs)
	blob, err := blobs.Load(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	moved := key + "-moved"
	blob.Key = moved
	blobs.mu.Lock()
	blobs.values[moved] = blob
	blobs.mu.Unlock()
	if _, err := consumer.Recover(context.Background(), moved); err == nil || runtime.recovers != 0 {
		t.Fatalf("moved operation reached recovery runtime: %v %#v", err, runtime)
	}
}

func TestConsumerRejectsRecoveryRouteDriftBeforeRuntime(t *testing.T) {
	for _, field := range []string{"host", "key"} {
		t.Run(field, func(t *testing.T) {
			consumer, authority, request, runtime := sessionFixture(t)
			if field == "host" {
				request.RecoveryEndpoint.Host = "other-recovery.sandbox.layerv.xyz"
			} else {
				request.RecoveryEndpoint.ServerPublicKeyB64 = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x53}, 32))
			}
			if _, _, err := consumer.Prepare(context.Background(), authority, request); err == nil || runtime.prepares != 0 {
				t.Fatalf("recovery route drift reached runtime: %v %#v", err, runtime)
			}
		})
	}
}

func TestConsumerRejectsPreparedOperationTimeDriftBeforeCommitOrRecovery(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*qurl.NativeSessionOperation)
	}{
		{name: "prepared-before", mutate: func(operation *qurl.NativeSessionOperation) { operation.PreparedAtMillis-- }},
		{name: "prepared-after", mutate: func(operation *qurl.NativeSessionOperation) { operation.PreparedAtMillis++ }},
		{name: "expires-before", mutate: func(operation *qurl.NativeSessionOperation) { operation.ExpiresAtMillis-- }},
		{name: "expires-after", mutate: func(operation *qurl.NativeSessionOperation) { operation.ExpiresAtMillis++ }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			consumer, authority, request, runtime := sessionFixture(t)
			mutation.mutate(&runtime.operation)
			blobs := consumer.Blobs.(*memoryBlobs)
			blobs.mu.Lock()
			before := len(blobs.values)
			blobs.mu.Unlock()
			if _, _, err := consumer.Prepare(context.Background(), authority, request); err == nil {
				t.Fatal("prepared operation time drift accepted")
			}
			blobs.mu.Lock()
			after := len(blobs.values)
			blobs.mu.Unlock()
			if runtime.prepares != 1 || runtime.recovers != 0 || after != before {
				t.Fatalf("time drift changed durable or recovery state: before=%d after=%d runtime=%#v", before, after, runtime)
			}
		})
	}
}

func TestConsumerRecoveryFirstResumesDispatchAndClassifiesLostCommit(t *testing.T) {
	ctx := context.Background()
	consumer, authority, request, runtime := sessionFixture(t)
	key, _, err := consumer.Prepare(ctx, authority, request)
	if err != nil {
		t.Fatal(err)
	}
	record, blob, _ := loadOperation(ctx, consumer.Blobs, key)
	record.Status = OperationDispatching
	record.DispatchToken = strings.Repeat("a", 64)
	if _, err := commitOperation(ctx, consumer.Blobs, key, blob, record); err != nil {
		t.Fatal(err)
	}
	blobs := consumer.Blobs.(*memoryBlobs)
	blobs.mu.Lock()
	blobs.failAfterCommit = true
	blobs.mu.Unlock()
	receipt, err := consumer.RecoverPrepared(ctx, authority, request)
	if err != nil || receipt.Terminal.State != OperationCanceled || runtime.admits != 0 || runtime.recovers != 1 {
		t.Fatalf("resumed recovery-first = %#v runtime=%#v err=%v", receipt, runtime, err)
	}
	loaded, _, loadErr := loadOperation(ctx, consumer.Blobs, key)
	if loadErr != nil || loaded.Status != OperationCanceled || loaded.Terminal == nil {
		t.Fatalf("lost-response terminal = %#v, %v", loaded, loadErr)
	}
}

func TestConsumerRecoveryFirstRetriesAmbiguousNetworkOnlyUntilTerminalIsDurable(t *testing.T) {
	ctx := context.Background()
	consumer, authority, request, runtime := sessionFixture(t)
	runtime.recoveryErrors = []error{context.DeadlineExceeded}
	if receipt, err := consumer.RecoverPrepared(ctx, authority, request); !errors.Is(err, context.DeadlineExceeded) || receipt != (RecoveryFirstReceipt{}) {
		t.Fatalf("ambiguous recovery-first = %#v, %v", receipt, err)
	}
	record, _, loadErr := loadOperation(ctx, consumer.Blobs, operationRecordKey(OperationRecord{
		ReleaseID: request.ReleaseID, Phase: request.Phase,
		Label: request.Identity.Label, Operation: runtime.operation,
	}))
	if loadErr != nil || record.Status != OperationDispatching || record.Terminal != nil || runtime.recovers != 1 {
		t.Fatalf("ambiguous recovery state = %#v runtime=%#v err=%v", record, runtime, loadErr)
	}
	receipt, err := consumer.RecoverPrepared(ctx, authority, request)
	if err != nil || receipt.Terminal.State != OperationCanceled || runtime.recovers != 2 {
		t.Fatalf("recovered ambiguity = %#v runtime=%#v err=%v", receipt, runtime, err)
	}
	if replayed, replayErr := consumer.RecoverPrepared(ctx, authority, request); replayErr != nil || replayed != receipt || runtime.recovers != 2 {
		t.Fatalf("terminal replay = %#v runtime=%#v err=%v", replayed, runtime, replayErr)
	}
}

func TestConsumerMappingPersistenceFailureDestroysLiveAuthorityAndRecovers(t *testing.T) {
	ctx := context.Background()
	consumer, authority, request, runtime := sessionFixture(t)
	blobs := consumer.Blobs.(*memoryBlobs)
	runtime.failMappedCommit = blobs
	runtime.recovery = []SessionTerminal{{State: OperationClosed, WasAdmitted: true, CellID: "cell-01", SessionID: 9,
		SessionIssuedAtMillis: 1700000000000, RunID: request.RunID, RunAttempt: request.RunAttempt, CloseEventID: "close-1"}}
	key, _, err := consumer.Prepare(ctx, authority, request)
	if err != nil {
		t.Fatal(err)
	}
	live, err := consumer.Admit(ctx, key)
	if live != nil || err == nil {
		t.Fatalf("mapping persistence failure = %#v, %v", live, err)
	}
	record, _, loadErr := loadOperation(ctx, consumer.Blobs, key)
	if loadErr != nil || record.Status != OperationClosed || runtime.retires != 1 || runtime.recovers != 1 || runtime.lastLive == nil || runtime.lastLive.ACToken != "" {
		t.Fatalf("mapping cleanup record=%#v runtime=%#v load=%v", record, runtime, loadErr)
	}
}

func TestDurableManagedAdmitterBridgesPreparedOperationAndExactRetirement(t *testing.T) {
	ctx := context.Background()
	consumer, authority, request, runtime := sessionFixture(t)
	key, record, err := consumer.Prepare(ctx, authority, request)
	if err != nil {
		t.Fatal(err)
	}
	admitter, err := newDurableManagedAdmitter(ctx, consumer, key, &authority.Identities[0])
	if err != nil {
		t.Fatal(err)
	}
	admission, err := admitter.Admit(ctx, record.Operation.ResourceID, authority.Identities[0].ResourceID)
	if err != nil || admission.Token == "" || admission.ResourceHost == "" || admission.RunID != record.Operation.RunID ||
		admission.RunAttempt != record.Operation.RunAttempt || admission.SessionID == 0 {
		t.Fatalf("Admit = %#v %v", admission, err)
	}
	if _, err := admitter.Admit(ctx, record.Operation.ResourceID, authority.Identities[0].ResourceID); err == nil {
		t.Fatal("duplicate Admit unexpectedly succeeded")
	}
	if err := admitter.Retire(ctx, admission); err != nil {
		t.Fatalf("Retire = %v", err)
	}
	closed, _, err := loadOperation(ctx, consumer.Blobs, key)
	if err != nil || closed.Status != OperationClosed || runtime.admits != 1 || runtime.retires != 1 {
		t.Fatalf("closed operation = %#v runtime=%#v err=%v", closed, runtime, err)
	}
}

func TestDurableManagedOperationRecoversUnsentOperationWithCanceledTombstone(t *testing.T) {
	ctx := context.Background()
	consumer, authority, request, runtime := sessionFixture(t)
	key, _, err := consumer.Prepare(ctx, authority, request)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := consumer.Recover(ctx, key)
	if err != nil || terminal.State != OperationCanceled {
		t.Fatalf("Recover before Admit = %#v %v", terminal, err)
	}
	canceled, _, err := loadOperation(ctx, consumer.Blobs, key)
	if err != nil || canceled.Status != OperationCanceled || runtime.admits != 0 || runtime.recovers != 1 {
		t.Fatalf("canceled operation = %#v runtime=%#v err=%v", canceled, runtime, err)
	}
}

func TestConsumerCrashAfterDispatchRecoversWithoutSecondAdmission(t *testing.T) {
	ctx := context.Background()
	consumer, authority, request, runtime := sessionFixture(t)
	key, _, err := consumer.Prepare(ctx, authority, request)
	if err != nil {
		t.Fatal(err)
	}
	record, blob, _ := loadOperation(ctx, consumer.Blobs, key)
	record.Status = OperationDispatching
	record.DispatchToken = strings.Repeat("d", 64)
	if _, err := commitOperation(ctx, consumer.Blobs, key, blob, record); err != nil {
		t.Fatal(err)
	}
	runtime.recovery = []SessionTerminal{{State: OperationCanceled, WasAdmitted: false}}
	if live, err := consumer.Admit(ctx, key); live != nil || !errors.Is(err, errSessionRecoveryRequired) {
		t.Fatalf("resumed Admit = %#v, %v", live, err)
	}
	terminal, _, _ := loadOperation(ctx, consumer.Blobs, key)
	if runtime.admits != 0 || runtime.recovers != 1 || terminal.Status != OperationCanceled {
		t.Fatalf("resume admitted or failed to cancel: runtime=%#v record=%#v", runtime, terminal)
	}
}

func TestConsumerConcurrentAdmitHasOneNetworkWinner(t *testing.T) {
	ctx := context.Background()
	consumer, authority, request, fixtureRuntime := sessionFixture(t)
	runtime := &contendedSessionRuntime{operation: fixtureRuntime.operation, started: make(chan struct{}), release: make(chan struct{})}
	consumer.Runtime = runtime
	key, _, err := consumer.Prepare(ctx, authority, request)
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() { _, err := consumer.Admit(ctx, key); first <- err }()
	<-runtime.started
	second := make(chan error, 1)
	go func() { _, err := consumer.Admit(ctx, key); second <- err }()
	if err := <-second; !errors.Is(err, errSessionRecoveryRequired) {
		t.Fatalf("contending Admit = %v", err)
	}
	close(runtime.release)
	if err := <-first; err == nil {
		t.Fatal("ambiguous first Admit unexpectedly succeeded after recovery won")
	}
	runtime.mu.Lock()
	admits, recovers := runtime.admits, runtime.recovers
	runtime.mu.Unlock()
	if admits != 1 || recovers != 1 {
		t.Fatalf("concurrent network calls admit=%d recover=%d", admits, recovers)
	}
}

func TestConsumerMappedCrashRecoveryRequiresClosedTerminal(t *testing.T) {
	ctx := context.Background()
	consumer, authority, request, runtime := sessionFixture(t)
	key, _, _ := consumer.Prepare(ctx, authority, request)
	record, blob, _ := loadOperation(ctx, consumer.Blobs, key)
	record.Status = OperationMapped
	record.DispatchToken = strings.Repeat("e", 64)
	record.Admission = &SessionAdmission{CellID: "cell-01", SessionID: 9, SessionIssuedAtMillis: 1700000000000,
		RunID: record.Operation.RunID, RunAttempt: record.Operation.RunAttempt}
	if _, err := commitOperation(ctx, consumer.Blobs, key, blob, record); err != nil {
		t.Fatal(err)
	}
	runtime.recovery = []SessionTerminal{
		{State: OperationClosing, WasAdmitted: true, CellID: "cell-01", SessionID: 9, CloseEventID: "close-1"},
		{State: OperationClosed, WasAdmitted: true, CellID: "cell-01", SessionID: 9, CloseEventID: "close-1"},
	}
	first, err := consumer.Recover(ctx, key)
	if err != nil || first.State != OperationClosing {
		t.Fatalf("first Recover = %#v, %v", first, err)
	}
	second, err := consumer.Recover(ctx, key)
	if err != nil || second.State != OperationClosed {
		t.Fatalf("second Recover = %#v, %v", second, err)
	}
	if runtime.admits != 0 || runtime.recovers != 2 {
		t.Fatalf("recovery runtime = %#v", runtime)
	}
}

func TestConsumerRejectsCrossAuthorityProjectionBeforePrepare(t *testing.T) {
	ctx := context.Background()
	consumer, authority, request, runtime := sessionFixture(t)
	request.Identity = authority.Identities[1]
	request.ExpectedAgentState = request.Identity.AgentState
	if _, _, err := consumer.Prepare(ctx, authority, request); err == nil {
		t.Fatal("identity/cohort projection drift accepted")
	}
	if runtime.prepares != 0 || runtime.admits != 0 {
		t.Fatalf("projection drift reached runtime: %#v", runtime)
	}
}

func TestOperationRecordRejectsAdmissionAndTerminalBindingDrift(t *testing.T) {
	consumer, authority, request, _ := sessionFixture(t)
	_, prepared, err := consumer.Prepare(context.Background(), authority, request)
	if err != nil {
		t.Fatal(err)
	}
	baseAdmission := SessionAdmission{CellID: prepared.Operation.CellID, SessionID: 9, SessionIssuedAtMillis: 1700000000000,
		RunID: prepared.Operation.RunID, RunAttempt: prepared.Operation.RunAttempt}
	baseTerminal := SessionTerminal{State: OperationClosed, WasAdmitted: true, CellID: prepared.Operation.CellID, SessionID: 9,
		SessionIssuedAtMillis: 1700000000000, RunID: prepared.Operation.RunID, RunAttempt: prepared.Operation.RunAttempt, CloseEventID: "close-1"}
	mutations := []func(*OperationRecord){
		func(record *OperationRecord) { record.Admission.CellID = "other-cell" },
		func(record *OperationRecord) { record.Admission.RunID = "fedcba9876543210" },
		func(record *OperationRecord) { record.Terminal.SessionID = 0 },
		func(record *OperationRecord) { record.Terminal.RunAttempt++ },
		func(record *OperationRecord) { record.Terminal.CloseEventID = "" },
	}
	for index, mutation := range mutations {
		record := prepared
		record.Status, record.DispatchToken = OperationClosed, strings.Repeat("f", 64)
		admission, terminal := baseAdmission, baseTerminal
		record.Admission, record.Terminal = &admission, &terminal
		mutation(&record)
		if validOperationRecord(record) {
			t.Fatalf("receipt binding mutation %d accepted", index)
		}
	}
	canceled := prepared
	canceled.Status, canceled.DispatchToken = OperationCanceled, strings.Repeat("f", 64)
	canceled.Terminal = &SessionTerminal{State: OperationCanceled, WasAdmitted: false, RunID: prepared.Operation.RunID}
	if validOperationRecord(canceled) {
		t.Fatal("canceled tombstone with session authority accepted")
	}
}

type fakeSessionRuntime struct {
	operation        qurl.NativeSessionOperation
	prepares         int
	admits           int
	retires          int
	recovers         int
	recovery         []SessionTerminal
	recoveryErrors   []error
	failMappedCommit *memoryBlobs
	lastLive         *LiveSession
	resourceHost     string
}

type contendedSessionRuntime struct {
	mu        sync.Mutex
	operation qurl.NativeSessionOperation
	started   chan struct{}
	release   chan struct{}
	admits    int
	recovers  int
}

func (r *contendedSessionRuntime) Prepare(context.Context, qurl.AgentStateStore, PrepareOperationRequest) (*qurl.NativeSessionOperation, error) {
	operation := r.operation
	return &operation, nil
}

func (r *contendedSessionRuntime) Admit(context.Context, qurl.AgentStateStore, OperationRecord) (*LiveSession, SessionAdmission, error) {
	r.mu.Lock()
	r.admits++
	r.mu.Unlock()
	close(r.started)
	<-r.release
	return nil, SessionAdmission{}, errors.New("test ambiguous admission")
}

func (*contendedSessionRuntime) Retire(context.Context, OperationRecord, *LiveSession) (SessionTerminal, error) {
	return SessionTerminal{}, errors.New("unexpected retire")
}

func (r *contendedSessionRuntime) Recover(context.Context, qurl.AgentStateStore, OperationRecord) (SessionTerminal, error) {
	r.mu.Lock()
	r.recovers++
	r.mu.Unlock()
	return SessionTerminal{State: OperationCanceled, WasAdmitted: false}, nil
}

func (f *fakeSessionRuntime) Prepare(context.Context, qurl.AgentStateStore, PrepareOperationRequest) (*qurl.NativeSessionOperation, error) {
	f.prepares++
	operation := f.operation
	return &operation, nil
}

func (f *fakeSessionRuntime) Admit(_ context.Context, _ qurl.AgentStateStore, record OperationRecord) (*LiveSession, SessionAdmission, error) { //nolint:gocritic // Implements the immutable production interface.
	f.admits++
	admission := SessionAdmission{CellID: record.Operation.CellID, SessionID: 9, SessionIssuedAtMillis: 1700000000000,
		RunID: record.Operation.RunID, RunAttempt: record.Operation.RunAttempt}
	receipt := qurl.NativeSessionReceipt{CellID: record.Operation.CellID, SessionID: 9,
		SessionIssuedAtMillis: 1700000000000, RunID: record.Operation.RunID, RunAttempt: record.Operation.RunAttempt}
	live := &LiveSession{ACToken: "short-lived-actoken", ResourceHost: f.resourceHost, OperationID: record.Operation.OperationID,
		OpenTime: time.Minute, receipt: receipt, value: struct{}{}}
	f.lastLive = live
	if f.failMappedCommit != nil {
		f.failMappedCommit.mu.Lock()
		f.failMappedCommit.failBeforeCommit = true
		f.failMappedCommit.mu.Unlock()
	}
	return live, admission, nil
}

func (f *fakeSessionRuntime) Retire(_ context.Context, record OperationRecord, live *LiveSession) (SessionTerminal, error) { //nolint:gocritic // Implements the immutable production interface.
	f.retires++
	live.ACToken = ""
	return SessionTerminal{State: OperationClosed, WasAdmitted: true, CellID: record.Operation.CellID, SessionID: 9,
		SessionIssuedAtMillis: 1700000000000, RunID: record.Operation.RunID, RunAttempt: record.Operation.RunAttempt,
		CloseEventID: "close-1"}, nil
}

func (f *fakeSessionRuntime) Recover(_ context.Context, _ qurl.AgentStateStore, record OperationRecord) (SessionTerminal, error) { //nolint:gocritic // Implements the immutable production interface.
	f.recovers++
	if len(f.recoveryErrors) != 0 {
		err := f.recoveryErrors[0]
		f.recoveryErrors = f.recoveryErrors[1:]
		return SessionTerminal{}, err
	}
	if len(f.recovery) == 0 {
		return SessionTerminal{State: OperationCanceled, WasAdmitted: false}, nil
	}
	result := f.recovery[0]
	f.recovery = f.recovery[1:]
	if result.RunID == "" && result.WasAdmitted {
		result.RunID = record.Operation.RunID
		result.RunAttempt = record.Operation.RunAttempt
		result.SessionIssuedAtMillis = 1700000000000
	}
	return result, nil
}

func sessionFixture(t *testing.T) (*Consumer, Authority, PrepareOperationRequest, *fakeSessionRuntime) {
	t.Helper()
	ctx := context.Background()
	blobs := newMemoryBlobs()
	plan := validPlan("6")
	stateStore, _ := NewDurableAgentStateStore(blobs, "generations/"+plan.GenerationID+"/shared/direct-a/agent-state")
	if err := stateStore.SaveAgentState(ctx, testAgentState("agent-a")); err != nil {
		t.Fatal(err)
	}
	stateRef, _ := stateStore.Reference(ctx)
	publicKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	operationJSON := `{"agent_id":"agent-a","agent_key_schema_version":2,"agent_public_key_b64":"` + publicKey +
		`","auth_service_id":"agent","aws_account_id":"111122223333","aws_region":"us-east-2","binding_schema":1,"binding_sha256":"73add3ded83c588697131214c3e362ecc651512afa9c2ff4bad7d790a43593d8","cell_id":"cell-01","connector_id_claim":"","enrollment_credential_kind":"account","expires_at_ms":1800001210000,"operation_id":"3b2a3a9eabea3af78d8c317ea710e7f0601580163e25c98d50d5e2e17b68f3cc","owner_id":"auth0|canary-owner","prepared_at_ms":1800000009000,"qurl_agent_keys_table":"control-agent-keys","resource_id":"resource-a","run_attempt":7,"run_id":"0123456789abcdef","schema":1,"session_control_table":"sandbox-session-control"}`
	var operation qurl.NativeSessionOperation
	if err := json.Unmarshal([]byte(operationJSON), &operation); err != nil {
		t.Fatal(err)
	}
	plan.AWSAccountID, plan.AWSRegion = operation.AWSAccountID, operation.AWSRegion
	plan.OwnerSubject = operation.OwnerID
	for index := range plan.Identities {
		plan.Identities[index].OwnerID = operation.OwnerID
	}
	plan.Cohorts[0].CellID, plan.Cohorts[0].SessionControlTable, plan.Cohorts[0].QURLAgentKeysTable = operation.CellID, operation.SessionControlTable, operation.QURLAgentKeysTable
	authority := Authority{Schema: plan.Schema, Environment: plan.Environment, GenerationID: plan.GenerationID,
		OwnerSubject: plan.OwnerSubject, AWSAccountID: plan.AWSAccountID, AWSRegion: plan.AWSRegion, NHPSourceSHA: plan.NHPSourceSHA,
		QURLGoSourceSHA:             plan.QURLGoSourceSHA,
		EnrollmentCredentialReceipt: StateReference{Key: "generations/" + plan.GenerationID + "/enrollment-credential-receipt", VersionID: "version", SHA256: strings.Repeat("d", 64)}, Cohorts: plan.Cohorts}
	for index, identityPlan := range plan.Identities {
		identity := FixedIdentity{Label: identityPlan.Label, OwnerID: identityPlan.OwnerID,
			AgentID: identityPlan.AgentID, AgentPublicKeyB64: publicKey, AgentKeySchemaVersion: 2,
			EnrollmentCredentialKind: "account", DeviceAPIKeyID: "device-" + identityPlan.AgentID,
			ConnectorID: identityPlan.ConnectorID, ResourceID: "resource-" + identityPlan.AgentID, CRID: "crid-" + identityPlan.AgentID,
			ConnectorRoutingID: "route-" + identityPlan.AgentID, KnockResourceID: "knock-" + identityPlan.AgentID,
			Selector: identityPlan.Selector,
			AgentState: StateReference{Key: "generations/" + plan.GenerationID + "/shared/" + identityPlan.Label + "/agent-state",
				VersionID: "version", SHA256: strings.Repeat("a", 64)},
			ConnectorState: StateReference{Key: "generations/" + plan.GenerationID + "/shared/" + identityPlan.Label + "/connector-state",
				VersionID: "version", SHA256: strings.Repeat("b", 64)}}
		if index == 0 {
			identity.AgentID, identity.OwnerID, identity.AgentPublicKeyB64, identity.KnockResourceID, identity.AgentState = operation.AgentID, operation.OwnerID, operation.AgentPublicKeyB64, operation.ResourceID, stateRef
		}
		authority.Identities = append(authority.Identities, identity)
	}
	if err := ValidateAuthority(authority); err != nil {
		t.Fatalf("fixture authority: %v", err)
	}
	selector := authority.Identities[0].Selector
	runtime := &fakeSessionRuntime{operation: operation, resourceHost: net.JoinHostPort(selector.Host, strconv.Itoa(selector.Port))}
	request := PrepareOperationRequest{ReleaseID: strings.Repeat("c", 64), Phase: "fixed_shared_recovery_first",
		AWSAccountID: authority.AWSAccountID, AWSRegion: authority.AWSRegion,
		Identity: authority.Identities[0], Cohort: authority.Cohorts[0], ExpectedAgentState: stateRef,
		RecoveryEndpoint: authority.Cohorts[0].CellEndpoint,
		RunID:            operation.RunID, RunAttempt: operation.RunAttempt, PreparedAt: time.UnixMilli(operation.PreparedAtMillis), ExpiresAt: time.UnixMilli(operation.ExpiresAtMillis)}
	return &Consumer{Blobs: blobs, Runtime: runtime}, authority, request, runtime
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
