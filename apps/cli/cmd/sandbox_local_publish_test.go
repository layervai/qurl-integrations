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
	"os/exec"
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
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

const (
	localPublishSandboxArming = "QURL_CLI_SANDBOX_LOCAL_PUBLISH"
	sandboxCleanupTimeout     = 30 * time.Second
	sandboxRouteFenceTimeout  = 2 * time.Minute
	sandboxRouteFencePoll     = 500 * time.Millisecond
	sandboxRouteFenceSettle   = 2 * time.Second
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
// The private orchestrator runs this tagged test with one exact customer CLI
// artifact. It creates a native device in a fresh state directory, so the lane
// must provide a short-lived JWT that can revoke the resulting device
// credential. The JWT must represent the same owner as QURL_API_KEY: 404 is a
// cleanup failure, because a wrong-owner JWT also cannot see the new key. The
// resource and device key are both reclaimed before returning. Run explicitly:
//
//	QURL_CLI_SANDBOX_LOCAL_PUBLISH=enabled \
//	QURL_CLI_SANDBOX_BINARY=/absolute/path/to/qurl \
//	QURL_API_KEY=... QURL_ENDPOINT=... QURL_CLI_SANDBOX_CLEANUP_JWT=... \
//	QURL_CONNECTOR_HUB_HOST=... QURL_CONNECTOR_HUB_PORT=... \
//	QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64=... \
//	go test -tags=clisandbox -count=1 -run '^TestSandboxLocalPublishLifecycleSmoke$' ./apps/cli/cmd
func TestSandboxLocalPublishLifecycleSmoke(t *testing.T) {
	if os.Getenv(localPublishSandboxArming) != "enabled" {
		t.Skipf("SKIPPED LOUDLY: unified local-publish sandbox smoke is disarmed — %s != enabled", localPublishSandboxArming)
	}
	fixture := startSandboxLocalPublish(t, "smoke")
	defer fixture.stopAndValidate(t)
	binary, cliEnv, stateDir, local := fixture.binary, fixture.env, fixture.stateDir, fixture.local

	if assessment, err := cridux.Assess(local.CRID); err != nil || assessment.Kind != cridux.KindCRID {
		t.Fatalf("local publish registry CRID = %q, want a valid full CRID: %v", local.CRID, err)
	}

	initial := waitSandboxSharingState(t, binary, cliEnv, stateDir, local.CRID, "on", "serving", 2*time.Minute)
	assertSandboxListRow(t, binary, cliEnv, stateDir, local, initial.ServingEpoch)
	assertSandboxLocalRoute(t, binary, cliEnv, stateDir, local.CRID, fixture.marker, 2*time.Minute)

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
	for _, share := range shares {
		if share.CRID == local.CRID {
			t.Fatalf("deleted CRID %s remains in local daemon registry", local.CRID)
		}
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
	binary, err := validateSandboxCLIBinary(os.Getenv(sandboxCLIBinaryEnv))
	if err != nil {
		t.Fatalf("load exact customer CLI binary: %v", err)
	}
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
	namespace, err := sandboxNamespace(label)
	if err != nil {
		t.Fatalf("derive local-publish namespace: %v", err)
	}
	stateDir := t.TempDir()
	if err := os.Chmod(stateDir, 0o700); err != nil { //nolint:gosec // Agent state requires a private directory.
		t.Fatalf("secure local-publish state directory: %v", err)
	}
	cliEnv[state.EnvStateDirPrimary] = stateDir
	cliEnv[state.EnvAgentID] = namespace.AgentID

	fixture := &sandboxLocalFixture{
		binary: binary, env: cliEnv, stateDir: stateDir, cleanupJWT: cleanupJWT,
		key: cliEnv["QURL_API_KEY"], marker: "sandbox-local-publish-" + namespace.ConnectorID,
	}
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fixture.backendHits.Add(1)
		_, _ = io.WriteString(w, fixture.marker)
	}))
	t.Cleanup(echo.Close)

	fixture.process = startSandboxPublishProcess(t, binary, cliEnv, namespace, stateDir, echo.URL)
	fixture.process.registerRecoveryCleanup(t, cliEnv["QURL_ENDPOINT"], cleanupJWT, namespace, stateDir, productionSandboxSiblingCleanupOps())
	crid := fixture.process.waitReady(t)
	shares, present, err := state.ReadLocalSharesIfPresent(context.Background(), stateDir)
	if err != nil || !present || len(shares) != 1 {
		fixture.forceStop(t)
		t.Fatalf("read exact local-publish registry = (%d shares, present %v, %v), want one", len(shares), present, err)
	}
	fixture.local = &shares[0]
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

