package qurlapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

// Fixture values of the access-request tests. No person or device has them.
const (
	testRequestCode  = "482913"
	testOtherCode    = "175306"
	testDeviceID     = "abcd-efgh-2345-mnop"
	testOtherDevice  = "qrst-uvwx-yz67-abcd"
	testRequester    = "Ana Lopez"
	testOtherPerson  = "Sam Okafor"
	unknownTestCRID  = "qaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	unsupportedStart = "this service does not offer access requests yet"
)

// requestBodies returns the bodies of the requests the mock received for one
// method and path.
func requestBodies(srv *apitest.Server, method, path string) []string {
	var bodies []string
	for _, request := range srv.Requests() {
		if request.Method == method && request.Path == path {
			bodies = append(bodies, string(request.Body))
		}
	}
	return bodies
}

// requestLines returns "METHOD path" for every request the mock received.
func requestLines(srv *apitest.Server) []string {
	requests := srv.Requests()
	lines := make([]string, 0, len(requests))
	for _, request := range requests {
		lines = append(lines, request.Method+" "+request.Path)
	}
	return lines
}

// wantUnsupported fails the test unless err is the unsupported error with the
// message every such error starts with.
func wantUnsupported(t *testing.T, err error) {
	t.Helper()
	var shown interface{ UserMessage() string }
	if !errors.Is(err, ErrAccessRequestsUnsupported) || !errors.As(err, &shown) || !strings.HasPrefix(shown.UserMessage(), unsupportedStart) {
		t.Fatalf("error = %v, want %q", err, unsupportedStart)
	}
	var problem *Error
	if errors.As(err, &problem) {
		t.Fatalf("the unsupported error carries a service problem, which would give it that problem's exit code: %v", err)
	}
}

func TestValidRequestCodeAndDeviceID(t *testing.T) {
	for code, want := range map[string]bool{
		"482913": true, "000000": true,
		"": false, "48291": false, "4829133": false, "482 913": false, "48291a": false, "４８２９１３": false,
	} {
		if got := ValidRequestCode(code); got != want {
			t.Errorf("ValidRequestCode(%q) = %t, want %t", code, got, want)
		}
	}
	for id, want := range map[string]bool{
		testDeviceID: true, "aaaa-aaaa-aaaa-aaaa": true, "2345-6722-3344-5566": true,
		"": false, "abcdefgh2345mnop": false, "ABCD-EFGH-2345-MNOP": false, "abcd-efgh-2345-mno": false,
		"abcd-efgh-2345-mnop-": false, "abcd-efgh-1890-mnop": false, "abcd_efgh_2345_mnop": false, "abcd-efgh-2345-mno\n": false,
	} {
		if got := ValidDeviceID(id); got != want {
			t.Errorf("ValidDeviceID(%q) = %t, want %t", id, got, want)
		}
	}
}

// TestPublishSendsAccessRequestsOnlyWhenAsked pins the create request: the
// access_requests member is true when the publisher asked for it and absent
// otherwise, so a service from before access requests is never sent a member
// it does not know.
func TestPublishSendsAccessRequestsOnlyWhenAsked(t *testing.T) {
	for _, connectorID := range []string{"", "requests-connector"} {
		for _, asked := range []bool{false, true} {
			t.Run(fmt.Sprintf("connector=%q/asked=%t", connectorID, asked), func(t *testing.T) {
				srv := apitest.NewServer(t)
				target := "https://example.com/data"
				if connectorID != "" {
					target = ""
				}
				result, err := newTestClient(t, srv, nil).Publish(t.Context(), target, PublishOptions{AllowRequests: asked, ConnectorID: connectorID})
				if err != nil {
					t.Fatal(err)
				}
				var sent map[string]json.RawMessage
				if err := json.Unmarshal(srv.Requests()[0].Body, &sent); err != nil {
					t.Fatal(err)
				}
				stated, present := sent["access_requests"]
				if present != asked || (asked && string(stated) != "true") {
					t.Fatalf("create request access_requests = %q (present %t), asked %t", stated, present, asked)
				}
				if result.AccessRequests == nil || *result.AccessRequests != asked {
					t.Fatalf("result access requests = %v, want %t", result.AccessRequests, asked)
				}
			})
		}
	}
}

// TestPublishRequiresTheAnswerToConfirmAccessRequests pins the check on the
// create answer when access requests were asked for. A row without the member
// is a service that does not offer them. A row that says they are off is a
// service that did not turn them on. Neither returns a result, so no caller
// can print a CRID as if the publisher got what they asked for.
func TestPublishRequiresTheAnswerToConfirmAccessRequests(t *testing.T) {
	for _, test := range []struct {
		name     string
		answered any
		check    func(*testing.T, error)
	}{
		{name: "confirmed", answered: true},
		{name: "no member", check: func(t *testing.T, err error) {
			wantUnsupported(t, err)
			if !strings.Contains(err.Error(), "published as a private resource without them") || !strings.Contains(err.Error(), "without --allow-requests") {
				t.Fatalf("the message does not say what was made and what to do: %v", err)
			}
		}},
		{name: "off", answered: false, check: func(t *testing.T, err error) {
			if !errors.Is(err, qurl.ErrInvalidAPIResponse) || err.Error() != msgAccessRequestsCreateUnconfirmed {
				t.Fatalf("error = %v, want the not-turned-on message", err)
			}
			for _, part := range []string{"no CRID was printed", "The resource is private", "`qurl requests <CRID> --on`", "`qurl list` shows its CRID"} {
				if !strings.Contains(err.Error(), part) {
					t.Fatalf("the message lost %q: %v", part, err)
				}
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
				data := map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": true}
				if test.answered != nil {
					data["access_requests"] = test.answered
				}
				apitest.WriteEnvelope(t, w, http.StatusCreated, data, nil)
			})
			result, err := newTestClient(t, srv, nil).Publish(t.Context(), "https://example.com/data", PublishOptions{AllowRequests: true})
			if test.check == nil {
				if err != nil || result.AccessRequests == nil || !*result.AccessRequests {
					t.Fatalf("confirmed publish = %+v, %v", result, err)
				}
				return
			}
			if result != nil {
				t.Fatalf("an unconfirmed publish returned %+v", result)
			}
			test.check(t, err)
		})
	}

	// The same through the mock of a service from before access requests.
	srv := apitest.NewServer(t)
	srv.PlayNoAccessRequests()
	_, err := newTestClient(t, srv, nil).Publish(t.Context(), "https://example.com/data", PublishOptions{AllowRequests: true})
	wantUnsupported(t, err)
	// Without the flag that service answers as it always did.
	if result, err := newTestClient(t, srv, nil).Publish(t.Context(), "https://example.com/data", PublishOptions{}); err != nil || result.AccessRequests != nil {
		t.Fatalf("a plain publish against that service = %+v, %v", result, err)
	}
}

// TestPublishWithAccessRequestsOfAPublicTargetIsAConflict pins a publish that
// asks for access requests when the target is already published as public.
// Access requests are for a private resource, so the publisher named a
// privacy: the refusal stands, and no second create is ever sent to keep the
// public resource, whatever else the caller set. The service never refuses a
// create over the access-request setting itself, so the conflict is read as
// for any request: the privacy-mismatch code means the target is published
// as public, and the older answer does not say what differs.
func TestPublishWithAccessRequestsOfAPublicTargetIsAConflict(t *testing.T) {
	for _, test := range []struct {
		code, detail string
		want         ExistingAccess
	}{
		{code: "invalid_input", detail: apitest.LegacyAccessSettingsDetail, want: ExistingAccessUnknown},
		{code: apitest.CodePrivacyMismatch, detail: "d", want: ExistingAccessPublic},
	} {
		srv := apitest.NewServer(t)
		srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteProblem(t, w, http.StatusBadRequest, test.code, "Refused", test.detail)
		})
		_, err := newTestClient(t, srv, nil).Publish(t.Context(), "https://example.com/data", PublishOptions{AllowRequests: true})
		var conflict *PublishAccessConflictError
		if !errors.As(err, &conflict) || conflict.Existing != test.want {
			t.Fatalf("code %q: error = %v, want conflict %d", test.code, err, test.want)
		}
	}
	// The mock of the service itself, and of an older one. KeepExistingPublic
	// is set as well, as a caller that did not count the flag would set it:
	// the request still asked for access requests, so nothing is kept.
	for _, older := range []bool{false, true} {
		for _, keep := range []bool{false, true} {
			srv := apitest.NewServer(t)
			want := ExistingAccessPublic
			if older {
				srv.PlayPublicByDefault()
				want = ExistingAccessUnknown
			}
			srv.SetResourceAccess(false)
			srv.SetPublishFoundExisting(true)
			result, err := newTestClient(t, srv, nil).Publish(t.Context(), "https://example.com/data", PublishOptions{AllowRequests: true, KeepExistingPublic: keep})
			var conflict *PublishAccessConflictError
			if result != nil || !errors.As(err, &conflict) || conflict.Existing != want {
				t.Fatalf("older %t, keep %t: publish = %+v, %v, want conflict %d", older, keep, result, err, want)
			}
			if lines := requestLines(srv); !slices.Equal(lines, []string{"POST /v1/resources"}) {
				t.Fatalf("older %t, keep %t: requests = %v, want the create alone", older, keep, lines)
			}
		}
	}
}

// turnOnRequests is what a publish sends to turn access requests on for a
// resource it found: the create, then the one change.
func turnOnRequests(srv *apitest.Server) []string {
	return []string{"POST /v1/resources", "PATCH /v1/resources/" + srv.Key.CRID}
}

