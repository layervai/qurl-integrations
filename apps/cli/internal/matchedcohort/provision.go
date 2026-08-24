package matchedcohort

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	qurl "github.com/layervai/qurl-go/qurl"
)

const connectorStateSchema = 1

// EnrollmentAuthority supplies the one run-scoped account credential and OTP.
// Its implementation owns the shared credential-writer mutex. No returned
// bearer is written into Plan, Authority, reports, or logs.
type EnrollmentAuthority interface {
	VerifyEnrollmentCredential(context.Context, string) (EnrollmentCredentialReceipt, error)
	EnrollmentCredential(context.Context, IdentityPlan) (string, error)
	OTP(context.Context, IdentityPlan, qurl.AgentOTPChallenge) (string, error)
}

// CredentialWriterOperation is the exact durable mutex binding.
type CredentialWriterOperation struct {
	Schema          int    `json:"schema"`
	OwnerSubject    string `json:"owner_subject"`
	Operation       string `json:"operation"`
	GenerationID    string `json:"generation_id"`
	PlanSHA256      string `json:"plan_sha256"`
	InvocationToken string `json:"invocation_token"`
}

// CredentialWriterLock is the one shared durable mutex used by normal-release,
// provision, and rotate operators for this dedicated owner. The adapter must
// serialize same-token callers too and let a successor resume only the exact
// persisted operation. It has no lease or TTL expiry.
type CredentialWriterLock interface {
	WithExclusive(context.Context, CredentialWriterOperation, func(context.Context) error) error
}

type connectorState struct {
	Schema     int                       `json:"schema"`
	Generation string                    `json:"generation_id"`
	Color      string                    `json:"color"`
	Label      string                    `json:"label"`
	Request    *connectorResourceRequest `json:"request,omitempty"`
	Resolution *connectorResolution      `json:"resolution,omitempty"`
}

type connectorResourceRequest struct {
	ConnectorID        string `json:"connector_id"`
	ExpectedResourceID string `json:"expected_resource_id,omitempty"`
	RequestNonce       string `json:"request_nonce"`
}

type connectorResolution struct {
	ConnectorID        string `json:"connector_id"`
	ResourceID         string `json:"resource_id"`
	CRID               string `json:"crid"`
	ConnectorRoutingID string `json:"connector_routing_id"`
	KnockResourceID    string `json:"knock_resource_id"`
	FoundExisting      bool   `json:"found_existing"`
}

// Provisioner performs the real qurl-go registration and resource exchanges.
// The injected stores checkpoint SDK and request state before each network
// boundary. ProvisionOne is safe to resume with the same plan and stores.
type Provisioner struct {
	Blobs           BlobAuthority
	Credentials     EnrollmentAuthority
	WriterLock      CredentialWriterLock
	InvocationToken string
	Runtime         IdentityRuntime
}

// RuntimeBinding is the non-secret completed identity returned by a runtime.
type RuntimeBinding struct {
	AgentID              string
	PublicKeyB64         string
	DeviceAPIKeyID       string
	CellID               string
	AssignmentGeneration int64
	Endpoint             qurl.NHPUDPEndpoint
	value                any
}

// IdentityRuntime is the narrow qurl-go exchange seam used by hermetic tests.
type IdentityRuntime interface {
	Connect(context.Context, qurl.AgentStateStore, CohortPlan, IdentityPlan, string, EnrollmentAuthority) (*RuntimeBinding, error)
	Resolve(context.Context, *RuntimeBinding, *qurl.NativeConnectorResourceRequest) (*qurl.ConnectorResourceResolution, error)
	Close(*RuntimeBinding)
}

type qurlIdentityRuntime struct{}

