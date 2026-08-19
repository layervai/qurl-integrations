package supervisor

import (
	"crypto/tls"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	frpclient "github.com/fatedier/frp/client"
	v1 "github.com/fatedier/frp/pkg/config/v1"
	"github.com/fatedier/frp/pkg/config/v1/validation"
)

// tlsRecordHandshake is the content type byte of a TLS record carrying a
// handshake message. A ClientHello is the first thing crypto/tls writes, so it
// is what a TLS-wrapped dial puts on the wire first.
const tlsRecordHandshake = 0x16

// plaintextSentinel is written through the conn the fork returns. On a
// plaintext conn it arrives verbatim; on a TLS-wrapped one the ClientHello
// goes first. The first byte the probe sees therefore says which happened,
// which is why this must differ from tlsRecordHandshake.
const plaintextSentinel = 0xAB

// A write is needed at all because the wrap is LAZY: golib's tlsAfterHook
// returns tls.Client(conn, cfg), which handshakes on first use, and the fork's
// eager tell — the custom TLS head byte — is not available to key on.
// Complete() defaults transport.tls.disableCustomTLSFirstByte to true, so on
// any production-shaped config that byte is never sent.
//
// The write is asynchronous everywhere below. A tls.Conn write blocks on the
// server's half of a handshake these probes never complete, so writing inline
// would hang the test rather than fail it. The ClientHello still goes out.

// wireProbe is a loopback TCP listener that reports the first bytes written on
// each accepted connection.
//
// Teardown follows newDialProbe's rule for the same reason: registered once,
// before the accept goroutine starts, owning the listener and every connection
// accepted through it. A cleanup registered from inside the accept loop can
// land after runCleanup has drained and then never runs at all.
type wireProbe struct {
	addr  string
	port  int
	first chan []byte

	mu     sync.Mutex
	conns  []net.Conn
	closed bool
}

func newWireProbe(t *testing.T) *wireProbe {
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
	p := &wireProbe{addr: "127.0.0.1", port: tcpAddr.Port, first: make(chan []byte, 8)}
	t.Cleanup(func() {
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
			go p.readFirst(conn)
		}
	}()
	return p
}

// readFirst reports one short read per connection and then leaves the
// connection OPEN for the owning cleanup to close. Closing it here would reset
// the peer mid-handshake, turning a clean observation into a dial error on the
// other side and losing the very distinction these tests draw.
func (p *wireProbe) readFirst(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(forkDialAcceptWait))
	buf := make([]byte, 8)
	n, err := conn.Read(buf)
	if n <= 0 || err != nil {
		return
	}
	select {
	case p.first <- append([]byte(nil), buf[:n]...):
	default:
	}
}

// awaitFirstByte returns the first byte written on the next connection.
func (p *wireProbe) awaitFirstByte(t *testing.T, what string) byte {
	t.Helper()
	select {
	case b := <-p.first:
		return b[0]
	case <-time.After(forkDialAcceptWait):
		t.Fatalf("%s: nothing was written to the listener within %s", what, forkDialAcceptWait)
		return 0
	}
}

// quiet fails if anything is written within the quiet window.
func (p *wireProbe) quiet(t *testing.T, what string) {
	t.Helper()
	select {
	case b := <-p.first:
		t.Fatalf("%s: % x reached the listener, but this dial must not get that far", what, b)
	case <-time.After(forkDialQuietWait):
	}
}

// dialThroughFork runs the fork's Connect on the given config and writes the
// sentinel through whatever it returns, asynchronously. The returned channel
// carries Connect's error, so a dial that never reaches the wire reports why.
func dialThroughFork(t *testing.T, common *v1.ClientCommonConfig) <-chan error {
	t.Helper()
	connector := frpclient.NewConnector(t.Context(), common)
	t.Cleanup(func() { _ = connector.Close() })
	failed := make(chan error, 1)
	conns := make(chan net.Conn, 1)
	t.Cleanup(func() {
		select {
		case c := <-conns:
			_ = c.Close()
		default:
		}
	})
	go func() {
		conn, err := connector.Connect()
		if err != nil {
			failed <- err
			return
		}
		// Buffered, so this never blocks even if the cleanup above has
		// already drained: the connection is closed either way, here by the
		// probe's teardown of its own end.
		conns <- conn
		_, _ = conn.Write([]byte{plaintextSentinel})
	}()
	return failed
}

