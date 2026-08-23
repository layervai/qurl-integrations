//go:build clisandbox && clisoak

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// TestSandboxLocalPublishSoak performs a short cold cycle so device-key resource
// cleanup is durably registered, then immediately restarts the same device and
// resource for the requested 75-90 minute interval. Final device-credential
// revocation belongs to the protected host after it refreshes Auth0 authority.
// Any autonomous tunnel exit,
// exact-session retirement failure, identity change, or missing route-ready
// evidence fails the selected protected lane.
func TestSandboxLocalPublishSoak(t *testing.T) {
	if os.Getenv(localPublishSoakArming) != "enabled" {
		t.Skipf("SKIPPED LOUDLY: unified local-publish soak is disarmed — %s != enabled", localPublishSoakArming)
	}
	if err := validateSandboxSoakCredentialBoundary(); err != nil {
		t.Fatal(err)
	}
	soak, err := time.ParseDuration(os.Getenv(localPublishSoakDuration))
	if err != nil || soak < localPublishSoakMin || soak > localPublishSoakMax || soak%time.Second != 0 {
		t.Fatalf("%s must be a whole-second duration from %s through %s", localPublishSoakDuration, localPublishSoakMin, localPublishSoakMax)
	}
	key, err := readSandboxSecretFile(sandboxAPIKeyFileEnv, "QURL_API_KEY")
	if err != nil {
		t.Fatalf("load protected sandbox API key: %v", err)
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
	cold := runSandboxLocalPublishCycle(t, "soak cold lifecycle", key, "", endpoint, echo.URL, namespace.ConnectorID, time.Second, func() {
		loaded = registerSandboxSoakResourceCleanup(t, endpoint, namespace.ConnectorID, stateDir)
	})
	if loaded == nil {
		t.Fatal("soak cold lifecycle produced no durable device state")
	}
	if err := validateSandboxDeviceIdentity(loaded, namespace.AgentID, ""); err != nil {
		t.Fatalf("soak cold lifecycle identity: %v", err)
	}

	warm := runSandboxLocalPublishCycle(t, "soak warm lifecycle", key, "", endpoint, echo.URL, namespace.ConnectorID, soak, nil)
	if strings.TrimSpace(warm.stdout.String()) != strings.TrimSpace(cold.stdout.String()) {
		t.Fatalf("soak warm lifecycle changed the stable Connector CRID")
	}
	reloaded := loadSandboxAgentState(t, stateDir)
	if err := validateSandboxDeviceIdentity(reloaded, namespace.AgentID, loaded.DeviceAPIKeyID); err != nil {
		t.Fatalf("soak warm lifecycle identity: %v", err)
	}
}

func validateSandboxSoakCredentialBoundary() error {
	for _, name := range []string{sandboxCleanupJWTFileEnv, "QURL_CLI_SANDBOX_CLEANUP_JWT"} {
		if _, present := os.LookupEnv(name); present {
			return errors.New("scheduled soak must not receive customer cleanup authority")
		}
	}
	return nil
}

func registerSandboxSoakResourceCleanup(t *testing.T, endpoint, connectorID, stateDir string) *qurl.AgentState {
	t.Helper()
	loaded := loadSandboxAgentState(t, stateDir)
	if loaded != nil {
		registerSandboxResourceCleanup(t, endpoint, connectorID, loaded.DeviceAPIKey)
	}
	return loaded
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

func TestSandboxLocalPublishSoakRejectsCleanupAuthority(t *testing.T) {
	for _, name := range []string{sandboxCleanupJWTFileEnv, "QURL_CLI_SANDBOX_CLEANUP_JWT"} {
		t.Run(name, func(t *testing.T) {
			for _, cleanupName := range []string{sandboxCleanupJWTFileEnv, "QURL_CLI_SANDBOX_CLEANUP_JWT"} {
				t.Setenv(cleanupName, "")
				if err := os.Unsetenv(cleanupName); err != nil {
					t.Fatalf("unset cleanup authority: %v", err)
				}
			}
			if err := validateSandboxSoakCredentialBoundary(); err != nil {
				t.Fatalf("absent cleanup authority rejected: %v", err)
			}
			t.Setenv(name, "forbidden")
			if err := validateSandboxSoakCredentialBoundary(); err == nil {
				t.Fatal("scheduled soak accepted cleanup authority")
			}
		})
	}
}

func TestSandboxLocalPublishSoakResourceCleanupAfterInterruptedRun(t *testing.T) {
	const connectorID = "connector-soak-interrupted-cleanup"
	row := mintConnectorRow(t, connectorID)
	requests := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer device-token" {
			t.Errorf("cleanup authorization = %q, want device credential", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/resources" && r.URL.Query().Get("slug") == connectorID:
			requests <- "find"
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]any{"data": []connectorResourceRow{row}}); err != nil {
				t.Errorf("encode resource lookup: %v", err)
			}
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/resources/"+row.ResourceID:
			requests <- "delete"
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatalf("secure interrupted-run state directory: %v", err)
	}
	store, err := qurl.OpenFileAgentState(filepath.Join(stateDir, state.AgentStateFile))
	if err != nil {
		t.Fatalf("open interrupted-run state: %v", err)
	}
	if err := store.SaveAgentState(context.Background(), &qurl.AgentState{
		AgentID:        "qurl-share-r123-a2-ck",
		DeviceAPIKey:   "device-token",
		DeviceAPIKeyID: "key_Device123456",
	}); err != nil {
		_ = store.Close()
		t.Fatalf("save interrupted-run state: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close interrupted-run state: %v", err)
	}

	t.Run("interrupted cycle cleanup", func(t *testing.T) {
		loaded := registerSandboxSoakResourceCleanup(t, server.URL, connectorID, stateDir)
		if loaded == nil {
			t.Fatal("interrupted-run cleanup loaded no durable state")
		}
	})
	close(requests)
	got := make([]string, 0, 2)
	for request := range requests {
		got = append(got, request)
	}
	if strings.Join(got, ",") != "find,delete" {
		t.Fatalf("interrupted-run cleanup requests = %v, want resource find then delete", got)
	}
}
