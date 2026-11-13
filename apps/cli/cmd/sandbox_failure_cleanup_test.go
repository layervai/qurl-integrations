//go:build clisandbox

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

const (
	sandboxControlledFailureLifecyclePhase = "controlled_failure_cleanup"
	sandboxFailureChildArmingEnv           = "QURL_CLI_SANDBOX_FAILURE_CHILD"
	sandboxFailureChildStateDirEnv         = "QURL_CLI_SANDBOX_FAILURE_STATE_DIR"
	sandboxFailureAPIKeyEnv                = "QURL_CLI_SANDBOX_FAILURE_API_KEY"
	sandboxFailureChildSentinel            = "controlled customer failure reached after route fencing"
	sandboxFailureCleanupMarker            = "QURL_CONTROLLED_FAILURE_CLEANED"
	sandboxFailurePhaseMarker              = "QURL_CONTROLLED_FAILURE_PHASE"
	sandboxFailureDiagnosticMarker         = "QURL_CONTROLLED_FAILURE_DIAGNOSTIC"
	sandboxFailureLoginExitMarker          = "QURL_CONTROLLED_FAILURE_LOGIN_EXIT"
	sandboxFailureReadinessMarker          = "QURL_CONTROLLED_FAILURE_READINESS"
	sandboxFailureDaemonStateMarker        = "QURL_CONTROLLED_FAILURE_DAEMON_STATE"
	sandboxFailureLoginExitTimeout         = "timeout"
	sandboxFailureLoginExitUnknown         = "unknown"
	sandboxLocalStateUnclassified          = "unclassified_local_state"
	sandboxStoppedRouteRefusalText         = "Error: This qURL Connector is stopped.\n\n  Hint: run `qurl start <CRID>`, then try again.\n"
	sandboxFailureChildTimeout             = 3 * time.Minute
	sandboxFailureDaemonStopTimeout        = 15 * time.Second
)

func sandboxStoppedRouteRefusal(_ *testing.T) string {
	// Keep this exact fixed-text contract in source because every packaged
	// customer-journey harness runs without a repository checkout. The command
	// is already bound to the journey's CRID; the CLI deliberately renders the
	// documented <CRID> placeholder so the shared error boundary does not echo
	// any server-controlled value.
	return sandboxStoppedRouteRefusalText
}

func validateSandboxStoppedCommandResult(code int, stdout, stderr, stoppedRefusal string) error {
	if stoppedRefusal == "" {
		return errors.New("stopped-route refusal contract is empty")
	}
	if code != exitcode.Unavailable || stdout != "" || stderr != stoppedRefusal {
		return fmt.Errorf("get did not return the exact stopped-resource refusal: exit=%d stdout-bytes=%d stderr-bytes=%d", code, len(stdout), len(stderr))
	}
	return nil
}

func validateSandboxStoppedDownloadResult(code int, stdout, stderr, destination, stoppedRefusal string) error {
	if err := validateSandboxStoppedCommandResult(code, stdout, stderr, stoppedRefusal); err != nil {
		return err
	}
	if destination == "" {
		return errors.New("stopped-route destination contract is empty")
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return errors.New("stopped-route refusal left a destination file")
	}
	return nil
}

type sandboxFailureDiagnostic struct {
	Category string
	Code     string
}

type sandboxLocalStateMarker struct {
	text   string
	reason string
}

// TODO(upstream-contract): These phrases mirror the pinned qurl-go and
// qurl-connector sentinel text. Keep this list in lockstep when those pins
// change so diagnostic drift fails review instead of silently degrading.
var sandboxLocalStateMarkers = []sandboxLocalStateMarker{
	{text: "native session operation journal is corrupt", reason: "operation_journal_corrupt"},
	{text: "native session operation state conflict", reason: "operation_conflict"},
	{text: "qurl: invalid native session operation", reason: "invalid_session_operation"},
	{text: "qurl: agent binding persistence failed", reason: "agent_binding_persistence"},
	{text: "qurl: native completion candidate durability is unknown", reason: "completion_persistence"},
	{text: "qurl: agent state setup lock failed", reason: "agent_state_lock"},
}

// sandboxLocalStateReason converts the daemon's private, redacted log text
// into one closed test-only reason. The live journey may print the returned
// value, but never the source line: paths, resource IDs, and deployment
// endpoints in that line stay out of CI output.
func sandboxLocalStateReason(logText string) string {
	lower := strings.ToLower(logText)
	// A daemon can recover from an earlier reason before the command fails on
	// a later one. Ignore records before the latest retry, but retain later
	// primary and error-log lines. The last line with a known reason is the
	// latest event; within that line the first marker is the outer wrapper.
	const retryRecord = "share daemon session attempt failed; retrying"
	if start := strings.LastIndex(lower, retryRecord); start >= 0 {
		lower = lower[start:]
	}
	reason := sandboxLocalStateUnclassified
	for _, line := range strings.Split(lower, "\n") {
		firstIndex := len(line)
		lineReason := ""
		for _, candidate := range sandboxLocalStateMarkers {
			if index := strings.Index(line, candidate.text); index >= 0 && index < firstIndex {
				firstIndex = index
				lineReason = candidate.reason
			}
		}
		if lineReason != "" {
			reason = lineReason
		}
	}
	return reason
}

type sandboxFailurePhase string

