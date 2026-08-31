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
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
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

// sandboxProtectedValueMinBytes is the shortest *derived* needle worth
// scanning for. A one- or two-character value matches ordinary output and
// would report a leak that never happened, so a degenerate derivation is
// dropped. The three primary credentials are scanned whatever their length —
// that is the fail-closed direction.
const sandboxProtectedValueMinBytes = 16

// sandboxProtectedChildValues is every statically known secret the child must
// keep out of its own output. The qv2 issuer key needs more than one form: it
// arrives as "<kid>=<standard-base64 key>", journeyDeploymentFile re-encodes
// that same key material base64url into the SDK settings file the child reads,
// and either the whole string or the key half alone can surface on its own.
func sandboxProtectedChildValues(t *testing.T, primaryAPIKey, failureAPIKey string) []string {
	t.Helper()
	// Read with a bare Getenv to match sandboxJourneyEnv, which is the only
	// provisioning path for this input. If it ever moves to sandboxSecret's
	// _FILE indirection, this degrades to scanning for nothing — silently.
	issuerForms := qv2IssuerKeyForms(os.Getenv("QURL_SANDBOX_QV2_ISSUER_KEY"))
	values := make([]string, 0, 3+len(issuerForms))
	values = append(values, primaryAPIKey, failureAPIKey, sandboxSecret(t, "QURL_CLI_SANDBOX_CLEANUP_JWT"))
	return append(values, issuerForms...)
}

