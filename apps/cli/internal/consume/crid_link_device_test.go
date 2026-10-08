package consume

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/layervai/qurl-go/qurl"
	"github.com/layervai/qurl-go/qurl/qurltest"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

// Tests for the request that asks for a link as a registered device. The
// opener, the settings file and the SDK call are the production ones. The
// SDK's own test server stands where the service would
// (qurltest.CRIDLinkServer), so nothing leaves the process. PrivateFor makes
// its resource private for one device key, and each request it records says
// whether it was sent under that key.

// testDeviceKey returns a new device key pair: the 32 bytes of the private
// key, and the public key a server is told to allow.
func testDeviceKey(t *testing.T) (privateKey, publicKey []byte) {
	t.Helper()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate device key: %v", err)
	}
	return key.Bytes(), key.PublicKey().Bytes()
}

// mustBeTheIssuedLink asserts that the call returned the link the server
// issues, as a link the CLI's own check accepts for the server's CRID.
func mustBeTheIssuedLink(t *testing.T, opener *AccessOpener, server *qurltest.CRIDLinkServer, issued *qurl.CRIDLink, err error) {
	t.Helper()
	if err != nil || issued == nil || issued.Link == "" {
		t.Fatalf("the call returned %v, %v; want the link the server issues", issued, err)
	}
	if err := opener.Verify(context.Background(), issued.Link, server.CRID()); err != nil {
		t.Errorf("Verify of the issued link against its CRID = %v, want nil", err)
	}
}

// TestRequestCRIDLinkAsDeviceFollowsTheRuleOfTheSDK pins what the call sends
// for each kind of resource, through the real SDK: a random key first, the
// device key only after "not found".
//
//   - A public resource is answered by the first request. The device key is
//     not used.
//   - A private resource this device may open costs two requests. Only the
//     second one is sent under the device key, and its answer is the link.
//   - A private resource this device may not open costs two requests too,
//     and the answer is "not found".
//
// The request with only the CRID gets "not found" for a private resource
// from one request, whoever asks.
func TestRequestCRIDLinkAsDeviceFollowsTheRuleOfTheSDK(t *testing.T) {
	t.Parallel()
	privateKey, publicKey := testDeviceKey(t)
	_, otherPublicKey := testDeviceKey(t)

	t.Run("public resource", func(t *testing.T) {
		t.Parallel()
		server := qurltest.NewCRIDLinkServer()
		opener := sdkServerOpener(t, server)

		issued, err := opener.RequestCRIDLinkAsDevice(context.Background(), privateKey, server.CRID())
		mustBeTheIssuedLink(t, opener, server, issued, err)
		if requests := server.Requests(); len(requests) != 1 || requests[0].CRID != server.CRID() || requests[0].UserAgent != "" {
			t.Errorf("the server answered %+v, want one request for the CRID with no user agent", requests)
		}
	})

	t.Run("private resource of this device", func(t *testing.T) {
		t.Parallel()
		server := qurltest.NewCRIDLinkServer()
		server.PrivateFor(publicKey)
		opener := sdkServerOpener(t, server)

		issued, err := opener.RequestCRIDLinkAsDevice(context.Background(), privateKey, server.CRID())
		mustBeTheIssuedLink(t, opener, server, issued, err)
		requests := server.Requests()
		if len(requests) != 2 || requests[0].AsDevice || !requests[1].AsDevice {
			t.Fatalf("the server answered %+v, want two requests and only the second under the device key", requests)
		}
		for _, request := range requests {
			if request.CRID != server.CRID() || request.UserAgent != "" {
				t.Errorf("request %+v, want the CRID and no user agent", request)
			}
		}

		// The same server, asked with only the CRID: one request, not found.
		alone, err := opener.RequestCRIDLink(context.Background(), server.CRID())
		if alone != nil || !errors.Is(err, qurl.ErrCRIDLinkNotFound) {
			t.Fatalf("RequestCRIDLink for a private resource = %v, %v; want no link and qurl.ErrCRIDLinkNotFound", alone, err)
		}
		if requests := server.Requests(); len(requests) != 3 || requests[2].AsDevice {
			t.Errorf("the server answered %+v, want one more request, not under the device key", requests)
		}
	})

	t.Run("private resource of another device", func(t *testing.T) {
		t.Parallel()
		server := qurltest.NewCRIDLinkServer()
		server.PrivateFor(otherPublicKey)
		opener := sdkServerOpener(t, server)

		issued, err := opener.RequestCRIDLinkAsDevice(context.Background(), privateKey, server.CRID())
		if issued != nil || !errors.Is(err, qurl.ErrCRIDLinkNotFound) {
			t.Fatalf("RequestCRIDLinkAsDevice = %v, %v; want no link and qurl.ErrCRIDLinkNotFound", issued, err)
		}
		if CRIDNotRequestable(err) || DeviceKeyRefused(err) {
			t.Fatalf("an answer of the server read as a request that was not made: %v", err)
		}
		if got := ClassifyCRIDLinkError(err, true); !errors.Is(got, ErrCRIDNotFound) || errors.Is(got, ErrCRIDNotFoundNoDevice) {
			t.Errorf("classified as %v, want ErrCRIDNotFound with the hint for a device", got)
		}
		// Two requests, and no third.
		if requests := server.Requests(); len(requests) != 2 {
			t.Errorf("the server answered %d request(s), want two", len(requests))
		}
	})
}

