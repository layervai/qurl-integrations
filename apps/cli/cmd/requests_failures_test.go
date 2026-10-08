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

// TestGrantsNeverReadsAMissingListAsNobody pins what the command says when
// the service's answer has no list of approved people. That is "the service
// did not say". It is not "nobody", and reading it as nobody would tell a
// publisher that a private resource opens for no one else when the command
// does not know that.
//
// After a removal, such an answer confirms nothing: a list that is not
// there cannot show that the people are off it. The removal was sent and
// answered as made, and the command says exactly that, exits 10, and prints
// no lists. It does not print "Approved people: none".
//
// A read of the grants shows "not said" where the count would be, and the
// JSON document has no approved_people member, as it has no access_requests
// member when the service did not say that. An empty list the service did
// send is still "none" and an empty array.
func TestGrantsNeverReadsAMissingListAsNobody(t *testing.T) {
	seed := func(t *testing.T) *apitest.Server {
		t.Helper()
		srv := twoApproved(t)
		srv.OmitApprovedPeople()
		return srv
	}
	for _, mode := range [][]string{nil, {"-o", "json"}, {"--quiet"}} {
		t.Run(fmt.Sprintf("a removal the list does not confirm %v", mode), func(t *testing.T) {
			srv := seed(t)
			res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "--remove", requesterDevice, "--remove", otherDevice}, mode...)})
			// The usual next step, a read of the lists, is not offered:
			// on this service it would show "not said" again.
			wantStderr := "Error: the service answered that access was taken away from " + requesterDevice + " and " + otherDevice +
				". This service does not show who has access, so the removal cannot be confirmed from here.\n\n" +
				"  Nothing more can be learned with this command against this service: `qurl grants " + srv.Key.CRID + "` does not show who has access either.\n"
			if res.code != exitcode.ServerError || res.stderr.String() != wantStderr {
				t.Fatalf("exit = %d, want %d; stderr =\n%s\nwant\n%s", res.code, exitcode.ServerError, res.stderr.String(), wantStderr)
			}
			if len(mode) == 2 {
				want := `{"crid":"` + srv.Key.CRID + `","removed":["` + requesterDevice + `","` + otherDevice + `"],"not_found":[],"not_removed":[]}`
				if got := compactJSON(t, res.stdout.Bytes()); got != want {
					t.Fatalf("outcome document = %s, want %s", got, want)
				}
			} else {
				mustEmptyStdout(t, res)
			}
			for _, never := range []string{"Approved people", "none", "approved_people"} {
				if strings.Contains(res.stdout.String()+res.stderr.String(), never) {
					t.Errorf("the output has %q:\n%s%s", never, res.stdout.String(), res.stderr.String())
				}
			}
			base := "/v1/resources/" + srv.Key.CRID
			want := []string{"GET " + base, "DELETE " + base + "/allowed-passkeys/" + requesterDevice, "DELETE " + base + "/allowed-passkeys/" + otherDevice, "GET " + base}
			if got := requestLog(srv); !slices.Equal(got, want) {
				t.Fatalf("requests = %v, want %v", got, want)
			}
		})
	}

	// The same in a command that also names a public key. The change to the
	// keys comes after the list is checked, so it was not made, and the
	// outcome names the command that makes it alone.
	// In JSON mode the outcome document says the same: the people in
	// removed, that no public key was changed, and that command.
	for _, mode := range [][]string{nil, {"-o", "json"}, {"--quiet"}} {
		t.Run(fmt.Sprintf("a removal the list does not confirm, with a public key waiting %v", mode), func(t *testing.T) {
			srv := seed(t)
			finish := "qurl grants " + srv.Key.CRID + " --add " + grantKey(2)
			res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "--remove", requesterDevice, "--add", grantKey(2)}, mode...)})
			wantStderr := "Error: the service answered that access was taken away from " + requesterDevice +
				". This service does not show who has access, so the removal cannot be confirmed from here.\n\n" +
				"  No public key was added or removed: that change comes after the removals, and the command stopped before it.\n\n" +
				"  Nothing more can be learned with this command against this service: `qurl grants " + srv.Key.CRID + "` does not show who has access either.\n" +
				"  To make the change to the public keys, run: " + finish + "\n"
			if res.code != exitcode.ServerError || res.stderr.String() != wantStderr {
				t.Fatalf("exit = %d, want %d; stderr =\n%s\nwant\n%s", res.code, exitcode.ServerError, res.stderr.String(), wantStderr)
			}
			if len(mode) == 2 {
				want := `{"crid":"` + srv.Key.CRID + `","removed":["` + requesterDevice + `"],"not_found":[],"not_removed":[],"public_keys_changed":false,"public_keys_command":"` + finish + `"}`
				if got := compactJSON(t, res.stdout.Bytes()); got != want {
					t.Fatalf("outcome document = %s, want %s", got, want)
				}
			} else {
				mustEmptyStdout(t, res)
			}
			for _, line := range requestLog(srv) {
				if strings.HasPrefix(line, "PATCH ") {
					t.Fatalf("a change to the public keys was sent: %v", requestLog(srv))
				}
			}
		})
	}

	// A service that sent the list before the removal and leaves it out of
	// the answer after it. That one answer confirms nothing, so the removal
	// is not reported as done. But this service does show who has access:
	// it did a moment ago. So the command does not say that it does not,
	// and the next step is the read that confirms the removal.
	for _, mode := range [][]string{nil, {"-o", "json"}, {"--quiet"}} {
		t.Run(fmt.Sprintf("the list was there before the removal and is missing after %v", mode), func(t *testing.T) {
			srv := twoApproved(t)
			choose := func(req *http.Request) *answerInPlace {
				if removalOf(req, otherDevice) {
					srv.OmitApprovedPeople()
				}
				return nil
			}
			res := runCLI(t, withChosenAnswers(srv, choose, append([]string{"grants", srv.Key.CRID, "--remove", requesterDevice, "--remove", otherDevice}, mode...)...))
			wantStderr := "Error: the service answered that access was taken away from " + requesterDevice + " and " + otherDevice +
				". Its answer afterwards did not include the list of approved people, so the removal is not confirmed yet.\n\n" +
				"  Run `qurl grants " + srv.Key.CRID + "` to see who has access now.\n"
			if res.code != exitcode.ServerError || res.stderr.String() != wantStderr {
				t.Fatalf("exit = %d, want %d; stderr =\n%s\nwant\n%s", res.code, exitcode.ServerError, res.stderr.String(), wantStderr)
			}
			if len(mode) == 2 {
				want := `{"crid":"` + srv.Key.CRID + `","removed":["` + requesterDevice + `","` + otherDevice + `"],"not_found":[],"not_removed":[]}`
				if got := compactJSON(t, res.stdout.Bytes()); got != want {
					t.Fatalf("outcome document = %s, want %s", got, want)
				}
			} else {
				mustEmptyStdout(t, res)
			}
			for _, never := range []string{"This service does not show", "Nothing more can be learned", "Approved people", "none"} {
				if strings.Contains(res.stdout.String()+res.stderr.String(), never) {
					t.Errorf("the output has %q:\n%s%s", never, res.stdout.String(), res.stderr.String())
				}
			}
		})
	}

	// A read of the grants on that service.
	t.Run("a read says not said", func(t *testing.T) {
		srv := seed(t)
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID}})
		if res.code != 0 || !strings.Contains(res.stdout.String(), "Approved people:      not said\n") || strings.Contains(res.stdout.String(), "none") {
			t.Fatalf("grants: exit %d\n%s%s", res.code, res.stdout.String(), res.stderr.String())
		}
		res = runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "-o", "json"}})
		var document map[string]json.RawMessage
		if err := json.Unmarshal(res.stdout.Bytes(), &document); res.code != 0 || err != nil {
			t.Fatalf("grants -o json: exit %d, %v: %s", res.code, err, res.stdout.String())
		}
		if _, has := document["approved_people"]; has || string(document["allowed_device_keys"]) == "" {
			t.Fatalf("grants -o json has approved_people for a list the service did not send: %s", res.stdout.String())
		}
	})

	// An empty list the service did send is "none", and an empty array.
	t.Run("an empty list is still none", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.SetResourceAccess(true, grantKey(1))
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID}})
		if res.code != 0 || !strings.Contains(res.stdout.String(), "Approved people:      none\n") {
			t.Fatalf("grants: exit %d\n%s%s", res.code, res.stdout.String(), res.stderr.String())
		}
		res = runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "-o", "json"}})
		var document map[string]json.RawMessage
		if err := json.Unmarshal(res.stdout.Bytes(), &document); res.code != 0 || err != nil || string(document["approved_people"]) != "[]" {
			t.Fatalf("grants -o json: exit %d, %v: %s", res.code, err, res.stdout.String())
		}
		// A removal on that service, with the list there to confirm it,
		// succeeds and prints the lists.
		srv.AddApprovedPerson(requesterDevice, requesterName)
		res = runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "--remove", requesterDevice}})
		if res.code != 0 || !strings.Contains(res.stdout.String(), "Approved people:      none\n") {
			t.Fatalf("a removal the list confirms: exit %d\n%s%s", res.code, res.stdout.String(), res.stderr.String())
		}
	})
}

