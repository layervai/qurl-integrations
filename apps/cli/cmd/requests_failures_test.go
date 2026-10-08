package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// answerInPlace is an answer a test gives in place of the mock: a status and
// a body.
type answerInPlace struct {
	status int
	body   string
}

// chosenAnswers sends each request to the mock unless choose gives an answer
// for it.
type chosenAnswers struct {
	next   http.RoundTripper
	choose func(*http.Request) *answerInPlace
}

func (c *chosenAnswers) RoundTrip(req *http.Request) (*http.Response, error) {
	given := c.choose(req)
	if given == nil {
		return c.next.RoundTrip(req)
	}
	return &http.Response{
		StatusCode: given.status, Status: http.StatusText(given.status), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(given.body)), Request: req,
	}, nil
}

// withChosenAnswers returns run options whose API client is one for the
// mock, with choose in front of it.
func withChosenAnswers(srv *apitest.Server, choose func(*http.Request) *answerInPlace, args ...string) *runOpts {
	return &runOpts{
		args: append([]string{"--endpoint", srv.URL}, args...),
		openAPIClient: func(context.Context) (qurlapi.Client, error) {
			return qurlapi.New(&qurlapi.Config{
				BaseURL: srv.URL, APIKey: testAPIKey, Version: "test", Sleep: func(time.Duration) {},
				HTTPClient: &http.Client{Transport: &chosenAnswers{next: srv.Client().Transport, choose: choose}},
			})
		},
	}
}

// busy is the service's answer when it cannot serve a request now.
func busy() *answerInPlace {
	return &answerInPlace{
		status: http.StatusServiceUnavailable,
		body:   `{"error":{"title":"Service Unavailable","status":503,"detail":"the resource is being changed; try again","code":"service_unavailable"},"meta":{"request_id":"req_test"}}`,
	}
}

// removalOf reports whether req removes the approved person deviceID.
func removalOf(req *http.Request, deviceID string) bool {
	return req.Method == http.MethodDelete && strings.HasSuffix(req.URL.Path, "/allowed-passkeys/"+deviceID)
}

// twoApproved is a mock with one allowed device and two approved people.
func twoApproved(t *testing.T) *apitest.Server {
	t.Helper()
	srv := apitest.NewServer(t)
	srv.SetResourceAccess(true, grantKey(1))
	srv.AddApprovedPerson(requesterDevice, requesterName)
	srv.AddApprovedPerson(otherDevice, otherRequester)
	return srv
}

// compactJSON is a document without its indentation, its members in the
// order they were written.
func compactJSON(t *testing.T, document []byte) string {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, document); err != nil {
		t.Fatalf("not a JSON document: %q (%v)", document, err)
	}
	return out.String()
}

