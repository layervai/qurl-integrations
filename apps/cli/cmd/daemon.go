package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	v1 "github.com/fatedier/frp/pkg/config/v1"
	"github.com/spf13/cobra"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	"github.com/layervai/qurl-go/crid"
	qurl "github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

var openShareNativeRuntime = connectorshare.OpenNativeRuntime

const connectorRefreshModeAuto = "auto"

var buildNativeSessionFactory = func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig, common *v1.ClientCommonConfig, version string) (connectordaemon.SessionFactory, error) {
	runtime, err := openShareNativeRuntime(ctx, cfg)
	if err != nil {
		return nil, err
	}
	admitter, err := connectorshare.NewNativeAdmitter(runtime)
	if err != nil {
		return nil, errors.Join(err, runtime.Close())
	}
	if common == nil {
		return nil, errors.Join(errors.New("qURL daemon FRP configuration is invalid"), admitter.Close())
	}
	return connectordaemon.NewNativeSessionFactory(admitter, common, version)
}

var waitHeadlessNativeRetry = func(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// daemonCmd is an implementation surface for the per-user LaunchAgent. It is
// intentionally hidden: customers manage shares with publish/start/stop and
// never need to supervise this process themselves.
func daemonCmd(opts *globalOpts) *cobra.Command {
	cmd := &cobra.Command{Use: "daemon", Hidden: true, Args: noArgs}
	var stateDir, jobVersion, headlessConfig, enrollmentTokenFile string
	var hubHost, hubServerPublicKeyB64 string
	var hubPort int
	run := &cobra.Command{
		Use:    "run",
		Hidden: true,
		Args:   noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireLocalShareSupport(opts.backgroundShareGOOS); err != nil {
				return err
			}
			expected, err := connectordaemon.JobVersion(opts.version)
			if err != nil {
				return err
			}
			if jobVersion != "" && jobVersion != expected {
				return fmt.Errorf("share daemon job version %q does not match binary %q", jobVersion, expected)
			}
			if jobVersion == "" {
				jobVersion = expected
			}
			hubBootstrap, hasHubOverride, err := exactDaemonHubOverride(hubHost, hubPort, hubServerPublicKeyB64)
			if err != nil {
				return err
			}
			var hubOverride *qurl.HubBootstrap
			if hasHubOverride {
				hubOverride = &hubBootstrap
			}
			return runShareDaemonWithDeployment(cmd.Context(), opts, stateDir, jobVersion, headlessConfig, enrollmentTokenFile, hubOverride)
		},
	}
	run.Flags().StringVar(&stateDir, "state-dir", "", "qURL share daemon state directory")
	run.Flags().StringVar(&jobVersion, "job-version", "", "qURL share daemon job definition version")
	run.Flags().StringVar(&headlessConfig, "headless-config", "", "declarative headless share configuration")
	run.Flags().StringVar(&enrollmentTokenFile, "enrollment-token-file", "", "first-bootstrap enrollment credential file")
	run.Flags().StringVar(&hubHost, "hub-host", "", "pinned share-daemon Hub host")
	run.Flags().IntVar(&hubPort, "hub-port", 0, "pinned share-daemon Hub port")
	run.Flags().StringVar(&hubServerPublicKeyB64, "hub-server-public-key-b64", "", "pinned share-daemon Hub server public key")
	for _, name := range []string{"hub-host", "hub-port", "hub-server-public-key-b64"} {
		_ = run.Flags().MarkHidden(name)
	}
	validateTestCRID := &cobra.Command{
		Use:    "validate-test-resource <crid> <resource-id>",
		Hidden: true,
		Args:   cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := requireTestResourceIdentity(args[0], args[1]); err != nil {
				return exitcode.UsageError(err)
			}
			return nil
		},
	}
	cmd.AddCommand(run, validateTestCRID)
	return cmd
}

func requireTestResourceIdentity(cridValue, resourceID string) error {
	if err := requireTestEnvironmentCRID(cridValue); err != nil {
		return err
	}
	der, err := base64.RawURLEncoding.DecodeString(resourceID)
	if err != nil || len(der) == 0 || base64.RawURLEncoding.EncodeToString(der) != resourceID {
		return errors.New("resource identity is not canonical")
	}
	matches, err := crid.KeyMatches(cridValue, der)
	if err != nil || !matches {
		return errors.New("CRID does not commit to the resource identity")
	}
	return nil
}