// TestGrantsRemoveTakesADeviceIDWithSpaceAroundIt pins that a device id
// pasted with a space before or after it is still a device id, as it is for
// `qurl deny`, and is not read as a public key that cannot be one.
func TestGrantsRemoveTakesADeviceIDWithSpaceAroundIt(t *testing.T) {
	for _, written := range []string{" " + requesterDevice, requesterDevice + " ", "\t" + strings.ToUpper(requesterDevice) + "\n"} {
		srv := twoApproved(t)
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "--remove", written}})
		if res.code != 0 {
			t.Fatalf("--remove %q: exit %d, stderr %q", written, res.code, res.stderr.String())
		}
		if got := approvedDevices(t, srv); !slices.Equal(got, []string{otherDevice}) {
			t.Fatalf("--remove %q: approved people = %v", written, got)
		}
	}
	// The same id twice is still the same id twice.
	srv := twoApproved(t)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "--remove", requesterDevice, "--remove", " " + requesterDevice}})
	if res.code != exitcode.Usage || !strings.Contains(res.stderr.String(), msgGrantsRemoveTwice) || len(srv.Requests()) != 0 {
		t.Fatalf("the same id twice: exit %d, %d requests, stderr %q", res.code, len(srv.Requests()), res.stderr.String())
	}
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

