//go:build clisandbox

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	qurl "github.com/layervai/qurl-go/qurl"

	connectoragent "github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

const (
	connectorResourceProofArming             = "QURL_CLI_SANDBOX_CONNECTOR_RESOURCE_PROOF"
	connectorResourceProofHubHost            = "hub.sandbox.nhp.layerv.xyz"
	connectorResourceProofSandboxAPIAudience = "https://api.layerv.xyz"
	connectorResourceProofSandboxAPIHost     = "api.layerv.xyz"
	connectorResourceProofAPIHost            = "api.layerv.ai"
	connectorResourceProofBootstrapHost      = "bootstrap.layerv.ai"
	connectorResourceProofAPIURL             = "https://api.layerv.ai/v1"
	connectorResourceProofTimeout            = 60 * time.Second
	connectorResourceProofMinJWTLifetime     = 20 * time.Minute
	connectorResourceProofMaxJWTLifetime     = 2 * time.Hour
)

var errConnectorResourceProofLostResponse = errors.New("proof-only lost Connector resource response")

type connectorResourceProofConfig struct {
	APIKey     string
	Endpoint   string
	CleanupJWT string
}

type connectorResourceProofState struct {
	Version  int                                              `json:"version"`
	Bindings map[string]state.ConnectorResourceBinding        `json:"bindings"`
	Pending  map[string]state.PendingConnectorResourceRequest `json:"pending"`
}