func (f *sandboxLocalFixture) stopAndValidate(t *testing.T) {
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

func runSandboxLocalCLI(t *testing.T, binary string, env map[string]string, stateDir string, args ...string) *runResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	return runSandboxLocalCLIContext(t, ctx, binary, env, stateDir, args...)
}

func runSandboxLocalCLIContext(t *testing.T, ctx context.Context, binary string, env map[string]string, stateDir string, args ...string) *runResult {
	t.Helper()
	commandArgs := append([]string{"--endpoint", env["QURL_ENDPOINT"]}, args...)
	cmd := exec.CommandContext(ctx, binary, commandArgs...) //nolint:gosec // The protected test validates the fixed binary and supplies closed arguments.
	commandEnv := cloneSandboxEnv(env)
	commandEnv[state.EnvStateDirPrimary] = stateDir
	cmd.Env = sandboxCommandEnv(commandEnv)
	cmd.Stdin = http.NoBody
	res := &runResult{}
	cmd.Stdout = &res.stdout
	cmd.Stderr = &res.stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.code = exitErr.ExitCode()
		} else {
			res.code = 1
		}
	}
	assertSandboxStreamsDoNotContainSecrets(t, res, env["QURL_API_KEY"])
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

func assertSandboxLocalRoute(t *testing.T, binary string, env map[string]string, stateDir, crid, marker string, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	var last string
	for time.Now().Before(deadline) {
		if err := sandboxLocalRouteOnce(t, binary, env, stateDir, crid, marker); err == nil {
			return
		} else {
			last = err.Error()
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("public qURL route for %s did not deliver the local backend bytes: %s", crid, last)
}

func sandboxLocalRouteOnce(t *testing.T, binary string, env map[string]string, stateDir, crid, marker string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	return sandboxLocalRouteOnceContext(t, ctx, binary, env, stateDir, crid, marker)
}

func sandboxLocalRouteOnceContext(t *testing.T, ctx context.Context, binary string, env map[string]string, stateDir, crid, marker string) error {
	t.Helper()
	probeState, err := probeSandboxLocalRoute(
		t, ctx, binary, env, stateDir, crid, marker,
		filepath.Join(t.TempDir(), "payload"), sandboxStoppedRouteRefusal(t),
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

func probeSandboxLocalRoute(t *testing.T, ctx context.Context, binary string, env map[string]string, stateDir, crid, marker, dest, stoppedRefusal string) (sandboxRouteProbeState, error) {
	t.Helper()
	if err := os.Remove(dest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, errors.New("clear prior route-probe destination")
	}
	res := runSandboxLocalCLIContext(t, ctx, binary, env, stateDir, "get", crid, "--file", dest)
	if res.code != 0 {
		if err := validateSandboxStoppedRouteRefusal(res, stoppedRefusal); err != nil {
			return 0, err
		}
		if _, err := os.Lstat(dest); !errors.Is(err, os.ErrNotExist) {
			return 0, errors.New("stopped-route refusal left a destination file")
		}
		return sandboxRouteRefused, nil
	}
	payload, err := os.ReadFile(dest)
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
	if stoppedRefusal == "" {
		return errors.New("stopped-route refusal contract is empty")
	}
	if res.code != exitcode.Unavailable || res.stdout.Len() != 0 || res.stderr.String() != stoppedRefusal {
		return fmt.Errorf("get did not return the exact stopped-resource refusal: exit=%d stdout-bytes=%d stderr=%q", res.code, res.stdout.Len(), res.stderr.String())
	}
	return nil
}

func sandboxStoppedRouteRefusal(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "golden", "error_dark503.plain.stderr.golden"))
	if err != nil {
		t.Fatalf("read stopped-route refusal golden: %v", err)
	}
	return string(data)
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
		func(ctx context.Context) (sandboxRouteProbeState, error) {
			return probeSandboxLocalRoute(t, ctx, binary, env, stateDir, crid, marker, dest, stoppedRefusal)
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
	probe func(context.Context) (sandboxRouteProbeState, error),
	backendHits func() uint64,
) error {
	if pollInterval <= 0 || settleWindow <= 0 {
		return errors.New("route-fence poll and settle durations must be positive")
	}
	validator := sandboxRouteFenceValidator{settleWindow: settleWindow, last: "route fence was not sampled"}
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("stopped local route did not settle before the bound: %s: %w", validator.last, ctx.Err())
		default:
		}
		probeState, err := probe(ctx)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fmt.Errorf("stopped local route probe did not finish before the bound: %w", ctxErr)
			}
			return fmt.Errorf("stopped local route probe failed without the exact refusal: %w", err)
		}
		hits := backendHits()
		now := time.Now()
		settled, err := validator.observe(now, probeState, hits)
		if err != nil {
			return err
		}
		if settled {
			// Close the sampling race before returning. A hit that landed after the
			// first atomic load restarts the full settle window.
			finalHits := backendHits()
			if finalHits == hits {
				return nil
			}
			if _, err := validator.observe(time.Now(), sandboxRouteRefused, finalHits); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("stopped local route did not settle before the bound: %s: %w", validator.last, ctx.Err())
		case <-ticker.C:
		}
	}
}

