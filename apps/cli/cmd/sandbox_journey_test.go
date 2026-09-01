//go:build clisandbox

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-go/crid"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/sessionrelay"
	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// T6 live sandbox CRID journey: the design doc's §28.1/§28.3 manual
// checklist, automated. This file carries the clisandbox build tag so the
// protected main workflow can compile and run it from the exact integrations
// commit without storing credentials here.
//
// Credential contract (four common inputs; full lifecycle entrypoints also
// require the isolated failure key before they start live work):
//
//	QURL_API_KEY  — a sandbox API key holding the qurl:agent, qurl:read,
//	    qurl:write, and qurl:resolve scopes. The CLI reads it only for
//	    bootstrap and does not write the account key to disk.
//	QURL_ENDPOINT — the sandbox qURL API base URL (a protected environment
//	    secret; the sandbox hostname is deliberately not public).
//	QURL_SANDBOX_QV2_ISSUER_KEY — the sandbox's link-signing identity as
//	    "<kid>=<standard-base64 P-256 SPKI DER>" (a protected environment
//	    secret).
//	QURL_SANDBOX_QV2_RELAY_URL — the sandbox NHP HTTPS relay origin used by
//	    both qv2 platform access and registered Connector session operations
//	    (a protected environment secret).
//	QURL_CLI_SANDBOX_FAILURE_API_KEY — a second one-time account key used
//	    only by the controlled-failure child in the full lifecycle.
//
// The issuer and relay values become a QURL_DEPLOYMENT settings file for the
// download step: `get --file` opens fragment-credential links through the
// platform access flow, which needs the deployment's trust settings. The same
// relay origin is passed to the Connector session path. Their values — like
// every minted link — never reach the log.
//
// Quota safety: each run publishes exactly ONE throwaway resource and always
// reclaims it. The happy path ends in delete + idempotent re-delete, and a
// t.Cleanup registered the moment the CRID exists deletes by CRID even on
// mid-test failure, tolerating already-gone. Retries are the API transport's
// own bounded 429 handling (Retry-After honored via realSleep) — the suite
// adds no retry loops of its own.
//
// The commands run through runCLI with the PRODUCTION wiring and no injected
// seams. The protected workflow requires an exact PASS and rejects SKIP.

// journeyTimeout bounds the whole journey. Every API call also carries the
// transport's own 30-second HTTP timeout; this outer bound exists so a
// pathological hang (a download that never ends, a stuck retry wait) fails
// the test instead of eating the job's timeout budget.
const journeyTimeout = 4 * time.Minute

// journeyDescription labels the throwaway resource so a human auditing the
// sandbox tenancy — or a sweeper reading `qurl list -o json` — can tell a
// leaked fixture from a real one. assertListFindsCRID holds the CLI to
// surfacing it: a label no listing carries identifies nothing.
const journeyDescription = "qurl CLI journey v2 (self-cleaning; safe to delete)"

func sandboxJourneyResourceDescription(t *testing.T, env map[string]string) string {
	t.Helper()
	runID := strings.TrimSpace(env[sandboxRunIDEnv])
	attempt := strings.TrimSpace(env[sandboxRunAttemptEnv])
	runtimeName := strings.TrimSpace(env[sandboxRuntimeEnv])
	if runID == "" && attempt == "" && runtimeName == "" {
		return journeyDescription
	}
	if !sandboxPositiveDecimal.MatchString(runID) || !sandboxPositiveDecimal.MatchString(attempt) ||
		(runtimeName != "host" && runtimeName != "hardened_container") {
		t.Fatal("run-scoped journey resource description received an incomplete identity")
	}
	return fmt.Sprintf("qurl CLI journey v2 resource %s/%s/%s", runID, attempt, runtimeName)
}

