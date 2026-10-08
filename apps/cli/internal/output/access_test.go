package output

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
)

// Fixture values of the access-request renderings. No person or device has
// them. The printer's clock is two minutes after the request time.
const (
	accessCRID      = "qexamplecridforaccessrequests"
	accessOtherCRID = "qanothercridforaccessrequests"
	accessCode      = "482913"
	accessDevice    = "abcd-efgh-2345-mnop"
	accessOther     = "qrst-uvwx-yz67-abcd"
	accessAddress   = "https://links.example.test/" + accessCRID
)

func accessTime(minutesBeforeNow int) *time.Time {
	at := fixedClock().Add(-time.Duration(minutesBeforeNow) * time.Minute)
	return &at
}

func fixtureRequests(name string) *qurlapi.AccessRequestList {
	return &qurlapi.AccessRequestList{Requests: []qurlapi.AccessRequest{
		{Name: name, DeviceID: accessDevice, RequestedAt: accessTime(2), ExpiresAt: accessTime(-58), CRID: accessCRID},
		{Name: "", DeviceID: accessOther, RequestedAt: accessTime(90), CRID: accessOtherCRID},
	}}
}

// oneRequest is a listing with the first fixture request alone.
func oneRequest(more bool) *qurlapi.AccessRequestList {
	list := fixtureRequests("Ana Lopez")
	list.Requests = list.Requests[:1]
	list.HasMore = more
	return list
}

// noRequests is an empty listing.
func noRequests(more bool) *qurlapi.AccessRequestList {
	return &qurlapi.AccessRequestList{HasMore: more}
}

func fixtureGrants(name string) *qurlapi.ResourceSummary {
	private, requests := true, true
	return &qurlapi.ResourceSummary{
		CRID: accessCRID, ResourceID: "rid", Type: "url", Status: "active", TargetURL: "https://example.com/data",
		Private: &private, AccessRequests: &requests, AllowedDeviceKeys: []string{"a-public-key"},
		AllowedPasskeys: []qurlapi.AllowedPasskey{
			{DeviceID: accessDevice, Name: name, ApprovedAt: accessTime(60 * 12)},
			{DeviceID: accessOther},
		},
	}
}

// requesterSurface is one rendering that shows a name a requester typed.
type requesterSurface struct {
	name   string
	render func(*Printer, string) error
}

func requesterSurfaces() []requesterSurface {
	return []requesterSurface{
		{"requests of every resource", func(p *Printer, name string) error { return p.AccessRequests(fixtureRequests(name), true) }},
		{"requests of one resource", func(p *Printer, name string) error { return p.AccessRequests(fixtureRequests(name), false) }},
		{"approved", func(p *Printer, name string) error {
			return p.Approved(accessCRID, &qurlapi.AllowedPasskey{DeviceID: accessDevice, Name: name, ApprovedAt: accessTime(0)})
		}},
		{"grants", func(p *Printer, name string) error { return p.Grants(fixtureGrants(name)) }},
	}
}

