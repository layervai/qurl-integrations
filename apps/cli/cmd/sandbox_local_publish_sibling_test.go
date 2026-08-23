//go:build clisandbox

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/layervai/qurl-go/qurl"

	connectoragent "github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
)

const (
	sandboxSiblingArmingEnv = "QURL_CLI_SANDBOX_SIBLING_CONTINUITY"
	sandboxCLIBinaryEnv     = "QURL_CLI_SANDBOX_BINARY"
	sandboxProcessTimeout   = 60 * time.Second
)

// TestSandboxLocalPublishSiblingContinuity runs the released customer binary
// as two independent processes. It checks bytes through both live routes,
// retires A while B stays usable, restarts A from the same durable state and
// resource, checks both routes again, and then retires both exact sessions.
func TestSandboxLocalPublishSiblingContinuity(t *testing.T) {
	if os.Getenv(sandboxSiblingArmingEnv) != "enabled" {
		t.Skipf("SKIPPED LOUDLY: sibling-continuity journey is disarmed — %s != enabled", sandboxSiblingArmingEnv)
	}
	binary, err := validateSandboxCLIBinary(os.Getenv(sandboxCLIBinaryEnv))
	if err != nil {
		t.Fatalf("load exact customer CLI binary: %v", err)
	}
	cleanupJWT, err := readSandboxSecretFile(sandboxCleanupJWTFileEnv, "QURL_CLI_SANDBOX_CLEANUP_JWT")
	if err != nil {
		t.Fatalf("load protected sandbox cleanup JWT: %v", err)
	}
	cliEnv := sandboxJourneyEnv(t)
	endpoint := cliEnv["QURL_ENDPOINT"]
	apiKey := cliEnv["QURL_API_KEY"]

	namespaceA, err := sandboxNamespace("sibling-a")
	if err != nil {
		t.Fatalf("derive sibling A namespace: %v", err)
	}
	namespaceB, err := sandboxNamespace("sibling-b")
	if err != nil {
		t.Fatalf("derive sibling B namespace: %v", err)
	}
	if namespaceA == namespaceB {
		t.Fatal("sibling namespaces are not distinct")
	}

	const bodyA = "qurl-sibling-a-live-bytes\n"
	const bodyB = "qurl-sibling-b-live-bytes\n"
	targetA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, bodyA)
	}))
	t.Cleanup(targetA.Close)
	targetB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, bodyB)
	}))
	t.Cleanup(targetB.Close)

	stateDirA := t.TempDir()
	stateDirB := t.TempDir()
	for _, dir := range []string{stateDirA, stateDirB} {
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // Agent state requires a private directory.
			t.Fatalf("secure sibling state directory: %v", err)
		}
	}
	processA := startSandboxPublishProcess(t, binary, cliEnv, namespaceA, stateDirA, targetA.URL)
	processA.registerRecoveryCleanup(t, endpoint, cleanupJWT, namespaceA, stateDirA, productionSandboxSiblingCleanupOps())
	cridA := processA.waitReady(t)
	assertSandboxSiblingIdentity(t, namespaceA, stateDirA)
	processB := startSandboxPublishProcess(t, binary, cliEnv, namespaceB, stateDirB, targetB.URL)
	processB.registerRecoveryCleanup(t, endpoint, cleanupJWT, namespaceB, stateDirB, productionSandboxSiblingCleanupOps())
	cridB := processB.waitReady(t)
	assertSandboxSiblingIdentity(t, namespaceB, stateDirB)
	if cridA == cridB {
		t.Fatalf("siblings published the same CRID %q", cridA)
	}

	assertSandboxGetBytes(t, binary, cliEnv, cridA, bodyA)
	assertSandboxGetBytes(t, binary, cliEnv, cridB, bodyB)
	firstARunID := processA.stopAndValidate(t, apiKey, cleanupJWT)
	processB.requireRunning(t, "after sibling A retirement")
	assertSandboxGetBytes(t, binary, cliEnv, cridB, bodyB)

	replacementA := startSandboxPublishProcess(t, binary, cliEnv, namespaceA, stateDirA, targetA.URL)
	replacementCRIDA := replacementA.waitReady(t)
	if replacementCRIDA != cridA {
		t.Fatalf("replacement A CRID = %q, want durable resource %q", replacementCRIDA, cridA)
	}
	assertSandboxGetBytes(t, binary, cliEnv, cridA, bodyA)
	processB.requireRunning(t, "after sibling A replacement")
	assertSandboxGetBytes(t, binary, cliEnv, cridB, bodyB)

	replacementARunID := replacementA.stopAndValidate(t, apiKey, cleanupJWT)
	if replacementARunID == firstARunID {
		t.Fatalf("replacement A reused admitted RunID %q", firstARunID)
	}
	processB.stopAndValidate(t, apiKey, cleanupJWT)
}

type lockedSandboxBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedSandboxBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedSandboxBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type sandboxPublishProcess struct {
	label  string
	cmd    *exec.Cmd
	stdout lockedSandboxBuffer
	stderr lockedSandboxBuffer
	done   chan struct{}

	waitMu  sync.Mutex
	waitErr error
	stopped bool
}

func startSandboxPublishProcess(
	t *testing.T,
	binary string,
	baseEnv map[string]string,
	namespace sandboxRunNamespace,
	stateDir string,
	targetURL string,
) *sandboxPublishProcess {
	t.Helper()
	if namespace.AgentID == "" || namespace.ConnectorID == "" {
		t.Fatal("sandbox publish process received an empty namespace")
	}
	env := cloneSandboxEnv(baseEnv)
	env[state.EnvStateDirPrimary] = stateDir
	env[state.EnvAgentID] = namespace.AgentID
	for _, name := range []string{hub.EnvHost, hub.EnvPort, hub.EnvServerPublicKey} {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			t.Fatalf("sandbox publish process is missing %s", name)
		}
		env[name] = value
	}

	p := &sandboxPublishProcess{label: namespace.ConnectorID, done: make(chan struct{})}
	p.cmd = exec.CommandContext(context.Background(), binary, "--endpoint", baseEnv["QURL_ENDPOINT"], "--quiet", "publish", targetURL, "--id", namespace.ConnectorID) //nolint:gosec // The protected test validates the fixed binary path and supplies closed arguments.
	p.cmd.Env = sandboxCommandEnv(env)
	p.cmd.Stdout = &p.stdout
	p.cmd.Stderr = &p.stderr
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start sandbox publish %s: %v", p.label, err)
	}
	go func() {
		err := p.cmd.Wait()
		p.waitMu.Lock()
		p.waitErr = err
		p.waitMu.Unlock()
		close(p.done)
	}()
	t.Cleanup(func() { p.forceStop(t) })
	return p
}

func (p *sandboxPublishProcess) waitReady(t *testing.T) string {
	t.Helper()
	crid, err := p.waitReadyResult(sandboxProcessTimeout)
	if err != nil {
		t.Fatalf("sandbox publish %s readiness: %v\nstderr: %s", p.label, err, p.stderr.String())
	}
	return crid
}

func (p *sandboxPublishProcess) waitReadyResult(timeout time.Duration) (string, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		stdoutRaw := p.stdout.String()
		stdout := strings.TrimSpace(stdoutRaw)
		stderr := p.stderr.String()
		if strings.Contains(stderr, "event=proxy_ready") && strings.HasSuffix(stdoutRaw, "\n") {
			assessment, err := cridux.Assess(stdout)
			if err != nil {
				return "", fmt.Errorf("stdout = %q, want one CRID: %w", stdout, err)
			}
			if assessment.Kind != cridux.KindCRID {
				return "", fmt.Errorf("stdout = %q, want one CRID", stdout)
			}
			admitted, admittedErr := sandboxEventRunID(stderr, "login_success")
			ready, readyErr := sandboxEventRunID(stderr, "proxy_ready")
			if admittedErr != nil {
				return "", fmt.Errorf("admitted RunID: %w", admittedErr)
			}
			if readyErr != nil {
				return "", fmt.Errorf("ready RunID: %w", readyErr)
			}
			if admitted != ready {
				return "", fmt.Errorf("ready RunID %q does not match admitted RunID %q", ready, admitted)
			}
			return stdout, nil
		}
		select {
		case <-p.done:
			p.waitMu.Lock()
			waitErr := p.waitErr
			p.waitMu.Unlock()
			return "", fmt.Errorf("exited before readiness: %w", waitErr)
		case <-deadline.C:
			return "", fmt.Errorf("did not become ready within %s", timeout)
		case <-ticker.C:
		}
	}
}

