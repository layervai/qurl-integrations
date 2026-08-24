package matchedcohort

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	qurl "github.com/layervai/qurl-go/qurl"
	"github.com/layervai/qurl-go/relayknock/nativeudp"
)

const (
	closureIntentSchema   = 1
	closureEvidenceSchema = 1
	// MaxClosureAttempts bounds runner-loss retries while admission remains
	// closed. Every retry uses four new, precommitted native operations.
	MaxClosureAttempts = 3
)

var closureLabels = [4]string{labelDirectA, labelDirectB, labelRelayC, labelRelayD}

// ClosureIntent is the immutable four-identity admission-negative authority.
// It is committed with all operation records before any public admission
// packet. The source-fenced recovery endpoint stays available while public
// admission is closed, so every failed attempt can become an exact CANCELED
// tombstone and a runner restart never needs to infer packet delivery.
type ClosureIntent struct {
	Schema            int                        `json:"schema"`
	ReleaseID         string                     `json:"release_id"`
	Phase             string                     `json:"phase"`
	Attempt           uint64                     `json:"attempt"`
	Color             string                     `json:"color"`
	AuthoritySHA256   string                     `json:"authority_sha256"`
	AdmissionEndpoint qurl.NHPUDPEndpoint        `json:"admission_endpoint"`
	RecoveryEndpoint  qurl.NHPUDPEndpoint        `json:"recovery_endpoint"`
	Previous          *ClosureAttemptReference   `json:"previous,omitempty"`
	Operations        []LifecycleOperationIntent `json:"operations"`
}

// ClosureAttemptReference is one immutable link in the bounded closure retry
// ledger. Old attempts stay addressable after a successor is committed.
type ClosureAttemptReference struct {
	Attempt       uint64         `json:"attempt"`
	Intent        StateReference `json:"intent"`
	OperationKeys [4]string      `json:"operation_keys"`
}

// ClosureIntentInput supplies all random and time authority before the first
// durable write. RunIDs are caller-generated and cannot change on resume.
type ClosureIntentInput struct {
	ReleaseID         string
	Phase             string
	Attempt           uint64
	Color             string
	AdmissionEndpoint qurl.NHPUDPEndpoint
	RecoveryEndpoint  qurl.NHPUDPEndpoint
	RunIDs            [4]string
	PreparedAt        time.Time
	ExpiresAt         time.Time
	AgentStates       map[string]StateReference
	Previous          *PreparedClosure
}

// PreparedClosure binds the immutable intent and exact ordered operation keys.
type PreparedClosure struct {
	IntentReference StateReference
	Intent          ClosureIntent
	OperationKeys   [4]string
}

// ClosureOutcome is successful only when all four authenticated admission
// attempts were denied and every operation is durably CANCELED. The four
// fixed identities cover the exact a,b,c selector set; relay-d deliberately
// repeats selector a but remains a distinct relay admission attempt.
type ClosureOutcome struct {
	AttemptedLabels       []string         `json:"attempted_labels"`
	CanceledOperationKeys []string         `json:"canceled_operation_keys"`
	UniqueFRPSSelectors   []string         `json:"unique_frps_selectors"`
	Evidence              []StateReference `json:"evidence"`
	AdmissionSucceeded    bool             `json:"admission_succeeded"`
}

// ClosureSettlement is the controller-facing retry receipt after a runner
// loss. A successor attempt is allowed only when all four prior operations are
// exact CANCELED and at least one immutable no-reply receipt is missing.
type ClosureSettlement struct {
	Attempt         uint64   `json:"attempt"`
	OperationKeys   []string `json:"operation_keys"`
	TerminalStates  []string `json:"terminal_states"`
	EvidencePresent []bool   `json:"evidence_present"`
	RetryRequired   bool     `json:"retry_required"`
}

// ClosureNoReplyEvidence is written immediately after qurl-go returns its
// sealed endpoint-no-reply observation and before recovery. It contains no
// bearer authority. Its immutable blob is the only delivery receipt accepted
// after a runner restart.
type ClosureNoReplyEvidence struct {
	Schema          int    `json:"schema"`
	ReleaseID       string `json:"release_id"`
	Phase           string `json:"phase"`
	Attempt         uint64 `json:"attempt"`
	Color           string `json:"color"`
	Label           string `json:"label"`
	IntentSHA256    string `json:"intent_sha256"`
	OperationID     string `json:"operation_id"`
	Endpoint        string `json:"endpoint"`
	AddressAttempts int    `json:"address_attempts"`
	ElapsedNanos    int64  `json:"elapsed_nanos"`
	Outcome         string `json:"outcome"`
}

