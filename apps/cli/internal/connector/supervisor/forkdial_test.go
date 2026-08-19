package supervisor

import (
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	frpclient "github.com/fatedier/frp/client"
	"github.com/fatedier/frp/pkg/config/source"
	v1 "github.com/fatedier/frp/pkg/config/v1"
	"github.com/fatedier/frp/pkg/policy/security"
)

// The two waits below fail in OPPOSITE directions, which is what should
// govern anyone retuning them under CI pressure:
//
//   - forkDialAcceptWait too short = a false FAILURE. A slow runner misses a
//     dial that did happen and reddens the build. Keep it generous.
//   - forkDialQuietWait too short = a false PASS. A dial that should not have
//     happened arrives after the window and goes unseen, silently weakening
//     the assertion rather than breaking anything.
//
// So shrink the first only with evidence, and lengthen the second freely —
// the only cost of a longer quiet window is wall-clock.

// forkDialAcceptWait bounds how long a dial-detecting assertion waits. The
// listener is on loopback and the dial is a bare TCP connect, so a real dial
// lands in microseconds; this is only the ceiling before a missing dial is
// called missing.
const forkDialAcceptWait = 2 * time.Second

// forkDialQuietWait is how long "no dial happened" is observed for. A dial
// this test expects NOT to see would have to be slower than every dial it
// does see, on the same loopback listener, to slip through.
const forkDialQuietWait = 250 * time.Millisecond

// dialProbe is a loopback listener that signals each accepted connection.
type dialProbe struct {
	addr     string
	port     int
	accepted chan struct{}

	mu     sync.Mutex
	conns  []net.Conn
	closed bool
}

// newDialProbe starts the listener and signals every accepted connection.
//
// Teardown is registered ONCE, before the accept goroutine starts, and owns
// both the listener and every connection accepted through it. Keep it that
// way. Registering per-connection cleanups from inside the accept loop works
// only under the LIFO ordering that happens to close the listener last, and
// the failure when that ordering changes is silent rather than loud:
// testing.(*common).Cleanup appends to a slice with no completed-test check
// (it panics only inside a fuzz target), so a registration that lands after
// runCleanup has drained is simply never run and the connection leaks to the
// end of the process.
func newDialProbe(t *testing.T) *dialProbe {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on loopback: %v", err)
	}
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		t.Fatalf("loopback listener address is %T, want *net.TCPAddr", ln.Addr())
	}
	p := &dialProbe{addr: "127.0.0.1", port: tcpAddr.Port, accepted: make(chan struct{}, 8)}
	t.Cleanup(func() {
		// Close the listener first: that is what unblocks Accept and ends
		// the goroutine. closed then makes any connection it accepted in the
		// same instant close itself rather than outlive the test.
		_ = ln.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		p.closed = true
		for _, c := range p.conns {
			_ = c.Close()
		}
		p.conns = nil
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed by cleanup
			}
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				_ = conn.Close()
				return
			}
			p.conns = append(p.conns, conn)
			p.mu.Unlock()
			// Non-blocking: a full buffer must not wedge this goroutine and
			// leak it past the test, and a dial beyond the buffer is already
			// far more than any case here makes.
			select {
			case p.accepted <- struct{}{}:
			default:
			}
		}
	}()
	return p
}

// awaitDial fails unless a connection arrives; quiet fails if one does.
func (p *dialProbe) awaitDial(t *testing.T, what string) {
	t.Helper()
	select {
	case <-p.accepted:
	case <-time.After(forkDialAcceptWait):
		t.Fatalf("%s: no connection reached the listener within %s", what, forkDialAcceptWait)
	}
}

func (p *dialProbe) quiet(t *testing.T, what string) {
	t.Helper()
	select {
	case <-p.accepted:
		t.Fatalf("%s: a connection reached the listener, but this seam must not dial", what)
	case <-time.After(forkDialQuietWait):
	}
}

