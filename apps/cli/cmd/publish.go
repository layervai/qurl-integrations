package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	qurl "github.com/layervai/qurl-go/qurl"
	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

func publishCmd(opts *globalOpts) *cobra.Command {
	var (
		description string
		tags        []string
		alias       string
		connectorID string
		foreground  bool
	)

	cmd := &cobra.Command{
		Use:   "publish <target-url>",
		Short: "Publish a URL or local app and get its CRID",
		Long: `Publish a local app or remote URL and get its CRID.

For a local app, pass its loopback HTTP address:

  qurl publish http://127.0.0.1:3000

On macOS and Windows, qURL starts a per-user background daemon, waits until the
route is serving, prints the CRID, and exits. The daemon resumes desired-on
shares after login, sleep, wake, and network changes. On Linux, use
--foreground; background lifecycle management is not yet available. Local app
sharing is supported on macOS, Windows, and Linux. Running the same command
later reuses the same resource and CRID.
Use --id only when you want to choose the Connector ID yourself.

For a remote URL, qURL registers it, prints the CRID, and exits:

  qurl publish https://api.example.com/reports

A CRID is safe to share: it identifies the resource but grants no access.
Authorized users open it with "qurl get <CRID>". The --quiet flag prints only
the CRID. Use --foreground for CI or daemon debugging; that process owns the
share and turns it off when it exits.`,
		Example: `  qurl publish http://127.0.0.1:3000
  qurl publish http://localhost:8080 --id local-dashboard
  qurl publish https://api.example.com/reports
  qurl publish https://grafana.internal.example.com --description "Team dashboard" --quiet`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := classifyPublishTarget(args[0])
			if err != nil {
				return err
			}
			if target.kind == publishTargetLocal {
				for _, name := range []string{"description", "tag", "alias"} {
					if cmd.Flags().Changed(name) {
						return exitcode.UsageError(fmt.Errorf("--%s is not supported for a local Connector publish", name))
					}
				}
				return runLocalPublish(cmd.Context(), opts, target, connectorID, foreground)
			}
			if cmd.Flags().Changed("id") {
				return exitcode.UsageError(errors.New("--id applies only when publishing a loopback HTTP origin"))
			}
			if cmd.Flags().Changed("foreground") {
				return exitcode.UsageError(errors.New("--foreground applies only when publishing a loopback HTTP origin"))
			}
			client, err := opts.newClient(cmd.Context())
			if err != nil {
				return err
			}

			result, err := client.Publish(cmd.Context(), args[0], qurlapi.PublishOptions{
				Description: description,
				Tags:        tags,
				Alias:       alias,
			})
			if err != nil {
				return err
			}

			printer := opts.printer()
			if result.CRID == "" {
				printer.Warnf("%s", msgNoCRIDReturned)
			}
			return printer.Publish(result)
		},
	}

	cmd.Flags().StringVar(&description, "description", "", "human-readable description stored with the resource")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "tag stored with the resource (repeatable)")
	cmd.Flags().StringVar(&alias, "alias", "", "memorable handle stored with the resource")
	cmd.Flags().StringVar(&connectorID, "id", "", "Connector ID for a local publish (default: stable ID for this machine and origin)")
	cmd.Flags().BoolVar(&foreground, "foreground", false, "serve in this process for debugging or CI and stop sharing when it exits")

	return cmd
}

func runLocalPublish(ctx context.Context, opts *globalOpts, target *publishTarget, flagID string, foreground bool) (retErr error) {
	requestedID, err := validateLocalPublishRequest(ctx, opts, target, flagID, foreground)
	if err != nil {
		return err
	}
	stateDir, err := opts.resolveShareStateDir("")
	if err != nil {
		return err
	}
	registry, err := opts.openShareRegistry(stateDir)
	if err != nil {
		return err
	}
	ownerID, client, err := localPublishOwner(ctx, opts, registry, stateDir)
	if err != nil {
		return err
	}
	sessionOperations, err := opts.resolveSessionConfig(ownerID)
	if err != nil {
		return err
	}
	enrollment := &localEnrollment{opts: opts, target: target, requestedID: requestedID}
	resolved, knockResourceID, err := prepareLocalPublishResource(ctx, opts, enrollment, stateDir, sessionOperations)
	if err != nil {
		return err
	}
	resource := resolved.Resource
	if client == nil {
		client, err = opts.newClient(ctx)
		if err != nil {
			return err
		}
	}
	local, sharing, compensateOff, err := activateLocalPublish(ctx, client, registry, resource, knockResourceID, target)
	if err != nil {
		return err
	}
	compensate := func(cause error) error {
		if !compensateOff {
			return cause
		}
		return compensateLocalPublish(client, registry, resource, cause)
	}
	if err := validateLocalSharing(local, sharing); err != nil {
		return compensate(err)
	}
	if err := registry.Put(ctx, local); err != nil {
		return compensate(err)
	}
	return finishLocalPublish(ctx, opts, client, registry, resolved, local, stateDir, foreground, compensate)
}

