//go:build cliprodcohort

package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/layervai/qurl-go/qurl"

	connectoragent "github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

const (
	prodClosureOperationEnv     = "QURL_PROD_MATCHED_COHORT_CLOSURE_OPERATION"
	prodClosureIntentFileEnv    = "QURL_PROD_MATCHED_COHORT_CLOSURE_INTENT_FILE"
	prodClosureJournalFileEnv   = "QURL_PROD_MATCHED_COHORT_CLOSURE_JOURNAL_FILE"
	prodClosureReportFileEnv    = "QURL_PROD_MATCHED_COHORT_CLOSURE_REPORT_FILE"
	prodClosureServerEnv        = "QURL_PROD_MATCHED_COHORT_SERVER_ENDPOINT"
	prodClosureACEnv            = "QURL_PROD_MATCHED_COHORT_AC_ENDPOINT"
	prodClosureSelectorsEnv     = "QURL_PROD_MATCHED_COHORT_FRPS_SELECTORS_JSON"
	prodClosureRelayEnv         = "QURL_PROD_MATCHED_COHORT_RELAY_HOSTNAME"
	prodClosureSchema           = 1
	prodClosureVerifyBudget     = 5 * time.Second
	prodClosureKeySearchLimit   = 4096
	prodClosureSelectorCount    = 3
	prodClosureStateDirName     = "closure-state"
	prodClosureDirectDeployName = "direct-deployment.json"
	prodClosureRelayDeployName  = "relay-deployment.json"
	prodClosureExpectedFRPSHost = "connect.layerv.ai"
	prodClosureOutcomeNoReply   = "bounded_no_reply_after_prevalidated_authority"
	prodClosureOutcomeRelay503  = "exact_relay_maintenance_503"
	prodClosureOutcomeSuccess   = "unexpected_success"
)