// TestPublishTurnsOnAccessRequestsOfAnExistingPrivateResource pins what a
// publish with access requests does when the target is already published.
// The service answers a create that finds a resource with the setting the
// resource has and changes nothing, so the client turns them on itself, with
// the change `qurl requests --on` makes, only when the answer says the
// resource existed, is private and has them off.
func TestPublishTurnsOnAccessRequestsOfAnExistingPrivateResource(t *testing.T) {
	stored := func(t *testing.T, srv *apitest.Server) bool {
		t.Helper()
		resource, err := newTestClient(t, srv, nil).Resource(t.Context(), srv.Key.CRID)
		if err != nil || resource.AccessRequests == nil {
			t.Fatalf("read the resource back: %+v, %v", resource, err)
		}
		return *resource.AccessRequests
	}

	t.Run("off: turned on", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.SetPublishFoundExisting(true)
		result, err := newTestClient(t, srv, nil).Publish(t.Context(), "https://example.com/data", PublishOptions{AllowRequests: true})
		if err != nil {
			t.Fatal(err)
		}
		if result.AccessRequests == nil || !*result.AccessRequests || !result.AccessRequestsTurnedOn {
			t.Fatalf("result = %+v, want access requests on and reported as turned on", result)
		}
		if result.FoundExisting == nil || !*result.FoundExisting || result.Private == nil || !*result.Private || result.CRID != srv.Key.CRID {
			t.Fatalf("result = %+v, want the existing private resource", result)
		}
		if lines := requestLines(srv); !slices.Equal(lines, turnOnRequests(srv)) {
			t.Fatalf("requests = %v, want %v", lines, turnOnRequests(srv))
		}
		if bodies := requestBodies(srv, http.MethodPatch, "/v1/resources/"+srv.Key.CRID); len(bodies) != 1 || bodies[0] != `{"access_requests":true}` {
			t.Fatalf("change = %v, want access_requests alone", bodies)
		}
		if !stored(t, srv) {
			t.Fatal("the resource still has access requests off")
		}
	})

	// Nothing to turn on, or nothing asked for: the create is the only
	// request, and the result carries what the resource has.
	for _, test := range []struct {
		name          string
		existing, was bool
		opts          PublishOptions
	}{
		{name: "already on", existing: true, was: true, opts: PublishOptions{AllowRequests: true}},
		{name: "not asked for, off", existing: true},
		{name: "not asked for, on", existing: true, was: true},
		{name: "a new resource", opts: PublishOptions{AllowRequests: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.SetPublishFoundExisting(test.existing)
			srv.SetAccessRequests(test.was)
			result, err := newTestClient(t, srv, nil).Publish(t.Context(), "https://example.com/data", test.opts)
			if err != nil {
				t.Fatal(err)
			}
			want := test.was || test.opts.AllowRequests
			if result.AccessRequests == nil || *result.AccessRequests != want || result.AccessRequestsTurnedOn {
				t.Fatalf("result = %+v, want access requests %t and nothing turned on", result, want)
			}
			if lines := requestLines(srv); !slices.Equal(lines, []string{"POST /v1/resources"}) {
				t.Fatalf("requests = %v, want the create alone", lines)
			}
			if got := stored(t, srv); got != want {
				t.Fatalf("stored setting = %t, want %t", got, want)
			}
		})
	}

	// An answer that does not say the resource existed is a resource that was
	// just made with access requests off. That is the service not doing what
	// was asked, not a resource to change.
	for _, meta := range []map[string]any{nil, {"found_existing": false}} {
		srv := apitest.NewServer(t)
		srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{
				"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": true, "access_requests": false,
			}, meta)
		})
		result, err := newTestClient(t, srv, nil).Publish(t.Context(), "https://example.com/data", PublishOptions{AllowRequests: true})
		if result != nil || !errors.Is(err, qurl.ErrInvalidAPIResponse) || err.Error() != msgAccessRequestsCreateUnconfirmed {
			t.Fatalf("meta %v: publish = %+v, %v, want the not-turned-on message", meta, result, err)
		}
		if lines := requestLines(srv); !slices.Equal(lines, []string{"POST /v1/resources"}) {
			t.Fatalf("meta %v: requests = %v, want the create alone", meta, lines)
		}
	}

	// A service from before access requests has no setting to turn on.
	t.Run("older service", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.PlayNoAccessRequests()
		srv.SetPublishFoundExisting(true)
		result, err := newTestClient(t, srv, nil).Publish(t.Context(), "https://example.com/data", PublishOptions{AllowRequests: true})
		wantUnsupported(t, err)
		if lines := requestLines(srv); result != nil || !slices.Equal(lines, []string{"POST /v1/resources"}) {
			t.Fatalf("publish = %+v with requests %v, want no result and the create alone", result, lines)
		}
	})
}

// TestPublishThatCannotTurnOnAccessRequestsNamesTheResource pins the failure
// of that change. The resource exists, is private and is as it was, so the
// error names its CRID for the retry; what went wrong stays in the chain for
// the exit code; and the change is sent once, never retried.
func TestPublishThatCannotTurnOnAccessRequestsNamesTheResource(t *testing.T) {
	row := func(srv *apitest.Server, private, requests bool) map[string]any {
		return map[string]any{
			"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "type": "url", "status": "active",
			"private": private, "access_requests": requests,
		}
	}
	for _, test := range []struct {
		name   string
		answer func(*testing.T, *apitest.Server, http.ResponseWriter)
		reason string
		check  func(*testing.T, error)
	}{
		{
			name: "the service is busy",
			answer: func(t *testing.T, _ *apitest.Server, w http.ResponseWriter) {
				w.Header().Set("Retry-After", "1")
				apitest.WriteProblem(t, w, http.StatusServiceUnavailable, "service_unavailable", "Service Unavailable", "the resource is being changed; try again")
			},
			reason: "the resource is being changed; try again",
			check: func(t *testing.T, err error) {
				var problem *Error
				if !errors.As(err, &problem) || problem.StatusCode != http.StatusServiceUnavailable || problem.RequestID == "" {
					t.Fatalf("the service's problem is not in the chain: %v", err)
				}
			},
		},
		{
			name: "a problem with a title alone",
			answer: func(t *testing.T, _ *apitest.Server, w http.ResponseWriter) {
				apitest.WriteProblem(t, w, http.StatusForbidden, "forbidden", "Forbidden", "")
			},
			reason: "Forbidden",
		},
		{
			name: "the answer says they are still off",
			answer: func(t *testing.T, srv *apitest.Server, w http.ResponseWriter) {
				apitest.WriteEnvelope(t, w, http.StatusOK, row(srv, true, false), nil)
			},
			reason: msgAccessRequestsSettingUnconfirmed,
			check: func(t *testing.T, err error) {
				if !errors.Is(err, qurl.ErrInvalidAPIResponse) {
					t.Fatalf("an unconfirmed change is not an invalid answer: %v", err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.SetPublishFoundExisting(true)
			srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
				test.answer(t, srv, w)
			})
			result, err := newTestClient(t, srv, nil).Publish(t.Context(), "https://example.com/data", PublishOptions{AllowRequests: true})
			var failed *AccessRequestsNotTurnedOnError
			if result != nil || !errors.As(err, &failed) {
				t.Fatalf("publish = %+v, %v, want the not-turned-on error and no result", result, err)
			}
			if failed.CRID != srv.Key.CRID || failed.Headline() != msgAccessRequestsNotTurnedOn || failed.Reason() != test.reason {
				t.Fatalf("error names %q, says %q because %q; want the resource's CRID and reason %q", failed.CRID, failed.Headline(), failed.Reason(), test.reason)
			}
			if got, want := err.Error(), msgAccessRequestsNotTurnedOn+": "+test.reason; got != want {
				t.Fatalf("error text = %q, want %q", got, want)
			}
			if test.check != nil {
				test.check(t, err)
			}
			if lines := requestLines(srv); !slices.Equal(lines, turnOnRequests(srv)) {
				t.Fatalf("requests = %v, want the create and one change", lines)
			}
		})
	}

	// A failure with no wording of its own is shown as it is.
	plain := &AccessRequestsNotTurnedOnError{CRID: "q", cause: errors.New("connection refused")}
	if plain.Reason() != "connection refused" || !strings.HasSuffix(plain.Error(), ": connection refused") {
		t.Fatalf("plain failure = %q / %q", plain.Reason(), plain.Error())
	}

	// The change answered, and its row says the resource is public. The two
	// answers disagree about who can open it, so this is the privacy failure
	// and no CRID is named.
	srv := apitest.NewServer(t)
	srv.SetPublishFoundExisting(true)
	srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, row(srv, false, true), nil)
	})
	result, err := newTestClient(t, srv, nil).Publish(t.Context(), "https://example.com/data", PublishOptions{AllowRequests: true})
	var failed *AccessRequestsNotTurnedOnError
	if result != nil || errors.As(err, &failed) || !errors.Is(err, qurl.ErrInvalidAPIResponse) || err.Error() != msgPrivateUnconfirmed || strings.Contains(err.Error(), srv.Key.CRID) {
		t.Fatalf("publish = %+v, %v, want the privacy failure without a CRID", result, err)
	}
}