// TestRequestCRIDLinkAsDeviceSendsNoSecondRequestAfterAnotherRefusal pins the
// other half of the rule: only "not found" leads to the request under the
// device key. Every other refusal of the first request is the answer, and
// the device key is not used.
func TestRequestCRIDLinkAsDeviceSendsNoSecondRequestAfterAnotherRefusal(t *testing.T) {
	t.Parallel()
	privateKey, publicKey := testDeviceKey(t)
	for _, tc := range []struct {
		code         string
		want         error
		wantRequests int
	}{
		{code: "52601", want: ErrCRIDLinkUnavailable, wantRequests: 1},
		{code: "52603", want: ErrCRIDLinkRateLimited, wantRequests: 1},
		{code: "52604", want: ErrCRIDPublisherOffline, wantRequests: 1},
		{code: "52605", want: ErrCRIDResourceClosed, wantRequests: 1},
		{code: "52606", want: ErrCRIDLinkRequestRejected, wantRequests: 1},
		{code: "51002", want: ErrCRIDLinkUnavailable, wantRequests: 1},
		// "Not found" is the one refusal that is asked about twice.
		{code: "52602", want: ErrCRIDNotFound, wantRequests: 2},
	} {
		t.Run(tc.code, func(t *testing.T) {
			t.Parallel()
			server := qurltest.NewCRIDLinkServer()
			server.PrivateFor(publicKey)
			server.Refuse(tc.code)

			issued, err := sdkServerOpener(t, server).RequestCRIDLinkAsDevice(context.Background(), privateKey, server.CRID())
			if issued != nil || err == nil {
				t.Fatalf("RequestCRIDLinkAsDevice = %v, %v; want no link and a refusal", issued, err)
			}
			if got := ClassifyCRIDLinkError(err, true); !errors.Is(got, tc.want) {
				t.Errorf("code %s classified as %v, want %v", tc.code, got, tc.want)
			}
			if code, ok := CRIDLinkRefusalCode(err); !ok || code != tc.code {
				t.Errorf("CRIDLinkRefusalCode = %q, %t; want %s for the diagnostic line", code, ok, tc.code)
			}
			requests := server.Requests()
			if len(requests) != tc.wantRequests {
				t.Fatalf("the server answered %d request(s), want %d", len(requests), tc.wantRequests)
			}
			if requests[0].AsDevice {
				t.Error("the first request was sent under the device key")
			}
		})
	}
}