func (p *sandboxPublishProcess) requireRunning(t *testing.T, phase string) {
	t.Helper()
	select {
	case <-p.done:
		p.waitMu.Lock()
		waitErr := p.waitErr
		p.waitMu.Unlock()
		t.Fatalf("sandbox publish %s exited %s: %v\nstderr: %s", p.label, phase, waitErr, p.stderr.String())
	default:
	}
}

func (p *sandboxPublishProcess) stopAndValidate(t *testing.T, secrets ...string) string {
	t.Helper()
	if p.stopped {
		t.Fatalf("sandbox publish %s was stopped twice", p.label)
	}
	p.stopped = true
	p.requireRunning(t, "before requested retirement")
	if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("interrupt sandbox publish %s: %v", p.label, err)
	}
	select {
	case <-p.done:
	case <-time.After(sandboxProcessTimeout):
		t.Fatalf("sandbox publish %s did not stop within %s", p.label, sandboxProcessTimeout)
	}
	p.waitMu.Lock()
	waitErr := p.waitErr
	p.waitMu.Unlock()
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 130 {
		t.Fatalf("sandbox publish %s exit = %v, want 130 after interrupt\nstderr: %s", p.label, waitErr, p.stderr.String())
	}
	stderr := p.stderr.String()
	stdout := p.stdout.String()
	if err := validateSandboxPublishRetirement(stderr); err != nil {
		t.Fatalf("sandbox publish %s retirement: %v\nstderr: %s", p.label, err, stderr)
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(stdout+stderr, secret) {
			t.Fatalf("sandbox publish %s exposed a protected credential", p.label)
		}
	}
	runID, _ := sandboxEventRunID(stderr, "login_success")
	return runID
}

func (p *sandboxPublishProcess) forceStop(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Signal(os.Interrupt)
	select {
	case <-p.done:
		return
	case <-time.After(5 * time.Second):
	}
	_ = p.cmd.Process.Kill()
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Errorf("sandbox publish %s could not be reaped", p.label)
	}
}

type sandboxSiblingCleanupOps struct {
	loadState      func(string) (*qurl.AgentState, error)
	deleteResource func(context.Context, string, string, string) error
	revokeDevice   func(context.Context, string, string, string) error
}

func productionSandboxSiblingCleanupOps() sandboxSiblingCleanupOps {
	return sandboxSiblingCleanupOps{
		loadState:      loadSandboxSiblingState,
		deleteResource: deleteSandboxSiblingResource,
		revokeDevice: func(ctx context.Context, endpoint, jwt, deviceKeyID string) error {
			return revokeSandboxDeviceCredential(ctx, sandboxCleanupHTTPClient, endpoint, jwt, deviceKeyID)
		},
	}
}

func (p *sandboxPublishProcess) registerRecoveryCleanup(
	t *testing.T,
	endpoint string,
	cleanupJWT string,
	namespace sandboxRunNamespace,
	stateDir string,
	ops sandboxSiblingCleanupOps,
) {
	t.Helper()
	// This cleanup is registered immediately after Start. It first stops and
	// reaps the process so no delayed admission can race remote deletion. It
	// then recovers any durable state written before readiness, deletes the
	// resource, and finally revokes the device credential.
	t.Cleanup(func() {
		p.forceStop(t)
		ctx, cancel := context.WithTimeout(context.Background(), sandboxCleanupTimeout)
		defer cancel()
		if err := cleanupSandboxSiblingAuthority(ctx, endpoint, cleanupJWT, namespace, stateDir, ops); err != nil {
			t.Error(err)
		}
	})
}

