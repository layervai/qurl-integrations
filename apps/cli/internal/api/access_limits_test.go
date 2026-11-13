package qurlapi

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

// TestTurningAccessRequestsOnNeedsAnAnswerThatSaysPrivate pins the rule for
// the answer that turns access requests on. What a publisher is told next is
// that the resource's address is safe to send to anyone, because a private
// resource opens only for the people they allow. So the answer must say that
// the resource is private. An answer that says it is public, from a service
// that accepted the setting where it should have refused it, fails with a
// message that says so. An answer that does not say fails too. Neither
// returns a resource, and neither sends anything after the one change: the
// client turns nothing else on, and nothing back off, on its own.
func TestTurningAccessRequestsOnNeedsAnAnswerThatSaysPrivate(t *testing.T) {
	t.Run("the answer says public", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.SetResourceAccess(false)
		srv.AcceptAccessRequestsOnPublic()
		resource, err := newTestClient(t, srv, nil).SetAccessRequests(t.Context(), srv.Key.CRID, true)
		var shown interface{ UserMessage() string }
		if resource != nil || !errors.Is(err, qurl.ErrInvalidAPIResponse) || !errors.As(err, &shown) || shown.UserMessage() != msgAccessRequestsOnPublic {
			t.Fatalf("SetAccessRequests = %+v, %v; want the refusal for a public resource", resource, err)
		}
		if lines := requestLines(srv); !slices.Equal(lines, []string{"PATCH /v1/resources/" + srv.Key.CRID}) {
			t.Fatalf("requests = %v, want the one change and nothing after it", lines)
		}
		for _, part := range []string{"access requests are for a private resource", "this one is public", "anyone who has the CRID can open it", "`qurl requests <CRID> --off`"} {
			if !strings.Contains(msgAccessRequestsOnPublic, part) {
				t.Errorf("the message lost %q: %q", part, msgAccessRequestsOnPublic)
			}
		}
		if strings.Contains(msgAccessRequestsOnPublic, "safe to send") {
			t.Errorf("the message for a public resource says it is safe to send: %q", msgAccessRequestsOnPublic)
		}
		// Turning the setting off again needs no privacy, and works.
		off, err := newTestClient(t, srv, nil).SetAccessRequests(t.Context(), srv.Key.CRID, false)
		if err != nil || off.AccessRequests == nil || *off.AccessRequests {
			t.Fatalf("turning the setting off for a public resource = %+v, %v", off, err)
		}
	})
	t.Run("the answer does not say", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
				"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "type": "url", "status": "active", "access_requests": true,
			}, nil)
		})
		resource, err := newTestClient(t, srv, nil).SetAccessRequests(t.Context(), srv.Key.CRID, true)
		var shown interface{ UserMessage() string }
		if resource != nil || !errors.Is(err, qurl.ErrInvalidAPIResponse) || !errors.As(err, &shown) || shown.UserMessage() != msgAccessRequestsPrivacyNotSaid {
			t.Fatalf("SetAccessRequests = %+v, %v; want the refusal for an answer that does not say", resource, err)
		}
		if strings.Contains(msgAccessRequestsPrivacyNotSaid, "safe to send") || !strings.Contains(msgAccessRequestsPrivacyNotSaid, "does not say that this resource is private") {
			t.Errorf("the message = %q", msgAccessRequestsPrivacyNotSaid)
		}
	})
	t.Run("the answer says private", func(t *testing.T) {
		srv := apitest.NewServer(t)
		resource, err := newTestClient(t, srv, nil).SetAccessRequests(t.Context(), srv.Key.CRID, true)
		if err != nil || resource.Private == nil || !*resource.Private || resource.AccessRequests == nil || !*resource.AccessRequests {
			t.Fatalf("SetAccessRequests = %+v, %v", resource, err)
		}
	})

	// The publish that turns requests on for a target that is already
	// published uses the same change, and gets the same rule: an answer
	// that does not say private is the privacy failure, with no CRID and no
	// result, whether it says public or says nothing.
	for name, row := range map[string]func(*apitest.Server) map[string]any{
		"public": func(srv *apitest.Server) map[string]any {
			return map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "type": "url", "status": "active", "private": false, "access_requests": true}
		},
		"not said": func(srv *apitest.Server) map[string]any {
			return map[string]any{"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "type": "url", "status": "active", "access_requests": true}
		},
	} {
		t.Run("publish for a target that is already published, "+name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.SetPublishFoundExisting(true)
			srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteEnvelope(t, w, http.StatusOK, row(srv), nil)
			})
			result, err := newTestClient(t, srv, nil).Publish(t.Context(), "https://example.com/data", PublishOptions{AllowRequests: true})
			var named *AccessRequestsNotTurnedOnError
			if result != nil || errors.As(err, &named) || !errors.Is(err, qurl.ErrInvalidAPIResponse) || err.Error() != msgPrivateUnconfirmed || strings.Contains(err.Error(), srv.Key.CRID) {
				t.Fatalf("publish = %+v, %v, want the privacy failure without a CRID", result, err)
			}
		})
	}
}

