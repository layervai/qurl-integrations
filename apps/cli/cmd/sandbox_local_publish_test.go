//go:build clisandbox

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/layervai/qurl-go/qurl"

	connectoragent "github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
)

const (
	localPublishSandboxArming = "QURL_CLI_SANDBOX_LOCAL_PUBLISH"
	sandboxCleanupTimeout     = 30 * time.Second
)

type sandboxHTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

var sandboxCleanupHTTPClient = &http.Client{
	Timeout: sandboxCleanupTimeout,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// TestSandboxLocalPublishLifecycleSmoke runs two complete unified customer
// command lifecycles against the live sandbox. The first cold-enrolls and
// serves; its graceful stop must retire the exact NHP session. The immediate
// second run must reuse the same durable device and Connector identities and
// serve again, proving exact retirement did not strand replacement admission.
//
// This is an attended, one-off proof rather than an every-commit CI test. It
// creates a native device in an ephemeral state directory, so the operator must
// provide a short-lived JWT that can revoke the resulting device credential;
// otherwise a hosted runner would leak durable device/assignment quota on every
// invocation. The JWT must represent the same owner as QURL_API_KEY: 404 is a
// cleanup failure, because a wrong-owner JWT also cannot see the new key. The
// resource and device key are both reclaimed before returning. Run explicitly:
//
//	QURL_CLI_SANDBOX_LOCAL_PUBLISH=enabled \
//	QURL_API_KEY_FILE=... QURL_ENDPOINT=... QURL_CLI_SANDBOX_CLEANUP_JWT_FILE=... \
//	QURL_SHARING_RUN_ID=... QURL_SHARING_RUN_ATTEMPT=... QURL_SHARING_RUNTIME=host \
//	QURL_CONNECTOR_HUB_HOST=... QURL_CONNECTOR_HUB_PORT=... \
//	QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64=... \
//	go test -tags=clisandbox -count=1 -run '^TestSandboxLocalPublishLifecycleSmoke$' ./apps/cli/cmd
func TestSandboxLocalPublishLifecycleSmoke(t *testing.T) {
	if os.Getenv(localPublishSandboxArming) != "enabled" {
		t.Skipf("SKIPPED LOUDLY: unified local-publish lifecycle smoke is disarmed — %s != enabled", localPublishSandboxArming)
	}
	key, err := readSandboxSecretFile(sandboxAPIKeyFileEnv, "QURL_API_KEY")
	if err != nil {
		t.Fatalf("load protected sandbox API key: %v", err)
	}
	cleanupJWT, err := readSandboxSecretFile(sandboxCleanupJWTFileEnv, "QURL_CLI_SANDBOX_CLEANUP_JWT")
	if err != nil {
		t.Fatalf("load protected sandbox cleanup JWT: %v", err)
	}
	namespace, err := sandboxNamespace("smoke")
	if err != nil {
		t.Fatalf("derive sandbox lifecycle namespace: %v", err)
	}
	endpoint := strings.TrimSpace(os.Getenv("QURL_ENDPOINT"))
	missing := []string{}
	for name, value := range map[string]string{
		"QURL_ENDPOINT":        endpoint,
		hub.EnvHost:            os.Getenv(hub.EnvHost),
		hub.EnvPort:            os.Getenv(hub.EnvPort),
		hub.EnvServerPublicKey: os.Getenv(hub.EnvServerPublicKey),
	} {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("unified local-publish lifecycle smoke is armed but missing %v", missing)
	}

	stateDir := t.TempDir()
	t.Setenv(state.EnvStateDirPrimary, stateDir)
	t.Setenv(state.EnvAgentID, namespace.AgentID)
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "sandbox-local-publish-lifecycle")
	}))
	t.Cleanup(echo.Close)

	var loaded *qurl.AgentState
	first := runSandboxLocalPublishCycle(t, "cold lifecycle", key, cleanupJWT, endpoint, echo.URL, namespace.ConnectorID, time.Second, func() {
		loaded = loadSandboxAgentState(t, stateDir)
		if loaded != nil {
			registerSandboxDeviceCredentialCleanup(t, endpoint, cleanupJWT, loaded.DeviceAPIKeyID)
			registerSandboxResourceCleanup(t, endpoint, namespace.ConnectorID, loaded.DeviceAPIKey)
		}
	})
	if loaded == nil {
		t.Fatal("cold lifecycle produced no durable device state")
	}
	if err := validateSandboxDeviceIdentity(loaded, namespace.AgentID, ""); err != nil {
		t.Fatalf("cold lifecycle identity: %v", err)
	}

	firstID := strings.TrimSpace(first.stdout.String())
	if assessment, assessErr := cridux.Assess(firstID); assessErr != nil || assessment.Kind != cridux.KindCRID {
		t.Fatalf("cold quiet local publish stdout = %q, want exactly one CRID: %v", first.stdout.String(), assessErr)
	}

	second := runSandboxLocalPublishCycle(t, "immediate replacement lifecycle", key, cleanupJWT, endpoint, echo.URL, namespace.ConnectorID, time.Second, nil)
	secondID := strings.TrimSpace(second.stdout.String())
	if secondID != firstID {
		t.Fatalf("replacement CRID = %q, want stable resource CRID %q", secondID, firstID)
	}
	firstRunID, firstRunErr := sandboxRetiredRunID(first.stderr.String())
	secondRunID, secondRunErr := sandboxRetiredRunID(second.stderr.String())
	if firstRunErr != nil || secondRunErr != nil || firstRunID == secondRunID {
		t.Fatalf("retirement run authorities = first %q (%v), second %q (%v); want two distinct positive retirement events", firstRunID, firstRunErr, secondRunID, secondRunErr)
	}
	reloaded := loadSandboxAgentState(t, stateDir)
	if err := validateSandboxDeviceIdentity(reloaded, namespace.AgentID, loaded.DeviceAPIKeyID); err != nil {
		t.Fatalf("replacement lifecycle identity: %v", err)
	}
}