const (
	sandboxFailurePhaseSetup      sandboxFailurePhase = "setup"
	sandboxFailurePhaseLogin      sandboxFailurePhase = "login"
	sandboxFailurePhaseIdentity   sandboxFailurePhase = "identity"
	sandboxFailurePhaseService    sandboxFailurePhase = "service"
	sandboxFailurePhasePublish    sandboxFailurePhase = "publish"
	sandboxFailurePhaseReadiness  sandboxFailurePhase = "readiness"
	sandboxFailurePhaseRoute      sandboxFailurePhase = "route"
	sandboxFailurePhaseStop       sandboxFailurePhase = "stop"
	sandboxFailurePhaseFence      sandboxFailurePhase = "fence"
	sandboxFailurePhaseStoppedGet sandboxFailurePhase = "stopped_get"
	sandboxFailurePhaseUnknown    sandboxFailurePhase = "unknown"
)

var sandboxFailurePhases = map[sandboxFailurePhase]struct{}{
	sandboxFailurePhaseSetup:      {},
	sandboxFailurePhaseLogin:      {},
	sandboxFailurePhaseIdentity:   {},
	sandboxFailurePhaseService:    {},
	sandboxFailurePhasePublish:    {},
	sandboxFailurePhaseReadiness:  {},
	sandboxFailurePhaseRoute:      {},
	sandboxFailurePhaseStop:       {},
	sandboxFailurePhaseFence:      {},
	sandboxFailurePhaseStoppedGet: {},
}

var sandboxFailureCategories = map[string]struct{}{
	"assignment":           {},
	"enrollment":           {},
	"identity":             {},
	"local_daemon":         {},
	"local_state":          {},
	"network":              {},
	"peer_timeout":         {},
	"platform_denied":      {},
	"verification":         {},
	"resource_unavailable": {},
	"unknown":              {},
}

// The readiness phase makes three checks in order. A child that stops there
// names the one that stopped it and what that check last saw.
const (
	sandboxReadinessShareRow     = "share_row"
	sandboxReadinessJobStatus    = "job_status"
	sandboxReadinessSharingState = "sharing_state"

	sandboxReadinessReadFailed         = "read_failed"
	sandboxReadinessTimeout            = "timeout"
	sandboxReadinessNotReady           = "not_ready"
	sandboxReadinessQueryFailed        = "query_failed"
	sandboxReadinessDelayed            = "delayed"
	sandboxReadinessDelayedQueryFailed = "delayed_query_failed"
	sandboxReadinessStateMismatch      = "state_mismatch"
	sandboxReadinessCommandFailed      = "command_failed"
	sandboxReadinessDecodeFailed       = "decode_failed"
	sandboxReadinessNotSampled         = "not_sampled"

	sandboxReadinessYes   = "yes"
	sandboxReadinessNo    = "no"
	sandboxReadinessNone  = "none"
	sandboxReadinessOther = "other"
)

// sandboxFailureReadiness is what one readiness check last saw. Every field is
// one token from the closed set of its check, so the parent can print it
// without forwarding anything else the child process wrote.
type sandboxFailureReadiness struct {
	Check  string
	Result string
	First  string
	Second string
}

type sandboxReadinessCheck struct {
	results     []string
	first       []string
	second      []string
	firstLabel  string
	secondLabel string
}

var sandboxReadinessChecks = map[string]sandboxReadinessCheck{
	sandboxReadinessShareRow: {
		results: []string{sandboxReadinessReadFailed, sandboxReadinessTimeout},
		first:   []string{sandboxReadinessNone},
		second:  []string{sandboxReadinessNone},
	},
	sandboxReadinessJobStatus: {
		results: []string{
			sandboxReadinessNotReady, sandboxReadinessQueryFailed,
			sandboxReadinessDelayed, sandboxReadinessDelayedQueryFailed,
		},
		first:      []string{sandboxReadinessYes, sandboxReadinessNo},
		second:     []string{sandboxReadinessYes, sandboxReadinessNo},
		firstLabel: "job installed", secondLabel: "job running",
	},
	sandboxReadinessSharingState: {
		results: []string{
			sandboxReadinessStateMismatch, sandboxReadinessCommandFailed,
			sandboxReadinessDecodeFailed, sandboxReadinessNotSampled,
		},
		first:      []string{"on", "off", sandboxReadinessOther, sandboxReadinessNone},
		second:     []string{"serving", "connecting", "stopped", sandboxReadinessOther, sandboxReadinessNone},
		firstLabel: "desired state", secondLabel: "observed state",
	},
}

// The daemon states `qurl inspect` can print. The child reports the one it
// reads after a failure, which says whether a daemon answered at that moment.
var sandboxFailureDaemonStates = map[string]struct{}{
	"failed":         {},
	"idle":           {},
	"not_registered": {},
	"not_running":    {},
	"retrying":       {},
	"serving":        {},
	"starting":       {},
	"stopped":        {},
	"unavailable":    {},
}