type prodFRPSSelector struct {
	ResourceID string `json:"resource_id"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
}

type prodClosureAuthority struct {
	ReleaseID                 string             `json:"release_id"`
	RunID                     string             `json:"run_id"`
	RunAttempt                string             `json:"run_attempt"`
	NHPSourceSHA              string             `json:"nhp_source_sha"`
	QURLSourceSHA             string             `json:"qurl_source_sha"`
	QURLBinarySHA256          string             `json:"qurl_binary_sha256"`
	CandidateDeploymentSHA256 string             `json:"candidate_deployment_sha256"`
	CanonicalDeploymentSHA256 string             `json:"canonical_deployment_sha256"`
	ServerEndpoint            string             `json:"server_endpoint"`
	ACEndpoint                string             `json:"ac_endpoint"`
	RelayHostname             string             `json:"relay_hostname"`
	FRPSSelectors             []prodFRPSSelector `json:"frps_selectors"`
}

type prodClosureArtifact struct {
	Selector       prodFRPSSelector `json:"selector"`
	AgentID        string           `json:"agent_id"`
	ConnectorID    string           `json:"connector_id"`
	StateFile      string           `json:"state_file"`
	StateSHA256    string           `json:"state_sha256,omitempty"`
	DeviceAPIKeyID string           `json:"device_api_key_id,omitempty"`
	ResourceID     string           `json:"resource_id,omitempty"`
	CRID           string           `json:"crid,omitempty"`
	Prepared       bool             `json:"prepared"`
}

type prodClosureJournal struct {
	Schema      int                   `json:"schema"`
	Environment string                `json:"environment"`
	Authority   prodClosureAuthority  `json:"authority"`
	Status      string                `json:"status"`
	Artifacts   []prodClosureArtifact `json:"artifacts"`
}

type prodClosureIntent struct {
	Schema               int                   `json:"schema"`
	Environment          string                `json:"environment"`
	Authority            prodClosureAuthority  `json:"authority"`
	JournalSHA256        string                `json:"journal_sha256"`
	DirectDeploymentFile string                `json:"direct_deployment_file"`
	DirectDeploymentSHA  string                `json:"direct_deployment_sha256"`
	RelayDeploymentFile  string                `json:"relay_deployment_file"`
	RelayDeploymentSHA   string                `json:"relay_deployment_sha256"`
	Artifacts            []prodClosureArtifact `json:"artifacts"`
}

type prodClosureOperationReport struct {
	Schema        int    `json:"schema"`
	Environment   string `json:"environment"`
	Operation     string `json:"operation"`
	ReleaseID     string `json:"release_id"`
	RunID         string `json:"run_id"`
	RunAttempt    string `json:"run_attempt"`
	JournalSHA256 string `json:"journal_sha256"`
	IntentSHA256  string `json:"intent_sha256"`
	BundleSHA256  string `json:"bundle_sha256"`
}

type prodClosureNegative struct {
	Attempted bool   `json:"attempted"`
	Success   bool   `json:"success"`
	Ready     bool   `json:"ready"`
	Outcome   string `json:"outcome"`
}

type prodClosureFRPSResult struct {
	Selector  prodFRPSSelector `json:"selector"`
	Attempted bool             `json:"attempted"`
	Success   bool             `json:"success"`
	Ready     bool             `json:"ready"`
	Outcome   string           `json:"outcome"`
}

type prodClosureVerifyReport struct {
	Schema                     int                     `json:"schema"`
	Environment                string                  `json:"environment"`
	Operation                  string                  `json:"operation"`
	ReleaseID                  string                  `json:"release_id"`
	RunID                      string                  `json:"run_id"`
	RunAttempt                 string                  `json:"run_attempt"`
	JournalSHA256              string                  `json:"journal_sha256"`
	IntentSHA256               string                  `json:"intent_sha256"`
	BundleSHA256               string                  `json:"bundle_sha256"`
	Direct                     prodClosureNegative     `json:"direct"`
	Relay                      prodClosureNegative     `json:"relay"`
	AgentRestart               prodClosureNegative     `json:"agent_restart"`
	FRPS                       []prodClosureFRPSResult `json:"frps"`
	ResourceInventoryUnchanged bool                    `json:"resource_inventory_unchanged"`
	CleanupComplete            bool                    `json:"cleanup_complete"`
}

type prodClosureConfig struct {
	Operation   string
	IntentPath  string
	JournalPath string
	ReportPath  string
	BaseDir     string
	Authority   prodClosureAuthority
}

type prodClosureAttemptResult struct {
	name    string
	index   int
	success bool
	ready   bool
	outcome string
	err     error
}

// runProdMatchedCohortAdmissionClosed owns both phases. Prepare is called while
// admission is open. Verify is called only after NHP has durably closed and
// bracketed the public gate. The verify phase consumes no caller-selected
// identity beyond the byte-identical private intent written by prepare.
func runProdMatchedCohortAdmissionClosed(t *testing.T) {
	t.Helper()
	inputs := loadProdLifecycleInputs(t)
	cfg := loadProdClosureConfig(t, inputs)
	switch cfg.Operation {
	case "prepare":
		prepareProdClosure(t, inputs, cfg)
	case "verify":
		verifyProdClosure(t, inputs, cfg)
	default:
		t.Fatalf("unsupported production closure operation %q", cfg.Operation)
	}
}

func loadProdClosureConfig(t *testing.T, inputs prodLifecycleInputs) prodClosureConfig { //nolint:gocritic // One immutable protected-run input snapshot is validated.
	t.Helper()
	operation := exactProdEnumEnv(t, prodClosureOperationEnv, "prepare", "verify")
	intentPath := exactProdPathEnv(t, prodClosureIntentFileEnv)
	journalPath := exactProdPathEnv(t, prodClosureJournalFileEnv)
	reportPath := exactProdPathEnv(t, prodClosureReportFileEnv)
	baseDir := filepath.Dir(intentPath)
	if filepath.Dir(journalPath) != baseDir || filepath.Dir(reportPath) != baseDir ||
		intentPath == journalPath || intentPath == reportPath || journalPath == reportPath {
		t.Fatal("closure authority files must be distinct siblings")
	}
	info, err := os.Lstat(baseDir)
	if err != nil || validateProdClosurePrivateDirInfo(info) != nil {
		t.Fatal("closure authority directory must already exist and be private")
	}
	if inputs.Target != "canonical" || inputs.Transport != "direct" {
		t.Fatal("closure operation must use the canonical direct deployment authority")
	}

	selectorsRaw := exactProdEnv(t, prodClosureSelectorsEnv)
	selectors, err := decodeProdFRPSSelectors([]byte(selectorsRaw))
	if err != nil {
		t.Fatal(err)
	}
	serverEndpoint := exactProdEnv(t, prodClosureServerEnv)
	acEndpoint := exactProdEnv(t, prodClosureACEnv)
	relayHostname := exactProdEnv(t, prodClosureRelayEnv)
	canonical := inputs.Manifest.Customer.Canonical
	if serverEndpoint != canonical.ServerEndpoint || acEndpoint != canonical.ACEndpoint || relayHostname != canonical.RelayHostname {
		t.Fatal("closure endpoint arguments differ from canonical signed deployment authority")
	}
	runID := exactProdEnv(t, prodRunIDEnv)
	runAttempt := exactProdEnv(t, prodRunAttemptEnv)
	if !canonicalProdUint64(runID) || !canonicalProdUint64(runAttempt) {
		t.Fatal("closure run identity is not canonical positive uint64")
	}

	return prodClosureConfig{
		Operation: operation, IntentPath: intentPath, JournalPath: journalPath, ReportPath: reportPath, BaseDir: baseDir,
		Authority: prodClosureAuthority{
			ReleaseID: inputs.ReleaseID, RunID: runID, RunAttempt: runAttempt,
			NHPSourceSHA: inputs.Manifest.Source.SHA, QURLSourceSHA: inputs.Manifest.Customer.QURLSourceSHA,
			QURLBinarySHA256:          inputs.Manifest.Customer.QURLBinarySHA256,
			CandidateDeploymentSHA256: sha256File(t, inputs.CandidateDeployPath),
			CanonicalDeploymentSHA256: sha256File(t, inputs.CanonicalDeployPath),
			ServerEndpoint:            serverEndpoint, ACEndpoint: acEndpoint, RelayHostname: relayHostname, FRPSSelectors: selectors,
		},
	}
}

func canonicalProdUint64(value string) bool {
	if !prodPositiveDecimal.MatchString(value) {
		return false
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	return err == nil && parsed > 0 && strconv.FormatUint(parsed, 10) == value
}

func decodeProdFRPSSelectors(raw []byte) ([]prodFRPSSelector, error) {
	var selectors []prodFRPSSelector
	if err := decodeStrictProdJSON(raw, &selectors); err != nil {
		return nil, fmt.Errorf("decode exact FRPS selectors: %w", err)
	}
	canonical, err := json.Marshal(selectors)
	if err != nil || !bytes.Equal(canonical, raw) || len(selectors) != prodClosureSelectorCount {
		return nil, errors.New("FRPS selectors must be one canonical three-entry JSON array")
	}
	seenID := map[string]bool{}
	seenTarget := map[string]bool{}
	for i, selector := range selectors {
		wantID := "qurl-tunnel-server-" + string(rune('a'+i))
		if selector.ResourceID != wantID || selector.Host != prodClosureExpectedFRPSHost ||
			net.ParseIP(selector.Host) != nil || selector.Port < 1 || selector.Port > 65535 {
			return nil, fmt.Errorf("FRPS selector %d is not exact canonical authority", i)
		}
		target := net.JoinHostPort(selector.Host, strconv.Itoa(selector.Port))
		if seenID[selector.ResourceID] || seenTarget[target] {
			return nil, errors.New("FRPS selector identities and targets must be unique")
		}
		seenID[selector.ResourceID], seenTarget[target] = true, true
	}
	return selectors, nil
}

func prepareProdClosure(t *testing.T, inputs prodLifecycleInputs, cfg prodClosureConfig) { //nolint:gocritic // The phase owns immutable copies across cleanup callbacks.
	t.Helper()
	if raw, ok := readExistingProdPrivateSibling(t, cfg.IntentPath); ok {
		intent := decodeAndValidateProdClosureIntent(t, raw, cfg)
		replayComplete := false
		t.Cleanup(func() {
			if replayComplete {
				return
			}
			if err := cleanupProdClosureArtifacts(inputs, cfg.BaseDir, intent.Artifacts); err != nil {
				t.Errorf("cleanup failed closure prepare replay: %v", err)
			}
		})
		journalRaw := readProdPrivateSibling(t, cfg.JournalPath)
		if digestBytes(journalRaw) != intent.JournalSHA256 {
			t.Fatal("existing closure intent journal digest mismatch")
		}
		var journal prodClosureJournal
		strictProdJSON(t, journalRaw, &journal, "closure journal")
		validateProdClosureJournal(t, cfg, journal, true)
		if !bytes.Equal(mustProdCanonicalJSON(journal.Artifacts), mustProdCanonicalJSON(intent.Artifacts)) {
			t.Fatal("existing closure intent artifacts differ from its journal")
		}
		direct, relay := expectedProdDeployments(inputs.Manifest.Customer.Canonical)
		assertProdClosureDeployment(t, filepath.Join(cfg.BaseDir, intent.DirectDeploymentFile), intent.DirectDeploymentSHA, direct)
		assertProdClosureDeployment(t, filepath.Join(cfg.BaseDir, intent.RelayDeploymentFile), intent.RelayDeploymentSHA, relay)
		for i := range intent.Artifacts {
			validatePreparedProdClosureArtifact(t, inputs, cfg, i, intent.Artifacts[i])
		}
		writeProdClosureOperationReport(t, cfg, journalRaw, raw, intent.Artifacts)
		replayComplete = true
		return
	}

	directPath := filepath.Join(cfg.BaseDir, prodClosureDirectDeployName)
	relayPath := filepath.Join(cfg.BaseDir, prodClosureRelayDeployName)
	writeCanonicalPrivateJSON(t, directPath, inputs.SelectedDeployment, false)
	_, relayDeployment := expectedProdDeployments(inputs.Manifest.Customer.Canonical)
	writeCanonicalPrivateJSON(t, relayPath, relayDeployment, false)

	journal := loadOrCreateProdClosureJournal(t, cfg)
	completed := false
	t.Cleanup(func() {
		if completed {
			return
		}
		if err := cleanupProdClosureArtifacts(inputs, cfg.BaseDir, journal.Artifacts); err != nil {
			t.Errorf("cleanup failed closure prepare: %v", err)
		}
	})

	for i := range journal.Artifacts {
		artifact := &journal.Artifacts[i]
		if artifact.Prepared {
			validatePreparedProdClosureArtifact(t, inputs, cfg, i, *artifact)
			continue
		}
		prepareProdClosureArtifact(t, inputs, cfg, directPath, artifact)
		writeProdClosureJournal(t, cfg.JournalPath, journal)
	}
	journal.Status = "prepared"
	journalRaw := writeProdClosureJournal(t, cfg.JournalPath, journal)
	intent := prodClosureIntent{
		Schema: prodClosureSchema, Environment: "prod", Authority: cfg.Authority,
		JournalSHA256:        digestBytes(journalRaw),
		DirectDeploymentFile: prodClosureDirectDeployName, DirectDeploymentSHA: sha256File(t, directPath),
		RelayDeploymentFile: prodClosureRelayDeployName, RelayDeploymentSHA: sha256File(t, relayPath),
		Artifacts: append([]prodClosureArtifact(nil), journal.Artifacts...),
	}
	intentRaw := writeCanonicalPrivateJSON(t, cfg.IntentPath, intent, true)
	writeProdClosureOperationReport(t, cfg, journalRaw, intentRaw, journal.Artifacts)
	completed = true
}

func loadOrCreateProdClosureJournal(t *testing.T, cfg prodClosureConfig) *prodClosureJournal { //nolint:gocritic // The returned journal owns its copied expected authority.
	t.Helper()
	if raw, ok := readExistingProdPrivateSibling(t, cfg.JournalPath); ok {
		var journal prodClosureJournal
		strictProdJSON(t, raw, &journal, "closure journal")
		validateProdClosureJournal(t, cfg, journal, false)
		return &journal
	}
	journal := &prodClosureJournal{Schema: prodClosureSchema, Environment: "prod", Authority: cfg.Authority, Status: "preparing"}
	for i, selector := range cfg.Authority.FRPSSelectors {
		namespace := prodClosureNamespace(t, cfg.Authority, i)
		journal.Artifacts = append(journal.Artifacts, prodClosureArtifact{
			Selector: selector, AgentID: namespace.AgentID, ConnectorID: namespace.ConnectorID,
			StateFile: filepath.ToSlash(filepath.Join(prodClosureStateDirName, selector.ResourceID, connectorstate.AgentStateFile)),
		})
	}
	writeProdClosureJournal(t, cfg.JournalPath, journal)
	return journal
}

func validateProdClosureJournal(t *testing.T, expected prodClosureConfig, journal prodClosureJournal, requirePrepared bool) { //nolint:gocritic // Closed values are compared as canonical immutable snapshots.
	t.Helper()
	if journal.Schema != prodClosureSchema || journal.Environment != "prod" ||
		(journal.Status != "preparing" && journal.Status != "prepared") ||
		!equalProdClosureAuthority(journal.Authority, expected.Authority) || len(journal.Artifacts) != prodClosureSelectorCount {
		t.Fatal("existing closure journal does not match exact authority")
	}
	if requirePrepared && journal.Status != "prepared" {
		t.Fatal("closure journal is not prepared")
	}
	for i := range journal.Artifacts {
		validateProdClosureArtifactIdentity(t, expected.Authority, i, journal.Artifacts[i])
		if journal.Status == "prepared" && !journal.Artifacts[i].Prepared {
			t.Fatal("prepared closure journal contains an incomplete artifact")
		}
	}
}

func prodClosureNamespace(t *testing.T, authority prodClosureAuthority, index int) prodRunNamespace { //nolint:gocritic // Namespace derivation owns one closed authority copy.
	t.Helper()
	if index < 0 || index >= prodClosureSelectorCount || !canonicalProdUint64(authority.RunID) || !canonicalProdUint64(authority.RunAttempt) {
		t.Fatal("closure namespace authority is invalid")
	}
	label := string(rune('a' + index))
	agentID := fmt.Sprintf("qurl-prod-close-r%s-a%s-%s", authority.RunID, authority.RunAttempt, label)
	if len(agentID) > 64 {
		t.Fatal("closure agent identity exceeds platform bound")
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{"qurl-prod-closure-v1", authority.ReleaseID, authority.RunID, authority.RunAttempt, label}, "\x00")))
	return prodRunNamespace{AgentID: agentID, ConnectorID: "connector-prod-close-" + hex.EncodeToString(digest[:12])}
}

func prepareProdClosureArtifact(t *testing.T, inputs prodLifecycleInputs, cfg prodClosureConfig, deploymentPath string, artifact *prodClosureArtifact) { //nolint:gocritic // Cleanup requires immutable protected-run inputs.
	t.Helper()
	statePath := resolveProdClosureRelative(t, cfg.BaseDir, artifact.StateFile)
	stateDir := filepath.Dir(statePath)
	if err := ensureProdClosurePrivateTree(cfg.BaseDir, filepath.Dir(artifact.StateFile)); err != nil {
		t.Fatalf("create closure state directory: %v", err)
	}
	if _, err := os.Lstat(statePath); errors.Is(err, os.ErrNotExist) {
		privateKey, publicKey := findProdSelectorKey(t, artifact.Selector, cfg.Authority.FRPSSelectors)
		seed := &qurl.AgentState{
			AgentID: artifact.AgentID, PrivateKeyB64: base64.StdEncoding.EncodeToString(privateKey),
			PublicKeyB64: base64.StdEncoding.EncodeToString(publicKey),
		}
		for i := range privateKey {
			privateKey[i] = 0
		}
		store, err := qurl.OpenFileAgentState(statePath)
		if err != nil {
			t.Fatalf("open precommitted closure state: %v", err)
		}
		saveErr := store.SaveAgentState(context.Background(), seed)
		closeErr := store.Close()
		if saveErr != nil || closeErr != nil {
			t.Fatalf("persist precommitted closure state: save=%v close=%v", saveErr, closeErr)
		}
	} else if err != nil {
		t.Fatalf("inspect precommitted closure state: %v", err)
	} else {
		_ = readProdAuthorityFile(t, statePath, true, prodAuthorityMaxBytes)
	}

	// The journal row and private state now exist before the first possible
	// registration, resource creation, or NHP admission side effect.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "prepared\n") }))
	defer target.Close()
	process := startProdPublishProcess(t, inputs, deploymentPath, prodRunNamespace{AgentID: artifact.AgentID, ConnectorID: artifact.ConnectorID}, stateDir, target.URL)
	crid := process.waitReady(t)
	host, port, err := prodSelectedFRPSTarget(process.stderr.String())
	if err != nil || host != artifact.Selector.Host || port != artifact.Selector.Port {
		process.forceStop(t)
		t.Fatalf("authenticated FRPS selection = %s:%d, %v; want %s:%d", host, port, err, artifact.Selector.Host, artifact.Selector.Port)
	}
	process.stopAndValidate(t, inputs)
	loaded, err := loadProdAgentState(stateDir)
	if err != nil || loaded.AgentID != artifact.AgentID || loaded.DeviceAPIKeyID == "" || loaded.DeviceAPIKey == "" {
		t.Fatalf("load prepared closure identity: %v", err)
	}
	client, err := prodConnectorClient(inputs.Manifest.Customer.APIEndpoint, loaded.DeviceAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), prodCleanupTimeout)
	defer cancel()
	resource, err := client.GetConnectorResourceBySlug(ctx, artifact.ConnectorID)
	if err != nil || resource == nil || resource.ResourceID == "" || resource.CRID != crid {
		t.Fatalf("read prepared closure resource: %v", err)
	}
	artifact.StateSHA256 = sha256PrivateFile(t, statePath)
	artifact.DeviceAPIKeyID = loaded.DeviceAPIKeyID
	artifact.ResourceID = resource.ResourceID
	artifact.CRID = resource.CRID
	artifact.Prepared = true
}

func findProdSelectorKey(t *testing.T, wanted prodFRPSSelector, selectors []prodFRPSSelector) (privateKey, publicKey []byte) {
	t.Helper()
	for attempt := 0; attempt < prodClosureKeySearchLimit; attempt++ {
		key, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("generate closure device key: %v", err)
		}
		publicB64 := base64.StdEncoding.EncodeToString(key.PublicKey().Bytes())
		if selectedProdFRPSResource(publicB64, selectors) == wanted.ResourceID {
			return append([]byte(nil), key.Bytes()...), append([]byte(nil), key.PublicKey().Bytes()...)
		}
	}
	t.Fatalf("could not bind a device key to selector %s within %d attempts", wanted.ResourceID, prodClosureKeySearchLimit)
	return nil, nil
}

func selectedProdFRPSResource(publicKeyB64 string, selectors []prodFRPSSelector) string {
	identity := "pub:" + publicKeyB64
	bestID := ""
	var bestScore uint64
	for _, selector := range selectors {
		sum := sha256.Sum256([]byte(identity + "\x00" + selector.ResourceID))
		score := uint64(sum[0])<<56 | uint64(sum[1])<<48 | uint64(sum[2])<<40 | uint64(sum[3])<<32 |
			uint64(sum[4])<<24 | uint64(sum[5])<<16 | uint64(sum[6])<<8 | uint64(sum[7])
		if bestID == "" || score > bestScore || (score == bestScore && selector.ResourceID < bestID) {
			bestID, bestScore = selector.ResourceID, score
		}
	}
	return bestID
}

func prodSelectedFRPSTarget(logText string) (host string, port int, err error) {
	host = ""
	port = 0
	count := 0
	for _, line := range strings.Split(logText, "\n") {
		fields := strings.Fields(line)
		if !containsProdField(fields, "event=knock_overlay_applied") {
			continue
		}
		lineHost, linePort := prodLogField(fields, "server_addr"), prodLogField(fields, "server_port")
		parsedPort, err := strconv.Atoi(linePort)
		if err != nil || lineHost == "" || parsedPort < 1 || parsedPort > 65535 {
			return "", 0, errors.New("FRPS selection log is malformed")
		}
		if count > 0 && (lineHost != host || parsedPort != port) {
			return "", 0, errors.New("FRPS selection changed within one prepared process")
		}
		host, port = lineHost, parsedPort
		count++
	}
	if count == 0 {
		return "", 0, errors.New("prepared process emitted no authenticated FRPS selection")
	}
	return host, port, nil
}

func containsProdField(fields []string, wanted string) bool {
	for _, field := range fields {
		if field == wanted {
			return true
		}
	}
	return false
}

func prodLogField(fields []string, name string) string {
	prefix := name + "="
	value := ""
	for _, field := range fields {
		if strings.HasPrefix(field, prefix) {
			if value != "" {
				return ""
			}
			value = strings.Trim(strings.TrimPrefix(field, prefix), `"`)
		}
	}
	return value
}

