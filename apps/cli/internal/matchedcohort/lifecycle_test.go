package matchedcohort

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	qurl "github.com/layervai/qurl-go/qurl"
)

func TestPrepareLifecycleCommitsIntentAndThreeOperationsBeforeReturn(t *testing.T) {
	consumer, authority, runtime := lifecycleFixture(t)
	input := lifecycleInput()
	prepared, err := consumer.PrepareLifecycle(context.Background(), authority, input)
	if err != nil {
		t.Fatalf("PrepareLifecycle: %v", err)
	}
	if runtime.prepares != 3 || prepared.IntentReference.Key == "" || prepared.PrimaryFirstKey == "" ||
		prepared.SiblingKey == "" || prepared.ReplacementKey == "" {
		t.Fatalf("prepared = %#v, runtime=%#v", prepared, runtime)
	}
	if err := consumer.ValidatePreparedLifecycle(context.Background(), prepared); err != nil {
		t.Fatalf("ValidatePreparedLifecycle: %v", err)
	}
	for index, key := range []string{prepared.PrimaryFirstKey, prepared.SiblingKey, prepared.ReplacementKey} {
		record, _, loadErr := loadOperation(context.Background(), consumer.Blobs, key)
		if loadErr != nil || record.Status != OperationPrepared || record.Operation.RunID != input.RunIDs[index] ||
			record.Operation.RunAttempt != input.Attempt || runtime.intentVisible[index] != prepared.IntentReference.Key {
			t.Fatalf("operation %d = %#v, visible=%q, err=%v", index, record, runtime.intentVisible[index], loadErr)
		}
	}
	// Exact replay performs offline reconstruction but does not create new
	// operation authority or change the immutable intent reference.
	replayed, err := consumer.PrepareLifecycle(context.Background(), authority, input)
	preparedRaw, _ := CanonicalJSON(prepared)
	replayedRaw, _ := CanonicalJSON(replayed)
	if err != nil || !bytes.Equal(replayedRaw, preparedRaw) {
		t.Fatalf("replay = %#v, %v", replayed, err)
	}
}

func TestPrepareLifecycleResumeAfterPartialOfflinePreparation(t *testing.T) {
	consumer, authority, runtime := lifecycleFixture(t)
	runtime.failPrepareAt = 3
	input := lifecycleInput()
	if _, err := consumer.PrepareLifecycle(context.Background(), authority, input); err == nil {
		t.Fatal("partial preparation unexpectedly succeeded")
	}
	runtime.failPrepareAt = 0
	prepared, err := consumer.PrepareLifecycle(context.Background(), authority, input)
	if err != nil {
		t.Fatalf("resumed PrepareLifecycle: %v", err)
	}
	if err := consumer.ValidatePreparedLifecycle(context.Background(), prepared); err != nil {
		t.Fatalf("resumed bundle: %v", err)
	}
	if runtime.admits != 0 {
		t.Fatalf("offline lifecycle performed %d admissions", runtime.admits)
	}
}

func TestPrepareLifecycleRejectsAuthorityAndIntentMutationUnion(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*LifecycleIntentInput)
	}{
		{"duplicate RunID", func(input *LifecycleIntentInput) { input.RunIDs[2] = input.RunIDs[0] }},
		{"unknown transport", func(input *LifecycleIntentInput) { input.Transport = "other" }},
		{"wrong recovery port", func(input *LifecycleIntentInput) { input.RecoveryEndpoint.Port = 62206 }},
		{"long admission window", func(input *LifecycleIntentInput) { input.ExpiresAt = input.PreparedAt.Add(31 * time.Minute) }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			consumer, authority, runtime := lifecycleFixture(t)
			input := lifecycleInput()
			mutation.mutate(&input)
			if _, err := consumer.PrepareLifecycle(context.Background(), authority, input); err == nil {
				t.Fatal("mutation accepted")
			}
			if runtime.prepares != 0 || runtime.admits != 0 {
				t.Fatalf("mutation reached runtime: %#v", runtime)
			}
		})
	}
}