// sandboxJourneyEnv reads the common suite env contract from the real process
// environment and skips loudly — naming every missing variable — when it
// is not fully provisioned. Run-scoped local-publish lanes add their exact
// run identity through addSandboxRunIdentity. The returned map is the ONLY
// environment the CLI invocations see, which is what keeps hermetic mode airtight: the
// deployment settings the download step needs enter it as QURL_DEPLOYMENT,
// built by journeyDeploymentFile below, never read from the process.
func sandboxJourneyEnv(t *testing.T) map[string]string {
	t.Helper()
	key := sandboxSecret(t, "QURL_API_KEY")
	endpoint := strings.TrimSpace(os.Getenv("QURL_ENDPOINT"))
	issuerKey := strings.TrimSpace(os.Getenv("QURL_SANDBOX_QV2_ISSUER_KEY"))
	relayURL := strings.TrimSpace(os.Getenv("QURL_SANDBOX_QV2_RELAY_URL"))
	missing := []string{}
	for name, value := range map[string]string{
		"QURL_API_KEY":                key,
		"QURL_ENDPOINT":               endpoint,
		"QURL_SANDBOX_QV2_ISSUER_KEY": issuerKey,
		"QURL_SANDBOX_QV2_RELAY_URL":  relayURL,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Skipf("SKIPPED LOUDLY: live sandbox CRID journey is disarmed — missing %v. "+
			"Arm this by setting QURL_API_KEY (a sandbox key with the qurl:agent, qurl:read, "+
			"qurl:write, and qurl:resolve scopes), QURL_ENDPOINT (the sandbox qURL API base URL — a "+
			"protected input), and the protected QURL_SANDBOX_QV2_ISSUER_KEY / "+
			"QURL_SANDBOX_QV2_RELAY_URL inputs the download step's deployment "+
			"settings are built from.", missing)
	}
	if err := sessionrelay.Validate(relayURL); err != nil {
		t.Fatal("QURL_SANDBOX_QV2_RELAY_URL must be one canonical lowercase HTTPS DNS origin with no credentials, path, query, fragment, or default port; refusing to print the malformed value (CI logs are public)")
	}
	return map[string]string{
		"QURL_API_KEY":                     key,
		"QURL_ENDPOINT":                    endpoint,
		"QURL_DEPLOYMENT":                  journeyDeploymentFile(t, issuerKey, relayURL),
		"QURL_CONNECTOR_SESSION_RELAY_URL": relayURL,
	}
}

// sandboxRunIdentity reads the exact protected workflow run identity. A
// hardened-container run must also bind the immutable image ID that the
// trusted workflow already verified. A host run must not inherit it.
func sandboxRunIdentity() (map[string]string, error) {
	values := map[string]string{
		sandboxRunIDEnv:      strings.TrimSpace(os.Getenv(sandboxRunIDEnv)),
		sandboxRunAttemptEnv: strings.TrimSpace(os.Getenv(sandboxRunAttemptEnv)),
		sandboxRuntimeEnv:    strings.TrimSpace(os.Getenv(sandboxRuntimeEnv)),
	}
	missing := []string{}
	for name, value := range values {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("run-scoped sandbox journey is disarmed — missing %v", missing)
	}
	switch values[sandboxRuntimeEnv] {
	case "host":
		return values, nil
	case "hardened_container":
		imageID := os.Getenv(sandboxQURLImageIDEnv)
		if imageID == "" {
			return nil, fmt.Errorf("run-scoped hardened-container journey is disarmed — missing [%s]", sandboxQURLImageIDEnv)
		}
		if imageID != strings.TrimSpace(imageID) || !sandboxImmutableImageID.MatchString(imageID) {
			return nil, fmt.Errorf("%s must be one exact immutable sha256 image ID", sandboxQURLImageIDEnv)
		}
		values[sandboxQURLImageIDEnv] = imageID
		return values, nil
	default:
		return nil, fmt.Errorf("%s is unsupported; accepted values are host and hardened_container", sandboxRuntimeEnv)
	}
}

// addSandboxRunIdentity adds the validated run identity only to lanes that use
// it for namespace separation or run-scoped receipts. It fails before an exact
// customer process can start when the hardened image binding is absent.
func addSandboxRunIdentity(t *testing.T, env map[string]string) {
	t.Helper()
	values, err := sandboxRunIdentity()
	if err != nil {
		t.Fatal(err)
	}
	// A host lane must not inherit a hardened-container image binding from a
	// reused environment map. sandboxRunIdentity intentionally omits it for
	// host runs; clear any earlier value before copying the validated result.
	delete(env, sandboxQURLImageIDEnv)
	for name, value := range values {
		env[name] = value
	}
}

// journeyDeploymentFile converts the two protected sandbox inputs into
// the SDK's deployment settings file and returns its path. Failures name
// the offending variable but NEVER its value: CI logs are public, and the
// values identify the sandbox.
func journeyDeploymentFile(t *testing.T, issuerKey, relayURL string) string {
	t.Helper()
	kid, keyStd, ok := strings.Cut(issuerKey, "=")
	if !ok || strings.TrimSpace(kid) == "" || strings.TrimSpace(keyStd) == "" {
		t.Fatal("QURL_SANDBOX_QV2_ISSUER_KEY must be of the form <kid>=<standard-base64 key>; refusing to print the malformed value (CI logs are public)")
	}
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keyStd))
	if err != nil {
		t.Fatal("QURL_SANDBOX_QV2_ISSUER_KEY's key part is not valid standard base64; refusing to print the malformed value (CI logs are public)")
	}
	relay, err := url.Parse(relayURL)
	if err != nil || relay.Scheme != "https" || relay.Host == "" {
		t.Fatal("QURL_SANDBOX_QV2_RELAY_URL must be an https URL; refusing to print the malformed value (CI logs are public)")
	}
	doc := map[string]any{
		"issuers": []map[string]string{{
			"kid": strings.TrimSpace(kid),
			// The SDK's settings files carry keys base64url-encoded.
			"spki_der_b64": base64.RawURLEncoding.EncodeToString(der),
		}},
		"cells":           []any{},
		"relay_allowlist": []string{relay.Host},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal deployment settings: %v", err)
	}
	path := filepath.Join(t.TempDir(), "deployment.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write deployment settings: %v", err)
	}
	return path
}

