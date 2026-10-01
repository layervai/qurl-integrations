package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	"github.com/layervai/qurl-go/qurl"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// Tests for the one share path `qurl share` and `qurl get` have in common.
// The contract: every share request carries this device's credential, none is
// ever sent without one, and the service's answer for a device that is
// neither the owner's nor allowed is the not-found result, whose guidance
// covers every cause the CLI cannot tell apart.
//
// These runs use the production registered-device path, not the harness's
// account-key client. Only the native runtime is a fake, because it is the
// boundary to the platform's enrollment protocol: it hands off device state,
// and the real registered REST client authenticates with the credential in
// that state. No account key exists anywhere in these runs.

// shareMode is one way a CRID becomes a share request: `share` itself, and
// get's browser, file, and stdout actions.
type shareMode struct {
	name string
	// tty selects get's browser action; every other mode runs piped.
	tty bool
	// downloads marks the modes that fetch the content after sharing. They
	// deliver the content; the others deliver the link.
	downloads bool
	// args builds the command line; dest is this run's --file destination.
	args func(crid, dest string) []string
	// delivered returns what a successful run handed the user.
	delivered func(t *testing.T, run *shareRun) string
}

func shareModes() []shareMode {
	return []shareMode{
		{
			name: "share",
			args: func(crid, _ string) []string { return []string{"share", crid} },
			delivered: func(_ *testing.T, run *shareRun) string {
				return strings.TrimSuffix(run.result.stdout.String(), "\n")
			},
		},
		{
			name: "get browser", tty: true,
			args:      func(crid, _ string) []string { return []string{"get", crid} },
			delivered: func(_ *testing.T, run *shareRun) string { return strings.Join(run.browser.opened, "\n") },
		},
		{
			name: "get file", downloads: true,
			args:      func(crid, dest string) []string { return []string{"get", crid, "--file", dest} },
			delivered: func(t *testing.T, run *shareRun) string { return string(readTestFile(t, run.dest)) },
		},
		{
			name: "get stdout", downloads: true,
			args:      func(crid, _ string) []string { return []string{"get", crid, "--file", "-"} },
			delivered: func(_ *testing.T, run *shareRun) string { return run.result.stdout.String() },
		},
	}
}

// shareRun is one CLI invocation of a shareMode against a mock server.
type shareRun struct {
	result  *runResult
	browser *fakeBrowser
	dest    string
	link    string
}

// runShareMode runs mode against srv. configure builds the invocation from
// the complete argument list, so each test chooses how the device opens.
func runShareMode(t *testing.T, srv *apitest.Server, endpoint string, mode shareMode, configure func(args []string) *runOpts) *shareRun {
	t.Helper()
	run := &shareRun{
		browser: &fakeBrowser{},
		dest:    filepath.Join(t.TempDir(), "content"),
		link:    srv.URL + apitest.DownloadPath,
	}
	opts := configure(append([]string{"--endpoint", endpoint}, mode.args(srv.Key.CRID, run.dest)...))
	opts.tty, opts.browser = mode.tty, run.browser
	run.result = runCLI(t, opts)
	return run
}

// mustHaveDelivered asserts the mode succeeded and handed over exactly its
// own result: the minted link, printed or opened once, or the content.
func (r *shareRun) mustHaveDelivered(t *testing.T, mode shareMode) {
	t.Helper()
	if r.result.code != 0 {
		t.Fatalf("exit = %d, stderr: %s", r.result.code, r.result.stderr.String())
	}
	want := r.link
	if mode.downloads {
		want = apitest.DefaultDownloadPayload
	}
	if got := mode.delivered(t, r); got != want {
		t.Errorf("%s delivered %q, want %q", mode.name, got, want)
	}
}

// mustNotHaveActed asserts a failed share left nothing behind: no link and
// no bytes on stdout, no browser launch, and no download file.
func (r *shareRun) mustNotHaveActed(t *testing.T) {
	t.Helper()
	mustEmptyStdout(t, r.result)
	if len(r.browser.opened) != 0 {
		t.Errorf("a failed share still launched a browser: %q", r.browser.opened)
	}
	mustNotExistCmd(t, r.dest)
	mustNotExistCmd(t, r.dest+".part")
}

// shareRequests returns the requests srv saw on the share route.
func shareRequests(srv *apitest.Server) []apitest.RecordedRequest {
	var shares []apitest.RecordedRequest
	for _, request := range srv.Requests() {
		if request.Method == http.MethodPost && request.Path == shareRoute(srv) {
			shares = append(shares, request)
		}
	}
	return shares
}

