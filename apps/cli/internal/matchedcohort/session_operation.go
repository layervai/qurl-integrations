package matchedcohort

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	qurl "github.com/layervai/qurl-go/qurl"
)

const (
	// OperationRecordSchema is the only accepted durable operation schema.
	OperationRecordSchema = 1
	// OperationPrepared starts the closed durable operation state union.
	OperationPrepared = "PREPARED"
	// OperationDispatching means the KNK boundary may have been crossed.
	OperationDispatching = "DISPATCHING"
	// OperationMapped means an authenticated admission was observed.
	OperationMapped = "MAPPED"
	// OperationClosing means exact retirement is durable but not terminal.
	OperationClosing = "CLOSING"
	// OperationCanceled is the terminal absent-operation tombstone.
	OperationCanceled = "CANCELED"
	// OperationClosed is the terminal admitted-session state.
	OperationClosed               = "CLOSED"
	sessionOperationCleanupBudget = 30 * time.Second
)

// errSessionRecoveryRequired means the caller must recover before replacement.
var errSessionRecoveryRequired = errors.New("matched cohort: native session operation recovery required")

// PrepareOperationRequest is the complete offline operation input. Routes are
// encrypted orchestration authority and are not copied to the UDP wire.
type PrepareOperationRequest struct {
	ReleaseID          string
	Phase              string
	AWSAccountID       string
	AWSRegion          string
	Identity           FixedIdentity
	Cohort             CohortPlan
	ExpectedAgentState StateReference
	RecoveryEndpoint   qurl.NHPUDPEndpoint
	RunID              string
	RunAttempt         uint64
	PreparedAt         time.Time
	ExpiresAt          time.Time
}

// SessionAdmission is the non-secret public receipt persisted after admission.
type SessionAdmission struct {
	CellID                string `json:"cell_id"`
	SessionID             uint64 `json:"session_id"`
	SessionIssuedAtMillis int64  `json:"session_issued_at_ms"`
	RunID                 string `json:"run_id"`
	RunAttempt            uint64 `json:"run_attempt"`
}

// SessionTerminal is the authenticated terminal or in-progress recovery result.
type SessionTerminal struct {
	State                 string `json:"state"`
	WasAdmitted           bool   `json:"was_admitted"`
	CellID                string `json:"cell_id,omitempty"`
	SessionID             uint64 `json:"session_id,omitempty"`
	SessionIssuedAtMillis int64  `json:"session_issued_at_ms,omitempty"`
	RunID                 string `json:"run_id,omitempty"`
	RunAttempt            uint64 `json:"run_attempt,omitempty"`
	CloseEventID          string `json:"close_event_id,omitempty"`
}

// RecoveryFirstReceipt binds one offline-prepared operation to its durable
// CANCELED tombstone. The tombstone is intentionally retained by NHP; its
// absence would let a delayed KNK create a session after this method returned.
type RecoveryFirstReceipt struct {
	OperationKey  string          `json:"operation_key"`
	Record        StateReference  `json:"record"`
	OperationID   string          `json:"operation_id"`
	BindingSHA256 string          `json:"binding_sha256"`
	Terminal      SessionTerminal `json:"terminal"`
}

// OperationRecord is the durable credential-free lifecycle receipt. PREPARED
// is committed before any KNK. DISPATCHING is committed immediately before the
// call, so a restarted process never emits a second admission and instead uses
// recovery-only EXT.
type OperationRecord struct {
	Schema           int                         `json:"schema"`
	ReleaseID        string                      `json:"release_id"`
	NHPSourceSHA     string                      `json:"nhp_source_sha"`
	AuthoritySHA256  string                      `json:"authority_sha256"`
	Phase            string                      `json:"phase"`
	Label            string                      `json:"label"`
	AgentState       StateReference              `json:"agent_state"`
	RecoveryEndpoint qurl.NHPUDPEndpoint         `json:"recovery_endpoint"`
	Operation        qurl.NativeSessionOperation `json:"operation"`
	Status           string                      `json:"status"`
	DispatchToken    string                      `json:"dispatch_token,omitempty"`
	Admission        *SessionAdmission           `json:"admission,omitempty"`
	Terminal         *SessionTerminal            `json:"terminal,omitempty"`
}

