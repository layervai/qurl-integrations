package output

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/layervai/qurl-go/qurl"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/sessionrelay"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

func TestResolveColor(t *testing.T) {
	noColorSet := lookupFrom(map[string]string{"NO_COLOR": ""})
	cases := []struct {
		name   string
		mode   string
		lookup func(string) (string, bool)
		tty    bool
		want   bool
	}{
		{"always wins without tty", ColorAlways, lookupFrom(nil), false, true},
		{"always wins over NO_COLOR", ColorAlways, noColorSet, true, true},
		{"never wins on tty", ColorNever, lookupFrom(nil), true, false},
		{"auto tty", ColorAuto, lookupFrom(nil), true, true},
		{"auto piped", ColorAuto, lookupFrom(nil), false, false},
		{"auto tty with NO_COLOR set (even empty)", ColorAuto, noColorSet, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveColor(tc.mode, tc.lookup, tc.tty); got != tc.want {
				t.Errorf("ResolveColor = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolveASCII(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"no locale set assumes UTF-8", nil, false},
		{"explicit UTF-8", map[string]string{"LANG": "en_US.UTF-8"}, false},
		{"lowercase utf8", map[string]string{"LC_CTYPE": "en_US.utf8"}, false},
		{"C locale degrades", map[string]string{"LC_ALL": "C"}, true},
		{"latin-1 degrades", map[string]string{"LANG": "en_US.ISO-8859-1"}, true},
		{"LC_ALL beats LANG", map[string]string{"LC_ALL": "C", "LANG": "en_US.UTF-8"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveASCII(lookupFrom(tc.env)); got != tc.want {
				t.Errorf("ResolveASCII = %v, want %v", got, tc.want)
			}
		})
	}
}

func fixedClock() time.Time { return time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC) }

func newTestPrinter(out, err *bytes.Buffer, format Format, quiet, color, ascii bool) *Printer {
	s := &Streams{Out: out, Err: err, In: strings.NewReader("")}
	return New(s, format, quiet, color, ascii, fixedClock)
}

func TestTextHelpers(t *testing.T) {
	var out, errBuf bytes.Buffer
	p := newTestPrinter(&out, &errBuf, FormatText, false, false, false)

	if got := formatDuration(26 * time.Hour); got != "1d" {
		t.Errorf("formatDuration = %q", got)
	}
	if got := formatDuration(90 * time.Second); got != "1m" {
		t.Errorf("formatDuration = %q", got)
	}
	created := fixedClock().Add(-30 * time.Second)
	if got := p.relativeTime(created); got != "just now" {
		t.Errorf("relativeTime = %q", got)
	}
	if got := p.relativeTime(fixedClock().Add(-3 * time.Hour)); got != "3h ago" {
		t.Errorf("relativeTime = %q", got)
	}
	if got := p.formatExpiry(fixedClock().Add(-time.Minute)); got != expiredLabel {
		t.Errorf("formatExpiry = %q", got)
	}

	long := strings.Repeat("x", 60)
	short := p.middleEllipsis(long, 24)
	if utf8.RuneCountInString(short) > 24 || !strings.Contains(short, "…") {
		t.Errorf("middleEllipsis = %q (runes %d)", short, utf8.RuneCountInString(short))
	}
	if got := p.middleEllipsis("short", 24); got != "short" {
		t.Errorf("middleEllipsis must not touch short values, got %q", got)
	}

	// Multi-byte inputs must be cut on rune boundaries (target columns can
	// carry IDN hosts and non-ASCII paths); a byte-sliced cut would emit
	// invalid UTF-8 into the table.
	wide := strings.Repeat("ü", 30)
	if got := p.middleEllipsis(wide, 24); !utf8.ValidString(got) || utf8.RuneCountInString(got) != 24 {
		t.Errorf("middleEllipsis multibyte = %q (valid=%t runes=%d)", got, utf8.ValidString(got), utf8.RuneCountInString(got))
	}
	if got := p.truncateEnd(wide, 24); !utf8.ValidString(got) || utf8.RuneCountInString(got) != 24 {
		t.Errorf("truncateEnd multibyte = %q (valid=%t runes=%d)", got, utf8.ValidString(got), utf8.RuneCountInString(got))
	}

	ap := newTestPrinter(&out, &errBuf, FormatText, false, false, true)
	if got := ap.middleEllipsis(long, 24); strings.Contains(got, "…") || !strings.Contains(got, "...") {
		t.Errorf("ascii degradation missing: %q", got)
	}
	if got := ap.truncateEnd(long, 10); got != "xxxxxxx..." {
		t.Errorf("truncateEnd ascii = %q", got)
	}
}

// TestStreamDiscipline pins the contract: data on stdout, prose on stderr.
func TestStreamDiscipline(t *testing.T) {
	var out, errBuf bytes.Buffer
	p := newTestPrinter(&out, &errBuf, FormatText, false, false, false)

	// Empty list: nothing on the data stream, note on stderr.
	if err := p.List(&qurlapi.ResourcePage{}); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("empty list wrote data: %q", out.String())
	}
	if !strings.Contains(errBuf.String(), "No resources found") {
		t.Errorf("expected the empty note on stderr, got %q", errBuf.String())
	}

	// Delete confirmation is prose.
	out.Reset()
	errBuf.Reset()
	if err := p.Delete("someid", false); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("delete confirmation leaked onto stdout: %q", out.String())
	}
	if !strings.Contains(errBuf.String(), "Deleted someid.") {
		t.Errorf("stderr = %q", errBuf.String())
	}

	// Already-gone suppresses the confirmation: the caller's note has said it,
	// and a second "Deleted" line would contradict it.
	out.Reset()
	errBuf.Reset()
	if err := p.Delete("someid", true); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || errBuf.Len() != 0 {
		t.Errorf("already-gone delete must render nothing: stdout=%q stderr=%q", out.String(), errBuf.String())
	}

	// Warnings never touch stdout.
	out.Reset()
	errBuf.Reset()
	p.Warnf("something to know")
	if out.Len() != 0 || !strings.Contains(errBuf.String(), "Warning: something to know") {
		t.Errorf("warn: stdout=%q stderr=%q", out.String(), errBuf.String())
	}
}

