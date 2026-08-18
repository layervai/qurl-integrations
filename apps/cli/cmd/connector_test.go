package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	goliblog "github.com/fatedier/golib/log"
	qurl "github.com/layervai/qurl-go/qurl"

	v1 "github.com/fatedier/frp/pkg/config/v1"
	frplog "github.com/fatedier/frp/pkg/util/log"
	frpserver "github.com/fatedier/frp/server"

	"github.com/layervai/qurl-integrations/apps/cli/internal/clitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/knock"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/replica"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/supervisor"
)

// Command-level contract suite for `qurl connector run`, over the same
// hermetic in-process-frps pattern as the supervisor suite: the REAL command
// tree drives the REAL frpgen/supervisor/FRP-client wiring against the FRP
// fork's real in-process server, with only the UDP legs faked at the
// command's two injection seams (agent open ladder, cycle knocker). The
// token-required, refresh-gate, and Hub-config paths run the PRODUCTION
// agent.Open ladder end to end — those failures are all pre-network.

// connectorTestHubKeyB64 is a valid canonical base64 32-byte key (value 9 in
// the first byte), mirroring the agent suite's test pin.
const connectorTestHubKeyB64 = "CQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

// TestMain pins the FRP library's process-global logger ONCE, before any test
// goroutine exists. The hermetic serve test runs the FRP fork's server and
// client in this one process, both logging through that global from their own
// goroutines — so the production per-invocation swap (redirectFRPLogsToStderr)
// would be a mid-flight write race here. runCLI injects a no-op redirect for
// every invocation instead.
func TestMain(m *testing.M) {
	frplog.Logger = goliblog.New(
		goliblog.WithCaller(false),
		goliblog.WithLevel(goliblog.WarnLevel),
		goliblog.WithOutput(goliblog.NewConsoleWriter(goliblog.ConsoleConfig{}, io.Discard)),
	)
	os.Exit(m.Run())
}