type sandboxRouteFenceValidator struct {
	settleWindow time.Duration
	stableSince  time.Time
	stableHits   uint64
	last         string
}

func (v *sandboxRouteFenceValidator) observe(now time.Time, probeState sandboxRouteProbeState, hits uint64) (bool, error) {
	if v.settleWindow <= 0 {
		return false, errors.New("route-fence settle duration must be positive")
	}
	switch probeState {
	case sandboxRouteServed:
		v.stableSince = time.Time{}
		v.stableHits = hits
		v.last = fmt.Sprintf("stopped local route still served its backend bytes at %d hits", hits)
		return false, nil
	case sandboxRouteRefused:
	default:
		return false, errors.New("route probe returned an unknown state")
	}
	if v.stableSince.IsZero() {
		v.stableSince = now
		v.stableHits = hits
		v.last = fmt.Sprintf("route refusal has not stayed stable for %s", v.settleWindow)
		return false, nil
	}
	if hits != v.stableHits {
		priorHits := v.stableHits
		v.stableSince = now
		v.stableHits = hits
		v.last = fmt.Sprintf("late backend hit changed the count from %d to %d", priorHits, hits)
		return false, nil
	}
	stableFor := now.Sub(v.stableSince)
	if stableFor >= v.settleWindow {
		return true, nil
	}
	v.last = fmt.Sprintf("route refusal has stayed stable for %s of %s", stableFor, v.settleWindow)
	return false, nil
}