func TestQuietProjections(t *testing.T) {
	var out, errBuf bytes.Buffer
	p := newTestPrinter(&out, &errBuf, FormatText, true, false, false)

	if err := p.Publish(&qurlapi.Published{CRID: "thecrid", ResourceID: "rid"}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "thecrid\n" {
		t.Errorf("quiet publish = %q", out.String())
	}

	out.Reset()
	if err := p.Resolve(&qurlapi.Resolved{QURL: "https://qurl.link/#x"}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "https://qurl.link/#x\n" {
		t.Errorf("quiet resolve = %q", out.String())
	}
}

// TestPublishFoundExistingTriState pins the local reconciliation boundary:
// unknown provenance must never be rendered as a confirmed fresh publish.
// Known service answers remain explicit for scripts in both directions.
func TestPublishFoundExistingTriState(t *testing.T) {
	t.Parallel()
	knownFalse := false
	knownTrue := true
	for _, tc := range []struct {
		name          string
		foundExisting *bool
		wantJSON      string
		wantNote      bool
	}{
		{name: "unknown is omitted", foundExisting: nil},
		{name: "known fresh is explicit", foundExisting: &knownFalse, wantJSON: `"found_existing": false`},
		{name: "known existing is explicit", foundExisting: &knownTrue, wantJSON: `"found_existing": true`, wantNote: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out, errBuf bytes.Buffer
			p := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false)
			if err := p.Publish(&qurlapi.Published{
				CRID:          "thecrid",
				ResourceID:    "rid",
				TargetURL:     "http://127.0.0.1:3000",
				FoundExisting: tc.foundExisting,
			}); err != nil {
				t.Fatal(err)
			}
			if tc.wantJSON == "" {
				if strings.Contains(out.String(), `"found_existing"`) {
					t.Fatalf("unknown provenance claimed a boolean outcome: %s", out.String())
				}
			} else if !strings.Contains(out.String(), tc.wantJSON) {
				t.Fatalf("JSON = %s, want %s", out.String(), tc.wantJSON)
			}
			if got := strings.Contains(errBuf.String(), msgAlreadyPublished); got != tc.wantNote {
				t.Fatalf("already-published note = %t, want %t; stderr=%q", got, tc.wantNote, errBuf.String())
			}
		})
	}

	for _, quiet := range []bool{false, true} {
		var out, errBuf bytes.Buffer
		p := newTestPrinter(&out, &errBuf, FormatText, quiet, false, false)
		if err := p.Publish(&qurlapi.Published{
			CRID:          "thecrid",
			TargetURL:     "http://127.0.0.1:3000",
			FoundExisting: nil,
		}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "Already published") || strings.Contains(out.String(), msgPublishFoundExisting) || errBuf.Len() != 0 {
			t.Fatalf("unknown provenance made an existing/fresh claim: stdout=%q stderr=%q", out.String(), errBuf.String())
		}
	}
}

// TestRedactionGrepProof plants a credential in every input a formatter or
// error rendering touches on the diagnostic surfaces and asserts the secret
// never reaches the rendered bytes.
func TestRedactionGrepProof(t *testing.T) {
	const secret = "lv_live_SUPERSECRETVALUE0000001"
	var out, errBuf bytes.Buffer
	p := newTestPrinter(&out, &errBuf, FormatText, false, true, false)

	p.Warnf("problem with key %s", secret)
	p.Notef("retrying with %s", secret)

	renderTargets := []error{
		&qurlapi.Error{StatusCode: 400, Title: "Bad Request", Detail: "key " + secret + " malformed", RequestID: "req_1"},
		fmt.Errorf("wrap: %w", errors.New("raw failure mentioning "+secret)),
		fmt.Errorf("authz Bearer %s rejected", secret),
	}
	for _, err := range renderTargets {
		RenderError(&errBuf, err, true)
	}

	combined := out.String() + errBuf.String()
	if strings.Contains(combined, "SUPERSECRETVALUE") {
		t.Fatalf("a credential survived redaction:\n%s", combined)
	}
	if !strings.Contains(combined, "lv_***") {
		t.Errorf("expected the redaction marker in output:\n%s", combined)
	}
}