// TestForkTLSDecisionMatchesTLSEnabled is the empirical half of TLSEnabled,
// which the predicate alone cannot falsify. It drives the REAL pinned
// connector against a loopback listener and reads the answer off the wire:
// a TLS-wrapped dial opens with a handshake record, a plaintext one opens with
// the sentinel this test wrote. TestTLSEnabledAndIPLiteralHost asserts the
// same table against itself and so agrees with any drift by construction —
// the structural gap that let the exact-match "quic" bug survive.
//
// Only the TCP dial path is observable this way. The QUIC branch is pinned
// separately by TestForkTLSEnabledAgreesWithTheForkOnMixedCaseQUIC.
func TestForkTLSDecisionMatchesTLSEnabled(t *testing.T) {
	t.Parallel()

	// The enable flag, both ways. These two are the control: without them a
	// wss assertion could pass against a probe that cannot tell TLS from
	// plaintext at all.
	t.Run("enable flag on wraps in tls", func(t *testing.T) {
		t.Parallel()
		probe := newWireProbe(t)
		common := forkDialCommon(t, probe.addr, probe.port)
		on := true
		common.Transport.TLS.Enable = &on
		if !TLSEnabled(common) {
			t.Fatal("TLSEnabled says plaintext for an explicitly enabled config")
		}
		dialThroughFork(t, common)
		if got := probe.awaitFirstByte(t, "Connect with tls.enable true"); got != tlsRecordHandshake {
			t.Fatalf("first byte on the wire is %#x, want a TLS handshake record (%#x): "+
				"the fork did not wrap a dial TLSEnabled reports as TLS, so both SNI guards are judging the wrong dial", got, tlsRecordHandshake)
		}
	})

	t.Run("enable flag off stays plaintext", func(t *testing.T) {
		t.Parallel()
		probe := newWireProbe(t)
		// forkDialCommon already pins TLS off.
		common := forkDialCommon(t, probe.addr, probe.port)
		if TLSEnabled(common) {
			t.Fatal("TLSEnabled says TLS for an explicitly disabled config")
		}
		dialThroughFork(t, common)
		if got := probe.awaitFirstByte(t, "Connect with tls.enable false"); got != plaintextSentinel {
			t.Fatalf("first byte on the wire is %#x, want the plaintext sentinel (%#x): "+
				"the fork wrapped a dial TLSEnabled reports as plaintext, so both SNI guards are silent where they must fire", got, plaintextSentinel)
		}
	})

	// The forced branch: wss is TLS with the enable flag OFF, which is the
	// only thing that distinguishes it from the control above.
	t.Run("wss forces tls with the flag off", func(t *testing.T) {
		t.Parallel()
		probe := newWireProbe(t)
		common := forkDialCommon(t, probe.addr, probe.port)
		common.Transport.Protocol = "wss"
		if enabled := common.Transport.TLS.Enable; enabled == nil || *enabled {
			t.Fatalf("this subtest needs the enable flag off to mean anything; it is %v", enabled)
		}
		if !TLSEnabled(common) {
			t.Fatal("TLSEnabled says plaintext for wss")
		}
		// No sentinel is needed here and none is relied on: the fork writes
		// first, sending its websocket upgrade through the TLS conn.
		dialThroughFork(t, common)
		if got := probe.awaitFirstByte(t, "Connect with protocol wss"); got != tlsRecordHandshake {
			t.Fatalf("first byte on the wire is %#x, want a TLS handshake record (%#x): "+
				"the fork no longer forces TLS for wss", got, tlsRecordHandshake)
		}
	})

	// The case-SENSITIVE half of the asymmetry. The fork forces wss with ==,
	// so an uppercase spelling is not wss to it — and golib then refuses to
	// dial the protocol at all, since only the lowercase literal is rewritten
	// to tcp beforehand. Uppercase wss therefore never reaches a TLS decision,
	// which is what makes TLSEnabled's exact match correct rather than merely
	// untested. Asserted as unreachability, deliberately not as an error
	// string: the message belongs to golib and is not a contract.
	t.Run("uppercase wss never reaches a tls decision", func(t *testing.T) {
		t.Parallel()
		probe := newWireProbe(t)
		common := forkDialCommon(t, probe.addr, probe.port)
		common.Transport.Protocol = "WSS"
		if TLSEnabled(common) {
			t.Fatal("TLSEnabled forces TLS for uppercase wss, but the fork's == does not")
		}
		failed := dialThroughFork(t, common)
		probe.quiet(t, "Connect with protocol WSS")
		select {
		case <-failed:
		case <-time.After(forkDialQuietWait):
			t.Fatal("Connect with protocol WSS neither dialed nor failed: " +
				"if the fork now accepts a mixed-case wss, TLSEnabled's exact match has to become EqualFold too")
		}
	})
}

