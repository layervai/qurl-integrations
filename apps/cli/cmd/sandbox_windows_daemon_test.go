//go:build clisandbox && windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	connectorservice "github.com/layervai/qurl-connector/pkg/service"
	"github.com/layervai/qurl-go/qurl"

	connectoragent "github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
)

const (
	windowsDaemonSandboxArming = "QURL_CLI_SANDBOX_WINDOWS_DAEMON"
	windowsSandboxCommandLimit = 3 * time.Minute
	windowsSandboxCleanupLimit = 30 * time.Second
)

// TestSandboxWindowsDefaultDaemonFullCustomerLifecycle is the protected
// release-candidate journey for Windows. It invokes the exact packaged
// qurl.exe and the real per-user Task Scheduler daemon. Source-only tests and
// cross-compilation cannot replace this test.
func TestSandboxWindowsDefaultDaemonFullCustomerLifecycle(t *testing.T) {
	if os.Getenv(windowsDaemonSandboxArming) != "enabled" {
		t.Skipf("SKIPPED LOUDLY: Windows qURL daemon sandbox journey is disarmed - %s != enabled", windowsDaemonSandboxArming)
	}
	binaryInput := strings.TrimSpace(os.Getenv("QURL_CLI_SANDBOX_BINARY"))
	if binaryInput == "" {
		t.Fatal("QURL_CLI_SANDBOX_BINARY is required")
	}
	binary, err := filepath.Abs(binaryInput)
	if err != nil {
		t.Fatalf("resolve exact Windows qurl candidate: %v", err)
	}
	if info, statErr := os.Stat(binary); statErr != nil || !info.Mode().IsRegular() {
		t.Fatalf("exact Windows qurl candidate is unavailable: %v", statErr)
	}

	cliEnv := sandboxJourneyEnv(t)
	addSandboxRunIdentity(t, cliEnv)
	cleanupJWT := sandboxSecret(t, "QURL_CLI_SANDBOX_CLEANUP_JWT")
	for _, name := range []string{hub.EnvHost, hub.EnvPort, hub.EnvServerPublicKey} {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			t.Fatalf("Windows daemon sandbox journey requires %s", name)
		}
		cliEnv[name] = value
	}
	stateDir := windowsSandboxStateDir(t)
	cliEnv[connectorstate.EnvStateDirPrimary] = stateDir
	namespace, err := sandboxNamespace("smoke")
	if err != nil {
		t.Fatalf("derive Windows journey namespace: %v", err)
	}
	cliEnv[connectorstate.EnvAgentID] = namespace.AgentID
	cliEnv["PATH"] = filepath.Dir(binary) + string(os.PathListSeparator) + os.Getenv("PATH")

	bootstrapKey := cliEnv["QURL_API_KEY"]
	delete(cliEnv, "QURL_API_KEY")
	login := runWindowsSandboxCLIInput(t, binary, cliEnv, bootstrapKey+"\n", "-o", "json", "login")
	if login.err != nil {
		t.Fatalf("one-time Windows customer login: %v; stderr %q", login.err, login.stderr)
	}
	var enrolled struct {
		OwnerID        string `json:"owner_id"`
		AuthType       string `json:"auth_type"`
		DeviceEnrolled bool   `json:"device_enrolled"`
	}
	if err := json.Unmarshal([]byte(login.stdout), &enrolled); err != nil {
		t.Fatalf("decode one-time Windows customer login output: %v", err)
	}
	if enrolled.OwnerID == "" || enrolled.AuthType != "api_key" || !enrolled.DeviceEnrolled {
		t.Fatalf("one-time Windows customer login returned incomplete device identity: %+v", enrolled)
	}
	device := loadWindowsSandboxAgentState(t, stateDir)
	if device.AgentID == "" || device.DeviceAPIKey == "" || device.DeviceAPIKeyID == "" ||
		device.PrivateKeyB64 == "" || device.PublicKeyB64 == "" {
		t.Fatal("one-time Windows customer login did not persist a complete device identity")
	}
	if cleanupJWT != "" {
		registerWindowsSandboxDeviceCleanup(t, cliEnv["QURL_ENDPOINT"], cleanupJWT, device.DeviceAPIKeyID)
	}
	assertWindowsSandboxStateExcludesSecret(t, stateDir, bootstrapKey)
	logout := runWindowsSandboxCLI(t, binary, cliEnv, "-o", "json", "logout")
	if logout.err != nil {
		t.Fatalf("post-enrollment Windows logout: %v; stderr %q", logout.err, logout.stderr)
	}
	var logoutResult struct {
		Removed []string `json:"removed"`
	}
	if err := json.Unmarshal([]byte(logout.stdout), &logoutResult); err != nil {
		t.Fatalf("decode post-enrollment Windows logout output: %v", err)
	}
	if len(logoutResult.Removed) != 0 {
		t.Fatalf("post-enrollment Windows logout removed legacy account-key stores: %v", logoutResult.Removed)
	}

	whoami := runWindowsSandboxCLI(t, binary, cliEnv, "-o", "json", "whoami")
	if whoami.err != nil {
		t.Fatalf("warm Windows device whoami: %v; stderr %q", whoami.err, whoami.stderr)
	}
	var warmIdentity struct {
		OwnerID  string `json:"owner_id"`
		AuthType string `json:"auth_type"`
		APIKey   *struct {
			KeyID  string   `json:"key_id"`
			Kind   string   `json:"kind"`
			Scopes []string `json:"scopes"`
		} `json:"api_key"`
	}
	if err := json.Unmarshal([]byte(whoami.stdout), &warmIdentity); err != nil {
		t.Fatalf("decode warm Windows device whoami output: %v", err)
	}
	if warmIdentity.OwnerID != enrolled.OwnerID || warmIdentity.AuthType != "api_key" || warmIdentity.APIKey == nil ||
		warmIdentity.APIKey.KeyID != device.DeviceAPIKeyID || warmIdentity.APIKey.Kind != "device" ||
		!slices.Equal(warmIdentity.APIKey.Scopes, []string{"qurl:read", "qurl:resolve", "qurl:write"}) {
		t.Fatalf("warm Windows device whoami = %+v, want enrolled owner %q and the durable device key", warmIdentity, enrolled.OwnerID)
	}

	jobManager := connectorservice.NewUserJobManager()
	if err := jobManager.Remove(connectordaemon.DaemonJobLabel); err != nil {
		t.Fatalf("remove pre-existing qURL Task Scheduler job: %v", err)
	}
	t.Cleanup(func() {
		if err := jobManager.Remove(connectordaemon.DaemonJobLabel); err != nil {
			t.Errorf("remove qURL Task Scheduler job after journey: %v", err)
		}
	})

	marker := fmt.Sprintf("sandbox-windows-daemon-%d", time.Now().UnixNano())
	var backendHits atomic.Uint64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHits.Add(1)
		_, _ = io.WriteString(w, marker)
	}))
	defer backend.Close()

	connectorID := namespace.ConnectorID
	registerWindowsSandboxResourceCleanup(t, cliEnv["QURL_ENDPOINT"], connectorID, device.DeviceAPIKey)
	published := runWindowsSandboxCLI(t, binary, cliEnv, "--quiet", "publish", backend.URL, "--id", connectorID)
	cridValue := strings.TrimSpace(published.stdout)
	if published.err != nil || cridValue == "" || strings.Contains(cridValue, "\n") {
		t.Fatalf("default Windows background publish = stdout %q, stderr %q, error %v", published.stdout, published.stderr, published.err)
	}
	if assessment, assessErr := cridux.Assess(cridValue); assessErr != nil || assessment.Kind != cridux.KindCRID {
		t.Fatalf("Windows background publish returned invalid CRID %q: %v", cridValue, assessErr)
	}
	local := waitWindowsSandboxShare(t, stateDir, cridValue, 2*time.Minute)
	if local.ResourceID == "" || local.ConnectorID != connectorID {
		t.Fatalf("Windows local-share registry row is incomplete: %+v", local)
	}
	jobStatus, err := jobManager.Status(connectordaemon.DaemonJobLabel)
	if err != nil || !jobStatus.Installed || !jobStatus.Running {
		t.Fatalf("qURL Task Scheduler job after publish = %+v, %v; want installed and running", jobStatus, err)
	}

	initial := waitWindowsSandboxState(t, binary, cliEnv, cridValue, "on", "serving", 2*time.Minute)
	inspect := windowsSandboxLifecycle(t, binary, cliEnv, "inspect", cridValue)
	if inspect != initial {
		t.Fatalf("Windows inspect state = %+v, want status state %+v", inspect, initial)
	}
	assertWindowsSandboxListContains(t, binary, cliEnv, cridValue)
	assertWindowsSandboxRoute(t, binary, cliEnv, cridValue, marker, 2*time.Minute)

	t.Run("remote_url_resource_lifecycle", func(t *testing.T) {
		runWindowsSandboxRemoteJourney(t, binary, cliEnv)
	})

	t.Run("local_connector_lifecycle", func(t *testing.T) {
		stopped := windowsSandboxLifecycle(t, binary, cliEnv, "stop", cridValue)
		if err := validateWindowsSandboxSharingTransition(stopped, "off", "stopped", initial.ServingEpoch); err != nil {
			t.Fatalf("Windows stop state = %+v: %v", stopped, err)
		}
		assertWindowsSandboxRouteFenced(t, binary, cliEnv, cridValue, &backendHits)

		started := windowsSandboxLifecycle(t, binary, cliEnv, "start", cridValue)
		if err := validateWindowsSandboxSharingTransition(started, "on", "serving", stopped.ServingEpoch); err != nil {
			t.Fatalf("Windows start state = %+v: %v", started, err)
		}
		assertWindowsSandboxRoute(t, binary, cliEnv, cridValue, marker, 2*time.Minute)

		restarted := windowsSandboxLifecycle(t, binary, cliEnv, "restart", cridValue)
		if err := validateWindowsSandboxSharingTransition(restarted, "on", "serving", started.ServingEpoch); err != nil {
			t.Fatalf("Windows restart state = %+v: %v", restarted, err)
		}
		assertWindowsSandboxRoute(t, binary, cliEnv, cridValue, marker, 2*time.Minute)
		if backendHits.Load() < 3 {
			t.Fatalf("Windows local backend saw %d route hits, want at least one before and after lifecycle changes", backendHits.Load())
		}

		deleted := runWindowsSandboxCLI(t, binary, cliEnv, "delete", cridValue, "--yes")
		if deleted.err != nil {
			t.Fatalf("delete Windows local share while serving: %v; stderr %q", deleted.err, deleted.stderr)
		}
		shares, present, readErr := connectorstate.ReadLocalSharesIfPresent(context.Background(), stateDir)
		if readErr != nil || !present {
			t.Fatalf("read Windows registry after delete = (present %v, %v)", present, readErr)
		}
		for _, share := range shares {
			if share.CRID == cridValue {
				t.Fatalf("deleted Windows CRID %s remains in local daemon registry", cridValue)
			}
		}
	})
	if err := jobManager.Remove(connectordaemon.DaemonJobLabel); err != nil {
		t.Fatalf("remove qURL Task Scheduler job after successful journey: %v", err)
	}
	assertWindowsSandboxLogsExcludeSecrets(t, stateDir, bootstrapKey, cleanupJWT)
}

