//go:build cliprodcohort

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/layervai/qurl-go/qurl"
	"golang.org/x/sys/unix"

	connectoragent "github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
)

const (
	prodCohortArmingEnv            = "QURL_PROD_MATCHED_COHORT_LIFECYCLE"
	prodCohortManifestEnv          = "QURL_PROD_MATCHED_COHORT_RELEASE_MANIFEST"
	prodCohortReleaseIDEnv         = "QURL_PROD_MATCHED_COHORT_RELEASE_ID"
	prodCohortTargetEnv            = "QURL_PROD_MATCHED_COHORT_TARGET"
	prodCohortTransportEnv         = "QURL_PROD_MATCHED_COHORT_TRANSPORT"
	prodCohortBinaryEnv            = "QURL_PROD_MATCHED_COHORT_BINARY"
	prodCandidateDeploymentFileEnv = "QURL_PROD_CANDIDATE_DEPLOYMENT_FILE"
	prodCanonicalDeploymentFileEnv = "QURL_PROD_CANONICAL_DEPLOYMENT_FILE"
	prodAPIKeyFileEnv              = "QURL_API_KEY_FILE"
	prodCleanupJWTFileEnv          = "QURL_CLI_PROD_CLEANUP_JWT_FILE"
	prodAPIKeyIDFileEnv            = "QURL_CLI_PROD_API_KEY_ID_FILE"
	prodRunIDEnv                   = "QURL_SHARING_RUN_ID"
	prodRunAttemptEnv              = "QURL_SHARING_RUN_ATTEMPT"
	prodRuntimeEnv                 = "QURL_SHARING_RUNTIME"
	prodAuthorityMaxBytes          = 1 << 20
	prodSecretMaxBytes             = 16 << 10
	prodProcessTimeout             = 90 * time.Second
	prodCleanupTimeout             = 30 * time.Second
	prodPublicNHPPort              = 443
	prodExpectedAPIEndpoint        = "https://api.layerv.ai"
	prodExpectedQURLRepository     = "layervai/qurl-integrations"
	prodExpectedInfraRepository    = "layervai/qurl-integrations-infra"
	prodExpectedSourceRepository   = "layervai/nhp"
	prodExactRetirementRetryTest   = "TestNativeEndCycleRetriesExactReceiptUntilAccepted"
	prodCustomerLifecycleTest      = "TestProdMatchedCohortCustomerLifecycle"
	prodCustomerSourceContractTest = "TestProdMatchedCohortSourceContract"
	prodClosureTest                = "TestProdMatchedCohortAdmissionClosed"
)

var (
	prodHex40            = regexp.MustCompile(`^[0-9a-f]{40}$`)
	prodHex64            = regexp.MustCompile(`^[0-9a-f]{64}$`)
	prodReleaseTag       = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	prodPositiveDecimal  = regexp.MustCompile(`^[1-9]\d{0,19}$`)
	prodCanonicalDNS     = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)
	prodCanonicalKeyID   = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	prodCanonicalSPKIB64 = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

type prodReleaseManifest struct {
	Schema              int                 `json:"schema"`
	Environment         string              `json:"environment"`
	Source              prodReleaseSource   `json:"source"`
	Contract            json.RawMessage     `json:"contract"`
	Components          json.RawMessage     `json:"components"`
	Customer            prodReleaseCustomer `json:"customer"`
	ManifestAttestation json.RawMessage     `json:"manifest_attestation"`
}

type prodReleaseSource struct {
	Repository        string `json:"repository"`
	SHA               string `json:"sha"`
	Tree              string `json:"tree"`
	SignatureVerified bool   `json:"signature_verified"`
}

type prodReleaseCustomer struct {
	QURLRepository      string                `json:"qurl_repository"`
	QURLReleaseTag      string                `json:"qurl_release_tag"`
	QURLReleaseAsset    string                `json:"qurl_release_asset"`
	QURLSourceSHA       string                `json:"qurl_source_sha"`
	QURLArchiveSHA256   string                `json:"qurl_archive_sha256"`
	QURLBinarySHA256    string                `json:"qurl_binary_sha256"`
	QURLChecksumsSHA256 string                `json:"qurl_checksums_sha256"`
	QURLInfraRepository string                `json:"qurl_infra_repository"`
	QURLInfraSHA        string                `json:"qurl_infra_sha"`
	APIEndpoint         string                `json:"api_endpoint"`
	Auth0ClientID       string                `json:"auth0_client_id"`
	OwnerSubject        string                `json:"owner_subject"`
	CandidateRunnerCIDR string                `json:"candidate_runner_cidr"`
	Candidate           prodEndpointAuthority `json:"candidate"`
	Canonical           prodEndpointAuthority `json:"canonical"`
}

type prodEndpointAuthority struct {
	ServerEndpoint         string `json:"server_endpoint"`
	ACEndpoint             string `json:"ac_endpoint"`
	RelayEndpoint          string `json:"relay_endpoint"`
	ServerHostname         string `json:"server_hostname"`
	ACHostnameSuffix       string `json:"ac_hostname_suffix"`
	RelayHostname          string `json:"relay_hostname"`
	HubServerPublicKeyB64  string `json:"hub_server_public_key_b64"`
	CellID                 string `json:"cell_id"`
	CellServerPublicKeyB64 string `json:"cell_server_public_key_b64"`
	IssuerKID              string `json:"issuer_kid"`
	IssuerSPKIDERB64       string `json:"issuer_spki_der_b64"`
}

type prodDeploymentAuthority struct {
	Schema        int             `json:"schema"`
	Environment   string          `json:"environment"`
	ReleaseID     string          `json:"release_id"`
	Target        string          `json:"target"`
	APIEndpoint   string          `json:"api_endpoint"`
	Auth0ClientID string          `json:"auth0_client_id"`
	OwnerSubject  string          `json:"owner_subject"`
	Direct        qurl.Deployment `json:"direct"`
	Relay         qurl.Deployment `json:"relay"`
}

type prodLifecycleInputs struct {
	Manifest            prodReleaseManifest
	ReleaseID           string
	Target              string
	Transport           string
	Runtime             string
	Binary              string
	APIKey              string
	CleanupJWT          string
	APIKeyID            string
	SelectedDeployment  qurl.Deployment
	SelectedAuthority   prodEndpointAuthority
	SelectedDeployPath  string
	CandidateDeployPath string
	CanonicalDeployPath string
}

type prodRunNamespace struct {
	AgentID     string
	ConnectorID string
}