// PrepareClosure durably commits the exact closure bundle before networking.
func (c *Consumer) PrepareClosure(ctx context.Context, authority Authority, input ClosureIntentInput) (PreparedClosure, error) { //nolint:gocritic,gocyclo,gocognit // Closed authority validation is intentionally explicit.
	if c == nil || c.Blobs == nil || ValidateAuthority(authority) != nil || !hex64Pattern.MatchString(input.ReleaseID) ||
		!validText(input.Phase) || input.Attempt == 0 || input.Attempt > MaxClosureAttempts ||
		(input.Color != ColorBlue && input.Color != ColorGreen) ||
		input.AdmissionEndpoint.Port != 443 || !validDNS(input.AdmissionEndpoint.Host) ||
		!validBase64Raw32(input.AdmissionEndpoint.ServerPublicKeyB64) || input.RecoveryEndpoint.Port != 443 ||
		!validDNS(input.RecoveryEndpoint.Host) || !validBase64Raw32(input.RecoveryEndpoint.ServerPublicKeyB64) ||
		input.AdmissionEndpoint == input.RecoveryEndpoint {
		return PreparedClosure{}, fmt.Errorf("%w: closure authority", errInvalidAuthority)
	}
	if input.PreparedAt.IsZero() || input.ExpiresAt.IsZero() || !input.ExpiresAt.After(input.PreparedAt) ||
		input.ExpiresAt.Sub(input.PreparedAt) > 30*time.Minute {
		return PreparedClosure{}, fmt.Errorf("%w: closure operation time", errInvalidAuthority)
	}
	if input.Attempt == 1 && input.Previous != nil || input.Attempt > 1 && input.Previous == nil ||
		input.Attempt > 1 && input.AgentStates != nil {
		return PreparedClosure{}, fmt.Errorf("%w: closure attempt predecessor", errInvalidAuthority)
	}
	if input.AgentStates != nil && len(input.AgentStates) != len(closureLabels) {
		return PreparedClosure{}, fmt.Errorf("%w: closure refreshed state set", errInvalidAuthority)
	}
	for _, label := range closureLabels {
		if input.AgentStates != nil && validateStateReference(input.AgentStates[label]) != nil {
			return PreparedClosure{}, fmt.Errorf("%w: closure refreshed state", errInvalidAuthority)
		}
	}
	seenRunIDs := make(map[string]struct{}, len(input.RunIDs))
	for _, runID := range input.RunIDs {
		if err := qurl.ValidateCycleRunID(runID); err != nil {
			return PreparedClosure{}, fmt.Errorf("%w: closure RunID", errInvalidAuthority)
		}
		if _, exists := seenRunIDs[runID]; exists {
			return PreparedClosure{}, fmt.Errorf("%w: closure RunIDs are not distinct", errInvalidAuthority)
		}
		seenRunIDs[runID] = struct{}{}
	}
	authorityRaw, err := CanonicalJSON(authority)
	if err != nil {
		return PreparedClosure{}, err
	}
	var previous *ClosureAttemptReference
	var predecessorStates map[string]StateReference
	if input.Previous != nil {
		previousValue, states, predecessorErr := c.validateClosurePredecessor(ctx, &authority, &input, input.Previous)
		if predecessorErr != nil {
			return PreparedClosure{}, predecessorErr
		}
		previous = &previousValue
		predecessorStates = states
	}
	intent := ClosureIntent{Schema: closureIntentSchema, ReleaseID: input.ReleaseID, Phase: input.Phase,
		Attempt: input.Attempt, Color: input.Color, AuthoritySHA256: Digest(authorityRaw), AdmissionEndpoint: input.AdmissionEndpoint,
		RecoveryEndpoint: input.RecoveryEndpoint, Previous: previous,
		Operations: make([]LifecycleOperationIntent, 0, len(closureLabels))}
	for index, label := range closureLabels {
		intent.Operations = append(intent.Operations, LifecycleOperationIntent{Role: "closed-admission", Label: label,
			RunID: input.RunIDs[index], RunAttempt: input.Attempt, PreparedAtMS: input.PreparedAt.UTC().UnixMilli(),
			ExpiresAtMS: input.ExpiresAt.UTC().UnixMilli()})
	}
	intentRaw, err := CanonicalJSON(intent)
	if err != nil {
		return PreparedClosure{}, err
	}
	intentKey := fmt.Sprintf("releases/%s/closure-intents/%s/%s/attempt-%d", input.ReleaseID, input.Phase, input.Color, input.Attempt)
	intentBlob, err := persistImmutable(ctx, c.Blobs, intentKey, "closure-intent", intentRaw)
	if err != nil {
		return PreparedClosure{}, fmt.Errorf("persist closure intent before operations: %w", err)
	}
	cohort, identities, err := closureProjection(authority, input.Color)
	if err != nil {
		return PreparedClosure{}, err
	}
	var keys [4]string
	for index, operation := range intent.Operations {
		identity := identities[operation.Label]
		expectedState := identity.AgentState
		if input.AgentStates != nil {
			expectedState = input.AgentStates[operation.Label]
		} else if predecessorStates != nil {
			expectedState = predecessorStates[operation.Label]
		}
		key, _, prepareErr := c.Prepare(ctx, authority, PrepareOperationRequest{
			ReleaseID: input.ReleaseID, Phase: input.Phase, AWSAccountID: authority.AWSAccountID, AWSRegion: authority.AWSRegion,
			Identity: identity, Cohort: cohort, ExpectedAgentState: expectedState,
			RecoveryEndpoint: input.RecoveryEndpoint, RunID: operation.RunID, RunAttempt: operation.RunAttempt,
			PreparedAt: time.UnixMilli(operation.PreparedAtMS).UTC(), ExpiresAt: time.UnixMilli(operation.ExpiresAtMS).UTC(),
		})
		if prepareErr != nil {
			return PreparedClosure{}, fmt.Errorf("prepare closure %s: %w", operation.Label, prepareErr)
		}
		keys[index] = key
	}
	return PreparedClosure{IntentReference: blobReference(intentBlob), Intent: intent, OperationKeys: keys}, nil
}