type windowsSandboxCLIResult struct {
	stdout string
	stderr string
	err    error
}

func runWindowsSandboxCLI(t *testing.T, binary string, env map[string]string, args ...string) windowsSandboxCLIResult {
	t.Helper()
	return runWindowsSandboxCLIInput(t, binary, env, "", args...)
}

func runWindowsSandboxCLIInput(t *testing.T, binary string, env map[string]string, input string, args ...string) windowsSandboxCLIResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), windowsSandboxCommandLimit)
	defer cancel()
	commandArgs := append([]string{"--endpoint", env["QURL_ENDPOINT"]}, args...)
	cmd := exec.CommandContext(ctx, binary, commandArgs...) //nolint:gosec // The protected lane selects one verified exact artifact.
	cmd.Env = windowsSandboxEnvironment(env)
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	for _, secret := range []string{strings.TrimSpace(input), env["QURL_API_KEY"], os.Getenv("QURL_CLI_SANDBOX_CLEANUP_JWT")} {
		if secret != "" && strings.Contains(stdout.String()+stderr.String(), secret) {
			t.Fatal("Windows customer command exposed a protected credential")
		}
	}
	return windowsSandboxCLIResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

func windowsSandboxEnvironment(overrides map[string]string) []string {
	values := map[string]string{}
	for _, key := range []string{
		"APPDATA", "COMSPEC", "LOCALAPPDATA", "PATH", "PATHEXT", "ProgramData",
		"ProgramFiles", "ProgramFiles(x86)", "SystemRoot", "TEMP", "TMP", "USERNAME",
		"USERPROFILE", "WINDIR",
	} {
		if value, ok := os.LookupEnv(key); ok {
			values[key] = value
		}
	}
	for key, value := range overrides {
		values[key] = value
	}
	result := make([]string, 0, len(values))
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	return result
}