// TestProdMatchedCohortCustomerLifecycle is the protected production customer
// journey. The NHP rollout owns ordinary Auth0/API-key creation and revocation;
// this test receives only file-backed credentials and manifest-derived public
// deployment authority. It uses the released qurl binary as two real processes,
// verifies bytes through both siblings, exact-retires A, keeps B available,
// restarts A from the same durable identity/resource, verifies both again, and
// then exact-retires and reclaims both resources and device credentials.
func TestProdMatchedCohortCustomerLifecycle(t *testing.T) {
	if os.Getenv(prodCohortArmingEnv) != "enabled" {
		t.Skipf("SKIPPED LOUDLY: production matched-cohort journey is disarmed: %s != enabled", prodCohortArmingEnv)
	}
	inputs := loadProdLifecycleInputs(t)
	deploymentPath := writeProdSelectedDeployment(t, inputs.SelectedDeployment)

	namespaceA := prodNamespace(t, inputs, "a")
	namespaceB := prodNamespace(t, inputs, "b")
	if namespaceA == namespaceB {
		t.Fatal("production sibling namespaces are not distinct")
	}

	const bodyA = "qurl-production-cohort-sibling-a\n"
	const bodyB = "qurl-production-cohort-sibling-b\n"
	targetA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, bodyA)
	}))
	t.Cleanup(targetA.Close)
	targetB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, bodyB)
	}))
	t.Cleanup(targetB.Close)

	stateDirA := secureProdStateDir(t)
	stateDirB := secureProdStateDir(t)
	processA := startProdPublishProcess(t, inputs, deploymentPath, namespaceA, stateDirA, targetA.URL)
	processA.registerRecoveryCleanup(t, inputs, namespaceA, stateDirA)
	cridA := processA.waitReady(t)
	assertProdDurableIdentity(t, namespaceA, stateDirA)
	processB := startProdPublishProcess(t, inputs, deploymentPath, namespaceB, stateDirB, targetB.URL)
	processB.registerRecoveryCleanup(t, inputs, namespaceB, stateDirB)
	cridB := processB.waitReady(t)
	assertProdDurableIdentity(t, namespaceB, stateDirB)
	if cridA == cridB {
		t.Fatalf("production siblings published the same CRID %q", cridA)
	}

	assertProdGetBytes(t, inputs, deploymentPath, cridA, bodyA)
	assertProdGetBytes(t, inputs, deploymentPath, cridB, bodyB)
	firstARunID := processA.stopAndValidate(t, inputs)
	processB.requireRunning(t, "after sibling A retirement")
	assertProdGetBytes(t, inputs, deploymentPath, cridB, bodyB)

	replacementA := startProdPublishProcess(t, inputs, deploymentPath, namespaceA, stateDirA, targetA.URL)
	replacementCRID := replacementA.waitReady(t)
	if replacementCRID != cridA {
		t.Fatalf("replacement A CRID = %q, want durable resource %q", replacementCRID, cridA)
	}
	assertProdGetBytes(t, inputs, deploymentPath, cridA, bodyA)
	processB.requireRunning(t, "after sibling A replacement")
	assertProdGetBytes(t, inputs, deploymentPath, cridB, bodyB)
	replacementRunID := replacementA.stopAndValidate(t, inputs)
	if replacementRunID == firstARunID {
		t.Fatalf("replacement A reused admitted RunID %q", firstARunID)
	}
	processB.stopAndValidate(t, inputs)

	t.Logf("production customer lifecycle passed: target=%s transport=%s release=%s", inputs.Target, inputs.Transport, inputs.ReleaseID)
}

func loadProdLifecycleInputs(t *testing.T) prodLifecycleInputs {
	t.Helper()
	for _, forbidden := range []string{
		"QURL_CLI_SANDBOX_CLEANUP_JWT", "QURL_CLI_SANDBOX_CLEANUP_JWT_FILE",
		"QURL_CLI_SANDBOX_LOCAL_PUBLISH", "QURL_CLI_SANDBOX_SIBLING_CONTINUITY",
		"QURL_API_KEY", "QURL_CONNECTOR_TOKEN", "QURL_CONNECTOR_TOKEN_FILE",
	} {
		if _, present := os.LookupEnv(forbidden); present {
			t.Fatalf("forbidden inline, sandbox, or enrollment input %s is present", forbidden)
		}
	}
	releaseID := exactProdEnv(t, prodCohortReleaseIDEnv)
	if !prodHex64.MatchString(releaseID) {
		t.Fatalf("%s is not one lowercase SHA-256", prodCohortReleaseIDEnv)
	}
	target := exactProdEnumEnv(t, prodCohortTargetEnv, "candidate", "canonical")
	transport := exactProdEnumEnv(t, prodCohortTransportEnv, "direct", "relay")
	runtimeName := exactProdEnv(t, prodRuntimeEnv)
	if want := "prod-" + target + "-" + transport; runtimeName != want {
		t.Fatalf("%s = %q, want exact %q", prodRuntimeEnv, runtimeName, want)
	}

	manifestPath := exactProdPathEnv(t, prodCohortManifestEnv)
	manifestRaw := readProdAuthorityFile(t, manifestPath, false, prodAuthorityMaxBytes)
	manifestDigest := sha256.Sum256(manifestRaw)
	if hex.EncodeToString(manifestDigest[:]) != releaseID {
		t.Fatal("release manifest bytes do not match the selected release ID")
	}
	var manifest prodReleaseManifest
	strictProdJSON(t, manifestRaw, &manifest, "release manifest")
	validateProdManifest(t, manifest)

	candidatePath := exactProdPathEnv(t, prodCandidateDeploymentFileEnv)
	canonicalPath := exactProdPathEnv(t, prodCanonicalDeploymentFileEnv)
	if candidatePath == canonicalPath {
		t.Fatal("candidate and canonical deployment authority paths are identical")
	}
	candidate := loadProdDeploymentAuthority(t, candidatePath, "candidate", releaseID, manifest)
	canonical := loadProdDeploymentAuthority(t, canonicalPath, "canonical", releaseID, manifest)
	selected := candidate
	selectedPath := candidatePath
	selectedEndpoint := manifest.Customer.Candidate
	if target == "canonical" {
		selected = canonical
		selectedPath = canonicalPath
		selectedEndpoint = manifest.Customer.Canonical
	}
	selectedDeployment := selected.Direct
	if transport == "relay" {
		selectedDeployment = selected.Relay
	}

	binary := validateProdCLIBinary(t, exactProdPathEnv(t, prodCohortBinaryEnv))
	binaryBytes := readProdAuthorityFile(t, binary, false, 256<<20)
	binaryDigest := sha256.Sum256(binaryBytes)
	if hex.EncodeToString(binaryDigest[:]) != manifest.Customer.QURLBinarySHA256 {
		t.Fatal("released qurl binary digest differs from the signed release manifest")
	}
	apiKey := readProdSecretFile(t, prodAPIKeyFileEnv)
	cleanupJWT := readProdSecretFile(t, prodCleanupJWTFileEnv)
	apiKeyID := readProdSecretFile(t, prodAPIKeyIDFileEnv)

	return prodLifecycleInputs{
		Manifest: manifest, ReleaseID: releaseID, Target: target, Transport: transport, Runtime: runtimeName,
		Binary: binary, APIKey: apiKey, CleanupJWT: cleanupJWT, APIKeyID: apiKeyID,
		SelectedDeployment: selectedDeployment, SelectedAuthority: selectedEndpoint, SelectedDeployPath: selectedPath,
		CandidateDeployPath: candidatePath, CanonicalDeployPath: canonicalPath,
	}
}

func validateProdManifest(t *testing.T, manifest prodReleaseManifest) { //nolint:gocritic // The validator owns one immutable decoded manifest value.
	t.Helper()
	if err := validateProdManifestValue(manifest); err != nil {
		t.Fatal(err)
	}
}