//nolint:gocyclo // Retry admission requires exact prior terminal authority.
func (c *Consumer) validateClosurePredecessor(ctx context.Context, authority *Authority, input *ClosureIntentInput,
	previous *PreparedClosure,
) (ClosureAttemptReference, map[string]StateReference, error) {
	if authority == nil || input == nil || previous == nil {
		return ClosureAttemptReference{}, nil, fmt.Errorf("%w: closure predecessor", errStateConflict)
	}
	records, err := c.validateClosureBundle(ctx, *previous, false)
	if err != nil || previous.Intent.Attempt+1 != input.Attempt || previous.Intent.ReleaseID != input.ReleaseID ||
		previous.Intent.Phase != input.Phase || previous.Intent.Color != input.Color ||
		previous.Intent.AdmissionEndpoint != input.AdmissionEndpoint || previous.Intent.RecoveryEndpoint != input.RecoveryEndpoint {
		return ClosureAttemptReference{}, nil, fmt.Errorf("%w: closure predecessor", errStateConflict)
	}
	authorityRaw, encodeErr := CanonicalJSON(*authority)
	if encodeErr != nil || previous.Intent.AuthoritySHA256 != Digest(authorityRaw) {
		return ClosureAttemptReference{}, nil, fmt.Errorf("%w: closure predecessor authority", errStateConflict)
	}
	states := make(map[string]StateReference, len(records))
	completeEvidence := 0
	priorRunIDs := make(map[string]struct{}, len(records))
	for index := range records {
		record := &records[index]
		if record.Status != OperationCanceled || record.Terminal == nil || !validCanceledTerminal(record.Terminal) {
			return ClosureAttemptReference{}, nil, fmt.Errorf("%w: closure predecessor is not CANCELED", errStateConflict)
		}
		states[record.Label] = record.AgentState
		priorRunIDs[record.Operation.RunID] = struct{}{}
		if _, evidenceErr := c.loadClosureEvidence(ctx, previous, index, record); evidenceErr == nil {
			completeEvidence++
		} else if !errors.Is(evidenceErr, errStateNotFound) {
			return ClosureAttemptReference{}, nil, evidenceErr
		}
	}
	if completeEvidence == len(records) {
		return ClosureAttemptReference{}, nil, fmt.Errorf("%w: closure predecessor already has complete evidence", errStateConflict)
	}
	for _, runID := range input.RunIDs {
		if _, reused := priorRunIDs[runID]; reused {
			return ClosureAttemptReference{}, nil, fmt.Errorf("%w: closure successor reused RunID", errStateConflict)
		}
	}
	return ClosureAttemptReference{Attempt: previous.Intent.Attempt, Intent: previous.IntentReference,
		OperationKeys: previous.OperationKeys}, states, nil
}