func cleanupSandboxSiblingAuthority(
	ctx context.Context,
	endpoint string,
	cleanupJWT string,
	namespace sandboxRunNamespace,
	stateDir string,
	ops sandboxSiblingCleanupOps,
) error {
	loaded, err := ops.loadState(stateDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("load sandbox sibling state for recovery cleanup failed")
	}
	if err := validateSandboxDeviceIdentity(loaded, namespace.AgentID, ""); err != nil {
		return fmt.Errorf("sandbox sibling cleanup identity: %w", err)
	}
	if err := ops.deleteResource(ctx, endpoint, namespace.ConnectorID, loaded.DeviceAPIKey); err != nil {
		// Preserve the device credential when resource deletion fails so the
		// serialized next-run sweeper still has authority to retry.
		return fmt.Errorf("sandbox sibling resource cleanup: %w", err)
	}
	if err := ops.revokeDevice(ctx, endpoint, cleanupJWT, loaded.DeviceAPIKeyID); err != nil {
		return fmt.Errorf("sandbox sibling device cleanup: %w", err)
	}
	return nil
}

func loadSandboxSiblingState(stateDir string) (*qurl.AgentState, error) {
	path := filepath.Join(stateDir, state.AgentStateFile)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("sandbox sibling state is not a regular file")
	}
	store, err := qurl.OpenFileAgentState(path)
	if err != nil {
		return nil, err
	}
	loaded, loadErr := store.LoadAgentState(context.Background())
	closeErr := store.Close()
	if loadErr != nil {
		return nil, loadErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if loaded == nil {
		return nil, errors.New("sandbox sibling state is empty")
	}
	return loaded, nil
}

func deleteSandboxSiblingResource(ctx context.Context, endpoint, connectorID, deviceAPIKey string) error {
	origin, err := connectoragent.ResourceSDKOrigin(endpoint)
	if err != nil {
		return errors.New("derive sandbox sibling resource API origin failed")
	}
	client, err := qurl.NewClient(qurl.BearerToken(deviceAPIKey), qurl.WithBaseURL(origin))
	if err != nil {
		return errors.New("open sandbox sibling resource client failed")
	}
	resource, err := client.GetConnectorResourceBySlug(ctx, connectorID)
	if errors.Is(err, qurl.ErrConnectorResourceNotFound) {
		return nil
	}
	if err != nil || resource == nil {
		return errors.New("find sandbox sibling resource failed")
	}
	if err := client.DeleteConnectorResource(ctx, resource.ResourceID); err != nil && !errors.Is(err, qurl.ErrConnectorResourceNotFound) {
		return errors.New("delete sandbox sibling resource failed")
	}
	return nil
}

func validateSandboxPublishRetirement(stderr string) error {
	for _, failure := range []string{"session_retirement_failed", "nhp_session_exit_failed"} {
		if strings.Contains(stderr, failure) {
			return fmt.Errorf("reported %s", failure)
		}
	}
	admitted, err := sandboxEventRunID(stderr, "login_success")
	if err != nil {
		return fmt.Errorf("admitted RunID: %w", err)
	}
	retired, err := sandboxEventRunID(stderr, "nhp_session_retired")
	if err != nil {
		return fmt.Errorf("retired RunID: %w", err)
	}
	ready, err := sandboxEventRunID(stderr, "proxy_ready")
	if err != nil {
		return fmt.Errorf("ready RunID: %w", err)
	}
	if admitted != ready || admitted != retired {
		return fmt.Errorf("ready RunID %q and retired RunID %q do not match admitted RunID %q", ready, retired, admitted)
	}
	return nil
}

