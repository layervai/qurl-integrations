//go:build clisandbox && (darwin || linux)

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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	connectorservice "github.com/layervai/qurl-connector/pkg/service"

	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

const (
	linuxDaemonSandboxArming         = "QURL_CLI_SANDBOX_LINUX_DAEMON"
	macOSDaemonSandboxArming         = "QURL_CLI_SANDBOX_MACOS_DAEMON"
	posixDefaultFailureChildTestName = "TestSandboxPOSIXDefaultDaemonControlledFailureCleanupChild"
)

// TestSandboxMacOSDefaultDaemonLifecycle is the executable contract for the
// customer-default background path. Unlike the portable foreground sandbox
// journey, this test invokes the exact candidate binary and real launchd.
func TestSandboxMacOSDefaultDaemonLifecycle(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS default-daemon journey runs only on macOS")
	}
	testSandboxPOSIXDefaultDaemonLifecycle(t, "macOS", macOSDaemonSandboxArming)
}

// TestSandboxLinuxDefaultDaemonLifecycle is the executable contract for the
// exact packaged binary and the real systemd user manager.
func TestSandboxLinuxDefaultDaemonLifecycle(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux default-daemon journey runs only on Linux")
	}
	testSandboxPOSIXDefaultDaemonLifecycle(t, "Linux", linuxDaemonSandboxArming)
}

