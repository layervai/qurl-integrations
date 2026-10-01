package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-go/qurl"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

const (
	fixtureShareLink        = "https://qurl.link/#qv2t1.1.1.1.AQ.AQ.AQ"
	wantUnnamedNotice       = "Warning: UNVERIFIED publisher, no name provided (not confirmed by LayerV).\n"
	wantNamedNotice         = "Warning: UNVERIFIED publisher \"Acme Docs\" (self-declared name, not confirmed by LayerV). Created 2026-03-01.\n"
	wantUnnamedPublisherRow = "Publisher: no name provided — UNVERIFIED (not confirmed by LayerV)"
	// wordVerified is how a verified publisher would be rendered. Nothing in
	// these tests may ever produce it.
	wordVerified = "verified by LayerV"
)

// shareAnswer scripts one share response whose data object is the given raw
// JSON members, appended to a valid link for the mock's CRID.
func shareAnswer(srv *apitest.Server, extra string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w,
			`{"data":{"qurl":%q,"crid":%q,"type":"qv2","expires_in_seconds":300,"single_use":true%s},"meta":{"request_id":"req_test"}}`,
			fixtureShareLink, srv.Key.CRID, extra)
	}
}

// TestShareFromOlderServiceRendersUnverified is the rollout case: a service
// that predates publisher metadata answers with neither publisher nor
// resource_created_at. The share still succeeds, and the reader is told the publisher
// is unnamed and UNVERIFIED rather than nothing at all.
func TestShareFromOlderServiceRendersUnverified(t *testing.T) {
	t.Run("terminal", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.OmitPublisherMetadata()
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "--color", "never", "share", srv.Key.CRID}, tty: true})
		if res.code != 0 {
			t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
		}
		want := fixtureShareLink + "\n\n" +
			"  " + wantUnnamedPublisherRow + "\n" +
			"  Expires in 5m (single use)\n"
		if got := res.stdout.String(); got != want {
			t.Errorf("terminal share =\n%s\nwant\n%s", got, want)
		}
		if res.stderr.Len() != 0 {
			t.Errorf("terminal share wrote stderr: %q", res.stderr.String())
		}
	})

	t.Run("piped", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.OmitPublisherMetadata()
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "share", srv.Key.CRID}})
		if res.code != 0 {
			t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
		}
		if got := res.stdout.String(); got != fixtureShareLink+"\n" {
			t.Errorf("piped stdout = %q, want the bare link", got)
		}
		if got := res.stderr.String(); got != wantUnnamedNotice {
			t.Errorf("piped stderr = %q, want %q", got, wantUnnamedNotice)
		}
	})

	t.Run("json", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.OmitPublisherMetadata()
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "share", srv.Key.CRID, "-o", "json"}})
		if res.code != 0 || res.stderr.Len() != 0 {
			t.Fatalf("exit = %d, stderr: %q", res.code, res.stderr.String())
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(res.stdout.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"resource_created_at", "created_at"} {
			if _, ok := doc[key]; ok {
				t.Errorf("%s was invented:\n%s", key, res.stdout.String())
			}
		}
		if got := strings.Join(strings.Fields(string(doc["publisher"])), ""); got != `{"verified":false}` {
			t.Errorf("publisher = %s, want exactly an explicit unverified object", doc["publisher"])
		}
	})
}

// TestShareNeverRendersVerifiedFromAMisplacedOrGarbledField plants
// `"verified": true` everywhere a careless decoder might pick it up. The
// outcome is one of two, and never a verified publisher: the share succeeds
// and reads UNVERIFIED, or the answer is rejected and no link is printed.
func TestShareNeverRendersVerifiedFromAMisplacedOrGarbledField(t *testing.T) {
	for name, test := range map[string]struct {
		extra string
		// rejected means the answer is refused outright rather than shown.
		rejected bool
	}{
		"verified beside the publisher": {extra: `,"verified":true`},
		"publisher_verified":            {extra: `,"publisher_verified":true,"verified":true`},
		"null publisher":                {extra: `,"verified":true,"publisher":null`},
		"empty publisher":               {extra: `,"verified":true,"publisher":{}`},
		"null flag":                     {extra: `,"publisher":{"name":"Acme Docs","verified":null}`},
		"renamed flag":                  {extra: `,"publisher":{"name":"Acme Docs","is_verified":true}`},
		"nested flag":                   {extra: `,"publisher":{"name":"Acme Docs","status":{"verified":true}}`},
		"string flag":                   {extra: `,"publisher":{"name":"Acme Docs","verified":"true"}`, rejected: true},
		"numeric flag":                  {extra: `,"publisher":{"name":"Acme Docs","verified":1}`, rejected: true},
		"string publisher":              {extra: `,"publisher":"verified"`, rejected: true},
		"array publisher":               {extra: `,"publisher":[{"verified":true}]`, rejected: true},
	} {
		t.Run(name, func(t *testing.T) {
			for _, tty := range []bool{false, true} {
				srv := apitest.NewServer(t)
				srv.Script(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/share", shareAnswer(srv, test.extra))
				res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "share", srv.Key.CRID}, tty: tty})
				shown := res.stdout.String() + res.stderr.String()
				if strings.Contains(shown, wordVerified) {
					t.Fatalf("tty=%t: a misplaced or garbled field rendered a verified publisher:\n%s", tty, shown)
				}
				if test.rejected {
					if res.code == 0 || res.stdout.Len() != 0 {
						t.Fatalf("tty=%t: exit=%d stdout=%q; a garbled answer must be refused without printing a link", tty, res.code, res.stdout.String())
					}
					continue
				}
				if res.code != 0 {
					t.Fatalf("tty=%t: exit = %d, stderr: %s", tty, res.code, res.stderr.String())
				}
				if !strings.Contains(shown, "UNVERIFIED") {
					t.Fatalf("tty=%t: the publisher was not marked UNVERIFIED:\n%s", tty, shown)
				}
			}
		})
	}
}

