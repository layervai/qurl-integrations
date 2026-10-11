package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	connectoragentstate "github.com/layervai/qurl-connector/pkg/agentstate"
	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	"github.com/layervai/qurl-go/qurl"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/clitest"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/consume"
	"github.com/layervai/qurl-integrations/apps/cli/internal/cridux"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

// Tests for where `qurl get` takes its link from: the link request, which
// asks the service for a link for a CRID, or the share request this device
// makes with its identity. The link request has two forms: with the CRID
// alone, and as this device. Both forms, the read of the device key, and the
// check for whether the request is offered are behind seams, so no test here
// sends a request. The tests in this file that use the real SDK call stop
// before the SDK sends anything; get_crid_link_offer_test.go has the ones
// that send to the SDK's test server.
//
// The rules that are pinned here:
//
//   - Where the link request is not offered, get is unchanged for every
//     machine: the same output, the same exit code, the same requests. No
//     device key is read.
//   - A device with an identity, where the request is offered and no share
//     option is set, makes the link request first. A link ends the run. Any
//     other answer leads to the share request, and the result is the one get
//     gave when the share request came first. For that, one case makes the
//     link request once more: get_link_request_first_test.go has it.
//   - That device asks as this device when its key can be read, and with the
//     CRID alone when it cannot.
//   - With a share option, get keeps the earlier order: the share request
//     first, and the link request with the CRID alone only after "not
//     found".
//   - A machine with no identity, where the request is offered, asks with the
//     CRID alone. Whatever the answer is, it is final, and no identity is
//     created.
//   - A download that needs a fresh link asks as the origin of the link in
//     use says: see the third table in get_crid_link.go.

// cridLinkNotOffered answers the way the deployment every release ships
// today does: it names no place to send the request, so the request is not
// offered. It is the harness default.
func cridLinkNotOffered() (bool, error) { return false, nil }

// cridLinkIsOffered answers for a deployment that offers the request. The
// harness uses it for a test that injects an answer to the request.
func cridLinkIsOffered() (bool, error) { return true, nil }

// mustNotCheckTheLinkOffer fails the test if the command asks whether the
// request is offered at all. A command that got its answer from the share
// request has no reason to.
func mustNotCheckTheLinkOffer(t *testing.T) func() (bool, error) {
	t.Helper()
	return func() (bool, error) {
		t.Error("the command asked whether a link can be requested with only the CRID")
		return false, errors.New("unexpected check for the request with only the CRID")
	}
}

// errCRIDVersionTheSDKCannotCheck and errCRIDTheSDKCallsInvalid are the two
// answers the SDK gives for a CRID it will not ask for, before it sends
// anything. The second cannot reach get in practice: the CLI's local check
// refuses such a CRID first.
var (
	errCRIDVersionTheSDKCannotCheck = fmt.Errorf("%w: %w: a link cannot be verified against CRID version 0x05", qurl.ErrInvalidResourceRequest, qurl.ErrUnsupportedCRIDVersion)
	errCRIDTheSDKCallsInvalid       = fmt.Errorf("%w: a CRID link request requires a valid CRID", qurl.ErrInvalidResourceRequest)
)

// linkRequests is the injected answer to the link request, in both of its
// forms. It records every CRID that was asked for, and for each request
// whether it was made as this device.
//
// It stands where the SDK stands, so the request as this device does with the
// device key what the SDK does: it asks for the key only after the answer
// "not found" to a first request, and only once.
type linkRequests struct {
	// link and err are the answer. For a resource that is not private it is
	// the answer to the first request. When that answer is "not found", a
	// request as this device with a key gets it a second time, as a device
	// that may not open the resource does.
	link *qurl.CRIDLink
	err  error
	// private says that the resource is private. A request with only the
	// CRID is then answered "not found", and so is the first request of a
	// request as this device. link and err are the answer to the request
	// under the device key.
	private bool
	asked   []string
	// asDevice has one entry for each entry of asked.
	asDevice []bool
	// keys holds a copy of the key each request under the device key was
	// sent with. It has no entry for a request that needed no key.
	keys [][]byte
}

// errSDKNotFound is the SDK's answer "not found", with the service's code.
var errSDKNotFound = sdkRefusal(qurl.ErrCRIDLinkNotFound, "52602")

// answer is the request with only the CRID.
func (r *linkRequests) answer(_ context.Context, resourceCRID string) (*qurl.CRIDLink, error) {
	r.asked = append(r.asked, resourceCRID)
	r.asDevice = append(r.asDevice, false)
	if r.private {
		return nil, errSDKNotFound
	}
	return r.result()
}

// answerAsDevice is the request as this device.
func (r *linkRequests) answerAsDevice(ctx context.Context, deviceKey qurl.DeviceKeySource, resourceCRID string) (*qurl.CRIDLink, error) {
	r.asked = append(r.asked, resourceCRID)
	r.asDevice = append(r.asDevice, true)
	first := r.err
	if r.private {
		first = errSDKNotFound
	}
	if !errors.Is(first, qurl.ErrCRIDLinkNotFound) {
		// The first request settles it. The SDK does not ask for the key.
		return r.result()
	}
	return asDeviceAfterNotFound(ctx, deviceKey, first, func(key []byte) (*qurl.CRIDLink, error) {
		r.keys = append(r.keys, bytes.Clone(key))
		return r.result()
	})
}

// asDeviceAfterNotFound is the part of the SDK's request as a device that
// follows the answer "not found" to the first request. An injected answer to
// that request calls it, so that it treats the device key as the SDK does.
//
// It asks deviceKey for the key, once, with the context of the request. With
// a key it calls second, the request under the device key, and then wipes
// the key, because the key is the SDK's. Without one it returns the SDK's
// error for that, which keeps notFound, the answer to the first request.
func asDeviceAfterNotFound(
	ctx context.Context, deviceKey qurl.DeviceKeySource, notFound error, second func(key []byte) (*qurl.CRIDLink, error),
) (*qurl.CRIDLink, error) {
	key, err := deviceKey(ctx)
	if err != nil {
		return nil, &qurl.DeviceKeySourceError{Err: err, FirstAnswer: notFound}
	}
	defer clear(key)
	return second(key)
}

func (r *linkRequests) result() (*qurl.CRIDLink, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.link == nil {
		return nil, nil //nolint:nilnil // One test pins how get treats an answer with neither a link nor an error.
	}
	issued := *r.link
	return &issued, nil
}

// askedAsDevice counts the requests that were made as this device.
func (r *linkRequests) askedAsDevice() int {
	n := 0
	for _, device := range r.asDevice {
		if device {
			n++
		}
	}
	return n
}

// linkRequestGuard is the harness default for both forms of the link
// request: it fails the owning test, by the name of the request, and answers
// with an error. So no hermetic test can send a link request it did not
// supply an answer for.
type linkRequestGuard struct {
	// report fails the owning test.
	report func(format string, args ...any)
}

func (g *linkRequestGuard) withTheCRIDAlone(context.Context, string) (*qurl.CRIDLink, error) {
	g.report("the command asked for a link with only the CRID")
	return nil, errors.New("unexpected request for a link with only the CRID")
}

func (g *linkRequestGuard) asTheDevice(context.Context, qurl.DeviceKeySource, string) (*qurl.CRIDLink, error) {
	g.report("the command asked for a link as this device")
	return nil, errors.New("unexpected request for a link as this device")
}

// mustNotAskWithTheCRIDAlone fails the test if the request with only the
// CRID is made at all.
func mustNotAskWithTheCRIDAlone(t *testing.T) func(context.Context, string) (*qurl.CRIDLink, error) {
	t.Helper()
	return (&linkRequestGuard{report: t.Errorf}).withTheCRIDAlone
}

// mustNotAskAsTheDevice fails the test if the request as this device is made
// at all.
func mustNotAskAsTheDevice(t *testing.T) func(context.Context, qurl.DeviceKeySource, string) (*qurl.CRIDLink, error) {
	t.Helper()
	return (&linkRequestGuard{report: t.Errorf}).asTheDevice
}

// mustNotReadTheDeviceKey fails the test if the command reads the device key
// at all. A command that makes no request as this device has no reason to.
func mustNotReadTheDeviceKey(t *testing.T) func(context.Context) ([]byte, connectorstate.NoDeviceKey) {
	t.Helper()
	return func(context.Context) ([]byte, connectorstate.NoDeviceKey) {
		t.Error("the command read the device key")
		return nil, connectorstate.NoDeviceKeyUnreadable
	}
}

// deviceKeyReads is an injected read of the device key. It makes the key
// readable or not readable on every platform, and it remembers what it
// handed out.
//
// The real SDK calls a read on a goroutine of its own, and it does not wait
// for the read when the context of the request ends. So the record has a
// lock, and a test looks at it through keysGiven and limitsSeen.
type deviceKeyReads struct {
	// key is the key a read returns; nil means the key cannot be read, and
	// why is then the reason.
	key []byte
	why connectorstate.NoDeviceKey

	mu sync.Mutex
	// given holds the slices the reads returned. The command owns them and
	// wipes them, so each read returns a copy of its own.
	given [][]byte
	// bounded has one entry for each read: whether the context of the read
	// had a time limit.
	bounded []bool
}

func (r *deviceKeyReads) read(ctx context.Context) ([]byte, connectorstate.NoDeviceKey) {
	_, bounded := ctx.Deadline()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bounded = append(r.bounded, bounded)
	if r.key == nil {
		r.given = append(r.given, nil)
		return nil, r.why
	}
	key := bytes.Clone(r.key)
	r.given = append(r.given, key)
	return key, ""
}

// keysGiven returns the slices the reads returned so far, one for each read.
// The slices are the ones the command was given, so a test sees whether the
// command wiped them.
func (r *deviceKeyReads) keysGiven() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.given)
}

// limitsSeen returns, for each read so far, whether its context had a time
// limit.
func (r *deviceKeyReads) limitsSeen() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.bounded)
}

// deviceKeyOf returns reads that give the device key in state.
func deviceKeyOf(t *testing.T, state *qurl.AgentState) *deviceKeyReads {
	t.Helper()
	key, err := base64.StdEncoding.DecodeString(state.PrivateKeyB64)
	if err != nil || len(key) != 32 {
		t.Fatalf("the fixture's device key is %d bytes, error %v; want 32 bytes", len(key), err)
	}
	return &deviceKeyReads{key: key}
}

// noDeviceKey returns reads that give no key, for the reason why.
func noDeviceKey(why connectorstate.NoDeviceKey) *deviceKeyReads {
	return &deviceKeyReads{why: why}
}

// deviceKeyReadable says whether the production read gives the key of the
// enrolledDevice fixture on this platform. It does on Linux and macOS, and
// nowhere else. TestEnrolledDeviceHoldsStateTheCLIAccepts pins both answers,
// so a test may use this to say which form of the link request a device with
// the production read makes.
var deviceKeyReadable = runtime.GOOS == "linux" || runtime.GOOS == "darwin"

// issuedLink is what the SDK returns for a CRID: the link, and the
// display-only facts the service reports beside it. The publisher and the
// creation date are the mock API's own, so the two paths can be compared byte
// for byte.
func issuedLink(link string) *qurl.CRIDLink {
	createdAt := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	return &qurl.CRIDLink{
		Link:              link,
		QURLID:            "q_issued0001",
		ExpiresAt:         fixedNow.Add(5 * time.Minute),
		ResourceCreatedAt: &createdAt,
		Publisher:         qurl.Publisher{Name: apitest.DefaultPublisherName},
	}
}

// sdkRefusal builds a refusal the way the SDK does: the typed sentinel and
// the deny that carries the service's code.
func sdkRefusal(sentinel error, code string) error {
	return fmt.Errorf("%w: %w", sentinel, &qurl.ServerDenyError{ErrCode: code})
}

// getModes are get's three actions: browser, file, and stdout.
func getModes() []shareMode {
	var modes []shareMode
	for _, mode := range shareModes() {
		if strings.HasPrefix(mode.name, "get ") {
			modes = append(modes, mode)
		}
	}
	return modes
}

// saveDeviceState stores state in stateDir through the CLI's own state store,
// which is how an enrollment writes it. After that, stateDir reads as holding
// a device identity.
//
// get reads the file back: where the link request is offered, it reads the
// device key from this state to ask as this device. The device runtime is
// still a fake, so the share path does not read it. The file must come from
// the store for a second reason. Every command checks the state directory
// before it uses it, and on Windows that check refuses a state file whose
// access control list (ACL) is not the protected, owner-only one. A file
// written with os.WriteFile gets the default ACL there, so the command would
// stop before the code under test runs.
func saveDeviceState(t *testing.T, stateDir string, state *qurl.AgentState) {
	t.Helper()
	store, err := connectorstate.Open(stateDir)
	if err != nil {
		t.Fatalf("open the device state store: %v", err)
	}
	sdkStore, err := store.Handoff()
	if err == nil {
		err = sdkStore.SaveAgentState(context.Background(), state)
	}
	if closeErr := store.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("save the device state: %v", err)
	}
}

// enrolledDevice is a machine that already holds device state. Opening the
// device hands that state off and enrolls nothing.
//
// The read of the device key is the production one, and it reads the state
// this fixture wrote. So on Linux and macOS this device asks as itself, and
// on a platform where the read is not supported it asks with the CRID alone
// (deviceKeyReadable). A test that must be the same on every platform
// injects the read: withDeviceKey.
func enrolledDevice(t *testing.T, state *qurl.AgentState) func(args []string) *runOpts {
	t.Helper()
	return func(args []string) *runOpts {
		opts := registeredDevice(t, state)(args)
		saveDeviceState(t, opts.shareStateDir, state)
		return opts
	}
}

// TestEnrolledDeviceHoldsStateTheCLIAccepts pins the fixture that every test
// of a device with an identity is built on. The state directory must pass the
// check each command makes before it uses the directory, and it must hold the
// device's state as the store wrote it.
//
// If this test fails, fix enrolledDevice first: the other tests of a device
// with an identity then fail for the same reason, before the code they test
// runs.
func TestEnrolledDeviceHoldsStateTheCLIAccepts(t *testing.T) {
	state := bootstrapRegisteredState(t)
	stateDir := enrolledDevice(t, state)(nil).shareStateDir

	if !connectorstate.EnvelopePresent(stateDir) {
		t.Fatal("the fixture's state directory does not read as holding a device identity")
	}
	// The share and get commands open the local share registry first. That
	// open is the check of the directory and of the state file in it.
	if _, err := connectorstate.OpenLocalShareRegistry(stateDir); err != nil {
		t.Fatalf("the CLI refuses the fixture's state directory: %v", err)
	}

	store, err := connectorstate.Open(stateDir)
	if err != nil {
		t.Fatalf("the CLI cannot open the fixture's device state: %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("close the device state store: %v", err)
		}
	}()
	sdkStore, err := store.Handoff()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := sdkStore.LoadAgentState(context.Background())
	if err != nil || loaded == nil {
		t.Fatalf("the CLI cannot load the fixture's device state: state present %t, error %v", loaded != nil, err)
	}
	if loaded.AgentID != state.AgentID || loaded.DeviceAPIKeyID != state.DeviceAPIKeyID {
		t.Errorf("the state directory holds device %q with key id %q, want %q with %q",
			loaded.AgentID, loaded.DeviceAPIKeyID, state.AgentID, state.DeviceAPIKeyID)
	}

	// The production read of the device key gives the key of this state on
	// Linux and macOS, and no key with the word "platform" anywhere else.
	// Tests of a device that asks as itself are built on this answer.
	key, why := connectorstate.ReadDeviceStaticPrivateKey(context.Background(), stateDir, connectorstate.RuntimeSupervisionNative)
	if deviceKeyReadable {
		if want := deviceKeyOf(t, state).key; why != "" || !bytes.Equal(key, want) {
			t.Errorf("the read of the device key gave %d bytes and %q, want the %d bytes of the fixture's key", len(key), why, len(want))
		}
	} else if key != nil || why != connectorstate.NoDeviceKeyPlatform {
		t.Errorf("the read of the device key gave %d bytes and %q, want no key and %q on this platform", len(key), why, connectorstate.NoDeviceKeyPlatform)
	}
}

