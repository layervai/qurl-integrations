package qurlapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

// The three approved people of the removal tests.
const (
	removalFirst  = "aaaa-aaaa-aaaa-aaaa"
	removalSecond = "bbbb-bbbb-bbbb-bbbb"
	removalThird  = "cccc-cccc-cccc-cccc"
)

// finishRemoval is the part of a removal that runs after access was taken
// away from the first person. Its failure type is the outcome that says so,
// and this line stops compiling when that type becomes a plain error: a path
// in it could then return a failure that says nothing about the people who
// already lost access.
var _ func(*client, context.Context, string, *removalProgress) (*ResourceSummary, *PasskeyRemovalError) = (*client).finishRemoval

// fault is an answer the test gives in place of the service: a failure to
// reach it, or a status with a body.
type fault struct {
	err    error
	status int
	body   string
}

// faultyTransport sends each request to the mock unless answer gives a fault
// for it. n counts the requests of one client from 1.
type faultyTransport struct {
	next   http.RoundTripper
	n      int
	answer func(n int, req *http.Request) *fault
}

func (f *faultyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.n++
	given := f.answer(f.n, req)
	if given == nil {
		return f.next.RoundTrip(req)
	}
	if given.err != nil {
		return nil, given.err
	}
	return &http.Response{
		StatusCode: given.status, Status: http.StatusText(given.status), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(given.body)), Request: req,
	}, nil
}

