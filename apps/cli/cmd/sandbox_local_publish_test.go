//go:build clisandbox && (linux || darwin)

package main

import (
	"bytes"
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
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectoragent "github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

const (
	localPublishSandboxArming           = "QURL_CLI_SANDBOX_LOCAL_PUBLISH"
	sandboxRemoteURLLifecyclePhase      = "remote_url_resource_lifecycle"
	sandboxLocalConnectorLifecyclePhase = "local_connector_lifecycle"
	sandboxPOSIXFailureChildTest        = "TestSandboxPOSIXControlledFailureCleanupChild"
	sandboxCleanupTimeout               = 30 * time.Second
	sandboxRegistryTimeout              = 30 * time.Second
	sandboxRouteFenceTimeout            = 2 * time.Minute
	sandboxRouteFencePoll               = 500 * time.Millisecond
	sandboxRouteFenceSettle             = 2 * time.Second
	sandboxRouteProbeTimeout            = 15 * time.Second
	// Serving after stop is a security-boundary failure, not eventual
	// convergence. Five seconds is the intentional initial hard SLO; the
	// private journey records the real propagation time before release. The
	// absolute deadline also bounds an in-flight probe, so a request issued near
	// the end of the grace cannot hide a later backend hit.
	sandboxRouteServeGrace = 5 * time.Second
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

type sandboxListRowDoc struct {
	CRID         string  `json:"crid"`
	ResourceID   string  `json:"resource_id"`
	TargetURL    string  `json:"target_url"`
	DesiredState string  `json:"desired_state"`
	ServingEpoch *uint64 `json:"serving_epoch"`
}

type sandboxListDoc struct {
	Resources []json.RawMessage `json:"resources"`
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

// TestSandboxFullCustomerLifecycleSmoke is the release-candidate contract. The
// private lifecycle gate requires this exact test name so an older local-only
// smoke cannot be mistaken for the full remote-plus-local customer journey.
func TestSandboxFullCustomerLifecycleSmoke(t *testing.T) {
	testSandboxFullCustomerLifecycleSmoke(t)
}

// TestSandboxLocalPublishLifecycleSmoke retains the scheduled smoke entrypoint.
// Both entrypoints execute the same full journey once this source reaches main.
func TestSandboxLocalPublishLifecycleSmoke(t *testing.T) {
	testSandboxFullCustomerLifecycleSmoke(t)
}

// TestSandboxFullCustomerLifecyclePhaseContract gives the credential-free CI
// lane a fail-fast contract for the three customer journeys that the protected
// release gate must execute. The protected runner also validates the emitted
// subtest results, so listing this test cannot replace any live journey.
func TestSandboxFullCustomerLifecyclePhaseContract(t *testing.T) {
	want := []string{"remote_url_resource_lifecycle", "local_connector_lifecycle", "controlled_failure_cleanup"}
	got := []string{sandboxRemoteURLLifecyclePhase, sandboxLocalConnectorLifecyclePhase, sandboxControlledFailureLifecyclePhase}
	if !slices.Equal(got, want) {
		t.Fatalf("full customer lifecycle phases = %q, want %q", got, want)
	}
}

// testSandboxFullCustomerLifecycleSmoke exercises the unified customer command against
// the live sandbox. Unlike the legacy Connector smoke, it starts with an
// ordinary login key and no pre-issued enrollment token or device state. It
// pipes that key into the exact `qurl login` artifact, then removes the key from
// every later customer command. A passing run validates that login can mint the exact one-shot credential,
// native UDP enrollment exchanges it for a device identity, the device creates
// the tunnel resource, and the FRP server admits the resulting route. The same
// exact artifact and device then complete the remote URL publish/read/get/delete
// journey before sending unique local HTTP bytes through a minted qURL,
// exercising stop/start/restart, and deleting the live share while its
// foreground daemon is still running.
//
// The private orchestrator runs this tagged test with one exact customer CLI
// artifact. It creates a native device in a fresh state directory, so the lane
// must provide a short-lived JWT that can revoke the resulting device
// credential. The JWT must represent the same owner as QURL_API_KEY: 404 is a
// cleanup failure, because a wrong-owner JWT also cannot see the new key. The
// resource and device key are both reclaimed before returning. Run explicitly:
//
//	QURL_CLI_SANDBOX_LOCAL_PUBLISH=enabled \
//	QURL_CLI_SANDBOX_BINARY=/absolute/path/to/qurl \
//	QURL_SHARING_RUN_ID=123 QURL_SHARING_RUN_ATTEMPT=1 QURL_SHARING_RUNTIME=host \
//	QURL_API_KEY=... QURL_ENDPOINT=... QURL_CLI_SANDBOX_CLEANUP_JWT=... \
//	QURL_CONNECTOR_HUB_HOST=... QURL_CONNECTOR_HUB_PORT=... \
//	QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64=... \
//	go test -tags=clisandbox -count=1 -run '^TestSandboxFullCustomerLifecycleSmoke$' ./apps/cli/cmd
func testSandboxFullCustomerLifecycleSmoke(t *testing.T) {
	if os.Getenv(localPublishSandboxArming) != "enabled" {
		t.Skipf("SKIPPED LOUDLY: unified local-publish sandbox smoke is disarmed — %s != enabled", localPublishSandboxArming)
	}
	requireSandboxFailureCredentials(t)
	fixture := startSandboxLocalPublish(t, "smoke")
	defer fixture.interruptAndValidate(t)
	binary, cliEnv, stateDir, local := fixture.binary, fixture.env, fixture.stateDir, fixture.local

	if assessment, err := cridux.Assess(local.CRID); err != nil || assessment.Kind != cridux.KindCRID {
		t.Fatalf("local publish registry CRID = %q, want a valid full CRID: %v", local.CRID, err)
	}

	initial := waitSandboxSharingState(t, binary, cliEnv, stateDir, local.CRID, "on", "serving", 2*time.Minute)
	inspect := runSandboxLocalCLI(t, binary, cliEnv, stateDir, "-o", "json", "inspect", local.CRID)
	var inspectErr error
	if inspect.code != 0 {
		inspectErr = fmt.Errorf("exit %d", inspect.code)
	}
	assertHealthySandboxInspection(t, inspect.stdout.Bytes(), inspectErr, inspect.stderr.String(),
		local.CRID, local.ResourceID, initial.DesiredState, initial.ConnectionState, initial.ServingEpoch,
		fixture.key, fixture.cleanupJWT, loadSandboxAgentState(t, stateDir).DeviceAPIKey)
	assertSandboxListRow(t, binary, cliEnv, stateDir, local, initial.ServingEpoch)
	assertSandboxLocalRoute(t, binary, cliEnv, stateDir, local.CRID, fixture.marker, 2*time.Minute)
	t.Run(sandboxControlledFailureLifecyclePhase, func(t *testing.T) {
		failureCRID := runSandboxFailureChild(t, sandboxPOSIXFailureChildTest)
		assertSandboxFailureRemoteDeleted(t, binary, cliEnv, stateDir, failureCRID)
	})
	t.Run(sandboxRemoteURLLifecyclePhase, func(t *testing.T) {
		assertSandboxRemoteURLDeviceJourney(t, binary, cliEnv, stateDir)
	})

	t.Run(sandboxLocalConnectorLifecyclePhase, func(t *testing.T) {
		stopped := runSandboxLocalCLI(t, binary, cliEnv, stateDir, "-o", "json", "stop", local.CRID)
		stoppedState := decodeSandboxSharing(t, stopped)
		if err := validateSandboxSharingTransition(stoppedState, "off", "stopped", initial.ServingEpoch); err != nil {
			t.Fatalf("stop state = %+v: %v", stoppedState, err)
		}
		assertSandboxLocalRouteFenced(t, binary, cliEnv, stateDir, local.CRID, fixture.marker, &fixture.backendHits)

		started := runSandboxLocalCLI(t, binary, cliEnv, stateDir, "-o", "json", "start", local.CRID)
		startedState := decodeSandboxSharing(t, started)
		if err := validateSandboxSharingTransition(startedState, "on", "serving", stoppedState.ServingEpoch); err != nil {
			t.Fatalf("start state = %+v: %v", startedState, err)
		}
		assertSandboxLocalRoute(t, binary, cliEnv, stateDir, local.CRID, fixture.marker, 2*time.Minute)

		restarted := runSandboxLocalCLI(t, binary, cliEnv, stateDir, "-o", "json", "restart", local.CRID)
		restartedState := decodeSandboxSharing(t, restarted)
		if err := validateSandboxSharingTransition(restartedState, "on", "serving", startedState.ServingEpoch); err != nil {
			t.Fatalf("restart state = %+v: %v", restartedState, err)
		}
		assertSandboxLocalRoute(t, binary, cliEnv, stateDir, local.CRID, fixture.marker, 2*time.Minute)
		if fixture.backendHits.Load() < 3 {
			t.Fatalf("local backend saw %d public-route hits, want at least one before and after lifecycle changes", fixture.backendHits.Load())
		}

		deleted := runSandboxLocalCLI(t, binary, cliEnv, stateDir, "delete", local.CRID, "--yes")
		if deleted.code != 0 {
			t.Fatalf("delete while serving exit = %d: %s", deleted.code, deleted.stderr.String())
		}
		shares, present, err := state.ReadLocalSharesIfPresent(context.Background(), stateDir)
		if err != nil || !present {
			t.Fatalf("read local registry after delete = (present %v, %v)", present, err)
		}
		for index := range shares {
			if shares[index].CRID == local.CRID {
				t.Fatalf("deleted CRID %s remains in local daemon registry", local.CRID)
			}
		}
	})
}

// TestSandboxPOSIXControlledFailureCleanupChild is invoked only by a trusted
// parent journey. It reaches one real customer-command failure and exits
// nonzero so the parent can prove that every registered cleanup still ran.
func TestSandboxPOSIXControlledFailureCleanupChild(t *testing.T) {
	stateDir := sandboxFailureChildStateDir(t)
	var crid string
	productCleanupComplete := false
	registerSandboxFailureFinalCleanup(t, stateDir, &crid, &productCleanupComplete)

	markSandboxFailurePhase(sandboxFailurePhaseSetup)
	fixture := startSandboxLocalPublishInState(t, "failure", stateDir)
	crid = fixture.local.CRID
	controlledFailureReached := false
	defer func() {
		if controlledFailureReached {
			return
		}
		inspection := runSandboxLocalCLI(t, fixture.binary, fixture.env, stateDir, "-o", "json", "inspect", crid)
		var commandErr error
		if inspection.code != 0 {
			commandErr = fmt.Errorf("exit %d", inspection.code)
		}
		markSandboxFailureDiagnosticFromCommand(inspection.stdout.String(), inspection.stderr.String(), commandErr)
	}()
	t.Cleanup(func() {
		deleted := runSandboxLocalCLI(t, fixture.binary, fixture.env, stateDir, "delete", crid, "--yes")
		if deleted.code != 0 {
			t.Errorf("controlled-failure delete exit = %d: %s", deleted.code, deleted.stderr.String())
			return
		}
		shares, present, err := state.ReadLocalSharesIfPresent(context.Background(), stateDir)
		if err != nil {
			t.Errorf("read controlled-failure local registry after delete: %v", err)
			return
		}
		for _, share := range shares {
			if share.CRID == crid {
				t.Errorf("controlled-failure CRID %s remains in local registry", crid)
				return
			}
		}
		if !present && len(shares) != 0 {
			t.Errorf("controlled-failure registry has %d rows while absent", len(shares))
			return
		}
		productCleanupComplete = true
	})
	t.Cleanup(func() { fixture.interruptAndValidate(t) })

	markSandboxFailurePhase(sandboxFailurePhaseReadiness)
	initial := waitSandboxSharingState(t, fixture.binary, fixture.env, stateDir, crid, "on", "serving", time.Minute)
	markSandboxFailurePhase(sandboxFailurePhaseRoute)
	assertSandboxLocalRoute(t, fixture.binary, fixture.env, stateDir, crid, fixture.marker, time.Minute)
	markSandboxFailurePhase(sandboxFailurePhaseStop)
	stopped := decodeSandboxSharing(t, runSandboxLocalCLI(
		t, fixture.binary, fixture.env, stateDir, "-o", "json", "stop", crid,
	))
	if err := validateSandboxSharingTransition(stopped, "off", "stopped", initial.ServingEpoch); err != nil {
		t.Fatalf("controlled-failure stop state = %+v: %v", stopped, err)
	}
	markSandboxFailurePhase(sandboxFailurePhaseFence)
	if err := controlledSandboxRouteFenceError(t, fixture.binary, fixture.env, fixture.stateDir,
		fixture.local.CRID, fixture.marker, &fixture.backendHits, 30*time.Second); err != nil {
		markSandboxFailureDiagnosticFromError(err)
		t.Fatal("controlled-failure route fence did not settle")
	}
	markSandboxFailurePhase(sandboxFailurePhaseStoppedGet)
	destination := filepath.Join(t.TempDir(), "fenced")
	failedGet := runSandboxLocalCLI(
		t, fixture.binary, fixture.env, stateDir, "--quiet", "get", crid, "--file", destination,
	)
	if err := validateSandboxStoppedDownloadResult(
		failedGet.code, failedGet.stdout.String(), failedGet.stderr.String(), destination, sandboxStoppedRouteRefusal(t),
	); err != nil {
		markSandboxFailureDiagnosticFromCommand(failedGet.stdout.String(), failedGet.stderr.String(), errors.New("controlled get failed"))
		t.Fatalf("controlled customer get did not fail as required: %v", err)
	}
	controlledFailureReached = true
	t.Fatal(sandboxFailureChildSentinel)
}

func assertSandboxRemoteURLDeviceJourney(t *testing.T, binary string, cliEnv map[string]string, stateDir string) {
	t.Helper()
	suffix := fmt.Sprintf("qurl-private-sandbox-device-journey=%d", time.Now().UnixNano())
	target := "https://example.com/?" + suffix
	// TODO(upstream-contract): qurl-service normalizes an empty root path from
	// "/" to "". Keep the common customer input above and assert its explicit
	// canonical form so a service contract change fails with useful evidence.
	canonicalTarget := "https://example.com?" + suffix
	description := sandboxJourneyResourceDescription(t, cliEnv)
	published := runSandboxLocalCLI(t, binary, cliEnv, stateDir,
		"-o", "json", "publish", target, "--description", description)
	if published.code != 0 {
		t.Fatalf("device-authenticated remote publish exit = %d: %s", published.code, published.stderr.String())
	}
	var pub journeyPublishDoc
	if err := json.Unmarshal(published.stdout.Bytes(), &pub); err != nil {
		t.Fatalf("decode device-authenticated remote publish output: %v", err)
	}
	if pub.CRID == "" || pub.ResourceID == "" || pub.TargetURL != canonicalTarget || pub.FoundExisting {
		t.Fatalf("device-authenticated remote publish = %+v, want one new URL resource", pub)
	}
	deleted := false
	t.Cleanup(func() {
		if deleted {
			return
		}
		cleanup := runSandboxLocalCLI(t, binary, cliEnv, stateDir, "delete", pub.CRID, "--yes")
		if cleanup.code != 0 {
			t.Errorf("cleanup device-authenticated remote resource exit = %d: %s", cleanup.code, cleanup.stderr.String())
		}
	})
	if assessment, err := cridux.Assess(pub.CRID); err != nil || assessment.Kind != cridux.KindCRID {
		t.Fatalf("device-authenticated remote publish CRID = %q, want a valid full CRID: %v", pub.CRID, err)
	}

	var status journeyResourceStatusDoc
	for _, command := range []string{"status", "inspect"} {
		result := runSandboxLocalCLI(t, binary, cliEnv, stateDir, "-o", "json", command, pub.CRID)
		if result.code != 0 {
			t.Fatalf("device-authenticated remote %s exit = %d: %s", command, result.code, result.stderr.String())
		}
		var got journeyResourceStatusDoc
		if err := json.Unmarshal(result.stdout.Bytes(), &got); err != nil {
			t.Fatalf("decode device-authenticated remote %s output: %v", command, err)
		}
		if got.CRID != pub.CRID || got.ResourceID != pub.ResourceID || got.TargetURL != pub.TargetURL ||
			got.Type != "url" || got.Status != "active" {
			t.Fatalf("device-authenticated remote %s = %+v, want published active URL %+v", command, got, pub)
		}
		if command == "status" {
			status = got
		} else if got != status {
			t.Fatalf("device-authenticated remote inspect = %+v, want status %+v", got, status)
		}
	}

	seen := 0
	cursor := ""
	for page := 1; page <= listMaxPages; page++ {
		args := []string{"-o", "json", "list", "--limit", strconv.Itoa(listPageLimit), "--status", "active"}
		if cursor != "" {
			args = append(args, "--cursor", cursor)
		}
		result := runSandboxLocalCLI(t, binary, cliEnv, stateDir, args...)
		if result.code != 0 {
			t.Fatalf("device-authenticated list page %d exit = %d: %s", page, result.code, result.stderr.String())
		}
		var document journeyListDoc
		if err := json.Unmarshal(result.stdout.Bytes(), &document); err != nil {
			t.Fatalf("decode device-authenticated list page %d: %v", page, err)
		}
		for _, resource := range document.Resources {
			if resource.CRID == pub.CRID {
				seen++
				if resource.Description != description {
					t.Errorf("device-authenticated list description = %q, want %q", resource.Description, description)
				}
			}
		}
		if !document.HasMore {
			break
		}
		if document.NextCursor == "" {
			t.Fatalf("device-authenticated list page %d reports more rows without a cursor", page)
		}
		cursor = document.NextCursor
	}
	if seen != 1 {
		t.Fatalf("device-authenticated remote CRID appeared %d times in the newest active list window, want once", seen)
	}

	resolved := runSandboxLocalCLI(t, binary, cliEnv, stateDir, "resolve", pub.CRID)
	if _, err := validateSandboxResolveCommandResult(
		"device-authenticated remote resolve",
		resolved.code,
		resolved.stdout.String(),
		resolved.stderr.String(),
	); err != nil {
		t.Fatal(err)
	}

	destination := filepath.Join(t.TempDir(), "remote-url-payload")
	downloaded := runSandboxLocalCLI(t, binary, cliEnv, stateDir, "get", pub.CRID, "--file", destination)
	if downloaded.code != 0 {
		t.Fatalf("device-authenticated remote get exit = %d: %s", downloaded.code, downloaded.stderr.String())
	}
	payload, err := os.ReadFile(destination) //nolint:gosec // Exact test-owned destination.
	if err != nil {
		t.Fatalf("read device-authenticated remote download: %v", err)
	}
	if !strings.Contains(string(payload), journeyTargetMarker) || strings.Contains(string(payload), apitest.InterstitialTitle) {
		t.Fatalf("device-authenticated remote get returned %d unexpected bytes", len(payload))
	}

	stopped := runSandboxLocalCLI(t, binary, cliEnv, stateDir, "stop", pub.CRID)
	if stopped.code == 0 || !strings.Contains(stopped.stderr.String(), "stop applies only to a local qURL Connector") ||
		!strings.Contains(stopped.stderr.String(), "qurl delete "+pub.CRID+" --yes") {
		t.Fatalf("remote URL stop guidance = exit %d, stderr %q", stopped.code, stopped.stderr.String())
	}

	removed := runSandboxLocalCLI(t, binary, cliEnv, stateDir, "delete", pub.CRID, "--yes")
	if removed.code != 0 {
		t.Fatalf("device-authenticated remote delete exit = %d: %s", removed.code, removed.stderr.String())
	}
	deleted = true
	redeleted := runSandboxLocalCLI(t, binary, cliEnv, stateDir, "delete", pub.CRID, "--yes")
	if redeleted.code != 0 {
		t.Fatalf("device-authenticated remote re-delete exit = %d: %s", redeleted.code, redeleted.stderr.String())
	}
	revoked := runSandboxLocalCLI(t, binary, cliEnv, stateDir, "resolve", pub.CRID)
	if err := validateSandboxDeletedCommandResult(
		"device-authenticated resolve",
		revoked.code,
		revoked.stdout.String(),
		revoked.stderr.String(),
	); err != nil {
		t.Fatal(err)
	}
}

type sandboxLocalFixture struct {
	binary     string
	env        map[string]string
	stateDir   string
	marker     string
	local      *state.LocalShare
	key        string
	cleanupJWT string

	backendHits atomic.Uint64
	process     *sandboxPublishProcess
	stopOnce    sync.Once
}

func startSandboxLocalPublish(t *testing.T, label string) *sandboxLocalFixture {
	t.Helper()
	return startSandboxLocalPublishInState(t, label, "")
}

func startSandboxLocalPublishInState(t *testing.T, label, requestedStateDir string) *sandboxLocalFixture {
	t.Helper()
	binary, err := validateSandboxCLIBinary(os.Getenv(sandboxCLIBinaryEnv))
	if err != nil {
		t.Fatalf("load exact customer CLI binary: %v", err)
	}
	cliEnv := sandboxJourneyEnv(t)
	addSandboxRunIdentity(t, cliEnv)
	cleanupJWT := sandboxSecret(t, "QURL_CLI_SANDBOX_CLEANUP_JWT")
	missing := []string{}
	for name, value := range map[string]string{
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
		t.Skipf("SKIPPED LOUDLY: unified local-publish sandbox %s is disarmed — missing %v", label, missing)
	}
	for _, name := range []string{hub.EnvHost, hub.EnvPort, hub.EnvServerPublicKey} {
		cliEnv[name] = strings.TrimSpace(os.Getenv(name))
	}
	namespace, err := sandboxNamespace(label)
	if err != nil {
		t.Fatalf("derive local-publish namespace: %v", err)
	}
	stateDir := requestedStateDir
	if stateDir == "" {
		stateDir = connectorStateTestDir(t)
	}
	if !filepath.IsAbs(stateDir) || filepath.Clean(stateDir) != stateDir {
		t.Fatal("local-publish state directory must be one exact absolute path")
	}
	if err := state.EnsureDirMode(stateDir); err != nil {
		t.Fatalf("secure local-publish state directory: %v", err)
	}
	cliEnv[state.EnvStateDirPrimary] = stateDir
	cliEnv[state.EnvAgentID] = namespace.AgentID
	bootstrapKey := cliEnv["QURL_API_KEY"]
	delete(cliEnv, "QURL_API_KEY")
	login := runSandboxLocalCLIInput(t, binary, cliEnv, stateDir, bootstrapKey+"\n", "-o", "json", "login")
	if login.code != 0 {
		t.Fatalf("one-time customer login exit = %d: %s", login.code, login.stderr.String())
	}
	var enrolled struct {
		OwnerID        string `json:"owner_id"`
		AuthType       string `json:"auth_type"`
		DeviceEnrolled bool   `json:"device_enrolled"`
	}
	if err := json.Unmarshal(login.stdout.Bytes(), &enrolled); err != nil {
		t.Fatalf("decode one-time customer login output: %v", err)
	}
	if enrolled.OwnerID == "" || enrolled.AuthType != "api_key" || !enrolled.DeviceEnrolled {
		t.Fatalf("one-time customer login returned incomplete device identity: %+v", enrolled)
	}
	whoami := runSandboxLocalCLI(t, binary, cliEnv, stateDir, "-o", "json", "whoami")
	if whoami.code != 0 {
		t.Fatalf("warm device whoami exit = %d: %s", whoami.code, whoami.stderr.String())
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
	if err := json.Unmarshal(whoami.stdout.Bytes(), &warmIdentity); err != nil {
		t.Fatalf("decode warm device whoami output: %v", err)
	}
	if warmIdentity.OwnerID != enrolled.OwnerID || warmIdentity.AuthType != "api_key" || warmIdentity.APIKey == nil ||
		warmIdentity.APIKey.KeyID == "" || warmIdentity.APIKey.Kind != "device" ||
		!slices.Equal(warmIdentity.APIKey.Scopes, []string{"qurl:read", "qurl:resolve", "qurl:write"}) {
		t.Fatalf("warm device whoami identity = %+v, want enrolled owner %q and a device key", warmIdentity, enrolled.OwnerID)
	}
	loadedAfterLogin := loadSandboxAgentState(t, stateDir)
	if err := validateSandboxDeviceIdentity(loadedAfterLogin, namespace.AgentID, ""); err != nil {
		t.Fatalf("one-time customer login durable identity: %v", err)
	}
	recordSandboxCleanupDeviceKey(t, loadedAfterLogin.DeviceAPIKeyID)
	assertSandboxStateExcludesSecret(t, stateDir, bootstrapKey)

	fixture := &sandboxLocalFixture{
		binary: binary, env: cliEnv, stateDir: stateDir, cleanupJWT: cleanupJWT,
		key: bootstrapKey, marker: fmt.Sprintf("sandbox-local-publish-%s-%d", namespace.ConnectorID, time.Now().UnixNano()),
	}
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fixture.backendHits.Add(1)
		_, _ = io.WriteString(w, fixture.marker)
	}))
	t.Cleanup(echo.Close)

	fixture.process = startSandboxPublishProcess(t, binary, cliEnv, namespace, stateDir, echo.URL)
	fixture.process.registerRecoveryCleanup(t, cliEnv["QURL_ENDPOINT"], cleanupJWT, namespace, stateDir, productionSandboxSiblingCleanupOps())
	crid := fixture.process.waitReady(t)
	registryCtx, cancelRegistryRead := context.WithTimeout(context.Background(), sandboxRegistryTimeout)
	defer cancelRegistryRead()
	local, err := waitSandboxLocalShareRegistry(registryCtx, stateDir, 100*time.Millisecond, fixture.process)
	if err != nil {
		fixture.process.requireRunning(t, "while waiting for the local-share registry")
		fixture.forceStop(t)
		t.Fatalf("read exact local-publish registry: %v", err)
	}
	fixture.local = &local
	if fixture.local.CRID != crid {
		fixture.forceStop(t)
		t.Fatalf("foreground publish CRID = %q, local registry CRID = %q", crid, fixture.local.CRID)
	}
	loaded := loadSandboxAgentState(t, fixture.stateDir)
	if err := validateSandboxDeviceIdentity(loaded, namespace.AgentID, ""); err != nil {
		fixture.forceStop(t)
		t.Fatalf("local-publish durable identity: %v", err)
	}
	if err := requireTestResourceIdentity(fixture.local.CRID, fixture.local.ResourceID); err != nil {
		fixture.forceStop(t)
		t.Fatalf("sandbox minted a non-test CRID: %v", err)
	}
	return fixture
}

func controlledSandboxRouteFenceError(t *testing.T, binary string, env map[string]string,
	stateDir, crid, marker string, backendHits *atomic.Uint64, limit time.Duration,
) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	destination := filepath.Join(t.TempDir(), "fenced-payload")
	return waitSandboxRouteFence(
		ctx,
		sandboxRouteFencePoll,
		sandboxRouteFenceSettle,
		sandboxRouteServeGrace,
		5*time.Second,
		func(ctx context.Context) (sandboxRouteProbeState, error) {
			return probeSandboxLocalRoute(
				ctx, t, binary, env, stateDir, crid,
				marker, destination, sandboxStoppedRouteRefusal(t),
			)
		},
		backendHits.Load,
	)
}

