package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// Fixture values of the access-request command tests. No person or device
// has them, and the link site is a placeholder no test resolves or contacts.
const (
	requestCode      = "482913"
	otherRequestCode = "175306"
	requesterDevice  = "abcd-efgh-2345-mnop"
	otherDevice      = "qrst-uvwx-yz67-abcd"
	requesterName    = "Ana Lopez"
	otherRequester   = "Sam Okafor"
	testLinkSite     = "https://links.example.test"
	unsupportedText  = "this service does not offer access requests yet"
	safetyLine       = "To let one of these people in, ask them for the six-digit code on their screen and run `qurl approve <CRID> <code>`; a name can be typed by anyone, so the code is the only proof of who is asking."
)

// pendingDevices reads the device ids of the pending requests of the mock's
// resource from `qurl requests <CRID> --quiet`.
func pendingDevices(t *testing.T, srv *apitest.Server) []string {
	t.Helper()
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "requests", srv.Key.CRID, "--quiet"}})
	if res.code != 0 {
		t.Fatalf("requests --quiet: exit %d: %s", res.code, res.stderr.String())
	}
	return strings.Fields(res.stdout.String())
}

// requestLog returns "METHOD path" for every request the mock received.
func requestLog(srv *apitest.Server) []string {
	requests := srv.Requests()
	lines := make([]string, 0, len(requests))
	for _, request := range requests {
		lines = append(lines, request.Method+" "+request.Path)
	}
	return lines
}

// twoRequests gives the mock two pending requests, the second one first, so
// a command that takes "the first row" instead of what it was given approves
// or refuses the wrong person.
func twoRequests(srv *apitest.Server) {
	srv.SetAccessRequests(true)
	srv.AddAccessRequest(otherRequestCode, otherRequester, otherDevice)
	srv.AddAccessRequest(requestCode, requesterName, requesterDevice)
}

// approvedDevices reads the device ids of the approved people from
// `qurl grants -o json`.
func approvedDevices(t *testing.T, srv *apitest.Server) []string {
	t.Helper()
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "-o", "json"}})
	var document struct {
		People []struct {
			DeviceID string `json:"device_id"`
		} `json:"approved_people"`
	}
	if err := json.Unmarshal(res.stdout.Bytes(), &document); res.code != 0 || err != nil {
		t.Fatalf("grants -o json: exit %d, %v: %s%s", res.code, err, res.stdout.String(), res.stderr.String())
	}
	devices := make([]string, 0, len(document.People))
	for _, person := range document.People {
		devices = append(devices, person.DeviceID)
	}
	return devices
}

// TestPublishWithAllowRequestsSaysWhatToSend pins a publish that turns access
// requests on, for a remote URL and for a local app: the create request asks
// for a private resource with access requests, and the output says what to
// send to people, what happens next, and that it is safe to send. The address
// is shown only when this install knows its link site; otherwise the CRID is,
// and no address is made up.
func TestPublishWithAllowRequestsSaysWhatToSend(t *testing.T) {
	for _, local := range []bool{false, true} {
		for _, site := range []string{testLinkSite, ""} {
			t.Run(fmt.Sprintf("local=%t/site=%q", local, site), func(t *testing.T) {
				srv := apitest.NewServer(t)
				opts := &runOpts{args: []string{"--endpoint", srv.URL, "publish", privacyRemoteTarget, "--allow-requests"}}
				if local {
					opts = servingLocalPublish(t, srv, false, "--allow-requests")
				}
				opts.linkSite = site
				res := runCLI(t, opts)
				if res.code != 0 || res.stderr.Len() != 0 {
					t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
				}
				creates := createRequests(t, srv)
				if len(creates) != 1 || string(creates[0]["access_requests"]) != "true" || string(creates[0]["private"]) != "true" {
					t.Fatalf("create requests = %v, want one private create with access requests on", creates)
				}
				stdout := res.stdout.String()
				wantSent := "\n  " + srv.Key.CRID + "\n"
				wantLines := []string{
					"People can ask you for access to this resource. This install does not know the web address where a CRID is opened for its deployment, so send them the CRID itself:",
					"Where they open it, they ask for access and get a six-digit code to give you. Approve a code with:",
					"The CRID is safe to send to anyone: a private resource opens only for you and the people you allow.",
				}
				if site != "" {
					wantSent = "\n  " + site + "/" + srv.Key.CRID + "\n"
					wantLines = []string{
						"People can ask you for access to this resource. Send them this address:",
						"They ask for access there and get a six-digit code to give you. Approve a code with:",
						"The address and the CRID are safe to send to anyone: a private resource opens only for you and the people you allow.",
					}
				}
				for _, want := range append(wantLines, wantSent, "\n  qurl approve "+srv.Key.CRID+" <code>\n") {
					if !strings.Contains(stdout, want) {
						t.Errorf("publish output lacks %q:\n%s", want, stdout)
					}
				}
				if site == "" && strings.Contains(stdout, "https://links") {
					t.Errorf("publish output names a link site this install was not told about:\n%s", stdout)
				}
				if !strings.HasSuffix(stdout, "\nCRID: "+srv.Key.CRID+"\n") || !strings.Contains(publishRows(stdout), "\n"+privateAccessRow+"\n") {
					t.Errorf("publish output lost its access row or its last CRID line:\n%s", stdout)
				}
			})
		}
	}
}

// TestPublishWithAllowRequestsScriptOutputs pins the JSON document and
// --quiet for the same publish: two members in JSON, the address only when
// the site is known, nothing on stderr, and --quiet still the CRID alone.
func TestPublishWithAllowRequestsScriptOutputs(t *testing.T) {
	for _, site := range []string{testLinkSite, ""} {
		srv := apitest.NewServer(t)
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", privacyRemoteTarget, "--allow-requests", "-o", "json"}, linkSite: site})
		var document struct {
			AccessRequests *bool  `json:"access_requests"`
			ResourceURL    string `json:"resource_url"`
			Private        *bool  `json:"private"`
		}
		if err := json.Unmarshal(res.stdout.Bytes(), &document); res.code != 0 || err != nil || res.stderr.Len() != 0 {
			t.Fatalf("site %q: exit %d, %v, stderr %q", site, res.code, err, res.stderr.String())
		}
		wantURL := ""
		if site != "" {
			wantURL = site + "/" + srv.Key.CRID
		}
		if document.AccessRequests == nil || !*document.AccessRequests || document.ResourceURL != wantURL || document.Private == nil || !*document.Private {
			t.Fatalf("site %q: publish -o json = %s", site, res.stdout.String())
		}
		if (site == "") == strings.Contains(res.stdout.String(), "resource_url") {
			t.Fatalf("site %q: resource_url presence is wrong: %s", site, res.stdout.String())
		}

		srv = apitest.NewServer(t)
		res = runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", privacyRemoteTarget, "--allow-requests", "--quiet"}, linkSite: site})
		if res.code != 0 || res.stdout.String() != srv.Key.CRID+"\n" || res.stderr.Len() != 0 {
			t.Fatalf("site %q: --quiet = %q / %q", site, res.stdout.String(), res.stderr.String())
		}
	}

	// Without the flag nothing about requests is sent or shown, and a known
	// link site changes nothing.
	srv := apitest.NewServer(t)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", privacyRemoteTarget}, linkSite: testLinkSite})
	if _, sent := createRequests(t, srv)[0]["access_requests"]; sent || res.code != 0 || strings.Contains(res.stdout.String(), "ask you for access") || strings.Contains(res.stdout.String(), testLinkSite) {
		t.Fatalf("a plain publish sent or showed access requests:\n%s", res.stdout.String())
	}
}

// TestPublishRefusesAllowRequestsWithPublic pins the usage error, before any
// request, for a remote URL and for a local app.
func TestPublishRefusesAllowRequestsWithPublic(t *testing.T) {
	for _, target := range []string{privacyRemoteTarget, privacyLocalTarget} {
		for _, flags := range [][]string{{"--public", "--allow-requests"}, {"--allow-requests", "--public=true"}} {
			srv := apitest.NewServer(t)
			// The stubs refuse, so a command line that is wrongly accepted
			// fails here and starts nothing on this machine.
			res := runCLI(t, &runOpts{
				args:            append([]string{"--endpoint", srv.URL, "publish", target}, flags...),
				preflightTarget: func(context.Context, string, int) error { return errors.New("unexpected target preflight") },
				localResource: func(context.Context, *connectorshare.NativeRuntimeConfig, func(string) (string, error)) (*agent.ResolvedResource, error) {
					return nil, errors.New("unexpected Connector resource request")
				},
			})
			if res.code != exitcode.Usage || !strings.Contains(res.stderr.String(), msgAllowRequestsWithPublic) || len(srv.Requests()) != 0 {
				t.Fatalf("%s %v: exit %d, %d requests, stderr %q", target, flags, res.code, len(srv.Requests()), res.stderr.String())
			}
			mustEmptyStdout(t, res)
		}
	}
}

// TestPublishWithAllowRequestsFailsWhenTheServiceDoesNotConfirm pins the
// check on the create answer through the command. A service from before
// access requests answers without the setting: exit 11, the message that
// says so and what was made, nothing on stdout in any output mode. A service
// that answers with the setting off: exit 10. Neither prints the CRID, and a
// local publish stops before the Connector resource request.
func TestPublishWithAllowRequestsFailsWhenTheServiceDoesNotConfirm(t *testing.T) {
	for _, test := range []struct {
		name     string
		prepare  func(*testing.T, *apitest.Server)
		wantCode int
		want     []string
	}{
		{
			name:     "a service without access requests",
			prepare:  func(_ *testing.T, srv *apitest.Server) { srv.PlayNoAccessRequests() },
			wantCode: exitcode.Unavailable,
			want:     []string{unsupportedText, "The resource was published as a private resource without them", "run the command again without --allow-requests to see its CRID"},
		},
		{
			name: "the answer says they are off",
			prepare: func(t *testing.T, srv *apitest.Server) {
				srv.Script(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
					apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{
						"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "private": true, "access_requests": false,
					}, nil)
				})
			},
			wantCode: exitcode.ServerError,
			want:     []string{"the service did not turn on access requests for this resource, so no CRID was printed", "`qurl requests <CRID> --on`"},
		},
	} {
		for _, local := range []bool{false, true} {
			for _, mode := range [][]string{nil, {"-o", "json"}, {"--quiet"}} {
				t.Run(fmt.Sprintf("%s/local=%t/%v", test.name, local, mode), func(t *testing.T) {
					srv := apitest.NewServer(t)
					test.prepare(t, srv)
					opts := &runOpts{args: []string{"--endpoint", srv.URL, "publish", privacyRemoteTarget, "--allow-requests"}}
					if local {
						opts = localPublish(t, srv, func(_ context.Context, _ *connectorshare.NativeRuntimeConfig, resolveID func(string) (string, error)) (*agent.ResolvedResource, error) {
							if _, err := resolveID("agent-one"); err != nil {
								return nil, err
							}
							t.Error("the Connector resource request ran after an unconfirmed create")
							return nil, errors.New("unexpected Connector resource request")
						}, "--allow-requests")
					}
					opts.args = append(opts.args, mode...)
					opts.linkSite = testLinkSite
					res := runCLI(t, opts)
					if res.code != test.wantCode {
						t.Fatalf("exit = %d, want %d; stderr: %s", res.code, test.wantCode, res.stderr.String())
					}
					mustEmptyStdout(t, res)
					for _, want := range test.want {
						if !strings.Contains(res.stderr.String(), want) {
							t.Errorf("stderr lacks %q: %s", want, res.stderr.String())
						}
					}
					if strings.Contains(res.stderr.String(), srv.Key.CRID) || strings.Contains(res.stderr.String(), testLinkSite) {
						t.Errorf("stderr shows the CRID or an address for a publish that did not get what was asked: %s", res.stderr.String())
					}
					if got := len(srv.Requests()); got != 1 {
						t.Fatalf("an unconfirmed create was followed by more requests: %v", requestLog(srv))
					}
				})
			}
		}
	}
}

// turnedOnLine is the one line a publish says when it turned access requests
// on for a target that was already published.
const turnedOnLine = "This target was already published. Access requests are now on for it."