func runSandboxFailureChild(t *testing.T, childTestName string) string {
	t.Helper()
	if childTestName == "" || strings.ContainsAny(childTestName, "^$[]()|*+?\\") {
		t.Fatal("controlled-failure child test name is invalid")
	}
	primaryAPIKey, failureAPIKey := requireSandboxFailureCredentials(t)
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("locate trusted customer-journey harness: %v", err)
	}
	testBinary, err = filepath.Abs(testBinary)
	if err != nil {
		t.Fatalf("resolve trusted customer-journey harness: %v", err)
	}
	root, err := canonicalSandboxFailureRoot(t.TempDir())
	if err != nil {
		t.Fatalf("resolve canonical controlled-failure state root: %v", err)
	}
	stateDir := filepath.Join(root, "failure-state")
	if err := connectorstate.EnsureDirMode(stateDir); err != nil {
		t.Fatalf("create controlled-failure state directory: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), sandboxFailureChildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, testBinary, //nolint:gosec // The parent executes its own trusted, already-running test harness.
		"-test.v", "-test.count=1", "-test.timeout=165s", "-test.run=^"+childTestName+"$")
	overrides := sandboxFailureChildCredentialOverrides(failureAPIKey)
	overrides[sandboxFailureChildArmingEnv] = "enabled"
	overrides[sandboxFailureChildStateDirEnv] = stateDir
	cmd.Env = sandboxFailureChildEnvironment(overrides)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("controlled-failure child exceeded %s", sandboxFailureChildTimeout)
	}
	combined := stdout.String() + stderr.String()
	if err := validateSandboxFailureChildResult(runErr, combined, childTestName, []string{
		primaryAPIKey,
		failureAPIKey,
		sandboxSecret(t, "QURL_CLI_SANDBOX_CLEANUP_JWT"),
	}); err != nil {
		t.Fatalf("controlled-failure child result: %v", err)
	}
	// A check that passed only after waiting never fails the child, so say it
	// here: it is the one sign that the wait was needed in this run.
	if readiness, ok := sandboxFailureLastReadiness(combined, true); ok {
		t.Logf("controlled-failure child readiness note: %s", sandboxFailureReadinessDetail(readiness))
	}
	crid, err := sandboxFailureCleanedCRID(combined)
	if err != nil {
		t.Fatalf("controlled-failure cleanup marker: %v", err)
	}
	assertSandboxFailureLocalCleanup(t, stateDir, crid)
	return crid
}

func canonicalSandboxFailureRoot(root string) (string, error) {
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(resolved) || filepath.Clean(resolved) != resolved {
		return "", errors.New("controlled-failure state root resolved to a noncanonical path")
	}
	return resolved, nil
}

func TestCanonicalSandboxFailureRootResolvesAlias(t *testing.T) {
	target := t.TempDir()
	alias := filepath.Join(t.TempDir(), "state-root-alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("create state-root alias: %v", err)
	}
	want, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	got, err := canonicalSandboxFailureRoot(alias)
	if err != nil {
		t.Fatalf("canonicalSandboxFailureRoot() error = %v", err)
	}
	if got != want {
		t.Fatalf("canonicalSandboxFailureRoot() = %q, want %q", got, want)
	}
}

func requireSandboxFailureCredentials(t *testing.T) (primaryAPIKey, failureAPIKey string) {
	t.Helper()
	primaryAPIKey = sandboxSecret(t, "QURL_API_KEY")
	failureAPIKey = sandboxSecret(t, sandboxFailureAPIKeyEnv)
	if primaryAPIKey == "" || failureAPIKey == "" {
		t.Fatalf("QURL_API_KEY and %s are required before the full lifecycle starts", sandboxFailureAPIKeyEnv)
	}
	if failureAPIKey == primaryAPIKey {
		t.Fatal("controlled-failure and primary enrollment keys must be distinct")
	}
	return primaryAPIKey, failureAPIKey
}

func TestSandboxFailureChildEnvironmentUsesItsOwnOneTimeKey(t *testing.T) {
	for name, failureFile := range map[string]string{
		"protected-file": "/protected/failure",
		"inline":         "",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("QURL_API_KEY", "primary-inline")
			t.Setenv("QURL_API_KEY_FILE", "/protected/primary")
			t.Setenv(sandboxFailureAPIKeyEnv, "failure-inline")
			t.Setenv(sandboxFailureAPIKeyEnv+"_FILE", failureFile)
			got := map[string]string{}
			for _, entry := range sandboxFailureChildEnvironment(sandboxFailureChildCredentialOverrides("failure-exact")) {
				key, value, ok := strings.Cut(entry, "=")
				if ok {
					got[key] = value
				}
			}
			wantInline := "failure-exact"
			if failureFile != "" {
				wantInline = ""
			}
			if got["QURL_API_KEY"] != wantInline || got["QURL_API_KEY_FILE"] != failureFile ||
				got[sandboxFailureAPIKeyEnv] != "" || got[sandboxFailureAPIKeyEnv+"_FILE"] != "" {
				t.Fatalf("controlled-failure credential environment = %#v", got)
			}
		})
	}
}

func sandboxFailureChildCredentialOverrides(failureAPIKey string) map[string]string {
	failureFile := os.Getenv(sandboxFailureAPIKeyEnv + "_FILE")
	overrides := map[string]string{
		"QURL_API_KEY":                    failureAPIKey,
		"QURL_API_KEY_FILE":               "",
		sandboxFailureAPIKeyEnv:           "",
		sandboxFailureAPIKeyEnv + "_FILE": "",
	}
	if failureFile != "" {
		overrides["QURL_API_KEY"] = ""
		overrides["QURL_API_KEY_FILE"] = failureFile
	}
	return overrides
}