// Connect enrolls or resumes the exact fixed qurl-go identity.
func (qurlIdentityRuntime) Connect(ctx context.Context, store qurl.AgentStateStore, cohort CohortPlan, identity IdentityPlan, credential string, enrollment EnrollmentAuthority) (*RuntimeBinding, error) { //nolint:gocritic // Closed qurl option values are intentionally copied.
	hub := qurl.HubBootstrap{Host: cohort.HubHost, Port: cohort.HubPort, ServerPublicKeyB64: cohort.HubServerPublicKeyB64}
	_, binding, err := qurl.ConnectAgentRuntime(ctx, store,
		qurl.WithAgentRuntimeIdentity(identity.AgentID), qurl.WithAgentRuntimeHub(hub),
		qurl.WithAgentRuntimeEnrollmentCredential(credential),
		qurl.WithAgentRuntimeOTPProvider(func(callCtx context.Context, challenge qurl.AgentOTPChallenge) (string, error) {
			return enrollment.OTP(callCtx, identity, challenge)
		}),
		qurl.WithAgentRuntimeAllowedRegistrationKeyKinds(qurl.RegistrationKeyKindAccount))
	if err != nil {
		return nil, err
	}
	assignment := binding.Assignment()
	return &RuntimeBinding{AgentID: binding.AgentID, PublicKeyB64: binding.PublicKeyB64, DeviceAPIKeyID: binding.DeviceAPIKeyID,
		CellID: assignment.CellID, AssignmentGeneration: assignment.AssignmentGeneration, Endpoint: assignment.Endpoint, value: binding}, nil
}

// Resolve performs the authenticated native connector-resource exchange.
func (qurlIdentityRuntime) Resolve(ctx context.Context, binding *RuntimeBinding, request *qurl.NativeConnectorResourceRequest) (*qurl.ConnectorResourceResolution, error) {
	native, ok := binding.value.(*qurl.AgentRuntimeBinding)
	if !ok {
		return nil, errors.New("fixed runtime binding is not qurl-go native")
	}
	return qurl.ResolveRegisteredAgentConnectorResource(ctx, native, request)
}

// Close destroys the runtime's in-memory private-key owner.
func (qurlIdentityRuntime) Close(binding *RuntimeBinding) {
	if binding == nil {
		return
	}
	if native, ok := binding.value.(*qurl.AgentRuntimeBinding); ok {
		native.Destroy()
	}
}

// ProvisionedGeneration is one complete authority and its immutable pointer.
type ProvisionedGeneration struct {
	Authority Authority
	Reference StateReference
}

// Provision creates or resumes all eight fixed identities under one writer lock.
func (p *Provisioner) Provision(ctx context.Context, plan Plan) (ProvisionedGeneration, error) { //nolint:gocritic // Provision pins one immutable plan value.
	if err := ValidatePlan(plan); err != nil {
		return ProvisionedGeneration{}, err
	}
	if p == nil || p.Blobs == nil || p.Credentials == nil || p.WriterLock == nil || !hex64Pattern.MatchString(p.InvocationToken) {
		return ProvisionedGeneration{}, fmt.Errorf("%w: provisioner dependencies", errInvalidAuthority)
	}
	planRaw, err := CanonicalJSON(plan)
	if err != nil {
		return ProvisionedGeneration{}, err
	}
	if _, err := persistImmutable(ctx, p.Blobs, fmt.Sprintf("generations/%s/plan", plan.GenerationID), "plan", planRaw); err != nil {
		return ProvisionedGeneration{}, fmt.Errorf("persist generation plan before network: %w", err)
	}
	var provisioned ProvisionedGeneration
	lockOperation := CredentialWriterOperation{Schema: 1, OwnerSubject: plan.OwnerSubject, Operation: "provision",
		GenerationID: plan.GenerationID, PlanSHA256: Digest(planRaw), InvocationToken: p.InvocationToken}
	err = p.WriterLock.WithExclusive(ctx, lockOperation, func(lockedCtx context.Context) error {
		receipt, verifyErr := p.Credentials.VerifyEnrollmentCredential(lockedCtx, plan.OwnerSubject)
		if verifyErr != nil {
			return fmt.Errorf("verify enrollment credential owner before network: %w", verifyErr)
		}
		if validateErr := validateEnrollmentCredentialReceipt(receipt, plan.OwnerSubject); validateErr != nil {
			return validateErr
		}
		receiptRaw, encodeErr := CanonicalJSON(receipt)
		if encodeErr != nil {
			return encodeErr
		}
		receiptBlob, persistErr := persistImmutable(lockedCtx, p.Blobs,
			fmt.Sprintf("generations/%s/enrollment-credential-receipt", plan.GenerationID), "enrollment-credential-receipt", receiptRaw)
		if persistErr != nil {
			return fmt.Errorf("persist enrollment credential receipt before network: %w", persistErr)
		}
		result := Authority{Schema: plan.Schema, Environment: plan.Environment, GenerationID: plan.GenerationID,
			OwnerSubject: plan.OwnerSubject, AWSAccountID: plan.AWSAccountID, AWSRegion: plan.AWSRegion,
			NHPSourceSHA: plan.NHPSourceSHA, QURLGoSourceSHA: plan.QURLGoSourceSHA,
			EnrollmentCredentialReceipt: blobReference(receiptBlob),
			Cohorts:                     append([]CohortPlan(nil), plan.Cohorts...), Identities: make([]FixedIdentity, 0, len(plan.Identities))}
		for _, identityPlan := range plan.Identities {
			identity, provisionErr := p.ProvisionOne(lockedCtx, plan, identityPlan)
			if provisionErr != nil {
				return fmt.Errorf("provision %s %s: %w", identityPlan.Color, identityPlan.Label, provisionErr)
			}
			result.Identities = append(result.Identities, identity)
		}
		if validateErr := ValidateAuthority(result); validateErr != nil {
			return validateErr
		}
		authorityRaw, encodeErr := CanonicalJSON(result)
		if encodeErr != nil {
			return encodeErr
		}
		committed, commitErr := persistImmutable(lockedCtx, p.Blobs, fmt.Sprintf("generations/%s/authority", plan.GenerationID), "authority", authorityRaw)
		if commitErr != nil {
			return fmt.Errorf("persist complete generation authority: %w", commitErr)
		}
		provisioned = ProvisionedGeneration{Authority: result, Reference: blobReference(committed)}
		return nil
	})
	if err != nil {
		return ProvisionedGeneration{}, err
	}
	return provisioned, nil
}

