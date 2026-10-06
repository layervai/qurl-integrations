package output

import (
	"bytes"
	"encoding/json"
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

func fixtureRequests(name string) []qurlapi.AccessRequest {
	return []qurlapi.AccessRequest{
		{Code: accessCode, Name: name, DeviceID: accessDevice, RequestedAt: accessTime(2), ExpiresAt: accessTime(-58), CRID: accessCRID},
		{Code: "175306", Name: "", DeviceID: accessOther, RequestedAt: accessTime(90), CRID: accessOtherCRID},
	}
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

// TestAccessRequestsListing pins the text table and its last line: the code,
// the name, the device id and how long ago for each request, the CRID when
// the listing covers every resource, and then the one plain line on what a
// code and a name are worth.
func TestAccessRequestsListing(t *testing.T) {
	t.Parallel()
	for _, all := range []bool{false, true} {
		var out, errBuf bytes.Buffer
		if err := newTestPrinter(&out, &errBuf, FormatText, false, false, false).AccessRequests(fixtureRequests("Ana Lopez"), all); err != nil {
			t.Fatal(err)
		}
		want := "CODE     NAME           DEVICE ID            REQUESTED\n" +
			"482 913  \"Ana Lopez\"    abcd-efgh-2345-mnop  2m ago\n" +
			"175 306  no name given  qrst-uvwx-yz67-abcd  1h ago\n"
		if all {
			want = "CODE     NAME           DEVICE ID            REQUESTED  CRID\n" +
				"482 913  \"Ana Lopez\"    abcd-efgh-2345-mnop  2m ago     " + accessCRID + "\n" +
				"175 306  no name given  qrst-uvwx-yz67-abcd  1h ago     " + accessOtherCRID + "\n"
		}
		want += "\nApprove a code only when the person gave it to you themselves; a name can be typed by anyone.\n"
		if got := out.String(); got != want || errBuf.Len() != 0 {
			t.Fatalf("all=%t: listing =\n%s\nwant\n%s\nstderr %q", all, got, want, errBuf.String())
		}
	}
}

// TestAccessRequestsListingAlwaysEndsWithTheSafetyLine pins that the line is
// the last line of every text listing that has a request, with and without
// color, and is written once.
func TestAccessRequestsListingAlwaysEndsWithTheSafetyLine(t *testing.T) {
	t.Parallel()
	for _, all := range []bool{false, true} {
		for _, color := range []bool{false, true} {
			var out, errBuf bytes.Buffer
			if err := newTestPrinter(&out, &errBuf, FormatText, false, color, false).AccessRequests(fixtureRequests("Ana Lopez")[:1], all); err != nil {
				t.Fatal(err)
			}
			text := out.String()
			if !strings.HasSuffix(text, "\n\n"+msgApproveOnlyGivenCodes+"\n") || strings.Count(text, msgApproveOnlyGivenCodes) != 1 {
				t.Fatalf("all=%t color=%t: the listing does not end with the safety line, once:\n%s", all, color, text)
			}
		}
	}
	for _, part := range []string{"only when the person gave it to you themselves", "a name can be typed by anyone"} {
		if !strings.Contains(msgApproveOnlyGivenCodes, part) {
			t.Errorf("the safety line lost %q", part)
		}
	}
}

// TestEmptyAccessRequestsListing pins the empty listing: nothing on stdout in
// text, a note on stderr, an empty array in JSON, and no safety line, because
// there is no code to approve.
func TestEmptyAccessRequestsListing(t *testing.T) {
	t.Parallel()
	for all, note := range map[bool]string{true: msgNoPendingRequests, false: msgNoPendingRequestsForOne} {
		var out, errBuf bytes.Buffer
		if err := newTestPrinter(&out, &errBuf, FormatText, false, false, false).AccessRequests(nil, all); err != nil {
			t.Fatal(err)
		}
		if out.Len() != 0 || errBuf.String() != note+"\n" {
			t.Fatalf("all=%t: empty listing wrote stdout %q stderr %q", all, out.String(), errBuf.String())
		}
		out.Reset()
		errBuf.Reset()
		if err := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false).AccessRequests(nil, all); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(strings.Fields(out.String()), ""); got != `{"requests":[]}` || errBuf.Len() != 0 {
			t.Fatalf("all=%t: empty JSON listing = %q, stderr %q", all, out.String(), errBuf.String())
		}
	}
}

// TestAccessRequestsJSONAndQuiet pins the two script-facing listings. JSON
// has the stable member names, the raw six digits, and name_verified false on
// every row, with or without a name. --quiet prints what `qurl approve`
// takes: the code, after the CRID when the listing covers every resource.
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
	first, second := document.Requests[0], document.Requests[1]
	want := map[string]any{
		"code": accessCode, "name": "Ana Lopez", "name_verified": false, "device_id": accessDevice,
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
	if _, named := second["name"]; named || second["name_verified"] != false || second["code"] != "175306" {
		t.Fatalf("a request with no name = %v, want no name member and name_verified false", second)
	}
	if _, has := second["expires_at"]; has {
		t.Fatalf("a request with no expiry has the member: %v", second)
	}

	for all, wantQuiet := range map[bool]string{
		false: accessCode + "\n175306\n",
		true:  accessCRID + " " + accessCode + "\n" + accessOtherCRID + " 175306\n",
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
	on := true
	for _, address := range []string{accessAddress, ""} {
		var out, errBuf bytes.Buffer
		p := newTestPrinter(&out, &errBuf, FormatText, false, false, false)
		if err := p.AccessRequestsSetting(&qurlapi.ResourceSummary{CRID: accessCRID, AccessRequests: &on}, address); err != nil {
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
	for people, want := range map[int]string{
		0: "Access requests are off for " + accessCRID + ". Nobody new can ask for access.\n",
		1: "Access requests are off for " + accessCRID + ". Nobody new can ask for access.\n" +
			"1 approved person still has access. See them, or take the access away, with `qurl grants " + accessCRID + "`.\n",
		3: "Access requests are off for " + accessCRID + ". Nobody new can ask for access.\n" +
			"3 approved people still have access. See them, or take access away, with `qurl grants " + accessCRID + "`.\n",
	} {
		resource := &qurlapi.ResourceSummary{CRID: accessCRID, AccessRequests: &off, AllowedPasskeys: make([]qurlapi.AllowedPasskey, people)}
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
	on := true
	var out, errBuf bytes.Buffer
	if err := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false).AccessRequestsSetting(&qurlapi.ResourceSummary{CRID: accessCRID, AccessRequests: &on}, accessAddress); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(strings.Fields(out.String()), ""); got != `{"crid":"`+accessCRID+`","access_requests":true,"resource_url":"`+accessAddress+`"}` {
		t.Fatalf("on JSON = %q", out.String())
	}
	out.Reset()
	if err := newTestPrinter(&out, &errBuf, FormatText, true, false, false).AccessRequestsSetting(&qurlapi.ResourceSummary{CRID: accessCRID, AccessRequests: &on}, accessAddress); err != nil || out.String() != accessCRID+"\n" {
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
	wantJSON := `{"crid":"` + accessCRID + `","approved":true,"name":"AnaLopez","name_verified":false,"device_id":"` + accessDevice + `","approved_at":"2026-03-02T00:00:00Z"}`
	if got := strings.Join(strings.Fields(out.String()), ""); got != wantJSON {
		t.Fatalf("approved JSON = %q, want %q", got, wantJSON)
	}
	out.Reset()
	if err := newTestPrinter(&out, &errBuf, FormatText, true, false, false).Approved(accessCRID, person); err != nil || out.String() != accessDevice+"\n" {
		t.Fatalf("--quiet = %q, %v; want the device id", out.String(), err)
	}
	if err := newTestPrinter(&out, &errBuf, FormatText, false, false, false).Approved(accessCRID, nil); err == nil {
		t.Fatal("an approval of nobody was rendered")
	}
}

// TestDeniedSaysThatNoAccessWasGiven pins the denial in its three forms.
func TestDeniedSaysThatNoAccessWasGiven(t *testing.T) {
	t.Parallel()
	var out, errBuf bytes.Buffer
	if err := newTestPrinter(&out, &errBuf, FormatText, false, false, false).Denied(accessCRID, accessCode); err != nil {
		t.Fatal(err)
	}
	if want := "Denied the request with the code 482 913 for " + accessCRID + ". No access was given.\n"; out.Len() != 0 || errBuf.String() != want {
		t.Fatalf("denied wrote stdout %q stderr %q, want stderr %q", out.String(), errBuf.String(), want)
	}
	errBuf.Reset()
	if err := newTestPrinter(&out, &errBuf, FormatJSON, false, false, false).Denied(accessCRID, accessCode); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(strings.Fields(out.String()), ""); got != `{"crid":"`+accessCRID+`","code":"`+accessCode+`","denied":true}` || errBuf.Len() != 0 {
		t.Fatalf("denied JSON = %q", out.String())
	}
	out.Reset()
	if err := newTestPrinter(&out, &errBuf, FormatText, true, false, false).Denied(accessCRID, accessCode); err != nil || out.String() != accessCode+"\n" {
		t.Fatalf("--quiet = %q, %v", out.String(), err)
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
