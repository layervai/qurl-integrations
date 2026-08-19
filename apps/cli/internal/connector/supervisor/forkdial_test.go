package supervisor

import (
	"context"
	"net"
	"testing"
	"time"

	frpclient "github.com/fatedier/frp/client"
	v1 "github.com/fatedier/frp/pkg/config/v1"
)

// forkDialAcceptWait bounds how long a dial-detecting assertion waits. The
// listener is on loopback and the dial is a bare TCP connect, so a real dial
// lands in microseconds; this is only the ceiling before a missing dial is
// called missing.
const forkDialAcceptWait = 2 * time.Second

// forkDialQuietWait is how long "no dial happened" is observed for. A dial
// this test expects NOT to see would have to be slower than every dial it
// does see, on the same loopback listener, to slip through.
const forkDialQuietWait = 250 * time.Millisecond

// dialProbe is a loopback listener that reports accepted connections.
type dialProbe struct {
	addr     string
	port     int
	accepted chan net.Conn
}

// newDialProbe starts the listener and drains accepts into a buffered
// channel. Everything is torn down through t.Cleanup.
func newDialProbe(t *testing.T) *dialProbe {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on loopback: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("loopback listener address is %T, want *net.TCPAddr", ln.Addr())
	}
	p := &dialProbe{addr: "127.0.0.1", port: tcpAddr.Port, accepted: make(chan net.Conn, 8)}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed by cleanup
			}
			t.Cleanup(func() { _ = conn.Close() })
			p.accepted <- conn
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
// real default. TLS is then forced off: the fork's realConnect would
// otherwise start a handshake the bare listener never answers, and this test
// asserts on the TCP accept that precedes it either way — off keeps the
// connection from being left mid-handshake.
func forkDialCommon(t *testing.T, p *dialProbe) *v1.ClientCommonConfig {
	t.Helper()
	common := commonForTest()
	if err := common.Complete(); err != nil {
		t.Fatalf("complete the common config: %v", err)
	}
	common.ServerAddr, common.ServerPort = p.addr, p.port
	off := false
	common.Transport.TLS.Enable = &off
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

	// Control first: with TCPMux on, Open owns the dial. Without this the
	// no-dial assertion below would also pass against a probe that simply
	// cannot observe dials.
	t.Run("tcpmux enabled dials from open", func(t *testing.T) {
		t.Parallel()
		probe := newDialProbe(t)
		common := forkDialCommon(t, probe)
		on := true
		common.Transport.TCPMux = &on

		connector := frpclient.NewConnector(context.Background(), common)
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

		connector := frpclient.NewConnector(context.Background(), common)
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