func TestValidatePreparedLifecycleRejectsTransitionBeforeNetworkStart(t *testing.T) {
	consumer, authority, _ := lifecycleFixture(t)
	prepared, err := consumer.PrepareLifecycle(context.Background(), authority, lifecycleInput())
	if err != nil {
		t.Fatal(err)
	}
	record, blob, err := loadOperation(context.Background(), consumer.Blobs, prepared.SiblingKey)
	if err != nil {
		t.Fatal(err)
	}
	record.Status = OperationDispatching
	record.DispatchToken = strings.Repeat("f", 64)
	if _, err := commitOperation(context.Background(), consumer.Blobs, prepared.SiblingKey, blob, record); err != nil {
		t.Fatal(err)
	}
	if err := consumer.ValidatePreparedLifecycle(context.Background(), prepared); !errors.Is(err, errStateConflict) {
		t.Fatalf("transitioned bundle = %v", err)
	}
}

func TestRunPreparedLifecycleDrivesBothGetRetireReplacementAndSiblingContinuity(t *testing.T) {
	consumer, authority, runtime := lifecycleFixture(t)
	prepared, err := consumer.PrepareLifecycle(context.Background(), authority, lifecycleInput())
	if err != nil {
		t.Fatal(err)
	}
	launcher := &fakeLifecycleLauncher{sessions: map[string]*fakeLifecycleSession{}}
	probe := &fakeLifecycleProbe{launcher: launcher}
	outcome, err := consumer.RunPreparedLifecycle(context.Background(), authority, prepared, launcher, probe)
	if err != nil {
		t.Fatalf("RunPreparedLifecycle: %v", err)
	}
	if !outcome.PrimaryExactRetirement || !outcome.GetBothBeforeRetire || !outcome.SiblingContinued ||
		!outcome.ReplacementReady || !outcome.GetBothAfterReplacement || !outcome.ReplacementExactRetirement ||
		!outcome.SiblingExactRetirement || probe.both != 2 || probe.sibling != 1 {
		t.Fatalf("outcome=%#v probe=%#v", outcome, probe)
	}
	if runtime.admits != 0 {
		t.Fatalf("coordinator bypassed launcher with %d direct admissions", runtime.admits)
	}
	for _, key := range []string{prepared.PrimaryFirstKey, prepared.SiblingKey, prepared.ReplacementKey} {
		if session := launcher.sessions[key]; session == nil || !session.stopped {
			t.Fatalf("session %q not exact-stopped: %#v", key, session)
		}
	}
}

func TestRunPreparedLifecyclePartialParallelStartSettlesWinner(t *testing.T) {
	consumer, authority, _ := lifecycleFixture(t)
	prepared, err := consumer.PrepareLifecycle(context.Background(), authority, lifecycleInput())
	if err != nil {
		t.Fatal(err)
	}
	launcher := &fakeLifecycleLauncher{sessions: map[string]*fakeLifecycleSession{}, failKey: prepared.SiblingKey}
	_, err = consumer.RunPreparedLifecycle(context.Background(), authority, prepared, launcher, &fakeLifecycleProbe{launcher: launcher})
	if err == nil {
		t.Fatal("partial start unexpectedly succeeded")
	}
	winner := launcher.sessions[prepared.PrimaryFirstKey]
	if winner == nil || !winner.stopped {
		t.Fatalf("parallel start winner was not settled: %#v", winner)
	}
}

func TestDurableCycleKnockerRejectsWrongAuthenticatedResourceHostAndCleansBothTransports(t *testing.T) {
	for _, transport := range []string{"direct", "relay"} {
		t.Run(transport, func(t *testing.T) {
			consumer, authority, _ := lifecycleFixture(t)
			runtime := &wrongResourceHostRuntime{}
			consumer.Runtime = runtime
			input := lifecycleInput()
			input.Transport = transport
			input.Phase = "candidate-" + transport
			prepared, err := consumer.PrepareLifecycle(context.Background(), authority, input)
			if err != nil {
				t.Fatal(err)
			}
			label := lifecycleLabels[transport][0]
			var identity FixedIdentity
			for index := range authority.Identities {
				if authority.Identities[index].Color == input.Color && authority.Identities[index].Label == label {
					identity = authority.Identities[index]
					break
				}
			}
			knocker, err := NewDurableCycleKnocker(context.Background(), consumer, prepared.PrimaryFirstKey,
				identity.KnockResourceID, identity.Selector)
			if err != nil {
				t.Fatal(err)
			}
			if err := knocker.BeginCycle(); err != nil {
				t.Fatal(err)
			}
			result, err := knocker.Knock(context.Background())
			if result != nil || err == nil || !errors.Is(err, errStateConflict) {
				t.Fatalf("wrong authenticated ResourceHost = %#v, %v", result, err)
			}
			record, _, loadErr := loadOperation(context.Background(), consumer.Blobs, prepared.PrimaryFirstKey)
			if loadErr != nil || record.Status != OperationClosed || record.Terminal == nil ||
				runtime.admits != 1 || runtime.retires != 1 || runtime.recovers != 0 || runtime.lastLive == nil ||
				runtime.lastLive.ACToken != "" {
				t.Fatalf("mismatch cleanup record=%#v runtime=%#v err=%v", record, runtime, loadErr)
			}
			expected := net.JoinHostPort(identity.Selector.Host, strconv.Itoa(identity.Selector.Port))
			if runtime.lastLive.ResourceHost == expected {
				t.Fatal("fixture did not provide a mismatched authenticated ResourceHost")
			}
		})
	}
}