// machineWithNoIdentity is a machine with no device state and no account key.
// Opening the device runtime is how such a machine gets an identity: it
// creates the state and registers the device. So opening it fails the test,
// and so does a read of the device key. stateDir does not exist, and the
// caller checks it still does not afterwards.
func machineWithNoIdentity(t *testing.T, stateDir string) func(args []string) *runOpts {
	t.Helper()
	return func(args []string) *runOpts {
		opts := registeredDeviceOpts(t, args, func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
			t.Error("get opened the device runtime on a machine with no device identity: that is where an identity is created and registered")
			if _, err := cfg.EnrollmentCredentialProvider(ctx, qurl.AgentEnrollmentCredentialRequest{AgentID: "agent-must-not-exist"}); err == nil {
				t.Error("get produced an enrollment credential for a machine that must not enroll")
			}
			return nil, errors.New("unexpected device runtime open")
		})
		opts.shareStateDir = stateDir
		// A machine with no identity has no device key to read.
		opts.readDeviceKey = mustNotReadTheDeviceKey(t)
		return opts
	}
}

// machineThatEnrolls is a machine with no device state and no account key
// whose device runtime may be opened. Opening it is the enrollment: it must
// ask for the account-free credential, and *enrolled records that it
// happened. It is the machine of the compatibility cases, where get does what
// it did before the second request existed.
func machineThatEnrolls(t *testing.T, state *qurl.AgentState, enrolled *bool) func(args []string) *runOpts {
	t.Helper()
	return func(args []string) *runOpts {
		return registeredDeviceOpts(t, args, func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
			request := qurl.AgentEnrollmentCredentialRequest{AgentID: state.AgentID, PublicKeyB64: state.PublicKeyB64}
			got, err := cfg.EnrollmentCredentialProvider(ctx, request)
			want, wantErr := qurl.AnonymousEnrollmentCredential(ctx, request)
			if err != nil || wantErr != nil || got != want {
				t.Errorf("enrollment credential error = %v (reference error %v), want the account-free credential", err, wantErr)
			}
			*enrolled = true
			return &bootstrapNativeRuntime{store: &bootstrapAgentStateStore{state: state}}, nil
		})
	}
}

// withLinkRequests adds one answer for the link request in both of its
// forms: the request with only the CRID, and the request as this device. The
// harness then answers that the request is offered.
//
// The answer is the result of the whole request, in either form. The request
// as this device does not ask for the device key here, so the key is not
// read. That keeps a test the same on every platform: whether a device with
// the production read can read its key depends on deviceKeyReadable. A test
// about the key uses withLinkRequestsOf, which asks for it as the SDK does.
func withLinkRequests(configure func(args []string) *runOpts, answer func(context.Context, string) (*qurl.CRIDLink, error)) func(args []string) *runOpts {
	return func(args []string) *runOpts {
		opts := configure(args)
		opts.requestCRIDLink = answer
		opts.requestCRIDLinkAsDevice = func(ctx context.Context, _ qurl.DeviceKeySource, resourceCRID string) (*qurl.CRIDLink, error) {
			return answer(ctx, resourceCRID)
		}
		return opts
	}
}

// withLinkRequestsOf adds requests as the answer to both forms of the link
// request. requests then knows which form each request had, and the key a
// request under the device key was sent with. Its request as this device
// asks for the device key as the SDK does: only after "not found".
func withLinkRequestsOf(configure func(args []string) *runOpts, requests *linkRequests) func(args []string) *runOpts {
	return func(args []string) *runOpts {
		opts := configure(args)
		opts.requestCRIDLink, opts.requestCRIDLinkAsDevice = requests.answer, requests.answerAsDevice
		return opts
	}
}

// withDeviceKey sets the read of the device key.
func withDeviceKey(configure func(args []string) *runOpts, read func(context.Context) ([]byte, connectorstate.NoDeviceKey)) func(args []string) *runOpts {
	return func(args []string) *runOpts {
		opts := configure(args)
		opts.readDeviceKey = read
		return opts
	}
}

// withArgs adds flags to the command line of an invocation.
func withArgs(configure func(args []string) *runOpts, extra ...string) func(args []string) *runOpts {
	return func(args []string) *runOpts { return configure(append(args, extra...)) }
}

// withLinkOffer sets the answer to "can this machine ask with only the CRID
// at all".
func withLinkOffer(configure func(args []string) *runOpts, offered func() (bool, error)) func(args []string) *runOpts {
	return func(args []string) *runOpts {
		opts := configure(args)
		opts.cridLinkOffered = offered
		return opts
	}
}

// linkOffer is the injected answer to "can this machine ask with only the
// CRID at all". It counts how often it was asked.
type linkOffer struct {
	offered bool
	err     error
	checks  int
}

func (o *linkOffer) answer() (bool, error) {
	o.checks++
	return o.offered, o.err
}

// apiRequests returns the requests srv saw on the qURL API, as "METHOD path"
// lines. The download route is the link host, not the API.
func apiRequests(srv *apitest.Server) []string {
	var seen []string
	for _, request := range srv.Requests() {
		if request.Path != apitest.DownloadPath && request.Path != apitest.PortalPath {
			seen = append(seen, request.Method+" "+request.Path)
		}
	}
	return seen
}

// terminalStyle matches the color and weight codes the printer adds on a
// terminal.
var terminalStyle = regexp.MustCompile("\x1b\\[[0-9;]*m")

// withoutStyle removes those codes, leaving the words.
func withoutStyle(s string) string { return terminalStyle.ReplaceAllString(s, "") }

// shareNotFoundTwice makes the share route answer "not found", queued twice
// so a second attempt is answered the same way and counted.
func shareNotFoundTwice(t *testing.T, srv *apitest.Server) {
	t.Helper()
	srv.ScriptRepeat(http.MethodPost, shareRoute(srv), 2, apitest.HandlerNotFound404(t, "resource_not_found"))
}

// goldenBytes reads a golden file as it is and never rewrites it. Most files
// read this way pin what the share path printed before the second request
// existed, so an update run must not be able to change them from here. The
// files of the new path are written by TestGetByCRIDAloneGoldens only.
func goldenBytes(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(filepath.Join("testdata", "golden", name)))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return string(data)
}

// answerRow is one row of the answer table: what the SDK said, and what get
// must print and exit with.
type answerRow struct {
	name string
	// err is the SDK's answer. A nil err is the answer that has neither a
	// link nor an error.
	err      error
	wantCode int
	// golden names the stderr golden of the row for a device with an
	// identity; goldenNoDevice the one for a machine with none, when it
	// differs.
	golden         string
	goldenNoDevice string
}

// refusalRows is every answer that is not a link and not "the request was not
// made".
func refusalRows() []answerRow {
	return []answerRow{
		{
			name: "not found", err: sdkRefusal(qurl.ErrCRIDLinkNotFound, "52602"), wantCode: exitcode.NotFound,
			golden: "error_get_crid_notfound", goldenNoDevice: "error_get_crid_notfound_no_device",
		},
		{name: "unavailable", err: sdkRefusal(qurl.ErrCRIDLinkUnavailable, "52601"), wantCode: exitcode.Unavailable, golden: "error_get_crid_unavailable"},
		{name: "no answer", err: &qurl.RelayError{Status: 502, Msg: "relay POST https://endpoint.example.test/x -> 502"}, wantCode: exitcode.Unavailable, golden: "error_get_crid_no_answer"},
		{
			name:     "timed out",
			err:      fmt.Errorf("qurl: CRID link request did not complete: %w: %w", context.DeadlineExceeded, &qurl.RelayError{Msg: "relay POST https://endpoint.example.test/x failed"}),
			wantCode: exitcode.Unavailable, golden: "error_get_crid_no_answer",
		},
		{name: "rate limited", err: sdkRefusal(qurl.ErrCRIDLinkRateLimited, "52603"), wantCode: exitcode.RateLimited, golden: "error_get_crid_ratelimited"},
		{name: "busy", err: qurl.ErrServerOverloaded, wantCode: exitcode.Unavailable, golden: "error_get_crid_busy"},
		{name: "publisher offline", err: sdkRefusal(qurl.ErrCRIDResourceOffline, "52604"), wantCode: exitcode.Unavailable, golden: "error_get_crid_offline"},
		{name: "resource closed", err: sdkRefusal(qurl.ErrCRIDResourceClosed, "52605"), wantCode: exitcode.NotFound, golden: "error_get_crid_closed"},
		{name: "request rejected", err: sdkRefusal(qurl.ErrInvalidCRIDLinkRequest, "52606"), wantCode: exitcode.InvalidInput, golden: "error_get_crid_request_rejected"},
		// A code outside the six this request defines reads as "try again
		// later". 51002 is what a service that does not serve the request
		// answers.
		{name: "general code", err: &qurl.ServerDenyError{ErrCode: "51002"}, wantCode: exitcode.Unavailable, golden: "error_get_crid_unavailable"},
		{name: "another code outside the set", err: &qurl.ServerDenyError{ErrCode: "52005"}, wantCode: exitcode.Unavailable, golden: "error_get_crid_unavailable"},
		{name: "rejected link", err: &qurl.CRIDLinkRejectedError{Class: qurl.CRIDLinkRejectCRIDMismatch}, wantCode: exitcode.VerificationFailed, golden: "error_get_crid_refused"},
		{
			name:     "link from an unknown signer",
			err:      errors.Join(&qurl.CRIDLinkRejectedError{Class: qurl.CRIDLinkRejectIssuerSignature}, qurl.ErrUnknownKID),
			wantCode: exitcode.VerificationFailed, golden: "error_get_crid_refused",
		},
		{name: "not an answer", err: fmt.Errorf("%w: the reply carries a success code", qurl.ErrCRIDLinkProtocol), wantCode: exitcode.VerificationFailed, golden: "error_get_crid_refused"},
		{name: "malformed reply", err: fmt.Errorf("%w: unexpected reply type 9", qurl.ErrMalformedReply), wantCode: exitcode.VerificationFailed, golden: "error_get_crid_refused"},
		{name: "reply that does not prove its source", err: errors.New("decrypt reply: message authentication failed"), wantCode: exitcode.VerificationFailed, golden: "error_get_crid_refused"},
		{name: "neither a link nor an error", err: nil, wantCode: exitcode.VerificationFailed, golden: "error_get_crid_refused"},
		// A settings file that cannot be used at all is not a row here: its
		// message names the file, so no error a test builds by hand is what
		// the CLI prints. TestGetReportsASettingsFileThatCannotBeUsed gives
		// get real files of that kind, for the check and for the request.
		//
		// The check for whether the request is offered reports these two
		// before any request. A request that still ends in one of them has
		// found the settings changed, and reads the same way.
		{
			name:     "endpoint that cannot be used",
			err:      fmt.Errorf("%w: relay URL: %w", qurl.ErrCRIDLinkMisconfigured, qurl.ErrRelayURL),
			wantCode: exitcode.Config, golden: "error_get_crid_setup",
		},
		{
			name:     "no endpoint after all",
			err:      fmt.Errorf("%w: the configuration names none", qurl.ErrCRIDLinkNotConfigured),
			wantCode: exitcode.Config, golden: "error_get_crid_not_set_up",
		},
	}
}

// goldenFor returns the stderr golden of row for the given starting state.
func (r *answerRow) goldenFor(device bool) string {
	if !device && r.goldenNoDevice != "" {
		return r.goldenNoDevice
	}
	return r.golden
}