// LiveSession contains short-lived bearer authority and is never serializable.
// The caller must retire it or call Consumer.Recover before replacement.
type LiveSession struct {
	ACToken      string
	ResourceHost string
	OperationID  string
	value        any
}

// SessionRuntime is the narrow real qurl-go session seam used by hermetic tests.
type SessionRuntime interface {
	Prepare(context.Context, qurl.AgentStateStore, PrepareOperationRequest) (*qurl.NativeSessionOperation, error)
	Admit(context.Context, qurl.AgentStateStore, OperationRecord) (*LiveSession, SessionAdmission, error)
	Retire(context.Context, OperationRecord, *LiveSession) (SessionTerminal, error)
	Recover(context.Context, qurl.AgentStateStore, OperationRecord) (SessionTerminal, error)
}

type qurlConnectAgentRuntime func(context.Context, qurl.AgentStateStore, ...qurl.AgentRuntimeRegistrationOption) (*qurl.Client, *qurl.AgentRuntimeBinding, error)

type qurlSessionRuntime struct {
	connect             qurlConnectAgentRuntime
	registrationOptions []qurl.AgentRuntimeRegistrationOption
	udpOptions          []qurl.AgentRuntimeUDPOption
}

// NewQURLSessionRuntime binds every online admission renewal to the exact
// signed Hub selected by the rollout. Offline preparation and exact recovery
// use the same trust root without performing a Hub exchange.
func NewQURLSessionRuntime(hub qurl.HubBootstrap) (SessionRuntime, error) {
	if !validDNS(hub.Host) || hub.Port != 443 || !validBase64Raw32(hub.ServerPublicKeyB64) {
		return nil, fmt.Errorf("%w: native session Hub", errInvalidAuthority)
	}
	return qurlSessionRuntime{registrationOptions: []qurl.AgentRuntimeRegistrationOption{qurl.WithAgentRuntimeHub(hub)}}, nil
}

func (r qurlSessionRuntime) connectAgentRuntime(ctx context.Context, store qurl.AgentStateStore,
	options ...qurl.AgentRuntimeRegistrationOption,
) (*qurl.Client, *qurl.AgentRuntimeBinding, error) {
	if r.connect != nil {
		return r.connect(ctx, store, options...)
	}
	return qurl.ConnectAgentRuntime(ctx, store, options...)
}

type qurlLiveSession struct {
	binding    *qurl.AgentRuntimeBinding
	privateKey []byte
	receipt    qurl.NativeSessionReceipt
}

// Prepare opens a completed state without network I/O and creates one intent.
func (r qurlSessionRuntime) Prepare(ctx context.Context, store qurl.AgentStateStore, request PrepareOperationRequest) (*qurl.NativeSessionOperation, error) { //nolint:gocritic // Request is one immutable authority snapshot.
	options := make([]qurl.AgentRuntimeRegistrationOption, 0, 2+len(r.registrationOptions))
	options = append(options, qurl.WithAgentRuntimeIdentity(request.Identity.AgentID), qurl.WithAgentRuntimeOfflineOpen())
	options = append(options, r.registrationOptions...)
	_, binding, err := r.connectAgentRuntime(ctx, store, options...)
	if err != nil {
		return nil, err
	}
	defer binding.Destroy()
	privateKey := binding.TakeDeviceStaticPrivateKey()
	defer clear(privateKey)
	return qurl.PrepareNativeSessionOperation(binding, privateKey, qurl.NativeSessionOperationInput{
		AWSAccountID: request.AWSAccountID, AWSRegion: request.AWSRegion, CellID: request.Cohort.CellID,
		ExpiresAtMillis: request.ExpiresAt.UTC().UnixMilli(), OwnerID: request.Identity.OwnerID,
		PreparedAtMillis: request.PreparedAt.UTC().UnixMilli(), QURLAgentKeysTable: request.Cohort.QURLAgentKeysTable,
		ResourceID: request.Identity.KnockResourceID, RunAttempt: request.RunAttempt, RunID: request.RunID,
		SessionControlTable: request.Cohort.SessionControlTable,
	})
}

