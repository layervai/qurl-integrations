package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	qurl "github.com/layervai/qurl-go/qurl"

	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

const (
	launchAgentLabel = "ai.layerv.qurl.share-daemon"
	envEndpoint      = "QURL_ENDPOINT"
	preflightTimeout = 60 * time.Second
)

// environment is everything resolved before the first mutation: where the
// resident daemon keeps state, how to reach it, which binaries run, and the
// exact child-process environment every CLI call receives.
type environment struct {
	StateDir         string `json:"state_dir"`
	SocketPath       string `json:"socket_path"`
	LogDir           string `json:"daemon_log_dir"`
	PlistPath        string `json:"launch_agent_plist,omitempty"`
	Endpoint         string `json:"endpoint"`
	EndpointSource   string `json:"endpoint_source"`
	HubSource        string `json:"hub_settings_source"`
	QurlBin          string `json:"qurl_bin"`
	QurlVersion      string `json:"qurl_version"`
	ConsumeBin       string `json:"consume_bin"`
	ConsumeVersion   string `json:"consume_version"`
	DaemonRunning    bool   `json:"daemon_running"`
	DaemonJobVersion string `json:"daemon_job_version,omitempty"`
	DeploymentSet    bool   `json:"deployment_settings_set"`
	GOOS             string `json:"goos"`
	Hostname         string `json:"-"`

	childEnv []string
	redactor *redactor

	// maxProbes bounds SDK-level diagnostic probes per run: each one mints a
	// share link, so a widespread platform flap must not be amplified by the
	// harness spending more budget on every failure.
	maxProbes    int
	probesUsed   atomic.Int64
	probesDenied atomic.Int64
}

// probe runs the SDK access probe while the run's probe budget lasts and
// counts the probes it had to skip.
func (e *environment) probe(ctx context.Context, crid string) *fetchDiagnosis {
	if e.maxProbes > 0 && e.probesUsed.Add(1) > int64(e.maxProbes) {
		e.probesDenied.Add(1)
		return nil
	}
	return probeAccess(ctx, e, crid)
}

// launchAgent is the subset of the resident daemon's job definition the
// harness cross-checks: the daemon and the CLI must agree on state dir,
// endpoint, and Hub trust root or no publish can ever serve.
type launchAgent struct {
	stateDir string
	endpoint string
	hubHost  string
	hubPort  string
	hubKey   string
}

func resolveEnvironment(ctx context.Context, opts *options) (*environment, error) {
	stateDir, err := connectorstate.ResolveDir("")
	if err != nil {
		return nil, err
	}
	logDir, err := connectordaemon.DefaultLogDir(stateDir)
	if err != nil {
		return nil, err
	}
	env := &environment{
		StateDir: stateDir, SocketPath: connectordaemon.StateSocketPath(stateDir), LogDir: logDir,
		QurlBin: opts.qurlBin, ConsumeBin: opts.consumeBin, GOOS: runtime.GOOS, maxProbes: opts.maxProbes,
	}
	agent, err := readLaunchAgent(ctx)
	if err != nil {
		return nil, err
	}
	if agent != nil {
		env.PlistPath = launchAgentPlistPath()
		if agent.stateDir != "" && agent.stateDir != stateDir {
			return nil, fmt.Errorf("the resident daemon uses state dir %s but this CLI resolves %s; set %s=%s so the harness and the daemon agree",
				agent.stateDir, stateDir, connectorstate.EnvStateDirPrimary, agent.stateDir)
		}
	}
	if err := env.buildChildEnv(opts, agent); err != nil {
		return nil, err
	}
	home, _ := os.UserHomeDir()
	env.redactor = newRedactor(home, hubLiteral(agent, env.childEnv)...)
	if env.Hostname, _ = os.Hostname(); strings.TrimSpace(env.Hostname) != "" {
		env.redactor.literals = append(env.redactor.literals, [2]string{env.Hostname, "<host>"})
	}
	if err := env.preflight(ctx, opts); err != nil {
		return nil, err
	}
	return env, nil
}

// buildChildEnv assembles the exact environment every CLI child receives.
// Precedence per value: an explicit flag, then the process environment, then
// the resident daemon's own job definition, so a dark release build (one
// without a pinned production Hub key) gets the same Hub trust triple the
// daemon already runs with instead of failing closed.
func (e *environment) buildChildEnv(opts *options, agent *launchAgent) error {
	base := os.Environ()
	set := map[string]string{}
	switch {
	case opts.endpoint != "":
		set[envEndpoint], e.EndpointSource = opts.endpoint, "flag"
	case os.Getenv(envEndpoint) != "":
		e.EndpointSource = "env"
	case agent != nil && agent.endpoint != "":
		set[envEndpoint], e.EndpointSource = agent.endpoint, "launch-agent"
	default:
		e.EndpointSource = "cli-config"
	}
	e.HubSource = resolveHubEnv(agent, set)
	if agent != nil && agent.endpoint != "" {
		effective := set[envEndpoint]
		if effective == "" {
			effective = os.Getenv(envEndpoint)
		}
		if effective != "" && effective != agent.endpoint {
			return fmt.Errorf("endpoint %s does not match the resident daemon's %s; every share would be published to a service the daemon does not talk to", effective, agent.endpoint)
		}
	}
	_, e.DeploymentSet = os.LookupEnv(qurl.EnvDeploymentPath)
	child := make([]string, 0, len(base)+len(set))
	for _, kv := range base {
		key, _, _ := strings.Cut(kv, "=")
		if _, override := set[key]; !override {
			child = append(child, kv)
		}
	}
	for key, value := range set {
		child = append(child, key+"="+value)
	}
	e.childEnv = child
	e.Endpoint = childValue(child, envEndpoint)
	if e.Endpoint == "" {
		e.Endpoint = "(cli config)"
	}
	return nil
}