// TestGetByCRIDAloneGoldens pins the rendered bytes of the new path.
//
// A link given for the CRID alone carries the same publisher and creation
// date as the mock API's share answer, so the file actions are compared with
// the share path's own golden files: the reader is told the same thing in the
// same words. The terminal document differs in one row, the expiry, because
// this answer has an expiry time and no lifetime in seconds.
//
// It is the first test in this file on purpose. Go runs tests in source
// order, and the tests below read the files this one writes, so an update
// run settles in one pass.
func TestGetByCRIDAloneGoldens(t *testing.T) {
	goldenDir, err := filepath.Abs(filepath.Join("testdata", "golden"))
	if err != nil {
		t.Fatalf("locate golden dir: %v", err)
	}
	read := func(t *testing.T, name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Clean(filepath.Join(goldenDir, name)))
		if err != nil {
			t.Fatalf("read golden %s: %v", name, err)
		}
		return string(data)
	}
	// run is one get on a machine with no identity, in a fresh working
	// directory so a relative --file path is stable.
	run := func(t *testing.T, tty bool, answer *linkRequests, args ...string) *runResult {
		t.Helper()
		t.Chdir(t.TempDir())
		srv := apitest.NewServerWithKey(t, apitest.FixedResourceKey(t))
		if answer.link != nil && answer.link.Link == "" {
			answer.link.Link = srv.URL + apitest.DownloadPath
		}
		return runCLI(t, &runOpts{
			args:            append([]string{"--endpoint", srv.URL, "get", srv.Key.CRID}, args...),
			env:             map[string]string{},
			tty:             tty,
			requestCRIDLink: answer.answer,
		})
	}

	t.Run("browser", func(t *testing.T) {
		result := run(t, true, &linkRequests{link: issuedLink("https://qurl.link/#qv2t1.1.1.1.AQ.AQ.AQ")})
		if result.code != exitcode.Success {
			t.Fatalf("exit = %d, stderr: %s", result.code, result.stderr.String())
		}
		clitest.GoldenAt(t, filepath.Join(goldenDir, "get_crid_browser.tty.golden"), result.stdout.Bytes())
		if got, want := result.stderr.String(), read(t, "get_browser.tty.stderr.golden"); got != want {
			t.Errorf("stderr = %q, want the share path's %q", got, want)
		}
		// Everything but the expiry row is the share path's document.
		shareDoc := strings.Split(read(t, "get_browser.tty.golden"), "\n")
		newDoc := strings.Split(result.stdout.String(), "\n")
		if len(shareDoc) != len(newDoc) || strings.Join(shareDoc[:4], "\n") != strings.Join(newDoc[:4], "\n") {
			t.Errorf("document = %q, want the share path's link, publisher and creation rows %q", newDoc, shareDoc)
		}
	})

	for _, variant := range []string{"plain", "tty"} {
		t.Run("file/"+variant, func(t *testing.T) {
			result := run(t, variant == "tty", &linkRequests{link: issuedLink("")}, "--file", "out.bin")
			if result.code != exitcode.Success || result.stdout.Len() != 0 {
				t.Fatalf("exit = %d, stdout = %q, stderr: %s", result.code, result.stdout.String(), result.stderr.String())
			}
			if got, want := result.stderr.String(), read(t, "get_file."+variant+".stderr.golden"); got != want {
				t.Errorf("stderr = %q, want the share path's %q", got, want)
			}
		})
	}

	t.Run("file/json", func(t *testing.T) {
		result := run(t, false, &linkRequests{link: issuedLink("")}, "--file", "out.bin", "-o", "json")
		if result.code != exitcode.Success || result.stderr.Len() != 0 {
			t.Fatalf("exit = %d, stderr: %s", result.code, result.stderr.String())
		}
		if got, want := result.stdout.String(), read(t, "get_file.json.golden"); got != want {
			t.Errorf("document = %q, want the share path's %q: the same fields and nothing more", got, want)
		}
	})

	t.Run("session duration", func(t *testing.T) {
		result := run(t, false, &linkRequests{link: issuedLink("")}, "--file", "out.bin", "--session-duration", "5m")
		if result.code != exitcode.Success {
			t.Fatalf("exit = %d, stderr: %s", result.code, result.stderr.String())
		}
		clitest.GoldenAt(t, filepath.Join(goldenDir, "get_crid_session_duration.plain.stderr.golden"), result.stderr.Bytes())
	})

	// A machine with no identity is told that this client cannot ask for a
	// link for the CRID, in the words the link check has for the same fact.
	t.Run("CRID this client cannot ask for", func(t *testing.T) {
		result := run(t, false, &linkRequests{err: errCRIDVersionTheSDKCannotCheck}, "--file", "out.bin")
		if result.code != exitcode.Config || result.stdout.Len() != 0 {
			t.Fatalf("exit = %d, want %d; stdout = %q, stderr: %s", result.code, exitcode.Config, result.stdout.String(), result.stderr.String())
		}
		clitest.GoldenAt(t, filepath.Join(goldenDir, "error_get_crid_version.plain.stderr.golden"), result.stderr.Bytes())
	})

	// A settings file that cannot be used at all, one golden for each kind of
	// file. The file is a real one and the production code reads it, so the
	// golden holds what the CLI prints. That message names the file by the
	// path the user gave. The directory the test made is replaced by a fixed
	// word before the golden is written or compared, so no golden holds a
	// path of the machine the test ran on.
	for _, file := range unusableSettingsFiles() {
		t.Run(file.name, func(t *testing.T) {
			if file.systemWords && runtime.GOOS == "windows" {
				t.Skip("the message ends with the operating system's own words for the failure, and the golden holds those of a Unix system")
			}
			dir := t.TempDir()
			opener := file.opener(t, dir)
			t.Chdir(t.TempDir())
			srv := apitest.NewServerWithKey(t, apitest.FixedResourceKey(t))
			result := runCLI(t, &runOpts{
				args:            []string{"--endpoint", srv.URL, "get", srv.Key.CRID, "--file", "out.bin"},
				env:             map[string]string{},
				cridLinkOffered: opener.CRIDLinkOffered,
			})
			if result.code != exitcode.Config || result.stdout.Len() != 0 {
				t.Fatalf("exit = %d, want %d; stdout = %q, stderr: %s", result.code, exitcode.Config, result.stdout.String(), result.stderr.String())
			}
			message := withoutSettingsDir(t, result.stderr.String(), dir)
			if file.parserWords {
				// This case has no golden: its message ends with the
				// words of Go's JSON parser. See unusableSettingsFile.
				file.mustBeTheMessage(t, message)
				return
			}
			clitest.GoldenAt(t, filepath.Join(goldenDir, file.golden+".plain.stderr.golden"), []byte(message))
		})
	}

	// One golden per answer. The plain variant runs the file action; the two
	// not-found answers also have a terminal variant, which runs the browser
	// action.
	written := map[string]bool{}
	for _, row := range refusalRows() {
		for _, device := range []bool{true, false} {
			name := row.goldenFor(device)
			variants := []string{"plain"}
			if row.goldenNoDevice != "" {
				variants = append(variants, "tty")
			}
			for _, variant := range variants {
				file := name + "." + variant + ".stderr.golden"
				if written[file] {
					continue
				}
				written[file] = true
				t.Run(file, func(t *testing.T) {
					t.Chdir(t.TempDir())
					srv := apitest.NewServerWithKey(t, apitest.FixedResourceKey(t))
					env := map[string]string{}
					if device {
						// The harness's account-key client is a device path with
						// no other stderr output.
						env["QURL_API_KEY"] = testAPIKey
						srv.Script(http.MethodPost, shareRoute(srv), apitest.HandlerNotFound404(t, "resource_not_found"))
					}
					args := []string{"--endpoint", srv.URL, "get", srv.Key.CRID}
					if variant == "plain" {
						args = append(args, "--file", "out.bin")
					}
					// A machine with no identity asks with the CRID alone. The
					// device here asks as this device. It holds no device
					// state, so it has no key: after "not found" the answer to
					// its first request stands.
					answer := &linkRequests{err: row.err}
					result := runCLI(t, &runOpts{
						args: args, env: env, tty: variant == "tty",
						requestCRIDLink: answer.answer, requestCRIDLinkAsDevice: answer.answerAsDevice,
					})
					if len(answer.asked) != 1 || answer.askedAsDevice() != map[bool]int{true: 1, false: 0}[device] || len(answer.keys) != 0 {
						t.Fatalf("made the link request %d times, %d of them as this device, %d under a device key; want once, in the form of the machine, and none under a device key",
							len(answer.asked), answer.askedAsDevice(), len(answer.keys))
					}
					if result.code != row.wantCode || result.stdout.Len() != 0 {
						t.Fatalf("exit = %d, want %d; stdout = %q, stderr: %s", result.code, row.wantCode, result.stdout.String(), result.stderr.String())
					}
					clitest.GoldenAt(t, filepath.Join(goldenDir, file), result.stderr.Bytes())
				})
			}
		}
	}
}

// TestGetOpensALinkGivenForTheCRIDAlone covers the rows of the tables that
// end in a link from the link request: a device with an identity, which
// makes the link request first, and a machine with no identity. The link is
// verified against the CRID and then used exactly as a share link is: opened
// in the browser, or opened through the platform and downloaded.
//
// Neither machine sends anything to the qURL API. For the device that is
// what the order is for: the link request gave the link, so no share request
// was sent, and the device did not prove its identity to the API either.
//
// The device here asks as this device, and has the production read of its
// key over the state the fixture wrote. The first request gives the link, so
// the key is not asked for and nothing is sent under it.
// TestGetAnsweredByTheLinkRequestLeavesTheDeviceStateAsItWas has the private
// resource, where the key in that state is used. A machine with no identity
// never asks as a device.
func TestGetOpensALinkGivenForTheCRIDAlone(t *testing.T) {
	state := bootstrapRegisteredState(t)
	for _, device := range []bool{true, false} {
		for _, mode := range getModes() {
			t.Run(fmt.Sprintf("device=%t/%s", device, mode.name), func(t *testing.T) {
				srv := downloadServer(t)
				link := srv.URL + apitest.PortalPath + "#qv2t1.1.1.1.claims.secret.sig"
				requests := &linkRequests{link: issuedLink(link)}
				stateDir := filepath.Join(t.TempDir(), "no-device-state")
				machine := machineWithNoIdentity(t, stateDir)
				if device {
					machine = enrolledDevice(t, state)
				}

				var verified, granted []string
				configure := func(args []string) *runOpts {
					opts := withLinkRequestsOf(machine, requests)(args)
					opts.verifyLink = func(_ context.Context, got, crid string) error {
						verified = append(verified, got+" for "+crid)
						return nil
					}
					opts.enterPortalGrant = func(_ context.Context, got string) (consume.AccessGrant, error) {
						granted = append(granted, got)
						return consume.AccessGrant{
							ContentURL: srv.URL + apitest.DownloadPath, OpenSeconds: 300,
							AuthorizeContentRequest: func(*http.Request) error { return nil },
						}, nil
					}
					return opts
				}

				run := runShareMode(t, srv, srv.URL, mode, configure)
				run.link = link
				run.mustHaveDelivered(t, mode)

				if len(requests.asked) != 1 || requests.asked[0] != srv.Key.CRID {
					t.Errorf("made the link request for %q, want exactly once for %s", requests.asked, srv.Key.CRID)
				}
				wantAsDevice := 0
				if device {
					wantAsDevice = 1
				}
				if got := requests.askedAsDevice(); got != wantAsDevice {
					t.Errorf("asked as this device %d times, want %d", got, wantAsDevice)
				}
				if len(requests.keys) != 0 {
					t.Errorf("%d request(s) were sent under a device key, want none: the first request gave the link", len(requests.keys))
				}
				// The same check a share link gets, before anything is done with it.
				if want := link + " for " + srv.Key.CRID; len(verified) != 1 || verified[0] != want {
					t.Errorf("verified %q, want exactly [%s]", verified, want)
				}
				wantGrants := 0
				if mode.downloads {
					wantGrants = 1
				}
				if len(granted) != wantGrants || (wantGrants == 1 && granted[0] != link) {
					t.Errorf("access was requested for %q, want %d request(s) for the link", granted, wantGrants)
				}
				mustNeverFetchPortalPage(t, srv)

				// What the reader is told about the publisher is what a share
				// link tells them.
				shown := run.result.stderr.String()
				want := `Warning: UNVERIFIED publisher "Acme Docs" (self-declared name, not confirmed by LayerV). Created 2026-03-01.`
				if !mode.downloads {
					// The browser action runs on a terminal, where the row is
					// styled; the words are what is compared.
					shown, want = withoutStyle(run.result.stdout.String()), `Publisher: "Acme Docs" — UNVERIFIED (self-declared name, not confirmed by LayerV)`
				}
				if strings.Count(shown, want) != 1 {
					t.Errorf("output %q, want the publisher shown once as %q", shown, want)
				}

				// No share request, and no proof of identity to the API.
				if got := apiRequests(srv); len(got) != 0 {
					t.Errorf("qURL API requests = %q, want none", got)
				}
				if !device {
					mustNotExistCmd(t, stateDir)
				}
			})
		}
	}
}

// shareAnswerWithPublisher answers a share request with a link to srv's own
// content route and the given publisher object. A nil publisher is the answer
// of a service that sends no publisher and no creation date.
func shareAnswerWithPublisher(t *testing.T, srv *apitest.Server, publisher map[string]any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		data := map[string]any{
			"qurl": srv.URL + apitest.DownloadPath, "crid": srv.Key.CRID, "type": "qv2",
			"expires_at": "2026-03-01T00:05:00Z", "expires_in_seconds": 300, "single_use": true,
		}
		if publisher != nil {
			data["resource_created_at"] = "2026-03-01T00:00:00Z"
			data["publisher"] = publisher
		}
		apitest.WriteEnvelope(t, w, http.StatusOK, data, nil)
	}
}

// publisherShown returns the one line of a get run that says who published
// the resource: the Publisher row of the terminal document, or the notice the
// piped actions write to stderr before anything else.
func publisherShown(t *testing.T, mode shareMode, run *shareRun) string {
	t.Helper()
	if mode.downloads {
		notice, _, _ := strings.Cut(run.result.stderr.String(), "\n")
		return notice
	}
	for line := range strings.SplitSeq(run.result.stdout.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(withoutStyle(line)), "Publisher:") {
			return line
		}
	}
	t.Fatalf("the terminal document has no Publisher row: %q", run.result.stdout.String())
	return ""
}

// TestGetShowsThePublisherOfALinkGivenForTheCRIDAloneAsShareDoes pins what
// the reader is told about the publisher of a link given for the CRID alone.
// The SDK reports the publisher beside the link, as the service stated it.
// get shows it by the rule the share path has, in the same words: "verified"
// only for a publisher the service reported as verified, and the UNVERIFIED
// warning for every other answer, including one with no publisher at all.
//
// Each case runs get twice. A device with an identity gets the publisher in
// a share answer. A machine with no identity gets the same publisher beside a
// link for the CRID alone. The line that names the publisher must be the same
// bytes on both paths.
//
// No service reports a verified publisher today, and the SDK's test server
// answers with one fixed reply that names none. So the answer is given at the
// seam where get takes the SDK's result.
func TestGetShowsThePublisherOfALinkGivenForTheCRIDAloneAsShareDoes(t *testing.T) {
	state := bootstrapRegisteredState(t)
	createdAt := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		// shared is the publisher object of the share answer. nil is an
		// answer with none.
		shared map[string]any
		// reported is the same publisher as the SDK reports it beside a link
		// for the CRID alone.
		reported qurl.Publisher
		// createdAt is the creation date the SDK reports. The share answer
		// carries one exactly when it carries a publisher.
		createdAt *time.Time
	}{
		{
			name:   "verified",
			shared: map[string]any{"name": "Acme Docs", "verified": true}, reported: qurl.Publisher{Name: "Acme Docs", Verified: true},
			createdAt: &createdAt,
		},
		{
			name:   "not verified",
			shared: map[string]any{"name": "Acme Docs", "verified": false}, reported: qurl.Publisher{Name: "Acme Docs"},
			createdAt: &createdAt,
		},
		{name: "no publisher"},
	} {
		for _, mode := range getModes() {
			t.Run(tc.name+"/"+mode.name, func(t *testing.T) {
				shown := func(run *shareRun) string {
					run.mustHaveDelivered(t, mode)
					return publisherShown(t, mode, run)
				}

				shareSrv := downloadServer(t)
				shareSrv.Script(http.MethodPost, shareRoute(shareSrv), shareAnswerWithPublisher(t, shareSrv, tc.shared))
				want := shown(runShareMode(t, shareSrv, shareSrv.URL, mode, enrolledDevice(t, state)))

				srv := downloadServer(t)
				requests := &linkRequests{link: &qurl.CRIDLink{
					Link: srv.URL + apitest.DownloadPath, ExpiresAt: fixedNow.Add(5 * time.Minute),
					ResourceCreatedAt: tc.createdAt, Publisher: tc.reported,
				}}
				stateDir := filepath.Join(t.TempDir(), "no-device-state")
				got := shown(runShareMode(t, srv, srv.URL, mode, withLinkRequests(machineWithNoIdentity(t, stateDir), requests.answer)))

				if got != want {
					t.Errorf("a link for the CRID alone shows the publisher as\n%q\nwant the share path's\n%q", got, want)
				}
				// The share path's own words, so that two equal wrong
				// renderings cannot pass.
				plain := withoutStyle(got)
				if tc.reported.Verified {
					if !strings.Contains(plain, `"Acme Docs"`) || !strings.Contains(plain, wordVerified) ||
						strings.Contains(plain, "UNVERIFIED") || strings.Contains(plain, "Warning") {
						t.Errorf("a publisher the service reported as verified is shown as %q, want the name, %q, and no warning", plain, wordVerified)
					}
					return
				}
				if strings.Count(plain, "UNVERIFIED") != 1 || strings.Contains(plain, wordVerified) {
					t.Errorf("a publisher the service did not report as verified is shown as %q, want it marked UNVERIFIED once", plain)
				}
				if mode.downloads && !strings.HasPrefix(plain, "Warning: UNVERIFIED publisher") {
					t.Errorf("the notice is %q, want the UNVERIFIED warning", plain)
				}
			})
		}
	}
}

