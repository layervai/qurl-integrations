package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	v1 "github.com/fatedier/frp/pkg/config/v1"
	"github.com/spf13/cobra"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	"github.com/layervai/qurl-go/crid"
	qurl "github.com/layervai/qurl-go/qurl"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

var openShareNativeRuntime = connectorshare.OpenNativeRuntime

type nativeRegisteredIdentityReader func(context.Context, *connectorshare.NativeRuntime, *qurlapi.Config) (*qurlapi.Identity, error)

func readNativeRegisteredIdentity(ctx context.Context, runtime *connectorshare.NativeRuntime, config *qurlapi.Config) (*qurlapi.Identity, error) {
	store, err := runtime.Handoff()
	if err != nil {
		return nil, err
	}
	client, err := qurlapi.NewRegistered(ctx, config, store)
	if err != nil {
		return nil, err
	}
	return client.Me(ctx)
}

const connectorRefreshModeAuto = "auto"

var errNativeSessionOwnerVerification = errors.New("registered Connector owner verification failed")

var buildNativeSessionFactory = func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig, common *v1.ClientCommonConfig, apiConfig *qurlapi.Config, verifyOwner bool) (connectordaemon.GroupFactory, error) {
	if apiConfig == nil {
		return nil, errors.New("qURL daemon registered-client configuration is missing")
	}
	// TODO(upstream-contract): OpenNativeRuntime must not execute session
	// operations. qurl-connector consumes that authority only when
	// NewNativeAdmitter takes the runtime, after this owner check.
	runtime, err := openShareNativeRuntime(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if verifyOwner {
		if err := verifyNativeSessionOwner(ctx, runtime, apiConfig, cfg.SessionOperations.OwnerID, readNativeRegisteredIdentity); err != nil {
			return nil, errors.Join(err, runtime.Close())
		}
	}
	admitter, err := connectorshare.NewNativeAdmitter(ctx, runtime)
	if err != nil {
		return nil, errors.Join(err, runtime.Close())
	}
	if common == nil {
		return nil, errors.Join(errors.New("qURL daemon FRP configuration is invalid"), admitter.Close())
	}
	return connectordaemon.NewNativeGroupFactory(admitter, common, apiConfig.Version)
}

func verifyNativeSessionOwner(ctx context.Context, runtime *connectorshare.NativeRuntime, apiConfig *qurlapi.Config, expectedOwner string, readIdentity nativeRegisteredIdentityReader) error {
	expectedOwner = strings.TrimSpace(expectedOwner)
	if expectedOwner == "" {
		return fmt.Errorf("%w: session-operation owner authority is empty", errNativeSessionOwnerVerification)
	}
	if readIdentity == nil {
		return fmt.Errorf("%w: identity reader is unavailable", errNativeSessionOwnerVerification)
	}
	identity, err := readIdentity(ctx, runtime, apiConfig)
	if err != nil {
		return fmt.Errorf("verify registered Connector owner: %w", err)
	}
	if identity == nil || strings.TrimSpace(identity.OwnerID) == "" {
		return fmt.Errorf("%w: qURL account identity response is empty", errNativeSessionOwnerVerification)
	}
	if identity.OwnerID != expectedOwner {
		return fmt.Errorf("%w: configured owner %q does not match authenticated owner %q", errNativeSessionOwnerVerification, expectedOwner, identity.OwnerID)
	}
	return nil
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

// daemonCmd exposes the long-running engine for headless deployments and
// foreground supervision. Ordinary Linux, macOS, and Windows users normally
// let publish/start manage the native per-user background job.
func daemonCmd(opts *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the local sharing daemon",
		Long: `Run the local sharing daemon directly.

On Linux, macOS, and Windows, qurl publish and qurl start normally manage the
per-user daemon for you. Use daemon run for a headless deployment or when
another service manager owns the process.`,
		Args: noArgs,
	}
	var stateDir, jobVersion, headlessConfig, enrollmentTokenFile string
	var hubHost, hubServerPublicKeyB64 string
	var hubPort int
	run := &cobra.Command{
		Use:   "run",
		Short: "Run the daemon in the foreground",
		Args:  noArgs,
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
	run.Flags().StringVar(&headlessConfig, "headless-config", "", "read-only version 2 YAML for one headless share")
	run.Flags().StringVar(&enrollmentTokenFile, "enrollment-token-file", "", "one-time enrollment credential file for first headless bootstrap")
	// root.go's persistent pre-run hook reads these before settings resolution,
	// so a first-start rejection reaches the native supervisor's durable log.
	run.Flags().String("job-stdout-log", "", "native background-job stdout log")
	run.Flags().String("job-stderr-log", "", "native background-job stderr log")
	run.Flags().StringVar(&hubHost, "hub-host", "", "pinned share-daemon Hub host")
	run.Flags().IntVar(&hubPort, "hub-port", 0, "pinned share-daemon Hub port")
	run.Flags().StringVar(&hubServerPublicKeyB64, "hub-server-public-key-b64", "", "pinned share-daemon Hub server public key")
	for _, name := range []string{"hub-host", "hub-port", "hub-server-public-key-b64", "job-version", "job-stdout-log", "job-stderr-log"} {
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
	headless, enrollmentCredential, err := loadHeadlessBootstrap(ctx, stateDir, headlessConfigPath, enrollmentTokenPath)
	if err != nil {
		return err
	}

	registry, err := connectorstate.OpenLocalShareRegistry(stateDir)
	if err != nil {
		return err
	}
	ownerID, ownerBound, err := daemonOwner(ctx, registry, headless)
	if err != nil {
		return err
	}
	// The headless YAML is declarative input, not an authority. Authenticate
	// its owner through the registered device before the first durable bind.
	// Once bound, daemonOwner enforces exact continuity without adding a REST
	// dependency to every warm daemon restart.
	verifyOwner := headless != nil && !ownerBound
	sessionOperations, err := opts.resolveSessionConfig(ownerID)
	if err != nil {
		return err
	}
	hubBootstrap, err := daemonHubBootstrap(opts, hubOverride)
	if err != nil {
		return err
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
	openFactory := func(initCtx context.Context) (connectordaemon.GroupFactory, error) {
		apiConfig := &qurlapi.Config{
			BaseURL: origin, Version: opts.version, Verbose: opts.verboseLogger(),
			Sleep: opts.sleep, NewRequestID: opts.newRequestID,
		}
		return buildNativeSessionFactory(initCtx, connectorshare.NativeRuntimeConfig{
			StateDir: stateDir, AgentID: connectorstate.ConfiguredAgentID(), Hub: hubBootstrap,
			Hostname: hostname, Version: opts.version, ClientBaseURL: origin,
			EnrollmentCredential: enrollmentCredential, RefreshMode: connectorRefreshModeAuto,
			SessionOperations: sessionOperations,
		}, common, apiConfig, verifyOwner)
	}
	var factory connectordaemon.GroupFactory
	var closeFactory func() error
	if headless != nil {
		// Native bootstrap succeeds before the share becomes runnable. This avoids
		// persisting owner or desired-on state after a bad/missing one-time
		// credential.
		factory, err = openHeadlessSessionFactory(ctx, openFactory)
		enrollmentCredential = ""
		if err == nil && !ownerBound {
			err = registry.BindOwner(ctx, headless.OwnerID)
		}
		if err == nil {
			err = registry.Put(ctx, &headless.Shares[0])
		}
		if closer, ok := factory.(interface{ Close() error }); ok {
			closeFactory = closer.Close
		}
	} else {
		var deferred *connectordaemon.DeferredGroupFactory
		deferred, err = connectordaemon.NewDeferredGroupFactory(openFactory)
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
		SocketPath: connectordaemon.StateSocketPath(stateDir),
		Manager:    manager, JobVersion: jobVersion,
	}
	return server.Run(ctx)
}

func daemonOwner(ctx context.Context, registry *connectorstate.LocalShareRegistry, headless *connectorstate.HeadlessConfig) (ownerID string, bound bool, err error) {
	if headless != nil {
		ownerID, present, err := registry.OwnerID(ctx)
		if err != nil {
			return "", false, err
		}
		if present && ownerID != headless.OwnerID {
			return "", false, errors.New("headless share config belongs to a different durable account owner; use a dedicated state volume")
		}
		if err := validateHeadlessRegistryOwnership(ctx, registry, &headless.Shares[0]); err != nil {
			return "", false, err
		}
		return headless.OwnerID, present, nil
	}
	ownerID, present, err := registry.OwnerID(ctx)
	if err != nil {
		return "", false, err
	}
	if !present {
		return "", false, errors.New("qURL share daemon has no durable account owner; run `qurl login` and publish a local app first, or use --headless-config with --enrollment-token-file")
	}
	return ownerID, true, nil
}

func daemonHubBootstrap(opts *globalOpts, override *qurl.HubBootstrap) (qurl.HubBootstrap, error) {
	if override != nil {
		return *override, nil
	}
	return opts.resolveHubBootstrap()
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

func openHeadlessSessionFactory(ctx context.Context, open func(context.Context) (connectordaemon.GroupFactory, error)) (connectordaemon.GroupFactory, error) {
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
		if isPermanentHeadlessNativeOpenError(err) {
			return nil, err
		}
		delay := headlessNativeRetryDelay(attempt)
		slog.WarnContext(ctx, "headless share daemon bootstrap failed; retrying",
			"attempt", attempt, "retry_in", delay, "error", qurlapi.Redact(err.Error()))
		if err := waitHeadlessNativeRetry(ctx, delay); err != nil {
			return nil, err
		}
	}
}

func isPermanentHeadlessNativeOpenError(err error) bool {
	return errors.Is(err, errNativeSessionOwnerVerification) || connectorshare.IsPermanentNativeOpenError(err)
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

func loadHeadlessBootstrap(ctx context.Context, stateDir, configPath, tokenPath string) (*connectorstate.HeadlessConfig, string, error) {
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
	stateStore, err := connectorstate.Open(stateDir)
	if err != nil {
		return nil, "", fmt.Errorf("inspect native agent state before headless bootstrap: %w", err)
	}
	defer func() { _ = stateStore.Close() }()
	sdkStore, err := stateStore.Handoff()
	if err != nil {
		return nil, "", fmt.Errorf("inspect native agent state before headless bootstrap: %w", err)
	}
	state, stateErr := sdkStore.LoadAgentState(ctx)
	switch {
	case stateErr == nil:
		if state != nil && state.RegisteredAt != nil && strings.TrimSpace(state.DeviceAPIKey) != "" {
			// A complete warm identity uses only its device keypair and device
			// credential. The one-time enrollment token is not reopened.
			return config, "", nil
		}
		if tokenPath == "" {
			return nil, "", errors.New("--enrollment-token-file is required to resume an incomplete headless bootstrap")
		}
		credential, err := connectorstate.ReadEnrollmentCredential(tokenPath)
		if err != nil {
			return nil, "", err
		}
		return config, credential, nil
	case errors.Is(stateErr, qurl.ErrAgentStateNotFound):
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