func verifyProdClosure(t *testing.T, inputs prodLifecycleInputs, cfg prodClosureConfig) { //nolint:gocritic // The phase owns immutable copies across concurrent attempts.
	t.Helper()
	intentRaw := readProdPrivateSibling(t, cfg.IntentPath)
	intent := decodeAndValidateProdClosureIntent(t, intentRaw, cfg)
	cleanupComplete := false
	t.Cleanup(func() {
		if cleanupComplete {
			return
		}
		if err := cleanupProdClosureArtifacts(inputs, cfg.BaseDir, intent.Artifacts); err != nil {
			t.Errorf("cleanup failed closure verify: %v", err)
		}
	})
	journalRaw := readProdPrivateSibling(t, cfg.JournalPath)
	if digestBytes(journalRaw) != intent.JournalSHA256 {
		t.Fatal("closure journal differs from precommitted intent")
	}
	var journal prodClosureJournal
	strictProdJSON(t, journalRaw, &journal, "closure journal")
	validateProdClosureJournal(t, cfg, journal, true)
	if !bytes.Equal(mustProdCanonicalJSON(journal.Artifacts), mustProdCanonicalJSON(intent.Artifacts)) {
		t.Fatal("closure intent artifacts differ from its journal")
	}
	for i := range intent.Artifacts {
		validatePreparedProdClosureArtifact(t, inputs, cfg, i, intent.Artifacts[i])
	}

	directPath := resolveProdClosureRelative(t, cfg.BaseDir, intent.DirectDeploymentFile)
	relayPath := resolveProdClosureRelative(t, cfg.BaseDir, intent.RelayDeploymentFile)
	direct, relay := expectedProdDeployments(inputs.Manifest.Customer.Canonical)
	assertProdClosureDeployment(t, directPath, intent.DirectDeploymentSHA, direct)
	assertProdClosureDeployment(t, relayPath, intent.RelayDeploymentSHA, relay)
	preflightProdClosureEndpoints(t, cfg.Authority)

	results := make(chan prodClosureAttemptResult, len(intent.Artifacts)+2)
	var wg sync.WaitGroup
	for i := range intent.Artifacts {
		wg.Add(1)
		go func(index int, value prodClosureArtifact) {
			defer wg.Done()
			result := runProdClosedPublishAttempt(inputs, directPath, cfg.BaseDir, value)
			result.name, result.index = "frps", index
			results <- result
		}(i, intent.Artifacts[i])
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		results <- runProdClosedGetAttempt(inputs, directPath, intent.Artifacts[0].CRID, "direct")
	}()
	go func() {
		defer wg.Done()
		results <- runProdClosedGetAttempt(inputs, relayPath, intent.Artifacts[0].CRID, "relay")
	}()
	wg.Wait()
	close(results)

	report := prodClosureVerifyReport{
		Schema: prodClosureSchema, Environment: "prod", Operation: "verify", ReleaseID: cfg.Authority.ReleaseID,
		RunID: cfg.Authority.RunID, RunAttempt: cfg.Authority.RunAttempt,
		JournalSHA256: digestBytes(journalRaw), IntentSHA256: digestBytes(intentRaw),
		BundleSHA256: prodClosureBundleDigest(journalRaw, intentRaw, intent.Artifacts),
		FRPS:         make([]prodClosureFRPSResult, len(intent.Artifacts)), ResourceInventoryUnchanged: true,
	}
	allRestartDenied := true
	for result := range results {
		if result.err != nil {
			t.Errorf("closed %s attempt: %v", result.name, result.err)
		}
		switch result.name {
		case "direct":
			report.Direct = prodClosureNegative{Attempted: true, Success: result.success, Ready: result.ready, Outcome: result.outcome}
		case "relay":
			report.Relay = prodClosureNegative{Attempted: true, Success: result.success, Ready: result.ready, Outcome: result.outcome}
		case "frps":
			report.FRPS[result.index] = prodClosureFRPSResult{
				Selector: intent.Artifacts[result.index].Selector, Attempted: true, Success: result.success, Ready: result.ready, Outcome: result.outcome,
			}
			allRestartDenied = allRestartDenied && !result.success && !result.ready
		}
	}
	report.AgentRestart = prodClosureNegative{
		Attempted: true, Success: !allRestartDenied, Ready: !allRestartDenied,
		Outcome: prodClosureOutcomeNoReply,
	}
	if !allRestartDenied {
		report.AgentRestart.Outcome = prodClosureOutcomeSuccess
	}
	for i := range intent.Artifacts {
		if err := assertProdClosureResourceUnchanged(inputs, cfg.BaseDir, intent.Artifacts[i]); err != nil {
			report.ResourceInventoryUnchanged = false
			t.Error(err)
		}
	}
	if report.Direct.Success || report.Direct.Ready || report.Direct.Outcome != prodClosureOutcomeNoReply ||
		report.Relay.Success || report.Relay.Ready || report.Relay.Outcome != prodClosureOutcomeRelay503 || !allRestartDenied {
		t.Error("public customer admission succeeded after the maintenance gate closed")
	}
	for _, result := range report.FRPS {
		if result.Outcome != prodClosureOutcomeNoReply {
			t.Error("FRPS restart did not end at the exact bounded no-reply boundary")
		}
	}
	if t.Failed() {
		return
	}
	if err := cleanupProdClosureArtifacts(inputs, cfg.BaseDir, intent.Artifacts); err != nil {
		t.Fatalf("cleanup closure verification authority: %v", err)
	}
	cleanupComplete = true
	report.CleanupComplete = true
	writeCanonicalPrivateJSON(t, cfg.ReportPath, report, true)
}