func validateSandboxFailureChildExit(runErr error, output, childTestName string) error {
	if runErr == nil {
		return errors.New("child succeeded, want the controlled terminal failure")
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() == 0 {
		return errors.New("child did not return a bounded test failure")
	}
	if !strings.Contains(output, sandboxFailureChildSentinel) {
		return sandboxFailureMissingSentinelError(output)
	}
	if !strings.Contains(output, "--- FAIL: "+childTestName) {
		return errors.New("child output did not identify the selected failing test")
	}
	return nil
}

func validateSandboxFailureChildResult(runErr error, output, childTestName string, secrets []string) error {
	for _, secret := range secrets {
		if secret != "" && strings.Contains(output, secret) {
			return errors.New("child exposed a protected credential")
		}
	}
	return validateSandboxFailureChildExit(runErr, output, childTestName)
}

func markSandboxFailurePhase(phase sandboxFailurePhase) {
	if _, ok := sandboxFailurePhases[phase]; !ok {
		panic("invalid controlled-failure phase")
	}
	_, _ = fmt.Fprintf(os.Stdout, "%s %s\n", sandboxFailurePhaseMarker, phase)
}

func validSandboxFailureCode(code string) bool {
	if code == "" {
		return true
	}
	if len(code) != 5 {
		return false
	}
	for _, character := range code {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func validSandboxFailureExitCode(code int) bool {
	return (code >= exitcode.General && code <= exitcode.VerificationFailed) || code == exitcode.Interrupted
}

func validSandboxFailureDiagnostic(diagnostic sandboxFailureDiagnostic) bool {
	_, categoryOK := sandboxFailureCategories[diagnostic.Category]
	return categoryOK && validSandboxFailureCode(diagnostic.Code)
}

func markSandboxFailureDiagnostic(diagnostic sandboxFailureDiagnostic) {
	writeSandboxFailureDiagnostic(os.Stdout, diagnostic)
}

func writeSandboxFailureDiagnostic(w io.Writer, diagnostic sandboxFailureDiagnostic) {
	if !validSandboxFailureDiagnostic(diagnostic) {
		panic("invalid controlled-failure diagnostic")
	}
	code := "none"
	if diagnostic.Code != "" {
		code = diagnostic.Code
	}
	_, _ = fmt.Fprintf(w, "%s %s %s\n", sandboxFailureDiagnosticMarker, diagnostic.Category, code)
}

func validSandboxFailureReadiness(readiness sandboxFailureReadiness) bool {
	check, ok := sandboxReadinessChecks[readiness.Check]
	return ok && slices.Contains(check.results, readiness.Result) &&
		slices.Contains(check.first, readiness.First) && slices.Contains(check.second, readiness.Second)
}

func markSandboxFailureReadiness(readiness sandboxFailureReadiness) {
	writeSandboxFailureReadiness(os.Stdout, readiness)
}

func writeSandboxFailureReadiness(w io.Writer, readiness sandboxFailureReadiness) {
	if !validSandboxFailureReadiness(readiness) {
		panic("invalid controlled-failure readiness")
	}
	_, _ = fmt.Fprintf(w, "%s %s %s %s %s\n", sandboxFailureReadinessMarker,
		readiness.Check, readiness.Result, readiness.First, readiness.Second)
}

// sandboxReadinessSettledLate reports a check that passed, but not when it was
// first asked. It is a note about a run, not the reason a child stopped.
func sandboxReadinessSettledLate(result string) bool {
	return result == sandboxReadinessDelayed || result == sandboxReadinessDelayedQueryFailed
}

// sandboxFailureLastReadiness returns the last valid readiness record of one
// kind: the check that stopped the child, or a check that settled late.
func sandboxFailureLastReadiness(output string, settledLate bool) (sandboxFailureReadiness, bool) {
	var last sandboxFailureReadiness
	found := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSuffix(line, "\r")
		fields := strings.Split(line, " ")
		if len(fields) != 5 || fields[0] != sandboxFailureReadinessMarker {
			continue
		}
		readiness := sandboxFailureReadiness{Check: fields[1], Result: fields[2], First: fields[3], Second: fields[4]}
		if validSandboxFailureReadiness(readiness) && sandboxReadinessSettledLate(readiness.Result) == settledLate {
			last, found = readiness, true
		}
	}
	return last, found
}

func sandboxFailureReadinessDetail(readiness sandboxFailureReadiness) string {
	check := sandboxReadinessChecks[readiness.Check]
	detail := "readiness check: " + readiness.Check + ", result: " + readiness.Result
	if check.firstLabel != "" {
		detail += ", " + check.firstLabel + ": " + readiness.First + ", " + check.secondLabel + ": " + readiness.Second
	}
	return detail
}

func sandboxFailureDaemonStateFromInspection(raw []byte) (string, bool) {
	var document sandboxInspectionDoc
	if json.Unmarshal(raw, &document) != nil || document.DaemonState == nil {
		return "", false
	}
	if _, ok := sandboxFailureDaemonStates[*document.DaemonState]; !ok {
		return "", false
	}
	return *document.DaemonState, true
}

// markSandboxFailureDaemonStateFromCommand reports the daemon state of a
// successful `qurl inspect`. A failed or unreadable inspection reports nothing.
func markSandboxFailureDaemonStateFromCommand(stdout string, commandErr error) {
	writeSandboxFailureDaemonStateFromCommand(os.Stdout, stdout, commandErr)
}

func writeSandboxFailureDaemonStateFromCommand(w io.Writer, stdout string, commandErr error) {
	if commandErr != nil {
		return
	}
	if state, ok := sandboxFailureDaemonStateFromInspection([]byte(stdout)); ok {
		_, _ = fmt.Fprintf(w, "%s %s\n", sandboxFailureDaemonStateMarker, state)
	}
}

func sandboxFailureLastDaemonState(output string) (string, bool) {
	last, found := "", false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSuffix(line, "\r")
		value, ok := strings.CutPrefix(line, sandboxFailureDaemonStateMarker+" ")
		if !ok {
			continue
		}
		if _, known := sandboxFailureDaemonStates[value]; known {
			last, found = value, true
		}
	}
	return last, found
}

func sandboxFailureLastPhase(output string) sandboxFailurePhase {
	last := sandboxFailurePhaseUnknown
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSuffix(line, "\r")
		value, found := strings.CutPrefix(line, sandboxFailurePhaseMarker+" ")
		if !found {
			continue
		}
		phase := sandboxFailurePhase(value)
		if _, ok := sandboxFailurePhases[phase]; ok {
			last = phase
		}
	}
	return last
}

