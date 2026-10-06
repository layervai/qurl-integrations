package main

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/layervai/qurl-go/qurl"
	"github.com/layervai/qurl-go/qurl/qurltest"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/consume"
	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// Tests for `qurl get` with nothing injected between the command and the
// SDK. The check for whether a link can be asked for with only the CRID, the
// request itself, and the check of the link against the CRID are the
// production functions over the real SDK calls. The SDK's own test server
// (qurltest.CRIDLinkServer) stands where the service would, in process, so no
// request leaves the test.
//
// That server answers the link request and nothing after it. Its link is a
// fixture of the public test vectors, which names a cell that is not the
// server and has an expiry in the past, so it cannot be opened. These tests
// therefore go as far as the issued link: which link the browser is given,
// which link access is requested for, and how a refusal is told to the user.
// Fetching content through a link is covered with injected answers in
// get_crid_link_test.go.

// sdkLinkPath is the SDK's test server with the production opener in front
// of it. The opener reads its settings from a file, as it does for a user
// who set QURL_DEPLOYMENT.
type sdkLinkPath struct {
	server *qurltest.CRIDLinkServer
	opener *consume.AccessOpener
	// link is the link the server issues for its CRID.
	link string
}

// newSDKLinkPath starts a server and writes its deployment as the settings
// file. edit changes the settings before they are written; nil leaves them
// as the server gives them, which offers the request.
func newSDKLinkPath(t *testing.T, edit func(d *qurl.Deployment)) *sdkLinkPath {
	t.Helper()
	// The link is the same fixture for every server. It is read from a
	// second one, so the requests of the server under test count only what
	// the command sent.
	reference := qurltest.NewCRIDLinkServer()
	issued, err := qurl.RequestCRIDLinkWith(t.Context(), reference.CRID(), reference.Config())
	if err != nil {
		t.Fatalf("the SDK's test server did not issue its link: %v", err)
	}

	server := qurltest.NewCRIDLinkServer()
	deployment := server.Deployment()
	if edit != nil {
		edit(deployment)
	}
	return &sdkLinkPath{
		server: server,
		opener: &consume.AccessOpener{LookupEnv: writeTestDeployment(t, deployment), CRIDLinkHTTPClient: server.Client()},
		link:   issued.Link,
	}
}

// wire puts the production check, request and link check into an invocation.
//
// A link from the SDK's server gets the production check against the CRID.
// The mock API's share link is synthetic and cannot pass that check; the
// share path is not what these tests are about, so it is accepted as the
// harness does by default. granted records every link access was requested
// for; the grant then points at the mock's download route.
func (p *sdkLinkPath) wire(t *testing.T, srv *apitest.Server, configure func(args []string) *runOpts, verified, granted *[]string) func(args []string) *runOpts {
	t.Helper()
	return func(args []string) *runOpts {
		opts := configure(args)
		opts.cridLinkOffered = p.opener.CRIDLinkOffered
		opts.requestCRIDLink = p.opener.RequestCRIDLink
		opts.verifyLink = func(ctx context.Context, link, expectedCRID string) error {
			if link != p.link {
				return nil
			}
			*verified = append(*verified, expectedCRID)
			return p.opener.Verify(ctx, link, expectedCRID)
		}
		opts.enterPortalGrant = func(_ context.Context, link string) (consume.AccessGrant, error) {
			*granted = append(*granted, link)
			return consume.AccessGrant{
				ContentURL: srv.URL + apitest.DownloadPath, OpenSeconds: 300,
				AuthorizeContentRequest: func(*http.Request) error { return nil },
			}, nil
		}
		return opts
	}
}