func validateProdManifestValue(manifest prodReleaseManifest) error { //nolint:gocritic // Tests deliberately mutate independent manifest values.
	if manifest.Schema != 1 || manifest.Environment != "prod" {
		return errors.New("release manifest is not exact production schema 1")
	}
	if manifest.Source.Repository != prodExpectedSourceRepository || !prodHex40.MatchString(manifest.Source.SHA) ||
		!prodHex40.MatchString(manifest.Source.Tree) || !manifest.Source.SignatureVerified {
		return errors.New("release manifest source authority is malformed")
	}
	for label, raw := range map[string]json.RawMessage{
		"contract": manifest.Contract, "components": manifest.Components, "manifest_attestation": manifest.ManifestAttestation,
	} {
		if !rawJSONObject(raw) {
			return fmt.Errorf("release manifest %s is not one nonempty object", label)
		}
	}
	customer := manifest.Customer
	if customer.QURLRepository != prodExpectedQURLRepository || customer.QURLInfraRepository != prodExpectedInfraRepository ||
		!prodReleaseTag.MatchString(customer.QURLReleaseTag) || !prodHex40.MatchString(customer.QURLSourceSHA) ||
		!prodHex40.MatchString(customer.QURLInfraSHA) || !prodHex64.MatchString(customer.QURLArchiveSHA256) ||
		!prodHex64.MatchString(customer.QURLBinarySHA256) || !prodHex64.MatchString(customer.QURLChecksumsSHA256) {
		return errors.New("release manifest qurl release authority is malformed")
	}
	wantAsset := "qurl_" + strings.TrimPrefix(customer.QURLReleaseTag, "v") + "_linux_amd64.tar.gz"
	if customer.QURLReleaseAsset != wantAsset {
		return fmt.Errorf("release asset = %q, want %q", customer.QURLReleaseAsset, wantAsset)
	}
	if customer.APIEndpoint != prodExpectedAPIEndpoint || customer.Auth0ClientID == "" ||
		customer.OwnerSubject != customer.Auth0ClientID+"@clients" {
		return errors.New("release manifest customer owner/API authority is malformed")
	}
	runnerPrefix, err := netip.ParsePrefix(customer.CandidateRunnerCIDR)
	if err != nil || !runnerPrefix.Addr().Is4() || runnerPrefix.Bits() != 32 || runnerPrefix.String() != customer.CandidateRunnerCIDR {
		return errors.New("release manifest candidate runner CIDR is not one canonical IPv4 /32")
	}
	if err := validateProdEndpointAuthorityValue("candidate", customer.Candidate); err != nil {
		return err
	}
	if err := validateProdEndpointAuthorityValue("canonical", customer.Canonical); err != nil {
		return err
	}
	if reflect.DeepEqual(customer.Candidate, customer.Canonical) {
		return errors.New("candidate and canonical endpoint authorities are identical")
	}
	for label, pair := range map[string][2]string{
		"hub key":     {customer.Candidate.HubServerPublicKeyB64, customer.Canonical.HubServerPublicKeyB64},
		"cell id":     {customer.Candidate.CellID, customer.Canonical.CellID},
		"cell key":    {customer.Candidate.CellServerPublicKeyB64, customer.Canonical.CellServerPublicKeyB64},
		"issuer kid":  {customer.Candidate.IssuerKID, customer.Canonical.IssuerKID},
		"issuer spki": {customer.Candidate.IssuerSPKIDERB64, customer.Canonical.IssuerSPKIDERB64},
	} {
		if pair[0] != pair[1] {
			return fmt.Errorf("candidate and canonical %s differ", label)
		}
	}
	return nil
}

func validateProdEndpointAuthorityValue(label string, endpoint prodEndpointAuthority) error { //nolint:gocritic // The closed endpoint value is small enough for one validation pass.
	for name, value := range map[string]string{
		"server_endpoint": endpoint.ServerEndpoint, "ac_endpoint": endpoint.ACEndpoint,
		"relay_endpoint": endpoint.RelayEndpoint, "server_hostname": endpoint.ServerHostname,
		"ac_hostname_suffix": endpoint.ACHostnameSuffix, "relay_hostname": endpoint.RelayHostname,
	} {
		if value == "" || value != strings.TrimSpace(value) || !prodCanonicalDNS.MatchString(value) || strings.Contains(value, "..") {
			return fmt.Errorf("%s deployment %s is not canonical DNS", label, name)
		}
	}
	if endpoint.CellID != "cell0" || endpoint.ServerHostname != "cell0.nhp.layerv.ai" ||
		endpoint.ACHostnameSuffix != "nhp.layerv.ai" || endpoint.RelayHostname != "relay.layerv.ai" {
		return fmt.Errorf("%s deployment does not bind the exact production identity", label)
	}
	for name, value := range map[string]string{"hub key": endpoint.HubServerPublicKeyB64, "cell key": endpoint.CellServerPublicKeyB64} {
		raw, err := base64.StdEncoding.Strict().DecodeString(value)
		if err != nil || len(raw) != 32 || base64.StdEncoding.EncodeToString(raw) != value {
			return fmt.Errorf("%s deployment %s is not one canonical 32-byte key", label, name)
		}
	}
	if !prodCanonicalKeyID.MatchString(endpoint.IssuerKID) || !prodCanonicalSPKIB64.MatchString(endpoint.IssuerSPKIDERB64) {
		return fmt.Errorf("%s deployment issuer authority is malformed", label)
	}
	return nil
}

//nolint:gocritic // The loader validates one immutable manifest snapshot.
func loadProdDeploymentAuthority(
	t *testing.T,
	path string,
	target string,
	releaseID string,
	manifest prodReleaseManifest,
) prodDeploymentAuthority {
	t.Helper()
	raw := readProdAuthorityFile(t, path, true, 64<<10)
	var authority prodDeploymentAuthority
	strictProdJSON(t, raw, &authority, target+" deployment authority")
	if err := validateProdDeploymentValue(authority, target, releaseID, manifest); err != nil {
		t.Fatal(err)
	}
	return authority
}

func validateProdDeploymentValue(authority prodDeploymentAuthority, target, releaseID string, manifest prodReleaseManifest) error { //nolint:gocritic // Mutation tests require independent closed value copies.
	if authority.Schema != 1 || authority.Environment != "prod" || authority.ReleaseID != releaseID || authority.Target != target {
		return fmt.Errorf("%s deployment authority identity is malformed", target)
	}
	if authority.APIEndpoint != manifest.Customer.APIEndpoint || authority.Auth0ClientID != manifest.Customer.Auth0ClientID ||
		authority.OwnerSubject != manifest.Customer.OwnerSubject {
		return fmt.Errorf("%s deployment customer authority differs from the release manifest", target)
	}
	endpoint := manifest.Customer.Candidate
	if target == "canonical" {
		endpoint = manifest.Customer.Canonical
	}
	wantDirect, wantRelay := expectedProdDeployments(endpoint)
	if !reflect.DeepEqual(authority.Direct, wantDirect) {
		return fmt.Errorf("%s direct deployment differs from the signed manifest projection", target)
	}
	if !reflect.DeepEqual(authority.Relay, wantRelay) {
		return fmt.Errorf("%s relay deployment differs from the signed manifest projection", target)
	}
	return nil
}

func expectedProdDeployments(endpoint prodEndpointAuthority) (qurl.Deployment, qurl.Deployment) { //nolint:gocritic // Projection takes one immutable endpoint value.
	issuer := qurl.ManifestIssuer{Kid: endpoint.IssuerKID, SPKIDERB64: endpoint.IssuerSPKIDERB64}
	hubAuthority := &qurl.HubBootstrap{
		Host: endpoint.ServerEndpoint, Port: prodPublicNHPPort, ServerPublicKeyB64: endpoint.HubServerPublicKeyB64,
	}
	direct := qurl.Deployment{
		Issuers: []qurl.ManifestIssuer{issuer},
		Cells: []qurl.DeploymentCell{{
			CellID: endpoint.CellID, Host: endpoint.ACEndpoint, Port: prodPublicNHPPort,
			ServerPublicKeyB64: endpoint.CellServerPublicKeyB64,
		}},
		RelayAllowlist: []string{},
		Hub:            hubAuthority,
	}
	relayHub := *hubAuthority
	relay := qurl.Deployment{
		Issuers:        []qurl.ManifestIssuer{issuer},
		Cells:          []qurl.DeploymentCell{},
		RelayAllowlist: []string{endpoint.RelayHostname},
		Hub:            &relayHub,
	}
	return direct, relay
}

func strictProdJSON(t *testing.T, raw []byte, destination any, label string) {
	t.Helper()
	if err := decodeStrictProdJSON(raw, destination); err != nil {
		t.Fatalf("decode %s: %v", label, err)
	}
}

