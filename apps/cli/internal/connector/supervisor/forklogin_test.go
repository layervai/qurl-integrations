package supervisor

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	frpclient "github.com/fatedier/frp/client"
	"github.com/fatedier/frp/pkg/config/source"
	v1 "github.com/fatedier/frp/pkg/config/v1"
	"github.com/fatedier/frp/pkg/policy/security"
	frpserver "github.com/fatedier/frp/server"
)

// This file is the empirical half of the TODO(upstream-contract) marker on
// classifyRunError: it drives the REAL pinned FRP client to a REAL failed
// Login and asserts against the error the fork actually returns, rather than
// against a string this package hand-builds.
//
// The distinction is the whole point. contract_test.go's frpLoginWrap is a
// local constant fed to IsTokenLoginError, so every assertion over it holds
// just as well when the fork has stopped emitting that text — the constant is
// both the input and the specification. classifyRunError's "login to the
// server failed" case had the same shape and no observation at all behind it.
// A reword upstream would have changed nothing local: no compile error, no
// test failure, just every Login-stage failure quietly rebucketed as
// frp_runtime_error on the operator dashboards. These tests are what fails
// instead.

// forkLoginRejectPluginPath is the path the login-rejecting server plugin is
// mounted at. The fork posts every op to this one endpoint and distinguishes
// them by the "op" field in the body, which is why the handler filters on it.
const forkLoginRejectPluginPath = "/"

