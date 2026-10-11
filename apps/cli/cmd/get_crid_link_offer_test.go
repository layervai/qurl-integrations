package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	"github.com/layervai/qurl-go/qurl"
	"github.com/layervai/qurl-go/qurl/qurltest"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/consume"
	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// Tests for `qurl get` with nothing injected between the command and the
// SDK. The check for whether the link request is offered, the request itself
// in both of its forms (with the CRID alone, and as this device), and the
// check of the link against the CRID are the production functions over the
// real SDK calls. The SDK's own test server
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
		opts.requestCRIDLinkAsDevice = p.opener.RequestCRIDLinkAsDevice
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

// Who the resource of the SDK's test server opens for.
const (
	// resourcePublic opens for anyone who asks with its CRID.
	resourcePublic = "public"
	// resourceOfThisDevice is private, and the device under test may open it.
	resourceOfThisDevice = "private, this device may open it"
	// resourceOfAnotherDevice is private, and only another device may open it.
	resourceOfAnotherDevice = "private, another device may open it"
)

// What the read of the device key gives in one row.
const (
	// keyNotRead marks a row in which the key must not be read at all: the
	// read fails the test.
	keyNotRead = "not read"
	// keyReadable gives the key in the device state.
	keyReadable = "readable"
	// keyUnreadable gives no key.
	keyUnreadable = "unreadable"
)

// getResult is what one row of the decision table must end in.
type getResult int

const (
	// resultLinkFromLinkRequest: the link the SDK's server issued is used.
	resultLinkFromLinkRequest getResult = iota
	// resultLinkFromShare: the share request's link is used, as before.
	resultLinkFromShare
	// resultShareNotFound: the share request's not-found answer stands.
	resultShareNotFound
	// resultNotFoundForDevice: the link request's not-found answer, with the
	// hint for a device that has an identity.
	resultNotFoundForDevice
	// resultNotFoundNoDevice: the link request's not-found answer, with the
	// hint for a machine that has no identity.
	resultNotFoundNoDevice
	// resultRefusedVersion: this client cannot ask for a link for the CRID.
	resultRefusedVersion
	// resultRefusedSetup: the settings name an endpoint that cannot be used.
	resultRefusedSetup
	// resultRefusedMalformed: the local check refuses the operand.
	resultRefusedMalformed
)

