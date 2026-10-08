package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
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
		description       string
		tags              []string
		alias             string
		connectorID       string
		public            bool
		private           bool
		allowedDeviceKeys []string
		foreground        bool
	)

	cmd := &cobra.Command{
		Use:   "publish <target-url>",
		Short: "Publish a URL or local app and get its CRID",
		Long: `Publish a local app or remote URL and get its CRID.

For a local app, pass its loopback HTTP address:

  qurl publish http://127.0.0.1:3000

On Linux, macOS, and Windows, qURL starts a per-user background daemon, waits
until the route is serving, prints the CRID, and exits. The daemon resumes
desired-on shares after login, sleep, wake, and network changes. Running the
same command later reuses the same resource and CRID.
Use --id only when you want to choose the Connector ID yourself.
After "qurl delete", you can reuse that ID to create a new resource with a new CRID.
Old links remain invalid.

For a remote URL, qURL registers it, prints the CRID, and exits:

  qurl publish https://api.example.com/reports

A resource is private unless you publish it with --public. A private resource
is limited to its owner and the devices you allow: with --allow-device-key
when you publish it, and with "qurl grants" afterwards. Run
"qurl whoami -o json" on a recipient's device to find its public key.

"qurl share" and "qurl get" use the identity of the device they run on. They
work on the resource owner's devices and, for a private resource, on the
devices you allowed; any other device gets "not found".

Where the deployment offers it, a public resource can also be opened by anyone
who has its CRID. The deployment this release ships does not offer it yet.
Privacy is set at creation and cannot be changed later. Publishing a target
again reuses its resource, with the privacy it has.

If you published a target with a release that made resources public by
default, that resource is still public. Publishing it again with no privacy
flag keeps it, and warns you that it stays public. To make it private, delete
it with "qurl delete <CRID>" and publish again; the new resource gets a new
CRID. A flag that asks for what the existing resource is not is refused:
--allow-device-key for a public resource, --public for a private one.

Authorized users open the resource with "qurl get <CRID>". The --quiet flag
prints only the CRID. Use --foreground for CI or daemon debugging; that process
owns the share and turns it off when it exits.`,
		Example: `  qurl publish http://127.0.0.1:3000
  qurl publish http://localhost:8080 --id local-dashboard
  qurl publish https://api.example.com/reports
  qurl publish https://docs.example.com/handbook --public
  qurl publish https://grafana.internal.example.com --description "Team dashboard" --quiet`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validatePublishAccessFlags(cmd, public, private, allowedDeviceKeys); err != nil {
				return exitcode.UsageError(err)
			}
			named := publishAccessFlags(cmd)
			access := qurlapi.PublishOptions{
				Public: public, AllowedDeviceKeys: allowedDeviceKeys,
				KeepExistingPublic: len(named) == 0, NamedAccessFlags: named,
			}
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
				return runLocalPublish(cmd.Context(), opts, target, connectorID, foreground, &access)
			}
			if cmd.Flags().Changed("id") {
				return exitcode.UsageError(errors.New("--id applies only when publishing a loopback HTTP origin"))
			}
			if cmd.Flags().Changed("foreground") {
				return exitcode.UsageError(errors.New("--foreground applies only when publishing a loopback HTTP origin"))
			}
			if err := opts.requireRuntimeSupervisionIfNamespace(); err != nil {
				return err
			}
			client, err := opts.newClient(cmd.Context())
			if err != nil {
				return err
			}

			access.Description, access.Tags, access.Alias = description, tags, alias
			result, err := client.Publish(cmd.Context(), args[0], access)
			if err != nil {
				return err
			}

			printer := opts.printer()
			return printer.Publish(result)
		},
	}

	cmd.Flags().BoolVar(&public, "public", false, "let anyone who has the CRID open the resource (default: private, only you and the devices you allow)")
	// --private was how a private resource was published while public was the
	// default. Private is the default now, so the flag changes nothing. It is
	// kept, hidden, so a command copied from earlier documentation still runs.
	cmd.Flags().BoolVar(&private, "private", false, "publish a private resource (the default)")
	if err := cmd.Flags().MarkHidden("private"); err != nil {
		panic(err) // unreachable: the flag is defined on the line above
	}
	cmd.Flags().StringArrayVar(&allowedDeviceKeys, "allow-device-key", nil, "recipient public key allowed to open the private resource (repeatable)")
	cmd.Flags().StringVar(&description, "description", "", "human-readable description stored with the resource")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "tag stored with the resource (repeatable)")
	cmd.Flags().StringVar(&alias, "alias", "", "memorable handle stored with the resource")
	cmd.Flags().StringVar(&connectorID, "id", "", "Connector ID for a local publish (default: stable ID for this machine and origin)")
	cmd.Flags().BoolVar(&foreground, "foreground", false, "serve in this process for debugging or CI and stop sharing when it exits")

	return cmd
}