// registeredDevice returns the invocation builder for a device that already
// holds state: opening it hands off that state and enrolls nothing.
func registeredDevice(t *testing.T, state *qurl.AgentState) func(args []string) *runOpts {
	t.Helper()
	return func(args []string) *runOpts {
		return registeredDeviceOpts(t, args, func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
			return &bootstrapNativeRuntime{store: &bootstrapAgentStateStore{state: state}}, nil
		})
	}
}

// registeredDeviceOpts routes newClient through the production
// registered-device open with openRuntime as its native runtime.
func registeredDeviceOpts(
	t *testing.T,
	args []string,
	openRuntime func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error),
) *runOpts {
	t.Helper()
	// The open asks which key storage a new namespace would get so it can
	// print the sealing notice; answer for it rather than probe this host.
	original := connectorstate.ResolveKeyProvider
	connectorstate.ResolveKeyProvider = func(string) (string, error) { return connectoragentstate.KeyProviderFile, nil }
	t.Cleanup(func() { connectorstate.ResolveKeyProvider = original })
	return &runOpts{
		args:              args,
		env:               map[string]string{},
		nativeClient:      true,
		shareStateDir:     connectorStateTestDir(t),
		openNativeRuntime: openRuntime,
	}
}

// otherRegisteredState is a second device: the same shape as
// bootstrapRegisteredState with its own credential.
//
// TODO(upstream-contract): qurl-go opens persisted device state only when the
// device credential has the one shape the platform mints, so a test-prefixed
// value cannot stand in for it. The prefix is therefore taken from the shared
// fixture instead of being spelled out here, and the rest is plainly
// synthetic.
func otherRegisteredState(t *testing.T) *qurl.AgentState {
	t.Helper()
	state := bootstrapRegisteredState(t)
	parts := strings.SplitAfterN(state.DeviceAPIKey, "_", 3)
	if len(parts) != 3 {
		t.Fatalf("shared device fixture credential has %d underscore-separated parts, want a two-part prefix and a body", len(parts))
	}
	state.AgentID = "agent-durable-02"
	state.DeviceAPIKey = parts[0] + parts[1] + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x41}, 32))
	state.DeviceAPIKeyID = "key_ZyXwVu654321"
	return state
}

// TestShareAndGetSendTheDeviceCredential pins the wire contract: every share
// request share and get send carries this device's credential, including the
// renewal after a link expired mid-download, and the credential never follows
// the link to the download host. The device proves its identity once per
// process, when the client opens, however many share requests follow. An
// endpoint written with its /v1 suffix reaches the same routes with the same
// credential.
func TestShareAndGetSendTheDeviceCredential(t *testing.T) {
	state := bootstrapRegisteredState(t)
	want := "Bearer " + state.DeviceAPIKey
	for _, endpoint := range []struct{ name, suffix string }{{"origin endpoint", ""}, {"v1 endpoint", "/v1"}} {
		for _, mode := range shareModes() {
			t.Run(endpoint.name+"/"+mode.name, func(t *testing.T) {
				srv := downloadServer(t)
				wantShares := 1
				if mode.downloads {
					// The first link expires before any byte is served, so the
					// download renews it with a second share request.
					srv.Script(http.MethodGet, apitest.DownloadPath, handlerGone)
					wantShares = 2
				}

				run := runShareMode(t, srv, srv.URL+endpoint.suffix, mode, registeredDevice(t, state))
				run.mustHaveDelivered(t, mode)

				shares := shareRequests(srv)
				if len(shares) != wantShares {
					t.Fatalf("share requests = %d, want %d", len(shares), wantShares)
				}
				for i, request := range shares {
					if got := request.Header.Get("Authorization"); got != want {
						t.Errorf("share request %d did not carry the device credential (Authorization is %d bytes)", i+1, len(got))
					}
				}
				identityChecks := 0
				for _, request := range srv.Requests() {
					switch {
					case request.Method == http.MethodGet && request.Path == "/v1/me":
						identityChecks++
						if got := request.Header.Get("Authorization"); got != want {
							t.Errorf("identity request did not carry the device credential (Authorization is %d bytes)", len(got))
						}
					case request.Path == apitest.DownloadPath && request.Header.Get("Authorization") != "":
						t.Error("the device credential followed the minted link to the download host")
					}
				}
				// One per process, not one per share: the renewal above reuses
				// the client that is already open.
				if identityChecks != 1 {
					t.Errorf("identity requests = %d for %d share requests, want exactly one per process", identityChecks, wantShares)
				}
			})
		}
	}
}