func TestLifecycleInterruptedAttemptSettlesBeforeAttemptTwo(t *testing.T) {
	consumer, authority, _ := lifecycleFixture(t)
	firstInput := lifecycleInput()
	prepared, err := consumer.PrepareLifecycle(context.Background(), authority, firstInput)
	if err != nil {
		t.Fatal(err)
	}
	record, blob, err := loadOperation(context.Background(), consumer.Blobs, prepared.PrimaryFirstKey)
	if err != nil {
		t.Fatal(err)
	}
	record.Status = OperationDispatching
	record.DispatchToken = strings.Repeat("f", 64)
	if _, err := commitOperation(context.Background(), consumer.Blobs, prepared.PrimaryFirstKey, blob, record); err != nil {
		t.Fatal(err)
	}
	needsSettlement, err := consumer.LifecycleAttemptNeedsSettlement(context.Background(), prepared)
	if err != nil || !needsSettlement {
		t.Fatalf("needs settlement = %v, %v", needsSettlement, err)
	}
	recovery := &closureRuntime{}
	consumer.Runtime = recovery
	settlement, err := consumer.SettlePreparedLifecycle(context.Background(), authority, prepared, time.Second)
	if err != nil || settlement.Attempt != 1 || !settlement.RetryRequired || len(settlement.TerminalStates) != 3 {
		t.Fatalf("settlement = %#v, %v", settlement, err)
	}
	if recovery.admits != 0 || recovery.recovers != 3 {
		t.Fatalf("settlement performed admission: %#v", recovery)
	}
	secondInput := firstInput
	secondInput.Attempt = 2
	secondInput.RunIDs = [3]string{"4123456789abcdef", "5123456789abcdef", "6123456789abcdef"}
	secondRuntime := &lifecycleRuntime{blobs: consumer.Blobs,
		intentKey: "releases/" + secondInput.ReleaseID + "/lifecycle-intents/" + secondInput.Phase + "/" + secondInput.Color + "/" + secondInput.Transport + "/attempt-2"}
	consumer.Runtime = secondRuntime
	second, err := consumer.PrepareLifecycle(context.Background(), authority, secondInput)
	if err != nil {
		t.Fatalf("prepare attempt two: %v", err)
	}
	if second.Intent.Attempt != 2 || second.IntentReference == prepared.IntentReference || second.PrimaryFirstKey == prepared.PrimaryFirstKey {
		t.Fatalf("attempt two reused consumed authority: first=%#v second=%#v", prepared, second)
	}
}

type fakeLifecycleSession struct {
	mu      sync.Mutex
	stopped bool
}

func (s *fakeLifecycleSession) Alive() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return errors.New("session stopped")
	}
	return nil
}

func (s *fakeLifecycleSession) Stop(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	return nil
}

type fakeLifecycleLauncher struct {
	mu       sync.Mutex
	sessions map[string]*fakeLifecycleSession
	failKey  string
}

func (l *fakeLifecycleLauncher) Start(_ context.Context, key string, _ FixedIdentity) (LifecycleSession, error) { //nolint:gocritic // Interface requires the immutable identity value.
	l.mu.Lock()
	defer l.mu.Unlock()
	if key == l.failKey {
		return nil, errors.New("injected launch failure")
	}
	session := &fakeLifecycleSession{}
	l.sessions[key] = session
	return session, nil
}

func (l *fakeLifecycleLauncher) liveCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	for _, session := range l.sessions {
		if session.Alive() == nil {
			count++
		}
	}
	return count
}

type fakeLifecycleProbe struct {
	launcher *fakeLifecycleLauncher
	both     int
	sibling  int
}

func (p *fakeLifecycleProbe) Both(context.Context, FixedIdentity, FixedIdentity) error {
	if p.launcher.liveCount() != 2 {
		return errors.New("both customer resources are not live")
	}
	p.both++
	return nil
}