func TestRenderErrorAnatomies(t *testing.T) {
	var buf bytes.Buffer

	// A stopped Connector keeps the existing unavailable class, but the
	// machine-readable service code selects a fixed, actionable message before
	// the generic 503 posture. Server-controlled detail must not be repeated.
	stopped := fmt.Errorf("%w: %w", qurl.ErrTemporaryAccessLinksDisabled, &qurlapi.Error{
		StatusCode: http.StatusServiceUnavailable,
		Code:       "CoNnEcToR_StOpPeD",
		Title:      "Connector Stopped",
		Detail:     "internal sandbox cell us-secret-1 uses credential lv_live_NEVER_PRINT_THIS",
	})
	RenderError(&buf, stopped, false)
	if got := buf.String(); got != "Error: "+msgConnectorStopped+"\n\n  "+hintConnectorStopped+"\n" {
		t.Errorf("stopped Connector rendering = %q", got)
	}
	for _, banned := range []string{"sandbox", "us-secret-1", "lv_live_", "Connector Stopped"} {
		if strings.Contains(buf.String(), banned) {
			t.Errorf("server-controlled text %q reached the customer surface: %q", banned, buf.String())
		}
	}
	if got := exitcode.FromError(stopped); got != exitcode.Unavailable {
		t.Errorf("stopped Connector exit code = %d, want %d", got, exitcode.Unavailable)
	}

	buf.Reset()
	// Typed dark-surface posture wins over the generic API rendering.
	dark := fmt.Errorf("%w: %w", qurl.ErrTemporaryAccessLinksDisabled, &qurlapi.Error{
		StatusCode: http.StatusServiceUnavailable,
		Code:       "service_unavailable",
	})
	RenderError(&buf, dark, false)
	if got := buf.String(); got != "Error: "+msgLinksUnavailable+"\n" {
		t.Errorf("dark rendering = %q", buf.String())
	}

	buf.Reset()
	RenderError(&buf, auth.ErrNoCredential, false)
	if !strings.Contains(buf.String(), "QURL_API_KEY") {
		t.Errorf("credential rendering lacks the remedy: %q", buf.String())
	}

	buf.Reset()
	RenderError(&buf, &qurlapi.Error{
		StatusCode:    422,
		Title:         "Validation failed",
		Detail:        "the request had invalid fields",
		InvalidFields: map[string]string{"target_url": "must be http or https", "alias": "too long"},
		RequestID:     "req_9",
	}, false)
	rendered := buf.String()
	for _, want := range []string{"Validation failed (HTTP 422)", "Invalid fields:", "alias: too long", "target_url: must be http or https", "Request ID: req_9"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("anatomy missing %q in:\n%s", want, rendered)
		}
	}
	// Fields render sorted.
	if strings.Index(rendered, "alias:") > strings.Index(rendered, "target_url:") {
		t.Errorf("invalid fields not sorted:\n%s", rendered)
	}

	buf.Reset()
	custom := exitcode.InvalidInputError("stop applies only to a local qURL Connector", &qurlapi.Error{
		StatusCode: http.StatusBadRequest,
		Title:      "Invalid Input",
		RequestID:  "req_stop",
	})
	RenderError(&buf, custom, false)
	rendered = buf.String()
	if !strings.Contains(rendered, "stop applies only to a local qURL Connector") ||
		!strings.Contains(rendered, "Request ID: req_stop") || strings.Contains(rendered, "Invalid Input") {
		t.Errorf("custom invalid-input rendering = %q", rendered)
	}
}

func TestRenderEnrollmentScopeRemedy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteProblem(t, w, http.StatusForbidden, "insufficient_scope", "Forbidden", "minting enrollment tokens requires qurl:agent")
	}))
	t.Cleanup(srv.Close)
	client, err := qurlapi.New(&qurlapi.Config{
		BaseURL: srv.URL,
		APIKey:  "lv_test_logincredential123456789",
		Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.MintConnectorEnrollmentToken(context.Background(), qurlapi.MintConnectorEnrollmentTokenOptions{
		ConnectorID:    "local-scope-test",
		IdempotencyKey: "0123456789abcdef0123456789abcdef",
	})
	if err == nil {
		t.Fatal("mint unexpectedly succeeded")
	}

	var buf bytes.Buffer
	RenderError(&buf, fmt.Errorf("bootstrap local Connector: %w", err), false)
	got := buf.String()
	if !strings.Contains(got, "registered device") || !strings.Contains(got, "publish local apps") {
		t.Errorf("operation-specific remedy missing:\n%s", got)
	}
	if strings.Contains(got, hintScope) {
		t.Errorf("generic resource-scope remedy won over enrollment remedy:\n%s", got)
	}
}

// TestConnectorAssignmentRenderings is the customer-language contract for
// qurl-go's enrollment/assignment taxonomy. Every row is asserted twice —
// bare, and wrapped the way the enroll/refresh path really wraps it — because
// a mapping that only fires on a bare sentinel is dead code on the real path:
// agent.registerRuntime returns the SDK's *AssignmentError as-is and
// refreshRuntime wraps it with "refresh native assignment binding: %w".
func TestConnectorAssignmentRenderings(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		headline string
		hint     string
	}{
		{"bootstrap consumed", qurl.ErrAssignmentBootstrapConsumed, msgConnectorTokenConsumed, hintConnectorTokenConsumed},
		{"key rejected", qurl.ErrAssignmentKeyRejected, msgConnectorTokenRejected, hintConnectorTokenRejected},
		{"request rejected", qurl.ErrAssignmentRequestRejected, msgConnectorEnrollmentRejected, hintConnectorEnrollmentRejected},
		{"registration disabled", qurl.ErrAssignmentRegistrationDisabled, msgConnectorEnrollmentDisabled, hintConnectorEnrollmentDisabled},
		{"identity rejected", qurl.ErrAssignmentIdentityRejected, msgConnectorIdentityRejected, hintConnectorIdentityRejected},
		{"quota exceeded", qurl.ErrAssignmentQuotaExceeded, msgConnectorQuotaExceeded, hintConnectorQuotaExceeded},
		{"rate limited", qurl.ErrAssignmentRateLimited, msgConnectorAssignmentUnavailable, hintConnectorAssignmentUnavailable},
		{"unavailable", qurl.ErrAssignmentUnavailable, msgConnectorAssignmentUnavailable, hintConnectorAssignmentUnavailable},
		{"reassignment required", qurl.ErrAssignmentReassignmentRequired, msgConnectorAssignmentUnavailable, hintConnectorAssignmentUnavailable},
		{"recovery required", qurl.ErrAssignmentRecoveryRequired, msgConnectorAssignmentUnavailable, hintConnectorAssignmentUnavailable},
		{"lease expired", qurl.ErrAssignmentLeaseExpired, msgConnectorAssignmentExpired, hintConnectorAssignmentExpired},
		{"invalid response", qurl.ErrAssignmentInvalidResponse, msgConnectorAssignmentInvalid, hintConnectorAssignmentInvalid},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, shape := range []struct {
				label string
				err   error
			}{
				{"bare", tc.err},
				{"wrapped", fmt.Errorf("refresh native assignment binding: %w", tc.err)},
				{"double wrapped", fmt.Errorf("open Connector runtime: %w", fmt.Errorf("native registration: %w", tc.err))},
			} {
				var buf bytes.Buffer
				RenderError(&buf, shape.err, false)
				got := buf.String()
				if !strings.Contains(got, tc.headline) {
					t.Errorf("%s: headline missing\nwant: %s\ngot:\n%s", shape.label, tc.headline, got)
				}
				if !strings.Contains(got, tc.hint) {
					t.Errorf("%s: hint missing\nwant: %s\ngot:\n%s", shape.label, tc.hint, got)
				}
			}
		})
	}
}

