package consume

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/layervai/qurl-go/qurl"
	"github.com/layervai/qurl-go/qurl/qurltest"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

// Tests that put the SDK's own test server where the service would stand
// (qurltest.CRIDLinkServer). The opener, the settings file and the SDK call
// are the production ones: the SDK builds, seals and sends the request,
// checks that the reply is the server's, and runs every check it has on an
// issued link. Only the HTTP client is the double, so nothing leaves the
// process.
//
// The server answers the link request and nothing after it. Its link is a
// fixture of the public test vectors: it names a cell that is not this
// server and its expiry is in the past, so it cannot be opened. These tests
// therefore stop at the issued link and at how a refusal is read. They do
// not fetch content.

// sdkServerOpener returns an opener whose settings file is the server's
// deployment and whose requests go to the server and nowhere else.
func sdkServerOpener(t *testing.T, server *qurltest.CRIDLinkServer) *AccessOpener {
	t.Helper()
	return deploymentOpener(t, server.Deployment(), server.Client())
}

// TestRequestCRIDLinkIssuedByTheSDKTestServer pins the answer that is a link,
// end to end through the SDK: the request is offered, exactly one request is
// sent, it names the CRID and no user agent, and the link that comes back is
// one the CLI's own check of a link against a CRID accepts.
func TestRequestCRIDLinkIssuedByTheSDKTestServer(t *testing.T) {
	t.Parallel()
	server := qurltest.NewCRIDLinkServer()
	opener := sdkServerOpener(t, server)

	if offered, err := opener.CRIDLinkOffered(); !offered || err != nil {
		t.Fatalf("CRIDLinkOffered = %t, %v; want offered", offered, err)
	}
	if got := server.Requests(); len(got) != 0 {
		t.Fatalf("the check sent %d request(s), want none", len(got))
	}

	issued, err := opener.RequestCRIDLink(context.Background(), server.CRID())
	if err != nil || issued == nil {
		t.Fatalf("RequestCRIDLink = %v, %v; want the link the server issues", issued, err)
	}
	origin := server.Deployment().CRIDLink.LinkOrigin
	if !strings.HasPrefix(issued.Link, origin+"/#") {
		t.Errorf("the issued link is not on the link origin %s the settings name", origin)
	}
	if !NeedsAccessGrant(issued.Link) {
		t.Error("the issued link does not read as one that is opened through the platform")
	}
	// The command checks every link against the CRID once more before it
	// acts on it. A link the SDK issued must pass that check too.
	if err := opener.Verify(context.Background(), issued.Link, server.CRID()); err != nil {
		t.Errorf("Verify of the issued link against its CRID = %v, want nil", err)
	}
	// And only against its own CRID.
	if err := opener.Verify(context.Background(), issued.Link, apitest.GenerateResourceKey(t).CRID); !errors.Is(err, ErrLinkVerification) {
		t.Errorf("Verify of the issued link against another CRID = %v, want ErrLinkVerification", err)
	}

	requests := server.Requests()
	if len(requests) != 1 || requests[0].CRID != server.CRID() {
		t.Fatalf("the server answered %+v, want exactly one request for %s", requests, server.CRID())
	}
	if requests[0].UserAgent != "" {
		t.Errorf("the request carried the user agent %q, want none", requests[0].UserAgent)
	}
}