func sandboxFailureLastDiagnostic(output string) (sandboxFailureDiagnostic, bool) {
	var last sandboxFailureDiagnostic
	found := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSuffix(line, "\r")
		fields := strings.Split(line, " ")
		if len(fields) != 3 || fields[0] != sandboxFailureDiagnosticMarker {
			continue
		}
		diagnostic := sandboxFailureDiagnostic{Category: fields[1], Code: fields[2]}
		if diagnostic.Code == "none" {
			diagnostic.Code = ""
		}
		if validSandboxFailureDiagnostic(diagnostic) {
			last, found = diagnostic, true
		}
	}
	return last, found
}

func sandboxFailureLastLoginExit(output string) string {
	last := ""
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSuffix(line, "\r")
		value, found := strings.CutPrefix(line, sandboxFailureLoginExitMarker+" ")
		if !found {
			continue
		}
		if value == sandboxFailureLoginExitUnknown || value == sandboxFailureLoginExitTimeout {
			last = value
			continue
		}
		if code, err := strconv.Atoi(value); err == nil && validSandboxFailureExitCode(code) {
			last = strconv.Itoa(code)
		}
	}
	return last
}

func sandboxFailureMissingSentinelError(output string) error {
	detail := fmt.Sprintf("last phase: %s", sandboxFailureLastPhase(output))
	if exit := sandboxFailureLastLoginExit(output); exit != "" {
		detail += ", login exit: " + exit
	}
	// A check that settled late comes before the check that stopped the child.
	if late, ok := sandboxFailureLastReadiness(output, true); ok {
		detail += ", " + sandboxFailureReadinessDetail(late)
	}
	if stopped, ok := sandboxFailureLastReadiness(output, false); ok {
		detail += ", " + sandboxFailureReadinessDetail(stopped)
	}
	if diagnostic, ok := sandboxFailureLastDiagnostic(output); ok {
		detail += ", failure category: " + diagnostic.Category
		if diagnostic.Code != "" {
			detail += ", failure code: " + diagnostic.Code
		}
	}
	if state, ok := sandboxFailureLastDaemonState(output); ok {
		detail += ", daemon state: " + state
	}
	return fmt.Errorf("child did not reach the controlled customer failure (%s)", detail)
}