func runProdClosedPublishAttempt(inputs prodLifecycleInputs, deploymentPath, baseDir string, artifact prodClosureArtifact) prodClosureAttemptResult { //nolint:gocritic // A goroutine owns one immutable protected-run snapshot.
	statePath, err := safeProdClosureRelative(baseDir, artifact.StateFile)
	if err != nil {
		return prodClosureAttemptResult{err: err}
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "must-not-open\n") }))
	defer target.Close()
	ctx, cancel := context.WithTimeout(context.Background(), prodClosureVerifyBudget)
	defer cancel()
	//nolint:gosec // The protected source validates the binary and all closed arguments before this call.
	cmd := exec.CommandContext(ctx, inputs.Binary, "--endpoint", inputs.Manifest.Customer.APIEndpoint, "--quiet", "publish", target.URL, "--id", artifact.ConnectorID, "--refresh-mode", "disabled")
	cmd.Env = prodCommandEnv(inputs, deploymentPath, artifact.AgentID, filepath.Dir(statePath))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	text := stderr.String()
	ready := strings.Contains(text, "event=proxy_ready")
	success := ready || strings.Contains(text, "event=login_success") || strings.TrimSpace(stdout.String()) != ""
	if runErr == nil {
		success = true
	}
	if success {
		return prodClosureAttemptResult{success: true, ready: ready, outcome: prodClosureOutcomeSuccess}
	}
	outcome, err := classifyProdClosureNoReply(ctx.Err(), runErr, text)
	if err != nil {
		return prodClosureAttemptResult{err: err}
	}
	return prodClosureAttemptResult{outcome: outcome}
}

