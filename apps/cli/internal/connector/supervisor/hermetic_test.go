package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	v1 "github.com/fatedier/frp/pkg/config/v1"
	frpserver "github.com/fatedier/frp/server"
	qurl "github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/knock"
)

// This file is the hermetic end-to-end proof for the production wiring: the
// real supervisor drives the real FRP client service (built by
// NewFRPRunnerFactory) against the FRP fork's real in-process server, and
// HTTP bytes cross the tunnel to a local echo backend. No network beyond
// loopback, no real NHP wire — the knocker is a scripted CycleKnocker whose
// ACK pins the in-process server as the dial target, which is exactly the
// seam the native knocker fills in production.

// reserveHermeticTCPPort reserves a loopback TCP port and releases it so the
// in-process server (or its restarted successor) can bind it.
func reserveHermeticTCPPort(t *testing.T) int {
	t.Helper()
	var lc net.ListenConfig
	listener, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback TCP port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// hermeticCycleKnocker is a real CycleKnocker over canonical qurl-go RunIDs,
// returning a fixed ACK that pins the hermetic server. It counts knocks and
// records begun/ended cycles so the wire-cadence assertions have evidence.
type hermeticCycleKnocker struct {
	mu           sync.Mutex
	resourceHost string
	current      string
	knocks       int
	begun        []string
	ended        []string
}

func (k *hermeticCycleKnocker) BeginCycle() error {
	runID, err := qurl.NewCycleRunID()
	if err != nil {
		return err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.current = runID
	k.begun = append(k.begun, runID)
	return nil
}

func (k *hermeticCycleKnocker) CycleRunID() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.current
}

func (k *hermeticCycleKnocker) Knock(context.Context) (*knock.Result, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.knocks++
	return &knock.Result{
		ACTokens:     map[string]string{testResource: "ac-hermetic"},
		ResourceHost: map[string]string{testResource: k.resourceHost},
	}, nil
}

func (k *hermeticCycleKnocker) EndCycle(context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.ended = append(k.ended, k.current)
	k.current = ""
	return nil
}

func (k *hermeticCycleKnocker) stats() (knocks int, begun, ended []string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.knocks, append([]string(nil), k.begun...), append([]string(nil), k.ended...)
}

// hermeticNewProxyObservation is one server-side NewProxy admission: the
// RunID the server authenticated for the session and the proxy registered
// under it.
type hermeticNewProxyObservation struct {
	runID     string
	proxyName string
}

// hermeticProxyRecorder is an in-process server plugin (the fork's
// server-plugin HTTP protocol) that records every NewProxy without rejecting
// — the server-side evidence for the RunID→Login binding assertions.
type hermeticProxyRecorder struct {
	server *httptest.Server

	mu           sync.Mutex
	observations []hermeticNewProxyObservation
}

func newHermeticProxyRecorder(t *testing.T) *hermeticProxyRecorder {
	t.Helper()
	recorder := &hermeticProxyRecorder{}
	recorder.server = httptest.NewServer(http.HandlerFunc(recorder.handle))
	t.Cleanup(recorder.server.Close)
	return recorder
}

func (p *hermeticProxyRecorder) handle(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Op      string `json:"op"`
		Content struct {
			User struct {
				RunID string `json:"run_id"`
			} `json:"user"`
			ProxyName string `json:"proxy_name"`
		} `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if request.Op == "NewProxy" {
		p.mu.Lock()
		p.observations = append(p.observations, hermeticNewProxyObservation{
			runID:     request.Content.User.RunID,
			proxyName: request.Content.ProxyName,
		})
		p.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"reject": false, "unchange": true}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (p *hermeticProxyRecorder) snapshot() []hermeticNewProxyObservation {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]hermeticNewProxyObservation, len(p.observations))
	copy(out, p.observations)
	return out
}

// startHermeticFRPS runs the FRP fork's real server on the given loopback
// port, wired to the NewProxy recorder plugin. NewService binds the listener
// synchronously; Close stops it (idempotent against Run's own shutdown).
func startHermeticFRPS(t *testing.T, bindPort int, pluginURL string) *frpserver.Service {
	t.Helper()
	cfg := &v1.ServerConfig{
		BindAddr:      "127.0.0.1",
		BindPort:      bindPort,
		ProxyBindAddr: "127.0.0.1",
		HTTPPlugins: []v1.HTTPPluginOptions{{
			Name: "newproxy-recorder",
			Addr: pluginURL,
			Path: "/",
			Ops:  []string{"NewProxy"},
		}},
	}
	if err := cfg.Complete(); err != nil {
		t.Fatalf("complete hermetic server config: %v", err)
	}
	svc, err := frpserver.NewService(cfg)
	if err != nil {
		t.Fatalf("construct hermetic server on 127.0.0.1:%d: %v", bindPort, err)
	}
	go svc.Run(context.Background())
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

// pollHermeticHTTPBody polls the tunneled endpoint until the echo body
// round-trips or the guard expires; an early supervisor exit fails fast.
func pollHermeticHTTPBody(t *testing.T, endpoint, wantBody, phase string, guard time.Duration, supervisorResult <-chan error) {
	t.Helper()
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(guard)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case err := <-supervisorResult:
			t.Fatalf("supervisor exited during %s round-trip: %v", phase, err)
		default:
		}
		response, err := client.Get(endpoint) //nolint:noctx // hermetic loopback poll bounded by the client timeout and the guard deadline.
		if err != nil {
			lastErr = err
		} else {
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			switch {
			case readErr != nil:
				lastErr = readErr
			case response.StatusCode == http.StatusOK && string(body) == wantBody:
				return
			default:
				lastErr = fmt.Errorf("status %d body %q", response.StatusCode, body)
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s round-trip through the tunnel never returned the echo body: %v", phase, lastErr)
}

func hermeticBackendPort(t *testing.T, rawURL string) int {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse echo backend URL %q: %v", rawURL, err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("parse echo backend port from %q: %v", rawURL, err)
	}
	return port
}

// TestHermeticTunnelRoundTripAndRedialReKnock drives the full story: knock →
// ACK dial target → FRP Login bound to the native RunID → proxy registration
// → HTTP bytes across the in-process server to a local echo backend. Then the
// reconnect leg: the server is killed after a successful round-trip and a new
// instance comes up on the same port; the FRP client's INTERNAL reconnect
// must re-knock through the redial refresher before its physical redial
// (fresh knock evidence) and heal inside the same supervisor cycle under the
// same RunID — the no-sniffer degrade path this port documents.
func TestHermeticTunnelRoundTripAndRedialReKnock(t *testing.T) {
	if testing.Short() {
		t.Skip("hermetic tunnel round-trip is the slowest test in the package")
	}
	const echoBody = "hermetic-roundtrip-echo-payload"
	echoBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, echoBody)
	}))
	t.Cleanup(echoBackend.Close)

	frpsPort := reserveHermeticTCPPort(t)
	remotePort := reserveHermeticTCPPort(t)
	// The ACK dial target must be a DNS name: the completed FRP client
	// config enables TLS by default and the knock overlay fails closed on
	// IP-literal targets with no explicit SNI. localhost keeps it hermetic.
	resourceHost := "localhost:" + strconv.Itoa(frpsPort)
	proxyEndpoint := "http://127.0.0.1:" + strconv.Itoa(remotePort)

	recorder := newHermeticProxyRecorder(t)
	firstFRPS := startHermeticFRPS(t, frpsPort, recorder.server.URL)

	knocker := &hermeticCycleKnocker{resourceHost: resourceHost}
	common := &v1.ClientCommonConfig{}
	common.Log.Level = "error"
	if err := common.Complete(); err != nil {
		t.Fatalf("complete FRP client common config: %v", err)
	}
	proxies := []v1.ProxyConfigurer{&v1.TCPProxyConfig{
		ProxyBaseConfig: v1.ProxyBaseConfig{
			Name: "hermetic-route",
			Type: "tcp",
			ProxyBackend: v1.ProxyBackend{
				LocalIP:   "127.0.0.1",
				LocalPort: hermeticBackendPort(t, echoBackend.URL),
			},
		},
		RemotePort: remotePort,
	}}
	factory, err := NewFRPRunnerFactory(FRPFactoryConfig{
		Knocker:         knocker,
		ResourceID:      testResource,
		Proxies:         proxies,
		Logger:          discardLogger(),
		RedialKnockGate: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("build FRP runner factory: %v", err)
	}
	sup, err := New(&Config{
		Common:           common,
		Knocker:          knocker,
		KnockResourceID:  testResource,
		RunnerFactory:    factory,
		Logger:           discardLogger(),
		MinBackoff:       5 * time.Millisecond,
		MaxBackoff:       25 * time.Millisecond,
		MinKnockInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("build supervisor: %v", err)
	}

	supCtx, cancelSup := context.WithCancel(context.Background())
	defer cancelSup()
	supervisorResult := make(chan error, 1)
	go func() { supervisorResult <- sup.Run(supCtx) }()

	// Leg 1: knock → Login under the native RunID → registration → bytes.
	pollHermeticHTTPBody(t, proxyEndpoint, echoBody, "first tunnel server", 15*time.Second, supervisorResult)
	knocksAfterFirstLeg, begun, _ := knocker.stats()
	if len(begun) != 1 {
		t.Fatalf("native cycles begun after first round-trip = %d, want 1", len(begun))
	}
	if knocksAfterFirstLeg != 1 {
		t.Fatalf("knocks after first round-trip = %d, want exactly the supervisor's cycle knock (the redial refresher's first call is the handoff)", knocksAfterFirstLeg)
	}

	// Leg 2: kill the server after the successful round-trip and bring up a
	// fresh instance on the same port. The FRP client's internal reconnect
	// owns recovery; every physical redial must consume a fresh knock.
	if err := firstFRPS.Close(); err != nil {
		t.Fatalf("stop first hermetic server: %v", err)
	}
	startHermeticFRPS(t, frpsPort, recorder.server.URL)
	pollHermeticHTTPBody(t, proxyEndpoint, echoBody, "restarted tunnel server", 30*time.Second, supervisorResult)

	knocksAfterSecondLeg, _, _ := knocker.stats()
	if knocksAfterSecondLeg <= knocksAfterFirstLeg {
		t.Fatalf("knocks after redial recovery = %d, want > %d: the internal reconnect must re-knock before its physical redial", knocksAfterSecondLeg, knocksAfterFirstLeg)
	}

	// Shutdown: clean cancellation, every begun native cycle ended.
	cancelSup()
	select {
	case err := <-supervisorResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("supervisor exit = %v, want clean cancellation (it must have survived the server restart)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor did not exit after cancellation")
	}
	_, begun, ended := knocker.stats()
	if len(begun) != len(ended) {
		t.Fatalf("native cycles begun %d ended %d, want every begun cycle ended", len(begun), len(ended))
	}

	// Server-side admission evidence: every NewProxy ran under the one
	// supervised cycle's canonical RunID (the internal reconnect reuses the
	// session identity — the supervisor never recycled), for the one route.
	observations := recorder.snapshot()
	if len(observations) < 2 {
		t.Fatalf("NewProxy observations = %d, want at least one per server instance", len(observations))
	}
	cycleRunID := begun[0]
	if err := qurl.ValidateCycleRunID(cycleRunID); err != nil {
		t.Fatalf("cycle RunID %q is not canonical: %v", cycleRunID, err)
	}
	for i, observation := range observations {
		if observation.runID != cycleRunID {
			t.Fatalf("NewProxy[%d] RunID = %q, want the presented cycle RunID %q (Login→RunID binding)", i, observation.runID, cycleRunID)
		}
		if observation.proxyName != "hermetic-route" {
			t.Fatalf("NewProxy[%d] proxy name = %q, want the one configured route", i, observation.proxyName)
		}
	}
	if got := sup.Cycles(); got != 0 {
		t.Fatalf("completed supervisor cycles = %d, want 0 (recovery stayed inside the first, still-running cycle)", got)
	}
}