// runSandboxCLI invokes the real command tree against the live sandbox:
// injected environment only (hermetic credential mode), non-TTY streams (the
// piped contracts are what scripts see), the production sleep path so the
// transport's bounded 429 retries wait like they would in the field, and the
// production access opener so `get --file` exercises the real platform
// access flow the injected QURL_DEPLOYMENT settings configure.
func runSandboxCLI(ctx context.Context, t *testing.T, cliEnv map[string]string, args ...string) *runResult {
	t.Helper()
	return runCLI(t, &runOpts{args: args, env: cliEnv, ctx: ctx, realSleep: true, realOpener: true})
}

// journeyPublishDoc mirrors the publish `-o json` document (output/shapes.go).
type journeyPublishDoc struct {
	CRID          string `json:"crid"`
	ResourceID    string `json:"resource_id"`
	TargetURL     string `json:"target_url"`
	FoundExisting bool   `json:"found_existing"`
}

type journeyResourceStatusDoc struct {
	CRID       string `json:"crid"`
	ResourceID string `json:"resource_id"`
	TargetURL  string `json:"target_url"`
	Type       string `json:"type"`
	Status     string `json:"status"`
}

type sandboxInspectionDoc struct {
	CRID            string     `json:"crid"`
	ResourceID      string     `json:"resource_id"`
	TargetURL       string     `json:"target_url"`
	DesiredState    string     `json:"desired_state"`
	ConnectionState string     `json:"connection_state"`
	ServingEpoch    uint64     `json:"serving_epoch"`
	DaemonState     *string    `json:"daemon_state"`
	LastTransition  *time.Time `json:"last_transition"`
	FailureCategory *string    `json:"failure_category"`
	FailureCode     *string    `json:"failure_code"`
	RetryAttempt    *int       `json:"retry_attempt"`
	NextRetryAt     *time.Time `json:"next_retry_at"`
	TargetHealth    *string    `json:"local_target_health"`
}

func sandboxFailureDiagnosticFromInspection(raw []byte) (sandboxFailureDiagnostic, bool) {
	var document sandboxInspectionDoc
	if json.Unmarshal(raw, &document) != nil || document.FailureCategory == nil {
		return sandboxFailureDiagnostic{}, false
	}
	diagnostic := sandboxFailureDiagnostic{Category: *document.FailureCategory}
	if document.FailureCode != nil {
		diagnostic.Code = *document.FailureCode
	}
	if !validSandboxFailureDiagnostic(diagnostic) {
		return sandboxFailureDiagnostic{}, false
	}
	return diagnostic, true
}

func sandboxFailureASCIIIdentifierByte(character byte) bool {
	return (character >= '0' && character <= '9') ||
		(character >= 'A' && character <= 'Z') ||
		(character >= 'a' && character <= 'z') || character == '_'
}

func sandboxFailureDiagnosticFromCLIError(stderr string) (sandboxFailureDiagnostic, bool) {
	const prefix = "error "
	var code string
	for offset := 0; offset < len(stderr); {
		index := strings.Index(stderr[offset:], prefix)
		if index < 0 {
			break
		}
		prefixStart := offset + index
		start := prefixStart + len(prefix)
		end := start
		for end < len(stderr) && stderr[end] >= '0' && stderr[end] <= '9' {
			end++
		}
		candidate := stderr[start:end]
		prefixBoundaryOK := prefixStart == 0 || !sandboxFailureASCIIIdentifierByte(stderr[prefixStart-1])
		codeBoundaryOK := end == len(stderr) || !sandboxFailureASCIIIdentifierByte(stderr[end])
		if validSandboxFailureCode(candidate) && candidate != "" && prefixBoundaryOK && codeBoundaryOK {
			if code != "" {
				return sandboxFailureDiagnostic{}, false
			}
			code = candidate
		}
		offset = start
	}
	if code == "" {
		return sandboxFailureDiagnostic{}, false
	}
	return sandboxFailureDiagnostic{Category: "unknown", Code: code}, true
}

func markSandboxFailureDiagnosticFromCommand(stdout, stderr string, commandErr error) {
	if commandErr == nil {
		if diagnostic, ok := sandboxFailureDiagnosticFromInspection([]byte(stdout)); ok {
			markSandboxFailureDiagnostic(diagnostic)
		}
		return
	}
	if diagnostic, ok := sandboxFailureDiagnosticFromCLIError(stderr); ok {
		markSandboxFailureDiagnostic(diagnostic)
	}
}