func assertSandboxGetBytes(t *testing.T, binary string, baseEnv map[string]string, crid, want string) {
	t.Helper()
	destination := filepath.Join(t.TempDir(), "download")
	ctx, cancel := context.WithTimeout(context.Background(), sandboxProcessTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--endpoint", baseEnv["QURL_ENDPOINT"], "--quiet", "get", crid, "--file", destination) //nolint:gosec // The protected test validates the fixed binary and CRID.
	cmd.Env = sandboxCommandEnv(baseEnv)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("get %s failed: %v\nstdout: %s\nstderr: %s", crid, err, stdout.String(), stderr.String())
	}
	if secret := baseEnv["QURL_API_KEY"]; secret != "" && strings.Contains(stdout.String()+stderr.String(), secret) {
		t.Fatal("get command exposed the protected API key")
	}
	got, err := os.ReadFile(destination) //nolint:gosec // destination is an exact test-owned TempDir child.
	if err != nil {
		t.Fatalf("read downloaded bytes for %s: %v", crid, err)
	}
	if err := validateSandboxDownloadedBytes(got, []byte(want)); err != nil {
		t.Fatalf("get %s: %v", crid, err)
	}
}

func validateSandboxDownloadedBytes(got, want []byte) error {
	if !bytes.Equal(got, want) {
		return fmt.Errorf("downloaded bytes differ: got %d bytes, want %d", len(got), len(want))
	}
	return nil
}

func assertSandboxSiblingIdentity(t *testing.T, namespace sandboxRunNamespace, stateDir string) {
	t.Helper()
	loaded := loadSandboxAgentState(t, stateDir)
	if err := validateSandboxDeviceIdentity(loaded, namespace.AgentID, ""); err != nil {
		t.Fatalf("sandbox sibling %s identity: %v", namespace.ConnectorID, err)
	}
}

func cloneSandboxEnv(input map[string]string) map[string]string {
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func sandboxCommandEnv(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env
}

func validateSandboxCLIBinary(raw string) (string, error) {
	path := strings.TrimSpace(raw)
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("customer CLI binary path must be an exact absolute path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", errors.New("customer CLI binary is unavailable")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("customer CLI binary must be one executable regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("customer CLI binary metadata is unavailable")
	}
	if err := validateSandboxCLIBinaryMetadata(info.Mode(), stat.Uid, uint32(os.Geteuid()), uint64(stat.Nlink)); err != nil {
		return "", err
	}
	return path, nil
}

func validateSandboxCLIBinaryMetadata(mode os.FileMode, ownerUID, effectiveUID uint32, links uint64) error {
	if !mode.IsRegular() || mode.Perm()&0o111 == 0 || mode.Perm()&0o022 != 0 || links != 1 {
		return errors.New("customer CLI binary must be one non-writable executable regular file")
	}
	if ownerUID != effectiveUID && ownerUID != 0 {
		return errors.New("customer CLI binary must be owned by the current user or root")
	}
	return nil
}

func TestValidateSandboxCLIBinary(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "qurl")
	if err := os.WriteFile(valid, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(valid, 0o700); err != nil { //nolint:gosec // The fixture must be executable.
		t.Fatal(err)
	}
	if got, err := validateSandboxCLIBinary(valid); err != nil || got != valid {
		t.Fatalf("valid binary = %q, %v", got, err)
	}
	for name, path := range map[string]string{
		"missing":      filepath.Join(dir, "missing"),
		"relative":     "qurl",
		"directory":    dir,
		"unclean-path": filepath.Join(dir, ".", "qurl") + string(filepath.Separator) + ".." + string(filepath.Separator) + "qurl",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateSandboxCLIBinary(path); err == nil {
				t.Fatalf("invalid binary path %q accepted", path)
			}
		})
	}
	nonExecutable := filepath.Join(dir, "not-executable")
	if err := os.WriteFile(nonExecutable, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := validateSandboxCLIBinary(nonExecutable); err == nil {
		t.Fatal("non-executable binary accepted")
	}
	symlink := filepath.Join(dir, "link")
	if err := os.Symlink(valid, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := validateSandboxCLIBinary(symlink); err == nil {
		t.Fatal("symlink binary accepted")
	}
	for name, fixture := range map[string]struct {
		mode  os.FileMode
		owner uint32
		links uint64
		ok    bool
	}{
		"effective-user-owned": {mode: 0o500, owner: 65532, links: 1, ok: true},
		"root-owned":           {mode: 0o555, owner: 0, links: 1, ok: true},
		"writable-root-owned":  {mode: 0o577, owner: 0, links: 1},
		"foreign-owned":        {mode: 0o555, owner: 1000, links: 1},
		"hard-linked":          {mode: 0o555, owner: 0, links: 2},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateSandboxCLIBinaryMetadata(fixture.mode, fixture.owner, 65532, fixture.links)
			if (err == nil) != fixture.ok {
				t.Fatalf("metadata validation = %v, want accepted=%t", err, fixture.ok)
			}
		})
	}
}