// Admit executes the exact precommitted operation and retains its opaque receipt.
func (r qurlSessionRuntime) Admit(ctx context.Context, store qurl.AgentStateStore, record OperationRecord) (*LiveSession, SessionAdmission, error) { //nolint:gocritic // Record is one immutable authority snapshot.
	options := make([]qurl.AgentRuntimeRegistrationOption, 0, 2+len(r.registrationOptions))
	options = append(options, qurl.WithAgentRuntimeIdentity(record.Operation.AgentID), qurl.WithAgentRuntimePinnedAssignment())
	options = append(options, r.registrationOptions...)
	_, binding, err := r.connectAgentRuntime(ctx, store, options...)
	if err != nil {
		return nil, SessionAdmission{}, err
	}
	privateKey := binding.TakeDeviceStaticPrivateKey()
	result, err := qurl.KnockRegisteredAgent(ctx, binding, privateKey, record.Operation.ResourceID,
		qurl.NativeKnockOptions{RunID: record.Operation.RunID, RunAttempt: record.Operation.RunAttempt, Operation: &record.Operation}, r.udpOptions...)
	if err != nil {
		clear(privateKey)
		binding.Destroy()
		return nil, SessionAdmission{}, err
	}
	if result == nil || result.ACToken == "" || result.ResourceHost == "" {
		clear(privateKey)
		binding.Destroy()
		return nil, SessionAdmission{}, errors.New("native session admission is incomplete")
	}
	admission := SessionAdmission{CellID: result.SessionReceipt.CellID, SessionID: result.SessionReceipt.SessionID,
		SessionIssuedAtMillis: result.SessionReceipt.SessionIssuedAtMillis, RunID: result.SessionReceipt.RunID,
		RunAttempt: result.SessionReceipt.RunAttempt}
	return &LiveSession{ACToken: result.ACToken, ResourceHost: result.ResourceHost, OperationID: record.Operation.OperationID,
		value: &qurlLiveSession{binding: binding, privateKey: privateKey, receipt: result.SessionReceipt}}, admission, nil
}

// Retire sends the strict exact-session close using the live opaque receipt.
func (qurlSessionRuntime) Retire(ctx context.Context, record OperationRecord, live *LiveSession) (SessionTerminal, error) { //nolint:gocritic // Record is one immutable authority snapshot.
	owned, ok := live.value.(*qurlLiveSession)
	if !ok || live.OperationID != record.Operation.OperationID {
		return SessionTerminal{}, errors.New("live session does not match durable operation")
	}
	defer func() {
		clear(owned.privateKey)
		owned.binding.Destroy()
		live.ACToken = ""
		live.value = nil
	}()
	result, err := qurl.RetireRegisteredAgentSession(ctx, owned.binding, owned.privateKey, owned.receipt)
	if err != nil {
		return SessionTerminal{}, err
	}
	state := OperationClosing
	if result.State == "closed" {
		state = OperationClosed
	} else if result.State != "closing" {
		return SessionTerminal{}, errors.New("native session retirement state is invalid")
	}
	return SessionTerminal{State: state, WasAdmitted: true, CellID: result.SessionReceipt.CellID,
		SessionID: result.SessionReceipt.SessionID, SessionIssuedAtMillis: result.SessionReceipt.SessionIssuedAtMillis,
		RunID: result.SessionReceipt.RunID, RunAttempt: result.SessionReceipt.RunAttempt, CloseEventID: result.CloseEventID}, nil
}

// Recover sends recovery-only EXT to the source-fenced endpoint.
func (r qurlSessionRuntime) Recover(ctx context.Context, store qurl.AgentStateStore, record OperationRecord) (SessionTerminal, error) { //nolint:gocritic // Record is one immutable authority snapshot.
	options := make([]qurl.AgentRuntimeRegistrationOption, 0, 2+len(r.registrationOptions))
	options = append(options, qurl.WithAgentRuntimeIdentity(record.Operation.AgentID), qurl.WithAgentRuntimeOfflineOpen())
	options = append(options, r.registrationOptions...)
	_, binding, err := r.connectAgentRuntime(ctx, store, options...)
	if err != nil {
		return SessionTerminal{}, err
	}
	defer binding.Destroy()
	privateKey := binding.TakeDeviceStaticPrivateKey()
	defer clear(privateKey)
	result, err := qurl.RecoverNativeSessionOperation(ctx, binding, privateKey, record.Operation, record.RecoveryEndpoint)
	if err != nil {
		return SessionTerminal{}, err
	}
	return SessionTerminal{State: result.State, WasAdmitted: result.State != OperationCanceled, CellID: result.CellID,
		SessionID: result.SessionID, SessionIssuedAtMillis: result.SessionIssuedAtMillis, RunID: result.RunID,
		RunAttempt: result.RunAttempt, CloseEventID: result.CloseEventID}, nil
}