func markSandboxFailureLoginDiagnostic(stderr string, err error) {
	writeSandboxFailureLoginDiagnostic(os.Stdout, stderr, err)
}

func writeSandboxFailureLoginDiagnostic(w io.Writer, stderr string, err error) {
	exit := sandboxFailureLoginExitUnknown
	if errors.Is(err, context.DeadlineExceeded) {
		exit = sandboxFailureLoginExitTimeout
	} else if code, ok := sandboxFailureExitCodeFromError(err); ok {
		exit = strconv.Itoa(code)
	}
	_, _ = fmt.Fprintf(w, "%s %s\n", sandboxFailureLoginExitMarker, exit)
	if parsed, ok := sandboxFailureDiagnosticFromCLIError(stderr); ok {
		writeSandboxFailureDiagnostic(w, parsed)
	}
}

// The child process boundary removes the CLI's typed error. Preserve only its
// stable public exit class and an optional five-digit support code; neither can
// carry a credential, request ID, path, endpoint, or private topology.
func sandboxFailureExitCodeFromError(err error) (int, bool) {
	var exitCoder interface{ ExitCode() int }
	if !errors.As(err, &exitCoder) {
		return 0, false
	}
	code := exitCoder.ExitCode()
	return code, validSandboxFailureExitCode(code)
}

func markSandboxFailureDiagnosticFromError(err error) {
	writeSandboxFailureDiagnosticFromError(os.Stdout, err)
}

func writeSandboxFailureDiagnosticFromError(w io.Writer, err error) {
	diagnostic := sandboxFailureDiagnostic{Category: "unknown"}
	if err != nil {
		if parsed, ok := sandboxFailureDiagnosticFromCLIError(err.Error()); ok {
			diagnostic.Code = parsed.Code
		}
	}
	writeSandboxFailureDiagnostic(w, diagnostic)
}

func TestSandboxFailureDiagnosticExtractionIsClosed(t *testing.T) {
	t.Run("inspection", func(t *testing.T) {
		for _, test := range []struct {
			name string
			raw  string
			want sandboxFailureDiagnostic
			ok   bool
		}{
			{name: "category and code", raw: `{"failure_category":"assignment","failure_code":"52201","target_url":"http://127.0.0.1"}`, want: sandboxFailureDiagnostic{Category: "assignment", Code: "52201"}, ok: true},
			{name: "category only", raw: `{"failure_category":"network"}`, want: sandboxFailureDiagnostic{Category: "network"}, ok: true},
			{name: "unknown category", raw: `{"failure_category":"internal_topology","failure_code":"52201"}`},
			{name: "invalid code", raw: `{"failure_category":"identity","failure_code":"secret"}`},
			{name: "missing category", raw: `{"failure_code":"52201"}`},
			{name: "malformed", raw: `{"failure_category":`},
		} {
			t.Run(test.name, func(t *testing.T) {
				got, ok := sandboxFailureDiagnosticFromInspection([]byte(test.raw))
				if ok != test.ok || got != test.want {
					t.Fatalf("inspection diagnostic = %#v, %t; want %#v, %t", got, ok, test.want, test.ok)
				}
			})
		}
	})
	t.Run("CLI error", func(t *testing.T) {
		for _, test := range []struct {
			name   string
			stderr string
			wantOK bool
		}{
			{name: "one canonical code", stderr: "request failed: error 52401", wantOK: true},
			{name: "absent", stderr: "request failed"},
			{name: "short", stderr: "error 5240"},
			{name: "long", stderr: "error 524010"},
			{name: "embedded", stderr: "error 52401secret"},
			{name: "embedded prefix", stderr: "terror 52401"},
			{name: "unicode", stderr: "error ５2401"},
			{name: "multiple", stderr: "error 52201 then error 52401"},
		} {
			t.Run(test.name, func(t *testing.T) {
				got, ok := sandboxFailureDiagnosticFromCLIError(test.stderr)
				if ok != test.wantOK {
					t.Fatalf("CLI diagnostic = %#v, %t; want ok %t", got, ok, test.wantOK)
				}
				if ok && got != (sandboxFailureDiagnostic{Category: "unknown", Code: "52401"}) {
					t.Fatalf("CLI diagnostic = %#v", got)
				}
			})
		}
	})
	t.Run("error marker never relays text", func(t *testing.T) {
		capture := func(err error) string {
			t.Helper()
			var output bytes.Buffer
			writeSandboxFailureDiagnosticFromError(&output, err)
			return output.String()
		}
		const secret = "lv_test_marker_must_not_relay"
		withCode := capture(errors.New(secret + ": assignment error 52028 at a private path"))
		withoutCode := capture(errors.New(secret + ": internal endpoint did not settle"))
		if withCode != sandboxFailureDiagnosticMarker+" unknown 52028\n" ||
			withoutCode != sandboxFailureDiagnosticMarker+" unknown none\n" ||
			strings.Contains(withCode+withoutCode, secret) || strings.Contains(withCode+withoutCode, "private path") ||
			strings.Contains(withCode+withoutCode, "internal endpoint") {
			t.Fatalf("redacted error markers = %q and %q", withCode, withoutCode)
		}
	})
	t.Run("failed login marker is closed", func(t *testing.T) {
		const secret = "lv_test_command_marker_must_not_relay"
		for _, test := range []struct {
			name string
			err  error
			want int
		}{
			{name: "authentication", err: sandboxFailureExitError(exitcode.Auth), want: exitcode.Auth},
			{name: "configuration", err: sandboxFailureExitError(exitcode.Config), want: exitcode.Config},
			{name: "forbidden", err: sandboxFailureExitError(exitcode.Forbidden), want: exitcode.Forbidden},
			{name: "unavailable", err: sandboxFailureExitError(exitcode.Unavailable), want: exitcode.Unavailable},
			{name: "server", err: sandboxFailureExitError(exitcode.ServerError), want: exitcode.ServerError},
		} {
			t.Run(test.name, func(t *testing.T) {
				var output bytes.Buffer
				writeSandboxFailureLoginDiagnostic(&output,
					secret+": https://private.invalid/request/secret error 52401", test.err)
				want := fmt.Sprintf("%s %d\n%s unknown 52401\n",
					sandboxFailureLoginExitMarker, test.want, sandboxFailureDiagnosticMarker)
				if output.String() != want || strings.Contains(output.String(), secret) ||
					strings.Contains(output.String(), "private.invalid") {
					t.Fatalf("closed command diagnostic = %q, want %q", output.String(), want)
				}
			})
		}
		var unknown bytes.Buffer
		writeSandboxFailureLoginDiagnostic(&unknown,
			secret+": error 52201 then error 52401 at https://private.invalid", errors.New(secret))
		if want := sandboxFailureLoginExitMarker + " " + sandboxFailureLoginExitUnknown + "\n"; unknown.String() != want ||
			strings.Contains(unknown.String(), secret) || strings.Contains(unknown.String(), "private.invalid") {
			t.Fatalf("closed unknown login diagnostic = %q, want %q", unknown.String(), want)
		}
		var timeout bytes.Buffer
		writeSandboxFailureLoginDiagnostic(&timeout, secret, context.DeadlineExceeded)
		if want := sandboxFailureLoginExitMarker + " " + sandboxFailureLoginExitTimeout + "\n"; timeout.String() != want ||
			strings.Contains(timeout.String(), secret) {
			t.Fatalf("closed timeout login diagnostic = %q, want %q", timeout.String(), want)
		}
	})
}

