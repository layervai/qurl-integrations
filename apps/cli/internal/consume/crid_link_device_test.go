package consume

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"net/http"
	"sync"
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
//
// The device key is not passed to the request. A function that reads it is,
// and these tests count how often the SDK calls it.

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

// deviceKeyReads is the function a caller passes to RequestCRIDLinkAsDevice
// to read the device key. It counts its calls and keeps what it returned.
// The SDK calls it on a goroutine of its own, so the record has a lock.
type deviceKeyReads struct {
	// key is what a read returns: a new copy on every call, because the SDK
	// wipes what it is given. err, when set, is returned in its place.
	key []byte
	err error

	mu       sync.Mutex
	returned [][]byte
	bounded  []bool
}

func readsOf(key []byte) *deviceKeyReads { return &deviceKeyReads{key: key} }

func (r *deviceKeyReads) read(ctx context.Context) ([]byte, error) {
	_, bounded := ctx.Deadline()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bounded = append(r.bounded, bounded)
	if r.err != nil {
		r.returned = append(r.returned, nil)
		return nil, r.err
	}
	var key []byte
	if r.key != nil {
		key = bytes.Clone(r.key)
	}
	r.returned = append(r.returned, key)
	return key, nil
}

// count returns how often the key was read.
func (r *deviceKeyReads) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.returned)
}

// mustHaveWipedEveryKey fails unless every slice a read returned holds only
// zeros now. Call it when the request has returned.
func (r *deviceKeyReads) mustHaveWipedEveryKey(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, key := range r.returned {
		if !bytes.Equal(key, make([]byte, len(key))) {
			t.Errorf("the key of read %d was not wiped when the request returned", i+1)
		}
	}
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
// for each kind of resource, through the real SDK, and when it reads the
// device key: a random key first, the device key only after "not found".
//
//   - A public resource is answered by the first request. The device key is
//     not read.
//   - A private resource this device may open costs two requests. The key is
//     read once, only the second request is sent under it, and its answer is
//     the link.
//   - A private resource this device may not open costs two requests and one
//     read too, and the answer is "not found".
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
		reads := readsOf(privateKey)

		issued, err := opener.RequestCRIDLinkAsDevice(context.Background(), reads.read, server.CRID())
		mustBeTheIssuedLink(t, opener, server, issued, err)
		if requests := server.Requests(); len(requests) != 1 || requests[0].CRID != server.CRID() || requests[0].UserAgent != "" {
			t.Errorf("the server answered %+v, want one request for the CRID with no user agent", requests)
		}
		if got := reads.count(); got != 0 {
			t.Errorf("the device key was read %d times for a public resource, want never", got)
		}
	})

	t.Run("private resource of this device", func(t *testing.T) {
		t.Parallel()
		server := qurltest.NewCRIDLinkServer()
		server.PrivateFor(publicKey)
		opener := sdkServerOpener(t, server)
		reads := readsOf(privateKey)

		issued, err := opener.RequestCRIDLinkAsDevice(context.Background(), reads.read, server.CRID())
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
		if got := reads.count(); got != 1 {
			t.Errorf("the device key was read %d times, want once", got)
		}
		// The read is inside the time limit of the request.
		if !reads.bounded[0] {
			t.Error("the read of the device key got a context with no time limit, want the context of the request")
		}
		// The SDK wiped the key the read gave it. The caller's own copy is
		// as it was.
		reads.mustHaveWipedEveryKey(t)
		if bytes.Equal(privateKey, make([]byte, len(privateKey))) {
			t.Error("the request changed the key the reads are made from")
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
		reads := readsOf(privateKey)

		issued, err := opener.RequestCRIDLinkAsDevice(context.Background(), reads.read, server.CRID())
		if issued != nil || !errors.Is(err, qurl.ErrCRIDLinkNotFound) {
			t.Fatalf("RequestCRIDLinkAsDevice = %v, %v; want no link and qurl.ErrCRIDLinkNotFound", issued, err)
		}
		if DeviceKeyNotGiven(err) || AnswerWithoutDeviceKey(err) != nil || CRIDNotRequestable(err) || DeviceKeyRefused(err) {
			t.Fatalf("an answer of the server read as a request that was not made: %v", err)
		}
		if got := ClassifyCRIDLinkError(err, true); !errors.Is(got, ErrCRIDNotFound) || errors.Is(got, ErrCRIDNotFoundNoDevice) {
			t.Errorf("classified as %v, want ErrCRIDNotFound with the hint for a device", got)
		}
		// Two requests, and no third. One read, for the second request.
		if requests := server.Requests(); len(requests) != 2 {
			t.Errorf("the server answered %d request(s), want two", len(requests))
		}
		if got := reads.count(); got != 1 {
			t.Errorf("the device key was read %d times, want once", got)
		}
		reads.mustHaveWipedEveryKey(t)
	})
}