// TestSetAccessRequests pins the change of the setting: one PATCH that
// carries only access_requests, and an answer that must confirm it.
func TestSetAccessRequests(t *testing.T) {
	for _, on := range []bool{true, false} {
		srv := apitest.NewServer(t)
		srv.SetAccessRequests(!on)
		srv.AddApprovedPerson(testDeviceID, testRequester)
		resource, err := newTestClient(t, srv, nil).SetAccessRequests(t.Context(), srv.Key.CRID, on)
		if err != nil || resource.AccessRequests == nil || *resource.AccessRequests != on || len(resource.AllowedPasskeys) != 1 {
			t.Fatalf("SetAccessRequests(%t) = %+v, %v", on, resource, err)
		}
		want := fmt.Sprintf(`{"access_requests":%t}`, on)
		if bodies := requestBodies(srv, http.MethodPatch, "/v1/resources/"+srv.Key.CRID); len(bodies) != 1 || bodies[0] != want || len(srv.Requests()) != 1 {
			t.Fatalf("requests = %v, want one PATCH with body %s", requestLines(srv), want)
		}
	}

	// The path is built as every request here builds it: the identifier is
	// trimmed, and one that could never name a resource is refused before
	// any request.
	t.Run("the identifier", func(t *testing.T) {
		srv := apitest.NewServer(t)
		if _, err := newTestClient(t, srv, nil).SetAccessRequests(t.Context(), "  "+srv.Key.CRID+"\n", true); err != nil {
			t.Fatalf("a padded identifier: %v", err)
		}
		if lines := requestLines(srv); !slices.Equal(lines, []string{"PATCH /v1/resources/" + srv.Key.CRID}) {
			t.Fatalf("requests = %v, want one change to the trimmed identifier", lines)
		}
		for _, id := range []string{"", "   ", "a/b", "a b", "../" + srv.Key.CRID, srv.Key.CRID + "?x=1"} {
			srv := apitest.NewServer(t)
			if _, err := newTestClient(t, srv, nil).SetAccessRequests(t.Context(), id, true); err == nil || len(srv.Requests()) != 0 {
				t.Fatalf("identifier %q: error %v after %d requests, want a refusal before any request", id, err, len(srv.Requests()))
			}
		}
	})
	t.Run("older service", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.PlayNoAccessRequests()
		resource, err := newTestClient(t, srv, nil).SetAccessRequests(t.Context(), srv.Key.CRID, true)
		wantUnsupported(t, err)
		if resource != nil || !strings.Contains(err.Error(), "does not show the setting") {
			t.Fatalf("an ignored change returned %+v, %v", resource, err)
		}
	})
	t.Run("the answer has the other setting", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
				"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": true, "type": "url", "status": "active", "access_requests": false,
			}, nil)
		})
		resource, err := newTestClient(t, srv, nil).SetAccessRequests(t.Context(), srv.Key.CRID, true)
		if resource != nil || !errors.Is(err, qurl.ErrInvalidAPIResponse) || err.Error() != msgAccessRequestsSettingUnconfirmed {
			t.Fatalf("an unconfirmed change returned %+v, %v", resource, err)
		}
	})
	t.Run("a public resource", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.SetResourceAccess(false)
		resource, err := newTestClient(t, srv, nil).SetAccessRequests(t.Context(), srv.Key.CRID, true)
		var problem *Error
		if resource != nil || !errors.As(err, &problem) || problem.StatusCode != http.StatusBadRequest || !strings.Contains(problem.Detail, "private resource") {
			t.Fatalf("turning requests on for a public resource returned %+v, %v", resource, err)
		}
	})
}

// pendingDevices returns the device ids of the pending requests of the mock's
// resource, as the listing gives them.
func pendingDevices(t *testing.T, srv *apitest.Server) []string {
	t.Helper()
	list, err := newTestClient(t, srv, nil).AccessRequests(t.Context(), srv.Key.CRID)
	if err != nil {
		t.Fatal(err)
	}
	devices := make([]string, 0, len(list.Requests))
	for index := range list.Requests {
		devices = append(devices, list.Requests[index].DeviceID)
	}
	return devices
}

// TestAccessRequestsListings pins the two listings: every field of a row, and
// the resource each row is for. A row says who asked, from which device, when
// and until when. It has no code.
func TestAccessRequestsListings(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.AddAccessRequest(testRequestCode, testRequester, testDeviceID)
	srv.AddAccessRequest(testOtherCode, "", testOtherDevice)
	client := newTestClient(t, srv, nil)

	for _, id := range []string{"", srv.Key.CRID} {
		list, err := client.AccessRequests(t.Context(), id)
		if err != nil || len(list.Requests) != 2 || list.HasMore {
			t.Fatalf("AccessRequests(%q) = %+v, %v", id, list, err)
		}
		first, second := list.Requests[0], list.Requests[1]
		if first.Name != testRequester || first.DeviceID != testDeviceID || second.Name != "" || second.DeviceID != testOtherDevice {
			t.Fatalf("AccessRequests(%q) rows = %+v", id, list.Requests)
		}
		if first.RequestedAt == nil || first.ExpiresAt == nil || !first.ExpiresAt.After(*first.RequestedAt) {
			t.Fatalf("AccessRequests(%q) lost the request times: %+v", id, first)
		}
		if first.CRID != srv.Key.CRID || second.CRID != srv.Key.CRID {
			t.Fatalf("AccessRequests(%q) rows do not name the resource: %+v", id, list.Requests)
		}
	}
	want := []string{"GET /v1/access-requests", "GET /v1/resources/" + srv.Key.CRID + "/access-requests"}
	if got := requestLines(srv); !slices.Equal(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
}

// TestAccessRequestsListingHoldsNoCode pins that a listing cannot carry the
// code of a request to anything that renders it. The service does not send
// one. A build of it that still does, in the request_code member, is read
// with a row type that has no such member, into a result type that has no
// such field: here every value the listing returns is searched for the two
// codes, and neither is anywhere.
func TestAccessRequestsListingHoldsNoCode(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.ListRequestCodes()
	srv.AddAccessRequest(testRequestCode, testRequester, testDeviceID)
	srv.AddAccessRequest(testOtherCode, testOtherPerson, testOtherDevice)
	for _, id := range []string{"", srv.Key.CRID} {
		list, err := newTestClient(t, srv, nil).AccessRequests(t.Context(), id)
		if err != nil || len(list.Requests) != 2 {
			t.Fatalf("AccessRequests(%q) = %+v, %v", id, list, err)
		}
		everything := fmt.Sprintf("%#v", list)
		for _, code := range []string{testRequestCode, testOtherCode} {
			if strings.Contains(everything, code) {
				t.Fatalf("AccessRequests(%q) holds the code %s: %s", id, code, everything)
			}
		}
	}
	// The service did send them: the test would pass for the wrong reason if
	// the mock had left them out.
	sent, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/v1/access-requests", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	sent.Header.Set("Authorization", "Bearer lv_test_apitestingvalue123456789")
	answer, err := srv.Client().Do(sent)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = answer.Body.Close() }()
	body, err := io.ReadAll(answer.Body)
	if err != nil || !strings.Contains(string(body), `"request_code":"`+testRequestCode+`"`) {
		t.Fatalf("the mock did not send the codes this test is about: %s (%v)", body, err)
	}
}

// TestAccessRequestsListingSaysWhenThereMayBeMore pins the one member of the
// listing's envelope the client reads. The service bounds the listing of all
// resources and sets has_more when it may be incomplete. A listing without
// the member is complete.
func TestAccessRequestsListingSaysWhenThereMayBeMore(t *testing.T) {
	for _, test := range []struct {
		name string
		set  *bool
		want bool
	}{
		{name: "not said"},
		{name: "false", set: new(bool)},
		{name: "true", set: func() *bool { more := true; return &more }(), want: true},
	} {
		srv := apitest.NewServer(t)
		srv.AddAccessRequest(testRequestCode, testRequester, testDeviceID)
		if test.set != nil {
			srv.SetAccessRequestsHasMore(*test.set)
		}
		for _, id := range []string{"", srv.Key.CRID} {
			list, err := newTestClient(t, srv, nil).AccessRequests(t.Context(), id)
			if err != nil || list.HasMore != test.want || len(list.Requests) != 1 {
				t.Fatalf("%s, AccessRequests(%q) = %+v, %v; want has-more %t", test.name, id, list, err, test.want)
			}
		}
	}
}

// TestAccessRequestsListingRefusesRowsOutsideTheContract pins that a row
// whose device id is not one, and a row of the all-resources listing that
// does not name a real resource, fail the listing: a device id is what a
// publisher passes to refuse a request, and it is never shown in a form that
// could not be passed back.
func TestAccessRequestsListingRefusesRowsOutsideTheContract(t *testing.T) {
	for name, row := range map[string]map[string]any{
		"no device id":        {"name": testRequester},
		"device id malformed": {"device_id": "ABCD-EFGH-2345-MNOP"},
		"device id escape":    {"device_id": "abcd-efgh-2345-mn\x1b["},
		"a code for an id":    {"device_id": testRequestCode},
	} {
		t.Run(name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.Script(http.MethodGet, "/v1/resources/"+srv.Key.CRID+"/access-requests", func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteEnvelope(t, w, http.StatusOK, []map[string]any{row}, nil)
			})
			list, err := newTestClient(t, srv, nil).AccessRequests(t.Context(), srv.Key.CRID)
			if list != nil || !errors.Is(err, qurl.ErrInvalidAPIResponse) {
				t.Fatalf("AccessRequests = %+v, %v; want an invalid-response error", list, err)
			}
		})
	}
	srv := apitest.NewServer(t)
	srv.Script(http.MethodGet, "/v1/access-requests", func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteEnvelope(t, w, http.StatusOK, []map[string]any{{"device_id": testDeviceID, "crid": "not-a-crid", "resource_id": srv.Key.ResourceID}}, nil)
	})
	if list, err := newTestClient(t, srv, nil).AccessRequests(t.Context(), ""); list != nil || !errors.Is(err, qurl.ErrInvalidAPIResponse) {
		t.Fatalf("a row for no real resource was accepted: %+v, %v", list, err)
	}
}

