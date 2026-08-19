package output

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/layervai/qurl-go/qurl"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
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
	if err := p.Publish(&qurlapi.Published{ResourceID: "rid-only"}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "rid-only\n" {
		t.Errorf("quiet publish without CRID = %q", out.String())
	}

	out.Reset()
	if err := p.Resolve(&qurlapi.Resolved{QURL: "https://qurl.link/#x"}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "https://qurl.link/#x\n" {
		t.Errorf("quiet resolve = %q", out.String())
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

	// Typed dark-surface posture wins over the generic API rendering.
	dark := fmt.Errorf("%w: %w", qurl.ErrTemporaryAccessLinksDisabled, &qurlapi.Error{StatusCode: 503})
	RenderError(&buf, dark, false)
	if !strings.Contains(buf.String(), "aren't available from this qURL endpoint") {
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

	// agent.ErrRefreshAlreadyAttempted is joined with its warm-open cause, so
	// it can carry an assignment sentinel too. The CLI's lifecycle reading is
	// the more specific one and must win.
	t.Run("CLI lifecycle beats the SDK sentinel it carries", func(t *testing.T) {
		joined := errors.Join(
			fmt.Errorf("%w in this failure episode", agent.ErrRefreshAlreadyAttempted),
			fmt.Errorf("warm-open after attempted refresh: %w", qurl.ErrAssignmentLeaseExpired),
		)
		var buf bytes.Buffer
		RenderError(&buf, joined, false)
		if !strings.Contains(buf.String(), msgConnectorRefreshExhausted) {
			t.Errorf("want the refresh-exhausted headline, got:\n%s", buf.String())
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

// TestConnectorServingNote pins the startup note byte-exactly at the
// rendering seam: the CRID lands last and alone on its line (the copy
// contract), a platform that returned no CRID gets the original one-line note
// with no empty label, and color styles only the label — never the CRID
// itself, which would bury escape bytes inside a value people paste.
func TestConnectorServingNote(t *testing.T) {
	const headline = "Starting Connector \"billing\" for your local app at 127.0.0.1:8080. Press Ctrl-C to stop.\n"

	var out, errBuf bytes.Buffer
	p := newTestPrinter(&out, &errBuf, FormatText, false, false, false)
	p.ConnectorServing("billing", "127.0.0.1:8080", "acrid")
	want := headline + "\n  Anyone authorized can reach it with `qurl get <CRID>`.\n\nCRID: acrid\n"
	if got := errBuf.String(); got != want {
		t.Errorf("serving note =\n%q\nwant\n%q", got, want)
	}
	// The note is status, not data: a serve loop that runs until interrupted
	// has no stdout document to put it in.
	if out.Len() != 0 {
		t.Errorf("serving note wrote to stdout: %q", out.String())
	}

	errBuf.Reset()
	p.ConnectorServing("billing", "127.0.0.1:8080", "")
	if got := errBuf.String(); got != headline {
		t.Errorf("note without a CRID =\n%q\nwant the unchanged one-liner\n%q", got, headline)
	}

	errBuf.Reset()
	colored := newTestPrinter(&out, &errBuf, FormatText, false, true, false)
	colored.ConnectorServing("billing", "127.0.0.1:8080", "acrid")
	if got := errBuf.String(); !strings.HasSuffix(got, " acrid\n") {
		t.Errorf("colored note must end with the bare CRID, got %q", got)
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
		msgConnectorServing, msgConnectorReachIt, labelCRID,
		msgConnectorTokenConsumed, hintConnectorTokenConsumed,
		msgConnectorTokenRejected, hintConnectorTokenRejected,
		msgConnectorEnrollmentRejected, hintConnectorEnrollmentRejected,
		msgConnectorEnrollmentDisabled, hintConnectorEnrollmentDisabled,
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

// TestListTextOmitsRowMetadata pins the deliberate table decision: the five
// columns stay as they are, and the metadata reaches scripts through JSON
// only. The apps/cli goldens pin the same thing from the other side (their
// mock row carries a description that never appears in the table).
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
	// tabwriter pads the header into columns, so compare the fields rather
	// than the raw line: exactly the five documented columns, in order.
	header := strings.Fields(strings.SplitN(got, "\n", 2)[0])
	if want := []string{"CRID", "TARGET", "STATUS", "CREATED", "EXPIRES"}; !slices.Equal(header, want) {
		t.Errorf("table header = %v, want %v", header, want)
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