// TestSandboxConnectorResourceNativeProof is the attended customer-CLI proof
// for the deployed connector_resource LST. It deliberately creates one live
// device and resource, then reclaims both through the sanctioned management
// removal APIs registered below.
//
// The first command discards an authenticated cold LRT after the server has
// handled it but before local Commit, modeling the only dangerous uncertainty
// boundary. A real command restart must replay the exact durable nonce and
// receive the frozen found_existing=false result. A second warm restart must
// use a fresh nonce with expected_resource_id and receive true. Both restarts
// run with api.layerv.ai and bootstrap.layerv.ai resolved to a dual-stack TCP
// trap by the governed workflow, so a successful LST -> KNK/ACK -> FRP Login
// proves runtime setup made no HTTPS management or rebootstrap request.
// Finally a valid-but-wrong continuity assertion must get
// the deployed 52503 identity-conflict result, clear pending as terminal, and
// never reach KNK.
func TestSandboxConnectorResourceNativeProof(t *testing.T) {
	if os.Getenv(connectorResourceProofArming) != "enabled" {
		t.Skipf("SKIPPED LOUDLY: Connector resource native proof is disarmed — %s != enabled", connectorResourceProofArming)
	}
	cfg, err := loadConnectorResourceProofConfig(os.Getenv, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	assertConnectorResourceProofHTTPBlackholes(t)

	stateDir := t.TempDir()
	t.Setenv(state.EnvStateDirPrimary, stateDir)
	t.Setenv(state.EnvAgentID, "")
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "sandbox-connector-resource-proof")
	}))
	t.Cleanup(echo.Close)
	connectorID := fmt.Sprintf("connector-resource-proof-%d", time.Now().UnixNano())

	var (
		coldRequest    qurl.NativeConnectorResourceRequest
		coldResolution *qurl.ConnectorResourceResolution
	)
	coldCtx, coldCancel := context.WithTimeout(context.Background(), connectorResourceProofTimeout)
	cold := runCLI(t, &runOpts{
		ctx: coldCtx,
		args: []string{
			"--endpoint", cfg.Endpoint, "--quiet", "publish", echo.URL, "--id", connectorID,
		},
		env:         map[string]string{"QURL_API_KEY": cfg.APIKey},
		syncStreams: true,
		realSleep:   true,
		connectorResolve: func(ctx context.Context, rt *connectoragent.Runtime, id string) (*connectoragent.ResolvedResource, error) {
			resolution, request, resolveErr := resolveConnectorResourceWithoutCommit(ctx, rt, id)
			if resolveErr != nil {
				return nil, resolveErr
			}
			coldRequest = request
			coldResolution = resolution
			return nil, errConnectorResourceProofLostResponse
		},
	})
	coldCancel()
	loaded := loadSandboxAgentState(t, stateDir)
	if loaded != nil {
		// Register device first: Cleanup is LIFO, so the resource is removed
		// while its device credential is still authorized. Register these
		// before any proof assertion can stop the test and leak live state.
		registerSandboxDeviceCredentialCleanup(t, cfg.Endpoint, cfg.CleanupJWT, loaded.DeviceAPIKeyID)
		registerSandboxResourceCleanup(t, cfg.Endpoint, connectorID, loaded.DeviceAPIKey)
	}
	assertSandboxStreamsDoNotContainSecrets(t, cold, cfg.APIKey, cfg.CleanupJWT)
	if cold.code != exitcode.General || !strings.Contains(cold.stderr.String(), errConnectorResourceProofLostResponse.Error()) {
		t.Fatalf("cold lost-response exit = %d, want General with the proof sentinel; stderr: %s", cold.code, cold.stderr.String())
	}
	if coldResolution == nil || coldResolution.Resource == nil || coldResolution.FoundExisting {
		t.Fatalf("cold resolution = %+v, want authenticated found_existing=false resource", coldResolution)
	}
	assertPersistedProofRequest(t, stateDir, connectorID, coldRequest)
	t.Log("PROOF connector_resource cold_create found_existing=false pending_before_dispatch=true")

	var (
		replayRequest    qurl.NativeConnectorResourceRequest
		replayResolution *connectoragent.ResolvedResource
	)
	replay := runConnectorResourceProofServe(t, stateDir, connectorID, func(ctx context.Context, rt *connectoragent.Runtime, id string) (*connectoragent.ResolvedResource, error) {
		resolved, resolveErr := connectoragent.ResolveResourceWithRequestObserver(ctx, rt.Binding, rt.Store, id, func(request qurl.NativeConnectorResourceRequest) error {
			replayRequest = request
			return nil
		})
		replayResolution = resolved
		return resolved, resolveErr
	})
	assertConnectorResourceProofAdmission(t, replay, false)
	if replayRequest != coldRequest {
		t.Fatalf("restart replay request = %+v, want exact cold request %+v", replayRequest, coldRequest)
	}
	if replayResolution == nil || replayResolution.Resource == nil || replayResolution.FoundExisting == nil || *replayResolution.FoundExisting {
		t.Fatalf("exact replay resolution = %+v, want frozen found_existing=false", replayResolution)
	}
	if !sameConnectorResource(replayResolution.Resource, coldResolution.Resource) {
		t.Fatalf("exact replay changed the authenticated binding:\ncold  %+v\nreplay %+v", coldResolution.Resource, replayResolution.Resource)
	}
	assertNoPendingProofRequest(t, stateDir, connectorID)
	t.Log("PROOF connector_resource exact_replay same_nonce=true found_existing=false knk_ack=true login=true runtime_http=zero")

	var warmRequest qurl.NativeConnectorResourceRequest
	warm := runConnectorResourceProofServe(t, stateDir, connectorID, func(ctx context.Context, rt *connectoragent.Runtime, id string) (*connectoragent.ResolvedResource, error) {
		resolved, resolveErr := connectoragent.ResolveResourceWithRequestObserver(ctx, rt.Binding, rt.Store, id, func(request qurl.NativeConnectorResourceRequest) error {
			warmRequest = request
			return nil
		})
		return resolved, resolveErr
	})
	assertConnectorResourceProofAdmission(t, warm, true)
	if warmRequest.RequestNonce == "" || warmRequest.RequestNonce == coldRequest.RequestNonce {
		t.Fatalf("warm nonce = %q, want a fresh nonce distinct from the cold logical request", warmRequest.RequestNonce)
	}
	if warmRequest.ExpectedResourceID != coldResolution.Resource.ResourceID {
		t.Fatalf("warm expected_resource_id = %q, want %q", warmRequest.ExpectedResourceID, coldResolution.Resource.ResourceID)
	}
	assertNoPendingProofRequest(t, stateDir, connectorID)
	t.Log("PROOF connector_resource warm_continuity fresh_nonce=true expected_resource_id=true found_existing=true knk_ack=true login=true runtime_http=zero")

	writeConflictingProofBinding(t, stateDir, connectorID)
	conflictCtx, conflictCancel := context.WithTimeout(context.Background(), connectorResourceProofTimeout)
	conflict := runCLI(t, &runOpts{
		ctx: conflictCtx,
		args: []string{
			"--endpoint", connectorResourceProofAPIURL, "--verbose", "connector", "run",
			"--id", connectorID, "--target", ":" + mustProofURLPort(t, echo.URL), "--state-dir", stateDir,
		},
		env:         map[string]string{},
		syncStreams: true,
		realSleep:   true,
	})
	conflictCancel()
	if conflict.code != exitcode.Conflict || !strings.Contains(conflict.stderr.String(), "52503") {
		t.Fatalf("continuity conflict exit = %d, want Conflict with deployed 52503; stderr: %s", conflict.code, conflict.stderr.String())
	}
	if strings.Contains(conflict.stderr.String(), "event=knock_success") || strings.Contains(conflict.stderr.String(), "login_success") {
		t.Fatalf("terminal resource conflict reached admission:\n%s", conflict.stderr.String())
	}
	assertNoPendingProofRequest(t, stateDir, connectorID)
	t.Log("PROOF connector_resource identity_conflict code=52503 exit=7 pending=cleared knk=false")
}