// resolveHubEnv fills every Hub trust variable the process environment
// leaves unset from the resident daemon's job definition and names the
// source. The triple is all-or-none for the CLI, so a partially exported
// triple is completed rather than passed through to fail closed at preflight.
func resolveHubEnv(agent *launchAgent, set map[string]string) string {
	hubVars := map[string]string{}
	if agent != nil {
		hubVars[hub.EnvHost], hubVars[hub.EnvPort], hubVars[hub.EnvServerPublicKey] = agent.hubHost, agent.hubPort, agent.hubKey
	}
	fromEnv, fromAgent := 0, 0
	for _, name := range []string{hub.EnvHost, hub.EnvPort, hub.EnvServerPublicKey} {
		if _, ok := os.LookupEnv(name); ok {
			fromEnv++
			continue
		}
		if hubVars[name] != "" {
			set[name] = hubVars[name]
			fromAgent++
		}
	}
	switch {
	case fromEnv == 3:
		return "env"
	case fromEnv > 0 && fromAgent > 0:
		return "env+launch-agent"
	case fromAgent == 3:
		return "launch-agent"
	default:
		return "build-default"
	}
}

func childValue(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// hubLiteral lists the Hub trust values as redaction pairs: the report must
// describe the trust root by source, never by value.
func hubLiteral(agent *launchAgent, child []string) []string {
	var pairs []string
	for _, value := range []string{childValue(child, hub.EnvServerPublicKey), childValue(child, hub.EnvHost)} {
		if value != "" {
			pairs = append(pairs, value, "<hub>")
		}
	}
	if agent != nil {
		for _, value := range []string{agent.hubKey, agent.hubHost} {
			if value != "" {
				pairs = append(pairs, value, "<hub>")
			}
		}
	}
	return pairs
}

func launchAgentPlistPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
}

// readLaunchAgent reads the resident macOS daemon's job definition through
// plutil. Absent on other platforms or before the first publish.
func readLaunchAgent(ctx context.Context) (*launchAgent, error) {
	if runtime.GOOS != "darwin" {
		return nil, nil //nolint:nilnil // no LaunchAgent on this platform is a valid, empty answer
	}
	path := launchAgentPlistPath()
	if path == "" {
		return nil, nil //nolint:nilnil // no home directory: nothing to read
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil //nolint:nilnil // daemon not installed yet: publish will install it
	}
	plutilCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(plutilCtx, "plutil", "-convert", "json", "-o", "-", path).Output() //nolint:gosec // G204: fixed plutil invocation on the fixed per-user LaunchAgent path.
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc struct {
		ProgramArguments []string `json:"ProgramArguments"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return parseLaunchAgentArgs(doc.ProgramArguments), nil
}

func parseLaunchAgentArgs(args []string) *launchAgent {
	agent := &launchAgent{}
	targets := map[string]*string{
		"--state-dir": &agent.stateDir, "--endpoint": &agent.endpoint,
		"--hub-host": &agent.hubHost, "--hub-port": &agent.hubPort, "--hub-server-public-key-b64": &agent.hubKey,
	}
	for i := 0; i+1 < len(args); i++ {
		if target, ok := targets[args[i]]; ok {
			*target = args[i+1]
			i++
		}
	}
	return agent
}

// preflight proves the environment can do everything the run needs before
// the first share is published: both binaries answer `version`, the release
// CLI authenticates and lists, and the daemon's IPC socket is inspectable.
func (e *environment) preflight(ctx context.Context, opts *options) error {
	version := runCLI(ctx, e.QurlBin, e.childEnv, preflightTimeout, "version")
	if version.ExitCode != cliExitOK {
		return fmt.Errorf("%s version: exit %d: %s", e.QurlBin, version.ExitCode, e.redactor.apply(lastErrorLine(version.Stderr)))
	}
	e.QurlVersion = strings.TrimSpace(version.Stdout)
	consume := runCLI(ctx, e.ConsumeBin, e.childEnv, preflightTimeout, "version")
	if consume.ExitCode != cliExitOK {
		return fmt.Errorf("%s version: exit %d", e.ConsumeBin, consume.ExitCode)
	}
	e.ConsumeVersion = strings.TrimSpace(consume.Stdout)
	whoami := runCLI(ctx, e.QurlBin, e.childEnv, preflightTimeout, "whoami", "--quiet")
	if whoami.ExitCode != cliExitOK {
		return fmt.Errorf("qurl whoami failed (exit %d): %s", whoami.ExitCode, e.redactor.apply(lastErrorLine(whoami.Stderr)))
	}
	list := runCLI(ctx, e.QurlBin, e.childEnv, preflightTimeout, "list", "--limit", "1", flagOutput, outputJSON)
	if list.ExitCode != cliExitOK {
		return fmt.Errorf("qurl list failed (exit %d): %s", list.ExitCode, e.redactor.apply(lastErrorLine(list.Stderr)))
	}
	status, running, err := connectordaemon.IPCClient{SocketPath: e.SocketPath}.Status(ctx)
	if err != nil {
		return fmt.Errorf("daemon status over %s: %w", e.SocketPath, err)
	}
	e.DaemonRunning = running
	if running {
		e.DaemonJobVersion = status.JobVersion
	}
	if (!opts.skipVerify || opts.probe != "") && !opts.teardown && !e.DeploymentSet {
		return fmt.Errorf("end-to-end fetches need %s (a deployment settings file with the sandbox issuer keys); set it, or pass --skip-verify", qurl.EnvDeploymentPath)
	}
	return nil
}