func validateEnrollmentCredentialReceipt(receipt EnrollmentCredentialReceipt, expectedOwner string) error { //nolint:gocritic // Closed receipt value is immutable.
	expectedScopes := []string{"qurl:agent", "qurl:read", "qurl:write"}
	if receipt.Schema != 1 || receipt.OwnerID != expectedOwner || receipt.AuthType != "api_key" || receipt.Kind != "api_key" ||
		!validText(receipt.KeyID) || len(receipt.KeyPrefix) != 12 || !validText(receipt.KeyPrefix) || !slices.Equal(receipt.Scopes, expectedScopes) {
		return fmt.Errorf("%w: enrollment credential identity", errInvalidAuthority)
	}
	return nil
}

// ProvisionOne creates or resumes one identity; callers must hold WriterLock.
func (p *Provisioner) ProvisionOne(ctx context.Context, plan Plan, identityPlan IdentityPlan) (FixedIdentity, error) { //nolint:gocritic // Closed plan values are intentionally copied.
	cohort, err := cohortFor(plan, identityPlan.Color)
	if err != nil {
		return FixedIdentity{}, err
	}
	stateKey := fmt.Sprintf("generations/%s/%s/%s/agent-state", plan.GenerationID, identityPlan.Color, identityPlan.Label)
	stateStore, err := NewDurableAgentStateStore(p.Blobs, stateKey)
	if err != nil {
		return FixedIdentity{}, err
	}
	credential, err := p.Credentials.EnrollmentCredential(ctx, identityPlan)
	if err != nil || !validText(credential) {
		return FixedIdentity{}, errors.New("obtain enrollment credential")
	}
	runtime := p.Runtime
	if runtime == nil {
		runtime = qurlIdentityRuntime{}
	}
	binding, err := runtime.Connect(ctx, stateStore, cohort, identityPlan, credential, p.Credentials)
	if err != nil {
		return FixedIdentity{}, fmt.Errorf("connect fixed agent runtime: %w", err)
	}
	defer runtime.Close(binding)
	if binding.AgentID != identityPlan.AgentID || !validBase64Raw32(binding.PublicKeyB64) || binding.DeviceAPIKeyID == "" ||
		binding.CellID != cohort.CellID || binding.AssignmentGeneration != cohort.AssignmentGeneration ||
		binding.Endpoint != cohort.CellEndpoint {
		return FixedIdentity{}, errors.New("fixed agent runtime contradicts signed cohort")
	}

	connectorKey := fmt.Sprintf("generations/%s/%s/%s/connector-state", plan.GenerationID, identityPlan.Color, identityPlan.Label)
	resolution, connectorRef, err := p.resolveConnector(ctx, connectorKey, plan.GenerationID, identityPlan, runtime, binding)
	if err != nil {
		return FixedIdentity{}, err
	}
	agentRef, err := stateStore.Reference(ctx)
	if err != nil {
		return FixedIdentity{}, fmt.Errorf("read committed agent state reference: %w", err)
	}
	return FixedIdentity{Color: identityPlan.Color, Label: identityPlan.Label, OwnerID: identityPlan.OwnerID,
		AgentID: identityPlan.AgentID, AgentPublicKeyB64: binding.PublicKeyB64, AgentKeySchemaVersion: AgentKeySchemaVersion,
		EnrollmentCredentialKind: EnrollmentCredentialKindAccount, ConnectorIDClaim: "", DeviceAPIKeyID: binding.DeviceAPIKeyID,
		ConnectorID: resolution.ConnectorID, ResourceID: resolution.ResourceID, CRID: resolution.CRID,
		ConnectorRoutingID: resolution.ConnectorRoutingID, KnockResourceID: resolution.KnockResourceID,
		Selector: identityPlan.Selector, AgentState: agentRef, ConnectorState: connectorRef}, nil
}