func validateSandboxDeviceIdentity(loaded *qurl.AgentState, wantAgentID, wantDeviceKeyID string) error {
	if loaded == nil {
		return errors.New("durable device state is missing")
	}
	if loaded.AgentID != wantAgentID {
		return errors.New("durable agent identity does not match the canonical run namespace")
	}
	if wantDeviceKeyID != "" && loaded.DeviceAPIKeyID != wantDeviceKeyID {
		return errors.New("durable device credential identity changed across lifecycle restart")
	}
	return nil
}

func runSandboxLocalPublishCycle(t *testing.T, label, key, cleanupJWT, endpoint, targetURL, connectorID string, serveFor time.Duration, afterRun func()) *runResult {
	t.Helper()
	if serveFor <= 0 || serveFor > 90*time.Minute {
		t.Fatalf("%s serve duration %s is outside the protected lifecycle bound", label, serveFor)
	}
	ctx, cancel := context.WithTimeout(context.Background(), serveFor+60*time.Second)
	defer cancel()
	var cancelAfterReady *time.Timer
	res := runCLI(t, &runOpts{
		ctx:         ctx,
		args:        []string{"--endpoint", endpoint, "--quiet", "publish", targetURL, "--id", connectorID},
		env:         map[string]string{"QURL_API_KEY": key},
		syncStreams: true,
		realSleep:   true,
		connectorReady: func() {
			// Local publish writes its one-CRID stdout contract immediately
			// after this readiness callback. Keep the admitted route alive for
			// the requested interval, then simulate the customer's interrupt.
			cancelAfterReady = time.AfterFunc(serveFor, cancel)
		},
	})
	// Enrollment and resource creation may have committed even when the
	// command later reports a teardown/evidence failure. Install cleanup from
	// the durable local authority before any assertion below can stop the test.
	if afterRun != nil {
		afterRun()
	}
	if cancelAfterReady != nil {
		cancelAfterReady.Stop()
	}
	assertSandboxStreamsDoNotContainSecrets(t, res, key, cleanupJWT)
	if res.code != 130 {
		t.Fatalf("%s exit = %d, want 130 after ready-triggered graceful cancellation; stderr: %s", label, res.code, res.stderr.String())
	}
	for _, evidence := range []string{"login_success", "event=proxy_ready", "event=nhp_session_retired", "Stopped."} {
		if !strings.Contains(res.stderr.String(), evidence) {
			t.Fatalf("%s lacks %q evidence:\n%s", label, evidence, res.stderr.String())
		}
	}
	for _, failure := range []string{"session_retirement_failed", "nhp_session_exit_failed"} {
		if strings.Contains(res.stderr.String(), failure) {
			t.Fatalf("%s reported exact-session cleanup failure %q:\n%s", label, failure, res.stderr.String())
		}
	}
	retiredRunID, retiredErr := sandboxEventRunID(res.stderr.String(), "nhp_session_retired")
	admittedRunID, admittedErr := sandboxEventRunID(res.stderr.String(), "login_success")
	if retiredErr != nil || admittedErr != nil || retiredRunID != admittedRunID {
		t.Fatalf("%s exact-session evidence = admitted %q (%v), retired %q (%v); want the final admitted RunID retired", label, admittedRunID, admittedErr, retiredRunID, retiredErr)
	}
	return res
}

