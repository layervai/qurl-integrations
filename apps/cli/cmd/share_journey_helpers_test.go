package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	v1 "github.com/fatedier/frp/pkg/config/v1"
	frplog "github.com/fatedier/frp/pkg/util/log"
	frpserver "github.com/fatedier/frp/server"
	goliblog "github.com/fatedier/golib/log"
	golibmux "github.com/fatedier/golib/net/mux"

	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

type cmdFRPSTestService struct {
	service *frpserver.Service
	done    <-chan struct{}
}

var cmdFRPSTestServices struct {
	sync.Mutex
	items []cmdFRPSTestService
}

func openOwnedTestShareRegistry(dir string) (*connectorstate.LocalShareRegistry, error) {
	registry, err := connectorstate.OpenLocalShareRegistry(dir)
	if err != nil {
		return nil, err
	}
	if err := registry.BindOwner(context.Background(), "own_cli_fixture"); err != nil {
		return nil, err
	}
	return registry, nil
}

// TestMain pins FRP's process-global logger before journey-test goroutines
// start. The in-process client and server share this logger.
func TestMain(m *testing.M) {
	frplog.Logger = goliblog.New(
		goliblog.WithCaller(false),
		goliblog.WithLevel(goliblog.WarnLevel),
		goliblog.WithOutput(goliblog.NewConsoleWriter(goliblog.ConsoleConfig{}, io.Discard)),
	)
	code := m.Run()
	if err := closeCmdFRPSTestServices(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "close journey FRP servers: %v\n", err)
		code = 1
	}
	os.Exit(code)
}

func closeCmdFRPSTestServices() error {
	cmdFRPSTestServices.Lock()
	services := append([]cmdFRPSTestService(nil), cmdFRPSTestServices.items...)
	cmdFRPSTestServices.items = nil
	cmdFRPSTestServices.Unlock()
	if len(services) == 0 {
		return nil
	}

	// golib's mux starts one classifier goroutine per accepted connection but
	// does not expose a join. All test clients have stopped when m.Run returns.
	// Its read deadline is therefore the strict upper bound for any classifier
	// that could still be sending to a listener while Service.Close closes it.
	time.Sleep(golibmux.DefaultTimeout + 100*time.Millisecond)
	for _, server := range services {
		if err := server.service.Close(); err != nil {
			return err
		}
	}
	for _, server := range services {
		select {
		case <-server.done:
		case <-time.After(2 * time.Second):
			return errors.New("FRP server did not stop")
		}
	}
	return nil
}

type cmdProxyRecorder struct {
	server *httptest.Server

	mu           sync.Mutex
	observations []cmdProxyObservation
}

type cmdProxyObservation struct{ op, runID, proxyName string }

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
	if request.Op == "NewProxy" || request.Op == "CloseProxy" {
		p.mu.Lock()
		p.observations = append(p.observations, cmdProxyObservation{
			op: request.Op, runID: request.Content.User.RunID, proxyName: request.Content.ProxyName,
		})
		p.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"reject": false, "unchange": true}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (p *cmdProxyRecorder) snapshot() []cmdProxyObservation {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]cmdProxyObservation(nil), p.observations...)
}

func reserveCmdTCPPort(t *testing.T) int {
	t.Helper()
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve loopback TCP port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func startCmdFRPS(t *testing.T, bindPort, vhostPort int, subDomainHost, pluginURL string) {
	t.Helper()
	cfg := &v1.ServerConfig{
		BindAddr: "127.0.0.1", BindPort: bindPort, ProxyBindAddr: "127.0.0.1",
		VhostHTTPPort: vhostPort, SubDomainHost: subDomainHost,
		HTTPPlugins: []v1.HTTPPluginOptions{{
			Name: "proxy-lifecycle-recorder", Addr: pluginURL, Path: "/", Ops: []string{"NewProxy", "CloseProxy"},
		}},
	}
	if err := cfg.Complete(); err != nil {
		t.Fatalf("complete journey server config: %v", err)
	}
	service, err := frpserver.NewService(cfg)
	if err != nil {
		t.Fatalf("construct journey server on 127.0.0.1:%d: %v", bindPort, err)
	}
	done := make(chan struct{})
	go func() {
		service.Run(context.Background())
		close(done)
	}()
	cmdFRPSTestServices.Lock()
	cmdFRPSTestServices.items = append(cmdFRPSTestServices.items, cmdFRPSTestService{service: service, done: done})
	cmdFRPSTestServices.Unlock()
}