func requireTestEnvironmentCRID(value string) error {
	parsed, err := crid.Parse(value)
	if err != nil {
		return errors.New("CRID is not canonical")
	}
	if !parsed.Known() || parsed.Environment() != crid.EnvironmentTest {
		return errors.New("CRID is not registered for the test environment")
	}
	return nil
}

func runShareDaemon(ctx context.Context, opts *globalOpts, stateDirOverride, jobVersion string) (retErr error) {
	return runShareDaemonWithBootstrap(ctx, opts, stateDirOverride, jobVersion, "", "")
}

func runShareDaemonWithBootstrap(ctx context.Context, opts *globalOpts, stateDirOverride, jobVersion, headlessConfigPath, enrollmentTokenPath string) (retErr error) {
	return runShareDaemonWithDeployment(ctx, opts, stateDirOverride, jobVersion, headlessConfigPath, enrollmentTokenPath, nil)
}

func runShareDaemonWithDeployment(ctx context.Context, opts *globalOpts, stateDirOverride, jobVersion, headlessConfigPath, enrollmentTokenPath string, hubOverride *qurl.HubBootstrap) (retErr error) {
	stateDir, err := opts.resolveShareStateDir(stateDirOverride)
	if err != nil {
		return err
	}
	headless, enrollmentCredential, err := loadHeadlessBootstrap(stateDir, headlessConfigPath, enrollmentTokenPath)
	if err != nil {
		return err
	}

	registry, err := connectorstate.OpenLocalShareRegistry(stateDir)
	if err != nil {
		return err
	}
	if headless != nil {
		if err := validateHeadlessRegistryOwnership(ctx, registry, &headless.Shares[0]); err != nil {
			return err
		}
	}
	hubBootstrap := qurl.HubBootstrap{}
	if hubOverride != nil {
		hubBootstrap = *hubOverride
	} else {
		hubBootstrap, err = opts.resolveHubBootstrap()
		if err != nil {
			return err
		}
	}
	origin, err := agent.ResourceSDKOrigin(opts.resolvedEndpoint)
	if err != nil {
		return err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("read local hostname: %w", err)
	}
	common, err := connectordaemon.DefaultFRPCommon(10, 60)
	if err != nil {
		return err
	}
	openFactory := func(initCtx context.Context) (connectordaemon.SessionFactory, error) {
		return buildNativeSessionFactory(initCtx, connectorshare.NativeRuntimeConfig{
			StateDir: stateDir, AgentID: connectorstate.ConfiguredAgentID(), Hub: hubBootstrap,
			Hostname: hostname, Version: opts.version, ClientBaseURL: origin,
			EnrollmentCredential: enrollmentCredential, RefreshMode: connectorRefreshModeAuto,
		}, common, opts.version)
	}
	var factory connectordaemon.SessionFactory
	var closeFactory func() error
	if headless != nil {
		// Native bootstrap succeeds before the share becomes runnable. This avoids
		// persisting a desired-on row after a bad/missing one-time credential.
		factory, err = openHeadlessSessionFactory(ctx, openFactory)
		enrollmentCredential = ""
		if err == nil {
			err = registry.Put(ctx, &headless.Shares[0])
		}
		if closer, ok := factory.(interface{ Close() error }); ok {
			closeFactory = closer.Close
		}
	} else {
		var deferred *connectordaemon.DeferredSessionFactory
		deferred, err = connectordaemon.NewDeferredSessionFactory(openFactory)
		factory = deferred
		closeFactory = deferred.Close
	}
	if err != nil {
		if closeFactory != nil {
			return errors.Join(err, closeFactory())
		}
		return err
	}
	defer func() {
		if closeFactory != nil {
			retErr = errors.Join(retErr, closeFactory())
		}
	}()
	manager, err := connectordaemon.NewManager(registry, factory)
	if err != nil {
		return err
	}
	opts.redirectFRPLogs()
	server := &connectordaemon.IPCServer{
		SocketPath: filepath.Join(stateDir, connectordaemon.SocketFile),
		Manager:    manager, JobVersion: jobVersion,
	}
	return server.Run(ctx)
}

