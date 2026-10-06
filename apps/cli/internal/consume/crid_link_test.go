package consume

import (
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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

// Tests for the request that asks for a link with only a CRID. The SDK call
// is the real one in every test that makes it. None of them can reach a
// network: either the SDK stops before it sends anything, or the HTTP client
// is a double. There are two doubles. linkRequestDoer, below, answers no
// request, for the tests about what is sent and when. The SDK's own
// qurltest.CRIDLinkServer answers as the service does, for the tests about a
// link that was issued and a request that was refused.

// linkRequestDoer stands in for the HTTP client of a link request. It records
// each request and answers it with err, so nothing leaves the process.
type linkRequestDoer struct {
	mu        sync.Mutex
	hosts     []string
	deadlines []time.Time
	bounded   []bool
	// answer builds the error a request is answered with; nil means a fixed
	// transport failure.
	answer func(*http.Request) error
}

func (d *linkRequestDoer) Do(req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.hosts = append(d.hosts, req.URL.Scheme+"://"+req.URL.Host)
	deadline, bounded := req.Context().Deadline()
	d.deadlines = append(d.deadlines, deadline)
	d.bounded = append(d.bounded, bounded)
	if d.answer != nil {
		return nil, d.answer(req)
	}
	return nil, errors.New("the test double answers no request")
}

func (d *linkRequestDoer) sent() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.hosts)
}

// Fixed values of the deployment files below. They are placeholders: no test
// here resolves or contacts them.
const (
	testLinkRequestHost = "relay.example.test"
	testLinkRequestURL  = "https://" + testLinkRequestHost
	testLinkOrigin      = "https://links.example.test"
)

// linkDeployment is a deployment settings file a test edits before writing.
type linkDeployment struct {
	t *testing.T
	d qurl.Deployment
}

// newLinkDeployment returns settings that can open links and that name a
// usable endpoint for a link request: one trusted key, one cell, the relay on
// the allowlist.
func newLinkDeployment(t *testing.T) *linkDeployment {
	t.Helper()
	signer, err := qurl.GenerateLocalSigner("kid-link-request")
	if err != nil {
		t.Fatalf("generate signer: %v", err)
	}
	der, err := signer.PublicKeyDER()
	if err != nil {
		t.Fatalf("public key DER: %v", err)
	}
	return &linkDeployment{t: t, d: qurl.Deployment{
		Issuers:        []qurl.ManifestIssuer{{Kid: signer.KID(), SPKIDERB64: base64.RawURLEncoding.EncodeToString(der)}},
		Cells:          []qurl.DeploymentCell{testDeploymentCell(t, "cell-a")},
		RelayAllowlist: []string{testLinkRequestHost},
		CRIDLink:       &qurl.DeploymentCRIDLink{RelayURL: testLinkRequestURL, LinkOrigin: testLinkOrigin},
	}}
}

// testDeploymentCell is a cell entry with a freshly generated, usable key.
func testDeploymentCell(t *testing.T, id string) qurl.DeploymentCell {
	t.Helper()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate cell key: %v", err)
	}
	return qurl.DeploymentCell{
		CellID: id, Host: id + ".example.test", Port: 443,
		ServerPublicKeyB64: base64.StdEncoding.EncodeToString(key.PublicKey().Bytes()),
	}
}

// opener writes the settings and returns an opener that reads them through
// its injected environment and sends through doer.
func (l *linkDeployment) opener(doer qurl.HTTPDoer) *AccessOpener {
	l.t.Helper()
	return deploymentOpener(l.t, &l.d, doer)
}

// deploymentOpener writes d as a settings file and returns an opener that
// reads it through its injected environment and sends through doer.
func deploymentOpener(t *testing.T, d *qurl.Deployment, doer qurl.HTTPDoer) *AccessOpener {
	t.Helper()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("encode deployment fixture: %v", err)
	}
	return &AccessOpener{
		LookupEnv:          envMap(map[string]string{qurl.EnvDeploymentPath: writeDeployment(t, string(raw))}),
		CRIDLinkHTTPClient: doer,
	}
}

// msgCRIDLinkSetup is the whole message for settings that name a place to ask
// for a link with only a CRID and name it wrongly.
const msgCRIDLinkSetup = MsgAccessNotConfigured + ` (the settings name a "crid_link" entry that cannot be used)`

// mustBeCRIDLinkSetupError asserts err is the setup fault for an unusable
// "crid_link" entry: the existing "not set up" error, with the detail that
// names the entry and nothing else.
func mustBeCRIDLinkSetupError(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, ErrAccessNotConfigured) || err.Error() != msgCRIDLinkSetup {
		t.Fatalf("%s = %v, want ErrAccessNotConfigured with the message %q", what, err, msgCRIDLinkSetup)
	}
}