func closureProjection(authority Authority, color string) (CohortPlan, map[string]FixedIdentity, error) { //nolint:gocritic // Authority is one immutable snapshot.
	cohort, err := cohortFor(Plan{Cohorts: authority.Cohorts}, color)
	if err != nil {
		return CohortPlan{}, nil, err
	}
	identities := make(map[string]FixedIdentity, len(closureLabels))
	for index := range authority.Identities {
		identity := authority.Identities[index]
		if identity.Color == color {
			identities[identity.Label] = identity
		}
	}
	if len(identities) != len(closureLabels) {
		return CohortPlan{}, nil, fmt.Errorf("%w: closure identity set", errInvalidAuthority)
	}
	for _, label := range closureLabels {
		if _, ok := identities[label]; !ok {
			return CohortPlan{}, nil, fmt.Errorf("%w: closure identity %s", errInvalidAuthority, label)
		}
	}
	return cohort, identities, nil
}

// ValidatePreparedClosure verifies the immutable intent and requires all four
// operations to remain PREPARED before the first public admission attempt.
//
//nolint:gocritic // PreparedClosure is one immutable authority snapshot.
func (c *Consumer) ValidatePreparedClosure(ctx context.Context, prepared PreparedClosure) error {
	_, err := c.validateClosureBundle(ctx, prepared, true)
	return err
}

//nolint:gocritic // PreparedClosure is one immutable authority snapshot.
func (c *Consumer) validateClosureBundle(ctx context.Context, prepared PreparedClosure, requirePrepared bool) ([]OperationRecord, error) { //nolint:gocyclo // Closed immutable union is validated explicitly.
	if c == nil || c.Blobs == nil || prepared.Intent.Schema != closureIntentSchema ||
		prepared.Intent.Attempt == 0 || prepared.Intent.Attempt > MaxClosureAttempts ||
		len(prepared.Intent.Operations) != len(closureLabels) || prepared.Intent.AdmissionEndpoint.Port != 443 ||
		!validDNS(prepared.Intent.AdmissionEndpoint.Host) || !validBase64Raw32(prepared.Intent.AdmissionEndpoint.ServerPublicKeyB64) ||
		prepared.Intent.RecoveryEndpoint.Port != 443 || !validDNS(prepared.Intent.RecoveryEndpoint.Host) ||
		!validBase64Raw32(prepared.Intent.RecoveryEndpoint.ServerPublicKeyB64) ||
		prepared.Intent.AdmissionEndpoint == prepared.Intent.RecoveryEndpoint {
		return nil, fmt.Errorf("%w: prepared closure", errInvalidAuthority)
	}
	if prepared.Intent.Attempt == 1 && prepared.Intent.Previous != nil ||
		prepared.Intent.Attempt > 1 && !validClosureAttemptReference(prepared.Intent.Previous, prepared.Intent.Attempt-1) {
		return nil, fmt.Errorf("%w: prepared closure predecessor", errInvalidAuthority)
	}
	raw, err := CanonicalJSON(prepared.Intent)
	if err != nil {
		return nil, err
	}
	blob, err := c.Blobs.Load(ctx, prepared.IntentReference.Key)
	if err != nil || blobReference(blob) != prepared.IntentReference || !bytes.Equal(blob.Body, raw) {
		return nil, fmt.Errorf("%w: closure intent readback", errStateConflict)
	}
	records := make([]OperationRecord, 0, len(prepared.OperationKeys))
	for index, key := range prepared.OperationKeys {
		record, _, loadErr := loadOperation(ctx, c.Blobs, key)
		want := prepared.Intent.Operations[index]
		if loadErr != nil || record.Operation.RunID != want.RunID ||
			record.Operation.RunAttempt != prepared.Intent.Attempt || want.RunAttempt != prepared.Intent.Attempt ||
			record.Label != want.Label || key != prepared.OperationKeys[index] {
			return nil, fmt.Errorf("%w: closure operation %s", errStateConflict, want.Label)
		}
		if requirePrepared && record.Status != OperationPrepared {
			return nil, fmt.Errorf("%w: closure operation %s already dispatched", errStateConflict, want.Label)
		}
		records = append(records, record)
	}
	return records, nil
}