func exactDaemonHubOverride(host string, port int, serverPublicKeyB64 string) (qurl.HubBootstrap, bool, error) {
	present := 0
	if host != "" {
		present++
	}
	if port != 0 {
		present++
	}
	if serverPublicKeyB64 != "" {
		present++
	}
	if present == 0 {
		return qurl.HubBootstrap{}, false, nil
	}
	if present != 3 {
		return qurl.HubBootstrap{}, false, errors.New("share daemon Hub host, port, and server public key must be set together")
	}
	bootstrap := qurl.HubBootstrap{Host: host, Port: port, ServerPublicKeyB64: serverPublicKeyB64}
	if err := hub.ValidateBootstrap(bootstrap); err != nil {
		return qurl.HubBootstrap{}, false, err
	}
	return bootstrap, true, nil
}

func validateHeadlessRegistryOwnership(ctx context.Context, registry *connectorstate.LocalShareRegistry, configured *connectorstate.LocalShare) error {
	if registry == nil || configured == nil {
		return errors.New("headless share registry ownership is incomplete")
	}
	shares, err := registry.List(ctx)
	if err != nil {
		return fmt.Errorf("inspect existing headless share registry: %w", err)
	}
	if len(shares) == 0 {
		return nil
	}
	if len(shares) != 1 {
		return fmt.Errorf("headless share config owns exactly one resource but the state directory contains %d resources; use a dedicated state volume", len(shares))
	}
	existing := shares[0]
	if existing.ResourceID != configured.ResourceID || existing.CRID != configured.CRID ||
		existing.ConnectorID != configured.ConnectorID ||
		existing.ConnectorRoutingID != configured.ConnectorRoutingID ||
		existing.KnockResourceID != configured.KnockResourceID {
		return errors.New("headless share config does not own the resource already stored in this state directory; use a dedicated state volume")
	}
	return nil
}

func openHeadlessSessionFactory(ctx context.Context, open func(context.Context) (connectordaemon.SessionFactory, error)) (connectordaemon.SessionFactory, error) {
	if open == nil {
		return nil, errors.New("headless native session factory opener is nil")
	}
	for attempt := 1; ; attempt++ {
		factory, err := open(ctx)
		if err == nil {
			if factory == nil {
				return nil, errors.New("headless native session factory opener returned nil")
			}
			return factory, nil
		}
		if ctx.Err() != nil {
			return nil, errors.Join(err, ctx.Err())
		}
		if connectorshare.IsPermanentNativeOpenError(err) {
			return nil, err
		}
		if err := waitHeadlessNativeRetry(ctx, headlessNativeRetryDelay(attempt)); err != nil {
			return nil, err
		}
	}
}

func headlessNativeRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := 250 * time.Millisecond
	for step := 1; step < attempt && delay < 30*time.Second; step++ {
		delay *= 2
	}
	if delay > 30*time.Second {
		return 30 * time.Second
	}
	return delay
}

func loadHeadlessBootstrap(stateDir, configPath, tokenPath string) (*connectorstate.HeadlessConfig, string, error) {
	if configPath == "" {
		if tokenPath != "" {
			return nil, "", errors.New("--enrollment-token-file requires --headless-config")
		}
		return nil, "", nil
	}
	config, err := connectorstate.LoadHeadlessConfig(configPath)
	if err != nil {
		return nil, "", err
	}
	_, stateErr := os.Lstat(filepath.Join(stateDir, connectorstate.AgentStateFile))
	switch {
	case stateErr == nil:
		// Warm starts never reopen or require the one-time credential file.
		return config, "", nil
	case errors.Is(stateErr, os.ErrNotExist):
		if tokenPath == "" {
			return nil, "", errors.New("--enrollment-token-file is required for first headless bootstrap")
		}
		credential, err := connectorstate.ReadEnrollmentCredential(tokenPath)
		if err != nil {
			return nil, "", err
		}
		return config, credential, nil
	default:
		return nil, "", fmt.Errorf("inspect native agent state before headless bootstrap: %w", stateErr)
	}
}