// TestRequestCRIDLinkUnderTheShippedDeploymentIsNotMade pins the default every
// user has today: with no deployment file, the SDK's own settings name no
// place to send the request. So the request is not offered, and one that is
// made anyway is not sent.
//
// TODO(upstream-contract): this is a fact about the deployment qurl-go ships.
// When a qurl-go release names an endpoint there, this test fails on the
// version bump. That is the moment `qurl get` starts to behave differently for
// every user, so the bump needs that decision, not an edit here.
//
// The context is already canceled, so even a release that does name an
// endpoint could not send a request from this test.
func TestRequestCRIDLinkUnderTheShippedDeploymentIsNotMade(t *testing.T) {
	t.Setenv(qurl.EnvDeploymentPath, "")
	opener := &AccessOpener{LookupEnv: envMap(nil)}

	if offered, err := opener.CRIDLinkOffered(); offered || err != nil {
		t.Fatalf("CRIDLinkOffered under the shipped deployment = %t, %v; want not offered and no error", offered, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	link, err := opener.RequestCRIDLink(ctx, apitest.GenerateResourceKey(t).CRID)
	if link != nil || !errors.Is(err, qurl.ErrCRIDLinkNotConfigured) || errors.Is(err, qurl.ErrCRIDLinkMisconfigured) {
		t.Fatalf("RequestCRIDLink under the shipped deployment = %v, %v; want no link and the SDK's answer for a deployment that names no endpoint", link, err)
	}
}

// TestRequestCRIDLinkRefusesAnHTTPClientWithNoSettingsFile pins that an HTTP
// client set for the link request is never dropped in silence. Without
// QURL_DEPLOYMENT the request would go through the SDK's own resolution,
// which takes no client from the opener. It would then pass by the double a
// test set to keep requests inside the process. So the opener refuses before
// it calls the SDK: no link, nothing sent, and a fixed error that reads as a
// setup fault and carries no SDK text.
//
// The context is already canceled and QURL_DEPLOYMENT is empty in the
// process too, so a version of the code without the check could not send a
// request from this test either.
func TestRequestCRIDLinkRefusesAnHTTPClientWithNoSettingsFile(t *testing.T) {
	t.Setenv(qurl.EnvDeploymentPath, "")
	const want = MsgAccessNotConfigured + " (an HTTP client was set for the request with only a CRID, and it is used only with a settings file)"
	crid := apitest.GenerateResourceKey(t).CRID
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, lookupEnv := range map[string]func(string) (string, bool){
		"no environment":          nil,
		"QURL_DEPLOYMENT not set": envMap(nil),
		"QURL_DEPLOYMENT blank":   envMap(map[string]string{qurl.EnvDeploymentPath: "  "}),
	} {
		t.Run(name, func(t *testing.T) {
			doer := &linkRequestDoer{}
			opener := &AccessOpener{LookupEnv: lookupEnv, CRIDLinkHTTPClient: doer}

			link, err := opener.RequestCRIDLink(ctx, crid)
			if link != nil || !errors.Is(err, ErrAccessNotConfigured) || err.Error() != want {
				t.Fatalf("RequestCRIDLink = %v, %v; want no link and ErrAccessNotConfigured with the message %q", link, err, want)
			}
			if errors.Is(err, qurl.ErrNotConfigured) || CRIDNotRequestable(err) {
				t.Fatalf("the refusal %v reads as an answer of the SDK, which must not have been called", err)
			}
			// The classifier keeps it: the user would see the fixed message.
			if got := ClassifyCRIDLinkError(err, true); got != err { //nolint:errorlint // The classifier must return this error itself.
				t.Fatalf("ClassifyCRIDLinkError(%v) = %v, want the same error", err, got)
			}
			if sent := doer.sent(); sent != 0 {
				t.Fatalf("a request was sent %d times", sent)
			}
		})
	}

	// With no client set, the same opener asks the SDK as before.
	_, err := (&AccessOpener{LookupEnv: envMap(nil)}).RequestCRIDLink(ctx, crid)
	if !errors.Is(err, qurl.ErrCRIDLinkNotConfigured) {
		t.Fatalf("RequestCRIDLink with no client set = %v, want the SDK's answer for a deployment that names no endpoint", err)
	}
}

// TestCRIDLinkOfferedFromADeploymentFile drives the real SDK check and the
// real SDK call with settings from QURL_DEPLOYMENT, for the three things a
// file can say about the request.
//
// A file that names no endpoint does not offer the request, like the shipped
// deployment. A file that names one that cannot be used is a setup fault,
// and it is reported as one: it must not read as "not offered", or a mistake
// in the file would go unnoticed. A file that names a usable one offers the
// request, and exactly one request is sent, to that endpoint, with a
// deadline.
//
// In the first two cases nothing is sent, by the check or by a request made
// anyway.
func TestCRIDLinkOfferedFromADeploymentFile(t *testing.T) {
	t.Parallel()
	crid := apitest.GenerateResourceKey(t).CRID

	t.Run("not offered/no endpoint, like the shipped deployment", func(t *testing.T) {
		t.Parallel()
		settings := newLinkDeployment(t)
		settings.d.CRIDLink, settings.d.RelayAllowlist = nil, nil
		doer := &linkRequestDoer{}
		opener := settings.opener(doer)

		if offered, err := opener.CRIDLinkOffered(); offered || err != nil {
			t.Fatalf("CRIDLinkOffered = %t, %v; want not offered and no error", offered, err)
		}
		// A caller that asked anyway gets the SDK's "names none" answer. That
		// contradicts a check that said "offered", so it reads as a fault in
		// the settings and never as an answer from the service.
		link, err := opener.RequestCRIDLink(context.Background(), crid)
		if link != nil || !errors.Is(err, qurl.ErrCRIDLinkNotConfigured) || errors.Is(err, qurl.ErrCRIDLinkMisconfigured) {
			t.Fatalf("RequestCRIDLink = %v, %v; want no link and the answer for a file that names no endpoint", link, err)
		}
		if got := ClassifyCRIDLinkError(err, true); !errors.Is(got, ErrAccessNotConfigured) || got.Error() != MsgAccessNotConfigured {
			t.Fatalf("ClassifyCRIDLinkError(%v) = %v, want the bare ErrAccessNotConfigured", err, got)
		}
		if sent := doer.sent(); sent != 0 {
			t.Fatalf("a request that is not offered was sent %d times", sent)
		}
	})

	for name, edit := range map[string]func(d *qurl.Deployment){
		"endpoint host not allowed":        func(d *qurl.Deployment) { d.RelayAllowlist = []string{"other.example.test"} },
		"endpoint address not https":       func(d *qurl.Deployment) { d.CRIDLink.RelayURL = "http://" + testLinkRequestHost },
		"no endpoint address":              func(d *qurl.Deployment) { d.CRIDLink.RelayURL = "" },
		"link origin written with a slash": func(d *qurl.Deployment) { d.CRIDLink.LinkOrigin = testLinkOrigin + "/" },
		"no link origin":                   func(d *qurl.Deployment) { d.CRIDLink.LinkOrigin = "" },
		"more than one cell":               func(d *qurl.Deployment) { d.Cells = append(d.Cells, testDeploymentCell(t, "cell-b")) },
		"no cell":                          func(d *qurl.Deployment) { d.Cells = nil },
	} {
		t.Run("wrong setup/"+name, func(t *testing.T) {
			t.Parallel()
			settings := newLinkDeployment(t)
			edit(&settings.d)
			doer := &linkRequestDoer{}
			opener := settings.opener(doer)

			offered, err := opener.CRIDLinkOffered()
			if offered {
				t.Fatal("CRIDLinkOffered = true for settings that cannot be used")
			}
			mustBeCRIDLinkSetupError(t, "CRIDLinkOffered", err)

			// The request reports the same fault, with the same message, and
			// sends nothing either.
			link, reqErr := opener.RequestCRIDLink(context.Background(), crid)
			if link != nil || !errors.Is(reqErr, qurl.ErrCRIDLinkMisconfigured) {
				t.Fatalf("RequestCRIDLink = %v, %v; want no link and the SDK's answer for an endpoint that cannot be used", link, reqErr)
			}
			mustBeCRIDLinkSetupError(t, "ClassifyCRIDLinkError", ClassifyCRIDLinkError(reqErr, true))
			if sent := doer.sent(); sent != 0 {
				t.Fatalf("a request that cannot be made was sent %d times", sent)
			}
		})
	}

	t.Run("offered", func(t *testing.T) {
		t.Parallel()
		doer := &linkRequestDoer{}
		opener := newLinkDeployment(t).opener(doer)

		if offered, err := opener.CRIDLinkOffered(); !offered || err != nil {
			t.Fatalf("CRIDLinkOffered = %t, %v; want offered", offered, err)
		}
		if sent := doer.sent(); sent != 0 {
			t.Fatalf("the check sent %d request(s), want none: it only reads the settings", sent)
		}

		before := time.Now()
		link, err := opener.RequestCRIDLink(context.Background(), crid)
		if link != nil || err == nil || CRIDNotRequestable(err) {
			t.Fatalf("RequestCRIDLink = %v, %v; want no link and the failure of a request that was made", link, err)
		}
		if doer.sent() != 1 || doer.hosts[0] != testLinkRequestURL {
			t.Fatalf("requests went to %q, want exactly one to %s", doer.hosts, testLinkRequestURL)
		}
		// A request with no deadline would hold the command for as long as the
		// other side stays silent.
		if !doer.bounded[0] || doer.deadlines[0].Before(before) || doer.deadlines[0].After(time.Now().Add(cridLinkRequestTimeout)) {
			t.Fatalf("request deadline = %v (set: %t), want one within %s of the call", doer.deadlines[0], doer.bounded[0], cridLinkRequestTimeout)
		}
		// A request nobody answered is "no answer": try again later.
		if got := ClassifyCRIDLinkError(err, true); !errors.Is(got, ErrCRIDLinkNoAnswer) {
			t.Fatalf("ClassifyCRIDLinkError(%v) = %v, want ErrCRIDLinkNoAnswer", err, got)
		}
	})
}

// TestRequestCRIDLinkReportsAnUnusableSettingsFile pins that a QURL_DEPLOYMENT
// file that cannot be used is reported as such, by the check and by the
// request. It is not "not offered": the user named a file, and it is wrong.
func TestRequestCRIDLinkReportsAnUnusableSettingsFile(t *testing.T) {
	t.Parallel()
	crid := apitest.GenerateResourceKey(t).CRID
	for name, path := range map[string]string{
		"missing file": filepath.Join(t.TempDir(), "absent.json"),
		"not JSON":     writeDeployment(t, "not json"),
		"no issuers":   writeDeployment(t, `{"issuers": [], "cells": [], "relay_allowlist": ["`+testLinkRequestHost+`"]}`),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			doer := &linkRequestDoer{}
			opener := &AccessOpener{LookupEnv: envMap(map[string]string{qurl.EnvDeploymentPath: path}), CRIDLinkHTTPClient: doer}

			if offered, err := opener.CRIDLinkOffered(); offered || !errors.Is(err, ErrAccessNotConfigured) {
				t.Fatalf("CRIDLinkOffered = %t, %v; want not offered and ErrAccessNotConfigured", offered, err)
			}
			link, err := opener.RequestCRIDLink(context.Background(), crid)
			if link != nil || !errors.Is(err, ErrAccessNotConfigured) {
				t.Fatalf("RequestCRIDLink = %v, %v; want no link and ErrAccessNotConfigured", link, err)
			}
			if CRIDNotRequestable(err) {
				t.Fatalf("an unusable settings file read as a CRID the SDK will not ask for: %v", err)
			}
			if got := ClassifyCRIDLinkError(err, true); !errors.Is(got, ErrAccessNotConfigured) {
				t.Fatalf("ClassifyCRIDLinkError(%v) = %v, want ErrAccessNotConfigured kept", err, got)
			}
			if sent := doer.sent(); sent != 0 {
				t.Fatalf("a request was sent %d times with unusable settings", sent)
			}
		})
	}
}