// TestGetDecisionTable is the whole decision of where get takes its link
// from, one row per case, through the real SDK and its test server. It is
// the first table of get_crid_link.go: whether the machine holds an
// identity, whether the command has a share option, what its settings say
// about the link request, what kind of CRID it was given, and whether the
// device can read its key. Where a link request is sent, a row also says who
// the resource opens for. A device with an identity has one row for each
// answer of its share request that the row can reach.
//
// Every row states what must not happen as well.
//
//   - A machine with no identity is enrolled in exactly the rows marked
//     enrolls, which are the rows where the request is not offered and get
//     does what it always did. In every other row for that machine, opening
//     the device runtime fails the test, the qURL API sees no request, and
//     no state directory appears.
//   - The share request is sent in exactly the rows that name its answer.
//     Where the link request gave the link, the qURL API sees nothing.
//   - The device key is read in exactly the rows where the link request
//     comes first and its first request is answered "not found": once, and
//     it is wiped afterwards. A public resource is answered by the first
//     request, so its rows read no key, and neither do the rows for a CRID
//     this client cannot ask for. In every other row a read fails the test.
//   - The service answers exactly the number of link requests the row
//     states, and exactly that many of them under the key of this device.
func TestGetDecisionTable(t *testing.T) {
	state := bootstrapRegisteredState(t)
	devicePublicKey, err := base64.StdEncoding.DecodeString(state.PublicKeyB64)
	if err != nil {
		t.Fatal(err)
	}
	anotherDevice, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const (
		shareAnswersLink     = "link"
		shareAnswersNotFound = "not found"
		// shareNotAsked marks a row where the share request must not be made.
		shareNotAsked = ""
	)
	type row struct {
		identity bool
		// option says the command has a share option: --session-duration.
		option bool
		offer  string
		crid   string
		// resource is who the server's resource opens for. Empty is public.
		resource string
		// key is what the read of the device key gives. Empty is keyNotRead:
		// a read fails the test.
		key string
		// reads is how often the device key must be read.
		reads int
		share string
		want  getResult
		// enrolls: a machine with no identity creates one on the share path.
		enrolls bool
		// linkRequests is how many requests the SDK's server must answer, and
		// asDevice how many of them under the key of this device.
		linkRequests, asDevice int
	}
	rows := []row{
		// A machine with no identity. A share option changes nothing for it.
		{offer: offerOffered, crid: cridOK, share: shareNotAsked, want: resultLinkFromLinkRequest, linkRequests: 1},
		{option: true, offer: offerOffered, crid: cridOK, share: shareNotAsked, want: resultLinkFromLinkRequest, linkRequests: 1},
		{offer: offerOffered, crid: cridOK, resource: resourceOfAnotherDevice, share: shareNotAsked, want: resultNotFoundNoDevice, linkRequests: 1},
		{offer: offerOffered, crid: cridUnsupported, share: shareNotAsked, want: resultRefusedVersion},
		{offer: offerOffered, crid: cridMalformed, share: shareNotAsked, want: resultRefusedMalformed},
		{offer: offerNotOffered, crid: cridOK, share: shareAnswersLink, want: resultLinkFromShare, enrolls: true},
		{option: true, offer: offerNotOffered, crid: cridOK, share: shareAnswersLink, want: resultLinkFromShare, enrolls: true},
		{offer: offerNotOffered, crid: cridOK, share: shareAnswersNotFound, want: resultShareNotFound, enrolls: true},
		{offer: offerNotOffered, crid: cridUnsupported, share: shareAnswersLink, want: resultLinkFromShare, enrolls: true},
		{offer: offerNotOffered, crid: cridUnsupported, share: shareAnswersNotFound, want: resultShareNotFound, enrolls: true},
		{offer: offerNotOffered, crid: cridMalformed, share: shareNotAsked, want: resultRefusedMalformed},
		{offer: offerWrongSetup, crid: cridOK, share: shareNotAsked, want: resultRefusedSetup},
		{option: true, offer: offerWrongSetup, crid: cridOK, share: shareNotAsked, want: resultRefusedSetup},
		{offer: offerWrongSetup, crid: cridUnsupported, share: shareNotAsked, want: resultRefusedSetup},
		{offer: offerWrongSetup, crid: cridMalformed, share: shareNotAsked, want: resultRefusedMalformed},

		// A device with an identity and no share option, where the request is
		// offered: the link request first. A public resource is answered by
		// the first request, and the device key is not read for it. The first
		// row says so with a read that fails the test. The next two show
		// that it does not matter what a read would have given.
		{identity: true, offer: offerOffered, crid: cridOK, share: shareNotAsked, want: resultLinkFromLinkRequest, linkRequests: 1},
		{identity: true, offer: offerOffered, crid: cridOK, key: keyReadable, share: shareNotAsked, want: resultLinkFromLinkRequest, linkRequests: 1},
		{identity: true, offer: offerOffered, crid: cridOK, key: keyUnreadable, share: shareNotAsked, want: resultLinkFromLinkRequest, linkRequests: 1},
		// A private resource this device may open. The first request is
		// answered "not found", and only then is the key read. With its key
		// the device gets the link from the second request, and sends no
		// share request. Without its key it has asked with the CRID alone,
		// the answer "not found" stands, and its share request decides.
		{
			identity: true, offer: offerOffered, crid: cridOK, resource: resourceOfThisDevice, key: keyReadable, reads: 1,
			share: shareNotAsked, want: resultLinkFromLinkRequest, linkRequests: 2, asDevice: 1,
		},
		{
			identity: true, offer: offerOffered, crid: cridOK, resource: resourceOfThisDevice, key: keyUnreadable, reads: 1,
			share: shareAnswersLink, want: resultLinkFromShare, linkRequests: 1,
		},
		{
			identity: true, offer: offerOffered, crid: cridOK, resource: resourceOfThisDevice, key: keyUnreadable, reads: 1,
			share: shareAnswersNotFound, want: resultNotFoundForDevice, linkRequests: 1,
		},
		// A private resource this device may not open: two requests with the
		// key, one without, and "not found" every time. The share request
		// then decides, and on its "not found" the answer is the link
		// request's.
		{
			identity: true, offer: offerOffered, crid: cridOK, resource: resourceOfAnotherDevice, key: keyReadable, reads: 1,
			share: shareAnswersLink, want: resultLinkFromShare, linkRequests: 2,
		},
		{
			identity: true, offer: offerOffered, crid: cridOK, resource: resourceOfAnotherDevice, key: keyReadable, reads: 1,
			share: shareAnswersNotFound, want: resultNotFoundForDevice, linkRequests: 2,
		},
		{
			identity: true, offer: offerOffered, crid: cridOK, resource: resourceOfAnotherDevice, key: keyUnreadable, reads: 1,
			share: shareAnswersNotFound, want: resultNotFoundForDevice, linkRequests: 1,
		},
		// A CRID this client cannot ask for: nothing is sent for a link, the
		// key is not read, and the share request's answer stands.
		{identity: true, offer: offerOffered, crid: cridUnsupported, key: keyReadable, share: shareAnswersLink, want: resultLinkFromShare},
		{identity: true, offer: offerOffered, crid: cridUnsupported, key: keyReadable, share: shareAnswersNotFound, want: resultShareNotFound},
		{identity: true, offer: offerOffered, crid: cridUnsupported, key: keyUnreadable, share: shareAnswersLink, want: resultLinkFromShare},
		{identity: true, offer: offerOffered, crid: cridUnsupported, key: keyUnreadable, share: shareAnswersNotFound, want: resultShareNotFound},
		{identity: true, offer: offerOffered, crid: cridUnsupported, share: shareAnswersNotFound, want: resultShareNotFound},
		{identity: true, offer: offerOffered, crid: cridMalformed, share: shareNotAsked, want: resultRefusedMalformed},

		// A device with an identity and a share option, where the request is
		// offered: the share request first, as before. The key is not read,
		// and the link request is made with the CRID alone, also for a
		// private resource this device may open.
		{identity: true, option: true, offer: offerOffered, crid: cridOK, share: shareAnswersLink, want: resultLinkFromShare},
		{identity: true, option: true, offer: offerOffered, crid: cridOK, share: shareAnswersNotFound, want: resultLinkFromLinkRequest, linkRequests: 1},
		{
			identity: true, option: true, offer: offerOffered, crid: cridOK, resource: resourceOfThisDevice,
			share: shareAnswersNotFound, want: resultNotFoundForDevice, linkRequests: 1,
		},
		{identity: true, option: true, offer: offerOffered, crid: cridUnsupported, share: shareAnswersLink, want: resultLinkFromShare},
		{identity: true, option: true, offer: offerOffered, crid: cridUnsupported, share: shareAnswersNotFound, want: resultShareNotFound},
		{identity: true, option: true, offer: offerOffered, crid: cridMalformed, share: shareNotAsked, want: resultRefusedMalformed},

		// A device with an identity where the request is not offered, or the
		// settings are wrong: the share request, as before, with or without a
		// share option. The key is not read.
		{identity: true, offer: offerNotOffered, crid: cridOK, share: shareAnswersLink, want: resultLinkFromShare},
		{identity: true, offer: offerNotOffered, crid: cridOK, share: shareAnswersNotFound, want: resultShareNotFound},
		{identity: true, option: true, offer: offerNotOffered, crid: cridOK, share: shareAnswersLink, want: resultLinkFromShare},
		{identity: true, option: true, offer: offerNotOffered, crid: cridOK, share: shareAnswersNotFound, want: resultShareNotFound},
		{identity: true, offer: offerNotOffered, crid: cridUnsupported, share: shareAnswersLink, want: resultLinkFromShare},
		{identity: true, offer: offerNotOffered, crid: cridUnsupported, share: shareAnswersNotFound, want: resultShareNotFound},
		{identity: true, offer: offerNotOffered, crid: cridMalformed, share: shareNotAsked, want: resultRefusedMalformed},
		{identity: true, offer: offerWrongSetup, crid: cridOK, share: shareAnswersLink, want: resultLinkFromShare},
		{identity: true, offer: offerWrongSetup, crid: cridOK, share: shareAnswersNotFound, want: resultRefusedSetup},
		{identity: true, option: true, offer: offerWrongSetup, crid: cridOK, share: shareAnswersLink, want: resultLinkFromShare},
		{identity: true, option: true, offer: offerWrongSetup, crid: cridOK, share: shareAnswersNotFound, want: resultRefusedSetup},
		{identity: true, offer: offerWrongSetup, crid: cridUnsupported, share: shareAnswersLink, want: resultLinkFromShare},
		{identity: true, offer: offerWrongSetup, crid: cridUnsupported, share: shareAnswersNotFound, want: resultRefusedSetup},
		{identity: true, offer: offerWrongSetup, crid: cridMalformed, share: shareNotAsked, want: resultRefusedMalformed},
	}

	for _, row := range rows {
		if row.resource == "" {
			row.resource = resourcePublic
		}
		if row.key == "" {
			row.key = keyNotRead
		}
		for _, mode := range getModes() {
			name := fmt.Sprintf("identity=%t/option=%t/%s/CRID %s/%s/key %s", row.identity, row.option, row.offer, row.crid, row.resource, row.key)
			if row.share != shareNotAsked {
				name += "/share " + row.share
			}
			t.Run(name+"/"+mode.name, func(t *testing.T) {
				path := newSDKLinkPath(t, offerEdits()[row.offer])
				switch row.resource {
				case resourceOfThisDevice:
					path.server.PrivateFor(devicePublicKey)
				case resourceOfAnotherDevice:
					path.server.PrivateFor(anotherDevice.PublicKey().Bytes())
				}

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
				// The read of the device key.
				var keyReads *deviceKeyReads
				switch row.key {
				case keyReadable:
					keyReads = deviceKeyOf(t, state)
				case keyUnreadable:
					keyReads = noDeviceKey(connectorstate.NoDeviceKeyUnreadable)
				}
				read := mustNotReadTheDeviceKey(t)
				if keyReads != nil {
					read = keyReads.read
				}
				if row.option {
					machine = withArgs(machine, "--session-duration", "5m")
				}

				var verified, granted []string
				run := runShareMode(t, srv, srv.URL, mode, withDeviceKey(path.wire(t, srv, machine, &verified, &granted), read))
				stderr := run.result.stderr.String()

				// What the user gets.
				wantCode, wantGolden := exitcode.Success, ""
				switch row.want {
				case resultLinkFromLinkRequest:
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
				case resultNotFoundForDevice:
					wantCode, wantGolden = exitcode.NotFound, "error_get_crid_notfound"
				case resultNotFoundNoDevice:
					wantCode, wantGolden = exitcode.NotFound, "error_get_crid_notfound_no_device"
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
					if resourceCRID == path.server.CRID() {
						// The server's CRID is a production one, and the mock
						// API is not production. Every command says so first.
						want = "Warning: " + cridux.MsgProductionOnOther + "\n" + want
					}
					if stderr != want {
						t.Errorf("stderr = %q, want %q", stderr, want)
					}
				}

				// What was sent for a link.
				requests := path.server.Requests()
				asDevice := 0
				for _, request := range requests {
					if request.AsDevice {
						asDevice++
					}
					if request.CRID != resourceCRID || request.UserAgent != "" {
						t.Errorf("link request %+v, want the CRID that was asked for and no user agent", request)
					}
				}
				if len(requests) != row.linkRequests || asDevice != row.asDevice {
					t.Errorf("the service answered %d link request(s), %d of them under the key of this device; want %d and %d",
						len(requests), asDevice, row.linkRequests, row.asDevice)
				}
				if asDevice > 0 && requests[0].AsDevice {
					t.Error("the first link request was sent under the device key, want a random key first")
				}
				// What was sent to the qURL API.
				wantAPI := []string(nil)
				if row.share != shareNotAsked {
					wantAPI = []string{"GET /v1/me", "POST " + shareRoute(srv)}
				}
				if got := apiRequests(srv); strings.Join(got, "\n") != strings.Join(wantAPI, "\n") {
					t.Errorf("qURL API requests = %q, want %q", got, wantAPI)
				}

				// The device key: read as often as the row says, and wiped. A
				// row with no read of its own fails on any read at all.
				if keyReads != nil {
					if len(keyReads.given) != row.reads {
						t.Errorf("the device key was read %d times, want %d", len(keyReads.given), row.reads)
					}
					for _, given := range keyReads.given {
						if given != nil && !bytes.Equal(given, make([]byte, len(given))) {
							t.Error("the device key was not wiped after the link request")
						}
					}
				} else if row.reads != 0 {
					t.Fatalf("the row wants %d read(s) of the device key and names no key to read", row.reads)
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

// relayThatIsSlowOnce is the HTTP transport of a relay that does not answer
// the first request it gets. That request waits until its context ends, and
// then fails with the error of the context, as a transport does. Every later
// request goes to next.
type relayThatIsSlowOnce struct {
	next     http.RoundTripper
	requests int
}

func (r *relayThatIsSlowOnce) RoundTrip(req *http.Request) (*http.Response, error) {
	r.requests++
	if r.requests > 1 {
		return r.next.RoundTrip(req)
	}
	if req.Body != nil {
		_ = req.Body.Close()
	}
	<-req.Context().Done()
	return nil, req.Context().Err()
}

// TestGetMakesTheLinkRequestOnceMoreThroughTheSDK runs the link request that
// is made once more with nothing injected between the command and the SDK.
// The relay does not answer the first link request, so it is the SDK itself
// that reports the end of the short time limit. get must read that report as
// "no answer when the short limit ran out". If it read it as anything else,
// it would not ask once more, and the device would get no link.
//
// The share request says "not found". The request made once more reaches the
// SDK's server, which issues the link, and the link passes the production
// check against the CRID. The server answers one request, and not under a
// device key, although the first request was made as this device. The device
// key is not read: no request was answered "not found".
func TestGetMakesTheLinkRequestOnceMoreThroughTheSDK(t *testing.T) {
	state := bootstrapRegisteredState(t)
	for _, readable := range []bool{true, false} {
		for _, mode := range getModes() {
			t.Run(fmt.Sprintf("key readable=%t/%s", readable, mode.name), func(t *testing.T) {
				path := newSDKLinkPath(t, nil)
				relay := &relayThatIsSlowOnce{next: path.server}
				path.opener.CRIDLinkHTTPClient = &http.Client{Transport: relay}
				srv := serverForCRID(t, path.server.CRID())
				shareNotFoundTwice(t, srv)
				keyReads := noDeviceKey(connectorstate.NoDeviceKeyUnreadable)
				if readable {
					keyReads = deviceKeyOf(t, state)
				}
				var verified, granted []string
				configure := func(args []string) *runOpts {
					opts := withDeviceKey(path.wire(t, srv, enrolledDevice(t, state), &verified, &granted), keyReads.read)(args)
					opts.linkTimeoutBeforeShare = testShortLimit
					return opts
				}

				run := runShareMode(t, srv, srv.URL, mode, configure)
				run.link = path.link
				run.mustHaveDelivered(t, mode)

				requests := path.server.Requests()
				if relay.requests != 2 || len(requests) != 1 || requests[0].AsDevice || requests[0].CRID != path.server.CRID() {
					t.Errorf("the relay got %d request(s) and the service answered %+v; want two requests to the relay, and one answer of the service, for the CRID and not under a device key",
						relay.requests, requests)
				}
				if len(verified) != 1 || verified[0] != path.server.CRID() {
					t.Errorf("the issued link was checked against %q, want exactly once against %s", verified, path.server.CRID())
				}
				if got, want := apiRequests(srv), []string{"GET /v1/me", "POST " + shareRoute(srv)}; strings.Join(got, "\n") != strings.Join(want, "\n") {
					t.Errorf("qURL API requests = %q, want %q: one share request, between the two link requests", got, want)
				}
				// The first request got no answer, and the request made once
				// more takes no key.
				mustNotHaveReadTheKey(t, keyReads)
			})
		}
	}
}

// TestGetOfAPublicResourceDoesNotOpenTheDeviceState is the reason the device
// key is read on demand. A device with real state gets a public resource,
// through the real SDK and with the production read of the device key. The
// first request gives the link, so the device state is never opened for the
// key. On a machine that keeps its state sealed, that open is where the
// state is unsealed.
//
// The test counts the calls of connectorstate.ResolveKeyProvider. The read of
// the key asks it which key storage the state directory uses, and that is the
// step before it opens the state. Nothing else in a get that the link
// request answers asks it. So no call means the state was not opened.
//
// The control is a private resource this device may open. There the first
// answer is "not found", the state is opened once, and the link comes from
// the request under the key in that state. So the zero for a public resource
// is not the zero of a read that never opens anything.
func TestGetOfAPublicResourceDoesNotOpenTheDeviceState(t *testing.T) {
	state := bootstrapRegisteredState(t)
	devicePublicKey, err := base64.StdEncoding.DecodeString(state.PublicKeyB64)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []bool{false, true} {
		for _, mode := range getModes() {
			t.Run(fmt.Sprintf("private=%t/%s", private, mode.name), func(t *testing.T) {
				if private && !deviceKeyReadable {
					t.Skip("the production read opens no device state on this platform, so there is no control to run")
				}
				path := newSDKLinkPath(t, nil)
				if private {
					path.server.PrivateFor(devicePublicKey)
				}
				srv := serverForCRID(t, path.server.CRID())

				// The SDK calls the read on a goroutine of its own, so the
				// count is atomic.
				var opens atomic.Int32
				var stateDir, before string
				var verified, granted []string
				configure := func(args []string) *runOpts {
					opts := path.wire(t, srv, enrolledDevice(t, state), &verified, &granted)(args)
					opts.openNativeRuntime = func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
						t.Error("the device runtime was opened although the link request gave the link")
						return nil, errors.New("unexpected device runtime open")
					}
					stateDir = opts.shareStateDir
					before = stateSnapshot(t, stateDir)
					// Counted from here on: the fixture has written the state,
					// and what follows is the command.
					resolve := connectorstate.ResolveKeyProvider
					connectorstate.ResolveKeyProvider = func(dir string) (string, error) {
						opens.Add(1)
						return resolve(dir)
					}
					t.Cleanup(func() { connectorstate.ResolveKeyProvider = resolve })
					return opts
				}

				run := runShareMode(t, srv, srv.URL, mode, configure)
				run.link = path.link
				run.mustHaveDelivered(t, mode)

				wantOpens, wantRequests := int32(0), 1
				if private {
					wantOpens, wantRequests = 1, 2
				}
				if got := opens.Load(); got != wantOpens {
					t.Errorf("the device state was opened for the key %d times, want %d", got, wantOpens)
				}
				requests := path.server.Requests()
				if len(requests) != wantRequests || requests[0].AsDevice || (private && !requests[1].AsDevice) {
					t.Errorf("the service answered %+v; want %d request(s), the first under a random key, and for a private resource the second under the device key", requests, wantRequests)
				}
				if after := stateSnapshot(t, stateDir); after != before {
					t.Errorf("the command changed the device state directory.\nbefore:\n%s\nafter:\n%s", before, after)
				}
				if got := apiRequests(srv); len(got) != 0 {
					t.Errorf("qURL API requests = %q, want none: the link request gave the link", got)
				}
			})
		}
	}
}

// TestGetDoesNotWaitForTheKeyWhenTheShortLimitRunsOut runs the case in which
// the short time limit of the link request runs out while the device key is
// read, through the real SDK. The resource is private, so the first request
// is answered "not found" and the SDK asks for the key. The read does not
// return.
//
// The short limit covers the read. When it runs out, the SDK ends the request
// without the key, and get goes on as it does for a link request that got no
// answer in time: to its share request, and after a share "not found" to the
// link request made once more. Nothing is sent under the device key.
//
// The read got the context of the command and not the context of the
// request: it has no time limit here. The command does not give the read up
// early, and it does not wait for it either.
func TestGetDoesNotWaitForTheKeyWhenTheShortLimitRunsOut(t *testing.T) {
	state := bootstrapRegisteredState(t)
	devicePublicKey, err := base64.StdEncoding.DecodeString(state.PublicKeyB64)
	if err != nil {
		t.Fatal(err)
	}
	mode := getFileMode(t)
	// The first request is answered in process and at once. The limit is far
	// above that, also on a slow machine, so the request is answered before
	// the limit runs out. Each run then waits for the rest of the limit.
	const shortLimit = time.Second

	for _, tc := range []struct {
		name          string
		shareNotFound bool
		wantCode      int
		// wantLinkRequests is how many link requests the service answers.
		wantLinkRequests int
	}{
		{name: "the share request gives the link", wantCode: exitcode.Success, wantLinkRequests: 1},
		// The link request is made once more, with the CRID alone. The
		// resource is private, so the answer is "not found".
		{name: "the share request says not found", shareNotFound: true, wantCode: exitcode.NotFound, wantLinkRequests: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := newSDKLinkPath(t, nil)
			path.server.PrivateFor(devicePublicKey)
			srv := serverForCRID(t, path.server.CRID())
			if tc.shareNotFound {
				shareNotFoundTwice(t, srv)
			}

			// A read that does not return until the test lets it. The test
			// lets it go at its end, or after a long time, so a command that
			// waits for the read fails here and does not hang.
			started, release := make(chan struct{}), make(chan struct{})
			var reads, bounded atomic.Int32
			key := deviceKeyOf(t, state).key
			read := func(ctx context.Context) ([]byte, connectorstate.NoDeviceKey) {
				if _, has := ctx.Deadline(); has {
					bounded.Add(1)
				}
				if reads.Add(1) == 1 {
					close(started)
				}
				<-release
				return bytes.Clone(key), ""
			}
			giveUp := time.AfterFunc(time.Minute, func() { close(release) })
			t.Cleanup(func() {
				if giveUp.Stop() {
					close(release)
				}
			})

			var verified, granted []string
			configure := func(args []string) *runOpts {
				opts := withDeviceKey(path.wire(t, srv, enrolledDevice(t, state), &verified, &granted), read)(args)
				opts.linkTimeoutBeforeShare = shortLimit
				return opts
			}
			begun := time.Now()
			run := runShareMode(t, srv, srv.URL, mode, configure)
			if waited := time.Since(begun); waited > 30*time.Second {
				t.Fatalf("the command took %s: it waited for the read of the device key", waited)
			}
			select {
			case <-started:
			default:
				t.Fatal("the device key was not asked for after \"not found\": the short limit ran out before the first answer, and this run shows nothing")
			}

			if run.result.code != tc.wantCode {
				t.Fatalf("exit = %d, want %d; stderr: %s", run.result.code, tc.wantCode, run.result.stderr.String())
			}
			if tc.wantCode == exitcode.Success {
				// The share link of the mock API.
				run.mustHaveDelivered(t, mode)
			} else {
				run.mustNotHaveActed(t)
				if want := goldenBytes(t, "error_get_crid_notfound.plain.stderr.golden"); !strings.HasSuffix(run.result.stderr.String(), want) {
					t.Errorf("stderr = %q, want it to end with the link request's own not-found %q", run.result.stderr.String(), want)
				}
			}
			requests := path.server.Requests()
			if len(requests) != tc.wantLinkRequests {
				t.Fatalf("the service answered %d link request(s), want %d", len(requests), tc.wantLinkRequests)
			}
			for _, request := range requests {
				if request.AsDevice {
					t.Errorf("link request %+v was sent under the device key, want none: the key never arrived", request)
				}
			}
			if got, want := apiRequests(srv), []string{"GET /v1/me", "POST " + shareRoute(srv)}; strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("qURL API requests = %q, want %q: the share request after the short limit ran out", got, want)
			}
			if got := reads.Load(); got != 1 {
				t.Errorf("the device key was read %d times, want once", got)
			}
			if bounded.Load() != 0 {
				t.Error("the read of the device key got a context with a time limit, want the context of the command, which has none here")
			}
		})
	}
}

// TestGetOpensTheDeviceStateWhileTheKeyIsStillRead runs the two things that
// can run at the same time after the short time limit ran out during the read
// of the device key: the read, which goes on, and the share request, which
// opens the same device state.
//
// The read is the production one, over the state of this device. The test
// holds it back until the short limit has run out and the share request has
// the state open. Then the read runs, next to the open store. The store is
// the one the device runtime opens (qurl-connector's NewSDKStore). After the
// read it saves the state, which replaces the state file, and closes.
//
// Each side must do what it does alone. The store opens, loads, saves and
// closes without an error. The read gives the key of the state: it got the
// context of the command, so the end of the short limit did not end it. The
// command gives the share link. Nothing is sent under the device key,
// because the SDK had stopped waiting for it.
func TestGetOpensTheDeviceStateWhileTheKeyIsStillRead(t *testing.T) {
	if !deviceKeyReadable {
		t.Skip("the production read opens no device state on this platform, so nothing can run next to the share request")
	}
	state := bootstrapRegisteredState(t)
	devicePublicKey, err := base64.StdEncoding.DecodeString(state.PublicKeyB64)
	if err != nil {
		t.Fatal(err)
	}
	mode := getFileMode(t)
	// As in TestGetDoesNotWaitForTheKeyWhenTheShortLimitRunsOut: the first
	// request is answered at once, and the run waits for the rest of the
	// limit.
	const shortLimit = time.Second

	path := newSDKLinkPath(t, nil)
	path.server.PrivateFor(devicePublicKey)
	srv := serverForCRID(t, path.server.CRID())

	// The read waits until the share request lets it go. finished is closed
	// when the read has returned, and readKey and readWhy then hold what it
	// returned. A second read would be a fault, and gives no key.
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	letTheReadGo := func() { releaseOnce.Do(func() { close(release) }) }
	var reads atomic.Int32
	var stateDir string
	var readKey []byte
	var readWhy connectorstate.NoDeviceKey
	read := func(ctx context.Context) ([]byte, connectorstate.NoDeviceKey) {
		if reads.Add(1) != 1 {
			return nil, connectorstate.NoDeviceKeyUnreadable
		}
		defer close(finished)
		close(started)
		<-release
		key, why := connectorstate.ReadDeviceStaticPrivateKey(ctx, stateDir, connectorstate.RuntimeSupervisionNative)
		// The SDK wipes the slice it is given, so the test keeps a copy.
		readKey, readWhy = bytes.Clone(key), why
		return key, why
	}
	// A command that waits for the read must fail here, and not hang.
	giveUp := time.AfterFunc(time.Minute, letTheReadGo)
	t.Cleanup(func() {
		giveUp.Stop()
		letTheReadGo()
	})

	storeOpens := 0
	var verified, granted []string
	configure := func(args []string) *runOpts {
		opts := withDeviceKey(path.wire(t, srv, enrolledDevice(t, state), &verified, &granted), read)(args)
		opts.linkTimeoutBeforeShare = shortLimit
		stateDir = opts.shareStateDir
		opts.openNativeRuntime = func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
			storeOpens++
			select {
			case <-started:
			case <-time.After(30 * time.Second):
				t.Error("the device key was not asked for before the share request: the short limit ran out before the first answer, and this run shows nothing")
				return nil, errors.New("the device key was not asked for")
			}
			select {
			case <-finished:
				t.Error("the read of the device key had returned before the share request opened the device state, so the two did not run together")
			default:
			}

			// The store refuses a path with a symbolic link in it, and the
			// temporary directory of a test has one on macOS. So the store
			// gets the same directory by its real path.
			storeDir, err := filepath.EvalSymlinks(cfg.StateDir)
			if err != nil {
				t.Errorf("resolve the device state directory: %v", err)
				return nil, err
			}
			store, err := connectoragentstate.NewSDKStore(storeDir, cfg.AgentID)
			if err != nil {
				t.Errorf("open the device state next to the read: %v", err)
				return nil, err
			}
			defer func() {
				if err := store.Close(); err != nil {
					t.Errorf("close the device state store: %v", err)
				}
			}()
			sdkStore, err := store.Handoff()
			if err != nil {
				t.Errorf("hand off the device state store: %v", err)
				return nil, err
			}
			loaded, err := sdkStore.LoadAgentState(ctx)
			if err != nil || loaded == nil {
				t.Errorf("load the device state next to the read: state present %t, error %v", loaded != nil, err)
				return nil, errors.New("the device state did not load")
			}

			// Now the read runs, with this store open beside it.
			letTheReadGo()
			select {
			case <-finished:
			case <-time.After(30 * time.Second):
				t.Error("the read of the device key did not return while the share request had the device state open")
				return nil, errors.New("the read of the device key did not return")
			}
			if err := sdkStore.SaveAgentState(ctx, loaded); err != nil {
				t.Errorf("save the device state after the read: %v", err)
				return nil, err
			}
			return &bootstrapNativeRuntime{store: &bootstrapAgentStateStore{state: state}}, nil
		}
		return opts
	}

	run := runShareMode(t, srv, srv.URL, mode, configure)
	// The share link of the mock API.
	run.mustHaveDelivered(t, mode)
	if storeOpens != 1 {
		t.Fatalf("the share request opened the device state %d times, want once", storeOpens)
	}
	select {
	case <-finished:
	default:
		t.Fatal("the read of the device key has not returned")
	}
	if want := deviceKeyOf(t, state).key; readWhy != "" || !bytes.Equal(readKey, want) {
		t.Errorf("the read next to the open store gave %d bytes and %q, want the %d bytes of the key in the device state", len(readKey), readWhy, len(want))
	}
	if got := reads.Load(); got != 1 {
		t.Errorf("the device key was read %d times, want once", got)
	}
	requests := path.server.Requests()
	if len(requests) != 1 || requests[0].AsDevice {
		t.Errorf("the service answered %+v, want one link request, not under the device key: the key arrived after the short limit", requests)
	}
	if got, want := apiRequests(srv), []string{"GET /v1/me", "POST " + shareRoute(srv)}; strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("qURL API requests = %q, want %q: the share request after the short limit ran out", got, want)
	}
	// The state is whole after the read and the save.
	if key, why := connectorstate.ReadDeviceStaticPrivateKey(t.Context(), stateDir, connectorstate.RuntimeSupervisionNative); why != "" || !bytes.Equal(key, deviceKeyOf(t, state).key) {
		t.Errorf("a read after the command gave %d bytes and %q, want the key in the device state", len(key), why)
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
