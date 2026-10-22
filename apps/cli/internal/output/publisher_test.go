package output

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

const fixturePublisherLink = "https://qurl.link/#x"

var fixtureCreated = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

// publisherPrinter builds a printer whose stdout TTY-ness is chosen by the
// test, which newTestPrinter does not expose.
func publisherPrinter(out, errBuf *bytes.Buffer, format Format, tty, quiet, color, ascii bool) *Printer {
	streams := &Streams{Out: out, Err: errBuf, In: strings.NewReader(""), OutIsTTY: tty, ErrIsTTY: tty}
	return New(streams, format, quiet, color, ascii, fixedClock)
}

func fixtureShareLink(publisher qurlapi.Publisher, created *time.Time) *qurlapi.ShareLink {
	return &qurlapi.ShareLink{
		QURL: fixturePublisherLink, CRID: "acrid", Type: "qv2",
		ExpiresInSeconds: 300, SingleUse: true,
		ResourceCreatedAt: created, Publisher: publisher,
	}
}

// hostileNames are publisher names a resource owner (or a service answering
// outside its contract) could try to use to forge terminal output. The
// service refuses all of them; the renderer must be safe anyway.
var hostileNames = map[string]string{
	"escape sequence":    "\x1b[2K\x1b[1G\x1b[32mLayerV\x1b[0m",
	"bidi override":      "Acme\u202e)VreL yb deifirev( \u202c",
	"bidi isolate":       "Acme\u2066\u2069",
	"zero width":         "Ac\u200bme\u200d\ufeff",
	"embedded quote":     `Acme" — verified by LayerV "`,
	"backslash":          `Acme\" — verified`,
	"newline":            "Acme\nPublisher: \"LayerV\" — verified by LayerV",
	"carriage return":    "Acme\rLayerV — verified by LayerV            ",
	"tab":                "Acme\tDocs",
	"NUL and DEL":        "Ac\x00me\x7f",
	"C1 control":         "Acme\u009b31m",
	"line separator":     "Acme\u2028LayerV",
	"invalid UTF-8":      "Acme\xff\xfeDocs",
	"no-break space":     "Acme\u00a0Docs",
	"private use":        "Acme\ue000\U000f0000",
	"tag characters":     "Acme\U000e0041",
	"percent verbs":      "Acme %s %d %!v(MISSING)",
	"only control bytes": "\x07\x08",
}

var ansiStyles = []string{ansiReset, ansiBold, ansiDim, ansiRed, ansiGreen, ansiYellow}

// mustBeTerminalSafe asserts text is valid UTF-8 holding nothing but
// printable characters and line feeds, once the printer's own styling is
// removed. A raw escape, control, or format character from a name fails it.
func mustBeTerminalSafe(t *testing.T, where, text string) {
	t.Helper()
	for _, style := range ansiStyles {
		text = strings.ReplaceAll(text, style, "")
	}
	if !utf8.ValidString(text) {
		t.Errorf("%s wrote invalid UTF-8: %q", where, text)
	}
	for _, r := range text {
		if r != '\n' && !strconv.IsPrint(r) {
			t.Errorf("%s wrote raw %U to the terminal: %q", where, r, text)
			return
		}
	}
}

// publisherSurface is one rendering that shows a publisher.
type publisherSurface struct {
	name   string
	render func(*Printer, qurlapi.Publisher) error
}

// textShowsPublisher reports whether the surface's text output shows the
// publisher. Four deliberately do not, and carry it in JSON only: the list
// table, the publish document, the outcome of a lifecycle change (start,
// stop, restart), and the download confirmation, whose publisher was already
// announced by the notice before the content was fetched.
func (s publisherSurface) textShowsPublisher() bool {
	switch s.name {
	case "list", "publish", "lifecycle", "download":
		return false
	default:
		return true
	}
}

// TestTextSurfacesWithoutAPublisherRow pins that deliberate absence: a first
// publish or a start must not open with a status word about the publisher,
// and a download does not repeat a notice it already gave.
func TestTextSurfacesWithoutAPublisherRow(t *testing.T) {
	t.Parallel()
	for _, surface := range publisherSurfaces() {
		if surface.textShowsPublisher() {
			continue
		}
		for _, tty := range []bool{false, true} {
			var out, errBuf bytes.Buffer
			p := publisherPrinter(&out, &errBuf, FormatText, tty, false, false, false)
			if err := surface.render(p, qurlapi.Publisher{Name: "Acme Docs"}); err != nil {
				t.Fatal(err)
			}
			shown := out.String() + errBuf.String()
			for _, unwanted := range []string{"Acme Docs", labelPublisher, msgPublisherUnverified} {
				if strings.Contains(shown, unwanted) {
					t.Errorf("%s text (tty=%t) shows %q:\n%s", surface.name, tty, unwanted, shown)
				}
			}
		}
	}
}

