//go:build clisandbox && darwin

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	connectorservice "github.com/layervai/qurl-connector/pkg/service"

	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

const macOSDaemonSandboxArming = "QURL_CLI_SANDBOX_MACOS_DAEMON"

// TestSandboxMacOSDefaultDaemonLifecycle is the executable contract for the
// customer-default background path. Unlike the portable foreground sandbox
// journey, this test invokes the exact candidate binary and real launchd.
func TestSandboxMacOSDefaultDaemonLifecycle(t *testing.T) {
	if os.Getenv(macOSDaemonSandboxArming) != "enabled" {
		t.Skipf("SKIPPED LOUDLY: macOS qURL daemon sandbox journey is disarmed — %s != enabled", macOSDaemonSandboxArming)
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
	cleanupJWT := sandboxSecret(t, "QURL_CLI_SANDBOX_CLEANUP_JWT")
	for _, name := range []string{hub.EnvHost, hub.EnvPort, hub.EnvServerPublicKey} {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			t.Fatalf("macOS daemon sandbox journey requires %s", name)
		}
		cliEnv[name] = value
	}
	stateDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve canonical macOS state directory: %v", err)
	}
	cliEnv[connectorstate.EnvStateDirPrimary] = stateDir
	cliEnv["PATH"] = filepath.Dir(binary) + string(os.PathListSeparator) + os.Getenv("PATH")

	jobManager := connectorservice.NewUserJobManager()
	if err := jobManager.Remove(connectordaemon.LaunchAgentLabel); err != nil {
		t.Fatalf("remove pre-existing qURL LaunchAgent: %v", err)
	}
	t.Cleanup(func() {
		if err := jobManager.Remove(connectordaemon.LaunchAgentLabel); err != nil {
			t.Errorf("remove qURL LaunchAgent after journey: %v", err)
		}
	})

	marker := fmt.Sprintf("sandbox-macos-daemon-%d", time.Now().UnixNano())
	var backendHits atomic.Uint64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHits.Add(1)
		_, _ = io.WriteString(w, marker)
	}))
	defer backend.Close()

	connectorID := fmt.Sprintf("sandbox-macos-daemon-%d", time.Now().UnixNano())
	publish := runExternalSandboxCLI(t, binary, cliEnv, "--quiet", "publish", backend.URL, "--id", connectorID)
	cridValue := strings.TrimSpace(publish.stdout)
	if publish.err != nil || cridValue == "" || strings.Contains(cridValue, "\n") {
		t.Fatalf("default background publish = stdout %q, stderr %q, error %v", publish.stdout, publish.stderr, publish.err)
	}

	local := waitExternalSandboxShare(t, stateDir, cridValue, 2*time.Minute)
	loaded := loadSandboxAgentState(t, stateDir)
	if loaded != nil {
		registerSandboxDeviceCredentialCleanup(t, cliEnv["QURL_ENDPOINT"], cleanupJWT, loaded.DeviceAPIKeyID)
		registerSandboxResourceCleanup(t, cliEnv["QURL_ENDPOINT"], connectorID, loaded.DeviceAPIKey)
	}
	if err := requireTestResourceIdentity(local.CRID, local.ResourceID); err != nil {
		t.Fatalf("sandbox minted a non-test CRID: %v", err)
	}

	status, err := jobManager.Status(connectordaemon.LaunchAgentLabel)
	if err != nil || !status.Installed || !status.Running {
		t.Fatalf("qURL LaunchAgent after publish = %+v, %v; want installed and running", status, err)
	}
	assertMacOSLaunchAgentContainsNoCredential(t, cliEnv["QURL_ENDPOINT"], cliEnv[hub.EnvHost], cliEnv[hub.EnvServerPublicKey], cliEnv["QURL_API_KEY"], cleanupJWT)

	initial := waitExternalSandboxState(t, binary, cliEnv, cridValue, "on", "serving", 2*time.Minute)
	assertExternalSandboxRoute(t, binary, cliEnv, cridValue, marker, 2*time.Minute)

	stopped := externalSandboxLifecycle(t, binary, cliEnv, "stop", cridValue)
	if stopped.DesiredState != "off" || stopped.ConnectionState != "stopped" || stopped.ServingEpoch <= initial.ServingEpoch {
		t.Fatalf("stop state = %+v, want off/stopped at an advanced epoch", stopped)
	}
	started := externalSandboxLifecycle(t, binary, cliEnv, "start", cridValue)
	if started.DesiredState != "on" || started.ConnectionState != "serving" || started.ServingEpoch < stopped.ServingEpoch {
		t.Fatalf("start state = %+v, want on/serving without epoch regression", started)
	}
	assertExternalSandboxRoute(t, binary, cliEnv, cridValue, marker, 2*time.Minute)
	restarted := externalSandboxLifecycle(t, binary, cliEnv, "restart", cridValue)
	if restarted.DesiredState != "on" || restarted.ConnectionState != "serving" || restarted.ServingEpoch <= started.ServingEpoch {
		t.Fatalf("restart state = %+v, want on/serving at an advanced epoch", restarted)
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
	for _, share := range shares {
		if share.CRID == cridValue {
			t.Fatalf("deleted CRID %s remains in local daemon registry", cridValue)
		}
	}
}

type externalCLIResult struct {
	stdout string
	stderr string
	err    error
}

func runExternalSandboxCLI(t *testing.T, binary string, env map[string]string, args ...string) externalCLIResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	commandArgs := append([]string{"--endpoint", env["QURL_ENDPOINT"]}, args...)
	cmd := exec.CommandContext(ctx, binary, commandArgs...)
	cmd.Env = externalSandboxEnvironment(env)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return externalCLIResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

func externalSandboxEnvironment(overrides map[string]string) []string {
	values := map[string]string{}
	for _, entry := range os.Environ() {
		if key, value, ok := strings.Cut(entry, "="); ok {
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
	t.Fatalf("timed out waiting for macOS daemon registry row for %s", cridValue)
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
			payload, err := os.ReadFile(destination)
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

func assertMacOSLaunchAgentContainsNoCredential(t *testing.T, endpoint, hubHost, hubKey, apiKey, cleanupJWT string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	path, err := connectorservice.UserJobPlistPath(home, connectordaemon.LaunchAgentLabel)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read qURL LaunchAgent definition: %v", err)
	}
	definition := string(raw)
	for _, expected := range []string{endpoint, hubHost, hubKey} {
		if !strings.Contains(definition, expected) {
			t.Fatalf("qURL LaunchAgent omitted required non-secret deployment identity")
		}
	}
	for _, secret := range []string{apiKey, cleanupJWT} {
		if secret != "" && strings.Contains(definition, secret) {
			t.Fatal("qURL LaunchAgent persisted a bearer credential")
		}
	}
}