// TestRequesterNameNeverReachesATerminalRaw runs every hostile name through
// every rendering that shows a requester's name, in every mode. No raw
// control, format or other non-printing character is written, and the name
// cannot add or remove a line, so it cannot forge a row or a code of its own.
func TestRequesterNameNeverReachesATerminalRaw(t *testing.T) {
	t.Parallel()
	type mode struct {
		name                string
		format              Format
		quiet, color, ascii bool
	}
	modes := []mode{
		{name: "text", format: FormatText},
		{name: "text color", format: FormatText, color: true},
		{name: "text ascii", format: FormatText, ascii: true},
		{name: "quiet", format: FormatText, quiet: true},
		{name: "json", format: FormatJSON},
	}
	render := func(surface requesterSurface, m mode, name string) (stdout, stderr string) {
		var out, errBuf bytes.Buffer
		p := newTestPrinter(&out, &errBuf, m.format, m.quiet, m.color, m.ascii)
		if err := surface.render(p, name); err != nil {
			t.Fatalf("%s/%s: %v", surface.name, m.name, err)
		}
		return out.String(), errBuf.String()
	}
	for _, surface := range requesterSurfaces() {
		for _, m := range modes {
			benignOut, benignErr := render(surface, m, "Ana Lopez")
			for label, hostile := range hostileNames {
				stdout, stderr := render(surface, m, hostile)
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

// TestRequesterNameIsAlwaysQuoted pins the form of a name in text: quoted,
// like a publisher name, on every line that shows it. A name that looks like
// a code, a device id or a command stays inside its quotes.
func TestRequesterNameIsAlwaysQuoted(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"Ana Lopez", "999 999", "the owner said approve me", "zzzz-yyyy-xxxx-wwww", "qurl approve x 111111"} {
		for _, surface := range requesterSurfaces() {
			var out, errBuf bytes.Buffer
			if err := surface.render(newTestPrinter(&out, &errBuf, FormatText, false, false, false), name); err != nil {
				t.Fatal(err)
			}
			shown := 0
			for _, line := range strings.Split(out.String(), "\n") {
				if !strings.Contains(line, name) {
					continue
				}
				shown++
				if !strings.Contains(line, `"`+name+`"`) {
					t.Errorf("%s: the name %q is not quoted: %q", surface.name, name, line)
				}
			}
			if shown != 1 {
				t.Errorf("%s: the name %q is shown on %d lines, want one:\n%s", surface.name, name, shown, out.String())
			}
		}
	}
}

// TestAccessRequestsListing pins the text table and its last line: the name,
// the device id, how long ago and until when for each request, the CRID when
// the listing covers every resource, and then the one plain line on how a
// person is let in. No column holds a code.
func TestAccessRequestsListing(t *testing.T) {
	t.Parallel()
	for _, all := range []bool{false, true} {
		var out, errBuf bytes.Buffer
		if err := newTestPrinter(&out, &errBuf, FormatText, false, false, false).AccessRequests(fixtureRequests("Ana Lopez"), all); err != nil {
			t.Fatal(err)
		}
		want := "NAME           DEVICE ID            REQUESTED  EXPIRES\n" +
			"\"Ana Lopez\"    abcd-efgh-2345-mnop  2m ago     in 58m\n" +
			"no name given  qrst-uvwx-yz67-abcd  1h ago     -\n"
		if all {
			want = "NAME           DEVICE ID            REQUESTED  EXPIRES  CRID\n" +
				"\"Ana Lopez\"    abcd-efgh-2345-mnop  2m ago     in 58m   " + accessCRID + "\n" +
				"no name given  qrst-uvwx-yz67-abcd  1h ago     -        " + accessOtherCRID + "\n"
		}
		want += "\nTo let one of these people in, ask them for the six-digit code on their screen and run `qurl approve <CRID> <code>`; a name can be typed by anyone, so the code is the only proof of who is asking.\n"
		if got := out.String(); got != want || errBuf.Len() != 0 {
			t.Fatalf("all=%t: listing =\n%s\nwant\n%s\nstderr %q", all, got, want, errBuf.String())
		}
	}

	// A request whose time has passed says so; the service may not have
	// taken it off the list yet.
	lapsed := oneRequest(false)
	lapsed.Requests[0].ExpiresAt = accessTime(1)
	var out, errBuf bytes.Buffer
	if err := newTestPrinter(&out, &errBuf, FormatText, false, false, false).AccessRequests(lapsed, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "2m ago     expired\n") {
		t.Fatalf("a lapsed request reads as:\n%s", out.String())
	}
}

// TestAccessRequestsListingAlwaysEndsWithTheSafetyLine pins that the line is
// the last line of every text listing that has a request, with and without
// color, and is written once.
func TestAccessRequestsListingAlwaysEndsWithTheSafetyLine(t *testing.T) {
	t.Parallel()
	for _, all := range []bool{false, true} {
		for _, color := range []bool{false, true} {
			for _, more := range []bool{false, true} {
				var out, errBuf bytes.Buffer
				if err := newTestPrinter(&out, &errBuf, FormatText, false, color, false).AccessRequests(oneRequest(more), all); err != nil {
					t.Fatal(err)
				}
				text := out.String()
				if !strings.HasSuffix(text, "\n\n"+msgApproveOnlyGivenCodes+"\n") || strings.Count(text, msgApproveOnlyGivenCodes) != 1 {
					t.Fatalf("all=%t color=%t more=%t: the listing does not end with the safety line, once:\n%s", all, color, more, text)
				}
			}
		}
	}
	for _, part := range []string{
		"ask them for the six-digit code on their screen", "`qurl approve <CRID> <code>`",
		"a name can be typed by anyone", "the code is the only proof of who is asking",
	} {
		if !strings.Contains(msgApproveOnlyGivenCodes, part) {
			t.Errorf("the safety line lost %q", part)
		}
	}
}

// TestAccessRequestsListingSaysWhenThereMayBeMore pins what a listing the
// service called incomplete says, and where: after the rows and before the
// last line in text, the has_more member in JSON, and stderr with --quiet,
// whose stdout stays values only. The listing of every resource says what to
// do, which is to list one resource. The listing of one resource does not
// send its reader to the command they just ran. A complete listing says
// nothing, and its JSON member is false.
func TestAccessRequestsListingSaysWhenThereMayBeMore(t *testing.T) {
	t.Parallel()
	const forAll = "There may be more requests than are shown here. To see all the requests for one resource, run `qurl requests <CRID>`."
	if msgRequestsMayBeMore != forAll {
		t.Fatalf("the line changed its words: %q", msgRequestsMayBeMore)
	}
	if strings.Contains(msgRequestsMayBeMoreForOne, "qurl requests <CRID>") || !strings.Contains(msgRequestsMayBeMoreForOne, "run this command again") {
		t.Fatalf("the line for one resource = %q", msgRequestsMayBeMoreForOne)
	}
	for all, line := range map[bool]string{true: msgRequestsMayBeMore, false: msgRequestsMayBeMoreForOne} {
		for _, more := range []bool{false, true} {
			where := fmt.Sprintf("all=%t more=%t", all, more)
			var out, errBuf bytes.Buffer
			if err := newTestPrinter(&out, &errBuf, FormatText, false, false, false).AccessRequests(oneRequest(more), all); err != nil {
				t.Fatal(err)
			}
			text := out.String()
			if got := strings.Contains(text, line); got != more || errBuf.Len() != 0 {
				t.Fatalf("%s: text =\n%s\nstderr %q", where, text, errBuf.String())
			}
			if more {
				rows, rule := strings.Index(text, accessDevice), strings.Index(text, msgApproveOnlyGivenCodes)
				if at := strings.Index(text, "\n\n"+line+"\n\n"); at < rows || at > rule {
					t.Fatalf("%s: the line is not between the rows and the last line:\n%s", where, text)
				}
			}

			out.Reset()
			if err := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false).AccessRequests(oneRequest(more), all); err != nil {
				t.Fatal(err)
			}
			var document struct {
				HasMore *bool `json:"has_more"`
			}
			if err := json.Unmarshal(out.Bytes(), &document); err != nil || document.HasMore == nil || *document.HasMore != more || errBuf.Len() != 0 {
				t.Fatalf("%s: JSON = %s (%v), stderr %q", where, out.String(), err, errBuf.String())
			}

			out.Reset()
			if err := newTestPrinter(&out, &errBuf, FormatText, true, false, false).AccessRequests(oneRequest(more), all); err != nil {
				t.Fatal(err)
			}
			wantErr := ""
			if more {
				wantErr = line + "\n"
			}
			if strings.Contains(out.String(), "There may be") || errBuf.String() != wantErr {
				t.Fatalf("%s: --quiet wrote stdout %q stderr %q, want the line on stderr only", where, out.String(), errBuf.String())
			}

			// An empty page of a listing that goes on is not "none".
			out.Reset()
			errBuf.Reset()
			if err := newTestPrinter(&out, &errBuf, FormatText, false, false, false).AccessRequests(noRequests(more), all); err != nil {
				t.Fatal(err)
			}
			if got := errBuf.String() == line+"\n"; got != more || out.Len() != 0 || (more && strings.Contains(errBuf.String(), "No pending")) {
				t.Fatalf("%s: an empty listing wrote stdout %q stderr %q", where, out.String(), errBuf.String())
			}
		}
	}
}

// TestEmptyAccessRequestsListing pins the empty listing: nothing on stdout in
// text, a note on stderr and no safety line, because there is nobody to let
// in. JSON has an empty array, and the rule all the same: the document has
// one shape, and the reader may list again when there is a request.
func TestEmptyAccessRequestsListing(t *testing.T) {
	t.Parallel()
	for all, note := range map[bool]string{true: msgNoPendingRequests, false: msgNoPendingRequestsForOne} {
		var out, errBuf bytes.Buffer
		if err := newTestPrinter(&out, &errBuf, FormatText, false, false, false).AccessRequests(noRequests(false), all); err != nil {
			t.Fatal(err)
		}
		if out.Len() != 0 || errBuf.String() != note+"\n" {
			t.Fatalf("all=%t: empty listing wrote stdout %q stderr %q", all, out.String(), errBuf.String())
		}
		out.Reset()
		errBuf.Reset()
		if err := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false).AccessRequests(noRequests(false), all); err != nil {
			t.Fatal(err)
		}
		// The documents of this CLI write an angle bracket as its JSON
		// escape, like every other; a reader of JSON gets the sentence.
		rule := strings.NewReplacer("<", `\u003c`, ">", `\u003e`).Replace(msgApproveOnlyGivenCodes)
		if want := "{\n  \"requests\": [],\n  \"approval_rule\": \"" + rule + "\",\n  \"has_more\": false\n}\n"; out.String() != want || errBuf.Len() != 0 {
			t.Fatalf("all=%t: empty JSON listing = %q, stderr %q, want %q", all, out.String(), errBuf.String(), want)
		}
	}
	var out, errBuf bytes.Buffer
	if err := newTestPrinter(&out, &errBuf, FormatText, false, false, false).AccessRequests(nil, true); err == nil || out.Len() != 0 {
		t.Fatalf("a listing that is not there was rendered: %q, %v", out.String(), err)
	}
}

// TestAccessRequestsJSONAndQuiet pins the two script-facing listings. JSON
// has the stable member names, no member for a code, and name_verified false
// on every row, with or without a name. --quiet prints what `qurl deny`
// takes: the device id, after the CRID when the listing covers every
// resource.
func TestAccessRequestsJSONAndQuiet(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	if err := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false).AccessRequests(fixtureRequests("Ana Lopez"), true); err != nil {
		t.Fatal(err)
	}
	var document struct {
		Requests []map[string]any `json:"requests"`
	}
	if err := json.Unmarshal(out.Bytes(), &document); err != nil || len(document.Requests) != 2 {
		t.Fatalf("JSON listing %q: %v", out.String(), err)
	}
	// The document has three members: the rows, the rule the text listing
	// ends with, in the same words, and whether there may be more. The rule
	// is said once, beside the rows, not on each of them.
	for _, all := range []bool{true, false} {
		var listing bytes.Buffer
		if err := newTestPrinter(&listing, &errBuf, FormatJSON, false, false, false).AccessRequests(fixtureRequests("Ana Lopez"), all); err != nil {
			t.Fatal(err)
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(listing.Bytes(), &top); err != nil {
			t.Fatal(err)
		}
		var rule string
		if err := json.Unmarshal(top["approval_rule"], &rule); err != nil || rule != msgApproveOnlyGivenCodes || len(top) != 3 || string(top["has_more"]) != "false" {
			t.Fatalf("all=%t: listing members = %v with approval_rule %q, want requests, has_more and the rule %q", all, top, rule, msgApproveOnlyGivenCodes)
		}
		if strings.Count(listing.String(), "a name can be typed by anyone") != 1 {
			t.Fatalf("all=%t: the rule is not said exactly once:\n%s", all, listing.String())
		}
	}
	first, second := document.Requests[0], document.Requests[1]
	want := map[string]any{
		"name": "Ana Lopez", "name_verified": false, "device_id": accessDevice,
		"requested_at": "2026-03-01T23:58:00Z", "expires_at": "2026-03-02T00:58:00Z", "crid": accessCRID,
	}
	if len(first) != len(want) {
		t.Fatalf("first request has members %v, want exactly %v", first, want)
	}
	for member, value := range want {
		if first[member] != value {
			t.Errorf("first request %s = %v, want %v", member, first[member], value)
		}
	}
	if _, named := second["name"]; named || second["name_verified"] != false || second["device_id"] != accessOther {
		t.Fatalf("a request with no name = %v, want no name member and name_verified false", second)
	}
	if _, has := second["expires_at"]; has {
		t.Fatalf("a request with no expiry has the member: %v", second)
	}
	for _, row := range document.Requests {
		for _, member := range []string{"code", "request_code"} {
			if _, has := row[member]; has {
				t.Fatalf("a listing row has the member %q: %v", member, row)
			}
		}
	}

	for all, wantQuiet := range map[bool]string{
		false: accessDevice + "\n" + accessOther + "\n",
		true:  accessCRID + " " + accessDevice + "\n" + accessOtherCRID + " " + accessOther + "\n",
	} {
		out.Reset()
		errBuf.Reset()
		if err := newTestPrinter(&out, &errBuf, FormatText, true, false, false).AccessRequests(fixtureRequests("Ana Lopez"), all); err != nil {
			t.Fatal(err)
		}
		if out.String() != wantQuiet || errBuf.Len() != 0 {
			t.Fatalf("all=%t: --quiet listing = %q, want %q", all, out.String(), wantQuiet)
		}
	}
}

// TestRequestGuidanceSaysWhatToSendAndWhatHappensNext pins the guidance a
// publisher gets when access requests are on, in both forms. With the address
// it shows that address. Without it, it shows the CRID and says that this
// install does not know the address, and it names no site.
func TestRequestGuidanceSaysWhatToSendAndWhatHappensNext(t *testing.T) {
	t.Parallel()
	on, private := true, true
	for _, address := range []string{accessAddress, ""} {
		var out, errBuf bytes.Buffer
		p := newTestPrinter(&out, &errBuf, FormatText, false, false, false)
		if err := p.AccessRequestsSetting(&qurlapi.ResourceSummary{CRID: accessCRID, AccessRequests: &on, Private: &private}, address); err != nil {
			t.Fatal(err)
		}
		want := "Access requests are on for " + accessCRID + ".\n\n" +
			"People can ask you for access to this resource. Send them this address:\n\n" +
			"  " + accessAddress + "\n\n" +
			"They ask for access there and get a six-digit code to give you. Approve a code with:\n\n" +
			"  qurl approve " + accessCRID + " <code>\n\n" +
			"The address and the CRID are safe to send to anyone: a private resource opens only for you and the people you allow.\n"
		if address == "" {
			want = "Access requests are on for " + accessCRID + ".\n\n" +
				"People can ask you for access to this resource. This install does not know the web address where a CRID is opened for its deployment, so send them the CRID itself:\n\n" +
				"  " + accessCRID + "\n\n" +
				"Where they open it, they ask for access and get a six-digit code to give you. Approve a code with:\n\n" +
				"  qurl approve " + accessCRID + " <code>\n\n" +
				"The CRID is safe to send to anyone: a private resource opens only for you and the people you allow.\n"
		}
		if got := out.String(); got != want || errBuf.Len() != 0 {
			t.Fatalf("address %q: guidance =\n%s\nwant\n%s", address, got, want)
		}
		if address == "" && strings.Contains(out.String(), "://") {
			t.Fatalf("guidance without a known site names an address:\n%s", out.String())
		}
	}
}

// TestGuidanceIsOnlyForAResourceTheServiceSaidIsPrivate pins the rule at the
// two places that print the guidance. It says that the resource's address
// is safe to send to anyone, because a private resource opens only for the
// people the publisher allows. That is true of a private resource and of no
// other, so it is printed only for a resource the service said is private.
//
// The setting change with access requests on, for a resource that is public
// or whose privacy was not said, is an error and writes nothing, in every
// output mode: no sentence, no address, no document. A publish result like
// that prints its document without the guidance and without an address.
// Turning requests off says nothing about privacy and needs none.
func TestGuidanceIsOnlyForAResourceTheServiceSaidIsPrivate(t *testing.T) {
	t.Parallel()
	on, off, public := true, false, false
	for name, private := range map[string]*bool{"public": &public, "privacy not said": nil} {
		for mode, printer := range map[string]func(out, errBuf *bytes.Buffer) *Printer{
			"text": func(out, errBuf *bytes.Buffer) *Printer {
				return newTestPrinter(out, errBuf, FormatText, false, false, false)
			},
			"json": func(out, errBuf *bytes.Buffer) *Printer {
				return newTestPrinter(out, errBuf, FormatJSON, false, false, false)
			},
			"quiet": func(out, errBuf *bytes.Buffer) *Printer {
				return newTestPrinter(out, errBuf, FormatText, true, false, false)
			},
		} {
			var out, errBuf bytes.Buffer
			err := printer(&out, &errBuf).AccessRequestsSetting(&qurlapi.ResourceSummary{CRID: accessCRID, AccessRequests: &on, Private: private}, accessAddress)
			if err == nil || out.Len() != 0 || errBuf.Len() != 0 {
				t.Errorf("%s, %s: the setting was rendered: %v, stdout %q, stderr %q", name, mode, err, out.String(), errBuf.String())
			}

			out.Reset()
			if err := printer(&out, &errBuf).AccessRequestsSetting(&qurlapi.ResourceSummary{CRID: accessCRID, AccessRequests: &off, Private: private}, accessAddress); err != nil {
				t.Errorf("%s, %s: turning requests off needs no privacy: %v", name, mode, err)
			}

			out.Reset()
			published := &qurlapi.Published{
				Private: private, AccessRequests: &on, LinkSiteURL: accessAddress,
				CRID: accessCRID, ResourceID: "rid", TargetURL: "https://example.com/data", Status: "active",
			}
			if err := printer(&out, &errBuf).Publish(published); err != nil {
				t.Fatalf("%s, %s: %v", name, mode, err)
			}
			for _, never := range []string{"safe to send", "People can ask you for access", accessAddress, "qurl approve"} {
				if strings.Contains(out.String(), never) {
					t.Errorf("%s, %s: a publish result that is not private shows %q:\n%s", name, mode, never, out.String())
				}
			}
		}
	}
}

// TestPublishWithAccessRequestsPrintsTheGuidanceBeforeTheCRID pins the
// publish document of a resource that accepts requests: the guidance is in
// it, the CRID is still last and alone on its line, JSON has the two members,
// and --quiet is the CRID alone. A resource that does not accept requests
// gets none of it, and never an address.
func TestPublishWithAccessRequestsPrintsTheGuidanceBeforeTheCRID(t *testing.T) {
	t.Parallel()
	private, on, off := true, true, false
	published := func(requests *bool) *qurlapi.Published {
		return &qurlapi.Published{
			Private: &private, AccessRequests: requests, LinkSiteURL: accessAddress,
			CRID: accessCRID, ResourceID: "rid", TargetURL: "https://example.com/data", Status: "active",
		}
	}
	render := func(res *qurlapi.Published, format Format, quiet bool) string {
		t.Helper()
		var out, errBuf bytes.Buffer
		if err := newTestPrinter(&out, &errBuf, format, quiet, false, false).Publish(res); err != nil {
			t.Fatal(err)
		}
		if errBuf.Len() != 0 {
			t.Fatalf("publish wrote to stderr: %q", errBuf.String())
		}
		return out.String()
	}

	text := render(published(&on), FormatText, false)
	want := "Published\n\n" +
		"  Target:  https://example.com/data\n" +
		"  Access:  private — only you and the people you allow can open it\n" +
		"  Status:  active\n\n" +
		"People can ask you for access to this resource. Send them this address:\n\n" +
		"  " + accessAddress + "\n\n" +
		"They ask for access there and get a six-digit code to give you. Approve a code with:\n\n" +
		"  qurl approve " + accessCRID + " <code>\n\n" +
		"The address and the CRID are safe to send to anyone: a private resource opens only for you and the people you allow.\n\n" +
		"CRID: " + accessCRID + "\n"
	if text != want {
		t.Fatalf("publish text =\n%s\nwant\n%s", text, want)
	}
	if document := render(published(&on), FormatJSON, false); !strings.Contains(document, `"access_requests": true`) || !strings.Contains(document, `"resource_url": "`+accessAddress+`"`) {
		t.Fatalf("publish JSON lacks the two members: %s", document)
	}
	if quiet := render(published(&on), FormatText, true); quiet != accessCRID+"\n" {
		t.Fatalf("--quiet = %q, want the CRID alone", quiet)
	}

	for name, requests := range map[string]*bool{"off": &off, "not said": nil} {
		text := render(published(requests), FormatText, false)
		document := render(published(requests), FormatJSON, false)
		if strings.Contains(text, "ask you for access") || strings.Contains(text, accessAddress) || strings.Contains(document, "resource_url") {
			t.Fatalf("%s: a resource that does not accept requests got the guidance or an address:\n%s\n%s", name, text, document)
		}
		if has := strings.Contains(document, `"access_requests"`); has != (requests != nil) {
			t.Fatalf("%s: JSON access_requests present = %t", name, has)
		}
	}
}

// TestAccessRequestsSettingOff pins what turning requests off says: nobody
// new can ask, and how many approved people still have access, counted from
// the service's answer.
func TestAccessRequestsSettingOff(t *testing.T) {
	t.Parallel()
	off := false
	// -1 stands for an answer that does not say who is approved. That is not
	// "nobody": it gets no count, and a line that still says where to look.
	for people, want := range map[int]string{
		-1: "Access requests are off for " + accessCRID + ". Nobody new can ask for access.\n" +
			"Anyone you approved earlier still has access. See them, or take access away, with `qurl grants " + accessCRID + "`.\n",
		0: "Access requests are off for " + accessCRID + ". Nobody new can ask for access.\n",
		1: "Access requests are off for " + accessCRID + ". Nobody new can ask for access.\n" +
			"1 approved person still has access. See them, or take access away, with `qurl grants " + accessCRID + "`.\n",
		3: "Access requests are off for " + accessCRID + ". Nobody new can ask for access.\n" +
			"3 approved people still have access. See them, or take access away, with `qurl grants " + accessCRID + "`.\n",
	} {
		resource := &qurlapi.ResourceSummary{CRID: accessCRID, AccessRequests: &off}
		if people >= 0 {
			resource.AllowedPasskeys = make([]qurlapi.AllowedPasskey, people)
		}
		var out, errBuf bytes.Buffer
		if err := newTestPrinter(&out, &errBuf, FormatText, false, false, false).AccessRequestsSetting(resource, accessAddress); err != nil {
			t.Fatal(err)
		}
		if out.String() != want || errBuf.Len() != 0 {
			t.Fatalf("%d people: off =\n%s\nwant\n%s", people, out.String(), want)
		}
		out.Reset()
		if err := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false).AccessRequestsSetting(resource, accessAddress); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(strings.Fields(out.String()), ""); got != `{"crid":"`+accessCRID+`","access_requests":false}` {
			t.Fatalf("off JSON = %q; it must not carry an address", out.String())
		}
	}
	on, private := true, true
	var out, errBuf bytes.Buffer
	if err := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false).AccessRequestsSetting(&qurlapi.ResourceSummary{CRID: accessCRID, AccessRequests: &on, Private: &private}, accessAddress); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(strings.Fields(out.String()), ""); got != `{"crid":"`+accessCRID+`","access_requests":true,"resource_url":"`+accessAddress+`"}` {
		t.Fatalf("on JSON = %q", out.String())
	}
	out.Reset()
	if err := newTestPrinter(&out, &errBuf, FormatText, true, false, false).AccessRequestsSetting(&qurlapi.ResourceSummary{CRID: accessCRID, AccessRequests: &on, Private: &private}, accessAddress); err != nil || out.String() != accessCRID+"\n" {
		t.Fatalf("--quiet = %q, %v", out.String(), err)
	}
	if err := newTestPrinter(&out, &errBuf, FormatText, false, false, false).AccessRequestsSetting(&qurlapi.ResourceSummary{CRID: accessCRID}, ""); err == nil {
		t.Fatal("a resource whose setting the service did not say was rendered as a setting")
	}
}