func runProdClosedGetAttempt(inputs prodLifecycleInputs, deploymentPath, crid, name string) prodClosureAttemptResult { //nolint:gocritic // A goroutine owns one immutable protected-run snapshot.
	ctx, cancel := context.WithTimeout(context.Background(), prodClosureVerifyBudget)
	defer cancel()
	dir, err := os.MkdirTemp("", "qurl-prod-close-get-")
	if err != nil {
		return prodClosureAttemptResult{name: name, err: err}
	}
	defer func() { _ = os.RemoveAll(dir) }()
	//nolint:gosec // The temporary state directory must be private, so 0700 is intentional.
	if err := os.Chmod(dir, 0o700); err != nil {
		return prodClosureAttemptResult{name: name, err: err}
	}
	destination := filepath.Join(dir, "download")
	//nolint:gosec // The protected source validates the binary, CRID, and closed deployment before this call.
	cmd := exec.CommandContext(ctx, inputs.Binary, "--endpoint", inputs.Manifest.Customer.APIEndpoint, "--quiet", "get", crid, "--file", destination)
	cmd.Env = prodCommandEnv(inputs, deploymentPath, "qurl-prod-close-get", dir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	info, statErr := os.Stat(destination)
	success := runErr == nil || (statErr == nil && info.Size() > 0)
	if success {
		return prodClosureAttemptResult{name: name, success: true, ready: true, outcome: prodClosureOutcomeSuccess}
	}
	if name == "relay" {
		if !exactProdRelayMaintenanceDenial(stderr.String(), inputs.Manifest.Customer.Canonical.RelayHostname) {
			return prodClosureAttemptResult{name: name, err: errors.New("relay attempt did not receive exact maintenance 503")}
		}
		return prodClosureAttemptResult{name: name, outcome: prodClosureOutcomeRelay503}
	}
	outcome, classifyErr := classifyProdClosureNoReply(ctx.Err(), runErr, stderr.String())
	if classifyErr != nil {
		return prodClosureAttemptResult{name: name, err: classifyErr}
	}
	return prodClosureAttemptResult{name: name, outcome: outcome}
}

func classifyProdClosureNoReply(ctxErr, runErr error, stderr string) (string, error) {
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		return prodClosureOutcomeNoReply, nil
	}
	if runErr == nil {
		return "", errors.New("closed attempt returned success without a customer outcome")
	}
	for _, marker := range []string{
		"qurl: no reply from ",
		"qurl: endpoint never replied",
		"native NHP registration did not complete",
	} {
		if strings.Contains(stderr, marker) {
			return prodClosureOutcomeNoReply, nil
		}
	}
	return "", errors.New("closed attempt failed outside the exact bounded no-reply contract")
}

func exactProdRelayMaintenanceDenial(stderr, expectedHost string) bool {
	const marker = ` -> 503: {"error":"maintenance"}`
	return expectedHost != "" && strings.Count(stderr, "relay POST ") == 1 && strings.Count(stderr, marker) == 1 &&
		strings.Contains(stderr, "relay POST https://"+expectedHost+"/relay/")
}

func preflightProdClosureEndpoints(t *testing.T, authority prodClosureAuthority) { //nolint:gocritic // The resolver consumes one immutable authority snapshot.
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, endpoint := range []struct{ name, host string }{
		{"server", authority.ServerEndpoint},
		{"ac", authority.ACEndpoint},
		{"relay", authority.RelayHostname},
	} {
		addresses, err := net.DefaultResolver.LookupHost(ctx, endpoint.host)
		if err != nil || len(addresses) == 0 {
			t.Fatalf("%s closure endpoint did not resolve before the bounded attempts: %v", endpoint.name, err)
		}
	}
}

func cleanupProdClosureArtifacts(inputs prodLifecycleInputs, baseDir string, artifacts []prodClosureArtifact) error { //nolint:gocritic // Cleanup callbacks own the immutable credential snapshot.
	var failures []string
	for i := range artifacts {
		artifact := &artifacts[i]
		statePath, err := safeProdClosureRelative(baseDir, artifact.StateFile)
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		if _, err := os.Lstat(statePath); errors.Is(err, os.ErrNotExist) && !artifact.Prepared {
			// The private state file is persisted before the first network side
			// effect. Its absence for an unprepared row means no remote cleanup
			// authority can exist yet.
			continue
		} else if err != nil {
			failures = append(failures, artifact.AgentID+": inspect cleanup state: "+err.Error())
			continue
		}
		if err := validateProdClosurePrivateTree(baseDir, filepath.Dir(artifact.StateFile)); err != nil {
			failures = append(failures, artifact.AgentID+": "+err.Error())
			continue
		}
		stateDir := filepath.Dir(statePath)
		ctx, cancel := context.WithTimeout(context.Background(), prodCleanupTimeout)
		err = cleanupProdAuthority(ctx, inputs.Manifest.Customer.APIEndpoint, inputs.CleanupJWT,
			prodRunNamespace{AgentID: artifact.AgentID, ConnectorID: artifact.ConnectorID}, stateDir,
			prodCleanupOps{loadState: loadProdAgentState, deleteResource: deleteProdResource, revokeDevice: revokeProdDeviceCredential})
		cancel()
		if err != nil {
			failures = append(failures, artifact.AgentID+": "+err.Error())
		}
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

func assertProdClosureResourceUnchanged(inputs prodLifecycleInputs, baseDir string, artifact prodClosureArtifact) error { //nolint:gocritic // The assertion owns one immutable protected-run snapshot.
	statePath, err := safeProdClosureRelative(baseDir, artifact.StateFile)
	if err != nil {
		return err
	}
	loaded, err := loadProdAgentState(filepath.Dir(statePath))
	if err != nil || loaded.DeviceAPIKey == "" || loaded.DeviceAPIKeyID != artifact.DeviceAPIKeyID {
		return errors.New("closure device identity changed during denied attempts")
	}
	client, err := prodConnectorClient(inputs.Manifest.Customer.APIEndpoint, loaded.DeviceAPIKey)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), prodCleanupTimeout)
	defer cancel()
	resource, err := client.GetConnectorResourceBySlug(ctx, artifact.ConnectorID)
	if err != nil || resource == nil || resource.ResourceID != artifact.ResourceID || resource.CRID != artifact.CRID {
		return errors.New("closure resource inventory changed during denied attempts")
	}
	return nil
}

func prodConnectorClient(endpoint, key string) (*qurl.Client, error) {
	origin, err := connectoragent.ResourceSDKOrigin(endpoint)
	if err != nil {
		return nil, errors.New("derive closure resource API origin failed")
	}
	client, err := qurl.NewClient(qurl.BearerToken(key), qurl.WithBaseURL(origin))
	if err != nil {
		return nil, errors.New("open closure resource client failed")
	}
	return client, nil
}

func validateProdClosureArtifactIdentity(t *testing.T, authority prodClosureAuthority, index int, artifact prodClosureArtifact) { //nolint:gocritic // Exact identity comparison intentionally copies closed values.
	t.Helper()
	if index < 0 || index >= len(authority.FRPSSelectors) || artifact.Selector != authority.FRPSSelectors[index] {
		t.Fatal("closure artifact selector differs from exact authority")
	}
	want := prodClosureNamespace(t, authority, index)
	wantState := filepath.ToSlash(filepath.Join(prodClosureStateDirName, artifact.Selector.ResourceID, connectorstate.AgentStateFile))
	if artifact.AgentID != want.AgentID || artifact.ConnectorID != want.ConnectorID || artifact.StateFile != wantState {
		t.Fatal("closure artifact identity differs from its deterministic namespace")
	}
}

func validatePreparedProdClosureArtifact(t *testing.T, inputs prodLifecycleInputs, cfg prodClosureConfig, index int, artifact prodClosureArtifact) { //nolint:gocritic // Validation owns immutable protected-run and artifact snapshots.
	t.Helper()
	validateProdClosureArtifactIdentity(t, cfg.Authority, index, artifact)
	if !artifact.Prepared || artifact.AgentID == "" || artifact.ConnectorID == "" || artifact.DeviceAPIKeyID == "" ||
		artifact.ResourceID == "" || artifact.CRID == "" || !prodHex64.MatchString(artifact.StateSHA256) {
		t.Fatal("closure artifact is not complete")
	}
	statePath := resolveProdClosureRelative(t, cfg.BaseDir, artifact.StateFile)
	if err := validateProdClosurePrivateTree(cfg.BaseDir, filepath.Dir(artifact.StateFile)); err != nil {
		t.Fatal(err)
	}
	if sha256PrivateFile(t, statePath) != artifact.StateSHA256 {
		t.Fatal("closure state differs from precommitted digest")
	}
	if err := assertProdClosureResourceUnchanged(inputs, cfg.BaseDir, artifact); err != nil {
		t.Fatal(err)
	}
}

func decodeAndValidateProdClosureIntent(t *testing.T, raw []byte, cfg prodClosureConfig) prodClosureIntent { //nolint:gocritic // Intent validation owns one immutable authority snapshot.
	t.Helper()
	var intent prodClosureIntent
	strictProdJSON(t, raw, &intent, "closure intent")
	if intent.Schema != prodClosureSchema || intent.Environment != "prod" || !equalProdClosureAuthority(intent.Authority, cfg.Authority) ||
		len(intent.Artifacts) != prodClosureSelectorCount || !prodHex64.MatchString(intent.JournalSHA256) {
		t.Fatal("closure intent differs from current exact authority")
	}
	if intent.DirectDeploymentFile != prodClosureDirectDeployName || intent.RelayDeploymentFile != prodClosureRelayDeployName ||
		!prodHex64.MatchString(intent.DirectDeploymentSHA) || !prodHex64.MatchString(intent.RelayDeploymentSHA) {
		t.Fatal("closure intent deployment authority is malformed")
	}
	for i := range intent.Artifacts {
		validateProdClosureArtifactIdentity(t, cfg.Authority, i, intent.Artifacts[i])
		if !intent.Artifacts[i].Prepared {
			t.Fatal("closure intent contains an incomplete artifact")
		}
	}
	return intent
}

