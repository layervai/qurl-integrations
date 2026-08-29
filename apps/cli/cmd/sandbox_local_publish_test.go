//go:build clisandbox

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base32"
	"encoding/base64"
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
	"sync/atomic"
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

type connectorResourceRow struct {
	ResourceID         string `json:"resource_id"`
	ConnectorRoutingID string `json:"connector_routing_id"`
	KnockResourceID    string `json:"knock_resource_id"`
	Type               string `json:"type"`
	Status             string `json:"status"`
	Slug               string `json:"slug"`
	CRID               string `json:"crid,omitempty"`
}

var connectorRoutingIDEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func mintConnectorRow(t *testing.T, slug string) connectorResourceRow {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(der)
	return connectorResourceRow{
		ResourceID:         base64.RawURLEncoding.EncodeToString(der),
		ConnectorRoutingID: "c-" + connectorRoutingIDEncoding.EncodeToString(digest[:]),
		KnockResourceID:    "resource-public-key", Type: "tunnel", Status: "active", Slug: slug,
	}
}

var sandboxCleanupHTTPClient = &http.Client{
	Timeout: sandboxCleanupTimeout,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// TestSandboxLocalPublishLifecycleSmoke exercises the unified customer command against
// the live sandbox. Unlike the legacy Connector smoke, it starts with an
// ordinary login key and no pre-issued enrollment token or device state. A
// passing run validates that the login key can mint the exact one-shot credential,
// native UDP enrollment exchanges it for a device identity, the device creates
// the tunnel resource, and the FRP server admits the resulting route. It then
// sends unique HTTP bytes through a minted qURL, exercises stop/start/restart,
// and deletes the live share while its foreground daemon is still running.
//
// This is an attended, one-off smoke rather than an every-commit CI test. It
// creates a native device in an ephemeral state directory, so the operator must
// provide a short-lived JWT that can revoke the resulting device credential;
// otherwise a hosted runner would leak durable device/assignment quota on every
// invocation. The JWT must represent the same owner as QURL_API_KEY: 404 is a
// cleanup failure, because a wrong-owner JWT also cannot see the new key. The
// resource and device key are both reclaimed before returning. Run explicitly:
//
//	QURL_CLI_SANDBOX_LOCAL_PUBLISH=enabled \
//	QURL_API_KEY=... QURL_ENDPOINT=... QURL_CLI_SANDBOX_CLEANUP_JWT=... \
//	QURL_CONNECTOR_HUB_HOST=... QURL_CONNECTOR_HUB_PORT=... \
//	QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64=... \
//	go test -tags=clisandbox -count=1 -run '^TestSandboxLocalPublishLifecycleSmoke$' ./apps/cli/cmd
func TestSandboxLocalPublishLifecycleSmoke(t *testing.T) {
	if os.Getenv(localPublishSandboxArming) != "enabled" {
		t.Skipf("SKIPPED LOUDLY: unified local-publish sandbox smoke is disarmed — %s != enabled", localPublishSandboxArming)
	}
	fixture := startSandboxLocalPublish(t, "smoke")
	defer fixture.stop(t)
	cliEnv, stateDir, local := fixture.env, fixture.stateDir, fixture.local

	if assessment, err := cridux.Assess(local.CRID); err != nil || assessment.Kind != cridux.KindCRID {
		t.Fatalf("local publish registry CRID = %q, want a valid full CRID: %v", local.CRID, err)
	}

	initial := waitSandboxSharingState(t, cliEnv, stateDir, local.CRID, "on", "serving", 2*time.Minute)
	assertSandboxListRow(t, cliEnv, stateDir, local, initial.ServingEpoch)
	assertSandboxLocalRoute(t, cliEnv, stateDir, local.CRID, fixture.marker, 2*time.Minute)

	stopped := runSandboxLocalCLI(t, cliEnv, stateDir, "-o", "json", "stop", local.CRID)
	stoppedState := decodeSandboxSharing(t, stopped)
	if stoppedState.DesiredState != "off" || stoppedState.ConnectionState != "stopped" || stoppedState.ServingEpoch <= initial.ServingEpoch {
		t.Fatalf("stop state = %+v, want off/stopped at an advanced epoch", stoppedState)
	}
	started := runSandboxLocalCLI(t, cliEnv, stateDir, "-o", "json", "start", local.CRID)
	startedState := decodeSandboxSharing(t, started)
	if startedState.DesiredState != "on" || startedState.ConnectionState != "serving" || startedState.ServingEpoch < stoppedState.ServingEpoch {
		t.Fatalf("start state = %+v, want on/serving without epoch regression", startedState)
	}
	assertSandboxLocalRoute(t, cliEnv, stateDir, local.CRID, fixture.marker, 2*time.Minute)

	restarted := runSandboxLocalCLI(t, cliEnv, stateDir, "-o", "json", "restart", local.CRID)
	restartedState := decodeSandboxSharing(t, restarted)
	if restartedState.DesiredState != "on" || restartedState.ConnectionState != "serving" || restartedState.ServingEpoch <= startedState.ServingEpoch {
		t.Fatalf("restart state = %+v, want on/serving at an advanced epoch", restartedState)
	}
	assertSandboxLocalRoute(t, cliEnv, stateDir, local.CRID, fixture.marker, 2*time.Minute)
	if fixture.backendHits.Load() < 3 {
		t.Fatalf("local backend saw %d public-route hits, want at least one before and after lifecycle changes", fixture.backendHits.Load())
	}

	deleted := runSandboxLocalCLI(t, cliEnv, stateDir, "delete", local.CRID, "--yes")
	if deleted.code != 0 {
		t.Fatalf("delete while serving exit = %d: %s", deleted.code, deleted.stderr.String())
	}
	shares, present, err := state.ReadLocalSharesIfPresent(context.Background(), stateDir)
	if err != nil || !present {
		t.Fatalf("read local registry after delete = (present %v, %v)", present, err)
	}
	for _, share := range shares {
		if share.CRID == local.CRID {
			t.Fatalf("deleted CRID %s remains in local daemon registry", local.CRID)
		}
	}
}

type sandboxLocalFixture struct {
	env        map[string]string
	stateDir   string
	marker     string
	local      *state.LocalShare
	key        string
	cleanupJWT string

	backendHits atomic.Uint64
	cancel      context.CancelFunc
	done        <-chan *runResult
	stopOnce    sync.Once
}

func startSandboxLocalPublish(t *testing.T, label string) *sandboxLocalFixture {
	t.Helper()
	cliEnv := sandboxJourneyEnv(t)
	cleanupJWT := sandboxSecret(t, "QURL_CLI_SANDBOX_CLEANUP_JWT")
	missing := []string{}
	for name, value := range map[string]string{
		"QURL_CLI_SANDBOX_CLEANUP_JWT": cleanupJWT,
		hub.EnvHost:                    os.Getenv(hub.EnvHost),
		hub.EnvPort:                    os.Getenv(hub.EnvPort),
		hub.EnvServerPublicKey:         os.Getenv(hub.EnvServerPublicKey),
	} {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Skipf("SKIPPED LOUDLY: unified local-publish sandbox %s is disarmed — missing %v", label, missing)
	}
	for _, name := range []string{hub.EnvHost, hub.EnvPort, hub.EnvServerPublicKey} {
		cliEnv[name] = strings.TrimSpace(os.Getenv(name))
	}

	fixture := &sandboxLocalFixture{
		env: cliEnv, stateDir: t.TempDir(), cleanupJWT: cleanupJWT,
		key: cliEnv["QURL_API_KEY"], marker: fmt.Sprintf("sandbox-local-publish-%s-%d", label, time.Now().UnixNano()),
	}
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fixture.backendHits.Add(1)
		_, _ = io.WriteString(w, fixture.marker)
	}))
	t.Cleanup(echo.Close)

	connectorID := fmt.Sprintf("connector-sandbox-local-publish-%s-%d", label, time.Now().UnixNano())
	publishCtx, cancelPublish := context.WithCancel(context.Background())
	publishDone := make(chan *runResult, 1)
	publishConfigDir := t.TempDir()
	go func() {
		publishDone <- runCLI(t, &runOpts{
			args: []string{
				"--endpoint", cliEnv["QURL_ENDPOINT"], "--quiet", "publish", echo.URL,
				"--id", connectorID, "--foreground",
			},
			env: cliEnv, ctx: publishCtx, configDir: publishConfigDir, shareStateDir: fixture.stateDir,
			syncStreams: true, realSleep: true,
		})
	}()
	fixture.cancel, fixture.done = cancelPublish, publishDone
	fixture.local = waitSandboxLocalShare(t, fixture.stateDir, publishDone, 2*time.Minute)
	loaded := loadSandboxAgentState(t, fixture.stateDir)
	if loaded != nil {
		registerSandboxDeviceCredentialCleanup(t, cliEnv["QURL_ENDPOINT"], cleanupJWT, loaded.DeviceAPIKeyID)
		registerSandboxResourceCleanup(t, cliEnv["QURL_ENDPOINT"], connectorID, loaded.DeviceAPIKey)
	}
	if err := requireTestResourceIdentity(fixture.local.CRID, fixture.local.ResourceID); err != nil {
		fixture.stop(t)
		t.Fatalf("sandbox minted a non-test CRID: %v", err)
	}
	return fixture
}