func (p *Provisioner) resolveConnector(ctx context.Context, key, generation string, plan IdentityPlan, runtime IdentityRuntime, binding *RuntimeBinding) (connectorResolution, StateReference, error) { //nolint:gocritic // Closed identity values are intentionally copied.
	state, blob, err := loadConnectorState(ctx, p.Blobs, key, generation, plan)
	if err != nil {
		return connectorResolution{}, StateReference{}, err
	}
	if state.Resolution != nil {
		if state.Request != nil || state.Resolution.ConnectorID != plan.ConnectorID {
			return connectorResolution{}, StateReference{}, fmt.Errorf("%w: completed connector state", errStateConflict)
		}
		return *state.Resolution, blobReference(blob), nil
	}
	if state.Request == nil {
		request, err := qurl.NewNativeConnectorResourceRequest(plan.ConnectorID, "")
		if err != nil {
			return connectorResolution{}, StateReference{}, err
		}
		state.Request = &connectorResourceRequest{ConnectorID: request.ConnectorID, ExpectedResourceID: request.ExpectedResourceID, RequestNonce: request.RequestNonce}
		blob, err = commitConnectorState(ctx, p.Blobs, key, blob, state)
		if err != nil {
			return connectorResolution{}, StateReference{}, err
		}
	}
	request := &qurl.NativeConnectorResourceRequest{ConnectorID: state.Request.ConnectorID,
		ExpectedResourceID: state.Request.ExpectedResourceID, RequestNonce: state.Request.RequestNonce}
	resolved, err := runtime.Resolve(ctx, binding, request)
	if err != nil {
		return connectorResolution{}, StateReference{}, fmt.Errorf("resolve fixed Connector resource: %w", err)
	}
	if resolved == nil || resolved.Resource == nil {
		return connectorResolution{}, StateReference{}, errors.New("fixed Connector resource response is empty")
	}
	resolution := connectorResolution{ConnectorID: plan.ConnectorID, ResourceID: resolved.Resource.ResourceID, CRID: resolved.Resource.CRID,
		ConnectorRoutingID: resolved.Resource.ConnectorRoutingID, KnockResourceID: resolved.Resource.KnockResourceID, FoundExisting: resolved.FoundExisting}
	state.Request = nil
	state.Resolution = &resolution
	blob, err = commitConnectorState(ctx, p.Blobs, key, blob, state)
	if err != nil {
		return connectorResolution{}, StateReference{}, err
	}
	return resolution, blobReference(blob), nil
}