func TestConnectorRecoveryCredentialRenderingHidesSDKInternals(t *testing.T) {
	err := errors.Join(
		&qurl.CredentialRecoveryError{Code: "52401", Phase: "hub_issue_recovery"},
		qurl.ErrRecoveryCredentialRejected,
	)
	var buf bytes.Buffer
	RenderError(&buf, fmt.Errorf("recover rejected native identity: %w", err), false)
	got := buf.String()
	for _, want := range []string{msgConnectorRecoveryCredentialRejected, hintConnectorRecoveryCredentialRejected} {
		if !strings.Contains(got, want) {
			t.Fatalf("recovery rendering missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"hub_issue_recovery", "52401", "qurl:"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("recovery rendering exposed SDK detail %q:\n%s", forbidden, got)
		}
	}
}

func TestConnectorRecoveryTaxonomyRenderingHidesEveryWirePhase(t *testing.T) {
	tests := []struct {
		name     string
		code     string
		phase    string
		sentinel error
		headline string
		hint     string
	}{
		{"hub unavailable", "52400", "hub_issue_recovery", qurl.ErrCredentialRecoveryUnavailable, msgConnectorRecoveryUnavailable, hintConnectorRecoveryUnavailable},
		{"hub identity rejected", "52402", "hub_issue_recovery", qurl.ErrCredentialRecoveryIdentityRejected, msgConnectorRecoveryIdentityRejected, hintConnectorRecoveryIdentityRejected},
		{"revoke required", "52403", "hub_issue_recovery", qurl.ErrCredentialRecoveryRevokeRequired, msgConnectorRecoveryRevokeRequired, hintConnectorRecoveryRevokeRequired},
		{"rate limited", "52404", "hub_issue_recovery", qurl.ErrCredentialRecoveryRateLimited, msgConnectorRecoveryUnavailable, hintConnectorRecoveryUnavailable},
		{"hub invalid", "52405", "hub_issue_recovery", qurl.ErrCredentialRecoveryRequestRejected, msgConnectorRecoveryInvalid, hintConnectorRecoveryInvalid},
		{"assignment required", "52406", "hub_issue_recovery", qurl.ErrCredentialRecoveryAssignmentRequired, msgConnectorRecoveryUnavailable, hintConnectorRecoveryUnavailable},
		{"replacement unavailable", "52410", "assigned_cell_complete_recovery", qurl.ErrCredentialReplacementUnavailable, msgConnectorRecoveryUnavailable, hintConnectorRecoveryUnavailable},
		{"grant rejected", "52411", "assigned_cell_complete_recovery", qurl.ErrCredentialRecoveryGrantRejected, msgConnectorRecoveryUnavailable, hintConnectorRecoveryUnavailable},
		{"cell identity rejected", "52412", "assigned_cell_complete_recovery", qurl.ErrCredentialRecoveryIdentityRejected, msgConnectorRecoveryIdentityRejected, hintConnectorRecoveryIdentityRejected},
		{"candidate conflict", "52413", "assigned_cell_complete_recovery", qurl.ErrCredentialRecoveryCandidateConflict, msgConnectorRecoveryConflict, hintConnectorRecoveryConflict},
		{"cell invalid", "52414", "assigned_cell_complete_recovery", qurl.ErrCredentialRecoveryRequestRejected, msgConnectorRecoveryInvalid, hintConnectorRecoveryInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := errors.Join(
				&qurl.CredentialRecoveryError{Code: test.code, Phase: test.phase},
				test.sentinel,
			)
			if test.code == "52400" || test.code == "52404" || test.code == "52410" {
				err = &qurl.CredentialRecoveryRetryRequiredError{
					Phase: test.phase, Attempts: 3, Elapsed: time.Second, Last: err,
				}
			}
			var buf bytes.Buffer
			RenderError(&buf, fmt.Errorf("recover rejected native identity: %w", err), false)
			got := buf.String()
			for _, want := range []string{test.headline, test.hint} {
				if !strings.Contains(got, want) {
					t.Fatalf("recovery rendering missing %q:\n%s", want, got)
				}
			}
			for _, forbidden := range []string{test.phase, test.code, "qurl:"} {
				if strings.Contains(got, forbidden) {
					t.Fatalf("recovery rendering exposed SDK detail %q:\n%s", forbidden, got)
				}
			}
		})
	}
}

func TestConnectorRecoveryStateRenderingHidesSDKDetails(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		headline string
		hint     string
	}{
		{"persistence", qurl.ErrCredentialRecoveryCandidatePersistence, msgConnectorRecoveryPersistence, hintConnectorRecoveryPersistence},
		{"invalid response", qurl.ErrCredentialRecoveryInvalidResponse, msgConnectorRecoveryInvalid, hintConnectorRecoveryInvalid},
		{"expired", qurl.ErrCredentialRecoveryExpired, msgConnectorRecoveryExpired, hintConnectorRecoveryExpired},
		{"retry exhausted", qurl.ErrCredentialRecoveryRetryRequired, msgConnectorRecoveryUnavailable, hintConnectorRecoveryUnavailable},
		{"refresh required", qurl.ErrCredentialRecoveredAssignmentRefreshRequired, msgConnectorRecoveryUnavailable, hintConnectorRecoveryUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var buf bytes.Buffer
			RenderError(&buf, fmt.Errorf("recover rejected native identity: internal phase: %w", test.err), false)
			got := buf.String()
			for _, want := range []string{test.headline, test.hint} {
				if !strings.Contains(got, want) {
					t.Fatalf("recovery rendering missing %q:\n%s", want, got)
				}
			}
			if strings.Contains(got, "internal phase") || strings.Contains(got, "qurl:") {
				t.Fatalf("recovery rendering exposed SDK detail:\n%s", got)
			}
		})
	}
}