func decodeStrictProdJSON(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}

func rawJSONObject(raw json.RawMessage) bool {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return false
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil || len(value) == 0 {
		return false
	}
	return true
}

func exactProdEnv(t *testing.T, name string) string {
	t.Helper()
	value, present := os.LookupEnv(name)
	if !present || value == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\x00\r\n") {
		t.Fatalf("%s is missing or malformed", name)
	}
	return value
}

func exactProdEnumEnv(t *testing.T, name string, values ...string) string {
	t.Helper()
	value := exactProdEnv(t, name)
	for _, allowed := range values {
		if value == allowed {
			return value
		}
	}
	t.Fatalf("%s = %q, want one of %v", name, value, values)
	return ""
}

func exactProdPathEnv(t *testing.T, name string) string {
	t.Helper()
	path := exactProdEnv(t, name)
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		t.Fatalf("%s must be one exact absolute path", name)
	}
	return path
}

func readProdAuthorityFile(t *testing.T, path string, requirePrivate bool, maxBytes int64) []byte {
	t.Helper()
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("read authority file metadata: %v", err)
	}
	validateProdFileInfo(t, before, requirePrivate)
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatalf("open authority file: %v", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		t.Fatal("open authority file returned no file")
	}
	defer func() { _ = file.Close() }()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("authority file identity changed while opening")
	}
	validateProdFileInfo(t, after, requirePrivate)
	raw, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		t.Fatalf("read authority file: %v", err)
	}
	if len(raw) == 0 || int64(len(raw)) > maxBytes {
		t.Fatal("authority file is empty or exceeds its bound")
	}
	final, err := file.Stat()
	if err != nil || !os.SameFile(after, final) || final.Size() != after.Size() || !final.ModTime().Equal(after.ModTime()) {
		t.Fatal("authority file changed while reading")
	}
	validateProdFileInfo(t, final, requirePrivate)
	return raw
}

func validateProdFileInfo(t *testing.T, info os.FileInfo, requirePrivate bool) {
	t.Helper()
	if info == nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		t.Fatal("authority file must be one non-writable regular file")
	}
	if requirePrivate && info.Mode().Perm() != 0o600 && info.Mode().Perm() != 0o440 {
		t.Fatal("private authority file must have mode 0600 or 0440")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 || (stat.Uid != uint32(os.Geteuid()) && stat.Uid != 0) {
		t.Fatal("authority file must have one trusted owner and link")
	}
}

func readProdSecretFile(t *testing.T, envName string) string {
	t.Helper()
	path := exactProdPathEnv(t, envName)
	raw := readProdAuthorityFile(t, path, true, prodSecretMaxBytes)
	value := string(raw)
	if value == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, "\r\n") {
		t.Fatalf("%s must contain one exact nonempty line", envName)
	}
	return value
}

func validateProdCLIBinary(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal("released qurl binary is unavailable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("released qurl binary metadata is unavailable")
	}
	if err := validateProdBinaryMetadata(info.Mode(), stat.Uid, uint32(os.Geteuid()), uint64(stat.Nlink)); err != nil {
		t.Fatal(err)
	}
	return path
}

func validateProdBinaryMetadata(mode os.FileMode, ownerUID, effectiveUID uint32, links uint64) error {
	if !mode.IsRegular() || mode.Perm()&0o111 == 0 || mode.Perm()&0o022 != 0 || links != 1 {
		return errors.New("released qurl binary must be one non-writable executable regular file")
	}
	if ownerUID != effectiveUID && ownerUID != 0 {
		return errors.New("released qurl binary must be owned by the current user or root")
	}
	return nil
}

func writeProdSelectedDeployment(t *testing.T, deployment qurl.Deployment) string { //nolint:gocritic // The writer owns one immutable deployment projection.
	t.Helper()
	raw, err := json.Marshal(deployment)
	if err != nil {
		t.Fatalf("encode selected qurl deployment: %v", err)
	}
	raw = append(raw, '\n')
	path := filepath.Join(secureProdStateDir(t), "deployment.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write selected qurl deployment: %v", err)
	}
	if _, err := qurl.LoadDeployment(path); err != nil {
		t.Fatalf("load selected qurl deployment through the released SDK contract: %v", err)
	}
	return path
}

func secureProdStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	//nolint:gosec // Production journey state must be private, so 0700 is intentional.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("secure production journey state directory: %v", err)
	}
	return dir
}

func prodNamespace(t *testing.T, inputs prodLifecycleInputs, label string) prodRunNamespace { //nolint:gocritic // Namespace derivation owns one immutable protected-run snapshot.
	t.Helper()
	runID := exactProdEnv(t, prodRunIDEnv)
	attempt := exactProdEnv(t, prodRunAttemptEnv)
	for name, value := range map[string]string{prodRunIDEnv: runID, prodRunAttemptEnv: attempt} {
		if !prodPositiveDecimal.MatchString(value) {
			t.Fatalf("%s is not a canonical positive decimal", name)
		}
		if _, err := strconv.ParseUint(value, 10, 64); err != nil {
			t.Fatalf("%s exceeds uint64", name)
		}
	}
	if label != "a" && label != "b" {
		t.Fatalf("unsupported production journey label %q", label)
	}
	targetCode := string(inputs.Target[0])
	transportCode := string(inputs.Transport[0])
	agentID := fmt.Sprintf("qurl-prod-r%s-a%s-%s%s%s", runID, attempt, targetCode, transportCode, label)
	if len(agentID) > 64 {
		t.Fatal("production journey agent identity exceeds the platform bound")
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{
		"qurl-prod-matched-cohort-v1", inputs.ReleaseID, runID, attempt, inputs.Target, inputs.Transport, label,
	}, "\x00")))
	connectorID := "connector-prod-cohort-" + hex.EncodeToString(digest[:12])
	return prodRunNamespace{AgentID: agentID, ConnectorID: connectorID}
}

type lockedProdBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedProdBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedProdBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type prodPublishProcess struct {
	label  string
	cmd    *exec.Cmd
	stdout lockedProdBuffer
	stderr lockedProdBuffer
	done   chan struct{}

	waitMu  sync.Mutex
	waitErr error
	stopped bool
}

//nolint:gocritic // The subprocess and its cleanup callbacks own an immutable protected-run snapshot.
func startProdPublishProcess(
	t *testing.T,
	inputs prodLifecycleInputs,
	deploymentPath string,
	namespace prodRunNamespace,
	stateDir string,
	targetURL string,
) *prodPublishProcess {
	t.Helper()
	if namespace.AgentID == "" || namespace.ConnectorID == "" {
		t.Fatal("production publish process received an empty namespace")
	}
	env := prodCommandEnv(inputs, deploymentPath, namespace.AgentID, stateDir)
	p := &prodPublishProcess{label: namespace.ConnectorID, done: make(chan struct{})}
	//nolint:gosec // The protected journey validates the exact binary and supplies only closed arguments.
	p.cmd = exec.CommandContext(
		context.Background(), inputs.Binary, "--endpoint", inputs.Manifest.Customer.APIEndpoint,
		"--quiet", "publish", targetURL, "--id", namespace.ConnectorID, "--refresh-mode", "disabled",
	)
	p.cmd.Env = env
	p.cmd.Stdout = &p.stdout
	p.cmd.Stderr = &p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start production publish %s: %v", p.label, err)
	}
	go func() {
		err := p.cmd.Wait()
		p.waitMu.Lock()
		p.waitErr = err
		p.waitMu.Unlock()
		close(p.done)
	}()
	// This local cleanup is intentionally registered at Start, before the
	// process can enroll or publish. Remote cleanup is registered by the caller
	// immediately afterward and recovers any durable state created before ready.
	t.Cleanup(func() { p.forceStop(t) })
	return p
}