// TestApprovedSaysWhoHasAccessAndHowToTakeItAway pins the approval document:
// the name, marked as unchecked, the device id, and the exact command that
// removes this person, with this CRID and this device id in it.
func TestApprovedSaysWhoHasAccessAndHowToTakeItAway(t *testing.T) {
	t.Parallel()
	person := &qurlapi.AllowedPasskey{DeviceID: accessDevice, Name: "Ana Lopez", ApprovedAt: accessTime(0)}
	var out, errBuf bytes.Buffer
	if err := newTestPrinter(&out, &errBuf, FormatText, false, false, false).Approved(accessCRID, person); err != nil {
		t.Fatal(err)
	}
	want := "Approved\n\n" +
		"  Name:       \"Ana Lopez\" (typed by them, not checked)\n" +
		"  Device ID:  " + accessDevice + "\n" +
		"  Approved:   2026-03-02 (just now)\n\n" +
		"This person can now open " + accessCRID + ". To take the access away, run:\n\n" +
		"  qurl grants " + accessCRID + " --remove " + accessDevice + "\n"
	if out.String() != want || errBuf.Len() != 0 {
		t.Fatalf("approved =\n%s\nwant\n%s", out.String(), want)
	}

	out.Reset()
	if err := newTestPrinter(&out, &errBuf, FormatText, false, false, false).Approved(accessCRID, &qurlapi.AllowedPasskey{DeviceID: accessDevice}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "  Name:       no name given\n") || strings.Contains(out.String(), "Approved:") {
		t.Fatalf("an approval with no name and no date =\n%s", out.String())
	}

	out.Reset()
	if err := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false).Approved(accessCRID, person); err != nil {
		t.Fatal(err)
	}
	wantJSON := "{\n" +
		"  \"crid\": \"" + accessCRID + "\",\n" +
		"  \"approved\": true,\n" +
		"  \"name\": \"Ana Lopez\",\n" +
		"  \"name_verified\": false,\n" +
		"  \"device_id\": \"" + accessDevice + "\",\n" +
		"  \"approved_at\": \"2026-03-02T00:00:00Z\",\n" +
		"  \"name_note\": \"The name was typed by the person who asked. Nobody checked it.\"\n" +
		"}\n"
	if out.String() != wantJSON {
		t.Fatalf("approved JSON = %q, want %q", out.String(), wantJSON)
	}
	// The note is there for a person who gave no name too: the document has
	// one shape, and name_verified is false either way.
	out.Reset()
	if err := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false).Approved(accessCRID, &qurlapi.AllowedPasskey{DeviceID: accessDevice}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "\"name_note\": \""+msgRequesterNameNote+"\"") || strings.Contains(out.String(), "\"name\":") {
		t.Fatalf("approved JSON for a person with no name = %s", out.String())
	}
	out.Reset()
	if err := newTestPrinter(&out, &errBuf, FormatText, true, false, false).Approved(accessCRID, person); err != nil || out.String() != accessDevice+"\n" {
		t.Fatalf("--quiet = %q, %v; want the device id", out.String(), err)
	}
	if err := newTestPrinter(&out, &errBuf, FormatText, false, false, false).Approved(accessCRID, nil); err == nil {
		t.Fatal("an approval of nobody was rendered")
	}
}