// TestWrongCodeLimitIsShownAndNeverRetried pins what the client does with the
// service's limit on wrong codes. After too many wrong codes for a resource,
// an approval and a denial by code are answered "too many requests", with a
// wait that can be an hour, whatever the code, a right one included.
//
// The client sends such a request once and does not wait and try again by
// itself: another attempt could be one more wrong code. The error carries
// the service's own answer, its sentence and its wait, and is the "too many
// requests" problem for the exit code. A denial by device id cannot be a
// wrong guess at a code, and is neither limited nor counted.
func TestWrongCodeLimitIsShownAndNeverRetried(t *testing.T) {
	const wrong = "000000"
	seed := func(t *testing.T) (*apitest.Server, Client, *[]time.Duration) {
		t.Helper()
		srv := apitest.NewServer(t)
		srv.AddAccessRequest(testRequestCode, testRequester, testDeviceID)
		srv.AddAccessRequest(testOtherCode, testOtherPerson, testOtherDevice)
		var sleeps []time.Duration
		client := newTestClient(t, srv, &sleeps)
		for attempt := range apitest.WrongCodeLimit {
			_, err := client.ApproveAccessRequest(t.Context(), srv.Key.CRID, wrong)
			var limit *RequestCodeLimitError
			var problem *Error
			if errors.As(err, &limit) || !errors.As(err, &problem) || problem.StatusCode != http.StatusNotFound {
				t.Fatalf("wrong code number %d: error = %v, want the answer for a code that is not pending", attempt+1, err)
			}
		}
		return srv, client, &sleeps
	}
	limited := func(t *testing.T, srv *apitest.Server, sleeps *[]time.Duration, sent int, wantRoute string, err error) {
		t.Helper()
		var limit *RequestCodeLimitError
		if !errors.As(err, &limit) || limit.Problem == nil {
			t.Fatalf("error = %v, want the wrong-code limit", err)
		}
		if limit.Problem.StatusCode != http.StatusTooManyRequests || limit.Problem.RetryAfter != apitest.WrongCodesRetryAfter || limit.Problem.Detail != apitest.WrongCodesDetail || limit.Problem.RequestID == "" {
			t.Fatalf("the service's answer is not carried as it was: %+v", limit.Problem)
		}
		var problem *Error
		if !errors.As(err, &problem) || problem != limit.Problem {
			t.Fatalf("the service's problem is not in the chain for the exit code: %v", err)
		}
		if got := requestLines(srv)[sent:]; !slices.Equal(got, []string{wantRoute}) {
			t.Fatalf("requests = %v, want %q sent once and nothing else", got, wantRoute)
		}
		if len(*sleeps) != 0 {
			t.Fatalf("the client waited %v to try again by itself", *sleeps)
		}
	}
	base := func(srv *apitest.Server) string { return "/v1/resources/" + srv.Key.CRID + "/access-requests/" }

	for name, code := range map[string]string{"another wrong code": "111111", "the right code": testRequestCode} {
		t.Run("approve with "+name, func(t *testing.T) {
			srv, client, sleeps := seed(t)
			sent := len(srv.Requests())
			person, err := client.ApproveAccessRequest(t.Context(), srv.Key.CRID, code)
			if person != nil {
				t.Fatalf("an approval was made during the limit: %+v", person)
			}
			limited(t, srv, sleeps, sent, "POST "+base(srv)+code+"/approve", err)
			if got := approvedIDs(t, srv); len(got) != 0 {
				t.Fatalf("approved people = %v, want nobody", got)
			}
		})
		t.Run("deny with "+name, func(t *testing.T) {
			srv, client, sleeps := seed(t)
			sent := len(srv.Requests())
			limited(t, srv, sleeps, sent, "DELETE "+base(srv)+code, client.DenyAccessRequest(t.Context(), srv.Key.CRID, code))
			if pending := pendingDevices(t, srv); !slices.Equal(pending, []string{testDeviceID, testOtherDevice}) {
				t.Fatalf("pending = %v, want both requests", pending)
			}
		})
	}
	t.Run("deny by device id is not limited", func(t *testing.T) {
		srv, client, _ := seed(t)
		if err := client.DenyAccessRequest(t.Context(), srv.Key.CRID, testDeviceID); err != nil {
			t.Fatalf("denial by device id during the limit: %v", err)
		}
		if pending := pendingDevices(t, srv); !slices.Equal(pending, []string{testOtherDevice}) {
			t.Fatalf("pending = %v", pending)
		}
	})
	t.Run("denials by a device id that is not there are not counted", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.AddAccessRequest(testRequestCode, testRequester, testDeviceID)
		client := newTestClient(t, srv, nil)
		for range 2 * apitest.WrongCodeLimit {
			if err := client.DenyAccessRequest(t.Context(), srv.Key.CRID, "nope-nope-nope-nope"); err == nil {
				t.Fatal("a denial of a device id that is not there succeeded")
			}
		}
		if person, err := client.ApproveAccessRequest(t.Context(), srv.Key.CRID, testRequestCode); err != nil || person.DeviceID != testDeviceID {
			t.Fatalf("an approval after denials by device id = %+v, %v", person, err)
		}
	})
	t.Run("denials by a wrong code count like approvals", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.AddAccessRequest(testRequestCode, testRequester, testDeviceID)
		client := newTestClient(t, srv, nil)
		for range apitest.WrongCodeLimit {
			if err := client.DenyAccessRequest(t.Context(), srv.Key.CRID, wrong); err == nil {
				t.Fatal("a denial by a wrong code succeeded")
			}
		}
		_, err := client.ApproveAccessRequest(t.Context(), srv.Key.CRID, testRequestCode)
		var limit *RequestCodeLimitError
		if !errors.As(err, &limit) {
			t.Fatalf("an approval after too many wrong denials: %v, want the limit", err)
		}
	})
}

