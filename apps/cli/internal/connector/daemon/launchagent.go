package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	connectorservice "github.com/layervai/qurl-connector/pkg/service"
	qurl "github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	connectorhub "github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/sessionrelay"
)

// DaemonJobLabel is the stable per-user background-job identifier.
const DaemonJobLabel = "ai.layerv.qurl.share-daemon"
const daemonJobProtocolVersion = "2"

// JobController installs, upgrades, and signals the per-user daemon job.
type JobController struct {
	Manager             connectorservice.UserJobManager
	IPC                 IPCClient
	StateDir            string
	LogDir              string
	BinaryVersion       string
	InvocationPath      string
	Endpoint            string
	ResolveHub          func() (qurl.HubBootstrap, error)
	ResolveSessionRelay func() (string, error)
	LookPath            func(string) (string, error)
	ProbeStatus         func(context.Context) (IPCStatus, bool, error)
	Reload              func(context.Context) (bool, error)
}

// NewJobController builds the production native per-user job controller.
func NewJobController(stateDir, logDir, binaryVersion, endpoint string, resolveHub func() (qurl.HubBootstrap, error), resolveSessionRelay func() (string, error)) *JobController {
	controller := &JobController{
		Manager:  connectorservice.NewUserJobManager(),
		IPC:      IPCClient{SocketPath: StateSocketPath(stateDir)},
		StateDir: stateDir, LogDir: logDir, BinaryVersion: strings.TrimSpace(binaryVersion),
		InvocationPath: os.Args[0], Endpoint: endpoint, ResolveHub: resolveHub,
		ResolveSessionRelay: resolveSessionRelay, LookPath: exec.LookPath,
	}
	controller.ProbeStatus = controller.IPC.Status
	controller.Reload = controller.IPC.ReloadIfRunning
	return controller
}

// Ensure reloads a compatible live daemon or installs the current job definition.
func (c *JobController) Ensure(ctx context.Context) error {
	if err := c.validateController(); err != nil {
		return err
	}
	status, running, err := c.ProbeStatus(ctx)
	if err != nil {
		return err
	}
	expectedJobVersion, err := JobVersion(c.BinaryVersion)
	if err != nil {
		return err
	}
	if running && status.JobVersion == expectedJobVersion {
		reloaded, err := c.Reload(ctx)
		if err != nil {
			return err
		}
		if reloaded {
			return nil
		}
		// The compatible owner exited between the status and reload calls.
		// Continue as an absent owner instead of reporting false convergence.
		running = false
	}
	if running {
		managed, err := c.Manager.Status(DaemonJobLabel)
		if err != nil {
			return fmt.Errorf("inspect native background ownership before replacing incompatible share daemon: %w", err)
		}
		if !managed.Installed || !managed.Running {
			return fmt.Errorf(
				"share daemon version %q does not match qURL version %q; stop the foreground or externally managed daemon and retry",
				status.JobVersion, expectedJobVersion,
			)
		}
	}
	hub, sessionRelayURL, err := c.validatedDeployment()
	if err != nil {
		return err
	}
	job, err := c.jobDefinition(hub, sessionRelayURL, expectedJobVersion)
	if err != nil {
		return err
	}
	if running {
		// IPC proved a resident daemon is on an incompatible protocol/binary
		// version and the native manager proved that it owns that process. Force
		// replacement even if an interrupted upgrade already wrote the current
		// job definition around the old process.
		return c.Manager.Replace(job)
	}
	// A successful native job install transfers ownership to the durable daemon.
	// Native assignment recovery can legitimately outlive a CLI readiness
	// deadline, so serving convergence is observed through the control plane.
	return c.Manager.Ensure(job)
}

func (c *JobController) validateController() error {
	if c == nil || c.Manager == nil || c.LookPath == nil || c.ProbeStatus == nil || c.Reload == nil ||
		c.ResolveHub == nil || c.ResolveSessionRelay == nil {
		return errors.New("share daemon job controller is incomplete")
	}
	return nil
}