// faultyClient is a client for the mock whose requests go through answer.
func faultyClient(t *testing.T, srv *apitest.Server, answer func(n int, req *http.Request) *fault) Client {
	t.Helper()
	client, err := New(&Config{
		BaseURL: srv.URL, APIKey: "lv_test_apitestingvalue123456789", Version: "test",
		NewRequestID: func() string { return testRequestID }, Sleep: func(time.Duration) {},
		HTTPClient: &http.Client{Transport: &faultyTransport{next: srv.Client().Transport, answer: answer}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// problemBody is the service's problem document.
func problemBody(status int, code, detail string) string {
	return fmt.Sprintf(`{"error":{"title":%q,"status":%d,"detail":%q,"code":%q},"meta":{"request_id":"req_fault"}}`, http.StatusText(status), status, detail, code)
}

// threeApproved is a mock with the three people approved.
func threeApproved(t *testing.T) *apitest.Server {
	t.Helper()
	srv := apitest.NewServer(t)
	for _, deviceID := range []string{removalFirst, removalSecond, removalThird} {
		srv.AddApprovedPerson(deviceID, "person "+deviceID)
	}
	return srv
}

// isRemovalOf reports whether req is the DELETE that removes deviceID.
func isRemovalOf(req *http.Request, deviceID string) bool {
	return req.Method == http.MethodDelete && strings.HasSuffix(req.URL.Path, "/allowed-passkeys/"+deviceID)
}

// TestRemovalThatTookAccessAwaySaysSoOnEveryFailure has one case for each way
// a removal can fail after its first person is off the list. Each must come
// back as the outcome: who lost access, what failed, who still has access as
// far as the command knows, and the command that shows the list as it is.
// None may be a plain error, which would read as "nothing happened", and the
// same command run again would then say that nothing was removed.
func TestRemovalThatTookAccessAwaySaysSoOnEveryFailure(t *testing.T) {
	unavailable := &fault{status: http.StatusServiceUnavailable, body: problemBody(http.StatusServiceUnavailable, "service_unavailable", "the resource is being changed; try again")}
	notFound := &fault{status: http.StatusNotFound, body: problemBody(http.StatusNotFound, "not_found", "nothing here")}
	for _, test := range []struct {
		name string
		// answer decides the faults. after says that the request that
		// removes the second person was seen.
		answer func(srv *apitest.Server, after *bool) func(int, *http.Request) *fault
		// removed, notFound and notRemoved are the outcome's three lists.
		removed, notFound, notRemoved []string
		// headline is the outcome's own sentence, and reason what follows
		// it, empty when the headline is the whole reason. anyReason says
		// that there is one and the test does not pin its words.
		headline, reason string
		anyReason        bool
		// nextStep is what the outcome says to do next, with %s for the
		// CRID. Empty means the command that shows who has access now.
		nextStep string
		// is must match the error, and isNot must not.
		is, isNot error
		// approved is who the mock still lists afterwards.
		approved []string
	}{
		{
			// The second removal is refused.
			name: "a later removal fails",
			answer: func(*apitest.Server, *bool) func(int, *http.Request) *fault {
				return func(_ int, req *http.Request) *fault {
					if isRemovalOf(req, removalSecond) {
						return unavailable
					}
					return nil
				}
			},
			removed: []string{removalFirst}, notRemoved: []string{removalSecond, removalThird},
			headline: "access was taken away from " + removalFirst + ". Then taking it away from " + removalSecond + " failed, and the command stopped. " + removalSecond + " and " + removalThird + " still have access",
			reason:   "the resource is being changed; try again",
			isNot:    ErrApprovedPersonNotFound, approved: []string{removalSecond, removalThird},
		},
		{
			// The second removal gets no answer at all.
			name: "a later removal gets no answer",
			answer: func(*apitest.Server, *bool) func(int, *http.Request) *fault {
				return func(_ int, req *http.Request) *fault {
					if isRemovalOf(req, removalSecond) {
						return &fault{err: errors.New("connection reset")}
					}
					return nil
				}
			},
			removed: []string{removalFirst}, notRemoved: []string{removalSecond, removalThird},
			headline: "access was taken away from " + removalFirst + ". Then taking it away from " + removalSecond + " failed, and the command stopped. " + removalSecond + " and " + removalThird + " still have access",
			reason:   "connection reset",
			isNot:    ErrApprovedPersonNotFound, approved: []string{removalSecond, removalThird},
		},
		{
			// The second person is gone by the time they are removed.
			name: "a later person is not found",
			answer: func(*apitest.Server, *bool) func(int, *http.Request) *fault {
				return func(_ int, req *http.Request) *fault {
					if isRemovalOf(req, removalSecond) {
						return notFound
					}
					return nil
				}
			},
			removed: []string{removalFirst}, notFound: []string{removalSecond}, notRemoved: []string{removalThird},
			headline: "access was taken away from " + removalFirst + ". Then no approved person had the device id " + removalSecond + " on this resource, and the command stopped. " + removalThird + " still has access",
			is:       ErrApprovedPersonNotFound, approved: []string{removalSecond, removalThird},
		},
		{
			// The second removal is answered "not found", and the read that
			// would say what was not found fails.
			name: "not found, and the read that would explain it fails",
			answer: func(_ *apitest.Server, after *bool) func(int, *http.Request) *fault {
				return func(_ int, req *http.Request) *fault {
					switch {
					case isRemovalOf(req, removalSecond):
						*after = true
						return notFound
					case *after:
						return unavailable
					}
					return nil
				}
			},
			removed: []string{removalFirst}, notFound: []string{removalSecond}, notRemoved: []string{removalThird},
			headline: "access was taken away from " + removalFirst + ". Then the service found nothing to remove for " + removalSecond + ", and the command stopped before it could find out why. " + removalThird + " still has access",
			reason:   "the resource is being changed; try again",
			isNot:    ErrApprovedPersonNotFound, approved: []string{removalSecond, removalThird},
		},
		{
			// The same, and the read says that the resource is gone.
			name: "not found, and the resource is gone",
			answer: func(_ *apitest.Server, after *bool) func(int, *http.Request) *fault {
				return func(_ int, req *http.Request) *fault {
					if isRemovalOf(req, removalSecond) {
						*after = true
					}
					if *after {
						return notFound
					}
					return nil
				}
			},
			removed: []string{removalFirst}, notFound: []string{removalSecond}, notRemoved: []string{removalThird},
			headline: "access was taken away from " + removalFirst + ". Then the service found nothing to remove for " + removalSecond + ", and the command stopped before it could find out why. " + removalThird + " still has access",
			reason:   "nothing here",
			isNot:    ErrApprovedPersonNotFound, approved: []string{removalSecond, removalThird},
		},
		{
			// The same, and the service turns out to have no access
			// requests: the mock becomes one that has none between the two
			// removals.
			name: "not found, and the service has no access requests",
			answer: func(srv *apitest.Server, _ *bool) func(int, *http.Request) *fault {
				return func(_ int, req *http.Request) *fault {
					if isRemovalOf(req, removalSecond) {
						srv.PlayNoAccessRequests()
					}
					return nil
				}
			},
			removed: []string{removalFirst}, notFound: []string{removalSecond}, notRemoved: []string{removalThird},
			headline: "access was taken away from " + removalFirst + ". Then the service found nothing to remove for " + removalSecond + ", and the command stopped before it could find out why. " + removalThird + " still has access",
			reason:   unsupportedStart,
			is:       ErrAccessRequestsUnsupported, isNot: ErrApprovedPersonNotFound,
		},
		{
			// Everyone is removed, and the list cannot be read again.
			name: "the list cannot be read after the removals",
			answer: func(_ *apitest.Server, after *bool) func(int, *http.Request) *fault {
				return func(_ int, req *http.Request) *fault {
					if isRemovalOf(req, removalThird) {
						*after = true
						return nil
					}
					if *after {
						return unavailable
					}
					return nil
				}
			},
			removed:  []string{removalFirst, removalSecond, removalThird},
			headline: "access was taken away from " + removalFirst + ", " + removalSecond + " and " + removalThird + ". Then the list of who has access could not be read",
			reason:   "the resource is being changed; try again",
			isNot:    ErrApprovedPersonNotFound, approved: []string{},
		},
		{
			// Everyone is removed, and the answer to the read is not one.
			name: "the list that is read after the removals cannot be understood",
			answer: func(_ *apitest.Server, after *bool) func(int, *http.Request) *fault {
				return func(_ int, req *http.Request) *fault {
					if isRemovalOf(req, removalThird) {
						*after = true
						return nil
					}
					if *after {
						return &fault{status: http.StatusOK, body: `{"data":{}}`}
					}
					return nil
				}
			},
			removed:   []string{removalFirst, removalSecond, removalThird},
			headline:  "access was taken away from " + removalFirst + ", " + removalSecond + " and " + removalThird + ". Then the list of who has access could not be read",
			anyReason: true,
			is:        qurl.ErrInvalidAPIResponse, isNot: ErrApprovedPersonNotFound, approved: []string{},
		},
		{
			// Everyone is removed, and the resource the service sends
			// afterwards has no list of approved people, though the one
			// it sent before the removals had it. A missing list is "not
			// said", not "nobody", so it confirms nothing: the removal was
			// sent and answered, and is not reported as done. One answer
			// left the list out. Nothing is said about the service, and
			// the next step is the read that showed the list a moment ago.
			name: "the list was there before the removals and is missing after",
			answer: func(srv *apitest.Server, _ *bool) func(int, *http.Request) *fault {
				return func(_ int, req *http.Request) *fault {
					if isRemovalOf(req, removalThird) {
						srv.OmitApprovedPeople()
					}
					return nil
				}
			},
			removed: []string{removalFirst, removalSecond, removalThird},
			headline: "the service answered that access was taken away from " + removalFirst + ", " + removalSecond + " and " + removalThird +
				". Its answer afterwards did not include the list of approved people, so the removal is not confirmed yet",
			is: qurl.ErrInvalidAPIResponse, isNot: ErrApprovedPersonNotFound,
		},
		{
			// The same, on a service that sent no list before the removals
			// either. This service does not show who has access, and the
			// read that would be the next step shows nothing more on it,
			// so it is not offered.
			name: "the list was missing before the removals as well",
			answer: func(srv *apitest.Server, _ *bool) func(int, *http.Request) *fault {
				return func(n int, _ *http.Request) *fault {
					if n == 1 {
						srv.OmitApprovedPeople()
					}
					return nil
				}
			},
			removed: []string{removalFirst, removalSecond, removalThird},
			headline: "the service answered that access was taken away from " + removalFirst + ", " + removalSecond + " and " + removalThird +
				". This service does not show who has access, so the removal cannot be confirmed from here",
			nextStep: "Nothing more can be learned with this command against this service: `qurl grants %s` does not show who has access either",
			is:       qurl.ErrInvalidAPIResponse, isNot: ErrApprovedPersonNotFound,
		},
		{
			// The service answers a removal as made and does not make it:
			// its list still shows the person. The list is what it says
			// now, so that person still has access.
			name: "the list still shows a person after the removals",
			answer: func(*apitest.Server, *bool) func(int, *http.Request) *fault {
				return func(_ int, req *http.Request) *fault {
					if isRemovalOf(req, removalSecond) {
						return &fault{status: http.StatusNoContent}
					}
					return nil
				}
			},
			removed: []string{removalFirst, removalThird}, notRemoved: []string{removalSecond},
			headline: "access was taken away from " + removalFirst + " and " + removalThird + ". The service answered the same for " + removalSecond + ", but its list still shows " + removalSecond + ". " + removalSecond + " still has access",
			is:       qurl.ErrInvalidAPIResponse, isNot: ErrApprovedPersonNotFound, approved: []string{removalSecond},
		},
		{
			// The same for every person: nobody lost access.
			name: "the list still shows everyone after the removals",
			answer: func(*apitest.Server, *bool) func(int, *http.Request) *fault {
				return func(_ int, req *http.Request) *fault {
					if req.Method == http.MethodDelete {
						return &fault{status: http.StatusNoContent}
					}
					return nil
				}
			},
			notRemoved: []string{removalFirst, removalSecond, removalThird},
			headline: "the service answered that access was taken away from " + removalFirst + ", " + removalSecond + " and " + removalThird + ", but its list still shows " +
				removalFirst + ", " + removalSecond + " and " + removalThird + ". " + removalFirst + ", " + removalSecond + " and " + removalThird + " still have access",
			is: qurl.ErrInvalidAPIResponse, isNot: ErrApprovedPersonNotFound, approved: []string{removalFirst, removalSecond, removalThird},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := threeApproved(t)
			after := false
			client := faultyClient(t, srv, test.answer(srv, &after))
			resource, err := client.RemoveAllowedPasskeys(t.Context(), srv.Key.CRID, []string{removalFirst, removalSecond, removalThird})
			if resource != nil {
				t.Fatalf("a removal that failed returned a resource: %+v", resource)
			}
			outcome := removalOutcome(t, err, test.removed, test.notFound, test.notRemoved)
			if got := outcome.Headline(); got != test.headline {
				t.Errorf("headline =\n%s\nwant\n%s", got, test.headline)
			}
			// The reason is the failure's own text. A case that names none
			// has none, or one this test does not pin: the text of an
			// answer that could not be understood.
			if got := outcome.Reason(); !strings.Contains(got, test.reason) || (test.reason == "" && got != "" && !test.anyReason) {
				t.Errorf("reason = %q, want %q in it", got, test.reason)
			}
			wantNext := "Run `qurl grants " + srv.Key.CRID + "` to see who has access now"
			if test.nextStep != "" {
				wantNext = fmt.Sprintf(test.nextStep, srv.Key.CRID)
			}
			if got := outcome.NextStep(); got != wantNext {
				t.Errorf("next step = %q, want %q", got, wantNext)
			}
			if test.is != nil && !errors.Is(err, test.is) {
				t.Errorf("error %v does not match %v", err, test.is)
			}
			if test.isNot != nil && errors.Is(err, test.isNot) {
				t.Errorf("error %v matches %v", err, test.isNot)
			}
			if test.approved != nil {
				if got := approvedIDs(t, srv); !slices.Equal(got, test.approved) {
					t.Errorf("the mock still lists %v, want %v", got, test.approved)
				}
			}
		})
	}
}

// TestNoFailureAfterARemovalIsAPlainError is the test that a failure path
// added later cannot escape. It does not list the paths. It makes a removal
// of three people fail at every request it sends after the first person is
// off the list, in every way a request can fail, and holds each result to
// the same rule: the error is the outcome, and the outcome names the person
// who lost access.
//
// The requests are counted on a run with no fault, so a request that is
// added to the removal later is faulted here without anyone editing this
// test. From the faulted request on, every request fails the same way, which
// also fails whatever the client asks next in order to explain the failure.
func TestNoFailureAfterARemovalIsAPlainError(t *testing.T) {
	named := []string{removalFirst, removalSecond, removalThird}

	// The run with no fault: how many requests, and which one removes the
	// first person.
	total, firstRemoval := 0, 0
	clean := threeApproved(t)
	if _, err := faultyClient(t, clean, func(n int, req *http.Request) *fault {
		total = n
		if firstRemoval == 0 && isRemovalOf(req, removalFirst) {
			firstRemoval = n
		}
		return nil
	}).RemoveAllowedPasskeys(t.Context(), clean.Key.CRID, named); err != nil {
		t.Fatal(err)
	}
	if firstRemoval == 0 || total <= firstRemoval {
		t.Fatalf("the run with no fault sent %d requests, the first removal was request %d", total, firstRemoval)
	}

	kinds := []struct {
		name string
		// honest says that this fault never tells the client that a
		// change was made when it was not.
		honest bool
		make   func() *fault
	}{
		{name: "no answer", honest: true, make: func() *fault { return &fault{err: errors.New("connection reset")} }},
		{name: "503", honest: true, make: func() *fault {
			return &fault{status: http.StatusServiceUnavailable, body: problemBody(http.StatusServiceUnavailable, "service_unavailable", "try again")}
		}},
		{name: "404", honest: true, make: func() *fault {
			return &fault{status: http.StatusNotFound, body: problemBody(http.StatusNotFound, "not_found", "nothing here")}
		}},
		{name: "403", honest: true, make: func() *fault {
			return &fault{status: http.StatusForbidden, body: problemBody(http.StatusForbidden, "forbidden", "not allowed")}
		}},
		{name: "429", honest: true, make: func() *fault {
			return &fault{status: http.StatusTooManyRequests, body: problemBody(http.StatusTooManyRequests, "rate_limited", "slow down")}
		}},
		{name: "500 that is not a problem document", honest: true, make: func() *fault { return &fault{status: http.StatusInternalServerError, body: "<html>oops</html>"} }},
		{name: "200 with an empty document", make: func() *fault { return &fault{status: http.StatusOK, body: `{}`} }},
		{name: "200 that is not a document", make: func() *fault { return &fault{status: http.StatusOK, body: `not json`} }},
		{name: "204", make: func() *fault { return &fault{status: http.StatusNoContent} }},
	}
	runs := 0
	for from := firstRemoval + 1; from <= total; from++ {
		for _, kind := range kinds {
			runs++
			where := fmt.Sprintf("every request from number %d on answered with %s", from, kind.name)
			srv := threeApproved(t)
			client := faultyClient(t, srv, func(n int, _ *http.Request) *fault {
				if n < from {
					return nil
				}
				return kind.make()
			})
			resource, err := client.RemoveAllowedPasskeys(t.Context(), srv.Key.CRID, named)
			if err == nil || resource != nil {
				t.Errorf("%s: the removal returned %+v, %v", where, resource, err)
				continue
			}
			var outcome *PasskeyRemovalError
			if !errors.As(err, &outcome) {
				t.Errorf("%s: access was taken away from %s and the failure does not say so: %v", where, removalFirst, err)
				continue
			}
			if !slices.Contains(outcome.Removed, removalFirst) || !strings.Contains(err.Error(), "access was taken away from "+removalFirst) {
				t.Errorf("%s: the outcome does not name the person who lost access: %v", where, err)
			}
			if !strings.Contains(err.Error(), "Run `qurl grants "+srv.Key.CRID+"` to see who has access now") {
				t.Errorf("%s: the outcome does not say how to see the list: %v", where, err)
			}
			// Every device id the command named is in exactly one list.
			all := slices.Concat(outcome.Removed, outcome.NotFound, outcome.NotRemoved)
			slices.Sort(all)
			if !slices.Equal(all, named) {
				t.Errorf("%s: the three lists hold %v, want each of %v once", where, all, named)
			}
			// What the outcome says was removed is what the mock removed,
			// for every fault that does not lie about a change.
			if kind.honest {
				left := approvedIDs(t, srv)
				for _, deviceID := range named {
					if gone, said := !slices.Contains(left, deviceID), slices.Contains(outcome.Removed, deviceID); gone != said {
						t.Errorf("%s: %s removed is %t at the service and %t in the outcome", where, deviceID, gone, said)
					}
				}
			}
		}
	}
	if want := (total - firstRemoval) * len(kinds); runs != want || runs == 0 {
		t.Fatalf("ran %d faulted removals, want %d", runs, want)
	}

	// The other half of the rule: a failure that is not the outcome is one
	// from before anyone lost access. Every request up to the first
	// removal is failed, in the ways that do not lie about a change, and
	// the mock must still list everyone.
	for from := 1; from <= firstRemoval; from++ {
		for _, kind := range kinds {
			if !kind.honest {
				continue
			}
			where := fmt.Sprintf("every request from number %d on answered with %s", from, kind.name)
			srv := threeApproved(t)
			client := faultyClient(t, srv, func(n int, _ *http.Request) *fault {
				if n < from {
					return nil
				}
				return kind.make()
			})
			_, err := client.RemoveAllowedPasskeys(t.Context(), srv.Key.CRID, named)
			if err == nil {
				t.Errorf("%s: the removal succeeded", where)
				continue
			}
			var outcome *PasskeyRemovalError
			if errors.As(err, &outcome) && len(outcome.Removed) > 0 {
				t.Errorf("%s: the outcome says that %v lost access before any removal was made", where, outcome.Removed)
			}
			if left := approvedIDs(t, srv); !slices.Equal(left, named) {
				t.Errorf("%s: the failure came before any removal, and the mock lists %v", where, left)
			}
		}
	}
}