// TestRequestCodeIsNotInDiagnostics pins that the code of an access request
// is masked wherever a request path is written to a diagnostic surface: the
// six digits after /access-requests/, in the approval and in the denial. The
// publisher typed the code, but it gives a person access for as long as the
// request is pending, and a diagnostic line is what gets pasted into a chat
// or a ticket. A device id in the same place stays as it is, and so does
// text that is not a path.
func TestRequestCodeIsNotInDiagnostics(t *testing.T) {
	for _, test := range []struct{ in, want string }{
		{"> POST /v1/resources/qabc/access-requests/482913/approve", "> POST /v1/resources/qabc/access-requests/******/approve"},
		{"> DELETE /v1/resources/qabc/access-requests/482913", "> DELETE /v1/resources/qabc/access-requests/******"},
		{"request denied: DELETE /v1/resources/qabc/access-requests/482913 is not a route", "request denied: DELETE /v1/resources/qabc/access-requests/****** is not a route"},
		{"> DELETE /v1/resources/qabc/access-requests/abcd-efgh-2345-mnop", "> DELETE /v1/resources/qabc/access-requests/abcd-efgh-2345-mnop"},
		{"> DELETE /v1/resources/qabc/access-requests/2345-6723-2345-6723", "> DELETE /v1/resources/qabc/access-requests/2345-6723-2345-6723"},
		{"> GET /v1/resources/qabc/access-requests", "> GET /v1/resources/qabc/access-requests"},
		{"> GET /v1/access-requests", "> GET /v1/access-requests"},
		{"/access-requests/4829133", "/access-requests/4829133"},
		{"/access-requests/48291", "/access-requests/48291"},
		{"no pending request has the code 482 913 for this resource", "no pending request has the code 482 913 for this resource"},
		{"Retry after 482913s.", "Retry after 482913s."},
	} {
		if got := Redact(test.in); got != test.want {
			t.Errorf("Redact(%q) = %q, want %q", test.in, got, test.want)
		}
	}

	// Through the client: the lines it writes for an approval and for a
	// denial by code have the route and no code, and the line for a denial
	// by device id has the device id.
	srv := apitest.NewServer(t)
	srv.AddAccessRequest(testRequestCode, testRequester, testDeviceID)
	srv.AddAccessRequest(testOtherCode, testOtherPerson, testOtherDevice)
	srv.AddAccessRequest("660021", "", "ijkl-mnop-qrst-uvwx")
	var lines []string
	client, err := New(&Config{
		BaseURL: srv.URL, APIKey: "lv_test_apitestingvalue123456789", Version: "test", Sleep: func(time.Duration) {},
		Verbose: func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ApproveAccessRequest(t.Context(), srv.Key.CRID, testRequestCode); err != nil {
		t.Fatal(err)
	}
	if err := client.DenyAccessRequest(t.Context(), srv.Key.CRID, testOtherCode); err != nil {
		t.Fatal(err)
	}
	if err := client.DenyAccessRequest(t.Context(), srv.Key.CRID, "ijkl-mnop-qrst-uvwx"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ApproveAccessRequest(t.Context(), srv.Key.CRID, "000000"); err == nil {
		t.Fatal("an approval of a code that is not pending succeeded")
	}
	base := "/v1/resources/" + srv.Key.CRID + "/access-requests/"
	for _, want := range []string{"> POST " + base + "******/approve", "> DELETE " + base + "******", "> DELETE " + base + "ijkl-mnop-qrst-uvwx"} {
		if !slices.Contains(lines, want) {
			t.Errorf("the diagnostics lack %q: %q", want, lines)
		}
	}
	for _, line := range lines {
		for _, code := range []string{testRequestCode, testOtherCode, "000000"} {
			if strings.Contains(line, code) {
				t.Errorf("a diagnostic line has the code %s: %q", code, line)
			}
		}
	}
}

// TestSpacedRequestCode pins the one place a code is written as it is read
// aloud: two groups of three. The messages of this client and the output of
// the commands both use it, so the two cannot drift apart. A value that is
// not six characters is returned as it is.
func TestSpacedRequestCode(t *testing.T) {
	for in, want := range map[string]string{"482913": "482 913", "000000": "000 000", "": "", "48291": "48291", "4829133": "4829133", "abcd-efgh-2345-mnop": "abcd-efgh-2345-mnop"} {
		if got := SpacedRequestCode(in); got != want {
			t.Errorf("SpacedRequestCode(%q) = %q, want %q", in, got, want)
		}
	}
	if want := "no pending request has the code 482 913 for this resource"; !strings.HasPrefix(fmt.Sprintf(msgRequestCodeNotFound, SpacedRequestCode("482913")), want) {
		t.Errorf("the message for a code that is not pending does not write it that way")
	}
}