// serverForCRID is a mock API whose resource has the given CRID, so the
// shared run helpers address that CRID. Its share answers point at its own
// download route.
func serverForCRID(t *testing.T, resourceCRID string) *apitest.Server {
	t.Helper()
	key := apitest.GenerateResourceKey(t)
	srv := apitest.NewServerWithKey(t, &apitest.ResourceKey{DER: key.DER, ResourceID: key.ResourceID, CRID: resourceCRID})
	srv.SetShareQURL(srv.URL + apitest.DownloadPath)
	return srv
}

// mistypedCRID returns valid with one character changed, so it has the
// length and alphabet of a CRID and fails its consistency check.
func mistypedCRID(valid string) string {
	mistyped := []byte(valid)
	if mistyped[10] == 'a' {
		mistyped[10] = 'b'
	} else {
		mistyped[10] = 'a'
	}
	return string(mistyped)
}

// The three things deployment settings can say about the request with only
// the CRID, as edits of the SDK test server's own deployment.
const (
	offerOffered    = "offered"
	offerNotOffered = "not offered"
	offerWrongSetup = "wrong setup"
)

func offerEdits() map[string]func(d *qurl.Deployment) {
	return map[string]func(d *qurl.Deployment){
		offerOffered:    nil,
		offerNotOffered: func(d *qurl.Deployment) { d.CRIDLink = nil },
		// The settings name where to send the request, at a host they do not
		// allow.
		offerWrongSetup: func(d *qurl.Deployment) { d.RelayAllowlist = []string{"other.qurltest.invalid"} },
	}
}

// The three kinds of CRID a user can hand to get.
const (
	cridOK          = "ok"
	cridUnsupported = "version this client cannot check"
	cridMalformed   = "malformed"
)

// getResult is what one row of the decision table must end in.
type getResult int

const (
	// resultLinkForCRIDAlone: the link the SDK's server issued is used.
	resultLinkForCRIDAlone getResult = iota
	// resultLinkFromShare: the share request's link is used, as before.
	resultLinkFromShare
	// resultShareNotFound: the share request's not-found answer stands.
	resultShareNotFound
	// resultRefusedVersion: this client cannot ask for a link for the CRID.
	resultRefusedVersion
	// resultRefusedSetup: the settings name an endpoint that cannot be used.
	resultRefusedSetup
	// resultRefusedMalformed: the local check refuses the operand.
	resultRefusedMalformed
)