// TestRequestCRIDLinkAsDeviceWithAKeyTheSDKCannotUse pins the answer for a
// key that is not a device key: none, too short, too long, or only zero
// bytes as a wiped key holds. The SDK refuses it before it looks at the CRID
// or the settings, and sends nothing.
//
// The answer is DeviceKeyRefused and never CRIDNotRequestable. A caller reads
// CRIDNotRequestable as "this client cannot ask for this CRID" and tells the
// user that the CRID is the problem. A bad key must not be read that way, not
// even for a CRID the SDK would refuse too: the key is checked first.
func TestRequestCRIDLinkAsDeviceWithAKeyTheSDKCannotUse(t *testing.T) {
	t.Parallel()
	privateKey, publicKey := testDeviceKey(t)
	uncheckable := apitest.DeriveCRID(t, apitest.GenerateResourceKey(t).DER, 0x05)
	for name, key := range map[string][]byte{
		"no key":          nil,
		"empty key":       {},
		"31 bytes":        privateKey[:31],
		"33 bytes":        append(append([]byte{}, privateKey...), 1),
		"only zero bytes": make([]byte, 32),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := qurltest.NewCRIDLinkServer()
			server.PrivateFor(publicKey)
			opener := sdkServerOpener(t, server)

			for what, resourceCRID := range map[string]string{"a CRID the server holds": server.CRID(), "a CRID the SDK cannot check": uncheckable} {
				issued, err := opener.RequestCRIDLinkAsDevice(context.Background(), key, resourceCRID)
				if issued != nil || !DeviceKeyRefused(err) {
					t.Errorf("%s: RequestCRIDLinkAsDevice = %v, %v; want no link and the SDK's refusal of the key", what, issued, err)
				}
				if CRIDNotRequestable(err) {
					t.Errorf("%s: the refusal of the key reads as a CRID the SDK will not ask for: %v", what, err)
				}
			}
			if got := len(server.Requests()); got != 0 {
				t.Errorf("the server answered %d request(s) for a key the SDK cannot use, want none", got)
			}
		})
	}

	// The other order of the same two facts: a key the SDK can use and a
	// CRID it cannot check. That is the CRID's refusal, and not the key's.
	t.Run("usable key, CRID the SDK cannot check", func(t *testing.T) {
		t.Parallel()
		server := qurltest.NewCRIDLinkServer()
		issued, err := sdkServerOpener(t, server).RequestCRIDLinkAsDevice(context.Background(), privateKey, uncheckable)
		if issued != nil || !CRIDNotRequestable(err) || DeviceKeyRefused(err) {
			t.Fatalf("RequestCRIDLinkAsDevice = %v, %v; want no link and the SDK's refusal of the CRID version", issued, err)
		}
		if got := len(server.Requests()); got != 0 {
			t.Errorf("the server answered %d request(s), want none", got)
		}
	})
}