// TestRequestCRIDLinkAsDeviceSendsNoSecondRequestAfterAnotherRefusal pins the
// other half of the rule: only "not found" leads to the request under the
// device key. Every other refusal of the first request is the answer, and
// the device key is not read.
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
			reads := readsOf(privateKey)

			issued, err := sdkServerOpener(t, server).RequestCRIDLinkAsDevice(context.Background(), reads.read, server.CRID())
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
			// One read for each request under the device key, and no other.
			if got, want := reads.count(), tc.wantRequests-1; got != want {
				t.Errorf("the device key was read %d times, want %d", got, want)
			}
		})
	}
}

// TestRequestCRIDLinkAsDeviceWhenTheReadGivesNoKey pins the answer for a
// read that gives no key the SDK can use: an error, no bytes, too few or too
// many bytes, or only zero bytes as a wiped key holds. The SDK asks for the
// key only after the first request was answered "not found", so one request
// was sent, and nothing under a device key.
//
// The answer is DeviceKeyNotGiven, and it keeps the answer to the first
// request: "not found", as the request with only the CRID gets it. It is
// never CRIDNotRequestable. A caller reads that one as "this client cannot
// ask for this CRID" and tells the user that the CRID is the problem.
func TestRequestCRIDLinkAsDeviceWhenTheReadGivesNoKey(t *testing.T) {
	t.Parallel()
	privateKey, publicKey := testDeviceKey(t)
	errNoState := errors.New("this device holds no key")
	for name, reads := range map[string]*deviceKeyReads{
		"the read fails":  {err: errNoState},
		"no key":          {},
		"empty key":       {key: []byte{}},
		"31 bytes":        {key: privateKey[:31]},
		"33 bytes":        {key: append(append([]byte{}, privateKey...), 1)},
		"only zero bytes": {key: make([]byte, 32)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := qurltest.NewCRIDLinkServer()
			// The device may open the resource. With a key it would get the
			// link, so "no link" below is the missing key.
			server.PrivateFor(publicKey)
			opener := sdkServerOpener(t, server)

			issued, err := opener.RequestCRIDLinkAsDevice(context.Background(), reads.read, server.CRID())
			if issued != nil || !DeviceKeyNotGiven(err) {
				t.Fatalf("RequestCRIDLinkAsDevice = %v, %v; want no link and the answer for a read that gave no key", issued, err)
			}
			answer := AnswerWithoutDeviceKey(err)
			if CRIDNotRequestable(err) {
				t.Errorf("a read that gave no key reads as a CRID the SDK will not ask for: %v", err)
			}
			// The error itself is not "not found": nobody asked as the
			// device. The answer it keeps is the one of the first request.
			if errors.Is(err, qurl.ErrCRIDLinkNotFound) {
				t.Errorf("the error %v matches qurl.ErrCRIDLinkNotFound itself", err)
			}
			if got := ClassifyCRIDLinkError(answer, true); !errors.Is(got, ErrCRIDNotFound) {
				t.Errorf("the answer to the first request is %v, classified as %v; want ErrCRIDNotFound", answer, got)
			}
			if code, ok := CRIDLinkRefusalCode(answer); !ok || code != "52602" {
				t.Errorf("CRIDLinkRefusalCode of the answer = %q, %t; want 52602 for the diagnostic line", code, ok)
			}
			// A failed read keeps its own error as the cause. Bytes that are
			// not a key are the SDK's refusal of a key.
			if wantCause := reads.err != nil; errors.Is(err, errNoState) != wantCause || DeviceKeyRefused(err) == wantCause {
				t.Errorf("error %v: matches the error of the read = %t, DeviceKeyRefused = %t", err, errors.Is(err, errNoState), DeviceKeyRefused(err))
			}

			requests := server.Requests()
			if len(requests) != 1 || requests[0].AsDevice {
				t.Errorf("the server answered %+v, want one request, not under a device key", requests)
			}
			if got := reads.count(); got != 1 {
				t.Errorf("the device key was read %d times, want once", got)
			}
			reads.mustHaveWipedEveryKey(t)
		})
	}

	// The same reads, for a public resource: the first request gives the
	// link, and the key is not read. So a key that cannot be read costs a
	// public resource nothing.
	t.Run("public resource, the read would fail", func(t *testing.T) {
		t.Parallel()
		server := qurltest.NewCRIDLinkServer()
		opener := sdkServerOpener(t, server)
		reads := &deviceKeyReads{err: errNoState}
		issued, err := opener.RequestCRIDLinkAsDevice(context.Background(), reads.read, server.CRID())
		mustBeTheIssuedLink(t, opener, server, issued, err)
		if got := reads.count(); got != 0 {
			t.Errorf("the device key was read %d times for a public resource, want never", got)
		}
	})
}