func sandboxRetiredRunID(logText string) (string, error) {
	return sandboxEventRunID(logText, "nhp_session_retired")
}

func sandboxEventRunID(logText, event string) (string, error) {
	if event == "" || strings.ContainsAny(event, " \t\r\n=") {
		return "", errors.New("sandbox evidence event name is invalid")
	}
	runID := ""
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
				break
			}
		}
	}
	if runID == "" {
		return "", errors.New("sandbox lifecycle event carried no cycle RunID")
	}
	if err := qurl.ValidateCycleRunID(runID); err != nil {
		return "", errors.New("sandbox lifecycle event carried a noncanonical cycle RunID")
	}
	return runID, nil
}

func assertSandboxStreamsDoNotContainSecrets(t *testing.T, res *runResult, secrets ...string) {
	t.Helper()
	combined := res.stdout.String() + res.stderr.String()
	for _, secret := range secrets {
		if secret != "" && strings.Contains(combined, secret) {
			t.Fatal("sandbox credential leaked into command output")
		}
	}
}

func loadSandboxAgentState(t *testing.T, stateDir string) *qurl.AgentState {
	t.Helper()
	store, err := qurl.OpenFileAgentState(filepath.Join(stateDir, state.AgentStateFile))
	if err != nil {
		t.Error("open sandbox agent state for cleanup failed")
		return nil
	}
	loaded, loadErr := store.LoadAgentState(context.Background())
	closeErr := store.Close()
	if loadErr != nil {
		t.Error("load sandbox agent state for cleanup failed")
		return nil
	}
	if closeErr != nil {
		// The already-loaded remote credential IDs still let cleanup proceed.
		t.Error("close sandbox agent state after cleanup read failed")
	}
	if loaded == nil {
		t.Error("sandbox enrollment returned no durable agent state to reclaim")
		return nil
	}
	return loaded
}

func registerSandboxDeviceCredentialCleanup(t *testing.T, endpoint, jwt, deviceKeyID string) {
	t.Helper()
	if strings.TrimSpace(deviceKeyID) == "" {
		t.Error("sandbox enrollment returned no durable device credential id to revoke")
		return
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), sandboxCleanupTimeout)
		defer cancel()
		if err := revokeSandboxDeviceCredential(ctx, sandboxCleanupHTTPClient, endpoint, jwt, deviceKeyID); err != nil {
			t.Error(err)
		}
	})
}

func revokeSandboxDeviceCredential(ctx context.Context, client sandboxHTTPDoer, endpoint, jwt, deviceKeyID string) error {
	requestURL := strings.TrimRight(endpoint, "/") + "/v1/api-keys/" + url.PathEscape(deviceKeyID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, requestURL, http.NoBody)
	if err != nil {
		return errors.New("build sandbox device credential cleanup request failed")
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("sandbox device credential cleanup request failed")
	}
	_, copyErr := io.Copy(io.Discard, resp.Body)
	closeErr := resp.Body.Close()
	if copyErr != nil || closeErr != nil {
		return errors.New("consume sandbox device credential cleanup response failed")
	}
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("sandbox device credential cleanup status = %d, want 204", resp.StatusCode)
	}
	return nil
}