// TestForkTLSEnabledAgreesWithTheForkOnMixedCaseQUIC pins the branch that was
// wrong: TLSEnabled matched "quic" exactly while the fork matches it with
// strings.EqualFold, so a mixed-case protocol dialed an always-TLS QUIC
// session that both IP-literal SNI guards scored as plaintext and waved through.
//
// The spelling is uppercase for the reason argued at TestForkDialsQUICFromOpen
// — nothing on this repo's path runs frp's own protocol validation, so a
// mixed-case protocol really does reach the connector, and a lowercase literal
// would leave both sides' case handling unexercised.
//
// What the datagram proves is that the fork took its QUIC path, which is
// unconditionally TLS: it builds a *tls.Config on both branches of the enable
// flag and hands it to quic.DialAddr, which refuses a nil one before putting
// anything on the wire. So an observed packet is also evidence a TLS config
// was built — there is no plaintext QUIC to have dialed instead.
func TestForkTLSEnabledAgreesWithTheForkOnMixedCaseQUIC(t *testing.T) {
	t.Parallel()
	probe := newPacketProbe(t)
	common := forkDialCommon(t, probe.addr, probe.port)
	common.Transport.Protocol = "QUIC"
	if enabled := common.Transport.TLS.Enable; enabled == nil || *enabled {
		t.Fatalf("this test needs the enable flag off, or it proves nothing about the protocol branch; it is %v", enabled)
	}
	if !TLSEnabled(common) {
		t.Fatal("TLSEnabled says plaintext for a mixed-case QUIC config, but the fork's EqualFold takes the always-TLS QUIC path: " +
			"the overlay and the redial refresher both fail open on the one protocol that is always TLS")
	}

	// Goroutined, and joined in cleanup, for TestForkDialsQUICFromOpen's
	// reasons: the handshake cannot complete against a probe speaking no
	// QUIC, so Open returns only on context cancellation or quic-go's own
	// idle timeout, both far longer than the assertion waits.
	connector := frpclient.NewConnector(t.Context(), common)
	dialed := make(chan error, 1)
	dialDone := make(chan struct{})
	go func() {
		defer close(dialDone)
		dialed <- connector.Open()
	}()
	t.Cleanup(func() {
		select {
		case <-dialDone:
			_ = connector.Close()
		case <-time.After(forkDialJoinWait):
			t.Errorf("Open did not return within %s of the test context being canceled: "+
				"the fork may no longer propagate the connector's context into the QUIC dial", forkDialJoinWait)
		}
	})
	probe.awaitDatagram(t, "Open with protocol QUIC and tls.enable false", dialed)
}

// errSNIRecorded aborts the handshake the moment the server name has been
// read. The probe has no certificate to offer and does not need one — the
// ClientHello is the whole observation — so ending it here keeps the failure
// deliberate and named instead of letting it surface later as a confusing
// "no certificates configured".
var errSNIRecorded = errors.New("sni probe: server name recorded, no handshake wanted")

// newSNIProbe is a loopback listener that terminates TLS just far enough to
// record the server name the client asked for.
func newSNIProbe(t *testing.T) (addr string, port int, sni <-chan string) {
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
	seen := make(chan string, 8)
	ctx := t.Context()
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed by cleanup
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(forkDialAcceptWait))
				server := tls.Server(conn, &tls.Config{
					GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
						select {
						case seen <- hello.ServerName:
						default:
						}
						return nil, errSNIRecorded
					},
				})
				_ = server.HandshakeContext(ctx)
			}()
		}
	}()
	return "127.0.0.1", tcpAddr.Port, seen
}