// TestAccessRequestsOnAServiceWithoutThem pins what a 404 from the listings
// means. For all resources it can only be a service without the route. For
// one resource the client asks whether the resource exists: when it does, the
// route is what is missing; when it does not, the answer is the plain
// not-found of that resource and never "this service does not offer".
func TestAccessRequestsOnAServiceWithoutThem(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.PlayNoAccessRequests()
	client := newTestClient(t, srv, nil)
	_, err := client.AccessRequests(t.Context(), "")
	wantUnsupported(t, err)
	_, err = client.AccessRequests(t.Context(), srv.Key.CRID)
	wantUnsupported(t, err)

	for _, older := range []bool{false, true} {
		srv := apitest.NewServer(t)
		if older {
			srv.PlayNoAccessRequests()
		}
		_, err := newTestClient(t, srv, nil).AccessRequests(t.Context(), unknownTestCRID)
		var problem *Error
		if errors.Is(err, ErrAccessRequestsUnsupported) || !errors.As(err, &problem) || problem.StatusCode != http.StatusNotFound {
			t.Fatalf("older=%t: an unknown resource read as %v, want its plain not-found", older, err)
		}
	}
}

// TestApproveAccessRequest pins the approval: one POST on the route of the
// code that was given, never repeated, and the person the service says now
// has access.
func TestApproveAccessRequest(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.AddAccessRequest(testOtherCode, testOtherPerson, testOtherDevice)
	srv.AddAccessRequest(testRequestCode, testRequester, testDeviceID)
	client := newTestClient(t, srv, nil)
	person, err := client.ApproveAccessRequest(t.Context(), srv.Key.CRID, testRequestCode)
	if err != nil || person.DeviceID != testDeviceID || person.Name != testRequester || person.ApprovedAt == nil {
		t.Fatalf("ApproveAccessRequest = %+v, %v", person, err)
	}
	want := []string{"POST /v1/resources/" + srv.Key.CRID + "/access-requests/" + testRequestCode + "/approve"}
	if got := requestLines(srv); !slices.Equal(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	if body := string(srv.Requests()[0].Body); body != "{}" {
		t.Fatalf("approval body = %q, want an empty JSON object", body)
	}
	// The other request is still pending and the approved one is gone.
	if pending := pendingDevices(t, srv); !slices.Equal(pending, []string{testOtherDevice}) {
		t.Fatalf("pending after the approval = %v", pending)
	}
	resource, err := client.Resource(t.Context(), srv.Key.CRID)
	if err != nil || len(resource.AllowedPasskeys) != 1 || resource.AllowedPasskeys[0].DeviceID != testDeviceID {
		t.Fatalf("approved people after the approval = %+v, %v", resource, err)
	}
}

// TestApprovalAndDenialOfARequestThatIsNotPending pins the three meanings of
// a 404 from the routes that name a request, for an approval by code and for
// a denial by code and by device id. The message for a wrong code says
// nothing about which codes exist.
func TestApprovalAndDenialOfARequestThatIsNotPending(t *testing.T) {
	const absentDevice = "nope-nope-nope-nope"
	calls := map[string]struct {
		call func(Client, string) error
		want []string
	}{
		"approve": {
			call: func(client Client, id string) error {
				_, err := client.ApproveAccessRequest(t.Context(), id, testRequestCode)
				return err
			},
			want: []string{"no pending request has the code 482 913 for this resource", "Ask the person for the code on their screen"},
		},
		"deny by code": {
			call: func(client Client, id string) error {
				return client.DenyAccessRequest(t.Context(), id, testRequestCode)
			},
			want: []string{"no pending request has the code 482 913 for this resource"},
		},
		"deny by device id": {
			call: func(client Client, id string) error { return client.DenyAccessRequest(t.Context(), id, absentDevice) },
			want: []string{"no pending request is from the device id " + absentDevice + " for this resource", "`qurl requests <CRID>`"},
		},
	}
	for name, test := range calls {
		t.Run(name+"/the request is not pending", func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.AddAccessRequest(testOtherCode, testOtherPerson, testOtherDevice)
			err := test.call(newTestClient(t, srv, nil), srv.Key.CRID)
			var problem *Error
			var shown interface{ UserMessage() string }
			if !errors.As(err, &problem) || problem.StatusCode != http.StatusNotFound || problem.RequestID == "" || !errors.As(err, &shown) {
				t.Fatalf("error = %v, want a not-found with its own message", err)
			}
			for _, want := range test.want {
				if !strings.Contains(shown.UserMessage(), want) {
					t.Fatalf("message %q lacks %q", shown.UserMessage(), want)
				}
			}
			// The code that is pending is the other one, and no message may
			// give it away.
			if strings.Contains(shown.UserMessage(), testOtherCode) || strings.Contains(shown.UserMessage(), testOtherCode[:3]+" "+testOtherCode[3:]) {
				t.Fatalf("the message hints at a code that is pending: %q", shown.UserMessage())
			}
			if errors.Is(err, ErrAccessRequestsUnsupported) {
				t.Fatal("a request that is not pending read as a service without access requests")
			}
			// The other request was not touched.
			if pending := pendingDevices(t, srv); !slices.Equal(pending, []string{testOtherDevice}) {
				t.Fatalf("pending = %v", pending)
			}
		})
		t.Run(name+"/the service has no access requests", func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.PlayNoAccessRequests()
			wantUnsupported(t, test.call(newTestClient(t, srv, nil), srv.Key.CRID))
		})
		t.Run(name+"/the resource is unknown", func(t *testing.T) {
			srv := apitest.NewServer(t)
			err := test.call(newTestClient(t, srv, nil), unknownTestCRID)
			var problem *Error
			var shown interface{ UserMessage() string }
			if errors.Is(err, ErrAccessRequestsUnsupported) || errors.As(err, &shown) || !errors.As(err, &problem) || problem.StatusCode != http.StatusNotFound {
				t.Fatalf("an unknown resource read as %v, want its plain not-found", err)
			}
		})
	}

	// An approval takes a code and nothing else. A denial takes a device id
	// or a code. Anything else is refused before any request, a device id
	// given to an approval included.
	srv := apitest.NewServer(t)
	srv.AddAccessRequest(testRequestCode, testRequester, testDeviceID)
	for _, value := range []string{"", "48291", "482 913", "48291a", "../482913", testDeviceID} {
		if _, err := newTestClient(t, srv, nil).ApproveAccessRequest(t.Context(), srv.Key.CRID, value); !errors.Is(err, qurl.ErrInvalidResourceRequest) {
			t.Errorf("approval of %q: error = %v", value, err)
		}
	}
	for _, value := range []string{"", "48291", "482 913", "48291a", "../482913", "ABCD-EFGH-2345-MNOP", "abcd-efgh-2345", testDeviceID + "/approve"} {
		if err := newTestClient(t, srv, nil).DenyAccessRequest(t.Context(), srv.Key.CRID, value); !errors.Is(err, qurl.ErrInvalidResourceRequest) {
			t.Errorf("denial of %q: error = %v", value, err)
		}
	}
	if got := len(srv.Requests()); got != 0 {
		t.Fatalf("a refused value sent %d requests", got)
	}
}

// TestApprovalAnswerMustNameTheDevice pins that an approval answer without a
// device id is not reported as an approval of anyone.
func TestApprovalAnswerMustNameTheDevice(t *testing.T) {
	for name, data := range map[string]map[string]any{
		"no device id":        {"name": testRequester},
		"malformed device id": {"name": testRequester, "device_id": "abcd"},
	} {
		srv := apitest.NewServer(t)
		srv.Script(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/access-requests/"+testRequestCode+"/approve", func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteEnvelope(t, w, http.StatusOK, data, nil)
		})
		person, err := newTestClient(t, srv, nil).ApproveAccessRequest(t.Context(), srv.Key.CRID, testRequestCode)
		if person != nil || !errors.Is(err, qurl.ErrInvalidAPIResponse) || err.Error() != msgApprovalUnconfirmed {
			t.Fatalf("%s: ApproveAccessRequest = %+v, %v", name, person, err)
		}
	}
}

// TestDenyAccessRequest pins the denial: one DELETE that names the request by
// the device id it came from, as a listing shows it, or by its code, each
// sent as it was given, and no access for anyone. The request that is
// refused is the one that was named, never another one.
func TestDenyAccessRequest(t *testing.T) {
	for name, named := range map[string]string{"by device id": testDeviceID, "by code": testRequestCode} {
		t.Run(name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			// The other request is first, so a denial that took "the first
			// row" would refuse the wrong person.
			srv.AddAccessRequest(testOtherCode, testOtherPerson, testOtherDevice)
			srv.AddAccessRequest(testRequestCode, testRequester, testDeviceID)
			client := newTestClient(t, srv, nil)
			if err := client.DenyAccessRequest(t.Context(), srv.Key.CRID, named); err != nil {
				t.Fatal(err)
			}
			want := []string{"DELETE /v1/resources/" + srv.Key.CRID + "/access-requests/" + named}
			if got := requestLines(srv); !slices.Equal(got, want) {
				t.Fatalf("requests = %v, want %v: the value is sent as it was given, and nothing is looked up", got, want)
			}
			resource, err := client.Resource(t.Context(), srv.Key.CRID)
			if err != nil || len(resource.AllowedPasskeys) != 0 {
				t.Fatalf("a denial gave access: %+v, %v", resource, err)
			}
			if pending := pendingDevices(t, srv); !slices.Equal(pending, []string{testOtherDevice}) {
				t.Fatalf("pending after the denial = %v, want the other request alone", pending)
			}
		})
	}
}