// TestPublishWithAllowRequestsTurnsThemOnForAnAlreadyPublishedTarget pins a
// publish that asks for access requests when the target is already published
// as a private resource with them off, for a remote URL and for a local app.
// The service answers the create with the resource as it is, so the command
// turns them on with the change `qurl requests --on` sends, and then prints
// what a first publish with the flag prints, plus the one line that says what
// happened: in the document in text mode, on stderr for JSON and --quiet,
// whose stdout documents keep their shape.
func TestPublishWithAllowRequestsTurnsThemOnForAnAlreadyPublishedTarget(t *testing.T) {
	for _, local := range []bool{false, true} {
		for _, mode := range [][]string{nil, {"-o", "json"}, {"--quiet"}} {
			t.Run(fmt.Sprintf("local=%t/%v", local, mode), func(t *testing.T) {
				srv := apitest.NewServer(t)
				srv.SetPublishFoundExisting(true)
				opts := &runOpts{args: []string{"--endpoint", srv.URL, "publish", privacyRemoteTarget, "--allow-requests"}}
				if local {
					opts = servingLocalPublish(t, srv, true, "--allow-requests")
				}
				opts.args = append(opts.args, mode...)
				opts.linkSite = testLinkSite
				res := runCLI(t, opts)
				if res.code != 0 {
					t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
				}
				// The create, then the one change, before anything else.
				log := requestLog(srv)
				change := "PATCH /v1/resources/" + srv.Key.CRID
				if len(log) < 2 || log[0] != "POST /v1/resources" || log[1] != change || slices.Index(log[2:], change) >= 0 {
					t.Fatalf("requests = %v, want the create and then one %s", log, change)
				}
				for _, request := range srv.Requests() {
					if request.Method == http.MethodPatch && string(request.Body) != `{"access_requests":true}` {
						t.Fatalf("change body = %s, want access_requests alone", request.Body)
					}
				}
				if on := resourceAccessRequests(t, srv); !on {
					t.Fatal("the resource still has access requests off")
				}

				stdout, stderr := res.stdout.String(), res.stderr.String()
				address := testLinkSite + "/" + srv.Key.CRID
				switch {
				case mode == nil:
					for _, want := range []string{
						"Already published\n",
						"\n" + turnedOnLine + "\n",
						"People can ask you for access to this resource. Send them this address:",
						"\n  " + address + "\n",
						"\n  qurl approve " + srv.Key.CRID + " <code>\n",
					} {
						if !strings.Contains(stdout, want) {
							t.Errorf("publish output lacks %q:\n%s", want, stdout)
						}
					}
					if !strings.HasSuffix(stdout, "\nCRID: "+srv.Key.CRID+"\n") || !strings.Contains(publishRows(stdout), "\n"+privateAccessRow+"\n") {
						t.Errorf("publish output lost its access row or its last CRID line:\n%s", stdout)
					}
					// One line says what happened. The other already-published
					// note, about deleting the resource, is not also shown.
					if strings.Contains(stdout, "Delete it first") || stderr != "" {
						t.Errorf("publish says more than the one line:\n%s%s", stdout, stderr)
					}
				case mode[0] == "--quiet":
					if stdout != srv.Key.CRID+"\n" || stderr != turnedOnLine+"\n" {
						t.Fatalf("--quiet = %q / %q", stdout, stderr)
					}
				default:
					var document struct {
						AccessRequests *bool  `json:"access_requests"`
						FoundExisting  *bool  `json:"found_existing"`
						Private        *bool  `json:"private"`
						ResourceURL    string `json:"resource_url"`
						CRID           string `json:"crid"`
					}
					if err := json.Unmarshal(res.stdout.Bytes(), &document); err != nil {
						t.Fatalf("publish -o json: %v: %s", err, stdout)
					}
					if document.AccessRequests == nil || !*document.AccessRequests || document.FoundExisting == nil || !*document.FoundExisting ||
						document.Private == nil || !*document.Private || document.ResourceURL != address || document.CRID != srv.Key.CRID {
						t.Fatalf("publish -o json = %s", stdout)
					}
					if stderr != turnedOnLine+"\n" {
						t.Fatalf("stderr = %q, want the one line", stderr)
					}
				}
			})
		}
	}

	// A target that already has access requests on, and one published again
	// without the flag, are found as they are: no change is sent and the one
	// line is not said.
	for _, test := range []struct {
		name  string
		was   bool
		flags []string
	}{
		{name: "already on", was: true, flags: []string{"--allow-requests"}},
		{name: "not asked for", was: true},
		{name: "not asked for, off"},
	} {
		srv := apitest.NewServer(t)
		srv.SetPublishFoundExisting(true)
		srv.SetAccessRequests(test.was)
		res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL, "publish", privacyRemoteTarget}, test.flags...), linkSite: testLinkSite})
		if res.code != 0 || !slices.Equal(requestLog(srv), []string{"POST /v1/resources"}) || strings.Contains(res.stdout.String(), turnedOnLine) {
			t.Fatalf("%s: exit %d, requests %v:\n%s", test.name, res.code, requestLog(srv), res.stdout.String())
		}
		if got := strings.Contains(res.stdout.String(), "People can ask you for access"); got != test.was {
			t.Fatalf("%s: guidance shown = %t, want %t:\n%s", test.name, got, test.was, res.stdout.String())
		}
		if on := resourceAccessRequests(t, srv); on != test.was {
			t.Fatalf("%s: the publish changed the setting to %t", test.name, on)
		}
	}
}

// resourceAccessRequests reads the resource's setting back from the mock,
// through `qurl grants -o json`.
func resourceAccessRequests(t *testing.T, srv *apitest.Server) bool {
	t.Helper()
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "-o", "json"}})
	var document struct {
		AccessRequests *bool `json:"access_requests"`
	}
	if err := json.Unmarshal(res.stdout.Bytes(), &document); res.code != 0 || err != nil || document.AccessRequests == nil {
		t.Fatalf("grants -o json: exit %d, %v: %s%s", res.code, err, res.stdout.String(), res.stderr.String())
	}
	return *document.AccessRequests
}

// TestPublishThatCannotTurnOnAccessRequestsPrintsTheCRID pins the failure of
// that change through the command. The resource exists and is private, as
// the create answer confirmed, and it is as it was, so this error is the one
// publish failure that names the CRID: in the command that tries again.
// Nothing is on stdout in any output mode, the exit code is the failure's
// own, the change is not retried, and a local publish stops before the
// Connector resource request.
func TestPublishThatCannotTurnOnAccessRequestsPrintsTheCRID(t *testing.T) {
	for _, test := range []struct {
		name     string
		answer   func(*testing.T, *apitest.Server, http.ResponseWriter)
		wantCode int
		reason   string
	}{
		{
			name: "the service is busy",
			answer: func(t *testing.T, _ *apitest.Server, w http.ResponseWriter) {
				w.Header().Set("Retry-After", "1")
				apitest.WriteProblem(t, w, http.StatusServiceUnavailable, "service_unavailable", "Service Unavailable", "the resource is being changed; try again")
			},
			wantCode: exitcode.Unavailable,
			reason:   "the resource is being changed; try again",
		},
		{
			name: "the answer says they are still off",
			answer: func(t *testing.T, srv *apitest.Server, w http.ResponseWriter) {
				apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
					"resource_id": srv.Key.ResourceID, "crid": srv.Key.CRID, "type": "url", "status": "active", "private": true, "access_requests": false,
				}, nil)
			},
			wantCode: exitcode.ServerError,
			reason:   "the service did not confirm the change to access requests.",
		},
	} {
		for _, local := range []bool{false, true} {
			for _, mode := range [][]string{nil, {"-o", "json"}, {"--quiet"}} {
				t.Run(fmt.Sprintf("%s/local=%t/%v", test.name, local, mode), func(t *testing.T) {
					srv := apitest.NewServer(t)
					srv.SetPublishFoundExisting(true)
					srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
						test.answer(t, srv, w)
					})
					opts := &runOpts{args: []string{"--endpoint", srv.URL, "publish", privacyRemoteTarget, "--allow-requests"}}
					if local {
						opts = localPublish(t, srv, func(_ context.Context, _ *connectorshare.NativeRuntimeConfig, resolveID func(string) (string, error)) (*agent.ResolvedResource, error) {
							if _, err := resolveID("agent-one"); err != nil {
								return nil, err
							}
							t.Error("the Connector resource request ran after access requests could not be turned on")
							return nil, errors.New("unexpected Connector resource request")
						}, "--allow-requests")
					}
					opts.args = append(opts.args, mode...)
					opts.linkSite = testLinkSite
					res := runCLI(t, opts)
					if res.code != test.wantCode {
						t.Fatalf("exit = %d, want %d; stderr: %s", res.code, test.wantCode, res.stderr.String())
					}
					mustEmptyStdout(t, res)
					stderr := res.stderr.String()
					for _, want := range []string{
						"Error: this target is already published as a private resource, but access requests could not be turned on for it\n",
						"\n  " + test.reason,
						"\n  Hint: to try again, run `qurl requests " + srv.Key.CRID + " --on`, or run this command again.\n",
					} {
						if !strings.Contains(stderr, want) {
							t.Errorf("stderr lacks %q:\n%s", want, stderr)
						}
					}
					if strings.Contains(stderr, testLinkSite) {
						t.Errorf("stderr shows an address for a resource that does not take requests:\n%s", stderr)
					}
					if want := []string{"POST /v1/resources", "PATCH /v1/resources/" + srv.Key.CRID}; !slices.Equal(requestLog(srv), want) {
						t.Fatalf("requests = %v, want %v", requestLog(srv), want)
					}
				})
			}
		}
	}
}

// TestPublishTurnOnFailureKeepsTheServiceReasonToOneLine pins what the
// failure shows of a reason the service wrote: one printable line. A reason
// with line breaks and terminal controls cannot add a line of its own after
// the error, such as a second "Hint:" with another command in it.
func TestPublishTurnOnFailureKeepsTheServiceReasonToOneLine(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.SetPublishFoundExisting(true)
	srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
		apitest.WriteProblem(t, w, http.StatusBadRequest, "invalid_input", "Invalid Input", "not now\n\n  Hint: run `qurl delete everything`\x1b[2J\x07")
	})
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", privacyRemoteTarget, "--allow-requests"}})
	stderr := res.stderr.String()
	want := "Error: this target is already published as a private resource, but access requests could not be turned on for it\n\n" +
		"  not now Hint: run `qurl delete everything`\ufffd[2J\ufffd\n\n" +
		"  Hint: to try again, run `qurl requests " + srv.Key.CRID + " --on`, or run this command again.\n" +
		"  Request ID: req_test\n"
	if res.code != exitcode.InvalidInput || stderr != want {
		t.Fatalf("exit %d, stderr %q, want exit %d and %q", res.code, stderr, exitcode.InvalidInput, want)
	}
	mustEmptyStdout(t, res)
}

// TestPublishWithAllowRequestsOfAPublicTargetIsAConflict pins the case the
// command does not settle by itself: the target is already published as
// public. Access requests are for a private resource, so --allow-requests is
// a named choice like --allow-device-key: exit 7, nothing on stdout, and no
// second create that would keep the public resource, as a publish with no
// privacy flag does. Nothing is changed on that resource. It is so against
// the service and against an older one, for a remote URL and for a local
// app.
func TestPublishWithAllowRequestsOfAPublicTargetIsAConflict(t *testing.T) {
	for _, olderService := range []bool{false, true} {
		for _, local := range []bool{false, true} {
			t.Run(fmt.Sprintf("older_service=%t/local=%t", olderService, local), func(t *testing.T) {
				srv := apitest.NewServer(t)
				want := "this target is already published as public"
				if olderService {
					srv.PlayPublicByDefault()
					want = "its privacy or its allowed devices differ from what this command asked for"
				}
				srv.SetResourceAccess(false)
				srv.SetPublishFoundExisting(true)
				opts := &runOpts{args: []string{"--endpoint", srv.URL, "publish", privacyRemoteTarget, "--allow-requests"}}
				if local {
					opts = refusingLocalPublish(t, srv, "--allow-requests")
				}
				opts.linkSite = testLinkSite
				res := runCLI(t, opts)
				if res.code != exitcode.Conflict || !strings.Contains(res.stderr.String(), want) {
					t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
				}
				mustEmptyStdout(t, res)
				// The step that keeps the public resource names the flag to
				// leave out; --public cannot be combined with it.
				if hint := "run the command again with --public and without --allow-requests."; !olderService && !strings.Contains(res.stderr.String(), hint) {
					t.Errorf("stderr lacks the hint %q:\n%s", hint, res.stderr.String())
				}
				if log := requestLog(srv); !slices.Equal(log, []string{"POST /v1/resources"}) {
					t.Fatalf("requests = %v, want the one create: no second create and no change", log)
				}
				if strings.Contains(res.stderr.String(), srv.Key.CRID) || strings.Contains(res.stderr.String(), "Warning") {
					t.Fatalf("a conflict named the CRID or read as a kept resource:\n%s", res.stderr.String())
				}
			})
		}
	}
}