// TestGetDecisionTable is the whole decision of where get takes its link
// from, one row per case: whether the machine holds an identity, what its
// settings say about the request with only the CRID, and what kind of CRID
// it was given. A device with an identity has one row for each answer of its
// share request.
//
// Every row states what must not happen as well. A machine with no identity
// is enrolled in exactly the rows marked enrolls, which are the rows where
// the request is not offered and get does what it always did. In every other
// row for that machine, opening the device runtime fails the test, the qURL
// API sees no request, and no state directory appears.
func TestGetDecisionTable(t *testing.T) {
	state := bootstrapRegisteredState(t)
	const (
		shareAnswersLink     = "link"
		shareAnswersNotFound = "not found"
		// shareNotAsked marks a row where the share request must not be made.
		shareNotAsked = ""
	)
	rows := []struct {
		identity bool
		offer    string
		crid     string
		share    string
		want     getResult
		// enrolls: a machine with no identity creates one on the share path.
		enrolls bool
		// linkRequests is how many requests the SDK's server must answer.
		linkRequests int
	}{
		// A machine with no identity.
		{offer: offerOffered, crid: cridOK, share: shareNotAsked, want: resultLinkForCRIDAlone, linkRequests: 1},
		{offer: offerOffered, crid: cridUnsupported, share: shareNotAsked, want: resultRefusedVersion},
		{offer: offerOffered, crid: cridMalformed, share: shareNotAsked, want: resultRefusedMalformed},
		{offer: offerNotOffered, crid: cridOK, share: shareAnswersLink, want: resultLinkFromShare, enrolls: true},
		{offer: offerNotOffered, crid: cridOK, share: shareAnswersNotFound, want: resultShareNotFound, enrolls: true},
		{offer: offerNotOffered, crid: cridUnsupported, share: shareAnswersLink, want: resultLinkFromShare, enrolls: true},
		{offer: offerNotOffered, crid: cridUnsupported, share: shareAnswersNotFound, want: resultShareNotFound, enrolls: true},
		{offer: offerNotOffered, crid: cridMalformed, share: shareNotAsked, want: resultRefusedMalformed},
		{offer: offerWrongSetup, crid: cridOK, share: shareNotAsked, want: resultRefusedSetup},
		{offer: offerWrongSetup, crid: cridUnsupported, share: shareNotAsked, want: resultRefusedSetup},
		{offer: offerWrongSetup, crid: cridMalformed, share: shareNotAsked, want: resultRefusedMalformed},

		// A device with an identity.
		{identity: true, offer: offerOffered, crid: cridOK, share: shareAnswersLink, want: resultLinkFromShare},
		{identity: true, offer: offerOffered, crid: cridOK, share: shareAnswersNotFound, want: resultLinkForCRIDAlone, linkRequests: 1},
		{identity: true, offer: offerOffered, crid: cridUnsupported, share: shareAnswersLink, want: resultLinkFromShare},
		{identity: true, offer: offerOffered, crid: cridUnsupported, share: shareAnswersNotFound, want: resultShareNotFound},
		{identity: true, offer: offerOffered, crid: cridMalformed, share: shareNotAsked, want: resultRefusedMalformed},
		{identity: true, offer: offerNotOffered, crid: cridOK, share: shareAnswersLink, want: resultLinkFromShare},
		{identity: true, offer: offerNotOffered, crid: cridOK, share: shareAnswersNotFound, want: resultShareNotFound},
		{identity: true, offer: offerNotOffered, crid: cridUnsupported, share: shareAnswersLink, want: resultLinkFromShare},
		{identity: true, offer: offerNotOffered, crid: cridUnsupported, share: shareAnswersNotFound, want: resultShareNotFound},
		{identity: true, offer: offerNotOffered, crid: cridMalformed, share: shareNotAsked, want: resultRefusedMalformed},
		{identity: true, offer: offerWrongSetup, crid: cridOK, share: shareAnswersLink, want: resultLinkFromShare},
		{identity: true, offer: offerWrongSetup, crid: cridOK, share: shareAnswersNotFound, want: resultRefusedSetup},
		{identity: true, offer: offerWrongSetup, crid: cridUnsupported, share: shareAnswersLink, want: resultLinkFromShare},
		{identity: true, offer: offerWrongSetup, crid: cridUnsupported, share: shareAnswersNotFound, want: resultRefusedSetup},
		{identity: true, offer: offerWrongSetup, crid: cridMalformed, share: shareNotAsked, want: resultRefusedMalformed},
	}

	for _, row := range rows {
		for _, mode := range getModes() {
			name := fmt.Sprintf("identity=%t/%s/CRID %s/share %s/%s", row.identity, row.offer, row.crid, row.share, mode.name)
			if row.share == shareNotAsked {
				name = fmt.Sprintf("identity=%t/%s/CRID %s/%s", row.identity, row.offer, row.crid, mode.name)
			}
			t.Run(name, func(t *testing.T) {
				path := newSDKLinkPath(t, offerEdits()[row.offer])

				// The CRID. A link is issued only for the server's own CRID,
				// which is a production one; the rows that compare stderr
				// byte for byte use a test CRID, which draws no
				// wrong-environment warning.
				key := apitest.GenerateResourceKey(t)
				resourceCRID := key.CRID
				switch {
				case row.crid == cridUnsupported:
					resourceCRID = apitest.DeriveCRID(t, key.DER, 0x05)
				case row.crid == cridMalformed:
					resourceCRID = mistypedCRID(key.CRID)
				case row.offer == offerOffered:
					resourceCRID = path.server.CRID()
				}
				srv := serverForCRID(t, resourceCRID)
				if row.share == shareAnswersNotFound {
					shareNotFoundTwice(t, srv)
				}

				// The machine.
				stateDir := filepath.Join(t.TempDir(), "no-device-state")
				enrolled := false
				var machine func(args []string) *runOpts
				switch {
				case row.identity:
					machine = enrolledDevice(t, state)
				case row.enrolls:
					machine = machineThatEnrolls(t, state, &enrolled)
				default:
					machine = machineWithNoIdentity(t, stateDir)
				}

				var verified, granted []string
				run := runShareMode(t, srv, srv.URL, mode, path.wire(t, srv, machine, &verified, &granted))
				stderr := run.result.stderr.String()

				// What the user gets.
				wantCode, wantGolden := exitcode.Success, ""
				switch row.want {
				case resultLinkForCRIDAlone:
					run.link = path.link
					run.mustHaveDelivered(t, mode)
					// The production check ran on the issued link, against the
					// CRID that was asked for, before anything was done with it.
					if len(verified) != 1 || verified[0] != resourceCRID {
						t.Errorf("the issued link was checked against %q, want exactly once against %s", verified, resourceCRID)
					}
					wantGrants := 0
					if mode.downloads {
						wantGrants = 1
					}
					if len(granted) != wantGrants || (wantGrants == 1 && granted[0] != path.link) {
						t.Errorf("access was requested for %d link(s), want %d and only for the issued link", len(granted), wantGrants)
					}
				case resultLinkFromShare:
					run.mustHaveDelivered(t, mode)
				case resultShareNotFound:
					wantCode, wantGolden = exitcode.NotFound, "error_share_notfound"
				case resultRefusedVersion:
					wantCode, wantGolden = exitcode.Config, "error_get_crid_version"
				case resultRefusedSetup:
					wantCode, wantGolden = exitcode.Config, "error_get_crid_setup"
				case resultRefusedMalformed:
					wantCode = exitcode.InvalidInput
					if !strings.Contains(stderr, cridux.MsgTypo) {
						t.Errorf("stderr = %q, want the local check's message for a mistyped CRID", stderr)
					}
				}
				if run.result.code != wantCode {
					t.Fatalf("exit = %d, want %d; stderr: %s", run.result.code, wantCode, stderr)
				}
				if wantCode != exitcode.Success {
					run.mustNotHaveActed(t)
					if len(verified) != 0 || len(granted) != 0 {
						t.Errorf("a refusal checked %d link(s) and requested access for %d", len(verified), len(granted))
					}
				}
				if wantGolden != "" && !mode.tty {
					want := goldenBytes(t, wantGolden+".plain.stderr.golden")
					if row.enrolls {
						// A machine that just enrolled says so first, as it
						// always has.
						want = msgAnonymousDevice + "\n" + want
					}
					if stderr != want {
						t.Errorf("stderr = %q, want %q", stderr, want)
					}
				}

				// What was sent.
				if got := len(path.server.Requests()); got != row.linkRequests {
					t.Errorf("the service answered %d request(s) for a link with only the CRID, want %d", got, row.linkRequests)
				}
				wantAPI := []string(nil)
				if row.share != shareNotAsked {
					wantAPI = []string{"GET /v1/me", "POST " + shareRoute(srv)}
				}
				if got := apiRequests(srv); strings.Join(got, "\n") != strings.Join(wantAPI, "\n") {
					t.Errorf("qURL API requests = %q, want %q", got, wantAPI)
				}

				// What was created.
				if enrolled != row.enrolls {
					t.Errorf("the machine enrolled = %t, want %t", enrolled, row.enrolls)
				}
				if !row.identity && !row.enrolls {
					mustNotExistCmd(t, stateDir)
					if strings.Contains(stderr, msgAnonymousDevice) {
						t.Errorf("stderr %q announces a new device identity", stderr)
					}
				}
			})
		}
	}
}