// TestDeviceCredentialDeniesByDeviceID pins the denial a publisher makes on a
// real install, where every command uses the device's own credential through
// the SDK: `qurl deny <CRID> <device id>`, with the device id a listing
// shows. The SDK sends on the routes it lists and refuses any other before
// anything leaves the machine, and it lists this one for a device id as for
// a code. One DELETE is sent, with the device id in the path as it was
// given and the device credential on it. Nothing is read first, the request
// is gone, and the other person's request is not touched.
func TestDeviceCredentialDeniesByDeviceID(t *testing.T) {
	srv := apitest.NewServer(t)
	// The other request is first, so a denial that took "the first row"
	// would refuse the wrong person.
	srv.AddAccessRequest(testOtherCode, testOtherPerson, testOtherDevice)
	srv.AddAccessRequest(testRequestCode, testRequester, testDeviceID)
	client := newRegisteredTestClient(t, srv)

	if err := client.DenyAccessRequest(t.Context(), srv.Key.CRID, testDeviceID); err != nil {
		t.Fatalf("denial by device id through a device credential: %v", err)
	}
	want := []string{"DELETE /v1/resources/" + srv.Key.CRID + "/access-requests/" + testDeviceID}
	if got := requestLines(srv); !slices.Equal(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	credential := "Bearer " + registeredAPIState(t).DeviceAPIKey
	if got := srv.Requests()[0].Header.Get("Authorization"); got != credential {
		t.Fatalf("the denial's authorization = %q, want the device credential", got)
	}
	if pending := pendingDevices(t, srv); !slices.Equal(pending, []string{testOtherDevice}) {
		t.Fatalf("pending after the denial = %v, want the other request alone", pending)
	}

	// A device id with no pending request is the service's answer, sent and
	// answered like any other, not a refusal on this machine.
	sent := len(srv.Requests())
	err := client.DenyAccessRequest(t.Context(), srv.Key.CRID, testDeviceID)
	var problem *Error
	if !errors.As(err, &problem) || problem.StatusCode != http.StatusNotFound || errors.Is(err, qurl.ErrRegisteredAgentResourceRequestDenied) {
		t.Fatalf("a second denial by device id: error = %v, want the service's not-found", err)
	}
	if got := requestLines(srv)[sent:]; len(got) == 0 || got[0] != want[0] {
		t.Fatalf("a second denial by device id sent %v", got)
	}
}

// removalOutcome fails the test unless err is a PasskeyRemovalError with these
// three lists, and returns it.
func removalOutcome(t *testing.T, err error, removed, notFound, notRemoved []string) *PasskeyRemovalError {
	t.Helper()
	var outcome *PasskeyRemovalError
	if !errors.As(err, &outcome) {
		t.Fatalf("error = %v, want a removal outcome", err)
	}
	if !slices.Equal(outcome.Removed, removed) || !slices.Equal(outcome.NotFound, notFound) || !slices.Equal(outcome.NotRemoved, notRemoved) {
		t.Fatalf("outcome: removed %v, not found %v, not removed %v; want %v, %v, %v",
			outcome.Removed, outcome.NotFound, outcome.NotRemoved, removed, notFound, notRemoved)
	}
	return outcome
}

// approvedIDs reads the device ids of the approved people from the mock.
func approvedIDs(t *testing.T, srv *apitest.Server) []string {
	t.Helper()
	resource, err := newTestClient(t, srv, nil).Resource(t.Context(), srv.Key.CRID)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(resource.AllowedPasskeys))
	for index := range resource.AllowedPasskeys {
		ids = append(ids, resource.AllowedPasskeys[index].DeviceID)
	}
	return ids
}

// TestRemoveAllowedPasskeys pins the removal of approved people when every
// device id is on the list: one read of the list, one DELETE for each device
// id, in order, then one read of the resource, which must not list a removed
// person.
func TestRemoveAllowedPasskeys(t *testing.T) {
	const kept = "keep-keep-keep-keep"
	t.Run("removed", func(t *testing.T) {
		srv := apitest.NewServer(t)
		for _, id := range []string{testDeviceID, kept, testOtherDevice} {
			srv.AddApprovedPerson(id, "person "+id)
		}
		resource, err := newTestClient(t, srv, nil).RemoveAllowedPasskeys(t.Context(), srv.Key.CRID, []string{testDeviceID, testOtherDevice})
		if err != nil || len(resource.AllowedPasskeys) != 1 || resource.AllowedPasskeys[0].DeviceID != kept {
			t.Fatalf("RemoveAllowedPasskeys = %+v, %v", resource, err)
		}
		base := "/v1/resources/" + srv.Key.CRID
		want := []string{"GET " + base, "DELETE " + base + "/allowed-passkeys/" + testDeviceID, "DELETE " + base + "/allowed-passkeys/" + testOtherDevice, "GET " + base}
		if got := requestLines(srv); !slices.Equal(got, want) {
			t.Fatalf("requests = %v, want %v", got, want)
		}
	})
	t.Run("the service has no access requests", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.PlayNoAccessRequests()
		_, err := newTestClient(t, srv, nil).RemoveAllowedPasskeys(t.Context(), srv.Key.CRID, []string{testDeviceID})
		wantUnsupported(t, err)
	})
	t.Run("the resource is unknown", func(t *testing.T) {
		srv := apitest.NewServer(t)
		_, err := newTestClient(t, srv, nil).RemoveAllowedPasskeys(t.Context(), unknownTestCRID, []string{testDeviceID})
		var problem *Error
		var outcome *PasskeyRemovalError
		if !errors.As(err, &problem) || problem.StatusCode != http.StatusNotFound || errors.As(err, &outcome) {
			t.Fatalf("error = %v, want the resource's own not-found answer", err)
		}
	})
	t.Run("the person is still listed", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.AddApprovedPerson(testDeviceID, testRequester)
		srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID+"/allowed-passkeys/"+testDeviceID, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})
		resource, err := newTestClient(t, srv, nil).RemoveAllowedPasskeys(t.Context(), srv.Key.CRID, []string{testDeviceID})
		if resource != nil || !errors.Is(err, qurl.ErrInvalidAPIResponse) {
			t.Fatalf("an answer that still lists the person returned %+v, %v", resource, err)
		}
		// The list is what the service says now: the person still has
		// access, and nobody lost it.
		outcome := removalOutcome(t, err, nil, nil, []string{testDeviceID})
		want := "the service answered that access was taken away from " + testDeviceID + ", but its list still shows " + testDeviceID + ". " + testDeviceID + " still has access"
		if outcome.Headline() != want || outcome.Reason() != "" {
			t.Fatalf("headline = %q, reason = %q; want %q and no reason", outcome.Headline(), outcome.Reason(), want)
		}
	})
	t.Run("nothing that is not a device id is sent", func(t *testing.T) {
		srv := apitest.NewServer(t)
		for _, ids := range [][]string{nil, {"abcd"}, {testDeviceID, "../" + testOtherDevice}, {testDeviceID, testDeviceID}} {
			if _, err := newTestClient(t, srv, nil).RemoveAllowedPasskeys(t.Context(), srv.Key.CRID, ids); !errors.Is(err, qurl.ErrInvalidResourceRequest) {
				t.Errorf("ids %v: error = %v", ids, err)
			}
		}
		if got := len(srv.Requests()); got != 0 {
			t.Fatalf("a refused removal sent %d requests", got)
		}
	})
}

// TestRemovalChecksEveryDeviceIDBeforeItRemovesAny pins what makes "nothing
// was removed" true with several device ids in one command. The list is read
// first, and a device id that is not on it stops the command before any
// access is taken away, wherever in the command line it stands. The error
// names the ids that were not found and the ones that still have access, has
// the not-found exit code, and the only request is the read.
func TestRemovalChecksEveryDeviceIDBeforeItRemovesAny(t *testing.T) {
	const (
		known, known2     = testDeviceID, testOtherDevice
		unknown, unknown2 = "nope-nope-nope-nope", "gone-gone-gone-gone"
	)
	for _, test := range []struct {
		name                 string
		ids                  []string
		notFound, notRemoved []string
		message              string
	}{
		{name: "one, unknown", ids: []string{unknown}, notFound: []string{unknown},
			message: "no approved person has the device id " + unknown + " on this resource, so nothing was removed"},
		{name: "unknown first", ids: []string{unknown, known}, notFound: []string{unknown}, notRemoved: []string{known},
			message: "no approved person has the device id " + unknown + " on this resource, so nothing was removed. " + known + " still has access"},
		{name: "unknown last", ids: []string{known, unknown}, notFound: []string{unknown}, notRemoved: []string{known},
			message: "no approved person has the device id " + unknown + " on this resource, so nothing was removed. " + known + " still has access"},
		{name: "unknown in the middle", ids: []string{known, unknown, known2}, notFound: []string{unknown}, notRemoved: []string{known, known2},
			message: "no approved person has the device id " + unknown + " on this resource, so nothing was removed. " + known + " and " + known2 + " still have access"},
		{name: "two unknown", ids: []string{unknown, known, unknown2}, notFound: []string{unknown, unknown2}, notRemoved: []string{known},
			message: "no approved person has the device ids " + unknown + " and " + unknown2 + " on this resource, so nothing was removed. " + known + " still has access"},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.AddApprovedPerson(known, testRequester)
			srv.AddApprovedPerson(known2, testOtherPerson)
			resource, err := newTestClient(t, srv, nil).RemoveAllowedPasskeys(t.Context(), srv.Key.CRID, test.ids)
			if resource != nil {
				t.Fatalf("a refused removal returned %+v", resource)
			}
			outcome := removalOutcome(t, err, nil, test.notFound, test.notRemoved)
			if !errors.Is(err, ErrApprovedPersonNotFound) {
				t.Fatalf("error = %v, want the not-found sentinel", err)
			}
			if outcome.Headline() != test.message || outcome.Reason() != "" {
				t.Fatalf("message = %q with reason %q, want %q", outcome.Headline(), outcome.Reason(), test.message)
			}
			if want := test.message + ". Run `qurl grants " + srv.Key.CRID + "` to see who has access now"; err.Error() != want {
				t.Fatalf("error text = %q, want %q", err.Error(), want)
			}
			if lines := requestLines(srv); !slices.Equal(lines, []string{"GET /v1/resources/" + srv.Key.CRID}) {
				t.Fatalf("requests = %v, want the one read: nothing may be removed", lines)
			}
			if ids := approvedIDs(t, srv); !slices.Equal(ids, []string{known, known2}) {
				t.Fatalf("approved people afterwards = %v, want both still there", ids)
			}
		})
	}
}