// TestGrantsRemoveSaysWhoLostAccessWhateverFailsNext pins, through the
// command, the failures that come after every person was removed. Each used
// to be the failure alone, which said nothing about the people who had lost
// access, so the same command run again said that nothing was removed.
//
// Now each says who lost access, what failed, who still has access as far
// as the command knows, and how to see the list as it is. In JSON mode the
// same outcome is the document on stdout. The exit code is the failure's.
func TestGrantsRemoveSaysWhoLostAccessWhateverFailsNext(t *testing.T) {
	modes := [][]string{nil, {"-o", "json"}, {"--quiet"}}

	// Both people are removed, and then the list cannot be read again.
	t.Run("the list cannot be read after the removals", func(t *testing.T) {
		for _, mode := range modes {
			srv := twoApproved(t)
			removed := false
			choose := func(req *http.Request) *answerInPlace {
				if removalOf(req, otherDevice) {
					removed = true
					return nil
				}
				if removed {
					return busy()
				}
				return nil
			}
			command := []string{"grants", srv.Key.CRID, "--remove", requesterDevice, "--remove", otherDevice}
			res := runCLI(t, withChosenAnswers(srv, choose, append(slices.Clone(command), mode...)...))
			wantStderr := "Error: access was taken away from " + requesterDevice + " and " + otherDevice + ". Then the list of who has access could not be read.\n\n" +
				"  the resource is being changed; try again\n\n" +
				"  Run `qurl grants " + srv.Key.CRID + "` to see who has access now.\n" +
				"  Request ID: req_test\n"
			if res.code != exitcode.Unavailable || res.stderr.String() != wantStderr {
				t.Fatalf("%v: exit = %d, want %d; stderr =\n%s\nwant\n%s", mode, res.code, exitcode.Unavailable, res.stderr.String(), wantStderr)
			}
			if len(mode) == 2 {
				want := `{"crid":"` + srv.Key.CRID + `","removed":["` + requesterDevice + `","` + otherDevice + `"],"not_found":[],"not_removed":[]}`
				if got := compactJSON(t, res.stdout.Bytes()); got != want {
					t.Fatalf("outcome document = %s, want %s", got, want)
				}
			} else {
				mustEmptyStdout(t, res)
			}
			// What the outcome said is what happened.
			if got := approvedDevices(t, srv); len(got) != 0 {
				t.Fatalf("%v: approved people afterwards = %v, want none", mode, got)
			}
			// The same command again finds them gone. That is the answer
			// the first run must never have left the publisher to work out.
			again := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL}, command...)})
			if again.code != exitcode.NotFound || !strings.Contains(again.stderr.String(), "so nothing was removed") {
				t.Fatalf("%v: the same command again: exit %d, stderr %q", mode, again.code, again.stderr.String())
			}
		}
	})

	// The same, in a command that also names public keys. The change to
	// the keys comes after the list is read again, so it was not made, and
	// it is all that is left to do: the outcome names the command for it.
	t.Run("the list cannot be read, and public keys were waiting", func(t *testing.T) {
		for _, mode := range modes {
			srv := twoApproved(t)
			removed := false
			choose := func(req *http.Request) *answerInPlace {
				if removalOf(req, otherDevice) {
					removed = true
					return nil
				}
				if removed {
					return busy()
				}
				return nil
			}
			command := []string{"grants", srv.Key.CRID, "--remove", requesterDevice, "--add", grantKey(2), "--remove", otherDevice, "--remove", grantKey(1)}
			res := runCLI(t, withChosenAnswers(srv, choose, append(slices.Clone(command), mode...)...))
			finish := "qurl grants " + srv.Key.CRID + " --add " + grantKey(2) + " --remove " + grantKey(1)
			wantStderr := "Error: access was taken away from " + requesterDevice + " and " + otherDevice + ". Then the list of who has access could not be read.\n\n" +
				"  the resource is being changed; try again\n\n" +
				"  No public key was added or removed: that change comes after the removals, and the command stopped before it.\n\n" +
				"  Run `qurl grants " + srv.Key.CRID + "` to see who has access now.\n" +
				"  To make the change to the public keys, run: " + finish + "\n" +
				"  Request ID: req_test\n"
			if res.code != exitcode.Unavailable || res.stderr.String() != wantStderr {
				t.Fatalf("%v: exit = %d, want %d; stderr =\n%s\nwant\n%s", mode, res.code, exitcode.Unavailable, res.stderr.String(), wantStderr)
			}
			if len(mode) == 2 {
				want := `{"crid":"` + srv.Key.CRID + `","removed":["` + requesterDevice + `","` + otherDevice + `"],"not_found":[],"not_removed":[],"public_keys_changed":false,"public_keys_command":"` + finish + `"}`
				if got := compactJSON(t, res.stdout.Bytes()); got != want {
					t.Fatalf("outcome document = %s, want %s", got, want)
				}
			} else {
				mustEmptyStdout(t, res)
			}
			for _, line := range requestLog(srv) {
				if strings.HasPrefix(line, "PATCH ") {
					t.Fatalf("%v: a change to the public keys was sent: %v", mode, requestLog(srv))
				}
			}
			// The command the outcome names finishes the job.
			done := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL}, append(strings.Fields(finish)[1:], "-o", "json")...)})
			var lists struct {
				Keys   []string          `json:"allowed_device_keys"`
				People []json.RawMessage `json:"approved_people"`
			}
			if err := json.Unmarshal(done.stdout.Bytes(), &lists); done.code != 0 || err != nil || !slices.Equal(lists.Keys, []string{grantKey(2)}) || len(lists.People) != 0 {
				t.Fatalf("%v: the finishing command: exit %d, %v, stdout %s, stderr %s", mode, done.code, err, done.stdout.String(), done.stderr.String())
			}
		}
	})

	// The service answers a removal as made and does not make it. The list
	// it sends afterwards is what it says now: that person still has
	// access, and the other one lost it.
	t.Run("the list still shows a person after the removals", func(t *testing.T) {
		for _, mode := range modes {
			srv := twoApproved(t)
			srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID+"/allowed-passkeys/"+otherDevice, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})
			res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "--remove", requesterDevice, "--remove", otherDevice}, mode...)})
			wantStderr := "Error: access was taken away from " + requesterDevice + ". The service answered the same for " + otherDevice + ", but its list still shows " + otherDevice + ". " + otherDevice + " still has access.\n\n" +
				"  Run `qurl grants " + srv.Key.CRID + "` to see who has access now.\n"
			if res.code != exitcode.ServerError || res.stderr.String() != wantStderr {
				t.Fatalf("%v: exit = %d, want %d; stderr =\n%s\nwant\n%s", mode, res.code, exitcode.ServerError, res.stderr.String(), wantStderr)
			}
			if len(mode) == 2 {
				want := `{"crid":"` + srv.Key.CRID + `","removed":["` + requesterDevice + `"],"not_found":[],"not_removed":["` + otherDevice + `"]}`
				if got := compactJSON(t, res.stdout.Bytes()); got != want {
					t.Fatalf("outcome document = %s, want %s", got, want)
				}
			} else {
				mustEmptyStdout(t, res)
			}
			if got := approvedDevices(t, srv); !slices.Equal(got, []string{otherDevice}) {
				t.Fatalf("%v: approved people afterwards = %v, want the one the list still shows", mode, got)
			}
		}
	})

	// The second removal is answered "not found", and the read that would
	// say whether the person or the resource was not found fails. The
	// command does not say that the second person still has access: it
	// does not know.
	t.Run("a removal finds nothing, and the command cannot find out why", func(t *testing.T) {
		const third = "keep-keep-keep-keep"
		for _, mode := range modes {
			srv := twoApproved(t)
			srv.AddApprovedPerson(third, "Kim")
			stopped := false
			choose := func(req *http.Request) *answerInPlace {
				if removalOf(req, otherDevice) {
					stopped = true
					return &answerInPlace{status: http.StatusNotFound, body: `{"error":{"title":"Not Found","status":404,"detail":"nothing here","code":"not_found"},"meta":{"request_id":"req_test"}}`}
				}
				if stopped {
					return busy()
				}
				return nil
			}
			res := runCLI(t, withChosenAnswers(srv, choose, append([]string{"grants", srv.Key.CRID, "--remove", requesterDevice, "--remove", otherDevice, "--remove", third}, mode...)...))
			wantStderr := "Error: access was taken away from " + requesterDevice + ". Then the service found nothing to remove for " + otherDevice + ", and the command stopped before it could find out why. " + third + " still has access.\n\n" +
				"  the resource is being changed; try again\n\n" +
				"  Run `qurl grants " + srv.Key.CRID + "` to see who has access now.\n" +
				"  Request ID: req_test\n"
			if res.code != exitcode.Unavailable || res.stderr.String() != wantStderr {
				t.Fatalf("%v: exit = %d, want %d; stderr =\n%s\nwant\n%s", mode, res.code, exitcode.Unavailable, res.stderr.String(), wantStderr)
			}
			if len(mode) == 2 {
				want := `{"crid":"` + srv.Key.CRID + `","removed":["` + requesterDevice + `"],"not_found":["` + otherDevice + `"],"not_removed":["` + third + `"]}`
				if got := compactJSON(t, res.stdout.Bytes()); got != want {
					t.Fatalf("outcome document = %s, want %s", got, want)
				}
			} else {
				mustEmptyStdout(t, res)
			}
		}
	})

	// A failure before anyone was removed is that failure and nothing more,
	// with no document: nothing was taken away.
	t.Run("a failure before any removal is only that failure", func(t *testing.T) {
		srv := twoApproved(t)
		choose := func(req *http.Request) *answerInPlace {
			if removalOf(req, requesterDevice) {
				return busy()
			}
			return nil
		}
		res := runCLI(t, withChosenAnswers(srv, choose, "grants", srv.Key.CRID, "--remove", requesterDevice, "--remove", otherDevice, "-o", "json"))
		if res.code != exitcode.Unavailable || strings.Contains(res.stderr.String(), "access was taken away") {
			t.Fatalf("exit %d, stderr %q", res.code, res.stderr.String())
		}
		mustEmptyStdout(t, res)
		if got := approvedDevices(t, srv); !slices.Equal(got, []string{requesterDevice, otherDevice}) {
			t.Fatalf("approved people afterwards = %v, want both", got)
		}
	})
}