func TestSandboxFailureDiagnosticsAreAllowListedAndRedacted(t *testing.T) {
	const secret = "lv_test_captured_child_secret"
	t.Run("last recognized phase", func(t *testing.T) {
		output := strings.Join([]string{
			sandboxFailurePhaseMarker + " " + string(sandboxFailurePhaseLogin),
			sandboxFailurePhaseMarker + " " + secret,
			"arbitrary child detail " + secret,
			sandboxFailurePhaseMarker + " " + string(sandboxFailurePhasePublish),
			"    " + sandboxFailurePhaseMarker + " " + string(sandboxFailurePhaseStoppedGet),
		}, "\n")
		if got := sandboxFailureLastPhase(output); got != sandboxFailurePhasePublish {
			t.Fatalf("last controlled-failure phase = %q, want %q", got, sandboxFailurePhasePublish)
		}
		err := sandboxFailureMissingSentinelError(output)
		if strings.Contains(err.Error(), secret) || err.Error() != "child did not reach the controlled customer failure (last phase: publish)" {
			t.Fatalf("redacted controlled-failure diagnostic = %q", err)
		}
	})
	t.Run("closed diagnostic", func(t *testing.T) {
		output := strings.Join([]string{
			sandboxFailureDiagnosticMarker + " network none",
			sandboxFailureDiagnosticMarker + " internal_topology 52401",
			sandboxFailureDiagnosticMarker + " identity ５2401",
			sandboxFailureDiagnosticMarker + " identity 524010",
			" " + sandboxFailureDiagnosticMarker + " identity 52401",
			sandboxFailureDiagnosticMarker + " identity 52401 extra",
			sandboxFailureDiagnosticMarker + " identity 52401\r",
		}, "\n")
		diagnostic, ok := sandboxFailureLastDiagnostic(output)
		if !ok || diagnostic != (sandboxFailureDiagnostic{Category: "identity", Code: "52401"}) {
			t.Fatalf("last controlled-failure diagnostic = %#v, %t", diagnostic, ok)
		}
		err := sandboxFailureMissingSentinelError(sandboxFailurePhaseMarker + " publish\n" + output)
		want := "child did not reach the controlled customer failure (last phase: publish, failure category: identity, failure code: 52401)"
		if err.Error() != want || strings.Contains(err.Error(), "internal_topology") {
			t.Fatalf("controlled-failure diagnostic error = %q, want %q", err, want)
		}
	})
	t.Run("closed login exit", func(t *testing.T) {
		output := strings.Join([]string{
			sandboxFailureLoginExitMarker + " " + sandboxFailureLoginExitUnknown,
			sandboxFailureLoginExitMarker + " " + secret,
			sandboxFailureLoginExitMarker + " 999999",
			sandboxFailureLoginExitMarker + " +011",
			" " + sandboxFailureLoginExitMarker + " 7",
		}, "\n")
		if got := sandboxFailureLastLoginExit(output); got != "11" {
			t.Fatalf("last controlled-failure login exit = %q, want 11", got)
		}
		err := sandboxFailureMissingSentinelError(sandboxFailurePhaseMarker + " login\n" + output)
		want := "child did not reach the controlled customer failure (last phase: login, login exit: 11)"
		if err.Error() != want || strings.Contains(err.Error(), secret) {
			t.Fatalf("controlled-failure login error = %q, want %q", err, want)
		}

		for _, token := range []string{sandboxFailureLoginExitTimeout, sandboxFailureLoginExitUnknown} {
			t.Run(token, func(t *testing.T) {
				marker := sandboxFailureLoginExitMarker + " " + token
				if got := sandboxFailureLastLoginExit(marker); got != token {
					t.Fatalf("closed login token = %q, want %q", got, token)
				}
				want := "child did not reach the controlled customer failure (last phase: unknown, login exit: " + token + ")"
				if got := sandboxFailureMissingSentinelError(marker).Error(); got != want {
					t.Fatalf("closed login token error = %q, want %q", got, want)
				}
			})
		}
	})
	t.Run("closed readiness", func(t *testing.T) {
		// Every record after the second is refused, so the second is the last.
		output := strings.Join([]string{
			sandboxFailureReadinessMarker + " job_status not_ready yes no",
			sandboxFailureReadinessMarker + " job_status query_failed no no\r",
			sandboxFailureReadinessMarker + " job_status not_ready yes " + secret,
			sandboxFailureReadinessMarker + " internal_topology not_ready yes no",
			sandboxFailureReadinessMarker + " share_row timeout yes no",
			sandboxFailureReadinessMarker + " job_status state_mismatch yes no",
			sandboxFailureReadinessMarker + " sharing_state state_mismatch on serving extra",
			sandboxFailureReadinessMarker + " sharing_state state_mismatch on",
			" " + sandboxFailureReadinessMarker + " sharing_state state_mismatch on stopped",
			sandboxFailureReadinessMarker + "  sharing_state state_mismatch on stopped",
		}, "\n")
		readiness, ok := sandboxFailureLastReadiness(output, false)
		if !ok || readiness != (sandboxFailureReadiness{Check: sandboxReadinessJobStatus, Result: sandboxReadinessQueryFailed, First: sandboxReadinessNo, Second: sandboxReadinessNo}) {
			t.Fatalf("last controlled-failure readiness = %#v, %t", readiness, ok)
		}
		if late, found := sandboxFailureLastReadiness(output, true); found {
			t.Fatalf("a stopped check was read as one that settled late: %#v", late)
		}
		err := sandboxFailureMissingSentinelError(sandboxFailurePhaseMarker + " readiness\n" + output)
		want := "child did not reach the controlled customer failure (last phase: readiness, readiness check: job_status, result: query_failed, job installed: no, job running: no)"
		if err.Error() != want || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "internal_topology") {
			t.Fatalf("controlled-failure readiness error = %q, want %q", err, want)
		}
	})
	t.Run("readiness names each check", func(t *testing.T) {
		for _, test := range []struct {
			record sandboxFailureReadiness
			want   string
		}{
			{
				record: sandboxFailureReadiness{Check: sandboxReadinessShareRow, Result: sandboxReadinessTimeout, First: sandboxReadinessNone, Second: sandboxReadinessNone},
				want:   "readiness check: share_row, result: timeout",
			},
			{
				record: sandboxFailureReadiness{Check: sandboxReadinessJobStatus, Result: sandboxReadinessNotReady, First: sandboxReadinessYes, Second: sandboxReadinessNo},
				want:   "readiness check: job_status, result: not_ready, job installed: yes, job running: no",
			},
			{
				record: sandboxFailureReadiness{Check: sandboxReadinessSharingState, Result: sandboxReadinessStateMismatch, First: "on", Second: "connecting"},
				want:   "readiness check: sharing_state, result: state_mismatch, desired state: on, observed state: connecting",
			},
		} {
			var written strings.Builder
			writeSandboxFailureReadiness(&written, test.record)
			err := sandboxFailureMissingSentinelError(sandboxFailurePhaseMarker + " readiness\n" + written.String())
			want := "child did not reach the controlled customer failure (last phase: readiness, " + test.want + ")"
			if err.Error() != want {
				t.Errorf("controlled-failure readiness error = %q, want %q", err, want)
			}
		}
	})
	t.Run("every allowed readiness record is read back", func(t *testing.T) {
		records := 0
		for name, check := range sandboxReadinessChecks {
			for _, result := range check.results {
				for _, first := range check.first {
					for _, second := range check.second {
						record := sandboxFailureReadiness{Check: name, Result: result, First: first, Second: second}
						var written strings.Builder
						writeSandboxFailureReadiness(&written, record)
						got, ok := sandboxFailureLastReadiness(written.String(), sandboxReadinessSettledLate(result))
						if !ok || got != record {
							t.Errorf("readiness record %#v was read back as %#v, %t", record, got, ok)
						}
						records++
					}
				}
			}
		}
		if records == 0 {
			t.Fatal("no readiness record was checked")
		}
	})
	t.Run("readiness writer refuses a token outside its set", func(t *testing.T) {
		for _, record := range []sandboxFailureReadiness{
			{Check: sandboxReadinessJobStatus, Result: sandboxReadinessNotReady, First: secret, Second: sandboxReadinessNo},
			{Check: sandboxReadinessJobStatus, Result: sandboxReadinessTimeout, First: sandboxReadinessYes, Second: sandboxReadinessNo},
			{Check: secret, Result: sandboxReadinessTimeout, First: sandboxReadinessNone, Second: sandboxReadinessNone},
			{},
		} {
			var written strings.Builder
			func() {
				defer func() {
					if recover() == nil {
						t.Errorf("readiness record %#v was written", record)
					}
				}()
				writeSandboxFailureReadiness(&written, record)
			}()
			if written.Len() != 0 {
				t.Errorf("refused readiness record %#v still wrote %q", record, written.String())
			}
		}
	})
	t.Run("a check that settled late is a note beside the check that stopped", func(t *testing.T) {
		output := strings.Join([]string{
			sandboxFailurePhaseMarker + " readiness",
			sandboxFailureReadinessMarker + " job_status delayed yes no",
			sandboxFailureReadinessMarker + " sharing_state state_mismatch on connecting",
		}, "\n")
		late, ok := sandboxFailureLastReadiness(output, true)
		if !ok || sandboxFailureReadinessDetail(late) != "readiness check: job_status, result: delayed, job installed: yes, job running: no" {
			t.Fatalf("late readiness note = %#v, %t", late, ok)
		}
		err := sandboxFailureMissingSentinelError(output)
		want := "child did not reach the controlled customer failure (last phase: readiness, " +
			"readiness check: job_status, result: delayed, job installed: yes, job running: no, " +
			"readiness check: sharing_state, result: state_mismatch, desired state: on, observed state: connecting)"
		if err.Error() != want {
			t.Fatalf("controlled-failure readiness error = %q, want %q", err, want)
		}
	})
	t.Run("closed daemon state", func(t *testing.T) {
		// Every record after the second is refused, so the second is the last.
		output := strings.Join([]string{
			sandboxFailureDaemonStateMarker + " serving",
			sandboxFailureDaemonStateMarker + " starting\r",
			sandboxFailureDaemonStateMarker + " " + secret,
			sandboxFailureDaemonStateMarker + " internal_topology",
			sandboxFailureDaemonStateMarker + " not_running extra",
			sandboxFailureDaemonStateMarker + "  failed",
			" " + sandboxFailureDaemonStateMarker + " failed",
		}, "\n")
		if state, ok := sandboxFailureLastDaemonState(output); !ok || state != "starting" {
			t.Fatalf("last controlled-failure daemon state = %q, %t", state, ok)
		}
		err := sandboxFailureMissingSentinelError(strings.Join([]string{
			sandboxFailurePhaseMarker + " readiness",
			sandboxFailureDiagnosticMarker + " local_daemon none",
			output,
		}, "\n"))
		want := "child did not reach the controlled customer failure (last phase: readiness, failure category: local_daemon, daemon state: starting)"
		if err.Error() != want || strings.Contains(err.Error(), secret) {
			t.Fatalf("controlled-failure daemon state error = %q, want %q", err, want)
		}
	})
	t.Run("daemon state comes only from a readable inspection", func(t *testing.T) {
		for _, test := range []struct {
			name   string
			stdout string
			err    error
			want   string
		}{
			{name: "known state", stdout: `{"daemon_state":"not_running","target_url":"http://127.0.0.1:1"}`, want: sandboxFailureDaemonStateMarker + " not_running\n"},
			{name: "state outside the set", stdout: `{"daemon_state":"` + secret + `"}`},
			{name: "no state", stdout: `{"target_url":"` + secret + `"}`},
			{name: "malformed", stdout: `{"daemon_state":`},
			{name: "failed command", stdout: `{"daemon_state":"serving"}`, err: errors.New(secret)},
		} {
			var written strings.Builder
			writeSandboxFailureDaemonStateFromCommand(&written, test.stdout, test.err)
			if written.String() != test.want {
				t.Errorf("%s: daemon state record = %q, want %q", test.name, written.String(), test.want)
			}
		}
	})
	t.Run("diagnostic cannot carry secret", func(t *testing.T) {
		output := sandboxFailureDiagnosticMarker + " unknown none\narbitrary child detail " + secret
		err := validateSandboxFailureChildResult(errors.New("not an exit error"), output, "ChildTest", []string{secret})
		if err == nil || err.Error() != "child exposed a protected credential" || strings.Contains(err.Error(), secret) {
			t.Fatalf("protected diagnostic validation = %q", err)
		}
	})
	t.Run("secret scan precedes exit validation", func(t *testing.T) {
		err := validateSandboxFailureChildResult(errors.New("not an exit error"), "arbitrary child detail "+secret, "ChildTest", []string{secret})
		if err == nil || err.Error() != "child exposed a protected credential" || strings.Contains(err.Error(), secret) {
			t.Fatalf("protected child-output validation = %q", err)
		}
	})
}