// TestRemovalSaysExactlyWhatWasRemovedWhenItStopsPartWay pins the outcome of
// a removal that stops after access was taken away from someone: the list
// changed between the read and a removal, or a removal failed for another
// reason. The error says which device ids were removed, which was not found
// and which still have access, so that access taken away never reads as
// "nothing happened". The removals after the one that stopped are not sent.
func TestRemovalSaysExactlyWhatWasRemovedWhenItStopsPartWay(t *testing.T) {
	const first, second, third = testDeviceID, testOtherDevice, "keep-keep-keep-keep"
	people := func(srv *apitest.Server) {
		for _, id := range []string{first, second, third} {
			srv.AddApprovedPerson(id, "person "+id)
		}
	}
	gone := func(t *testing.T) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteProblem(t, w, http.StatusNotFound, "not_found", "Not Found", "no such approved person")
		}
	}
	removal := func(srv *apitest.Server, deviceID string) string {
		return "/v1/resources/" + srv.Key.CRID + "/allowed-passkeys/" + deviceID
	}
	sentRemovals := func(srv *apitest.Server) []string {
		var ids []string
		for _, request := range srv.Requests() {
			if request.Method == http.MethodDelete {
				ids = append(ids, request.Path[strings.LastIndex(request.Path, "/")+1:])
			}
		}
		return ids
	}

	t.Run("the second id is gone by the time it is removed", func(t *testing.T) {
		srv := apitest.NewServer(t)
		people(srv)
		srv.Script(http.MethodDelete, removal(srv, second), gone(t))
		_, err := newTestClient(t, srv, nil).RemoveAllowedPasskeys(t.Context(), srv.Key.CRID, []string{first, second, third})
		outcome := removalOutcome(t, err, []string{first}, []string{second}, []string{third})
		want := "access was taken away from " + first + ". Then no approved person had the device id " + second + " on this resource, and the command stopped. " + third + " still has access"
		if outcome.Headline() != want || outcome.Reason() != "" || !errors.Is(err, ErrApprovedPersonNotFound) {
			t.Fatalf("message = %q (not found: %t), want %q", outcome.Headline(), errors.Is(err, ErrApprovedPersonNotFound), want)
		}
		var problem *Error
		if !errors.As(err, &problem) || problem.StatusCode != http.StatusNotFound || problem.RequestID == "" {
			t.Fatalf("the service's answer is not in the chain: %v", err)
		}
		if sent := sentRemovals(srv); !slices.Equal(sent, []string{first, second}) {
			t.Fatalf("removals sent = %v, want the first two and not the third", sent)
		}
		if ids := approvedIDs(t, srv); !slices.Equal(ids, []string{second, third}) {
			t.Fatalf("approved people afterwards = %v: the outcome must match what happened", ids)
		}
	})
	t.Run("the first id is gone by the time it is removed", func(t *testing.T) {
		srv := apitest.NewServer(t)
		people(srv)
		srv.Script(http.MethodDelete, removal(srv, first), gone(t))
		_, err := newTestClient(t, srv, nil).RemoveAllowedPasskeys(t.Context(), srv.Key.CRID, []string{first, second})
		outcome := removalOutcome(t, err, nil, []string{first}, []string{second})
		want := "no approved person has the device id " + first + " on this resource, so nothing was removed. " + second + " still has access"
		if outcome.Headline() != want {
			t.Fatalf("message = %q, want %q", outcome.Headline(), want)
		}
	})
	t.Run("the last id is gone by the time it is removed", func(t *testing.T) {
		srv := apitest.NewServer(t)
		people(srv)
		srv.Script(http.MethodDelete, removal(srv, third), gone(t))
		_, err := newTestClient(t, srv, nil).RemoveAllowedPasskeys(t.Context(), srv.Key.CRID, []string{first, second, third})
		outcome := removalOutcome(t, err, []string{first, second}, []string{third}, nil)
		want := "access was taken away from " + first + " and " + second + ". Then no approved person had the device id " + third + " on this resource, and the command stopped"
		if outcome.Headline() != want {
			t.Fatalf("message = %q, want %q", outcome.Headline(), want)
		}
	})
	t.Run("a removal fails for another reason after one was made", func(t *testing.T) {
		srv := apitest.NewServer(t)
		people(srv)
		srv.Script(http.MethodDelete, removal(srv, second), func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteProblem(t, w, http.StatusServiceUnavailable, "service_unavailable", "Service Unavailable", "the resource is being changed; try again")
		})
		_, err := newTestClient(t, srv, nil).RemoveAllowedPasskeys(t.Context(), srv.Key.CRID, []string{first, second, third})
		outcome := removalOutcome(t, err, []string{first}, nil, []string{second, third})
		want := "access was taken away from " + first + ". Then taking it away from " + second + " failed, and the command stopped. " + second + " and " + third + " still have access"
		if outcome.Headline() != want || outcome.Reason() != "the resource is being changed; try again" {
			t.Fatalf("message = %q with reason %q, want %q", outcome.Headline(), outcome.Reason(), want)
		}
		var problem *Error
		if errors.Is(err, ErrApprovedPersonNotFound) || !errors.As(err, &problem) || problem.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("error = %v, want the failure's own exit class, not not-found", err)
		}
		if sent := sentRemovals(srv); !slices.Equal(sent, []string{first, second}) {
			t.Fatalf("removals sent = %v, want the first two, the second not retried", sent)
		}
	})
	t.Run("the first removal fails for another reason", func(t *testing.T) {
		srv := apitest.NewServer(t)
		people(srv)
		srv.Script(http.MethodDelete, removal(srv, first), func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteProblem(t, w, http.StatusServiceUnavailable, "service_unavailable", "Service Unavailable", "try again")
		})
		_, err := newTestClient(t, srv, nil).RemoveAllowedPasskeys(t.Context(), srv.Key.CRID, []string{first, second})
		var outcome *PasskeyRemovalError
		var problem *Error
		if errors.As(err, &outcome) || !errors.As(err, &problem) || problem.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("error = %v, want the plain failure: nothing was removed and nothing needs sorting out", err)
		}
		if ids := approvedIDs(t, srv); len(ids) != 3 {
			t.Fatalf("approved people afterwards = %v, want all three", ids)
		}
	})

	// A service that leaves the list out of its rows cannot be checked
	// against before the removals. They then find out one at a time, and the
	// outcome is as exact.
	t.Run("the list is not sent", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.AddApprovedPerson(first, testRequester)
		srv.OmitApprovedPeople()
		_, err := newTestClient(t, srv, nil).RemoveAllowedPasskeys(t.Context(), srv.Key.CRID, []string{first, second, third})
		if outcome := removalOutcome(t, err, []string{first}, []string{second}, []string{third}); !errors.Is(outcome, ErrApprovedPersonNotFound) {
			t.Fatalf("outcome = %v, want the not-found sentinel", outcome)
		}
		if sent := sentRemovals(srv); !slices.Equal(sent, []string{first, second}) {
			t.Fatalf("removals sent = %v, want the first two", sent)
		}
	})

	// On that service a removal of people who are all there is sent and
	// answered, and the resource read afterwards has no list either. A list
	// that is not there cannot show that the people are off it. So the
	// removal is not reported as done, and no resource is returned for the
	// caller to print as "nobody has access": the outcome says that each
	// removal was answered as made and that the list does not confirm it.
	t.Run("the list is not sent, and every removal is answered as made", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.AddApprovedPerson(first, testRequester)
		srv.AddApprovedPerson(third, testOtherPerson)
		srv.OmitApprovedPeople()
		resource, err := newTestClient(t, srv, nil).RemoveAllowedPasskeys(t.Context(), srv.Key.CRID, []string{first, third})
		if resource != nil {
			t.Fatalf("a removal the list does not confirm returned a resource: %+v", resource)
		}
		outcome := removalOutcome(t, err, []string{first, third}, nil, nil)
		want := "the service answered that access was taken away from " + first + " and " + third + ". The list does not confirm it: the service's answer afterwards does not show who has access now"
		if outcome.Headline() != want || outcome.Reason() != "" || !errors.Is(err, qurl.ErrInvalidAPIResponse) || errors.Is(err, ErrApprovedPersonNotFound) {
			t.Fatalf("outcome = %q, reason %q, error %v; want %q, no reason, and an answer that does not confirm", outcome.Headline(), outcome.Reason(), err, want)
		}
		base := "/v1/resources/" + srv.Key.CRID
		if got, want := requestLines(srv), []string{"GET " + base, "DELETE " + base + "/allowed-passkeys/" + first, "DELETE " + base + "/allowed-passkeys/" + third, "GET " + base}; !slices.Equal(got, want) {
			t.Fatalf("requests = %v, want %v", got, want)
		}
	})
}