func registerSandboxResourceCleanup(t *testing.T, endpoint, connectorID, deviceAPIKey string) {
	t.Helper()
	if strings.TrimSpace(deviceAPIKey) == "" {
		t.Error("sandbox enrollment returned no durable device credential for resource cleanup")
		return
	}
	origin, err := connectoragent.ResourceSDKOrigin(endpoint)
	if err != nil {
		t.Error("derive sandbox resource API origin for cleanup failed")
		return
	}
	client, err := qurl.NewClient(qurl.BearerToken(deviceAPIKey), qurl.WithBaseURL(origin))
	if err != nil {
		t.Error("open sandbox device resource client for cleanup failed")
		return
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), sandboxCleanupTimeout)
		defer cancel()
		resource, err := client.GetConnectorResourceBySlug(ctx, connectorID)
		if errors.Is(err, qurl.ErrConnectorResourceNotFound) {
			return
		}
		if err != nil || resource == nil {
			t.Error("find sandbox Connector resource for cleanup failed")
			return
		}
		if err := client.DeleteConnectorResource(ctx, resource.ResourceID); err != nil && !errors.Is(err, qurl.ErrConnectorResourceNotFound) {
			t.Error("revoke sandbox Connector resource cleanup failed")
		}
	})
}

func TestRegisterSandboxDeviceCredentialCleanup(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatalf("secure test state directory: %v", err)
	}
	store, err := qurl.OpenFileAgentState(filepath.Join(stateDir, state.AgentStateFile))
	if err != nil {
		t.Fatalf("open test agent state: %v", err)
	}
	if err := store.SaveAgentState(context.Background(), &qurl.AgentState{
		AgentID:        "agent-cleanup-test",
		DeviceAPIKeyID: "key/test",
	}); err != nil {
		_ = store.Close()
		t.Fatalf("save test agent state: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close test agent state: %v", err)
	}

	requestSeen := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("cleanup method = %q, want DELETE", r.Method)
		}
		if r.URL.EscapedPath() != "/v1/api-keys/key%2Ftest" {
			t.Errorf("cleanup path = %q, want escaped device key id", r.URL.EscapedPath())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer cleanup-jwt" {
			t.Errorf("cleanup authorization = %q, want cleanup JWT", got)
		}
		requestSeen <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	t.Run("registered cleanup", func(t *testing.T) {
		loaded := loadSandboxAgentState(t, stateDir)
		if loaded == nil {
			return
		}
		registerSandboxDeviceCredentialCleanup(t, server.URL, "cleanup-jwt", loaded.DeviceAPIKeyID)
	})
	select {
	case <-requestSeen:
	case <-time.After(time.Second):
		t.Fatal("registered device credential cleanup did not run")
	}
}

func TestRevokeSandboxDeviceCredentialFailsClosedAndRedacts(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			forwarded := false
			destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				forwarded = true
			}))
			t.Cleanup(destination.Close)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if status == http.StatusFound {
					http.Redirect(w, r, destination.URL, http.StatusFound)
					return
				}
				w.WriteHeader(status)
			}))
			t.Cleanup(server.Close)

			err := revokeSandboxDeviceCredential(context.Background(), sandboxCleanupHTTPClient, server.URL, "jwt-secret", "key-secret")
			if err == nil {
				t.Fatalf("cleanup status %d succeeded, want fail closed", status)
			}
			if forwarded {
				t.Fatal("cleanup client followed a redirect with its credential")
			}
			if strings.Contains(err.Error(), server.URL) || strings.Contains(err.Error(), "jwt-secret") || strings.Contains(err.Error(), "key-secret") {
				t.Fatalf("cleanup error exposed endpoint or credential detail: %q", err)
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := revokeSandboxDeviceCredential(ctx, sandboxCleanupHTTPClient, "https://secret-endpoint.invalid", "jwt-secret", "key-secret")
	if err == nil {
		t.Fatal("canceled cleanup succeeded, want error")
	}
	for _, secret := range []string{"secret-endpoint", "jwt-secret", "key-secret"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("canceled cleanup error exposed secret marker %q: %q", secret, err)
		}
	}
}