func prodCommandEnv(inputs prodLifecycleInputs, deploymentPath, agentID, stateDir string) []string { //nolint:gocritic // Environment projection owns one immutable protected-run snapshot.
	values := map[string]string{
		"LANG":                   "C.UTF-8",
		"LC_ALL":                 "C.UTF-8",
		"NO_COLOR":               "1",
		"QURL_API_KEY":           inputs.APIKey,
		"QURL_DEPLOYMENT":        deploymentPath,
		"QURL_ENDPOINT":          inputs.Manifest.Customer.APIEndpoint,
		state.EnvStateDirPrimary: stateDir,
		state.EnvAgentID:         agentID,
		hub.EnvHost:              inputs.SelectedAuthority.ServerEndpoint,
		hub.EnvPort:              strconv.Itoa(prodPublicNHPPort),
		hub.EnvServerPublicKey:   inputs.SelectedAuthority.HubServerPublicKeyB64,
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env
}

func (p *prodPublishProcess) waitReady(t *testing.T) string {
	t.Helper()
	crid, err := p.waitReadyResult(prodProcessTimeout)
	if err != nil {
		t.Fatalf("production publish %s readiness: %v\nstderr: %s", p.label, err, p.stderr.String())
	}
	return crid
}

func (p *prodPublishProcess) waitReadyResult(timeout time.Duration) (string, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		stdoutRaw := p.stdout.String()
		stderr := p.stderr.String()
		if strings.Contains(stderr, "event=proxy_ready") && strings.HasSuffix(stdoutRaw, "\n") {
			stdout := strings.TrimSpace(stdoutRaw)
			assessment, err := cridux.Assess(stdout)
			if err != nil || assessment.Kind != cridux.KindCRID {
				return "", fmt.Errorf("stdout = %q, want exactly one CRID: %w", stdout, err)
			}
			admitted, err := prodEventRunID(stderr, "login_success")
			if err != nil {
				return "", fmt.Errorf("admitted RunID: %w", err)
			}
			ready, err := prodEventRunID(stderr, "proxy_ready")
			if err != nil {
				return "", fmt.Errorf("ready RunID: %w", err)
			}
			if admitted != ready {
				return "", fmt.Errorf("ready RunID %q differs from admitted RunID %q", ready, admitted)
			}
			return stdout, nil
		}
		select {
		case <-p.done:
			p.waitMu.Lock()
			waitErr := p.waitErr
			p.waitMu.Unlock()
			return "", fmt.Errorf("process exited before readiness: %w", waitErr)
		case <-deadline.C:
			return "", fmt.Errorf("process did not become ready within %s", timeout)
		case <-ticker.C:
		}
	}
}

func (p *prodPublishProcess) requireRunning(t *testing.T, phase string) {
	t.Helper()
	select {
	case <-p.done:
		p.waitMu.Lock()
		waitErr := p.waitErr
		p.waitMu.Unlock()
		t.Fatalf("production publish %s exited %s: %v\nstderr: %s", p.label, phase, waitErr, p.stderr.String())
	default:
	}
}

func (p *prodPublishProcess) stopAndValidate(t *testing.T, inputs prodLifecycleInputs) string { //nolint:gocritic // Validation owns one immutable protected-run snapshot.
	t.Helper()
	if p.stopped {
		t.Fatalf("production publish %s was stopped twice", p.label)
	}
	p.stopped = true
	p.requireRunning(t, "before requested exact retirement")
	if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("interrupt production publish %s: %v", p.label, err)
	}
	select {
	case <-p.done:
	case <-time.After(prodProcessTimeout):
		t.Fatalf("production publish %s did not stop within %s", p.label, prodProcessTimeout)
	}
	p.waitMu.Lock()
	waitErr := p.waitErr
	p.waitMu.Unlock()
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 130 {
		t.Fatalf("production publish %s exit = %v, want 130 after interrupt\nstderr: %s", p.label, waitErr, p.stderr.String())
	}
	stderr := p.stderr.String()
	if err := validateProdRetirementLog(stderr); err != nil {
		t.Fatalf("production publish %s exact retirement: %v\nstderr: %s", p.label, err, stderr)
	}
	assertProdNoSecret(t, p.stdout.String()+stderr, inputs.APIKey, inputs.CleanupJWT, inputs.APIKeyID)
	runID, _ := prodEventRunID(stderr, "login_success")
	return runID
}

func (p *prodPublishProcess) forceStop(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Signal(os.Interrupt)
	select {
	case <-p.done:
		return
	case <-time.After(5 * time.Second):
	}
	_ = p.cmd.Process.Kill()
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Errorf("production publish %s could not be reaped", p.label)
	}
}

func validateProdRetirementLog(stderr string) error {
	for _, failure := range []string{"session_retirement_failed", "nhp_session_exit_failed"} {
		if strings.Contains(stderr, failure) {
			return fmt.Errorf("process reported %s", failure)
		}
	}
	admitted, err := prodEventRunID(stderr, "login_success")
	if err != nil {
		return fmt.Errorf("admitted RunID: %w", err)
	}
	ready, err := prodEventRunID(stderr, "proxy_ready")
	if err != nil {
		return fmt.Errorf("ready RunID: %w", err)
	}
	retired, err := prodEventRunID(stderr, "nhp_session_retired")
	if err != nil {
		return fmt.Errorf("retired RunID: %w", err)
	}
	if admitted != ready || admitted != retired {
		return fmt.Errorf("ready RunID %q and retired RunID %q differ from admitted RunID %q", ready, retired, admitted)
	}
	return nil
}

func prodEventRunID(logText, event string) (string, error) {
	if event == "" || strings.ContainsAny(event, " \t\r\n=") {
		return "", errors.New("lifecycle event name is invalid")
	}
	runID := ""
	count := 0
	for _, line := range strings.Split(logText, "\n") {
		fields := strings.Fields(line)
		hasEvent := false
		for _, field := range fields {
			if field == "event="+event {
				hasEvent = true
				break
			}
		}
		if !hasEvent {
			continue
		}
		for _, field := range fields {
			if strings.HasPrefix(field, "run_id=") {
				runID = strings.Trim(strings.TrimPrefix(field, "run_id="), `"`)
				count++
				break
			}
		}
	}
	if count != 1 || runID == "" {
		return "", fmt.Errorf("lifecycle event has %d cycle RunIDs, want one", count)
	}
	if err := qurl.ValidateCycleRunID(runID); err != nil {
		return "", errors.New("lifecycle event has a noncanonical cycle RunID")
	}
	return runID, nil
}

func assertProdGetBytes(t *testing.T, inputs prodLifecycleInputs, deploymentPath, crid, want string) { //nolint:gocritic // The subprocess owns one immutable protected-run snapshot.
	t.Helper()
	destination := filepath.Join(t.TempDir(), "download")
	ctx, cancel := context.WithTimeout(context.Background(), prodProcessTimeout)
	defer cancel()
	//nolint:gosec // The binary and CRID are validated protected-runner authority.
	cmd := exec.CommandContext(
		ctx, inputs.Binary, "--endpoint", inputs.Manifest.Customer.APIEndpoint,
		"--quiet", "get", crid, "--file", destination,
	)
	cmd.Env = prodCommandEnv(inputs, deploymentPath, "qurl-prod-get", secureProdStateDir(t))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("get %s failed: %v\nstdout: %s\nstderr: %s", crid, err, stdout.String(), stderr.String())
	}
	assertProdNoSecret(t, stdout.String()+stderr.String(), inputs.APIKey, inputs.CleanupJWT, inputs.APIKeyID)
	got, err := os.ReadFile(destination) //nolint:gosec // Exact test TempDir child.
	if err != nil {
		t.Fatalf("read downloaded bytes for %s: %v", crid, err)
	}
	if !bytes.Equal(got, []byte(want)) {
		t.Fatalf("downloaded bytes for %s differ: got %d bytes, want %d", crid, len(got), len(want))
	}
}

