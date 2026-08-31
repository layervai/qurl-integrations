//go:build clisandbox

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
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
	sandboxFailureChildTimeout             = 3 * time.Minute
	sandboxFailureDaemonStopTimeout        = 15 * time.Second
)

type sandboxFailureDiagnostic struct {
	Category string
	Code     string
}

// sandboxLocalStateReason converts the daemon's private, redacted log text
// into one closed test-only reason. The live journey may print the returned
// value, but never the source line: paths, resource IDs, and deployment
// endpoints in that line stay out of CI output.
func sandboxLocalStateReason(logText string) string {
	type marker struct {
		text   string
		reason string
	}
	markers := []marker{
		{text: "native session operation journal is corrupt", reason: "operation_journal_corrupt"},
		{text: "native session operation state conflict", reason: "operation_conflict"},
		{text: "qurl: invalid native session operation", reason: "invalid_session_operation"},
		{text: "qurl: agent binding persistence failed", reason: "agent_binding_persistence"},
		{text: "qurl: native completion candidate durability is unknown", reason: "completion_persistence"},
		{text: "qurl: agent state setup lock failed", reason: "agent_state_lock"},
	}

	lower := strings.ToLower(logText)
	// A daemon can recover from an earlier reason before the command fails on
	// a later one. Classify only the latest retry record so stale log history
	// cannot be reported as the current cause.
	const retryRecord = "share daemon session attempt failed; retrying"
	if start := strings.LastIndex(lower, retryRecord); start >= 0 {
		lower = lower[start:]
		if end := strings.IndexByte(lower, '\n'); end >= 0 {
			lower = lower[:end]
		}
	}
	latestIndex := -1
	reason := "unclassified_local_state"
	for _, candidate := range markers {
		if index := strings.LastIndex(lower, candidate.text); index > latestIndex {
			latestIndex = index
			reason = candidate.reason
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
	"resource_unavailable": {},
	"unknown":              {},
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
	root := t.TempDir()
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
	crid, err := sandboxFailureCleanedCRID(combined)
	if err != nil {
		t.Fatalf("controlled-failure cleanup marker: %v", err)
	}
	assertSandboxFailureLocalCleanup(t, stateDir, crid)
	return crid
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

func sandboxFailureMissingSentinelError(output string) error {
	detail := fmt.Sprintf("last phase: %s", sandboxFailureLastPhase(output))
	if diagnostic, ok := sandboxFailureLastDiagnostic(output); ok {
		detail += ", failure category: " + diagnostic.Category
		if diagnostic.Code != "" {
			detail += ", failure code: " + diagnostic.Code
		}
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
		{name: "latest wins", log: "qurl: invalid native session operation then native session operation state conflict", want: "operation_conflict"},
		{
			name: "latest retry does not reuse stale reason",
			log: "share daemon session attempt failed; retrying qurl: invalid native session operation\n" +
				"share daemon session attempt failed; retrying arbitrary later cause",
			want: "unclassified_local_state",
		},
		{name: "unknown", log: "arbitrary private failure: " + privateDetail, want: "unclassified_local_state"},
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
	deadline := time.Now().Add(limit)
	client := connectordaemon.IPCClient{SocketPath: connectordaemon.StateSocketPath(stateDir)}
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