func TestSandboxSiblingFailureClassifiers(t *testing.T) {
	validLog := "event=login_success run_id=01abcdef23456789\n" +
		"event=proxy_ready run_id=01abcdef23456789\n" +
		"event=nhp_session_retired run_id=01abcdef23456789\n"
	if err := validateSandboxPublishRetirement(validLog); err != nil {
		t.Fatal(err)
	}
	for name, logText := range map[string]string{
		"absent retirement": strings.ReplaceAll(validLog, "event=nhp_session_retired", "event=stopped"),
		"wrong retirement":  strings.ReplaceAll(validLog, "event=nhp_session_retired run_id=01abcdef23456789", "event=nhp_session_retired run_id=02abcdef23456789"),
		"failure marker":    validLog + "event=session_retirement_failed\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateSandboxPublishRetirement(logText); err == nil {
				t.Fatalf("invalid retirement log accepted: %s", name)
			}
		})
	}
	if err := validateSandboxDownloadedBytes([]byte("changed"), []byte("sibling")); err == nil {
		t.Fatal("sibling byte drift accepted")
	}
}

func TestSandboxPublishProcessReportsEarlyExit(t *testing.T) {
	script := filepath.Join(t.TempDir(), "qurl")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(script, 0o700); err != nil { //nolint:gosec // The fixture must be executable.
		t.Fatal(err)
	}
	for _, name := range []string{hub.EnvHost, hub.EnvPort, hub.EnvServerPublicKey} {
		t.Setenv(name, "fixture")
	}
	process := startSandboxPublishProcess(t, script, map[string]string{"QURL_ENDPOINT": "https://sandbox.invalid"}, sandboxRunNamespace{
		AgentID: "qurl-share-r1-a1-ha", ConnectorID: "connector-sandbox-local-publish-early",
	}, t.TempDir(), "http://127.0.0.1:1")
	select {
	case <-process.done:
	case <-time.After(5 * time.Second):
		t.Fatal("early-exit fixture did not terminate")
	}
	if _, err := process.waitReadyResult(time.Second); err == nil || !strings.Contains(err.Error(), "exited before readiness") {
		t.Fatalf("early-exit readiness result = %v", err)
	}
}