func validClosureAttemptReference(value *ClosureAttemptReference, attempt uint64) bool {
	if value == nil || value.Attempt != attempt || validateStateReference(value.Intent) != nil {
		return false
	}
	seen := make(map[string]struct{}, len(value.OperationKeys))
	for _, key := range value.OperationKeys {
		if !validText(key) {
			return false
		}
		if _, exists := seen[key]; exists {
			return false
		}
		seen[key] = struct{}{}
	}
	return true
}

func closureEvidenceKey(intent ClosureIntent, label string) string { //nolint:gocritic // Intent is one immutable value.
	return fmt.Sprintf("releases/%s/closure-evidence/%s/%s/attempt-%d/%s", intent.ReleaseID, intent.Phase,
		intent.Color, intent.Attempt, label)
}

func exactClosureNoReply(err error, endpoint string) (*qurl.EndpointNoReplyError, bool) {
	var noReply *qurl.EndpointNoReplyError
	if !errors.Is(err, qurl.ErrEndpointNoReply) || !errors.As(err, &noReply) || noReply == nil ||
		noReply.Endpoint != endpoint || noReply.Attempts != 1 || noReply.Elapsed <= 0 ||
		!nativeudp.IsInitialKnockNoReply(noReply.Last) {
		return nil, false
	}
	return noReply, true
}

func (c *Consumer) persistClosureEvidence(ctx context.Context, prepared *PreparedClosure, record *OperationRecord,
	noReply *qurl.EndpointNoReplyError,
) (StateReference, error) {
	if prepared == nil || record == nil || noReply == nil {
		return StateReference{}, fmt.Errorf("%w: closure evidence", errInvalidAuthority)
	}
	evidence := ClosureNoReplyEvidence{Schema: closureEvidenceSchema, ReleaseID: prepared.Intent.ReleaseID,
		Phase: prepared.Intent.Phase, Attempt: prepared.Intent.Attempt, Color: prepared.Intent.Color, Label: record.Label,
		IntentSHA256: prepared.IntentReference.SHA256, OperationID: record.Operation.OperationID,
		Endpoint: noReply.Endpoint, AddressAttempts: noReply.Attempts, ElapsedNanos: int64(noReply.Elapsed),
		Outcome: "endpoint_no_reply"}
	raw, err := CanonicalJSON(evidence)
	if err != nil {
		return StateReference{}, err
	}
	blob, err := persistImmutable(ctx, c.Blobs, closureEvidenceKey(prepared.Intent, record.Label), "closure-no-reply-evidence", raw)
	if err != nil {
		return StateReference{}, err
	}
	return blobReference(blob), nil
}

//nolint:gocyclo // Receipt validation is exact and value-oriented.
func (c *Consumer) loadClosureEvidence(ctx context.Context, prepared *PreparedClosure, index int,
	record *OperationRecord,
) (StateReference, error) {
	if prepared == nil || record == nil {
		return StateReference{}, fmt.Errorf("%w: closure evidence", errInvalidAuthority)
	}
	if index < 0 || index >= len(prepared.Intent.Operations) || record.Label != prepared.Intent.Operations[index].Label {
		return StateReference{}, fmt.Errorf("%w: closure evidence index", errStateConflict)
	}
	blob, err := c.Blobs.Load(ctx, closureEvidenceKey(prepared.Intent, record.Label))
	if err != nil {
		return StateReference{}, err
	}
	var evidence ClosureNoReplyEvidence
	if decodeErr := json.Unmarshal(blob.Body, &evidence); decodeErr != nil {
		return StateReference{}, fmt.Errorf("%w: closure evidence JSON", errStateConflict)
	}
	canonical, encodeErr := CanonicalJSON(evidence)
	evidenceKey := closureEvidenceKey(prepared.Intent, record.Label)
	expectedOperationID := Digest([]byte("layerv/matched-cohort-immutable/v1\x00closure-no-reply-evidence\x00" +
		evidenceKey + "\x00" + Digest(canonical)))
	expectedEndpoint := net.JoinHostPort(prepared.Intent.AdmissionEndpoint.Host,
		strconv.Itoa(prepared.Intent.AdmissionEndpoint.Port))
	if encodeErr != nil || !bytes.Equal(canonical, blob.Body) || Digest(blob.Body) != blob.SHA256 ||
		blob.Key != evidenceKey || blob.PreviousVersion != "" || blob.OperationID != expectedOperationID ||
		evidence.Schema != closureEvidenceSchema || evidence.ReleaseID != prepared.Intent.ReleaseID ||
		evidence.Phase != prepared.Intent.Phase || evidence.Attempt != prepared.Intent.Attempt ||
		evidence.Color != prepared.Intent.Color || evidence.Label != record.Label ||
		evidence.IntentSHA256 != prepared.IntentReference.SHA256 || evidence.OperationID != record.Operation.OperationID ||
		evidence.Endpoint != expectedEndpoint || evidence.AddressAttempts != 1 || evidence.ElapsedNanos <= 0 ||
		evidence.Outcome != "endpoint_no_reply" {
		return StateReference{}, fmt.Errorf("%w: closure evidence binding", errStateConflict)
	}
	return blobReference(blob), nil
}