// shareNeedsDevice is the context shareResource puts in front of a failure to
// open this device's identity.
const shareNeedsDevice = "sharing needs this device's identity: "

// TestShareIsNeverSentWithoutACredential pins the other half: when this
// device's credential cannot be had, share and get stop before the share
// route instead of trying it without one. The mock fails any test that sends
// it such a request; these cases also show none was attempted. The error
// says that sharing needed the device and then gives the cause, with the
// cause's own exit code.
func TestShareIsNeverSentWithoutACredential(t *testing.T) {
	errDeviceUnavailable := errors.New("device state is unavailable")
	for _, cause := range []struct {
		name     string
		prepare  func(srv *apitest.Server)
		device   func(t *testing.T) func(args []string) *runOpts
		wantCode int
		wantText []string
	}{
		{
			name: "device cannot be opened",
			device: func(t *testing.T) func(args []string) *runOpts {
				return func(args []string) *runOpts {
					return registeredDeviceOpts(t, args, func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
						return nil, errDeviceUnavailable
					})
				}
			},
			wantCode: exitcode.General,
			wantText: []string{shareNeedsDevice + errDeviceUnavailable.Error()},
		},
		{
			// The identity check every registered open makes is refused, so
			// the device has no credential the service accepts. A service
			// answer renders through its own anatomy, which shows the cause;
			// TestShareResourceSaysSharingNeededTheDevice checks that the
			// context is on that error too.
			name: "service rejects the device credential",
			prepare: func(srv *apitest.Server) {
				srv.Script(http.MethodGet, "/v1/me", apitest.HandlerAPIKeyInvalid401(t))
			},
			device: func(t *testing.T) func(args []string) *runOpts {
				return registeredDevice(t, bootstrapRegisteredState(t))
			},
			wantCode: exitcode.Auth,
			wantText: []string{"Unauthorized (HTTP 401)", "the provided API key is not valid"},
		},
	} {
		for _, mode := range shareModes() {
			t.Run(cause.name+"/"+mode.name, func(t *testing.T) {
				srv := downloadServer(t)
				if cause.prepare != nil {
					cause.prepare(srv)
				}

				run := runShareMode(t, srv, srv.URL, mode, cause.device(t))
				if run.result.code != cause.wantCode {
					t.Fatalf("exit = %d, want %d; stderr: %s", run.result.code, cause.wantCode, run.result.stderr.String())
				}
				for _, want := range cause.wantText {
					if !strings.Contains(run.result.stderr.String(), want) {
						t.Errorf("stderr = %q, want it to say %q", run.result.stderr.String(), want)
					}
				}
				run.mustNotHaveActed(t)
				if shares := shareRequests(srv); len(shares) != 0 {
					t.Fatalf("share route was contacted %d times without a usable device credential", len(shares))
				}
				for _, request := range srv.Requests() {
					if request.Path == apitest.DownloadPath {
						t.Fatal("content was requested although no link was minted")
					}
				}
			})
		}
	}
}

// TestShareResourceSaysSharingNeededTheDevice pins the context shareResource
// adds when the device client cannot open, for every kind of cause: one the
// renderer prints as text, and ones it renders through their own anatomy and
// so never prints the context for. The message leads with what sharing
// needed, the cause stays reachable, and the exit code is the cause's own.
func TestShareResourceSaysSharingNeededTheDevice(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
	}{
		{"plain failure", errors.New("device state is unavailable")},
		{"no state directory", fmt.Errorf("%w: set %s", connectorstate.ErrNoDefaultStateDir, connectorstate.EnvStateDirPrimary)},
		{"supervised the other way", fmt.Errorf("%w is %q, not %q", connectorstate.ErrRuntimeSupervision, "external", "native")},
		{"no credential", auth.ErrNoCredential},
		{"service rejects the device credential", &qurlapi.Error{StatusCode: http.StatusUnauthorized, Code: "api_key_invalid", Title: "Unauthorized"}},
		{"interrupted", context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := &globalOpts{openAPIClient: func(context.Context) (qurlapi.Client, error) { return nil, tc.cause }}
			link, err := opts.shareResource(context.Background(), exampleCRID, qurlapi.ShareOptions{})
			if link != nil || !errors.Is(err, tc.cause) {
				t.Fatalf("shareResource = %v, %v; want no link and the cause %v", link, err, tc.cause)
			}
			if want := shareNeedsDevice + tc.cause.Error(); err.Error() != want {
				t.Errorf("error = %q, want %q", err.Error(), want)
			}
			if got, want := exitcode.FromError(err), exitcode.FromError(tc.cause); got != want {
				t.Errorf("exit code = %d, want the cause's own %d", got, want)
			}
		})
	}
}