func loadConnectorState(ctx context.Context, authority BlobAuthority, key, generation string, plan IdentityPlan) (connectorState, Blob, error) { //nolint:gocritic // Closed identity values are intentionally copied.
	blob, err := authority.Load(ctx, key)
	if errors.Is(err, errStateNotFound) {
		return connectorState{Schema: connectorStateSchema, Generation: generation, Color: plan.Color, Label: plan.Label}, Blob{Key: key}, nil
	}
	if err != nil {
		return connectorState{}, Blob{}, err
	}
	if Digest(blob.Body) != blob.SHA256 || !validText(blob.VersionID) {
		return connectorState{}, Blob{}, fmt.Errorf("%w: connector state blob", errStateConflict)
	}
	var state connectorState
	if err := json.Unmarshal(blob.Body, &state); err != nil {
		return connectorState{}, Blob{}, fmt.Errorf("%w: connector state JSON", errStateConflict)
	}
	canonical, _ := CanonicalJSON(state)
	if !bytes.Equal(canonical, blob.Body) || state.Schema != connectorStateSchema || state.Generation != generation || state.Color != plan.Color || state.Label != plan.Label {
		return connectorState{}, Blob{}, fmt.Errorf("%w: connector state binding", errStateConflict)
	}
	return state, cloneBlob(blob), nil
}

func commitConnectorState(ctx context.Context, authority BlobAuthority, key string, previous Blob, state connectorState) (Blob, error) { //nolint:gocritic // Previous is one immutable receipt.
	raw, err := CanonicalJSON(state)
	if err != nil {
		return Blob{}, err
	}
	digest := Digest(raw)
	operationID := Digest([]byte("layerv/matched-cohort-connector-state/v1\x00" + key + "\x00" + previous.VersionID + "\x00" + digest))
	candidate := BlobCandidate{Key: key, ExpectedVersion: previous.VersionID, OperationID: operationID, SHA256: digest, Body: raw}
	committed, err := authority.Commit(ctx, candidate)
	if err != nil {
		observed, loadErr := authority.Load(ctx, key)
		if loadErr != nil || !sameCommittedBlob(observed, candidate) {
			return Blob{}, fmt.Errorf("%w: connector state commit", errStateAmbiguous)
		}
		committed = observed
	}
	if !sameCommittedBlob(committed, candidate) {
		return Blob{}, fmt.Errorf("%w: connector state commit receipt", errStateConflict)
	}
	return committed, nil
}

func blobReference(blob Blob) StateReference { //nolint:gocritic // Reference intentionally snapshots one receipt value.
	return StateReference{Key: blob.Key, VersionID: blob.VersionID, SHA256: blob.SHA256}
}

func persistImmutable(ctx context.Context, authority BlobAuthority, key, kind string, raw []byte) (Blob, error) {
	digest := Digest(raw)
	operationID := Digest([]byte("layerv/matched-cohort-immutable/v1\x00" + kind + "\x00" + key + "\x00" + digest))
	current, err := authority.Load(ctx, key)
	if err == nil {
		if current.PreviousVersion != "" || current.OperationID != operationID || current.SHA256 != digest || !bytes.Equal(current.Body, raw) {
			return Blob{}, fmt.Errorf("%w: immutable %s drift", errStateConflict, kind)
		}
		return current, nil
	}
	if !errors.Is(err, errStateNotFound) {
		return Blob{}, err
	}
	candidate := BlobCandidate{Key: key, OperationID: operationID, SHA256: digest, Body: raw}
	committed, err := authority.Commit(ctx, candidate)
	if err != nil {
		observed, loadErr := authority.Load(ctx, key)
		if loadErr != nil || !sameCommittedBlob(observed, candidate) {
			return Blob{}, fmt.Errorf("%w: persist immutable %s", errStateAmbiguous, kind)
		}
		committed = observed
	}
	if !sameCommittedBlob(committed, candidate) {
		return Blob{}, fmt.Errorf("%w: immutable %s commit receipt", errStateConflict, kind)
	}
	return committed, nil
}

func cohortFor(plan Plan, color string) (CohortPlan, error) { //nolint:gocritic // The closed two-item plan favors value semantics.
	for i := range plan.Cohorts {
		if plan.Cohorts[i].Color == color {
			return plan.Cohorts[i], nil
		}
	}
	return CohortPlan{}, fmt.Errorf("%w: missing %s cohort", errInvalidAuthority, color)
}