func TestConnectorDeviceCredentialRenderingHidesIdentityAndSDKAdvice(t *testing.T) {
	err := &qurl.NativeCredentialRecoveryRequiredError{
		AgentID: "agent-private-identity",
		Cause:   fmt.Errorf("internal recovery phase: %w", qurl.ErrDeviceCredentialMissing),
	}
	var buf bytes.Buffer
	RenderError(&buf, fmt.Errorf("open registered runtime: %w", err), false)
	got := buf.String()
	for _, want := range []string{msgConnectorDeviceCredential, hintConnectorDeviceCredential} {
		if !strings.Contains(got, want) {
			t.Fatalf("device credential rendering missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"agent-private-identity", "RecoverAgentRuntime", "internal recovery phase", "qurl:", "X25519"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("device credential rendering exposed %q:\n%s", forbidden, got)
		}
	}
}

func TestConnectorPeerTimeoutRenderingHidesEndpointAndTopology(t *testing.T) {
	err := errors.Join(
		&qurl.EndpointNoReplyError{
			Endpoint: "private-cell.sandbox.invalid:443",
			Attempts: 4,
			Elapsed:  3 * time.Second,
			Last:     errors.New("private route dropped UDP"),
		},
		qurl.ErrAssignmentRecoveryRequired,
	)
	var buf bytes.Buffer
	RenderError(&buf, fmt.Errorf("refresh assignment: %w", err), false)
	got := buf.String()
	for _, want := range []string{msgConnectorPeerTimeout, hintConnectorPeerTimeout} {
		if !strings.Contains(got, want) {
			t.Fatalf("peer-timeout rendering missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"private-cell", "sandbox", "private route", "source-fenced", "qurl:", "UDP"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("peer-timeout rendering exposed %q:\n%s", forbidden, got)
		}
	}
}

func TestConnectorEnrollmentRenderingsHideSDKInternals(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		headline string
		hint     string
	}{
		{"invalid local config", qurl.ErrInvalidRegisterConfig, msgConnectorEnrollmentConfig, hintConnectorEnrollmentConfig},
		{"binding persistence", qurl.ErrAgentBindingPersistence, msgConnectorEnrollmentPersistence, hintConnectorEnrollmentPersistence},
		{"candidate persistence", qurl.ErrAgentCompletionCandidatePersistence, msgConnectorEnrollmentPersistence, hintConnectorEnrollmentPersistence},
		{"setup lock", qurl.ErrAgentSetupLock, msgConnectorEnrollmentPersistence, hintConnectorEnrollmentPersistence},
		{"REG recovery", qurl.ErrRegistrationRecoveryRequired, msgConnectorEnrollmentUnavailable, hintConnectorEnrollmentUnavailable},
		{"REG rate limited", qurl.ErrRegistrationRateLimited, msgConnectorEnrollmentUnavailable, hintConnectorEnrollmentUnavailable},
		{"ticket expired", qurl.ErrAssignmentTicketExpired, msgConnectorEnrollmentUnavailable, hintConnectorEnrollmentUnavailable},
		{"completion unavailable", qurl.ErrCompletionUnavailable, msgConnectorEnrollmentUnavailable, hintConnectorEnrollmentUnavailable},
		{"completion recovery", qurl.ErrCompletionRecoveryRequired, msgConnectorEnrollmentUnavailable, hintConnectorEnrollmentUnavailable},
		{"key rejected", qurl.ErrKeyRejected, msgConnectorTokenRejected, hintConnectorTokenRejected},
		{"bootstrap consumed", qurl.ErrBootstrapSetupKeyConsumed, msgConnectorTokenConsumed, hintConnectorTokenConsumed},
		{"completion identity", qurl.ErrCompletionIdentityRejected, msgConnectorEnrollmentIdentity, hintConnectorEnrollmentIdentity},
		{"agent identity conflict", qurl.ErrAgentIdentityConflict, msgConnectorEnrollmentConflict, hintConnectorEnrollmentConflict},
		{"completion conflict", qurl.ErrCompletionCredentialConflict, msgConnectorEnrollmentConflict, hintConnectorEnrollmentConflict},
		{"REG invalid input", qurl.ErrRegistrationInvalidInput, msgConnectorEnrollmentInvalid, hintConnectorEnrollmentInvalid},
		{"completion rejected", qurl.ErrCompletionRequestRejected, msgConnectorEnrollmentInvalid, hintConnectorEnrollmentInvalid},
		{"registration disabled", qurl.ErrRegistrationDisabled, msgConnectorEnrollmentDisabled, hintConnectorEnrollmentDisabled},
		{"device quota", qurl.ErrDeviceKeyQuotaExceeded, msgConnectorDeviceQuota, hintConnectorDeviceQuota},
		{"ticket invalid", qurl.ErrAssignmentTicketInvalid, msgConnectorEnrollmentMismatch, hintConnectorEnrollmentMismatch},
		{"reply malformed", qurl.ErrRegisterReplyMalformed, msgConnectorEnrollmentMismatch, hintConnectorEnrollmentMismatch},
		{"key kind disallowed", qurl.ErrRegistrationKeyKindDisallowed, msgConnectorEnrollmentMismatch, hintConnectorEnrollmentMismatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := fmt.Errorf(
				"private assigned-cell phase for agent-private; call WithAgentRuntimeHeadlessEnrollment; code 52399: %w",
				test.err,
			)
			var buf bytes.Buffer
			RenderError(&buf, err, false)
			got := buf.String()
			for _, want := range []string{test.headline, test.hint} {
				if !strings.Contains(got, want) {
					t.Fatalf("enrollment rendering missing %q:\n%s", want, got)
				}
			}
			for _, forbidden := range []string{"assigned-cell", "agent-private", "WithAgentRuntime", "52399", "qurl:"} {
				if strings.Contains(got, forbidden) {
					t.Fatalf("enrollment rendering exposed %q:\n%s", forbidden, got)
				}
			}
		})
	}
}