// TestGetAnswersForALinkAskedWithTheCRIDAlone walks the rest of the answer
// table: every refusal of the link request, for both starting states and all
// three actions. Each answer has one message and one exit code, nothing is
// opened or saved, and the message is the same for both states except for
// the not-found hint.
//
// A device with an identity goes on to its share request after the refusal.
// That request is answered "not found" here, so the refusal is the result.
func TestGetAnswersForALinkAskedWithTheCRIDAlone(t *testing.T) {
	state := bootstrapRegisteredState(t)
	for _, row := range refusalRows() {
		for _, device := range []bool{true, false} {
			for _, mode := range getModes() {
				t.Run(fmt.Sprintf("%s/device=%t/%s", row.name, device, mode.name), func(t *testing.T) {
					srv := downloadServer(t)
					requests := &linkRequests{err: row.err}
					stateDir := filepath.Join(t.TempDir(), "no-device-state")
					machine := machineWithNoIdentity(t, stateDir)
					if device {
						machine = enrolledDevice(t, state)
						shareNotFoundTwice(t, srv)
					}
					configure := func(args []string) *runOpts {
						opts := withLinkRequests(machine, requests.answer)(args)
						opts.verifyLink = func(context.Context, string, string) error {
							t.Error("a refusal reached link verification: there is no link to verify")
							return nil
						}
						opts.enterPortalGrant = func(context.Context, string) (consume.AccessGrant, error) {
							t.Error("a refusal reached the platform access request")
							return consume.AccessGrant{}, errors.New("unexpected access request")
						}
						return opts
					}

					run := runShareMode(t, srv, srv.URL, mode, configure)
					if run.result.code != row.wantCode {
						t.Fatalf("exit = %d, want %d; stderr: %s", run.result.code, row.wantCode, run.result.stderr.String())
					}
					run.mustNotHaveActed(t)
					if len(requests.asked) != 1 {
						t.Errorf("asked with the CRID alone %d times, want once and no retry", len(requests.asked))
					}

					// The piped actions print exactly the row's golden. The
					// browser action runs on a terminal, where the same lines
					// carry color; TestGetByCRIDAloneGoldens pins the terminal
					// form of the two not-found rows.
					stderr := run.result.stderr.String()
					if !mode.tty {
						if want := goldenBytes(t, row.goldenFor(device)+".plain.stderr.golden"); stderr != want {
							t.Errorf("stderr = %q, want the golden %q", stderr, want)
						}
					}
					// Nothing the SDK or the service wrote may reach the reader.
					for _, leaked := range []string{"endpoint.example.test", "relay", "52602", "51002", "52005", "errCode", "qurl:"} {
						if strings.Contains(strings.ToLower(stderr), strings.ToLower(leaked)) {
							t.Errorf("stderr %q carries %q from the SDK's own error text", stderr, leaked)
						}
					}

					wantAPI := []string(nil)
					if device {
						wantAPI = []string{"GET /v1/me", "POST " + shareRoute(srv)}
					}
					if got := apiRequests(srv); strings.Join(got, "\n") != strings.Join(wantAPI, "\n") {
						t.Errorf("qURL API requests = %q, want %q", got, wantAPI)
					}
					for _, request := range srv.Requests() {
						if request.Path == apitest.DownloadPath {
							t.Error("content was requested although no link was given")
						}
					}
					if !device {
						mustNotExistCmd(t, stateDir)
					}
				})
			}
		}
	}
}

// TestGetOnAMachineWithNoIdentityNeverEnrolls is the rule on its own: a
// machine with no device state and no account key gets its answer from the
// request that uses only the CRID, whatever that answer is, and no identity is
// created or registered. machineWithNoIdentity fails the test if the device
// runtime is opened. This test adds the two things that would show an
// enrollment from outside: a request to the qURL API, and a state directory.
//
// A CRID the SDK will not ask for is an answer too. On this machine it is a
// refusal, because the only other path would create an identity first.
//
// The one case that is not an answer is the compatibility case, a deployment
// that does not offer the request:
// TestGetIsUnchangedWhenTheLinkRequestIsNotOffered pins that such a machine
// then enrolls exactly as it did before.
func TestGetOnAMachineWithNoIdentityNeverEnrolls(t *testing.T) {
	rows := append(refusalRows(),
		answerRow{name: "interrupted", err: errCRIDLinkInterrupted},
		answerRow{name: "CRID version this client cannot check", err: errCRIDVersionTheSDKCannotCheck},
		answerRow{name: "CRID the SDK calls invalid", err: errCRIDTheSDKCallsInvalid},
	)
	for _, row := range rows {
		for _, mode := range getModes() {
			t.Run(row.name+"/"+mode.name, func(t *testing.T) {
				srv := downloadServer(t)
				requests := &linkRequests{err: row.err}
				stateDir := filepath.Join(t.TempDir(), "no-device-state")

				run := runShareMode(t, srv, srv.URL, mode, withLinkRequests(machineWithNoIdentity(t, stateDir), requests.answer))
				if run.result.code == exitcode.Success {
					t.Fatalf("a refusal exited 0; stderr: %s", run.result.stderr.String())
				}
				if len(requests.asked) != 1 {
					t.Errorf("asked with the CRID alone %d times, want once", len(requests.asked))
				}
				if got := srv.Requests(); len(got) != 0 {
					t.Errorf("the machine sent %d request(s) to the qURL API, want none: %+v", len(got), got)
				}
				mustNotExistCmd(t, stateDir)
				if strings.Contains(run.result.stderr.String(), msgAnonymousDevice) {
					t.Errorf("stderr %q announces a new device identity", run.result.stderr.String())
				}
			})
		}
	}

	// And the answer that is a link: the content is fetched, and still no
	// identity exists afterwards.
	for _, mode := range getModes() {
		t.Run("link/"+mode.name, func(t *testing.T) {
			srv := downloadServer(t)
			requests := &linkRequests{link: issuedLink(srv.URL + apitest.DownloadPath)}
			stateDir := filepath.Join(t.TempDir(), "no-device-state")

			run := runShareMode(t, srv, srv.URL, mode, withLinkRequests(machineWithNoIdentity(t, stateDir), requests.answer))
			run.mustHaveDelivered(t, mode)
			if got := apiRequests(srv); len(got) != 0 {
				t.Errorf("the machine sent requests to the qURL API, want none: %q", got)
			}
			mustNotExistCmd(t, stateDir)
		})
	}
}

// errCRIDLinkInterrupted is the SDK's error for a request the user interrupted.
var errCRIDLinkInterrupted = fmt.Errorf("qurl: CRID link request did not complete: %w: %w", context.Canceled, &qurl.RelayError{Msg: "relay POST failed"})

// TestGetInterruptedWhileAskingForALink pins the rule for an interrupt
// during the link request: exit 130 and no error text.
//
// A device with an identity stops there too. It does not go on to its share
// request, which the user did not wait for: nothing is sent to the qURL API,
// and the device runtime is not opened.
func TestGetInterruptedWhileAskingForALink(t *testing.T) {
	state := bootstrapRegisteredState(t)

	t.Run("machine with no identity", func(t *testing.T) {
		srv := downloadServer(t)
		requests := &linkRequests{err: errCRIDLinkInterrupted}
		stateDir := filepath.Join(t.TempDir(), "no-device-state")
		for _, mode := range getModes() {
			run := runShareMode(t, srv, srv.URL, mode, withLinkRequests(machineWithNoIdentity(t, stateDir), requests.answer))
			if run.result.code != exitcode.Interrupted || run.result.stderr.Len() != 0 {
				t.Errorf("%s: exit = %d, stderr = %q; want %d and nothing printed", mode.name, run.result.code, run.result.stderr.String(), exitcode.Interrupted)
			}
			run.mustNotHaveActed(t)
		}
	})

	// The device runtime of this device fails the test when it is opened:
	// opening it is the first step of the share request.
	deviceThatMustNotShare := func(t *testing.T) func(args []string) *runOpts {
		return func(args []string) *runOpts {
			opts := enrolledDevice(t, state)(args)
			opts.openNativeRuntime = func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
				t.Error("the device runtime was opened for a share request after the user interrupted the command")
				return nil, errors.New("unexpected device runtime open")
			}
			return opts
		}
	}
	for name, key := range map[string]*deviceKeyReads{
		"device that reads its key":       deviceKeyOf(t, state),
		"device that cannot read its key": noDeviceKey(connectorstate.NoDeviceKeyUnreadable),
	} {
		for _, mode := range getModes() {
			// The SDK reports the interrupt as the error of the request.
			t.Run(name+"/the request reports the interrupt/"+mode.name, func(t *testing.T) {
				srv := downloadServer(t)
				requests := &linkRequests{err: errCRIDLinkInterrupted}
				run := runShareMode(t, srv, srv.URL, mode, withDeviceKey(withLinkRequestsOf(deviceThatMustNotShare(t), requests), key.read))
				if run.result.code != exitcode.Interrupted || run.result.stderr.Len() != 0 {
					t.Errorf("exit = %d, stderr = %q; want %d and nothing printed", run.result.code, run.result.stderr.String(), exitcode.Interrupted)
				}
				run.mustNotHaveActed(t)
				if got := srv.Requests(); len(got) != 0 {
					t.Errorf("the device sent %d request(s) to the qURL API after the interrupt, want none", len(got))
				}
			})

			// The interrupt comes while the request is answered, and the answer
			// itself is an ordinary one. The command has still been
			// interrupted, and it still stops.
			t.Run(name+"/the request is answered/"+mode.name, func(t *testing.T) {
				srv := downloadServer(t)
				ctx, interrupt := context.WithCancel(t.Context())
				defer interrupt()
				answer := func(context.Context, string) (*qurl.CRIDLink, error) {
					interrupt()
					return nil, sdkRefusal(qurl.ErrCRIDLinkNotFound, "52602")
				}
				configure := func(args []string) *runOpts {
					opts := withDeviceKey(withLinkRequests(deviceThatMustNotShare(t), answer), key.read)(args)
					opts.ctx = ctx
					return opts
				}
				run := runShareMode(t, srv, srv.URL, mode, configure)
				if run.result.code != exitcode.Interrupted || run.result.stderr.Len() != 0 {
					t.Errorf("exit = %d, stderr = %q; want %d and nothing printed", run.result.code, run.result.stderr.String(), exitcode.Interrupted)
				}
				run.mustNotHaveActed(t)
				if got := srv.Requests(); len(got) != 0 {
					t.Errorf("the device sent %d request(s) to the qURL API after the interrupt, want none", len(got))
				}
			})
		}
	}
}

// getOrder is one of the two orders get has for a device with an identity
// where the link request is offered.
type getOrder struct {
	name string
	// flags are added to the command line. --session-duration is get's one
	// share option, and a share option keeps the earlier order.
	flags []string
	// linkRequestFirst says the link request comes before the share request.
	linkRequestFirst bool
}

// getOrders returns both orders: the link request first, which is the order
// with no share option, and the share request first, which a share option
// keeps.
func getOrders() []getOrder {
	return []getOrder{
		{name: "no share option", linkRequestFirst: true},
		{name: "session duration", flags: []string{"--session-duration", "5m"}},
	}
}

// TestGetMakesTheShareRequestOnlyWhenTheLinkRequestGivesNoLink pins the order
// of the two requests for a device with an identity, where the link request
// is offered.
//
// With no share option the link request comes first, once, before anything
// is sent to the qURL API. A link from it ends the run and no share request
// is sent, and the device key is not read for it. Any other answer leads to
// one share request, and a link or a failure of that request is its own
// result. The answer here is "not found", which is the one answer the device
// key is read for.
//
// With a share option the order is the earlier one: the share request first,
// and the link request only after its "not found". Then the request is made
// with the CRID alone, never as this device, and the device key is not read.
func TestGetMakesTheShareRequestOnlyWhenTheLinkRequestGivesNoLink(t *testing.T) {
	state := bootstrapRegisteredState(t)
	other := apitest.GenerateResourceKey(t)
	for _, tc := range []struct {
		name string
		// linkGiven says the link request is answered with a link. Otherwise
		// it is answered "not found".
		linkGiven bool
		prepare   func(t *testing.T, srv *apitest.Server)
		// wantCode is the exit code when the link request is answered "not
		// found", or is not made: the share request's own result.
		wantCode int
		// shareNotFound says the share request is answered "not found", the
		// one answer that leads to the link request in the earlier order.
		shareNotFound bool
		// noShareRequest says the device cannot send its share request,
		// because the API refuses its credential before that.
		noShareRequest bool
	}{
		{name: "link from the link request", linkGiven: true, prepare: func(*testing.T, *apitest.Server) {}, wantCode: exitcode.Success},
		{name: "link from the share request", prepare: func(*testing.T, *apitest.Server) {}, wantCode: exitcode.Success},
		{
			name: "not found", wantCode: exitcode.NotFound, shareNotFound: true,
			prepare: func(t *testing.T, srv *apitest.Server) { shareNotFoundTwice(t, srv) },
		},
		{
			name: "not found, the other code", wantCode: exitcode.NotFound, shareNotFound: true,
			prepare: func(t *testing.T, srv *apitest.Server) {
				srv.Script(http.MethodPost, shareRoute(srv), apitest.HandlerNotFound404(t, "not_found"))
			},
		},
		{
			name: "deleted, told to the owner", wantCode: exitcode.NotFound,
			prepare: func(t *testing.T, srv *apitest.Server) {
				srv.Script(http.MethodPost, shareRoute(srv), apitest.HandlerRevoked400(t))
			},
		},
		{
			name: "retired", wantCode: exitcode.NotFound,
			prepare: func(t *testing.T, srv *apitest.Server) {
				srv.Script(http.MethodPost, shareRoute(srv), apitest.HandlerTombstoned410(t))
			},
		},
		{
			name: "links not served", wantCode: exitcode.Unavailable,
			prepare: func(t *testing.T, srv *apitest.Server) {
				srv.Script(http.MethodPost, shareRoute(srv), apitest.HandlerDark503(t))
			},
		},
		{
			name: "connector stopped", wantCode: exitcode.Unavailable,
			prepare: func(t *testing.T, srv *apitest.Server) {
				srv.Script(http.MethodPost, shareRoute(srv), apitest.HandlerConnectorStopped503(t))
			},
		},
		{
			name: "account frozen", wantCode: exitcode.Forbidden,
			prepare: func(t *testing.T, srv *apitest.Server) {
				srv.Script(http.MethodPost, shareRoute(srv), apitest.HandlerAccountFrozen403(t))
			},
		},
		{
			name: "device credential rejected", wantCode: exitcode.Auth, noShareRequest: true,
			prepare: func(t *testing.T, srv *apitest.Server) {
				srv.Script(http.MethodGet, "/v1/me", apitest.HandlerAPIKeyInvalid401(t))
			},
		},
		{
			name: "answer for another CRID", wantCode: exitcode.VerificationFailed,
			prepare: func(_ *testing.T, srv *apitest.Server) { srv.SetShareCRID(other.CRID) },
		},
	} {
		for _, order := range getOrders() {
			for _, mode := range getModes() {
				t.Run(tc.name+"/"+order.name+"/"+mode.name, func(t *testing.T) {
					srv := downloadServer(t)
					tc.prepare(t, srv)
					requests := &linkRequests{err: sdkRefusal(qurl.ErrCRIDLinkNotFound, "52602")}
					if tc.linkGiven {
						requests = &linkRequests{link: issuedLink(srv.URL + apitest.DownloadPath)}
					}
					// What the qURL API had seen when each link request was made.
					var apiSeen [][]string
					observed := &linkRequests{}
					answer := func(ctx context.Context, resourceCRID string) (*qurl.CRIDLink, error) {
						apiSeen = append(apiSeen, apiRequests(srv))
						observed.asDevice = append(observed.asDevice, false)
						return requests.answer(ctx, resourceCRID)
					}
					answerAsDevice := func(ctx context.Context, deviceKey qurl.DeviceKeySource, resourceCRID string) (*qurl.CRIDLink, error) {
						apiSeen = append(apiSeen, apiRequests(srv))
						observed.asDevice = append(observed.asDevice, true)
						return requests.answerAsDevice(ctx, deviceKey, resourceCRID)
					}
					key := deviceKeyOf(t, state)
					configure := func(args []string) *runOpts {
						opts := withArgs(enrolledDevice(t, state), order.flags...)(args)
						opts.requestCRIDLink, opts.requestCRIDLinkAsDevice, opts.readDeviceKey = answer, answerAsDevice, key.read
						return opts
					}

					run := runShareMode(t, srv, srv.URL, mode, configure)

					// The link request: how often, in which form, and when. The
					// device key is read only when the request as this device
					// is answered "not found". A link from its first request
					// needs no key.
					wantAsks, wantAsDevice, wantKeyReads := 1, 1, 1
					if tc.linkGiven {
						wantKeyReads = 0
					}
					if !order.linkRequestFirst {
						wantAsDevice, wantKeyReads = 0, 0
						if !tc.shareNotFound {
							wantAsks = 0
						}
					}
					if len(requests.asked) != wantAsks || observed.askedAsDevice() != wantAsDevice || len(key.keysGiven()) != wantKeyReads {
						t.Errorf("made the link request %d times, %d of them as this device, and read the device key %d times; want %d, %d and %d",
							len(requests.asked), observed.askedAsDevice(), len(key.keysGiven()), wantAsks, wantAsDevice, wantKeyReads)
					}
					for _, seen := range apiSeen {
						if order.linkRequestFirst && len(seen) != 0 {
							t.Errorf("the link request came after %q, want it before anything is sent to the qURL API", seen)
						}
						if want := []string{"GET /v1/me", "POST " + shareRoute(srv)}; !order.linkRequestFirst && strings.Join(seen, "\n") != strings.Join(want, "\n") {
							t.Errorf("the link request came after %q, want it after the share request: %q", seen, want)
						}
					}

					// The share request: sent once, except where the link request
					// came first and gave the link.
					wantShares := 1
					if tc.noShareRequest || (order.linkRequestFirst && tc.linkGiven) {
						wantShares = 0
					}
					if got := len(shareRequests(srv)); got != wantShares {
						t.Errorf("the share request was sent %d times, want %d", got, wantShares)
					}

					// The result. Where a link request would give a link, a
					// share request that is asked gives one too.
					wantCode := tc.wantCode
					if tc.linkGiven {
						wantCode = exitcode.Success
					}
					if run.result.code != wantCode {
						t.Fatalf("exit = %d, want %d; stderr: %s", run.result.code, wantCode, run.result.stderr.String())
					}
					if wantCode == exitcode.Success {
						run.mustHaveDelivered(t, mode)
					} else {
						run.mustNotHaveActed(t)
					}
				})
			}
		}
	}
}