// qv2IssuerKeyForms returns every form the qv2 issuer key can surface in: the
// whole "<kid>=<standard-base64 key>" string, the key half on its own, and the
// base64url re-encoding journeyDeploymentFile writes into the SDK settings
// file the child reads. A malformed key yields only the forms that could be
// derived, never a short needle that would match ordinary output.
func qv2IssuerKeyForms(issuerKey string) []string {
	issuerKey = strings.TrimSpace(issuerKey)
	if len(issuerKey) < sandboxProtectedValueMinBytes {
		return nil
	}
	forms := []string{issuerKey}
	_, keyStd, ok := strings.Cut(issuerKey, "=")
	keyStd = strings.TrimSpace(keyStd)
	if !ok || len(keyStd) < sandboxProtectedValueMinBytes {
		return forms
	}
	forms = append(forms, keyStd)
	der, err := base64.StdEncoding.DecodeString(keyStd)
	if err != nil {
		return forms
	}
	if keyURL := base64.RawURLEncoding.EncodeToString(der); len(keyURL) >= sandboxProtectedValueMinBytes && keyURL != keyStd {
		forms = append(forms, keyURL)
	}
	return forms
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

// sandboxRuntimeCredentialPatterns removes the credentials the child mints at
// runtime, which no exact-value scan can cover because the parent never sees
// them. Each is matched structurally, by shape:
//
//   - A minted qURL link's fragment. qv2.ParseFragment defines it as four
//     dot-separated parts, "qv2.<claims>.<secret>.<sig>", so the dots are part
//     of the credential — a word-character-only class would mask "qv2" and
//     print the secret and signature in the clear.
//   - A qURL API key or one-shot enrollment token ("lv_live_"/"lv_test_").
//   - A JWT, which is the shape the cleanup and enrollment paths return.
//
// TODO(upstream-contract): keep the fragment shape in lockstep with qurl-go's
// qv2.ParseFragment and the key prefixes with qurl-service's credential
// grammar. A change upstream degrades this to a partial mask, which reads as
// redacted while still leaking material.
var sandboxRuntimeCredentialPatterns = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`#[A-Za-z0-9_\-.~%]{16,}={0,2}`), "#<redacted>"},
	{regexp.MustCompile(`lv_(?:live|test)_[A-Za-z0-9_\-]{8,}={0,2}`), "lv_<redacted>"},
	{regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`), "<redacted-jwt>"},
}

// scrubSandboxRuntimeCredentials applies every shape above.
func scrubSandboxRuntimeCredentials(text string) string {
	for _, rule := range sandboxRuntimeCredentialPatterns {
		text = rule.pattern.ReplaceAllString(text, rule.replacement)
	}
	return text
}

// sandboxChildOutputRedactor masks the protected identifiers a failing child
// routinely embeds in transport errors. The exact-value scan in
// runSandboxFailureChild can only prove the absence of credentials it knows
// statically; the sandbox endpoint, relay URL and Hub coordinates are
// deliberately not public either, and a request URL carries them into the log
// verbatim. Failing on those would be wrong — they appear in routine output —
// so they are redacted rather than treated as a leak.
func sandboxChildOutputRedactor() *strings.Replacer {
	// Ordered, not a map: two inputs that share a host would otherwise pick a
	// placeholder at random between runs.
	sources := []struct{ placeholder, env string }{
		{"<sandbox-endpoint>", "QURL_ENDPOINT"},
		{"<sandbox-relay>", "QURL_SANDBOX_QV2_RELAY_URL"},
		{"<hub-host>", hub.EnvHost},
		{"<hub-port>", hub.EnvPort},
		{"<hub-key>", hub.EnvServerPublicKey},
	}
	var pairs []string
	for _, source := range sources {
		value := strings.TrimSpace(os.Getenv(source.env))
		if len(value) < sandboxRedactedValueMinBytes {
			continue
		}
		pairs = append(pairs, value, source.placeholder)
		parsed, err := url.Parse(value)
		if err != nil || parsed.Host == "" {
			continue
		}
		pairs = append(pairs, parsed.Host, source.placeholder)
		if hostname := parsed.Hostname(); hostname != "" && hostname != parsed.Host {
			pairs = append(pairs, hostname, source.placeholder)
		}
	}
	return strings.NewReplacer(pairs...)
}

// sandboxRedactedValueMinBytes keeps a short or absent identifier out of the
// replacer. A one- or two-character needle would rewrite ordinary output into
// noise; the Hub port is the reason this is a length rule and not just an
// emptiness check.
const sandboxRedactedValueMinBytes = 8

// boundedSandboxChildOutput returns the tail of the child's already
// leak-checked output, with the protected identifiers redacted and any minted
// link credential scrubbed. The parent reports it whenever the child stopped
// before the controlled failure, because the child's own reason is otherwise
// discarded — which leaves the packaged journey debuggable only by guessing.
// The result is always valid UTF-8, including when the child itself wrote
// invalid bytes, because go test -json carries this text.
func boundedSandboxChildOutput(combined string, redactor *strings.Replacer) string {
	if redactor != nil {
		combined = redactor.Replace(combined)
	}
	combined = scrubSandboxRuntimeCredentials(combined)
	trimmed := strings.ToValidUTF8(strings.TrimRight(combined, "\n"), "")
	if strings.TrimSpace(trimmed) == "" {
		return "(the controlled-failure child produced no output)"
	}
	if len(trimmed) <= sandboxFailureChildOutputTailBytes {
		return trimmed
	}
	tail := trimmed[len(trimmed)-sandboxFailureChildOutputTailBytes:]
	// Resume at a line boundary, but not when the first newline sits so late
	// that the excerpt would be nearly empty — which is exactly the one-huge-
	// line case where the tail matters most.
	if index := strings.IndexByte(tail, '\n'); index >= 0 && index < sandboxFailureChildOutputTailBytes/2 {
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
		if !strings.HasSuffix(got, "last line") {
			t.Fatalf("truncated excerpt did not keep the tail: %q", got[max(0, len(got)-40):])
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
		clearSandboxRedactedEnv(t)
		t.Setenv("QURL_ENDPOINT", "https://sandbox-host.example.invalid")
		// Port-bearing on purpose: a transport error prints the bare hostname
		// in some places and host:port in others, so both forms must go.
		t.Setenv("QURL_SANDBOX_QV2_RELAY_URL", "https://relay-host.example.invalid:8443/relay")
		redactor := sandboxChildOutputRedactor()
		got := boundedSandboxChildOutput(
			`Post "https://sandbox-host.example.invalid/v1/resources": dial relay-host.example.invalid:8443 `+
				`(resolved relay-host.example.invalid): refused`,
			redactor,
		)
		for _, secret := range []string{"sandbox-host.example.invalid", "relay-host.example.invalid:8443", "relay-host.example.invalid"} {
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
	t.Run("scrubs runtime-minted credentials", func(t *testing.T) {
		// These are the exact shapes the child mints at runtime, which no
		// exact-value scan can cover. The fragment fixture uses qv2's real
		// four-part "qv2.<claims>.<secret>.<sig>" grammar on purpose: a
		// single-run fixture would pass against a word-character-only class
		// that leaves the secret and signature in the clear.
		const (
			secret = "c2VjcmV0LW1hdGVyaWFsLXg"
			sig    = "c2lnbmF0dXJlLW1hdGVyaWFs"
			apiKey = "lv_live_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
			jwt    = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJjbGVhbnVwIn0.c2lnbmF0dXJl"
		)
		fragment := "#qv2.Y2xhaW1zLW1hdGVyaWFs." + secret + "." + sig
		got := boundedSandboxChildOutput(
			"resolve https://example.invalid/abc"+fragment+" with "+apiKey+" and "+jwt+" after 3 tries", nil)
		for _, leaked := range []string{secret, sig, apiKey, jwt, "Y2xhaW1zLW1hdGVyaWFs"} {
			if strings.Contains(got, leaked) {
				t.Fatalf("excerpt still exposes runtime credential material %q: %q", leaked, got)
			}
		}
		for _, want := range []string{"#<redacted>", "lv_<redacted>", "<redacted-jwt>", "after 3 tries"} {
			if !strings.Contains(got, want) {
				t.Fatalf("scrub lost %q from the diagnostic: %q", want, got)
			}
		}
	})
	t.Run("keeps short fragments that are not credentials", func(t *testing.T) {
		const line = "see docs#usage for details"
		if got := boundedSandboxChildOutput(line, nil); got != line {
			t.Fatalf("boundedSandboxChildOutput = %q, want %q", got, line)
		}
	})
	t.Run("absent identifiers redact nothing", func(t *testing.T) {
		// Clear all five, not just two: on the armed lane the Hub values are
		// set, and this subtest would otherwise pass only because the fixture
		// happens not to contain one.
		clearSandboxRedactedEnv(t)
		const line = "plain child failure"
		if got := boundedSandboxChildOutput(line, sandboxChildOutputRedactor()); got != line {
			t.Fatalf("boundedSandboxChildOutput = %q, want %q", got, line)
		}
	})
}

// TestQV2IssuerKeyFormsCoversEveryEncoding pins the derivation that decides
// which needles the child's output is scanned for. A silently dropped form is
// a credential this suite would stop looking for, so the base64 round-trip is
// the half most worth pinning.
func TestQV2IssuerKeyFormsCoversEveryEncoding(t *testing.T) {
	// DER whose standard and URL-safe base64 encodings differ ("+/" vs "-_").
	der := []byte{0xFF, 0xFE, 0xFD, 0xFC, 0xFB, 0xFA, 0xF9, 0xF8, 0xF7, 0xF6, 0xF5, 0xF4, 0xF3, 0xF2, 0xF1, 0xF0}
	keyStd := base64.StdEncoding.EncodeToString(der)
	keyURL := base64.RawURLEncoding.EncodeToString(der)
	if keyStd == keyURL {
		t.Fatal("fixture encodings coincide; this test would prove nothing")
	}
	issuerKey := "sandbox-kid=" + keyStd

	forms := qv2IssuerKeyForms(issuerKey)
	for _, want := range []string{issuerKey, keyStd, keyURL} {
		if !slices.Contains(forms, want) {
			t.Fatalf("qv2IssuerKeyForms(%q) = %q, missing %q", issuerKey, forms, want)
		}
	}

	for name, test := range map[string]struct {
		issuerKey string
		want      []string
	}{
		"absent":            {issuerKey: "", want: nil},
		"too short to scan": {issuerKey: "kid=aa", want: nil},
		"no key half":       {issuerKey: "sandbox-kid-with-no-separator", want: []string{"sandbox-kid-with-no-separator"}},
		"short key half":    {issuerKey: "sandbox-kid-long-enough=aa", want: []string{"sandbox-kid-long-enough=aa"}},
		"key half is not base64": {
			issuerKey: "sandbox-kid=not-valid-base64-material!!",
			want:      []string{"sandbox-kid=not-valid-base64-material!!", "not-valid-base64-material!!"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := qv2IssuerKeyForms(test.issuerKey); !slices.Equal(got, test.want) {
				t.Fatalf("qv2IssuerKeyForms(%q) = %q, want %q", test.issuerKey, got, test.want)
			}
		})
	}
	t.Run("no form is short enough to match ordinary output", func(t *testing.T) {
		for _, form := range qv2IssuerKeyForms(issuerKey) {
			if len(form) < sandboxProtectedValueMinBytes {
				t.Fatalf("qv2IssuerKeyForms produced the over-broad needle %q", form)
			}
		}
	})
}

// clearSandboxRedactedEnv empties every input sandboxChildOutputRedactor
// reads, so a subtest asserting redaction behavior controls all of them
// rather than inheriting whatever the armed lane provisioned.
func clearSandboxRedactedEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"QURL_ENDPOINT", "QURL_SANDBOX_QV2_RELAY_URL",
		hub.EnvHost, hub.EnvPort, hub.EnvServerPublicKey,
	} {
		t.Setenv(name, "")
	}
}