func TestConnectorResourceRenderings(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		code     string
		headline string
		hint     string
	}{
		{"invalid local request", qurl.ErrInvalidNativeConnectorResourceRequest, "", msgConnectorResourceInvalidRequest, hintConnectorResourceInvalidRequest},
		{"request rejected", qurl.ErrConnectorResourceRequestRejected, "52506", msgConnectorResourceInvalidRequest, hintConnectorResourceInvalidRequest},
		{"identity rejected", qurl.ErrConnectorResourceIdentityRejected, "52501", msgConnectorIdentityRejected, hintConnectorIdentityRejected},
		{"entitlement", qurl.ErrConnectorResourceEntitlementDenied, "52502", msgConnectorResourceEntitlement, hintConnectorResourceEntitlement},
		{"continuity conflict", qurl.ErrConnectorResourceIdentityConflict, "52503", msgConnectorResourceConflict, hintConnectorResourceConflict},
		{"quota", qurl.ErrConnectorResourceQuotaExceeded, "52504", msgConnectorResourceQuota, hintConnectorResourceQuota},
		{"rate limited", qurl.ErrConnectorResourceRateLimited, "52505", msgConnectorResourceUnavailable, hintConnectorResourceUnavailable},
		{"unavailable", qurl.ErrConnectorResourceUnavailable, "52500", msgConnectorResourceUnavailable, hintConnectorResourceUnavailable},
		{"invalid response", qurl.ErrInvalidNativeConnectorResourceResponse, "", msgConnectorResourceInvalidResponse, hintConnectorResourceInvalidResponse},
		{"local verification", state.ErrConnectorResourceVerification, "", msgConnectorResourceLocalVerification, hintConnectorResourceLocalVerification},
		{"local cross-Connector conflict", state.ErrConnectorResourceStateConflict, "", msgConnectorResourceLocalConflict, hintConnectorResourceLocalConflict},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			poison := "assigned cell private-cell.example:443 returned LRT during routing/knock with QURL_CONNECTOR_HUB_HOST and qurl: raw detail"
			err := fmt.Errorf("%s: %w", poison, test.err)
			if test.code != "" {
				err = errors.Join(err, &qurl.ConnectorResourceDiscoveryError{Code: test.code})
			}
			for _, rendering := range []struct {
				err      error
				wantCode bool
			}{
				{err: test.err},
				{err: err, wantCode: test.code != ""},
			} {
				var buf bytes.Buffer
				RenderError(&buf, rendering.err, false)
				got := buf.String()
				if !strings.Contains(got, test.headline) || !strings.Contains(got, test.hint) {
					t.Fatalf("rendered error missing customer posture:\n%s", got)
				}
				if rendering.wantCode && !strings.Contains(got, labelConnectorErrorCode+" "+test.code) {
					t.Fatalf("rendered error missing safe support code %q:\n%s", test.code, got)
				}
				for _, forbidden := range []string{
					"assigned cell", "private-cell.example", "LRT", "routing/knock",
					"QURL_CONNECTOR_HUB_HOST", "qurl: raw detail",
				} {
					if strings.Contains(got, forbidden) {
						t.Fatalf("resource rendering exposed %q:\n%s", forbidden, got)
					}
				}
			}
		})
	}

	t.Run("malformed support code stays hidden", func(t *testing.T) {
		err := errors.Join(
			qurl.ErrConnectorResourceUnavailable,
			&qurl.ConnectorResourceDiscoveryError{Code: "host1"},
		)
		var buf bytes.Buffer
		RenderError(&buf, err, false)
		if got := buf.String(); strings.Contains(got, "host1") || strings.Contains(got, labelConnectorErrorCode) {
			t.Fatalf("malformed support code reached customer output:\n%s", got)
		}
	})
}

func TestConnectorConnectionConfigRenderingHidesTopology(t *testing.T) {
	for name, err := range map[string]error{
		"Hub": fmt.Errorf(
			"%w: private-cell.example:443 Hub server public key via QURL_CONNECTOR_HUB_HOST/QURL_CONNECTOR_HUB_PORT",
			hub.ErrConfig,
		),
		"session relay": fmt.Errorf(
			"%w: https://private-relay.example/secret via QURL_CONNECTOR_SESSION_RELAY_URL",
			sessionrelay.ErrConfig,
		),
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			RenderError(&buf, err, false)
			got := buf.String()
			for _, want := range []string{msgConnectorConnectionConfig, hintConnectorConnectionConfig} {
				if !strings.Contains(got, want) {
					t.Fatalf("connection configuration rendering missing %q:\n%s", want, got)
				}
			}
			for _, forbidden := range []string{
				"private-cell.example", "private-relay.example", "secret", "Hub", "server public key",
				"QURL_CONNECTOR_HUB_", "QURL_CONNECTOR_SESSION_RELAY_URL", ":443",
			} {
				if strings.Contains(got, forbidden) {
					t.Fatalf("connection configuration rendering exposed %q:\n%s", forbidden, got)
				}
			}
		})
	}
}