func assertSandboxFailureRemoteDeleted(t *testing.T, binary string, env map[string]string, stateDir, crid string) {
	t.Helper()
	result := runSandboxLocalCLI(t, binary, env, stateDir, "resolve", crid)
	if err := validateSandboxDeletedCommandResult(
		"controlled-failure resolve",
		result.code,
		result.stdout.String(),
		result.stderr.String(),
	); err != nil {
		t.Fatal(err)
	}
}

func waitSandboxLocalShareRegistry(ctx context.Context, stateDir string, pollInterval time.Duration,
	process *sandboxPublishProcess,
) (state.LocalShare, error) {
	if pollInterval <= 0 {
		return state.LocalShare{}, errors.New("local-share registry poll duration must be positive")
	}
	if process == nil {
		return state.LocalShare{}, errors.New("local-share registry requires the foreground publish process")
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	var last string
	for {
		shares, present, err := state.ReadLocalSharesIfPresent(ctx, stateDir)
		if err == nil && present && len(shares) == 1 {
			return shares[0], nil
		}
		last = fmt.Sprintf("%d shares, present %t, error %v", len(shares), present, err)
		select {
		case <-process.done:
			process.waitMu.Lock()
			waitErr := process.waitErr
			process.waitMu.Unlock()
			if waitErr == nil {
				return state.LocalShare{}, errors.New("foreground publish exited successfully before persisting a local share")
			}
			return state.LocalShare{}, fmt.Errorf("foreground publish exited before persisting a local share: %w", waitErr)
		case <-ctx.Done():
			return state.LocalShare{}, fmt.Errorf("local-share registry did not publish exactly one row: %s: %w", last, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (f *sandboxLocalFixture) interruptAndValidate(t *testing.T) {
	t.Helper()
	if f == nil {
		return
	}
	f.stopOnce.Do(func() {
		f.process.interruptAndValidate(t, f.key, f.cleanupJWT)
	})
}

func (f *sandboxLocalFixture) forceStop(t *testing.T) {
	t.Helper()
	if f != nil && f.process != nil {
		f.process.forceStop(t)
	}
}

type sandboxSharingDoc struct {
	CRID            string `json:"crid"`
	ResourceID      string `json:"resource_id"`
	TargetURL       string `json:"target_url"`
	DesiredState    string `json:"desired_state"`
	ConnectionState string `json:"connection_state"`
	ServingEpoch    uint64 `json:"serving_epoch"`
}

func assertSandboxStateExcludesSecret(t *testing.T, root, secret string) {
	t.Helper()
	if secret == "" {
		t.Fatal("cannot verify state without the one-time bootstrap secret")
	}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return fmt.Errorf("sandbox state contains an unsupported file type at %s", path)
		}
		raw, err := os.ReadFile(path) //nolint:gosec // Exact test-owned state path.
		if err != nil {
			return err
		}
		if bytes.Contains(raw, []byte(secret)) {
			return fmt.Errorf("one-time account API key remained in sandbox state file %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func runSandboxLocalCLI(t *testing.T, binary string, env map[string]string, stateDir string, args ...string) *runResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	return runSandboxLocalCLIContextInput(ctx, t, binary, env, stateDir, "", args...)
}

func runSandboxLocalCLIInput(t *testing.T, binary string, env map[string]string, stateDir, input string, args ...string) *runResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	return runSandboxLocalCLIContextInput(ctx, t, binary, env, stateDir, input, args...)
}

func runSandboxLocalCLIContext(ctx context.Context, t *testing.T, binary string, env map[string]string, stateDir string, args ...string) *runResult {
	t.Helper()
	return runSandboxLocalCLIContextInput(ctx, t, binary, env, stateDir, "", args...)
}

func runSandboxLocalCLIContextInput(ctx context.Context, t *testing.T, binary string, env map[string]string, stateDir, input string, args ...string) *runResult {
	t.Helper()
	commandArgs := append([]string{"--endpoint", env["QURL_ENDPOINT"]}, args...)
	cmd := exec.CommandContext(ctx, binary, commandArgs...) //nolint:gosec // The protected test validates the fixed binary and supplies closed arguments.
	commandEnv := cloneSandboxEnv(env)
	commandEnv[state.EnvStateDirPrimary] = stateDir
	cmd.Env = sandboxCommandEnv(commandEnv)
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	res := &runResult{}
	cmd.Stdout = &res.stdout
	cmd.Stderr = &res.stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.code = exitErr.ExitCode()
		} else {
			res.code = 1
			_, _ = fmt.Fprintf(&res.stderr, "Error: execute exact qurl test artifact: %v\n", err)
		}
	}
	assertSandboxStreamsDoNotContainSecrets(t, res, env["QURL_API_KEY"], strings.TrimSpace(input))
	return res
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

func waitSandboxSharingState(t *testing.T, binary string, env map[string]string, stateDir, crid, desired, observed string, limit time.Duration) sandboxSharingDoc {
	t.Helper()
	deadline := time.Now().Add(limit)
	var last string
	for time.Now().Before(deadline) {
		res := runSandboxLocalCLI(t, binary, env, stateDir, "-o", "json", "status", crid)
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

func assertSandboxListRow(t *testing.T, binary string, env map[string]string, stateDir string, local *state.LocalShare, epoch uint64) {
	t.Helper()
	res := runSandboxLocalCLI(t, binary, env, stateDir, "-o", "json", "list", "--status", "active", "--limit", "100")
	if res.code != 0 {
		t.Fatalf("list local share exit = %d: %s", res.code, res.stderr.String())
	}
	var doc sandboxListDoc
	if err := json.Unmarshal(res.stdout.Bytes(), &doc); err != nil {
		t.Fatalf("decode list output: %v", err)
	}
	for _, rawRow := range doc.Resources {
		var row sandboxListRowDoc
		if err := json.Unmarshal(rawRow, &row); err != nil {
			t.Fatalf("decode list row: %v", err)
		}
		if row.CRID == local.CRID {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(rawRow, &fields); err != nil {
				t.Fatalf("decode list row fields: %v", err)
			}
			connectionState, hasConnectionState := fields["connection_state"]
			if row.ResourceID != local.ResourceID || row.TargetURL != local.TargetURL || row.DesiredState != "on" ||
				hasConnectionState || row.ServingEpoch == nil || *row.ServingEpoch != epoch {
				connectionStateValue := "<absent>"
				if hasConnectionState {
					connectionStateValue = string(connectionState)
				}
				servingEpoch := any("<absent>")
				if row.ServingEpoch != nil {
					servingEpoch = *row.ServingEpoch
				}
				t.Fatalf("list row crid=%q resource_id=%q target_url=%q desired_state=%q connection_state=%v serving_epoch=%v; want full local target, desired on, epoch %d, and no fabricated live observation",
					row.CRID, row.ResourceID, row.TargetURL, row.DesiredState, connectionStateValue, servingEpoch, epoch)
			}
			return
		}
	}
	t.Fatalf("full CRID %s not found in local share listing", local.CRID)
}

func assertSandboxLocalRoute(t *testing.T, binary string, env map[string]string, stateDir, crid, marker string, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	dest := filepath.Join(t.TempDir(), "payload")
	var last string
	for time.Now().Before(deadline) {
		err := sandboxLocalRouteOnce(t, binary, env, stateDir, crid, marker, dest)
		if err == nil {
			return
		}
		last = err.Error()
		time.Sleep(time.Second)
	}
	t.Fatalf("public qURL route for %s did not deliver the local backend bytes: %s", crid, last)
}

func sandboxLocalRouteOnce(t *testing.T, binary string, env map[string]string, stateDir, crid, marker, dest string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	return sandboxLocalRouteOnceContext(ctx, t, binary, env, stateDir, crid, marker, dest)
}

func sandboxLocalRouteOnceContext(ctx context.Context, t *testing.T, binary string, env map[string]string, stateDir, crid, marker, dest string) error {
	t.Helper()
	probeState, err := probeSandboxLocalRoute(
		ctx, t, binary, env, stateDir, crid, marker, dest, sandboxStoppedRouteRefusal(t),
	)
	if err != nil {
		return err
	}
	if probeState != sandboxRouteServed {
		return errors.New("route returned the exact stopped-resource refusal while serving was expected")
	}
	return nil
}

type sandboxRouteProbeState uint8

const (
	sandboxRouteServed sandboxRouteProbeState = iota + 1
	sandboxRouteRefused
)

func probeSandboxLocalRoute(ctx context.Context, t *testing.T, binary string, env map[string]string, stateDir, crid, marker, dest, stoppedRefusal string) (sandboxRouteProbeState, error) {
	t.Helper()
	if err := os.Remove(dest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("clear prior route-probe destination: %w", err)
	}
	res := runSandboxLocalCLIContext(ctx, t, binary, env, stateDir, "--quiet", "get", crid, "--file", dest)
	if res.code != 0 {
		if err := validateSandboxStoppedDownloadResult(
			res.code, res.stdout.String(), res.stderr.String(), dest, stoppedRefusal,
		); err != nil {
			return 0, err
		}
		return sandboxRouteRefused, nil
	}
	payload, err := os.ReadFile(dest) //nolint:gosec // The destination is an isolated route-probe file under t.TempDir.
	if err != nil {
		return 0, err
	}
	if string(payload) != marker {
		return 0, fmt.Errorf("payload length %d did not match unique local marker", len(payload))
	}
	return sandboxRouteServed, nil
}

func validateSandboxStoppedRouteRefusal(res *runResult, stoppedRefusal string) error {
	if res == nil {
		return errors.New("stopped-route probe returned no command result")
	}
	return validateSandboxStoppedCommandResult(res.code, res.stdout.String(), res.stderr.String(), stoppedRefusal)
}

func TestSandboxStoppedRouteRefusalMatchesQuietGet(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/resolve", apitest.HandlerConnectorStopped503(t))
	destination := filepath.Join(t.TempDir(), "payload")
	res := runCLI(t, &runOpts{args: []string{
		"--endpoint", srv.URL, "--quiet", "get", srv.Key.CRID, "--file", destination,
	}})
	// The packaged harness runs without apps/cli/cmd as its working directory.
	// Prove this contract has no repository-relative runtime dependency.
	t.Chdir(t.TempDir())
	if err := validateSandboxStoppedDownloadResult(
		res.code, res.stdout.String(), res.stderr.String(), destination, sandboxStoppedRouteRefusal(t),
	); err != nil {
		t.Fatal(err)
	}

	dark := apitest.NewServer(t)
	dark.Script(http.MethodPost, "/v1/resources/"+dark.Key.CRID+"/resolve", apitest.HandlerDark503(t))
	darkResult := runCLI(t, &runOpts{args: []string{
		"--endpoint", dark.URL, "--quiet", "get", dark.Key.CRID, "--file", filepath.Join(t.TempDir(), "payload"),
	}})
	if err := validateSandboxStoppedRouteRefusal(darkResult, sandboxStoppedRouteRefusal(t)); err == nil {
		t.Fatal("generic dark 503 was accepted as the stopped-Connector refusal")
	}
}

func assertSandboxLocalRouteFenced(
	t *testing.T,
	binary string,
	env map[string]string,
	stateDir string,
	crid string,
	marker string,
	backendHits *atomic.Uint64,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), sandboxRouteFenceTimeout)
	defer cancel()
	dest := filepath.Join(t.TempDir(), "payload")
	stoppedRefusal := sandboxStoppedRouteRefusal(t)
	if err := waitSandboxRouteFence(
		ctx,
		sandboxRouteFencePoll,
		sandboxRouteFenceSettle,
		sandboxRouteServeGrace,
		sandboxRouteProbeTimeout,
		func(ctx context.Context) (sandboxRouteProbeState, error) {
			return probeSandboxLocalRoute(ctx, t, binary, env, stateDir, crid, marker, dest, stoppedRefusal)
		},
		backendHits.Load,
	); err != nil {
		t.Fatal(err)
	}
}

func waitSandboxRouteFence(
	ctx context.Context,
	pollInterval time.Duration,
	settleWindow time.Duration,
	serveGrace time.Duration,
	probeTimeout time.Duration,
	probe func(context.Context) (sandboxRouteProbeState, error),
	backendHits func() uint64,
) error {
	if pollInterval <= 0 || settleWindow <= 0 || serveGrace <= 0 || probeTimeout <= 0 {
		return errors.New("route-fence poll, settle, backend-serving grace, and probe timeout durations must be positive")
	}
	validator := sandboxRouteFenceValidator{
		settleWindow: settleWindow,
		serveGrace:   serveGrace,
		startedAt:    time.Now(),
		stableHits:   backendHits(),
		last:         "route fence was not sampled",
	}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("stopped local route did not settle before the bound: %s: %w", validator.last, ctx.Err())
		default:
		}
		probeStartedAt := time.Now()
		probeDeadline := sandboxRouteProbeDeadline(
			probeStartedAt,
			validator.startedAt.Add(serveGrace),
			probeTimeout,
		)
		probeCtx, cancelProbe := context.WithDeadline(ctx, probeDeadline)
		probeState, err := probe(probeCtx)
		cancelProbe()
		probeCompletedAt := time.Now()
		hits := backendHits()
		if err != nil {
			if err := validator.observeProbeFailure(probeStartedAt, probeCompletedAt, err, hits); err != nil {
				return err
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fmt.Errorf("stopped local route probe did not finish before the bound: %s: %w", validator.last, ctxErr)
			}
			// Route teardown can briefly reset a connection or return a gateway
			// response before the exact stopped-resource refusal is stable. A
			// non-exact result never counts as fencing: it resets the full settle
			// window and remains the terminal diagnostic if it does not converge.
		} else {
			settled, err := validator.observe(probeStartedAt, probeCompletedAt, probeState, hits)
			if err != nil {
				return err
			}
			if settled {
				// Close the sampling race before returning. A hit observed after the
				// settle window is a security-boundary failure, not a sampling artifact.
				finalHits := backendHits()
				if finalHits == hits {
					return nil
				}
				// This read happens after the probe completed. Charge a newly
				// observed hit at the read time, not at the probe issue time.
				finalReadAt := time.Now()
				if _, finalErr := validator.observe(finalReadAt, finalReadAt, sandboxRouteRefused, finalHits); finalErr != nil {
					return fmt.Errorf("stopped local route recorded a backend hit after settling: %w", finalErr)
				}
				return errors.New("stopped local route accepted a backend hit after settling")
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("stopped local route did not settle before the bound: %s: %w", validator.last, ctx.Err())
		case <-ticker.C:
		}
	}
}

func sandboxRouteProbeDeadline(now, graceDeadline time.Time, probeTimeout time.Duration) time.Time {
	deadline := now.Add(probeTimeout)
	if now.Before(graceDeadline) && graceDeadline.Before(deadline) {
		return graceDeadline
	}
	return deadline
}

type sandboxRouteFenceValidator struct {
	settleWindow time.Duration
	serveGrace   time.Duration
	startedAt    time.Time
	stableSince  time.Time
	stableHits   uint64
	last         string
}

func (v *sandboxRouteFenceValidator) observeProbeFailure(probeStartedAt, probeCompletedAt time.Time, err error, hits uint64) error {
	if v.settleWindow <= 0 || v.serveGrace <= 0 {
		return errors.New("route-fence settle and backend-serving grace durations must be positive")
	}
	if probeCompletedAt.Before(probeStartedAt) {
		return errors.New("route probe completed before it started")
	}
	if v.startedAt.IsZero() {
		v.startedAt = probeStartedAt
	}
	if hits != v.stableHits {
		priorHits := v.stableHits
		if probeCompletedAt.Sub(v.startedAt) >= v.serveGrace {
			return fmt.Errorf("stopped local route recorded a late backend hit after the %s teardown grace: %d to %d", v.serveGrace, priorHits, hits)
		}
		v.stableHits = hits
	}
	v.stableSince = time.Time{}
	v.last = fmt.Sprintf("last route probe was not the exact stopped-resource refusal: %v", err)
	return nil
}

func (v *sandboxRouteFenceValidator) observe(probeStartedAt, probeCompletedAt time.Time, probeState sandboxRouteProbeState, hits uint64) (bool, error) {
	if v.settleWindow <= 0 || v.serveGrace <= 0 {
		return false, errors.New("route-fence settle and backend-serving grace durations must be positive")
	}
	if probeCompletedAt.Before(probeStartedAt) {
		return false, errors.New("route probe completed before it started")
	}
	if v.startedAt.IsZero() {
		v.startedAt = probeStartedAt
	}
	switch probeState {
	case sandboxRouteServed:
		if probeCompletedAt.Sub(v.startedAt) >= v.serveGrace {
			return false, fmt.Errorf("stopped local route served backend bytes after the %s teardown grace at %d hits", v.serveGrace, hits)
		}
		v.stableSince = time.Time{}
		v.stableHits = hits
		v.last = fmt.Sprintf("stopped local route served backend bytes within the %s teardown grace at %d hits", v.serveGrace, hits)
		return false, nil
	case sandboxRouteRefused:
	default:
		return false, errors.New("route probe returned an unknown state")
	}
	if hits != v.stableHits {
		priorHits := v.stableHits
		if probeCompletedAt.Sub(v.startedAt) >= v.serveGrace {
			return false, fmt.Errorf("stopped local route recorded a late backend hit after the %s teardown grace: %d to %d", v.serveGrace, priorHits, hits)
		}
		v.stableSince = probeCompletedAt
		v.stableHits = hits
		v.last = fmt.Sprintf("late backend hit changed the count from %d to %d", priorHits, hits)
		return false, nil
	}
	if v.stableSince.IsZero() {
		v.stableSince = probeCompletedAt
		v.last = fmt.Sprintf("route refusal has not stayed stable for %s", v.settleWindow)
		return false, nil
	}
	stableFor := probeCompletedAt.Sub(v.stableSince)
	graceElapsed := probeCompletedAt.Sub(v.startedAt)
	if stableFor >= v.settleWindow && graceElapsed >= v.serveGrace {
		return true, nil
	}
	if graceElapsed < v.serveGrace {
		v.last = fmt.Sprintf("route refusal is stable but teardown grace has elapsed for %s of %s", graceElapsed, v.serveGrace)
		return false, nil
	}
	v.last = fmt.Sprintf("route refusal has stayed stable for %s of %s", stableFor, v.settleWindow)
	return false, nil
}

// Each stop, start, and restart changes the durable lifecycle authority. The
// serving epoch must therefore advance. An equal epoch is stale authority, not
// successful convergence, even when the desired and observed states match.
func validateSandboxSharingTransition(doc sandboxSharingDoc, desired, observed string, priorEpoch uint64) error { //nolint:gocritic // Passing the immutable decoded snapshot by value keeps validation isolated from later mutation.
	if doc.DesiredState != desired || doc.ConnectionState != observed {
		return fmt.Errorf("got %s/%s, want %s/%s", doc.DesiredState, doc.ConnectionState, desired, observed)
	}
	if doc.ServingEpoch <= priorEpoch {
		return fmt.Errorf("serving epoch %d did not advance beyond %d", doc.ServingEpoch, priorEpoch)
	}
	return nil
}

func TestRunSandboxLocalCLIUsesExactBinaryAndState(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "qurl")
	script := `#!/bin/sh
printf 'state=%s\n' "${QURL_CONNECTOR_STATE_DIR-unset}"
printf 'agent=%s\n' "${QURL_CONNECTOR_AGENT_ID-unset}"
printf 'home=%s\n' "${HOME-unset}"
printf 'arg=%s\n' "$@"
`
	if err := os.WriteFile(binary, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(binary, 0o500); err != nil { //nolint:gosec // The fixture must be executable and non-writable.
		t.Fatal(err)
	}
	stateDir := connectorStateTestDir(t)
	res := runSandboxLocalCLI(t, binary, map[string]string{
		"QURL_API_KEY":           "lv_test_exact_external_binary_key",
		"QURL_ENDPOINT":          "https://sandbox.invalid",
		state.EnvAgentID:         "qurl-share-r17-a3-hs",
		state.EnvStateDirPrimary: "/must-be-replaced",
	}, stateDir, "-o", "json", "status", "test-crid")
	if res.code != 0 {
		t.Fatalf("external CLI fixture exit = %d: %s", res.code, res.stderr.String())
	}
	want := strings.Join([]string{
		"state=" + stateDir,
		"agent=qurl-share-r17-a3-hs",
		"home=unset",
		"arg=--endpoint",
		"arg=https://sandbox.invalid",
		"arg=-o",
		"arg=json",
		"arg=status",
		"arg=test-crid",
		"",
	}, "\n")
	if got := res.stdout.String(); got != want {
		t.Fatalf("external CLI fixture output = %q, want %q", got, want)
	}

	nonExecutable := filepath.Join(t.TempDir(), "qurl-no-exec")
	if err := os.WriteFile(nonExecutable, []byte("not executable"), 0o600); err != nil {
		t.Fatal(err)
	}
	failed := runSandboxLocalCLI(t, nonExecutable, map[string]string{
		"QURL_API_KEY":  "lv_test_exact_external_binary_key",
		"QURL_ENDPOINT": "https://sandbox.invalid",
	}, t.TempDir(), "status", "test-crid")
	if failed.code != 1 || failed.stderr.Len() == 0 ||
		!strings.Contains(failed.stderr.String(), nonExecutable) {
		t.Fatalf("unstartable exact CLI diagnostic = exit %d stderr %q", failed.code, failed.stderr.String())
	}
}

func TestRunSandboxLocalCLIForwardsOnlyHardenedImageBinding(t *testing.T) {
	const exactImageID = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	binary := filepath.Join(t.TempDir(), "qurl")
	script := `#!/bin/sh
printf 'image=%s\n' "${QURL_SHARING_QURL_IMAGE-unset}"
`
	if err := os.WriteFile(binary, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(binary, 0o500); err != nil { //nolint:gosec // The fixture must be executable and non-writable.
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, runtimeName, want string
	}{
		{name: "hardened-container", runtimeName: "hardened_container", want: "image=" + exactImageID + "\n"},
		{name: "host", runtimeName: "host", want: "image=unset\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(sandboxRunIDEnv, "32635672597")
			t.Setenv(sandboxRunAttemptEnv, "2")
			t.Setenv(sandboxRuntimeEnv, tc.runtimeName)
			t.Setenv(sandboxQURLImageIDEnv, exactImageID)
			env := map[string]string{
				"QURL_API_KEY":        "lv_test_exact_external_binary_key",
				"QURL_ENDPOINT":       "https://sandbox.invalid",
				sandboxQURLImageIDEnv: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			}
			addSandboxRunIdentity(t, env)
			res := runSandboxLocalCLI(t, binary, env, t.TempDir(), "status", "test-crid")
			if res.code != 0 || res.stdout.String() != tc.want || res.stderr.Len() != 0 {
				t.Fatalf("exact child image binding = exit %d, stdout %q, stderr %q", res.code, res.stdout.String(), res.stderr.String())
			}
		})
	}
}

func TestSandboxHarnessPassesInlineAPIKeyToExactBinary(t *testing.T) {
	const fixtureAPIKey = "lv_test_protected_input_becomes_inline_api_key"
	apiKeyFile := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(apiKeyFile, []byte(fixtureAPIKey), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QURL_API_KEY", "")
	t.Setenv(sandboxAPIKeyFileEnv, apiKeyFile)
	apiKey, err := readSandboxSecretFile(sandboxAPIKeyFileEnv, "QURL_API_KEY")
	if err != nil {
		t.Fatalf("read protected API key fixture: %v", err)
	}

	binaryFixture := filepath.Join(t.TempDir(), "qurl")
	script := `#!/bin/sh
set -eu
if [ "${QURL_API_KEY-}" != "lv_test_protected_input_becomes_inline_api_key" ]; then
  printf 'QURL_API_KEY missing or incorrect\n' >&2
  exit 41
fi
if [ "${QURL_API_KEY_FILE+x}" = x ]; then
  printf 'QURL_API_KEY_FILE reached customer process\n' >&2
  exit 42
fi
printf 'inline API key received\n'
`
	if err := os.WriteFile(binaryFixture, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(binaryFixture, 0o500); err != nil { //nolint:gosec // The exact-binary fixture must be executable and non-writable.
		t.Fatal(err)
	}
	t.Setenv(sandboxCLIBinaryEnv, binaryFixture)
	binary, err := validateSandboxCLIBinary(os.Getenv(sandboxCLIBinaryEnv))
	if err != nil {
		t.Fatalf("validate exact customer CLI fixture: %v", err)
	}

	res := runSandboxLocalCLI(t, binary, map[string]string{
		"QURL_API_KEY":  apiKey,
		"QURL_ENDPOINT": "https://sandbox.invalid",
	}, t.TempDir(), "status", "test-crid")
	if res.code != 0 || res.stdout.String() != "inline API key received\n" || res.stderr.Len() != 0 {
		t.Fatalf("exact customer CLI did not receive one inline API key: exit %d", res.code)
	}

	missing := runSandboxLocalCLI(t, binary, map[string]string{
		"QURL_ENDPOINT": "https://sandbox.invalid",
	}, t.TempDir(), "status", "test-crid")
	if missing.code != 41 || missing.stdout.Len() != 0 || missing.stderr.String() != "QURL_API_KEY missing or incorrect\n" {
		t.Fatalf("exact customer CLI accepted a missing inline API key: exit %d", missing.code)
	}
	assertSandboxStreamsDoNotContainSecrets(t, missing, fixtureAPIKey)
}

func TestValidateSandboxSharingTransitionRequiresAdvancedEpoch(t *testing.T) {
	valid := sandboxSharingDoc{DesiredState: "on", ConnectionState: "serving", ServingEpoch: 8}
	if err := validateSandboxSharingTransition(valid, "on", "serving", 7); err != nil {
		t.Fatalf("valid transition: %v", err)
	}
	for name, observedState := range map[string]sandboxSharingDoc{
		"wrong desired state":    {DesiredState: "off", ConnectionState: "serving", ServingEpoch: 8},
		"wrong connection state": {DesiredState: "on", ConnectionState: "stopped", ServingEpoch: 8},
		"equal epoch":            {DesiredState: "on", ConnectionState: "serving", ServingEpoch: 7},
		"regressed epoch":        {DesiredState: "on", ConnectionState: "serving", ServingEpoch: 6},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateSandboxSharingTransition(observedState, "on", "serving", 7); err == nil {
				t.Fatal("invalid lifecycle transition accepted")
			}
		})
	}
}

func TestValidateSandboxRouteFence(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	settle := 10 * time.Second
	serveGrace := 5 * time.Second
	stoppedRefusal := sandboxStoppedRouteRefusal(t)

	t.Run("probe deadline is capped at absolute grace", func(t *testing.T) {
		graceDeadline := base.Add(serveGrace)
		nearGrace := graceDeadline.Add(-time.Nanosecond)
		if got := sandboxRouteProbeDeadline(nearGrace, graceDeadline, time.Minute); !got.Equal(graceDeadline) {
			t.Fatalf("in-grace probe deadline = %s, want %s", got, graceDeadline)
		}
		afterGrace := graceDeadline.Add(time.Nanosecond)
		if got, want := sandboxRouteProbeDeadline(afterGrace, graceDeadline, time.Minute), afterGrace.Add(time.Minute); !got.Equal(want) {
			t.Fatalf("post-grace probe deadline = %s, want %s", got, want)
		}
	})

	t.Run("stable exact refusal settles", func(t *testing.T) {
		validator := sandboxRouteFenceValidator{settleWindow: settle, serveGrace: serveGrace}
		for _, observation := range []struct {
			at      time.Time
			settled bool
		}{
			{at: base},
			{at: base.Add(settle - time.Nanosecond)},
			{at: base.Add(settle), settled: true},
		} {
			settled, err := validator.observe(observation.at, observation.at, sandboxRouteRefused, 4)
			if err != nil {
				t.Fatal(err)
			}
			if settled != observation.settled {
				t.Fatalf("observation at %s settled=%t, want %t", observation.at.Sub(base), settled, observation.settled)
			}
		}
	})

	t.Run("short settle window still samples through serving grace", func(t *testing.T) {
		validator := sandboxRouteFenceValidator{settleWindow: 2 * time.Second, serveGrace: serveGrace}
		for _, observation := range []struct {
			at      time.Time
			settled bool
		}{
			{at: base},
			{at: base.Add(2 * time.Second)},
			{at: base.Add(serveGrace - time.Nanosecond)},
			{at: base.Add(serveGrace), settled: true},
		} {
			settled, err := validator.observe(observation.at, observation.at, sandboxRouteRefused, 4)
			if err != nil {
				t.Fatal(err)
			}
			if settled != observation.settled {
				t.Fatalf("observation at %s settled=%t, want %t", observation.at.Sub(base), settled, observation.settled)
			}
		}
	})

	t.Run("brief serving and late hit restart full settle window", func(t *testing.T) {
		validator := sandboxRouteFenceValidator{settleWindow: settle, serveGrace: serveGrace}
		observations := []struct {
			at    time.Time
			state sandboxRouteProbeState
			hits  uint64
		}{
			{at: base, state: sandboxRouteRefused, hits: 4},
			{at: base.Add(time.Second), state: sandboxRouteServed, hits: 5},
			{at: base.Add(2 * time.Second), state: sandboxRouteRefused, hits: 5},
			{at: base.Add(4 * time.Second), state: sandboxRouteRefused, hits: 6},
		}
		for _, observation := range observations {
			settled, err := validator.observe(observation.at, observation.at, observation.state, observation.hits)
			if err != nil {
				t.Fatal(err)
			}
			if settled {
				t.Fatalf("route fence settled early at %s", observation.at.Sub(base))
			}
		}
		settledAt := base.Add(14 * time.Second)
		settled, err := validator.observe(settledAt, settledAt, sandboxRouteRefused, 6)
		if err != nil || !settled {
			t.Fatalf("route fence after reset window = (%t, %v), want settled", settled, err)
		}
	})

	t.Run("backend bytes after teardown grace fail", func(t *testing.T) {
		for name, observation := range map[string]struct {
			state sandboxRouteProbeState
			hits  uint64
		}{
			"served response": {state: sandboxRouteServed, hits: 5},
			"late hit":        {state: sandboxRouteRefused, hits: 5},
		} {
			t.Run(name, func(t *testing.T) {
				validator := sandboxRouteFenceValidator{settleWindow: settle, serveGrace: serveGrace}
				if _, err := validator.observe(base, base, sandboxRouteRefused, 4); err != nil {
					t.Fatal(err)
				}
				atGrace := base.Add(serveGrace)
				if _, err := validator.observe(atGrace, atGrace, observation.state, observation.hits); err == nil {
					t.Fatal("backend serving after the teardown grace was accepted")
				}
			})
		}
	})

	t.Run("first refusal after reset rejects a post-grace hit", func(t *testing.T) {
		validator := sandboxRouteFenceValidator{settleWindow: settle, serveGrace: serveGrace}
		if _, err := validator.observe(base, base, sandboxRouteRefused, 4); err != nil {
			t.Fatal(err)
		}
		withinGrace := base.Add(time.Second)
		if _, err := validator.observe(withinGrace, withinGrace, sandboxRouteServed, 5); err != nil {
			t.Fatal(err)
		}
		postGrace := base.Add(serveGrace + time.Second)
		if _, err := validator.observe(postGrace, postGrace, sandboxRouteRefused, 6); err == nil ||
			!strings.Contains(err.Error(), "late backend hit") {
			t.Fatalf("post-grace hit after reset = %v, want late-hit failure", err)
		}
	})

	t.Run("probe failure cannot hide a late backend hit", func(t *testing.T) {
		probeErr := errors.New("non-exact response after backend access")
		withinGrace := sandboxRouteFenceValidator{
			settleWindow: settle,
			serveGrace:   serveGrace,
			startedAt:    base,
			stableHits:   4,
		}
		withinGraceAt := base.Add(serveGrace - time.Nanosecond)
		if err := withinGrace.observeProbeFailure(withinGraceAt, withinGraceAt, probeErr, 5); err != nil {
			t.Fatalf("backend hit within teardown grace: %v", err)
		}
		if withinGrace.stableHits != 5 || !withinGrace.stableSince.IsZero() {
			t.Fatalf("probe-failure baseline = hits %d, stable since %s", withinGrace.stableHits, withinGrace.stableSince)
		}

		afterGrace := sandboxRouteFenceValidator{
			settleWindow: settle,
			serveGrace:   serveGrace,
			startedAt:    base,
			stableHits:   4,
		}
		atGrace := base.Add(serveGrace)
		if err := afterGrace.observeProbeFailure(atGrace, atGrace, probeErr, 5); err == nil {
			t.Fatal("probe failure hid a backend hit after the teardown grace")
		}

		crossesGrace := sandboxRouteFenceValidator{
			settleWindow: settle,
			serveGrace:   serveGrace,
			startedAt:    base,
			stableHits:   4,
		}
		startedNearGrace := base.Add(serveGrace - time.Nanosecond)
		completedAfterGrace := base.Add(serveGrace + time.Nanosecond)
		if err := crossesGrace.observeProbeFailure(startedNearGrace, completedAfterGrace, probeErr, 5); err == nil ||
			!strings.Contains(err.Error(), "late backend hit") {
			t.Fatalf("near-grace probe completion = %v, want late-hit failure", err)
		}
	})

	t.Run("unexpected command failures are not refusal", func(t *testing.T) {
		for name, res := range map[string]*runResult{
			"missing result": nil,
			"auth":           sandboxRouteResult(exitcode.Auth, "", "Error: Unauthorized (HTTP 401)\n"),
			"dns":            sandboxRouteResult(exitcode.Unavailable, "", "Error: lookup sandbox.invalid: no such host\n"),
			"start":          sandboxRouteResult(exitcode.General, "", "Error: fork/exec qurl: permission denied\n"),
			"generic 503":    sandboxRouteResult(exitcode.Unavailable, "", "Error: Service Unavailable (HTTP 503)\n"),
			"unexpected out": sandboxRouteResult(exitcode.Unavailable, "bytes", stoppedRefusal),
		} {
			t.Run(name, func(t *testing.T) {
				if err := validateSandboxStoppedRouteRefusal(res, stoppedRefusal); err == nil {
					t.Fatal("unexpected get failure accepted as stopped-route refusal")
				}
			})
		}
		if err := validateSandboxStoppedRouteRefusal(sandboxRouteResult(exitcode.Unavailable, "", stoppedRefusal), stoppedRefusal); err != nil {
			t.Fatalf("exact stopped-route refusal: %v", err)
		}
		if err := validateSandboxStoppedRouteRefusal(sandboxRouteResult(exitcode.Unavailable, "", stoppedRefusal), ""); err == nil {
			t.Fatal("empty stopped-route refusal contract accepted")
		}

		hostileStderr := "\x1b]8;;https://private.invalid\aendpoint\x1b]8;;\a\n::error::forged workflow command\nlv_live_NEVER_PRINT_THIS\n"
		hostile := sandboxRouteResult(exitcode.Unavailable, "", hostileStderr)
		err := validateSandboxStoppedRouteRefusal(hostile, stoppedRefusal)
		want := fmt.Sprintf(
			"get did not return the exact stopped-resource refusal: exit=%d stdout-bytes=0 stderr-bytes=%d",
			exitcode.Unavailable, len(hostileStderr),
		)
		if err == nil || err.Error() != want {
			t.Fatalf("hostile stopped-route diagnostic = %v, want closed metadata %q", err, want)
		}
		for _, forbidden := range []string{"private.invalid", "::error::", "lv_live_", "\x1b"} {
			if strings.Contains(err.Error(), forbidden) {
				t.Fatalf("hostile stopped-route diagnostic exposed %q", forbidden)
			}
		}

		ctx, cancel := context.WithCancel(context.Background())
		fenceErr := waitSandboxRouteFence(
			ctx, time.Millisecond, time.Second, time.Second, time.Second,
			func(context.Context) (sandboxRouteProbeState, error) {
				cancel()
				return 0, err
			},
			func() uint64 { return 0 },
		)
		if fenceErr == nil || !strings.Contains(fenceErr.Error(), want) {
			t.Fatalf("terminal fence diagnostic = %v, want closed validator metadata", fenceErr)
		}
		for _, forbidden := range []string{"private.invalid", "::error::", "lv_live_", "\x1b"} {
			if strings.Contains(fenceErr.Error(), forbidden) {
				t.Fatalf("terminal fence diagnostic exposed %q", forbidden)
			}
		}
	})

	t.Run("wait fails on final-read late hit", func(t *testing.T) {
		var probes atomic.Uint64
		var hitReads atomic.Uint64
		var lastHitProbe atomic.Uint64
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		err := waitSandboxRouteFence(
			ctx,
			time.Millisecond,
			time.Millisecond,
			5*time.Millisecond,
			time.Second,
			func(context.Context) (sandboxRouteProbeState, error) {
				probes.Add(1)
				return sandboxRouteRefused, nil
			},
			func() uint64 {
				hitReads.Add(1)
				probe := probes.Load()
				if probe > 0 && lastHitProbe.Swap(probe) == probe {
					return 1
				}
				return 0
			},
		)
		if err == nil || !strings.Contains(err.Error(), "backend hit after settling") ||
			!strings.Contains(err.Error(), "late backend hit") || hitReads.Load() <= probes.Load()+1 {
			t.Fatalf("final-read late hit result = %v; probes=%d reads=%d", err, probes.Load(), hitReads.Load())
		}
	})

	t.Run("wait tolerates a transient non-exact probe without accepting it", func(t *testing.T) {
		var probes atomic.Uint64
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		err := waitSandboxRouteFence(ctx, time.Millisecond, time.Millisecond, time.Second, time.Second, func(context.Context) (sandboxRouteProbeState, error) {
			if probes.Add(1) == 1 {
				return 0, errors.New("connection reset during route teardown")
			}
			return sandboxRouteRefused, nil
		}, func() uint64 { return 0 })
		if err != nil {
			t.Fatalf("transient route teardown error prevented exact fence: %v", err)
		}
		if probes.Load() < 3 {
			t.Fatalf("route fence settled without a full exact-refusal window: probes=%d", probes.Load())
		}
	})

	t.Run("in-grace probe cannot launder post-grace hit", func(t *testing.T) {
		var hits atomic.Uint64
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		err := waitSandboxRouteFence(ctx, time.Millisecond, time.Millisecond, 50*time.Millisecond, time.Second, func(probeCtx context.Context) (sandboxRouteProbeState, error) {
			deadline, ok := probeCtx.Deadline()
			if !ok {
				return 0, errors.New("route probe has no deadline")
			}
			if remaining := time.Until(deadline); remaining <= 0 || remaining > 50*time.Millisecond {
				return 0, fmt.Errorf("in-grace probe deadline is outside the absolute grace: %s", remaining)
			}
			<-probeCtx.Done()
			hits.Store(1)
			return 0, probeCtx.Err()
		}, hits.Load)
		if err == nil || !strings.Contains(err.Error(), "late backend hit") {
			t.Fatalf("post-grace backend hit result = %v, want late-hit failure", err)
		}
	})

	t.Run("permanent non-exact probe fails at the bound", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		err := waitSandboxRouteFence(ctx, time.Millisecond, time.Millisecond, time.Second, 20*time.Millisecond, func(context.Context) (sandboxRouteProbeState, error) {
			return 0, errors.New("authentication failed")
		}, func() uint64 { return 0 })
		if err == nil || !strings.Contains(err.Error(), "authentication failed") || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("permanent unexpected probe result = %v", err)
		}
	})

	t.Run("blocked probe observes cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		entered := make(chan struct{})
		result := make(chan error, 1)
		go func() {
			result <- waitSandboxRouteFence(ctx, time.Second, time.Second, time.Second, time.Second, func(ctx context.Context) (sandboxRouteProbeState, error) {
				close(entered)
				<-ctx.Done()
				return 0, ctx.Err()
			}, func() uint64 { return 0 })
		}()
		<-entered
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("blocked probe error = %v, want canceled", err)
		}
	})

	for name, timing := range map[string]struct {
		poll       time.Duration
		settle     time.Duration
		serveGrace time.Duration
		probe      time.Duration
	}{
		"zero poll":          {settle: time.Second, serveGrace: time.Second, probe: time.Second},
		"zero settle":        {poll: time.Second, serveGrace: time.Second, probe: time.Second},
		"zero serving grace": {poll: time.Second, settle: time.Second, probe: time.Second},
		"zero probe timeout": {poll: time.Second, settle: time.Second, serveGrace: time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			if err := waitSandboxRouteFence(context.Background(), timing.poll, timing.settle, timing.serveGrace, timing.probe, func(context.Context) (sandboxRouteProbeState, error) {
				return sandboxRouteRefused, nil
			}, func() uint64 { return 0 }); err == nil {
				t.Fatal("invalid route-fence timing accepted")
			}
		})
	}
}

func sandboxRouteResult(code int, stdout, stderr string) *runResult {
	res := &runResult{code: code}
	_, _ = res.stdout.WriteString(stdout)
	_, _ = res.stderr.WriteString(stderr)
	return res
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
	stateDir := connectorStateTestDir(t)
	if err := os.Chmod(stateDir, 0o700); err != nil { //nolint:gosec // A private directory must allow owner traversal as well as read/write.
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

func TestSandboxResourceCleanupIsSafeBeforePublish(t *testing.T) {
	const connectorID = "connector-not-created"
	var lookups atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/resources" || r.URL.Query().Get("slug") != connectorID {
			http.Error(w, "unexpected cleanup request", http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer device-token" {
			t.Errorf("resource lookup authorization = %q, want device credential", got)
		}
		lookups.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"data": []connectorResourceRow{}}); err != nil {
			t.Errorf("encode empty resource lookup: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	t.Run("cleanup registered before publish", func(t *testing.T) {
		registerSandboxResourceCleanup(t, server.URL, connectorID, "device-token")
	})
	if got := lookups.Load(); got != 1 {
		t.Fatalf("resource cleanup lookups = %d, want 1", got)
	}
}