func (f *sandboxLocalFixture) stop(t *testing.T) {
	t.Helper()
	if f == nil {
		return
	}
	f.stopOnce.Do(func() {
		f.cancel()
		select {
		case res := <-f.done:
			assertSandboxStreamsDoNotContainSecrets(t, res, f.key, f.cleanupJWT)
			if f.local != nil && res.stdout.String() != f.local.CRID+"\n" {
				t.Errorf("foreground publish stdout = %q, want exactly the full CRID and newline", res.stdout.String())
			}
			if strings.Contains(res.stderr.String(), "refresh-mode") || strings.Contains(res.stderr.String(), "explicit approval") {
				t.Errorf("foreground publish exposed retired assignment-approval UX: %s", res.stderr.String())
			}
		case <-time.After(15 * time.Second):
			t.Error("foreground qurl publish did not stop after cancellation")
		}
	})
}

type sandboxSharingDoc struct {
	CRID            string `json:"crid"`
	ResourceID      string `json:"resource_id"`
	TargetURL       string `json:"target_url"`
	DesiredState    string `json:"desired_state"`
	ConnectionState string `json:"connection_state"`
	ServingEpoch    uint64 `json:"serving_epoch"`
}

func sandboxSecret(t *testing.T, name string) string {
	t.Helper()
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	path := strings.TrimSpace(os.Getenv(name + "_FILE"))
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s_FILE: %v", name, err)
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		t.Fatalf("%s_FILE is empty", name)
	}
	return value
}