// TestResourceRowsCarryAccessRequestsAndApprovedPeople pins the two members a
// resource read gains, and that a row from a service without them reads as
// "not said", never as off.
func TestResourceRowsCarryAccessRequestsAndApprovedPeople(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.SetAccessRequests(true)
	srv.AddApprovedPerson(testDeviceID, testRequester)
	resource, err := newTestClient(t, srv, nil).Resource(t.Context(), srv.Key.CRID)
	if err != nil || resource.AccessRequests == nil || !*resource.AccessRequests || len(resource.AllowedPasskeys) != 1 {
		t.Fatalf("Resource = %+v, %v", resource, err)
	}
	if person := resource.AllowedPasskeys[0]; person.DeviceID != testDeviceID || person.Name != testRequester || person.ApprovedAt == nil {
		t.Fatalf("approved person = %+v", person)
	}

	older := apitest.NewServer(t)
	older.PlayNoAccessRequests()
	resource, err = newTestClient(t, older, nil).Resource(t.Context(), older.Key.CRID)
	if err != nil || resource.AccessRequests != nil || resource.AllowedPasskeys != nil {
		t.Fatalf("Resource from a service without access requests = %+v, %v", resource, err)
	}

	bad := apitest.NewServer(t)
	bad.AddApprovedPerson("not-a-device-id", testRequester)
	if resource, err := newTestClient(t, bad, nil).Resource(t.Context(), bad.Key.CRID); resource != nil || !errors.Is(err, qurl.ErrInvalidAPIResponse) {
		t.Fatalf("a row with a malformed device id was accepted: %+v, %v", resource, err)
	}
}