func publisherSurfaces() []publisherSurface {
	created := fixtureCreated
	sharing := func(publisher qurlapi.Publisher) *qurlapi.Sharing {
		return &qurlapi.Sharing{
			CRID: "acrid", ResourceID: "rid", DesiredState: qurlapi.DesiredStateOn,
			ConnectionState: qurlapi.ConnectionServing, ServingEpoch: 5,
			CreatedAt: &created, Publisher: publisher,
		}
	}
	return []publisherSurface{
		{"share", func(p *Printer, publisher qurlapi.Publisher) error {
			return p.ShareLink(fixtureShareLink(publisher, &created))
		}},
		{"download", func(p *Printer, publisher qurlapi.Publisher) error {
			return p.Downloaded(fixtureShareLink(publisher, &created), "out.bin", 23)
		}},
		{"notice", func(p *Printer, publisher qurlapi.Publisher) error {
			p.PublisherNotice(fixtureShareLink(publisher, &created))
			return nil
		}},
		{"publish", func(p *Printer, publisher qurlapi.Publisher) error {
			return p.Publish(&qurlapi.Published{
				CRID: "acrid", ResourceID: "rid", TargetURL: "https://example.com", Status: "active",
				CreatedAt: &created, Publisher: publisher,
			})
		}},
		{"list", func(p *Printer, publisher qurlapi.Publisher) error {
			return p.List(&qurlapi.ResourcePage{Items: []qurlapi.ResourceSummary{{
				CRID: "acrid", ResourceID: "rid", Type: "url", Status: "active", Publisher: publisher,
			}}})
		}},
		{"status", func(p *Printer, publisher qurlapi.Publisher) error {
			return p.ResourceStatus(&qurlapi.ResourceSummary{
				CRID: "acrid", ResourceID: "rid", Type: "url", Status: "active",
				CreatedAt: &created, Publisher: publisher,
			})
		}},
		{"grants", func(p *Printer, publisher qurlapi.Publisher) error {
			return p.Grants(&qurlapi.ResourceSummary{
				CRID: "acrid", ResourceID: "rid", Type: "url", Status: "active",
				CreatedAt: &created, Publisher: publisher,
			})
		}},
		{"sharing status", func(p *Printer, publisher qurlapi.Publisher) error {
			return p.SharingStatus("http://127.0.0.1:3000", sharing(publisher))
		}},
		{"lifecycle", func(p *Printer, publisher qurlapi.Publisher) error {
			return p.Sharing("http://127.0.0.1:3000", sharing(publisher))
		}},
		{"inspect", func(p *Printer, publisher qurlapi.Publisher) error {
			return p.InspectSharing(&SharingInspection{State: sharing(publisher), DaemonState: "running", TargetHealth: "healthy"})
		}},
		{"profile", func(p *Printer, publisher qurlapi.Publisher) error {
			return p.PublisherProfile(&publisher, PublisherNameSet)
		}},
	}
}

// TestPublisherNameNeverReachesATerminalRaw runs every hostile name through
// every surface, in every mode, and holds two properties: no raw control,
// format, or other non-printing character is written, and the name cannot
// add or remove a line (so it cannot forge a row of its own).
func TestPublisherNameNeverReachesATerminalRaw(t *testing.T) {
	t.Parallel()
	type mode struct {
		name                     string
		format                   Format
		tty, quiet, color, ascii bool
	}
	modes := []mode{
		{name: "terminal color", format: FormatText, tty: true, color: true},
		{name: "terminal plain", format: FormatText, tty: true},
		{name: "terminal ascii", format: FormatText, tty: true, ascii: true},
		{name: "piped", format: FormatText},
		{name: "piped color", format: FormatText, color: true},
		{name: "quiet", format: FormatText, tty: true, quiet: true},
		{name: "json", format: FormatJSON, tty: true},
	}
	render := func(surface publisherSurface, m mode, publisher qurlapi.Publisher) (stdout, stderr string) {
		var out, errBuf bytes.Buffer
		p := publisherPrinter(&out, &errBuf, m.format, m.tty, m.quiet, m.color, m.ascii)
		if err := surface.render(p, publisher); err != nil {
			t.Fatalf("%s/%s: %v", surface.name, m.name, err)
		}
		return out.String(), errBuf.String()
	}
	for _, surface := range publisherSurfaces() {
		for _, m := range modes {
			benignOut, benignErr := render(surface, m, qurlapi.Publisher{Name: "Acme Docs"})
			for label, hostile := range hostileNames {
				stdout, stderr := render(surface, m, qurlapi.Publisher{Name: hostile})
				where := surface.name + "/" + m.name + "/" + label
				mustBeTerminalSafe(t, where+" stdout", stdout)
				mustBeTerminalSafe(t, where+" stderr", stderr)
				if got, want := strings.Count(stdout, "\n"), strings.Count(benignOut, "\n"); got != want {
					t.Errorf("%s: the name changed stdout from %d to %d lines:\n%s", where, want, got, stdout)
				}
				if got, want := strings.Count(stderr, "\n"), strings.Count(benignErr, "\n"); got != want {
					t.Errorf("%s: the name changed stderr from %d to %d lines:\n%s", where, want, got, stderr)
				}
			}
		}
	}
}