func TestSandboxLocalStateReasonIsClosedAndUsesLatestCause(t *testing.T) {
	const privateDetail = `C:\Users\runner\private-state\native_session_operation.json cell0.private.example`
	tests := []struct {
		name string
		log  string
		want string
	}{
		{name: "journal", log: privateDetail + ": native session operation journal is corrupt", want: "operation_journal_corrupt"},
		{name: "conflict", log: "NATIVE SESSION OPERATION STATE CONFLICT: " + privateDetail, want: "operation_conflict"},
		{name: "invalid operation", log: "prepare: qurl: invalid native session operation: " + privateDetail, want: "invalid_session_operation"},
		{name: "binding persistence", log: "qurl: agent binding persistence failed: " + privateDetail, want: "agent_binding_persistence"},
		{name: "completion persistence", log: "qurl: native completion candidate durability is unknown: " + privateDetail, want: "completion_persistence"},
		{name: "state lock", log: "qurl: agent state setup lock failed: " + privateDetail, want: "agent_state_lock"},
		{name: "outer wrapper wins within one line", log: "qurl: invalid native session operation then native session operation state conflict", want: "invalid_session_operation"},
		{name: "latest relevant line wins", log: "qurl: invalid native session operation\nnative session operation state conflict", want: "operation_conflict"},
		{
			name: "later error log is retained after retry",
			log: "share daemon session attempt failed; retrying arbitrary cause\n" +
				"qurl: agent state setup lock failed: private detail",
			want: "agent_state_lock",
		},
		{
			name: "latest retry does not reuse stale reason",
			log: "share daemon session attempt failed; retrying qurl: invalid native session operation\n" +
				"share daemon session attempt failed; retrying arbitrary later cause",
			want: sandboxLocalStateUnclassified,
		},
		{name: "unknown", log: "arbitrary private failure: " + privateDetail, want: sandboxLocalStateUnclassified},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := sandboxLocalStateReason(test.log)
			if got != test.want {
				t.Fatalf("sandboxLocalStateReason() = %q, want %q", got, test.want)
			}
			if strings.Contains(got, privateDetail) || strings.Contains(got, "runner") || strings.Contains(got, "private.example") {
				t.Fatalf("closed local-state reason exposed private input: %q", got)
			}
		})
	}
}