type windowsSandboxSharingDoc struct {
	CRID            string `json:"crid"`
	ResourceID      string `json:"resource_id"`
	TargetURL       string `json:"target_url"`
	DesiredState    string `json:"desired_state"`
	ConnectionState string `json:"connection_state"`
	ServingEpoch    uint64 `json:"serving_epoch"`
}

func windowsSandboxLifecycle(t *testing.T, binary string, env map[string]string, command, cridValue string) windowsSandboxSharingDoc {
	t.Helper()
	result := runWindowsSandboxCLI(t, binary, env, "-o", "json", command, cridValue)
	if result.err != nil {
		t.Fatalf("Windows qurl %s: %v; stderr %q", command, result.err, result.stderr)
	}
	var document windowsSandboxSharingDoc
	if err := json.Unmarshal([]byte(result.stdout), &document); err != nil {
		t.Fatalf("decode Windows qurl %s output: %v", command, err)
	}
	return document
}

func validateWindowsSandboxSharingTransition(document windowsSandboxSharingDoc, desired, observed string, priorEpoch uint64) error { //nolint:gocritic // Keep validation on one immutable decoded snapshot.
	if document.DesiredState != desired || document.ConnectionState != observed {
		return fmt.Errorf("got %s/%s, want %s/%s", document.DesiredState, document.ConnectionState, desired, observed)
	}
	if document.ServingEpoch <= priorEpoch {
		return fmt.Errorf("serving epoch %d did not advance beyond %d", document.ServingEpoch, priorEpoch)
	}
	return nil
}