// TestGetRefusesEveryMalformedCRIDBeforeAnything covers the malformed column
// of the table for each kind of operand the local check refuses, on a
// machine with no identity whose settings offer the request. Nothing is
// asked, nothing is sent, and no identity is created.
func TestGetRefusesEveryMalformedCRIDBeforeAnything(t *testing.T) {
	key := apitest.GenerateResourceKey(t)
	for name, operand := range map[string]string{
		"mistyped":               mistypedCRID(key.CRID),
		"excluded digits":        "q018" + strings.Repeat("a", 56),
		"forbidden version":      apitest.DeriveCRID(t, key.DER, 0x00),
		"a public key":           key.ResourceID,
		"not a CRID at all":      "report-2026",
		"a character never used": key.CRID[:20] + "!" + key.CRID[21:],
	} {
		for _, mode := range getModes() {
			t.Run(name+"/"+mode.name, func(t *testing.T) {
				path := newSDKLinkPath(t, nil)
				srv := serverForCRID(t, operand)
				stateDir := filepath.Join(t.TempDir(), "no-device-state")
				offer := &linkOffer{offered: true}
				configure := withLinkOffer(withLinkRequests(machineWithNoIdentity(t, stateDir), path.opener.RequestCRIDLink), offer.answer)

				run := runShareMode(t, srv, srv.URL, mode, configure)
				if run.result.code != exitcode.InvalidInput {
					t.Fatalf("exit = %d, want %d; stderr: %s", run.result.code, exitcode.InvalidInput, run.result.stderr.String())
				}
				run.mustNotHaveActed(t)
				if offer.checks != 0 {
					t.Errorf("asked whether the request is offered %d times for an operand that is not a CRID, want never", offer.checks)
				}
				if got := len(path.server.Requests()); got != 0 {
					t.Errorf("the service answered %d request(s), want none", got)
				}
				if got := srv.Requests(); len(got) != 0 {
					t.Errorf("the machine sent %d request(s) to the qURL API, want none", len(got))
				}
				mustNotExistCmd(t, stateDir)
			})
		}
	}
}