// TestGetSaysTheServiceDidNotAnswerAfterShareNotFound pins one row of the
// second table in get_crid_link.go. The share request said "not found", and
// the link request got no answer: it timed out, or the service could not be
// reached. Nobody knows then whether the resource opens for this device, and
// "not found" would be wrong for every public resource. So get says that the
// service did not answer, with the exit code for "try again later". It
// prints neither "not found" nor the not-found hint, and it sends each of
// the two requests once.
//
// The result is the same in both orders: the link request first, and the
// share request first, which a share option keeps.
//
// Where the link request comes first, this is the row "no answer for another
// reason": both answers here come at once, while the short time limit of
// that request still has time left. A request that ends because the short
// limit ran out is made once more, and
// TestGetMakesTheLinkRequestOnceMoreAfterItsShortLimitRanOut has that row.
func TestGetSaysTheServiceDidNotAnswerAfterShareNotFound(t *testing.T) {
	state := bootstrapRegisteredState(t)
	for name, noAnswer := range map[string]error{
		"timed out": fmt.Errorf("qurl: CRID link request did not complete: %w: %w",
			context.DeadlineExceeded, &qurl.RelayError{Msg: "relay POST https://endpoint.example.test/x failed"}),
		"cannot connect": &qurl.RelayError{Msg: "relay POST https://endpoint.example.test/x failed: connection refused"},
	} {
		for _, order := range getOrders() {
			for _, mode := range getModes() {
				t.Run(name+"/"+order.name+"/"+mode.name, func(t *testing.T) {
					srv := downloadServer(t)
					shareNotFoundTwice(t, srv)
					requests := &linkRequests{err: noAnswer}
					// How many share requests had been sent when the link
					// request was made.
					sharesBefore := -1
					answer := func(ctx context.Context, resourceCRID string) (*qurl.CRIDLink, error) {
						sharesBefore = len(shareRequests(srv))
						return requests.answer(ctx, resourceCRID)
					}

					run := runShareMode(t, srv, srv.URL, mode, withLinkRequests(withArgs(enrolledDevice(t, state), order.flags...), answer))
					if run.result.code != exitcode.Unavailable {
						t.Fatalf("exit = %d, want %d; stderr: %s", run.result.code, exitcode.Unavailable, run.result.stderr.String())
					}
					run.mustNotHaveActed(t)

					// The whole of stderr is the one message: no hint follows it.
					stderr := withoutStyle(run.result.stderr.String())
					if want := "Error: " + consume.MsgCRIDLinkNoAnswer + "\n"; stderr != want {
						t.Errorf("stderr = %q, want exactly %q", stderr, want)
					}
					for _, unwanted := range []string{consume.MsgCRIDNotFound, "not found", "Hint:"} {
						if strings.Contains(stderr, unwanted) {
							t.Errorf("stderr %q carries %q from the share request's answer", stderr, unwanted)
						}
					}
					if got := len(shareRequests(srv)); got != 1 {
						t.Errorf("the share request was sent %d times, want once", got)
					}
					if len(requests.asked) != 1 {
						t.Errorf("made the link request %d times, want once and no retry", len(requests.asked))
					}
					wantSharesBefore := 1
					if order.linkRequestFirst {
						wantSharesBefore = 0
					}
					if sharesBefore != wantSharesBefore {
						t.Errorf("the link request was made after %d share request(s), want %d", sharesBefore, wantSharesBefore)
					}
				})
			}
		}
	}
}

// TestShareNeverAsksWithTheCRIDAlone pins that `qurl share` is unchanged. It
// shares with the device and reports the service's answer, whatever the link
// request would have said. It makes the link request in neither form, it
// does not ask whether the request is offered, and it does not read the
// device key.
func TestShareNeverAsksWithTheCRIDAlone(t *testing.T) {
	state := bootstrapRegisteredState(t)
	share := shareModes()[0]
	if share.name != "share" {
		t.Fatalf("first share mode is %q, want share", share.name)
	}
	for name, machine := range map[string]func(args []string) *runOpts{
		"device with an identity": enrolledDevice(t, state),
		"account key":             func(args []string) *runOpts { return &runOpts{args: args} },
	} {
		t.Run(name, func(t *testing.T) {
			srv := downloadServer(t)
			shareNotFoundTwice(t, srv)
			configure := func(args []string) *runOpts {
				opts := withLinkOffer(machine, mustNotCheckTheLinkOffer(t))(args)
				opts.requestCRIDLink, opts.requestCRIDLinkAsDevice = mustNotAskWithTheCRIDAlone(t), mustNotAskAsTheDevice(t)
				opts.readDeviceKey = mustNotReadTheDeviceKey(t)
				return opts
			}
			run := runShareMode(t, srv, srv.URL, share, configure)
			if run.result.code != exitcode.NotFound {
				t.Fatalf("exit = %d, want %d; stderr: %s", run.result.code, exitcode.NotFound, run.result.stderr.String())
			}
			if got, want := run.result.stderr.String(), goldenBytes(t, "error_share_notfound.plain.stderr.golden"); got != want {
				t.Errorf("share not-found stderr = %q, want the unchanged golden %q", got, want)
			}
		})
	}
}

// todaysMint is what get's mint did before the link request existed: the
// share request, then the check that the answer names the CRID that was asked
// for. Neither function was changed, so this is the reference linkForGet is
// compared with.
func todaysMint(ctx context.Context, opts *globalOpts, assessment *cridux.Assessment, options qurlapi.ShareOptions) (*qurlapi.ShareLink, error) {
	link, err := opts.shareResource(ctx, assessment.Input, options)
	if err := verifyShareLink(assessment, link, err); err != nil {
		return nil, err
	}
	return link, nil
}

