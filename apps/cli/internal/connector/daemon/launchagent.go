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
)

// DaemonJobLabel is the stable per-user background-job identifier.
const DaemonJobLabel = "ai.layerv.qurl.share-daemon"

// daemonJobProtocolVersion identifies the persisted service-manager argument
// contract. Increment it for each incompatible shape; do not reuse an earlier
// value even when a later shape resembles it.
const daemonJobProtocolVersion = "3"

// JobController installs, upgrades, and signals the per-user daemon job.
type JobController struct {
	Manager        connectorservice.UserJobManager
	IPC            IPCClient
	StateDir       string
	LogDir         string
	BinaryVersion  string
	InvocationPath string
	Endpoint       string
	// ShareGroupMode is part of the job definition: a resident daemon runs in
	// exactly the mode its job carries, and a changed mode is a definition
	// change that replaces the daemon just as a binary-version change does.
	ShareGroupMode GroupMode
	ResolveHub     func() (qurl.HubBootstrap, error)
	LookPath       func(string) (string, error)
	ProbeStatus    func(context.Context) (IPCStatus, bool, error)
	Reload         func(context.Context) (bool, error)
}

// NewJobController builds the production native per-user job controller.
func NewJobController(stateDir, logDir, binaryVersion, endpoint string, mode GroupMode, resolveHub func() (qurl.HubBootstrap, error)) *JobController {
	controller := &JobController{
		Manager:  connectorservice.NewUserJobManager(),
		IPC:      IPCClient{SocketPath: StateSocketPath(stateDir)},
		StateDir: stateDir, LogDir: logDir, BinaryVersion: strings.TrimSpace(binaryVersion),
		InvocationPath: os.Args[0], Endpoint: endpoint, ShareGroupMode: mode, ResolveHub: resolveHub, LookPath: exec.LookPath,
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
	expectedJobVersion, err := JobVersion(c.BinaryVersion, c.ShareGroupMode)
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
				"share daemon job version %q does not match this qURL job version %q; stop the foreground or externally managed daemon and retry",
				status.JobVersion, expectedJobVersion,
			)
		}
	}
	hub, err := c.validatedDeployment()
	if err != nil {
		return err
	}
	job, err := c.jobDefinition(hub, expectedJobVersion)
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
	if c == nil || c.Manager == nil || c.LookPath == nil || c.ProbeStatus == nil || c.Reload == nil || c.ResolveHub == nil {
		return errors.New("share daemon job controller is incomplete")
	}
	return nil
}

func (c *JobController) validatedDeployment() (qurl.HubBootstrap, error) {
	if c.Endpoint == "" || c.Endpoint != strings.TrimSpace(c.Endpoint) {
		return qurl.HubBootstrap{}, errors.New("share daemon API endpoint is empty or non-canonical")
	}
	// The endpoint is persisted in an owner-readable plist. Reuse the native
	// resource origin validator so userinfo, query, and fragment data can never
	// turn that durable non-secret job definition into a credential store.
	if _, err := agent.ResourceSDKOrigin(c.Endpoint); err != nil {
		return qurl.HubBootstrap{}, err
	}
	hub, err := c.ResolveHub()
	if err != nil {
		return qurl.HubBootstrap{}, err
	}
	if err := connectorhub.ValidateBootstrap(hub); err != nil {
		return qurl.HubBootstrap{}, err
	}
	return hub, nil
}

func (c *JobController) jobDefinition(hub qurl.HubBootstrap, jobVersion string) (connectorservice.UserJob, error) {
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
		// The mode is always explicit so the daemon runs in the mode this job
		// version was computed for, whatever its own environment or config file
		// would resolve to.
		"--share-group-mode", string(c.ShareGroupMode),
		"--hub-host", hub.Host, "--hub-port", strconv.Itoa(hub.Port),
		"--hub-server-public-key-b64", hub.ServerPublicKeyB64,
	)
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

// JobVersion combines the IPC protocol version, the installed qurl binary
// version, and the session group mode into the job definition version a
// resident daemon reports over IPC. The default mode is elided, so a
// single-mode job version is exactly the pre-mode string; any other mode is a
// definition change that replaces the resident daemon.
func JobVersion(binaryVersion string, mode GroupMode) (string, error) {
	binaryVersion = strings.TrimSpace(binaryVersion)
	if binaryVersion == "" {
		return "", errors.New("qURL binary version is empty")
	}
	if _, err := ParseGroupMode(string(mode)); err != nil {
		return "", err
	}
	version := daemonJobProtocolVersion + "/" + binaryVersion
	if mode != DefaultGroupMode {
		version += "/" + string(mode)
	}
	return version, nil
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