// TestPublisherStatusNeverSeparatesFromTheName pins that every text line
// carrying a publisher name also carries the status word, in capitals, with
// and without color.
func TestPublisherStatusNeverSeparatesFromTheName(t *testing.T) {
	t.Parallel()
	const name = "Acme Docs"
	for _, surface := range publisherSurfaces() {
		if !surface.textShowsPublisher() {
			continue
		}
		for _, color := range []bool{false, true} {
			for _, tty := range []bool{false, true} {
				var out, errBuf bytes.Buffer
				p := publisherPrinter(&out, &errBuf, FormatText, tty, false, color, false)
				if err := surface.render(p, qurlapi.Publisher{Name: name}); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, line := range strings.Split(out.String()+errBuf.String(), "\n") {
					if !strings.Contains(line, name) {
						continue
					}
					found = true
					if !strings.Contains(line, `"`+name+`"`) {
						t.Errorf("%s (tty=%t color=%t): name is not quoted: %q", surface.name, tty, color, line)
					}
					if !strings.Contains(line, msgPublisherUnverified) {
						t.Errorf("%s (tty=%t color=%t): name rendered without %s: %q", surface.name, tty, color, msgPublisherUnverified, line)
					}
					if color != strings.Contains(line, ansiBold+ansiYellow+msgPublisherUnverified+ansiReset) {
						t.Errorf("%s (tty=%t): color=%t but the status word styling disagrees: %q", surface.name, tty, color, line)
					}
				}
				if !found {
					t.Errorf("%s (tty=%t color=%t) did not show the publisher:\nstdout=%q\nstderr=%q",
						surface.name, tty, color, out.String(), errBuf.String())
				}
			}
		}
	}
}

func TestPublisherStatusExactText(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name         string
		publisher    qurlapi.Publisher
		color, ascii bool
		want         string
	}{
		{
			name:      "named unverified",
			publisher: qurlapi.Publisher{Name: "Acme Docs"},
			want:      `"Acme Docs" — UNVERIFIED (self-declared name, not confirmed by LayerV)`,
		},
		{
			name: "unnamed unverified",
			want: `no name provided — UNVERIFIED (not confirmed by LayerV)`,
		},
		{
			name:      "named unverified color",
			publisher: qurlapi.Publisher{Name: "Acme Docs"},
			color:     true,
			want:      `"Acme Docs" — ` + "\x1b[1m\x1b[33mUNVERIFIED\x1b[0m" + ` (self-declared name, not confirmed by LayerV)`,
		},
		{
			name:  "unnamed unverified color",
			color: true,
			want:  "no name provided — \x1b[1m\x1b[33mUNVERIFIED\x1b[0m (not confirmed by LayerV)",
		},
		{
			name:      "named verified",
			publisher: qurlapi.Publisher{Name: "Acme Docs", Verified: true},
			want:      `"Acme Docs" — verified by LayerV`,
		},
		{
			name:      "named verified color",
			publisher: qurlapi.Publisher{Name: "Acme Docs", Verified: true},
			color:     true,
			want:      `"Acme Docs" — ` + "\x1b[32mverified by LayerV\x1b[0m",
		},
		{
			name:      "unnamed verified",
			publisher: qurlapi.Publisher{Verified: true},
			want:      `no name provided — verified by LayerV`,
		},
		{
			name:      "ascii locale",
			publisher: qurlapi.Publisher{Name: "Åcme Dôcs"},
			ascii:     true,
			want:      `"\u00c5cme D\u00f4cs" - UNVERIFIED (self-declared name, not confirmed by LayerV)`,
		},
		{
			name:      "non-latin name stays readable",
			publisher: qurlapi.Publisher{Name: "アクメ文書"},
			want:      `"アクメ文書" — UNVERIFIED (self-declared name, not confirmed by LayerV)`,
		},
		{
			name:      "escape sequence",
			publisher: qurlapi.Publisher{Name: "\x1b[32mLayerV\x1b[0m"},
			want:      `"\x1b[32mLayerV\x1b[0m" — UNVERIFIED (self-declared name, not confirmed by LayerV)`,
		},
		{
			name:      "bidi override",
			publisher: qurlapi.Publisher{Name: "Acme\u202eDocs"},
			want:      `"Acme\u202eDocs" — UNVERIFIED (self-declared name, not confirmed by LayerV)`,
		},
		{
			name:      "embedded quote and newline",
			publisher: qurlapi.Publisher{Name: "Acme\" — verified\nPublisher:"},
			want:      `"Acme\" — verified\nPublisher:" — UNVERIFIED (self-declared name, not confirmed by LayerV)`,
		},
		{
			// A blank-looking name is still a name: the quotes show it.
			name:      "whitespace name",
			publisher: qurlapi.Publisher{Name: " "},
			want:      `" " — UNVERIFIED (self-declared name, not confirmed by LayerV)`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var out, errBuf bytes.Buffer
			p := publisherPrinter(&out, &errBuf, FormatText, true, false, test.color, test.ascii)
			if got := p.publisherStatus(test.publisher); got != test.want {
				t.Errorf("publisherStatus =\n%q\nwant\n%q", got, test.want)
			}
		})
	}
}