// TestRequestAndRequestsNameEachOther pins the pointers between two commands
// whose names differ by one letter. `qurl request` is for a supervising app;
// a person who wanted the access requests and typed it lands on its operand
// count or on a flag it does not have, and both errors name `qurl requests`.
// The other way round, a method and a path given to `qurl requests` name
// `qurl request`. Every one is a usage error before any request, and each
// command's help names the other.
func TestRequestAndRequestsNameEachOther(t *testing.T) {
	srv := apitest.NewServer(t)
	const (
		toRequests = "\n\n  Hint: if you meant to see who asked for access to your resources, use `qurl requests`.\n"
		toRequest  = "\n\n  Hint: if you meant to make a request for a supervising app, use `qurl request METHOD PATH`.\n"
	)
	for _, test := range []struct {
		args []string
		want string
	}{
		{args: []string{"request"}, want: "Error: accepts 2 arg(s), received 0" + toRequests},
		{args: []string{"request", srv.Key.CRID}, want: "Error: accepts 2 arg(s), received 1" + toRequests},
		{args: []string{"request", srv.Key.CRID, "--on"}, want: "Error: unknown flag: --on" + toRequests},
		{args: []string{"request", "--off", srv.Key.CRID}, want: "Error: unknown flag: --off" + toRequests},
		{args: []string{"requests", "GET", "/v1/me"}, want: "Error: accepts at most 1 arg(s), received 2" + toRequest},
		{args: []string{"requests", "delete", "/v1/resources/r_abc/sessions"}, want: "Error: accepts at most 1 arg(s), received 2" + toRequest},
		// Two operands that are not a method and a path are not a request
		// for a supervising app, so that command is not named.
		{args: []string{"requests", srv.Key.CRID, "482913"}, want: "Error: accepts at most 1 arg(s), received 2\n"},
	} {
		res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL}, test.args...)})
		if res.code != exitcode.Usage || res.stderr.String() != test.want {
			t.Errorf("qurl %v: exit %d, stderr %q, want exit %d and %q", test.args, res.code, res.stderr.String(), exitcode.Usage, test.want)
		}
		mustEmptyStdout(t, res)
	}
	if got := len(srv.Requests()); got != 0 {
		t.Fatalf("a usage error sent %d requests: %v", got, requestLog(srv))
	}

	// The usage errors of a real supervised request are as they were.
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "request", "GET", "/v1/me"}})
	if res.code != exitcode.Usage || strings.Contains(res.stderr.String(), "qurl requests") {
		t.Fatalf("request GET /v1/me: exit %d, stderr %q", res.code, res.stderr.String())
	}

	for command, want := range map[string]string{
		"request":  `To see who asked for access` + "\n" + `to your resources, use "qurl requests".`,
		"requests": `"qurl request", without the s, is another command`,
	} {
		res := runCLI(t, &runOpts{args: []string{command, "--help"}})
		if res.code != 0 || !strings.Contains(res.stdout.String(), want) {
			t.Errorf("qurl %s --help does not name the other command (%q):\n%s", command, want, res.stdout.String())
		}
	}
}

// TestRequestsListsPendingRequests pins the two listings through the command:
// the route each uses, the rows, the last line, the JSON document and
// --quiet. The listing changes nothing, and shows no code.
func TestRequestsListsPendingRequests(t *testing.T) {
	for _, all := range []bool{true, false} {
		t.Run(fmt.Sprintf("all=%t", all), func(t *testing.T) {
			srv := apitest.NewServer(t)
			twoRequests(srv)
			args := []string{"--endpoint", srv.URL, "requests"}
			wantRoute := "GET /v1/access-requests"
			wantHeader := "NAME          DEVICE ID            REQUESTED  EXPIRES  CRID"
			wantRows := []string{
				`"Sam Okafor"  qrst-uvwx-yz67-abcd  2m ago     in 58m   ` + srv.Key.CRID,
				`"Ana Lopez"   abcd-efgh-2345-mnop  2m ago     in 58m   ` + srv.Key.CRID,
			}
			wantQuiet := srv.Key.CRID + " " + otherDevice + "\n" + srv.Key.CRID + " " + requesterDevice + "\n"
			if !all {
				args = append(args, srv.Key.CRID)
				wantRoute = "GET /v1/resources/" + srv.Key.CRID + "/access-requests"
				wantHeader = "NAME          DEVICE ID            REQUESTED  EXPIRES"
				wantRows = []string{
					`"Sam Okafor"  qrst-uvwx-yz67-abcd  2m ago     in 58m`,
					`"Ana Lopez"   abcd-efgh-2345-mnop  2m ago     in 58m`,
				}
				wantQuiet = otherDevice + "\n" + requesterDevice + "\n"
			}

			res := runCLI(t, &runOpts{args: args})
			if res.code != 0 || res.stderr.Len() != 0 {
				t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
			}
			lines := strings.Split(strings.TrimRight(res.stdout.String(), "\n"), "\n")
			if len(lines) != 5 || lines[0] != wantHeader || lines[1] != wantRows[0] || lines[2] != wantRows[1] || lines[3] != "" || lines[4] != safetyLine {
				t.Fatalf("listing =\n%s", res.stdout.String())
			}

			res = runCLI(t, &runOpts{args: append(slices.Clone(args), "-o", "json")})
			var document struct {
				Requests []struct {
					Name         string `json:"name"`
					NameVerified *bool  `json:"name_verified"`
					DeviceID     string `json:"device_id"`
					CRID         string `json:"crid"`
				} `json:"requests"`
				ApprovalRule string `json:"approval_rule"`
				HasMore      *bool  `json:"has_more"`
			}
			if err := json.Unmarshal(res.stdout.Bytes(), &document); res.code != 0 || err != nil || len(document.Requests) != 2 || res.stderr.Len() != 0 {
				t.Fatalf("requests -o json: exit %d, %v: %s", res.code, err, res.stdout.String())
			}
			second := document.Requests[1]
			if second.Name != requesterName || second.NameVerified == nil || *second.NameVerified || second.DeviceID != requesterDevice || second.CRID != srv.Key.CRID {
				t.Fatalf("requests -o json row = %+v", second)
			}
			if document.ApprovalRule != safetyLine || document.HasMore == nil || *document.HasMore {
				t.Fatalf("requests -o json: approval_rule %q, has_more %v", document.ApprovalRule, document.HasMore)
			}

			res = runCLI(t, &runOpts{args: append(slices.Clone(args), "--quiet")})
			if res.code != 0 || res.stdout.String() != wantQuiet || res.stderr.Len() != 0 {
				t.Fatalf("requests --quiet = %q, want %q", res.stdout.String(), wantQuiet)
			}

			for _, line := range requestLog(srv) {
				if line != wantRoute {
					t.Fatalf("the listing sent %q, want only %q", line, wantRoute)
				}
			}
		})
	}

	// No request pending: nothing on stdout, a note on stderr, no safety line.
	srv := apitest.NewServer(t)
	for args, note := range map[string]string{"": "No pending access requests.\n", srv.Key.CRID: "No pending access requests for this resource.\n"} {
		res := runCLI(t, &runOpts{args: slices.DeleteFunc([]string{"--endpoint", srv.URL, "requests", args}, func(arg string) bool { return arg == "" })})
		if res.code != 0 || res.stdout.Len() != 0 || res.stderr.String() != note {
			t.Fatalf("empty listing %q: exit %d, stdout %q, stderr %q", args, res.code, res.stdout.String(), res.stderr.String())
		}
	}
}

// codeForms returns every form in which a six-digit code could be written
// into an output: bare, and in the two groups of three a person reads aloud,
// with each separator.
func codeForms(code string) []string {
	return []string{code, code[:3] + " " + code[3:], code[:3] + "-" + code[3:]}
}

// TestNoCommandShowsAPendingCode pins the rule the scheme rests on: the code
// of a pending request is on the screen of the person who asked, and no
// command of the publisher shows it. A publisher, or an agent working for
// one, who could read the codes in a listing could approve from the list,
// which is approval by name with one more step.
//
// The service here is a build that still sends each request's code in its
// listings. Every command that reads or lists is run in every output mode,
// with and without --verbose, and so are the errors a publisher can get
// while the requests are pending. Neither code is anywhere on stdout or on
// stderr.
func TestNoCommandShowsAPendingCode(t *testing.T) {
	const wrongCode, absentDevice = "000000", "nope-nope-nope-nope"
	seed := func(t *testing.T) *apitest.Server {
		t.Helper()
		srv := apitest.NewServer(t)
		twoRequests(srv)
		srv.ListRequestCodes()
		return srv
	}
	// The mock does send the codes: without that this test would pass
	// whatever the commands did.
	probe := seed(t)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, probe.URL+"/v1/access-requests", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	answer, err := probe.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	sent, err := io.ReadAll(answer.Body)
	_ = answer.Body.Close()
	if err != nil || !strings.Contains(string(sent), `"request_code":"`+requestCode+`"`) || !strings.Contains(string(sent), `"request_code":"`+otherRequestCode+`"`) {
		t.Fatalf("the mock did not send the codes this test is about: %s (%v)", sent, err)
	}

	commands := map[string]func(*apitest.Server) []string{
		"requests":                   func(*apitest.Server) []string { return []string{"requests"} },
		"requests <CRID>":            func(srv *apitest.Server) []string { return []string{"requests", srv.Key.CRID} },
		"requests <CRID> --off":      func(srv *apitest.Server) []string { return []string{"requests", srv.Key.CRID, "--off"} },
		"requests <CRID> --on":       func(srv *apitest.Server) []string { return []string{"requests", srv.Key.CRID, "--on"} },
		"grants <CRID>":              func(srv *apitest.Server) []string { return []string{"grants", srv.Key.CRID} },
		"status <CRID>":              func(srv *apitest.Server) []string { return []string{"status", srv.Key.CRID} },
		"deny <CRID> <device id>":    func(srv *apitest.Server) []string { return []string{"deny", srv.Key.CRID, requesterDevice} },
		"deny of a device not there": func(srv *apitest.Server) []string { return []string{"deny", srv.Key.CRID, absentDevice} },
		"deny of a wrong code":       func(srv *apitest.Server) []string { return []string{"deny", srv.Key.CRID, wrongCode} },
		"approve of a wrong code":    func(srv *apitest.Server) []string { return []string{"approve", srv.Key.CRID, wrongCode} },
		"approve of a device id":     func(srv *apitest.Server) []string { return []string{"approve", srv.Key.CRID, requesterDevice} },
		"deny of a name":             func(srv *apitest.Server) []string { return []string{"deny", srv.Key.CRID, requesterName} },
	}
	modes := map[string][]string{"text": nil, "json": {"-o", "json"}, "quiet": {"--quiet"}}
	for name, command := range commands {
		for mode, flags := range modes {
			for _, verbose := range []bool{false, true} {
				srv := seed(t)
				args := append(append([]string{"--endpoint", srv.URL}, command(srv)...), flags...)
				if verbose {
					args = append(args, "--verbose")
				}
				res := runCLI(t, &runOpts{args: args})
				where := fmt.Sprintf("%s (%s, verbose %t)", name, mode, verbose)
				for _, code := range []string{requestCode, otherRequestCode} {
					for _, form := range codeForms(code) {
						if strings.Contains(res.stdout.String(), form) {
							t.Errorf("%s: stdout shows the pending code %q:\n%s", where, form, res.stdout.String())
						}
						if strings.Contains(res.stderr.String(), form) {
							t.Errorf("%s: stderr shows the pending code %q:\n%s", where, form, res.stderr.String())
						}
					}
				}
				for _, member := range []string{`"code"`, `"request_code"`} {
					if strings.HasPrefix(name, "requests") && strings.Contains(res.stdout.String(), member) {
						t.Errorf("%s: the document has the member %s:\n%s", where, member, res.stdout.String())
					}
				}
				// --verbose did write its diagnostics, so their silence
				// about the codes is not the silence of a flag that did
				// nothing.
				if verbose && len(srv.Requests()) > 0 && !strings.Contains(res.stderr.String(), "[debug] ") {
					t.Errorf("%s: --verbose wrote no diagnostics:\n%s", where, res.stderr.String())
				}
			}
		}
	}
}

// TestRequestsListingSaysWhenThereMayBeMore pins the bounded listing through
// the command. When the service says there may be more requests than it
// sent, the listing of all resources says so after its rows and says what to
// do, the JSON document has has_more true, and --quiet says it on stderr and
// keeps stdout to values. A listing the service says nothing about, or calls
// complete, has no such line, and has_more false.
func TestRequestsListingSaysWhenThereMayBeMore(t *testing.T) {
	const (
		forAll = "There may be more requests than are shown here. To see all the requests for one resource, run `qurl requests <CRID>`."
		forOne = "There may be more requests for this resource than are shown here. A request leaves the list when it is approved, denied or expired; run this command again to see the rest."
	)
	for _, test := range []struct {
		name string
		set  *bool
		more bool
	}{
		{name: "the service does not say"},
		{name: "the service says there is no more", set: new(bool)},
		{name: "the service says there may be more", set: func() *bool { more := true; return &more }(), more: true},
	} {
		for _, all := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, all=%t", test.name, all), func(t *testing.T) {
				srv := apitest.NewServer(t)
				twoRequests(srv)
				if test.set != nil {
					srv.SetAccessRequestsHasMore(*test.set)
				}
				args, line := []string{"--endpoint", srv.URL, "requests"}, forAll
				if !all {
					args, line = append(args, srv.Key.CRID), forOne
				}

				res := runCLI(t, &runOpts{args: args})
				text := res.stdout.String()
				if res.code != 0 || res.stderr.Len() != 0 || strings.Contains(text, "There may be more") != test.more {
					t.Fatalf("text: exit %d, stderr %q:\n%s", res.code, res.stderr.String(), text)
				}
				if test.more && (!strings.HasSuffix(text, "\n\n"+line+"\n\n"+safetyLine+"\n") || strings.Index(text, requesterDevice) > strings.Index(text, line)) {
					t.Fatalf("text: the line is not after the rows and before the last line:\n%s", text)
				}

				res = runCLI(t, &runOpts{args: append(slices.Clone(args), "-o", "json")})
				var document struct {
					Requests []json.RawMessage `json:"requests"`
					HasMore  *bool             `json:"has_more"`
				}
				if err := json.Unmarshal(res.stdout.Bytes(), &document); res.code != 0 || err != nil || document.HasMore == nil || *document.HasMore != test.more || len(document.Requests) != 2 || res.stderr.Len() != 0 {
					t.Fatalf("json: exit %d, %v, stderr %q:\n%s", res.code, err, res.stderr.String(), res.stdout.String())
				}

				res = runCLI(t, &runOpts{args: append(slices.Clone(args), "--quiet")})
				wantErr := ""
				if test.more {
					wantErr = line + "\n"
				}
				if res.code != 0 || len(strings.Fields(res.stdout.String())) != map[bool]int{true: 4, false: 2}[all] || res.stderr.String() != wantErr {
					t.Fatalf("quiet: exit %d, stdout %q, stderr %q, want stderr %q", res.code, res.stdout.String(), res.stderr.String(), wantErr)
				}
			})
		}
	}

	// A page with no rows of a listing that goes on is not "none".
	srv := apitest.NewServer(t)
	srv.SetAccessRequestsHasMore(true)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "requests"}})
	if res.code != 0 || res.stdout.Len() != 0 || res.stderr.String() != forAll+"\n" {
		t.Fatalf("an empty page of a listing that goes on: exit %d, stdout %q, stderr %q", res.code, res.stdout.String(), res.stderr.String())
	}
}