func equalProdClosureAuthority(left, right prodClosureAuthority) bool { //nolint:gocritic // Canonical comparison intentionally copies closed values.
	return bytes.Equal(mustProdCanonicalJSON(left), mustProdCanonicalJSON(right))
}

func assertProdClosureDeployment(t *testing.T, path, wantDigest string, want qurl.Deployment) { //nolint:gocritic // The assertion owns one immutable deployment projection.
	t.Helper()
	if sha256File(t, path) != wantDigest {
		t.Fatal("closure deployment digest differs from intent")
	}
	loaded, err := qurl.LoadDeployment(path)
	if err != nil || !reflectDeepEqualDeployment(loaded, want) {
		t.Fatal("closure deployment bytes differ from signed manifest projection")
	}
}

func reflectDeepEqualDeployment(left *qurl.Deployment, right qurl.Deployment) bool { //nolint:gocritic // Canonical comparison owns one immutable deployment projection.
	return left != nil && bytes.Equal(mustProdCanonicalJSON(*left), mustProdCanonicalJSON(right))
}

func writeProdClosureJournal(t *testing.T, path string, journal *prodClosureJournal) []byte {
	t.Helper()
	return writeCanonicalPrivateJSON(t, path, journal, true)
}

func writeProdClosureOperationReport(t *testing.T, cfg prodClosureConfig, journalRaw, intentRaw []byte, artifacts []prodClosureArtifact) { //nolint:gocritic // Report emission owns a stable authority snapshot.
	t.Helper()
	report := prodClosureOperationReport{
		Schema: prodClosureSchema, Environment: "prod", Operation: "prepare", ReleaseID: cfg.Authority.ReleaseID,
		RunID: cfg.Authority.RunID, RunAttempt: cfg.Authority.RunAttempt,
		JournalSHA256: digestBytes(journalRaw), IntentSHA256: digestBytes(intentRaw),
		BundleSHA256: prodClosureBundleDigest(journalRaw, intentRaw, artifacts),
	}
	writeCanonicalPrivateJSON(t, cfg.ReportPath, report, true)
}

func prodClosureBundleDigest(journalRaw, intentRaw []byte, artifacts []prodClosureArtifact) string {
	parts := make([]string, 0, 2+2*len(artifacts))
	parts = append(parts, digestBytes(journalRaw), digestBytes(intentRaw))
	for i := range artifacts {
		parts = append(parts, artifacts[i].StateFile, artifacts[i].StateSHA256)
	}
	return digestBytes([]byte(strings.Join(parts, "\x00")))
}

