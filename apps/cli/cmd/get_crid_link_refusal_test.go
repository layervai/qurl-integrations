package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// Tests for how `qurl get` tells the user that no link was given for a CRID:
// the service refused, or the SDK would not ask. The refusal comes from the
// SDK's own test server through the real SDK call, or from the SDK itself,
// so the error get reads is the one the SDK builds, not one a test put
// together. get_crid_link_offer_test.go has the helpers and says what that
// server can and cannot do.

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

// TestGetSaysWhyNoLinkWasAskedForOnlyWithVerbose pins the diagnostic line for
// a CRID the SDK will not ask a link for. Without --verbose nothing is added
// to what the user is told. With --verbose there is one more line, and the
// message for the user is the same, byte for byte.
//
// The line is fixed text and one word for the class of the cause. The SDK's
// own error text is never shown: for a character outside the CRID alphabet
// it quotes the byte and its position, and for a wrong length the length.
//
// Only one of these causes can reach get from a real user: a CRID version
// this client cannot check. The CLI's own check refuses the others first. To
// run that branch anyway, the test puts itself between get and the SDK: get
// is given a valid CRID, and the real SDK call is made with another operand,
// one the SDK refuses. So the error get reads is the SDK's own, and the
// SDK's test server shows that nothing was sent.
func TestGetSaysWhyNoLinkWasAskedForOnlyWithVerbose(t *testing.T) {
	state := bootstrapRegisteredState(t)
	// Another resource, so no part of an operand can appear in the output
	// for a reason of its own.
	other := apitest.GenerateResourceKey(t)
	file := getModes()[1]
	if file.name != "get file" {
		t.Fatalf("second get mode is %q, want get file", file.name)
	}
	debugLine := func(class string) string {
		return "[debug] " + fmt.Sprintf(msgCRIDLinkNotSent, class) + "\n"
	}
	// withoutDebugLines is what the user is told: stderr without the
	// diagnostics --verbose adds.
	withoutDebugLines := func(stderr string) string {
		var kept strings.Builder
		for line := range strings.SplitAfterSeq(stderr, "\n") {
			if !strings.HasPrefix(line, "[debug] ") {
				kept.WriteString(line)
			}
		}
		return kept.String()
	}

	for _, tc := range []struct {
		name string
		// operand is what the SDK is asked for.
		operand string
		class   string
		// sdkWords are pieces of the SDK's own text for this cause.
		sdkWords []string
		// noDeviceCode and noDeviceMessage are what a machine with no
		// identity is told. A device with an identity is told what its share
		// request answered.
		noDeviceCode    int
		noDeviceMessage string
	}{
		{
			name: "version this client cannot check", operand: apitest.DeriveCRID(t, other.DER, 0x05),
			class: "unsupported_version", sdkWords: []string{"0x05", "cannot be verified"},
			noDeviceCode: exitcode.Config, noDeviceMessage: goldenBytes(t, "error_get_crid_version.plain.stderr.golden"),
		},
		{
			name: "character outside the alphabet", operand: other.CRID[:20] + "!" + other.CRID[21:],
			class: "charset", sdkWords: []string{"0x21", "index 20", "alphabet"},
			noDeviceCode: exitcode.InvalidInput, noDeviceMessage: "Error: " + msgValidCRIDRequired + "\n",
		},
		{
			name: "wrong length", operand: other.CRID[:59],
			class: "length", sdkWords: []string{"59 characters"},
			noDeviceCode: exitcode.InvalidInput, noDeviceMessage: "Error: " + msgValidCRIDRequired + "\n",
		},
		{
			name: "mistyped", operand: mistypedCRID(other.CRID),
			class: "checksum", sdkWords: []string{"crc32c", "does not match"},
			noDeviceCode: exitcode.InvalidInput, noDeviceMessage: "Error: " + msgValidCRIDRequired + "\n",
		},
	} {
		for _, device := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/device=%t", tc.name, device), func(t *testing.T) {
				wantCode, wantMessage := tc.noDeviceCode, tc.noDeviceMessage
				if device {
					wantCode, wantMessage = exitcode.NotFound, goldenBytes(t, "error_share_notfound.plain.stderr.golden")
				}
				run := func(t *testing.T, verbose bool) string {
					t.Helper()
					path := newSDKLinkPath(t, nil)
					srv := downloadServer(t)
					stateDir := filepath.Join(t.TempDir(), "no-device-state")
					machine := machineWithNoIdentity(t, stateDir)
					if device {
						machine = enrolledDevice(t, state)
						shareNotFoundTwice(t, srv)
					}
					var sdkErr error
					ask := func(ctx context.Context, _ string) (*qurl.CRIDLink, error) {
						link, err := path.opener.RequestCRIDLink(ctx, tc.operand)
						sdkErr = err
						return link, err
					}
					configure := func(args []string) *runOpts {
						if verbose {
							args = append(args, "--verbose")
						}
						return withLinkRequests(machine, ask)(args)
					}

					result := runShareMode(t, srv, srv.URL, file, configure)
					stderr := result.result.stderr.String()
					if result.result.code != wantCode {
						t.Fatalf("verbose=%t: exit = %d, want %d; stderr: %s", verbose, result.result.code, wantCode, stderr)
					}
					result.mustNotHaveActed(t)
					if sdkErr == nil {
						t.Fatalf("verbose=%t: the SDK was not asked, or it did not refuse the operand", verbose)
					}
					if got := len(path.server.Requests()); got != 0 {
						t.Errorf("verbose=%t: the service answered %d request(s), want none", verbose, got)
					}
					// The case is real: the SDK's text holds these words, and
					// none of them may reach the reader.
					for _, word := range tc.sdkWords {
						if !strings.Contains(sdkErr.Error(), word) {
							t.Fatalf("the SDK's error %q does not hold %q: the row no longer tests what it names", sdkErr, word)
						}
						if strings.Contains(strings.ReplaceAll(stderr, debugLine(tc.class), ""), word) {
							t.Errorf("verbose=%t: stderr %q carries %q from the SDK's own error text", verbose, stderr, word)
						}
					}
					if strings.Contains(stderr, tc.operand) {
						t.Errorf("verbose=%t: stderr %q carries the operand the SDK refused", verbose, stderr)
					}
					if !device {
						mustNotExistCmd(t, stateDir)
					}
					return stderr
				}

				quiet := run(t, false)
				if quiet != wantMessage {
					t.Errorf("stderr = %q, want exactly %q", quiet, wantMessage)
				}

				verbose := run(t, true)
				if got := strings.Count(verbose, debugLine(tc.class)); got != 1 {
					t.Errorf("stderr = %q, want the diagnostic line %q once, found %d", verbose, debugLine(tc.class), got)
				}
				if got := strings.Count(verbose, "CRID link request not sent"); got != 1 {
					t.Errorf("stderr = %q, want one line about the request that was not sent, found %d", verbose, got)
				}
				if got := withoutDebugLines(verbose); got != wantMessage {
					t.Errorf("with --verbose the user is told %q, want the same message %q", got, wantMessage)
				}
			})
		}
	}
}