func validateLocalPublishRequest(ctx context.Context, opts *globalOpts, target *publishTarget, flagID string, foreground bool) (string, error) {
	if err := requireLocalShareSupport(opts.backgroundShareGOOS); err != nil {
		return "", err
	}
	if !foreground {
		if err := requireBackgroundShareSupport(opts.backgroundShareGOOS); err != nil {
			return "", err
		}
	}
	requestedID := resolveLocalConnectorID(opts, flagID)
	if requestedID != "" {
		if err := validateConnectorID(requestedID); err != nil {
			return "", err
		}
	}
	if err := opts.preflightTarget(ctx, target.localIP, target.localPort); err != nil {
		return "", err
	}
	return requestedID, nil
}

func localPublishOwner(ctx context.Context, opts *globalOpts, registry localShareRegistry, stateDir string) (string, qurlapi.Client, error) {
	ownerID, present, err := registry.OwnerID(ctx)
	if err != nil || present {
		return ownerID, nil, err
	}
	client, err := opts.newClient(ctx)
	if err != nil {
		return "", nil, err
	}
	identity := opts.registeredIdentity
	if identity == nil {
		identity, err = client.Me(ctx)
		if err != nil {
			return "", nil, err
		}
	}
	if identity == nil {
		return "", nil, errors.New("qURL account identity response is empty")
	}
	deviceKeyID := ""
	if identity.Key != nil {
		deviceKeyID = identity.Key.KeyID
	}
	if err := bindRegisteredDeviceOwner(ctx, registry, stateDir, deviceKeyID, identity.OwnerID); err != nil {
		return "", nil, err
	}
	return identity.OwnerID, client, nil
}

type localEnrollment struct {
	opts        *globalOpts
	target      *publishTarget
	requestedID string

	mu       sync.Mutex
	mintedID string
}

func (e *localEnrollment) connectorID(agentID string) (string, error) {
	if e.requestedID != "" {
		return e.requestedID, nil
	}
	return generatedLocalConnectorID(agentID, e.target.canonicalOrigin)
}