// Consumer prepares, admits, retires, and recovers durable native operations.
type Consumer struct {
	Blobs   BlobAuthority
	Runtime SessionRuntime
}

// Prepare persists one exact operation before any network-facing child starts.
func (c *Consumer) Prepare(ctx context.Context, authority Authority, request PrepareOperationRequest) (string, OperationRecord, error) { //nolint:gocritic,gocyclo // Closed projection checks remain explicit at the trust boundary.
	if c == nil || c.Blobs == nil || ValidateAuthority(authority) != nil || request.ReleaseID == "" || !hex64Pattern.MatchString(request.ReleaseID) ||
		request.ExpectedAgentState.Key != request.Identity.AgentState.Key || validateStateReference(request.ExpectedAgentState) != nil {
		return "", OperationRecord{}, fmt.Errorf("%w: operation preparation authority", errInvalidAuthority)
	}
	if !validSharedOperationPhase(request.Phase, request.Identity.Label) {
		return "", OperationRecord{}, fmt.Errorf("%w: operation phase", errInvalidAuthority)
	}
	if request.AWSAccountID != authority.AWSAccountID || request.AWSRegion != authority.AWSRegion {
		return "", OperationRecord{}, fmt.Errorf("%w: operation AWS authority", errInvalidAuthority)
	}
	if !authorityContainsIdentity(authority, request.Identity) || !authorityContainsCohort(authority, request.Cohort) {
		return "", OperationRecord{}, fmt.Errorf("%w: operation identity or cohort projection", errInvalidAuthority)
	}
	if request.RecoveryEndpoint != request.Cohort.CellEndpoint {
		return "", OperationRecord{}, fmt.Errorf("%w: operation recovery route", errInvalidAuthority)
	}
	stateStore, err := NewDurableAgentStateStore(c.Blobs, request.ExpectedAgentState.Key)
	if err != nil {
		return "", OperationRecord{}, err
	}
	stateRef, err := stateStore.Reference(ctx)
	if err != nil || stateRef != request.ExpectedAgentState {
		return "", OperationRecord{}, fmt.Errorf("%w: operation agent state readback", errStateConflict)
	}
	runtime := c.Runtime
	if runtime == nil {
		runtime = qurlSessionRuntime{}
	}
	operation, prepareErr := runtime.Prepare(ctx, stateStore, request)
	if prepareErr != nil {
		return "", OperationRecord{}, fmt.Errorf("prepare native operation offline: %w", prepareErr)
	}
	if operation.AgentID != request.Identity.AgentID || operation.AgentPublicKeyB64 != request.Identity.AgentPublicKeyB64 ||
		operation.OwnerID != request.Identity.OwnerID || operation.ResourceID != request.Identity.KnockResourceID ||
		operation.CellID != request.Cohort.CellID || operation.SessionControlTable != request.Cohort.SessionControlTable ||
		operation.QURLAgentKeysTable != request.Cohort.QURLAgentKeysTable || operation.AWSAccountID != authority.AWSAccountID ||
		operation.AWSRegion != authority.AWSRegion || operation.RunID != request.RunID || operation.RunAttempt != request.RunAttempt ||
		operation.PreparedAtMillis != request.PreparedAt.UnixMilli() || operation.ExpiresAtMillis != request.ExpiresAt.UnixMilli() {
		return "", OperationRecord{}, fmt.Errorf("%w: prepared operation projection", errStateConflict)
	}
	authorityRaw, encodeErr := CanonicalJSON(authority)
	if encodeErr != nil {
		return "", OperationRecord{}, encodeErr
	}
	record := OperationRecord{Schema: OperationRecordSchema, ReleaseID: request.ReleaseID, NHPSourceSHA: authority.NHPSourceSHA,
		AuthoritySHA256: Digest(authorityRaw), Phase: request.Phase,
		Label: request.Identity.Label, AgentState: stateRef,
		RecoveryEndpoint: request.RecoveryEndpoint, Operation: *operation, Status: OperationPrepared}
	key := operationRecordKey(record)
	committed, err := persistInitialOperation(ctx, c.Blobs, key, record)
	if err != nil {
		return "", OperationRecord{}, err
	}
	return key, committed, nil
}