// TestStatusNeverRendersVerifiedFromAGarbledField is the same guarantee on
// the owner read, which this repo decodes itself: a garbled publisher never
// fails the read and never verifies.
func TestStatusNeverRendersVerifiedFromAGarbledField(t *testing.T) {
	for name, publisher := range map[string]string{
		"absent":           ``,
		"beside":           `,"verified":true`,
		"null":             `,"publisher":null`,
		"string":           `,"publisher":"verified"`,
		"array":            `,"publisher":[{"verified":true}]`,
		"capitalized flag": `,"publisher":{"Verified":true}`,
		"uppercase flag":   `,"publisher":{"VERIFIED":true}`,
		"string flag":      `,"publisher":{"verified":"true"}`,
		"numeric flag":     `,"publisher":{"verified":1}`,
		"repeated flag":    `,"publisher":{"verified":false,"verified":true}`,
		"repeated object":  `,"publisher":{"verified":false},"publisher":{"verified":true}`,
		"repeated named":   `,"publisher":{"name":"Acme","verified":true},"publisher":{"name":"Acme","verified":true}`,
		"nested flag":      `,"publisher":{"status":{"verified":true}}`,
	} {
		t.Run(name, func(t *testing.T) {
			for _, format := range []string{"text", "json"} {
				srv := apitest.NewServer(t)
				srv.Script(http.MethodGet, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"data":{"resource":{"resource_id":%q,"crid":%q,"type":"url","status":"active"%s}}}`,
						srv.Key.ResourceID, srv.Key.CRID, publisher)
				})
				res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "status", srv.Key.CRID, "-o", format}})
				if res.code != 0 {
					t.Fatalf("%s: exit = %d; publisher metadata must never fail the read. stderr: %s", format, res.code, res.stderr.String())
				}
				got := res.stdout.String()
				if strings.Contains(got, wordVerified) || strings.Contains(got, `"verified": true`) {
					t.Fatalf("%s: a garbled field rendered a verified publisher:\n%s", format, got)
				}
				want := wantUnnamedPublisherRow[len("Publisher: "):]
				if format == "json" {
					want = `"verified": false`
				}
				if !strings.Contains(got, want) {
					t.Fatalf("%s: stdout missing %q:\n%s", format, want, got)
				}
			}
		})
	}
}

// TestOwnerReadsOmitAZeroCreationDate holds the text and JSON projections of
// the owner reads to one answer for an all-zeros created_at: it is an unset
// date, so text has no Created row and JSON has no created_at key. Without
// that, JSON printed year 1 while text printed nothing.
func TestOwnerReadsOmitAZeroCreationDate(t *testing.T) {
	const zero = `,"created_at":"0001-01-01T00:00:00Z"`
	script := func(srv *apitest.Server, createdAt string) {
		row := fmt.Sprintf(`{"resource_id":%q,"crid":%q,"type":"url","status":"active","target_url":"https://example.com/data","allowed_device_keys":[]%s}`,
			srv.Key.ResourceID, srv.Key.CRID, createdAt)
		answer := func(status int, body string) http.HandlerFunc {
			return func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(body))
			}
		}
		srv.Script(http.MethodGet, "/v1/resources", answer(http.StatusOK, `{"data":[`+row+`],"meta":{"has_more":false}}`))
		srv.Script(http.MethodGet, "/v1/resources/"+srv.Key.CRID, answer(http.StatusOK, `{"data":{"resource":`+row+`}}`))
		srv.Script(http.MethodPost, "/v1/resources", answer(http.StatusCreated, `{"data":`+row+`,"meta":{}}`))
	}
	// How each format shows the mock's real date, a day before the test clock.
	shown := map[string]string{"text": "1d ago", "json": `"created_at": "2026-03-01T00:00:00Z"`}
	for name, command := range map[string]func(*apitest.Server) []string{
		"status":  func(srv *apitest.Server) []string { return []string{"status", srv.Key.CRID} },
		"list":    func(*apitest.Server) []string { return []string{"list"} },
		"publish": func(*apitest.Server) []string { return []string{"publish", "https://example.com/data"} },
	} {
		t.Run(name, func(t *testing.T) {
			for _, format := range []string{"text", "json"} {
				srv := apitest.NewServer(t)
				script(srv, zero)
				res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL, "-o", format}, command(srv)...)})
				if res.code != 0 {
					t.Fatalf("%s: exit = %d, stderr: %s", format, res.code, res.stderr.String())
				}
				if got := res.stdout.String(); strings.Contains(got, "created_at") || strings.Contains(got, "Created:") ||
					strings.Contains(got, "0001") || strings.Contains(got, " ago") {
					t.Errorf("%s: a zero created_at was rendered as a date:\n%s", format, got)
				}

				// The same row with a real date does show it, so the check
				// above is not passing on a projection that never shows dates.
				srv = apitest.NewServer(t)
				script(srv, `,"created_at":"2026-03-01T00:00:00Z"`)
				res = runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL, "-o", format}, command(srv)...)})
				if got := res.stdout.String(); res.code != 0 || !strings.Contains(got, shown[format]) {
					t.Errorf("%s: exit = %d; a real created_at is missing (want %q):\n%s", format, res.code, shown[format], got)
				}
			}
		})
	}
}

// TestShareMetadataNeedsNoSecondRequest holds the footprint rule for every
// projection: the publisher and creation date come from the share answer
// itself. The share request is the only request about the resource — no
// resource read, no sharing-state read, no publisher-profile read — and it
// carries this device's credential.
func TestShareMetadataNeedsNoSecondRequest(t *testing.T) {
	for name, opts := range map[string]*runOpts{
		"piped":    {args: []string{"share"}},
		"terminal": {args: []string{"share"}, tty: true},
		"json":     {args: []string{"-o", "json", "share"}},
	} {
		t.Run(name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			opts.args = append([]string{"--endpoint", srv.URL}, append(opts.args, srv.Key.CRID)...)
			res := runCLI(t, opts)
			if res.code != 0 {
				t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
			}
			if shown := res.stdout.String() + res.stderr.String(); !strings.Contains(shown, apitest.DefaultPublisherName) {
				t.Fatalf("the publisher was not shown:\n%s", shown)
			}
			sharePath := "/v1/resources/" + srv.Key.CRID + "/share"
			shares := 0
			for _, request := range srv.Requests() {
				switch {
				case request.Method == http.MethodPost && request.Path == sharePath:
					shares++
					if request.Header.Get("Authorization") == "" {
						t.Fatalf("the share request carried no credential: %+v", request)
					}
				case strings.HasPrefix(request.Path, "/v1/resources"), request.Path == "/v1/me/publisher":
					t.Fatalf("publisher metadata must ride the share answer, not a second request: %s %s", request.Method, request.Path)
				}
			}
			if shares != 1 {
				t.Fatalf("share requests = %d, want exactly 1: %+v", shares, srv.Requests())
			}
		})
	}
}

func TestShareQuietPrintsOnlyTheLink(t *testing.T) {
	for _, tty := range []bool{false, true} {
		srv := apitest.NewServer(t)
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "--quiet", "share", srv.Key.CRID}, tty: tty})
		if res.code != 0 || res.stdout.String() != fixtureShareLink+"\n" || res.stderr.Len() != 0 {
			t.Errorf("tty=%t: exit=%d stdout=%q stderr=%q; want only the link", tty, res.code, res.stdout.String(), res.stderr.String())
		}
	}
}

// TestGetShowsThePublisherBeforeTheBrowserOpens pins the order a person
// relies on: who published the resource is on screen when the browser takes
// over, not after.
func TestGetShowsThePublisherBeforeTheBrowserOpens(t *testing.T) {
	srv := apitest.NewServer(t)
	browser := &fakeBrowser{}
	res := runCLI(t, &runOpts{
		args:    []string{"--endpoint", srv.URL, "--color", "never", "get", srv.Key.CRID},
		tty:     true,
		browser: browser,
	})
	if res.code != 0 || len(browser.stdoutAtOpen) != 1 {
		t.Fatalf("exit = %d, launches = %d, stderr: %s", res.code, len(browser.stdoutAtOpen), res.stderr.String())
	}
	want := fixtureShareLink + "\n\n" +
		"  Publisher: \"Acme Docs\" — UNVERIFIED (self-declared name, not confirmed by LayerV)\n" +
		"  Created:   2026-03-01 (1d ago)\n" +
		"  Expires in 5m (single use)\n"
	if got := browser.stdoutAtOpen[0]; got != want {
		t.Errorf("stdout when the browser opened =\n%s\nwant\n%s", got, want)
	}
}

func TestGetFileReportsThePublisher(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		t.Chdir(t.TempDir())
		srv := downloadServer(t)
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "get", srv.Key.CRID, "--file", "out.bin"}})
		if res.code != 0 || res.stdout.Len() != 0 {
			t.Fatalf("exit = %d, stdout = %q, stderr: %s", res.code, res.stdout.String(), res.stderr.String())
		}
		// The publisher is announced before the content is fetched; the saved
		// message follows and does not repeat it.
		if want := wantNamedNotice + "Saved to out.bin (23 bytes).\n"; res.stderr.String() != want {
			t.Errorf("stderr =\n%s\nwant\n%s", res.stderr.String(), want)
		}
	})

	// The notice is written when the verified answer arrives, so a download
	// that then fails has still told the reader whose content it was fetching.
	t.Run("announced before a failed fetch", func(t *testing.T) {
		t.Chdir(t.TempDir())
		srv := downloadServer(t)
		srv.ScriptRepeat(http.MethodGet, apitest.DownloadPath, 2, handlerGone)
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "get", srv.Key.CRID, "--file", "out.bin"}})
		if res.code != exitcode.NotFound || res.stdout.Len() != 0 {
			t.Fatalf("exit = %d, stdout = %q, stderr: %s", res.code, res.stdout.String(), res.stderr.String())
		}
		got := res.stderr.String()
		if !strings.HasPrefix(got, wantNamedNotice) || strings.Count(got, "UNVERIFIED") != 1 || strings.Contains(got, "Saved to") {
			t.Errorf("stderr = %q, want the notice once, first, and no saved message", got)
		}
		mustNotExistCmd(t, "out.bin")
	})

	t.Run("announced once across a refresh", func(t *testing.T) {
		t.Chdir(t.TempDir())
		srv := downloadServer(t)
		srv.Script(http.MethodGet, apitest.DownloadPath, handlerGone)
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "get", srv.Key.CRID, "--file", "out.bin"}})
		if res.code != 0 {
			t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
		}
		if want := wantNamedNotice + "Saved to out.bin (23 bytes).\n"; res.stderr.String() != want {
			t.Errorf("stderr =\n%s\nwant\n%s", res.stderr.String(), want)
		}
	})

	t.Run("quiet", func(t *testing.T) {
		t.Chdir(t.TempDir())
		srv := downloadServer(t)
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "--quiet", "get", srv.Key.CRID, "--file", "out.bin"}})
		if res.code != 0 || res.stdout.String() != "out.bin\n" || res.stderr.Len() != 0 {
			t.Errorf("exit=%d stdout=%q stderr=%q; want only the path", res.code, res.stdout.String(), res.stderr.String())
		}
	})

	t.Run("older service", func(t *testing.T) {
		t.Chdir(t.TempDir())
		srv := downloadServer(t)
		srv.OmitPublisherMetadata()
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "get", srv.Key.CRID, "--file", "out.bin"}})
		if res.code != 0 {
			t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
		}
		if want := wantUnnamedNotice + "Saved to out.bin (23 bytes).\n"; res.stderr.String() != want {
			t.Errorf("stderr =\n%s\nwant\n%s", res.stderr.String(), want)
		}
	})

	// A link refreshed after an expiry names the same resource, so streaming
	// to stdout announces the publisher once, not once per link.
	t.Run("stream announces once across a refresh", func(t *testing.T) {
		srv := downloadServer(t)
		srv.Script(http.MethodGet, apitest.DownloadPath, handlerGone)
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "get", srv.Key.CRID, "--file", "-"}})
		if res.code != 0 || res.stdout.String() != apitest.DefaultDownloadPayload {
			t.Fatalf("exit = %d, stdout = %q, stderr: %s", res.code, res.stdout.String(), res.stderr.String())
		}
		if res.stderr.String() != wantNamedNotice {
			t.Errorf("stderr = %q, want the notice exactly once", res.stderr.String())
		}
		shares := 0
		for _, request := range srv.Requests() {
			if request.Path == shareRoute(srv) {
				shares++
			}
		}
		if shares != 2 {
			t.Fatalf("share requests = %d, want the original and one refresh", shares)
		}
	})
}

func sharingAnswer(t *testing.T, srv *apitest.Server, extra map[string]any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		data := map[string]any{
			"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID,
			"desired_state": "on", "serving_epoch": 5, "connection_state": "serving",
		}
		for key, value := range extra {
			data[key] = value
		}
		apitest.WriteEnvelope(t, w, http.StatusOK, data, nil)
	}
}

// TestConnectorStatusShowsPublisherAndCreated covers the Connector views,
// whose sharing-state answer carries the metadata so no second request is
// made for it.
func TestConnectorStatusShowsPublisherAndCreated(t *testing.T) {
	metadata := map[string]any{
		"created_at": "2026-03-01T00:00:00Z",
		"publisher":  map[string]any{"name": "Acme Docs", "verified": false},
	}
	for _, command := range []string{"status", "inspect"} {
		t.Run(command+" text", func(t *testing.T) {
			srv := apitest.NewServer(t)
			path := "/v1/resources/" + srv.Key.CRID + "/sharing"
			srv.Script(http.MethodGet, path, sharingAnswer(t, srv, metadata))
			res := runCLI(t, &runOpts{
				args:             []string{"--endpoint", srv.URL, command, srv.Key.CRID},
				shareStateDirErr: connectorstate.ErrNoDefaultStateDir,
			})
			if res.code != 0 {
				t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
			}
			fields := map[string]string{}
			for _, line := range strings.Split(res.stdout.String(), "\n") {
				if label, value, ok := strings.Cut(line, ":"); ok {
					fields[label] = strings.TrimSpace(value)
				}
			}
			if got := fields["Publisher"]; got != `"Acme Docs" — UNVERIFIED (self-declared name, not confirmed by LayerV)` {
				t.Errorf("Publisher row = %q\n%s", got, res.stdout.String())
			}
			if got := fields["Created"]; got != "2026-03-01 (1d ago)" {
				t.Errorf("Created row = %q\n%s", got, res.stdout.String())
			}
			if requests := srv.Requests(); len(requests) != 1 || requests[0].Path != path {
				t.Errorf("%s made %d requests, want only the sharing-state read", command, len(requests))
			}
		})

		t.Run(command+" json", func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.Script(http.MethodGet, "/v1/resources/"+srv.Key.CRID+"/sharing", sharingAnswer(t, srv, metadata))
			res := runCLI(t, &runOpts{
				args:             []string{"--endpoint", srv.URL, "-o", "json", command, srv.Key.CRID},
				shareStateDirErr: connectorstate.ErrNoDefaultStateDir,
			})
			if res.code != 0 {
				t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
			}
			var doc struct {
				CreatedAt string `json:"created_at"`
				Publisher struct {
					Name     string `json:"name"`
					Verified *bool  `json:"verified"`
				} `json:"publisher"`
			}
			if err := json.Unmarshal(res.stdout.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			if doc.CreatedAt != "2026-03-01T00:00:00Z" || doc.Publisher.Name != "Acme Docs" ||
				doc.Publisher.Verified == nil || *doc.Publisher.Verified {
				t.Errorf("%s JSON = %s", command, res.stdout.String())
			}
		})
	}
}

// TestLocalPublishCarriesThePublisherInJSONOnly pins both halves of the
// publish contract: the JSON document reports the publisher taken from the
// sharing state, and the text document has no publisher row at all, so a
// first publish never opens with a status word about the publisher.
func TestLocalPublishCarriesThePublisherInJSONOnly(t *testing.T) {
	render := func(format output.Format, sharing *qurlapi.Sharing) string {
		var stdout, stderr bytes.Buffer
		opts := &globalOpts{
			streams:        &output.Streams{In: strings.NewReader(""), Out: &stdout, Err: &stderr},
			resolvedFormat: format,
			now:            func() time.Time { return fixedNow },
		}
		local := &connectorstate.LocalShare{CRID: "<CRID>", ResourceID: "rid", TargetURL: "http://127.0.0.1:3000"}
		if err := printLocalPublishServing(opts, &agent.ResolvedResource{}, local, sharing); err != nil {
			t.Fatal(err)
		}
		if stderr.Len() != 0 {
			t.Fatalf("local publish wrote stderr: %q", stderr.String())
		}
		return stdout.String()
	}
	publisherOf := func(sharing *qurlapi.Sharing) string {
		var doc map[string]json.RawMessage
		if err := json.Unmarshal([]byte(render(output.FormatJSON, sharing)), &doc); err != nil {
			t.Fatal(err)
		}
		return strings.Join(strings.Fields(string(doc["publisher"])), "")
	}
	named := &qurlapi.Sharing{Publisher: qurlapi.Publisher{Name: "Acme Docs"}}
	if got := publisherOf(named); got != `{"name":"AcmeDocs","verified":false}` {
		t.Errorf("local publish JSON publisher = %s", got)
	}
	// No sharing state, or one from an older service: unnamed and unverified.
	for _, sharing := range []*qurlapi.Sharing{nil, {}} {
		if got := publisherOf(sharing); got != `{"verified":false}` {
			t.Errorf("local publish JSON publisher without one = %s", got)
		}
	}
	text := render(output.FormatText, named)
	if want := "Published\n\n  Target:  http://127.0.0.1:3000\n  Status:  serving\n\nCRID: <CRID>\n"; text != want {
		t.Errorf("local publish text =\n%s\nwant exactly\n%s", text, want)
	}
}

func TestPublisherShowsTheProfile(t *testing.T) {
	srv := apitest.NewServer(t)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publisher"}})
	if res.code != 0 || res.stderr.Len() != 0 {
		t.Fatalf("exit = %d, stderr: %q", res.code, res.stderr.String())
	}
	if want := "Publisher: \"Acme Docs\" — UNVERIFIED (self-declared name, not confirmed by LayerV)\n"; res.stdout.String() != want {
		t.Errorf("stdout = %q, want %q", res.stdout.String(), want)
	}
	requests := srv.Requests()
	if len(requests) != 1 || requests[0].Method != http.MethodGet || requests[0].Path != "/v1/me/publisher" ||
		requests[0].Header.Get("Authorization") == "" {
		t.Fatalf("publisher requests = %+v, want one authenticated profile read", requests)
	}

	t.Run("no name", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.SetPublisherName("")
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publisher"}})
		if res.code != 0 {
			t.Fatalf("exit = %d, stderr: %q", res.code, res.stderr.String())
		}
		if want := wantUnnamedPublisherRow + "\n"; res.stdout.String() != want {
			t.Errorf("stdout = %q, want %q", res.stdout.String(), want)
		}
		if want := "No publisher name is set. Run `qurl publisher set <name>` to add one.\n"; res.stderr.String() != want {
			t.Errorf("stderr = %q, want %q", res.stderr.String(), want)
		}
	})

	t.Run("quiet", func(t *testing.T) {
		srv := apitest.NewServer(t)
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "-q", "publisher"}})
		if res.code != 0 || res.stdout.String() != "Acme Docs\n" || res.stderr.Len() != 0 {
			t.Errorf("exit=%d stdout=%q stderr=%q; want the bare name", res.code, res.stdout.String(), res.stderr.String())
		}
		srv.SetPublisherName("")
		res = runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "-q", "publisher"}})
		if res.code != 0 || res.stdout.Len() != 0 || res.stderr.Len() != 0 {
			t.Errorf("unnamed: exit=%d stdout=%q stderr=%q; want nothing", res.code, res.stdout.String(), res.stderr.String())
		}
	})

	// A profile whose answer is hostile reaches the terminal escaped, on one
	// line, still marked UNVERIFIED.
	t.Run("hostile name", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.SetPublisherName("LayerV\x1b[2K\r\u202e — verified by LayerV\nPublisher: ok")
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publisher"}})
		if res.code != 0 {
			t.Fatalf("exit = %d, stderr: %q", res.code, res.stderr.String())
		}
		got := res.stdout.String()
		if strings.ContainsAny(got, "\x1b\r\u202e") || strings.Count(got, "\n") != 1 {
			t.Errorf("a hostile name reached the terminal raw: %q", got)
		}
		if !strings.HasSuffix(got, " — UNVERIFIED (self-declared name, not confirmed by LayerV)\n") {
			t.Errorf("a hostile name displaced the status: %q", got)
		}
	})
}

func TestPublisherSetAndClear(t *testing.T) {
	bodies := func(srv *apitest.Server) *[]string {
		var seen []string
		record := func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			seen = append(seen, string(raw))
			var body struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(raw, &body)
			srv.SetPublisherName(body.Name)
			publisher := map[string]any{"verified": false}
			if body.Name != "" {
				publisher["name"] = body.Name
			}
			apitest.WriteEnvelope(t, w, http.StatusOK, publisher, nil)
		}
		srv.ScriptRepeat(http.MethodPatch, "/v1/me/publisher", 4, record)
		return &seen
	}

	t.Run("set", func(t *testing.T) {
		srv := apitest.NewServer(t)
		seen := bodies(srv)
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publisher", "set", "Northwind Labs"}})
		if res.code != 0 {
			t.Fatalf("exit = %d, stderr: %q", res.code, res.stderr.String())
		}
		if want := "Publisher: \"Northwind Labs\" — UNVERIFIED (self-declared name, not confirmed by LayerV)\n"; res.stdout.String() != want {
			t.Errorf("stdout = %q, want %q", res.stdout.String(), want)
		}
		if !strings.Contains(res.stderr.String(), "Publisher name saved.") || !strings.Contains(res.stderr.String(), "UNVERIFIED") {
			t.Errorf("stderr = %q, want the saved note naming the status", res.stderr.String())
		}
		// The request is exactly the name: there is no way to ask to be verified.
		if len(*seen) != 1 || (*seen)[0] != `{"name":"Northwind Labs"}` {
			t.Fatalf("request bodies = %q", *seen)
		}
	})

	t.Run("clear", func(t *testing.T) {
		srv := apitest.NewServer(t)
		seen := bodies(srv)
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publisher", "clear"}})
		if res.code != 0 {
			t.Fatalf("exit = %d, stderr: %q", res.code, res.stderr.String())
		}
		if want := wantUnnamedPublisherRow + "\n"; res.stdout.String() != want {
			t.Errorf("stdout = %q, want %q", res.stdout.String(), want)
		}
		if !strings.Contains(res.stderr.String(), "Publisher name removed.") {
			t.Errorf("stderr = %q, want the removed note", res.stderr.String())
		}
		if len(*seen) != 1 || (*seen)[0] != `{"name":""}` {
			t.Fatalf("request bodies = %q, want one explicit empty name", *seen)
		}
	})

	t.Run("quiet and json print no status note", func(t *testing.T) {
		srv := apitest.NewServer(t)
		quiet := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "-q", "publisher", "set", "Northwind Labs"}})
		if quiet.code != 0 || quiet.stdout.String() != "Northwind Labs\n" || quiet.stderr.Len() != 0 {
			t.Errorf("quiet set: exit=%d stdout=%q stderr=%q", quiet.code, quiet.stdout.String(), quiet.stderr.String())
		}
		asJSON := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "-o", "json", "publisher", "clear"}})
		if asJSON.code != 0 || asJSON.stderr.Len() != 0 ||
			strings.Join(strings.Fields(asJSON.stdout.String()), "") != `{"publisher":{"verified":false}}` {
			t.Errorf("json clear: exit=%d stdout=%q stderr=%q", asJSON.code, asJSON.stdout.String(), asJSON.stderr.String())
		}
		cleared := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "-q", "publisher", "clear"}})
		if cleared.code != 0 || cleared.stdout.Len() != 0 || cleared.stderr.Len() != 0 {
			t.Errorf("quiet clear: exit=%d stdout=%q stderr=%q; want nothing", cleared.code, cleared.stdout.String(), cleared.stderr.String())
		}
	})

	// What recipients see follows the profile: the next share carries the
	// new name.
	t.Run("the new name reaches a share", func(t *testing.T) {
		srv := apitest.NewServer(t)
		if res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publisher", "set", "Northwind Labs"}}); res.code != 0 {
			t.Fatalf("set: exit = %d, stderr: %q", res.code, res.stderr.String())
		}
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "share", srv.Key.CRID}})
		if want := "Warning: UNVERIFIED publisher \"Northwind Labs\" (self-declared name, not confirmed by LayerV). Created 2026-03-01.\n"; res.code != 0 || res.stderr.String() != want {
			t.Errorf("share after set: exit=%d stderr=%q, want %q", res.code, res.stderr.String(), want)
		}
	})
}

// TestPublisherSetRefusals pins exit 8 with the reason on stderr, and that
// neither the argument nor anything unprintable in the service's reason is
// echoed to the terminal.
func TestPublisherSetRefusals(t *testing.T) {
	t.Run("service refusal", func(t *testing.T) {
		srv := apitest.NewServer(t)
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publisher", "set", "\x1b[31mAcme\x1b[0m Verified\u202e"}})
		if res.code != exitcode.InvalidInput {
			t.Fatalf("exit = %d, want %d; stderr: %q", res.code, exitcode.InvalidInput, res.stderr.String())
		}
		mustEmptyStdout(t, res)
		if want := "Error: that publisher name can't be used: name must not contain the word verified\n"; res.stderr.String() != want {
			t.Errorf("stderr = %q, want %q", res.stderr.String(), want)
		}
	})

	t.Run("hostile reason", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.Script(http.MethodPatch, "/v1/me/publisher", func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteProblem(t, w, http.StatusBadRequest, "invalid_input", "Bad Request",
				"name \x1b[31mAcme\x1b[0m\u202e is not allowed\nError: everything is fine\r\x07")
		})
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publisher", "set", "Acme"}})
		if res.code != exitcode.InvalidInput {
			t.Fatalf("exit = %d, want %d", res.code, exitcode.InvalidInput)
		}
		got := res.stderr.String()
		if strings.ContainsAny(got, "\x1b\r\x07\u202e") || strings.Count(got, "\n") != 1 {
			t.Errorf("the service's reason reached the terminal raw, or forged a line: %q", got)
		}
		if !strings.HasPrefix(got, "Error: that publisher name can't be used: name ") || !strings.Contains(got, "is not allowed Error: everything is fine") {
			t.Errorf("stderr = %q, want the reason on the one error line", got)
		}
	})

	// A refused removal has the same outcome as a refused name: exit 8 with
	// the service's reason, not the generic problem anatomy. Its sentence is
	// its own, because clear takes no name for "that publisher name" to mean.
	t.Run("clear refused", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.Script(http.MethodPatch, "/v1/me/publisher", func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteProblem(t, w, http.StatusBadRequest, "invalid_input", "Bad Request", "name cannot be removed right now")
		})
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publisher", "clear"}})
		if res.code != exitcode.InvalidInput {
			t.Fatalf("exit = %d, want %d; stderr: %q", res.code, exitcode.InvalidInput, res.stderr.String())
		}
		mustEmptyStdout(t, res)
		if want := "Error: the publisher name can't be removed: name cannot be removed right now\n"; res.stderr.String() != want {
			t.Errorf("stderr = %q, want %q", res.stderr.String(), want)
		}
	})

	t.Run("clear refused, reason missing", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.Script(http.MethodPatch, "/v1/me/publisher", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_input"}}`))
		})
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publisher", "clear"}})
		if res.code != exitcode.InvalidInput {
			t.Fatalf("exit = %d, want %d; stderr: %q", res.code, exitcode.InvalidInput, res.stderr.String())
		}
		mustEmptyStdout(t, res)
		if want := "Error: the publisher name can't be removed. Run `qurl publisher` to see the name shown with your CRIDs\n"; res.stderr.String() != want {
			t.Errorf("stderr = %q, want %q", res.stderr.String(), want)
		}
	})

	t.Run("clear: other failures are not a refused removal", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.Script(http.MethodPatch, "/v1/me/publisher", apitest.HandlerAccountFrozen403(t))
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publisher", "clear"}})
		if res.code != exitcode.Forbidden || strings.Contains(res.stderr.String(), "publisher name can't be") {
			t.Fatalf("exit = %d, stderr = %q; want the account-standing error", res.code, res.stderr.String())
		}
	})

	// A runaway explanation is cut at the display bound and marked; it cannot
	// flood the terminal.
	t.Run("multi-kilobyte reason", func(t *testing.T) {
		for prefix, command := range map[string][]string{
			"Error: that publisher name can't be used: ":   {"publisher", "set", "Acme"},
			"Error: the publisher name can't be removed: ": {"publisher", "clear"},
		} {
			srv := apitest.NewServer(t)
			srv.Script(http.MethodPatch, "/v1/me/publisher", func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteProblem(t, w, http.StatusBadRequest, "invalid_input", "Bad Request", strings.Repeat("no ", 8000))
			})
			res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL}, command...)})
			if res.code != exitcode.InvalidInput {
				t.Fatalf("%q: exit = %d, want %d", command, res.code, exitcode.InvalidInput)
			}
			got := res.stderr.String()
			reason := strings.TrimSuffix(strings.TrimPrefix(got, prefix), "\n")
			if !strings.HasPrefix(got, prefix) || strings.Count(got, "\n") != 1 ||
				len([]rune(reason)) != 257 || !strings.HasSuffix(reason, "…") {
				t.Errorf("%q: a 24 KB reason rendered as %d bytes (%d runes of reason)", command, len(got), len([]rune(reason)))
			}
		}
	})

	t.Run("reason missing", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.Script(http.MethodPatch, "/v1/me/publisher", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_input","detail":"\u0007\u001b"}}`))
		})
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publisher", "set", "Acme"}})
		if res.code != exitcode.InvalidInput || !strings.HasPrefix(res.stderr.String(), "Error: that publisher name can't be used") ||
			strings.ContainsAny(res.stderr.String(), "\x07\x1b") {
			t.Errorf("exit=%d stderr=%q", res.code, res.stderr.String())
		}
	})

	t.Run("never valid", func(t *testing.T) {
		srv := apitest.NewServer(t)
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publisher", "set", "Acme\xff\xfe"}})
		if res.code != exitcode.InvalidInput {
			t.Fatalf("exit = %d, want %d; stderr: %q", res.code, exitcode.InvalidInput, res.stderr.String())
		}
		if want := "Error: that publisher name can't be used: name is not valid UTF-8\n"; res.stderr.String() != want {
			t.Errorf("stderr = %q, want %q", res.stderr.String(), want)
		}
		if requests := srv.Requests(); len(requests) != 0 {
			t.Errorf("a name that can never be valid was sent: %+v", requests)
		}
	})

	t.Run("empty name is a usage error", func(t *testing.T) {
		srv := apitest.NewServer(t)
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publisher", "set", ""}})
		if res.code != exitcode.Usage || !strings.Contains(res.stderr.String(), "qurl publisher clear") {
			t.Fatalf("exit = %d, stderr = %q; want a usage error naming clear", res.code, res.stderr.String())
		}
		if requests := srv.Requests(); len(requests) != 0 {
			t.Errorf("an empty name removed the publisher name: %+v", requests)
		}
	})

	t.Run("other failures are not a bad name", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.Script(http.MethodPatch, "/v1/me/publisher", apitest.HandlerAccountFrozen403(t))
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publisher", "set", "Acme"}})
		if res.code != exitcode.Forbidden || strings.Contains(res.stderr.String(), "publisher name can't be") {
			t.Fatalf("exit = %d, stderr = %q; want the account-standing error", res.code, res.stderr.String())
		}
	})

	t.Run("arity", func(t *testing.T) {
		for _, args := range [][]string{
			{"publisher", "set"},
			{"publisher", "set", "Acme", "Docs"},
			{"publisher", "clear", "now"},
			{"publisher", "rename"},
		} {
			res := runCLI(t, &runOpts{args: args})
			if res.code != exitcode.Usage {
				t.Errorf("%q: exit = %d, want %d; stderr: %q", args, res.code, exitcode.Usage, res.stderr.String())
			}
		}
	})
}