func assertProdNoSecret(t *testing.T, output string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && strings.Contains(output, secret) {
			t.Fatal("production customer command exposed a protected credential")
		}
	}
}

type prodCleanupOps struct {
	loadState      func(string) (*qurl.AgentState, error)
	deleteResource func(context.Context, string, string, string) error
	revokeDevice   func(context.Context, string, string, string) error
}

//nolint:gocritic // Cleanup callbacks own one immutable protected-run snapshot.
func (p *prodPublishProcess) registerRecoveryCleanup(
	t *testing.T,
	inputs prodLifecycleInputs,
	namespace prodRunNamespace,
	stateDir string,
) {
	t.Helper()
	// Register immediately after Start. Cleanup first stops the process so a
	// delayed admission cannot race deletion, then recovers any state persisted
	// before readiness, deletes the resource, and only then revokes the device.
	t.Cleanup(func() {
		p.forceStop(t)
		ctx, cancel := context.WithTimeout(context.Background(), prodCleanupTimeout)
		defer cancel()
		err := cleanupProdAuthority(ctx, inputs.Manifest.Customer.APIEndpoint, inputs.CleanupJWT, namespace, stateDir, prodCleanupOps{
			loadState:      loadProdAgentState,
			deleteResource: deleteProdResource,
			revokeDevice:   revokeProdDeviceCredential,
		})
		if err != nil {
			t.Error(err)
		}
	})
}

func cleanupProdAuthority(
	ctx context.Context,
	endpoint string,
	cleanupJWT string,
	namespace prodRunNamespace,
	stateDir string,
	ops prodCleanupOps,
) error {
	loaded, err := ops.loadState(stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("load production customer state for recovery cleanup failed")
	}
	if loaded == nil || loaded.AgentID != namespace.AgentID || loaded.DeviceAPIKey == "" || loaded.DeviceAPIKeyID == "" {
		return errors.New("production cleanup durable identity differs from its run namespace")
	}
	if err := ops.deleteResource(ctx, endpoint, namespace.ConnectorID, loaded.DeviceAPIKey); err != nil {
		// Keep the device credential usable when resource cleanup is uncertain;
		// the serialized protected lane can recover it on its next run.
		return fmt.Errorf("production customer resource cleanup: %w", err)
	}
	if err := ops.revokeDevice(ctx, endpoint, cleanupJWT, loaded.DeviceAPIKeyID); err != nil {
		return fmt.Errorf("production customer device cleanup: %w", err)
	}
	return nil
}

func loadProdAgentState(stateDir string) (*qurl.AgentState, error) {
	path := filepath.Join(stateDir, state.AgentStateFile)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("production customer state is not one private regular file")
	}
	store, err := qurl.OpenFileAgentState(path)
	if err != nil {
		return nil, err
	}
	loaded, loadErr := store.LoadAgentState(context.Background())
	closeErr := store.Close()
	if loadErr != nil {
		return nil, loadErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if loaded == nil {
		return nil, errors.New("production customer state is empty")
	}
	return loaded, nil
}

func assertProdDurableIdentity(t *testing.T, namespace prodRunNamespace, stateDir string) {
	t.Helper()
	loaded, err := loadProdAgentState(stateDir)
	if err != nil {
		t.Fatalf("load production durable identity: %v", err)
	}
	if loaded.AgentID != namespace.AgentID || loaded.DeviceAPIKeyID == "" || loaded.DeviceAPIKey == "" {
		t.Fatalf("production durable identity differs from namespace %s", namespace.AgentID)
	}
}

func deleteProdResource(ctx context.Context, endpoint, connectorID, deviceAPIKey string) error {
	origin, err := connectoragent.ResourceSDKOrigin(endpoint)
	if err != nil {
		return errors.New("derive production resource API origin failed")
	}
	client, err := qurl.NewClient(qurl.BearerToken(deviceAPIKey), qurl.WithBaseURL(origin))
	if err != nil {
		return errors.New("open production device resource client failed")
	}
	resource, err := client.GetConnectorResourceBySlug(ctx, connectorID)
	if errors.Is(err, qurl.ErrConnectorResourceNotFound) {
		return nil
	}
	if err != nil || resource == nil {
		return errors.New("find production Connector resource failed")
	}
	if err := client.DeleteConnectorResource(ctx, resource.ResourceID); err != nil && !errors.Is(err, qurl.ErrConnectorResourceNotFound) {
		return errors.New("delete production Connector resource failed")
	}
	return nil
}

var prodCleanupHTTPClient = &http.Client{
	Timeout: prodCleanupTimeout,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func revokeProdDeviceCredential(ctx context.Context, endpoint, cleanupJWT, deviceKeyID string) error {
	requestURL := strings.TrimRight(endpoint, "/") + "/v1/api-keys/" + url.PathEscape(deviceKeyID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, requestURL, http.NoBody)
	if err != nil {
		return errors.New("build production device cleanup request failed")
	}
	req.Header.Set("Authorization", "Bearer "+cleanupJWT)
	resp, err := prodCleanupHTTPClient.Do(req)
	if err != nil {
		return errors.New("production device cleanup request failed")
	}
	_, copyErr := io.Copy(io.Discard, resp.Body)
	closeErr := resp.Body.Close()
	if copyErr != nil || closeErr != nil {
		return errors.New("consume production device cleanup response failed")
	}
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("production device cleanup status = %d, want 204", resp.StatusCode)
	}
	return nil
}

// TestProdMatchedCohortSourceContract is run by the protected NHP rollout
// before any credential is minted or gate is changed. It prevents selecting a
// qurl release that contains this journey but lacks the exact-session consumer
// and its executable duplicate-receipt retry test. The live journey never
// claims to send a duplicate successful receipt because the public binary has
// no truthful operator surface for that action.
func TestProdMatchedCohortSourceContract(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate production cohort source contract")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	required := map[string][]string{
		filepath.Join(repoRoot, "apps", "cli", "cmd", "prod_matched_cohort_lifecycle_test.go"): {
			"func " + prodCustomerSourceContractTest + "(",
			"func " + prodCustomerLifecycleTest + "(",
			"func " + prodClosureTest + "(",
			prodCandidateDeploymentFileEnv,
			prodCanonicalDeploymentFileEnv,
		},
		filepath.Join(repoRoot, "apps", "cli", "cmd", "prod_matched_cohort_closure_test.go"): {
			"func runProdMatchedCohortAdmissionClosed(",
			prodClosureOperationEnv,
			prodClosureSelectorsEnv,
			"ResourceInventoryUnchanged",
		},
		filepath.Join(repoRoot, "apps", "cli", "internal", "connector", "knock", "native.go"): {
			`"event", "nhp_session_retired"`,
		},
		filepath.Join(repoRoot, "apps", "cli", "internal", "connector", "knock", "native_test.go"): {
			"func " + prodExactRetirementRetryTest + "(",
		},
	}
	for path, needles := range required {
		raw, err := os.ReadFile(path) //nolint:gosec // Exact repository-owned contract paths.
		if err != nil {
			t.Fatalf("read required selected source %s: %v", filepath.Base(path), err)
		}
		for _, needle := range needles {
			if !bytes.Contains(raw, []byte(needle)) {
				t.Fatalf("selected qurl source %s lacks required contract %q", filepath.Base(path), needle)
			}
		}
	}
	probePath := filepath.Join(repoRoot, "apps", "cli", "scripts", "probe-prod-matched-cohort-admission-closed")
	probeInfo, err := os.Lstat(probePath)
	if err != nil || !probeInfo.Mode().IsRegular() || probeInfo.Mode().Perm()&0o111 == 0 || probeInfo.Mode().Perm()&0o022 != 0 {
		t.Fatal("selected qurl source lacks one non-writable executable production closure command")
	}
	probeRaw, err := os.ReadFile(probePath)
	if err != nil || bytes.Contains(probeRaw, []byte("closure operations are not implemented")) {
		t.Fatal("selected qurl source has no cleanup-safe authenticated admission-closure implementation")
	}
	for _, needle := range []string{"--operation", "--intent-file", "--journal-file", "--report-file", "--frps-selectors-json", "ports-only FRPS authority is forbidden"} {
		if !bytes.Contains(probeRaw, []byte(needle)) {
			t.Fatalf("selected qurl closure command lacks required contract %q", needle)
		}
	}
}