func TestSandboxCleanupReclaimsResourceBeforeDeviceCredential(t *testing.T) {
	const connectorID = "connector-cleanup-order"
	row := mintConnectorRow(t, connectorID)
	var (
		mu    sync.Mutex
		order []string
	)
	record := func(event string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, event)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/resources" && r.URL.Query().Get("slug") == connectorID:
			if got := r.Header.Get("Authorization"); got != "Bearer device-token" {
				t.Errorf("resource lookup authorization = %q, want device credential", got)
			}
			record("find-resource")
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]any{"data": []connectorResourceRow{row}}); err != nil {
				t.Errorf("encode resource lookup: %v", err)
			}
		case r.Method == http.MethodDelete && r.URL.EscapedPath() == "/v1/resources/"+url.PathEscape(row.ResourceID):
			if got := r.Header.Get("Authorization"); got != "Bearer device-token" {
				t.Errorf("resource cleanup authorization = %q, want device credential", got)
			}
			record("revoke-resource")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/api-keys/key-cleanup-order":
			if got := r.Header.Get("Authorization"); got != "Bearer cleanup-jwt" {
				t.Errorf("device cleanup authorization = %q, want cleanup JWT", got)
			}
			record("revoke-device")
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected cleanup request", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	t.Run("registered cleanups", func(t *testing.T) {
		// Device is registered first so LIFO cleanup keeps it authorized until
		// the Connector resource has been found and revoked.
		registerSandboxDeviceCredentialCleanup(t, server.URL, "cleanup-jwt", "key-cleanup-order")
		registerSandboxResourceCleanup(t, server.URL, connectorID, "device-token")
	})

	mu.Lock()
	defer mu.Unlock()
	want := []string{"find-resource", "revoke-resource", "revoke-device"}
	if len(order) != len(want) {
		t.Fatalf("cleanup order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("cleanup order = %v, want %v", order, want)
		}
	}
}

func TestValidateSandboxDeviceIdentity(t *testing.T) {
	valid := &qurl.AgentState{AgentID: "qurl-share-r1-a1-hs", DeviceAPIKeyID: "key-1"}
	if err := validateSandboxDeviceIdentity(valid, valid.AgentID, valid.DeviceAPIKeyID); err != nil {
		t.Fatalf("valid identity: %v", err)
	}
	for name, loaded := range map[string]*qurl.AgentState{
		"missing":          nil,
		"wrong agent":      {AgentID: "other", DeviceAPIKeyID: "key-1"},
		"wrong credential": {AgentID: valid.AgentID, DeviceAPIKeyID: "key-2"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateSandboxDeviceIdentity(loaded, valid.AgentID, valid.DeviceAPIKeyID); err == nil {
				t.Fatal("invalid durable identity accepted")
			}
		})
	}
}

func TestSandboxRetiredRunIDEvidence(t *testing.T) {
	got, err := sandboxRetiredRunID("time=x event=nhp_session_retired run_id=01abcdef23456789\n")
	if err != nil || got != "01abcdef23456789" {
		t.Fatalf("retired RunID = %q, %v", got, err)
	}
	for name, logText := range map[string]string{
		"missing": "event=proxy_ready run_id=01abcdef23456789\n",
		"empty":   "event=nhp_session_retired run_id=\n",
		"invalid": "event=nhp_session_retired run_id=not-canonical\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := sandboxRetiredRunID(logText); err == nil {
				t.Fatal("invalid retirement evidence accepted")
			}
		})
	}
	multiple, multipleErr := sandboxRetiredRunID("event=nhp_session_retired run_id=01abcdef23456789\nevent=nhp_session_retired run_id=02abcdef23456789\n")
	if multipleErr != nil || multiple != "02abcdef23456789" {
		t.Fatalf("last authoritative retirement = %q, %v", multiple, multipleErr)
	}
	matching := "event=login_success run_id=01abcdef23456789\nevent=nhp_session_retired run_id=01abcdef23456789\n"
	admitted, admittedErr := sandboxEventRunID(matching, "login_success")
	retired, retiredErr := sandboxEventRunID(matching, "nhp_session_retired")
	if admittedErr != nil || retiredErr != nil || admitted != retired {
		t.Fatalf("matching lifecycle evidence = admitted %q (%v), retired %q (%v)", admitted, admittedErr, retired, retiredErr)
	}
}