// TestPublisherAgainstAnOlderService is the rollout case for the new command:
// an endpoint without the profile route answers 404, and the reader is told
// that, not that a CRID may be mistyped.
func TestPublisherAgainstAnOlderService(t *testing.T) {
	for _, args := range [][]string{
		{"publisher"},
		{"publisher", "set", "Acme Docs"},
		{"publisher", "clear"},
	} {
		srv := apitest.NewServer(t)
		srv.OmitPublisherMetadata()
		res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL}, args...)})
		if res.code != exitcode.NotFound {
			t.Fatalf("%q: exit = %d, want %d; stderr: %q", args, res.code, exitcode.NotFound, res.stderr.String())
		}
		mustEmptyStdout(t, res)
		got := res.stderr.String()
		if !strings.HasPrefix(got, "Error: this qURL endpoint doesn't support publisher names yet.") || strings.Contains(got, "CRID may be mistyped") {
			t.Errorf("%q: stderr = %q", args, got)
		}
	}
}

func TestPublisherNameErrorPassesOtherErrorsThrough(t *testing.T) {
	other := fmt.Errorf("%w: unrelated", qurl.ErrInvalidAPIResponse)
	printer := output.New(discardStreams(), output.FormatText, false, false, false, nil)
	for name, render := range map[string]func(*output.Printer, error) error{
		"set": publisherNameError, "clear": publisherClearError,
	} {
		if got := render(printer, other); !errors.Is(got, qurl.ErrInvalidAPIResponse) || exitcode.FromError(got) != exitcode.ServerError ||
			got.Error() != other.Error() {
			t.Fatalf("%s: an unrelated error was rewritten: %v", name, got)
		}
	}
	if got := printPublisherProfile(&globalOpts{}, nil, output.PublisherShown); got == nil || exitcode.FromError(got) != exitcode.ServerError {
		t.Fatalf("an empty profile = %v, want an invalid API response", got)
	}
}