// TestDeniedSaysThatNoAccessWasGiven pins the denial in its three forms, for
// each way a request is named. A denial by device id shows the device id and
// no code: it never had one. A denial by code shows the code its publisher
// typed.
func TestDeniedSaysThatNoAccessWasGiven(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		request, text, document string
	}{
		{
			request:  accessDevice,
			text:     "Denied the request from the device id " + accessDevice + " for " + accessCRID + ". No access was given.\n",
			document: `{"crid":"` + accessCRID + `","device_id":"` + accessDevice + `","denied":true}`,
		},
		{
			request:  accessCode,
			text:     "Denied the request with the code 482 913 for " + accessCRID + ". No access was given.\n",
			document: `{"crid":"` + accessCRID + `","code":"` + accessCode + `","denied":true}`,
		},
	} {
		var out, errBuf bytes.Buffer
		if err := newTestPrinter(&out, &errBuf, FormatText, false, false, false).Denied(accessCRID, test.request); err != nil {
			t.Fatal(err)
		}
		if out.Len() != 0 || errBuf.String() != test.text {
			t.Fatalf("denied wrote stdout %q stderr %q, want stderr %q", out.String(), errBuf.String(), test.text)
		}
		errBuf.Reset()
		if err := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false).Denied(accessCRID, test.request); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(strings.Fields(out.String()), ""); got != test.document || errBuf.Len() != 0 {
			t.Fatalf("denied JSON = %q, want %q", out.String(), test.document)
		}
		out.Reset()
		if err := newTestPrinter(&out, &errBuf, FormatText, true, false, false).Denied(accessCRID, test.request); err != nil || out.String() != test.request+"\n" {
			t.Fatalf("--quiet = %q, %v", out.String(), err)
		}
	}
}