func TestSandboxLocalStateReasonDoesNotForwardHostileLogText(t *testing.T) {
	hostile := strings.Join([]string{
		"lv_live_secret-that-must-not-escape",
		"::error title=forged::forged workflow command",
		"\x1b[31mterminal-control\x1b[0m",
		`C:\Users\runner\private-state cell0.private.example`,
	}, "\n")
	got := sandboxLocalStateReason(hostile + "\nqurl: invalid native session operation: " + hostile)
	if got != "invalid_session_operation" {
		t.Fatalf("hostile local-state reason = %q, want closed invalid_session_operation", got)
	}
	for _, forbidden := range []string{"lv_live_", "::error", "\x1b", "runner", "private.example"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("closed local-state reason exposed hostile fragment %q: %q", forbidden, got)
		}
	}
}

func sandboxFailureChildEnvironment(overrides map[string]string) []string {
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if _, replaced := overrides[key]; found && replaced {
			continue
		}
		environment = append(environment, entry)
	}
	for key, value := range overrides {
		environment = append(environment, key+"="+value)
	}
	return environment
}

func sandboxFailureChildStateDir(t *testing.T) string {
	t.Helper()
	if os.Getenv(sandboxFailureChildArmingEnv) != "enabled" {
		t.Skip("controlled-failure child is parent-armed only")
	}
	stateDir := os.Getenv(sandboxFailureChildStateDirEnv)
	if stateDir == "" || stateDir != strings.TrimSpace(stateDir) || !filepath.IsAbs(stateDir) || filepath.Clean(stateDir) != stateDir {
		t.Fatalf("%s must be one exact absolute path", sandboxFailureChildStateDirEnv)
	}
	if filepath.Clean(stateDir) == filepath.Clean(filepath.VolumeName(stateDir)+string(filepath.Separator)) {
		t.Fatal("controlled-failure state directory cannot be a volume root")
	}
	if err := connectorstate.EnsureDirMode(stateDir); err != nil {
		t.Fatalf("secure controlled-failure state directory: %v", err)
	}
	return stateDir
}

func registerSandboxFailureFinalCleanup(
	t *testing.T,
	stateDir string,
	crid *string,
	productCleanupComplete *bool,
) {
	t.Helper()
	t.Cleanup(func() {
		if crid == nil || strings.TrimSpace(*crid) == "" || productCleanupComplete == nil || !*productCleanupComplete {
			return
		}
		if err := waitSandboxFailureDaemonStopped(stateDir, sandboxFailureDaemonStopTimeout); err != nil {
			t.Error(err)
			return
		}
		shares, present, err := connectorstate.ReadLocalSharesIfPresent(context.Background(), stateDir)
		if err != nil {
			t.Errorf("read controlled-failure local shares after cleanup: %v", err)
			return
		}
		for index := range shares {
			if shares[index].CRID == *crid {
				t.Errorf("controlled-failure CRID %s remains in local registry", *crid)
				return
			}
		}
		if !present && len(shares) != 0 {
			t.Errorf("controlled-failure registry has %d rows while absent", len(shares))
			return
		}
		_, _ = fmt.Fprintf(os.Stdout, "%s %s\n", sandboxFailureCleanupMarker, *crid)
	})
}

func sandboxFailureCleanedCRID(output string) (string, error) {
	prefix := sandboxFailureCleanupMarker + " "
	var found string
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		crid := strings.TrimPrefix(line, prefix)
		if found != "" || strings.TrimSpace(crid) == "" || crid != strings.TrimSpace(crid) {
			return "", errors.New("cleanup marker is malformed or repeated")
		}
		found = crid
	}
	if found == "" {
		return "", errors.New("cleanup marker is missing")
	}
	return found, nil
}

func waitSandboxFailureDaemonStopped(stateDir string, limit time.Duration) error {
	// The child CLI inherits this process's environment, so resolve as it does.
	socketPath, err := connectordaemon.SocketPathForStateDir(stateDir, os.LookupEnv)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(limit)
	client := connectordaemon.IPCClient{SocketPath: socketPath}
	var last error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, running, err := client.Status(ctx)
		cancel()
		if err == nil && !running {
			return nil
		}
		last = err
		time.Sleep(100 * time.Millisecond)
	}
	if last != nil {
		return fmt.Errorf("controlled-failure daemon remained reachable: %w", last)
	}
	return errors.New("controlled-failure daemon remained reachable")
}

func assertSandboxFailureLocalCleanup(t *testing.T, stateDir, crid string) {
	t.Helper()
	shares, present, err := connectorstate.ReadLocalSharesIfPresent(context.Background(), stateDir)
	if err != nil {
		t.Fatalf("read controlled-failure local shares after cleanup: %v", err)
	}
	for index := range shares {
		if shares[index].CRID == crid {
			t.Fatalf("controlled-failure CRID %s remains in local registry", crid)
		}
	}
	if !present && len(shares) != 0 {
		t.Fatalf("controlled-failure local registry has %d rows while absent", len(shares))
	}
	if err := waitSandboxFailureDaemonStopped(stateDir, time.Second); err != nil {
		t.Fatal(err)
	}
}