// TestAnswerWithoutDeviceKeyWhenThereIsNoAnswer pins the two cases in which
// AnswerWithoutDeviceKey has no answer to return, and returns nil. A caller
// treats nil as no answer at all, and fails closed.
//
//   - The error says that the read gave no key, and it holds no answer to the
//     first request. The SDK does not build such an error today.
//   - The error is of another kind, or there is none.
func TestAnswerWithoutDeviceKeyWhenThereIsNoAnswer(t *testing.T) {
	t.Parallel()
	errNoState := errors.New("this device holds no key")

	noFirstAnswer := &qurl.DeviceKeySourceError{Err: errNoState}
	if !DeviceKeyNotGiven(noFirstAnswer) {
		t.Fatalf("DeviceKeyNotGiven(%v) = false, want true", noFirstAnswer)
	}
	if got := AnswerWithoutDeviceKey(noFirstAnswer); got != nil {
		t.Errorf("AnswerWithoutDeviceKey of an error with no first answer = %v, want nil", got)
	}
	// The cause is not the answer. An error of the read must never be handed
	// on as what the service said.
	if got := AnswerWithoutDeviceKey(&qurl.DeviceKeySourceError{Err: qurl.ErrCRIDLinkNotFound}); got != nil {
		t.Errorf("AnswerWithoutDeviceKey returned the cause %v as the answer, want nil", got)
	}

	for name, err := range map[string]error{
		"no error":         nil,
		"another error":    errNoState,
		"the SDK's answer": sdkRefusal(qurl.ErrCRIDLinkNotFound, "52602"),
	} {
		if got := AnswerWithoutDeviceKey(err); got != nil {
			t.Errorf("%s: AnswerWithoutDeviceKey = %v, want nil", name, got)
		}
	}
}

// TestRequestCRIDLinkAsDeviceIsRefusedBeforeAnythingIsSent pins the two
// answers the SDK gives before it sends anything, and their order.
//
// No function to read the key with is DeviceKeyRefused, for every CRID: a
// request "as a device" must not turn into the request with only the CRID.
// It is never CRIDNotRequestable, not even for a CRID the SDK would refuse
// too. A CRID the SDK cannot check, with a function to read the key, is
// CRIDNotRequestable, and the key is not read for it.
func TestRequestCRIDLinkAsDeviceIsRefusedBeforeAnythingIsSent(t *testing.T) {
	t.Parallel()
	privateKey, publicKey := testDeviceKey(t)
	uncheckable := apitest.DeriveCRID(t, apitest.GenerateResourceKey(t).DER, 0x05)

	t.Run("no function to read the key with", func(t *testing.T) {
		t.Parallel()
		server := qurltest.NewCRIDLinkServer()
		server.PrivateFor(publicKey)
		opener := sdkServerOpener(t, server)
		for what, resourceCRID := range map[string]string{"a CRID the server holds": server.CRID(), "a CRID the SDK cannot check": uncheckable} {
			issued, err := opener.RequestCRIDLinkAsDevice(context.Background(), nil, resourceCRID)
			if issued != nil || !DeviceKeyRefused(err) {
				t.Errorf("%s: RequestCRIDLinkAsDevice = %v, %v; want no link and the SDK's refusal", what, issued, err)
			}
			if DeviceKeyNotGiven(err) || AnswerWithoutDeviceKey(err) != nil || CRIDNotRequestable(err) {
				t.Errorf("%s: the refusal reads as another answer: %v", what, err)
			}
		}
		if got := len(server.Requests()); got != 0 {
			t.Errorf("the server answered %d request(s), want none", got)
		}
	})

	t.Run("a CRID the SDK cannot check", func(t *testing.T) {
		t.Parallel()
		server := qurltest.NewCRIDLinkServer()
		reads := readsOf(privateKey)
		issued, err := sdkServerOpener(t, server).RequestCRIDLinkAsDevice(context.Background(), reads.read, uncheckable)
		if issued != nil || !CRIDNotRequestable(err) || DeviceKeyRefused(err) {
			t.Fatalf("RequestCRIDLinkAsDevice = %v, %v; want no link and the SDK's refusal of the CRID version", issued, err)
		}
		if DeviceKeyNotGiven(err) {
			t.Fatalf("the refusal of the CRID reads as a read that gave no key: %v", err)
		}
		if got := len(server.Requests()); got != 0 {
			t.Errorf("the server answered %d request(s), want none", got)
		}
		if got := reads.count(); got != 0 {
			t.Errorf("the device key was read %d times for a request that was not sent, want never", got)
		}
	})
}