// TestConnectorAssignmentOrdering pins the two overlaps that a naive switch
// order would render wrongly, because in both the SDK (or the CLI) matches two
// sentinels at once.
func TestConnectorAssignmentOrdering(t *testing.T) {
	// AgentAssignment.Validate wraps an expired lease with BOTH
	// ErrAssignmentInvalidResponse and ErrAssignmentLeaseExpired. The lease
	// reading has to win, or every expiry reads as a platform-contract fault.
	t.Run("expired lease beats invalid response", func(t *testing.T) {
		both := fmt.Errorf("%w: assignment lease must be in the future: %w",
			qurl.ErrAssignmentInvalidResponse, qurl.ErrAssignmentLeaseExpired)
		var buf bytes.Buffer
		RenderError(&buf, both, false)
		if !strings.Contains(buf.String(), msgConnectorAssignmentExpired) {
			t.Errorf("want the expiry headline, got:\n%s", buf.String())
		}
		if strings.Contains(buf.String(), msgConnectorAssignmentInvalid) {
			t.Errorf("the contract-violation headline must not win:\n%s", buf.String())
		}
	})
}

// TestConnectorRequestRejectedDropsSDKRemedy is the regression guard for the
// reported defect: the 52109 rendering must not put the SDK's own sentence —
// which names a Go option and prescribes the wrong fix — in front of a
// customer. The detail block is suppressed for exactly this case.
func TestConnectorRequestRejectedDropsSDKRemedy(t *testing.T) {
	// The shape the live sandbox Hub produced: the SDK's error text, wrapped.
	sdkText := "qurl: native Hub assignment request rejected (52109); " +
		"correct WithAgentRuntimeIdentity or the Hub request contract before retrying"
	live := fmt.Errorf("%s: %w", sdkText, qurl.ErrAssignmentRequestRejected)

	var buf bytes.Buffer
	RenderError(&buf, live, false)
	got := buf.String()

	if !strings.Contains(got, msgConnectorEnrollmentRejected) {
		t.Errorf("want the customer headline, got:\n%s", got)
	}
	for _, banned := range []string{"WithAgentRuntimeIdentity", "request contract", "52109"} {
		if strings.Contains(got, banned) {
			t.Errorf("SDK vocabulary %q reached the customer surface:\n%s", banned, got)
		}
	}
}

// TestEveryConnectorMessageIsRegistered guards the jargon gate's reach: a
// headline or hint that renders but is missing from CustomerMessages is never
// checked for jargon by cmd's gate.
func TestEveryConnectorMessageIsRegistered(t *testing.T) {
	registered := map[string]bool{}
	for _, msg := range CustomerMessages() {
		registered[msg] = true
	}
	rendered := []string{
		labelCRID,
		msgConnectorStopped, hintConnectorStopped,
		msgConnectorResourceLocalVerification, hintConnectorResourceLocalVerification,
		msgConnectorResourceLocalConflict, hintConnectorResourceLocalConflict,
		msgConnectorTokenConsumed, hintConnectorTokenConsumed,
		msgConnectorTokenRejected, hintConnectorTokenRejected,
		msgConnectorEnrollmentRejected, hintConnectorEnrollmentRejected,
		msgConnectorEnrollmentDisabled, hintConnectorEnrollmentDisabled,
		msgConnectorRecoveryCredentialRejected, hintConnectorRecoveryCredentialRejected,
		msgConnectorRecoveryIdentityRejected, hintConnectorRecoveryIdentityRejected,
		msgConnectorRecoveryRevokeRequired, hintConnectorRecoveryRevokeRequired,
		msgConnectorRecoveryUnavailable, hintConnectorRecoveryUnavailable,
		msgConnectorRecoveryConflict, hintConnectorRecoveryConflict,
		msgConnectorRecoveryPersistence, hintConnectorRecoveryPersistence,
		msgConnectorRecoveryInvalid, hintConnectorRecoveryInvalid,
		msgConnectorRecoveryExpired, hintConnectorRecoveryExpired,
		msgConnectorDeviceCredential, hintConnectorDeviceCredential,
		msgConnectorPeerTimeout, hintConnectorPeerTimeout,
		msgConnectorEnrollmentConfig, hintConnectorEnrollmentConfig,
		msgConnectorEnrollmentUnavailable, hintConnectorEnrollmentUnavailable,
		msgConnectorEnrollmentIdentity, hintConnectorEnrollmentIdentity,
		msgConnectorEnrollmentConflict, hintConnectorEnrollmentConflict,
		msgConnectorEnrollmentInvalid, hintConnectorEnrollmentInvalid,
		msgConnectorEnrollmentMismatch, hintConnectorEnrollmentMismatch,
		msgConnectorDeviceQuota, hintConnectorDeviceQuota,
		msgConnectorEnrollmentPersistence, hintConnectorEnrollmentPersistence,
		msgConnectorIdentityRejected, hintConnectorIdentityRejected,
		msgConnectorQuotaExceeded, hintConnectorQuotaExceeded,
		msgConnectorAssignmentUnavailable, hintConnectorAssignmentUnavailable,
		msgConnectorAssignmentInvalid, hintConnectorAssignmentInvalid,
		msgConnectorAssignmentExpired, hintConnectorAssignmentExpired,
	}
	for _, msg := range rendered {
		if !registered[msg] {
			t.Errorf("message not registered in CustomerMessages, so the jargon gate never sees it: %q", msg)
		}
	}
}

func TestJSONProjectionIsRepoOwned(t *testing.T) {
	var out, errBuf bytes.Buffer
	p := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false)
	expires := time.Date(2026, 3, 1, 0, 5, 0, 0, time.UTC)
	if err := p.Resolve(&qurlapi.Resolved{
		QURL:             "https://qurl.link/#x",
		CRID:             "acrid",
		Type:             "qv2",
		ExpiresAt:        expires,
		ExpiresInSeconds: 300,
		SingleUse:        true,
	}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{`"qurl"`, `"crid"`, `"type"`, `"expires_at"`, `"expires_in_seconds"`, `"single_use"`} {
		if !strings.Contains(got, want) {
			t.Errorf("resolve JSON missing key %s:\n%s", want, got)
		}
	}
}