// TestPublisherHelpNeedsNoAccount holds the product rule on the new
// command's help: the CLI flow is install, publish, share. Nothing here may
// tell a customer to sign up, log in, or get a key.
func TestPublisherHelpNeedsNoAccount(t *testing.T) {
	for _, args := range [][]string{
		{"publisher", "--help"},
		{"publisher", "set", "--help"},
		{"publisher", "clear", "--help"},
	} {
		res := runCLI(t, &runOpts{args: args})
		if res.code != 0 {
			t.Fatalf("%q: exit = %d", args, res.code)
		}
		help := res.stdout.String()
		lower := strings.ToLower(help)
		for _, banned := range []string{"login", "log in", "sign in", "sign up", "signup", "api key", "account", "password"} {
			if strings.Contains(lower, banned) {
				t.Errorf("%q help mentions %q:\n%s", args, banned, help)
			}
		}
		if !strings.Contains(help, "UNVERIFIED") {
			t.Errorf("%q help does not say publishers are UNVERIFIED:\n%s", args, help)
		}
	}
	help := runCLI(t, &runOpts{args: []string{"publisher", "--help"}}).stdout.String()
	for _, want := range []string{"optional", "self-declared", "requests a link", "set", "clear"} {
		if !strings.Contains(help, want) {
			t.Errorf("publisher help is missing %q:\n%s", want, help)
		}
	}
}