// Admit marks dispatch before KNK. A resumed non-PREPARED operation is never
// admitted again and must pass through Recover.
func (c *Consumer) Admit(ctx context.Context, key string) (*LiveSession, error) {
	record, blob, err := loadOperation(ctx, c.Blobs, key)
	if err != nil {
		return nil, err
	}
	if record.Status != OperationPrepared {
		terminal, recoverErr := c.Recover(ctx, key)
		return nil, errors.Join(errSessionRecoveryRequired, recoverErr, terminalError(terminal))
	}
	dispatchBytes := make([]byte, 32)
	if _, err := rand.Read(dispatchBytes); err != nil {
		return nil, fmt.Errorf("generate operation dispatch token: %w", err)
	}
	record.DispatchToken = Digest(dispatchBytes)
	clear(dispatchBytes)
	record.Status = OperationDispatching
	blob, err = commitOperation(ctx, c.Blobs, key, blob, record)
	if err != nil {
		return nil, err
	}
	stateStore, _ := NewDurableAgentStateStore(c.Blobs, record.AgentState.Key)
	runtime := c.runtime()
	live, admission, err := runtime.Admit(ctx, stateStore, record)
	if err != nil {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), sessionOperationCleanupBudget)
		defer cancelCleanup()
		_, recoverErr := c.Recover(cleanupCtx, key)
		return nil, errors.Join(err, recoverErr)
	}
	record.Status = OperationMapped
	record.Admission = &admission
	if _, err := commitOperation(ctx, c.Blobs, key, blob, record); err != nil {
		// A live bearer and opaque receipt exist even if the MAPPED receipt could
		// not be committed. Destroy that live authority through exact retirement,
		// then converge the durable operation through recovery. This bounded
		// cleanup deliberately survives caller cancellation.
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), sessionOperationCleanupBudget)
		defer cancelCleanup()
		_, retireErr := runtime.Retire(cleanupCtx, record, live)
		terminal, recoverErr := c.Recover(cleanupCtx, key)
		return nil, errors.Join(err, retireErr, recoverErr, terminalError(terminal))
	}
	return live, nil
}

// Retire consumes a live bearer and persists CLOSING or CLOSED before return.
func (c *Consumer) Retire(ctx context.Context, key string, live *LiveSession) (SessionTerminal, error) {
	record, blob, err := loadOperation(ctx, c.Blobs, key)
	if err != nil {
		return SessionTerminal{}, err
	}
	if record.Status != OperationMapped || live == nil || live.OperationID != record.Operation.OperationID {
		return SessionTerminal{}, errSessionRecoveryRequired
	}
	terminal, err := c.runtime().Retire(ctx, record, live)
	if err != nil {
		return c.Recover(ctx, key)
	}
	record.Status = terminal.State
	record.Terminal = &terminal
	if _, err := commitOperation(ctx, c.Blobs, key, blob, record); err != nil {
		return c.Recover(ctx, key)
	}
	return terminal, nil
}

