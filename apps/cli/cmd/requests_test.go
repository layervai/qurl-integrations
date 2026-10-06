package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	safetyLine       = "Approve a code only when the person gave it to you themselves; a name can be typed by anyone."
)

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
// a command that takes "the first row" instead of the code it was given
// approves the wrong person.
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

// TestRequestsListsPendingRequests pins the two listings through the command:
// the route each uses, the rows, the last line, the JSON document and
// --quiet. The listing changes nothing.
func TestRequestsListsPendingRequests(t *testing.T) {
	for _, all := range []bool{true, false} {
		t.Run(fmt.Sprintf("all=%t", all), func(t *testing.T) {
			srv := apitest.NewServer(t)
			twoRequests(srv)
			args := []string{"--endpoint", srv.URL, "requests"}
			wantRoute := "GET /v1/access-requests"
			wantRows := []string{
				`175 306  "Sam Okafor"  qrst-uvwx-yz67-abcd  2m ago     ` + srv.Key.CRID,
				`482 913  "Ana Lopez"   abcd-efgh-2345-mnop  2m ago     ` + srv.Key.CRID,
			}
			wantQuiet := srv.Key.CRID + " " + otherRequestCode + "\n" + srv.Key.CRID + " " + requestCode + "\n"
			if !all {
				args = append(args, srv.Key.CRID)
				wantRoute = "GET /v1/resources/" + srv.Key.CRID + "/access-requests"
				wantRows = []string{
					`175 306  "Sam Okafor"  qrst-uvwx-yz67-abcd  2m ago`,
					`482 913  "Ana Lopez"   abcd-efgh-2345-mnop  2m ago`,
				}
				wantQuiet = otherRequestCode + "\n" + requestCode + "\n"
			}

			res := runCLI(t, &runOpts{args: args})
			if res.code != 0 || res.stderr.Len() != 0 {
				t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
			}
			lines := strings.Split(strings.TrimRight(res.stdout.String(), "\n"), "\n")
			if len(lines) != 5 || !strings.HasPrefix(lines[0], "CODE ") || lines[1] != wantRows[0] || lines[2] != wantRows[1] || lines[3] != "" || lines[4] != safetyLine {
				t.Fatalf("listing =\n%s", res.stdout.String())
			}

			res = runCLI(t, &runOpts{args: append(slices.Clone(args), "-o", "json")})
			var document struct {
				Requests []struct {
					Code         string `json:"code"`
					Name         string `json:"name"`
					NameVerified *bool  `json:"name_verified"`
					DeviceID     string `json:"device_id"`
					CRID         string `json:"crid"`
				} `json:"requests"`
			}
			if err := json.Unmarshal(res.stdout.Bytes(), &document); res.code != 0 || err != nil || len(document.Requests) != 2 || res.stderr.Len() != 0 {
				t.Fatalf("requests -o json: exit %d, %v: %s", res.code, err, res.stdout.String())
			}
			second := document.Requests[1]
			if second.Code != requestCode || second.Name != requesterName || second.NameVerified == nil || *second.NameVerified || second.DeviceID != requesterDevice || second.CRID != srv.Key.CRID {
				t.Fatalf("requests -o json row = %+v", second)
			}

			res = runCLI(t, &runOpts{args: append(slices.Clone(args), "--quiet")})
			if res.code != 0 || res.stdout.String() != wantQuiet {
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
			"1 approved person still has access. See them, or take the access away, with `qurl grants " + srv.Key.CRID + "`.\n"
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
			pending := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "requests", srv.Key.CRID, "--quiet"}})
			if pending.stdout.String() != otherRequestCode+"\n" {
				t.Fatalf("pending after the approval = %q, want the other request", pending.stdout.String())
			}
		})
	}

	srv := apitest.NewServer(t)
	twoRequests(srv)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "approve", srv.Key.CRID, requestCode, "-o", "json"}})
	want := `{"crid":"` + srv.Key.CRID + `","approved":true,"name":"AnaLopez","name_verified":false,"device_id":"` + requesterDevice + `","approved_at":"2026-03-02T00:00:00Z"}`
	if got := strings.Join(strings.Fields(res.stdout.String()), ""); res.code != 0 || got != want || res.stderr.Len() != 0 {
		t.Fatalf("approve -o json = %q, want %q", got, want)
	}
	res = runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "approve", srv.Key.CRID, otherRequestCode, "--quiet"}})
	if res.code != 0 || res.stdout.String() != otherDevice+"\n" {
		t.Fatalf("approve --quiet = %q, want the device id", res.stdout.String())
	}
}