func testSandboxPOSIXDefaultDaemonLifecycle(t *testing.T, platform, arming string) {
	t.Helper()
	if os.Getenv(arming) != "enabled" {
		t.Skipf("SKIPPED LOUDLY: %s qURL daemon sandbox journey is disarmed — %s != enabled", platform, arming)
	}
	binaryInput := strings.TrimSpace(os.Getenv("QURL_CLI_SANDBOX_QURL_BINARY"))
	if binaryInput == "" {
		t.Fatal("QURL_CLI_SANDBOX_QURL_BINARY is required")
	}
	binary, err := filepath.Abs(binaryInput)
	if err != nil {
		t.Fatalf("resolve exact qurl candidate: %v", err)
	}
	if info, statErr := os.Stat(binary); statErr != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("exact qurl candidate is not executable: %v", statErr)
	}

	cliEnv := sandboxJourneyEnv(t)
	addSandboxRunIdentity(t, cliEnv)
	cleanupJWT := sandboxSecret(t, "QURL_CLI_SANDBOX_CLEANUP_JWT")
	for _, name := range []string{hub.EnvHost, hub.EnvPort, hub.EnvServerPublicKey} {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			t.Fatalf("%s daemon sandbox journey requires %s", platform, name)
		}
		cliEnv[name] = value
	}
	stateDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve canonical %s state directory: %v", platform, err)
	}
	cliEnv[connectorstate.EnvStateDirPrimary] = stateDir
	namespace, err := sandboxNamespace("smoke")
	if err != nil {
		t.Fatalf("derive %s journey namespace: %v", platform, err)
	}
	cliEnv[connectorstate.EnvAgentID] = namespace.AgentID
	cliEnv["PATH"] = filepath.Dir(binary) + string(os.PathListSeparator) + os.Getenv("PATH")
	bootstrapKey := cliEnv["QURL_API_KEY"]
	delete(cliEnv, "QURL_API_KEY")
	login := runExternalSandboxCLIInput(t, binary, cliEnv, bootstrapKey+"\n", "-o", "json", "login")
	if login.err != nil {
		t.Fatalf("one-time %s customer login: %v; stderr %q", platform, login.err, login.stderr)
	}
	var enrolled struct {
		OwnerID        string `json:"owner_id"`
		AuthType       string `json:"auth_type"`
		DeviceEnrolled bool   `json:"device_enrolled"`
	}
	if err := json.Unmarshal([]byte(login.stdout), &enrolled); err != nil {
		t.Fatalf("decode one-time %s customer login output: %v", platform, err)
	}
	if enrolled.OwnerID == "" || enrolled.AuthType != "api_key" || !enrolled.DeviceEnrolled {
		t.Fatalf("one-time %s customer login returned incomplete device identity: %+v", platform, enrolled)
	}
	loadedAfterLogin := loadSandboxAgentState(t, stateDir)
	if loadedAfterLogin == nil {
		t.Fatalf("one-time %s customer login did not persist a device identity", platform)
	}
	if err := validateSandboxDeviceIdentity(loadedAfterLogin, loadedAfterLogin.AgentID, ""); err != nil {
		t.Fatalf("one-time %s customer login durable identity: %v", platform, err)
	}
	recordSandboxCleanupDeviceKey(t, loadedAfterLogin.DeviceAPIKeyID)
	assertSandboxStateExcludesSecret(t, stateDir, bootstrapKey)
	if cleanupJWT != "" {
		registerSandboxDeviceCredentialCleanup(t, cliEnv["QURL_ENDPOINT"], cleanupJWT, loadedAfterLogin.DeviceAPIKeyID)
	}
	whoami := runExternalSandboxCLI(t, binary, cliEnv, "-o", "json", "whoami")
	if whoami.err != nil {
		t.Fatalf("warm %s device whoami: %v; stderr %q", platform, whoami.err, whoami.stderr)
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
		t.Fatalf("decode warm %s device whoami output: %v", platform, err)
	}
	if warmIdentity.OwnerID != enrolled.OwnerID || warmIdentity.AuthType != "api_key" || warmIdentity.APIKey == nil ||
		warmIdentity.APIKey.KeyID == "" || warmIdentity.APIKey.Kind != "device" ||
		!slices.Equal(warmIdentity.APIKey.Scopes, []string{"qurl:read", "qurl:resolve", "qurl:write"}) {
		t.Fatalf("warm %s device whoami = %+v, want enrolled owner %q and a device key", platform, warmIdentity, enrolled.OwnerID)
	}

	jobManager := connectorservice.NewUserJobManager()
	if err := jobManager.Remove(connectordaemon.DaemonJobLabel); err != nil {
		t.Fatalf("remove pre-existing qURL user job: %v", err)
	}
	t.Cleanup(func() {
		if err := jobManager.Remove(connectordaemon.DaemonJobLabel); err != nil {
			t.Errorf("remove qURL user job after journey: %v", err)
		}
	})
	t.Run(sandboxControlledFailureLifecyclePhase, func(t *testing.T) {
		failureCRID := runSandboxFailureChild(t, posixDefaultFailureChildTestName)
		assertSandboxFailureRemoteDeleted(t, binary, cliEnv, stateDir, failureCRID)
		status, statusErr := jobManager.Status(connectordaemon.DaemonJobLabel)
		if statusErr != nil || status.Installed || status.Running {
			t.Fatalf("controlled-failure user-job cleanup = %+v, %v; want absent", status, statusErr)
		}
	})

	marker := fmt.Sprintf("sandbox-macos-daemon-%d", time.Now().UnixNano())
	var backendHits atomic.Uint64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHits.Add(1)
		_, _ = io.WriteString(w, marker)
	}))
	defer backend.Close()

	connectorID := namespace.ConnectorID
	registerSandboxResourceCleanup(t, cliEnv["QURL_ENDPOINT"], connectorID, loadedAfterLogin.DeviceAPIKey)
	publish := runExternalSandboxCLI(t, binary, cliEnv, "--quiet", "publish", backend.URL,
		"--id", connectorID)
	cridValue := strings.TrimSpace(publish.stdout)
	if publish.err != nil || cridValue == "" || strings.Contains(cridValue, "\n") {
		t.Fatalf("default background publish = stdout %q, stderr %q, error %v", publish.stdout, publish.stderr, publish.err)
	}

	local := waitExternalSandboxShare(t, stateDir, cridValue, 2*time.Minute)
	if err := requireTestResourceIdentity(local.CRID, local.ResourceID); err != nil {
		t.Fatalf("sandbox minted a non-test CRID: %v", err)
	}

	status, err := jobManager.Status(connectordaemon.DaemonJobLabel)
	if err != nil || !status.Installed || !status.Running {
		t.Fatalf("qURL user job after publish = %+v, %v; want installed and running", status, err)
	}
	assertPOSIXUserJobContainsNoCredential(t, cliEnv["QURL_ENDPOINT"], cliEnv[hub.EnvHost], cliEnv[hub.EnvServerPublicKey], bootstrapKey, cleanupJWT)

	initial := waitExternalSandboxState(t, binary, cliEnv, cridValue, "on", "serving", 2*time.Minute)
	inspect := runExternalSandboxCLI(t, binary, cliEnv, "-o", "json", "inspect", cridValue)
	assertHealthySandboxInspection(t, []byte(inspect.stdout), inspect.err, inspect.stderr,
		cridValue, local.ResourceID, initial.DesiredState, initial.ConnectionState, initial.ServingEpoch,
		bootstrapKey, cleanupJWT, loadedAfterLogin.DeviceAPIKey)
	assertSandboxListRow(t, binary, cliEnv, stateDir, local, initial.ServingEpoch)
	assertExternalSandboxRoute(t, binary, cliEnv, cridValue, marker, 2*time.Minute)
	assertSandboxRemoteURLDeviceJourney(t, binary, cliEnv, stateDir)

	stopped := externalSandboxLifecycle(t, binary, cliEnv, "stop", cridValue)
	if err := validateSandboxSharingTransition(stopped, "off", "stopped", initial.ServingEpoch); err != nil {
		t.Fatalf("stop state = %+v: %v", stopped, err)
	}
	assertSandboxLocalRouteFenced(t, binary, cliEnv, stateDir, cridValue, marker, &backendHits)
	started := externalSandboxLifecycle(t, binary, cliEnv, "start", cridValue)
	if err := validateSandboxSharingTransition(started, "on", "serving", stopped.ServingEpoch); err != nil {
		t.Fatalf("start state = %+v: %v", started, err)
	}
	assertExternalSandboxRoute(t, binary, cliEnv, cridValue, marker, 2*time.Minute)
	restarted := externalSandboxLifecycle(t, binary, cliEnv, "restart", cridValue)
	if err := validateSandboxSharingTransition(restarted, "on", "serving", started.ServingEpoch); err != nil {
		t.Fatalf("restart state = %+v: %v", restarted, err)
	}
	assertExternalSandboxRoute(t, binary, cliEnv, cridValue, marker, 2*time.Minute)
	if backendHits.Load() < 3 {
		t.Fatalf("local backend saw %d route hits, want at least one before and after lifecycle changes", backendHits.Load())
	}

	deleted := runExternalSandboxCLI(t, binary, cliEnv, "delete", cridValue, "--yes")
	if deleted.err != nil {
		t.Fatalf("delete while daemon is serving: %v; stderr %q", deleted.err, deleted.stderr)
	}
	shares, present, err := connectorstate.ReadLocalSharesIfPresent(context.Background(), stateDir)
	if err != nil || !present {
		t.Fatalf("read local registry after delete = (present %v, %v)", present, err)
	}
	for index := range shares {
		if shares[index].CRID == cridValue {
			t.Fatalf("deleted CRID %s remains in local daemon registry", cridValue)
		}
	}
}