type closureAttemptResult struct {
	index    int
	live     *LiveSession
	admitErr error
	terminal SessionTerminal
	closeErr error
}

// RunPreparedClosure attempts all four authenticated admissions concurrently.
// A successful admission is immediately exact-retired but always fails this
// phase. Failed/ambiguous admissions are recovered through the source-fenced
// endpoint and must finish as CANCELED, never CLOSING or CLOSED.
func (c *Consumer) RunPreparedClosure(ctx context.Context, authority Authority, prepared PreparedClosure, attemptBudget, recoveryBudget time.Duration) (ClosureOutcome, error) { //nolint:gocritic,gocyclo,gocognit // The closed four-way failure matrix stays explicit.
	authorityRaw, encodeErr := CanonicalJSON(authority)
	if ValidateAuthority(authority) != nil || encodeErr != nil || Digest(authorityRaw) != prepared.Intent.AuthoritySHA256 {
		return ClosureOutcome{}, fmt.Errorf("%w: closure execution authority", errInvalidAuthority)
	}
	if attemptBudget <= 0 || attemptBudget > 5*time.Second || recoveryBudget <= 0 || recoveryBudget > 30*time.Second {
		return ClosureOutcome{}, fmt.Errorf("%w: closure budgets", errInvalidAuthority)
	}
	records, err := c.validateClosureBundle(ctx, prepared, false)
	if err != nil {
		return ClosureOutcome{}, err
	}
	attemptCtx, cancelAttempts := context.WithTimeout(ctx, attemptBudget)
	defer cancelAttempts()
	results := make(chan closureAttemptResult, len(prepared.OperationKeys))
	for index, key := range prepared.OperationKeys {
		go func(index int, key string, status string) { //nolint:gosec // Recovery must outlive a canceled admission context and has a strict bound.
			var live *LiveSession
			var admitErr error
			if status == OperationPrepared {
				live, admitErr = c.admitObserved(attemptCtx, key, func(observeCtx context.Context, record OperationRecord, observed error) error {
					expectedEndpoint := net.JoinHostPort(prepared.Intent.AdmissionEndpoint.Host,
						strconv.Itoa(prepared.Intent.AdmissionEndpoint.Port))
					noReply, ok := exactClosureNoReply(observed, expectedEndpoint)
					if !ok {
						return nil
					}
					_, persistErr := c.persistClosureEvidence(observeCtx, &prepared, &record, noReply)
					return persistErr
				})
			} else {
				admitErr = errSessionRecoveryRequired
			}
			result := closureAttemptResult{index: index, live: live, admitErr: admitErr}
			recoveryCtx, cancelRecovery := context.WithTimeout(context.Background(), recoveryBudget)
			defer cancelRecovery()
			if live != nil {
				result.terminal, result.closeErr = c.Retire(recoveryCtx, key, live)
			} else {
				result.terminal, result.closeErr = c.Recover(recoveryCtx, key)
			}
			results <- result
		}(index, key, records[index].Status)
	}
	ordered := make([]closureAttemptResult, len(prepared.OperationKeys))
	for range prepared.OperationKeys {
		result := <-results
		ordered[result.index] = result
	}
	var failures []error
	selectors := make(map[string]struct{}, 3)
	outcome := ClosureOutcome{AttemptedLabels: make([]string, 0, len(closureLabels)),
		CanceledOperationKeys: make([]string, 0, len(closureLabels)), UniqueFRPSSelectors: make([]string, 0, 3),
		Evidence: make([]StateReference, 0, len(closureLabels))}
	_, identities, projectionErr := closureProjection(authority, prepared.Intent.Color)
	if projectionErr != nil {
		return ClosureOutcome{}, projectionErr
	}
	for index := range ordered {
		result := ordered[index]
		label := prepared.Intent.Operations[index].Label
		outcome.AttemptedLabels = append(outcome.AttemptedLabels, label)
		selectors[identities[label].Selector.ResourceID] = struct{}{}
		if result.live != nil {
			outcome.AdmissionSucceeded = true
			failures = append(failures, fmt.Errorf("closed admission unexpectedly succeeded for %s", label))
		}
		if result.admitErr == nil {
			failures = append(failures, fmt.Errorf("closed admission returned no denial for %s", label))
		}
		if result.closeErr != nil || result.terminal.State != OperationCanceled || result.terminal.WasAdmitted {
			failures = append(failures, fmt.Errorf("closed admission did not become CANCELED for %s: %w", label,
				errors.Join(result.admitErr, result.closeErr, terminalError(result.terminal))))
			continue
		}
		outcome.CanceledOperationKeys = append(outcome.CanceledOperationKeys, prepared.OperationKeys[index])
		record, _, loadErr := loadOperation(ctx, c.Blobs, prepared.OperationKeys[index])
		evidence, evidenceErr := c.loadClosureEvidence(ctx, &prepared, index, &record)
		if loadErr != nil || evidenceErr != nil {
			failures = append(failures, fmt.Errorf("closed admission lacked durable no-reply evidence for %s: %w", label,
				errors.Join(result.admitErr, loadErr, evidenceErr)))
			continue
		}
		outcome.Evidence = append(outcome.Evidence, evidence)
	}
	for _, resourceID := range []string{selectorResourceA, selectorResourceB, selectorResourceC} {
		if _, ok := selectors[resourceID]; !ok {
			failures = append(failures, fmt.Errorf("closed admission did not cover FRPS selector %s", resourceID))
			continue
		}
		outcome.UniqueFRPSSelectors = append(outcome.UniqueFRPSSelectors, resourceID)
	}
	if len(selectors) != 3 {
		failures = append(failures, errors.New("closed admission selector set is not exact"))
	}
	if len(failures) != 0 {
		return ClosureOutcome{}, errors.Join(failures...)
	}
	return outcome, nil
}