// Recover closes or tombstones one exact operation and never opens a session.
func (c *Consumer) Recover(ctx context.Context, key string) (SessionTerminal, error) {
	record, blob, err := loadOperation(ctx, c.Blobs, key)
	if err != nil {
		return SessionTerminal{}, err
	}
	if isTerminal(record.Status) && record.Terminal != nil {
		return *record.Terminal, nil
	}
	// Recovery-first can create the server-side CANCELED tombstone. Mark that
	// network boundary durably before the EXT for the same reason admission is
	// marked before KNK: after runner loss, a successor may recover this exact
	// operation but may never infer that no packet was sent.
	if record.Status == OperationPrepared {
		dispatchBytes := make([]byte, 32)
		if _, err := rand.Read(dispatchBytes); err != nil {
			return SessionTerminal{}, fmt.Errorf("generate operation recovery token: %w", err)
		}
		record.DispatchToken = Digest(dispatchBytes)
		clear(dispatchBytes)
		record.Status = OperationDispatching
		blob, err = commitOperation(ctx, c.Blobs, key, blob, record)
		if err != nil {
			return SessionTerminal{}, err
		}
	}
	stateStore, _ := NewDurableAgentStateStore(c.Blobs, record.AgentState.Key)
	terminal, err := c.runtime().Recover(ctx, stateStore, record)
	if err != nil {
		return SessionTerminal{}, err
	}
	if terminal.State != OperationCanceled && terminal.State != OperationClosing && terminal.State != OperationClosed {
		return SessionTerminal{}, fmt.Errorf("%w: recovery state", errStateConflict)
	}
	record.Status = terminal.State
	record.Terminal = &terminal
	if _, err := commitOperation(ctx, c.Blobs, key, blob, record); err != nil {
		return SessionTerminal{}, err
	}
	return terminal, nil
}

// RecoverPrepared commits an offline operation before network I/O, then sends
// only its authenticated recovery EXT and requires the exact absent-session
// CANCELED terminal. Replaying the same request reads the retained terminal
// receipt and emits no second recovery packet.
//
//nolint:gocritic // Both values are one immutable signed authority projection.
func (c *Consumer) RecoverPrepared(ctx context.Context, authority Authority,
	request PrepareOperationRequest,
) (RecoveryFirstReceipt, error) {
	if request.Phase != "fixed_shared_recovery_first" || request.Identity.Label != labelDirectA {
		return RecoveryFirstReceipt{}, fmt.Errorf("%w: recovery-first phase", errInvalidAuthority)
	}
	key, prepared, err := c.Prepare(ctx, authority, request)
	if err != nil {
		return RecoveryFirstReceipt{}, err
	}
	terminal, err := c.Recover(ctx, key)
	if err != nil {
		return RecoveryFirstReceipt{}, err
	}
	record, blob, err := loadOperation(ctx, c.Blobs, key)
	if err != nil || record.Status != OperationCanceled || record.Terminal == nil || *record.Terminal != terminal ||
		terminal.State != OperationCanceled || terminal.WasAdmitted || record.Operation.OperationID != prepared.Operation.OperationID ||
		record.Operation.BindingSHA256 != prepared.Operation.BindingSHA256 {
		return RecoveryFirstReceipt{}, errors.Join(err, fmt.Errorf("%w: recovery-first terminal receipt", errStateConflict))
	}
	return RecoveryFirstReceipt{OperationKey: key,
		Record:      StateReference{Key: key, VersionID: blob.VersionID, SHA256: blob.SHA256},
		OperationID: record.Operation.OperationID, BindingSHA256: record.Operation.BindingSHA256, Terminal: terminal}, nil
}

func (c *Consumer) runtime() SessionRuntime {
	if c.Runtime != nil {
		return c.Runtime
	}
	return qurlSessionRuntime{}
}

func persistInitialOperation(ctx context.Context, authority BlobAuthority, key string, record OperationRecord) (OperationRecord, error) { //nolint:gocritic // Record is one immutable authority snapshot.
	if !validOperationRecord(record) {
		return OperationRecord{}, fmt.Errorf("%w: initial operation record", errInvalidAuthority)
	}
	raw, err := CanonicalJSON(record)
	if err != nil {
		return OperationRecord{}, err
	}
	operationID := Digest([]byte("layerv/matched-cohort-operation-record/v1\x00" + key + "\x00" + Digest(raw)))
	current, err := authority.Load(ctx, key)
	if err == nil {
		restored, restoreErr := decodeOperation(current)
		if restoreErr != nil || !sameOperationAuthority(restored, record) {
			return OperationRecord{}, fmt.Errorf("%w: operation replay drift", errStateConflict)
		}
		return restored, nil
	}
	if !errors.Is(err, errStateNotFound) {
		return OperationRecord{}, err
	}
	candidate := BlobCandidate{Key: key, OperationID: operationID, SHA256: Digest(raw), Body: raw}
	committed, err := authority.Commit(ctx, candidate)
	if err != nil {
		observed, loadErr := authority.Load(ctx, key)
		if loadErr != nil || !sameCommittedBlob(observed, candidate) {
			return OperationRecord{}, errStateAmbiguous
		}
		committed = observed
	}
	return decodeOperation(committed)
}