// TestSandboxPOSIXDefaultDaemonControlledFailureCleanupChild drives the exact
// packaged binary through its default background-service path, then fails on
// purpose. The parent requires the resource, registry, and user job to be gone
// after Go runs this child's cleanup stack.
func TestSandboxPOSIXDefaultDaemonControlledFailureCleanupChild(t *testing.T) {
	stateDir := sandboxFailureChildStateDir(t)
	var cridValue string
	productCleanupComplete := false
	registerSandboxFailureFinalCleanup(t, stateDir, &cridValue, &productCleanupComplete)

	binaryInput := strings.TrimSpace(os.Getenv("QURL_CLI_SANDBOX_QURL_BINARY"))
	if binaryInput == "" {
		t.Fatal("QURL_CLI_SANDBOX_QURL_BINARY is required")
	}
	binary, err := filepath.Abs(binaryInput)
	if err != nil {
		t.Fatalf("resolve exact qurl candidate: %v", err)
	}
	if info, statErr := os.Stat(binary); statErr != nil || info.Mode()&0o111 == 0 {
		t.Fatalf("exact qurl candidate is not executable: %v", statErr)
	}
	cliEnv := sandboxJourneyEnv(t)
	addSandboxRunIdentity(t, cliEnv)
	cleanupJWT := sandboxSecret(t, "QURL_CLI_SANDBOX_CLEANUP_JWT")
	for _, name := range []string{hub.EnvHost, hub.EnvPort, hub.EnvServerPublicKey} {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			t.Fatalf("POSIX controlled-failure journey requires %s", name)
		}
		cliEnv[name] = value
	}
	namespace, err := sandboxNamespace("failure")
	if err != nil {
		t.Fatalf("derive POSIX controlled-failure namespace: %v", err)
	}
	cliEnv[connectorstate.EnvStateDirPrimary] = stateDir
	cliEnv[connectorstate.EnvAgentID] = namespace.AgentID
	cliEnv["PATH"] = filepath.Dir(binary) + string(os.PathListSeparator) + os.Getenv("PATH")
	bootstrapKey := cliEnv["QURL_API_KEY"]
	delete(cliEnv, "QURL_API_KEY")
	login := runExternalSandboxCLIInput(t, binary, cliEnv, bootstrapKey+"\n", "-o", "json", "login")
	if login.err != nil {
		t.Fatalf("controlled-failure POSIX login: %v; stderr %q", login.err, login.stderr)
	}
	device := loadSandboxAgentState(t, stateDir)
	if err := validateSandboxDeviceIdentity(device, namespace.AgentID, ""); err != nil {
		t.Fatalf("controlled-failure POSIX durable identity: %v", err)
	}
	recordSandboxCleanupDeviceKey(t, device.DeviceAPIKeyID)
	if cleanupJWT != "" {
		registerSandboxDeviceCredentialCleanup(t, cliEnv["QURL_ENDPOINT"], cleanupJWT, device.DeviceAPIKeyID)
	}
	assertSandboxStateExcludesSecret(t, stateDir, bootstrapKey)

	jobManager := connectorservice.NewUserJobManager()
	if err := jobManager.Remove(connectordaemon.DaemonJobLabel); err != nil {
		t.Fatalf("remove POSIX controlled-failure user job: %v", err)
	}
	t.Cleanup(func() {
		if err := jobManager.Remove(connectordaemon.DaemonJobLabel); err != nil {
			t.Errorf("remove POSIX controlled-failure user job after failure: %v", err)
		}
	})

	marker := fmt.Sprintf("sandbox-posix-controlled-failure-%d", time.Now().UnixNano())
	var backendHits atomic.Uint64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHits.Add(1)
		_, _ = io.WriteString(w, marker)
	}))
	defer backend.Close()
	registerSandboxResourceCleanup(t, cliEnv["QURL_ENDPOINT"], namespace.ConnectorID, device.DeviceAPIKey)
	published := runExternalSandboxCLI(t, binary, cliEnv, "--quiet", "publish", backend.URL,
		"--id", namespace.ConnectorID)
	cridValue = strings.TrimSpace(published.stdout)
	if published.err != nil || cridValue == "" || strings.Contains(cridValue, "\n") {
		t.Fatalf("controlled-failure POSIX publish = stdout %q, stderr %q, error %v", published.stdout, published.stderr, published.err)
	}
	t.Cleanup(func() {
		deleted := runExternalSandboxCLI(t, binary, cliEnv, "delete", cridValue, "--yes")
		if deleted.err != nil {
			t.Errorf("controlled-failure POSIX delete: %v; stderr %q", deleted.err, deleted.stderr)
			return
		}
		shares, present, readErr := connectorstate.ReadLocalSharesIfPresent(context.Background(), stateDir)
		if readErr != nil {
			t.Errorf("read controlled-failure POSIX registry after delete: %v", readErr)
			return
		}
		for _, share := range shares {
			if share.CRID == cridValue {
				t.Errorf("controlled-failure POSIX CRID %s remains in local registry", cridValue)
				return
			}
		}
		if !present && len(shares) != 0 {
			t.Errorf("controlled-failure POSIX registry has %d rows while absent", len(shares))
			return
		}
		productCleanupComplete = true
	})

	local := waitExternalSandboxShare(t, stateDir, cridValue, time.Minute)
	status, statusErr := jobManager.Status(connectordaemon.DaemonJobLabel)
	if statusErr != nil || !status.Installed || !status.Running {
		t.Fatalf("controlled-failure POSIX user job = %+v, %v; want installed and running", status, statusErr)
	}
	initial := waitExternalSandboxState(t, binary, cliEnv, cridValue, "on", "serving", time.Minute)
	assertExternalSandboxRoute(t, binary, cliEnv, cridValue, marker, time.Minute)
	stopped := externalSandboxLifecycle(t, binary, cliEnv, "stop", cridValue)
	if err := validateSandboxSharingTransition(stopped, "off", "stopped", initial.ServingEpoch); err != nil {
		t.Fatalf("controlled-failure POSIX stop state = %+v: %v", stopped, err)
	}
	assertSandboxControlledFailureRouteFencedInputs(t, binary, cliEnv, stateDir, local.CRID, marker, &backendHits, 30*time.Second)
	failedGet := runExternalSandboxCLI(t, binary, cliEnv, "--quiet", "get", cridValue,
		"--file", filepath.Join(t.TempDir(), "fenced"))
	if sandboxExternalExitCode(failedGet.err) != exitcode.Unavailable || failedGet.stdout != "" ||
		!strings.Contains(strings.ToLower(failedGet.stderr), "aren't available") {
		t.Fatalf("controlled customer get did not return the fenced-resource failure: error %v, stdout %q, stderr %q",
			failedGet.err, failedGet.stdout, failedGet.stderr)
	}
	t.Fatal(sandboxFailureChildSentinel)
}

func sandboxExternalExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

type externalCLIResult struct {
	stdout string
	stderr string
	err    error
}

func runExternalSandboxCLI(t *testing.T, binary string, env map[string]string, args ...string) externalCLIResult {
	t.Helper()
	return runExternalSandboxCLIInput(t, binary, env, "", args...)
}

func runExternalSandboxCLIInput(t *testing.T, binary string, env map[string]string, input string, args ...string) externalCLIResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	commandArgs := append([]string{"--endpoint", env["QURL_ENDPOINT"]}, args...)
	cmd := exec.CommandContext(ctx, binary, commandArgs...) //nolint:gosec // The protected test validates the exact CLI binary before use.
	cmd.Env = externalSandboxEnvironment(env)
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if secret := strings.TrimSpace(input); secret != "" && strings.Contains(stdout.String()+stderr.String(), secret) {
		t.Fatal("macOS customer command exposed the one-time API key")
	}
	return externalCLIResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

func externalSandboxEnvironment(overrides map[string]string) []string {
	values := map[string]string{}
	for _, key := range []string{"DBUS_SESSION_BUS_ADDRESS", "HOME", "LANG", "LC_ALL", "LOGNAME", "PATH", "SHELL", "TERM", "TMPDIR", "USER", "XDG_RUNTIME_DIR"} {
		if value, ok := os.LookupEnv(key); ok {
			values[key] = value
		}
	}
	for key, value := range overrides {
		values[key] = value
	}
	// The protected harness reads the API key from QURL_API_KEY_FILE, then
	// passes only the exact value to the customer process. Do not inherit the
	// file source as well: the production CLI rejects two credential sources.
	if _, ok := overrides["QURL_API_KEY"]; ok {
		delete(values, "QURL_API_KEY_FILE")
	}
	result := make([]string, 0, len(values))
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	return result
}