// TestGrantsListsApprovedPeopleBesideTheDeviceKeys pins the grants document:
// whether people can ask, the device keys, and each approved person with the
// name they typed, their device id and when they were approved, then how to
// take one person's access away.
func TestGrantsListsApprovedPeopleBesideTheDeviceKeys(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	if err := newTestPrinter(&out, &errBuf, FormatText, false, false, false).Grants(fixtureGrants("Ana Lopez")); err != nil {
		t.Fatal(err)
	}
	want := "CRID:                 " + accessCRID + "\n" +
		"Target:               https://example.com/data\n" +
		"Type:                 url\n" +
		"Status:               active\n" +
		"Private:              true\n" +
		"Access requests:      on\n" +
		"Allowed device keys:  [a-public-key]\n" +
		"Approved people:      2\n" +
		"Publisher:            no name provided — UNVERIFIED (not confirmed by LayerV)\n\n" +
		"Approved people:\n" +
		"  NAME           DEVICE ID            APPROVED\n" +
		"  \"Ana Lopez\"    abcd-efgh-2345-mnop  2026-03-01 (12h ago)\n" +
		"  no name given  qrst-uvwx-yz67-abcd  -\n\n" +
		"Take one person's access away with `qurl grants " + accessCRID + " --remove <device id>`.\n"
	if out.String() != want || errBuf.Len() != 0 {
		t.Fatalf("grants =\n%s\nwant\n%s", out.String(), want)
	}

	out.Reset()
	if err := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false).Grants(fixtureGrants("Ana Lopez")); err != nil {
		t.Fatal(err)
	}
	var document struct {
		AllowedDeviceKeys []string         `json:"allowed_device_keys"`
		ApprovedPeople    []map[string]any `json:"approved_people"`
		AccessRequests    *bool            `json:"access_requests"`
	}
	if err := json.Unmarshal(out.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.AllowedDeviceKeys) != 1 || document.AccessRequests == nil || !*document.AccessRequests || len(document.ApprovedPeople) != 2 {
		t.Fatalf("grants JSON = %s", out.String())
	}
	first := document.ApprovedPeople[0]
	if first["name"] != "Ana Lopez" || first["name_verified"] != false || first["device_id"] != accessDevice || first["approved_at"] != "2026-03-01T12:00:00Z" || len(first) != 4 {
		t.Fatalf("first approved person = %v", first)
	}
	if second := document.ApprovedPeople[1]; len(second) != 2 || second["name_verified"] != false || second["device_id"] != accessOther {
		t.Fatalf("an approved person with no name and no date = %v", second)
	}

	// Nobody approved and a service that does not say whether people can ask:
	// one row that says "none", an empty array, and no setting.
	plain := &qurlapi.ResourceSummary{CRID: accessCRID, ResourceID: "rid", Type: "url", Status: "active"}
	out.Reset()
	if err := newTestPrinter(&out, &errBuf, FormatText, false, false, false).Grants(plain); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Approved people:      none\n") || strings.Contains(out.String(), "Access requests:") || strings.Contains(out.String(), "--remove") {
		t.Fatalf("grants with nobody approved =\n%s", out.String())
	}
	out.Reset()
	if err := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false).Grants(plain); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"approved_people": []`) || !strings.Contains(out.String(), `"allowed_device_keys": []`) || strings.Contains(out.String(), "access_requests") {
		t.Fatalf("grants JSON with nobody approved = %s", out.String())
	}
}

// TestStatusAndInspectDocumentsKeepTheirKeys pins that the two rows and the
// two members `qurl grants` gained are not in the resource status document
// that status and inspect print for the same resource.
func TestStatusAndInspectDocumentsKeepTheirKeys(t *testing.T) {
	t.Parallel()
	for _, format := range []Format{FormatText, FormatJSON} {
		var out, errBuf bytes.Buffer
		if err := newTestPrinter(&out, &errBuf, format, false, false, false).ResourceStatus(fixtureGrants("Ana Lopez")); err != nil {
			t.Fatal(err)
		}
		for _, added := range []string{"Access requests", "Approved people", "access_requests", "approved_people", "Ana Lopez", accessDevice} {
			if strings.Contains(out.String(), added) {
				t.Errorf("the status document (%s) gained %q:\n%s", format, added, out.String())
			}
		}
	}
}

// TestRemovalOutcomeDocumentAndMessage pins the two forms of a removal of
// approved people that did not remove everyone it named. The message says
// which device ids were removed, which was not found and which still have
// access, then the command that shows who has access now. In JSON mode the
// same outcome is a document with three arrays that are always there, so a
// script can tell which people lost access. Text and --quiet write no
// document: the message is the outcome.
func TestRemovalOutcomeDocumentAndMessage(t *testing.T) {
	t.Parallel()
	const first, second, third = "aaaa-aaaa-aaaa-aaaa", "bbbb-bbbb-bbbb-bbbb", "cccc-cccc-cccc-cccc"
	for _, test := range []struct {
		name     string
		outcome  *qurlapi.PasskeyRemovalError
		message  string
		document string
	}{
		{
			name:    "nothing removed",
			outcome: &qurlapi.PasskeyRemovalError{ID: accessCRID, NotFound: []string{second}, NotRemoved: []string{first}},
			message: "Error: no approved person has the device id " + second + " on this resource, so nothing was removed. " + first + " still has access.\n\n" +
				"  Run `qurl grants " + accessCRID + "` to see who has access now.\n",
			document: `{"crid":"` + accessCRID + `","removed":[],"not_found":["` + second + `"],"not_removed":["` + first + `"]}`,
		},
		{
			name:    "removed, then one not found",
			outcome: &qurlapi.PasskeyRemovalError{ID: accessCRID, Removed: []string{first}, NotFound: []string{second}, NotRemoved: []string{third}},
			message: "Error: access was taken away from " + first + ". Then no approved person had the device id " + second + " on this resource, and the command stopped. " + third + " still has access.\n\n" +
				"  Run `qurl grants " + accessCRID + "` to see who has access now.\n",
			document: `{"crid":"` + accessCRID + `","removed":["` + first + `"],"not_found":["` + second + `"],"not_removed":["` + third + `"]}`,
		},
		{
			// Every person was removed, and then the change to the public
			// keys failed. Who lost access, why the key change failed, and
			// the command that makes the key change alone.
			name: "all removed, then the key change failed",
			outcome: &qurlapi.PasskeyRemovalError{
				ID: accessCRID, Removed: []string{first, second},
				KeyChange: errors.New("the service is not reachable"), KeyChangeCommand: "qurl grants " + accessCRID + " --add a-public-key",
			},
			message: "Error: access was taken away from " + first + " and " + second + ". Then the change to the public keys failed.\n\n" +
				"  the service is not reachable\n\n" +
				"  Run `qurl grants " + accessCRID + "` to see who has access now.\n" +
				"  To make the change to the public keys, run: qurl grants " + accessCRID + " --add a-public-key\n",
			document: `{"crid":"` + accessCRID + `","removed":["` + first + `","` + second + `"],"not_found":[],"not_removed":[],"public_keys_command":"qurl` + "grants" + accessCRID + "--add" + `a-public-key"}`,
		},
		{
			name:    "the last one not found",
			outcome: &qurlapi.PasskeyRemovalError{ID: accessCRID, Removed: []string{first, second}, NotFound: []string{third}},
			message: "Error: access was taken away from " + first + " and " + second + ". Then no approved person had the device id " + third + " on this resource, and the command stopped.\n\n" +
				"  Run `qurl grants " + accessCRID + "` to see who has access now.\n",
			document: `{"crid":"` + accessCRID + `","removed":["` + first + `","` + second + `"],"not_found":["` + third + `"],"not_removed":[]}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var rendered bytes.Buffer
			RenderError(&rendered, fmt.Errorf("grants: %w", test.outcome), false)
			if rendered.String() != test.message {
				t.Fatalf("message =\n%s\nwant\n%s", rendered.String(), test.message)
			}
			var out, errBuf bytes.Buffer
			if err := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false).RemovalOutcome(test.outcome); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(strings.Fields(out.String()), ""); got != test.document || errBuf.Len() != 0 {
				t.Fatalf("document = %s, want %s", got, test.document)
			}
			for _, quiet := range []bool{false, true} {
				out.Reset()
				if err := newTestPrinter(&out, &errBuf, FormatText, quiet, false, false).RemovalOutcome(test.outcome); err != nil || out.Len() != 0 || errBuf.Len() != 0 {
					t.Fatalf("quiet %t: text mode wrote %q / %q, %v; want nothing", quiet, out.String(), errBuf.String(), err)
				}
			}
		})
	}
	var out, errBuf bytes.Buffer
	if err := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false).RemovalOutcome(nil); err != nil || out.Len() != 0 {
		t.Fatalf("no outcome wrote %q, %v", out.String(), err)
	}
}