type sandboxFailureExitError int

func (e sandboxFailureExitError) Error() string { return "closed test exit" }
func (e sandboxFailureExitError) ExitCode() int { return int(e) }

// assertHealthySandboxInspection proves that inspect is the real redacted
// diagnostic surface, not an alias for status. The healthy journey requires
// every always-present diagnostic and requires failure and retry details to be
// absent when no failure exists.
func assertHealthySandboxInspection(
	t *testing.T,
	raw []byte,
	commandErr error,
	stderr, cridValue, resourceID, desired, observed string,
	epoch uint64,
	forbidden ...string,
) {
	t.Helper()
	if commandErr != nil {
		t.Fatalf("qurl inspect failed: %v; stderr %q", commandErr, stderr)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var document sandboxInspectionDoc
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode qurl inspect output: %v; output %q", err, string(raw))
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("qurl inspect output has trailing data: %q", string(raw))
	}
	if document.CRID != cridValue || document.ResourceID != resourceID || document.DesiredState != desired ||
		document.ConnectionState != observed || document.ServingEpoch != epoch {
		t.Fatalf("qurl inspect lifecycle = %+v, want %s/%s at epoch %d for %s", document, desired, observed, epoch, cridValue)
	}
	if document.DaemonState == nil || *document.DaemonState != "serving" ||
		document.LastTransition == nil || document.LastTransition.IsZero() ||
		document.TargetHealth == nil || *document.TargetHealth != "healthy" ||
		document.RetryAttempt == nil || *document.RetryAttempt != 0 {
		t.Fatalf("qurl inspect healthy diagnostics are incomplete: %+v", document)
	}
	if document.FailureCategory != nil || document.FailureCode != nil || document.NextRetryAt != nil {
		t.Fatalf("qurl inspect exposed failure or retry details for a healthy share: %+v", document)
	}
	for _, secret := range forbidden {
		if secret != "" && bytes.Contains(raw, []byte(secret)) {
			t.Fatal("qurl inspect exposed a bearer credential")
		}
	}
}

// journeyListDoc mirrors the list `-o json` document. HasMore — not cursor
// presence — is the continuation signal, per the ResourcePage contract.
type journeyListDoc struct {
	Resources []struct {
		CRID        string `json:"crid"`
		Description string `json:"description"`
	} `json:"resources"`
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor"`
}