// connectorTestEnv pins a hermetic Connector environment: a valid custom Hub
// triple and no ambient operator overrides. These packages read the real
// process environment (they are shared operator contracts, deliberately
// outside the harness's injected env map), so t.Setenv is the seam.
func connectorTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv(hub.EnvHost, "hub.test.nhp.layerv.ai")
	t.Setenv(hub.EnvPort, "443")
	t.Setenv(hub.EnvServerPublicKey, connectorTestHubKeyB64)
	for _, name := range []string{
		state.EnvStateDir, state.EnvStateDirPrimary, state.EnvAgentID,
		agent.EnvRefreshMode, agent.EnvEnrollmentToken, agent.EnvEnrollmentTokenFile,
		agent.EnvKnockResourceID, replica.EnvReplicaID,
		"QURL_CONNECTOR_ID", "QURL_CONNECTOR_SLUG",
	} {
		t.Setenv(name, "restore-after-test")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

// unsetHubTriple removes the triple connectorTestEnv set, for the dark-build
// Hub-config path (t.Setenv above already registered restoration).
func unsetHubTriple(t *testing.T) {
	t.Helper()
	for _, name := range []string{hub.EnvHost, hub.EnvPort, hub.EnvServerPublicKey} {
		t.Setenv(name, "restore-after-test")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

// skipWithoutPinnedState skips where qurl-go's pinned local agent state is
// unsupported (Windows today), mirroring the agent suite's harness guard —
// every path through agent.Open needs the state store.
func skipWithoutPinnedState(t *testing.T) {
	t.Helper()
	probe, err := state.Open(t.TempDir())
	if err != nil {
		if errors.Is(err, qurl.ErrAgentStateContinuity) && strings.Contains(err.Error(), "unsupported on this platform") {
			t.Skipf("qurl-go pinned agent state unsupported here: %v", err)
		}
		t.Fatal(err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
}

// connectorResourceRow is the producer's generic resource payload as the mock
// serves it (the agent suite's wire shape).
type connectorResourceRow struct {
	ResourceID         string `json:"resource_id"`
	ConnectorRoutingID string `json:"connector_routing_id"`
	KnockResourceID    string `json:"knock_resource_id"`
	Type               string `json:"type"`
	Status             string `json:"status"`
	Slug               string `json:"slug"`
}

var connectorRoutingIDEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// mintConnectorRow mints a wire row that satisfies qurl-go's fail-closed
// response validation: a real P-256 resource id and a canonical c-prefixed
// routing id.
func mintConnectorRow(t *testing.T, slug string) connectorResourceRow {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(der)
	return connectorResourceRow{
		ResourceID:         base64.RawURLEncoding.EncodeToString(der),
		ConnectorRoutingID: "c-" + connectorRoutingIDEncoding.EncodeToString(digest[:]),
		KnockResourceID:    "resource-public-key",
		Type:               "tunnel",
		Status:             "active",
		Slug:               slug,
	}
}

// connectorProducer mocks the producer's Connector-resource routes for the
// command's ensure path and counts every request for zero-network proofs.
type connectorProducer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []string
	rows     []connectorResourceRow
}

func newConnectorProducer(t *testing.T, rows ...connectorResourceRow) *connectorProducer {
	t.Helper()
	p := &connectorProducer{rows: rows}
	p.Server = httptest.NewServer(http.HandlerFunc(p.handle))
	t.Cleanup(p.Close)
	return p
}

func (p *connectorProducer) requestCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

func (p *connectorProducer) handle(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.requests = append(p.requests, r.Method+" "+r.URL.Path)
	rows := append([]connectorResourceRow(nil), p.rows...)
	p.mu.Unlock()

	writeJSON := func(status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/resources":
		slug := r.URL.Query().Get("slug")
		matches := []connectorResourceRow{}
		for _, row := range rows {
			if row.Slug == slug {
				matches = append(matches, row)
			}
		}
		writeJSON(http.StatusOK, map[string]any{"data": matches})
	default:
		writeJSON(http.StatusNotFound, map[string]any{"code": "not_found", "title": "Not Found", "detail": "no such route in the mock producer"})
	}
}

// fakeConnectorOpen stands in for the agent enroll/open ladder at the
// command's seam: it opens the REAL state store (the marker ladder stays
// real) and a REAL qurl-go resource client against the mock producer, while
// asserting the config contract the command owes the agent package.
func fakeConnectorOpen(t *testing.T, producerURL string) func(context.Context, *agent.Config) (*agent.Runtime, error) {
	return func(_ context.Context, cfg *agent.Config) (*agent.Runtime, error) {
		if cfg.EnrollmentToken != "" {
			t.Errorf("command set Config.EnrollmentToken = %q; the token must never travel through the command layer", cfg.EnrollmentToken)
		}
		if cfg.APIBaseURL != producerURL {
			t.Errorf("Config.APIBaseURL = %q, want the resolved endpoint %q", cfg.APIBaseURL, producerURL)
		}
		dir, err := state.ResolveDir(cfg.StateDir)
		if err != nil {
			return nil, err
		}
		store, err := state.Open(dir)
		if err != nil {
			return nil, err
		}
		client, err := qurl.NewClient(qurl.BearerToken("test-device-credential"), qurl.WithBaseURL(producerURL))
		if err != nil {
			_ = store.Close()
			return nil, err
		}
		return &agent.Runtime{Client: client, Store: store, AgentID: "agent-cmd-test"}, nil
	}
}

// cmdCycleKnocker is the loopback CycleKnocker for the hermetic serve test:
// canonical qurl-go RunIDs, a fixed ACK pinning the in-process server, and
// begin/end/close accounting.
type cmdCycleKnocker struct {
	mu           sync.Mutex
	resourceID   string
	resourceHost string
	knockErr     error
	current      string
	knocks       int
	begun        []string
	ended        []string
	closed       bool
}

func (k *cmdCycleKnocker) BeginCycle() error {
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

func (k *cmdCycleKnocker) CycleRunID() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.current
}

func (k *cmdCycleKnocker) Knock(context.Context) (*knock.Result, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.knocks++
	if k.knockErr != nil {
		return nil, k.knockErr
	}
	return &knock.Result{
		ACTokens:     map[string]string{k.resourceID: "ac-cmd-hermetic"},
		ResourceHost: map[string]string{k.resourceID: k.resourceHost},
	}, nil
}

func (k *cmdCycleKnocker) EndCycle(context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.ended = append(k.ended, k.current)
	k.current = ""
	return nil
}

func (k *cmdCycleKnocker) Close() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.closed = true
}

func (k *cmdCycleKnocker) stats() (begun, ended []string, closed bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]string(nil), k.begun...), append([]string(nil), k.ended...), k.closed
}

// cmdProxyRecorder is the in-process server plugin recording every NewProxy
// admission (the supervisor suite's server-side evidence pattern).
type cmdProxyRecorder struct {
	server *httptest.Server

	mu           sync.Mutex
	observations []struct{ runID, proxyName string }
}

func newCmdProxyRecorder(t *testing.T) *cmdProxyRecorder {
	t.Helper()
	recorder := &cmdProxyRecorder{}
	recorder.server = httptest.NewServer(http.HandlerFunc(recorder.handle))
	t.Cleanup(recorder.server.Close)
	return recorder
}

func (p *cmdProxyRecorder) handle(w http.ResponseWriter, r *http.Request) {
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
		p.observations = append(p.observations, struct{ runID, proxyName string }{request.Content.User.RunID, request.Content.ProxyName})
		p.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"reject": false, "unchange": true}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (p *cmdProxyRecorder) snapshot() []struct{ runID, proxyName string } {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]struct{ runID, proxyName string }(nil), p.observations...)
}