// productionRequests returns the run options for an access-request command
// pointed at the production endpoint, with the API client replaced by one
// for the mock, so the command sees a production endpoint and no request
// leaves the machine.
func productionRequests(t *testing.T, srv *apitest.Server, args ...string) *runOpts {
	t.Helper()
	return &runOpts{
		args: append([]string{"--endpoint", "https://api.layerv.ai"}, args...),
		openAPIClient: func(context.Context) (qurlapi.Client, error) {
			return qurlapi.New(&qurlapi.Config{BaseURL: srv.URL, APIKey: testAPIKey, Version: "test"})
		},
	}
}

// TestListingRequestsNeedsNoConfirmationForATestCRID pins which
// access-request commands the guard for a test CRID on the production
// endpoint applies to. A listing acts on nothing, so like `qurl grants
// <CRID>` it needs no --yes and draws no warning, for one resource and for
// all of them. A change is refused before any request without --yes, and
// goes through, with the warning, with it. An operand that is not a CRID is
// refused locally either way.
func TestListingRequestsNeedsNoConfirmationForATestCRID(t *testing.T) {
	for name, args := range map[string]func(*apitest.Server) []string{
		"all resources": func(*apitest.Server) []string { return []string{"requests"} },
		"one resource":  func(srv *apitest.Server) []string { return []string{"requests", srv.Key.CRID} },
	} {
		t.Run("listing "+name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			twoRequests(srv)
			res := runCLI(t, productionRequests(t, srv, args(srv)...))
			if res.code != 0 || res.stderr.Len() != 0 || !strings.Contains(res.stdout.String(), requesterDevice) {
				t.Fatalf("exit %d, stdout %q, stderr %q", res.code, res.stdout.String(), res.stderr.String())
			}
			if log := requestLog(srv); len(log) != 1 || !strings.HasPrefix(log[0], "GET ") {
				t.Fatalf("requests = %v, want the one read", log)
			}
		})
	}
	changes := map[string]func(*apitest.Server) []string{
		"requests --on":     func(srv *apitest.Server) []string { return []string{"requests", srv.Key.CRID, "--on"} },
		"requests --off":    func(srv *apitest.Server) []string { return []string{"requests", srv.Key.CRID, "--off"} },
		"approve":           func(srv *apitest.Server) []string { return []string{"approve", srv.Key.CRID, requestCode} },
		"deny by device id": func(srv *apitest.Server) []string { return []string{"deny", srv.Key.CRID, requesterDevice} },
		"deny by code":      func(srv *apitest.Server) []string { return []string{"deny", srv.Key.CRID, requestCode} },
	}
	for name, args := range changes {
		t.Run(name+" without --yes", func(t *testing.T) {
			srv := apitest.NewServer(t)
			twoRequests(srv)
			res := runCLI(t, productionRequests(t, srv, args(srv)...))
			if res.code != exitcode.Usage || !strings.Contains(res.stderr.String(), "Re-run with --yes") || len(srv.Requests()) != 0 {
				t.Fatalf("exit %d, %d requests, stderr %q", res.code, len(srv.Requests()), res.stderr.String())
			}
			mustEmptyStdout(t, res)
		})
		t.Run(name+" with --yes", func(t *testing.T) {
			srv := apitest.NewServer(t)
			twoRequests(srv)
			res := runCLI(t, productionRequests(t, srv, append(args(srv), "--yes")...))
			if res.code != 0 || !strings.Contains(res.stderr.String(), "because --yes was given") {
				t.Fatalf("exit %d, stderr %q", res.code, res.stderr.String())
			}
			if log := requestLog(srv); len(log) != 1 || strings.HasPrefix(log[0], "GET ") {
				t.Fatalf("requests = %v, want the one change", log)
			}
		})
	}
	srv := apitest.NewServer(t)
	res := runCLI(t, productionRequests(t, srv, "requests", "not-a-crid"))
	if res.code != exitcode.InvalidInput || len(srv.Requests()) != 0 {
		t.Fatalf("a listing for an operand that is not a CRID: exit %d, %d requests, stderr %q", res.code, len(srv.Requests()), res.stderr.String())
	}
	help := runCLI(t, &runOpts{args: []string{"requests", "--help"}})
	if !strings.Contains(help.stdout.String(), "listing never needs it") {
		t.Fatalf("requests --help does not say that a listing needs no --yes:\n%s", help.stdout.String())
	}
}

// TestRequestsOnAndOff pins the change of the setting through the command:
// one PATCH that carries only the setting, then what to send to people (on)
// or who still has access (off).
func TestRequestsOnAndOff(t *testing.T) {
	t.Run("on", func(t *testing.T) {
		for _, site := range []string{testLinkSite, ""} {
			srv := apitest.NewServer(t)
			res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "requests", srv.Key.CRID, "--on"}, linkSite: site})
			if res.code != 0 || res.stderr.Len() != 0 {
				t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
			}
			if got := requestLog(srv); len(got) != 1 || got[0] != "PATCH /v1/resources/"+srv.Key.CRID || string(srv.Requests()[0].Body) != `{"access_requests":true}` {
				t.Fatalf("--on sent %v with body %s", got, srv.Requests()[0].Body)
			}
			stdout := res.stdout.String()
			sent := srv.Key.CRID
			if site != "" {
				sent = site + "/" + srv.Key.CRID
			}
			for _, want := range []string{"Access requests are on for " + srv.Key.CRID + ".\n", "\n  " + sent + "\n", "\n  qurl approve " + srv.Key.CRID + " <code>\n", "safe to send to anyone: a private resource opens only for you and the people you allow."} {
				if !strings.Contains(stdout, want) {
					t.Errorf("site %q: --on output lacks %q:\n%s", site, want, stdout)
				}
			}
			if site == "" && (strings.Contains(stdout, "://") || !strings.Contains(stdout, "does not know the web address")) {
				t.Errorf("--on without a known site must say so and name no address:\n%s", stdout)
			}
		}
	})
	t.Run("off", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.SetAccessRequests(true)
		srv.AddApprovedPerson(requesterDevice, requesterName)
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "requests", srv.Key.CRID, "--off"}, linkSite: testLinkSite})
		want := "Access requests are off for " + srv.Key.CRID + ". Nobody new can ask for access.\n" +
			"1 approved person still has access. See them, or take access away, with `qurl grants " + srv.Key.CRID + "`.\n"
		if res.code != 0 || res.stdout.String() != want || res.stderr.Len() != 0 {
			t.Fatalf("--off: exit %d\n%s%s", res.code, res.stdout.String(), res.stderr.String())
		}
		if body := string(srv.Requests()[0].Body); len(srv.Requests()) != 1 || body != `{"access_requests":false}` {
			t.Fatalf("--off sent %v with body %s", requestLog(srv), body)
		}
		if got := approvedDevices(t, srv); !slices.Equal(got, []string{requesterDevice}) {
			t.Fatalf("turning requests off changed who has access: %v", got)
		}
	})
	t.Run("script outputs", func(t *testing.T) {
		srv := apitest.NewServer(t)
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "requests", srv.Key.CRID, "--on", "-o", "json"}, linkSite: testLinkSite})
		want := `{"crid":"` + srv.Key.CRID + `","access_requests":true,"resource_url":"` + testLinkSite + "/" + srv.Key.CRID + `"}`
		if got := strings.Join(strings.Fields(res.stdout.String()), ""); res.code != 0 || got != want {
			t.Fatalf("--on -o json = %q, want %q", got, want)
		}
		res = runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "requests", srv.Key.CRID, "--off", "--quiet"}})
		if res.code != 0 || res.stdout.String() != srv.Key.CRID+"\n" {
			t.Fatalf("--off --quiet = %q", res.stdout.String())
		}
	})
	t.Run("usage errors send nothing", func(t *testing.T) {
		srv := apitest.NewServer(t)
		for _, test := range []struct {
			args []string
			want string
		}{
			{args: []string{srv.Key.CRID, "--on", "--off"}, want: msgRequestsOnAndOff},
			{args: []string{"--on"}, want: msgRequestsSettingNeedsCRID},
			{args: []string{"--off"}, want: msgRequestsSettingNeedsCRID},
			{args: []string{srv.Key.CRID, "another"}, want: "accepts at most 1 arg"},
		} {
			res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL, "requests"}, test.args...)})
			if res.code != exitcode.Usage || !strings.Contains(res.stderr.String(), test.want) {
				t.Errorf("requests %v: exit %d, stderr %q", test.args, res.code, res.stderr.String())
			}
			mustEmptyStdout(t, res)
		}
		if got := len(srv.Requests()); got != 0 {
			t.Fatalf("a refused command line sent %d requests", got)
		}
	})
	t.Run("a public resource", func(t *testing.T) {
		srv := apitest.NewServer(t)
		srv.SetResourceAccess(false)
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "requests", srv.Key.CRID, "--on"}})
		if res.code != exitcode.InvalidInput || !strings.Contains(res.stderr.String(), "only for a private resource") {
			t.Fatalf("--on for a public resource: exit %d, stderr %q", res.code, res.stderr.String())
		}
		mustEmptyStdout(t, res)
	})
}

// TestAccessRequestCommandsOnAServiceWithoutThem pins the one answer every
// command gives against a service from before access requests: the message
// that says so, exit 11, nothing on stdout in any output mode. Nothing is
// reported as done.
func TestAccessRequestCommandsOnAServiceWithoutThem(t *testing.T) {
	for _, test := range []struct {
		name string
		args func(*apitest.Server) []string
	}{
		{name: "requests", args: func(*apitest.Server) []string { return []string{"requests"} }},
		{name: "requests for one resource", args: func(srv *apitest.Server) []string { return []string{"requests", srv.Key.CRID} }},
		{name: "requests on", args: func(srv *apitest.Server) []string { return []string{"requests", srv.Key.CRID, "--on"} }},
		{name: "requests off", args: func(srv *apitest.Server) []string { return []string{"requests", srv.Key.CRID, "--off"} }},
		{name: "approve", args: func(srv *apitest.Server) []string { return []string{"approve", srv.Key.CRID, requestCode} }},
		{name: "deny", args: func(srv *apitest.Server) []string { return []string{"deny", srv.Key.CRID, requestCode} }},
		{name: "remove a person", args: func(srv *apitest.Server) []string {
			return []string{"grants", srv.Key.CRID, "--remove", requesterDevice}
		}},
	} {
		for _, mode := range [][]string{nil, {"-o", "json"}, {"--quiet"}} {
			t.Run(fmt.Sprintf("%s/%v", test.name, mode), func(t *testing.T) {
				srv := apitest.NewServer(t)
				srv.AddAccessRequest(requestCode, requesterName, requesterDevice)
				srv.AddApprovedPerson(requesterDevice, requesterName)
				srv.PlayNoAccessRequests()
				args := append([]string{"--endpoint", srv.URL}, test.args(srv)...)
				res := runCLI(t, &runOpts{args: append(args, mode...)})
				if res.code != exitcode.Unavailable || !strings.Contains(res.stderr.String(), "Error: "+unsupportedText) {
					t.Fatalf("exit = %d, want %d; stderr: %s", res.code, exitcode.Unavailable, res.stderr.String())
				}
				mustEmptyStdout(t, res)
			})
		}
	}
	// On that service a read of the grants still works, and says nothing
	// about requests or people it was not told about.
	srv := apitest.NewServer(t)
	srv.PlayNoAccessRequests()
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID}})
	if res.code != 0 || strings.Contains(res.stdout.String(), "Access requests:") || !strings.Contains(res.stdout.String(), "Approved people:      none") {
		t.Fatalf("grants against a service without access requests: exit %d\n%s%s", res.code, res.stdout.String(), res.stderr.String())
	}
}