// TestRequestsOnSaysNothingIsSafeToSendUnlessTheResourceIsPrivate pins the
// one sentence in this feature that must never be wrong. After `qurl
// requests <CRID> --on` the command says that the resource's address is safe
// to send to anyone, because a private resource opens only for the people
// the publisher allows. It says so only when the service's answer says the
// resource is private.
//
// The service refuses the setting for a public resource. One that accepted
// it would otherwise have the command tell a publisher to hand out the
// address of a public resource as if it were private. So an answer that
// says public, or does not say, is an error in every output mode: no "safe
// to send" line, no address, no document, exit 10, and nothing sent after
// the one change.
func TestRequestsOnSaysNothingIsSafeToSendUnlessTheResourceIsPrivate(t *testing.T) {
	modes := [][]string{nil, {"-o", "json"}, {"--quiet"}}
	for _, mode := range modes {
		t.Run(fmt.Sprintf("the answer says public %v", mode), func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.SetResourceAccess(false)
			srv.AcceptAccessRequestsOnPublic()
			res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL, "requests", srv.Key.CRID, "--on"}, mode...), linkSite: testLinkSite})
			want := "Error: access requests are for a private resource, and the service's answer says this one is public: anyone who has the CRID can open it, whether you approve them or not. " +
				"The service turned the setting on all the same. To turn it off again, run `qurl requests <CRID> --off`\n"
			if res.code != exitcode.ServerError || res.stderr.String() != want {
				t.Fatalf("exit = %d, want %d; stderr =\n%s\nwant\n%s", res.code, exitcode.ServerError, res.stderr.String(), want)
			}
			mustEmptyStdout(t, res)
			if got := requestLog(srv); !slices.Equal(got, []string{"PATCH /v1/resources/" + srv.Key.CRID}) {
				t.Fatalf("requests = %v, want the one change and nothing after it", got)
			}
			// The command the message names works, and says nothing about
			// what is safe to send.
			off := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "requests", srv.Key.CRID, "--off"}})
			if off.code != 0 || !strings.Contains(off.stdout.String(), "Access requests are off for "+srv.Key.CRID) || strings.Contains(off.stdout.String(), "safe to send") {
				t.Fatalf("--off afterwards: exit %d, stdout %q, stderr %q", off.code, off.stdout.String(), off.stderr.String())
			}
		})
		t.Run(fmt.Sprintf("the answer does not say %v", mode), func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
					"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "type": "url", "status": "active", "access_requests": true,
				}, nil)
			})
			res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL, "requests", srv.Key.CRID, "--on"}, mode...), linkSite: testLinkSite})
			if res.code != exitcode.ServerError || !strings.Contains(res.stderr.String(), "its answer does not say that this resource is private") {
				t.Fatalf("exit = %d, want %d; stderr = %s", res.code, exitcode.ServerError, res.stderr.String())
			}
			mustEmptyStdout(t, res)
		})
	}
	// Whatever the mode, nothing in either failure reads as the guidance.
	for _, public := range []bool{true, false} {
		srv := apitest.NewServer(t)
		if public {
			srv.SetResourceAccess(false)
			srv.AcceptAccessRequestsOnPublic()
		} else {
			srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
					"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "type": "url", "status": "active", "access_requests": true,
				}, nil)
			})
		}
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "requests", srv.Key.CRID, "--on"}, linkSite: testLinkSite})
		for _, never := range []string{"safe to send", "Send them", testLinkSite, "qurl approve"} {
			if strings.Contains(res.stdout.String()+res.stderr.String(), never) {
				t.Errorf("public %t: the output has %q:\n%s%s", public, never, res.stdout.String(), res.stderr.String())
			}
		}
	}
	// A private resource gets the guidance, as before.
	srv := apitest.NewServer(t)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "requests", srv.Key.CRID, "--on"}, linkSite: testLinkSite})
	if res.code != 0 || !strings.Contains(res.stdout.String(), "safe to send to anyone: a private resource opens only for you and the people you allow.") {
		t.Fatalf("a private resource: exit %d\n%s%s", res.code, res.stdout.String(), res.stderr.String())
	}
}