// TestCRIDLinkOfferedWithNoUsableSDKSettings pins the last answer of the
// check: the SDK's own settings cannot be used at all. The SDK's error text
// names a file path, so only the fixed message is kept.
//
// QURL_DEPLOYMENT is set in the process only, so the opener's own lookup does
// not see it and the SDK's resolution does. In production the two are the
// same environment and the opener reports the file first.
func TestCRIDLinkOfferedWithNoUsableSDKSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.json")
	t.Setenv(qurl.EnvDeploymentPath, path)
	opener := &AccessOpener{LookupEnv: envMap(nil)}

	offered, err := opener.CRIDLinkOffered()
	if offered || !errors.Is(err, ErrAccessNotConfigured) || err.Error() != MsgAccessNotConfigured {
		t.Fatalf("CRIDLinkOffered = %t, %v; want not offered and the bare ErrAccessNotConfigured", offered, err)
	}
	if strings.Contains(err.Error(), path) {
		t.Fatalf("the error %q carries the file path", err)
	}
}

// TestRequestCRIDLinkIsNotMadeForACRIDTheSDKCannotCheck pins the answer for a
// well-formed CRID whose version this SDK does not know resources to carry:
// the SDK will not ask for a link for it, and sends nothing.
//
// The SDK gives that answer before it looks at any settings. So the answer
// is the same whether the request is offered or not, and it cannot tell a
// caller which. CRIDLinkOffered can: its answer for each of these openers
// does not depend on the CRID.
func TestRequestCRIDLinkIsNotMadeForACRIDTheSDKCannotCheck(t *testing.T) {
	der := apitest.GenerateResourceKey(t).DER
	doer := &linkRequestDoer{}
	noEndpoint := newLinkDeployment(t)
	noEndpoint.d.CRIDLink = nil

	t.Setenv(qurl.EnvDeploymentPath, "")
	for name, tc := range map[string]struct {
		opener  *AccessOpener
		offered bool
	}{
		"shipped deployment":          {&AccessOpener{LookupEnv: envMap(nil)}, false},
		"file that names an endpoint": {newLinkDeployment(t).opener(doer), true},
		"file that names no endpoint": {noEndpoint.opener(doer), false},
	} {
		// An unregistered version byte, and one that is registered for a
		// different length. Both have a valid checksum.
		for _, version := range []byte{0x05, 0x02} {
			crid := apitest.DeriveCRID(t, der, version)
			link, err := tc.opener.RequestCRIDLink(context.Background(), crid)
			if link != nil || !errors.Is(err, qurl.ErrInvalidResourceRequest) || !errors.Is(err, qurl.ErrUnsupportedCRIDVersion) {
				t.Errorf("%s, version 0x%02x: RequestCRIDLink = %v, %v; want the SDK's refusal of a CRID version it cannot check", name, version, link, err)
			}
			if !CRIDNotRequestable(err) {
				t.Errorf("%s, version 0x%02x: CRIDNotRequestable(%v) = false, want true", name, version, err)
			}
		}
		if offered, err := tc.opener.CRIDLinkOffered(); offered != tc.offered || err != nil {
			t.Errorf("%s: CRIDLinkOffered = %t, %v; want %t and no error", name, offered, err, tc.offered)
		}
	}
	if sent := doer.sent(); sent != 0 {
		t.Fatalf("a request for a CRID the SDK cannot check was sent %d times", sent)
	}
}