// wrongCodesSpent gives the mock as many wrong codes as the service allows
// for one resource, so that the next approval or denial by code is limited.
func wrongCodesSpent(t *testing.T, srv *apitest.Server) {
	t.Helper()
	for range apitest.WrongCodeLimit {
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "approve", srv.Key.CRID, "000000"}})
		if res.code != exitcode.NotFound {
			t.Fatalf("a wrong code before the limit: exit %d, stderr %q", res.code, res.stderr.String())
		}
	}
}

// TestTooManyWrongCodes pins what `qurl approve` and `qurl deny <code>` do
// when the service's limit on wrong codes is reached: the service's own
// sentence, how long to wait, in words, the line that says to ask the
// person for the code on their screen, and the exit code for "too many
// requests", in every output mode, with nothing on stdout.
//
// The command sends the request once. It does not wait and try again by
// itself, as it does for a read: another attempt could be one more wrong
// code, and the wait can be an hour. A right code is refused during the
// limit too, and gives nobody access.
func TestTooManyWrongCodes(t *testing.T) {
	want := "Error: Too Many Requests (HTTP 429)\n\n" +
		"  Too many wrong codes were tried for this resource. Try again later.\n\n" +
		"  Try again in 1 hour.\n" +
		"  Ask the person for the code on their screen.\n" +
		"  Request ID: req_test\n"
	for _, test := range []struct {
		name, method, suffix string
		args                 []string
	}{
		{name: "approve with a wrong code", method: "POST", suffix: "111111/approve", args: []string{"approve", "111111"}},
		{name: "approve with the right code", method: "POST", suffix: requestCode + "/approve", args: []string{"approve", requestCode}},
		{name: "deny with a wrong code", method: "DELETE", suffix: "111111", args: []string{"deny", "111111"}},
		{name: "deny with the right code", method: "DELETE", suffix: requestCode, args: []string{"deny", requestCode}},
	} {
		for _, mode := range [][]string{nil, {"-o", "json"}, {"--quiet"}} {
			t.Run(fmt.Sprintf("%s %v", test.name, mode), func(t *testing.T) {
				srv := apitest.NewServer(t)
				twoRequests(srv)
				wrongCodesSpent(t, srv)
				sent := len(srv.Requests())
				var sleeps []time.Duration
				res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL, test.args[0], srv.Key.CRID, test.args[1]}, mode...), sleeps: &sleeps})
				if res.code != exitcode.RateLimited || res.stderr.String() != want {
					t.Fatalf("exit = %d, want %d; stderr =\n%s\nwant\n%s", res.code, exitcode.RateLimited, res.stderr.String(), want)
				}
				mustEmptyStdout(t, res)
				route := test.method + " /v1/resources/" + srv.Key.CRID + "/access-requests/" + test.suffix
				if got := requestLog(srv)[sent:]; !slices.Equal(got, []string{route}) {
					t.Fatalf("requests = %v, want %q once and nothing else", got, route)
				}
				if len(sleeps) != 0 {
					t.Fatalf("the command waited %v to try again by itself", sleeps)
				}
				if got := approvedDevices(t, srv); len(got) != 0 {
					t.Fatalf("approved people = %v, want nobody", got)
				}
				if pending := pendingDevices(t, srv); len(pending) != 2 {
					t.Fatalf("pending = %v, want both requests", pending)
				}
			})
		}
	}

	// A denial by device id is not a guess at a code. It is not limited.
	srv := apitest.NewServer(t)
	twoRequests(srv)
	wrongCodesSpent(t, srv)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "deny", srv.Key.CRID, requesterDevice}})
	if res.code != 0 || !strings.Contains(res.stderr.String(), "No access was given.") {
		t.Fatalf("deny by device id during the limit: exit %d, stderr %q", res.code, res.stderr.String())
	}

	// The same answer with a shorter wait says it in the words for it, and
	// with no wait at all says none.
	for wait, line := range map[string]string{"45": "  Try again in 45 seconds.\n", "90": "  Try again in 2 minutes.\n", "5400": "  Try again in 1 hour 30 minutes.\n", "": ""} {
		srv := apitest.NewServer(t)
		twoRequests(srv)
		srv.Script(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/access-requests/"+requestCode+"/approve", func(w http.ResponseWriter, _ *http.Request) {
			if wait != "" {
				w.Header().Set("Retry-After", wait)
			}
			apitest.WriteProblem(t, w, http.StatusTooManyRequests, "rate_limited", "Too Many Requests", apitest.WrongCodesDetail)
		})
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "approve", srv.Key.CRID, requestCode}})
		wantWait := "  Too many wrong codes were tried for this resource. Try again later.\n\n" + line + "  Ask the person for the code on their screen.\n"
		if res.code != exitcode.RateLimited || !strings.Contains(res.stderr.String(), wantWait) || len(srv.Requests()) != 1 {
			t.Fatalf("Retry-After %q: exit %d, %d requests, stderr =\n%s\nwant in it\n%s", wait, res.code, len(srv.Requests()), res.stderr.String(), wantWait)
		}
	}
}

