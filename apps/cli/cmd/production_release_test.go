//go:build clisandbox && cliproduction

package main

import (
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
	"regexp"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	connectorservice "github.com/layervai/qurl-connector/pkg/service"

	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/consume"
)

const productionProofEndpoint = "https://api.layerv.ai"

type productionPublishDoc struct {
	CRID          string `json:"crid"`
	ResourceID    string `json:"resource_id"`
	Private       *bool  `json:"private"`
	FoundExisting *bool  `json:"found_existing"`
}

type productionSharingDoc struct {
	CRID            string `json:"crid"`
	ResourceID      string `json:"resource_id"`
	DesiredState    string `json:"desired_state"`
	ConnectionState string `json:"connection_state"`
	ServingEpoch    uint64 `json:"serving_epoch"`
}

// TestProductionReleaseJourney executes the packaged candidate, with production
// defaults and no Hub, relay, deployment, proxy, or account-key overrides.
func TestProductionReleaseJourney(t *testing.T) {
	if os.Getenv("QURL_PRODUCTION_PROOF") != "enabled" {
		t.Fatal("production proof is not armed")
	}
	binary := os.Getenv("QURL_PRODUCTION_BINARY")
	if !filepath.IsAbs(binary) {
		t.Fatal("production proof requires the exact unpacked release binary")
	}
	key := productionCredential(t)
	owner := os.Getenv("QURL_PRODUCTION_OWNER_ID")
	if key == "" || owner == "" {
		t.Fatal("production proof requires disposable credentials and their expected owner")
	}
	namespace, err := sandboxNamespace("smoke")
	if err != nil {
		t.Fatal(err)
	}
	stateDir, err := filepath.EvalSymlinks(productionStateDir(t))
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"QURL_ENDPOINT":                   productionProofEndpoint,
		connectorstate.EnvStateDirPrimary: stateDir,
		connectorstate.EnvAgentID:         namespace.AgentID,
		"LAYERV_KEY_PROVIDER":             "file",
	}
	manager := connectorservice.NewUserJobManager()
	t.Cleanup(func() {
		if err := manager.Remove(connectordaemon.DaemonJobLabel); err != nil {
			t.Error("production proof could not remove its user daemon")
		}
	})
	login := productionCLI(t, binary, env, key+"\n", "login", "-o", "json")
	var identity struct {
		OwnerID        string `json:"owner_id"`
		DeviceEnrolled bool   `json:"device_enrolled"`
	}
	productionDecode(t, login, &identity)
	if identity.OwnerID != owner || !identity.DeviceEnrolled {
		t.Fatal("fresh production login did not confirm the expected enrolled owner")
	}
	productionCLI(t, binary, env, "", "whoami", "-o", "json")

	for _, test := range []struct {
		name, label string
		private     bool
	}{
		{"public", "smoke", false}, {"private", "failure", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			namespace, err := sandboxNamespace(test.label)
			if err != nil {
				t.Fatal(err)
			}
			marker := fmt.Sprintf("production-candidate-%s-%s", namespace.ConnectorID, test.name)
			var hits atomic.Uint64
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				_, _ = io.WriteString(w, marker)
			}))
			defer backend.Close()
			args := []string{"publish", backend.URL, "--id", namespace.ConnectorID, "-o", "json"}
			if !test.private {
				args = append(args, "--public")
			}
			var published productionPublishDoc
			productionDecode(t, productionCLI(t, binary, env, "", args...), &published)
			if published.CRID == "" || published.ResourceID == "" {
				t.Fatal("publication returned no resource identity")
			}
			t.Cleanup(func() { productionCLI(t, binary, env, "", "delete", published.CRID, "--yes") })
			if published.Private == nil || *published.Private != test.private || published.FoundExisting == nil || *published.FoundExisting {
				t.Fatal("fresh publication did not confirm the requested privacy and a new resource")
			}
			var epoch uint64
			for _, command := range []string{"status", "inspect"} {
				epoch = productionSharing(t, binary, env, command, published)
			}
			productionCLI(t, binary, env, "", "list", "-o", "json")
			productionGet(t, binary, env, published.CRID, marker, false)
			consumer := map[string]string{"QURL_ENDPOINT": productionProofEndpoint, connectorstate.EnvStateDirPrimary: productionStateDir(t), "LAYERV_KEY_PROVIDER": "file"}
			var shared struct {
				QURL string `json:"qurl"`
			}
			productionDecode(t, productionCLI(t, binary, env, "", "share", published.CRID, "-o", "json", "--session-duration", "5m"), &shared)
			if shared.QURL == "" {
				t.Fatal("production sharing returned no link")
			}
			before := hits.Load()
			if test.private {
				productionGet(t, binary, consumer, published.CRID, marker, true)
			}
			if test.private && hits.Load() != before {
				t.Fatal("unauthorized private access reached the backend")
			}
			var reused productionPublishDoc
			productionDecode(t, productionCLI(t, binary, env, "", args...), &reused)
			if reused.CRID != published.CRID || reused.ResourceID != published.ResourceID || reused.FoundExisting == nil || !*reused.FoundExisting || reused.Private == nil || *reused.Private != test.private {
				t.Fatal("repeat publication did not reuse the same resource and privacy")
			}
			for _, command := range []string{"stop", "start", "restart"} {
				next := productionSharing(t, binary, env, command, published)
				if command != "stop" && next <= epoch {
					t.Fatal("production restart did not advance its serving epoch")
				}
				epoch = next
				if command == "stop" {
					before := hits.Load()
					destination := filepath.Join(t.TempDir(), "stopped-content")
					_, code := productionCommand(t, binary, env, "", "get", published.CRID, "--file", destination)
					if code == 0 || hits.Load() != before {
						t.Fatal("stopped production route still served backend content")
					}
					if _, err := os.Stat(destination); !os.IsNotExist(err) {
						t.Fatal("stopped production route wrote a download")
					}
				} else {
					productionGet(t, binary, env, published.CRID, marker, false)
				}
			}
			productionCLI(t, binary, env, "", "delete", published.CRID, "--yes")
			productionGet(t, binary, env, published.CRID, marker, true)
		})
	}
}