// publishAccessFlags returns the flags on the command line that said who may
// open the resource, as they were written. A publish that gave none asks for
// a private resource only because that is the default, so it keeps a target
// that is already published as public instead of failing: the person may
// have published it while public was the default and never chose either. A
// flag that was given, in any form, is a choice, and a resource that cannot
// be given that way stays a conflict, whose next step names these flags.
func publishAccessFlags(cmd *cobra.Command) []string {
	var named []string
	for _, name := range []string{"public", "private", "allow-device-key"} {
		flag := cmd.Flags().Lookup(name)
		if flag == nil || !flag.Changed {
			continue
		}
		written := "--" + name
		if flag.Value.Type() == "bool" && flag.Value.String() == "false" {
			written += "=false"
		}
		named = append(named, written)
	}
	return named
}

// validatePublishAccessFlags refuses flag combinations that contradict each
// other, before any state or network is touched. The hidden --private flag is
// accepted alone because it asks for what a publish does anyway. Its explicit
// false form is refused instead of ignored: read literally it asks for a
// public resource, and a private one must never be the silent answer to that,
// nor a public one to anything but --public.
func validatePublishAccessFlags(cmd *cobra.Command, public, private bool, allowedDeviceKeys []string) error {
	if cmd.Flags().Changed("private") {
		if public {
			return errors.New(msgPublicAndPrivate)
		}
		if !private {
			return errors.New(msgPrivateFalse)
		}
	}
	if public && len(allowedDeviceKeys) > 0 {
		return errors.New(msgAllowKeyWithPublic)
	}
	return validateAllowedDeviceKeys(allowedDeviceKeys)
}