// TestGetOnAMachineWithNoIdentityThroughTheSDK is get on a machine with no
// identity, with a link issued by the SDK's server. It adds what the table
// does not look at: what the reader is shown about the publisher, which comes
// from the server's answer, and that the machine sent nothing to the qURL
// API.
func TestGetOnAMachineWithNoIdentityThroughTheSDK(t *testing.T) {
	for _, mode := range getModes() {
		t.Run(mode.name, func(t *testing.T) {
			path := newSDKLinkPath(t, nil)
			srv := serverForCRID(t, path.server.CRID())
			stateDir := filepath.Join(t.TempDir(), "no-device-state")
			var verified, granted []string

			run := runShareMode(t, srv, srv.URL, mode, path.wire(t, srv, machineWithNoIdentity(t, stateDir), &verified, &granted))
			run.link = path.link
			run.mustHaveDelivered(t, mode)

			requests := path.server.Requests()
			if len(requests) != 1 || requests[0].CRID != path.server.CRID() || requests[0].UserAgent != "" {
				t.Errorf("the service answered %+v, want one request for the CRID with no user agent", requests)
			}
			if len(verified) != 1 {
				t.Errorf("the issued link was checked %d times, want once", len(verified))
			}

			// The publisher is the one the server's answer names, shown as
			// unverified: a publisher name is never more than a claim.
			shown := run.result.stderr.String()
			if !mode.downloads {
				shown = withoutStyle(run.result.stdout.String())
			}
			if strings.Count(shown, "UNVERIFIED") != 1 || !strings.Contains(shown, `"Example Publisher"`) {
				t.Errorf("output %q, want the publisher of the server's answer shown once, as unverified", shown)
			}

			if got := apiRequests(srv); len(got) != 0 {
				t.Errorf("the machine sent requests to the qURL API, want none: %q", got)
			}
			mustNotExistCmd(t, stateDir)
		})
	}
}

