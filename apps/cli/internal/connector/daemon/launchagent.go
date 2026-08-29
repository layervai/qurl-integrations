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

// LaunchAgentLabel is the stable per-user launchd service identifier.
const LaunchAgentLabel = "ai.layerv.qurl.share-daemon"
const daemonJobProtocolVersion = "1"

// JobController installs, upgrades, and signals the per-user daemon job.
type JobController struct {
	Manager       connectorservice.UserJobManager
	IPC           IPCClient
	StateDir      string
	LogDir        string
	BinaryVersion string
	Endpoint      string
	ResolveHub    func() (qurl.HubBootstrap, error)
	LookPath      func(string) (string, error)
	ProbeStatus   func(context.Context) (IPCStatus, bool, error)
	Reload        func(context.Context) (bool, error)
}

// NewJobController builds the production launchd controller.
func NewJobController(stateDir, logDir, binaryVersion, endpoint string, resolveHub func() (qurl.HubBootstrap, error)) *JobController {
	controller := &JobController{
		Manager:  connectorservice.NewUserJobManager(),
		IPC:      IPCClient{SocketPath: filepath.Join(stateDir, SocketFile)},
		StateDir: stateDir, LogDir: logDir, BinaryVersion: strings.TrimSpace(binaryVersion),
		Endpoint: endpoint, ResolveHub: resolveHub, LookPath: exec.LookPath,
	}
	controller.ProbeStatus = controller.IPC.Status
	controller.Reload = controller.IPC.ReloadIfRunning
	return controller
}

// Ensure reloads a compatible live daemon or installs the current job definition.
func (c *JobController) Ensure(ctx context.Context) error {
	hub, expectedJobVersion, err := c.validatedDeployment()
	if err != nil {
		return err
	}
	status, running, err := c.ProbeStatus(ctx)
	if err != nil {
		return err
	}
	job, err := c.jobDefinition(hub, expectedJobVersion)
	if err != nil {
		return err
	}
	install := c.Manager.Ensure
	if running && status.JobVersion != expectedJobVersion {
		// IPC proved a resident daemon is on an incompatible protocol/binary
		// version. Force replacement even if a prior interrupted upgrade already
		// wrote the current plist around that old process.
		install = c.Manager.Replace
	}
	if err := install(job); err != nil {
		return err
	}
	if running && status.JobVersion == expectedJobVersion {
		// Manager.Ensure first compares the complete non-secret definition, so
		// an endpoint or Hub change replaces the process instead of reloading an
		// old deployment. A matching process receives the registry-reconcile
		// signal; a just-replaced process performs reconciliation on startup.
		if _, err := c.Reload(ctx); err != nil {
			return err
		}
	}
	// A successful launchd load transfers ownership to the durable daemon.
	// Native assignment recovery can legitimately outlive a CLI readiness
	// deadline, so serving convergence is observed through the control plane.
	return nil
}

func (c *JobController) validatedDeployment() (qurl.HubBootstrap, string, error) {
	if c == nil || c.Manager == nil || c.LookPath == nil || c.ProbeStatus == nil || c.Reload == nil || c.ResolveHub == nil {
		return qurl.HubBootstrap{}, "", errors.New("share daemon LaunchAgent controller is incomplete")
	}
	if c.Endpoint == "" || c.Endpoint != strings.TrimSpace(c.Endpoint) {
		return qurl.HubBootstrap{}, "", errors.New("share daemon API endpoint is empty or non-canonical")
	}
	// The endpoint is persisted in an owner-readable plist. Reuse the native
	// resource origin validator so userinfo, query, and fragment data can never
	// turn that durable non-secret job definition into a credential store.
	if _, err := agent.ResourceSDKOrigin(c.Endpoint); err != nil {
		return qurl.HubBootstrap{}, "", err
	}
	expectedJobVersion, err := JobVersion(c.BinaryVersion)
	if err != nil {
		return qurl.HubBootstrap{}, "", err
	}
	hub, err := c.ResolveHub()
	if err != nil {
		return qurl.HubBootstrap{}, "", err
	}
	if err := connectorhub.ValidateBootstrap(hub); err != nil {
		return qurl.HubBootstrap{}, "", err
	}
	return hub, expectedJobVersion, nil
}

func (c *JobController) jobDefinition(hub qurl.HubBootstrap, jobVersion string) (connectorservice.UserJob, error) {
	binary, err := c.LookPath("qurl")
	if err != nil {
		return connectorservice.UserJob{}, fmt.Errorf("find installed qurl command: %w", err)
	}
	if !filepath.IsAbs(binary) {
		binary, err = filepath.Abs(binary)
		if err != nil {
			return connectorservice.UserJob{}, fmt.Errorf("resolve installed qurl command path: %w", err)
		}
	}
	// Deliberately keep the PATH-resolved Homebrew shim. Resolving its symlink
	// would persist a versioned Cellar path that breaks on the next upgrade.
	if err := os.MkdirAll(c.LogDir, 0o700); err != nil {
		return connectorservice.UserJob{}, fmt.Errorf("create qURL daemon log directory: %w", err)
	}
	if err := os.Chmod(c.LogDir, 0o700); err != nil { // #nosec G302 -- owner-only directory mode, not a file mode.
		return connectorservice.UserJob{}, fmt.Errorf("restrict qURL daemon log directory: %w", err)
	}
	return connectorservice.UserJob{
		Label: LaunchAgentLabel, BinaryPath: binary,
		Arguments: []string{
			"--endpoint", c.Endpoint,
			"daemon", "run", "--state-dir", c.StateDir, "--job-version", jobVersion,
			"--hub-host", hub.Host, "--hub-port", strconv.Itoa(hub.Port),
			"--hub-server-public-key-b64", hub.ServerPublicKeyB64,
		},
		StandardOut: filepath.Join(c.LogDir, "share-daemon.log"),
		StandardErr: filepath.Join(c.LogDir, "share-daemon.err.log"),
		ExitTimeout: 15, Umask: 0o077, RunAtLoad: true, KeepAlive: true,
	}, nil
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

// Status returns launchd's loaded and running process state.
func (c *JobController) Status() (connectorservice.ServiceStatus, error) {
	if c == nil || c.Manager == nil {
		return connectorservice.ServiceStatus{}, errors.New("share daemon LaunchAgent controller is incomplete")
	}
	return c.Manager.Status(LaunchAgentLabel)
}

// DefaultLogDir returns the owner-local macOS daemon log directory.
func DefaultLogDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	home = strings.TrimSpace(home)
	if !filepath.IsAbs(home) {
		return "", errors.New("user home is not absolute")
	}
	return filepath.Join(home, "Library", "Logs", "qurl"), nil
}