func loadConnectorResourceProofConfig(lookup func(string) string, now time.Time) (connectorResourceProofConfig, error) {
	cfg := connectorResourceProofConfig{
		APIKey:     strings.TrimSpace(lookup("QURL_API_KEY")),
		Endpoint:   strings.TrimSpace(lookup("QURL_ENDPOINT")),
		CleanupJWT: strings.TrimSpace(lookup("QURL_CLI_SANDBOX_CLEANUP_JWT")),
	}
	missing := []string{}
	for name, value := range map[string]string{
		"QURL_API_KEY":                         cfg.APIKey,
		"QURL_ENDPOINT":                        cfg.Endpoint,
		"QURL_CLI_SANDBOX_ENDPOINT_SHA256":     lookup("QURL_CLI_SANDBOX_ENDPOINT_SHA256"),
		"QURL_CLI_SANDBOX_CLEANUP_JWT":         cfg.CleanupJWT,
		hub.EnvHost:                            lookup(hub.EnvHost),
		hub.EnvPort:                            lookup(hub.EnvPort),
		hub.EnvServerPublicKey:                 lookup(hub.EnvServerPublicKey),
		"QURL_CLI_SANDBOX_ROLLOUT_ATTESTATION": lookup("QURL_CLI_SANDBOX_ROLLOUT_ATTESTATION"),
	} {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return connectorResourceProofConfig{}, fmt.Errorf("Connector resource proof is armed but missing required inputs: %v", missing)
	}
	wantEndpointDigest := strings.TrimSpace(lookup("QURL_CLI_SANDBOX_ENDPOINT_SHA256"))
	digestBytes, digestErr := hex.DecodeString(wantEndpointDigest)
	actualEndpointDigest := sha256.Sum256([]byte(cfg.Endpoint))
	if digestErr != nil || len(digestBytes) != sha256.Size || wantEndpointDigest != hex.EncodeToString(digestBytes) ||
		wantEndpointDigest != hex.EncodeToString(actualEndpointDigest[:]) {
		return connectorResourceProofConfig{}, errors.New("Connector resource proof sandbox endpoint does not match its pinned SHA-256 digest")
	}
	endpointURL, endpointErr := url.Parse(cfg.Endpoint)
	endpointHost := strings.ToLower(endpointURL.Hostname())
	if endpointErr != nil || endpointURL.Scheme != "https" || endpointURL.User != nil || endpointURL.RawQuery != "" || endpointURL.Fragment != "" ||
		endpointURL.Path != "/v1" || (endpointURL.Port() != "" && endpointURL.Port() != "443") ||
		endpointHost != connectorResourceProofSandboxAPIHost {
		return connectorResourceProofConfig{}, errors.New("Connector resource proof endpoint must be the pinned HTTPS sandbox /v1 endpoint under layerv.xyz")
	}
	if got := strings.TrimSpace(lookup("QURL_CLI_SANDBOX_ROLLOUT_ATTESTATION")); got != "five-op-cells-ready" {
		return connectorResourceProofConfig{}, fmt.Errorf("Connector resource proof rollout attestation = %q, want five-op-cells-ready", got)
	}
	if strings.TrimSpace(lookup(hub.EnvHost)) != connectorResourceProofHubHost || strings.TrimSpace(lookup(hub.EnvPort)) != "443" {
		return connectorResourceProofConfig{}, errors.New("Connector resource proof Hub must be the pinned sandbox NHP endpoint on port 443")
	}
	if err := validateConnectorResourceProofCleanupJWT(cfg.CleanupJWT, now); err != nil {
		return connectorResourceProofConfig{}, err
	}
	return cfg, nil
}