// TestGetReportsLinkSettingsThatCannotBeUsed covers each way the settings
// can name where to ask for a link and be wrong about it. On a machine with
// no identity that is the whole answer: the setup message, the configuration
// exit code, nothing sent and no identity created. It must not read as "not
// offered", which would enroll the machine and then say "not found".
func TestGetReportsLinkSettingsThatCannotBeUsed(t *testing.T) {
	for name, edit := range map[string]func(d *qurl.Deployment){
		"host not allowed":             func(d *qurl.Deployment) { d.RelayAllowlist = []string{"other.qurltest.invalid"} },
		"address not https":            func(d *qurl.Deployment) { d.CRIDLink.RelayURL = "http://relay.qurltest.invalid" },
		"no address":                   func(d *qurl.Deployment) { d.CRIDLink.RelayURL = "" },
		"link origin with a slash":     func(d *qurl.Deployment) { d.CRIDLink.LinkOrigin += "/" },
		"no link origin":               func(d *qurl.Deployment) { d.CRIDLink.LinkOrigin = "" },
		"two cells":                    func(d *qurl.Deployment) { d.Cells = append(d.Cells, secondTestCell(t)) },
		"no cell":                      func(d *qurl.Deployment) { d.Cells = nil },
		"cell key that cannot be used": func(d *qurl.Deployment) { d.Cells[0].ServerPublicKeyB64 = allZeroKeyB64 },
	} {
		for _, mode := range getModes() {
			t.Run(name+"/"+mode.name, func(t *testing.T) {
				path := newSDKLinkPath(t, edit)
				srv := downloadServer(t)
				stateDir := filepath.Join(t.TempDir(), "no-device-state")
				var verified, granted []string

				run := runShareMode(t, srv, srv.URL, mode, path.wire(t, srv, machineWithNoIdentity(t, stateDir), &verified, &granted))
				stderr := run.result.stderr.String()
				if run.result.code != exitcode.Config {
					t.Fatalf("exit = %d, want %d; stderr: %s", run.result.code, exitcode.Config, stderr)
				}
				run.mustNotHaveActed(t)
				if !mode.tty {
					if want := goldenBytes(t, "error_get_crid_setup.plain.stderr.golden"); stderr != want {
						t.Errorf("stderr = %q, want the golden %q", stderr, want)
					}
				}
				if got := len(path.server.Requests()); got != 0 {
					t.Errorf("the service answered %d request(s), want none", got)
				}
				if got := srv.Requests(); len(got) != 0 {
					t.Errorf("the machine sent %d request(s) to the qURL API, want none", len(got))
				}
				mustNotExistCmd(t, stateDir)
			})
		}
	}
}

// settingsDirWord stands for the directory a test made its settings files
// in. The message for a settings file that cannot be used names the file by
// the path the user gave, so what such a test prints holds a path of the
// machine it ran on. withoutSettingsDir puts this word in its place before
// anything is compared or written to a golden.
const settingsDirWord = "<settings-dir>"

// withoutSettingsDir returns output with dir, the directory a test made its
// settings files in, replaced by settingsDirWord. The separator after it
// becomes "/", so the result is the same on every system.
func withoutSettingsDir(t *testing.T, output, dir string) string {
	t.Helper()
	fixed := strings.ReplaceAll(output, dir+string(filepath.Separator), settingsDirWord+"/")
	if strings.Contains(fixed, dir) {
		t.Fatalf("the output still names the test's own directory: %q", fixed)
	}
	return fixed
}