// TestSandboxCRIDJourney walks the whole customer journey against the real
// sandbox: publish → status/inspect → list (paginated) → resolve (verified,
// piped bare-URL) → get --file (real bytes through the minted link) → delete --yes →
// idempotent re-delete → resolve-after-delete (owner-truthful revoked exit).
func TestSandboxCRIDJourney(t *testing.T) {
	cliEnv := sandboxJourneyEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), journeyTimeout)
	defer cancel()

	// Publish one throwaway resource. The target is unique per run (the
	// query string keeps example.com serving its stable 200 page) so
	// serialized runs never adopt each other's resources even if the service
	// dedupes an already-published target. The download step's content
	// assertion leans on the same stability: this page's body is known to
	// carry journeyTargetMarker.
	target := "https://example.com/?qurl-private-sandbox-crid-journey=" + strconv.FormatInt(time.Now().UnixNano(), 10)
	description := sandboxJourneyResourceDescription(t, cliEnv)
	res := runSandboxCLI(ctx, t, cliEnv, "-o", "json", "publish", target, "--description", description)
	if res.code != 0 {
		t.Fatalf("publish exit = %d, want 0\nstderr: %s", res.code, res.stderr.String())
	}
	var pub journeyPublishDoc
	if err := json.Unmarshal(res.stdout.Bytes(), &pub); err != nil {
		t.Fatalf("publish -o json emitted %q: %v", res.stdout.String(), err)
	}
	if pub.CRID == "" {
		t.Fatalf("publish minted no CRID (resource %s); this endpoint cannot carry the CRID journey", pub.ResourceID)
	}
	// Reclaim the one resource no matter where the journey stops below. The
	// happy path deletes it first, so this is normally the idempotent no-op.
	t.Cleanup(func() { reclaimSandboxResource(t, cliEnv, pub.CRID) })
	if pub.FoundExisting {
		t.Logf("note: the service reports the target was already published (found_existing); adopting and reclaiming it")
	}
	t.Logf("published %s -> %s", pub.CRID, pub.TargetURL)

	// The minted identifier is a full-form test-environment CRID: 60
	// characters, and the leading 'q' that marks the test version bytes —
	// the premise of the environment-guard assertion at the resolve step.
	if len(pub.CRID) != 60 || !strings.HasPrefix(pub.CRID, "q") {
		t.Fatalf("CRID = %q (len %d), want the 60-character 'q'-prefixed test-environment form", pub.CRID, len(pub.CRID))
	}
	assessment, err := cridux.Assess(pub.CRID)
	if err != nil || assessment.Kind != cridux.KindCRID {
		t.Fatalf("published CRID %q does not pass the CLI's own local gate (kind %d): %v", pub.CRID, assessment.Kind, err)
	}
	if environment := assessment.CRID.Environment(); environment != crid.EnvironmentTest {
		t.Fatalf("published CRID environment = %v, want the test environment on the sandbox", environment)
	}

	assertRemoteStatusAndInspect(ctx, t, cliEnv, pub)
	assertListFindsCRID(ctx, t, cliEnv, pub.CRID, description)
	link := assertResolveJourney(ctx, t, cliEnv, pub.CRID)
	// The link value never reaches the log: CI logs are public, and a
	// minted link carries the sandbox hostname and a live qURL credential.
	t.Logf("resolved %s -> a verified %d-byte https link", pub.CRID, len(link))
	assertGetDownloadsBytes(ctx, t, cliEnv, pub.CRID)
	assertDeleteJourney(ctx, t, cliEnv, pub.CRID)
}

func assertRemoteStatusAndInspect(ctx context.Context, t *testing.T, cliEnv map[string]string, pub journeyPublishDoc) {
	t.Helper()
	var status journeyResourceStatusDoc
	for _, command := range []string{"status", "inspect"} {
		res := runSandboxCLI(ctx, t, cliEnv, "-o", "json", command, pub.CRID)
		if res.code != 0 {
			t.Fatalf("%s remote URL exit = %d, want 0\nstderr: %s", command, res.code, res.stderr.String())
		}
		var got journeyResourceStatusDoc
		if err := json.Unmarshal(res.stdout.Bytes(), &got); err != nil {
			t.Fatalf("%s remote URL output %q: %v", command, res.stdout.String(), err)
		}
		if got.CRID != pub.CRID || got.ResourceID != pub.ResourceID || got.TargetURL != pub.TargetURL ||
			got.Type != "url" || got.Status != "active" {
			t.Fatalf("%s remote URL state = %+v, want published active URL %+v", command, got, pub)
		}
		if command == "status" {
			status = got
		} else if got != status {
			t.Fatalf("inspect remote URL state = %+v, want status state %+v", got, status)
		}
	}
}

