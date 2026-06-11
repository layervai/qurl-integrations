package main

// Tests for the conversations.members membership seam (container Slice 3b): request shape
// (channel + limit + bearer), member detection, bounded-scan paging (follows next_cursor up
// to maxMembershipPages then stops), and Slack ok:false → error.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/layervai/qurl-integrations/shared/auth"
)

type membershipReq struct{ query, auth string }

// membershipServer serves the given JSON bodies in order (clamped to the last) and records
// each request's query + Authorization header.
func membershipServer(t *testing.T, bodies ...string) (*httptest.Server, *[]membershipReq) {
	t.Helper()
	var got []membershipReq
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, membershipReq{query: r.URL.RawQuery, auth: r.Header.Get("Authorization")})
		body := bodies[len(bodies)-1]
		if i < len(bodies) {
			body = bodies[i]
		}
		i++
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestSlackChannelMembership_FoundAndRequestShape(t *testing.T) {
	srv, got := membershipServer(t, `{"ok":true,"members":["U1","U2","U3"]}`)
	fn := newSlackChannelMembershipFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "qurl-slack/test", srv.URL, srv.Client())

	member, err := fn(context.Background(), "T1", "", "C9", "U2")
	if err != nil || !member {
		t.Fatalf("U2 is in the channel: member=%v err=%v", member, err)
	}
	if len(*got) != 1 {
		t.Fatalf("a hit on the first page must not page further, got %d requests", len(*got))
	}
	req := (*got)[0]
	if req.auth != testBearerXoxb {
		t.Errorf("auth = %q", req.auth)
	}
	q, _ := url.ParseQuery(req.query)
	if q.Get("channel") != "C9" || q.Get("limit") != "1000" {
		t.Errorf("request query = %q (want channel=C9 limit=1000)", req.query)
	}
}

func TestSlackChannelMembership_NotFound(t *testing.T) {
	srv, _ := membershipServer(t, `{"ok":true,"members":["U1","U3"]}`)
	fn := newSlackChannelMembershipFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "qurl-slack/test", srv.URL, srv.Client())

	member, err := fn(context.Background(), "T1", "", "C9", "U2")
	if err != nil || member {
		t.Fatalf("U2 absent + no next page → not a member: member=%v err=%v", member, err)
	}
}

func TestSlackChannelMembership_PagingFindsOnSecondPage(t *testing.T) {
	srv, got := membershipServer(t,
		`{"ok":true,"members":["U1"],"response_metadata":{"next_cursor":"CURSOR2"}}`,
		`{"ok":true,"members":["U2"]}`,
	)
	fn := newSlackChannelMembershipFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "qurl-slack/test", srv.URL, srv.Client())

	member, err := fn(context.Background(), "T1", "", "C9", "U2")
	if err != nil || !member {
		t.Fatalf("U2 on page 2 → member: member=%v err=%v", member, err)
	}
	if len(*got) != 2 {
		t.Fatalf("want 2 requests (paged once), got %d", len(*got))
	}
	if q, _ := url.ParseQuery((*got)[1].query); q.Get("cursor") != "CURSOR2" {
		t.Errorf("second request must carry the cursor, query = %q", (*got)[1].query)
	}
}

func TestSlackChannelMembership_BoundedScanStopsAtTwoPages(t *testing.T) {
	// Every page has a next_cursor and never the user: the scan must STOP at
	// maxMembershipPages (2) rather than follow the cursor forever — the user reads as
	// not-confirmed (fail-closed degradation).
	srv, got := membershipServer(t,
		`{"ok":true,"members":["U1"],"response_metadata":{"next_cursor":"C1"}}`,
		`{"ok":true,"members":["U3"],"response_metadata":{"next_cursor":"C2"}}`,
		`{"ok":true,"members":["U2"],"response_metadata":{"next_cursor":"C3"}}`,
	)
	fn := newSlackChannelMembershipFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "qurl-slack/test", srv.URL, srv.Client())

	member, err := fn(context.Background(), "T1", "", "C9", "U2")
	if err != nil || member {
		t.Fatalf("a member beyond the 2-page bound must read as not-confirmed: member=%v err=%v", member, err)
	}
	if len(*got) != maxMembershipPages {
		t.Fatalf("the scan must stop at maxMembershipPages=%d, made %d requests", maxMembershipPages, len(*got))
	}
}

func TestSlackChannelMembership_SlackError(t *testing.T) {
	srv, _ := membershipServer(t, `{"ok":false,"error":"missing_scope"}`)
	fn := newSlackChannelMembershipFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "qurl-slack/test", srv.URL, srv.Client())

	if _, err := fn(context.Background(), "T1", "", "C9", "U2"); err == nil {
		t.Fatal("a Slack ok:false must surface as an error (the caller fails closed)")
	}
}

func TestSlackChannelMembership_GridFallback(t *testing.T) {
	// On Enterprise Grid the workspace bot token may be unconfigured; the seam retries on the
	// org token, mirroring the conversations.info seam.
	srv, _ := membershipServer(t, `{"ok":true,"members":["U2"]}`)
	var owners []string
	lookup := func(_ context.Context, ownerID string) (string, error) {
		owners = append(owners, ownerID)
		if ownerID == "T1" {
			return "", auth.ErrSlackBotTokenNotConfigured // workspace itself has no bot token
		}
		return "xoxb-e1", nil // the E1 org token
	}
	fn := newSlackChannelMembershipFuncWithTokenLookup(lookup, "qurl-slack/test", srv.URL, srv.Client())

	member, err := fn(context.Background(), "T1", "E1", "C9", "U2")
	if err != nil || !member {
		t.Fatalf("grid fallback membership: member=%v err=%v", member, err)
	}
	if len(owners) != 2 || owners[0] != "T1" || owners[1] != "E1" {
		t.Fatalf("grid fallback should try team then enterprise, got owners=%v", owners)
	}
}