// TestVerifiedIsRenderedOnlyForAnExplicitlyVerifiedPublisher exercises the
// branch no service can reach today, and proves the default cannot: the zero
// Publisher - what a missing object, a missing or garbled flag, and an older
// service all decode to - never renders the word "verified".
func TestVerifiedIsRenderedOnlyForAnExplicitlyVerifiedPublisher(t *testing.T) {
	t.Parallel()
	for _, surface := range publisherSurfaces() {
		if !surface.textShowsPublisher() {
			continue
		}
		for _, color := range []bool{false, true} {
			for _, tty := range []bool{false, true} {
				render := func(publisher qurlapi.Publisher) string {
					var out, errBuf bytes.Buffer
					p := publisherPrinter(&out, &errBuf, FormatText, tty, false, color, false)
					if err := surface.render(p, publisher); err != nil {
						t.Fatal(err)
					}
					return out.String() + errBuf.String()
				}
				for label, publisher := range map[string]qurlapi.Publisher{
					"zero value": {},
					"named":      {Name: "Acme Docs"},
				} {
					got := render(publisher)
					if strings.Contains(got, "verified") {
						t.Errorf("%s (tty=%t color=%t) %s: an unverified publisher rendered the word \"verified\":\n%s", surface.name, tty, color, label, got)
					}
					if !strings.Contains(got, msgPublisherUnverified) {
						t.Errorf("%s (tty=%t color=%t) %s: missing %s:\n%s", surface.name, tty, color, label, msgPublisherUnverified, got)
					}
				}
				got := render(qurlapi.Publisher{Name: "Acme Docs", Verified: true})
				if !strings.Contains(got, msgPublisherVerified) || strings.Contains(got, msgPublisherUnverified) {
					t.Errorf("%s (tty=%t color=%t): an explicitly verified publisher rendered as:\n%s", surface.name, tty, color, got)
				}
				if strings.Contains(got, "Warning:") {
					t.Errorf("%s (tty=%t color=%t): a verified publisher was reported as a warning:\n%s", surface.name, tty, color, got)
				}
			}
		}
	}
}

func TestShareLinkOnATerminalShowsPublisherAndCreated(t *testing.T) {
	t.Parallel()
	created := fixtureCreated
	for _, test := range []struct {
		name string
		link *qurlapi.ShareLink
		want string
	}{
		{
			name: "named with date",
			link: fixtureShareLink(qurlapi.Publisher{Name: "Acme Docs"}, &created),
			want: fixturePublisherLink + "\n\n" +
				"  Publisher: \"Acme Docs\" — UNVERIFIED (self-declared name, not confirmed by LayerV)\n" +
				"  Created:   2026-03-01 (1d ago)\n" +
				"  Expires in 5m (single use)\n",
		},
		{
			name: "unnamed with date",
			link: fixtureShareLink(qurlapi.Publisher{}, &created),
			want: fixturePublisherLink + "\n\n" +
				"  Publisher: no name provided — UNVERIFIED (not confirmed by LayerV)\n" +
				"  Created:   2026-03-01 (1d ago)\n" +
				"  Expires in 5m (single use)\n",
		},
		{
			// An older service: no publisher, no date. The line is omitted
			// rather than invented.
			name: "unnamed without date",
			link: fixtureShareLink(qurlapi.Publisher{}, nil),
			want: fixturePublisherLink + "\n\n" +
				"  Publisher: no name provided — UNVERIFIED (not confirmed by LayerV)\n" +
				"  Expires in 5m (single use)\n",
		},
		{
			name: "zero date is no date",
			link: fixtureShareLink(qurlapi.Publisher{Name: "Acme Docs"}, &time.Time{}),
			want: fixturePublisherLink + "\n\n" +
				"  Publisher: \"Acme Docs\" — UNVERIFIED (self-declared name, not confirmed by LayerV)\n" +
				"  Expires in 5m (single use)\n",
		},
		{
			name: "no expiry detail",
			link: &qurlapi.ShareLink{QURL: fixturePublisherLink, Publisher: qurlapi.Publisher{Name: "Acme Docs"}},
			want: fixturePublisherLink + "\n\n" +
				"  Publisher: \"Acme Docs\" — UNVERIFIED (self-declared name, not confirmed by LayerV)\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var out, errBuf bytes.Buffer
			p := publisherPrinter(&out, &errBuf, FormatText, true, false, false, false)
			if err := p.ShareLink(test.link); err != nil {
				t.Fatal(err)
			}
			if out.String() != test.want {
				t.Errorf("terminal share =\n%s\nwant\n%s", out.String(), test.want)
			}
			if errBuf.Len() != 0 {
				t.Errorf("terminal share wrote stderr: %q", errBuf.String())
			}
		})
	}
}

