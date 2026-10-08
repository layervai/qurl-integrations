package qurlapi

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

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