// listPageLimit keeps pages small enough that pagination is real without
// hammering the tenancy; listMaxPages bounds the walk so a broken cursor can
// never spin the suite.
const (
	listPageLimit = 10
	listMaxPages  = 25
)

// assertListFindsCRID walks `qurl list -o json` page by page under the
// documented pagination contract — continue exactly while has_more, via
// next_cursor — and requires the published CRID to appear exactly once in
// what it walks (a page overlap or a dropped row is a pagination bug even
// when the row is eventually found). It then holds the listed row to
// carrying the label publish gave it, which is the only thing that makes a
// leaked fixture identifiable to anything built on `qurl list`.
//
// The shared tenancy accumulates rows from every sandbox surface (bots,
// suites), so the whole listing can legitimately outgrow any fixed page
// budget — it first crossed listMaxPages*listPageLimit rows on 2026-08-19,
// turning the old walk-everything Fatal into a permanent red. The walk
// therefore stops at the budget and asserts over the window it scanned.
// That window is sufficient because the row under test was published
// moments ago and the platform lists newest first.
//
// The walk asks for --status active for the same reason: deleting a
// resource is a status flip, not a row removal, so every run of every
// sandbox suite leaves a permanent revoked row in the unfiltered listing.
// Filtering spends the page budget on rows that could still be leaked
// fixtures instead of on the tenancy's accumulated history. That filter
// couples the exactly-once assertion to the row still being active when
// the walk runs — it is, because the journey deletes only afterwards. A
// future reordering that deletes first has to drop the filter with it.
//
// TODO(upstream-contract): two qurl-service behaviors hold this walk up.
// created_at descending is its pinned default sort for the resource listing
// (handlers/server.go, "default: created_at:desc") — if that default ever
// changes, the window argument breaks loudly (seen stays 0) and the walk
// needs an explicit sort or a different presence strategy. `?status=` is
// honored server-side (handlers/resource.go parses it into ListFilters) —
// if it ever stops being, the filter silently becomes a no-op and this walk
// quietly reverts to scanning the whole history.
func assertListFindsCRID(ctx context.Context, t *testing.T, cliEnv map[string]string, id, expectedDescription string) {
	t.Helper()
	seen := 0
	label := ""
	cursor := ""
	pages := 0
	for page := 1; ; page++ {
		args := []string{"-o", "json", "list", "--limit", strconv.Itoa(listPageLimit), "--status", "active"}
		if cursor != "" {
			args = append(args, "--cursor", cursor)
		}
		res := runSandboxCLI(ctx, t, cliEnv, args...)
		if res.code != 0 {
			t.Fatalf("list page %d exit = %d, want 0\nstderr: %s", page, res.code, res.stderr.String())
		}
		var doc journeyListDoc
		if err := json.Unmarshal(res.stdout.Bytes(), &doc); err != nil {
			t.Fatalf("list page %d -o json emitted %q: %v", page, res.stdout.String(), err)
		}
		for _, row := range doc.Resources {
			if row.CRID == id {
				seen++
				label = row.Description
			}
		}
		pages = page
		if !doc.HasMore {
			break
		}
		if doc.NextCursor == "" {
			t.Fatalf("list page %d reports has_more with no next_cursor; pagination cannot continue", page)
		}
		if page == listMaxPages {
			t.Logf("list still reports more rows after %d pages of %d; asserting over the newest-first window walked so far", listMaxPages, listPageLimit)
			break
		}
		cursor = doc.NextCursor
	}
	if seen != 1 {
		t.Fatalf("published CRID appeared %d times across %d newest-first list pages, want exactly once", seen, pages)
	}
	// Not a Fatal: the row was found, so the rest of the journey (resolve,
	// download, delete) is still worth running and still reclaims the row.
	if label != expectedDescription {
		t.Errorf("listed row description = %q, want %q; nothing built on `qurl list` can identify this fixture",
			label, expectedDescription)
	}
}

// assertResolveJourney resolves the CRID and holds the piped contract: with
// stdout not a terminal the command emits the bare link and nothing else, so
// `link="$(qurl resolve <CRID>)"` captures it cleanly (the link opens in a
// browser — downloading is get's job). Exit 0 here IS the verification
// evidence — the CLI discards any answer that fails CRID verification before
// printing (exit 12), so a printed link is a verified link. It also holds
// the environment-guard case: the sandbox is the test environment, so its
// 'q'-prefixed CRID at this non-production endpoint must produce no warning
// at all.
func assertResolveJourney(ctx context.Context, t *testing.T, cliEnv map[string]string, id string) string {
	t.Helper()
	res := runSandboxCLI(ctx, t, cliEnv, "resolve", id)
	link, err := validateSandboxResolveCommandResult("resolve", res.code, res.stdout.String(), res.stderr.String())
	if err != nil {
		t.Fatal(err)
	}
	return link
}