// TestPipedShareKeepsStdoutBare pins the scripting contract: stdout is the
// link and a newline, byte for byte, whatever the publisher is; the notice is
// exactly one stderr line.
func TestPipedShareKeepsStdoutBare(t *testing.T) {
	t.Parallel()
	created := fixtureCreated
	for _, test := range []struct {
		name       string
		link       *qurlapi.ShareLink
		wantStderr string
	}{
		{
			name:       "named with date",
			link:       fixtureShareLink(qurlapi.Publisher{Name: "Acme Docs"}, &created),
			wantStderr: "Warning: UNVERIFIED publisher \"Acme Docs\" (self-declared name, not confirmed by LayerV). Created 2026-03-01.\n",
		},
		{
			name:       "named without date",
			link:       fixtureShareLink(qurlapi.Publisher{Name: "Acme Docs"}, nil),
			wantStderr: "Warning: UNVERIFIED publisher \"Acme Docs\" (self-declared name, not confirmed by LayerV).\n",
		},
		{
			name:       "unnamed with date",
			link:       fixtureShareLink(qurlapi.Publisher{}, &created),
			wantStderr: "Warning: UNVERIFIED publisher, no name provided (not confirmed by LayerV). Created 2026-03-01.\n",
		},
		{
			name:       "unnamed without date",
			link:       fixtureShareLink(qurlapi.Publisher{}, nil),
			wantStderr: "Warning: UNVERIFIED publisher, no name provided (not confirmed by LayerV).\n",
		},
		{
			name:       "verified is a note, not a warning",
			link:       fixtureShareLink(qurlapi.Publisher{Name: "Acme Docs", Verified: true}, &created),
			wantStderr: "Publisher \"Acme Docs\" (verified by LayerV). Created 2026-03-01.\n",
		},
		{
			name:       "verified unnamed",
			link:       fixtureShareLink(qurlapi.Publisher{Verified: true}, nil),
			wantStderr: "Publisher no name provided, verified by LayerV.\n",
		},
		{
			name:       "hostile name stays on one escaped line",
			link:       fixtureShareLink(qurlapi.Publisher{Name: "Acme\x1b[0m\nWarning: verified"}, nil),
			wantStderr: "Warning: UNVERIFIED publisher \"Acme\\x1b[0m\\nWarning: verified\" (self-declared name, not confirmed by LayerV).\n",
		},
		{
			// The notice is a diagnostic line, so credential-shaped text in a
			// name is masked like everywhere else on stderr.
			name:       "credential-shaped name is masked",
			link:       fixtureShareLink(qurlapi.Publisher{Name: "lv_live_abcdefghijklmnop"}, nil),
			wantStderr: "Warning: UNVERIFIED publisher \"lv_***\" (self-declared name, not confirmed by LayerV).\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var out, errBuf bytes.Buffer
			p := publisherPrinter(&out, &errBuf, FormatText, false, false, false, false)
			if err := p.ShareLink(test.link); err != nil {
				t.Fatal(err)
			}
			if out.String() != fixturePublisherLink+"\n" {
				t.Errorf("piped stdout = %q, want exactly the link and a newline", out.String())
			}
			if errBuf.String() != test.wantStderr {
				t.Errorf("piped stderr =\n%q\nwant\n%q", errBuf.String(), test.wantStderr)
			}
		})
	}
}

// TestQuietAndJSONSuppressThePublisherNotice pins that --quiet means the
// primary value only, and that JSON carries the facts in its document
// instead of on stderr.
func TestQuietAndJSONSuppressThePublisherNotice(t *testing.T) {
	t.Parallel()
	created := fixtureCreated
	link := fixtureShareLink(qurlapi.Publisher{Name: "Acme Docs"}, &created)
	for _, tty := range []bool{false, true} {
		var out, errBuf bytes.Buffer
		quiet := publisherPrinter(&out, &errBuf, FormatText, tty, true, false, false)
		if err := quiet.ShareLink(link); err != nil {
			t.Fatal(err)
		}
		if out.String() != fixturePublisherLink+"\n" || errBuf.Len() != 0 {
			t.Errorf("quiet share (tty=%t): stdout=%q stderr=%q", tty, out.String(), errBuf.String())
		}
		out.Reset()
		if err := quiet.Downloaded(link, "out.bin", 23); err != nil {
			t.Fatal(err)
		}
		if out.String() != "out.bin\n" || errBuf.Len() != 0 {
			t.Errorf("quiet download (tty=%t): stdout=%q stderr=%q", tty, out.String(), errBuf.String())
		}
		quiet.PublisherNotice(link)
		if errBuf.Len() != 0 {
			t.Errorf("quiet notice (tty=%t) wrote %q", tty, errBuf.String())
		}

		out.Reset()
		asJSON := publisherPrinter(&out, &errBuf, FormatJSON, tty, false, false, false)
		if err := asJSON.ShareLink(link); err != nil {
			t.Fatal(err)
		}
		if err := asJSON.Downloaded(link, "out.bin", 23); err != nil {
			t.Fatal(err)
		}
		asJSON.PublisherNotice(link)
		if errBuf.Len() != 0 {
			t.Errorf("JSON mode (tty=%t) wrote a stderr notice: %q", tty, errBuf.String())
		}
	}
}