func reserveCmdTCPPort(t *testing.T) int {
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

// startCmdFRPS runs the FRP fork's real server with an HTTP vhost listener,
// so the command's REAL generated subdomain route can serve bytes.
func startCmdFRPS(t *testing.T, bindPort, vhostPort int, subDomainHost, pluginURL string) {
	t.Helper()
	cfg := &v1.ServerConfig{
		BindAddr:      "127.0.0.1",
		BindPort:      bindPort,
		ProxyBindAddr: "127.0.0.1",
		VhostHTTPPort: vhostPort,
		SubDomainHost: subDomainHost,
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
}

// pollCmdVhost polls the vhost listener with the route's Host header until
// the echo body round-trips; an early command exit fails fast with its output.
func pollCmdVhost(t *testing.T, vhostPort int, hostHeader, wantBody string, guard time.Duration, done <-chan *runResult) {
	t.Helper()
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(guard)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case res := <-done:
			t.Fatalf("connector run exited during the round-trip: code=%d\nstdout: %s\nstderr: %s", res.code, res.stdout.String(), res.stderr.String())
		default:
		}
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(vhostPort)+"/", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = hostHeader
		response, err := client.Do(req)
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
	t.Fatalf("round-trip through the command-built tunnel never returned the echo body: %v", lastErr)
}

// TestConnectorRunHermeticServeAndGracefulStop is the command-level serve
// proof: `qurl connector run` resolves the slug against the producer,
// generates the managed route, knocks (loopback), logs into the in-process
// FRP server under the native cycle RunID, serves HTTP bytes from the local
// app through the vhost route, and stops gracefully on the simulated signal
// with the Interrupted exit code.
func TestConnectorRunHermeticServeAndGracefulStop(t *testing.T) {
	if testing.Short() {
		t.Skip("hermetic connector serve is the slowest cmd test")
	}
	skipWithoutPinnedState(t)
	connectorTestEnv(t)
	// Deterministic replica salt so the registered proxy name is assertable.
	t.Setenv(replica.EnvReplicaID, "replica-a")
	// Ambient token: the fake open asserts the command does NOT copy it into
	// the agent config (the env is the agent package's to read, never argv's).
	t.Setenv(agent.EnvEnrollmentToken, "one-shot-cmd-token")

	const echoBody = "connector-cmd-roundtrip"
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, echoBody)
	}))
	t.Cleanup(echo.Close)
	echoURL, err := url.Parse(echo.URL)
	if err != nil {
		t.Fatal(err)
	}

	row := mintConnectorRow(t, "cmd-slug")
	producer := newConnectorProducer(t, row)
	recorder := newCmdProxyRecorder(t)
	frpsPort := reserveCmdTCPPort(t)
	vhostPort := reserveCmdTCPPort(t)
	startCmdFRPS(t, frpsPort, vhostPort, "hermetic.test", recorder.server.URL)

	knocker := &cmdCycleKnocker{resourceHost: "localhost:" + strconv.Itoa(frpsPort)}
	var knockerMu sync.Mutex
	gotKnockResourceID := ""

	stateDir := t.TempDir()
	configDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan *runResult, 1)
	go func() {
		done <- runCLI(t, &runOpts{
			ctx: ctx,
			args: []string{
				"--endpoint", producer.URL, "connector", "run",
				"--id", "cmd-slug", "--target", ":" + echoURL.Port(),
				"--state-dir", stateDir,
			},
			env:           map[string]string{},
			configDir:     configDir,
			syncStreams:   true,
			connectorOpen: fakeConnectorOpen(t, producer.URL),
			newKnocker: func(_ *agent.Runtime, knockResourceID string) (connectorKnocker, error) {
				knockerMu.Lock()
				gotKnockResourceID = knockResourceID
				knocker.resourceID = knockResourceID
				knockerMu.Unlock()
				return knocker, nil
			},
			connectorTune: func(cfg *supervisor.Config) {
				cfg.MinBackoff = 5 * time.Millisecond
				cfg.MaxBackoff = 25 * time.Millisecond
				cfg.MinKnockInterval = time.Millisecond
			},
		})
	}()

	// Request through the tunnel: the vhost route is the routing identity the
	// producer issued, never the slug.
	pollCmdVhost(t, vhostPort, row.ConnectorRoutingID+".hermetic.test", echoBody, 30*time.Second, done)

	// Simulated INT/TERM → graceful stop.
	cancel()
	var res *runResult
	select {
	case res = <-done:
	case <-time.After(supervisor.StopWait + 10*time.Second):
		t.Fatal("connector run did not exit after cancellation")
	}

	if res.code != 130 {
		t.Fatalf("exit = %d, want 130 (Interrupted) after a graceful signal stop\nstderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
	stderr := res.stderr.String()
	for _, want := range []string{`Starting Connector "cmd-slug"`, "127.0.0.1:" + echoURL.Port(), "Stopped."} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}

	knockerMu.Lock()
	gotID := gotKnockResourceID
	knockerMu.Unlock()
	if gotID != row.KnockResourceID {
		t.Errorf("command passed knock resource %q, want the producer-assigned %q", gotID, row.KnockResourceID)
	}
	begun, ended, closed := knocker.stats()
	if len(begun) == 0 || len(begun) != len(ended) {
		t.Errorf("native cycles begun %d ended %d, want every begun cycle ended", len(begun), len(ended))
	}
	if !closed {
		t.Error("the command never Closed the knocker (device runtime would leak)")
	}

	observations := recorder.snapshot()
	if len(observations) == 0 {
		t.Fatal("no server-side NewProxy admission observed")
	}
	wantProxy := "cmd-slug-replica-a"
	for i, observation := range observations {
		if observation.proxyName != wantProxy {
			t.Errorf("NewProxy[%d] name = %q, want %q (slug + replica salt)", i, observation.proxyName, wantProxy)
		}
		if err := qurl.ValidateCycleRunID(observation.runID); err != nil {
			t.Errorf("NewProxy[%d] RunID %q not canonical: %v", i, observation.runID, err)
		}
		if observation.runID != begun[0] {
			t.Errorf("NewProxy[%d] RunID = %q, want the presented cycle RunID %q", i, observation.runID, begun[0])
		}
	}
	if producer.requestCount() == 0 {
		t.Error("the slug was never resolved against the producer")
	}
}