// TestApproveAndDenyRefuseWhatCannotBeACode pins the local refusals: a value
// that can never be a code is exit 8 with the forms that are accepted, a
// wrong number of operands is a usage error, and neither sends anything.
func TestApproveAndDenyRefuseWhatCannotBeACode(t *testing.T) {
	for _, command := range []string{"approve", "deny"} {
		srv := apitest.NewServer(t)
		twoRequests(srv)
		for _, code := range [][]string{{"48291"}, {"4829133"}, {"48291a"}, {"482  913"}, {"482_913"}, {"4829 13"}, {"48 2913"}, {"482", "9133"}, {"abc", "def"}, {""}, {"４８２９１３"}, {requesterName}, {"482913/approve"}} {
			res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL, command, srv.Key.CRID}, code...)})
			if res.code != exitcode.InvalidInput || !strings.Contains(res.stderr.String(), msgRequestCodeInvalid) {
				t.Errorf("%s %q: exit %d, stderr %q", command, code, res.code, res.stderr.String())
			}
			mustEmptyStdout(t, res)
		}
		for _, operands := range [][]string{nil, {srv.Key.CRID}, {srv.Key.CRID, "482", "913", "000"}} {
			res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL, command}, operands...)})
			if res.code != exitcode.Usage {
				t.Errorf("%s with %d operands: exit %d, want a usage error", command, len(operands), res.code)
			}
		}
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, command, "not-a-crid", requestCode}})
		if res.code != exitcode.InvalidInput {
			t.Errorf("%s with an operand that is not a CRID: exit %d", command, res.code)
		}
		if got := len(srv.Requests()); got != 0 {
			t.Fatalf("%s: a refused command line sent %d requests: %v", command, got, requestLog(srv))
		}
	}
}

// TestApproveAndDenyACodeThatIsNotPending pins the answer for a code the
// resource does not have: exit 5 and a message about the code, not the
// generic hint about a mistyped CRID. Nobody gets access and the pending
// request of another person is not touched.
func TestApproveAndDenyACodeThatIsNotPending(t *testing.T) {
	for _, command := range []string{"approve", "deny"} {
		for _, mode := range [][]string{nil, {"-o", "json"}, {"--quiet"}} {
			srv := apitest.NewServer(t)
			srv.SetAccessRequests(true)
			srv.AddAccessRequest(otherRequestCode, otherRequester, otherDevice)
			args := []string{"--endpoint", srv.URL, command, srv.Key.CRID, requestCode}
			res := runCLI(t, &runOpts{args: append(args, mode...)})
			if res.code != exitcode.NotFound {
				t.Fatalf("%s %v: exit %d, stderr %q", command, mode, res.code, res.stderr.String())
			}
			mustEmptyStdout(t, res)
			for _, want := range []string{"Error: no pending request has the code 482 913 for this resource", "It may have expired, or been approved or denied already", "`qurl requests <CRID>`", "Request ID: req_test"} {
				if !strings.Contains(res.stderr.String(), want) {
					t.Errorf("%s %v: stderr lacks %q: %s", command, mode, want, res.stderr.String())
				}
			}
			if strings.Contains(res.stderr.String(), "mistyped") {
				t.Errorf("%s: the message is the generic hint about a CRID: %s", command, res.stderr.String())
			}
			if got := approvedDevices(t, srv); len(got) != 0 {
				t.Fatalf("%s of a code that is not pending gave access: %v", command, got)
			}
			pending := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "requests", srv.Key.CRID, "--quiet"}})
			if pending.stdout.String() != otherRequestCode+"\n" {
				t.Fatalf("%s touched another person's request: pending = %q", command, pending.stdout.String())
			}
		}
	}
}