// Each stop, start, and restart changes the durable lifecycle authority. The
// serving epoch must therefore advance. An equal epoch is stale authority, not
// successful convergence, even when the desired and observed states match.
func validateSandboxSharingTransition(doc sandboxSharingDoc, desired, observed string, priorEpoch uint64) error {
	if doc.DesiredState != desired || doc.ConnectionState != observed {
		return fmt.Errorf("want %s/%s", desired, observed)
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
	stateDir := t.TempDir()
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
	stoppedRefusal := sandboxStoppedRouteRefusal(t)

	t.Run("stable exact refusal settles", func(t *testing.T) {
		validator := sandboxRouteFenceValidator{settleWindow: settle}
		for _, observation := range []struct {
			at      time.Time
			settled bool
		}{
			{at: base},
			{at: base.Add(settle - time.Nanosecond)},
			{at: base.Add(settle), settled: true},
		} {
			settled, err := validator.observe(observation.at, sandboxRouteRefused, 4)
			if err != nil {
				t.Fatal(err)
			}
			if settled != observation.settled {
				t.Fatalf("observation at %s settled=%t, want %t", observation.at.Sub(base), settled, observation.settled)
			}
		}
	})

	t.Run("serving and late hit restart full settle window", func(t *testing.T) {
		validator := sandboxRouteFenceValidator{settleWindow: settle}
		observations := []struct {
			at    time.Time
			state sandboxRouteProbeState
			hits  uint64
		}{
			{at: base, state: sandboxRouteRefused, hits: 4},
			{at: base.Add(9 * time.Second), state: sandboxRouteServed, hits: 5},
			{at: base.Add(10 * time.Second), state: sandboxRouteRefused, hits: 5},
			{at: base.Add(19 * time.Second), state: sandboxRouteRefused, hits: 6},
			{at: base.Add(28 * time.Second), state: sandboxRouteRefused, hits: 6},
		}
		for _, observation := range observations {
			settled, err := validator.observe(observation.at, observation.state, observation.hits)
			if err != nil {
				t.Fatal(err)
			}
			if settled {
				t.Fatalf("route fence settled early at %s", observation.at.Sub(base))
			}
		}
		settled, err := validator.observe(base.Add(29*time.Second), sandboxRouteRefused, 7)
		if err != nil || settled {
			t.Fatalf("same-time late hit = (%t, %v), want full reset", settled, err)
		}
		settled, err = validator.observe(base.Add(39*time.Second), sandboxRouteRefused, 7)
		if err != nil || !settled {
			t.Fatalf("route fence after reset window = (%t, %v), want settled", settled, err)
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
	})

	t.Run("wait settles only after final-read late hit", func(t *testing.T) {
		var hitReads atomic.Uint64
		started := time.Now()
		err := waitSandboxRouteFence(
			context.Background(),
			time.Millisecond,
			time.Microsecond,
			func(context.Context) (sandboxRouteProbeState, error) { return sandboxRouteRefused, nil },
			func() uint64 {
				if hitReads.Add(1) >= 3 {
					return 1
				}
				return 0
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		if hitReads.Load() < 5 || time.Since(started) < 2*time.Millisecond {
			t.Fatalf("late final-read hit did not restart the settle loop: reads=%d elapsed=%s", hitReads.Load(), time.Since(started))
		}
	})

	t.Run("wait fails immediately on unexpected probe error", func(t *testing.T) {
		err := waitSandboxRouteFence(context.Background(), time.Second, time.Second, func(context.Context) (sandboxRouteProbeState, error) {
			return 0, errors.New("authentication failed")
		}, func() uint64 { return 0 })
		if err == nil || !strings.Contains(err.Error(), "without the exact refusal") {
			t.Fatalf("unexpected probe result = %v", err)
		}
	})

	t.Run("blocked probe observes cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		entered := make(chan struct{})
		result := make(chan error, 1)
		go func() {
			result <- waitSandboxRouteFence(ctx, time.Second, time.Second, func(ctx context.Context) (sandboxRouteProbeState, error) {
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
		poll   time.Duration
		settle time.Duration
	}{
		"zero poll":   {settle: time.Second},
		"zero settle": {poll: time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			if err := waitSandboxRouteFence(context.Background(), timing.poll, timing.settle, func(context.Context) (sandboxRouteProbeState, error) {
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