// forkDialCommon returns a production-shaped common config aimed at the
// probe, Complete()d so every field except the one under test carries its
// real default, then pinned to the two defaults that would otherwise send the
// dial somewhere other than the probe:
//
//   - TLS off. The fork's realConnect would start a handshake the bare
//     listener never answers. This test asserts on the TCP accept that
//     precedes it either way; off just avoids leaving a connection
//     mid-handshake.
//   - ProxyURL cleared. Complete seeds it from the http_proxy ENVIRONMENT
//     VARIABLE, and realConnect hands that to libnet.WithProxy — so on any
//     machine or runner with http_proxy exported, every dial below goes to
//     the proxy instead of the probe and both subtests fail. Nothing in frp
//     or golib consults no_proxy, so even a correctly scoped proxy breaks it.
func forkDialCommon(t *testing.T, p *dialProbe) *v1.ClientCommonConfig {
	t.Helper()
	common := commonForTest()
	if err := common.Complete(); err != nil {
		t.Fatalf("complete the common config: %v", err)
	}
	common.ServerAddr, common.ServerPort = p.addr, p.port
	off := false
	common.Transport.TLS.Enable = &off
	common.Transport.ProxyURL = ""
	return common
}

// TestForkDialsFromConnectWithoutTCPMux is the empirical half of
// physicalDialInOpen: it drives the REAL pinned connector against a loopback
// listener and observes which method actually dials.
//
// It exists because the predicate is otherwise unfalsifiable from inside this
// package — it is a hand-maintained model of another repository's control
// flow, and it was wrong: it answered Open for an unset TCPMux, where the
// fork's `if !lo.FromPtr(c.cfg.Transport.TCPMux) { return nil }` returns from
// Open having dialed nothing. Both guards that hang off the dial seam (the
// redial re-knock and the reconnect watchdog) then attached to a no-op and
// the dialing method ran unguarded. A test over the predicate alone could
// never have caught that, because the predicate WAS the specification. This
// one fails instead if a future fork bump moves the dial.
func TestForkDialsFromConnectWithoutTCPMux(t *testing.T) {
	t.Parallel()

	// The connectors below take t.Context(), not context.Background():
	// it is canceled just before the cleanups run, so the fork's dial path
	// gets a stop signal that does not depend on Close() being the only
	// thing a future fork version listens to.
	//
	// Control first: with TCPMux on, Open owns the dial. Without this the
	// no-dial assertion below would also pass against a probe that simply
	// cannot observe dials.
	t.Run("tcpmux enabled dials from open", func(t *testing.T) {
		t.Parallel()
		probe := newDialProbe(t)
		common := forkDialCommon(t, probe)
		on := true
		common.Transport.TCPMux = &on

		connector := frpclient.NewConnector(t.Context(), common)
		t.Cleanup(func() { _ = connector.Close() })
		if err := connector.Open(); err != nil {
			t.Fatalf("Open with TCPMux enabled: %v", err)
		}
		probe.awaitDial(t, "Open with TCPMux enabled")
		if !physicalDialInOpen(common) {
			t.Fatal("physicalDialInOpen says Connect, but Open is what dialed")
		}
	})

	// The regression: an unset TCPMux is the unmuxed path, not the default-on
	// one. Complete() is deliberately not re-run after clearing the field.
	t.Run("unset tcpmux dials from connect", func(t *testing.T) {
		t.Parallel()
		probe := newDialProbe(t)
		common := forkDialCommon(t, probe)
		common.Transport.TCPMux = nil

		connector := frpclient.NewConnector(t.Context(), common)
		t.Cleanup(func() { _ = connector.Close() })
		if err := connector.Open(); err != nil {
			t.Fatalf("Open with unset TCPMux: %v", err)
		}
		probe.quiet(t, "Open with unset TCPMux")

		conn, err := connector.Connect()
		if err != nil {
			t.Fatalf("Connect with unset TCPMux: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		probe.awaitDial(t, "Connect with unset TCPMux")

		if physicalDialInOpen(common) {
			t.Fatal("physicalDialInOpen says Open, but Open dialed nothing and Connect dialed: " +
				"the redial re-knock and the reconnect watchdog would both attach to the wrong method")
		}
	})
}

// TestForkTCPMuxCompletionDefault pins the other half of the same trap: the
// nil pointer above is only ever absent in production because Complete fills
// it. If a fork bump changed that default, the command would start reaching
// supervisor.New with the unmuxed transport and quietly take the Connect
// seam — where the reconnect watchdog counts one storm dial per work
// connection (see the WATCHDOG COUPLING note on knockingConnector.Connect).
func TestForkTCPMuxCompletionDefault(t *testing.T) {
	t.Parallel()
	common := commonForTest()
	if common.Transport.TCPMux != nil {
		t.Fatalf("an uncompleted config already carries TCPMux = %v; this test's premise is gone", *common.Transport.TCPMux)
	}
	if err := common.Complete(); err != nil {
		t.Fatalf("complete the common config: %v", err)
	}
	if common.Transport.TCPMux == nil || !*common.Transport.TCPMux {
		t.Fatalf("Complete left TCPMux = %v, want true: production would take the Connect dial seam", common.Transport.TCPMux)
	}
	if !physicalDialInOpen(common) {
		t.Fatal("a Complete()d config must dial from Open")
	}
}

// TestForkServiceCompletesTheCommonConfigInPlace pins the guarantee that
// actually protects production, and which nothing else in this repo pins.
//
// physicalDialInOpen's nil-TCPMux case reads like a live hazard for any
// caller that hands supervisor.New an uncompleted config. It is not, and the
// reason is inside the fork: frpclient.NewService runs
// setServiceOptionsDefault -> options.Common.Complete() BEFORE it builds
// anything, mutating the caller's config through the same pointer it then
// stores as svr.common, hands to controlSessionDialer, and finally passes to
// the ConnectorCreator. So the knockingConnector this package installs is
// always handed an already-completed config — TCPMux non-nil — no matter what
// reached supervisor.New.
//
// That makes the fork the load-bearing guard, so a fork bump that dropped the
// Complete call would silently remove it while every comment in this package
// still pointed at cmd/connector.go. This test is what fails instead.
func TestForkServiceCompletesTheCommonConfigInPlace(t *testing.T) {
	t.Parallel()
	common := commonForTest()
	if common.Transport.TCPMux != nil {
		t.Fatal("premise gone: commonForTest is already completed")
	}

	cfgSource := source.NewConfigSource()
	if err := cfgSource.ReplaceAll(nil, nil); err != nil {
		t.Fatalf("seed an empty proxy source: %v", err)
	}
	// The error is deliberately ignored: setServiceOptionsDefault is the very
	// first thing NewService does, so the completion under test has already
	// happened whether or not the rest of construction succeeds. Asserting on
	// the mutation rather than on a successful build keeps this test pinned to
	// the one behavior it is about.
	//
	// The service is then dropped, NOT closed, and that is deliberate in both
	// directions. Construction is inert: measured on the pinned fork it
	// returns a service having started no goroutines, and with no WebServer
	// port set it binds nothing, so there is nothing to release. Closing it
	// anyway is worse than useless — Service.Close calls GracefulClose, which
	// calls svr.cancel, and svr.cancel is only assigned in Run, so Close on a
	// never-run service nil-panics (client/service.go:493-496). If a fork bump
	// ever makes construction acquire something, the fix is a Run/Close pair
	// or a narrower seam, not a bare Close here.
	_, _ = frpclient.NewService(frpclient.ServiceOptions{
		Common:                 common,
		ConfigSourceAggregator: source.NewAggregator(cfgSource),
		UnsafeFeatures:         &security.UnsafeFeatures{},
		ConnectorCreator:       newKnockingConnectorCreator(nil),
	})

	if common.Transport.TCPMux == nil {
		t.Fatal("NewService left TCPMux nil: the fork no longer completes the caller's config, " +
			"so an uncompleted config now reaches knockingConnector and the dial-seam comments in " +
			"refresher.go and supervisor.go are no longer true")
	}
	if !*common.Transport.TCPMux {
		t.Fatalf("NewService completed TCPMux to %v, want true", *common.Transport.TCPMux)
	}
	if !physicalDialInOpen(common) {
		t.Fatal("a config completed by NewService must dial from Open")
	}
}

// connectSeamBase stands in for the FRP connector on the unmuxed path, where
// Open establishes nothing and Connect is what dials.
//
// Connect counts its calls and returns an error rather than a conn: the test
// below drives it with a FAILING refresh, so the wrapper must reject before
// ever delegating. Returning an error makes a regression that delegates
// anyway fail loudly here instead of surfacing as a nil conn somewhere
// downstream, and connectCalls pins the same thing directly.
type connectSeamBase struct{ connectCalls int }

func (c *connectSeamBase) Open() error { return nil }
func (c *connectSeamBase) Connect() (net.Conn, error) {
	c.connectCalls++
	return nil, errors.New("connectSeamBase.Connect called: the wrapper delegated the dial even though the refresh failed")
}
func (c *connectSeamBase) Close() error { return nil }

// TestKnockingConnectorRefreshesOnTheConnectSeam covers the branch an unset or
// disabled TCPMux selects, which had no test at all: the refresh call inside
// knockingConnector.Connect and its error return were 0%-covered, so deleting
// the refresh outright, mislabeling its reason, or swallowing its error each
// left the whole package green. Every other test in this file and in
// reconnect_test.go drives the Open seam.
//
// The seam is unreachable in production (see the WATCHDOG COUPLING note on
// Connect), which is exactly why it needs a test rather than why it does not:
// nothing else would notice it rotting.
//
// A failing knocker pins all three behaviors at once. The wrapped error text
// carries the reason string, so one assertion covers "the refresh ran", "it
// ran as the connect seam", and "its failure reaches FRP instead of being
// swallowed".
func TestKnockingConnectorRefreshesOnTheConnectSeam(t *testing.T) {
	t.Parallel()
	clk := newManualClock()
	knocker := &fakeKnocker{script: []knockResp{{err: errors.New("boom")}}}
	r := newTestRefresher(knocker, time.Hour, clk.now)

	// No stamped token, so the first refresh is a real knock rather than the
	// supervisor's first-cycle handoff.
	common := commonForTest()
	if physicalDialInOpen(common) {
		t.Fatal("premise broken: an unset TCPMux must select the Connect seam")
	}
	base := &connectSeamBase{}
	conn := &knockingConnector{base: base, ctx: t.Context(), common: common, refresher: r}

	// Open owns no dial here, so it must not spend a knock.
	if err := conn.Open(); err != nil {
		t.Fatalf("Open on the Connect seam: %v", err)
	}
	if got := knocker.calls.Load(); got != 0 {
		t.Fatalf("Open knocked %d time(s) on the Connect seam; the refresh belongs to Connect", got)
	}

	got, err := conn.Connect()
	if err == nil {
		t.Fatal("Connect swallowed the refresh failure; FRP must see the dial fail")
	}
	if got != nil {
		t.Fatal("Connect returned a conn alongside the refresh error")
	}
	if base.connectCalls != 0 {
		t.Fatal("Connect delegated to the base connector after the refresh failed: the dial must not happen")
	}
	if want := "redial connect knock failed"; !strings.Contains(err.Error(), want) {
		t.Fatalf("Connect error = %q, want it to contain %q (the reason string is operator-facing: "+
			"it is logged as \"reason\" and interpolated into the budget exit detail)", err, want)
	}
	if knocker.calls.Load() != 1 {
		t.Fatalf("Connect knocked %d time(s), want exactly 1", knocker.calls.Load())
	}
}