// TestRequestCRIDLinkAsDeviceDoesNotChangeTheKey pins that the key the caller
// passed is the same bytes after the call. The caller wipes it; the opener
// and the SDK do not.
func TestRequestCRIDLinkAsDeviceDoesNotChangeTheKey(t *testing.T) {
	t.Parallel()
	privateKey, publicKey := testDeviceKey(t)
	before := append([]byte{}, privateKey...)
	server := qurltest.NewCRIDLinkServer()
	server.PrivateFor(publicKey)

	if _, err := sdkServerOpener(t, server).RequestCRIDLinkAsDevice(context.Background(), privateKey, server.CRID()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(privateKey, before) {
		t.Fatal("the call changed the key the caller passed")
	}
}

// TestRequestCRIDLinkAsDeviceIsBoundedAndReadsTheSettingsAsTheOtherRequest
// pins what the two requests share: the time limit, and how they find their
// settings.
func TestRequestCRIDLinkAsDeviceIsBoundedAndReadsTheSettingsAsTheOtherRequest(t *testing.T) {
	privateKey, _ := testDeviceKey(t)
	resourceCRID := apitest.GenerateResourceKey(t).CRID

	t.Run("one time limit for the call", func(t *testing.T) {
		doer := &linkRequestDoer{}
		before := time.Now()
		_, err := newLinkDeployment(t).opener(doer).RequestCRIDLinkAsDevice(context.Background(), privateKey, resourceCRID)
		var relay *qurl.RelayError
		if !errors.As(err, &relay) {
			t.Fatalf("RequestCRIDLinkAsDevice = %v, want the SDK's error for a request that got no answer", err)
		}
		if doer.sent() != 1 {
			t.Fatalf("a request was sent %d times, want once: no answer is not \"not found\"", doer.sent())
		}
		if !doer.bounded[0] || doer.deadlines[0].Before(before) || doer.deadlines[0].After(time.Now().Add(cridLinkRequestTimeout)) {
			t.Fatalf("request deadline = %v (set: %t), want one within %s of the call", doer.deadlines[0], doer.bounded[0], cridLinkRequestTimeout)
		}
	})

	t.Run("interrupted", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		doer := &linkRequestDoer{answer: func(req *http.Request) error { return req.Context().Err() }}
		_, err := newLinkDeployment(t).opener(doer).RequestCRIDLinkAsDevice(ctx, privateKey, resourceCRID)
		if got := ClassifyCRIDLinkError(err, true); got != context.Canceled { //nolint:errorlint // The classifier must return the bare sentinel.
			t.Fatalf("ClassifyCRIDLinkError(%v) = %v, want context.Canceled itself", err, got)
		}
	})

	t.Run("settings that name no endpoint", func(t *testing.T) {
		noEndpoint := newLinkDeployment(t)
		noEndpoint.d.CRIDLink = nil
		doer := &linkRequestDoer{}
		_, err := noEndpoint.opener(doer).RequestCRIDLinkAsDevice(context.Background(), privateKey, resourceCRID)
		if !errors.Is(err, qurl.ErrCRIDLinkNotConfigured) || errors.Is(err, qurl.ErrCRIDLinkMisconfigured) || doer.sent() != 0 {
			t.Fatalf("RequestCRIDLinkAsDevice = %v after %d request(s); want the SDK's answer for settings that name no endpoint, and nothing sent", err, doer.sent())
		}
	})

	t.Run("settings file that cannot be read", func(t *testing.T) {
		opener := &AccessOpener{LookupEnv: envMap(map[string]string{qurl.EnvDeploymentPath: t.TempDir() + "/absent.json"})}
		_, err := opener.RequestCRIDLinkAsDevice(context.Background(), privateKey, resourceCRID)
		if !errors.Is(err, ErrAccessNotConfigured) {
			t.Fatalf("RequestCRIDLinkAsDevice = %v, want ErrAccessNotConfigured", err)
		}
	})

	t.Run("HTTP client and no settings file", func(t *testing.T) {
		t.Setenv(qurl.EnvDeploymentPath, "")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		doer := &linkRequestDoer{}
		_, err := (&AccessOpener{LookupEnv: envMap(nil), CRIDLinkHTTPClient: doer}).RequestCRIDLinkAsDevice(ctx, privateKey, resourceCRID)
		if !errors.Is(err, errCRIDLinkClientNeedsSettings) || doer.sent() != 0 {
			t.Fatalf("RequestCRIDLinkAsDevice = %v after %d request(s); want the refusal of a client with no settings file, and nothing sent", err, doer.sent())
		}
	})

	// Under the deployment every release ships, the request as a device is
	// not made either. The context has ended, so a release that does name an
	// endpoint could not send a request from this test.
	t.Run("shipped deployment", func(t *testing.T) {
		t.Setenv(qurl.EnvDeploymentPath, "")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		link, err := (&AccessOpener{LookupEnv: envMap(nil)}).RequestCRIDLinkAsDevice(ctx, privateKey, resourceCRID)
		if link != nil || !errors.Is(err, qurl.ErrCRIDLinkNotConfigured) || errors.Is(err, qurl.ErrCRIDLinkMisconfigured) {
			t.Fatalf("RequestCRIDLinkAsDevice under the shipped deployment = %v, %v; want no link and the SDK's answer for a deployment that names no endpoint", link, err)
		}
	})
}