// TestConnectorRunTokenAbsentRefusesWithZeroNetwork drives the PRODUCTION
// agent.Open ladder: no stored identity, no token env → the Auth exit with
// remediation naming the env surface, before any network request.
func TestConnectorRunTokenAbsentRefusesWithZeroNetwork(t *testing.T) {
	skipWithoutPinnedState(t)
	connectorTestEnv(t)
	producer := newConnectorProducer(t)

	res := runCLI(t, &runOpts{
		args: []string{"--endpoint", producer.URL, "connector", "run",
			"--id", "cmd-slug", "--target", ":8080", "--state-dir", t.TempDir()},
		env: map[string]string{},
	})
	if res.code != 4 {
		t.Fatalf("exit = %d, want 4 (Auth); stderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
	for _, want := range []string{"isn't enrolled", "QURL_CONNECTOR_TOKEN", "QURL_CONNECTOR_TOKEN_FILE"} {
		if !strings.Contains(res.stderr.String(), want) {
			t.Errorf("stderr missing %q:\n%s", want, res.stderr.String())
		}
	}
	if got := producer.requestCount(); got != 0 {
		t.Errorf("producer requests = %d, want 0 (the refusal must be zero-network)", got)
	}
}

// TestConnectorRunRefreshGates drives the PRODUCTION refresh ladder through
// the command: the manual gate is a missing confirmation (2), disabled is
// configuration (3), a flag typo is usage (2), and an environment typo is
// configuration (3). None of them touch the network.
func TestConnectorRunRefreshGates(t *testing.T) {
	skipWithoutPinnedState(t)

	armStateDir := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		store, err := state.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = store.Close() }()
		if err := store.RequestRefresh("test outage episode"); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	t.Run("manual gate is exit 2 with approval guidance", func(t *testing.T) {
		connectorTestEnv(t)
		producer := newConnectorProducer(t)
		res := runCLI(t, &runOpts{
			args: []string{"--endpoint", producer.URL, "connector", "run",
				"--id", "cmd-slug", "--target", ":8080", "--state-dir", armStateDir(t)},
			env: map[string]string{},
		})
		if res.code != 2 {
			t.Fatalf("exit = %d, want 2; stderr: %s", res.code, res.stderr.String())
		}
		for _, want := range []string{"explicit approval", "--refresh-mode auto"} {
			if !strings.Contains(res.stderr.String(), want) {
				t.Errorf("stderr missing %q:\n%s", want, res.stderr.String())
			}
		}
		if got := producer.requestCount(); got != 0 {
			t.Errorf("producer requests = %d, want 0", got)
		}
	})

	t.Run("disabled flag is exit 3", func(t *testing.T) {
		connectorTestEnv(t)
		res := runCLI(t, &runOpts{
			args: []string{"connector", "run", "--id", "cmd-slug", "--target", ":8080",
				"--state-dir", armStateDir(t), "--refresh-mode", "disabled"},
			env: map[string]string{},
		})
		if res.code != 3 {
			t.Fatalf("exit = %d, want 3; stderr: %s", res.code, res.stderr.String())
		}
		if !strings.Contains(res.stderr.String(), "disabled by its configuration") {
			t.Errorf("stderr missing the disabled posture:\n%s", res.stderr.String())
		}
	})

	t.Run("flag typo is usage exit 2", func(t *testing.T) {
		connectorTestEnv(t)
		res := runCLI(t, &runOpts{
			args: []string{"connector", "run", "--id", "cmd-slug", "--target", ":8080",
				"--state-dir", t.TempDir(), "--refresh-mode", "sometimes"},
			env: map[string]string{},
		})
		if res.code != 2 {
			t.Fatalf("exit = %d, want 2; stderr: %s", res.code, res.stderr.String())
		}
		if !strings.Contains(res.stderr.String(), "--refresh-mode must be manual, auto, or disabled") {
			t.Errorf("stderr missing the flag guidance:\n%s", res.stderr.String())
		}
	})

	t.Run("environment typo is configuration exit 3", func(t *testing.T) {
		connectorTestEnv(t)
		t.Setenv(agent.EnvRefreshMode, "sometimes")
		res := runCLI(t, &runOpts{
			args: []string{"connector", "run", "--id", "cmd-slug", "--target", ":8080",
				"--state-dir", t.TempDir()},
			env: map[string]string{},
		})
		if res.code != 3 {
			t.Fatalf("exit = %d, want 3; stderr: %s", res.code, res.stderr.String())
		}
		if !strings.Contains(res.stderr.String(), agent.EnvRefreshMode) {
			t.Errorf("stderr must name %s:\n%s", agent.EnvRefreshMode, res.stderr.String())
		}
		// The customer rendering, not the raw operator string: headline
		// parity with the other seven connector sentinels.
		if !strings.Contains(res.stderr.String(), "recognized values") {
			t.Errorf("expected the customer headline for an invalid refresh mode, got:\n%s", res.stderr.String())
		}
	})
}

// TestConnectorRunBudgetExhaustionIsUnavailable wires a knocker that never
// succeeds through the real supervisor: the retry budget exhausts, the
// command exits Unavailable (11) with the customer posture, and the refresh
// marker is armed in the real state directory for the next start's ladder.
func TestConnectorRunBudgetExhaustionIsUnavailable(t *testing.T) {
	skipWithoutPinnedState(t)
	connectorTestEnv(t)

	row := mintConnectorRow(t, "cmd-slug")
	producer := newConnectorProducer(t, row)
	knocker := &cmdCycleKnocker{knockErr: errors.New("assigned endpoint unreachable")}
	stateDir := t.TempDir()

	res := runCLI(t, &runOpts{
		args: []string{"--endpoint", producer.URL, "connector", "run",
			"--id", "cmd-slug", "--target", ":8080", "--state-dir", stateDir},
		env:           map[string]string{},
		connectorOpen: fakeConnectorOpen(t, producer.URL),
		newKnocker: func(_ *agent.Runtime, knockResourceID string) (connectorKnocker, error) {
			knocker.resourceID = knockResourceID
			return knocker, nil
		},
		connectorTune: func(cfg *supervisor.Config) {
			cfg.MinBackoff = time.Millisecond
			cfg.MaxBackoff = 2 * time.Millisecond
			cfg.MinKnockInterval = time.Millisecond
			cfg.MaxConsecutiveKnockFailures = 2
		},
	})
	if res.code != 11 {
		t.Fatalf("exit = %d, want 11 (Unavailable); stderr: %s", res.code, res.stderr.String())
	}
	mustEmptyStdout(t, res)
	if !strings.Contains(res.stderr.String(), "kept refusing or not answering") {
		t.Errorf("stderr missing the retry-budget posture:\n%s", res.stderr.String())
	}

	store, err := state.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	marker, present, err := store.LoadRefreshMarker()
	if err != nil {
		t.Fatal(err)
	}
	if !present || marker.Attempted {
		t.Fatalf("marker = (%+v, present=%v), want an armed unattempted episode for the next start", marker, present)
	}
	if _, _, closed := knocker.stats(); !closed {
		t.Error("the command never Closed the knocker after the budget exit")
	}
}

// TestConnectorRunIDResolution pins the flag > env > profile chain for the
// Connector ID by driving each source past ID validation into the
// (zero-network) token-required refusal, including every deprecated v1.1.0
// alias surface (--slug, QURL_CONNECTOR_SLUG, connector_slug) and the
// flag-conflict refusal. Tier-internal precedence (ID names over their
// deprecated twins) is pinned value-exactly by TestResolveConnectorIDLadder.
func TestConnectorRunIDResolution(t *testing.T) {
	skipWithoutPinnedState(t)

	// pastIDValidation asserts an invocation resolved SOME id and moved on
	// to the token-required refusal (exit 4) without touching the network.
	pastIDValidation := func(t *testing.T, o *runOpts) {
		t.Helper()
		res := runCLI(t, o)
		if res.code != 4 {
			t.Fatalf("exit = %d, want 4 (past ID validation into token-required); stderr: %s", res.code, res.stderr.String())
		}
	}

	t.Run("missing everywhere is usage", func(t *testing.T) {
		connectorTestEnv(t)
		res := runCLI(t, &runOpts{
			args: []string{"connector", "run", "--target", ":8080", "--state-dir", t.TempDir()},
			env:  map[string]string{},
		})
		if res.code != 2 || !strings.Contains(res.stderr.String(), "--id is required") {
			t.Fatalf("exit = %d stderr = %q, want the ID usage refusal", res.code, res.stderr.String())
		}
	})

	t.Run("deprecated flag alias still works", func(t *testing.T) {
		connectorTestEnv(t)
		pastIDValidation(t, &runOpts{
			args: []string{"connector", "run", "--slug", "v110-id", "--target", ":8080", "--state-dir", t.TempDir()},
			env:  map[string]string{},
		})
	})

	t.Run("redundant flag and alias agree", func(t *testing.T) {
		connectorTestEnv(t)
		pastIDValidation(t, &runOpts{
			args: []string{"connector", "run", "--id", "same-id", "--slug", "same-id", "--target", ":8080", "--state-dir", t.TempDir()},
			env:  map[string]string{},
		})
	})

	t.Run("flag and alias disagreeing is usage", func(t *testing.T) {
		connectorTestEnv(t)
		res := runCLI(t, &runOpts{
			args: []string{"connector", "run", "--id", "one", "--slug", "two", "--target", ":8080", "--state-dir", t.TempDir()},
			env:  map[string]string{},
		})
		if res.code != 2 {
			t.Fatalf("exit = %d, want 2 (usage) for the flag/alias conflict; stderr: %s", res.code, res.stderr.String())
		}
		for _, want := range []string{`"one"`, `"two"`, "deprecated alias", "pass only --id"} {
			if !strings.Contains(res.stderr.String(), want) {
				t.Errorf("conflict refusal missing %q:\n%s", want, res.stderr.String())
			}
		}
	})

	t.Run("environment supplies it", func(t *testing.T) {
		connectorTestEnv(t)
		pastIDValidation(t, &runOpts{
			args: []string{"connector", "run", "--target", ":8080", "--state-dir", t.TempDir()},
			env:  map[string]string{"QURL_CONNECTOR_ID": "env-id"},
		})
	})

	t.Run("deprecated environment name still works", func(t *testing.T) {
		connectorTestEnv(t)
		pastIDValidation(t, &runOpts{
			args: []string{"connector", "run", "--target", ":8080", "--state-dir", t.TempDir()},
			env:  map[string]string{"QURL_CONNECTOR_SLUG": "env-v110-id"},
		})
	})

	t.Run("profile supplies it", func(t *testing.T) {
		connectorTestEnv(t)
		configDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("connector_id: profile-id\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		pastIDValidation(t, &runOpts{
			args:      []string{"connector", "run", "--target", ":8080", "--state-dir", t.TempDir()},
			env:       map[string]string{},
			configDir: configDir,
		})
	})

	t.Run("deprecated profile key still works", func(t *testing.T) {
		connectorTestEnv(t)
		configDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("connector_slug: profile-v110-id\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		pastIDValidation(t, &runOpts{
			args:      []string{"connector", "run", "--target", ":8080", "--state-dir", t.TempDir()},
			env:       map[string]string{},
			configDir: configDir,
		})
	})
}