func waitSandboxLocalShare(t *testing.T, stateDir string, publishDone <-chan *runResult, limit time.Duration) *state.LocalShare {
	t.Helper()
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		shares, present, err := state.ReadLocalSharesIfPresent(context.Background(), stateDir)
		if err != nil {
			t.Fatalf("read local share registry: %v", err)
		}
		if present && len(shares) == 1 {
			return &shares[0]
		}
		select {
		case res := <-publishDone:
			t.Fatalf("foreground publish exited before persisting a local share (exit %d): %s", res.code, res.stderr.String())
		case <-deadline.C:
			t.Fatal("timed out waiting for foreground publish to persist its local share")
		case <-ticker.C:
		}
	}
}

func runSandboxLocalCLI(t *testing.T, env map[string]string, stateDir string, args ...string) *runResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	return runCLI(t, &runOpts{
		args: args, env: env, ctx: ctx, configDir: t.TempDir(), shareStateDir: stateDir,
		realSleep: true, realOpener: true,
	})
}

func decodeSandboxSharing(t *testing.T, res *runResult) sandboxSharingDoc {
	t.Helper()
	if res.code != 0 {
		t.Fatalf("sharing command exit = %d: %s", res.code, res.stderr.String())
	}
	var doc sandboxSharingDoc
	if err := json.Unmarshal(res.stdout.Bytes(), &doc); err != nil {
		t.Fatalf("decode sharing output %q: %v", res.stdout.String(), err)
	}
	return doc
}

func waitSandboxSharingState(t *testing.T, env map[string]string, stateDir, crid, desired, observed string, limit time.Duration) sandboxSharingDoc {
	t.Helper()
	deadline := time.Now().Add(limit)
	var last string
	for time.Now().Before(deadline) {
		res := runSandboxLocalCLI(t, env, stateDir, "-o", "json", "status", crid)
		if res.code == 0 {
			var doc sandboxSharingDoc
			if err := json.Unmarshal(res.stdout.Bytes(), &doc); err == nil {
				if doc.DesiredState == desired && doc.ConnectionState == observed {
					return doc
				}
				last = res.stdout.String()
			} else {
				last = err.Error()
			}
		} else {
			last = res.stderr.String()
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s/%s sharing state for %s; last result: %s", desired, observed, crid, last)
	return sandboxSharingDoc{}
}

func assertSandboxListRow(t *testing.T, env map[string]string, stateDir string, local *state.LocalShare, epoch uint64) {
	t.Helper()
	res := runSandboxLocalCLI(t, env, stateDir, "-o", "json", "list", "--status", "active", "--limit", "100")
	if res.code != 0 {
		t.Fatalf("list local share exit = %d: %s", res.code, res.stderr.String())
	}
	var doc struct {
		Resources []sandboxSharingDoc `json:"resources"`
	}
	if err := json.Unmarshal(res.stdout.Bytes(), &doc); err != nil {
		t.Fatalf("decode list output: %v", err)
	}
	for _, row := range doc.Resources {
		if row.CRID == local.CRID {
			if row.TargetURL != local.TargetURL || row.DesiredState != "on" || row.ConnectionState != "serving" || row.ServingEpoch != epoch {
				t.Fatalf("list row = %+v, want full local target and on/serving epoch %d", row, epoch)
			}
			return
		}
	}
	t.Fatalf("full CRID %s not found in local share listing", local.CRID)
}

func assertSandboxLocalRoute(t *testing.T, env map[string]string, stateDir, crid, marker string, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	var last string
	for time.Now().Before(deadline) {
		if err := sandboxLocalRouteOnce(t, env, stateDir, crid, marker); err == nil {
			return
		} else {
			last = err.Error()
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("public qURL route for %s did not deliver the local backend bytes: %s", crid, last)
}

func sandboxLocalRouteOnce(t *testing.T, env map[string]string, stateDir, crid, marker string) error {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "payload")
	res := runSandboxLocalCLI(t, env, stateDir, "get", crid, "--file", dest)
	if res.code != 0 {
		return fmt.Errorf("get exited %d: %s", res.code, res.stderr.String())
	}
	payload, err := os.ReadFile(dest)
	if err != nil {
		return err
	}
	if string(payload) != marker {
		return fmt.Errorf("payload length %d did not match unique local marker", len(payload))
	}
	return nil
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