func (e *localEnrollment) credential(ctx context.Context, request qurl.AgentEnrollmentCredentialRequest) (string, error) {
	id, err := e.connectorID(request.AgentID)
	if err != nil {
		return "", err
	}
	idempotencyKey, err := localEnrollmentIdempotencyKey(request.AgentID, id)
	if err != nil {
		return "", err
	}
	client, err := e.opts.newClient(ctx)
	if err != nil {
		return "", err
	}
	token, err := client.MintConnectorEnrollmentToken(ctx, qurlapi.MintConnectorEnrollmentTokenOptions{
		ConnectorID: id, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return "", err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.mintedID != "" && e.mintedID != id {
		return "", fmt.Errorf("local Connector identity changed during enrollment: %q then %q", e.mintedID, id)
	}
	e.mintedID = id
	return token.Token, nil
}

func (e *localEnrollment) recoveryCredential(context.Context) (string, error) {
	return e.opts.apiCredential()
}

func (e *localEnrollment) resolveID(agentID string) (string, error) {
	id, err := e.connectorID(agentID)
	if err != nil {
		return "", err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.mintedID != "" && e.mintedID != id {
		return "", fmt.Errorf("local Connector identity does not match its enrollment claim: runtime resolved %q, token was bound to %q", id, e.mintedID)
	}
	return id, nil
}

func prepareLocalPublishResource(
	ctx context.Context,
	opts *globalOpts,
	enrollment *localEnrollment,
	stateDir string,
	sessionOperations connectorshare.NativeSessionOperationAuthority,
) (resolved *agent.ResolvedResource, knockResourceID string, err error) {
	hubBootstrap, err := opts.resolveHubBootstrap()
	if err != nil {
		return nil, "", err
	}
	origin, err := agent.ResourceSDKOrigin(opts.resolvedEndpoint)
	if err != nil {
		return nil, "", err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return nil, "", fmt.Errorf("read local hostname: %w", err)
	}
	cfg := &connectorshare.NativeRuntimeConfig{
		StateDir: stateDir, AgentID: connectorstate.ConfiguredAgentID(), Hub: hubBootstrap,
		Hostname: hostname, Version: opts.version, ClientBaseURL: origin,
		EnrollmentCredentialProvider: enrollment.credential,
		RecoveryCredentialProvider:   enrollment.recoveryCredential,
		RefreshMode:                  connectorRefreshModeAuto,
		SessionOperations:            sessionOperations,
	}
	resolved, err = opts.resolveLocalResource(ctx, cfg, enrollment.resolveID)
	if err != nil {
		return nil, "", err
	}
	knockResourceID, err = agent.KnockResourceID(resolved.Resource)
	if err != nil {
		return nil, "", err
	}
	return resolved, knockResourceID, nil
}

func activateLocalPublish(
	ctx context.Context,
	client qurlapi.Client,
	registry localShareRegistry,
	resource *qurl.ConnectorResource,
	knockResourceID string,
	target *publishTarget,
) (*connectorstate.LocalShare, *qurlapi.Sharing, bool, error) {
	existing, err := registry.Get(ctx, resource.ResourceID)
	localMissing := errors.Is(err, os.ErrNotExist)
	localPresent := err == nil
	targetChanged := err == nil && (existing.TargetURL != target.canonicalOrigin || existing.LocalIP != target.localIP || existing.LocalPort != target.localPort)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, false, err
	}
	prior, err := client.Sharing(ctx, resource.CRID)
	if err != nil {
		return nil, nil, false, err
	}
	if prior.ResourceID != resource.ResourceID || prior.CRID != resource.CRID {
		return nil, nil, false, errors.New("qURL sharing response identity does not match the published resource")
	}
	var sharing *qurlapi.Sharing
	terminalRecovery := localPresent && existing.DesiredState == string(qurlapi.DesiredStateOff) && prior.DesiredState == qurlapi.DesiredStateOn
	restartRequired := targetChanged || (localMissing && prior.DesiredState == qurlapi.DesiredStateOn) || terminalRecovery
	if restartRequired {
		sharing, err = restartSharingReconciled(ctx, client, resource.CRID, prior)
	} else {
		sharing, err = client.SetSharing(ctx, resource.CRID, qurlapi.DesiredStateOn)
	}
	if err != nil {
		return nil, nil, false, err
	}
	local := &connectorstate.LocalShare{
		CRID: resource.CRID, ResourceID: resource.ResourceID, ConnectorID: resource.Slug,
		ConnectorRoutingID: resource.ConnectorRoutingID, KnockResourceID: knockResourceID,
		TargetURL: target.canonicalOrigin, LocalIP: target.localIP, LocalPort: target.localPort,
		DesiredState: string(sharing.DesiredState), ServingEpoch: sharing.ServingEpoch,
	}
	return local, sharing, restartRequired || prior.DesiredState == qurlapi.DesiredStateOff, nil
}

func compensateLocalPublish(
	client qurlapi.Client,
	registry localShareRegistry,
	resource *qurl.ConnectorResource,
	cause error,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	off, offErr := client.SetSharing(ctx, resource.CRID, qurlapi.DesiredStateOff)
	var localErr error
	if offErr == nil {
		_, localErr = registry.SetDesired(ctx, resource.ResourceID, string(off.DesiredState), off.ServingEpoch)
		if errors.Is(localErr, os.ErrNotExist) {
			localErr = nil
		}
	}
	return errors.Join(cause, offErr, localErr)
}

func finishLocalPublish(
	ctx context.Context,
	opts *globalOpts,
	client qurlapi.Client,
	registry localShareRegistry,
	resolved *agent.ResolvedResource,
	local *connectorstate.LocalShare,
	stateDir string,
	foreground bool,
	compensate func(error) error,
) error {
	if foreground {
		return runForegroundLocalPublish(ctx, opts, client, registry, resolved, local, stateDir)
	}
	logDir, err := connectordaemon.DefaultLogDir(stateDir)
	if err != nil {
		return compensate(err)
	}
	if err := opts.newShareDaemon(stateDir, logDir).Ensure(ctx); err != nil {
		return compensate(err)
	}
	if _, err := waitForSharing(ctx, client, local, local.ServingEpoch, opts.sharingWaitLimit); err != nil {
		return err
	}
	return printLocalPublishServing(opts, resolved, local)
}

func runForegroundLocalPublish(
	ctx context.Context,
	opts *globalOpts,
	client qurlapi.Client,
	registry localShareRegistry,
	resolved *agent.ResolvedResource,
	local *connectorstate.LocalShare,
	stateDir string,
) (retErr error) {
	var (
		cancelDaemon context.CancelFunc
		daemonErr    chan error
		joined       bool
	)
	defer func() {
		if cancelDaemon != nil {
			cancelDaemon()
		}
		if daemonErr != nil && !joined {
			timer := time.NewTimer(10 * time.Second)
			select {
			case stopErr := <-daemonErr:
				// cancelDaemon deliberately stops a daemon that outlived an
				// earlier publish error. Do not let that expected cancellation
				// replace the actionable error with exit 130.
				if !errors.Is(stopErr, context.Canceled) {
					retErr = errors.Join(retErr, stopErr)
				}
			case <-timer.C:
				retErr = errors.Join(retErr, errors.New("foreground qURL daemon did not stop within 10 seconds"))
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		// Foreground mode deliberately owns cloud/local desired state. Leaving
		// the process must not strand a desired-on share without a launchd owner.
		retErr = compensateLocalPublish(client, registry, resolved.Resource, retErr)
	}()
	jobVersion, err := connectordaemon.JobVersion(opts.version)
	if err != nil {
		return err
	}
	daemonCtx, cancel := context.WithCancel(ctx)
	cancelDaemon = cancel
	daemonErr = make(chan error, 1)
	go func() { daemonErr <- opts.runForegroundDaemon(daemonCtx, opts, stateDir, jobVersion) }()
	ipc := connectordaemon.IPCClient{SocketPath: filepath.Join(stateDir, connectordaemon.SocketFile)}
	readyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	readyErr := make(chan error, 1)
	go func() { readyErr <- ipc.WaitReady(readyCtx) }()
	select {
	case err = <-readyErr:
	case err = <-daemonErr:
		joined = true
		if err == nil {
			err = errors.New("foreground qURL daemon exited before becoming ready")
		}
	}
	cancel()
	if err != nil {
		return err
	}
	if _, err := waitForSharing(ctx, client, local, local.ServingEpoch, opts.sharingWaitLimit); err != nil {
		return err
	}
	if err := printLocalPublishServing(opts, resolved, local); err != nil {
		return err
	}
	retErr = <-daemonErr
	joined = true
	return retErr
}

func printLocalPublishServing(opts *globalOpts, resolved *agent.ResolvedResource, local *connectorstate.LocalShare) error {
	printer := opts.printer()
	if strings.TrimSpace(local.CRID) == "" {
		printer.Warnf("%s", msgNoCRIDReturned)
	}
	return printer.Publish(&qurlapi.Published{
		CRID: local.CRID, ResourceID: local.ResourceID, TargetURL: local.TargetURL,
		Status: "serving", FoundExisting: resolved.FoundExisting,
	})
}

func resolveLocalConnectorID(opts *globalOpts, flagID string) string {
	if id := strings.TrimSpace(flagID); id != "" {
		return id
	}
	if id, ok := opts.lookupEnv("QURL_CONNECTOR_ID"); ok {
		if id = strings.TrimSpace(id); id != "" {
			return id
		}
	}
	return strings.TrimSpace(opts.profileConnectorID)
}

type localResourceResolver func(
	context.Context,
	*connectorshare.NativeRuntimeConfig,
	func(string) (string, error),
) (*agent.ResolvedResource, error)

// resolveLocalPublishResource uses qurl-connector's complete native runtime
// lifecycle, while integrations retains only the crash-safe resource-request
// journal and the enrollment-token callback/account authentication.
func resolveLocalPublishResource(ctx context.Context, cfg *connectorshare.NativeRuntimeConfig, resolveID func(string) (string, error)) (_ *agent.ResolvedResource, retErr error) {
	if cfg == nil {
		return nil, errors.New("local Connector runtime configuration is nil")
	}
	nativeRuntime, err := connectorshare.OpenNativeRuntime(ctx, *cfg)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, nativeRuntime.Close()) }()
	id, err := resolveID(nativeRuntime.AgentID)
	if err != nil {
		return nil, err
	}
	resourceStore, err := connectorstate.Open(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, resourceStore.Close()) }()
	return agent.ResolveResourceWithResult(ctx, nativeRuntime.Binding, resourceStore, id)
}