// TestResolveConnectorIDLadder pins the resolution ladder value-exactly at
// the unit seam: flag (--id merged with its deprecated --slug alias) > env
// (QURL_CONNECTOR_ID > QURL_CONNECTOR_SLUG) > profile (connector_id >
// connector_slug). The deprecated names sit below their ID twins WITHIN a
// tier but keep their tier's rank — the v1.1.0 env still beats any profile
// value, which is exactly the "precedence unchanged" contract.
func TestResolveConnectorIDLadder(t *testing.T) {
	env := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}

	cases := []struct {
		name        string
		flags       connectorRunFlags
		env         map[string]string
		profileID   string
		profileSlug string
		want        string
		wantErr     string
	}{
		{name: "flag wins over everything",
			flags:     connectorRunFlags{id: "flag-id"},
			env:       map[string]string{"QURL_CONNECTOR_ID": "env-id", "QURL_CONNECTOR_SLUG": "env-old"},
			profileID: "prof-id", profileSlug: "prof-old", want: "flag-id"},
		{name: "alias flag counts as the flag tier",
			flags: connectorRunFlags{slugAlias: "alias-id"},
			env:   map[string]string{"QURL_CONNECTOR_ID": "env-id"},
			want:  "alias-id"},
		{name: "flag and alias agreeing is redundant not fatal",
			flags: connectorRunFlags{id: "same", slugAlias: "same"}, want: "same"},
		{name: "flag and alias disagreeing is refused",
			flags:   connectorRunFlags{id: "one", slugAlias: "two"},
			wantErr: "pass only --id"},
		{name: "env ID beats deprecated env",
			env:  map[string]string{"QURL_CONNECTOR_ID": "env-id", "QURL_CONNECTOR_SLUG": "env-old"},
			want: "env-id"},
		{name: "deprecated env beats any profile value",
			env:       map[string]string{"QURL_CONNECTOR_SLUG": "env-old"},
			profileID: "prof-id", want: "env-old"},
		{name: "profile ID beats deprecated profile key",
			profileID: "prof-id", profileSlug: "prof-old", want: "prof-id"},
		{name: "deprecated profile key is the last resort",
			profileSlug: "prof-old", want: "prof-old"},
		{name: "nothing anywhere resolves empty",
			want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := &globalOpts{
				lookupEnv:            env(tc.env),
				profileConnectorID:   tc.profileID,
				profileConnectorSlug: tc.profileSlug,
			}
			flags := tc.flags
			got, err := resolveConnectorID(opts, &flags)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("resolved ID = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestConnectorRunTargetValidation pins the --target grammar as usage errors
// that fire before anything else runs (no state dir, no environment needed).
func TestConnectorRunTargetValidation(t *testing.T) {
	connectorTestEnv(t)
	cases := map[string][]string{
		"missing":         {"connector", "run", "--id", "s"},
		"bare host":       {"connector", "run", "--id", "s", "--target", "localhost"},
		"port zero":       {"connector", "run", "--id", "s", "--target", ":0"},
		"port too big":    {"connector", "run", "--id", "s", "--target", ":70000"},
		"not a port":      {"connector", "run", "--id", "s", "--target", "localhost:http"},
		"whitespace only": {"connector", "run", "--id", "s", "--target", "   "},
		"group unknown":   {"connector", "frobnicate"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			res := runCLI(t, &runOpts{args: args, env: map[string]string{}})
			if res.code != 2 {
				t.Fatalf("exit = %d, want 2; stderr: %s", res.code, res.stderr.String())
			}
		})
	}

	t.Run("group bare shows help", func(t *testing.T) {
		res := runCLI(t, &runOpts{args: []string{"connector"}, env: map[string]string{}})
		if res.code != 0 || !strings.Contains(res.stdout.String(), "run") {
			t.Fatalf("bare group: exit = %d stdout = %q, want help listing run", res.code, res.stdout.String())
		}
	})
}

// TestConnectorRunClosedStdinNeverHangs: connector run never reads stdin, so
// a closed pipe must change nothing — the token-required refusal still lands
// promptly (a regression here times the whole suite out).
func TestConnectorRunClosedStdinNeverHangs(t *testing.T) {
	skipWithoutPinnedState(t)
	connectorTestEnv(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })

	res := runCLI(t, &runOpts{
		args:  []string{"connector", "run", "--id", "s", "--target", ":8080", "--state-dir", t.TempDir()},
		env:   map[string]string{},
		stdin: r,
	})
	if res.code != 4 {
		t.Fatalf("exit = %d, want the prompt-free token refusal; stderr: %s", res.code, res.stderr.String())
	}
}

// TestConnectorRunEnrollmentFailuresAreCustomerLanguage drives the whole
// command — the real error rendering and the real exit-code table — with the
// enrollment/assignment failures the live Hub produces. The errors are
// injected at the runtime-open seam already wrapped the way the enroll path
// wraps them, so each row proves the customer rendering survives wrapping
// rather than only matching a bare sentinel.
//
// The request-rejected row is the regression guard for the reported defect: a
// deliberately invalid enrollment token against the sandbox Hub printed
// qurl-go's own remedy sentence, which names a Go SDK option and blames the
// wrong thing.
func TestConnectorRunEnrollmentFailuresAreCustomerLanguage(t *testing.T) {
	skipWithoutPinnedState(t)

	const sdk52109 = "qurl: native Hub assignment request rejected (52109); " +
		"correct WithAgentRuntimeIdentity or the Hub request contract before retrying"

	cases := []struct {
		name     string
		err      error
		wantCode int
		want     []string
		banned   []string
	}{
		{
			name:     "request rejected loses the SDK remedy",
			err:      fmt.Errorf("%s: %w", sdk52109, qurl.ErrAssignmentRequestRejected),
			wantCode: 8,
			want:     []string{"refused this Connector's enrollment request", "created for a different Connector"},
			banned:   []string{"WithAgentRuntimeIdentity", "52109", "request contract"},
		},
		{
			name:     "bootstrap consumed steers to a new token",
			err:      fmt.Errorf("native registration: %w", qurl.ErrAssignmentBootstrapConsumed),
			wantCode: 4,
			want:     []string{"already been used", "work exactly once", "QURL_CONNECTOR_TOKEN", "--state-dir"},
		},
		{
			name:     "key rejected steers to a new token",
			err:      fmt.Errorf("native registration: %w", qurl.ErrAssignmentKeyRejected),
			wantCode: 4,
			want:     []string{"didn't accept this machine's enrollment token", "mistyped, expired, revoked"},
		},
		{
			name:     "registration disabled is an entitlement refusal",
			err:      fmt.Errorf("native registration: %w", qurl.ErrAssignmentRegistrationDisabled),
			wantCode: 6,
			want:     []string{"isn't accepting new Connector enrollments", "administrator"},
		},
		{
			name:     "quota exceeded does not promise a retry will work",
			err:      fmt.Errorf("native registration: %w", qurl.ErrAssignmentQuotaExceeded),
			wantCode: 6,
			want:     []string{"limit on enrolled Connectors", "retire a Connector"},
		},
		{
			name:     "identity rejected names the override to drop",
			err:      fmt.Errorf("native registration: %w", qurl.ErrAssignmentIdentityRejected),
			wantCode: 4,
			want:     []string{"refused the Connector identity", "LAYERV_AGENT_ID"},
		},
		{
			name:     "rate limited is the busy story on exit 9",
			err:      fmt.Errorf("native registration: %w", qurl.ErrAssignmentRateLimited),
			wantCode: 9,
			want:     []string{"couldn't give this Connector its platform assignment", "again in a few minutes"},
		},
		{
			name:     "unavailable shares the busy story on exit 11",
			err:      fmt.Errorf("native registration: %w", qurl.ErrAssignmentUnavailable),
			wantCode: 11,
			want:     []string{"couldn't give this Connector its platform assignment"},
		},
		{
			name:     "invalid response is a platform-side fault",
			err:      fmt.Errorf("native registration: %w", qurl.ErrAssignmentInvalidResponse),
			wantCode: 10,
			want:     []string{"can't accept", "problem on the qURL platform side"},
		},
		{
			name:     "expired lease beats the invalid-response reading it also carries",
			err:      fmt.Errorf("%w: assignment lease must be in the future: %w", qurl.ErrAssignmentInvalidResponse, qurl.ErrAssignmentLeaseExpired),
			wantCode: 11,
			want:     []string{"platform assignment has expired"},
			banned:   []string{"problem on the qURL platform side"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			connectorTestEnv(t)
			producer := newConnectorProducer(t)
			res := runCLI(t, &runOpts{
				args: []string{"--endpoint", producer.URL, "connector", "run",
					"--id", "billing", "--target", ":8080", "--state-dir", t.TempDir()},
				env: map[string]string{},
				connectorOpen: func(context.Context, *agent.Config) (*agent.Runtime, error) {
					return nil, tc.err
				},
			})
			if res.code != tc.wantCode {
				t.Fatalf("exit = %d, want %d\nstderr: %s", res.code, tc.wantCode, res.stderr.String())
			}
			mustEmptyStdout(t, res)
			stderr := res.stderr.String()
			for _, want := range tc.want {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr missing %q:\n%s", want, stderr)
				}
			}
			for _, banned := range tc.banned {
				if strings.Contains(stderr, banned) {
					t.Errorf("stderr leaked %q to the customer:\n%s", banned, stderr)
				}
			}
			// Every rendering must carry a next step, never a bare restatement.
			if !strings.Contains(stderr, "Hint:") {
				t.Errorf("no hint offered:\n%s", stderr)
			}
		})
	}
}