// TestShareAndGetNeedAUsableDeviceNamespace pins two preconditions share and
// get have because they use the device's identity, the same two publish and
// list have: a state directory the identity can live in, and a namespace
// supervised the way the command was invoked. Either one stops the command
// before any request, with an error that says sharing needed the device and
// then names the problem, and with the configuration exit code.
func TestShareAndGetNeedAUsableDeviceNamespace(t *testing.T) {
	mustNotOpen := func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
		t.Error("the device runtime was opened in a namespace the command cannot use")
		return nil, errors.New("unexpected device runtime open")
	}
	for _, precondition := range []struct {
		name     string
		device   func(t *testing.T) func(args []string) *runOpts
		wantText []string
	}{
		{
			name: "no state directory",
			device: func(t *testing.T) func(args []string) *runOpts {
				return func(args []string) *runOpts {
					opts := registeredDeviceOpts(t, args, mustNotOpen)
					// The error connectorstate.ResolveDir returns on a host with
					// no state override and no platform user-state directory.
					opts.shareStateDirErr = fmt.Errorf("%w: set %s", connectorstate.ErrNoDefaultStateDir, connectorstate.EnvStateDirPrimary)
					return opts
				}
			},
			wantText: []string{shareNeedsDevice + "no default qurl sharing state directory: set QURL_CONNECTOR_STATE_DIR"},
		},
		{
			// The namespace belongs to an external supervisor and the command
			// runs with the default, native supervision.
			name: "externally supervised namespace",
			device: func(t *testing.T) func(args []string) *runOpts {
				return func(args []string) *runOpts {
					opts := registeredDeviceOpts(t, args, mustNotOpen)
					if err := connectorstate.EstablishExternalRuntimeMode(context.Background(), opts.shareStateDir); err != nil {
						t.Fatal(err)
					}
					return opts
				}
			},
			wantText: []string{shareNeedsDevice + `runtime supervision is "external", not "native"; run this command with --supervision external`},
		},
	} {
		for _, mode := range shareModes() {
			t.Run(precondition.name+"/"+mode.name, func(t *testing.T) {
				srv := downloadServer(t)
				run := runShareMode(t, srv, srv.URL, mode, precondition.device(t))
				if run.result.code != exitcode.Config {
					t.Fatalf("exit = %d, want %d; stderr: %s", run.result.code, exitcode.Config, run.result.stderr.String())
				}
				for _, want := range precondition.wantText {
					if !strings.Contains(run.result.stderr.String(), want) {
						t.Errorf("stderr = %q, want it to say %q", run.result.stderr.String(), want)
					}
				}
				run.mustNotHaveActed(t)
				if got := len(srv.Requests()); got != 0 {
					t.Fatalf("the command sent %d requests before its namespace was usable, want none", got)
				}
			})
		}
	}
}