// TestRequestCRIDLinkInterrupted pins that a command the user interrupted
// keeps reading as an interrupt, with none of the SDK's text beside it.
func TestRequestCRIDLinkInterrupted(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	doer := &linkRequestDoer{answer: func(req *http.Request) error { return req.Context().Err() }}

	_, err := newLinkDeployment(t).opener(doer).RequestCRIDLink(ctx, apitest.GenerateResourceKey(t).CRID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RequestCRIDLink with a canceled context = %v, want an error that matches context.Canceled", err)
	}
	// Exactly the cancellation, not an error that merely matches it.
	if got := ClassifyCRIDLinkError(err, true); got != context.Canceled { //nolint:errorlint // The classifier must return the bare sentinel.
		t.Fatalf("ClassifyCRIDLinkError(%v) = %v, want context.Canceled itself", err, got)
	}
}

// sdkRefusal builds a refusal the way the SDK does: the typed sentinel and the
// deny that carries the service's code.
func sdkRefusal(sentinel error, code string) error {
	return fmt.Errorf("%w: %w", sentinel, &qurl.ServerDenyError{ErrCode: code})
}

// TestClassifyCRIDLinkError pins the whole mapping from an SDK answer to the
// CLI's fixed messages, one row per answer.
func TestClassifyCRIDLinkError(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		in   error
		want error
	}{
		"not found":                       {sdkRefusal(qurl.ErrCRIDLinkNotFound, "52602"), ErrCRIDNotFound},
		"unavailable":                     {sdkRefusal(qurl.ErrCRIDLinkUnavailable, "52601"), ErrCRIDLinkUnavailable},
		"rate limited":                    {sdkRefusal(qurl.ErrCRIDLinkRateLimited, "52603"), ErrCRIDLinkRateLimited},
		"publisher offline":               {sdkRefusal(qurl.ErrCRIDResourceOffline, "52604"), ErrCRIDPublisherOffline},
		"resource closed":                 {sdkRefusal(qurl.ErrCRIDResourceClosed, "52605"), ErrCRIDResourceClosed},
		"request rejected":                {sdkRefusal(qurl.ErrInvalidCRIDLinkRequest, "52606"), ErrCRIDLinkRequestRejected},
		"busy":                            {qurl.ErrServerOverloaded, ErrAccessBusy},
		"a general code":                  {&qurl.ServerDenyError{ErrCode: "51002"}, ErrCRIDLinkUnavailable},
		"another code outside the set":    {&qurl.ServerDenyError{ErrCode: "52005"}, ErrCRIDLinkUnavailable},
		"no answer":                       {&qurl.RelayError{Status: 0, Msg: "relay POST failed"}, ErrCRIDLinkNoAnswer},
		"HTTP error":                      {&qurl.RelayError{Status: 502, Msg: "relay POST -> 502"}, ErrCRIDLinkNoAnswer},
		"timed out":                       {fmt.Errorf("did not complete: %w: %w", context.DeadlineExceeded, &qurl.RelayError{Msg: "relay POST failed"}), ErrCRIDLinkNoAnswer},
		"timed out, bare":                 {context.DeadlineExceeded, ErrCRIDLinkNoAnswer},
		"interrupted":                     {fmt.Errorf("did not complete: %w: %w", context.Canceled, &qurl.RelayError{Msg: "relay POST failed"}), context.Canceled},
		"rejected link":                   {&qurl.CRIDLinkRejectedError{Class: qurl.CRIDLinkRejectOrigin}, ErrCRIDLinkRefused},
		"protocol violation":              {fmt.Errorf("%w: the reply carries a success code", qurl.ErrCRIDLinkProtocol), ErrCRIDLinkRefused},
		"malformed reply":                 {fmt.Errorf("%w: unexpected reply type", qurl.ErrMalformedReply), ErrCRIDLinkRefused},
		"reply does not prove its source": {errors.New("decrypt reply: message authentication failed"), ErrCRIDLinkRefused},
		"settings file":                   {fmt.Errorf("%w (%w)", ErrAccessNotConfigured, os.ErrNotExist), ErrAccessNotConfigured},
		"no deployment settings":          {qurl.ErrNoDeployment, ErrAccessNotConfigured},
		"endpoint that cannot be used":    {fmt.Errorf("%w: relay URL: %w", qurl.ErrCRIDLinkMisconfigured, qurl.ErrRelayURL), ErrAccessNotConfigured},
		"no endpoint after all":           {fmt.Errorf("%w: the configuration names none", qurl.ErrCRIDLinkNotConfigured), ErrAccessNotConfigured},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, deviceIdentity := range []bool{true, false} {
				got := ClassifyCRIDLinkError(tc.in, deviceIdentity)
				if !errors.Is(got, tc.want) {
					t.Fatalf("ClassifyCRIDLinkError(%v, %t) = %v, want %v", tc.in, deviceIdentity, got, tc.want)
				}
				// The mapping is one to one: an answer must not also read as
				// another answer's message.
				for _, other := range cridLinkSentinels() {
					if !errors.Is(tc.want, other) && errors.Is(got, other) {
						t.Fatalf("ClassifyCRIDLinkError(%v, %t) = %v, which also matches %v", tc.in, deviceIdentity, got, other)
					}
				}
			}
		})
	}

	if got := ClassifyCRIDLinkError(nil, true); got != nil {
		t.Errorf("ClassifyCRIDLinkError(nil) = %v, want nil", got)
	}
}