// describeLink writes out every field of a share result, so two results can
// be compared by value.
func describeLink(link *qurlapi.ShareLink) string {
	if link == nil {
		return "no link"
	}
	created := "none"
	if link.ResourceCreatedAt != nil {
		created = link.ResourceCreatedAt.UTC().Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("link=%s crid=%s type=%s expires=%s seconds=%d single=%t created=%s publisher=%q verified=%t",
		link.QURL, link.CRID, link.Type, link.ExpiresAt.UTC().Format(time.RFC3339Nano), link.ExpiresInSeconds,
		link.SingleUse, created, link.Publisher.Name, link.Publisher.Verified)
}

// renderedError is what the user is shown for err, and the exit code.
func renderedError(err error) string {
	if err == nil {
		return "exit 0"
	}
	var rendered strings.Builder
	output.RenderError(&rendered, err, false)
	return fmt.Sprintf("exit %d\n%s", exitcode.FromError(err), rendered.String())
}

// accountKeyOpts is a command context that shares with the harness's account
// key against srv, with offered as the check for the link request and answer
// as the answer to a link request sent under a random key. That is the
// request with only the CRID, and the first request of the request as this
// device. The machine holds no device state, so keyReads gives no key, and a
// request under a device key fails the test.
func accountKeyOpts(
	t *testing.T, srv *apitest.Server, offered func() (bool, error),
	answer func(context.Context, string) (*qurl.CRIDLink, error), keyReads *deviceKeyReads,
) *globalOpts {
	t.Helper()
	opts := &globalOpts{
		resolvedEndpoint: srv.URL,
		version:          "test",
		lookupEnv:        func(key string) (string, bool) { return testAPIKey, key == "QURL_API_KEY" },
		newRequestID:     func() string { return "cli-req-fixed" },
		sleep:            func(time.Duration) {},
		cridLinkOffered:  offered,
		requestCRIDLink:  answer,
		requestCRIDLinkAsDevice: func(ctx context.Context, deviceKey qurl.DeviceKeySource, resourceCRID string) (*qurl.CRIDLink, error) {
			link, err := answer(ctx, resourceCRID)
			if !errors.Is(err, qurl.ErrCRIDLinkNotFound) {
				return link, err
			}
			return asDeviceAfterNotFound(ctx, deviceKey, err, func([]byte) (*qurl.CRIDLink, error) {
				t.Error("a link request was sent under a device key on a machine that holds no device state")
				return nil, err
			})
		},
		readDeviceKey: keyReads.read,
	}
	opts.openAPIClient = func(context.Context) (qurlapi.Client, error) { return opts.apiClient(testAPIKey) }
	return opts
}

// TestLinkForGetIsTheSharePathWhenNoLinkCanBeAskedFor compares linkForGet
// with todaysMint for every kind of answer the share request can get, in the
// three cases where the link request leads nowhere: the deployment does not
// offer the request, and the two answers of the SDK for a CRID it will not
// ask for. The link, or the error as the user sees it, the exit code, and the
// requests sent to the qURL API must all be equal: nothing is added and
// nothing is dropped.
//
// It runs in both orders. With a share option, the link request is looked at
// only after a share "not found", and then exactly once. With no share
// option, the check for whether the request is offered comes first, once,
// whatever the share request then answers; where the request is offered, the
// link request is made first, once, and the SDK sends nothing for it.
//
// The device key is never read here. Where the request is not offered, no
// link request is made. Where it is made first, the SDK refuses the CRID
// before it sends anything, and the key is read only after an answer "not
// found".
func TestLinkForGetIsTheSharePathWhenNoLinkCanBeAskedFor(t *testing.T) {
	key := apitest.FixedResourceKey(t)
	other := apitest.GenerateResourceKey(t)
	route := "/v1/resources/" + key.CRID + "/share"
	// cause is why the link request leads nowhere. notRequestable is nil when
	// the request is not offered at all, and then it must never be made.
	causes := map[string]struct{ notRequestable error }{
		"not offered":                {},
		"CRID the SDK cannot check":  {errCRIDVersionTheSDKCannotCheck},
		"CRID the SDK calls invalid": {errCRIDTheSDKCallsInvalid},
	}
	services := map[string]func(t *testing.T, srv *apitest.Server){
		"link":                   func(*testing.T, *apitest.Server) {},
		"link with no publisher": func(_ *testing.T, srv *apitest.Server) { srv.OmitPublisherMetadata() },
		"not found": func(t *testing.T, srv *apitest.Server) {
			srv.Script(http.MethodPost, route, apitest.HandlerNotFound404(t, "resource_not_found"))
		},
		"not found, other code": func(t *testing.T, srv *apitest.Server) {
			srv.Script(http.MethodPost, route, apitest.HandlerNotFound404(t, "not_found"))
		},
		"deleted": func(t *testing.T, srv *apitest.Server) {
			srv.Script(http.MethodPost, route, apitest.HandlerRevoked400(t))
		},
		"retired": func(t *testing.T, srv *apitest.Server) {
			srv.Script(http.MethodPost, route, apitest.HandlerTombstoned410(t))
		},
		"links not served": func(t *testing.T, srv *apitest.Server) { srv.Script(http.MethodPost, route, apitest.HandlerDark503(t)) },
		"connector stopped": func(t *testing.T, srv *apitest.Server) {
			srv.Script(http.MethodPost, route, apitest.HandlerConnectorStopped503(t))
		},
		"account frozen": func(t *testing.T, srv *apitest.Server) {
			srv.Script(http.MethodPost, route, apitest.HandlerAccountFrozen403(t))
		},
		"key not allowed": func(t *testing.T, srv *apitest.Server) {
			srv.Script(http.MethodPost, route, apitest.HandlerInsufficientScope403(t))
		},
		"key rejected": func(t *testing.T, srv *apitest.Server) {
			srv.Script(http.MethodPost, route, apitest.HandlerAPIKeyInvalid401(t))
		},
		"rate limited": func(t *testing.T, srv *apitest.Server) {
			srv.ScriptRepeat(http.MethodPost, route, 3, apitest.Handler429(t, 2))
		},
		"answer for another CRID": func(_ *testing.T, srv *apitest.Server) { srv.SetShareCRID(other.CRID) },
	}
	assessment, err := cridux.Assess(key.CRID)
	if err != nil {
		t.Fatal(err)
	}
	orders := map[string]qurlapi.ShareOptions{
		"share option":    {SessionDurationSeconds: 300},
		"no share option": {},
	}

	for orderName, options := range orders {
		linkRequestFirst := options == (qurlapi.ShareOptions{})
		for cause, tc := range causes {
			for name, prepare := range services {
				t.Run(orderName+"/"+cause+"/"+name, func(t *testing.T) {
					// Two servers with the same script, so each side's requests can
					// be counted on its own.
					before := apitest.NewServerWithKey(t, key)
					prepare(t, before)
					unread := noDeviceKey(connectorstate.NoDeviceKeyNoState)
					wantLink, wantErr := todaysMint(t.Context(), accountKeyOpts(t, before, mustNotCheckTheLinkOffer(t), mustNotAskWithTheCRIDAlone(t), unread), assessment, options)

					after := apitest.NewServerWithKey(t, key)
					prepare(t, after)
					offer := &linkOffer{offered: tc.notRequestable != nil}
					keyReads := noDeviceKey(connectorstate.NoDeviceKeyNoState)
					asks := 0
					gotLink, origin, gotErr := accountKeyOpts(t, after, offer.answer, func(context.Context, string) (*qurl.CRIDLink, error) {
						asks++
						return nil, tc.notRequestable
					}, keyReads).linkForGet(t.Context(), assessment, options)

					if origin.byLinkRequest() {
						t.Error("linkForGet reports a link from the link request although none could be asked for")
					}
					if got, want := renderedError(gotErr), renderedError(wantErr); got != want {
						t.Errorf("linkForGet failed with\n%s\nwant what the share path gives:\n%s", got, want)
					}
					if got, want := describeLink(gotLink), describeLink(wantLink); got != want {
						t.Errorf("linkForGet link = %s, want the share path's %s", got, want)
					}
					if got, want := apiRequests(after), apiRequests(before); strings.Join(got, "\n") != strings.Join(want, "\n") {
						t.Errorf("linkForGet sent %q, want the share path's %q", got, want)
					}

					// With a share option the link request is looked at only
					// after a share not-found, and then exactly once. With none
					// it is looked at first, once. The request itself is made
					// only where it is offered. The key is not read for a
					// request that the SDK does not send.
					wantChecks, wantAsks := 1, 0
					switch {
					case linkRequestFirst && offer.offered:
						wantAsks = 1
					case linkRequestFirst:
					case !strings.HasPrefix(name, "not found"):
						wantChecks = 0
					case offer.offered:
						wantAsks = 1
					}
					if offer.checks != wantChecks || asks != wantAsks || len(keyReads.keysGiven()) != 0 || len(unread.keysGiven()) != 0 {
						t.Errorf("asked whether the request is offered %d times, made it %d times and read the device key %d times; want %d, %d and 0",
							offer.checks, asks, len(keyReads.keysGiven()), wantChecks, wantAsks)
					}
				})
			}
		}
	}
}

// TestGetIsUnchangedWhenTheLinkRequestIsNotOffered is the compatibility
// guarantee at the command level, for every starting state and action: where
// the deployment does not offer the link request, get prints what it printed
// before, exits as before, and sends the requests it sent before. A machine
// with no identity therefore still enrolls here, exactly as it did. The link
// request itself is never made, in either form: the harness fails a test
// that makes it. And the device key is never read: a machine that is never
// offered the request never opens its device state for it.
//
// "Before" is pinned three ways: the share path's own golden files, which
// this change does not touch; the request list each case states; and
// TestLinkForGetIsTheSharePathWhenNoLinkCanBeAskedFor above.
//
// One thing is not as before, and it sends and creates nothing: a device
// with an identity now asks whether the request is offered before it shares,
// once, where it asked only after a share "not found". With a share option
// that device asks when it always did.
func TestGetIsUnchangedWhenTheLinkRequestIsNotOffered(t *testing.T) {
	state := bootstrapRegisteredState(t)

	type machine struct {
		name string
		// configure builds the invocation; enrolled reports afterwards whether
		// the device runtime created an identity.
		configure func(t *testing.T, enrolled *bool) func(args []string) *runOpts
		// noIdentity says the machine starts with neither device state nor an
		// account key. It asks whether the request is offered before anything
		// else, and it enrolls on the share path.
		noIdentity bool
		// wantAPI is the qURL API request list for a share that is answered.
		wantAPI func(srv *apitest.Server) []string
	}
	machines := []machine{
		{
			name: "device with an identity",
			configure: func(t *testing.T, _ *bool) func(args []string) *runOpts {
				return enrolledDevice(t, state)
			},
			wantAPI: func(srv *apitest.Server) []string { return []string{"GET /v1/me", "POST " + shareRoute(srv)} },
		},
		{
			// The case TestShareOnANewDeviceEnrollsBeforeSharing has always
			// covered: no device state and no account key.
			name: "machine with no identity", noIdentity: true,
			configure: func(t *testing.T, enrolled *bool) func(args []string) *runOpts {
				return machineThatEnrolls(t, state, enrolled)
			},
			wantAPI: func(srv *apitest.Server) []string { return []string{"GET /v1/me", "POST " + shareRoute(srv)} },
		},
		{
			// The harness's account-key client: no device state, a key in the
			// environment.
			name: "account key",
			configure: func(*testing.T, *bool) func(args []string) *runOpts {
				return func(args []string) *runOpts { return &runOpts{args: args} }
			},
			wantAPI: func(srv *apitest.Server) []string { return []string{"POST " + shareRoute(srv)} },
		},
	}

	for _, m := range machines {
		for _, order := range getOrders() {
			// notOffered is the machine with the answer "not offered", the
			// flags of the order, and a read of the device key that fails the
			// test.
			notOffered := func(t *testing.T, offer *linkOffer, enrolled *bool) func(args []string) *runOpts {
				return withDeviceKey(withLinkOffer(withArgs(m.configure(t, enrolled), order.flags...), offer.answer), mustNotReadTheDeviceKey(t))
			}
			for _, mode := range getModes() {
				t.Run(m.name+"/"+order.name+"/link/"+mode.name, func(t *testing.T) {
					srv := downloadServer(t)
					offer := &linkOffer{}
					enrolled := false

					run := runShareMode(t, srv, srv.URL, mode, notOffered(t, offer, &enrolled))
					run.mustHaveDelivered(t, mode)

					// With no share option, every machine looks at the link
					// request before it shares, once. With a share option only a
					// machine with no identity does: a share that is answered
					// ends it for the others, as before.
					wantChecks := 1
					if !order.linkRequestFirst && !m.noIdentity {
						wantChecks = 0
					}
					if offer.checks != wantChecks {
						t.Errorf("asked whether the request is offered %d times, want %d", offer.checks, wantChecks)
					}
					if enrolled != m.noIdentity {
						t.Errorf("enrolled = %t, want %t: a machine with no identity enrolls on the share path, as before", enrolled, m.noIdentity)
					}
					if got, want := apiRequests(srv), m.wantAPI(srv); strings.Join(got, "\n") != strings.Join(want, "\n") {
						t.Errorf("qURL API requests = %q, want %q", got, want)
					}
				})

				t.Run(m.name+"/"+order.name+"/not found/"+mode.name, func(t *testing.T) {
					srv := downloadServer(t)
					shareNotFoundTwice(t, srv)
					offer := &linkOffer{}
					enrolled := false

					run := runShareMode(t, srv, srv.URL, mode, notOffered(t, offer, &enrolled))
					if run.result.code != exitcode.NotFound {
						t.Fatalf("exit = %d, want %d; stderr: %s", run.result.code, exitcode.NotFound, run.result.stderr.String())
					}
					run.mustNotHaveActed(t)

					// The share path's own not-found text, byte for byte. A machine
					// that just enrolled says so first, as it always has.
					want := goldenBytes(t, "error_share_notfound.plain.stderr.golden")
					if mode.tty {
						want = goldenBytes(t, "error_share_notfound.tty.stderr.golden")
					}
					if m.noIdentity {
						want = dimIf(mode.tty, msgAnonymousDevice) + "\n" + want
					}
					if got := run.result.stderr.String(); got != want {
						t.Errorf("stderr = %q, want the share path's not-found %q", got, want)
					}

					// Asked once, never twice: a machine that asked before it
					// shared already knows the answer when the share request
					// fails.
					if offer.checks != 1 {
						t.Errorf("asked whether the request is offered %d times, want once", offer.checks)
					}
					if got, want := apiRequests(srv), m.wantAPI(srv); strings.Join(got, "\n") != strings.Join(want, "\n") {
						t.Errorf("qURL API requests = %q, want %q (one share request, no retry)", got, want)
					}
				})
			}
		}
	}
}

// dimIf renders a stderr note the way the printer does on a terminal.
func dimIf(tty bool, line string) string {
	if !tty {
		return line
	}
	return "\x1b[2m" + line + "\x1b[0m"
}

// writeTestDeployment writes a deployment settings file and returns an
// environment lookup that names it.
func writeTestDeployment(t *testing.T, deployment *qurl.Deployment) func(string) (string, bool) {
	t.Helper()
	raw, err := json.Marshal(deployment)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "deployment.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return func(name string) (string, bool) { return path, name == qurl.EnvDeploymentPath }
}

// shippedShapeDeployment is a deployment file with the shape of the one every
// release ships: keys to verify links with, one cell, and no place to send a
// request with only a CRID.
func shippedShapeDeployment(t *testing.T) *qurl.Deployment {
	t.Helper()
	signer, err := qurl.GenerateLocalSigner("get-crid-link-test")
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := signer.PublicKeyDER()
	if err != nil {
		t.Fatal(err)
	}
	return &qurl.Deployment{
		Issuers: []qurl.ManifestIssuer{{Kid: signer.KID(), SPKIDERB64: base64.RawURLEncoding.EncodeToString(issuer)}},
		Cells: []qurl.DeploymentCell{{
			CellID: "cell-a", Host: "cell-a.example.test", Port: 443,
			ServerPublicKeyB64: bootstrapRegisteredState(t).PublicKeyB64,
		}},
	}
}

// TestGetThroughTheRealSDKCheckIsUnchanged runs get with the production check
// and the production request function over the real SDK. No fake stands in
// for the answer: the SDK itself reports that a deployment file shaped like
// the one every release ships does not offer the request, and get behaves as
// it did before.
//
// The CRID does not change that. A CRID whose version the SDK cannot check is
// on the same path as any other here, because the check needs no CRID. There
// is no HTTP double on purpose: nothing may be sent, and if the SDK ever
// tried, the request would go to a reserved test name and fail the run.
//
// Both forms of the link request are the production functions here, and
// neither is reached. The device key is not read: a read fails the test.
func TestGetThroughTheRealSDKCheckIsUnchanged(t *testing.T) {
	state := bootstrapRegisteredState(t)
	key := apitest.GenerateResourceKey(t)

	for _, tc := range []struct {
		name string
		crid string
	}{
		{"file that names no endpoint", key.CRID},
		{"file that names no endpoint, CRID version the SDK cannot check", apitest.DeriveCRID(t, key.DER, 0x05)},
	} {
		opener := &consume.AccessOpener{LookupEnv: writeTestDeployment(t, shippedShapeDeployment(t))}
		// Proof that the real check gives the "not offered" answer.
		if offered, err := opener.CRIDLinkOffered(); offered || err != nil {
			t.Fatalf("%s: the real SDK check returned %t, %v; want not offered and no error", tc.name, offered, err)
		}
		share := "/v1/resources/" + tc.crid + "/share"

		for _, mode := range getModes() {
			t.Run(tc.name+"/device with an identity/not found/"+mode.name, func(t *testing.T) {
				srv := downloadServer(t)
				srv.ScriptRepeat(http.MethodPost, share, 2, apitest.HandlerNotFound404(t, "resource_not_found"))
				dest := filepath.Join(t.TempDir(), "content")
				browser := &fakeBrowser{}
				opts := enrolledDevice(t, state)(append([]string{"--endpoint", srv.URL}, mode.args(tc.crid, dest)...))
				opts.tty, opts.browser = mode.tty, browser
				opts.cridLinkOffered, opts.requestCRIDLink = opener.CRIDLinkOffered, opener.RequestCRIDLink
				opts.requestCRIDLinkAsDevice, opts.readDeviceKey = opener.RequestCRIDLinkAsDevice, mustNotReadTheDeviceKey(t)

				result := runCLI(t, opts)
				want := goldenBytes(t, "error_share_notfound.plain.stderr.golden")
				if mode.tty {
					want = goldenBytes(t, "error_share_notfound.tty.stderr.golden")
				}
				if result.code != exitcode.NotFound || result.stderr.String() != want || result.stdout.Len() != 0 {
					t.Fatalf("exit = %d, stdout = %q, stderr = %q; want exit %d, no output, and the share path's not-found text",
						result.code, result.stdout.String(), result.stderr.String(), exitcode.NotFound)
				}
				if got, want := apiRequests(srv), []string{"GET /v1/me", "POST " + share}; strings.Join(got, "\n") != strings.Join(want, "\n") {
					t.Errorf("qURL API requests = %q, want %q", got, want)
				}
				if len(browser.opened) != 0 {
					t.Errorf("a browser was opened: %q", browser.opened)
				}
				mustNotExistCmd(t, dest)
			})

			t.Run(tc.name+"/machine with no identity/link/"+mode.name, func(t *testing.T) {
				srv := downloadServer(t)
				enrolled := false
				dest := filepath.Join(t.TempDir(), "content")
				browser := &fakeBrowser{}
				opts := machineThatEnrolls(t, state, &enrolled)(append([]string{"--endpoint", srv.URL}, mode.args(tc.crid, dest)...))
				opts.tty, opts.browser = mode.tty, browser
				opts.cridLinkOffered, opts.requestCRIDLink = opener.CRIDLinkOffered, opener.RequestCRIDLink
				opts.requestCRIDLinkAsDevice, opts.readDeviceKey = opener.RequestCRIDLinkAsDevice, mustNotReadTheDeviceKey(t)

				result := runCLI(t, opts)
				if result.code != exitcode.Success || !enrolled {
					t.Fatalf("exit = %d, enrolled = %t; want the share path to run as before: %s", result.code, enrolled, result.stderr.String())
				}
				if got, want := apiRequests(srv), []string{"GET /v1/me", "POST " + share}; strings.Join(got, "\n") != strings.Join(want, "\n") {
					t.Errorf("qURL API requests = %q, want %q", got, want)
				}
			})
		}
	}
}

// TestGetWithAnAccountKeyAsksWithTheCRIDAloneFirst covers the third starting
// state: no device state, but an account key in the environment. That key is
// the owner's instruction to enroll this machine under their account, so the
// machine counts as one with an identity and keeps its share request.
//
// Where the link request is offered, it comes first here too, and that is
// intended. The machine has no device state, so it has no device key. What
// it sends is the request with the CRID alone: nothing is ever sent under a
// device key. When the answer is "not found", the machine looks for its key,
// finds no device state, and the read changes nothing in its empty state
// directory. The cost is one link request before the share request, with the
// short time limit a link request has when a share request follows it.
//
//   - A resource that opens with the CRID alone is answered there. The share
//     request is not sent, so the machine is not enrolled by this run. That
//     is what a machine with no identity already does.
//   - For the owner's own private resource the link request says "not
//     found". The share request follows, enrolls the machine under the
//     account as before, and gives the link. An owner's fresh machine keeps
//     opening the owner's own private resources.
//   - A resource this machine may not open ends in "not found", after the
//     machine enrolled, as before.
//
// With a share option the machine shares first, exactly as before.
func TestGetWithAnAccountKeyAsksWithTheCRIDAloneFirst(t *testing.T) {
	const enrollmentToken = "lv_test_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
	state := bootstrapRegisteredState(t)
	accountKeyMachine := func(t *testing.T, srv *apitest.Server, enrolled *bool) func(args []string) *runOpts {
		srv.Script(http.MethodPost, "/v1/api-keys", func(w http.ResponseWriter, _ *http.Request) {
			apitest.WriteEnvelope(t, w, http.StatusCreated, map[string]any{
				"api_key": enrollmentToken, "key_id": "key_enrollment01",
				"kind": "enrollment_token", "target": "agent", "claims": []any{},
				"status": "active", "expires_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
			}, nil)
		})
		return func(args []string) *runOpts {
			opts := registeredDeviceOpts(t, args, func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
				token, err := cfg.EnrollmentCredentialProvider(ctx, qurl.AgentEnrollmentCredentialRequest{AgentID: state.AgentID})
				if err != nil || token != enrollmentToken {
					t.Errorf("enrollment credential = %d bytes, %v; want the token minted with the account key", len(token), err)
				}
				*enrolled = true
				return &bootstrapNativeRuntime{store: &bootstrapAgentStateStore{state: state}}, nil
			})
			opts.env = map[string]string{"QURL_API_KEY": testAPIKey}
			return opts
		}
	}
	enrollingShare := func(srv *apitest.Server) []string {
		return []string{"GET /v1/me", "POST /v1/api-keys", "GET /v1/me", "POST " + shareRoute(srv)}
	}
	notFound := sdkRefusal(qurl.ErrCRIDLinkNotFound, "52602")

	for _, mode := range getModes() {
		t.Run("a resource that opens with the CRID alone/"+mode.name, func(t *testing.T) {
			srv := downloadServer(t)
			shareNotFoundTwice(t, srv)
			enrolled := false
			requests := &linkRequests{link: issuedLink(srv.URL + apitest.DownloadPath)}
			run := runShareMode(t, srv, srv.URL, mode, withLinkRequestsOf(accountKeyMachine(t, srv, &enrolled), requests))
			run.mustHaveDelivered(t, mode)
			if len(requests.asked) != 1 || len(requests.keys) != 0 {
				t.Errorf("made the link request %d times, and sent %d request(s) under a device key; want once, and nothing under a device key", len(requests.asked), len(requests.keys))
			}
			if enrolled {
				t.Error("the machine enrolled although the link request gave the link")
			}
			if got := apiRequests(srv); len(got) != 0 {
				t.Errorf("qURL API requests = %q, want none", got)
			}
		})

		// What --verbose says on this machine when the first request gives
		// the link. The machine has no device state, so the line must not
		// say that it asked as a device. It says that the request went under
		// a random key and that no device key was read. A read of the key
		// fails the test: only the answer "not found" leads to one.
		t.Run("the line with --verbose does not name a device/"+mode.name, func(t *testing.T) {
			srv := downloadServer(t)
			enrolled := false
			requests := &linkRequests{link: issuedLink(srv.URL + apitest.DownloadPath)}
			machine := withDeviceKey(withLinkRequestsOf(accountKeyMachine(t, srv, &enrolled), requests), mustNotReadTheDeviceKey(t))
			run := runShareMode(t, srv, srv.URL, mode, withArgs(machine, "--verbose"))
			run.mustHaveDelivered(t, mode)

			stderr := run.result.stderr.String()
			if want := "[debug] " + msgCRIDLinkKeyNotNeeded + "\n"; strings.Count(stderr, want) != 1 {
				t.Errorf("stderr = %q, want the line %q once", stderr, want)
			}
			if strings.Contains(stderr, msgCRIDLinkAsDevice) || strings.Contains(stderr, "> CRID link request with the CRID alone") {
				t.Errorf("stderr = %q, must not say that the machine asked as a device, or that it looked for a device key", stderr)
			}
			if enrolled {
				t.Error("the machine enrolled although the link request gave the link")
			}
		})

		t.Run("the owner's own private resource/"+mode.name, func(t *testing.T) {
			srv := downloadServer(t)
			enrolled := false
			requests := &linkRequests{err: notFound}
			run := runShareMode(t, srv, srv.URL, mode, withLinkRequestsOf(accountKeyMachine(t, srv, &enrolled), requests))
			run.mustHaveDelivered(t, mode)
			if len(requests.asked) != 1 || len(requests.keys) != 0 {
				t.Errorf("made the link request %d times, and sent %d request(s) under a device key; want once, and nothing under a device key", len(requests.asked), len(requests.keys))
			}
			if !enrolled {
				t.Error("the machine did not enroll under the account")
			}
			if got, want := apiRequests(srv), enrollingShare(srv); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("qURL API requests = %q, want %q", got, want)
			}
		})

		// The order and the cost, with the production read of the device key.
		// The one link request is made before anything is sent to the qURL
		// API, and with the short limit. It is answered "not found", the read
		// then finds no device state, and nothing is sent under a device key.
		// The share request comes after it and enrolls the machine. No second
		// link request is made: the request with only the CRID fails the
		// test.
		t.Run("the link request first, then the share request/"+mode.name, func(t *testing.T) {
			srv := downloadServer(t)
			enrolled := false
			requests := &linkRequests{err: notFound}
			contexts := &requestContexts{}
			var apiSeen [][]string
			configure := func(args []string) *runOpts {
				opts := accountKeyMachine(t, srv, &enrolled)(append(args, "--verbose"))
				opts.requestCRIDLinkAsDevice = func(ctx context.Context, deviceKey qurl.DeviceKeySource, resourceCRID string) (*qurl.CRIDLink, error) {
					contexts.record(ctx)
					apiSeen = append(apiSeen, apiRequests(srv))
					return requests.answerAsDevice(ctx, deviceKey, resourceCRID)
				}
				opts.requestCRIDLink = mustNotAskWithTheCRIDAlone(t)
				return opts
			}
			run := runShareMode(t, srv, srv.URL, mode, configure)
			run.mustHaveDelivered(t, mode)

			if len(requests.keys) != 0 {
				t.Errorf("%d request(s) were sent under a device key, want none: the machine holds no device state", len(requests.keys))
			}
			if len(requests.asked) != 1 || len(apiSeen[0]) != 0 {
				t.Fatalf("made the link request %d times, the first after %q; want once, before anything is sent to the qURL API", len(requests.asked), apiSeen)
			}
			if !contexts.bounded[0] || contexts.limits[0] > cridLinkTimeoutBeforeShare {
				t.Errorf("the link request: time limit set = %t (%s), want a limit of at most %s", contexts.bounded[0], contexts.limits[0], cridLinkTimeoutBeforeShare)
			}
			stderr := run.result.stderr.String()
			if want := "[debug] " + fmt.Sprintf(msgCRIDLinkDeviceKeyNotRead, connectorstate.NoDeviceKeyNoState) + "\n"; strings.Count(stderr, want) != 1 {
				t.Errorf("stderr = %q, want the line %q once: the read of the device key found no device state", stderr, want)
			}
			if strings.Contains(stderr, msgCRIDLinkAsDevice) {
				t.Errorf("stderr = %q, must not say that the machine asked as a device", stderr)
			}
			if !enrolled {
				t.Error("the machine did not enroll under the account")
			}
			if got, want := apiRequests(srv), enrollingShare(srv); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("qURL API requests = %q, want %q", got, want)
			}
		})

		t.Run("a resource this machine may not open/"+mode.name, func(t *testing.T) {
			srv := downloadServer(t)
			shareNotFoundTwice(t, srv)
			enrolled := false
			requests := &linkRequests{err: notFound}
			run := runShareMode(t, srv, srv.URL, mode, withLinkRequestsOf(accountKeyMachine(t, srv, &enrolled), requests))
			if run.result.code != exitcode.NotFound {
				t.Fatalf("exit = %d, want %d; stderr: %s", run.result.code, exitcode.NotFound, run.result.stderr.String())
			}
			run.mustNotHaveActed(t)
			if !enrolled || len(requests.asked) != 1 || len(requests.keys) != 0 {
				t.Errorf("enrolled = %t, made the link request %d times, and sent %d request(s) under a device key; want the machine enrolled, one link request, and nothing under a device key",
					enrolled, len(requests.asked), len(requests.keys))
			}
			if got, want := apiRequests(srv), enrollingShare(srv); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("qURL API requests = %q, want %q", got, want)
			}
		})

		// With a share option: the share request first, as before. The
		// owner's own resource is answered there and nothing else is looked
		// at.
		t.Run("share option, the owner's own resource/"+mode.name, func(t *testing.T) {
			srv := downloadServer(t)
			enrolled := false
			configure := func(args []string) *runOpts {
				opts := withLinkOffer(accountKeyMachine(t, srv, &enrolled), mustNotCheckTheLinkOffer(t))(append(args, "--session-duration", "5m"))
				opts.requestCRIDLink, opts.requestCRIDLinkAsDevice = mustNotAskWithTheCRIDAlone(t), mustNotAskAsTheDevice(t)
				opts.readDeviceKey = mustNotReadTheDeviceKey(t)
				return opts
			}
			run := runShareMode(t, srv, srv.URL, mode, configure)
			run.mustHaveDelivered(t, mode)
			if !enrolled {
				t.Error("the machine did not enroll under the account")
			}
			if got, want := apiRequests(srv), enrollingShare(srv); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("qURL API requests = %q, want %q", got, want)
			}
		})

		t.Run("share option, somebody else's resource/"+mode.name, func(t *testing.T) {
			srv := downloadServer(t)
			shareNotFoundTwice(t, srv)
			enrolled := false
			requests := &linkRequests{link: issuedLink(srv.URL + apitest.DownloadPath)}
			configure := withDeviceKey(withLinkRequestsOf(withArgs(accountKeyMachine(t, srv, &enrolled), "--session-duration", "5m"), requests), mustNotReadTheDeviceKey(t))
			run := runShareMode(t, srv, srv.URL, mode, configure)
			run.mustHaveDelivered(t, mode)
			if !enrolled || len(requests.asked) != 1 || requests.askedAsDevice() != 0 {
				t.Errorf("enrolled = %t, made the link request %d times, %d of them as a device; want the share path first, then one request with the CRID alone",
					enrolled, len(requests.asked), requests.askedAsDevice())
			}
		})
	}

	// The read of the key on this machine is the production one, and it is
	// read-only: the empty state directory is still empty after a run that
	// was answered by the link request.
	t.Run("the state directory stays empty", func(t *testing.T) {
		srv := downloadServer(t)
		enrolled := false
		requests := &linkRequests{link: issuedLink(srv.URL + apitest.DownloadPath)}
		var stateDir string
		configure := func(args []string) *runOpts {
			opts := withLinkRequestsOf(accountKeyMachine(t, srv, &enrolled), requests)(args)
			stateDir = opts.shareStateDir
			return opts
		}
		file := getModes()[1]
		if file.name != "get file" {
			t.Fatalf("second get mode is %q, want get file", file.name)
		}
		runShareMode(t, srv, srv.URL, file, configure).mustHaveDelivered(t, file)
		entries, err := os.ReadDir(stateDir)
		if err != nil || len(entries) != 0 {
			t.Errorf("the state directory holds %d entries (error %v) after a run that the link request answered, want it empty", len(entries), err)
		}
	})
}