func loadOperation(ctx context.Context, authority BlobAuthority, key string) (OperationRecord, Blob, error) {
	blob, err := authority.Load(ctx, key)
	if err != nil {
		return OperationRecord{}, Blob{}, err
	}
	if blob.Key != key {
		return OperationRecord{}, Blob{}, fmt.Errorf("%w: operation key readback", errStateConflict)
	}
	record, err := decodeOperation(blob)
	return record, blob, err
}

func decodeOperation(blob Blob) (OperationRecord, error) { //nolint:gocritic // Blob is an immutable receipt value.
	if Digest(blob.Body) != blob.SHA256 || !validText(blob.VersionID) {
		return OperationRecord{}, fmt.Errorf("%w: operation blob", errStateConflict)
	}
	var record OperationRecord
	if err := json.Unmarshal(blob.Body, &record); err != nil {
		return OperationRecord{}, fmt.Errorf("%w: operation JSON", errStateConflict)
	}
	canonical, _ := CanonicalJSON(record)
	if !bytes.Equal(canonical, blob.Body) || !validOperationRecord(record) || blob.Key != operationRecordKey(record) {
		return OperationRecord{}, fmt.Errorf("%w: operation binding", errStateConflict)
	}
	return record, nil
}

func commitOperation(ctx context.Context, authority BlobAuthority, key string, previous Blob, record OperationRecord) (Blob, error) { //nolint:gocritic // Previous and record are exact immutable snapshots.
	if !validOperationRecord(record) {
		return Blob{}, fmt.Errorf("%w: operation transition", errInvalidAuthority)
	}
	raw, err := CanonicalJSON(record)
	if err != nil {
		return Blob{}, err
	}
	digest := Digest(raw)
	candidate := BlobCandidate{Key: key, ExpectedVersion: previous.VersionID,
		OperationID: Digest([]byte("layerv/matched-cohort-operation-transition/v1\x00" + key + "\x00" + previous.VersionID + "\x00" + digest)),
		SHA256:      digest, Body: raw}
	committed, err := authority.Commit(ctx, candidate)
	if err != nil {
		observed, loadErr := authority.Load(ctx, key)
		if loadErr != nil || !sameCommittedBlob(observed, candidate) {
			return Blob{}, errStateAmbiguous
		}
		committed = observed
	}
	if !sameCommittedBlob(committed, candidate) {
		return Blob{}, errStateConflict
	}
	return committed, nil
}

func validOperationRecord(record OperationRecord) bool { //nolint:gocritic,gocyclo // Each state has a distinct closed field union.
	if record.Schema != OperationRecordSchema || !hex64Pattern.MatchString(record.ReleaseID) || !validSharedOperationPhase(record.Phase, record.Label) ||
		!hex40Pattern.MatchString(record.NHPSourceSHA) || !hex64Pattern.MatchString(record.AuthoritySHA256) || !containsLabel(record.Label) ||
		record.Operation.OperationID == "" || record.RecoveryEndpoint.Port != 443 || !validDNS(record.RecoveryEndpoint.Host) ||
		!validBase64Raw32(record.RecoveryEndpoint.ServerPublicKeyB64) {
		return false
	}
	switch record.Status {
	case OperationPrepared:
		return record.Admission == nil && record.Terminal == nil && record.DispatchToken == ""
	case OperationDispatching:
		return record.Admission == nil && record.Terminal == nil && hex64Pattern.MatchString(record.DispatchToken)
	case OperationMapped:
		return validSessionAdmission(record.Admission, record.Operation) && record.Terminal == nil && hex64Pattern.MatchString(record.DispatchToken)
	case OperationClosing:
		return validAdmittedTerminal(record.Terminal, record.Operation, OperationClosing) &&
			(record.Admission == nil || validSessionAdmission(record.Admission, record.Operation)) && hex64Pattern.MatchString(record.DispatchToken)
	case OperationCanceled:
		return validCanceledTerminal(record.Terminal) && record.Admission == nil && hex64Pattern.MatchString(record.DispatchToken)
	case OperationClosed:
		return validAdmittedTerminal(record.Terminal, record.Operation, OperationClosed) &&
			(record.Admission == nil || validSessionAdmission(record.Admission, record.Operation)) && hex64Pattern.MatchString(record.DispatchToken)
	default:
		return false
	}
}

