//go:build clisandbox && (darwin || linux)

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	connectorservice "github.com/layervai/qurl-connector/pkg/service"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
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
	requireSandboxFailureCredentials(t)
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

	registerSandboxTempDirRemoval(t)
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
		// The state directory is a temp directory of this test. Do not let it be
		// removed under a daemon that is still stopping.
		if err := waitSandboxDaemonExited(stateDir, sandboxDaemonExitTimeout); err != nil {
			t.Error(err)
		}
	})
	t.Run(sandboxControlledFailureLifecyclePhase, func(t *testing.T) {
		registerSandboxTempDirRemoval(t)
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

	jobWait, err := waitSandboxUserJobRunning(func() (connectorservice.ServiceStatus, error) {
		return jobManager.Status(connectordaemon.DaemonJobLabel)
	}, 2*time.Minute, sandboxUserJobPoll, time.Now, time.Sleep)
	if err != nil {
		t.Fatalf("qURL user job after publish = %+v, %v; want installed and running", jobWait.Last, err)
	}
	if jobWait.Samples > 1 {
		t.Logf("qURL user job after publish was %+v (answer failed: %t) when first asked; it was installed and running after %d answers in %s",
			jobWait.First, jobWait.FirstErr != nil, jobWait.Samples, jobWait.Waited.Round(time.Millisecond))
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
	sharedBeforeDelete := runExternalSandboxCLI(t, binary, cliEnv, "share", cridValue)
	if sharedBeforeDelete.err != nil || strings.TrimSpace(sharedBeforeDelete.stdout) == "" {
		t.Fatal("share before Connector delete failed; private details withheld")
	}
	var grantedBeforeDelete *sandboxGrantedRoute
	if platform != "macOS" {
		// GitHub-hosted macOS runners use different public egress addresses for
		// the NHP relay and the content connection. The resulting /32 admission
		// correctly fences this retained-grant probe before it reaches the local
		// backend. Linux and Windows keep the exact retained-grant fence proof;
		// this lane still proves the macOS customer route with fresh qurl get
		// commands before and after every lifecycle transition below.
		grantedBeforeDelete = prepareSandboxGrantedRoute(t, cliEnv, sharedBeforeDelete.stdout, marker, backendHits.Load)
	}

	deleted := runExternalSandboxCLI(t, binary, cliEnv, "delete", cridValue, "--yes")
	if deleted.err != nil {
		t.Fatalf("delete while daemon is serving: %v; stderr %q", deleted.err, deleted.stderr)
	}
	if grantedBeforeDelete != nil {
		assertSandboxGrantedRouteFenced(t, grantedBeforeDelete)
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
	assertExternalSandboxDeleted(t, binary, cliEnv, cridValue)

	// Reusing the friendly name must create a new identity. On lanes with a
	// retained access grant, that old grant remains valid but fenced. The
	// cleanup registered by slug above also covers a replacement created before
	// a later command fails.
	republished := runExternalSandboxCLI(t, binary, cliEnv, "--quiet", "publish", backend.URL,
		"--id", connectorID)
	replacementCRID := strings.TrimSpace(republished.stdout)
	if republished.err != nil || replacementCRID == "" || replacementCRID == cridValue || strings.Contains(replacementCRID, "\n") {
		t.Fatalf("same-name publish = stdout %q, stderr %q, error %v", republished.stdout, republished.stderr, republished.err)
	}
	replacement := waitExternalSandboxShare(t, stateDir, replacementCRID, 2*time.Minute)
	if err := requireTestResourceIdentity(replacement.CRID, replacement.ResourceID); err != nil {
		t.Fatalf("same-name publish minted a non-test CRID: %v", err)
	}
	if replacement.ConnectorID != connectorID || replacement.ResourceID == local.ResourceID || replacement.ConnectorRoutingID == local.ConnectorRoutingID {
		t.Fatalf("same-name replacement retained old identity or changed name: old=%+v new=%+v", local, replacement)
	}
	assertExternalSandboxRoute(t, binary, cliEnv, replacementCRID, marker, 2*time.Minute)
	if grantedBeforeDelete != nil {
		assertSandboxGrantedRouteFenced(t, grantedBeforeDelete)
	}
	repeated := runExternalSandboxCLI(t, binary, cliEnv, "--quiet", "publish", backend.URL,
		"--id", connectorID)
	if repeated.err != nil || repeated.stdout != replacementCRID+"\n" {
		t.Fatalf("repeat same-name publish = stdout %q, stderr %q, error %v", repeated.stdout, repeated.stderr, repeated.err)
	}
	redeleted := runExternalSandboxCLI(t, binary, cliEnv, "delete", cridValue, "--yes")
	if redeleted.err != nil {
		t.Fatalf("re-delete old CRID: %v; stderr %q", redeleted.err, redeleted.stderr)
	}
	assertExternalSandboxDeleted(t, binary, cliEnv, cridValue)
	assertExternalSandboxRoute(t, binary, cliEnv, replacementCRID, marker, 2*time.Minute)
	removedReplacement := runExternalSandboxCLI(t, binary, cliEnv, "delete", replacementCRID, "--yes")
	if removedReplacement.err != nil {
		t.Fatalf("delete replacement CRID: %v; stderr %q", removedReplacement.err, removedReplacement.stderr)
	}
	assertExternalSandboxDeleted(t, binary, cliEnv, replacementCRID)
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
	markSandboxFailurePhase(sandboxFailurePhaseSetup)

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
	markSandboxFailurePhase(sandboxFailurePhaseLogin)
	login := runExternalSandboxCLIInput(t, binary, cliEnv, bootstrapKey+"\n", "-o", "json", "login")
	if login.err != nil {
		markSandboxFailureLoginDiagnostic(login.stderr, login.err)
		t.Fatalf("controlled-failure POSIX login: %v; stderr %q", login.err, login.stderr)
	}
	markSandboxFailurePhase(sandboxFailurePhaseIdentity)
	device := loadSandboxAgentState(t, stateDir)
	if err := validateSandboxDeviceIdentity(device, namespace.AgentID, ""); err != nil {
		t.Fatalf("controlled-failure POSIX durable identity: %v", err)
	}
	if cleanupJWT != "" {
		registerSandboxDeviceCredentialCleanup(t, cliEnv["QURL_ENDPOINT"], cleanupJWT, device.DeviceAPIKeyID)
	}
	assertSandboxStateExcludesSecret(t, stateDir, bootstrapKey)

	markSandboxFailurePhase(sandboxFailurePhaseService)
	jobManager := connectorservice.NewUserJobManager()
	if err := jobManager.Remove(connectordaemon.DaemonJobLabel); err != nil {
		t.Fatalf("remove POSIX controlled-failure user job: %v", err)
	}
	t.Cleanup(func() {
		if err := jobManager.Remove(connectordaemon.DaemonJobLabel); err != nil {
			t.Errorf("remove POSIX controlled-failure user job after failure: %v", err)
		}
		// The parent removes this state directory when the child has returned.
		// A daemon that is still stopping withholds the cleanup marker, as a
		// daemon that still answers does.
		if err := waitSandboxDaemonExited(stateDir, sandboxDaemonExitTimeout); err != nil {
			productCleanupComplete = false
			t.Error(err)
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
	markSandboxFailurePhase(sandboxFailurePhasePublish)
	published := runExternalSandboxCLI(t, binary, cliEnv, "--quiet", "publish", backend.URL,
		"--id", namespace.ConnectorID)
	cridValue = strings.TrimSpace(published.stdout)
	if published.err != nil || cridValue == "" || strings.Contains(cridValue, "\n") {
		markSandboxFailureDiagnosticFromCommand(published.stdout, published.stderr, published.err)
		t.Fatalf("controlled-failure POSIX publish = stdout %q, stderr %q, error %v", published.stdout, published.stderr, published.err)
	}
	controlledFailureReached := false
	defer func() {
		if controlledFailureReached {
			return
		}
		inspection := runExternalSandboxCLI(t, binary, cliEnv, "-o", "json", "inspect", cridValue)
		markSandboxFailureDiagnosticFromCommand(inspection.stdout, inspection.stderr, inspection.err)
		markSandboxFailureDaemonStateFromCommand(inspection.stdout, inspection.err)
	}()
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

	// Each readiness check that stops the child names itself first: the parent
	// prints only the phase and these closed tokens.
	markSandboxFailurePhase(sandboxFailurePhaseReadiness)
	local, rowResult, err := externalSandboxShareRow(stateDir, cridValue, time.Minute)
	if err != nil {
		markSandboxFailureReadiness(sandboxFailureReadiness{
			Check: sandboxReadinessShareRow, Result: rowResult,
			First: sandboxReadinessNone, Second: sandboxReadinessNone,
		})
		t.Fatal(err)
	}
	jobWait, err := waitSandboxUserJobRunning(func() (connectorservice.ServiceStatus, error) {
		return jobManager.Status(connectordaemon.DaemonJobLabel)
	}, time.Minute, sandboxUserJobPoll, time.Now, time.Sleep)
	if err != nil {
		markSandboxFailureReadiness(jobWait.stopped())
		t.Fatalf("controlled-failure POSIX user job = %+v, %v; want installed and running", jobWait.Last, err)
	}
	if late, ok := jobWait.settledLate(); ok {
		markSandboxFailureReadiness(late)
	}
	initial, lastState, ok := externalSandboxState(func() externalCLIResult {
		return runExternalSandboxCLI(t, binary, cliEnv, "-o", "json", "status", cridValue)
	}, "on", "serving", time.Minute, externalSandboxStatePoll, time.Now, time.Sleep)
	if !ok {
		markSandboxFailureReadiness(lastState.readiness)
		t.Fatalf("timed out waiting for on/serving state for %s; last result: %s", cridValue, lastState.text)
	}
	markSandboxFailurePhase(sandboxFailurePhaseRoute)
	assertExternalSandboxRoute(t, binary, cliEnv, cridValue, marker, time.Minute)
	markSandboxFailurePhase(sandboxFailurePhaseStop)
	stopped := externalSandboxLifecycle(t, binary, cliEnv, "stop", cridValue)
	if err := validateSandboxSharingTransition(stopped, "off", "stopped", initial.ServingEpoch); err != nil {
		t.Fatalf("controlled-failure POSIX stop state = %+v: %v", stopped, err)
	}
	markSandboxFailurePhase(sandboxFailurePhaseFence)
	if err := controlledSandboxRouteFenceError(t, binary, cliEnv, stateDir, local.CRID, marker, &backendHits, 30*time.Second); err != nil {
		markSandboxFailureDiagnosticFromError(err)
		t.Fatal("controlled-failure route fence did not settle")
	}
	markSandboxFailurePhase(sandboxFailurePhaseStoppedGet)
	destination := filepath.Join(t.TempDir(), "fenced")
	failedGet := runExternalSandboxCLI(t, binary, cliEnv, "--quiet", "get", cridValue,
		"--file", destination)
	if err := validateSandboxStoppedDownloadResult(
		sandboxExternalExitCode(failedGet.err), failedGet.stdout, failedGet.stderr, destination, sandboxStoppedRouteRefusal(t),
	); err != nil {
		markSandboxFailureDiagnosticFromCommand(failedGet.stdout, failedGet.stderr, failedGet.err)
		t.Fatalf("controlled customer get did not return the exact stopped-Connector failure: %v", err)
	}
	controlledFailureReached = true
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
	share, _, err := externalSandboxShareRow(stateDir, cridValue, limit)
	if err != nil {
		t.Fatal(err)
	}
	return share
}

// externalSandboxShareRow waits for the registry row of one CRID. When it gives
// up, result is the closed readiness token for why.
func externalSandboxShareRow(stateDir, cridValue string, limit time.Duration) (share *connectorstate.LocalShare, result string, err error) {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		shares, present, readErr := connectorstate.ReadLocalSharesIfPresent(context.Background(), stateDir)
		if readErr != nil {
			return nil, sandboxReadinessReadFailed, fmt.Errorf("read macOS daemon share registry: %w", readErr)
		}
		if present {
			for i := range shares {
				if shares[i].CRID == cridValue {
					return &shares[i], "", nil
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return nil, sandboxReadinessTimeout, fmt.Errorf("timed out waiting for daemon registry row for %s", cridValue)
}

const sandboxUserJobPoll = 250 * time.Millisecond

var errSandboxUserJobNotRunning = errors.New("qURL user job is not installed and running")

// sandboxUserJobWait is what waitSandboxUserJobRunning saw: the first and the
// last answer of the job manager, and how long it asked.
type sandboxUserJobWait struct {
	First    connectorservice.ServiceStatus
	FirstErr error
	Last     connectorservice.ServiceStatus
	LastErr  error
	Samples  int
	Waited   time.Duration
}

// waitSandboxUserJobRunning asks the job manager until the job is installed
// and running, or until limit has passed. An answer of the manager is one
// sample of the service manager's state. On macOS an install loads the job,
// which starts it, and then asks launchd to restart it; the manager returns
// without checking that a process runs, and it reports a job as running only
// while launchd shows the running state together with a process ID. One sample
// can therefore fall between two processes of an installed job. The journey
// requires that the job is installed and comes to run, so the wait has a
// budget and fails with the last answer when the budget is spent.
func waitSandboxUserJobRunning(
	status func() (connectorservice.ServiceStatus, error),
	limit, poll time.Duration,
	now func() time.Time,
	sleep func(time.Duration),
) (sandboxUserJobWait, error) {
	var wait sandboxUserJobWait
	if status == nil || limit <= 0 || poll <= 0 || now == nil || sleep == nil {
		return wait, errors.New("user job wait configuration is invalid")
	}
	started := now()
	deadline := started.Add(limit)
	for {
		wait.Last, wait.LastErr = status()
		if wait.Samples == 0 {
			wait.First, wait.FirstErr = wait.Last, wait.LastErr
		}
		wait.Samples++
		wait.Waited = now().Sub(started)
		if wait.LastErr == nil && wait.Last.Installed && wait.Last.Running {
			return wait, nil
		}
		if !now().Before(deadline) {
			err := fmt.Errorf("%w after %s and %d answers", errSandboxUserJobNotRunning, limit, wait.Samples)
			if wait.LastErr != nil {
				err = fmt.Errorf("%w: last answer failed: %w", err, wait.LastErr)
			}
			return wait, err
		}
		sleep(poll)
	}
}

// stopped is the readiness record of a wait that failed: the last answer.
func (w *sandboxUserJobWait) stopped() sandboxFailureReadiness {
	result := sandboxReadinessNotReady
	if w.LastErr != nil {
		result = sandboxReadinessQueryFailed
	}
	return sandboxUserJobReadiness(result, w.Last)
}

// settledLate is the readiness record of a wait that passed after more than
// one answer: the first answer, which is the one that was not ready.
func (w *sandboxUserJobWait) settledLate() (sandboxFailureReadiness, bool) {
	if w.Samples < 2 {
		return sandboxFailureReadiness{}, false
	}
	result := sandboxReadinessDelayed
	if w.FirstErr != nil {
		result = sandboxReadinessDelayedQueryFailed
	}
	return sandboxUserJobReadiness(result, w.First), true
}

func sandboxUserJobReadiness(result string, status connectorservice.ServiceStatus) sandboxFailureReadiness {
	flag := func(set bool) string {
		if set {
			return sandboxReadinessYes
		}
		return sandboxReadinessNo
	}
	return sandboxFailureReadiness{
		Check: sandboxReadinessJobStatus, Result: result,
		First: flag(status.Installed), Second: flag(status.Running),
	}
}

// sandboxDaemonExitTimeout is the exit timeout of the daemon job definition
// (15 seconds, after which the service manager kills the process) and a margin.
const sandboxDaemonExitTimeout = 20 * time.Second

// waitSandboxDaemonExited returns when no daemon owns stateDir any more. On
// macOS the job manager returns from Remove while launchd is still stopping
// the process, and a stopping daemon still works in its state directory. The
// daemon takes this lease before it opens its registry and releases it when
// its run has returned; the system releases it if the process is killed. The
// control socket is not the sign to wait for: it closes when shutdown starts.
func waitSandboxDaemonExited(stateDir string, limit time.Duration) error {
	unlock, err := connectorstate.AcquireDaemonLeaseWithin(context.Background(), stateDir, limit)
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("qURL daemon did not release its state directory within %s of the removal of its job", limit)
	}
	if err != nil {
		return fmt.Errorf("check that the qURL daemon released its state directory: %w", err)
	}
	return unlock()
}

const (
	sandboxTempEntryNameLimit = 32
	sandboxTempEntryListLimit = 16
)

// registerSandboxTempDirRemoval removes the test's temp directories itself,
// as the last of the test's own cleanups, and fails the test with the names
// that are left when one cannot be removed. The test framework removes the
// same directories next; alone, it reports only that a directory was not
// empty. Call it before the test registers any other cleanup: the framework's
// removal is registered by the first t.TempDir, which this function makes, and
// cleanups run last-registered first.
func registerSandboxTempDirRemoval(t *testing.T) {
	t.Helper()
	registerSandboxTempDirRemovalWith(t, os.RemoveAll)
}

func registerSandboxTempDirRemovalWith(t *testing.T, remove func(string) error) {
	t.Helper()
	root := filepath.Dir(t.TempDir())
	t.Cleanup(func() {
		for _, line := range sandboxTempDirRemovalReport(root, remove) {
			t.Error(line)
		}
	})
}

// sandboxTempDirRemovalReport removes each directory below root and returns
// one line for each that remove could not remove.
func sandboxTempDirRemovalReport(root string, remove func(string) error) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var lines []string
	for _, entry := range entries {
		dir := filepath.Join(root, entry.Name())
		removeErr := remove(dir)
		if removeErr == nil {
			continue
		}
		names, total := sandboxTempDirLeftovers(dir)
		lines = append(lines, fmt.Sprintf("test temp directory %s could not be removed (%s); still present (%d): %s",
			sandboxTempEntryToken(entry.Name()), sandboxTempRemovalReason(removeErr), total, strings.Join(names, " ")))
	}
	return lines
}

func sandboxTempRemovalReason(err error) string {
	switch {
	case errors.Is(err, syscall.ENOTEMPTY):
		return "not_empty"
	case errors.Is(err, fs.ErrPermission):
		return "permission_denied"
	case errors.Is(err, syscall.EBUSY):
		return "busy"
	default:
		return "other"
	}
}

// sandboxTempDirLeftovers lists what is below dir: reduced base names, at most
// sandboxTempEntryListLimit of them, and the count of all entries. A directory
// that cannot be read counts as an entry and lists nothing below it.
func sandboxTempDirLeftovers(dir string) (names []string, total int) {
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		total++
		if len(names) < sandboxTempEntryListLimit {
			names = append(names, sandboxTempEntryToken(entry.Name()))
		}
		if !entry.IsDir() {
			continue
		}
		nested, count := sandboxTempDirLeftovers(filepath.Join(dir, entry.Name()))
		total += count
		names = append(names, nested[:min(len(nested), sandboxTempEntryListLimit-len(names))]...)
	}
	return names, total
}

// sandboxTempEntryToken reduces an entry name to what a public log may show:
// the base name, in a fixed character set, cut short. A cut name ends in "~".
func sandboxTempEntryToken(name string) string {
	name = filepath.Base(name)
	token := make([]byte, 0, sandboxTempEntryNameLimit+1)
	for index := 0; index < len(name); index++ {
		if len(token) == sandboxTempEntryNameLimit {
			return string(append(token, '~'))
		}
		character := name[index]
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9', character == '.', character == '-', character == '_':
		default:
			character = '_'
		}
		token = append(token, character)
	}
	return string(token)
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
	document, last, ok := externalSandboxState(func() externalCLIResult {
		return runExternalSandboxCLI(t, binary, env, "-o", "json", "status", cridValue)
	}, desired, observed, limit, externalSandboxStatePoll, time.Now, time.Sleep)
	if !ok {
		t.Fatalf("timed out waiting for %s/%s state for %s; last result: %s", desired, observed, cridValue, last.text)
	}
	return document
}

const externalSandboxStatePoll = 500 * time.Millisecond

// externalSandboxStateSample is one `qurl status` answer that was not the
// wanted state.
type externalSandboxStateSample struct {
	// text is the answer as the command printed it. It can hold private detail
	// and stays in the process that ran the command.
	text string
	// readiness is the same answer reduced to closed tokens.
	readiness sandboxFailureReadiness
}

// externalSandboxState asks for the sharing state until it is the wanted one
// or limit has passed, and then returns the last answer.
func externalSandboxState(
	status func() externalCLIResult,
	desired, observed string,
	limit, poll time.Duration,
	now func() time.Time,
	sleep func(time.Duration),
) (sandboxSharingDoc, externalSandboxStateSample, bool) {
	last := externalSandboxStateSample{readiness: sandboxFailureReadiness{
		Check: sandboxReadinessSharingState, Result: sandboxReadinessNotSampled,
		First: sandboxReadinessNone, Second: sandboxReadinessNone,
	}}
	deadline := now().Add(limit)
	for now().Before(deadline) {
		document, sample, decoded := sandboxSharingStateSample(status())
		if decoded && document.DesiredState == desired && document.ConnectionState == observed {
			return document, externalSandboxStateSample{}, true
		}
		last = sample
		sleep(poll)
	}
	return sandboxSharingDoc{}, last, false
}

func sandboxSharingStateSample(result externalCLIResult) (document sandboxSharingDoc, sample externalSandboxStateSample, decoded bool) {
	sample.readiness = sandboxFailureReadiness{
		Check: sandboxReadinessSharingState, Result: sandboxReadinessCommandFailed,
		First: sandboxReadinessNone, Second: sandboxReadinessNone,
	}
	if result.err != nil {
		sample.text = result.stderr
		return sandboxSharingDoc{}, sample, false
	}
	if err := json.Unmarshal([]byte(result.stdout), &document); err != nil {
		sample.text = err.Error()
		sample.readiness.Result = sandboxReadinessDecodeFailed
		return sandboxSharingDoc{}, sample, false
	}
	sample.text = result.stdout
	sample.readiness.Result = sandboxReadinessStateMismatch
	sample.readiness.First = sandboxReadinessStateToken(document.DesiredState, "on", "off")
	sample.readiness.Second = sandboxReadinessStateToken(document.ConnectionState, "serving", "connecting", "stopped")
	return document, sample, true
}

// sandboxReadinessStateToken keeps a state the product is known to print and
// replaces any other text by a fixed word.
func sandboxReadinessStateToken(value string, known ...string) string {
	switch {
	case value == "":
		return sandboxReadinessNone
	case slices.Contains(known, value):
		return value
	default:
		return sandboxReadinessOther
	}
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

func assertExternalSandboxDeleted(
	t *testing.T,
	binary string,
	env map[string]string,
	cridValue string,
) {
	t.Helper()
	downloadDir := t.TempDir()
	destination := filepath.Join(downloadDir, "deleted-payload")
	for _, check := range []struct {
		name string
		args []string
	}{
		{name: "share", args: []string{"share", cridValue}},
		{name: "get", args: []string{"get", cridValue, "--file", destination}},
		{name: "status", args: []string{"status", cridValue}},
	} {
		result := runExternalSandboxCLI(t, binary, env, check.args...)
		if err := validateSandboxDeletedCommandResult(
			check.name,
			sandboxExternalExitCode(result.err),
			result.stdout,
			result.stderr,
		); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(downloadDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("get after Connector delete left %d download artifacts: %v", len(entries), err)
	}
}

func TestSandboxUserJobWait(t *testing.T) {
	type answer struct {
		status connectorservice.ServiceStatus
		err    error
	}
	const poll = 250 * time.Millisecond
	ready := answer{status: connectorservice.ServiceStatus{Installed: true, Running: true}}
	// wait runs the wait on a clock that only sleep moves. The last answer
	// repeats, so a wait that is never satisfied ends on its budget alone.
	wait := func(limit time.Duration, answers ...answer) (result sandboxUserJobWait, slept time.Duration, err error) {
		clock := time.Unix(0, 0)
		asked := 0
		result, err = waitSandboxUserJobRunning(func() (connectorservice.ServiceStatus, error) {
			next := answers[min(asked, len(answers)-1)]
			asked++
			return next.status, next.err
		}, limit, poll, func() time.Time { return clock }, func(pause time.Duration) {
			clock = clock.Add(pause)
			slept += pause
		})
		if result.Samples != asked {
			t.Fatalf("wait counted %d answers of %d", result.Samples, asked)
		}
		return result, slept, err
	}

	t.Run("returns on the first good answer", func(t *testing.T) {
		result, slept, err := wait(time.Minute, ready)
		if err != nil || result.Samples != 1 || slept != 0 || result.Last != ready.status {
			t.Fatalf("first good answer = %+v, %v after sleeping %s", result, err, slept)
		}
		if late, ok := result.settledLate(); ok {
			t.Fatalf("a job that was ready when first asked was reported late: %#v", late)
		}
	})
	t.Run("waits through answers that are not ready", func(t *testing.T) {
		result, slept, err := wait(time.Minute,
			answer{},
			answer{status: connectorservice.ServiceStatus{Installed: true}},
			ready,
		)
		if err != nil || result.Samples != 3 || slept != 2*poll || result.Waited != 2*poll ||
			result.First != (connectorservice.ServiceStatus{}) || result.Last != ready.status {
			t.Fatalf("late good answer = %+v, %v after sleeping %s", result, err, slept)
		}
		late, ok := result.settledLate()
		want := sandboxFailureReadiness{Check: sandboxReadinessJobStatus, Result: sandboxReadinessDelayed, First: sandboxReadinessNo, Second: sandboxReadinessNo}
		if !ok || late != want || !validSandboxFailureReadiness(late) {
			t.Fatalf("late readiness record = %#v, %t; want %#v", late, ok, want)
		}
	})
	t.Run("keeps asking after an answer that failed", func(t *testing.T) {
		const private = "launchctl print gui/501/private-label: exit status 5"
		result, _, err := wait(time.Minute,
			answer{status: connectorservice.ServiceStatus{Installed: true}, err: errors.New(private)},
			ready,
		)
		if err != nil || result.Samples != 2 || result.LastErr != nil {
			t.Fatalf("good answer after a failed one = %+v, %v", result, err)
		}
		late, ok := result.settledLate()
		want := sandboxFailureReadiness{Check: sandboxReadinessJobStatus, Result: sandboxReadinessDelayedQueryFailed, First: sandboxReadinessYes, Second: sandboxReadinessNo}
		if !ok || late != want || !validSandboxFailureReadiness(late) {
			t.Fatalf("late readiness record = %#v, %t; want %#v", late, ok, want)
		}
		var written strings.Builder
		writeSandboxFailureReadiness(&written, late)
		if strings.Contains(written.String(), "launchctl") || strings.Contains(written.String(), "private-label") {
			t.Fatalf("readiness record carried the failed answer: %q", written.String())
		}
	})
	t.Run("fails with the last answer when the budget is spent", func(t *testing.T) {
		result, slept, err := wait(time.Second,
			answer{},
			answer{status: connectorservice.ServiceStatus{Installed: true}},
		)
		// Asked at 0, 250, 500, 750 and 1000 ms: the last answer is the one at
		// the end of the budget, and no sleep follows it.
		if !errors.Is(err, errSandboxUserJobNotRunning) || result.Samples != 5 || slept != time.Second ||
			result.Last != (connectorservice.ServiceStatus{Installed: true}) || result.LastErr != nil {
			t.Fatalf("spent budget = %+v, %v after sleeping %s", result, err, slept)
		}
		want := sandboxFailureReadiness{Check: sandboxReadinessJobStatus, Result: sandboxReadinessNotReady, First: sandboxReadinessYes, Second: sandboxReadinessNo}
		if got := result.stopped(); got != want || !validSandboxFailureReadiness(got) {
			t.Fatalf("stopped readiness record = %#v, want %#v", got, want)
		}
	})
	t.Run("fails with the last failed answer when the budget is spent", func(t *testing.T) {
		cause := errors.New("launchctl print gui/501/private-label: exit status 5")
		result, _, err := wait(time.Second, answer{status: connectorservice.ServiceStatus{Installed: true, Running: true}, err: cause})
		if !errors.Is(err, errSandboxUserJobNotRunning) || !errors.Is(err, cause) || !errors.Is(result.LastErr, cause) {
			t.Fatalf("spent budget on failed answers = %+v, %v", result, err)
		}
		// An answer that failed is not ready, whatever flags came with it.
		want := sandboxFailureReadiness{Check: sandboxReadinessJobStatus, Result: sandboxReadinessQueryFailed, First: sandboxReadinessYes, Second: sandboxReadinessYes}
		if got := result.stopped(); got != want || !validSandboxFailureReadiness(got) {
			t.Fatalf("stopped readiness record = %#v, want %#v", got, want)
		}
	})
	t.Run("refuses a wait it cannot bound", func(t *testing.T) {
		status := func() (connectorservice.ServiceStatus, error) { return ready.status, nil }
		for name, run := range map[string]func() (sandboxUserJobWait, error){
			"no status": func() (sandboxUserJobWait, error) {
				return waitSandboxUserJobRunning(nil, time.Second, poll, time.Now, time.Sleep)
			},
			"no budget": func() (sandboxUserJobWait, error) {
				return waitSandboxUserJobRunning(status, 0, poll, time.Now, time.Sleep)
			},
			"no pause": func() (sandboxUserJobWait, error) {
				return waitSandboxUserJobRunning(status, time.Second, 0, time.Now, time.Sleep)
			},
		} {
			if result, err := run(); err == nil || result.Samples != 0 {
				t.Errorf("%s: wait = %+v, %v; want a refusal before the first answer", name, result, err)
			}
		}
	})
}

func TestSandboxShareRowWaitNamesWhyItGaveUp(t *testing.T) {
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	row := localShareFixture(apitest.NewServer(t))
	if err := registry.Put(context.Background(), &row); err != nil {
		t.Fatal(err)
	}
	gaveUp := func(result string) sandboxFailureReadiness {
		return sandboxFailureReadiness{
			Check: sandboxReadinessShareRow, Result: result,
			First: sandboxReadinessNone, Second: sandboxReadinessNone,
		}
	}

	share, result, err := externalSandboxShareRow(stateDir, row.CRID, time.Minute)
	if err != nil || result != "" || share == nil || share.CRID != row.CRID || share.ResourceID != row.ResourceID {
		t.Fatalf("row that is present = %+v, %q, %v", share, result, err)
	}

	// The budget ends during the first pause, so the row is asked for once.
	share, result, err = externalSandboxShareRow(stateDir, "crid-of-no-row", time.Millisecond)
	if err == nil || share != nil || result != sandboxReadinessTimeout || !validSandboxFailureReadiness(gaveUp(result)) {
		t.Fatalf("row that never appears = %+v, %q, %v", share, result, err)
	}

	// A registry that cannot be read ends the wait; it is not waited out.
	if err := os.WriteFile(filepath.Join(stateDir, connectorstate.LocalSharesFile), []byte("not a registry"), 0o600); err != nil {
		t.Fatal(err)
	}
	share, result, err = externalSandboxShareRow(stateDir, row.CRID, 5*time.Second)
	if err == nil || share != nil || result != sandboxReadinessReadFailed || !validSandboxFailureReadiness(gaveUp(result)) {
		t.Fatalf("registry that cannot be read = %+v, %q, %v", share, result, err)
	}
}

func TestSandboxStateWaitReportsLastAnswerAsClosedTokens(t *testing.T) {
	const secret = "lv_test_captured_status_secret"
	const poll = 500 * time.Millisecond
	// wait runs the wait on a clock that only sleep moves; the last answer repeats.
	wait := func(limit time.Duration, answers ...externalCLIResult) (sandboxSharingDoc, externalSandboxStateSample, bool, int) {
		clock := time.Unix(0, 0)
		asked := 0
		document, last, ok := externalSandboxState(func() externalCLIResult {
			next := answers[min(asked, len(answers)-1)]
			asked++
			return next
		}, "on", "serving", limit, poll, func() time.Time { return clock }, func(pause time.Duration) { clock = clock.Add(pause) })
		return document, last, ok, asked
	}
	state := func(desired, observed string) externalCLIResult {
		raw, err := json.Marshal(sandboxSharingDoc{CRID: "crid-" + secret, DesiredState: desired, ConnectionState: observed, ServingEpoch: 7})
		if err != nil {
			t.Fatal(err)
		}
		return externalCLIResult{stdout: string(raw)}
	}

	t.Run("returns the first wanted state", func(t *testing.T) {
		document, _, ok, asked := wait(time.Minute, state("on", "connecting"), state("on", "serving"))
		if !ok || asked != 2 || document.ConnectionState != "serving" || document.ServingEpoch != 7 {
			t.Fatalf("wanted state = %+v, %t after %d answers", document, ok, asked)
		}
	})
	for _, test := range []struct {
		name   string
		last   externalCLIResult
		result string
		first  string
		second string
	}{
		{name: "another state", last: state("on", "connecting"), result: sandboxReadinessStateMismatch, first: "on", second: "connecting"},
		{name: "stopped", last: state("off", "stopped"), result: sandboxReadinessStateMismatch, first: "off", second: "stopped"},
		{name: "text outside the known states", last: state(secret, "::error::"+secret), result: sandboxReadinessStateMismatch, first: sandboxReadinessOther, second: sandboxReadinessOther},
		{name: "no state", last: externalCLIResult{stdout: "{}"}, result: sandboxReadinessStateMismatch, first: sandboxReadinessNone, second: sandboxReadinessNone},
		{name: "failed command", last: externalCLIResult{stdout: state("on", "serving").stdout, stderr: secret, err: errors.New(secret)}, result: sandboxReadinessCommandFailed, first: sandboxReadinessNone, second: sandboxReadinessNone},
		{name: "unreadable answer", last: externalCLIResult{stdout: secret}, result: sandboxReadinessDecodeFailed, first: sandboxReadinessNone, second: sandboxReadinessNone},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, last, ok, asked := wait(time.Second, state("on", "connecting"), test.last)
			// Asked at 0 and 500 ms; the budget ends before a third answer.
			if ok || asked != 2 {
				t.Fatalf("wait = %t after %d answers, want a spent budget after 2", ok, asked)
			}
			want := sandboxFailureReadiness{Check: sandboxReadinessSharingState, Result: test.result, First: test.first, Second: test.second}
			if last.readiness != want || !validSandboxFailureReadiness(last.readiness) {
				t.Fatalf("last answer = %#v, want %#v", last.readiness, want)
			}
			var written strings.Builder
			writeSandboxFailureReadiness(&written, last.readiness)
			if strings.Contains(written.String(), secret) || strings.Contains(written.String(), "::") {
				t.Fatalf("readiness record carried text of the answer: %q", written.String())
			}
		})
	}
	t.Run("no answer inside the budget", func(t *testing.T) {
		_, last, ok, asked := wait(0, state("on", "serving"))
		want := sandboxFailureReadiness{Check: sandboxReadinessSharingState, Result: sandboxReadinessNotSampled, First: sandboxReadinessNone, Second: sandboxReadinessNone}
		if ok || asked != 0 || last.readiness != want || !validSandboxFailureReadiness(last.readiness) {
			t.Fatalf("wait without a budget = %#v, %t after %d answers", last.readiness, ok, asked)
		}
	})
}

func TestSandboxDaemonExitWaitIsBounded(t *testing.T) {
	stateDir := connectorStateTestDir(t)
	if err := waitSandboxDaemonExited(stateDir, time.Second); err != nil {
		t.Fatalf("state directory no daemon owns: %v", err)
	}

	// Stand in for a daemon: hold the lease a daemon holds while it runs.
	release, err := connectorstate.AcquireDaemonLease(context.Background(), stateDir)
	if err != nil {
		t.Fatalf("hold the daemon lease: %v", err)
	}
	released := false
	defer func() {
		if !released {
			_ = release()
		}
	}()
	err = waitSandboxDaemonExited(stateDir, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "did not release its state directory within 50ms") || strings.Contains(err.Error(), stateDir) {
		t.Fatalf("wait under a held lease = %v, want the bound and no path", err)
	}

	done := make(chan error, 1)
	go func() { done <- waitSandboxDaemonExited(stateDir, time.Minute) }()
	select {
	case err := <-done:
		t.Fatalf("wait returned %v while the lease was held", err)
	case <-time.After(100 * time.Millisecond):
	}
	released = true
	if err := release(); err != nil {
		t.Fatalf("release the daemon lease: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("wait after the lease was released: %v", err)
	}
	// The wait must leave the lease free for the next owner.
	if err := waitSandboxDaemonExited(stateDir, time.Second); err != nil {
		t.Fatalf("state directory after the wait: %v", err)
	}
}

func TestSandboxTempDirRemovalReport(t *testing.T) {
	t.Run("names are base names in a fixed character set and cut", func(t *testing.T) {
		reduced := regexp.MustCompile(`^[A-Za-z0-9._-]*~?$`)
		for name, want := range map[string]string{
			"payload":                   "payload",
			".connector_resources.lock": ".connector_resources.lock",
			".local_shares.json.tmp-abcdefabcdefabcd": ".local_shares.json.tmp-abcdefabc~",
			"exactly-thirty-two-characters-xx":        "exactly-thirty-two-characters-xx",
			"thirty-three-characters-long-name":       "thirty-three-characters-long-nam~",
			"a b\nc\td":                               "a_b_c_d",
			"::error title=forged::message":           "__error_title_forged__message",
			"\x1b[31mred\x1b[0m":                      "__31mred__0m",
			"/var/private/directory/secret-file":      "secret-file",
			"caf\u00e9":                               "caf__",
			"":                                        ".",
		} {
			got := sandboxTempEntryToken(name)
			if got != want {
				t.Errorf("sandboxTempEntryToken(%q) = %q, want %q", name, got, want)
			}
			if len(got) > sandboxTempEntryNameLimit+1 || !reduced.MatchString(got) {
				t.Errorf("sandboxTempEntryToken(%q) = %q is longer than the limit or outside the character set", name, got)
			}
		}
	})
	t.Run("removes what can be removed and says nothing", func(t *testing.T) {
		root := t.TempDir()
		for _, path := range []string{"001/deployment.json", "002/state/nested/file", "003/payload"} {
			writeSandboxTempFixture(t, filepath.Join(root, path))
		}
		if lines := sandboxTempDirRemovalReport(root, os.RemoveAll); len(lines) != 0 {
			t.Fatalf("report for removable directories = %q", lines)
		}
		if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
			t.Fatalf("root after removal holds %d entries: %v", len(entries), err)
		}
	})
	t.Run("lists what is left in a directory that cannot be removed", func(t *testing.T) {
		root := t.TempDir()
		writeSandboxTempFixture(t, filepath.Join(root, "001", "deployment.json"))
		writeSandboxTempFixture(t, filepath.Join(root, "002", "agent-state.json"))
		lateName := "written by a late process\n::error::forged"
		late := filepath.Join(root, "002", "nested", lateName)
		// Stand in for a process that writes while its directory is removed:
		// the first entries go, another appears, and the directory stays.
		remove := func(dir string) error {
			if filepath.Base(dir) != "002" {
				return os.RemoveAll(dir)
			}
			if err := os.Remove(filepath.Join(dir, "agent-state.json")); err != nil {
				return err
			}
			writeSandboxTempFixture(t, late)
			return &os.PathError{Op: "unlinkat", Path: dir, Err: syscall.ENOTEMPTY}
		}
		lines := sandboxTempDirRemovalReport(root, remove)
		want := []string{"test temp directory 002 could not be removed (not_empty); still present (2): nested written_by_a_late_process___erro~"}
		if !slices.Equal(lines, want) {
			t.Fatalf("report = %q, want %q", lines, want)
		}
		if strings.Contains(lines[0], root) || strings.ContainsAny(lines[0], "\n/") || strings.Contains(lines[0], "::") {
			t.Fatalf("report carried a path or a raw name: %q", lines[0])
		}
	})
	t.Run("lists only the first names and counts the rest", func(t *testing.T) {
		root := t.TempDir()
		for index := range 10 {
			writeSandboxTempFixture(t, filepath.Join(root, "001", fmt.Sprintf("entry-%02d", index)))
			writeSandboxTempFixture(t, filepath.Join(root, "001", "nested", fmt.Sprintf("entry-%02d", 10+index)))
		}
		for index := range 20 {
			writeSandboxTempFixture(t, filepath.Join(root, "002", fmt.Sprintf("flat-%02d", index)))
		}
		lines := sandboxTempDirRemovalReport(root, func(string) error { return syscall.EBUSY })
		// Ten files, the nested directory and its ten files are 21 entries. A
		// directory is listed before what is in it.
		want := []string{
			"test temp directory 001 could not be removed (busy); still present (21): " +
				"entry-00 entry-01 entry-02 entry-03 entry-04 entry-05 entry-06 entry-07 entry-08 entry-09 " +
				"nested entry-10 entry-11 entry-12 entry-13 entry-14",
			"test temp directory 002 could not be removed (busy); still present (20): " +
				"flat-00 flat-01 flat-02 flat-03 flat-04 flat-05 flat-06 flat-07 " +
				"flat-08 flat-09 flat-10 flat-11 flat-12 flat-13 flat-14 flat-15",
		}
		if !slices.Equal(lines, want) || sandboxTempEntryListLimit != 16 {
			t.Fatalf("report = %q, want %q", lines, want)
		}
	})
	t.Run("reasons are a closed set", func(t *testing.T) {
		for want, err := range map[string]error{
			"not_empty":         &os.PathError{Op: "unlinkat", Path: "/private/path", Err: syscall.ENOTEMPTY},
			"permission_denied": &os.PathError{Op: "unlinkat", Path: "/private/path", Err: syscall.EACCES},
			"busy":              fmt.Errorf("wrapped: %w", syscall.EBUSY),
			"other":             errors.New("/private/path: any other cause"),
		} {
			if got := sandboxTempRemovalReason(err); got != want {
				t.Errorf("sandboxTempRemovalReason(%v) = %q, want %q", err, got, want)
			}
		}
	})
	t.Run("a root that is gone has nothing to report", func(t *testing.T) {
		if lines := sandboxTempDirRemovalReport(filepath.Join(t.TempDir(), "absent"), os.RemoveAll); lines != nil {
			t.Fatalf("report for an absent root = %q", lines)
		}
	})
	t.Run("removes the directories itself, before the framework would", func(t *testing.T) {
		left := -1
		t.Run("journey", func(t *testing.T) {
			// Registered after the framework's removal and before the removal
			// under test, so it runs between the two.
			root := filepath.Dir(t.TempDir())
			t.Cleanup(func() {
				if entries, err := os.ReadDir(root); err == nil {
					left = len(entries)
				}
			})
			registerSandboxTempDirRemoval(t)
			writeSandboxTempFixture(t, filepath.Join(t.TempDir(), "state"))
		})
		if left != 0 {
			t.Fatalf("temp directories left for the framework = %d, want 0", left)
		}
	})
	t.Run("removes every temp directory after the test's other cleanups", func(t *testing.T) {
		var root string
		var order []string
		t.Run("journey", func(t *testing.T) {
			registerSandboxTempDirRemovalWith(t, func(dir string) error {
				order = append(order, "remove "+filepath.Base(dir))
				return os.RemoveAll(dir)
			})
			dir := t.TempDir()
			root = filepath.Dir(dir)
			file := filepath.Join(dir, "state")
			writeSandboxTempFixture(t, file)
			t.Cleanup(func() {
				if _, err := os.Stat(file); err == nil {
					order = append(order, "later cleanup found its file")
				}
			})
		})
		// 001 is the directory the registration made; 002 came after it.
		if want := []string{"later cleanup found its file", "remove 001", "remove 002"}; !slices.Equal(order, want) {
			t.Fatalf("cleanup order = %q, want %q", order, want)
		}
		if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("temp root after the test: %v, want it removed", err)
		}
	})
}

func writeSandboxTempFixture(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
}