// TestRequestCRIDLinkAsDeviceIsBoundedAndReadsTheSettingsAsTheOtherRequest
// pins what the two requests share: the time limit, and how they find their
// settings. In none of these cases is the device key read: no request is
// answered "not found".
func TestRequestCRIDLinkAsDeviceIsBoundedAndReadsTheSettingsAsTheOtherRequest(t *testing.T) {
	privateKey, _ := testDeviceKey(t)
	resourceCRID := apitest.GenerateResourceKey(t).CRID
	reads := readsOf(privateKey)
	t.Cleanup(func() {
		if got := reads.count(); got != 0 {
			t.Errorf("the device key was read %d times, want never: no request was answered \"not found\"", got)
		}
	})

	t.Run("one time limit for the call", func(t *testing.T) {
		doer := &linkRequestDoer{}
		before := time.Now()
		_, err := newLinkDeployment(t).opener(doer).RequestCRIDLinkAsDevice(context.Background(), reads.read, resourceCRID)
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
		_, err := newLinkDeployment(t).opener(doer).RequestCRIDLinkAsDevice(ctx, reads.read, resourceCRID)
		if got := ClassifyCRIDLinkError(err, true); got != context.Canceled { //nolint:errorlint // The classifier must return the bare sentinel.
			t.Fatalf("ClassifyCRIDLinkError(%v) = %v, want context.Canceled itself", err, got)
		}
	})

	t.Run("settings that name no endpoint", func(t *testing.T) {
		noEndpoint := newLinkDeployment(t)
		noEndpoint.d.CRIDLink = nil
		doer := &linkRequestDoer{}
		_, err := noEndpoint.opener(doer).RequestCRIDLinkAsDevice(context.Background(), reads.read, resourceCRID)
		if !errors.Is(err, qurl.ErrCRIDLinkNotConfigured) || errors.Is(err, qurl.ErrCRIDLinkMisconfigured) || doer.sent() != 0 {
			t.Fatalf("RequestCRIDLinkAsDevice = %v after %d request(s); want the SDK's answer for settings that name no endpoint, and nothing sent", err, doer.sent())
		}
	})

	t.Run("settings file that cannot be read", func(t *testing.T) {
		opener := &AccessOpener{LookupEnv: envMap(map[string]string{qurl.EnvDeploymentPath: t.TempDir() + "/absent.json"})}
		_, err := opener.RequestCRIDLinkAsDevice(context.Background(), reads.read, resourceCRID)
		if !errors.Is(err, ErrAccessNotConfigured) {
			t.Fatalf("RequestCRIDLinkAsDevice = %v, want ErrAccessNotConfigured", err)
		}
	})

	t.Run("HTTP client and no settings file", func(t *testing.T) {
		t.Setenv(qurl.EnvDeploymentPath, "")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		doer := &linkRequestDoer{}
		_, err := (&AccessOpener{LookupEnv: envMap(nil), CRIDLinkHTTPClient: doer}).RequestCRIDLinkAsDevice(ctx, reads.read, resourceCRID)
		if !errors.Is(err, errCRIDLinkClientNeedsSettings) || doer.sent() != 0 {
			t.Fatalf("RequestCRIDLinkAsDevice = %v after %d request(s); want the refusal of a client with no settings file, and nothing sent", err, doer.sent())
		}
	})

	// Where no settings file is named, the SDK finds its own settings, and
	// the request is still the SDK's request as a device: it refuses a call
	// that gives it no function to read the key with, before it reads any
	// settings. The request with only the CRID has no such refusal.
	t.Run("no settings file, and no function to read the key with", func(t *testing.T) {
		t.Setenv(qurl.EnvDeploymentPath, "")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		issued, err := (&AccessOpener{LookupEnv: envMap(nil)}).RequestCRIDLinkAsDevice(ctx, nil, resourceCRID)
		if issued != nil || !DeviceKeyRefused(err) || CRIDNotRequestable(err) {
			t.Fatalf("RequestCRIDLinkAsDevice = %v, %v; want no link and the SDK's refusal", issued, err)
		}
	})

	// Under the deployment every release ships, the request as a device is
	// not made either. The context has ended, so a release that does name an
	// endpoint could not send a request from this test.
	t.Run("shipped deployment", func(t *testing.T) {
		t.Setenv(qurl.EnvDeploymentPath, "")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		link, err := (&AccessOpener{LookupEnv: envMap(nil)}).RequestCRIDLinkAsDevice(ctx, reads.read, resourceCRID)
		if link != nil || !errors.Is(err, qurl.ErrCRIDLinkNotConfigured) || errors.Is(err, qurl.ErrCRIDLinkMisconfigured) {
			t.Fatalf("RequestCRIDLinkAsDevice under the shipped deployment = %v, %v; want no link and the SDK's answer for a deployment that names no endpoint", link, err)
		}
	})
}
