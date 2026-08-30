// Package daemon owns qURL's per-user local sharing process and IPC contract.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

// SocketFile is the fixed logical IPC socket name. Unix uses it directly below
// short state directories and includes it in the identity of a bounded path
// for long state directories.
const SocketFile = "daemon.sock"

// StateSocketPath returns the stable platform IPC address for one state
// namespace. Unix keeps the socket below short state paths and uses a bounded,
// owner-only per-user runtime path when the state path cannot fit sockaddr_un.
// Windows hashes this path into its named-pipe address as before.
func StateSocketPath(stateDir string) string {
	path := filepath.Join(strings.TrimSpace(stateDir), SocketFile)
	return platformStateSocketPath(path)
}

// ErrAlreadyRunning reports a live or ambiguously stale daemon socket.
var ErrAlreadyRunning = errors.New("qURL share daemon is already running")

var probeIPCStatus = func(c IPCClient, ctx context.Context) (*http.Response, bool, error) {
	return c.do(ctx, http.MethodGet, "/status")
}

// The Unix socket path becomes visible between bind and the immediate chmod
// to 0600. WaitReady may retry that one closed startup state, but ordinary
// status and reload calls still fail closed on it.
var errIPCSocketRestrictionPending = errors.New("share daemon socket restriction is not complete")

// IPCServer exposes daemon status and reconciliation over an owner-only socket.
type IPCServer struct {
	SocketPath string
	Manager    *Manager
	JobVersion string
}

// Run serves IPC and the share manager until ctx ends.
func (s *IPCServer) Run(ctx context.Context) (retErr error) {
	if s == nil || s.Manager == nil {
		return errors.New("share daemon IPC server requires a manager")
	}
	path, err := validateSocketPath(s.SocketPath)
	if err != nil {
		return err
	}
	if err := connectorstate.EnsureDirMode(filepath.Dir(path)); err != nil {
		return fmt.Errorf("secure share daemon socket directory: %w", err)
	}
	listener, cleanup, err := listenDaemonIPC(ctx, path)
	if err != nil {
		return err
	}
	defer func() {
		retErr = errors.Join(retErr, listener.Close())
		retErr = errors.Join(retErr, cleanup())
	}()
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /reload", func(w http.ResponseWriter, _ *http.Request) {
		s.Manager.Trigger()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			JobVersion string            `json:"job_version"`
			Running    map[string]string `json:"running"`
		}{JobVersion: s.JobVersion, Running: s.Manager.Running()})
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	managerDone := make(chan error, 1)
	go func() { managerDone <- s.Manager.Run(runCtx) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return errors.Join(server.Shutdown(shutdownCtx), <-managerDone)
	case err := <-managerDone:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return errors.Join(err, server.Shutdown(shutdownCtx))
	case err := <-serveDone:
		cancelRun()
		managerErr := <-managerDone
		if errors.Is(err, http.ErrServerClosed) {
			return managerErr
		}
		return errors.Join(err, managerErr)
	}
}

const ipcRequestTimeout = 5 * time.Second

// IPCClient talks to an already-running local share daemon.
type IPCClient struct {
	SocketPath string
	// requestTimeout is test-only configuration. Production calls use the
	// fixed bounded timeout so an unresponsive daemon cannot block lifecycle
	// commands after their cloud operation has completed.
	requestTimeout time.Duration
}

// IPCStatus is the daemon version handshake and active resource set.
type IPCStatus struct {
	JobVersion string            `json:"job_version"`
	Running    map[string]string `json:"running"`
}

// Status reads the daemon handshake without starting it.
func (c IPCClient) Status(ctx context.Context) (IPCStatus, bool, error) {
	response, running, err := c.do(ctx, http.MethodGet, "/status")
	if err != nil || !running {
		return IPCStatus{}, running, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return IPCStatus{}, true, fmt.Errorf("share daemon status returned HTTP %d", response.StatusCode)
	}
	status, err := decodeIPCStatus(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return IPCStatus{}, true, err
	}
	return status, true, nil
}

func decodeIPCStatus(reader io.Reader) (IPCStatus, error) {
	var status IPCStatus
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&status); err != nil {
		return IPCStatus{}, fmt.Errorf("decode share daemon status: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return IPCStatus{}, errors.New("decode share daemon status: trailing JSON value")
		}
		return IPCStatus{}, fmt.Errorf("decode share daemon status trailing data: %w", err)
	}
	if strings.TrimSpace(status.JobVersion) == "" || status.JobVersion != strings.TrimSpace(status.JobVersion) {
		return IPCStatus{}, errors.New("decode share daemon status: job_version is missing or invalid")
	}
	if status.Running == nil {
		return IPCStatus{}, errors.New("decode share daemon status: running map is missing")
	}
	for resourceID, crid := range status.Running {
		if strings.TrimSpace(resourceID) == "" || resourceID != strings.TrimSpace(resourceID) || strings.TrimSpace(crid) == "" || crid != strings.TrimSpace(crid) {
			return IPCStatus{}, errors.New("decode share daemon status: running resource identity is invalid")
		}
	}
	return status, nil
}

// ReloadIfRunning requests reconciliation without starting an absent daemon.
func (c IPCClient) ReloadIfRunning(ctx context.Context) (bool, error) {
	response, running, err := c.do(ctx, http.MethodPost, "/reload")
	if err != nil || !running {
		return running, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusNoContent {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return true, fmt.Errorf("share daemon reload returned HTTP %d", response.StatusCode)
	}
	return true, nil
}

// WaitReady waits only through explicit absent or refused socket states.
func (c IPCClient) WaitReady(ctx context.Context) error {
	if _, err := validateSocketPath(c.SocketPath); err != nil {
		return err
	}
	delay := 10 * time.Millisecond
	restrictionRetries := 8
	for {
		response, running, err := probeIPCStatus(c, ctx)
		if err != nil {
			if !errors.Is(err, errIPCSocketRestrictionPending) || restrictionRetries == 0 {
				return err
			}
			restrictionRetries--
		}
		if err == nil && response != nil {
			_ = response.Body.Close()
			if !running {
				return errors.New("share daemon readiness returned a response without a running socket")
			}
			if response.StatusCode != http.StatusOK {
				return fmt.Errorf("share daemon readiness returned HTTP %d", response.StatusCode)
			}
			return nil
		}
		if err == nil && running {
			return errors.New("share daemon readiness reported a running socket without a response")
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if delay < 250*time.Millisecond {
			delay *= 2
		}
	}
}

func (c IPCClient) do(ctx context.Context, method, path string) (*http.Response, bool, error) {
	socket, err := validateSocketPath(c.SocketPath)
	if err != nil {
		return nil, false, err
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialDaemonIPC(ctx, socket)
	}}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, method, "http://qurl.local"+path, http.NoBody)
	if err != nil {
		return nil, true, err
	}
	timeout := c.requestTimeout
	if timeout <= 0 {
		timeout = ipcRequestTimeout
	}
	response, err := (&http.Client{Transport: transport, Timeout: timeout}).Do(request)
	if err != nil {
		if isUnavailableIPCError(err) {
			return nil, false, nil
		}
		return nil, true, err
	}
	return response, true, nil
}

func validateSocketPath(raw string) (string, error) {
	path := filepath.Clean(strings.TrimSpace(raw))
	if !filepath.IsAbs(path) || path == string(filepath.Separator) {
		return "", errors.New("share daemon socket path must be absolute")
	}
	if err := validatePlatformIPCPath(path); err != nil {
		return "", err
	}
	return path, nil
}