// TestAccessRequestCommandsForAnUnknownResource pins that a CRID the service
// does not know is the ordinary not-found, exit 5, on a service with access
// requests and on one without, and never "this service does not offer".
func TestAccessRequestCommandsForAnUnknownResource(t *testing.T) {
	// A well-formed CRID of a resource the mock does not have.
	unknownCRID := apitest.DeriveCRID(t, []byte("a-resource-the-mock-does-not-have"), apitest.VersionTest)
	for _, older := range []bool{false, true} {
		for _, args := range [][]string{{"requests", unknownCRID}, {"approve", unknownCRID, requestCode}, {"deny", unknownCRID, requestCode}, {"grants", unknownCRID, "--remove", requesterDevice}} {
			srv := apitest.NewServer(t)
			if older {
				srv.PlayNoAccessRequests()
			}
			res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL}, args...)})
			if res.code != exitcode.NotFound || strings.Contains(res.stderr.String(), unsupportedText) || !strings.Contains(res.stderr.String(), "HTTP 404") {
				t.Errorf("older=%t %v: exit %d, stderr %q", older, args[0], res.code, res.stderr.String())
			}
			mustEmptyStdout(t, res)
		}
	}
}

// TestApproveSendsTheCodeThatWasGiven pins the approval through the command,
// for every way of writing the code. Two requests are pending and the one to
// approve is not the first: the person who gets access is the one whose code
// was given, and the other request stays pending.
func TestApproveSendsTheCodeThatWasGiven(t *testing.T) {
	for _, written := range [][]string{{"482913"}, {"482 913"}, {"482-913"}, {"482", "913"}, {" 482913 "}} {
		t.Run(strings.Join(written, "+"), func(t *testing.T) {
			srv := apitest.NewServer(t)
			twoRequests(srv)
			res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL, "approve", srv.Key.CRID}, written...)})
			if res.code != 0 || res.stderr.Len() != 0 {
				t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
			}
			want := []string{"POST /v1/resources/" + srv.Key.CRID + "/access-requests/" + requestCode + "/approve"}
			if got := requestLog(srv); !slices.Equal(got, want) {
				t.Fatalf("approve sent %v, want %v", got, want)
			}
			wantOut := "Approved\n\n" +
				"  Name:       \"Ana Lopez\" (typed by them, not checked)\n" +
				"  Device ID:  " + requesterDevice + "\n" +
				"  Approved:   2026-03-02 (just now)\n\n" +
				"This person can now open " + srv.Key.CRID + ". To take the access away, run:\n\n" +
				"  qurl grants " + srv.Key.CRID + " --remove " + requesterDevice + "\n"
			if res.stdout.String() != wantOut {
				t.Fatalf("approve output =\n%s\nwant\n%s", res.stdout.String(), wantOut)
			}
			if got := approvedDevices(t, srv); !slices.Equal(got, []string{requesterDevice}) {
				t.Fatalf("approved people = %v, want only the person whose code was given", got)
			}
			if pending := pendingDevices(t, srv); !slices.Equal(pending, []string{otherDevice}) {
				t.Fatalf("pending after the approval = %v, want the other request", pending)
			}
		})
	}

	srv := apitest.NewServer(t)
	twoRequests(srv)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "approve", srv.Key.CRID, requestCode, "-o", "json"}})
	var approval map[string]any
	if err := json.Unmarshal(res.stdout.Bytes(), &approval); res.code != 0 || err != nil || res.stderr.Len() != 0 {
		t.Fatalf("approve -o json: exit %d, %v: %s%s", res.code, err, res.stdout.String(), res.stderr.String())
	}
	// name_note says in words, for a reader of JSON, what the text document
	// says beside the name.
	wantApproval := map[string]any{
		"crid": srv.Key.CRID, "approved": true, "name": "Ana Lopez", "name_verified": false,
		"device_id": requesterDevice, "approved_at": "2026-03-02T00:00:00Z",
		"name_note": "The name was typed by the person who asked. Nobody checked it.",
	}
	if len(approval) != len(wantApproval) {
		t.Fatalf("approve -o json has members %v, want exactly %v", approval, wantApproval)
	}
	for member, value := range wantApproval {
		if approval[member] != value {
			t.Errorf("approve -o json %s = %v, want %v", member, approval[member], value)
		}
	}
	res = runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "approve", srv.Key.CRID, otherRequestCode, "--quiet"}})
	if res.code != 0 || res.stdout.String() != otherDevice+"\n" {
		t.Fatalf("approve --quiet = %q, want the device id", res.stdout.String())
	}
}

// TestApproveAndDenyRefuseWhatCannotNameARequest pins the local refusals. For
// an approval, a value that can never be a code is exit 8 with the forms
// that are accepted, a device id included: an approval is by code and
// nothing else. For a denial, a value that is neither a device id nor a code
// is exit 8 with the two things that are accepted. A wrong number of
// operands is a usage error that says so. None of them sends anything.
func TestApproveAndDenyRefuseWhatCannotNameARequest(t *testing.T) {
	notCodes := [][]string{{"48291"}, {"4829133"}, {"48291a"}, {"482  913"}, {"482_913"}, {"4829 13"}, {"48 2913"}, {""}, {"４８２９１３"}, {requesterName}, {"482913/approve"}}
	for command, test := range map[string]struct {
		refused [][]string
		message string
	}{
		"approve": {refused: append(slices.Clone(notCodes), []string{requesterDevice}), message: msgRequestCodeInvalid},
		"deny": {
			refused: append(slices.Clone(notCodes), []string{"abcd-efgh-2345"}, []string{"abcd-efgh-1890-mnop"}, []string{"abcd_efgh_2345_mnop"}, []string{requesterDevice + "/approve"}, []string{"../" + requesterDevice}),
			message: msgDeniedRequestInvalid,
		},
	} {
		srv := apitest.NewServer(t)
		twoRequests(srv)
		for _, value := range test.refused {
			res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL, command, srv.Key.CRID}, value...)})
			if res.code != exitcode.InvalidInput || !strings.Contains(res.stderr.String(), test.message) {
				t.Errorf("%s %q: exit %d, stderr %q", command, value, res.code, res.stderr.String())
			}
			mustEmptyStdout(t, res)
		}
		// One operand too many is reported as that. Only a code that a
		// shell split into its two halves is two operands.
		for operands, received := range map[string]int{
			"":           0,
			srv.Key.CRID: 1,
			srv.Key.CRID + " " + requestCode + " junk":               3,
			srv.Key.CRID + " 482 9133":                               3,
			srv.Key.CRID + " abc def":                                3,
			srv.Key.CRID + " " + requesterDevice + " " + otherDevice: 3,
			srv.Key.CRID + " 482 913 000":                            4,
		} {
			res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL, command}, strings.Fields(operands)...)})
			if want := fmt.Sprintf("accepts 2 arg(s), received %d", received); res.code != exitcode.Usage || !strings.Contains(res.stderr.String(), want) {
				t.Errorf("%s %q: exit %d, stderr %q, want a usage error with %q", command, operands, res.code, res.stderr.String(), want)
			}
			mustEmptyStdout(t, res)
		}
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, command, "not-a-crid", requestCode}})
		if res.code != exitcode.InvalidInput {
			t.Errorf("%s with an operand that is not a CRID: exit %d", command, res.code)
		}
		if got := len(srv.Requests()); got != 0 {
			t.Fatalf("%s: a refused command line sent %d requests: %v", command, got, requestLog(srv))
		}
	}
	if !strings.Contains(msgDeniedRequestInvalid, "xxxx-xxxx-xxxx-xxxx") || !strings.Contains(msgDeniedRequestInvalid, "six-digit code") {
		t.Fatalf("the message for a denial lost one of the two forms: %q", msgDeniedRequestInvalid)
	}
}

// TestApproveAndDenyARequestThatIsNotPending pins the answer for a code, or
// for a denial a device id, that the resource has no pending request for:
// exit 5 and a message about what was named, not the generic hint about a
// mistyped CRID, and not a word about which codes are pending. Nobody gets
// access and the pending request of another person is not touched.
func TestApproveAndDenyARequestThatIsNotPending(t *testing.T) {
	const absentDevice = "nope-nope-nope-nope"
	for _, test := range []struct {
		command, named, want string
	}{
		{command: "approve", named: requestCode, want: "Error: no pending request has the code 482 913 for this resource"},
		{command: "deny", named: requestCode, want: "Error: no pending request has the code 482 913 for this resource"},
		{command: "deny", named: absentDevice, want: "Error: no pending request is from the device id " + absentDevice + " for this resource"},
	} {
		for _, mode := range [][]string{nil, {"-o", "json"}, {"--quiet"}} {
			srv := apitest.NewServer(t)
			srv.SetAccessRequests(true)
			srv.AddAccessRequest(otherRequestCode, otherRequester, otherDevice)
			args := []string{"--endpoint", srv.URL, test.command, srv.Key.CRID, test.named}
			res := runCLI(t, &runOpts{args: append(args, mode...)})
			where := fmt.Sprintf("%s %s %v", test.command, test.named, mode)
			if res.code != exitcode.NotFound {
				t.Fatalf("%s: exit %d, stderr %q", where, res.code, res.stderr.String())
			}
			mustEmptyStdout(t, res)
			for _, want := range []string{test.want, "It may have expired, or been approved or denied already", "`qurl requests <CRID>`", "Request ID: req_test"} {
				if !strings.Contains(res.stderr.String(), want) {
					t.Errorf("%s: stderr lacks %q: %s", where, want, res.stderr.String())
				}
			}
			if strings.Contains(res.stderr.String(), "mistyped") {
				t.Errorf("%s: the message is the generic hint about a CRID: %s", where, res.stderr.String())
			}
			for _, form := range codeForms(otherRequestCode) {
				if strings.Contains(res.stderr.String(), form) {
					t.Errorf("%s: the message gives away a code that is pending: %s", where, res.stderr.String())
				}
			}
			if got := approvedDevices(t, srv); len(got) != 0 {
				t.Fatalf("%s gave access: %v", where, got)
			}
			if pending := pendingDevices(t, srv); !slices.Equal(pending, []string{otherDevice}) {
				t.Fatalf("%s touched another person's request: pending = %v", where, pending)
			}
		}
	}
}

// TestDenyRemovesTheRequestAndGivesNoAccess pins the denial through the
// command, for each way a request is named: by the device id a listing
// shows, in either case, and by the code its publisher was given. One DELETE
// is sent with that value in it and nothing is read first, so the request
// that is refused is the one that was named. The confirmation is a status
// line on stderr, and nobody gets access.
func TestDenyRemovesTheRequestAndGivesNoAccess(t *testing.T) {
	for _, test := range []struct {
		name, written, sent, note, member string
	}{
		{name: "by device id", written: requesterDevice, sent: requesterDevice, member: "device_id", note: "Denied the request from the device id " + requesterDevice + " for %s. No access was given.\n"},
		{name: "by device id in capitals", written: strings.ToUpper(requesterDevice), sent: requesterDevice, member: "device_id", note: "Denied the request from the device id " + requesterDevice + " for %s. No access was given.\n"},
		{name: "by code", written: "482 913", sent: requestCode, member: "code", note: "Denied the request with the code 482 913 for %s. No access was given.\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			route := func(srv *apitest.Server) []string {
				return []string{"DELETE /v1/resources/" + srv.Key.CRID + "/access-requests/" + test.sent}
			}
			srv := apitest.NewServer(t)
			twoRequests(srv)
			res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "deny", srv.Key.CRID, test.written}})
			if want := fmt.Sprintf(test.note, srv.Key.CRID); res.code != 0 || res.stdout.Len() != 0 || res.stderr.String() != want {
				t.Fatalf("deny: exit %d, stdout %q, stderr %q, want stderr %q", res.code, res.stdout.String(), res.stderr.String(), want)
			}
			if got := requestLog(srv); !slices.Equal(got, route(srv)) {
				t.Fatalf("deny sent %v, want %v: the value is sent as it was given, and nothing is looked up", got, route(srv))
			}
			if got := approvedDevices(t, srv); len(got) != 0 {
				t.Fatalf("a denial gave access: %v", got)
			}
			if pending := pendingDevices(t, srv); !slices.Equal(pending, []string{otherDevice}) {
				t.Fatalf("pending after the denial = %v, want the other request alone", pending)
			}

			srv = apitest.NewServer(t)
			twoRequests(srv)
			res = runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "deny", srv.Key.CRID, test.written, "-o", "json"}})
			want := `{"crid":"` + srv.Key.CRID + `","` + test.member + `":"` + test.sent + `","denied":true}`
			if got := strings.Join(strings.Fields(res.stdout.String()), ""); res.code != 0 || got != want || res.stderr.Len() != 0 {
				t.Fatalf("deny -o json = %q, want %q", got, want)
			}

			srv = apitest.NewServer(t)
			twoRequests(srv)
			res = runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "deny", srv.Key.CRID, test.written, "--quiet"}})
			if res.code != 0 || res.stdout.String() != test.sent+"\n" || res.stderr.Len() != 0 || !slices.Equal(requestLog(srv), route(srv)) {
				t.Fatalf("deny --quiet: exit %d, stdout %q, stderr %q, sent %v", res.code, res.stdout.String(), res.stderr.String(), requestLog(srv))
			}
		})
	}
}