func TestSandboxProcessRecoveryCleanupAfterPreReadyFailure(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		script string
		wait   time.Duration
		want   string
	}{
		{name: "enrolled then early exit", script: "#!/bin/sh\nexit 7\n", wait: time.Second, want: "exited before readiness"},
		{name: "enrolled then readiness timeout", script: "#!/bin/sh\nexec sleep 60\n", wait: 50 * time.Millisecond, want: "did not become ready"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			for _, name := range []string{hub.EnvHost, hub.EnvPort, hub.EnvServerPublicKey} {
				t.Setenv(name, "fixture")
			}
			namespace := sandboxRunNamespace{
				AgentID: "qurl-share-r1-a1-ha", ConnectorID: "connector-sandbox-local-publish-recovery",
			}
			stateDir := t.TempDir()
			writeSandboxSiblingStateFixture(t, stateDir, namespace.AgentID)
			binary := filepath.Join(t.TempDir(), "qurl")
			if err := os.WriteFile(binary, []byte(fixture.script), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(binary, 0o700); err != nil { //nolint:gosec // The fixture must be executable.
				t.Fatal(err)
			}
			order := []string{}
			t.Cleanup(func() {
				if got := strings.Join(order, ","); got != "resource,device" {
					t.Errorf("recovery cleanup order = %q, want resource,device", got)
				}
			})
			process := startSandboxPublishProcess(t, binary, map[string]string{"QURL_ENDPOINT": "https://sandbox.invalid"}, namespace, stateDir, "http://127.0.0.1:1")
			process.registerRecoveryCleanup(t, "https://sandbox.invalid", "cleanup-jwt", namespace, stateDir, sandboxSiblingCleanupOps{
				loadState: loadSandboxSiblingState,
				deleteResource: func(_ context.Context, _ string, connectorID, key string) error {
					if connectorID != namespace.ConnectorID || key != "device-secret" {
						t.Errorf("resource cleanup authority = %q/%q", connectorID, key)
					}
					order = append(order, "resource")
					return nil
				},
				revokeDevice: func(_ context.Context, _ string, jwt, keyID string) error {
					if jwt != "cleanup-jwt" || keyID != "device-key-id" {
						t.Errorf("device cleanup authority = %q/%q", jwt, keyID)
					}
					order = append(order, "device")
					return nil
				},
			})
			if _, err := process.waitReadyResult(fixture.wait); err == nil || !strings.Contains(err.Error(), fixture.want) {
				t.Fatalf("readiness result = %v, want %q", err, fixture.want)
			}
		})
	}
}

func TestSandboxSiblingCleanupPreservesDeviceAfterResourceFailure(t *testing.T) {
	stateDir := t.TempDir()
	namespace := sandboxRunNamespace{AgentID: "qurl-share-r1-a1-ha", ConnectorID: "connector-recovery"}
	writeSandboxSiblingStateFixture(t, stateDir, namespace.AgentID)
	revoked := false
	err := cleanupSandboxSiblingAuthority(context.Background(), "https://sandbox.invalid", "cleanup-jwt", namespace, stateDir, sandboxSiblingCleanupOps{
		loadState: loadSandboxSiblingState,
		deleteResource: func(context.Context, string, string, string) error {
			return errors.New("injected resource failure")
		},
		revokeDevice: func(context.Context, string, string, string) error {
			revoked = true
			return nil
		},
	})
	if err == nil || revoked {
		t.Fatalf("resource failure cleanup = %v, device revoked=%t", err, revoked)
	}
}

func TestSandboxPublishReadinessWaitsForCompleteCRIDLine(t *testing.T) {
	const crid = "qhtpthw4qt7wkw7khghr6x3z4hsfyn4zbuyhnee4i6bi67yu6yytgvwdbb4q"
	process := &sandboxPublishProcess{label: "split-write", done: make(chan struct{})}
	_, _ = process.stderr.Write([]byte("event=login_success run_id=01abcdef23456789\nevent=proxy_ready run_id=01abcdef23456789\n"))
	wrotePartial := make(chan struct{})
	go func() {
		_, _ = process.stdout.Write([]byte(crid[:20]))
		close(wrotePartial)
		time.Sleep(50 * time.Millisecond)
		_, _ = process.stdout.Write([]byte(crid[20:] + "\n"))
	}()
	<-wrotePartial
	got, err := process.waitReadyResult(time.Second)
	if err != nil || got != crid {
		t.Fatalf("split CRID readiness = %q, %v", got, err)
	}
}

func writeSandboxSiblingStateFixture(t *testing.T, stateDir, agentID string) {
	t.Helper()
	if err := os.Chmod(stateDir, 0o700); err != nil { //nolint:gosec // Agent state requires a private directory.
		t.Fatal(err)
	}
	store, err := qurl.OpenFileAgentState(filepath.Join(stateDir, state.AgentStateFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAgentState(context.Background(), &qurl.AgentState{
		AgentID: agentID, DeviceAPIKeyID: "device-key-id", DeviceAPIKey: "device-secret",
	}); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}