// TestGetVerifiesALinkGivenForTheCRIDAlone pins that a link from the link
// request goes through the same check as a share link before anything is
// done with it. A link that fails the check is discarded with exit code 12.
//
// That holds for both forms of the request. A device that asked as itself
// stops at the failed check too: it does not go on to its share request.
func TestGetVerifiesALinkGivenForTheCRIDAlone(t *testing.T) {
	state := bootstrapRegisteredState(t)
	for _, device := range []bool{false, true} {
		for _, mode := range getModes() {
			t.Run(fmt.Sprintf("device=%t/%s", device, mode.name), func(t *testing.T) {
				srv, link := portalServer(t)
				requests := &linkRequests{link: issuedLink(link)}
				stateDir := filepath.Join(t.TempDir(), "no-device-state")
				machine := machineWithNoIdentity(t, stateDir)
				if device {
					machine = withDeviceKey(enrolledDevice(t, state), deviceKeyOf(t, state).read)
				}
				configure := func(args []string) *runOpts {
					opts := withLinkRequestsOf(machine, requests)(args)
					opts.verifyLink = func(context.Context, string, string) error { return consume.ErrLinkVerification }
					opts.enterPortalGrant = func(context.Context, string) (consume.AccessGrant, error) {
						t.Error("access was requested for a link that failed its check")
						return consume.AccessGrant{}, errors.New("unexpected access request")
					}
					return opts
				}

				run := runShareMode(t, srv, srv.URL, mode, configure)
				if run.result.code != exitcode.VerificationFailed {
					t.Fatalf("exit = %d, want %d; stderr: %s", run.result.code, exitcode.VerificationFailed, run.result.stderr.String())
				}
				run.mustNotHaveActed(t)
				if !strings.Contains(run.result.stderr.String(), consume.MsgLinkVerification) {
					t.Errorf("stderr = %q, want the existing discard message", run.result.stderr.String())
				}
				mustNeverFetchPortalPage(t, srv)
				wantAsDevice := 0
				if device {
					wantAsDevice = 1
				}
				if len(requests.asked) != 1 || requests.askedAsDevice() != wantAsDevice {
					t.Errorf("made the link request %d times, %d of them as this device; want once, and %d as this device", len(requests.asked), requests.askedAsDevice(), wantAsDevice)
				}
				if got := apiRequests(srv); len(got) != 0 {
					t.Errorf("qURL API requests = %q, want none: a link that fails its check ends the run", got)
				}
			})
		}
	}
}

// TestGetRefreshesALinkGivenForTheCRIDAlone covers a link given for the CRID
// alone that expires before any byte is served, in a run that the share
// request cannot give a link. The download asks again the same way: the
// third row of the refresh table in get_crid_link.go. The service gets one
// more request with the CRID alone and nothing else.
//
// The run has a share option, --session-duration, so a device with an
// identity shares first. It sends its share request once, for the first
// link, and gets "not found". It does not send it again at the refresh,
// although the share route here would answer "not found" again and so lead
// to the same link. A machine with no identity still sends nothing to the
// qURL API. Neither asks a second time whether the request is offered, and
// neither reads a device key.
//
// The reader is told about the publisher once, and once that
// --session-duration was not applied.
//
// TestGetRefreshIsDecidedAgainAfterALinkFromTheLinkRequest has the refresh
// of a device that made the link request first.
func TestGetRefreshesALinkGivenForTheCRIDAlone(t *testing.T) {
	state := bootstrapRegisteredState(t)
	for _, device := range []bool{true, false} {
		for _, mode := range getModes() {
			if !mode.downloads {
				continue
			}
			t.Run(fmt.Sprintf("device=%t/%s", device, mode.name), func(t *testing.T) {
				srv := downloadServer(t)
				srv.Script(http.MethodGet, apitest.DownloadPath, handlerGone)
				requests := &linkRequests{link: issuedLink(srv.URL + apitest.DownloadPath)}
				offer := &linkOffer{offered: true}
				stateDir := filepath.Join(t.TempDir(), "no-device-state")
				machine := machineWithNoIdentity(t, stateDir)
				wantAPI := []string(nil)
				if device {
					machine = enrolledDevice(t, state)
					shareNotFoundTwice(t, srv)
					// One share request, for the first link. The device proves
					// its identity once per process.
					wantAPI = []string{"GET /v1/me", "POST " + shareRoute(srv)}
				}

				configure := func(args []string) *runOpts {
					opts := withLinkOffer(withLinkRequestsOf(machine, requests), offer.answer)(append(args, "--session-duration", "5m"))
					opts.readDeviceKey = mustNotReadTheDeviceKey(t)
					return opts
				}
				run := runShareMode(t, srv, srv.URL, mode, configure)
				run.mustHaveDelivered(t, mode)

				if len(requests.asked) != 2 || requests.askedAsDevice() != 0 {
					t.Errorf("made the link request %d times, %d of them as this device; want twice with the CRID alone: once, and once for the refresh",
						len(requests.asked), requests.askedAsDevice())
				}
				if got := apiRequests(srv); strings.Join(got, "\n") != strings.Join(wantAPI, "\n") {
					t.Errorf("qURL API requests = %q, want %q: the refresh sends no share request", got, wantAPI)
				}
				if offer.checks != 1 {
					t.Errorf("asked whether the request is offered %d times, want once, for the first link", offer.checks)
				}
				stderr := run.result.stderr.String()
				if strings.Count(stderr, "UNVERIFIED publisher") != 1 {
					t.Errorf("stderr = %q, want the publisher notice once", stderr)
				}
				if strings.Count(stderr, msgSessionDurationNotApplied) != 1 {
					t.Errorf("stderr = %q, want the session-duration note once", stderr)
				}
				if !device {
					mustNotExistCmd(t, stateDir)
				}
			})
		}
	}
}