func validateConnectorResourceProofCleanupJWT(token string, now time.Time) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return errors.New("Connector resource proof cleanup JWT must be a complete three-part token")
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || base64.RawURLEncoding.EncodeToString(payload) != parts[1] {
		return errors.New("Connector resource proof cleanup JWT payload must be canonical base64url")
	}
	var claims struct {
		ExpiresAt int64           `json:"exp"`
		Audience  json.RawMessage `json:"aud"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	if err := decoder.Decode(&claims); err != nil || claims.ExpiresAt <= 0 {
		return errors.New("Connector resource proof cleanup JWT must carry an integer exp claim")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("Connector resource proof cleanup JWT payload must contain one JSON value")
	}
	if !proofJWTAudienceContains(claims.Audience, connectorResourceProofSandboxAPIAudience) {
		return errors.New("Connector resource proof cleanup JWT must carry the sandbox API audience")
	}
	lifetime := time.Unix(claims.ExpiresAt, 0).Sub(now)
	if lifetime < connectorResourceProofMinJWTLifetime || lifetime > connectorResourceProofMaxJWTLifetime {
		return fmt.Errorf("Connector resource proof cleanup JWT lifetime must be between %s and %s at dispatch", connectorResourceProofMinJWTLifetime, connectorResourceProofMaxJWTLifetime)
	}
	return nil
}

func proofJWTAudienceContains(raw json.RawMessage, want string) bool {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == want
	}
	var many []string
	if json.Unmarshal(raw, &many) != nil {
		return false
	}
	for _, audience := range many {
		if audience == want {
			return true
		}
	}
	return false
}

func assertConnectorResourceProofHTTPBlackholes(t *testing.T) {
	t.Helper()
	for _, host := range []string{connectorResourceProofAPIHost, connectorResourceProofBootstrapHost} {
		addresses, err := net.LookupIP(host)
		if err != nil || len(addresses) == 0 {
			t.Fatalf("resolve %s proof blackhole: %v", host, err)
		}
		for _, address := range addresses {
			if !address.IsLoopback() {
				t.Fatalf("%s proof blackhole resolved to non-loopback %s", host, address)
			}
		}
	}
}

func resolveConnectorResourceWithoutCommit(ctx context.Context, rt *connectoragent.Runtime, connectorID string) (_ *qurl.ConnectorResourceResolution, request qurl.NativeConnectorResourceRequest, retErr error) {
	tx, err := rt.Store.BeginConnectorResource(ctx, connectorID)
	if err != nil {
		return nil, request, err
	}
	defer func() { retErr = errors.Join(retErr, tx.Close()) }()
	request = *tx.Request()
	persisted, ok := readPendingProofRequest(rt.Store.Dir(), connectorID)
	if !ok || persisted != request {
		return nil, request, errors.New("cold Connector resource request was not durably exact before dispatch")
	}
	resolution, err := qurl.ResolveRegisteredAgentConnectorResource(ctx, rt.Binding, &request)
	return resolution, request, err
}

func runConnectorResourceProofServe(t *testing.T, stateDir, connectorID string, resolver func(context.Context, *connectoragent.Runtime, string) (*connectoragent.ResolvedResource, error)) *runResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), connectorResourceProofTimeout)
	defer cancel()
	return runCLI(t, &runOpts{
		ctx: ctx,
		args: []string{
			"--endpoint", connectorResourceProofAPIURL, "--verbose", "connector", "run",
			"--id", connectorID, "--target", ":8080", "--state-dir", stateDir,
		},
		env:              map[string]string{},
		syncStreams:      true,
		realSleep:        true,
		connectorResolve: resolver,
		connectorReady:   cancel,
	})
}

func assertConnectorResourceProofAdmission(t *testing.T, result *runResult, foundExisting bool) {
	t.Helper()
	if result.code != exitcode.Interrupted {
		t.Fatalf("proof serve exit = %d, want Interrupted after readiness; stderr: %s", result.code, result.stderr.String())
	}
	stderr := result.stderr.String()
	for _, evidence := range []string{
		fmt.Sprintf("found_existing=%t", foundExisting),
		"event=connector_resource_resolved",
		"event=knock_success",
		"login_success",
		"Stopped.",
	} {
		if !strings.Contains(stderr, evidence) {
			t.Fatalf("proof serve lacks %q evidence:\n%s", evidence, stderr)
		}
	}
}

func assertPersistedProofRequest(t *testing.T, stateDir, connectorID string, want qurl.NativeConnectorResourceRequest) {
	t.Helper()
	got, ok := readPendingProofRequest(stateDir, connectorID)
	if !ok {
		t.Fatalf("no durable pending request for %q", connectorID)
	}
	if got != want {
		t.Fatalf("durable pending request = %+v, want exact %+v", got, want)
	}
}

func assertNoPendingProofRequest(t *testing.T, stateDir, connectorID string) {
	t.Helper()
	if got, ok := readPendingProofRequest(stateDir, connectorID); ok {
		t.Fatalf("completed or terminal request remains pending for %q: %+v", connectorID, got)
	}
}

func readPendingProofRequest(stateDir, connectorID string) (qurl.NativeConnectorResourceRequest, bool) {
	data, err := os.ReadFile(filepath.Join(stateDir, state.ConnectorResourcesFile))
	if err != nil {
		return qurl.NativeConnectorResourceRequest{}, false
	}
	var envelope connectorResourceProofState
	if json.Unmarshal(data, &envelope) != nil {
		return qurl.NativeConnectorResourceRequest{}, false
	}
	pending, ok := envelope.Pending[connectorID]
	return qurl.NativeConnectorResourceRequest{
		ConnectorID:        pending.ConnectorID,
		RequestNonce:       pending.RequestNonce,
		ExpectedResourceID: pending.ExpectedResourceID,
	}, ok
}

func writeConflictingProofBinding(t *testing.T, stateDir, connectorID string) {
	t.Helper()
	path := filepath.Join(stateDir, state.ConnectorResourcesFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var envelope connectorResourceProofState
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	binding, ok := envelope.Bindings[connectorID]
	if !ok {
		t.Fatalf("no committed binding for %q", connectorID)
	}
	binding.ResourceID = newProofResourceID(t)
	binding.CRID = ""
	envelope.Bindings[connectorID] = binding
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newProofResourceID(t *testing.T) string {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(der)
}

func sameConnectorResource(left, right *qurl.ConnectorResource) bool {
	return left != nil && right != nil &&
		left.Slug == right.Slug &&
		left.ResourceID == right.ResourceID &&
		left.CRID == right.CRID &&
		left.ConnectorRoutingID == right.ConnectorRoutingID &&
		left.KnockResourceID == right.KnockResourceID
}

func mustProofURLPort(t *testing.T, rawURL string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(rawURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestLoadConnectorResourceProofConfigFailsClosed(t *testing.T) {
	endpoint := "https://api.layerv.xyz/v1"
	endpointDigest := sha256.Sum256([]byte(endpoint))
	now := time.Unix(1_800_000_000, 0)
	cleanupJWT := proofJWT(t, now.Add(time.Hour).Unix())
	complete := map[string]string{
		"QURL_API_KEY":                         "lv_test_proof",
		"QURL_ENDPOINT":                        endpoint,
		"QURL_CLI_SANDBOX_ENDPOINT_SHA256":     hex.EncodeToString(endpointDigest[:]),
		"QURL_CLI_SANDBOX_CLEANUP_JWT":         cleanupJWT,
		hub.EnvHost:                            connectorResourceProofHubHost,
		hub.EnvPort:                            "443",
		hub.EnvServerPublicKey:                 "public-key",
		"QURL_CLI_SANDBOX_ROLLOUT_ATTESTATION": "five-op-cells-ready",
	}
	lookup := func(name string) string { return complete[name] }
	if _, err := loadConnectorResourceProofConfig(lookup, now); err != nil {
		t.Fatalf("complete proof config = %v", err)
	}
	for name := range complete {
		t.Run("missing "+name, func(t *testing.T) {
			copy := make(map[string]string, len(complete))
			for key, value := range complete {
				copy[key] = value
			}
			delete(copy, name)
			if _, err := loadConnectorResourceProofConfig(func(key string) string { return copy[key] }, now); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("missing %s error = %v", name, err)
			}
		})
	}
	t.Run("wrong rollout", func(t *testing.T) {
		copy := make(map[string]string, len(complete))
		for key, value := range complete {
			copy[key] = value
		}
		copy["QURL_CLI_SANDBOX_ROLLOUT_ATTESTATION"] = "not-ready"
		if _, err := loadConnectorResourceProofConfig(func(key string) string { return copy[key] }, now); err == nil || !strings.Contains(err.Error(), "five-op-cells-ready") {
			t.Fatalf("wrong rollout error = %v", err)
		}
	})
	t.Run("wrong endpoint digest", func(t *testing.T) {
		copy := make(map[string]string, len(complete))
		for key, value := range complete {
			copy[key] = value
		}
		copy["QURL_ENDPOINT"] = "https://different.example/v1"
		if _, err := loadConnectorResourceProofConfig(func(key string) string { return copy[key] }, now); err == nil || !strings.Contains(err.Error(), "pinned SHA-256") {
			t.Fatalf("wrong endpoint digest error = %v", err)
		}
	})
	t.Run("production endpoint rejected even when its digest matches", func(t *testing.T) {
		copy := make(map[string]string, len(complete))
		for key, value := range complete {
			copy[key] = value
		}
		copy["QURL_ENDPOINT"] = "https://api.layerv.ai/v1"
		digest := sha256.Sum256([]byte(copy["QURL_ENDPOINT"]))
		copy["QURL_CLI_SANDBOX_ENDPOINT_SHA256"] = hex.EncodeToString(digest[:])
		if _, err := loadConnectorResourceProofConfig(func(key string) string { return copy[key] }, now); err == nil || !strings.Contains(err.Error(), "sandbox") {
			t.Fatalf("production endpoint error = %v", err)
		}
	})
	t.Run("non-sandbox Hub rejected", func(t *testing.T) {
		copy := make(map[string]string, len(complete))
		for key, value := range complete {
			copy[key] = value
		}
		copy[hub.EnvHost] = "hub.nhp.layerv.ai"
		if _, err := loadConnectorResourceProofConfig(func(key string) string { return copy[key] }, now); err == nil || !strings.Contains(err.Error(), "sandbox NHP") {
			t.Fatalf("non-sandbox Hub error = %v", err)
		}
	})
	for name, expiresAt := range map[string]int64{
		"expired":        now.Add(-time.Minute).Unix(),
		"too short":      now.Add(10 * time.Minute).Unix(),
		"too long":       now.Add(3 * time.Hour).Unix(),
		"missing expiry": 0,
	} {
		t.Run("cleanup JWT "+name, func(t *testing.T) {
			copy := make(map[string]string, len(complete))
			for key, value := range complete {
				copy[key] = value
			}
			copy["QURL_CLI_SANDBOX_CLEANUP_JWT"] = proofJWT(t, expiresAt)
			if _, err := loadConnectorResourceProofConfig(func(key string) string { return copy[key] }, now); err == nil || !strings.Contains(err.Error(), "cleanup JWT") {
				t.Fatalf("cleanup JWT %s error = %v", name, err)
			}
		})
	}
	t.Run("cleanup JWT production audience", func(t *testing.T) {
		copy := make(map[string]string, len(complete))
		for key, value := range complete {
			copy[key] = value
		}
		copy["QURL_CLI_SANDBOX_CLEANUP_JWT"] = proofJWTForAudience(t, now.Add(time.Hour).Unix(), "https://api.layerv.ai")
		if _, err := loadConnectorResourceProofConfig(func(key string) string { return copy[key] }, now); err == nil || !strings.Contains(err.Error(), "sandbox API audience") {
			t.Fatalf("cleanup JWT production audience error = %v", err)
		}
	})
}

func proofJWT(t *testing.T, expiresAt int64) string {
	t.Helper()
	return proofJWTForAudience(t, expiresAt, connectorResourceProofSandboxAPIAudience)
}

func proofJWTForAudience(t *testing.T, expiresAt int64, audience string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, err := json.Marshal(map[string]any{"exp": expiresAt, "aud": audience})
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}