// TestPrivateShareIsDecidedByTheDeviceCredential plays the service's rule
// for a private CRID against two registered devices. The device the
// publisher allowed gets a link with its own credential. Any other device is
// answered the ambiguous 404, the same answer a device that does not own a
// public CRID receives. share and get cannot tell those cases apart, so both
// surface as the one not-found result, exit code 5, whose guidance names
// every cause and says what helps for a private resource and what a public
// one allows: one request, no retry, and nothing printed, opened, or saved.
func TestPrivateShareIsDecidedByTheDeviceCredential(t *testing.T) {
	allowed := bootstrapRegisteredState(t)
	other := otherRegisteredState(t)
	privateResource := func(t *testing.T) *apitest.Server {
		t.Helper()
		srv := downloadServer(t)
		// Queued twice so a second attempt would be answered the same way and
		// counted, rather than falling through to the mock's default link.
		srv.ScriptRepeat(http.MethodPost, shareRoute(srv), 2, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+allowed.DeviceAPIKey {
				apitest.HandlerNotFound404(t, "resource_not_found")(w, r)
				return
			}
			apitest.WriteEnvelope(t, w, http.StatusOK, map[string]any{
				"qurl": srv.URL + apitest.DownloadPath, "crid": srv.Key.CRID,
				"type": "qv2", "expires_in_seconds": 300,
			}, nil)
		})
		return srv
	}

	for _, mode := range shareModes() {
		t.Run("allowed device/"+mode.name, func(t *testing.T) {
			srv := privateResource(t)
			run := runShareMode(t, srv, srv.URL, mode, registeredDevice(t, allowed))
			run.mustHaveDelivered(t, mode)
			shares := shareRequests(srv)
			if len(shares) != 1 || shares[0].Header.Get("Authorization") != "Bearer "+allowed.DeviceAPIKey {
				t.Fatalf("share requests = %d, want one carrying the allowed device's credential", len(shares))
			}
		})

		t.Run("other device/"+mode.name, func(t *testing.T) {
			srv := privateResource(t)
			run := runShareMode(t, srv, srv.URL, mode, registeredDevice(t, other))
			if run.result.code != exitcode.NotFound {
				t.Fatalf("exit = %d, want %d; stderr: %s", run.result.code, exitcode.NotFound, run.result.stderr.String())
			}
			stderr := run.result.stderr.String()
			for _, want := range []string{
				"Not Found (HTTP 404)",
				"the CRID may be mistyped, the resource may have been removed, or this device may not be allowed to open it",
				"If the resource is private, send the publisher this device's public key from `qurl whoami -o json` so they can allow it",
				"A public resource opens only on its owner's devices in this release",
			} {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr = %q, want the not-found guidance %q", stderr, want)
				}
			}
			// The CLI cannot tell why the service said not-found, so it must
			// not read like the answer an owner gets for a deleted resource,
			// and it must not give the advice meant for an expired link.
			for _, wrong := range []string{"deleted", "Ask whoever shared it"} {
				if strings.Contains(strings.ToLower(stderr), strings.ToLower(wrong)) {
					t.Errorf("stderr = %q, must not say %q", stderr, wrong)
				}
			}
			run.mustNotHaveActed(t)
			shares := shareRequests(srv)
			if len(shares) != 1 || shares[0].Header.Get("Authorization") != "Bearer "+other.DeviceAPIKey {
				t.Fatalf("share requests = %d, want exactly one carrying this device's credential", len(shares))
			}
		})
	}
}

// TestShareOnANewDeviceEnrollsBeforeSharing covers a machine with neither a
// device identity nor an account key. share and get create the device
// identity the way every other command does, with no account, then send the
// share request with its credential.
func TestShareOnANewDeviceEnrollsBeforeSharing(t *testing.T) {
	for _, mode := range shareModes() {
		t.Run(mode.name, func(t *testing.T) {
			srv := downloadServer(t)
			state := bootstrapRegisteredState(t)
			enrolled := false
			newDevice := func(args []string) *runOpts {
				return registeredDeviceOpts(t, args, func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
					request := qurl.AgentEnrollmentCredentialRequest{AgentID: state.AgentID, PublicKeyB64: state.PublicKeyB64}
					got, err := cfg.EnrollmentCredentialProvider(ctx, request)
					want, wantErr := qurl.AnonymousEnrollmentCredential(ctx, request)
					if err != nil || wantErr != nil || got != want {
						t.Errorf("enrollment credential error = %v (reference error %v), want the account-free credential", err, wantErr)
					}
					enrolled = true
					return &bootstrapNativeRuntime{store: &bootstrapAgentStateStore{state: state}}, nil
				})
			}

			run := runShareMode(t, srv, srv.URL, mode, newDevice)
			run.mustHaveDelivered(t, mode)
			if !enrolled || !strings.Contains(run.result.stderr.String(), msgAnonymousDevice) {
				t.Fatalf("enrolled = %t, stderr = %q; want the new device identity announced", enrolled, run.result.stderr.String())
			}
			shares := shareRequests(srv)
			if len(shares) != 1 || shares[0].Header.Get("Authorization") != "Bearer "+state.DeviceAPIKey {
				t.Fatalf("share requests = %d, want one carrying the new device's credential", len(shares))
			}
			for _, request := range srv.Requests() {
				if request.Path == "/v1/api-keys" {
					t.Fatal("sharing on a new device asked for an account enrollment token")
				}
			}
		})
	}
}