func runLocalPublish(ctx context.Context, opts *globalOpts, target *publishTarget, flagID string, foreground bool, access *qurlapi.PublishOptions) (retErr error) {
	requestedID, err := validateLocalPublishRequest(ctx, opts, target, flagID, foreground)
	if err != nil {
		return err
	}
	stateDir, err := opts.resolveShareStateDir("")
	if err != nil {
		return err
	}
	if err := opts.requireRuntimeSupervision(stateDir); err != nil {
		return err
	}
	registry, err := opts.openShareRegistry(stateDir)
	if err != nil {
		return err
	}
	// The generated default ID needs the agent identity, which is only known
	// after cloud mutation begins. Checking its fixed prefix is exact: private
	// origin prefixes must end in a hyphen and the generated suffix has none,
	// so a prefix matches a generated ID only when it is that prefix itself.
	checkID := requestedID
	if checkID == "" {
		checkID = generatedLocalConnectorIDPrefix
	}
	if err := registry.ValidateTarget(ctx, checkID, target.localTarget()); err != nil {
		return err
	}
	ownerID, client, err := localPublishOwner(ctx, opts, registry, stateDir)
	if err != nil {
		return err
	}
	// Validate the owner-bound session authority before any Connector-resource
	// mutation. The short-lived discovery runtime below never transfers to
	// NewNativeAdmitter, so it must not retain that authority or non-default UDP
	// options.
	if _, err := opts.resolveSessionConfig(ownerID); err != nil {
		return err
	}
	enrollment := &localEnrollment{opts: opts, target: target, requestedID: requestedID, access: *access}
	// The registered REST client is open and cached before resource discovery
	// can request a Connector enrollment credential. Resource discovery can
	// therefore reuse it without a nested native-runtime open. The separate
	// resource runtime uses the completed-state fast path for the same state
	// directory; later mutations use operation leases, and qurl-connector locks
	// its session-operation journals across processes.
	// TODO(upstream-contract): Keep the completed-state fast path and journal
	// serialization assumption in lockstep with qurl-connector.
	resolved, knockResourceID, err := prepareLocalPublishResource(ctx, opts, enrollment, stateDir)
	if err != nil {
		return err
	}
	resource := resolved.Resource
	local, sharing, compensateOff, err := activateLocalPublish(ctx, client, registry, resource, knockResourceID, target)
	compensate := func(cause error) error {
		if !compensateOff {
			return cause
		}
		return compensateLocalPublish(client, registry, resource, cause)
	}
	if err != nil {
		return compensate(err)
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
	if err := preflightShareTarget(ctx, opts, target.localTarget()); err != nil {
		return "", err
	}
	return requestedID, nil
}

func localPublishOwner(ctx context.Context, opts *globalOpts, registry localShareRegistry, stateDir string) (string, qurlapi.Client, error) {
	ownerID, present, err := registry.OwnerID(ctx)
	if err != nil {
		return "", nil, err
	}
	client, err := opts.newClient(ctx)
	if err != nil {
		return "", nil, err
	}
	if present {
		return ownerID, client, nil
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
	// access is who may open the resource: public or private, and the first
	// list of allowed devices.
	access      qurlapi.PublishOptions
	opts        *globalOpts
	target      *publishTarget
	requestedID string

	mu                 sync.Mutex
	mintedID           string
	attemptAgentID     string
	attemptConnectorID string
	attemptKey         string
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
	idempotencyKey, err := e.enrollmentAttemptKey(request.AgentID, id)
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
	if token == nil || strings.TrimSpace(token.Token) == "" {
		return "", errors.New("qURL service returned an empty Connector enrollment credential")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.mintedID != "" && e.mintedID != id {
		return "", fmt.Errorf("local Connector identity changed during enrollment: %q then %q", e.mintedID, id)
	}
	e.mintedID = id
	return token.Token, nil
}

func (e *localEnrollment) enrollmentAttemptKey(agentID, connectorID string) (string, error) {
	agentID = strings.TrimSpace(agentID)
	connectorID = strings.TrimSpace(connectorID)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.attemptKey != "" {
		if e.attemptAgentID != agentID || e.attemptConnectorID != connectorID {
			return "", errors.New("local Connector identity changed during one enrollment attempt")
		}
		return e.attemptKey, nil
	}
	entropy := make([]byte, localEnrollmentEntropyBytes)
	if _, err := rand.Read(entropy); err != nil {
		return "", fmt.Errorf("generate Connector enrollment idempotency: %w", err)
	}
	key, err := localEnrollmentIdempotencyKey(agentID, connectorID, entropy)
	if err != nil {
		return "", err
	}
	e.attemptAgentID = agentID
	e.attemptConnectorID = connectorID
	e.attemptKey = key
	return key, nil
}

func (e *localEnrollment) recoveryCredential(context.Context) (string, error) {
	return e.opts.apiCredential()
}

func (e *localEnrollment) resolveID(ctx context.Context, stateDir, agentID string) (resolved string, retErr error) {
	id, err := e.connectorID(agentID)
	if err != nil {
		return "", err
	}
	resourceStore, err := connectorstate.Open(stateDir)
	if err != nil {
		return "", err
	}
	defer func() { retErr = errors.Join(retErr, resourceStore.Close()) }()
	if e.requestedID != "" {
		if err := resourceStore.PrepareConnectorResourceReuse(ctx, id); err != nil {
			return "", err
		}
	} else {
		id, _, err = resourceStore.ResolveDefaultConnectorID(ctx, id)
		if err != nil {
			return "", err
		}
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
	}
	// The Connector resource request that follows cannot say who may open the
	// resource, so the service would decide. The resource is therefore created
	// first, over the API, with its privacy stated and confirmed, for a public
	// resource as for a private one. The Connector request then finds that
	// resource, and the two answers must name the same one.
	var precreated *qurlapi.Published
	resolved, err = opts.resolveLocalResource(ctx, cfg, func(agentID string) (string, error) {
		id, err := enrollment.resolveID(ctx, stateDir, agentID)
		if err != nil {
			return "", err
		}
		client, err := opts.newClient(ctx)
		if err != nil {
			return "", err
		}
		access := enrollment.access
		access.ConnectorID = id
		precreated, err = client.Publish(ctx, "", access)
		if err != nil {
			return "", err
		}
		return id, nil
	})
	if err != nil {
		return nil, "", err
	}
	if precreated == nil || precreated.Private == nil {
		// No resource may be served whose privacy this command did not state.
		return nil, "", fmt.Errorf("%w: the resource was resolved before its privacy was stated; nothing was published", qurl.ErrInvalidAPIResponse)
	}
	if resolved == nil || resolved.Resource == nil || precreated.CRID != resolved.Resource.CRID || precreated.ResourceID != resolved.Resource.ResourcePublicKey {
		return nil, "", fmt.Errorf("%w: the resource created for this app does not match the Connector resource; check your published resources with `qurl list` before retrying", qurl.ErrInvalidAPIResponse)
	}
	resolved.Private = precreated.Private
	resolved.KeptPublic = precreated.KeptPublic
	// The Connector request always finds the resource created just above, so
	// its own answer cannot tell a first publish from a repeated one. The
	// create answer can.
	resolved.FoundExisting = precreated.FoundExisting
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
	if err := registry.ValidateTarget(ctx, resource.Slug, target.localTarget()); err != nil {
		return nil, nil, false, err
	}
	existing, err := registry.Get(ctx, resource.ResourcePublicKey)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, false, err
	}
	localMissing := errors.Is(err, os.ErrNotExist)
	localPresent := err == nil
	targetChanged := localPresent && (existing.Target() != target.localTarget())
	prior, err := client.Sharing(ctx, resource.CRID)
	if err != nil {
		return nil, nil, false, err
	}
	if prior.ResourceID != resource.ResourcePublicKey || prior.CRID != resource.CRID {
		return nil, nil, false, errors.New("qURL sharing response identity does not match the published resource")
	}
	var sharing *qurlapi.Sharing
	terminalRecovery := localPresent && existing.DesiredState == string(qurlapi.DesiredStateOff) && prior.DesiredState == qurlapi.DesiredStateOn
	restartRequired := targetChanged || (localMissing && prior.DesiredState == qurlapi.DesiredStateOn) || terminalRecovery
	compensateOff := restartRequired || prior.DesiredState == qurlapi.DesiredStateOff
	if restartRequired {
		sharing, err = restartSharingReconciled(ctx, client, resource.CRID, prior)
	} else {
		sharing, err = client.SetSharing(ctx, resource.CRID, qurlapi.DesiredStateOn)
	}
	if err != nil {
		return nil, nil, compensateOff, err
	}
	local := &connectorstate.LocalShare{
		CRID: resource.CRID, ResourceID: resource.ResourcePublicKey, ConnectorID: resource.Slug,
		ConnectorRoutingID: resource.ConnectorRoutingID, KnockResourceID: knockResourceID,
		TargetURL: target.canonicalOrigin, LocalIP: target.localIP, LocalPort: target.localPort, LocalSocketPath: target.localSocketPath,
		DesiredState: string(sharing.DesiredState), ServingEpoch: sharing.ServingEpoch,
	}
	return local, sharing, compensateOff, nil
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
		localErr = persistCompensatingOff(ctx, registry, resource.ResourcePublicKey, resource.CRID, off, true)
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
	daemon, err := opts.newShareDaemon(stateDir, logDir)
	if err != nil {
		return compensate(err)
	}
	if err := daemon.Ensure(ctx); err != nil {
		return compensate(err)
	}
	sharing, err := waitForSharingWithDiagnostics(ctx, opts, client, local, stateDir, local.ServingEpoch, opts.sharingWaitLimit)
	if err != nil {
		return err
	}
	return printLocalPublishServing(opts, resolved, local, sharing)
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
				// earlier publish error. Remove only the expected cancellation;
				// a joined shutdown or manager failure must remain actionable.
				retErr = errors.Join(retErr, withoutExpectedDaemonCancellation(stopErr))
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
		// Foreground mode deliberately takes ownership of cloud/local desired
		// state, even when the share was already on. Leaving the process must not
		// strand a desired-on share without this foreground process as its owner.
		retErr = compensateLocalPublish(client, registry, resolved.Resource, retErr)
	}()
	jobVersion, err := connectordaemon.JobVersion(opts.version, opts.resolvedShareGroupMode)
	if err != nil {
		return err
	}
	socketPath, err := connectordaemon.SocketPathForStateDir(stateDir, opts.lookupEnv)
	if err != nil {
		return err
	}
	daemonCtx, cancel := context.WithCancel(ctx)
	cancelDaemon = cancel
	daemonErr = make(chan error, 1)
	go func() { daemonErr <- opts.runForegroundDaemon(daemonCtx, opts, stateDir, jobVersion) }()
	ipc := connectordaemon.IPCClient{SocketPath: socketPath}
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
	sharing, err := waitForSharingWithDiagnostics(ctx, opts, client, local, stateDir, local.ServingEpoch, opts.sharingWaitLimit)
	if err != nil {
		return err
	}
	if err := printLocalPublishServing(opts, resolved, local, sharing); err != nil {
		return err
	}
	retErr = <-daemonErr
	joined = true
	return retErr
}

func withoutExpectedDaemonCancellation(err error) error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		kept := make([]error, 0, len(joined.Unwrap()))
		for _, cause := range joined.Unwrap() {
			if cause = withoutExpectedDaemonCancellation(cause); cause != nil {
				kept = append(kept, cause)
			}
		}
		return errors.Join(kept...)
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		cause := wrapped.Unwrap()
		if !errors.Is(cause, context.Canceled) {
			return err
		}
		// A normal annotation can wrap either expected cancellation alone or a
		// multi-cause shutdown error. Descend to the leaves: annotation around
		// cancellation alone is expected shutdown detail and is removed, while
		// every independent daemon-failure cause remains actionable.
		return withoutExpectedDaemonCancellation(cause)
	}
	// This filter runs only after this function canceled the daemon context.
	// Platform IPC can identify that expected cancellation. Remove that leaf
	// so it cannot mask the publish failure that caused shutdown.
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// printLocalPublishServing renders a serving local publish. The publisher in
// the JSON document comes from the sharing state that confirmed serving, so it
// costs no extra request; a service that does not report one leaves it
// unnamed and unverified. The text document deliberately has no publisher
// row: a first publish stays calm, and the status is shown where a recipient
// decides (share, get) and where an owner asks (status, inspect, publisher).
func printLocalPublishServing(opts *globalOpts, resolved *agent.ResolvedResource, local *connectorstate.LocalShare, sharing *qurlapi.Sharing) error {
	published := &qurlapi.Published{
		CRID: local.CRID, ResourceID: local.ResourceID, TargetURL: local.TargetURL,
		Status: "serving", FoundExisting: resolved.FoundExisting, Private: resolved.Private,
		KeptPublic: resolved.KeptPublic,
	}
	if sharing != nil {
		published.Publisher = sharing.Publisher
	}
	return opts.printer().Publish(published)
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

func validateAllowedDeviceKeys(keys []string) error {
	return validateDeviceKeys("--allow-device-key", keys)
}

// validateDeviceKeys checks the device public keys given to one flag, named
// in the message, before anything is sent: publish and grants apply the same
// rule to every list.
func validateDeviceKeys(flag string, keys []string) error {
	// TODO(upstream-contract): service grants allow at most 256 unique,
	// canonical padded-base64 X25519 public keys.
	if len(keys) > 256 {
		return fmt.Errorf("%s accepts at most 256 keys", flag)
	}
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		raw, err := base64.StdEncoding.DecodeString(key)
		if err != nil || len(raw) != devicePublicKeySize || base64.StdEncoding.EncodeToString(raw) != key || seen[key] {
			return fmt.Errorf("%s requires unique canonical base64 X25519 public keys", flag)
		}
		seen[key] = true
	}
	return nil
}