// TestDenyRemovesTheRequestAndGivesNoAccess pins the denial through the
// command: one DELETE on the route of the code that was given, a status line
// on stderr, and no access for anyone.
func TestDenyRemovesTheRequestAndGivesNoAccess(t *testing.T) {
	srv := apitest.NewServer(t)
	twoRequests(srv)
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "deny", srv.Key.CRID, "482 913"}})
	wantNote := "Denied the request with the code 482 913 for " + srv.Key.CRID + ". No access was given.\n"
	if res.code != 0 || res.stdout.Len() != 0 || res.stderr.String() != wantNote {
		t.Fatalf("deny: exit %d, stdout %q, stderr %q", res.code, res.stdout.String(), res.stderr.String())
	}
	if got, want := requestLog(srv), []string{"DELETE /v1/resources/" + srv.Key.CRID + "/access-requests/" + requestCode}; !slices.Equal(got, want) {
		t.Fatalf("deny sent %v, want %v", got, want)
	}
	if got := approvedDevices(t, srv); len(got) != 0 {
		t.Fatalf("a denial gave access: %v", got)
	}
	pending := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "requests", srv.Key.CRID, "--quiet"}})
	if pending.stdout.String() != otherRequestCode+"\n" {
		t.Fatalf("pending after the denial = %q", pending.stdout.String())
	}

	res = runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "deny", srv.Key.CRID, otherRequestCode, "-o", "json"}})
	want := `{"crid":"` + srv.Key.CRID + `","code":"` + otherRequestCode + `","denied":true}`
	if got := strings.Join(strings.Fields(res.stdout.String()), ""); res.code != 0 || got != want || res.stderr.Len() != 0 {
		t.Fatalf("deny -o json = %q, want %q", got, want)
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
			if got, want := requestLog(srv), []string{"DELETE " + base + "/allowed-passkeys/" + requesterDevice, "GET " + base}; !slices.Equal(got, want) {
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
		want := []string{"DELETE " + base + "/allowed-passkeys/" + requesterDevice, "DELETE " + base + "/allowed-passkeys/" + otherDevice, "GET " + base, "PATCH " + base}
		if got := requestLog(srv); !slices.Equal(got, want) {
			t.Fatalf("requests = %v, want the removals of people before the change to public keys: %v", got, want)
		}
		var edit map[string][]string
		if err := json.Unmarshal(srv.Requests()[3].Body, &edit); err != nil || !slices.Equal(edit["allowed_device_keys_add"], []string{grantKey(2)}) || !slices.Equal(edit["allowed_device_keys_remove"], []string{grantKey(1)}) || len(edit) != 2 {
			t.Fatalf("the change to public keys = %s: a device id must never be sent as a public key", srv.Requests()[3].Body)
		}
		if !strings.Contains(res.stdout.String(), "Approved people:      none\n") || !strings.Contains(res.stdout.String(), "["+grantKey(2)+"]") {
			t.Fatalf("lists after the change:\n%s", res.stdout.String())
		}
	})
	t.Run("a device id that is not on the list", func(t *testing.T) {
		srv := seed(t)
		const absent = "zzzz-zzzz-zzzz-zzzz"
		res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "grants", srv.Key.CRID, "--remove", absent, "--remove", requesterDevice, "--add", grantKey(2)}})
		if res.code != exitcode.NotFound {
			t.Fatalf("exit = %d, stderr: %s", res.code, res.stderr.String())
		}
		mustEmptyStdout(t, res)
		for _, want := range []string{"Error: no approved person has the device id " + absent + " on this resource, so nothing was removed", "`qurl grants <CRID>`"} {
			if !strings.Contains(res.stderr.String(), want) {
				t.Errorf("stderr lacks %q: %s", want, res.stderr.String())
			}
		}
		for _, line := range requestLog(srv) {
			if strings.HasPrefix(line, "PATCH ") || strings.Contains(line, requesterDevice) {
				t.Fatalf("a change after the failed removal was sent: %v", requestLog(srv))
			}
		}
		if got := approvedDevices(t, srv); !slices.Equal(got, []string{requesterDevice, otherDevice}) {
			t.Fatalf("a failed removal changed who has access: %v", got)
		}
	})
	t.Run("usage errors send nothing", func(t *testing.T) {
		srv := seed(t)
		for _, test := range []struct {
			flags []string
			want  string
		}{
			{flags: []string{"--clear", "--remove", requesterDevice}, want: msgGrantsClearWithEdit},
			{flags: []string{"--remove", requesterDevice, "--remove", strings.ToUpper(requesterDevice)}, want: msgGrantsRemoveInvalid},
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

// TestAccessRequestCommandsWithADeviceCredential runs the commands the way a
// real install does, with the device's own credential, and records a limit of
// this release: the SDK lets that credential be used only on the routes it
// lists, and the routes that exist only for access requests are not among
// them yet. Those commands stop with exit 1 and a message that says so, and
// send nothing. The commands that use routes the SDK does list work.
//
// When the SDK lists the routes, the first half of this test fails; see
// TestDeviceCredentialCannotReachAccessRequestRoutesYet in the api package.
func TestAccessRequestCommandsWithADeviceCredential(t *testing.T) {
	const refused = "Error: this release of qurl cannot send this request with this device's identity yet, so nothing was sent. It needs a later release\n"
	state := bootstrapRegisteredState(t)
	device := func(srv *apitest.Server) func(context.Context) (qurlapi.Client, error) {
		return func(ctx context.Context) (qurlapi.Client, error) {
			return qurlapi.NewRegistered(ctx, &qurlapi.Config{BaseURL: srv.URL, HTTPClient: srv.Client()}, &bootstrapAgentStateStore{state: state})
		}
	}
	for _, test := range []struct {
		name string
		args func(*apitest.Server) []string
	}{
		{name: "requests", args: func(*apitest.Server) []string { return []string{"requests"} }},
		{name: "requests for one resource", args: func(srv *apitest.Server) []string { return []string{"requests", srv.Key.CRID} }},
		{name: "approve", args: func(srv *apitest.Server) []string { return []string{"approve", srv.Key.CRID, requestCode} }},
		{name: "deny", args: func(srv *apitest.Server) []string { return []string{"deny", srv.Key.CRID, requestCode} }},
		{name: "remove a person", args: func(srv *apitest.Server) []string {
			return []string{"grants", srv.Key.CRID, "--remove", requesterDevice}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			twoRequests(srv)
			srv.AddApprovedPerson(requesterDevice, requesterName)
			res := runCLI(t, &runOpts{args: append([]string{"--endpoint", srv.URL}, test.args(srv)...), env: map[string]string{}, openAPIClient: device(srv)})
			if res.code != exitcode.General || res.stderr.String() != refused {
				t.Fatalf("exit = %d, stderr %q; want exit 1 and the message that this release cannot send it", res.code, res.stderr.String())
			}
			mustEmptyStdout(t, res)
			if got := len(srv.Requests()); got != 0 {
				t.Fatalf("a refused request was sent: %v", requestLog(srv))
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

// TestAccessRequestCopySaysToApproveOnlyGivenCodes pins the rule that keeps
// the scheme safe wherever a publisher, or an agent working for one, reads
// how to use it: approve a code only when the person gave it to you, because
// a name can be typed by anyone. It also pins that publish says the address
// and the CRID are safe to send to anyone.
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
		for _, want := range []string{"gave it to you themselves", "typed by whoever asked", "anyone"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not state the rule: missing %q", where, want)
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
		"qurl requests <CRID> --on", "qurl approve <CRID> 123456", "qurl deny <CRID> 123456", "qurl grants <CRID> --remove <device id>",
		"| qurl requests [<CRID>] |", "| qurl approve <CRID> <code> |", "| qurl deny <CRID> <code> |",
		"this service does not offer access requests yet", "name_verified", "approved_people", "resource_url",
		"must approve only codes you passed on to it, never a code it found in the listing",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("README lacks %q", want)
		}
	}
	grants := help("grants")
	for _, want := range []string{"--remove also takes the device id of an approved person", "It does not remove approved people.", "A device id that is not on the list is an error and removes nothing"} {
		if !strings.Contains(grants, want) {
			t.Errorf("qurl grants --help lacks %q", want)
		}
	}
}