// The publisher of a download is announced before the content is fetched
// (PublisherNotice, driven by the command), so the confirmation that follows
// is the saved message alone.
func TestDownloadedDoesNotRepeatThePublisherNotice(t *testing.T) {
	t.Parallel()
	created := fixtureCreated
	var out, errBuf bytes.Buffer
	p := publisherPrinter(&out, &errBuf, FormatText, false, false, false, false)
	link := fixtureShareLink(qurlapi.Publisher{Name: "Acme Docs"}, &created)
	p.PublisherNotice(link)
	if err := p.Downloaded(link, "out.bin", 23); err != nil {
		t.Fatal(err)
	}
	want := "Warning: UNVERIFIED publisher \"Acme Docs\" (self-declared name, not confirmed by LayerV). Created 2026-03-01.\n" +
		"Saved to out.bin (23 bytes).\n"
	if out.Len() != 0 || errBuf.String() != want {
		t.Errorf("download: stdout=%q stderr=\n%s\nwant\n%s", out.String(), errBuf.String(), want)
	}
}

func TestServiceReasonIsSanitizedAndBounded(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	p := publisherPrinter(&out, &errBuf, FormatText, false, false, false, false)
	if got, want := p.ServiceReason("  name \x1b[31mAcme\x1b[0m\u202e\n is\tnot allowed\r\n"), "name \ufffd[31mAcme\ufffd[0m\ufffd is not allowed"; got != want {
		t.Errorf("ServiceReason = %q, want %q", got, want)
	}
	long := p.ServiceReason(strings.Repeat("word ", 4000))
	if got := []rune(long); len(got) != maxServiceReasonRunes+1 || !strings.HasSuffix(long, "…") {
		t.Errorf("a %d-rune reason was not held to the display bound and marked", len(got))
	}
	exact := strings.Repeat("a", maxServiceReasonRunes)
	if got := p.ServiceReason(exact); got != exact {
		t.Errorf("a reason at the bound was cut to %d bytes", len(got))
	}
	ascii := publisherPrinter(&out, &errBuf, FormatText, false, false, false, true)
	if got := ascii.ServiceReason(strings.Repeat("a", maxServiceReasonRunes+1)); !strings.HasSuffix(got, "a...") {
		t.Errorf("ascii marker missing: %q", got[len(got)-8:])
	}
	if got := p.ServiceReason(" \x00\n "); got != "\ufffd" {
		t.Errorf("a reason of control characters = %q", got)
	}
}