func validSessionAdmission(admission *SessionAdmission, operation qurl.NativeSessionOperation) bool { //nolint:gocritic // Immutable receipt comparison.
	return admission != nil && admission.CellID == operation.CellID && admission.SessionID != 0 &&
		admission.SessionIssuedAtMillis > 0 && admission.RunID == operation.RunID && admission.RunAttempt == operation.RunAttempt
}

func validAdmittedTerminal(terminal *SessionTerminal, operation qurl.NativeSessionOperation, state string) bool { //nolint:gocritic // Immutable receipt comparison.
	return terminal != nil && terminal.State == state && terminal.WasAdmitted && terminal.CellID == operation.CellID &&
		terminal.SessionID != 0 && terminal.SessionIssuedAtMillis > 0 && terminal.RunID == operation.RunID &&
		terminal.RunAttempt == operation.RunAttempt && validText(terminal.CloseEventID)
}

func validCanceledTerminal(terminal *SessionTerminal) bool {
	return terminal != nil && terminal.State == OperationCanceled && !terminal.WasAdmitted && terminal.CellID == "" &&
		terminal.SessionID == 0 && terminal.SessionIssuedAtMillis == 0 && terminal.RunID == "" && terminal.RunAttempt == 0 &&
		terminal.CloseEventID == ""
}

func operationRecordKey(record OperationRecord) string { //nolint:gocritic // Record is one immutable authority snapshot.
	return fmt.Sprintf("releases/%s/operations/%s/shared/%s/%s", record.ReleaseID, record.Phase, record.Label, record.Operation.OperationID)
}

func sameOperationAuthority(left, right OperationRecord) bool { //nolint:gocritic // Closed record values are intentionally compared by canonical bytes.
	left.Status, left.Admission, left.Terminal = right.Status, right.Admission, right.Terminal
	left.DispatchToken = right.DispatchToken
	// A credential-free authenticated refresh may advance the same fixed state
	// key before a process replays this offline operation preparation. The OP
	// identity and all wire binding fields remain exact; the first durable record
	// keeps the state version that existed when it was prepared.
	left.AgentState = right.AgentState
	lraw, _ := CanonicalJSON(left)
	rraw, _ := CanonicalJSON(right)
	return bytes.Equal(lraw, rraw)
}

func containsLabel(label string) bool {
	for _, candidate := range labels {
		if label == candidate {
			return true
		}
	}
	return false
}

func validSharedOperationPhase(phase, label string) bool {
	switch phase {
	case "fixed_shared_direct":
		return label == labelDirectA || label == labelDirectB
	case "fixed_shared_relay":
		return label == labelRelayC || label == labelRelayD
	case "fixed_shared_recovery_first":
		return label == labelDirectA
	default:
		return false
	}
}

func authorityContainsIdentity(authority Authority, identity FixedIdentity) bool { //nolint:gocritic // Exact closed value equality is the contract.
	for i := range authority.Identities {
		if authority.Identities[i] == identity {
			return true
		}
	}
	return false
}

func authorityContainsCohort(authority Authority, cohort CohortPlan) bool { //nolint:gocritic // Exact closed value equality is the contract.
	for i := range authority.Cohorts {
		if authority.Cohorts[i] == cohort {
			return true
		}
	}
	return false
}

func isTerminal(status string) bool { return status == OperationCanceled || status == OperationClosed }

func terminalError(terminal SessionTerminal) error { //nolint:gocritic // Terminal is a small immutable receipt.
	if terminal.State == "" {
		return nil
	}
	return fmt.Errorf("operation reached %s", terminal.State)
}
