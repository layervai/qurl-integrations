//go:build clisandbox && clisoak

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	qurl "github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

const (
	localPublishSoakArming   = "QURL_CLI_SANDBOX_LOCAL_PUBLISH_SOAK"
	localPublishSoakDuration = "QURL_CLI_SANDBOX_SOAK_DURATION"
	localPublishSoakMin      = 75 * time.Minute
	localPublishSoakMax      = 90 * time.Minute
)

// TestSandboxLocalPublishSoak performs a short cold cycle so cleanup authority
// is durably registered, then immediately restarts the same device/resource
// for the requested 75-90 minute interval. Any autonomous tunnel exit,
// exact-session retirement failure, identity change, or missing route-ready
// evidence fails the selected protected lane.
func TestSandboxLocalPublishSoak(t *testing.T) {
	if os.Getenv(localPublishSoakArming) != "enabled" {
		t.Skipf("SKIPPED LOUDLY: unified local-publish soak is disarmed — %s != enabled", localPublishSoakArming)
	}
	soak, err := time.ParseDuration(os.Getenv(localPublishSoakDuration))
	if err != nil || soak < localPublishSoakMin || soak > localPublishSoakMax || soak%time.Second != 0 {
		t.Fatalf("%s must be a whole-second duration from %s through %s", localPublishSoakDuration, localPublishSoakMin, localPublishSoakMax)
	}
	key, err := readSandboxSecretFile(sandboxAPIKeyFileEnv, "QURL_API_KEY")
	if err != nil {
		t.Fatalf("load protected sandbox API key: %v", err)
	}
	cleanupJWT, err := readSandboxSecretFile(sandboxCleanupJWTFileEnv, "QURL_CLI_SANDBOX_CLEANUP_JWT")
	if err != nil {
		t.Fatalf("load protected sandbox cleanup JWT: %v", err)
	}
	namespace, err := sandboxNamespace("soak")
	if err != nil {
		t.Fatalf("derive sandbox soak namespace: %v", err)
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
		t.Fatalf("unified local-publish soak is armed but missing %v", missing)
	}

	stateDir := t.TempDir()
	t.Setenv(state.EnvStateDirPrimary, stateDir)
	t.Setenv(state.EnvAgentID, namespace.AgentID)
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "sandbox-local-publish-soak")
	}))
	t.Cleanup(echo.Close)

	var loaded *qurl.AgentState
	cold := runSandboxLocalPublishCycle(t, "soak cold lifecycle", key, cleanupJWT, endpoint, echo.URL, namespace.ConnectorID, time.Second, func() {
		loaded = loadSandboxAgentState(t, stateDir)
		if loaded != nil {
			registerSandboxDeviceCredentialCleanup(t, endpoint, cleanupJWT, loaded.DeviceAPIKeyID)
			registerSandboxResourceCleanup(t, endpoint, namespace.ConnectorID, loaded.DeviceAPIKey)
		}
	})
	if loaded == nil {
		t.Fatal("soak cold lifecycle produced no durable device state")
	}
	if err := validateSandboxDeviceIdentity(loaded, namespace.AgentID, ""); err != nil {
		t.Fatalf("soak cold lifecycle identity: %v", err)
	}

	warm := runSandboxLocalPublishCycle(t, "soak warm lifecycle", key, cleanupJWT, endpoint, echo.URL, namespace.ConnectorID, soak, nil)
	if strings.TrimSpace(warm.stdout.String()) != strings.TrimSpace(cold.stdout.String()) {
		t.Fatalf("soak warm lifecycle changed the stable Connector CRID")
	}
	reloaded := loadSandboxAgentState(t, stateDir)
	if err := validateSandboxDeviceIdentity(reloaded, namespace.AgentID, loaded.DeviceAPIKeyID); err != nil {
		t.Fatalf("soak warm lifecycle identity: %v", err)
	}
}

func TestSandboxLocalPublishSoakDurationContract(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{"75m", true}, {"80m", true}, {"90m", true},
		{"74m59s", false}, {"90m1s", false}, {"1.5h", true}, {"bad", false},
	} {
		d, err := time.ParseDuration(tc.value)
		valid := err == nil && d >= localPublishSoakMin && d <= localPublishSoakMax && d%time.Second == 0
		if valid != tc.valid {
			t.Fatalf("duration %q validity = %t, want %t", tc.value, valid, tc.valid)
		}
	}
}