func (p *fakeLifecycleProbe) Sibling(context.Context, FixedIdentity) error {
	if p.launcher.liveCount() != 1 {
		return errors.New("sibling is not the one continuing resource")
	}
	p.sibling++
	return nil
}

type lifecycleRuntime struct {
	blobs         BlobAuthority
	intentKey     string
	prepares      int
	admits        int
	failPrepareAt int
	intentVisible []string
}

type wrongResourceHostRuntime struct {
	admits   int
	retires  int
	recovers int
	lastLive *LiveSession
}

func (*wrongResourceHostRuntime) Prepare(ctx context.Context, store qurl.AgentStateStore,
	request PrepareOperationRequest, //nolint:gocritic // SessionRuntime deliberately has value-oriented authority.
) (*qurl.NativeSessionOperation, error) {
	return (qurlSessionRuntime{}).Prepare(ctx, store, request)
}

func (r *wrongResourceHostRuntime) Admit(_ context.Context, _ qurl.AgentStateStore,
	record OperationRecord, //nolint:gocritic // SessionRuntime deliberately has value-oriented authority.
) (*LiveSession, SessionAdmission, error) {
	r.admits++
	live := &LiveSession{ACToken: "short-lived", ResourceHost: "wrong-frps.sandbox.example:6553",
		OperationID: record.Operation.OperationID, value: struct{}{}}
	r.lastLive = live
	return live, SessionAdmission{CellID: record.Operation.CellID, SessionID: 44,
		SessionIssuedAtMillis: record.Operation.PreparedAtMillis, RunID: record.Operation.RunID,
		RunAttempt: record.Operation.RunAttempt}, nil
}

func (r *wrongResourceHostRuntime) Retire(_ context.Context, record OperationRecord, //nolint:gocritic // SessionRuntime deliberately has value-oriented authority.
	live *LiveSession,
) (SessionTerminal, error) {
	r.retires++
	live.ACToken = ""
	return SessionTerminal{State: OperationClosed, WasAdmitted: true, CellID: record.Operation.CellID,
		SessionID: record.Admission.SessionID, SessionIssuedAtMillis: record.Admission.SessionIssuedAtMillis,
		RunID: record.Operation.RunID, RunAttempt: record.Operation.RunAttempt, CloseEventID: "close-wrong-host"}, nil
}

func (r *wrongResourceHostRuntime) Recover(context.Context, qurl.AgentStateStore,
	OperationRecord,
) (SessionTerminal, error) {
	r.recovers++
	return SessionTerminal{}, errors.New("unexpected runtime recovery after exact close")
}

func (r *lifecycleRuntime) Prepare(ctx context.Context, store qurl.AgentStateStore, request PrepareOperationRequest) (*qurl.NativeSessionOperation, error) { //nolint:gocritic // Fixture records one immutable request.
	r.prepares++
	if _, err := r.blobs.Load(ctx, r.intentKey); err != nil {
		return nil, errors.New("intent was not durable before operation preparation")
	}
	r.intentVisible = append(r.intentVisible, r.intentKey)
	if r.failPrepareAt == r.prepares {
		return nil, errors.New("injected offline preparation interruption")
	}
	return (qurlSessionRuntime{}).Prepare(ctx, store, request)
}

func (r *lifecycleRuntime) Admit(context.Context, qurl.AgentStateStore, OperationRecord) (*LiveSession, SessionAdmission, error) {
	r.admits++
	return nil, SessionAdmission{}, errors.New("unexpected network admission")
}

func (*lifecycleRuntime) Retire(context.Context, OperationRecord, *LiveSession) (SessionTerminal, error) {
	return SessionTerminal{}, errors.New("unexpected retirement")
}

func (*lifecycleRuntime) Recover(context.Context, qurl.AgentStateStore, OperationRecord) (SessionTerminal, error) {
	return SessionTerminal{}, errors.New("unexpected recovery")
}