// refusingLoopbackPort returns a loopback TCP port that is verified CLOSED at
// the moment it is returned.
//
// reserve-then-release (reserveHermeticTCPPort) is not enough on its own here:
// the whole point of this port is that connecting to it is REFUSED, and a
// parallel test in this package that reserved the same freed port between the
// release and the dial would turn the refusal into a connect. That failure
// would be confusing rather than loud — the fork would fail Login against a
// non-FRP peer and still wrap it, so only the dial-substring half of the
// ordering assertion would break, and it would break as a puzzling message
// about a port that was supposed to be dead.
//
// So the refusal is confirmed by probing it, and a lost race just takes
// another port. The bound is small because the retry is only for that race:
// if loopback refuses nothing at all, something is wrong that a longer loop
// would only turn into a slower failure.
func refusingLoopbackPort(t *testing.T) int {
	t.Helper()
	const attempts = 8
	dialer := &net.Dialer{Timeout: forkDialAcceptWait}
	for range attempts {
		port := reserveHermeticTCPPort(t)
		conn, err := dialer.DialContext(t.Context(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			return port // refused (or unreachable): exactly what this port is for
		}
		_ = conn.Close() // someone took it between release and probe; try again
	}
	t.Fatalf("no reserved loopback port stayed closed across %d attempts; the refusal this test needs is unobtainable here", attempts)
	return 0
}

// startLoginRejectingFRPS runs the FRP fork's real server on the given
// loopback port with an HTTP server plugin that REJECTS the Login op with the
// given reason.
//
// This is the only way to make the real server produce a real RejectReason
// without standing up the tunnel server: the fork's plugin manager runs
// registered Login plugins inside the server's login handler, and a rejection
// there becomes LoginResp.Error on the wire, which the client turns back into
// an error and Run then wraps. That is the identical path a knock-token
// denial from qurl-service takes, so a reason put in here comes back out
// through every layer the production classifier sits behind.
func startLoginRejectingFRPS(t *testing.T, bindPort int, rejectReason string) {
	t.Helper()
	plugin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Op string `json:"op"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Anything other than Login passes through untouched: rejecting an op
		// this test did not register for would be a silent way to fail the
		// session somewhere other than where the assertion looks.
		response := map[string]any{"reject": false, "unchange": true}
		if request.Op == "Login" {
			response = map[string]any{"reject": true, "reject_reason": rejectReason}
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	t.Cleanup(plugin.Close)

	cfg := &v1.ServerConfig{
		BindAddr:      "127.0.0.1",
		BindPort:      bindPort,
		ProxyBindAddr: "127.0.0.1",
		HTTPPlugins: []v1.HTTPPluginOptions{{
			Name: "login-rejecter",
			Addr: plugin.URL,
			Path: forkLoginRejectPluginPath,
			Ops:  []string{"Login"},
		}},
	}
	if err := cfg.Complete(); err != nil {
		t.Fatalf("complete the login-rejecting server config: %v", err)
	}
	svc, err := frpserver.NewService(cfg)
	if err != nil {
		t.Fatalf("construct the login-rejecting server on 127.0.0.1:%d: %v", bindPort, err)
	}
	go svc.Run(context.Background())
	t.Cleanup(func() { _ = svc.Close() })
}

// runForkClientLoginFailure builds the REAL fork client against the given
// loopback port and returns the error its Run reports, which the caller
// asserts against.
//
// LoginFailExit is forced true because that is what production does: the
// supervisor overwrites it on every per-cycle config clone
// (forceLoginFailExit, pinned by TestKnockForcesLoginFailExit), so the fork
// branch these tests observe is the branch the connector actually runs. It
// also makes the test fast and bounded — the fork cancels itself on the first
// failed Login instead of backing off toward its retry ceiling.
//
// The knocking connector overlay is deliberately NOT wired in. It fails
// closed on IP-literal dial targets with no explicit SNI (see the hermetic
// round-trip's resourceHost note), and it sits below the seam under test:
// what these tests observe is how Run reports a failed Login, which is the
// same whatever produced the failure.
func runForkClientLoginFailure(t *testing.T, serverPort int) error {
	t.Helper()
	common := forkDialCommon(t, "127.0.0.1", serverPort)
	failFast := true
	common.LoginFailExit = &failFast
	common.Log.Level = "error"

	cfgSource := source.NewConfigSource()
	if err := cfgSource.ReplaceAll(nil, nil); err != nil {
		t.Fatalf("load an empty proxy set: %v", err)
	}
	svc, err := frpclient.NewService(frpclient.ServiceOptions{
		Common:                 common,
		ConfigSourceAggregator: source.NewAggregator(cfgSource),
		UnsafeFeatures:         &security.UnsafeFeatures{},
	})
	if err != nil {
		t.Fatalf("construct the fork client: %v", err)
	}
	t.Cleanup(svc.Close)
	runErr := svc.Run(t.Context())
	if runErr == nil {
		t.Fatal("the fork client's Run returned nil against a server that refuses every Login; " +
			"the failure these tests classify never happened")
	}
	return runErr
}

// TestForkEmitsTheLoginFailurePrefixThisPackageMatches is the observation
// behind classifyRunError's "login to the server failed" case and behind
// contract_test.go's frpLoginWrap: a real server-side Login rejection comes
// back out of the real client carrying that exact prefix.
//
// The reasons are the contract snapshot's own wire texts rather than a
// literal picked here, which is what makes this more than a fork test. The
// snapshot's client_needles are checked against its login_reject_wire_texts
// by TestQRTSKnockTokenLoginContract, but only as strings compared to other
// strings in the same file. Sending each wire text through a real frps and a
// real frpc closes that loop: the reason the server rejects with is the
// reason IsTokenLoginError is asked about, with the fork's own wrapping in
// between rather than a constant standing in for it.
func TestForkEmitsTheLoginFailurePrefixThisPackageMatches(t *testing.T) {
	t.Parallel()

	contract := decodeKnockTokenLoginContract(t, "CLI snapshot", qrtsKnockTokenLoginContractJSON)
	if len(contract.LoginRejectWireTexts) == 0 {
		t.Fatal("the contract snapshot lists no login_reject_wire_texts; there is nothing to reject with")
	}

	for _, wireText := range contract.LoginRejectWireTexts {
		t.Run(wireText, func(t *testing.T) {
			t.Parallel()
			port := reserveHermeticTCPPort(t)
			startLoginRejectingFRPS(t, port, wireText)

			runErr := runForkClientLoginFailure(t, port)

			// The mirrored literal, observed rather than assumed. HasPrefix
			// rather than Contains: the fork puts the wrap at the front, and
			// a Contains here would still pass if the text survived only as
			// some inner fragment of a differently-shaped message.
			if !strings.HasPrefix(runErr.Error(), frpLoginWrap) {
				t.Errorf("the fork reported %q, which does not start with %q;\n"+
					"the fork reworded its failed-Login wrap, so classifyRunError's login_failed case and "+
					"contract_test.go's frpLoginWrap now both describe a string nothing emits — "+
					"update them together with the marker's version in loginerror.go",
					runErr, frpLoginWrap)
			}

			// The bucketing that reword would have silently changed.
			if got := classifyRunError(runErr); got != "login_failed" {
				t.Errorf("classifyRunError(%q) = %q, want login_failed", runErr, got)
			}

			// Establish that the rejection is what failed this Login before
			// blaming the matcher for not recognizing it. Losing the
			// reserve/release race would leave nothing listening on the port,
			// and a refused dial still carries the wrap and still buckets
			// login_failed — so it clears both assertions above and lands on
			// the one below, which would report a classifier that never saw
			// the wire text as a classifier that ignored it. Failing here
			// instead keeps the diagnosis pointed at the port.
			if !strings.Contains(runErr.Error(), wireText) {
				t.Fatalf("the fork reported %q, which does not carry the reject reason %q; "+
					"the rejecting server never answered on this port, so the matcher assertion below "+
					"would be reporting a failure it did not cause", runErr, wireText)
			}

			// And the matcher, over a reject reason that reached it through
			// the real server and the real client instead of a local concat.
			if !IsTokenLoginError(runErr) {
				t.Errorf("IsTokenLoginError(%q) = false; the server rejected with a contract wire text and "+
					"the classifier did not recognize it, so a real knock-token denial would not count "+
					"against the unhealthy-knock budget", runErr)
			}
		})
	}
}

// TestForkLoginWrapOutranksTheDialSubstrings pins the ORDER of the substring
// switch in classifyRunError against a real error, not a composed one.
//
// The fork's most common Login failure wraps a dial error, so the message
// satisfies two of the switch's cases at once — the comment above the switch
// says as much and says the Login stage should win. Nothing observed that.
// The existing table in runner_test.go feeds hand-written strings, which pin
// the switch's behavior on those strings but cannot show that a real error
// ever contains both, and it is the real error's shape that makes the order
// load-bearing: reorder the cases and this bucket flips to dial_error, which
// is exactly the detail an operator uses to tell "the tunnel server is
// unreachable" from "the tunnel server refused us".
func TestForkLoginWrapOutranksTheDialSubstrings(t *testing.T) {
	t.Parallel()

	runErr := runForkClientLoginFailure(t, refusingLoopbackPort(t))
	msg := strings.ToLower(runErr.Error())

	// Both halves must really be present, or the ordering is not under test
	// and a green result here would mean nothing. "dial tcp" is Go's own
	// net.OpError prefix rather than the platform's refusal wording, so it is
	// the stable half to assert on.
	if !strings.HasPrefix(runErr.Error(), frpLoginWrap) {
		t.Fatalf("the fork reported %q, which does not start with %q; see the sibling test", runErr, frpLoginWrap)
	}
	if !strings.Contains(msg, "dial tcp") {
		t.Fatalf("the fork reported %q, which carries no dial substring; this test no longer exercises the "+
			"two-cases-match ordering it exists for", runErr)
	}

	if got := classifyRunError(runErr); got != "login_failed" {
		t.Errorf("classifyRunError(%q) = %q, want login_failed: a real refused Login matches BOTH the "+
			"login wrap and the dial substrings, and the switch must bucket it by the Login stage that "+
			"surfaced it — %q is what the reversed order reports", runErr, got, reasonDialError)
	}

	// A refused dial is not a token rejection; the needle set must stay
	// narrow enough to say so.
	if IsTokenLoginError(runErr) {
		t.Errorf("IsTokenLoginError(%q) = true for an unreachable server; the needle set has widened to "+
			"catch transport failures and would spend the unhealthy-knock budget on them", runErr)
	}
}