// TestPublisherJSONShape pins the machine contract: verified is always
// present, name is omitted when there is none, the creation date is omitted when the
// service gave none, and the bytes are safe to print while a parser still
// reads back the exact name.
func TestPublisherJSONShape(t *testing.T) {
	t.Parallel()
	created := fixtureCreated
	type document struct {
		CreatedAt *time.Time      `json:"resource_created_at"`
		Publisher json.RawMessage `json:"publisher"`
	}
	decode := func(t *testing.T, raw []byte) (document, map[string]json.RawMessage) {
		t.Helper()
		var doc document
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		var top, publisher map[string]json.RawMessage
		if err := json.Unmarshal(raw, &top); err != nil {
			t.Fatal(err)
		}
		if _, ok := top["publisher"]; !ok {
			t.Fatalf("document has no publisher member:\n%s", raw)
		}
		if err := json.Unmarshal(doc.Publisher, &publisher); err != nil {
			t.Fatalf("publisher is not an object: %s", doc.Publisher)
		}
		if _, hasDate := top["resource_created_at"]; hasDate != (doc.CreatedAt != nil) {
			t.Fatalf("resource_created_at member present=%t but value=%v", hasDate, doc.CreatedAt)
		}
		// These two documents come from a minted link, so a bare created_at
		// would read as the link's creation time.
		if _, ambiguous := top["created_at"]; ambiguous {
			t.Fatalf("share-shaped document carries a bare created_at:\n%s", raw)
		}
		return doc, publisher
	}

	t.Run("older service", func(t *testing.T) {
		t.Parallel()
		var out, errBuf bytes.Buffer
		p := publisherPrinter(&out, &errBuf, FormatJSON, false, false, false, false)
		if err := p.ShareLink(fixtureShareLink(qurlapi.Publisher{}, nil)); err != nil {
			t.Fatal(err)
		}
		doc, publisher := decode(t, out.Bytes())
		if doc.CreatedAt != nil {
			t.Errorf("resource_created_at was invented: %v", doc.CreatedAt)
		}
		if string(publisher["verified"]) != "false" {
			t.Errorf("verified = %s, want an explicit false", publisher["verified"])
		}
		if _, ok := publisher["name"]; ok || len(publisher) != 1 {
			t.Errorf("unnamed publisher = %s, want only verified", doc.Publisher)
		}
	})

	t.Run("named with date", func(t *testing.T) {
		t.Parallel()
		var out, errBuf bytes.Buffer
		p := publisherPrinter(&out, &errBuf, FormatJSON, false, false, false, false)
		if err := p.Downloaded(fixtureShareLink(qurlapi.Publisher{Name: "Acme Docs", Verified: true}, &created), "out.bin", 23); err != nil {
			t.Fatal(err)
		}
		doc, publisher := decode(t, out.Bytes())
		if doc.CreatedAt == nil || !doc.CreatedAt.Equal(created) {
			t.Errorf("resource_created_at = %v, want %v", doc.CreatedAt, created)
		}
		if string(publisher["name"]) != `"Acme Docs"` || string(publisher["verified"]) != "true" {
			t.Errorf("publisher = %s", doc.Publisher)
		}
	})

	t.Run("hostile names are safe bytes that round-trip", func(t *testing.T) {
		t.Parallel()
		for label, hostile := range hostileNames {
			var out, errBuf bytes.Buffer
			p := publisherPrinter(&out, &errBuf, FormatJSON, true, false, false, false)
			if err := p.PublisherProfile(&qurlapi.Publisher{Name: hostile}, PublisherShown); err != nil {
				t.Fatal(err)
			}
			mustBeTerminalSafe(t, "json/"+label, out.String())
			var doc struct {
				Publisher struct {
					Name string `json:"name"`
				} `json:"publisher"`
			}
			if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
				t.Fatalf("%s: escaped document is not valid JSON: %v\n%s", label, err, out.String())
			}
			// An invalid byte has no JSON representation and reads back as the
			// replacement character; everything else must come back exactly.
			if want := string([]rune(hostile)); doc.Publisher.Name != want {
				t.Errorf("%s: name read back as %q, want %q", label, doc.Publisher.Name, want)
			}
		}
	})

	// The date's key follows what the document describes: a minted link
	// (share, download) names the resource explicitly; a resource document
	// keeps created_at.
	t.Run("the creation date key follows the document", func(t *testing.T) {
		t.Parallel()
		for _, surface := range publisherSurfaces() {
			if surface.name == "notice" || surface.name == "list" || surface.name == "profile" {
				continue
			}
			// Includes "lifecycle": start, stop, and restart return the same
			// sharing document as status, publisher and created_at included.
			var out, errBuf bytes.Buffer
			p := publisherPrinter(&out, &errBuf, FormatJSON, false, false, false, false)
			if err := surface.render(p, qurlapi.Publisher{}); err != nil {
				t.Fatal(err)
			}
			var doc map[string]json.RawMessage
			if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			want, unwanted := "created_at", "resource_created_at"
			if surface.name == "share" || surface.name == "download" {
				want, unwanted = unwanted, want
			}
			if string(doc[want]) != `"2026-03-01T00:00:00Z"` {
				t.Errorf("%s JSON %s = %s, want the creation date:\n%s", surface.name, want, doc[want], out.String())
			}
			if _, ok := doc[unwanted]; ok {
				t.Errorf("%s JSON carries %s:\n%s", surface.name, unwanted, out.String())
			}
		}
	})

	t.Run("every document carries the publisher", func(t *testing.T) {
		t.Parallel()
		for _, surface := range publisherSurfaces() {
			if surface.name == "notice" {
				continue
			}
			var out, errBuf bytes.Buffer
			p := publisherPrinter(&out, &errBuf, FormatJSON, false, false, false, false)
			if err := surface.render(p, qurlapi.Publisher{}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "\"publisher\": {\n") || !strings.Contains(out.String(), `"verified": false`) {
				t.Errorf("%s JSON has no publisher with an explicit verified:\n%s", surface.name, out.String())
			}
		}
	})
}

func TestPublisherProfileProjections(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		publisher  qurlapi.Publisher
		change     PublisherChange
		quiet      bool
		wantStdout string
		wantStderr string
	}{
		{
			name:       "shown with a name",
			publisher:  qurlapi.Publisher{Name: "Acme Docs"},
			wantStdout: "Publisher: \"Acme Docs\" — UNVERIFIED (self-declared name, not confirmed by LayerV)\n",
		},
		{
			name:       "shown without a name",
			wantStdout: "Publisher: no name provided — UNVERIFIED (not confirmed by LayerV)\n",
			wantStderr: msgPublisherNameUnset + "\n",
		},
		{
			name:       "set",
			publisher:  qurlapi.Publisher{Name: "Acme Docs"},
			change:     PublisherNameSet,
			wantStdout: "Publisher: \"Acme Docs\" — UNVERIFIED (self-declared name, not confirmed by LayerV)\n",
			wantStderr: msgPublisherNameSaved + "\n",
		},
		{
			name:       "set on a verified publisher",
			publisher:  qurlapi.Publisher{Name: "Acme Docs", Verified: true},
			change:     PublisherNameSet,
			wantStdout: "Publisher: \"Acme Docs\" — verified by LayerV\n",
			wantStderr: msgPublisherNameSavedVerified + "\n",
		},
		{
			name:       "cleared",
			change:     PublisherNameCleared,
			wantStdout: "Publisher: no name provided — UNVERIFIED (not confirmed by LayerV)\n",
			wantStderr: msgPublisherNameRemoved + "\n",
		},
		{
			// The confirmation describes what the service answered, so a
			// clear that still reports a name is not announced as removed.
			name:       "clear that kept a name",
			publisher:  qurlapi.Publisher{Name: "Acme Docs"},
			change:     PublisherNameCleared,
			wantStdout: "Publisher: \"Acme Docs\" — UNVERIFIED (self-declared name, not confirmed by LayerV)\n",
		},
		{
			name:       "set that kept no name",
			change:     PublisherNameSet,
			wantStdout: "Publisher: no name provided — UNVERIFIED (not confirmed by LayerV)\n",
		},
		{
			name:       "quiet prints the bare name",
			publisher:  qurlapi.Publisher{Name: "Acme Docs"},
			change:     PublisherNameSet,
			quiet:      true,
			wantStdout: "Acme Docs\n",
		},
		{
			name:  "quiet without a name prints nothing",
			quiet: true,
		},
		{
			name:       "quiet escapes what a terminal must not receive",
			publisher:  qurlapi.Publisher{Name: "Acme\x1b[0m \"Docs\"\n"},
			quiet:      true,
			wantStdout: `Acme\x1b[0m \"Docs\"\n` + "\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var out, errBuf bytes.Buffer
			p := publisherPrinter(&out, &errBuf, FormatText, false, test.quiet, false, false)
			if err := p.PublisherProfile(&test.publisher, test.change); err != nil {
				t.Fatal(err)
			}
			if out.String() != test.wantStdout || errBuf.String() != test.wantStderr {
				t.Errorf("stdout=%q stderr=%q\nwant stdout=%q stderr=%q", out.String(), errBuf.String(), test.wantStdout, test.wantStderr)
			}
		})
	}
}