// TestDeviceCredentialReachesEveryAccessRequestRoute pins that access
// requests work the way a real install uses them: with the device's own
// credential. That credential is used through the SDK, which sends only on
// the routes it lists, and refuses any other before anything leaves the
// machine. The five routes that exist only for access requests are on that
// list: both listings, the approval, the denial by code and the removal of an
// approved person are sent, each with the device credential, and each does
// what it says. So are the three requests that use routes the SDK listed
// before: creating a resource with access requests, changing the setting and
// reading the resource. The denial by device id has its own test.
func TestDeviceCredentialReachesEveryAccessRequestRoute(t *testing.T) {
	srv := apitest.NewServer(t)
	client := newRegisteredTestClient(t, srv)
	crid := srv.Key.CRID

	if result, err := client.Publish(t.Context(), "https://example.com/data", PublishOptions{AllowRequests: true}); err != nil || result.AccessRequests == nil || !*result.AccessRequests {
		t.Fatalf("publish with access requests through a device credential = %+v, %v", result, err)
	}
	if resource, err := client.SetAccessRequests(t.Context(), crid, false); err != nil || *resource.AccessRequests {
		t.Fatalf("turning access requests off through a device credential = %+v, %v", resource, err)
	}
	if resource, err := client.SetAccessRequests(t.Context(), crid, true); err != nil || !*resource.AccessRequests {
		t.Fatalf("turning access requests on through a device credential = %+v, %v", resource, err)
	}
	// Two people ask. The second person is listed first, so a call that took
	// "the first row" instead of the code it was given would show here.
	srv.AddAccessRequest(testOtherCode, testOtherPerson, testOtherDevice)
	srv.AddAccessRequest(testRequestCode, testRequester, testDeviceID)
	sent := len(srv.Requests())

	devices := func(list *AccessRequestList) []string {
		out := make([]string, 0, len(list.Requests))
		for index := range list.Requests {
			out = append(out, list.Requests[index].DeviceID)
		}
		return out
	}
	all, err := client.AccessRequests(t.Context(), "")
	if err != nil || !slices.Equal(devices(all), []string{testOtherDevice, testDeviceID}) || all.Requests[1].CRID != crid {
		t.Fatalf("listing of every resource = %+v, %v", all, err)
	}
	one, err := client.AccessRequests(t.Context(), crid)
	if err != nil || !slices.Equal(devices(one), []string{testOtherDevice, testDeviceID}) {
		t.Fatalf("listing of one resource = %+v, %v", one, err)
	}
	person, err := client.ApproveAccessRequest(t.Context(), crid, testRequestCode)
	if err != nil || person.DeviceID != testDeviceID || person.Name != testRequester {
		t.Fatalf("approval = %+v, %v, want the person who was given that code", person, err)
	}
	if err := client.DenyAccessRequest(t.Context(), crid, testOtherCode); err != nil {
		t.Fatalf("denial: %v", err)
	}
	if left, err := client.AccessRequests(t.Context(), crid); err != nil || len(left.Requests) != 0 {
		t.Fatalf("requests after one approval and one denial = %+v, %v, want none", left, err)
	}
	resource, err := client.RemoveAllowedPasskeys(t.Context(), crid, []string{testDeviceID})
	if err != nil || len(resource.AllowedPasskeys) != 0 {
		t.Fatalf("removal = %+v, %v, want nobody left on the list", resource, err)
	}

	base := "/v1/resources/" + crid
	want := []string{
		"GET /v1/access-requests",
		"GET " + base + "/access-requests",
		"POST " + base + "/access-requests/" + testRequestCode + "/approve",
		"DELETE " + base + "/access-requests/" + testOtherCode,
		"GET " + base + "/access-requests",
		"GET " + base,
		"DELETE " + base + "/allowed-passkeys/" + testDeviceID,
		"GET " + base,
	}
	if got := requestLines(srv)[sent:]; !slices.Equal(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	credential := "Bearer " + registeredAPIState(t).DeviceAPIKey
	for _, request := range srv.Requests() {
		if got := request.Header.Get("Authorization"); got != credential {
			t.Errorf("%s %s authorization = %q, want the device credential", request.Method, request.Path, got)
		}
	}
}

// TestDeviceCredentialStillRefusesWhatIsNotARoute pins that the SDK's list is
// still a list. A request code or a device id that could never be one is
// refused by this client before the SDK is asked, and an identifier that
// could never name a resource is refused by the SDK. Nothing is sent in
// either case, and neither is shown as a missing feature of this release.
func TestDeviceCredentialStillRefusesWhatIsNotARoute(t *testing.T) {
	srv := apitest.NewServer(t)
	client := newRegisteredTestClient(t, srv)
	for name, call := range map[string]func() error{
		"a code that is not six digits": func() error {
			_, err := client.ApproveAccessRequest(t.Context(), srv.Key.CRID, "12345a")
			return err
		},
		"a device id in another form": func() error {
			_, err := client.RemoveAllowedPasskeys(t.Context(), srv.Key.CRID, []string{"ABCD-EFGH-2345-MNOP"})
			return err
		},
		"an identifier that is not one": func() error {
			_, err := client.AccessRequests(t.Context(), "not a resource")
			return err
		},
		"a denial for an identifier that is not one": func() error {
			return client.DenyAccessRequest(t.Context(), "a/b", testRequestCode)
		},
		// A denial by device id is a route. The same denial for an
		// identifier that could never name a resource is not.
		"a denial by device id for an identifier that is not one": func() error {
			return client.DenyAccessRequest(t.Context(), "a/b", testDeviceID)
		},
		"a denial by something that is neither a device id nor a code": func() error {
			return client.DenyAccessRequest(t.Context(), srv.Key.CRID, "ABCD-EFGH-2345-MNOP")
		},
	} {
		err := call()
		if err == nil || len(srv.Requests()) != 0 {
			t.Errorf("%s: error %v after %d requests, want a refusal before any request", name, err, len(srv.Requests()))
			continue
		}
		var shown interface{ UserMessage() string }
		if errors.As(err, &shown) && strings.Contains(shown.UserMessage(), "release") {
			t.Errorf("%s: the refusal reads as a missing feature of this release: %q", name, shown.UserMessage())
		}
	}
}

// TestAccessRequestMethodsHandleTheIdentifierOneWay pins that every request
// here that names a resource treats its identifier as a grant change does:
// surrounding space is trimmed, and an identifier that is empty or could
// never name a resource is refused before any request. A listing with no
// identifier is the listing of every resource, the one call an empty value
// has a meaning for.
func TestAccessRequestMethodsHandleTheIdentifierOneWay(t *testing.T) {
	calls := map[string]struct {
		call func(Client, string) error
		// sent is the first request for the trimmed identifier.
		sent func(base string) string
	}{
		"setting": {
			call: func(client Client, id string) error {
				_, err := client.SetAccessRequests(t.Context(), id, true)
				return err
			},
			sent: func(base string) string { return "PATCH " + base },
		},
		"listing": {
			call: func(client Client, id string) error { _, err := client.AccessRequests(t.Context(), id); return err },
			sent: func(base string) string { return "GET " + base + "/access-requests" },
		},
		"approval": {
			call: func(client Client, id string) error {
				_, err := client.ApproveAccessRequest(t.Context(), id, testRequestCode)
				return err
			},
			sent: func(base string) string { return "POST " + base + "/access-requests/" + testRequestCode + "/approve" },
		},
		"denial": {
			call: func(client Client, id string) error {
				return client.DenyAccessRequest(t.Context(), id, testRequestCode)
			},
			sent: func(base string) string { return "DELETE " + base + "/access-requests/" + testRequestCode },
		},
		"removal": {
			call: func(client Client, id string) error {
				_, err := client.RemoveAllowedPasskeys(t.Context(), id, []string{testDeviceID})
				return err
			},
			sent: func(base string) string { return "GET " + base },
		},
	}
	for name, test := range calls {
		srv := apitest.NewServer(t)
		srv.AddAccessRequest(testRequestCode, testRequester, testDeviceID)
		srv.AddApprovedPerson(testDeviceID, testRequester)
		if err := test.call(newTestClient(t, srv, nil), " \t"+srv.Key.CRID+"\n"); err != nil {
			t.Errorf("%s with a padded identifier: %v", name, err)
			continue
		}
		if lines, want := requestLines(srv), test.sent("/v1/resources/"+srv.Key.CRID); len(lines) == 0 || lines[0] != want {
			t.Errorf("%s: requests = %v, want the first to be %s", name, lines, want)
		}

		for _, id := range []string{"a/b", "a b", "a%2Fb", "../" + srv.Key.CRID, srv.Key.CRID + "?x=1", srv.Key.CRID + "#x"} {
			srv := apitest.NewServer(t)
			if err := test.call(newTestClient(t, srv, nil), id); err == nil || len(srv.Requests()) != 0 {
				t.Errorf("%s with identifier %q: error %v after %d requests, want a refusal before any request", name, id, err, len(srv.Requests()))
			}
		}
		if name == "listing" {
			continue
		}
		for _, id := range []string{"", "  \n"} {
			srv := apitest.NewServer(t)
			if err := test.call(newTestClient(t, srv, nil), id); !errors.Is(err, qurl.ErrInvalidResourceRequest) || len(srv.Requests()) != 0 {
				t.Errorf("%s with an empty identifier: error %v after %d requests, want an invalid request and nothing sent", name, err, len(srv.Requests()))
			}
		}
	}
}

// TestApprovedPeopleSaidToBeNobodyIsNotTheSameAsNotSaid pins the difference a
// caller needs before it tells a publisher who still has access. A row that
// says nobody was approved reads as an empty list. A row without the member
// reads as nil: the service did not say. That holds for a read and for the
// answer to a change.
func TestApprovedPeopleSaidToBeNobodyIsNotTheSameAsNotSaid(t *testing.T) {
	read := func(t *testing.T, srv *apitest.Server) (fromRead, fromChange []AllowedPasskey) {
		t.Helper()
		client := newTestClient(t, srv, nil)
		resource, err := client.Resource(t.Context(), srv.Key.CRID)
		if err != nil {
			t.Fatal(err)
		}
		changed, err := client.SetAccessRequests(t.Context(), srv.Key.CRID, false)
		if err != nil {
			t.Fatal(err)
		}
		return resource.AllowedPasskeys, changed.AllowedPasskeys
	}

	srv := apitest.NewServer(t)
	if fromRead, fromChange := read(t, srv); fromRead == nil || len(fromRead) != 0 || fromChange == nil || len(fromChange) != 0 {
		t.Fatalf("nobody approved: read %#v, change %#v; want two empty lists that are not nil", fromRead, fromChange)
	}
	srv = apitest.NewServer(t)
	srv.AddApprovedPerson(testDeviceID, testRequester)
	if fromRead, fromChange := read(t, srv); len(fromRead) != 1 || len(fromChange) != 1 {
		t.Fatalf("one person approved: read %#v, change %#v", fromRead, fromChange)
	}
	srv = apitest.NewServer(t)
	srv.AddApprovedPerson(testDeviceID, testRequester)
	srv.OmitApprovedPeople()
	if fromRead, fromChange := read(t, srv); fromRead != nil || fromChange != nil {
		t.Fatalf("the list left out: read %#v, change %#v; want nil for both, never an empty list", fromRead, fromChange)
	}
}

// TestServiceThatRefusesTheAccessRequestsMember pins the second way a service
// from before access requests answers a request that carries the setting. It
// does not ignore the member: it validates the body strictly and refuses it
// with its generic validation problem, before anything is created or changed.
// A publisher must be told what the other commands tell them, that the
// service does not offer access requests, not "validation error".
//
// The client does not read the problem's words. It asks the service whether
// it has access requests, with the listing that exists only when it does.
func TestServiceThatRefusesTheAccessRequestsMember(t *testing.T) {
	const listing = "GET /v1/access-requests"

	t.Run("publish", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.PlayStrictWithoutAccessRequests()
		result, err := newTestClient(t, srv, nil).Publish(t.Context(), "https://example.com/data", PublishOptions{AllowRequests: true})
		wantUnsupported(t, err)
		if result != nil || err.Error() != msgAccessRequestsUnsupported+msgAccessRequestsCreateRefused {
			t.Fatalf("publish = %+v, %v", result, err)
		}
		for _, part := range []string{"Nothing was published", "run the command again without --allow-requests to publish the resource as private"} {
			if !strings.Contains(err.Error(), part) {
				t.Fatalf("the message lost %q: %v", part, err)
			}
		}
		if lines := requestLines(srv); !slices.Equal(lines, []string{"POST /v1/resources", listing}) {
			t.Fatalf("requests = %v, want the create and the one question", lines)
		}
		// Without the flag that service publishes as it always did, and the
		// advice in the message is true.
		plain, err := newTestClient(t, srv, nil).Publish(t.Context(), "https://example.com/data", PublishOptions{})
		if err != nil || plain.Private == nil || !*plain.Private || plain.AccessRequests != nil {
			t.Fatalf("a plain publish against that service = %+v, %v", plain, err)
		}
	})

	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprintf("setting on=%t", on), func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.PlayStrictWithoutAccessRequests()
			resource, err := newTestClient(t, srv, nil).SetAccessRequests(t.Context(), srv.Key.CRID, on)
			wantUnsupported(t, err)
			if resource != nil || err.Error() != msgAccessRequestsUnsupported {
				t.Fatalf("SetAccessRequests = %+v, %v, want the message every other command gives", resource, err)
			}
			if lines := requestLines(srv); !slices.Equal(lines, []string{"PATCH /v1/resources/" + srv.Key.CRID, listing}) {
				t.Fatalf("requests = %v, want the change and the one question", lines)
			}
		})
	}

	// A service that has access requests and refuses the request has a
	// reason of its own. Its problem is shown as it is.
	t.Run("a real refusal of the setting", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.SetResourceAccess(false)
		_, err := newTestClient(t, srv, nil).SetAccessRequests(t.Context(), srv.Key.CRID, true)
		var problem *Error
		if errors.Is(err, ErrAccessRequestsUnsupported) || !errors.As(err, &problem) || problem.StatusCode != http.StatusBadRequest || !strings.Contains(problem.Detail, "private resource") {
			t.Fatalf("error = %v, want the service's own refusal", err)
		}
		if lines := requestLines(srv); !slices.Equal(lines, []string{"PATCH /v1/resources/" + srv.Key.CRID, listing}) {
			t.Fatalf("requests = %v", lines)
		}
	})
	t.Run("a real refusal of a create", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteProblem(t, w, http.StatusBadRequest, "invalid_input", "Invalid Input", "target_url is not allowed")
		})
		_, err := newTestClient(t, srv, nil).Publish(t.Context(), "https://example.com/data", PublishOptions{AllowRequests: true})
		var problem *Error
		if errors.Is(err, ErrAccessRequestsUnsupported) || !errors.As(err, &problem) || problem.Detail != "target_url is not allowed" {
			t.Fatalf("error = %v, want the service's own refusal", err)
		}
	})

	// The question is asked only when its answer can matter: for a 400, to a
	// request that carried the member, that is not a conflict with a
	// resource that exists. A conflict means the service read the request.
	for _, test := range []struct {
		name   string
		opts   PublishOptions
		status int
		code   string
	}{
		{name: "no access requests asked for", status: http.StatusBadRequest, code: "validation_error"},
		{name: "a conflict with a resource that exists", opts: PublishOptions{AllowRequests: true}, status: http.StatusBadRequest, code: apitest.CodePrivacyMismatch},
		{name: "another status", opts: PublishOptions{AllowRequests: true}, status: http.StatusInternalServerError, code: "internal_error"},
		{name: "unauthorized", opts: PublishOptions{AllowRequests: true}, status: http.StatusUnauthorized, code: "unauthorized"},
	} {
		t.Run("no question: "+test.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.PlayStrictWithoutAccessRequests()
			srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteProblem(t, w, test.status, test.code, "Refused", "d")
			})
			_, err := newTestClient(t, srv, nil).Publish(t.Context(), "https://example.com/data", test.opts)
			if err == nil || errors.Is(err, ErrAccessRequestsUnsupported) {
				t.Fatalf("error = %v, want the refusal as it is", err)
			}
			if lines := requestLines(srv); !slices.Equal(lines, []string{"POST /v1/resources"}) {
				t.Fatalf("requests = %v, want the create alone", lines)
			}
		})
	}

	// When the question cannot be answered, the refusal stands as it is: the
	// client claims nothing it did not learn.
	t.Run("the question is not answered", func(t *testing.T) {
		for _, status := range []int{http.StatusServiceUnavailable, http.StatusForbidden} {
			srv := apitest.NewServer(t)
			srv.PlayStrictWithoutAccessRequests()
			srv.ScriptRepeat(http.MethodGet, "/v1/access-requests", 8, func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteProblem(t, w, status, "refused", "Refused", "d")
			})
			_, err := newTestClient(t, srv, nil).SetAccessRequests(t.Context(), srv.Key.CRID, true)
			var problem *Error
			if errors.Is(err, ErrAccessRequestsUnsupported) || !errors.As(err, &problem) || problem.StatusCode != http.StatusBadRequest {
				t.Fatalf("listing answered %d: error = %v, want the change's own 400", status, err)
			}
		}
	})
}