func lifecycleFixture(t *testing.T) (*Consumer, Authority, *lifecycleRuntime) {
	t.Helper()
	blobs := newMemoryBlobs()
	plan := validPlan("8")
	privateKey, err := ecdh.X25519().NewPrivateKey(bytes.Repeat([]byte{0x21}, 32))
	if err != nil {
		t.Fatal(err)
	}
	publicKey := base64.StdEncoding.EncodeToString(privateKey.PublicKey().Bytes())
	registered := time.Unix(1_700_000_000, 0).UTC()
	authority := Authority{Schema: plan.Schema, Environment: plan.Environment, GenerationID: plan.GenerationID,
		OwnerSubject: plan.OwnerSubject, AWSAccountID: plan.AWSAccountID, AWSRegion: plan.AWSRegion,
		NHPSourceSHA:                plan.NHPSourceSHA,
		QURLGoSourceSHA:             plan.QURLGoSourceSHA,
		EnrollmentCredentialReceipt: StateReference{Key: "enrollment-receipt", VersionID: "version", SHA256: strings.Repeat("d", 64)}, Cohorts: plan.Cohorts}
	for identityIndex, identityPlan := range plan.Identities {
		cohort, cohortErr := cohortFor(plan, identityPlan.Color)
		if cohortErr != nil {
			t.Fatal(cohortErr)
		}
		stateKey := "generations/" + plan.GenerationID + "/" + identityPlan.Color + "/" + identityPlan.Label + "/agent-state"
		stateStore, err := NewDurableAgentStateStore(blobs, stateKey)
		if err != nil {
			t.Fatal(err)
		}
		deviceKeyID := fmt.Sprintf("key_%012d", identityIndex+1)
		state := &qurl.AgentState{AgentID: identityPlan.AgentID, PrivateKeyB64: base64.StdEncoding.EncodeToString(privateKey.Bytes()),
			PublicKeyB64: publicKey, SchemaVersion: 7, RegisteredAt: &registered,
			DeviceAPIKey: "lv_live_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8", DeviceAPIKeyID: deviceKeyID,
			Assignment: &qurl.AgentAssignment{CellID: cohort.CellID, AssignmentGeneration: cohort.AssignmentGeneration,
				EndpointRevision: 1, LeaseExpiresAt: time.Unix(2_000_000_000, 0).UTC(),
				Endpoint: qurl.NHPUDPEndpoint{Host: identityPlan.Selector.Host, Port: 443,
					ServerPublicKeyB64: cohort.HubServerPublicKeyB64}}}
		if err := stateStore.SaveAgentState(context.Background(), state); err != nil {
			t.Fatal(err)
		}
		stateRef, err := stateStore.Reference(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		authority.Identities = append(authority.Identities, FixedIdentity{Color: identityPlan.Color, Label: identityPlan.Label,
			OwnerID: identityPlan.OwnerID, AgentID: identityPlan.AgentID, AgentPublicKeyB64: publicKey,
			AgentKeySchemaVersion: 2, EnrollmentCredentialKind: "account", DeviceAPIKeyID: deviceKeyID,
			ConnectorID: identityPlan.ConnectorID, ResourceID: "resource-" + identityPlan.AgentID, CRID: "crid-" + identityPlan.AgentID,
			ConnectorRoutingID: "route-" + identityPlan.AgentID, KnockResourceID: "knock-" + identityPlan.AgentID,
			Selector: identityPlan.Selector, AgentState: stateRef,
			ConnectorState: StateReference{Key: "connector-" + identityPlan.AgentID, VersionID: "version", SHA256: strings.Repeat("c", 64)}})
	}
	if err := ValidateAuthority(authority); err != nil {
		t.Fatal(err)
	}
	input := lifecycleInput()
	intentKey := "releases/" + input.ReleaseID + "/lifecycle-intents/" + input.Phase + "/" + input.Color + "/" + input.Transport + "/attempt-1"
	runtime := &lifecycleRuntime{blobs: blobs, intentKey: intentKey}
	return &Consumer{Blobs: blobs, Runtime: runtime}, authority, runtime
}

func lifecycleInput() LifecycleIntentInput {
	prepared := time.Unix(1_800_000_000, 0).UTC()
	return LifecycleIntentInput{ReleaseID: strings.Repeat("d", 64), Phase: "candidate-direct", Attempt: 1, Color: "blue", Transport: "direct",
		RecoveryEndpoint: qurl.NHPUDPEndpoint{Host: "blue-recovery.sandbox.example", Port: 443,
			ServerPublicKeyB64: base64.StdEncoding.EncodeToString([]byte("abcdefghijklmnopqrstuvwxyzABCDEF"))},
		RunIDs:     [3]string{"0123456789abcdef", "1123456789abcdef", "2123456789abcdef"},
		PreparedAt: prepared, ExpiresAt: prepared.Add(20 * time.Minute)}
}