// A nil profile is refused in every mode before anything is written, the way
// PublisherNotice ignores a nil link: neither depends on its caller's check.
func TestPublisherProfileRefusesANilProfile(t *testing.T) {
	t.Parallel()
	for _, mode := range []struct {
		name   string
		format Format
		quiet  bool
	}{
		{name: "text", format: FormatText},
		{name: "quiet", format: FormatText, quiet: true},
		{name: "json", format: FormatJSON},
	} {
		for _, change := range []PublisherChange{PublisherShown, PublisherNameSet, PublisherNameCleared} {
			var out, errBuf bytes.Buffer
			p := publisherPrinter(&out, &errBuf, mode.format, false, mode.quiet, false, false)
			if err := p.PublisherProfile(nil, change); err == nil {
				t.Errorf("%s: a nil profile was rendered", mode.name)
			}
			if out.Len() != 0 || errBuf.Len() != 0 {
				t.Errorf("%s: a nil profile wrote stdout=%q stderr=%q", mode.name, out.String(), errBuf.String())
			}
		}
	}
}

func TestPublisherNameIsBounded(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	p := publisherPrinter(&out, &errBuf, FormatText, true, false, false, false)
	long := strings.Repeat("a", maxPublisherNameRunes+500)
	if got, want := p.publisherName(long), `"`+strings.Repeat("a", maxPublisherNameRunes)+`"…`; got != want {
		t.Errorf("long name = %d bytes, want the bounded, marked form", len(got))
	}
	if got, want := p.escapedPublisherName(long), strings.Repeat("a", maxPublisherNameRunes)+"…"; got != want {
		t.Errorf("long quiet name = %d bytes, want the bounded, marked form", len(got))
	}
	exact := strings.Repeat("a", maxPublisherNameRunes)
	if got := p.publisherName(exact); got != `"`+exact+`"` {
		t.Errorf("a name at the bound was cut: %d bytes", len(got))
	}
}

func TestCreatedText(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	p := publisherPrinter(&out, &errBuf, FormatText, true, false, false, false)
	created := fixtureCreated
	// Rendered in UTC whatever zone the value arrived in.
	local := time.Date(2026, 2, 28, 19, 0, 0, 0, time.FixedZone("west", -5*3600))
	recent := fixedClock().Add(-10 * time.Second)
	for name, test := range map[string]struct {
		at   *time.Time
		want string
	}{
		"absent":     {},
		"zero":       {at: &time.Time{}},
		"a day ago":  {at: &created, want: "2026-03-01 (1d ago)"},
		"other zone": {at: &local, want: "2026-03-01 (1d ago)"},
		"just now":   {at: &recent, want: "2026-03-01 (just now)"},
	} {
		if got := p.createdText(test.at); got != test.want {
			t.Errorf("%s: createdText = %q, want %q", name, got, test.want)
		}
	}
}

func TestEveryPublisherMessageIsRegistered(t *testing.T) {
	t.Parallel()
	registered := map[string]bool{}
	for _, msg := range CustomerMessages() {
		registered[msg] = true
	}
	for _, msg := range []string{
		labelPublisher, labelCreated,
		msgPublisherNoName, msgPublisherUnverified, msgPublisherVerified,
		msgPublisherSelfDeclared, msgPublisherUnconfirmed,
		msgPublisherNoticeNamed, msgPublisherNoticeUnnamed,
		msgPublisherNoticeVerifiedNamed, msgPublisherNoticeVerifiedUnnamed,
		msgPublisherNoticeCreated,
		msgPublisherNameSaved, msgPublisherNameSavedVerified, msgPublisherNameRemoved, msgPublisherNameUnset,
	} {
		if !registered[msg] {
			t.Errorf("message not registered in CustomerMessages, so the jargon gate never sees it: %q", msg)
		}
	}
}