// TestGrantsListsApprovedPeople pins the read: `qurl grants <CRID>` shows the
// approved people beside the device keys, each with the name in quotes, the
// device id and the approval date, and whether people can still ask.
func TestGrantsListsApprovedPeople(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.SetResourceAccess(true, grantKey(1))
	srv.SetAccessRequests(true)
	srv.AddApprovedPerson(requesterDevice, requesterName)
	srv.AddApprovedPerson(otherDevice, "")
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID}})
	if res.code != 0 || res.stderr.Len() != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
	}
	for _, want := range []string{
		"Access requests:      on\n",
		"Allowed device keys:  [" + grantKey(1) + "]\n",
		"Approved people:      2\n",
		"\nApproved people:\n  NAME           DEVICE ID            APPROVED\n",
		"  \"Ana Lopez\"    abcd-efgh-2345-mnop  2026-03-01 (12h ago)\n",
		"  no name given  qrst-uvwx-yz67-abcd  2026-03-01 (12h ago)\n",
		"\nTake one person's access away with `qurl grants " + srv.Key.CRID + " --remove <device id>`.\n",
	} {
		if !strings.Contains(res.stdout.String(), want) {
			t.Errorf("grants output lacks %q:\n%s", want, res.stdout.String())
		}
	}
	if got := approvedDevices(t, srv); !slices.Equal(got, []string{requesterDevice, otherDevice}) {
		t.Fatalf("grants -o json approved people = %v", got)
	}
}

// TestGrantsRemoveTakesADeviceID pins the removal of an approved person:
// --remove with a device id sends a DELETE for that person, a device id in
// capitals is the same id, the removal is made before any change to public
// keys, and the output is the lists as they are afterwards.
func TestGrantsRemoveTakesADeviceID(t *testing.T) {
	seed := func(t *testing.T) *apitest.Server {
		t.Helper()
		srv := apitest.NewServer(t)
		srv.SetResourceAccess(true, grantKey(1))
		srv.AddApprovedPerson(requesterDevice, requesterName)
		srv.AddApprovedPerson(otherDevice, otherRequester)
		return srv
	}
	t.Run("one person", func(t *testing.T) {
		for _, written := range []string{requesterDevice, strings.ToUpper(requesterDevice)} {
			srv := seed(t)
			res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "--remove", written, "-o", "json"}})
			if res.code != 0 {
				t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
			}
			base := "/v1/resources/" + srv.Key.CRID
			// The list is read, the person is removed, and the list is read
			// again for the output.
			if got, want := requestLog(srv), []string{"GET " + base, "DELETE " + base + "/allowed-passkeys/" + requesterDevice, "GET " + base}; !slices.Equal(got, want) {
				t.Fatalf("--remove %s sent %v, want %v", written, got, want)
			}
			var document struct {
				Keys   []string `json:"allowed_device_keys"`
				People []struct {
					DeviceID string `json:"device_id"`
				} `json:"approved_people"`
			}
			if err := json.Unmarshal(res.stdout.Bytes(), &document); err != nil || len(document.People) != 1 || document.People[0].DeviceID != otherDevice || !slices.Equal(document.Keys, []string{grantKey(1)}) {
				t.Fatalf("lists after the removal: %s (%v)", res.stdout.String(), err)
			}
		}
	})
	t.Run("people and public keys in one command", func(t *testing.T) {
		srv := seed(t)
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "--add", grantKey(2), "--remove", requesterDevice, "--remove", grantKey(1), "--remove", otherDevice}})
		if res.code != 0 {
			t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
		}
		base := "/v1/resources/" + srv.Key.CRID
		want := []string{"GET " + base, "DELETE " + base + "/allowed-passkeys/" + requesterDevice, "DELETE " + base + "/allowed-passkeys/" + otherDevice, "GET " + base, "PATCH " + base}
		if got := requestLog(srv); !slices.Equal(got, want) {
			t.Fatalf("requests = %v, want the removals of people before the change to public keys: %v", got, want)
		}
		var edit map[string][]string
		if err := json.Unmarshal(srv.Requests()[4].Body, &edit); err != nil || !slices.Equal(edit["allowed_device_keys_add"], []string{grantKey(2)}) || !slices.Equal(edit["allowed_device_keys_remove"], []string{grantKey(1)}) || len(edit) != 2 {
			t.Fatalf("the change to public keys = %s: a device id must never be sent as a public key", srv.Requests()[4].Body)
		}
		if !strings.Contains(res.stdout.String(), "Approved people:      none\n") || !strings.Contains(res.stdout.String(), "["+grantKey(2)+"]") {
			t.Fatalf("lists after the change:\n%s", res.stdout.String())
		}
	})
	// A device id that is not on the list, in every place it can stand
	// among ids that are. The list is read first, so nothing is removed:
	// exit 5, nothing on stdout in text mode, a message that names the id
	// that was not found and the people who still have access, and no
	// request after the read. In JSON mode the same outcome is a document.
	const absent = "zzzz-zzzz-zzzz-zzzz"
	for _, test := range []struct {
		name       string
		ids        []string
		notRemoved []string
	}{
		{name: "alone", ids: []string{absent}},
		{name: "first", ids: []string{absent, requesterDevice}, notRemoved: []string{requesterDevice}},
		{name: "last", ids: []string{requesterDevice, absent}, notRemoved: []string{requesterDevice}},
		{name: "in the middle", ids: []string{requesterDevice, absent, otherDevice}, notRemoved: []string{requesterDevice, otherDevice}},
	} {
		t.Run("a device id that is not on the list, "+test.name, func(t *testing.T) {
			flags := []string{"--add", grantKey(2)}
			for _, id := range test.ids {
				flags = append(flags, "--remove", id)
			}
			headline := "Error: no approved person has the device id " + absent + " on this resource, so nothing was removed."
			switch len(test.notRemoved) {
			case 1:
				headline += " " + test.notRemoved[0] + " still has access."
			case 2:
				headline += " " + test.notRemoved[0] + " and " + test.notRemoved[1] + " still have access."
			}
			for _, mode := range [][]string{nil, {"--quiet"}, {"-o", "json"}} {
				srv := seed(t)
				res := runCLI(t, &runOpts{args: append(append([]string{"--endpoint", srv.URL, "grants", srv.Key.CRID}, flags...), mode...)})
				if res.code != exitcode.NotFound {
					t.Fatalf("%v: exit = %d, stderr: %s", mode, res.code, res.stderr.String())
				}
				wantStderr := headline + "\n\n" +
					"  No public key was added or removed: that change comes after the removals, and they did not finish.\n\n" +
					"  Run `qurl grants " + srv.Key.CRID + "` to see who has access now.\n"
				if res.stderr.String() != wantStderr {
					t.Fatalf("%v: stderr =\n%s\nwant\n%s", mode, res.stderr.String(), wantStderr)
				}
				if log := requestLog(srv); !slices.Equal(log, []string{"GET /v1/resources/" + srv.Key.CRID}) {
					t.Fatalf("%v: requests = %v, want the one read: nothing may be removed or changed", mode, log)
				}
				if len(mode) == 2 {
					var document struct {
						CRID                          string
						Removed, NotFound, NotRemoved []string
						PublicKeysChanged             *bool `json:"public_keys_changed"`
					}
					raw := map[string]json.RawMessage{}
					if err := json.Unmarshal(res.stdout.Bytes(), &raw); err != nil {
						t.Fatalf("outcome document %q: %v", res.stdout.String(), err)
					}
					for member, into := range map[string]any{"crid": &document.CRID, "removed": &document.Removed, "not_found": &document.NotFound, "not_removed": &document.NotRemoved, "public_keys_changed": &document.PublicKeysChanged} {
						if err := json.Unmarshal(raw[member], into); err != nil {
							t.Fatalf("outcome document member %s in %s: %v", member, res.stdout.String(), err)
						}
					}
					if document.CRID != srv.Key.CRID || document.Removed == nil || len(document.Removed) != 0 || !slices.Equal(document.NotFound, []string{absent}) ||
						len(document.NotRemoved) != len(test.notRemoved) || document.NotRemoved == nil || document.PublicKeysChanged == nil || *document.PublicKeysChanged {
						t.Fatalf("outcome document = %s", res.stdout.String())
					}
				} else {
					mustEmptyStdout(t, res)
				}
				if got := approvedDevices(t, srv); !slices.Equal(got, []string{requesterDevice, otherDevice}) {
					t.Fatalf("%v: a refused removal changed who has access: %v", mode, got)
				}
			}
		})
	}

	// The list changes between the read and a removal: the first person is
	// removed, and the second is gone by the time the command gets to them.
	// Access was taken away, so the output must say from whom. It never
	// says that nothing was removed.
	t.Run("a person is gone by the time they are removed", func(t *testing.T) {
		const third = "keep-keep-keep-keep"
		for _, mode := range [][]string{nil, {"-o", "json"}} {
			srv := seed(t)
			srv.AddApprovedPerson(third, "Kim")
			srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID+"/allowed-passkeys/"+otherDevice, func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteProblem(t, w, http.StatusNotFound, "not_found", "Not Found", "no such approved person")
			})
			args := []string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "--remove", requesterDevice, "--remove", otherDevice, "--remove", third}
			res := runCLI(t, &runOpts{args: append(args, mode...)})
			if res.code != exitcode.NotFound {
				t.Fatalf("%v: exit = %d, stderr: %s", mode, res.code, res.stderr.String())
			}
			wantStderr := "Error: access was taken away from " + requesterDevice + ". Then no approved person had the device id " + otherDevice + " on this resource, and the command stopped. " + third + " still has access.\n\n" +
				"  Run `qurl grants " + srv.Key.CRID + "` to see who has access now.\n" +
				"  Request ID: req_test\n"
			if res.stderr.String() != wantStderr || strings.Contains(res.stderr.String(), "nothing was removed") {
				t.Fatalf("%v: stderr =\n%s\nwant\n%s", mode, res.stderr.String(), wantStderr)
			}
			if mode == nil {
				mustEmptyStdout(t, res)
			} else if got, want := strings.Join(strings.Fields(res.stdout.String()), ""), `{"crid":"`+srv.Key.CRID+`","removed":["`+requesterDevice+`"],"not_found":["`+otherDevice+`"],"not_removed":["`+third+`"]}`; got != want {
				t.Fatalf("outcome document = %s, want %s", got, want)
			}
			for _, line := range requestLog(srv) {
				if strings.HasSuffix(line, "/allowed-passkeys/"+third) {
					t.Fatalf("%v: a removal after the one that stopped was sent: %v", mode, requestLog(srv))
				}
			}
			// What the output said is what happened.
			if got := approvedDevices(t, srv); !slices.Equal(got, []string{otherDevice, third}) {
				t.Fatalf("%v: approved people afterwards = %v", mode, got)
			}
		}
	})

	// A removal that fails for another reason after one was made keeps that
	// reason's exit code, and still says who lost access.
	t.Run("a removal fails after one was made", func(t *testing.T) {
		srv := seed(t)
		srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID+"/allowed-passkeys/"+otherDevice, func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteProblem(t, w, http.StatusServiceUnavailable, "service_unavailable", "Service Unavailable", "the resource is being changed; try again")
		})
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "--remove", requesterDevice, "--remove", otherDevice}})
		wantStderr := "Error: access was taken away from " + requesterDevice + ". Then taking it away from " + otherDevice + " failed, and the command stopped. " + otherDevice + " still has access.\n\n" +
			"  the resource is being changed; try again\n\n" +
			"  Run `qurl grants " + srv.Key.CRID + "` to see who has access now.\n" +
			"  Request ID: req_test\n"
		if res.code != exitcode.Unavailable || res.stderr.String() != wantStderr {
			t.Fatalf("exit = %d, want %d; stderr =\n%s\nwant\n%s", res.code, exitcode.Unavailable, res.stderr.String(), wantStderr)
		}
		mustEmptyStdout(t, res)
	})
	// Every person is removed, and then the change to the public keys
	// fails. The failure alone would say nothing about the people, who have
	// lost access, and the same command run again would stop at device ids
	// that are gone. So the outcome says who lost access, that the key
	// change failed and why, where to see the list as it is, and the command
	// that makes the key change alone. The exit code is the failure's.
	t.Run("the people are removed and then the key change fails", func(t *testing.T) {
		for _, mode := range [][]string{nil, {"-o", "json"}, {"--quiet"}} {
			srv := seed(t)
			base := "/v1/resources/" + srv.Key.CRID
			srv.Script(http.MethodPatch, base, func(w http.ResponseWriter, _ *http.Request) {
				apitest.WriteProblem(t, w, http.StatusServiceUnavailable, "service_unavailable", "Service Unavailable", "the resource is being changed; try again")
			})
			command := []string{"grants", srv.Key.CRID, "--add", grantKey(2), "--remove", requesterDevice, "--remove", grantKey(1), "--remove", otherDevice}
			res := runCLI(t, &runOpts{args: append(append([]string{"--endpoint", srv.URL}, command...), mode...)})
			finish := "qurl grants " + srv.Key.CRID + " --add " + grantKey(2) + " --remove " + grantKey(1)
			wantStderr := "Error: access was taken away from " + requesterDevice + " and " + otherDevice + ". Then the change to the public keys failed.\n\n" +
				"  the resource is being changed; try again\n\n" +
				"  Run `qurl grants " + srv.Key.CRID + "` to see who has access now.\n" +
				"  To make the change to the public keys, run: " + finish + "\n" +
				"  Request ID: req_test\n"
			if res.code != exitcode.Unavailable || res.stderr.String() != wantStderr {
				t.Fatalf("%v: exit = %d, want %d; stderr =\n%s\nwant\n%s", mode, res.code, exitcode.Unavailable, res.stderr.String(), wantStderr)
			}
			if strings.Contains(res.stderr.String(), "No public key was added or removed") || strings.Contains(res.stderr.String(), "nothing was removed") {
				t.Fatalf("%v: the outcome claims something it does not know:\n%s", mode, res.stderr.String())
			}
			if len(mode) == 2 {
				want := `{"crid":"` + srv.Key.CRID + `","removed":["` + requesterDevice + `","` + otherDevice + `"],"not_found":[],"not_removed":[],"public_keys_command":"` + finish + `"}`
				var compact bytes.Buffer
				if err := json.Compact(&compact, res.stdout.Bytes()); err != nil || compact.String() != want {
					t.Fatalf("outcome document = %s (%v), want %s", res.stdout.String(), err, want)
				}
			} else {
				mustEmptyStdout(t, res)
			}
			want := []string{"GET " + base, "DELETE " + base + "/allowed-passkeys/" + requesterDevice, "DELETE " + base + "/allowed-passkeys/" + otherDevice, "GET " + base, "PATCH " + base}
			if got := requestLog(srv); !slices.Equal(got, want) {
				t.Fatalf("%v: requests = %v, want %v", mode, got, want)
			}
			// What the outcome said is what happened: the people are off
			// the list and the keys are as they were.
			if got := approvedDevices(t, srv); len(got) != 0 {
				t.Fatalf("%v: approved people afterwards = %v, want none", mode, got)
			}
			// The same command again stops at the device ids that are gone,
			// which is why the outcome names another one.
			again := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL}, command...)})
			if again.code != exitcode.NotFound || !strings.Contains(again.stderr.String(), "so nothing was removed") {
				t.Fatalf("%v: the same command again: exit %d, stderr %q", mode, again.code, again.stderr.String())
			}
			// The command the outcome names finishes the job.
			done := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL}, append(strings.Fields(finish)[1:], "-o", "json")...)})
			var lists struct {
				Keys []string `json:"allowed_device_keys"`
			}
			if err := json.Unmarshal(done.stdout.Bytes(), &lists); done.code != 0 || err != nil || !slices.Equal(lists.Keys, []string{grantKey(2)}) {
				t.Fatalf("%v: the finishing command: exit %d, %v, stdout %s, stderr %s", mode, done.code, err, done.stdout.String(), done.stderr.String())
			}
		}
	})
	// A key change that fails with no person removed is that failure and
	// nothing more: there is no outcome to report.
	t.Run("a key change alone that fails is only that failure", func(t *testing.T) {
		srv := seed(t)
		srv.Script(http.MethodPatch, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteProblem(t, w, http.StatusServiceUnavailable, "service_unavailable", "Service Unavailable", "the resource is being changed; try again")
		})
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "--add", grantKey(2), "-o", "json"}})
		if res.code != exitcode.Unavailable || strings.Contains(res.stderr.String(), "access was taken away") || strings.Contains(res.stderr.String(), "To make the change") {
			t.Fatalf("exit %d, stderr %q", res.code, res.stderr.String())
		}
		mustEmptyStdout(t, res)
	})
	t.Run("usage errors send nothing", func(t *testing.T) {
		srv := seed(t)
		for _, test := range []struct {
			flags []string
			want  string
		}{
			{flags: []string{"--clear", "--remove", requesterDevice}, want: msgGrantsClearWithEdit},
			{flags: []string{"--remove", requesterDevice, "--remove", strings.ToUpper(requesterDevice)}, want: msgGrantsRemoveTwice},
			{flags: []string{"--remove", "abcd-efgh-2345"}, want: msgGrantsRemoveInvalid},
			{flags: []string{"--remove", "abcd-efgh-1890-mnop"}, want: msgGrantsRemoveInvalid},
			{flags: []string{"--add", requesterDevice}, want: "--add requires unique canonical"},
		} {
			res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL, "grants", srv.Key.CRID}, test.flags...)})
			if res.code != exitcode.Usage || !strings.Contains(res.stderr.String(), test.want) {
				t.Errorf("grants %v: exit %d, stderr %q", test.flags, res.code, res.stderr.String())
			}
			mustEmptyStdout(t, res)
		}
		if got := len(srv.Requests()); got != 0 {
			t.Fatalf("a refused command line sent %d requests", got)
		}
	})
	t.Run("clear leaves approved people", func(t *testing.T) {
		srv := seed(t)
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "--clear"}})
		if res.code != 0 || !strings.Contains(res.stdout.String(), "Allowed device keys:  []\n") || !strings.Contains(res.stdout.String(), "Approved people:      2\n") {
			t.Fatalf("--clear: exit %d\n%s%s", res.code, res.stdout.String(), res.stderr.String())
		}
	})
}