// cridLinkSentinels lists every answer the classifier can give.
func cridLinkSentinels() []error {
	return []error{
		ErrCRIDNotFound, ErrCRIDLinkUnavailable, ErrCRIDLinkNoAnswer, ErrCRIDLinkRateLimited,
		ErrCRIDPublisherOffline, ErrCRIDResourceClosed, ErrCRIDLinkRequestRejected,
		ErrCRIDLinkRefused, ErrAccessBusy, ErrAccessNotConfigured,
		context.Canceled,
	}
}

// TestClassifyCRIDLinkErrorOrder pins the orderings the SDK's error shapes
// make necessary.
func TestClassifyCRIDLinkErrorOrder(t *testing.T) {
	t.Parallel()
	// A refused link also matches the sentinel of the check it failed. It must
	// read as refused, not as that check's own outcome.
	for _, also := range []error{qurl.ErrCRIDMismatch, qurl.ErrUnknownKID, qurl.ErrSignature, qurl.ErrFragment} {
		refused := errors.Join(&qurl.CRIDLinkRejectedError{Class: qurl.CRIDLinkRejectIssuerSignature}, also)
		if got := ClassifyCRIDLinkError(refused, true); !errors.Is(got, ErrCRIDLinkRefused) {
			t.Errorf("a refused link that also matches %v classified as %v, want ErrCRIDLinkRefused", also, got)
		}
	}
	// Every typed refusal is also a deny that carries a code. The typed
	// meaning must win over the one a code outside the set gets.
	typed := sdkRefusal(qurl.ErrCRIDLinkRateLimited, "52603")
	if got := ClassifyCRIDLinkError(typed, true); errors.Is(got, ErrCRIDLinkUnavailable) || !errors.Is(got, ErrCRIDLinkRateLimited) {
		t.Errorf("a typed refusal classified as %v, want ErrCRIDLinkRateLimited", got)
	}
	// An endpoint that cannot be used also matches the SDK's "names none"
	// error and its plain "not configured" one. The detail that says which
	// entry is wrong must survive.
	unusable := fmt.Errorf("%w: relay URL: %w", qurl.ErrCRIDLinkMisconfigured, qurl.ErrRelayURL)
	mustBeCRIDLinkSetupError(t, "an endpoint that cannot be used", ClassifyCRIDLinkError(unusable, true))
}