// TestVerboseNeverShowsATypedCode pins the diagnostics of every command that
// sends a code. With --verbose the command writes the route of each request
// to stderr, and the route of an approval, and of a denial by code, has the
// code in it. The publisher typed that code, but it gives a person access
// for as long as their request is pending, and a terminal gets pasted into
// chats and tickets. So the line shows ****** where the code is, whether the
// request succeeds or fails. A device id in the same place is shown.
func TestVerboseNeverShowsATypedCode(t *testing.T) {
	debugLines := func(stderr string) []string {
		var lines []string
		for _, line := range strings.Split(stderr, "\n") {
			if strings.HasPrefix(line, "[debug] ") {
				lines = append(lines, line)
			}
		}
		return lines
	}
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, *apitest.Server)
		args    func(*apitest.Server) []string
		code    string
		// route is the diagnostic line for the request that carried the
		// code. wantExit is the command's exit code.
		route    func(*apitest.Server) string
		wantExit int
	}{
		{
			name: "approve", code: requestCode,
			args: func(srv *apitest.Server) []string { return []string{"approve", srv.Key.CRID, "482 913"} },
			route: func(srv *apitest.Server) string {
				return "[debug] > POST /v1/resources/" + srv.Key.CRID + "/access-requests/******/approve"
			},
		},
		{
			name: "approve of a code that is not pending", code: "000000", wantExit: exitcode.NotFound,
			args: func(srv *apitest.Server) []string { return []string{"approve", srv.Key.CRID, "000000"} },
			route: func(srv *apitest.Server) string {
				return "[debug] > POST /v1/resources/" + srv.Key.CRID + "/access-requests/******/approve"
			},
		},
		{
			name: "approve that the service cannot serve", code: requestCode, wantExit: exitcode.Unavailable,
			prepare: func(t *testing.T, srv *apitest.Server) {
				srv.Script(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/access-requests/"+requestCode+"/approve", func(w http.ResponseWriter, _ *http.Request) {
					apitest.WriteProblem(t, w, http.StatusServiceUnavailable, "service_unavailable", "Service Unavailable", "try again")
				})
			},
			args: func(srv *apitest.Server) []string { return []string{"approve", srv.Key.CRID, requestCode} },
			route: func(srv *apitest.Server) string {
				return "[debug] > POST /v1/resources/" + srv.Key.CRID + "/access-requests/******/approve"
			},
		},
		{
			name: "approve during the limit on wrong codes", code: requestCode, wantExit: exitcode.RateLimited,
			prepare: func(t *testing.T, srv *apitest.Server) { wrongCodesSpent(t, srv) },
			args:    func(srv *apitest.Server) []string { return []string{"approve", srv.Key.CRID, requestCode} },
			route: func(srv *apitest.Server) string {
				return "[debug] > POST /v1/resources/" + srv.Key.CRID + "/access-requests/******/approve"
			},
		},
		{
			name: "deny by code", code: requestCode,
			args: func(srv *apitest.Server) []string { return []string{"deny", srv.Key.CRID, requestCode} },
			route: func(srv *apitest.Server) string {
				return "[debug] > DELETE /v1/resources/" + srv.Key.CRID + "/access-requests/******"
			},
		},
		{
			name: "deny by a code that is not pending", code: "000000", wantExit: exitcode.NotFound,
			args: func(srv *apitest.Server) []string { return []string{"deny", srv.Key.CRID, "000000"} },
			route: func(srv *apitest.Server) string {
				return "[debug] > DELETE /v1/resources/" + srv.Key.CRID + "/access-requests/******"
			},
		},
		{
			name: "deny by code that the service cannot serve", code: requestCode, wantExit: exitcode.Unavailable,
			prepare: func(t *testing.T, srv *apitest.Server) {
				srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID+"/access-requests/"+requestCode, func(w http.ResponseWriter, _ *http.Request) {
					apitest.WriteProblem(t, w, http.StatusServiceUnavailable, "service_unavailable", "Service Unavailable", "try again")
				})
			},
			args: func(srv *apitest.Server) []string { return []string{"deny", srv.Key.CRID, requestCode} },
			route: func(srv *apitest.Server) string {
				return "[debug] > DELETE /v1/resources/" + srv.Key.CRID + "/access-requests/******"
			},
		},
	} {
		for _, mode := range [][]string{nil, {"-o", "json"}, {"--quiet"}} {
			t.Run(fmt.Sprintf("%s %v", test.name, mode), func(t *testing.T) {
				srv := apitest.NewServer(t)
				twoRequests(srv)
				if test.prepare != nil {
					test.prepare(t, srv)
				}
				args := append(append([]string{"--endpoint", srv.URL}, test.args(srv)...), mode...)
				res := runCLI(t, &runOpts{args: append(args, "--verbose")})
				if res.code != test.wantExit {
					t.Fatalf("exit = %d, want %d; stderr: %s", res.code, test.wantExit, res.stderr.String())
				}
				lines := debugLines(res.stderr.String())
				if !slices.Contains(lines, test.route(srv)) {
					t.Fatalf("the diagnostics lack %q:\n%s", test.route(srv), res.stderr.String())
				}
				for _, line := range lines {
					for _, form := range codeForms(test.code) {
						if strings.Contains(line, form) {
							t.Errorf("a diagnostic line shows the code %q: %s", form, line)
						}
					}
				}
				// A request that failed leaves the code good for as long
				// as the person's request is pending, so nothing on
				// stderr may show it then, diagnostic or not.
				if test.code == requestCode && test.wantExit != 0 {
					for _, form := range codeForms(test.code) {
						if strings.Contains(res.stderr.String(), form) {
							t.Errorf("stderr of a failed request shows the code %q:\n%s", form, res.stderr.String())
						}
					}
				}
			})
		}
	}

	// A device id gives nobody access, and the diagnostics show it.
	srv := apitest.NewServer(t)
	twoRequests(srv)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "deny", srv.Key.CRID, requesterDevice, "--verbose"}})
	if want := "[debug] > DELETE /v1/resources/" + srv.Key.CRID + "/access-requests/" + requesterDevice; res.code != 0 || !slices.Contains(debugLines(res.stderr.String()), want) {
		t.Fatalf("exit %d; the diagnostics lack %q:\n%s", res.code, want, res.stderr.String())
	}
}