// TestAccessRequestsOnAServiceThatRefusesTheSetting pins the two commands a
// person meets first, against a service that has no access requests and
// validates request bodies strictly: it refuses the setting it does not know
// with its generic validation problem. Both commands must say what every
// other access-request command says on such a service, with exit 11, and not
// "validation error" with exit 8. Nothing is on stdout in any output mode.
// The publish also says that nothing was published and how to publish
// without access requests, and that advice works.
func TestAccessRequestsOnAServiceThatRefusesTheSetting(t *testing.T) {
	const listing = "GET /v1/access-requests"
	modes := [][]string{nil, {"-o", "json"}, {"--quiet"}}

	for _, flag := range []string{"--on", "--off"} {
		for _, mode := range modes {
			t.Run(fmt.Sprintf("requests %s/%v", flag, mode), func(t *testing.T) {
				srv := apitest.NewServer(t)
				srv.PlayStrictWithoutAccessRequests()
				res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL, "requests", srv.Key.CRID, flag}, mode...), linkSite: testLinkSite})
				if res.code != exitcode.Unavailable || res.stderr.String() != "Error: "+unsupportedText+"\n" {
					t.Fatalf("exit = %d, want %d; stderr %q", res.code, exitcode.Unavailable, res.stderr.String())
				}
				mustEmptyStdout(t, res)
				if log := requestLog(srv); !slices.Equal(log, []string{"PATCH /v1/resources/" + srv.Key.CRID, listing}) {
					t.Fatalf("requests = %v, want the change and the one question", log)
				}
			})
		}
	}

	const refused = "Error: " + unsupportedText + ". Nothing was published; run the command again without --allow-requests to publish the resource as private\n"
	for _, local := range []bool{false, true} {
		for _, mode := range modes {
			t.Run(fmt.Sprintf("publish/local=%t/%v", local, mode), func(t *testing.T) {
				srv := apitest.NewServer(t)
				srv.PlayStrictWithoutAccessRequests()
				opts := &runOpts{args: []string{"--endpoint", srv.URL, "publish", privacyRemoteTarget, "--allow-requests"}}
				if local {
					opts = refusingLocalPublish(t, srv, "--allow-requests")
				}
				opts.args = append(opts.args, mode...)
				opts.linkSite = testLinkSite
				res := runCLI(t, opts)
				if res.code != exitcode.Unavailable || res.stderr.String() != refused {
					t.Fatalf("exit = %d, want %d; stderr %q", res.code, exitcode.Unavailable, res.stderr.String())
				}
				mustEmptyStdout(t, res)
				if strings.Contains(res.stderr.String(), srv.Key.CRID) || strings.Contains(res.stderr.String(), testLinkSite) {
					t.Fatalf("stderr names a CRID or an address for a publish that made nothing: %s", res.stderr.String())
				}
				if log := requestLog(srv); !slices.Equal(log, []string{"POST /v1/resources", listing}) {
					t.Fatalf("requests = %v, want the create and the one question", log)
				}
			})
		}
	}

	// The message's advice, followed: the same publish without the flag.
	srv := apitest.NewServer(t)
	srv.PlayStrictWithoutAccessRequests()
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "publish", privacyRemoteTarget}})
	if res.code != 0 || !strings.Contains(publishRows(res.stdout.String()), "\n"+privateAccessRow+"\n") || !strings.HasSuffix(res.stdout.String(), "\nCRID: "+srv.Key.CRID+"\n") {
		t.Fatalf("publish without --allow-requests on that service: exit %d\n%s%s", res.code, res.stdout.String(), res.stderr.String())
	}

	// A service that has access requests and refuses the change has its own
	// reason, and that is what the publisher reads.
	srv = apitest.NewServer(t)
	srv.SetResourceAccess(false)
	res = runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "requests", srv.Key.CRID, "--on"}})
	if res.code != exitcode.InvalidInput || strings.Contains(res.stderr.String(), unsupportedText) || !strings.Contains(res.stderr.String(), "access requests can be turned on only for a private resource") {
		t.Fatalf("a real refusal: exit %d, stderr %q", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
}

// TestGrantsRemoveBoundsDeviceIDs pins that the device ids of one command
// are bounded as its public keys are. Each device id is its own request,
// sent one after another, so more than 256 of them are refused before
// anything is sent, with the same kind of message as for public keys.
func TestGrantsRemoveBoundsDeviceIDs(t *testing.T) {
	const letters = "abcdefghijklmnopqrstuvwxyz234567"
	ids := func(count int) []string {
		flags := make([]string, 0, 2*count)
		for index := range count {
			id := []byte("aaaa-aaaa-aaaa-aaaa")
			id[0], id[1] = letters[index%32], letters[(index/32)%32]
			flags = append(flags, "--remove", string(id))
		}
		return flags
	}
	srv := apitest.NewServer(t)
	res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL, "grants", srv.Key.CRID}, ids(257)...)})
	if res.code != exitcode.Usage || res.stderr.String() != "Error: --remove accepts at most 256 device ids\n" || len(srv.Requests()) != 0 {
		t.Fatalf("257 device ids: exit %d after %d requests, stderr %q", res.code, len(srv.Requests()), res.stderr.String())
	}
	mustEmptyStdout(t, res)

	// 256 are taken: the command goes on to read the list, where none of
	// them is found.
	srv = apitest.NewServer(t)
	res = runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL, "grants", srv.Key.CRID}, ids(256)...)})
	if res.code != exitcode.NotFound || !slices.Equal(requestLog(srv), []string{"GET /v1/resources/" + srv.Key.CRID}) {
		t.Fatalf("256 device ids: exit %d, requests %v, stderr %.200q", res.code, requestLog(srv), res.stderr.String())
	}
	// The bound on public keys is its own, with its own message.
	if msgGrantsRemoveTooManyPeople != "--remove accepts at most 256 device ids" {
		t.Fatalf("the message changed: %q", msgGrantsRemoveTooManyPeople)
	}
}

// TestRequestsOffSaysNoCountItWasNotGiven pins what turning access requests
// off says about the people who still have access. The count comes from the
// service's answer to the change. An answer that says nobody is approved adds
// nothing. An answer that leaves the list out is not "nobody": the output
// gives no count, and says that anyone approved earlier still has access.
func TestRequestsOffSaysNoCountItWasNotGiven(t *testing.T) {
	off := func(srv *apitest.Server) string {
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "requests", srv.Key.CRID, "--off"}})
		if res.code != 0 || res.stderr.Len() != 0 {
			t.Fatalf("--off: exit %d, stderr %q", res.code, res.stderr.String())
		}
		return res.stdout.String()
	}
	srv := apitest.NewServer(t)
	srv.SetAccessRequests(true)
	first := "Access requests are off for " + srv.Key.CRID + ". Nobody new can ask for access.\n"
	if got := off(srv); got != first {
		t.Fatalf("nobody approved: --off =\n%s\nwant the one line", got)
	}

	srv = apitest.NewServer(t)
	srv.SetAccessRequests(true)
	srv.AddApprovedPerson(requesterDevice, requesterName)
	srv.OmitApprovedPeople()
	want := "Access requests are off for " + srv.Key.CRID + ". Nobody new can ask for access.\n" +
		"Anyone you approved earlier still has access. See them, or take access away, with `qurl grants " + srv.Key.CRID + "`.\n"
	if got := off(srv); got != want {
		t.Fatalf("the list left out of the answer: --off =\n%s\nwant\n%s", got, want)
	}
}