// TestClassifyCRIDLinkErrorReadsAGeneralCodeAsTryAgainLater pins what the
// user is told when the service refuses with a code outside the six this
// request defines. The service answers with a general code when the part of
// it that was asked does not serve this request, for example before it has
// the request at all or after it was rolled back. Then the advice is to try
// again later. "Update qurl" would be wrong advice, because nothing is wrong
// with this client.
//
// The one code that does ask for an update is the service refusing the
// request itself as invalid, and it keeps that message.
func TestClassifyCRIDLinkErrorReadsAGeneralCodeAsTryAgainLater(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"51002", "52005", "52607", "59999", "7"} {
		for _, deviceIdentity := range []bool{true, false} {
			got := ClassifyCRIDLinkError(&qurl.ServerDenyError{ErrCode: code}, deviceIdentity)
			if !errors.Is(got, ErrCRIDLinkUnavailable) || got.Error() != MsgCRIDLinkUnavailable {
				t.Errorf("code %s classified as %v, want ErrCRIDLinkUnavailable with its message", code, got)
			}
			if strings.Contains(got.Error(), "update") || strings.Contains(got.Error(), code) {
				t.Errorf("code %s: message %q advises an update or carries the code", code, got.Error())
			}
		}
	}

	rejected := ClassifyCRIDLinkError(sdkRefusal(qurl.ErrInvalidCRIDLinkRequest, "52606"), true)
	if !errors.Is(rejected, ErrCRIDLinkRequestRejected) || !strings.Contains(rejected.Error(), "update qurl") {
		t.Errorf("an invalid-request refusal classified as %v, want ErrCRIDLinkRequestRejected and its update advice", rejected)
	}
}