// TestProdMatchedCohortAdmissionClosed is the Go side of the protected
// two-phase customer closure command. Raw UDP silence and port reachability are
// deliberately not accepted; the implementation uses precommitted ordinary
// customer identities and authenticated qurl operations.
func TestProdMatchedCohortAdmissionClosed(t *testing.T) {
	if os.Getenv("QURL_PROD_MATCHED_COHORT_ADMISSION_CLOSED") != "enabled" {
		t.Skip("SKIPPED LOUDLY: production admission-closed operation is disarmed")
	}
	runProdMatchedCohortAdmissionClosed(t)
}

func validProdManifestFixture() prodReleaseManifest {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	endpoint := prodEndpointAuthority{
		ServerEndpoint: "candidate-server.nhp.layerv.ai", ACEndpoint: "candidate-ac.nhp.layerv.ai",
		RelayEndpoint: "candidate-relay.layerv.ai", ServerHostname: "cell0.nhp.layerv.ai",
		ACHostnameSuffix: "nhp.layerv.ai", RelayHostname: "relay.layerv.ai",
		HubServerPublicKeyB64: key, CellID: "cell0", CellServerPublicKeyB64: key,
		IssuerKID: "issuer-prod-1", IssuerSPKIDERB64: "AQIDBA",
	}
	canonical := endpoint
	canonical.ServerEndpoint = "cell0.nhp.layerv.ai"
	canonical.ACEndpoint = "access.nhp.layerv.ai"
	canonical.RelayEndpoint = "relay.layerv.ai"
	return prodReleaseManifest{
		Schema: 1, Environment: "prod",
		Source: prodReleaseSource{
			Repository: prodExpectedSourceRepository, SHA: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40), SignatureVerified: true,
		},
		Contract: json.RawMessage(`{"schema":2}`), Components: json.RawMessage(`{"server":{}}`),
		ManifestAttestation: json.RawMessage(`{"workflow":"rollout"}`),
		Customer: prodReleaseCustomer{
			QURLRepository: prodExpectedQURLRepository, QURLReleaseTag: "v1.7.0",
			QURLReleaseAsset: "qurl_1.7.0_linux_amd64.tar.gz", QURLSourceSHA: strings.Repeat("c", 40),
			QURLArchiveSHA256: strings.Repeat("d", 64), QURLBinarySHA256: strings.Repeat("e", 64),
			QURLChecksumsSHA256: strings.Repeat("f", 64), QURLInfraRepository: prodExpectedInfraRepository,
			QURLInfraSHA: strings.Repeat("1", 40), APIEndpoint: prodExpectedAPIEndpoint,
			Auth0ClientID: "prodclient", OwnerSubject: "prodclient@clients", CandidateRunnerCIDR: "203.0.113.7/32",
			Candidate: endpoint, Canonical: canonical,
		},
	}
}

func validProdDeploymentFixture(manifest prodReleaseManifest, target, releaseID string) prodDeploymentAuthority { //nolint:gocritic // Each mutation fixture owns an independent manifest copy.
	endpoint := manifest.Customer.Candidate
	if target == "canonical" {
		endpoint = manifest.Customer.Canonical
	}
	direct, relay := expectedProdDeployments(endpoint)
	return prodDeploymentAuthority{
		Schema: 1, Environment: "prod", ReleaseID: releaseID, Target: target,
		APIEndpoint: manifest.Customer.APIEndpoint, Auth0ClientID: manifest.Customer.Auth0ClientID,
		OwnerSubject: manifest.Customer.OwnerSubject, Direct: direct, Relay: relay,
	}
}

func TestProdMatchedCohortAuthorityContract(t *testing.T) {
	manifest := validProdManifestFixture()
	if err := validateProdManifestValue(manifest); err != nil {
		t.Fatalf("valid manifest: %v", err)
	}
	releaseID := strings.Repeat("9", 64)
	for _, target := range []string{"candidate", "canonical"} {
		authority := validProdDeploymentFixture(manifest, target, releaseID)
		if err := validateProdDeploymentValue(authority, target, releaseID, manifest); err != nil {
			t.Fatalf("valid %s deployment: %v", target, err)
		}
	}

	manifestMutations := map[string]func(*prodReleaseManifest){
		"source repository": func(v *prodReleaseManifest) { v.Source.Repository = "other/nhp" },
		"unsigned source":   func(v *prodReleaseManifest) { v.Source.SignatureVerified = false },
		"qurl repository":   func(v *prodReleaseManifest) { v.Customer.QURLRepository = "other/qurl" },
		"public endpoint":   func(v *prodReleaseManifest) { v.Customer.APIEndpoint = "https://other.example" },
		"owner":             func(v *prodReleaseManifest) { v.Customer.OwnerSubject = "other@clients" },
		"runner cidr":       func(v *prodReleaseManifest) { v.Customer.CandidateRunnerCIDR = "203.0.113.0/24" },
		"candidate trust": func(v *prodReleaseManifest) {
			v.Customer.Candidate.HubServerPublicKeyB64 = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x41}, 32))
		},
		"canonical identity": func(v *prodReleaseManifest) { v.Customer.Canonical.CellID = "cell1" },
	}
	for name, mutate := range manifestMutations {
		t.Run("manifest/"+name, func(t *testing.T) {
			value := validProdManifestFixture()
			mutate(&value)
			if err := validateProdManifestValue(value); err == nil {
				t.Fatal("mutated manifest accepted")
			}
		})
	}

	deploymentMutations := map[string]func(*prodDeploymentAuthority){
		"release":               func(v *prodDeploymentAuthority) { v.ReleaseID = strings.Repeat("8", 64) },
		"target":                func(v *prodDeploymentAuthority) { v.Target = "canonical" },
		"api":                   func(v *prodDeploymentAuthority) { v.APIEndpoint = "https://other.example" },
		"direct cell port":      func(v *prodDeploymentAuthority) { v.Direct.Cells[0].Port = 62206 },
		"direct relay fallback": func(v *prodDeploymentAuthority) { v.Direct.RelayAllowlist = []string{"relay.layerv.ai"} },
		"relay cell fallback":   func(v *prodDeploymentAuthority) { v.Relay.Cells = []qurl.DeploymentCell{v.Direct.Cells[0]} },
		"relay hub":             func(v *prodDeploymentAuthority) { v.Relay.Hub.Host = v.Relay.RelayAllowlist[0] },
		"nil direct relay list": func(v *prodDeploymentAuthority) { v.Direct.RelayAllowlist = nil },
		"nil relay cells":       func(v *prodDeploymentAuthority) { v.Relay.Cells = nil },
	}
	for name, mutate := range deploymentMutations {
		t.Run("deployment/"+name, func(t *testing.T) {
			value := validProdDeploymentFixture(manifest, "candidate", releaseID)
			mutate(&value)
			if err := validateProdDeploymentValue(value, "candidate", releaseID, manifest); err == nil {
				t.Fatal("mutated deployment accepted")
			}
		})
	}

	raw, err := json.Marshal(validProdDeploymentFixture(manifest, "candidate", releaseID))
	if err != nil {
		t.Fatal(err)
	}
	for name, mutated := range map[string][]byte{
		"unknown":  bytes.Replace(raw, []byte(`"schema":1`), []byte(`"schema":1,"extra":true`), 1),
		"trailing": append(append([]byte{}, raw...), []byte(` {}`)...),
	} {
		t.Run("json/"+name, func(t *testing.T) {
			var destination prodDeploymentAuthority
			if err := decodeStrictProdJSON(mutated, &destination); err == nil {
				t.Fatal("malformed closed deployment JSON accepted")
			}
		})
	}
}