// SettlePreparedClosure returns the exact bounded-successor authority. It does
// no network I/O and cannot turn a successful admission into retry authority.
func (c *Consumer) SettlePreparedClosure(ctx context.Context, prepared PreparedClosure) (ClosureSettlement, error) { //nolint:gocritic // Prepared closure is an immutable snapshot.
	records, err := c.validateClosureBundle(ctx, prepared, false)
	if err != nil {
		return ClosureSettlement{}, err
	}
	settlement := ClosureSettlement{Attempt: prepared.Intent.Attempt,
		OperationKeys:  append([]string(nil), prepared.OperationKeys[:]...),
		TerminalStates: make([]string, 0, len(records)), EvidencePresent: make([]bool, 0, len(records)), RetryRequired: true}
	completeEvidence := 0
	for index := range records {
		record := &records[index]
		if record.Status != OperationCanceled || record.Terminal == nil || !validCanceledTerminal(record.Terminal) {
			return ClosureSettlement{}, fmt.Errorf("%w: closure attempt is not exact CANCELED", errStateConflict)
		}
		settlement.TerminalStates = append(settlement.TerminalStates, OperationCanceled)
		_, evidenceErr := c.loadClosureEvidence(ctx, &prepared, index, record)
		switch {
		case evidenceErr == nil:
			completeEvidence++
			settlement.EvidencePresent = append(settlement.EvidencePresent, true)
		case errors.Is(evidenceErr, errStateNotFound):
			settlement.EvidencePresent = append(settlement.EvidencePresent, false)
		default:
			return ClosureSettlement{}, evidenceErr
		}
	}
	if completeEvidence == len(records) || prepared.Intent.Attempt >= MaxClosureAttempts {
		return ClosureSettlement{}, fmt.Errorf("%w: closure successor is unavailable", errStateConflict)
	}
	return settlement, nil
}
