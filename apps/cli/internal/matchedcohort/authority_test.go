package matchedcohort

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	qurl "github.com/layervai/qurl-go/qurl"
)

func TestRequiredQURLGoSourceMatchesStandaloneModulePin(t *testing.T) {
	_, sourceFile, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("test source path is unavailable")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", ".."))
	moduleRaw, err := os.ReadFile(filepath.Join(repositoryRoot, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	moduleRaw = bytes.ReplaceAll(moduleRaw, []byte("\r\n"), []byte("\n"))
	version := "v0.8.1-0.20260824221936-" + RequiredQURLGoSourceSHA[:12]
	wantModuleLine := "\tgithub.com/layervai/qurl-go " + version + "\n"
	if bytes.Count(moduleRaw, []byte(wantModuleLine)) != 1 || bytes.Contains(moduleRaw, []byte("replace github.com/layervai/qurl-go")) {
		t.Fatalf("go.mod does not pin exact reviewed qurl-go source %s", RequiredQURLGoSourceSHA)
	}
	sumRaw, err := os.ReadFile(filepath.Join(repositoryRoot, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	sumRaw = bytes.ReplaceAll(sumRaw, []byte("\r\n"), []byte("\n"))
	wantModuleSum := "github.com/layervai/qurl-go " + version + " h1:FPdbSiIeZqvsKkHZKW0xfSHGnO/b9VimuLsgzLOuDTc=\n"
	if bytes.Count(sumRaw, []byte(wantModuleSum)) != 1 {
		t.Fatal("go.sum does not bind the exact reviewed qurl-go module bytes")
	}
}

func TestValidatePlanClosedTwoColorEightIdentityShape(t *testing.T) {
	plan := validPlan("1")
	if err := ValidatePlan(plan); err != nil {
		t.Fatalf("ValidatePlan: %v", err)
	}
	mutations := []struct {
		name string
		edit func(*Plan)
	}{
		{"environment", func(p *Plan) { p.Environment = "prod" }},
		{"NHP source", func(p *Plan) { p.NHPSourceSHA = strings.Repeat("f", 40) }},
		{"qurl-go source", func(p *Plan) { p.QURLGoSourceSHA = strings.Repeat("f", 40) }},
		{"cohort order", func(p *Plan) { p.Cohorts[0], p.Cohorts[1] = p.Cohorts[1], p.Cohorts[0] }},
		{"identity order", func(p *Plan) { p.Identities[0], p.Identities[1] = p.Identities[1], p.Identities[0] }},
		{"identity owner", func(p *Plan) { p.Identities[0].OwnerID = "different-owner@clients" }},
		{"duplicate agent", func(p *Plan) { p.Identities[1].AgentID = p.Identities[0].AgentID }},
		{"duplicate connector", func(p *Plan) { p.Identities[1].ConnectorID = p.Identities[0].ConnectorID }},
		{"wrong hub port", func(p *Plan) { p.Cohorts[0].HubPort = 62206 }},
		{"wrong cell endpoint", func(p *Plan) { p.Cohorts[0].CellEndpoint.Port = 62206 }},
		{"missing identity", func(p *Plan) { p.Identities = p.Identities[:7] }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := clonePlan(t, plan)
			mutation.edit(&changed)
			if err := ValidatePlan(changed); err == nil {
				t.Fatal("mutated plan accepted")
			}
		})
	}
}

func TestDurableAgentStateEverySaveCASAndLostResponse(t *testing.T) {
	ctx := context.Background()
	blobs := newMemoryBlobs()
	store, err := NewDurableAgentStateStore(blobs, "generations/g/blue/direct-a/agent-state")
	if err != nil {
		t.Fatal(err)
	}
	state := testAgentState("fixed-agent")
	if err := store.SaveAgentState(ctx, state); err != nil {
		t.Fatalf("first save: %v", err)
	}
	first, err := blobs.Load(ctx, "generations/g/blue/direct-a/agent-state")
	if err != nil {
		t.Fatal(err)
	}
	blobs.failAfterCommit = true
	state.DeviceAPIKeyID = "key-id-2"
	if err := store.SaveAgentState(ctx, state); err != nil {
		t.Fatalf("lost-response save: %v", err)
	}
	second, err := blobs.Load(ctx, first.Key)
	if err != nil {
		t.Fatal(err)
	}
	if second.PreviousVersion != first.VersionID || second.OperationID == first.OperationID || second.SHA256 == first.SHA256 {
		t.Fatalf("second commit did not advance exact CAS: %#v", second)
	}
	restarted, _ := NewDurableAgentStateStore(blobs, first.Key)
	loaded, err := restarted.LoadAgentState(ctx)
	if err != nil {
		t.Fatalf("restart load: %v", err)
	}
	if loaded.AgentID != state.AgentID || loaded.DeviceAPIKeyID != state.DeviceAPIKeyID {
		t.Fatalf("restart state = %#v", loaded)
	}
	loaded.AgentID = "caller-mutation"
	again, err := restarted.LoadAgentState(ctx)
	if err != nil || again.AgentID != state.AgentID {
		t.Fatalf("store retained caller-owned pointer: %#v, %v", again, err)
	}
}

func TestProvisionPersistsPlanAndConnectorIntentBeforeNetwork(t *testing.T) {
	ctx := context.Background()
	blobs := newMemoryBlobs()
	credentials := &fakeEnrollment{blobs: blobs}
	runtime := &fakeRuntime{t: t, blobs: blobs}
	lock := &fakeWriterLock{}
	provisioner := Provisioner{Blobs: blobs, Credentials: credentials, WriterLock: lock, InvocationToken: strings.Repeat("a", 64), Runtime: runtime}
	result, err := provisioner.Provision(ctx, validPlan("2"))
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if len(result.Authority.Identities) != 8 || runtime.connects != 8 || runtime.resolves != 8 {
		t.Fatalf("provision counts identities=%d connect=%d resolve=%d", len(result.Authority.Identities), runtime.connects, runtime.resolves)
	}
	if result.Authority.Cohorts[0].CellEndpoint.Host == result.Authority.Identities[0].Selector.Host {
		t.Fatal("fixture did not distinguish assigned cell endpoint from FRPS resource host")
	}
	if err := ValidateAuthority(result.Authority); err != nil {
		t.Fatalf("ValidateAuthority: %v", err)
	}
	if result.Reference.Key != "generations/"+result.Authority.GenerationID+"/authority" {
		t.Fatalf("authority reference = %#v", result.Reference)
	}
	if credentials.firstPlanMissing {
		t.Fatal("credential authority called before immutable generation plan commit")
	}
	if credentials.verifyCalls != 1 {
		t.Fatalf("credential identity verification calls = %d, want 1", credentials.verifyCalls)
	}
	if result.Authority.EnrollmentCredentialReceipt.Key != "generations/"+result.Authority.GenerationID+"/enrollment-credential-receipt" {
		t.Fatalf("enrollment credential receipt = %#v", result.Authority.EnrollmentCredentialReceipt)
	}
	if lock.entries != 1 || lock.operation.Operation != "provision" || lock.operation.OwnerSubject != result.Authority.OwnerSubject {
		t.Fatalf("credential writer lock = %#v entries=%d", lock.operation, lock.entries)
	}
}

func TestProvisionRejectsFRPSSelectorAsAssignedCellEndpoint(t *testing.T) {
	blobs := newMemoryBlobs()
	runtime := &fakeRuntime{t: t, blobs: blobs, useSelectorEndpoint: true}
	_, err := (&Provisioner{Blobs: blobs, Credentials: &fakeEnrollment{blobs: blobs}, WriterLock: &fakeWriterLock{},
		InvocationToken: strings.Repeat("a", 64), Runtime: runtime}).Provision(context.Background(), validPlan("7"))
	if err == nil {
		t.Fatal("FRPS selector was accepted as the assigned cell endpoint")
	}
	if runtime.connects != 1 || runtime.resolves != 0 {
		t.Fatalf("swapped endpoint reached connector resolution: connect=%d resolve=%d", runtime.connects, runtime.resolves)
	}
}

func TestProvisionRejectsEnrollmentCredentialDriftBeforeIdentityNetwork(t *testing.T) {
	base := EnrollmentCredentialReceipt{Schema: 1, OwnerID: "sandbox-sharing-owner@clients", AuthType: "api_key",
		KeyID: "key_AbCdEf123456", Kind: "api_key", Scopes: []string{"qurl:agent", "qurl:read", "qurl:write"}, KeyPrefix: "lv_test_abcd"}
	mutations := []struct {
		name string
		edit func(*EnrollmentCredentialReceipt)
	}{
		{"owner", func(r *EnrollmentCredentialReceipt) { r.OwnerID = "other-owner@clients" }},
		{"auth type", func(r *EnrollmentCredentialReceipt) { r.AuthType = "m2m" }},
		{"kind", func(r *EnrollmentCredentialReceipt) { r.Kind = "device" }},
		{"scopes", func(r *EnrollmentCredentialReceipt) { r.Scopes = []string{"qurl:agent", "qurl:read"} }},
		{"prefix", func(r *EnrollmentCredentialReceipt) { r.KeyPrefix = "lv_test_other" }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			receipt := base
			receipt.Scopes = slices.Clone(base.Scopes)
			mutation.edit(&receipt)
			blobs := newMemoryBlobs()
			runtime := &fakeRuntime{t: t, blobs: blobs}
			_, err := (&Provisioner{Blobs: blobs, Credentials: &fakeEnrollment{blobs: blobs, receipt: receipt},
				WriterLock: &fakeWriterLock{}, InvocationToken: strings.Repeat("e", 64), Runtime: runtime}).Provision(context.Background(), validPlan("9"))
			if err == nil {
				t.Fatal("mutated enrollment credential accepted")
			}
			if runtime.connects != 0 || runtime.resolves != 0 {
				t.Fatalf("credential drift reached identity network: connect=%d resolve=%d", runtime.connects, runtime.resolves)
			}
		})
	}
}

func TestProvisionImmutablePlanDriftFailsBeforeCredentialOrNetwork(t *testing.T) {
	ctx := context.Background()
	blobs := newMemoryBlobs()
	plan := validPlan("3")
	raw, _ := CanonicalJSON(plan)
	if _, err := persistImmutable(ctx, blobs, "generations/"+plan.GenerationID+"/plan", "plan", raw); err != nil {
		t.Fatal(err)
	}
	changed := clonePlan(t, plan)
	changed.Identities[0].AgentID = "changed-fixed-agent"
	credentials := &fakeEnrollment{blobs: blobs}
	runtime := &fakeRuntime{t: t, blobs: blobs}
	_, err := (&Provisioner{Blobs: blobs, Credentials: credentials, WriterLock: &fakeWriterLock{}, InvocationToken: strings.Repeat("b", 64), Runtime: runtime}).Provision(ctx, changed)
	if !errors.Is(err, errStateConflict) {
		t.Fatalf("plan drift error = %v", err)
	}
	if credentials.calls != 0 || runtime.connects != 0 || runtime.resolves != 0 {
		t.Fatalf("plan drift reached credential/network: credential=%d connect=%d resolve=%d", credentials.calls, runtime.connects, runtime.resolves)
	}
}

func TestRotationRetainsOldGenerationAndClassifiesLostResponse(t *testing.T) {
	ctx := context.Background()
	blobs := newMemoryBlobs()
	provisioner := Provisioner{Blobs: blobs, Credentials: &fakeEnrollment{blobs: blobs}, WriterLock: &fakeWriterLock{}, InvocationToken: strings.Repeat("c", 64), Runtime: &fakeRuntime{t: t, blobs: blobs}}
	first, err := provisioner.Provision(ctx, validPlan("4"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := provisioner.Provision(ctx, validPlan("5"))
	if err != nil {
		t.Fatal(err)
	}
	rotationLock := &fakeWriterLock{}
	rotator := Rotator{Blobs: blobs, WriterLock: rotationLock, RegistryKey: "fixed-canary/registry", InvocationToken: strings.Repeat("d", 64)}
	registry, _, err := rotator.Activate(ctx, first)
	if err != nil || registry.Active.GenerationID != first.Authority.GenerationID {
		t.Fatalf("activate first: %#v, %v", registry, err)
	}
	blobs.failAfterCommit = true
	registry, receipt, err := rotator.Activate(ctx, second)
	if err != nil {
		t.Fatalf("activate second after lost response: %v", err)
	}
	if registry.Active.GenerationID != second.Authority.GenerationID || len(registry.RetainedGenerations) != 1 ||
		registry.RetainedGenerations[0].GenerationID != first.Authority.GenerationID || receipt.VersionID == "" {
		t.Fatalf("rotation registry = %#v receipt=%#v", registry, receipt)
	}
	replayed, replayReceipt, err := rotator.Activate(ctx, second)
	if err != nil || replayed.Active != registry.Active || replayReceipt != receipt {
		t.Fatalf("activation replay = %#v %#v %v", replayed, replayReceipt, err)
	}
	if rotationLock.entries != 3 || rotationLock.operation.Operation != "rotate" || rotationLock.operation.GenerationID != second.Authority.GenerationID {
		t.Fatalf("rotation writer lock = %#v entries=%d", rotationLock.operation, rotationLock.entries)
	}
}

type memoryBlobs struct {
	mu               sync.Mutex
	values           map[string]Blob
	serial           int
	failAfterCommit  bool
	failBeforeCommit bool
}

func newMemoryBlobs() *memoryBlobs { return &memoryBlobs{values: map[string]Blob{}} }

func (m *memoryBlobs) Load(ctx context.Context, key string) (Blob, error) {
	if err := ctx.Err(); err != nil {
		return Blob{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.values[key]
	if !ok {
		return Blob{}, errStateNotFound
	}
	return cloneBlob(value), nil
}

func (m *memoryBlobs) Commit(ctx context.Context, candidate BlobCandidate) (Blob, error) { //nolint:gocritic // Implements the value-oriented production interface.
	if err := ctx.Err(); err != nil {
		return Blob{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failBeforeCommit {
		m.failBeforeCommit = false
		return Blob{}, errStateConflict
	}
	current, exists := m.values[candidate.Key]
	if exists && current.OperationID == candidate.OperationID && sameCommittedBlob(current, candidate) {
		return cloneBlob(current), nil
	}
	currentVersion := ""
	if exists {
		currentVersion = current.VersionID
	}
	if currentVersion != candidate.ExpectedVersion {
		return Blob{}, errStateConflict
	}
	m.serial++
	committed := Blob{Key: candidate.Key, VersionID: fmt.Sprintf("version-%d", m.serial), PreviousVersion: candidate.ExpectedVersion,
		OperationID: candidate.OperationID, SHA256: candidate.SHA256, Body: bytes.Clone(candidate.Body)}
	m.values[candidate.Key] = committed
	if m.failAfterCommit {
		m.failAfterCommit = false
		return Blob{}, errStateAmbiguous
	}
	return cloneBlob(committed), nil
}

type fakeEnrollment struct {
	blobs            *memoryBlobs
	calls            int
	verifyCalls      int
	firstPlanMissing bool
	receipt          EnrollmentCredentialReceipt
}

type fakeWriterLock struct {
	mu        sync.Mutex
	entries   int
	operation CredentialWriterOperation
	held      bool
}

func (l *fakeWriterLock) WithExclusive(ctx context.Context, operation CredentialWriterOperation, fn func(context.Context) error) error { //nolint:gocritic // Implements the value-oriented production interface.
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held {
		return errors.New("test lock reentry")
	}
	l.held = true
	l.entries++
	l.operation = operation
	defer func() { l.held = false }()
	return fn(ctx)
}

func (f *fakeEnrollment) EnrollmentCredential(ctx context.Context, plan IdentityPlan) (string, error) { //nolint:gocritic // Implements the value-oriented production interface.
	f.calls++
	// The generation is encoded in the agent ID fixture. Any plan commit is
	// sufficient here; Provision has one exact generation in flight.
	f.blobs.mu.Lock()
	found := false
	for key := range f.blobs.values {
		if len(key) > len("/plan") && key[len(key)-len("/plan"):] == "/plan" {
			found = true
		}
	}
	f.blobs.mu.Unlock()
	if !found {
		f.firstPlanMissing = true
	}
	return "account-credential-with-enough-entropy-1234567890", nil
}

func (f *fakeEnrollment) VerifyEnrollmentCredential(_ context.Context, expectedOwner string) (EnrollmentCredentialReceipt, error) {
	f.verifyCalls++
	if f.receipt.Schema != 0 {
		return f.receipt, nil
	}
	return EnrollmentCredentialReceipt{Schema: 1, OwnerID: expectedOwner, AuthType: "api_key", KeyID: "key_AbCdEf123456",
		Kind: "api_key", Scopes: []string{"qurl:agent", "qurl:read", "qurl:write"}, KeyPrefix: "lv_test_abcd"}, nil
}

func (*fakeEnrollment) OTP(context.Context, IdentityPlan, qurl.AgentOTPChallenge) (string, error) {
	return "123456", nil
}

type fakeRuntime struct {
	t                   *testing.T
	blobs               *memoryBlobs
	connects            int
	resolves            int
	useSelectorEndpoint bool
}

func (f *fakeRuntime) Connect(ctx context.Context, store qurl.AgentStateStore, cohort CohortPlan, plan IdentityPlan, _ string, _ EnrollmentAuthority) (*RuntimeBinding, error) { //nolint:gocritic // Implements the value-oriented production interface.
	f.connects++
	receiptKey := fmt.Sprintf("generations/%s/enrollment-credential-receipt", generationFromAgent(plan.AgentID))
	if _, err := f.blobs.Load(ctx, receiptKey); err != nil {
		f.t.Fatalf("enrollment credential receipt was not durable before identity network: %v", err)
	}
	state := testAgentState(plan.AgentID)
	state.DeviceAPIKeyID = "device-key-" + plan.Label + "-" + plan.Color
	if err := store.SaveAgentState(ctx, state); err != nil {
		return nil, err
	}
	endpoint := cohort.CellEndpoint
	if f.useSelectorEndpoint {
		endpoint.Host = plan.Selector.Host
	}
	return &RuntimeBinding{AgentID: plan.AgentID, PublicKeyB64: state.PublicKeyB64, DeviceAPIKeyID: state.DeviceAPIKeyID,
		CellID: cohort.CellID, AssignmentGeneration: cohort.AssignmentGeneration,
		Endpoint: endpoint}, nil
}

func (f *fakeRuntime) Resolve(ctx context.Context, binding *RuntimeBinding, request *qurl.NativeConnectorResourceRequest) (*qurl.ConnectorResourceResolution, error) {
	f.resolves++
	color, label := identityFromAgent(binding.AgentID)
	key := fmt.Sprintf("generations/%s/%s/%s/connector-state", generationFromAgent(binding.AgentID), color, label)
	blob, err := f.blobs.Load(ctx, key)
	if err != nil {
		f.t.Fatalf("connector request was not durable before network: %v", err)
	}
	var state connectorState
	if err := jsonUnmarshal(blob.Body, &state); err != nil || state.Request == nil || state.Request.RequestNonce != request.RequestNonce {
		f.t.Fatalf("durable connector request = %#v, %v", state, err)
	}
	return &qurl.ConnectorResourceResolution{Resource: &qurl.ConnectorResource{ResourceID: "resource-" + binding.AgentID,
		CRID: "crid-" + binding.AgentID, ConnectorRoutingID: "route-" + binding.AgentID,
		KnockResourceID: "knock-" + binding.AgentID, Slug: request.ConnectorID}, FoundExisting: false}, nil
}

func (*fakeRuntime) Close(*RuntimeBinding) {}

func validPlan(generationDigit string) Plan {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x31}, 32))
	plan := Plan{Schema: AuthoritySchema, Environment: EnvironmentSandbox, GenerationID: generationDigit + string(bytes.Repeat([]byte{'0'}, 63)),
		OwnerSubject: "sandbox-sharing-owner@clients", AWSAccountID: "123456789012", AWSRegion: "us-east-1",
		NHPSourceSHA: RequiredNHPSourceSHA, QURLGoSourceSHA: RequiredQURLGoSourceSHA}
	for _, color := range []string{ColorBlue, ColorGreen} {
		plan.Cohorts = append(plan.Cohorts, CohortPlan{Color: color, ServerASG: "sandbox-server-" + color, ACASG: "sandbox-ac-" + color,
			RelayASG: "sandbox-relay-" + color, SessionControlTable: "sandbox-session-control", QURLAgentKeysTable: "sandbox-agent-keys",
			CellID: "sandbox-cell", AssignmentGeneration: 7, HubHost: color + ".sandbox.layerv.xyz", HubPort: 443,
			HubServerPublicKeyB64: key, CellEndpoint: qurl.NHPUDPEndpoint{Host: color + "-cell.sandbox.layerv.xyz", Port: 443,
				ServerPublicKeyB64: key}})
		for _, label := range labels {
			selector := string(label[len(label)-1])
			if label == "relay-d" {
				selector = "a"
			}
			plan.Identities = append(plan.Identities, IdentityPlan{Color: color, Label: label, OwnerID: plan.OwnerSubject,
				AgentID: fmt.Sprintf("canary-%s-%s-g%s", color, label, generationDigit), ConnectorID: fmt.Sprintf("canary-%s-%s-g%s", color, label, generationDigit),
				Selector: FRPSSelector{ResourceID: "qurl-tunnel-server-" + selector, Host: color + "-ac.sandbox.layerv.xyz", Port: 6000 + int(selector[0])}})
		}
	}
	return plan
}

func testAgentState(agentID string) *qurl.AgentState {
	return &qurl.AgentState{AgentID: agentID, PrivateKeyB64: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x21}, 32)),
		PublicKeyB64: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x31}, 32)), SchemaVersion: 7,
		RegisteredAt: timePtr(time.Unix(1700000000, 0).UTC()), DeviceAPIKey: "device-secret-never-in-authority", DeviceAPIKeyID: "device-key-id"}
}

func timePtr(value time.Time) *time.Time { return &value }

func clonePlan(t *testing.T, plan Plan) Plan { //nolint:gocritic // Test clone snapshots one closed plan value.
	t.Helper()
	raw, _ := CanonicalJSON(plan)
	var cloned Plan
	if err := jsonUnmarshal(raw, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}

func jsonUnmarshal(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}

func identityFromAgent(agent string) (color, label string) {
	parts := strings.Split(agent, "-")
	return parts[1], parts[2] + "-" + parts[3]
}

func generationFromAgent(agent string) string {
	parts := strings.Split(agent, "-")
	digit := strings.TrimPrefix(parts[len(parts)-1], "g")
	return digit + string(bytes.Repeat([]byte{'0'}, 63))
}