func waitWindowsSandboxState(t *testing.T, binary string, env map[string]string, cridValue, desired, observed string, limit time.Duration) windowsSandboxSharingDoc {
	t.Helper()
	deadline := time.Now().Add(limit)
	last := "status was not attempted"
	for time.Now().Before(deadline) {
		result := runWindowsSandboxCLI(t, binary, env, "-o", "json", "status", cridValue)
		if result.err == nil {
			var document windowsSandboxSharingDoc
			if err := json.Unmarshal([]byte(result.stdout), &document); err == nil {
				if document.DesiredState == desired && document.ConnectionState == observed {
					return document
				}
				last = result.stdout
			} else {
				last = err.Error()
			}
		} else {
			last = result.stderr
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for Windows %s/%s state for %s; last result: %s", desired, observed, cridValue, last)
	return windowsSandboxSharingDoc{}
}

func waitWindowsSandboxShare(t *testing.T, stateDir, cridValue string, limit time.Duration) *connectorstate.LocalShare {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		shares, present, err := connectorstate.ReadLocalSharesIfPresent(context.Background(), stateDir)
		if err != nil {
			t.Fatalf("read Windows daemon share registry: %v", err)
		}
		if present {
			for index := range shares {
				if shares[index].CRID == cridValue {
					return &shares[index]
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for Windows daemon registry row for %s", cridValue)
	return nil
}

func assertWindowsSandboxRoute(t *testing.T, binary string, env map[string]string, cridValue, marker string, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	last := "route was not attempted"
	for time.Now().Before(deadline) {
		destination := filepath.Join(t.TempDir(), "payload")
		result := runWindowsSandboxCLI(t, binary, env, "get", cridValue, "--file", destination)
		if result.err == nil {
			payload, err := os.ReadFile(destination) //nolint:gosec // Isolated test destination.
			if err == nil && string(payload) == marker {
				return
			}
			last = fmt.Sprintf("payload read = %v, length %d", err, len(payload))
		} else {
			last = result.stderr
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("Windows public qURL route for %s did not deliver local backend bytes: %s", cridValue, last)
}

func assertWindowsSandboxRouteFenced(t *testing.T, binary string, env map[string]string, cridValue string, backendHits *atomic.Uint64) {
	t.Helper()
	time.Sleep(2 * time.Second)
	settledHits := backendHits.Load()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		destination := filepath.Join(t.TempDir(), "stopped-payload")
		result := runWindowsSandboxCLI(t, binary, env, "get", cridValue, "--file", destination)
		if result.err == nil {
			payload, _ := os.ReadFile(destination) //nolint:gosec // Isolated test destination.
			t.Fatalf("stopped Windows qURL route succeeded with %d bytes", len(payload))
		}
		if backendHits.Load() != settledHits {
			t.Fatal("stopped Windows qURL route reached the local backend")
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func assertWindowsSandboxListContains(t *testing.T, binary string, env map[string]string, cridValue string) {
	t.Helper()
	result := runWindowsSandboxCLI(t, binary, env, "-o", "json", "list", "--status", "active", "--limit", "100")
	if result.err != nil {
		t.Fatalf("Windows list: %v; stderr %q", result.err, result.stderr)
	}
	var document struct {
		Resources []struct {
			CRID string `json:"crid"`
		} `json:"resources"`
	}
	if err := json.Unmarshal([]byte(result.stdout), &document); err != nil {
		t.Fatalf("decode Windows list output: %v", err)
	}
	for _, resource := range document.Resources {
		if resource.CRID == cridValue {
			return
		}
	}
	t.Fatalf("Windows list did not contain newly published CRID %s", cridValue)
}

func runWindowsSandboxRemoteJourney(t *testing.T, binary string, env map[string]string) {
	t.Helper()
	target := fmt.Sprintf("https://example.com/?qurl-private-windows-journey=%d", time.Now().UnixNano())
	published := runWindowsSandboxCLI(t, binary, env, "-o", "json", "publish", target, "--description", journeyDescription)
	if published.err != nil {
		t.Fatalf("Windows remote publish: %v; stderr %q", published.err, published.stderr)
	}
	var resource journeyPublishDoc
	if err := json.Unmarshal([]byte(published.stdout), &resource); err != nil {
		t.Fatalf("decode Windows remote publish output: %v", err)
	}
	if resource.CRID == "" || resource.ResourceID == "" || resource.TargetURL != target || resource.FoundExisting {
		t.Fatalf("Windows remote publish = %+v, want one new URL resource", resource)
	}
	deleted := false
	t.Cleanup(func() {
		if !deleted {
			cleanup := runWindowsSandboxCLI(t, binary, env, "delete", resource.CRID, "--yes")
			if cleanup.err != nil {
				t.Errorf("cleanup Windows remote resource: %v; stderr %q", cleanup.err, cleanup.stderr)
			}
		}
	})
	for _, command := range []string{"status", "inspect"} {
		result := runWindowsSandboxCLI(t, binary, env, "-o", "json", command, resource.CRID)
		if result.err != nil {
			t.Fatalf("Windows remote %s: %v; stderr %q", command, result.err, result.stderr)
		}
		var status journeyResourceStatusDoc
		if err := json.Unmarshal([]byte(result.stdout), &status); err != nil {
			t.Fatalf("decode Windows remote %s output: %v", command, err)
		}
		if status.CRID != resource.CRID || status.ResourceID != resource.ResourceID || status.TargetURL != target ||
			status.Type != "url" || status.Status != "active" {
			t.Fatalf("Windows remote %s = %+v, want active URL %+v", command, status, resource)
		}
	}
	assertWindowsSandboxListContains(t, binary, env, resource.CRID)
	resolved := runWindowsSandboxCLI(t, binary, env, "resolve", resource.CRID)
	if resolved.err != nil {
		t.Fatalf("Windows remote resolve: %v; stderr %q", resolved.err, resolved.stderr)
	}
	parsed, err := url.Parse(strings.TrimSpace(resolved.stdout))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		t.Fatalf("Windows remote resolve did not return one HTTPS link: %v", err)
	}
	destination := filepath.Join(t.TempDir(), "remote-payload")
	downloaded := runWindowsSandboxCLI(t, binary, env, "get", resource.CRID, "--file", destination)
	if downloaded.err != nil {
		t.Fatalf("Windows remote get: %v; stderr %q", downloaded.err, downloaded.stderr)
	}
	payload, err := os.ReadFile(destination) //nolint:gosec // Isolated test destination.
	if err != nil || !strings.Contains(string(payload), journeyTargetMarker) {
		t.Fatalf("Windows remote get returned unexpected bytes: read %v, length %d", err, len(payload))
	}
	stopped := runWindowsSandboxCLI(t, binary, env, "stop", resource.CRID)
	if stopped.err == nil || !strings.Contains(stopped.stderr, "stop applies only to a local qURL Connector") {
		t.Fatalf("Windows remote stop guidance = error %v, stderr %q", stopped.err, stopped.stderr)
	}
	removed := runWindowsSandboxCLI(t, binary, env, "delete", resource.CRID, "--yes")
	if removed.err != nil {
		t.Fatalf("Windows remote delete: %v; stderr %q", removed.err, removed.stderr)
	}
	deleted = true
	redeleted := runWindowsSandboxCLI(t, binary, env, "delete", resource.CRID, "--yes")
	if redeleted.err != nil {
		t.Fatalf("Windows remote idempotent re-delete: %v; stderr %q", redeleted.err, redeleted.stderr)
	}
	revoked := runWindowsSandboxCLI(t, binary, env, "resolve", resource.CRID)
	if revoked.err == nil || revoked.stdout != "" || !strings.Contains(strings.ToLower(revoked.stderr), "deleted") {
		t.Fatalf("Windows resolve after delete = error %v, stdout %q, stderr %q", revoked.err, revoked.stdout, revoked.stderr)
	}
}

func windowsSandboxStateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	if err := connectorstate.EnsureDirMode(dir); err != nil {
		t.Fatalf("create protected Windows state directory: %v", err)
	}
	return dir
}

func loadWindowsSandboxAgentState(t *testing.T, stateDir string) *qurl.AgentState {
	t.Helper()
	store, err := qurl.OpenFileAgentState(filepath.Join(stateDir, connectorstate.AgentStateFile))
	if err != nil {
		t.Fatalf("open Windows sandbox agent state: %v", err)
	}
	loaded, loadErr := store.LoadAgentState(context.Background())
	closeErr := store.Close()
	if loadErr != nil || closeErr != nil || loaded == nil {
		t.Fatalf("load Windows sandbox agent state = state %v, load %v, close %v", loaded != nil, loadErr, closeErr)
	}
	return loaded
}

func assertWindowsSandboxStateExcludesSecret(t *testing.T, root, secret string) {
	t.Helper()
	if secret == "" {
		t.Fatal("cannot verify Windows state without the one-time bootstrap secret")
	}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return fmt.Errorf("Windows sandbox state contains unsupported file type at %s", path)
		}
		raw, err := os.ReadFile(path) //nolint:gosec // Exact test-owned state path.
		if err != nil {
			return err
		}
		if bytes.Contains(raw, []byte(secret)) {
			return errors.New("one-time account API key remained in Windows sandbox state")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func assertWindowsSandboxLogsExcludeSecrets(t *testing.T, stateDir string, secrets ...string) {
	t.Helper()
	for _, name := range []string{"share-daemon.log", "share-daemon.err.log"} {
		path := filepath.Join(stateDir, "logs", name)
		raw, err := os.ReadFile(path) //nolint:gosec // Exact protected log path under the test-owned state directory.
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatalf("read protected Windows daemon log %s: %v", name, err)
		}
		for _, secret := range secrets {
			if secret != "" && bytes.Contains(raw, []byte(secret)) {
				t.Fatalf("protected Windows daemon log %s contains a bearer credential", name)
			}
		}
	}
}

func registerWindowsSandboxDeviceCleanup(t *testing.T, endpoint, jwt, deviceKeyID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), windowsSandboxCleanupLimit)
		defer cancel()
		requestURL := strings.TrimRight(endpoint, "/") + "/v1/api-keys/" + url.PathEscape(deviceKeyID)
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, requestURL, http.NoBody)
		if err != nil {
			t.Errorf("build Windows device cleanup request: %v", err)
			return
		}
		req.Header.Set("Authorization", "Bearer "+jwt)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("Windows device cleanup request: %v", err)
			return
		}
		_, copyErr := io.Copy(io.Discard, resp.Body)
		closeErr := resp.Body.Close()
		if copyErr != nil || closeErr != nil || resp.StatusCode != http.StatusNoContent {
			t.Errorf("Windows device cleanup = status %d, copy %v, close %v", resp.StatusCode, copyErr, closeErr)
		}
	})
}

func registerWindowsSandboxResourceCleanup(t *testing.T, endpoint, connectorID, deviceAPIKey string) {
	t.Helper()
	origin, err := connectoragent.ResourceSDKOrigin(endpoint)
	if err != nil {
		t.Fatalf("derive Windows resource API origin: %v", err)
	}
	client, err := qurl.NewClient(qurl.BearerToken(deviceAPIKey), qurl.WithBaseURL(origin))
	if err != nil {
		t.Fatalf("open Windows device resource client: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), windowsSandboxCleanupLimit)
		defer cancel()
		resource, err := client.GetConnectorResourceBySlug(ctx, connectorID)
		if errors.Is(err, qurl.ErrConnectorResourceNotFound) {
			return
		}
		if err != nil || resource == nil {
			t.Errorf("find Windows Connector resource for cleanup: %v", err)
			return
		}
		if err := client.DeleteConnectorResource(ctx, resource.ResourceID); err != nil && !errors.Is(err, qurl.ErrConnectorResourceNotFound) {
			t.Errorf("revoke Windows Connector resource: %v", err)
		}
	})
}