// TestGetRefreshStaysWithTheCRIDAlone pins the third row of the refresh
// table in get_crid_link.go for the case it was written for. The first link
// of a download was given for the CRID alone, after the share request said
// "not found". The link expires before any byte is served, and the download
// asks again.
//
// A device gets into that row only when it shares first, so every run of a
// device here has a share option, --session-duration. A machine with no
// identity is in that row with or without one.
//
// Here the share route fails if it is asked a second time, with an answer
// that is not "not found": the service is unavailable. When get decided
// again at every refresh where its link comes from, it sent the share
// request again, and that failure ended a download which links for the CRID
// alone were serving. Now the run remembers where its link came from. The
// download completes, the share route is asked exactly once, and the reader
// is still told once that --session-duration was not applied.
//
// The answer at the refresh is final, whatever it is: a refusal is reported
// as that refusal, with the hint that fits the machine, and no share request
// follows it either.
func TestGetRefreshStaysWithTheCRIDAlone(t *testing.T) {
	state := bootstrapRegisteredState(t)
	// shareNotFoundThenUnavailable answers the first share request "not
	// found" and every later one "unavailable". The later answers are queued
	// more than once, so a request that is sent again is counted too.
	shareNotFoundThenUnavailable := func(t *testing.T, srv *apitest.Server) {
		t.Helper()
		srv.Script(http.MethodPost, shareRoute(srv), apitest.HandlerNotFound404(t, "resource_not_found"))
		srv.ScriptRepeat(http.MethodPost, shareRoute(srv), 3, apitest.HandlerDark503(t))
	}
	downloads := func() []shareMode {
		var modes []shareMode
		for _, mode := range getModes() {
			if mode.downloads {
				modes = append(modes, mode)
			}
		}
		return modes
	}

	// With no mode that downloads, both loops below would run nothing and
	// this test would pass while it holds nothing.
	if len(downloads()) == 0 {
		t.Fatal("no get mode downloads; this test would pin nothing")
	}

	for _, mode := range downloads() {
		t.Run("the share request would fail/"+mode.name, func(t *testing.T) {
			srv := downloadServer(t)
			srv.Script(http.MethodGet, apitest.DownloadPath, handlerGone)
			shareNotFoundThenUnavailable(t, srv)
			requests := &linkRequests{link: issuedLink(srv.URL + apitest.DownloadPath)}

			configure := func(args []string) *runOpts {
				return withLinkRequests(enrolledDevice(t, state), requests.answer)(append(args, "--session-duration", "5m"))
			}
			run := runShareMode(t, srv, srv.URL, mode, configure)
			run.mustHaveDelivered(t, mode)

			if got := len(shareRequests(srv)); got != 1 {
				t.Errorf("the share request was sent %d times, want once: the refresh must not send it again", got)
			}
			if len(requests.asked) != 2 {
				t.Errorf("asked with the CRID alone %d times, want twice: once, and once for the refresh", len(requests.asked))
			}
			stderr := run.result.stderr.String()
			if strings.Count(stderr, msgSessionDurationNotApplied) != 1 {
				t.Errorf("stderr = %q, want the session-duration note once", stderr)
			}
			if strings.Count(stderr, "UNVERIFIED publisher") != 1 {
				t.Errorf("stderr = %q, want the publisher notice once", stderr)
			}
		})
	}

	for _, tc := range []struct {
		name string
		// refusal is the answer to the request at the refresh.
		refusal  error
		wantCode int
		// golden and goldenNoDevice name the stderr golden the output must
		// end with, as in answerRow.
		golden, goldenNoDevice string
	}{
		{
			name: "unavailable", refusal: sdkRefusal(qurl.ErrCRIDLinkUnavailable, "52601"),
			wantCode: exitcode.Unavailable, golden: "error_get_crid_unavailable",
		},
		{
			name: "not found", refusal: sdkRefusal(qurl.ErrCRIDLinkNotFound, "52602"),
			wantCode: exitcode.NotFound, golden: "error_get_crid_notfound", goldenNoDevice: "error_get_crid_notfound_no_device",
		},
		{
			// Not an answer the SDK gives at a refresh: it asked for this
			// same CRID a moment earlier. If it ever does, the reader gets
			// the message for a CRID this client cannot ask for, and not an
			// internal error.
			name: "CRID this client cannot ask for", refusal: errCRIDVersionTheSDKCannotCheck,
			wantCode: exitcode.Config, golden: "error_get_crid_version",
		},
	} {
		for _, device := range []bool{true, false} {
			for _, mode := range downloads() {
				t.Run(fmt.Sprintf("the refresh is refused/%s/device=%t/%s", tc.name, device, mode.name), func(t *testing.T) {
					srv := downloadServer(t)
					srv.Script(http.MethodGet, apitest.DownloadPath, handlerGone)
					stateDir := filepath.Join(t.TempDir(), "no-device-state")
					machine := machineWithNoIdentity(t, stateDir)
					wantShares, golden := 0, tc.golden
					if device {
						machine = withDeviceKey(withArgs(enrolledDevice(t, state), "--session-duration", "5m"), mustNotReadTheDeviceKey(t))
						shareNotFoundThenUnavailable(t, srv)
						wantShares = 1
					} else if tc.goldenNoDevice != "" {
						golden = tc.goldenNoDevice
					}
					first := &linkRequests{link: issuedLink(srv.URL + apitest.DownloadPath)}
					asks := 0
					answer := func(ctx context.Context, resourceCRID string) (*qurl.CRIDLink, error) {
						asks++
						if asks == 1 {
							return first.answer(ctx, resourceCRID)
						}
						return nil, tc.refusal
					}

					run := runShareMode(t, srv, srv.URL, mode, withLinkRequests(machine, answer))
					stderr := run.result.stderr.String()
					if run.result.code != tc.wantCode {
						t.Fatalf("exit = %d, want %d; stderr: %s", run.result.code, tc.wantCode, stderr)
					}
					run.mustNotHaveActed(t)
					if want := goldenBytes(t, golden+".plain.stderr.golden"); !strings.HasSuffix(stderr, want) {
						t.Errorf("stderr = %q, want it to end with the golden %q", stderr, want)
					}
					if strings.Contains(stderr, errCRIDNotRequestable.Error()) {
						t.Errorf("stderr %q carries an error that must not leave get_crid_link.go", stderr)
					}
					if asks != 2 {
						t.Errorf("asked with the CRID alone %d times, want twice and no retry", asks)
					}
					if got := len(shareRequests(srv)); got != wantShares {
						t.Errorf("the share request was sent %d times, want %d: a refusal at the refresh is final", got, wantShares)
					}
					if !device {
						mustNotExistCmd(t, stateDir)
					}
				})
			}
		}
	}
}

// TestGetRefreshCanChangeToALinkGivenForTheCRIDAlone covers one download
// whose two links come from different paths. The share request gives the
// first link. That link expires before any byte is served, and the download
// asks again. This time the share request says "not found", so the second
// link is given for the CRID alone.
//
// It is the only way the session-duration note can appear in the middle of a
// download. The note is printed once, when the path changes, and so after the
// publisher notice of the first link. The publisher is still announced once:
// the second link is for the same resource.
func TestGetRefreshCanChangeToALinkGivenForTheCRIDAlone(t *testing.T) {
	state := bootstrapRegisteredState(t)
	for _, mode := range getModes() {
		if !mode.downloads {
			continue
		}
		t.Run(mode.name, func(t *testing.T) {
			srv := downloadServer(t)
			var durations []any
			recordDuration := func(next http.HandlerFunc) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode share request: %v", err)
					}
					durations = append(durations, body["session_duration"])
					next(w, r)
				}
			}
			srv.Script(http.MethodPost, shareRoute(srv),
				recordDuration(shareAnswerWithPublisher(t, srv, map[string]any{"name": apitest.DefaultPublisherName, "verified": false})),
				recordDuration(apitest.HandlerNotFound404(t, "resource_not_found")))
			srv.Script(http.MethodGet, apitest.DownloadPath, handlerGone)
			requests := &linkRequests{link: issuedLink(srv.URL + apitest.DownloadPath)}

			configure := func(args []string) *runOpts {
				return withLinkRequests(enrolledDevice(t, state), requests.answer)(append(args, "--session-duration", "5m"))
			}
			run := runShareMode(t, srv, srv.URL, mode, configure)
			run.mustHaveDelivered(t, mode)

			// Both share requests carry the flag. Only the second link could
			// not take it.
			if len(durations) != 2 || durations[0] != "5m" || durations[1] != "5m" {
				t.Errorf("session durations of the share requests = %v, want the flag on both", durations)
			}
			if len(requests.asked) != 1 {
				t.Errorf("asked with the CRID alone %d times, want once, for the refresh", len(requests.asked))
			}
			stderr := run.result.stderr.String()
			publisher, note := strings.Index(stderr, "UNVERIFIED publisher"), strings.Index(stderr, msgSessionDurationNotApplied)
			if strings.Count(stderr, "UNVERIFIED publisher") != 1 || strings.Count(stderr, msgSessionDurationNotApplied) != 1 || note < publisher {
				t.Errorf("stderr = %q, want the publisher notice once and, after it, the session-duration note once", stderr)
			}
		})
	}
}

// TestGetSessionDurationWithALinkGivenForTheCRIDAlone pins what happens to
// --session-duration. It is a share option, and only the share request can
// carry it. So with the flag a device with an identity shares first, as
// before, and does not read its device key. A link given for the CRID alone
// cannot carry the flag, so get says the flag was not applied and uses the
// link. On the share path the note never appears.
//
// Without the flag the device makes the link request first, and there is no
// note: nothing was asked for that the link could not carry.
func TestGetSessionDurationWithALinkGivenForTheCRIDAlone(t *testing.T) {
	state := bootstrapRegisteredState(t)
	// withTheFlag is the device with the flag, a read of the device key that
	// fails the test, and a request as this device that fails it too.
	withTheFlag := func(t *testing.T, answer func(context.Context, string) (*qurl.CRIDLink, error)) func(args []string) *runOpts {
		return func(args []string) *runOpts {
			opts := enrolledDevice(t, state)(append(args, "--session-duration", "5m"))
			opts.requestCRIDLink, opts.requestCRIDLinkAsDevice = answer, mustNotAskAsTheDevice(t)
			opts.readDeviceKey = mustNotReadTheDeviceKey(t)
			return opts
		}
	}
	for _, mode := range getModes() {
		t.Run("share path/"+mode.name, func(t *testing.T) {
			srv := downloadServer(t)
			var shareBody map[string]any
			srv.Script(http.MethodPost, shareRoute(srv), func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&shareBody); err != nil {
					t.Errorf("decode share request: %v", err)
				}
				shareAnswerWithPublisher(t, srv, map[string]any{"name": apitest.DefaultPublisherName, "verified": false})(w, r)
			})
			run := runShareMode(t, srv, srv.URL, mode, withTheFlag(t, mustNotAskWithTheCRIDAlone(t)))
			run.mustHaveDelivered(t, mode)
			if shareBody["session_duration"] != "5m" {
				t.Errorf("share request body = %v, want the requested session duration", shareBody)
			}
			if strings.Contains(run.result.stderr.String(), msgSessionDurationNotApplied) {
				t.Errorf("stderr = %q, must not carry the note on the share path", run.result.stderr.String())
			}
		})

		t.Run("CRID alone/"+mode.name, func(t *testing.T) {
			srv := downloadServer(t)
			var shareBody map[string]any
			srv.Script(http.MethodPost, shareRoute(srv), func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&shareBody); err != nil {
					t.Errorf("decode share request: %v", err)
				}
				apitest.HandlerNotFound404(t, "resource_not_found")(w, r)
			})
			requests := &linkRequests{link: issuedLink(srv.URL + apitest.DownloadPath)}
			// The share request must have been sent when the link request is
			// made: the flag keeps the share request first.
			sharesBefore := -1
			answer := func(ctx context.Context, resourceCRID string) (*qurl.CRIDLink, error) {
				sharesBefore = len(shareRequests(srv))
				return requests.answer(ctx, resourceCRID)
			}
			run := runShareMode(t, srv, srv.URL, mode, withTheFlag(t, answer))
			run.mustHaveDelivered(t, mode)
			if shareBody["session_duration"] != "5m" {
				t.Errorf("share request body = %v, want the requested session duration on the share path", shareBody)
			}
			if len(requests.asked) != 1 || sharesBefore != 1 {
				t.Errorf("made the link request %d times, after %d share request(s); want once, after the one share request", len(requests.asked), sharesBefore)
			}
			if strings.Count(run.result.stderr.String(), msgSessionDurationNotApplied) != 1 {
				t.Errorf("stderr = %q, want the note once", run.result.stderr.String())
			}
		})

		t.Run("link request first, no flag/"+mode.name, func(t *testing.T) {
			srv := downloadServer(t)
			requests := &linkRequests{link: issuedLink(srv.URL + apitest.DownloadPath)}
			run := runShareMode(t, srv, srv.URL, mode, withLinkRequests(enrolledDevice(t, state), requests.answer))
			run.mustHaveDelivered(t, mode)
			if strings.Contains(run.result.stderr.String(), msgSessionDurationNotApplied) {
				t.Errorf("stderr = %q, must not carry the note when the flag was not given", run.result.stderr.String())
			}
			if got := shareRequests(srv); len(got) != 0 {
				t.Errorf("the share request was sent %d times, want none: the link request gave the link", len(got))
			}
		})
	}
}

// TestHasDeviceIdentity pins the local check that picks the starting state.
// It reads the environment and two file names, and creates nothing.
func TestHasDeviceIdentity(t *testing.T) {
	withFile := func(name string) func(t *testing.T) string {
		return func(t *testing.T) string {
			dir := connectorStateTestDir(t)
			if name != "" {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			return dir
		}
	}
	absent := func(t *testing.T) string { return filepath.Join(t.TempDir(), "never-created") }

	for _, tc := range []struct {
		name     string
		env      map[string]string
		stateDir func(t *testing.T) string
		dirErr   error
		want     bool
	}{
		{name: "plaintext device state", stateDir: withFile(connectorstate.AgentStateFile), want: true},
		{name: "sealed device state", stateDir: withFile(connectoragentstate.SealedAgentStateFile), want: true},
		{name: "empty state directory", stateDir: withFile(""), want: false},
		{name: "other files only", stateDir: withFile(connectorstate.RuntimeModeFile), want: false},
		{name: "no state directory yet", stateDir: absent, want: false},
		{name: "host has no state directory", stateDir: absent, dirErr: fmt.Errorf("%w: set %s", connectorstate.ErrNoDefaultStateDir, connectorstate.EnvStateDirPrimary), want: false},
		// Anything else is a doubt, and a doubt keeps the share path.
		{name: "state directory cannot be resolved", stateDir: absent, dirErr: errors.New("resolve failed"), want: true},
		{name: "account key", env: map[string]string{"QURL_API_KEY": testAPIKey}, stateDir: absent, want: true},
		{name: "account key file", env: map[string]string{"QURL_API_KEY_FILE": "/does/not/matter"}, stateDir: absent, want: true},
		{name: "blank account key", env: map[string]string{"QURL_API_KEY": "  "}, stateDir: absent, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.stateDir(t)
			opts := &globalOpts{
				lookupEnv:            func(key string) (string, bool) { v, ok := tc.env[key]; return v, ok },
				resolveShareStateDir: func(string) (string, error) { return dir, tc.dirErr },
			}
			if got := opts.hasDeviceIdentity(); got != tc.want {
				t.Errorf("hasDeviceIdentity() = %t, want %t", got, tc.want)
			}
			if tc.name == "no state directory yet" {
				mustNotExistCmd(t, dir)
			}
		})
	}
}

// TestShareLinkFromCRIDLink pins the copy from the SDK's result into the
// CLI's share result: the fields a share link has, taken one by one, and
// nothing else.
func TestShareLinkFromCRIDLink(t *testing.T) {
	createdAt := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	expiresAt := time.Date(2026, 3, 2, 0, 5, 0, 0, time.UTC)
	issued := &qurl.CRIDLink{
		Link: "https://qurl.link/#qv2t1.1.1.1.AQ.AQ.AQ", QURLID: "q_issued0001",
		ExpiresAt: expiresAt, ResourceCreatedAt: &createdAt,
		Publisher: qurl.Publisher{Name: "Acme Docs", Verified: true},
	}

	got := shareLinkFromCRIDLink(exampleCRID, issued)
	want := qurlapi.ShareLink{
		QURL: issued.Link, CRID: exampleCRID, ExpiresAt: expiresAt,
		ResourceCreatedAt: &createdAt, Publisher: qurlapi.Publisher{Name: "Acme Docs", Verified: true},
	}
	if got.ResourceCreatedAt == nil || !got.ResourceCreatedAt.Equal(createdAt) || got.ResourceCreatedAt == issued.ResourceCreatedAt {
		t.Fatalf("resource creation date = %v, want a copy of %v", got.ResourceCreatedAt, createdAt)
	}
	got.ResourceCreatedAt = want.ResourceCreatedAt
	if *got != want {
		t.Errorf("share link = %+v, want %+v", *got, want)
	}

	// An unset creation date stays absent, as it does on the share path.
	for name, unset := range map[string]*time.Time{"nil": nil, "zero": {}} {
		issued.ResourceCreatedAt = unset
		if got := shareLinkFromCRIDLink(exampleCRID, issued); got.ResourceCreatedAt != nil {
			t.Errorf("%s creation date became %v, want absent", name, got.ResourceCreatedAt)
		}
	}
}