func TestExternalSandboxEnvironmentUsesOneAPIKeySource(t *testing.T) {
	t.Setenv("QURL_API_KEY", "inherited-inline")
	t.Setenv("QURL_API_KEY_FILE", "/run/secrets/inherited-api-key")
	t.Setenv("ACTIONS_RUNTIME_TOKEN", "runner-authority")
	got := map[string]string{}
	for _, entry := range externalSandboxEnvironment(map[string]string{
		"QURL_API_KEY":  "exact-customer-key",
		"QURL_ENDPOINT": "https://sandbox.example",
	}) {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			got[key] = value
		}
	}
	if got["QURL_API_KEY"] != "exact-customer-key" {
		t.Fatal("customer process did not receive the exact inline API key")
	}
	if _, present := got["QURL_API_KEY_FILE"]; present {
		t.Fatal("customer process inherited a second API key source")
	}
	if got["QURL_ENDPOINT"] != "https://sandbox.example" {
		t.Fatal("customer process lost its exact endpoint override")
	}
	if got["ACTIONS_RUNTIME_TOKEN"] != "" {
		t.Fatal("customer process inherited GitHub runner authority")
	}
	withoutCredential := map[string]string{}
	for _, entry := range externalSandboxEnvironment(map[string]string{"QURL_ENDPOINT": "https://sandbox.example"}) {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			withoutCredential[key] = value
		}
	}
	if withoutCredential["QURL_API_KEY"] != "" || withoutCredential["QURL_API_KEY_FILE"] != "" {
		t.Fatal("warm customer process inherited the one-time API key")
	}
}