// The Windows sandbox helper deliberately has no file reader. Reuse the
// customer reader so production proof retains its Windows ACL and link fences.
func productionCredential(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		return sandboxSecret(t, "QURL_API_KEY")
	}
	key, source, err := auth.Resolve(os.LookupEnv)
	if err != nil || source != auth.SourceEnvironmentFile {
		t.Fatalf("production proof requires one protected Windows API-key file: %s", productionCredentialFailureClass(err, source))
	}
	return key
}

// Classify only fixed reader failures; a hostile path or key must stay private.
func productionCredentialFailureClass(err error, source auth.Source) string {
	if errors.Is(err, auth.ErrCredentialConflict) {
		return "credential_conflict"
	}
	if errors.Is(err, auth.ErrNoCredential) {
		return "credential_missing"
	}
	if err == nil {
		if source != auth.SourceEnvironmentFile {
			return "credential_not_file"
		}
		return "none"
	}
	for _, known := range []struct{ message, class string }{
		{"must name one exact absolute private file", "file_path"},
		{"not one bounded owner-private regular file", "file_metadata"},
		{"not owned by the current user", "file_owner"},
		{"ACL must be protected from inheritance", "file_acl_inheritance"},
		{"DACL grants another principal access", "file_acl_principal"},
		{"unsafe ownership or link authority", "file_authority"},
		{"cannot open QURL_API_KEY_FILE without following links", "file_open"},
		{"changed while opening", "file_changed_on_open"},
		{"changed while reading", "file_changed_on_read"},
		{"exact key bytes plus one LF or CRLF", "file_line_format"},
		{"cannot read exact bounded", "file_read"},
		{"does not look like a qURL API key", "key_shape"},
	} {
		if strings.Contains(err.Error(), known.message) {
			return known.class
		}
	}
	return "reader_unclassified"
}

func productionDecode(t *testing.T, raw []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatal("production candidate returned malformed JSON")
	}
}

func productionCommand(t *testing.T, binary string, env map[string]string, input string, args ...string) (raw []byte, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...) //nolint:gosec // Exact release binary, selected and hashed by the trusted workflow.
	for _, name := range []string{"PATH", "HOME", "USER", "LOGNAME", "TMPDIR", "TEMP", "TMP", "SystemRoot", "APPDATA", "LOCALAPPDATA", "USERPROFILE", "USERNAME", "COMSPEC", "PATHEXT", "ProgramData", "ProgramFiles", "ProgramFiles(x86)", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if value, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	for name, value := range env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	cmd.Stdin = strings.NewReader(input)
	// Never print command output: it can contain bearer links or credentials.
	raw, err := cmd.Output()
	if ctx.Err() != nil {
		t.Fatal("production candidate command timed out")
	}
	if err == nil {
		return raw, 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		t.Logf("production candidate failure class: %s", productionFailureClass(exit.Stderr))
		if status := regexp.MustCompile(`HTTP \d{3}`).Find(exit.Stderr); len(status) > 0 {
			t.Logf("production API rejected %s: %s", args[0], status)
		}
		return raw, exit.ExitCode()
	}
	t.Fatal("production candidate could not start")
	return nil, -1
}