// TestClassifyCRIDLinkErrorForSettingsTheSDKCannotUse pins the answer for the
// SDK's "not configured" errors: it has no deployment settings to check a
// link with, or the settings name no place to send the request although the
// check before the request said they did. Each reads as ErrAccessNotConfigured
// itself. That is the fixed message with no detail, so nothing of the SDK's
// text, which names files and variables, comes with it.
func TestClassifyCRIDLinkErrorForSettingsTheSDKCannotUse(t *testing.T) {
	t.Parallel()
	const detail = "read deployment /etc/example/deployment.json"
	for name, in := range map[string]error{
		"not configured":         fmt.Errorf("%w: %s", qurl.ErrNotConfigured, detail),
		"no deployment settings": qurl.ErrNoDeployment,
		"names no endpoint":      fmt.Errorf("%w: %s", qurl.ErrCRIDLinkNotConfigured, detail),
	} {
		if !errors.Is(in, qurl.ErrNotConfigured) {
			t.Fatalf("%s: the input %v is not one of the SDK's not-configured errors", name, in)
		}
		for _, deviceIdentity := range []bool{true, false} {
			got := ClassifyCRIDLinkError(in, deviceIdentity)
			if got != ErrAccessNotConfigured { //nolint:errorlint // The classifier must return the bare sentinel.
				t.Errorf("%s: ClassifyCRIDLinkError(%v, %t) = %v, want ErrAccessNotConfigured itself", name, in, deviceIdentity, got)
			}
		}
	}
}

// TestClassifyCRIDLinkErrorNotFoundDependsOnlyOnTheDevice pins the one place
// the device identity matters: which of the two not-found sentinels is
// returned. Both are the same answer with the same text.
func TestClassifyCRIDLinkErrorNotFoundDependsOnlyOnTheDevice(t *testing.T) {
	t.Parallel()
	notFound := sdkRefusal(qurl.ErrCRIDLinkNotFound, "52602")

	withDevice := ClassifyCRIDLinkError(notFound, true)
	if !errors.Is(withDevice, ErrCRIDNotFound) || errors.Is(withDevice, ErrCRIDNotFoundNoDevice) {
		t.Errorf("with a device identity: %v, want ErrCRIDNotFound and not the no-device form", withDevice)
	}
	noDevice := ClassifyCRIDLinkError(notFound, false)
	if !errors.Is(noDevice, ErrCRIDNotFoundNoDevice) || !errors.Is(noDevice, ErrCRIDNotFound) {
		t.Errorf("with no device identity: %v, want ErrCRIDNotFoundNoDevice, which also matches ErrCRIDNotFound", noDevice)
	}
	if withDevice.Error() != noDevice.Error() || withDevice.Error() != MsgCRIDNotFound {
		t.Errorf("not-found text differs by device: %q and %q, want %q for both", withDevice.Error(), noDevice.Error(), MsgCRIDNotFound)
	}
}

// TestClassifyCRIDLinkErrorKeepsNoSDKText pins that nothing the SDK or the
// service wrote reaches the message: SDK errors name the address a request
// was sent to and quote codes.
func TestClassifyCRIDLinkErrorKeepsNoSDKText(t *testing.T) {
	t.Parallel()
	const secret = "https://endpoint.example.test/path/0123456789abcdef"
	for _, in := range []error{
		&qurl.RelayError{Status: 502, Msg: "relay POST " + secret + " -> 502"},
		fmt.Errorf("did not complete: %w: %w", context.Canceled, &qurl.RelayError{Msg: "relay POST " + secret + " failed"}),
		fmt.Errorf("%w: %s", qurl.ErrCRIDLinkProtocol, secret),
		errors.New("decrypt reply from " + secret),
		sdkRefusal(qurl.ErrCRIDLinkNotFound, "52602"),
		&qurl.ServerDenyError{ErrCode: "51002"},
		fmt.Errorf("%w: relay URL %s: %w", qurl.ErrCRIDLinkMisconfigured, secret, qurl.ErrRelayURL),
	} {
		got := strings.ToLower(ClassifyCRIDLinkError(in, true).Error())
		for _, leaked := range []string{secret, "endpoint.example.test", "52602", "51002", "errcode", "relay", "qurl:"} {
			if strings.Contains(got, strings.ToLower(leaked)) {
				t.Errorf("ClassifyCRIDLinkError(%v) = %q, which carries %q", in, got, leaked)
			}
		}
	}
}