func waitExternalSandboxShare(t *testing.T, stateDir, cridValue string, limit time.Duration) *connectorstate.LocalShare {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		shares, present, err := connectorstate.ReadLocalSharesIfPresent(context.Background(), stateDir)
		if err != nil {
			t.Fatalf("read macOS daemon share registry: %v", err)
		}
		if present {
			for i := range shares {
				if shares[i].CRID == cridValue {
					return &shares[i]
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for daemon registry row for %s", cridValue)
	return nil
}

func externalSandboxLifecycle(t *testing.T, binary string, env map[string]string, command, cridValue string) sandboxSharingDoc {
	t.Helper()
	result := runExternalSandboxCLI(t, binary, env, "-o", "json", command, cridValue)
	if result.err != nil {
		t.Fatalf("qurl %s: %v; stderr %q", command, result.err, result.stderr)
	}
	var document sandboxSharingDoc
	if err := json.Unmarshal([]byte(result.stdout), &document); err != nil {
		t.Fatalf("decode qurl %s output: %v", command, err)
	}
	return document
}

func waitExternalSandboxState(t *testing.T, binary string, env map[string]string, cridValue, desired, observed string, limit time.Duration) sandboxSharingDoc {
	t.Helper()
	deadline := time.Now().Add(limit)
	var last string
	for time.Now().Before(deadline) {
		result := runExternalSandboxCLI(t, binary, env, "-o", "json", "status", cridValue)
		if result.err == nil {
			var document sandboxSharingDoc
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
	t.Fatalf("timed out waiting for %s/%s state for %s; last result: %s", desired, observed, cridValue, last)
	return sandboxSharingDoc{}
}

func assertExternalSandboxRoute(t *testing.T, binary string, env map[string]string, cridValue, marker string, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	var last string
	for time.Now().Before(deadline) {
		destination := filepath.Join(t.TempDir(), "payload")
		result := runExternalSandboxCLI(t, binary, env, "get", cridValue, "--file", destination)
		if result.err == nil {
			payload, err := os.ReadFile(destination) //nolint:gosec // The destination is an isolated test file under t.TempDir.
			if err == nil && string(payload) == marker {
				return
			}
			last = fmt.Sprintf("payload read = %v, length %d", err, len(payload))
		} else {
			last = result.stderr
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("public qURL route for %s did not deliver the local backend bytes: %s", cridValue, last)
}