// TestRequestCRIDLinkRefusedByTheSDKTestServer pins how each refusal reads
// when it comes through the SDK from a server, not from a hand-built error:
// the six codes the request defines, and codes outside that set.
//
// A code outside the set is what a server answers when it does not serve
// this request, 51002 among them. It reads as "try again later" and never as
// "update qurl". Its code stays available for a diagnostic line.
//
// The last two codes do not have five digits. The SDK hands them on like any
// other decimal code, so the diagnostic line shows them too:
// maxRefusalCodeDigits says why its bound is not five.
func TestRequestCRIDLinkRefusedByTheSDKTestServer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		code string
		want error
		// advisesUpdate marks the one refusal whose remedy is a newer CLI.
		advisesUpdate bool
	}{
		{code: "52601", want: ErrCRIDLinkUnavailable},
		{code: "52602", want: ErrCRIDNotFoundNoDevice},
		{code: "52603", want: ErrCRIDLinkRateLimited},
		{code: "52604", want: ErrCRIDPublisherOffline},
		{code: "52605", want: ErrCRIDResourceClosed},
		{code: "52606", want: ErrCRIDLinkRequestRejected, advisesUpdate: true},
		{code: "51002", want: ErrCRIDLinkUnavailable},
		{code: "52005", want: ErrCRIDLinkUnavailable},
		{code: "52607", want: ErrCRIDLinkUnavailable},
		{code: "7", want: ErrCRIDLinkUnavailable},
		{code: "526001", want: ErrCRIDLinkUnavailable},
	} {
		t.Run(tc.code, func(t *testing.T) {
			t.Parallel()
			server := qurltest.NewCRIDLinkServer()
			server.Refuse(tc.code)
			opener := sdkServerOpener(t, server)

			issued, err := opener.RequestCRIDLink(context.Background(), server.CRID())
			if issued != nil || err == nil {
				t.Fatalf("RequestCRIDLink = %v, %v; want no link and a refusal", issued, err)
			}
			if CRIDNotRequestable(err) {
				t.Fatalf("a refusal from the server read as a CRID the SDK will not ask for: %v", err)
			}
			if len(server.Requests()) != 1 {
				t.Fatalf("the server answered %d request(s), want one and no retry", len(server.Requests()))
			}

			got := ClassifyCRIDLinkError(err, false)
			if !errors.Is(got, tc.want) {
				t.Fatalf("code %s classified as %v, want %v", tc.code, got, tc.want)
			}
			if strings.Contains(got.Error(), tc.code) {
				t.Errorf("the message %q carries the code", got.Error())
			}
			if advises := strings.Contains(got.Error(), "update"); advises != tc.advisesUpdate {
				t.Errorf("code %s: the message %q advises an update = %t, want %t", tc.code, got.Error(), advises, tc.advisesUpdate)
			}
			if code, ok := CRIDLinkRefusalCode(err); !ok || code != tc.code {
				t.Errorf("CRIDLinkRefusalCode = %q, %t; want %s for the diagnostic line", code, ok, tc.code)
			}
		})
	}
}

// TestRequestCRIDLinkForAnotherCRIDIsNotFound pins the default answer of a
// server for a CRID it does not hold: not found, with the hint that depends
// on the device.
func TestRequestCRIDLinkForAnotherCRIDIsNotFound(t *testing.T) {
	t.Parallel()
	server := qurltest.NewCRIDLinkServer()
	other := apitest.DeriveCRID(t, apitest.GenerateResourceKey(t).DER, apitest.VersionProduction)

	issued, err := sdkServerOpener(t, server).RequestCRIDLink(context.Background(), other)
	if issued != nil || !errors.Is(err, qurl.ErrCRIDLinkNotFound) {
		t.Fatalf("RequestCRIDLink = %v, %v; want no link and qurl.ErrCRIDLinkNotFound", issued, err)
	}
	if got := ClassifyCRIDLinkError(err, true); !errors.Is(got, ErrCRIDNotFound) || errors.Is(got, ErrCRIDNotFoundNoDevice) {
		t.Errorf("with a device identity: %v, want ErrCRIDNotFound", got)
	}
	if requests := server.Requests(); len(requests) != 1 || requests[0].CRID != other {
		t.Errorf("the server answered %+v, want one request for %s", requests, other)
	}
}

// TestRequestCRIDLinkForACRIDTheSDKCannotCheckReachesNoServer pins, against a
// server that would answer, that a CRID whose version the SDK cannot check is
// refused before anything is sent. The request is offered here, so this is
// the case a caller must not read as "not offered".
func TestRequestCRIDLinkForACRIDTheSDKCannotCheckReachesNoServer(t *testing.T) {
	t.Parallel()
	server := qurltest.NewCRIDLinkServer()
	opener := sdkServerOpener(t, server)
	unknownVersion := apitest.DeriveCRID(t, apitest.GenerateResourceKey(t).DER, 0x05)

	if offered, err := opener.CRIDLinkOffered(); !offered || err != nil {
		t.Fatalf("CRIDLinkOffered = %t, %v; want offered", offered, err)
	}
	issued, err := opener.RequestCRIDLink(context.Background(), unknownVersion)
	if issued != nil || !CRIDNotRequestable(err) || !errors.Is(err, qurl.ErrUnsupportedCRIDVersion) {
		t.Fatalf("RequestCRIDLink = %v, %v; want no link and the SDK's refusal of the CRID version", issued, err)
	}
	if got := server.Requests(); len(got) != 0 {
		t.Fatalf("the server answered %d request(s) for a CRID the SDK cannot check, want none", len(got))
	}
}
