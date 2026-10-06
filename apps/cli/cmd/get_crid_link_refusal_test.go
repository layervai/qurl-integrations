package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// Tests for how `qurl get` tells the user that the service refused to give a
// link for a CRID. The refusal comes from the SDK's own test server through
// the real SDK call, so the error get reads is the one the SDK builds from a
// server's answer, not one a test put together. get_crid_link_offer_test.go
// has the helpers and says what that server can and cannot do.

// TestGetRefusalsThroughTheSDK pins how get tells the user about a refusal
// that came from a server through the SDK, on a machine with no identity:
// the three refusals a user is most likely to meet, and two codes outside
// the set this request defines.
//
// 51002 is what a service answers when the part of it that was asked does
// not serve this request. It and any other code outside the set read as "try
// again later". Neither may tell the user to update qurl: nothing is wrong
// with the client.
func TestGetRefusalsThroughTheSDK(t *testing.T) {
	for _, tc := range []struct {
		code     string
		wantCode int
		golden   string
	}{
		{"52601", exitcode.Unavailable, "error_get_crid_unavailable"},
		{"52602", exitcode.NotFound, "error_get_crid_notfound_no_device"},
		{"52603", exitcode.RateLimited, "error_get_crid_ratelimited"},
		{"51002", exitcode.Unavailable, "error_get_crid_unavailable"},
		{"52005", exitcode.Unavailable, "error_get_crid_unavailable"},
	} {
		for _, mode := range getModes() {
			t.Run(tc.code+"/"+mode.name, func(t *testing.T) {
				path := newSDKLinkPath(t, nil)
				path.server.Refuse(tc.code)
				// A test CRID: the server refuses whatever CRID is asked for,
				// and a test CRID draws no wrong-environment warning.
				srv := downloadServer(t)
				stateDir := filepath.Join(t.TempDir(), "no-device-state")
				var verified, granted []string

				run := runShareMode(t, srv, srv.URL, mode, path.wire(t, srv, machineWithNoIdentity(t, stateDir), &verified, &granted))
				stderr := run.result.stderr.String()
				if run.result.code != tc.wantCode {
					t.Fatalf("exit = %d, want %d; stderr: %s", run.result.code, tc.wantCode, stderr)
				}
				run.mustNotHaveActed(t)
				if !mode.tty {
					if want := goldenBytes(t, tc.golden+".plain.stderr.golden"); stderr != want {
						t.Errorf("stderr = %q, want the golden %q", stderr, want)
					}
				}
				if strings.Contains(stderr, tc.code) {
					t.Errorf("stderr %q carries the service's code without --verbose", stderr)
				}
				if strings.Contains(strings.ToLower(stderr), "update") {
					t.Errorf("stderr %q advises an update for code %s", stderr, tc.code)
				}

				if got := len(path.server.Requests()); got != 1 {
					t.Errorf("the service answered %d request(s), want one and no retry", got)
				}
				if len(verified) != 0 || len(granted) != 0 {
					t.Errorf("a refusal checked %d link(s) and requested access for %d", len(verified), len(granted))
				}
				if got := srv.Requests(); len(got) != 0 {
					t.Errorf("the machine sent %d request(s) to the qURL API, want none", len(got))
				}
				mustNotExistCmd(t, stateDir)
			})
		}
	}
}

// TestGetShowsTheRefusalCodeOnlyWithVerbose pins where the service's code is
// visible. The message for the user never carries it, because several codes
// share one message. --verbose adds one diagnostic line with the code, for
// whoever looks into a failure, and the message stays the same.
func TestGetShowsTheRefusalCodeOnlyWithVerbose(t *testing.T) {
	state := bootstrapRegisteredState(t)
	for _, code := range []string{"51002", "52005", "52602"} {
		for _, device := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/device=%t", code, device), func(t *testing.T) {
				path := newSDKLinkPath(t, nil)
				path.server.Refuse(code)
				srv := downloadServer(t)
				stateDir := filepath.Join(t.TempDir(), "no-device-state")
				machine := machineWithNoIdentity(t, stateDir)
				if device {
					machine = enrolledDevice(t, state)
					shareNotFoundTwice(t, srv)
				}
				var verified, granted []string
				configure := func(args []string) *runOpts {
					return path.wire(t, srv, machine, &verified, &granted)(append(args, "--verbose"))
				}

				file := getModes()[1]
				if file.name != "get file" {
					t.Fatalf("second get mode is %q, want get file", file.name)
				}
				run := runShareMode(t, srv, srv.URL, file, configure)
				stderr := run.result.stderr.String()

				diagnostic := "[debug] < CRID link request refused, code " + code + "\n"
				if strings.Count(stderr, diagnostic) != 1 {
					t.Fatalf("stderr = %q, want the diagnostic line %q once", stderr, diagnostic)
				}
				// The code appears in that line and nowhere else.
				if strings.Count(stderr, code) != 1 {
					t.Errorf("stderr %q carries the code outside the diagnostic line", stderr)
				}
				if !device {
					// A machine with no identity sends nothing else, so the
					// diagnostic and the message are the whole output.
					golden := "error_get_crid_unavailable"
					if code == "52602" {
						golden = "error_get_crid_notfound_no_device"
					}
					if want := diagnostic + goldenBytes(t, golden+".plain.stderr.golden"); stderr != want {
						t.Errorf("stderr = %q, want %q", stderr, want)
					}
				}
				if code != "52602" && strings.Contains(strings.ToLower(stderr), "update") {
					t.Errorf("stderr %q advises an update for code %s", stderr, code)
				}
				run.mustNotHaveActed(t)
			})
		}
	}
}