// TestListJSONCarriesRowMetadata pins the one projection that exposes a
// row's publish-time metadata. Absent fields must stay absent rather than
// render as empty values. The line this draws is "a real label is present"
// versus "no visible label" — it deliberately does not separate unset from
// redacted (the service omits description and tags on connector-owned rows,
// and both cases arrive here as the zero value, so both drop the key). What
// it buys is that the document never asserts `"description": ""` as though
// the server had said so.
func TestListJSONCarriesRowMetadata(t *testing.T) {
	var out, errBuf bytes.Buffer
	p := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false)
	if err := p.List(&qurlapi.ResourcePage{Items: []qurlapi.ResourceSummary{
		{
			CRID:        "labeled",
			ResourceID:  "r1",
			TargetURL:   "https://a.example",
			Type:        "url",
			Status:      "active",
			Description: "cli sandbox e2e journey (safe to delete)",
			Tags:        []string{"sandbox", "e2e"},
		},
		{CRID: "bare", ResourceID: "r2", Status: "revoked"},
	}}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		`"type": "url"`,
		`"description": "cli sandbox e2e journey (safe to delete)"`,
		`"sandbox"`,
		`"e2e"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("list JSON missing %s:\n%s", want, got)
		}
	}
	// The bare row is the second document element; nothing may synthesize
	// an empty description, tag list, or type for it.
	second := strings.Index(got, `"bare"`)
	if second < 0 {
		t.Fatalf("second row missing from the document:\n%s", got)
	}
	bare := got[second:]
	for _, unwanted := range []string{`"description"`, `"tags"`, `"type"`} {
		if strings.Contains(bare, unwanted) {
			t.Errorf("row without metadata emitted %s:\n%s", unwanted, bare)
		}
	}
}

func TestListJSONCarriesZeroTunnelEpochButNotURLLifecycleFields(t *testing.T) {
	var out, errBuf bytes.Buffer
	p := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false)
	if err := p.List(&qurlapi.ResourcePage{Items: []qurlapi.ResourceSummary{
		{CRID: "tunnel", ResourceID: "r1", Type: "tunnel", Status: "active", DesiredState: qurlapi.DesiredStateOff},
		{CRID: "url", ResourceID: "r2", Type: "url", Status: "active"},
	}}); err != nil {
		t.Fatal(err)
	}
	var document struct {
		Resources []map[string]any `json:"resources"`
	}
	if err := json.Unmarshal(out.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if got, ok := document.Resources[0]["serving_epoch"]; !ok || got != float64(0) {
		t.Fatalf("tunnel serving_epoch = %#v, present=%v; want explicit zero", got, ok)
	}
	if got := document.Resources[0]["desired_state"]; got != "off" {
		t.Fatalf("tunnel desired_state = %#v, want off", got)
	}
	if _, ok := document.Resources[0]["connection_state"]; ok {
		t.Fatalf("list fabricated a live tunnel observation: %#v", document.Resources[0])
	}
	if _, ok := document.Resources[1]["serving_epoch"]; ok {
		t.Fatalf("URL row emitted tunnel serving_epoch: %#v", document.Resources[1])
	}
	if _, ok := document.Resources[1]["desired_state"]; ok {
		t.Fatalf("URL row emitted tunnel desired_state: %#v", document.Resources[1])
	}
}

// TestListTextOmitsRowMetadata keeps publish metadata JSON-only while the text
// table adds only lifecycle state needed to operate a share.
func TestListTextOmitsRowMetadata(t *testing.T) {
	var out, errBuf bytes.Buffer
	p := newTestPrinter(&out, &errBuf, FormatText, false, false, false)
	if err := p.List(&qurlapi.ResourcePage{Items: []qurlapi.ResourceSummary{{
		CRID:        "acrid",
		ResourceID:  "r1",
		TargetURL:   "https://a.example",
		Type:        "url",
		Status:      "active",
		Description: "a description nobody asked the table to render",
		Tags:        []string{"sandbox"},
	}}}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, unwanted := range []string{"DESCRIPTION", "TAGS", "a description nobody asked", "sandbox"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("text table rendered %q; the metadata columns are deliberately absent:\n%s", unwanted, got)
		}
	}
	// tabwriter pads the header into columns, so compare fields.
	header := strings.Fields(strings.SplitN(got, "\n", 2)[0])
	if want := []string{"CRID", "TARGET", "DESIRED", "OBSERVED", "CREATED", "EXPIRES"}; !slices.Equal(header, want) {
		t.Errorf("table header = %v, want %v", header, want)
	}
}

func TestListTextKeepsFullCRIDTargetAndTunnelStates(t *testing.T) {
	var out, errBuf bytes.Buffer
	p := newTestPrinter(&out, &errBuf, FormatText, false, false, false)
	full := "qf4ucjgkv5qabcdefghijklmnopqrstuvwxyz0123456789abbkntl3eifq"
	longTarget := "http://127.0.0.1:3000/a/path/longer/than/forty/characters/with-tail"
	if err := p.List(&qurlapi.ResourcePage{Items: []qurlapi.ResourceSummary{
		{CRID: full, ResourceID: "r1", TargetURL: longTarget, Type: "tunnel", DesiredState: qurlapi.DesiredStateOn},
		{CRID: "remote", ResourceID: "r2", Type: "tunnel", DesiredState: qurlapi.DesiredStateOff},
		{CRID: "serving", ResourceID: "r3", TargetURL: "http://127.0.0.1:4000", Type: "tunnel", DesiredState: qurlapi.DesiredStateOn},
	}}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{full, longTarget, "on", "off", "unknown"} {
		if !strings.Contains(got, want) {
			t.Errorf("list table missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "…") {
		t.Fatalf("list table truncated identity:\n%s", got)
	}
}

func TestValidFormat(t *testing.T) {
	if !ValidFormat(FormatText) || !ValidFormat(FormatJSON) {
		t.Error("canonical formats must validate")
	}
	if ValidFormat("yaml") || ValidFormat("") {
		t.Error("unknown formats must not validate")
	}
}