// TestCRIDNotRequestable pins exactly which SDK answers mean "the SDK will
// not ask for a link for this CRID", and that near misses do not. An answer
// about the deployment is never one of them: CRIDLinkOffered reports those.
func TestCRIDNotRequestable(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"CRID the SDK cannot check": {fmt.Errorf("%w: %w", qurl.ErrInvalidResourceRequest, qurl.ErrUnsupportedCRIDVersion), true},
		"CRID that fails the gate":  {fmt.Errorf("%w: a valid CRID is required", qurl.ErrInvalidResourceRequest), true},

		"no error":                     {nil, false},
		"no endpoint":                  {qurl.ErrCRIDLinkNotConfigured, false},
		"no endpoint, with cause":      {fmt.Errorf("%w: the configuration names none", qurl.ErrCRIDLinkNotConfigured), false},
		"endpoint that cannot be used": {fmt.Errorf("%w: relay URL: %w", qurl.ErrCRIDLinkMisconfigured, qurl.ErrRelayURL), false},
		"no deployment settings":       {qurl.ErrNotConfigured, false},
		"no issuer keys":               {qurl.ErrNoDeployment, false},
		"settings file":                {ErrAccessNotConfigured, false},
		"not found":                    {sdkRefusal(qurl.ErrCRIDLinkNotFound, "52602"), false},
		"unavailable":                  {sdkRefusal(qurl.ErrCRIDLinkUnavailable, "52601"), false},
		"a general code":               {&qurl.ServerDenyError{ErrCode: "51002"}, false},
		"no answer":                    {&qurl.RelayError{Msg: "relay POST failed"}, false},
		"rejected link":                {&qurl.CRIDLinkRejectedError{Class: qurl.CRIDLinkRejectCRIDMismatch}, false},
		"version alone":                {qurl.ErrUnsupportedCRIDVersion, false},
		"interrupted":                  {context.Canceled, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := CRIDNotRequestable(tc.err); got != tc.want {
				t.Errorf("CRIDNotRequestable(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

// TestCRIDLinkRefusalCode pins the one piece of the service's answer a
// diagnostic may show: the refusal code, and only when it is a short run of
// decimal digits.
func TestCRIDLinkRefusalCode(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"refusal the request defines": {sdkRefusal(qurl.ErrCRIDLinkNotFound, "52602"), "52602"},
		"general code":                {&qurl.ServerDenyError{ErrCode: "51002"}, "51002"},
		"wrapped":                     {fmt.Errorf("request failed: %w", &qurl.ServerDenyError{ErrCode: "51002"}), "51002"},
		"longest code shown":          {&qurl.ServerDenyError{ErrCode: strings.Repeat("9", maxRefusalCodeDigits)}, strings.Repeat("9", maxRefusalCodeDigits)},

		"no error":             {nil, ""},
		"no code":              {&qurl.ServerDenyError{}, ""},
		"too long":             {&qurl.ServerDenyError{ErrCode: strings.Repeat("9", maxRefusalCodeDigits+1)}, ""},
		"not digits":           {&qurl.ServerDenyError{ErrCode: "https://endpoint.example.test/#fragment"}, ""},
		"digits, then text":    {&qurl.ServerDenyError{ErrCode: "51002 endpoint.example.test"}, ""},
		"digits and a break":   {&qurl.ServerDenyError{ErrCode: "51002\n52600"}, ""},
		"signed":               {&qurl.ServerDenyError{ErrCode: "-51002"}, ""},
		"not a refusal":        {&qurl.RelayError{Status: 502, Msg: "relay POST -> 502"}, ""},
		"an error with digits": {errors.New("51002"), ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			code, ok := CRIDLinkRefusalCode(tc.err)
			if code != tc.want || ok != (tc.want != "") {
				t.Errorf("CRIDLinkRefusalCode(%v) = %q, %t; want %q", tc.err, code, ok, tc.want)
			}
		})
	}
}

// TestCRIDLinkConfigCopiesTheEndpoint pins the one thing cridLinkConfig adds
// to the opener settings, and that it adds nothing for a file with no
// endpoint.
func TestCRIDLinkConfigCopiesTheEndpoint(t *testing.T) {
	t.Parallel()
	settings := newLinkDeployment(t)

	cfg, err := cridLinkConfig(&settings.d)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CRIDLink == nil || cfg.CRIDLink.RelayURL != testLinkRequestURL || cfg.CRIDLink.LinkOrigin != testLinkOrigin || cfg.CRIDLink.UserAgent != "" {
		t.Fatalf("endpoint = %+v, want the file's two values and no user agent", cfg.CRIDLink)
	}
	if cfg.TrustStore == nil || cfg.Cells == nil || cfg.RelayAllowlist == nil {
		t.Fatalf("config = %+v, want the opener settings kept", cfg)
	}

	settings.d.CRIDLink = nil
	if cfg, err = cridLinkConfig(&settings.d); err != nil || cfg.CRIDLink != nil {
		t.Fatalf("a file with no endpoint gave %+v, %v; want none", cfg.CRIDLink, err)
	}
}