// TestForkDerivesSNIFromServerAddr pins the claim the IP-literal TLS-SNI
// guards rest on: with TLS enabled and no explicit ServerName, the fork falls back
// to ServerAddr — the field the overlay overwrites with the ACK dial target.
//
// It also pins the part that makes the guard necessary rather than merely
// tidy. An IP-literal ServerAddr does not travel as an IP-literal SNI; Go
// drops it from the extension entirely and the dial presents no server name at
// all. A guard written against the "servers reject an IP SNI" story would be
// guarding a thing that never happens, so the observation is worth having on
// record next to the fallback it qualifies.
func TestForkDerivesSNIFromServerAddr(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		serverAddr string // "" keeps the probe's IP literal
		explicit   string
		want       string
		why        string
	}{
		{
			name: "hostname server addr becomes the server name",
			// A hostname is required to observe the fallback at all, which is
			// exactly what the IP-literal case below demonstrates. localhost
			// resolves without a network and reaches the probe's loopback
			// listener.
			serverAddr: "localhost",
			want:       "localhost",
			why:        "the fork no longer falls back to ServerAddr, so the guard is reading a field the dial ignores",
		},
		{
			name:       "explicit server name wins",
			serverAddr: "localhost",
			explicit:   "explicit.example",
			want:       "explicit.example",
			why:        "the fork no longer prefers an explicit ServerName, so the guard's ServerName == \"\" narrowing is wrong",
		},
		{
			name: "ip literal server addr sends no server name",
			want: "",
			why:  "an IP literal now reaches the wire as a server name, which would change what the guard is preventing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			addr, port, sni := newSNIProbe(t)
			common := forkDialCommon(t, addr, port)
			if tc.serverAddr != "" {
				common.ServerAddr = tc.serverAddr
			}
			on := true
			common.Transport.TLS.Enable = &on
			common.Transport.TLS.ServerName = tc.explicit
			dialThroughFork(t, common)
			select {
			case got := <-sni:
				if got != tc.want {
					t.Fatalf("the fork asked for server name %q, want %q: %s", got, tc.want, tc.why)
				}
			case <-time.After(forkDialAcceptWait):
				t.Fatalf("no ClientHello reached the probe within %s", forkDialAcceptWait)
			}
		})
	}
}

// TestKnownTransportProtocolsMatchTheForkSupportedSet converts the audit that
// knownTransportProtocols used to only claim into one the build performs. The
// set is the fork's own SupportedTransportProtocols widened by "", so a bump
// that adds a protocol fails here instead of quietly widening TLSEnabled's
// fail-open hole by one value nobody re-audited.
//
// What this canNOT check is the half the marker at that map carries: whether a
// newly added protocol IMPLIES TLS. That is a claim about the fork's dialer,
// and the failure below is the prompt to go and read it.
func TestKnownTransportProtocolsMatchTheForkSupportedSet(t *testing.T) {
	t.Parallel()
	if len(validation.SupportedTransportProtocols) == 0 {
		t.Fatal("the fork exports an empty SupportedTransportProtocols; this guard has nothing to compare against")
	}
	want := append([]string{""}, validation.SupportedTransportProtocols...)
	slices.Sort(want)
	got := slices.Clone(knownTransportProtocolList)
	if !slices.Equal(got, want) {
		t.Errorf("knownTransportProtocols is %v, want %v (the fork's SupportedTransportProtocols plus \"\"): "+
			"re-audit TLSEnabled for whether the difference implies TLS, then update the map", got, want)
	}
}

// TestForkTLSEnableCompletionDefault pins the premise that makes the IP-literal
// SNI guards live rather than dead code: the command Completes the common
// config before supervisor.New, and Complete turns an unset enable flag ON.
// Were that default to flip, every real cycle would score as plaintext and both
// guards would stop firing — silently, since nothing else reads the flag.
func TestForkTLSEnableCompletionDefault(t *testing.T) {
	t.Parallel()
	common := commonForTest()
	if common.Transport.TLS.Enable != nil {
		t.Fatalf("an uncompleted config already carries tls.enable = %v; this test's premise is gone", *common.Transport.TLS.Enable)
	}
	if TLSEnabled(common) {
		t.Fatal("TLSEnabled reports TLS for an uncompleted, protocol-less config")
	}
	if err := common.Complete(); err != nil {
		t.Fatalf("complete the common config: %v", err)
	}
	if common.Transport.TLS.Enable == nil || !*common.Transport.TLS.Enable {
		t.Fatalf("Complete left tls.enable = %v, want true: the IP-literal SNI guards would never fire in production", common.Transport.TLS.Enable)
	}
	if !TLSEnabled(common) {
		t.Fatal("TLSEnabled disagrees with a Complete()d config's own enable flag")
	}
}