// unusableSettingsFile is one way QURL_DEPLOYMENT can name a file that cannot
// be read as deployment settings.
type unusableSettingsFile struct {
	name string
	// golden names the stderr golden of what get prints for the file.
	golden string
	// systemWords says that the message ends with the operating system's
	// own words for the failure. Windows words them differently. The golden
	// holds the words of a Unix system, so it is compared on Unix only.
	systemWords bool
	// parserWords says that the message ends with the words of Go's JSON
	// parser for what is wrong with the file. Those words are no contract:
	// a new Go release can word them differently. So this case has no
	// golden, and only the text around the parser's words is compared, on
	// every system.
	parserWords bool
	// create makes the file in dir and returns the path QURL_DEPLOYMENT
	// names. It skips the test on a system where the case cannot be made.
	create func(t *testing.T, dir string) string
}

// unusableSettingsFiles lists the cases: a file that does not exist, a
// directory, a file that is not JSON, and a file this user may not read.
//
// TODO(upstream-contract): the messages of these cases end with qurl-go's own
// words for a settings file it cannot use ("qurl: read deployment <path>: …"
// and "qurl: parse deployment <path>: …"), which the goldens and the
// assertions below hold. If qurl-go words them differently, these fail and
// the goldens are written again; the CLI's own sentence in front of them does
// not change.
func unusableSettingsFiles() []unusableSettingsFile {
	return []unusableSettingsFile{
		{
			name: "missing file", golden: "error_get_crid_settings_missing", systemWords: true,
			create: func(_ *testing.T, dir string) string { return filepath.Join(dir, "absent.json") },
		},
		{
			name: "directory", golden: "error_get_crid_settings_directory", systemWords: true,
			create: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "a-directory")
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "not JSON", parserWords: true,
			create: func(t *testing.T, dir string) string {
				path := filepath.Join(dir, "not-json.json")
				if err := os.WriteFile(path, []byte("not settings"), 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name: "no read permission", golden: "error_get_crid_settings_unreadable", systemWords: true,
			create: func(t *testing.T, dir string) string {
				if runtime.GOOS == "windows" {
					t.Skip("Windows has no permission bits that take read access to a file away from its owner")
				}
				if os.Geteuid() == 0 {
					t.Skip("the root user can read a file whatever its permission bits say")
				}
				// Settings that could be used, so the permission is the only
				// fault.
				raw, err := json.Marshal(shippedShapeDeployment(t))
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "unreadable.json")
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
	}
}

// opener makes the file in dir and returns the production opener of a user
// whose QURL_DEPLOYMENT names it.
func (f *unusableSettingsFile) opener(t *testing.T, dir string) *consume.AccessOpener {
	t.Helper()
	path := f.create(t, dir)
	return &consume.AccessOpener{LookupEnv: func(key string) (string, bool) { return path, key == qurl.EnvDeploymentPath }}
}

// mustBeTheMessage asserts that message is what get prints for the file.
// message is stderr with no terminal styling and with the test's directory
// replaced (withoutSettingsDir).
func (f *unusableSettingsFile) mustBeTheMessage(t *testing.T, message string) {
	t.Helper()
	if f.systemWords && runtime.GOOS == "windows" {
		// The golden holds a Unix system's words for the failure. Everything
		// around them is the same here.
		start := "Error: " + consume.MsgAccessNotConfigured + " (qurl: read deployment " + settingsDirWord + "/"
		if !strings.HasPrefix(message, start) || !strings.HasSuffix(message, ")\n") || strings.Count(message, "\n") != 1 {
			t.Errorf("stderr = %q, want one line that starts with %q and ends with the system's reason", message, start)
		}
		return
	}
	if f.parserWords {
		start := "Error: " + consume.MsgAccessNotConfigured + " (qurl: parse deployment " + settingsDirWord + "/not-json.json: "
		if !strings.HasPrefix(message, start) || !strings.HasSuffix(message, ")\n") || strings.Count(message, "\n") != 1 || len(message) <= len(start)+len(")\n") {
			t.Errorf("stderr = %q, want one line that starts with %q and ends with the parser's reason", message, start)
		}
		return
	}
	if want := goldenBytes(t, f.golden+".plain.stderr.golden"); message != want {
		t.Errorf("stderr = %q, want the golden %q", message, want)
	}
}

// TestGetReportsASettingsFileThatCannotBeUsed covers the other way a machine
// can be set up wrongly: QURL_DEPLOYMENT names a file that cannot be read as
// settings at all. Each case is a real file of that kind, and the production
// code reads it through the real SDK. What get prints is compared with a
// golden, so the goldens hold what the CLI says and not a message a test put
// together.
//
// The fault is found in one of two places, and get says the same in both:
//
//   - By the check for whether the request is offered. A machine with no
//     identity stops there, with nothing sent and no identity created. A
//     device with an identity is told after its share request was answered
//     "not found". The request is never made.
//   - By the request itself, when the file stopped being usable after the
//     check had said that the request is offered. The request then reads
//     the file again, finds the fault, and sends nothing.
func TestGetReportsASettingsFileThatCannotBeUsed(t *testing.T) {
	state := bootstrapRegisteredState(t)
	const byTheCheck, byTheRequest = "the check", "the request"
	for _, file := range unusableSettingsFiles() {
		for _, foundBy := range []string{byTheCheck, byTheRequest} {
			for _, device := range []bool{false, true} {
				for _, mode := range getModes() {
					t.Run(fmt.Sprintf("%s/found by %s/device=%t/%s", file.name, foundBy, device, mode.name), func(t *testing.T) {
						dir := t.TempDir()
						opener := file.opener(t, dir)
						srv := downloadServer(t)
						stateDir := filepath.Join(t.TempDir(), "no-device-state")
						machine := machineWithNoIdentity(t, stateDir)
						wantAPI := []string(nil)
						if device {
							machine = enrolledDevice(t, state)
							shareNotFoundTwice(t, srv)
							wantAPI = []string{"GET /v1/me", "POST " + shareRoute(srv)}
						}
						offered, request, asks, wantAsks := opener.CRIDLinkOffered, mustNotAskWithTheCRIDAlone(t), 0, 0
						if foundBy == byTheRequest {
							offered, wantAsks = cridLinkIsOffered, 1
							request = func(ctx context.Context, resourceCRID string) (*qurl.CRIDLink, error) {
								asks++
								return opener.RequestCRIDLink(ctx, resourceCRID)
							}
						}

						run := runShareMode(t, srv, srv.URL, mode, withLinkOffer(withLinkRequests(machine, request), offered))
						if run.result.code != exitcode.Config {
							t.Fatalf("exit = %d, want %d; stderr: %s", run.result.code, exitcode.Config, run.result.stderr.String())
						}
						run.mustNotHaveActed(t)
						file.mustBeTheMessage(t, withoutStyle(withoutSettingsDir(t, run.result.stderr.String(), dir)))
						if asks != wantAsks {
							t.Errorf("the request with only the CRID was made %d times, want %d", asks, wantAsks)
						}
						if got := apiRequests(srv); strings.Join(got, "\n") != strings.Join(wantAPI, "\n") {
							t.Errorf("qURL API requests = %q, want %q", got, wantAPI)
						}
						if !device {
							mustNotExistCmd(t, stateDir)
						}
					})
				}
			}
		}
	}
}

// allZeroKeyB64 is a cell key of the right length that no key agreement can
// use.
const allZeroKeyB64 = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

// secondTestCell returns a second usable cell, with a key of its own.
func secondTestCell(t *testing.T) qurl.DeploymentCell {
	t.Helper()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate cell key: %v", err)
	}
	return qurl.DeploymentCell{
		CellID: "qurltest-cell-b", Host: "cell-b.qurltest.invalid", Port: 443,
		ServerPublicKeyB64: base64.StdEncoding.EncodeToString(key.PublicKey().Bytes()),
	}
}
