//go:build clisandbox && (linux || darwin)

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

func validateSandboxDeviceIdentity(loaded *qurl.AgentState, wantAgentID, wantDeviceKeyID string) error {
	if loaded == nil {
		return errors.New("durable device state is missing")
	}
	if loaded.AgentID != wantAgentID {
		return errors.New("durable agent identity does not match the canonical run namespace")
	}
	if wantDeviceKeyID != "" && loaded.DeviceAPIKeyID != wantDeviceKeyID {
		return errors.New("durable device credential identity changed across lifecycle restart")
	}
	return nil
}

func TestValidateSandboxDeviceIdentity(t *testing.T) {
	valid := &qurl.AgentState{AgentID: "qurl-share-r1-a1-hs", DeviceAPIKeyID: "key-1"}
	if err := validateSandboxDeviceIdentity(valid, valid.AgentID, valid.DeviceAPIKeyID); err != nil {
		t.Fatalf("valid identity: %v", err)
	}
	for name, loaded := range map[string]*qurl.AgentState{
		"missing":          nil,
		"wrong agent":      {AgentID: "other", DeviceAPIKeyID: "key-1"},
		"wrong credential": {AgentID: valid.AgentID, DeviceAPIKeyID: "key-2"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateSandboxDeviceIdentity(loaded, valid.AgentID, valid.DeviceAPIKeyID); err == nil {
				t.Fatal("invalid durable identity accepted")
			}
		})
	}
}