// TestConnectorGoldens pins the rendered bytes of the connector help and
// error anatomies. Kept beside the connector suite (not in TestGoldens)
// because these cases need the real-process-environment pinning and the
// pinned-state skip that the shared golden table has no seams for.
func TestConnectorGoldens(t *testing.T) {
	goldenDir, err := filepath.Abs(filepath.Join("testdata", "golden"))
	if err != nil {
		t.Fatal(err)
	}

	armStateDir := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		store, err := state.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = store.Close() }()
		if err := store.RequestRefresh("sustained connection failures"); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	cases := []struct {
		name       string
		args       func(t *testing.T) []string
		variants   []string
		wantCode   int
		needsState bool
		darkHub    bool
		stdout     bool // golden the data stream instead of stderr
	}{
		{
			name:     "connector_help",
			args:     func(*testing.T) []string { return []string{"connector"} },
			variants: []string{"tty", "plain"},
			stdout:   true,
		},
		{
			name:     "connector_run_help",
			args:     func(*testing.T) []string { return []string{"connector", "run", "--help"} },
			variants: []string{"plain"},
			stdout:   true,
		},
		{
			name: "error_connector_token",
			args: func(t *testing.T) []string {
				return []string{"connector", "run", "--id", "billing", "--target", ":8080", "--state-dir", t.TempDir()}
			},
			variants:   []string{"tty", "plain", "json"},
			wantCode:   4,
			needsState: true,
		},
		{
			name: "error_connector_refresh_manual",
			args: func(t *testing.T) []string {
				return []string{"connector", "run", "--id", "billing", "--target", ":8080", "--state-dir", armStateDir(t)}
			},
			variants:   []string{"plain"},
			wantCode:   2,
			needsState: true,
		},
		{
			name: "error_connector_hub",
			args: func(t *testing.T) []string {
				return []string{"connector", "run", "--id", "billing", "--target", ":8080", "--state-dir", t.TempDir()}
			},
			variants:   []string{"plain"},
			wantCode:   3,
			needsState: true,
			darkHub:    true,
		},
	}

	for _, tc := range cases {
		for _, variant := range tc.variants {
			t.Run(tc.name+"_"+variant, func(t *testing.T) {
				if tc.needsState {
					skipWithoutPinnedState(t)
				}
				connectorTestEnv(t)
				if tc.darkHub {
					unsetHubTriple(t)
				}
				o := &runOpts{
					args: tc.args(t),
					env:  map[string]string{},
					tty:  variant == "tty",
				}
				if variant == "json" {
					o.args = append([]string{"-o", "json"}, o.args...)
				}
				res := runCLI(t, o)
				if res.code != tc.wantCode {
					t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", res.code, tc.wantCode, res.stdout.String(), res.stderr.String())
				}
				if tc.stdout {
					clitest.GoldenAt(t, filepath.Join(goldenDir, tc.name+"."+variant+".golden"), res.stdout.Bytes())
					if res.stderr.Len() != 0 {
						t.Fatalf("stderr must be empty, got %q", res.stderr.String())
					}
					return
				}
				clitest.GoldenAt(t, filepath.Join(goldenDir, tc.name+"."+variant+".stderr.golden"), res.stderr.Bytes())
				mustEmptyStdout(t, res)
			})
		}
	}
}