// Emit only a fixed class, never the stderr beside a public error message.
func productionFailureClass(stderr []byte) string {
	for _, known := range []struct{ message, class string }{
		{consume.MsgCRIDNotFound, "crid_not_found"},
		{consume.MsgCRIDLinkUnavailable, "crid_link_unavailable"},
		{consume.MsgCRIDLinkNoAnswer, "crid_link_no_answer"},
		{consume.MsgAccessBusy, "access_busy"},
		{consume.MsgLinkUnavailable, "download_host_unavailable"},
		{consume.MsgCRIDLinkRateLimited, "crid_link_rate_limited"},
		{consume.MsgCRIDPublisherOffline, "crid_publisher_offline"},
		{consume.MsgCRIDResourceClosed, "crid_resource_closed"},
		{consume.MsgCRIDLinkRequestRejected, "crid_link_request_rejected"},
		{consume.MsgCRIDLinkRefused, "crid_link_refused"},
		{"qURL could not start this Connector's device enrollment because the CLI's local enrollment settings are invalid.", "enrollment_config"},
		{"This Connector's device enrollment did not finish before its safe retry period ended.", "enrollment_unavailable"},
		{"The qURL platform could not verify this Connector during device enrollment.", "enrollment_identity"},
		{"The qURL platform refused this Connector's device enrollment request.", "enrollment_request_rejected"},
		{"The qURL platform and this CLI did not agree on this Connector's device enrollment, so qURL stopped safely.", "enrollment_mismatch"},
		{"qURL could not safely save this Connector's device enrollment state on this machine, so it stopped.", "enrollment_persistence"},
		{"The qURL platform isn't accepting new Connector enrollments here right now.", "enrollment_disabled"},
		{"qurl: platform denied access (errCode=", "platform_denied"},
		{"Access is denied.", "os_access_denied"},
		// TODO(upstream-contract): keep these fixed classes aligned with qurl-go Windows state errors.
		{"is not owned by the current Windows user", "state_owner"},
		{"grants access to another Windows principal", "state_acl"},
		{"grants delete or ACL control to an untrusted Windows principal", "state_ancestor_control"},
		{"is not owned by a trusted Windows principal", "state_ancestor_owner"},
		{"ACL must be protected from inheritance", "state_acl_inheritance"},
		{"has an inherited or inherit-only ACE", "state_acl_flags"},
		{"The system cannot find the file specified.", "os_file_missing"},
		{"context deadline exceeded", "native_deadline"},
	} {
		if strings.Contains(string(stderr), known.message) {
			return known.class
		}
	}
	return "unclassified"
}

func productionSharing(t *testing.T, binary string, env map[string]string, command string, resource productionPublishDoc) uint64 {
	t.Helper()
	var state productionSharingDoc
	productionDecode(t, productionCLI(t, binary, env, "", command, resource.CRID, "-o", "json"), &state)
	wantDesired, wantConnection := "on", "serving"
	if command == "stop" {
		wantDesired, wantConnection = "off", "stopped"
	}
	if state.CRID != resource.CRID || state.ResourceID != resource.ResourceID || state.DesiredState != wantDesired || state.ConnectionState != wantConnection || state.ServingEpoch == 0 {
		t.Fatalf("production %s returned the wrong resource or sharing state", command)
	}
	return state.ServingEpoch
}

func productionCLI(t *testing.T, binary string, env map[string]string, input string, args ...string) []byte {
	t.Helper()
	raw, code := productionCommand(t, binary, env, input, args...)
	if code != 0 {
		t.Fatalf("production candidate %s failed with exit %d", args[0], code)
	}
	return raw
}

func productionGet(t *testing.T, binary string, env map[string]string, crid, marker string, denied bool) {
	t.Helper()
	destination := filepath.Join(t.TempDir(), "content")
	_, code := productionCommand(t, binary, env, "", "get", crid, "--file", destination, "--session-duration", "5m")
	if denied {
		if code != 5 {
			t.Fatalf("production access denial exit = %d, want not found (5)", code)
		}
		if _, err := os.Stat(destination); !os.IsNotExist(err) {
			t.Fatal("denied production access created a download")
		}
		return
	}
	if code != 0 {
		t.Fatalf("production content download failed with exit %d", code)
	}
	content, err := os.ReadFile(destination) //nolint:gosec // Test-owned download path.
	if err != nil || string(content) != marker {
		t.Fatal("production route did not return the exact local backend bytes")
	}
}