func writeCanonicalPrivateJSON(t *testing.T, path string, value any, replace bool) []byte {
	t.Helper()
	raw := append(mustProdCanonicalJSON(value), '\n')
	dir := filepath.Dir(path)
	if info, err := os.Lstat(dir); err != nil || validateProdClosurePrivateDirInfo(info) != nil {
		t.Fatalf("private authority directory is unsafe: %v", err)
	}
	if !replace {
		if existing, ok := readExistingProdPrivateSibling(t, path); ok {
			if !bytes.Equal(existing, raw) {
				t.Fatalf("existing private authority %s differs", filepath.Base(path))
			}
			return raw
		}
	}
	if _, err := os.Lstat(path); err == nil {
		_ = readProdPrivateSibling(t, path)
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspect private authority %s: %v", filepath.Base(path), err)
	}
	tmp, err := os.CreateTemp(dir, ".closure-write-*")
	if err != nil {
		t.Fatalf("create private authority temp: %v", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		t.Fatalf("chmod private authority temp: %v", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		t.Fatalf("write private authority temp: %v", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		t.Fatalf("sync private authority temp: %v", err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatalf("close private authority temp: %v", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		t.Fatalf("install private authority: %v", err)
	}
	//nolint:gosec // dir is the already validated private sibling directory.
	directory, err := os.Open(dir)
	if err != nil {
		t.Fatalf("open authority directory for sync: %v", err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil || closeErr != nil {
		t.Fatalf("sync authority directory: sync=%v close=%v", syncErr, closeErr)
	}
	return raw
}

func mustProdCanonicalJSON(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}

func readProdPrivateSibling(t *testing.T, path string) []byte {
	t.Helper()
	return readProdAuthorityFile(t, path, true, prodAuthorityMaxBytes)
}

func readExistingProdPrivateSibling(t *testing.T, path string) ([]byte, bool) {
	t.Helper()
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, false
	} else if err != nil {
		t.Fatalf("inspect private authority %s: %v", filepath.Base(path), err)
	}
	return readProdPrivateSibling(t, path), true
}

func resolveProdClosureRelative(t *testing.T, baseDir, relative string) string {
	t.Helper()
	path, err := safeProdClosureRelative(baseDir, relative)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func safeProdClosureRelative(baseDir, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) || filepath.ToSlash(filepath.Clean(relative)) != relative || strings.HasPrefix(relative, "../") {
		return "", errors.New("closure authority contains an unsafe relative path")
	}
	path := filepath.Join(baseDir, filepath.FromSlash(relative))
	if filepath.Dir(path) == "." || !strings.HasPrefix(path+string(os.PathSeparator), filepath.Clean(baseDir)+string(os.PathSeparator)) {
		return "", errors.New("closure authority path escapes its private directory")
	}
	return path, nil
}

func validateProdClosurePrivateDirInfo(info os.FileInfo) error {
	if info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return errors.New("directory is not an exact private directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (stat.Uid != uint32(os.Geteuid()) && stat.Uid != 0) {
		return errors.New("directory owner is not trusted")
	}
	return nil
}

func ensureProdClosurePrivateTree(baseDir, relativeDir string) error {
	return walkProdClosurePrivateTree(baseDir, relativeDir, true)
}

func validateProdClosurePrivateTree(baseDir, relativeDir string) error {
	return walkProdClosurePrivateTree(baseDir, relativeDir, false)
}

func walkProdClosurePrivateTree(baseDir, relativeDir string, create bool) error {
	if relativeDir == "." || relativeDir == "" || filepath.IsAbs(relativeDir) ||
		filepath.ToSlash(filepath.Clean(relativeDir)) != relativeDir || strings.HasPrefix(relativeDir, "../") {
		return errors.New("closure private directory path is unsafe")
	}
	baseInfo, err := os.Lstat(baseDir)
	if err != nil || validateProdClosurePrivateDirInfo(baseInfo) != nil {
		return errors.New("closure base directory is unsafe")
	}
	current := baseDir
	for _, component := range strings.Split(relativeDir, "/") {
		if component == "" || component == "." || component == ".." {
			return errors.New("closure private directory component is unsafe")
		}
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) && create {
			if err := os.Mkdir(current, 0o700); err != nil {
				return fmt.Errorf("create closure private directory: %w", err)
			}
			info, statErr = os.Lstat(current)
		}
		if statErr != nil || validateProdClosurePrivateDirInfo(info) != nil {
			return errors.New("closure private directory tree is unsafe")
		}
	}
	return nil
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	raw := readProdAuthorityFile(t, path, false, 256<<20)
	return digestBytes(raw)
}

func sha256PrivateFile(t *testing.T, path string) string {
	t.Helper()
	raw := readProdAuthorityFile(t, path, true, 256<<20)
	return digestBytes(raw)
}

func digestBytes(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func TestProdMatchedCohortClosureSelectorContract(t *testing.T) {
	raw := []byte(`[{"resource_id":"qurl-tunnel-server-a","host":"connect.layerv.ai","port":7000},{"resource_id":"qurl-tunnel-server-b","host":"connect.layerv.ai","port":7001},{"resource_id":"qurl-tunnel-server-c","host":"connect.layerv.ai","port":7002}]`)
	selectors, err := decodeProdFRPSSelectors(raw)
	if err != nil {
		t.Fatal(err)
	}
	if selectedProdFRPSResource("CQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", selectors) == "" {
		t.Fatal("rendezvous placement returned no selector")
	}
	for name, mutated := range map[string][]byte{
		"ports only":       []byte(`[7000,7001,7002]`),
		"unknown":          bytes.Replace(raw, []byte(`"port":7000`), []byte(`"port":7000,"extra":true`), 1),
		"wrong id":         bytes.Replace(raw, []byte("qurl-tunnel-server-b"), []byte("qurl-tunnel-server-z"), 1),
		"wrong host":       bytes.Replace(raw, []byte(prodClosureExpectedFRPSHost), []byte("other.layerv.ai"), 1),
		"duplicate target": bytes.Replace(raw, []byte(`"port":7001`), []byte(`"port":7000`), 1),
		"noncanonical":     append([]byte(" "), raw...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeProdFRPSSelectors(mutated); err == nil {
				t.Fatal("mutated selector authority accepted")
			}
		})
	}
}

func TestProdMatchedCohortClosureDeniedOutcomeContract(t *testing.T) {
	if got, err := classifyProdClosureNoReply(context.DeadlineExceeded, errors.New("signal: killed"), ""); err != nil || got != prodClosureOutcomeNoReply {
		t.Fatalf("deadline classification = %q, %v", got, err)
	}
	if got, err := classifyProdClosureNoReply(nil, errors.New("exit 1"), "qurl: no reply from access.nhp.layerv.ai after 2 attempt(s)"); err != nil || got != prodClosureOutcomeNoReply {
		t.Fatalf("no-reply classification = %q, %v", got, err)
	}
	for name, stderr := range map[string]string{
		"local configuration": "deployment file is malformed",
		"dns":                 "lookup access.nhp.layerv.ai: no such host",
		"policy denial":       "registration key rejected",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := classifyProdClosureNoReply(nil, errors.New("exit 1"), stderr); err == nil {
				t.Fatal("unrelated failure accepted as a closed admission")
			}
		})
	}
	if !exactProdRelayMaintenanceDenial(`qurl: relay POST https://relay.layerv.ai/relay/server -> 503: {"error":"maintenance"}`, "relay.layerv.ai") {
		t.Fatal("exact relay maintenance response rejected")
	}
	for _, invalid := range []string{
		`qurl: relay POST https://relay.layerv.ai/relay/server -> 502: {"error":"maintenance"}`,
		`qurl: relay POST https://relay.layerv.ai/relay/server -> 503: {"error":"other"}`,
		`qurl: relay POST http://relay.layerv.ai/relay/server -> 503: {"error":"maintenance"}`,
		`qurl: relay POST https://other.layerv.ai/relay/server -> 503: {"error":"maintenance"}`,
	} {
		if exactProdRelayMaintenanceDenial(invalid, "relay.layerv.ai") {
			t.Fatal("noncanonical relay failure accepted as maintenance")
		}
	}
}

func TestProdMatchedCohortClosurePrivateTreeContract(t *testing.T) {
	base := t.TempDir()
	//nolint:gosec // The authority-tree fixture must be private, so 0700 is intentional.
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ensureProdClosurePrivateTree(base, "closure-state/qurl-tunnel-server-a"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(filepath.Join(base, "closure-state", "qurl-tunnel-server-a")); err != nil || validateProdClosurePrivateDirInfo(info) != nil {
		t.Fatalf("private tree = %v, %v", info, err)
	}
	missing := filepath.Join(base, "missing")
	if err := validateProdClosurePrivateTree(base, "missing/child"); err == nil {
		t.Fatal("missing authority tree accepted by the verify-only validator")
	}
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verify-only validation created a missing authority tree: %v", err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(base, "symlink")); err != nil {
		t.Fatal(err)
	}
	if err := ensureProdClosurePrivateTree(base, "symlink/child"); err == nil {
		t.Fatal("symlinked authority tree accepted")
	}
	//nolint:gosec // The intentionally unsafe fixture verifies rejection of a broad directory mode.
	if err := os.Mkdir(filepath.Join(base, "wide"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureProdClosurePrivateTree(base, "wide/child"); err == nil {
		t.Fatal("non-private authority tree accepted")
	}
}

func TestProdMatchedCohortClosureRunIdentityContract(t *testing.T) {
	for _, valid := range []string{"1", "32600000000", "18446744073709551615"} {
		if !canonicalProdUint64(valid) {
			t.Fatalf("canonical uint64 %q rejected", valid)
		}
	}
	for _, invalid := range []string{"", "0", "01", "-1", "18446744073709551616"} {
		if canonicalProdUint64(invalid) {
			t.Fatalf("noncanonical uint64 %q accepted", invalid)
		}
	}
}

func TestProdMatchedCohortClosurePersistedAuthorityContainsNoBearer(t *testing.T) {
	authority := prodClosureAuthority{
		ReleaseID: strings.Repeat("a", 64), RunID: "32600000000", RunAttempt: "1",
		NHPSourceSHA: strings.Repeat("b", 40), QURLSourceSHA: strings.Repeat("c", 40),
		QURLBinarySHA256: strings.Repeat("d", 64), CandidateDeploymentSHA256: strings.Repeat("e", 64),
		CanonicalDeploymentSHA256: strings.Repeat("f", 64), ServerEndpoint: "cell0.nhp.layerv.ai",
		ACEndpoint: "access.nhp.layerv.ai", RelayHostname: "relay.layerv.ai",
		FRPSSelectors: []prodFRPSSelector{{ResourceID: "qurl-tunnel-server-a", Host: prodClosureExpectedFRPSHost, Port: 7000}},
	}
	raw := string(mustProdCanonicalJSON(prodClosureJournal{Schema: 1, Environment: "prod", Authority: authority, Status: "preparing"}))
	for _, forbidden := range []string{"api_key\"", "cleanup_jwt", "bearer", "enrollment_token", "ac_token", "private_key"} {
		if strings.Contains(strings.ToLower(raw), forbidden) {
			t.Fatalf("persisted closure authority contains forbidden secret field %q", forbidden)
		}
	}
}

func TestProdMatchedCohortClosureCommandContract(t *testing.T) {
	script, err := filepath.Abs(filepath.Join("..", "scripts", "probe-prod-matched-cohort-admission-closed"))
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	//nolint:gosec // The command fixture authority directory must be private, so 0700 is intentional.
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeTest := filepath.Join(base, "closure-test")
	//nolint:gosec // This executable fixture is private inside t.TempDir.
	if err := os.WriteFile(fakeTest, []byte("#!/bin/sh\nset -eu\necho PASS\nprintf '%s\\n' '{\"schema\":1}' >\"$QURL_PROD_MATCHED_COHORT_CLOSURE_REPORT_FILE\"\nchmod 600 \"$QURL_PROD_MATCHED_COHORT_CLOSURE_REPORT_FILE\"\n"), 0o500); err != nil {
		t.Fatal(err)
	}
	intent := filepath.Join(base, "intent.json")
	journal := filepath.Join(base, "journal.json")
	report := filepath.Join(base, "report.json")
	selectors := `[{"resource_id":"qurl-tunnel-server-a","host":"connect.layerv.ai","port":7000},{"resource_id":"qurl-tunnel-server-b","host":"connect.layerv.ai","port":7001},{"resource_id":"qurl-tunnel-server-c","host":"connect.layerv.ai","port":7002}]`
	args := []string{
		"--operation", "prepare", "--intent-file", intent, "--journal-file", journal, "--report-file", report,
		"--server-endpoint", "cell0.nhp.layerv.ai", "--ac-endpoint", "access.nhp.layerv.ai",
		"--frps-selectors-json", selectors, "--relay-hostname", "relay.layerv.ai",
	}
	run := func(arguments []string, binary string) ([]byte, []byte, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		//nolint:gosec // Exact repository-owned script and closed fixture arguments.
		cmd := exec.CommandContext(ctx, script, arguments...)
		cmd.Env = prodClosureCommandFixtureEnv(binary)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		return stdout.Bytes(), stderr.Bytes(), err
	}

	stdout, stderr, err := run(args, fakeTest)
	if err != nil || len(stdout) != 0 || len(stderr) != 0 {
		t.Fatalf("closure command stdout = %q stderr = %q, %v", stdout, stderr, err)
	}
	//nolint:gosec // The report path is the exact private t.TempDir fixture path.
	reportRaw, err := os.ReadFile(report)
	if err != nil || string(reportRaw) != "{\"schema\":1}\n" {
		t.Fatalf("closure report = %q, %v", reportRaw, err)
	}
	reportInfo, err := os.Lstat(report)
	if err != nil || !reportInfo.Mode().IsRegular() || reportInfo.Mode().Perm() != 0o600 {
		t.Fatalf("closure report metadata = %#v, %v", reportInfo, err)
	}
	if _, err := os.Lstat(report + ".failure.log"); !os.IsNotExist(err) {
		t.Fatalf("successful closure retained diagnostics: %v", err)
	}

	failingTest := filepath.Join(base, "failing-closure-test")
	const privateDiagnostic = "PRIVATE_BEARER_SENTINEL"
	//nolint:gosec // This executable fixture is private inside t.TempDir.
	if err := os.WriteFile(failingTest, []byte("#!/bin/sh\nset -eu\npython3 - <<'PY'\nimport sys\nsys.stdout.write('"+privateDiagnostic+"\\n' + ('x' * 70000))\nsys.stderr.write('\\nPRIVATE_CHILD_STDERR\\n')\nPY\nexit 7\n"), 0o500); err != nil {
		t.Fatal(err)
	}
	failureReport := filepath.Join(base, "failure-report.json")
	failureArgs := append([]string(nil), args...)
	failureArgs[7] = failureReport
	stdout, stderr, err = run(failureArgs, failingTest)
	if err == nil || len(stdout) != 0 {
		t.Fatalf("failing closure stdout = %q stderr = %q, %v", stdout, stderr, err)
	}
	wantFailure := "closure test failed with status 7; private diagnostics retained at " + failureReport + ".failure.log\n"
	if string(stderr) != wantFailure || bytes.Contains(stderr, []byte(privateDiagnostic)) || bytes.Contains(stderr, []byte("PRIVATE_CHILD_STDERR")) {
		t.Fatalf("failing closure stderr = %q, want %q", stderr, wantFailure)
	}
	diagnosticsPath := failureReport + ".failure.log"
	//nolint:gosec // The diagnostics path is derived from the exact private fixture report path.
	diagnostics, err := os.ReadFile(diagnosticsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) > 65_536 || !bytes.Contains(diagnostics, []byte(privateDiagnostic)) ||
		!bytes.HasSuffix(diagnostics, []byte("\n[diagnostics truncated]\n")) {
		t.Fatalf("bounded private diagnostics size=%d prefix=%q suffix=%q", len(diagnostics), diagnostics[:min(len(diagnostics), 32)], diagnostics[max(0, len(diagnostics)-32):])
	}
	diagnosticsInfo, err := os.Lstat(diagnosticsPath)
	if err != nil || !diagnosticsInfo.Mode().IsRegular() || diagnosticsInfo.Mode().Perm() != 0o600 {
		t.Fatalf("diagnostics metadata = %#v, %v", diagnosticsInfo, err)
	}
	if system, ok := diagnosticsInfo.Sys().(*syscall.Stat_t); !ok || system.Nlink != 1 || (system.Uid != 0 && system.Uid != uint32(os.Geteuid())) {
		t.Fatalf("diagnostics owner/link authority = %#v", diagnosticsInfo.Sys())
	}

	stdout, stderr, err = run(failureArgs, fakeTest)
	if err != nil || len(stdout) != 0 || len(stderr) != 0 {
		t.Fatalf("closure retry stdout = %q stderr = %q, %v", stdout, stderr, err)
	}
	if _, err := os.Lstat(diagnosticsPath); !os.IsNotExist(err) {
		t.Fatalf("successful closure retry retained diagnostics: %v", err)
	}
	if err := os.Remove(failureReport); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside-diagnostics")
	if err := os.WriteFile(outside, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, diagnosticsPath); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err = run(failureArgs, fakeTest)
	if err == nil || len(stdout) != 0 || string(stderr) != "prior closure diagnostics file is unsafe\n" {
		t.Fatalf("unsafe diagnostics invocation stdout = %q stderr = %q, %v", stdout, stderr, err)
	}
	if _, err := os.Lstat(failureReport); !os.IsNotExist(err) {
		t.Fatalf("unsafe diagnostics path did not fence the child: %v", err)
	}
	outsideRaw, err := os.ReadFile(outside) //nolint:gosec // Exact private fixture path.
	if err != nil || string(outsideRaw) != "unchanged" {
		t.Fatalf("unsafe diagnostics target changed to %q, %v", outsideRaw, err)
	}

	args[12] = "--ac-frps-ports-json"
	args[13] = "[7000,7001,7002]"
	stdout, stderr, err = run(args, fakeTest)
	if err == nil || len(stdout) != 0 || !bytes.Contains(stderr, []byte("ports-only FRPS authority is forbidden")) {
		t.Fatalf("ports-only invocation stdout = %q stderr = %q, %v", stdout, stderr, err)
	}
}

func prodClosureCommandFixtureEnv(fakeTest string) []string {
	return []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"QURL_PROD_MATCHED_COHORT_TEST_BINARY=" + fakeTest,
		"QURL_PROD_MATCHED_COHORT_RELEASE_MANIFEST=/authority/release.json",
		"QURL_PROD_MATCHED_COHORT_RELEASE_ID=" + strings.Repeat("a", 64),
		"QURL_PROD_MATCHED_COHORT_BINARY=/authority/qurl",
		"QURL_PROD_CANDIDATE_DEPLOYMENT_FILE=/authority/candidate.json",
		"QURL_PROD_CANONICAL_DEPLOYMENT_FILE=/authority/canonical.json",
		"QURL_API_KEY_FILE=/authority/api-key",
		"QURL_CLI_PROD_CLEANUP_JWT_FILE=/authority/cleanup-jwt",
		"QURL_CLI_PROD_API_KEY_ID_FILE=/authority/key-id",
		"QURL_SHARING_RUN_ID=32600000000",
		"QURL_SHARING_RUN_ATTEMPT=1",
	}
}

func TestProdMatchedCohortClosureLogContract(t *testing.T) {
	valid := "time=x event=knock_overlay_applied server_addr=connect.layerv.ai server_port=7001\n"
	host, port, err := prodSelectedFRPSTarget(valid)
	if err != nil || host != "connect.layerv.ai" || port != 7001 {
		t.Fatalf("selection = %s:%d, %v", host, port, err)
	}
	for _, invalid := range []string{
		"event=knock_overlay_applied server_addr=connect.layerv.ai\n",
		valid + "event=knock_overlay_applied server_addr=connect.layerv.ai server_port=7002\n",
		"event=other server_addr=connect.layerv.ai server_port=7001\n",
	} {
		if _, _, err := prodSelectedFRPSTarget(invalid); err == nil {
			t.Fatal("malformed selection log accepted")
		}
	}
}

func TestProdMatchedCohortClosurePrecommittedStateContract(t *testing.T) {
	selectors, err := decodeProdFRPSSelectors([]byte(`[{"resource_id":"qurl-tunnel-server-a","host":"connect.layerv.ai","port":7000},{"resource_id":"qurl-tunnel-server-b","host":"connect.layerv.ai","port":7001},{"resource_id":"qurl-tunnel-server-c","host":"connect.layerv.ai","port":7002}]`))
	if err != nil {
		t.Fatal(err)
	}
	privateKey, publicKey := findProdSelectorKey(t, selectors[1], selectors)
	defer func() {
		for i := range privateKey {
			privateKey[i] = 0
		}
	}()
	state := &qurl.AgentState{
		AgentID: "qurl-prod-close-r32600000000-a1-b", PrivateKeyB64: base64.StdEncoding.EncodeToString(privateKey),
		PublicKeyB64: base64.StdEncoding.EncodeToString(publicKey),
	}
	dir := t.TempDir()
	//nolint:gosec // The precommitted state fixture must be private, so 0700 is intentional.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, connectorstate.AgentStateFile)
	store, err := qurl.OpenFileAgentState(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgentState(context.Background(), state); err != nil {
		_ = store.Close()
		t.Fatalf("save precommitted state: %v", err)
	}
	loaded, err := store.LoadAgentState(context.Background())
	closeErr := store.Close()
	if err != nil || closeErr != nil || loaded == nil || loaded.AgentID != state.AgentID || loaded.PublicKeyB64 != state.PublicKeyB64 {
		t.Fatalf("precommitted state roundtrip = %#v, load=%v close=%v", loaded, err, closeErr)
	}
	if selectedProdFRPSResource(loaded.PublicKeyB64, selectors) != selectors[1].ResourceID {
		t.Fatal("persisted precommitted state lost its exact FRPS placement")
	}
}