// TestSandboxLocalPublishSiblingContinuity runs the released customer binary
// as two independent processes. It checks bytes through both live routes,
// stops A while B stays usable, restarts A from the same durable state and
// resource at a later serving epoch, checks both routes again, and stops both.
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
	apiKey, err := readSandboxSecretFile(sandboxAPIKeyFileEnv, "QURL_API_KEY")
	if err != nil {
		t.Fatalf("load protected sandbox API key: %v", err)
	}
	cliEnv := sandboxJourneyEnv(t)
	addSandboxRunIdentity(t, cliEnv)
	endpoint := cliEnv["QURL_ENDPOINT"]
	if cliEnv["QURL_API_KEY"] != apiKey {
		t.Fatal("sandbox API key sources disagree")
	}

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
	firstAStopped := processA.stopAndValidate(t, apiKey, cleanupJWT)
	processB.requireRunning(t, "after sibling A stop")
	processB.requireSharingState(t, "on", "serving")
	assertSandboxGetBytes(t, binary, cliEnv, cridB, bodyB)

	replacementA := startSandboxPublishProcess(t, binary, cliEnv, namespaceA, stateDirA, targetA.URL)
	replacementCRIDA := replacementA.waitReady(t)
	if replacementCRIDA != cridA {
		t.Fatalf("replacement A CRID = %q, want durable resource %q", replacementCRIDA, cridA)
	}
	if replacementA.servingState.ServingEpoch <= firstAStopped.ServingEpoch {
		t.Fatalf("replacement A serving epoch = %d, want greater than stopped epoch %d", replacementA.servingState.ServingEpoch, firstAStopped.ServingEpoch)
	}
	assertSandboxGetBytes(t, binary, cliEnv, cridA, bodyA)
	processB.requireRunning(t, "after sibling A replacement")
	processB.requireSharingState(t, "on", "serving")
	assertSandboxGetBytes(t, binary, cliEnv, cridB, bodyB)

	replacementA.stopAndValidate(t, apiKey, cleanupJWT)
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
	label        string
	binary       string
	baseEnv      map[string]string
	stateDir     string
	crid         string
	servingState sandboxSharingDoc
	cmd          *exec.Cmd
	stdout       lockedSandboxBuffer
	stderr       lockedSandboxBuffer
	done         chan struct{}

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

	p := &sandboxPublishProcess{
		label: namespace.ConnectorID, binary: binary, baseEnv: cloneSandboxEnv(baseEnv),
		stateDir: stateDir, done: make(chan struct{}),
	}
	p.cmd = exec.CommandContext(context.Background(), binary, "--endpoint", baseEnv["QURL_ENDPOINT"], "--quiet", "publish", targetURL, "--id", namespace.ConnectorID, "--foreground") //nolint:gosec // The protected test validates the fixed binary path and supplies closed arguments.
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
	p.crid = crid
	p.servingState = p.waitForSharingState(t, "on", "serving")
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
		if strings.HasSuffix(stdoutRaw, "\n") {
			assessment, err := cridux.Assess(stdout)
			if err != nil {
				return "", fmt.Errorf("stdout = %q, want one CRID: %w", stdout, err)
			}
			if assessment.Kind != cridux.KindCRID {
				return "", fmt.Errorf("stdout = %q, want one CRID", stdout)
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

func (p *sandboxPublishProcess) requireSharingState(t *testing.T, desired, observed string) sandboxSharingDoc {
	t.Helper()
	return p.waitForSharingState(t, desired, observed)
}

func (p *sandboxPublishProcess) waitForSharingState(t *testing.T, desired, observed string) sandboxSharingDoc {
	t.Helper()
	deadline := time.Now().Add(sandboxProcessTimeout)
	last := "status was not attempted"
	for time.Now().Before(deadline) {
		doc, err := p.readSharingState()
		if err == nil {
			if doc.CRID != p.crid || doc.ResourceID == "" || doc.ServingEpoch == 0 {
				last = "status returned an incomplete resource identity"
			} else if doc.DesiredState == desired && doc.ConnectionState == observed {
				return doc
			} else {
				last = fmt.Sprintf("status is %s/%s at epoch %d", doc.DesiredState, doc.ConnectionState, doc.ServingEpoch)
			}
		} else {
			last = err.Error()
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("sandbox publish %s did not reach %s/%s within %s: %s", p.label, desired, observed, sandboxProcessTimeout, last)
	return sandboxSharingDoc{}
}

func (p *sandboxPublishProcess) readSharingState() (sandboxSharingDoc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	env := cloneSandboxEnv(p.baseEnv)
	env[state.EnvStateDirPrimary] = p.stateDir
	cmd := exec.CommandContext(ctx, p.binary, "--endpoint", p.baseEnv["QURL_ENDPOINT"], "-o", "json", "status", p.crid) //nolint:gosec // The protected test validates the fixed binary path and closed arguments.
	cmd.Env = sandboxCommandEnv(env)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return sandboxSharingDoc{}, fmt.Errorf("status command failed: %w", err)
	}
	var doc sandboxSharingDoc
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		return sandboxSharingDoc{}, errors.New("status command returned malformed JSON")
	}
	return doc, nil
}

func (p *sandboxPublishProcess) stopAndValidate(t *testing.T, secrets ...string) sandboxSharingDoc {
	t.Helper()
	p.interruptAndValidate(t, secrets...)
	stopped := p.waitForSharingState(t, "off", "stopped")
	if stopped.ServingEpoch <= p.servingState.ServingEpoch {
		t.Fatalf("sandbox publish %s stopped epoch = %d, want greater than serving epoch %d", p.label, stopped.ServingEpoch, p.servingState.ServingEpoch)
	}
	return stopped
}

func (p *sandboxPublishProcess) interruptAndValidate(t *testing.T, secrets ...string) {
	t.Helper()
	if p.stopped {
		t.Fatalf("sandbox publish %s was stopped twice", p.label)
	}
	p.stopped = true
	p.requireRunning(t, "before requested stop")
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
	stderr := p.stderr.String()
	stdout := p.stdout.String()
	if err := validateSandboxForegroundExit(waitErr, stdout, stderr, p.crid, secrets...); err != nil {
		t.Fatalf("sandbox publish %s stop: %v\nstderr: %s", p.label, err, stderr)
	}
}

func validateSandboxForegroundExit(waitErr error, stdout, stderr, crid string, secrets ...string) error {
	if err := validateSandboxInterruptedExit(waitErr); err != nil {
		return fmt.Errorf("foreground publish %w", err)
	}
	if stdout != crid+"\n" {
		return errors.New("foreground publish did not print exactly one complete CRID line")
	}
	return validateSandboxProtectedProcessOutput(stdout, stderr, secrets...)
}

func validateSandboxInterruptedExit(waitErr error) error {
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 130 {
		return fmt.Errorf("exit = %v, want 130 after interrupt", waitErr)
	}
	return nil
}

func validateSandboxProtectedProcessOutput(stdout, stderr string, secrets ...string) error {
	for _, secret := range secrets {
		if secret != "" && strings.Contains(stdout+stderr, secret) {
			return errors.New("sandbox process exposed a protected credential")
		}
	}
	if strings.Contains(stderr, "refresh-mode") || strings.Contains(stderr, "explicit approval") {
		return errors.New("sandbox process exposed retired assignment-approval UX")
	}
	return nil
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
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", errors.New("customer CLI binary is unavailable")
	}
	if !filepath.IsAbs(resolved) || filepath.Clean(resolved) != resolved {
		return "", errors.New("customer CLI binary resolved to a noncanonical path")
	}
	info, err := os.Lstat(resolved)
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
	return resolved, nil
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
	resolvedValid, err := filepath.EvalSymlinks(valid)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := validateSandboxCLIBinary(valid); err != nil || got != resolvedValid {
		t.Fatalf("valid binary = %q, %v; want %q", got, err, resolvedValid)
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
	if got, err := validateSandboxCLIBinary(symlink); err != nil || got != resolvedValid {
		t.Fatalf("Homebrew-style symlink = %q, %v; want resolved %q", got, err, resolvedValid)
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

func TestSandboxForegroundLifecycleStateContract(t *testing.T) {
	const crid = "qhtpthw4qt7wkw7khghr6x3z4hsfyn4zbuyhnee4i6bi67yu6yytgvwdbb4q"
	exit130 := exec.Command("sh", "-c", "exit 130").Run() //nolint:gosec // Fixed POSIX fixture for the Linux/macOS-only tagged test.
	if err := validateSandboxForegroundExit(exit130, crid+"\n", "", crid, "api-secret"); err != nil {
		t.Fatal(err)
	}
	for name, fixture := range map[string]struct {
		waitErr error
		stdout  string
		stderr  string
		secret  string
	}{
		"success exit":        {waitErr: nil, stdout: crid + "\n"},
		"wrong exit":          {waitErr: exec.Command("sh", "-c", "exit 1").Run(), stdout: crid + "\n"}, //nolint:gosec // Fixed POSIX fixture.
		"partial CRID":        {waitErr: exit130, stdout: crid[:20]},
		"extra output":        {waitErr: exit130, stdout: crid + "\nextra\n"},
		"stdout secret":       {waitErr: exit130, stdout: crid + "\napi-secret", secret: "api-secret"},
		"stderr secret":       {waitErr: exit130, stdout: crid + "\n", stderr: "api-secret", secret: "api-secret"},
		"retired approval UX": {waitErr: exit130, stdout: crid + "\n", stderr: "explicit approval"},
		"retired refresh UX":  {waitErr: exit130, stdout: crid + "\n", stderr: "refresh-mode"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateSandboxForegroundExit(fixture.waitErr, fixture.stdout, fixture.stderr, crid, fixture.secret); err == nil {
				t.Fatal("invalid foreground exit accepted")
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