func (c *JobController) validatedDeployment() (qurl.HubBootstrap, string, error) {
	if c.Endpoint == "" || c.Endpoint != strings.TrimSpace(c.Endpoint) {
		return qurl.HubBootstrap{}, "", errors.New("share daemon API endpoint is empty or non-canonical")
	}
	// The endpoint is persisted in an owner-readable plist. Reuse the native
	// resource origin validator so userinfo, query, and fragment data can never
	// turn that durable non-secret job definition into a credential store.
	if _, err := agent.ResourceSDKOrigin(c.Endpoint); err != nil {
		return qurl.HubBootstrap{}, "", err
	}
	hub, err := c.ResolveHub()
	if err != nil {
		return qurl.HubBootstrap{}, "", err
	}
	if err := connectorhub.ValidateBootstrap(hub); err != nil {
		return qurl.HubBootstrap{}, "", err
	}
	relayURL, err := c.ResolveSessionRelay()
	if err != nil {
		return qurl.HubBootstrap{}, "", err
	}
	if relayURL != "" {
		if err := sessionrelay.Validate(relayURL); err != nil {
			return qurl.HubBootstrap{}, "", err
		}
	}
	return hub, relayURL, nil
}

func (c *JobController) jobDefinition(hub qurl.HubBootstrap, sessionRelayURL, jobVersion string) (connectorservice.UserJob, error) {
	binary, err := c.currentExecutablePath()
	if err != nil {
		return connectorservice.UserJob{}, err
	}
	if err := prepareDaemonLogDir(c.LogDir); err != nil {
		return connectorservice.UserJob{}, err
	}
	stdoutPath := filepath.Join(c.LogDir, "share-daemon.log")
	stderrPath := filepath.Join(c.LogDir, "share-daemon.err.log")
	arguments := make([]string, 0, 20)
	arguments = append(arguments,
		"--endpoint", c.Endpoint,
		"daemon", "run", "--state-dir", c.StateDir, "--job-version", jobVersion,
		"--hub-host", hub.Host, "--hub-port", strconv.Itoa(hub.Port),
		"--hub-server-public-key-b64", hub.ServerPublicKeyB64,
	)
	if sessionRelayURL != "" {
		arguments = append(arguments, "--session-relay-url", sessionRelayURL)
	}
	arguments = append(arguments, daemonJobLogArguments(stdoutPath, stderrPath)...)
	return connectorservice.UserJob{
		Label: DaemonJobLabel, BinaryPath: binary,
		Arguments: arguments, StandardOut: stdoutPath, StandardErr: stderrPath,
		ExitTimeout: 15, Umask: 0o077, RunAtLoad: true, KeepAlive: true,
	}, nil
}

// currentExecutablePath returns the command path that launched this process,
// not another qurl binary that happens to appear first on PATH. A path-bearing
// invocation is made absolute without resolving symlinks, so a stable Homebrew
// or package-manager link remains stable across upgrades. A bare invocation is
// resolved by its exact name through PATH for the same reason.
func (c *JobController) currentExecutablePath() (string, error) {
	invocation := c.InvocationPath
	if invocation == "" || invocation != strings.TrimSpace(invocation) {
		return "", errors.New("qURL invocation path is empty or non-canonical")
	}
	if filepath.IsAbs(invocation) {
		return filepath.Clean(invocation), nil
	}
	if filepath.Dir(invocation) != "." {
		binary, err := filepath.Abs(invocation)
		if err != nil {
			return "", fmt.Errorf("resolve current qURL command path: %w", err)
		}
		return binary, nil
	}
	binary, err := c.LookPath(invocation)
	if err != nil {
		return "", fmt.Errorf("find current qURL command %q: %w", invocation, err)
	}
	if !filepath.IsAbs(binary) {
		binary, err = filepath.Abs(binary)
		if err != nil {
			return "", fmt.Errorf("resolve current qURL command path: %w", err)
		}
	}
	return filepath.Clean(binary), nil
}

// JobVersion combines the IPC protocol and installed qurl binary versions.
func JobVersion(binaryVersion string) (string, error) {
	binaryVersion = strings.TrimSpace(binaryVersion)
	if binaryVersion == "" {
		return "", errors.New("qURL binary version is empty")
	}
	return daemonJobProtocolVersion + "/" + binaryVersion, nil
}

// ReloadIfRunning reconciles an existing daemon without starting one.
func (c *JobController) ReloadIfRunning(ctx context.Context) (bool, error) {
	return c.IPC.ReloadIfRunning(ctx)
}

// Status returns the native per-user job's installed and running state.
func (c *JobController) Status() (connectorservice.ServiceStatus, error) {
	if c == nil || c.Manager == nil {
		return connectorservice.ServiceStatus{}, errors.New("share daemon job controller is incomplete")
	}
	return c.Manager.Status(DaemonJobLabel)
}