// TestAccessRequestCopySaysWhatTheServiceLimits pins, in the help and in the
// README, the facts a publisher would otherwise take for bugs: the listing
// of all resources has no next page, the listing of one resource holds at
// most 20 requests, wrong codes are limited, a failure after a removal says
// who lost access, and --on prints what to send only for a private resource.
func TestAccessRequestCopySaysWhatTheServiceLimits(t *testing.T) {
	collapse := func(text string) string { return strings.Join(strings.Fields(text), " ") }
	help := func(command string) string {
		t.Helper()
		res := runCLI(t, &runOpts{args: []string{command, "--help"}})
		if res.code != 0 {
			t.Fatalf("qurl %s --help exit = %d", command, res.code)
		}
		return collapse(res.stdout.String())
	}
	readme := collapse(strings.ReplaceAll(strings.ReplaceAll(readCLIREADME(t), "`", ""), "**", ""))
	for where, wants := range map[string][]string{
		"qurl requests --help": {
			"The listing of all your resources is bounded, and it has no next page.",
			"The listing of one resource holds at most 20 requests.",
			"It prints that only when the service's answer says the resource is private",
		},
		"qurl approve --help": {
			"after 5 wrong codes for one resource within an hour, it refuses every code for that resource for a time, a right one included",
			"does not try again by itself", "Ask the person for the code on their screen.",
		},
		// A denial by code can be a wrong code too, and its help says what
		// that costs and which form never costs anything.
		"qurl deny --help": {
			"A code that is not pending counts toward the service's limit on wrong codes: after 5 wrong codes for one resource within an hour, it refuses every code for that resource for a time, in \"qurl approve\" too.",
			"A denial by device id never counts, and is the usual way to refuse a request.",
		},
		"qurl grants --help": {
			"From the first person removed on, every failure says exactly which device ids were removed and which were not",
			"a failure that does not say so came before any access was taken away",
		},
	} {
		text := help(strings.Fields(where)[1])
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %q", where, want)
			}
		}
	}
	// The stars are what the diagnostics show, so they are looked for in
	// the README as it is written.
	if want := "shows `******` in place of the code. A device id is shown as it is."; !strings.Contains(collapse(readCLIREADME(t)), want) {
		t.Errorf("README lacks %q", want)
	}
	for _, want := range []string{
		"The listing of all your resources is bounded, and it has no next page: the service takes no cursor for it, so the command has none to pass.",
		"The listing of one resource holds at most 20 requests.",
		"There is no cursor and no next page",
		"qurl requests <CRID> --on prints what to send to people only when the service's answer says that the resource is private.",
		"the command fails (exit code 10) and prints no address",
		"After 5 wrong codes for one resource within an hour, it answers qurl approve, and qurl deny with a code, with \"too many requests\" (exit code 9) for a time, whatever the code, a right one included.",
		"does not try again by itself: another attempt could be one more wrong code. Ask the person for the code on their screen.",
		"qurl deny with a device id is not limited.",
		"A code that is not pending counts toward the service's limit on wrong codes: after 5 wrong codes for one resource within an hour, it refuses every code for that resource for a time, in qurl approve too.",
		"A denial by device id never counts, and is the usual way to refuse a request.",
		"From the first person removed on, every failure says exactly what happened",
		"a list that cannot be read again after the removals", "a list that still shows a person the service said it removed",
		"A failure that says none of this came before any access was taken away.",
		"or stops before it gets to them",
		// The line that ends a listing has the CRID where the command knows
		// it, in the text and in the document an agent reads.
		"In the listing of one resource, that line has the resource's CRID in the command",
		"In the listing of one resource it has that resource's CRID in place of <CRID>.",
		"a failure with no document came before any access was taken away",
		"So are the routes for access requests and approved people",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("README lacks %q", want)
		}
	}
}