func TestProdMatchedCohortNamespaceAndRetirementContract(t *testing.T) {
	inputs := prodLifecycleInputs{ReleaseID: strings.Repeat("1", 64), Target: "candidate", Transport: "direct"}
	t.Setenv(prodRunIDEnv, "32600000000")
	t.Setenv(prodRunAttemptEnv, "1")
	a := prodNamespace(t, inputs, "a")
	b := prodNamespace(t, inputs, "b")
	if a == b || len(a.AgentID) > 64 || !strings.HasPrefix(a.ConnectorID, "connector-prod-cohort-") {
		t.Fatalf("invalid production namespaces: %#v %#v", a, b)
	}
	valid := "event=login_success run_id=01abcdef23456789\n" +
		"event=proxy_ready run_id=01abcdef23456789\n" +
		"event=nhp_session_retired run_id=01abcdef23456789\n"
	if err := validateProdRetirementLog(valid); err != nil {
		t.Fatal(err)
	}
	for name, logText := range map[string]string{
		"missing retirement": strings.ReplaceAll(valid, "event=nhp_session_retired", "event=stopped"),
		"drift":              strings.ReplaceAll(valid, "event=nhp_session_retired run_id=01abcdef23456789", "event=nhp_session_retired run_id=02abcdef23456789"),
		"duplicate":          valid + "event=nhp_session_retired run_id=01abcdef23456789\n",
		"failure":            valid + "event=session_retirement_failed\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateProdRetirementLog(logText); err == nil {
				t.Fatal("invalid exact-retirement log accepted")
			}
		})
	}
}

func TestProdMatchedCohortBinaryPathContract(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "qurl")
	//nolint:gosec // This executable fixture is private inside t.TempDir.
	if err := os.WriteFile(valid, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := validateProdCLIBinary(t, valid); got != valid {
		t.Fatalf("validated binary = %q, want %q", got, valid)
	}
	for name, fixture := range map[string]struct {
		mode  os.FileMode
		owner uint32
		links uint64
		ok    bool
	}{
		"effective-user-owned": {mode: 0o500, owner: 65532, links: 1, ok: true},
		"root-owned":           {mode: 0o555, owner: 0, links: 1, ok: true},
		"non-executable":       {mode: 0o400, owner: 65532, links: 1},
		"writable-root":        {mode: 0o577, owner: 0, links: 1},
		"foreign-owned":        {mode: 0o555, owner: 1000, links: 1},
		"hard-linked":          {mode: 0o555, owner: 0, links: 2},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateProdBinaryMetadata(fixture.mode, fixture.owner, 65532, fixture.links)
			if (err == nil) != fixture.ok {
				t.Fatalf("binary metadata validation = %v, want accepted=%t", err, fixture.ok)
			}
		})
	}
}

func TestProdMatchedCohortProcessAndEnvironmentContract(t *testing.T) {
	inputs := prodLifecycleInputs{
		APIKey:   "customer-secret",
		Manifest: prodReleaseManifest{Customer: prodReleaseCustomer{APIEndpoint: prodExpectedAPIEndpoint}},
		SelectedAuthority: prodEndpointAuthority{
			ServerEndpoint: "cell0.nhp.layerv.ai", HubServerPublicKeyB64: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32)),
		},
	}
	env := prodCommandEnv(inputs, "/authority/deployment.json", "agent-id", "/state")
	joined := strings.Join(env, "\n")
	for _, required := range []string{
		"QURL_API_KEY=customer-secret", "QURL_DEPLOYMENT=/authority/deployment.json",
		"QURL_ENDPOINT=" + prodExpectedAPIEndpoint, state.EnvAgentID + "=agent-id",
		state.EnvStateDirPrimary + "=/state", hub.EnvHost + "=cell0.nhp.layerv.ai",
		hub.EnvPort + "=443",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("released process environment lacks %q", required)
		}
	}
	for _, forbidden := range []string{prodCleanupJWTFileEnv, prodAPIKeyIDFileEnv, "QURL_CLI_SANDBOX", "QURL_CONNECTOR_TOKEN"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("released process environment contains forbidden authority %q", forbidden)
		}
	}

	const crid = "qhtpthw4qt7wkw7khghr6x3z4hsfyn4zbuyhnee4i6bi67yu6yytgvwdbb4q"
	process := &prodPublishProcess{label: "split-crid", done: make(chan struct{})}
	_, _ = process.stderr.Write([]byte("event=login_success run_id=01abcdef23456789\nevent=proxy_ready run_id=01abcdef23456789\n"))
	partial := make(chan struct{})
	go func() {
		_, _ = process.stdout.Write([]byte(crid[:20]))
		close(partial)
		time.Sleep(50 * time.Millisecond)
		_, _ = process.stdout.Write([]byte(crid[20:] + "\n"))
	}()
	<-partial
	got, err := process.waitReadyResult(time.Second)
	if err != nil || got != crid {
		t.Fatalf("split CRID readiness = %q, %v", got, err)
	}

	early := &prodPublishProcess{label: "early-exit", done: make(chan struct{}), waitErr: errors.New("exit 7")}
	close(early.done)
	if _, err := early.waitReadyResult(time.Second); err == nil || !strings.Contains(err.Error(), "exited before readiness") {
		t.Fatalf("early exit result = %v", err)
	}
}

func TestProdMatchedCohortCleanupOrderAndFailure(t *testing.T) {
	namespace := prodRunNamespace{AgentID: "qurl-prod-r1-a1-cda", ConnectorID: "connector-prod-cohort-0123456789abcdef01234567"}
	stateFixture := &qurl.AgentState{AgentID: namespace.AgentID, DeviceAPIKeyID: "device-key-id", DeviceAPIKey: "device-key"}
	order := []string{}
	ops := prodCleanupOps{
		loadState: func(string) (*qurl.AgentState, error) { return stateFixture, nil },
		deleteResource: func(context.Context, string, string, string) error {
			order = append(order, "resource")
			return nil
		},
		revokeDevice: func(context.Context, string, string, string) error {
			order = append(order, "device")
			return nil
		},
	}
	if err := cleanupProdAuthority(context.Background(), prodExpectedAPIEndpoint, "jwt", namespace, "state", ops); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ","); got != "resource,device" {
		t.Fatalf("cleanup order = %q, want resource,device", got)
	}
	deviceCalled := false
	ops.deleteResource = func(context.Context, string, string, string) error { return errors.New("injected") }
	ops.revokeDevice = func(context.Context, string, string, string) error { deviceCalled = true; return nil }
	if err := cleanupProdAuthority(context.Background(), prodExpectedAPIEndpoint, "jwt", namespace, "state", ops); err == nil || deviceCalled {
		t.Fatalf("resource failure cleanup = %v, device called=%t", err, deviceCalled)
	}
}
