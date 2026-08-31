//go:build clisandbox

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
	sandboxFailureChildTimeout             = 3 * time.Minute
	sandboxFailureDaemonStopTimeout        = 15 * time.Second
)

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
	// Prove the child kept every protected value out of its own output before
	// that output is validated or reported. A child that both leaked a secret
	// and stopped early has to fail as a leak, and the diagnostic below must
	// never be the thing that prints one.
	for _, secret := range sandboxProtectedChildValues(t, primaryAPIKey, failureAPIKey) {
		if secret != "" && strings.Contains(combined, secret) {
			t.Fatal("controlled-failure child exposed a protected credential")
		}
	}
	redactor := sandboxChildOutputRedactor()
	if err := validateSandboxFailureChildExit(runErr, combined, childTestName); err != nil {
		t.Fatalf("controlled-failure child result: %v\ncontrolled-failure child output:\n%s",
			err, boundedSandboxChildOutput(combined, redactor))
	}
	crid, err := sandboxFailureCleanedCRID(combined)
	if err != nil {
		// The child reached its controlled failure but never published a
		// clean cleanup marker. That is the same blindness the excerpt above
		// exists to remove, so report it the same way.
		t.Fatalf("controlled-failure cleanup marker: %v\ncontrolled-failure child output:\n%s",
			err, boundedSandboxChildOutput(combined, redactor))
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

// sandboxProtectedValueMinBytes is the shortest needle worth scanning for. A
// one- or two-character value matches ordinary output and would report a leak
// that never happened, so a degenerate secret is skipped instead.
const sandboxProtectedValueMinBytes = 16

// sandboxProtectedChildValues is every statically known secret the child must
// keep out of its own output. The qv2 issuer key needs more than one form: it
// arrives as "<kid>=<standard-base64 key>", journeyDeploymentFile re-encodes
// that same key material base64url into the SDK settings file the child reads,
// and either the whole string or the key half alone can surface on its own.
func sandboxProtectedChildValues(t *testing.T, primaryAPIKey, failureAPIKey string) []string {
	t.Helper()
	values := []string{
		primaryAPIKey,
		failureAPIKey,
		sandboxSecret(t, "QURL_CLI_SANDBOX_CLEANUP_JWT"),
	}
	issuerKey := strings.TrimSpace(os.Getenv("QURL_SANDBOX_QV2_ISSUER_KEY"))
	if issuerKey == "" {
		return values
	}
	values = append(values, issuerKey)
	_, keyStd, ok := strings.Cut(issuerKey, "=")
	keyStd = strings.TrimSpace(keyStd)
	if !ok || len(keyStd) < sandboxProtectedValueMinBytes {
		return values
	}
	values = append(values, keyStd)
	der, err := base64.StdEncoding.DecodeString(keyStd)
	if err != nil {
		return values
	}
	if keyURL := base64.RawURLEncoding.EncodeToString(der); len(keyURL) >= sandboxProtectedValueMinBytes {
		values = append(values, keyURL)
	}
	return values
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

const sandboxFailureChildOutputTailBytes = 8 << 10

// sandboxChildOutputRedactor masks the protected identifiers a failing child
// routinely embeds in transport errors. The exact-value scan in
// runSandboxFailureChild can only prove the absence of credentials it knows
// statically; the sandbox hostname and relay URL are deliberately not public
// either, and a request URL carries them into the log verbatim. Failing on
// those would be wrong — they appear in routine output — so they are redacted
// rather than treated as a leak.
func sandboxChildOutputRedactor() *strings.Replacer {
	var pairs []string
	for placeholder, raw := range map[string]string{
		"<sandbox-endpoint>": os.Getenv("QURL_ENDPOINT"),
		"<sandbox-relay>":    os.Getenv("QURL_SANDBOX_QV2_RELAY_URL"),
	} {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		pairs = append(pairs, value, placeholder)
		if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
			pairs = append(pairs, parsed.Host, placeholder)
			if hostname := parsed.Hostname(); hostname != "" && hostname != parsed.Host {
				pairs = append(pairs, hostname, placeholder)
			}
		}
	}
	if len(pairs) == 0 {
		return strings.NewReplacer()
	}
	return strings.NewReplacer(pairs...)
}

// boundedSandboxChildOutput returns the tail of the child's already
// leak-checked output, with the protected host identifiers redacted. The
// parent reports it whenever the child stopped before the controlled failure,
// because the child's own reason is otherwise discarded — which leaves the
// packaged journey debuggable only by guessing. A truncated tail is cut at a
// line boundary when the excerpt contains one, and otherwise has any partial
// leading rune dropped, so the excerpt is always valid UTF-8.
func boundedSandboxChildOutput(combined string, redactor *strings.Replacer) string {
	if redactor != nil {
		combined = redactor.Replace(combined)
	}
	trimmed := strings.TrimRight(combined, "\n")
	if strings.TrimSpace(trimmed) == "" {
		return "(the controlled-failure child produced no output)"
	}
	if len(trimmed) <= sandboxFailureChildOutputTailBytes {
		return trimmed
	}
	tail := trimmed[len(trimmed)-sandboxFailureChildOutputTailBytes:]
	if index := strings.IndexByte(tail, '\n'); index >= 0 {
		return "(truncated to the last lines)\n" + tail[index+1:]
	}
	return "(truncated to the last bytes)\n" + strings.ToValidUTF8(tail, "")
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
		return errors.New("child did not reach the controlled customer failure")
	}
	if !strings.Contains(output, "--- FAIL: "+childTestName) {
		return errors.New("child output did not identify the selected failing test")
	}
	return nil
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

// TestBoundedSandboxChildOutputRedactsAndStaysValidUTF8 covers every branch of
// the diagnostic the parent prints when the controlled-failure child stops
// early. That excerpt only ever runs on the live push-only lane, and only once
// the child has already failed, so a defect in it would surface exactly when
// the diagnostic is needed most — and as a confusing excerpt rather than a
// test failure.
func TestBoundedSandboxChildOutputRedactsAndStaysValidUTF8(t *testing.T) {
	t.Run("no output", func(t *testing.T) {
		for _, empty := range []string{"", "\n\n", "   \n"} {
			if got := boundedSandboxChildOutput(empty, nil); got != "(the controlled-failure child produced no output)" {
				t.Fatalf("boundedSandboxChildOutput(%q) = %q", empty, got)
			}
		}
	})
	t.Run("under budget is verbatim", func(t *testing.T) {
		if got := boundedSandboxChildOutput("first\nsecond\n", nil); got != "first\nsecond" {
			t.Fatalf("boundedSandboxChildOutput = %q, want the trimmed original", got)
		}
	})
	t.Run("truncates at a line boundary", func(t *testing.T) {
		got := boundedSandboxChildOutput(strings.Repeat("filler line\n", 2000)+"last line", nil)
		if !strings.HasPrefix(got, "(truncated to the last lines)\n") {
			t.Fatalf("truncated excerpt lost its label: %q", got[:min(60, len(got))])
		}
		if !strings.HasSuffix(got, "last line") || strings.Contains(got, "\nfiller line\nfiller") == false {
			t.Fatalf("truncated excerpt did not keep the tail: %q", got[len(got)-40:])
		}
		if body := strings.TrimPrefix(got, "(truncated to the last lines)\n"); !strings.HasPrefix(body, "filler line") {
			t.Fatalf("excerpt did not resume at a line boundary: %q", body[:min(40, len(body))])
		}
	})
	t.Run("single long line stays valid UTF-8", func(t *testing.T) {
		// The tail budget is a fixed byte count, so a 2-byte rune can never
		// be cut mid-way: the cut offset and every rune start share the same
		// parity, and the excerpt comes out accidentally valid. "…" is three
		// bytes, which breaks that alignment. The assertion below pins it, so
		// this case cannot silently go vacuous if the budget changes.
		// No newline anywhere, so the line-boundary path cannot apply.
		oneLongLine := strings.Repeat("…", 4096)
		cut := len(oneLongLine) - sandboxFailureChildOutputTailBytes
		if cut <= 0 || utf8.RuneStart(oneLongLine[cut]) {
			t.Fatalf("fixture does not cut mid-rune at offset %d; this case would prove nothing", cut)
		}
		got := boundedSandboxChildOutput(oneLongLine, nil)
		if !strings.HasPrefix(got, "(truncated to the last bytes)\n") {
			t.Fatalf("no-newline excerpt lost its label: %q", got[:min(60, len(got))])
		}
		if !utf8.ValidString(got) {
			t.Fatal("no-newline excerpt is not valid UTF-8")
		}
	})
	t.Run("redacts protected identifiers", func(t *testing.T) {
		t.Setenv("QURL_ENDPOINT", "https://sandbox-host.example.invalid")
		t.Setenv("QURL_SANDBOX_QV2_RELAY_URL", "https://relay-host.example.invalid/relay")
		redactor := sandboxChildOutputRedactor()
		got := boundedSandboxChildOutput(
			`Post "https://sandbox-host.example.invalid/v1/resources": dial relay-host.example.invalid: refused`,
			redactor,
		)
		for _, secret := range []string{"sandbox-host.example.invalid", "relay-host.example.invalid"} {
			if strings.Contains(got, secret) {
				t.Fatalf("excerpt still exposes %q: %q", secret, got)
			}
		}
		if !strings.Contains(got, "<sandbox-endpoint>") || !strings.Contains(got, "<sandbox-relay>") {
			t.Fatalf("excerpt lost its redaction placeholders: %q", got)
		}
		if !strings.Contains(got, "refused") {
			t.Fatalf("redaction destroyed the diagnostic: %q", got)
		}
	})
	t.Run("absent identifiers redact nothing", func(t *testing.T) {
		t.Setenv("QURL_ENDPOINT", "")
		t.Setenv("QURL_SANDBOX_QV2_RELAY_URL", "")
		const line = "plain child failure"
		if got := boundedSandboxChildOutput(line, sandboxChildOutputRedactor()); got != line {
			t.Fatalf("boundedSandboxChildOutput = %q, want %q", got, line)
		}
	})
}