// journeyTargetMarker is the distinctive body text of the published target
// (example.com's stable page). The download assertion requires it, so the
// test can only pass when the actual target content was fetched — never
// when some other document (the in-browser verification page above all)
// was saved in its place.
const journeyTargetMarker = "Example Domain"

// assertGetDownloadsBytes pulls the published target's content through
// `get --file` and requires it to BE that content: the bytes must carry
// the target page's known marker and must not be the platform's in-browser
// verification page. It also holds the atomic-download contract: payload
// in the destination file, nothing on stdout, no .part left behind.
//
// The two content assertions are the regression guard for the defect where
// `get --file` saved the in-browser page (which a plain GET of a
// fragment-credential link always yields) and reported success. A size
// check alone passed on that page; content identity cannot. On failure,
// nothing about the payload is logged beyond sizes and booleans — the
// in-browser page embeds deployment hostnames, and CI logs are public.
func assertGetDownloadsBytes(ctx context.Context, t *testing.T, cliEnv map[string]string, id string) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "journey-payload")
	res := runSandboxCLI(ctx, t, cliEnv, "get", id, "--file", dest)
	if res.code != 0 {
		t.Fatalf("get --file exit = %d, want 0\nstderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
	if !strings.Contains(res.stderr.String(), "Saved to") {
		t.Errorf("get --file stderr = %q, want the saved-to confirmation", res.stderr.String())
	}
	// #nosec G304 -- dest is this test's own t.TempDir path.
	payload, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("downloaded file missing: %v", err)
	}
	if !bytes.Contains(payload, []byte(journeyTargetMarker)) {
		t.Errorf("downloaded %d bytes do not contain the published target's %q marker; "+
			"get saved something other than the target content (payload deliberately not logged)",
			len(payload), journeyTargetMarker)
	}
	if bytes.Contains(payload, []byte(apitest.InterstitialTitle)) {
		t.Errorf("downloaded %d bytes carry the in-browser verification page's title; "+
			"get saved the page a browser needs instead of the target content", len(payload))
	}
	if _, err := os.Stat(dest + ".part"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("atomic download left %s.part behind (stat err: %v)", dest, err)
	}
	t.Logf("downloaded %d bytes of verified target content", len(payload))
}

// assertDeleteJourney deletes the resource, proves the re-delete is the
// idempotent success the platform promises, and then holds the
// owner-truthful revoked path: resolving a deleted resource with the
// owner's own key exits with the not-found code and an honest "deleted"
// story on stderr — never a link, never a silent success.
func assertDeleteJourney(ctx context.Context, t *testing.T, cliEnv map[string]string, id string) {
	t.Helper()
	res := runSandboxCLI(ctx, t, cliEnv, "delete", id, "--yes")
	if res.code != 0 {
		t.Fatalf("delete exit = %d, want 0\nstderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
	if !strings.Contains(res.stderr.String(), "Deleted "+id) {
		t.Errorf("delete stderr = %q, want the deletion confirmation", res.stderr.String())
	}

	res = runSandboxCLI(ctx, t, cliEnv, "delete", id, "--yes")
	if res.code != 0 {
		t.Fatalf("re-delete exit = %d, want the idempotent 0\nstderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
	// Both shapes are platform-legal per the verified delete contract: a
	// soft-revoked row answers the second DELETE with 204 (the CLI truthfully
	// prints the deletion confirmation again — proven by this suite's first
	// live run), while a hard-reaped row answers 404 (the CLI's already-gone
	// note). Either way the exit is the idempotent 0 asserted above.
	stderrText := res.stderr.String()
	if !strings.Contains(stderrText, msgAlreadyGone) && !strings.Contains(stderrText, "Deleted "+id) {
		t.Errorf("re-delete stderr = %q, want the already-gone note or the repeated deletion confirmation", stderrText)
	}

	res = runSandboxCLI(ctx, t, cliEnv, "resolve", id)
	if err := validateSandboxDeletedCommandResult(
		"resolve",
		res.code,
		res.stdout.String(),
		res.stderr.String(),
	); err != nil {
		t.Fatal(err)
	}
}

// reclaimSandboxResource is the quota-safety net: delete the journey's one
// resource by CRID on its own fresh context (the journey's context may
// already be dead when a failure lands here). Already-gone is the expected
// idempotent success; any other outcome is a leaked fixture on the shared
// tenancy and fails the run loudly.
func reclaimSandboxResource(t *testing.T, cliEnv map[string]string, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res := runCLI(t, &runOpts{args: []string{"delete", id, "--yes"}, env: cliEnv, ctx: ctx, realSleep: true, realOpener: true})
	if res.code != 0 {
		t.Errorf("cleanup: delete %s exit = %d — the throwaway resource may be leaked on the sandbox tenancy\nstderr: %s",
			id, res.code, res.stderr.String())
	}
}