// TestAccessRequestCommandsWithADeviceCredential runs every command for
// access requests the way a real install does: with the device's own
// credential, which the SDK lets be used only on the routes it lists. Both
// listings, the approval, the denial by code and the removal of an approved
// person are sent and do what they say, and so are the commands that use
// routes the SDK listed before. Every request carries the device credential
// and no other. The one command the pinned SDK does not send yet, a denial
// by device id, has its own test below.
func TestAccessRequestCommandsWithADeviceCredential(t *testing.T) {
	state := bootstrapRegisteredState(t)
	device := func(srv *apitest.Server) func(context.Context) (qurlapi.Client, error) {
		return func(ctx context.Context) (qurlapi.Client, error) {
			return qurlapi.NewRegistered(ctx, &qurlapi.Config{BaseURL: srv.URL, HTTPClient: srv.Client()}, &bootstrapAgentStateStore{state: state})
		}
	}
	for _, test := range []struct {
		name string
		args func(*apitest.Server) []string
		// want is in the output; sent is what the command sent.
		want string
		sent func(*apitest.Server) []string
		// check looks at what the command left behind.
		check func(*testing.T, *apitest.Server)
	}{
		{
			name: "requests", args: func(*apitest.Server) []string { return []string{"requests"} },
			want: `"Ana Lopez"   abcd-efgh-2345-mnop`,
			sent: func(*apitest.Server) []string { return []string{"GET /v1/access-requests"} },
		},
		{
			name: "requests for one resource", args: func(srv *apitest.Server) []string { return []string{"requests", srv.Key.CRID} },
			want: `"Sam Okafor"  qrst-uvwx-yz67-abcd`,
			sent: func(srv *apitest.Server) []string {
				return []string{"GET /v1/resources/" + srv.Key.CRID + "/access-requests"}
			},
		},
		{
			name: "approve", args: func(srv *apitest.Server) []string { return []string{"approve", srv.Key.CRID, requestCode} },
			want: "This person can now open",
			sent: func(srv *apitest.Server) []string {
				return []string{"POST /v1/resources/" + srv.Key.CRID + "/access-requests/" + requestCode + "/approve"}
			},
			check: func(t *testing.T, srv *apitest.Server) {
				if devices := approvedDevices(t, srv); !slices.Contains(devices, requesterDevice) {
					t.Fatalf("approved devices = %v, want the person whose code was given", devices)
				}
			},
		},
		{
			name: "deny by code", args: func(srv *apitest.Server) []string { return []string{"deny", srv.Key.CRID, requestCode} },
			want: "No access was given.",
			sent: func(srv *apitest.Server) []string {
				return []string{"DELETE /v1/resources/" + srv.Key.CRID + "/access-requests/" + requestCode}
			},
			check: func(t *testing.T, srv *apitest.Server) {
				if devices := approvedDevices(t, srv); slices.Contains(devices, requesterDevice) {
					t.Fatalf("approved devices = %v: a denial gave access", devices)
				}
			},
		},
		{
			name: "remove a person", args: func(srv *apitest.Server) []string {
				return []string{"grants", srv.Key.CRID, "--remove", otherDevice}
			},
			want: "Approved people:",
			sent: func(srv *apitest.Server) []string {
				return []string{"GET /v1/resources/" + srv.Key.CRID, "DELETE /v1/resources/" + srv.Key.CRID + "/allowed-passkeys/" + otherDevice, "GET /v1/resources/" + srv.Key.CRID}
			},
			check: func(t *testing.T, srv *apitest.Server) {
				if devices := approvedDevices(t, srv); slices.Contains(devices, otherDevice) {
					t.Fatalf("approved devices = %v: the person is still on the list", devices)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			twoRequests(srv)
			srv.AddApprovedPerson(otherDevice, otherRequester)
			res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL}, test.args(srv)...), env: map[string]string{}, openAPIClient: device(srv)})
			// A denial is confirmed on stderr; the others print a document.
			if res.code != 0 || !strings.Contains(res.stdout.String()+res.stderr.String(), test.want) {
				t.Fatalf("exit = %d, want 0 with %q\nstdout: %s\nstderr: %s", res.code, test.want, res.stdout.String(), res.stderr.String())
			}
			if log := requestLog(srv); !slices.Equal(log, test.sent(srv)) {
				t.Fatalf("requests = %v, want %v", log, test.sent(srv))
			}
			for _, request := range srv.Requests() {
				if got := request.Header.Get("Authorization"); got != "Bearer "+state.DeviceAPIKey {
					t.Errorf("%s %s authorization = %q, want the device credential", request.Method, request.Path, got)
				}
			}
			if test.check != nil {
				test.check(t, srv)
			}
		})
	}

	for _, test := range []struct {
		name string
		args func(*apitest.Server) []string
		want string
	}{
		{name: "publish", args: func(*apitest.Server) []string { return []string{"publish", privacyRemoteTarget, "--allow-requests"} }, want: "People can ask you for access"},
		{name: "requests on", args: func(srv *apitest.Server) []string { return []string{"requests", srv.Key.CRID, "--on"} }, want: "Access requests are on for"},
		{name: "requests off", args: func(srv *apitest.Server) []string { return []string{"requests", srv.Key.CRID, "--off"} }, want: "Access requests are off for"},
		{name: "grants", args: func(srv *apitest.Server) []string { return []string{"grants", srv.Key.CRID} }, want: "Approved people:      1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.AddApprovedPerson(requesterDevice, requesterName)
			res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL}, test.args(srv)...), env: map[string]string{}, openAPIClient: device(srv)})
			if res.code != 0 || !strings.Contains(res.stdout.String(), test.want) {
				t.Fatalf("exit = %d\nstdout: %s\nstderr: %s", res.code, res.stdout.String(), res.stderr.String())
			}
		})
	}
}

// TestDenyByDeviceIDWithADeviceCredentialIsNotSentYet records a limit of
// this release through the command, and fails when the limit is gone.
//
// A real install runs every command with the device's own credential, which
// goes through the SDK. The pinned SDK admits a denial only with a six-digit
// code in the path, so `qurl deny <CRID> <device id>` is refused before
// anything is sent. The command says so, says that nothing was sent and that
// the request gives no access unless it is approved, and exits 1 in every
// output mode with nothing on stdout. The request is still pending.
//
// When the SDK admits a device id there, this test fails. The fix is to move
// the case into TestAccessRequestCommandsWithADeviceCredential, where the
// denial is sent with the device credential and the request is gone, and to
// remove the message.
func TestDenyByDeviceIDWithADeviceCredentialIsNotSentYet(t *testing.T) {
	state := bootstrapRegisteredState(t)
	for _, mode := range [][]string{nil, {"-o", "json"}, {"--quiet"}} {
		srv := apitest.NewServer(t)
		twoRequests(srv)
		res := runCLI(t, &runOpts{
			args: append([]string{"--endpoint", srv.URL, "deny", srv.Key.CRID, requesterDevice}, mode...), env: map[string]string{},
			openAPIClient: func(ctx context.Context) (qurlapi.Client, error) {
				return qurlapi.NewRegistered(ctx, &qurlapi.Config{BaseURL: srv.URL, HTTPClient: srv.Client()}, &bootstrapAgentStateStore{state: state})
			},
		})
		want := "Error: this release of qurl cannot refuse a request by its device id with this device's identity yet, so nothing was sent. " +
			"The request gives no access unless you approve it, and it expires by itself. " +
			"To refuse it now, use its six-digit code if the person gave it to you\n"
		if res.code != exitcode.General || res.stderr.String() != want {
			t.Fatalf("%v: exit %d, stderr %q; want exit %d and %q. If the denial went through, the SDK now admits it: see this test's comment", mode, res.code, res.stderr.String(), exitcode.General, want)
		}
		mustEmptyStdout(t, res)
		if got := len(srv.Requests()); got != 0 {
			t.Fatalf("%v: a refused denial was sent: %v", mode, requestLog(srv))
		}
		if pending := pendingDevices(t, srv); !slices.Equal(pending, []string{otherDevice, requesterDevice}) {
			t.Fatalf("%v: pending after a denial that was not sent = %v", mode, pending)
		}
	}
}

// TestAccessRequestCopySaysToApproveOnlyGivenCodes pins the rule that keeps
// the scheme safe wherever a publisher, or an agent working for one, reads
// how to use it: approve a code only when the person gave it to you, because
// a name can be typed by anyone, and no command shows a code. It also pins
// that publish says the address and the CRID are safe to send to anyone.
func TestAccessRequestCopySaysToApproveOnlyGivenCodes(t *testing.T) {
	collapse := func(text string) string { return strings.Join(strings.Fields(text), " ") }
	help := func(args ...string) string {
		t.Helper()
		res := runCLI(t, &runOpts{args: append(args, "--help")})
		if res.code != 0 {
			t.Fatalf("qurl %s --help exit = %d", strings.Join(args, " "), res.code)
		}
		return collapse(res.stdout.String())
	}
	readme := collapse(strings.ReplaceAll(strings.ReplaceAll(readCLIREADME(t), "`", ""), "**", ""))
	for where, text := range map[string]string{"qurl requests --help": help("requests"), "qurl approve --help": help("approve"), "README": readme} {
		for _, want := range []string{"typed by whoever asked", "anyone"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not state the rule: missing %q", where, want)
			}
		}
	}
	for where, text := range map[string]string{"qurl approve --help": help("approve"), "README": readme} {
		for _, want := range []string{"gave it to you themselves", "the person who asked"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not state the rule: missing %q", where, want)
			}
		}
	}
	// No command shows a code, and each place a publisher reads about the
	// listing says so, and says how a request is refused without one.
	for where, wants := range map[string][]string{
		"qurl requests --help": {"The listing never shows a request's six-digit code, in any output mode.", "it is the only proof of who is asking", "qurl deny <CRID> <device id>", "The listing of all your resources is bounded.", "listing never needs it"},
		"qurl approve --help":  {"no qURL command shows it"},
		"qurl deny --help":     {"Name the request by the device id it came from, in the form xxxx-xxxx-xxxx-xxxx", "the code is accepted in the same place", "gives no access and expires by itself"},
	} {
		text := help(strings.Fields(where)[1])
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %q", where, want)
			}
		}
	}
	for where, text := range map[string]string{"qurl publish --help": help("publish"), "README": readme} {
		for _, want := range []string{"--allow-requests", "six-digit code", "The address and the CRID are safe to send to anyone", "a private resource opens only for you and the people you allow"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %q", where, want)
			}
		}
	}
	for _, want := range []string{
		"qurl requests <CRID> --on", "qurl approve <CRID> 123456", "qurl deny <CRID> <device id>", "qurl grants <CRID> --remove <device id>",
		"| qurl requests [<CRID>] |", "| qurl approve <CRID> <code> |", "| qurl deny <CRID> <device id> |",
		"this service does not offer access requests yet", "name_verified", "approved_people", "resource_url",
		"must approve only codes you passed on to it.",
		// No command shows a code, in the section and in the scripting
		// table, and the listing that may be incomplete says so.
		"It never shows a code.", "No qURL command shows it, in any output mode.", "It shows no code.", "No member holds a request's code.",
		"NAME DEVICE ID REQUESTED EXPIRES", "has_more: true", "has_more: always present, true when there may be more requests than the listing shows",
		"It also takes a six-digit code in the same place", "Listing requests never needs it",
		"crid, denied (true), and what you named the request by: device_id, or code",
		"the device id for requests <CRID>, the CRID and the device id for requests with no argument, the device id or the code you gave for deny",
		// The two JSON members that carry the text output's sentences, with
		// the sentences as the documents have them.
		"approval_rule, in both listings: \"" + strings.ReplaceAll(safetyLine, "`", "") + "\"",
		"name_note, in the approve document: \"The name was typed by the person who asked. Nobody checked it.\"",
		// A service without access requests, in both ways it can answer a
		// publish that asks for them.
		"A service that ignores the setting published the resource as private, without access requests",
		"A service that refuses the setting published nothing: run the command again without --allow-requests to publish the resource as private.",
		// What a removal says when it did not remove everyone it named.
		"checks every device id against it before it takes any access away",
		"Access that was taken away never reads as \"nothing was removed\".",
		"removed, not_found, not_removed", "for at most 256 people in one command",
		// What a command says when it removed every person and then could
		// not change the public keys.
		"that the change to the public keys failed and why, and the command that makes that change alone",
		"public_keys_command when every person was removed and the change to the public keys then failed",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("README lacks %q", want)
		}
	}
	// Nothing a publisher reads still says that a listing has codes.
	for _, gone := range []string{"CODE NAME", "of code (six digits)", "never a code it found in the listing", "the code for requests <CRID>"} {
		if strings.Contains(readme, gone) {
			t.Errorf("README still says %q", gone)
		}
	}
	grants := help("grants")
	for _, want := range []string{
		"--remove also takes the device id of an approved person", "It does not remove approved people.",
		"Every device id is checked against the list before any access is taken away.",
		"One that is not on the list is an error and removes nothing, so a mistyped id is never mistaken for access taken away.",
		"the error says exactly which device ids were removed and which were not",
		"If the change to public keys fails after the people were removed, the error says who lost access and gives the command that makes that change alone.",
		"for at most 256 people in one command",
	} {
		if !strings.Contains(grants, want) {
			t.Errorf("qurl grants --help lacks %q", want)
		}
	}
}
