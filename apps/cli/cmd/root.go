package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	qurl "github.com/layervai/qurl-go/qurl"
	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
	"github.com/layervai/qurl-integrations/apps/cli/internal/config"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	connectordaemon "github.com/layervai/qurl-integrations/apps/cli/internal/connector/daemon"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/hub"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/sessionconfig"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/consume"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
)

const (
	httpURLScheme  = "http"
	httpsURLScheme = "https"
)

// globalOpts carries flag values, injected process context, and the settings
// resolved from the flag > env > profile > default precedence chain.
type globalOpts struct {
	// Flag-bound.
	endpoint  string
	format    string
	quiet     bool
	colorMode string
	verbose   bool
	profile   string

	version string

	// Injected process context; tests override via root options.
	streams      *output.Streams
	lookupEnv    func(string) (string, bool)
	configDir    string
	now          func() time.Time
	sleep        func(time.Duration)
	newRequestID func() string
	// newCredentialStore builds the storage chain; tests inject a fake
	// keyring so unit tests never touch a developer's real one.
	newCredentialStore func(dir string, onFileRead func()) *auth.Chain
	// openBrowser launches the user's browser at an already-verified link;
	// tests inject a recorder so no real browser ever starts under test.
	openBrowser func(ctx context.Context, link string) error
	// enterPortal asks the qURL platform for direct access to an
	// already-verified link and returns the granted content URL for
	// download; production wiring is consume.AccessOpener over the SDK
	// opener. Tests always inject (the harness refuses by default), so no
	// test ever sends a real access request.
	enterPortal func(ctx context.Context, link string) (string, error)

	// redirectFRPLogs rebinds the FRP library's process-global logger to this
	// invocation's stderr (production default). The cmd test binary injects a
	// no-op and pins the global once in TestMain instead, because its
	// in-process tunnel server logs through the same global concurrently.
	redirectFRPLogs      func()
	loadLocalShares      func(context.Context) ([]connectorstate.LocalShare, error)
	openShareRegistry    func(string) (localShareRegistry, error)
	newShareDaemon       func(string, string) shareDaemonController
	preflightTarget      func(context.Context, string, int) error
	resolveShareStateDir func(string) (string, error)
	resolveLocalResource localResourceResolver
	resolveHubBootstrap  func() (qurl.HubBootstrap, error)
	resolveSessionConfig func(string) (connectorshare.NativeSessionOperationAuthority, error)
	runForegroundDaemon  func(context.Context, *globalOpts, string, string) error
	sharingWaitLimit     time.Duration
	// backgroundShareGOOS is the platform contract used by lifecycle commands.
	// Production pins it to runtime.GOOS; hermetic tests inject darwin so they
	// can exercise the daemon control plane on every CI runner without enabling
	// unsupported production paths.
	backgroundShareGOOS string

	// openAPIClient is the hermetic command-test seam. Production leaves it
	// nil and opens the persisted registered-device client below.
	openAPIClient func(context.Context) (qurlapi.Client, error)
	// openRegisteredClient is the login/bootstrap seam. Production uses the
	// native NHP registration path; command tests inject a platform-only
	// client because their mock server does not implement NHP.
	openRegisteredClient func(context.Context, qurlapi.AccountClient, string, *qurlapi.Identity) (qurlapi.Client, *qurlapi.Identity, error)
	openNativeRuntime    func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error)
	registeredClient     qurlapi.Client
	registeredIdentity   *qurlapi.Identity
	nativeRuntime        registeredNativeRuntime
	warnedCleartextAuth  bool

	// Resolved in PersistentPreRunE.
	resolved           bool
	resolvedEndpoint   string
	resolvedFormat     output.Format
	outColor           bool
	errColorOn         bool
	ascii              bool
	profileConnectorID string
}

// rootOption is a test hook for injecting process context.
type rootOption func(*globalOpts)

type registeredNativeRuntime interface {
	Handoff() (qurl.AgentStateStore, error)
	Close() error
}

// Main wires the real process context and runs the CLI. It returns the exit
// code; main() is the only caller of os.Exit. SIGTERM joins the interrupt set
// for the foreground daemon and supervised invocations; on Windows the extra
// signal is simply never delivered.
func Main(version string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	root, opts := newRoot(version, output.Detect())
	return run(ctx, root, opts)
}

// run executes the tree, renders any error to stderr, and maps it to the one
// exit code. A cancellation the user caused (Ctrl-C / SIGTERM) keeps the
// Interrupted exit code but renders no error anatomy: the interrupt was the
// user's own act.
func run(ctx context.Context, root *cobra.Command, opts *globalOpts) int {
	err := root.ExecuteContext(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		output.RenderError(opts.streams.Err, err, opts.errColor())
	}
	if closeErr := opts.closeAPIClient(); closeErr != nil {
		// The command has already completed. Process exit releases remaining OS
		// handles, so native-runtime teardown cannot reverse a successful remote
		// or lifecycle operation. Keep the diagnostic without changing its exit.
		opts.printer().Warnf("local native-state cleanup reported a problem: %v", closeErr)
	}
	return exitcode.FromError(err)
}

// newRoot builds the v2 command tree.
func newRoot(version string, streams *output.Streams, options ...rootOption) (*cobra.Command, *globalOpts) {
	opts := &globalOpts{
		version:             version,
		streams:             streams,
		lookupEnv:           os.LookupEnv,
		now:                 time.Now,
		backgroundShareGOOS: runtime.GOOS,
	}
	for _, opt := range options {
		opt(opts)
	}
	opts.applyDefaults()

	cmd := &cobra.Command{
		Use:   "qurl",
		Short: "Publish, resolve, and manage qURL resources by CRID",
		Long: `Publish a local app or remote URL as a protected qURL resource, then use
its CRID to open it when access is authorized.

A CRID is a permanent, shareable resource ID — it contains no secret and grants
no access by itself. Authorized users turn it into a short-lived access link
with "qurl get" or "qurl resolve".

Authentication: use ` + "`qurl login`" + ` to enroll this machine. The account API key is
used only for enrollment and is not stored by qurl. Scripts and CI can set
QURL_API_KEY for the same one-time bootstrap.`,
		Example: "  qurl publish http://127.0.0.1:3000\n" +
			"  qurl get " + exampleCRID + "\n" +
			"  qurl publish https://api.example.com/reports",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return exitcode.UsageError(fmt.Errorf("unknown command %q — run `qurl --help` for the command list", args[0]))
		},
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			// Task Scheduler starts `daemon run` directly. Redirect before
			// settings resolution so a malformed profile or environment value is
			// present in the durable log on the first background start.
			if cmd.Name() == "run" && cmd.Parent() != nil && cmd.Parent().Name() == "daemon" {
				stdoutPath, err := cmd.Flags().GetString("job-stdout-log")
				if err != nil {
					return err
				}
				stderrPath, err := cmd.Flags().GetString("job-stderr-log")
				if err != nil {
					return err
				}
				if err := redirectDaemonJobOutput(stdoutPath, stderrPath, opts.streams); err != nil {
					return err
				}
			}
			if skipsSettings(cmd) {
				return nil
			}
			return opts.resolveSettings()
		},
	}

	flags := cmd.PersistentFlags()
	flags.StringVar(&opts.endpoint, "endpoint", "", "qURL API endpoint (default "+config.DefaultEndpoint+")")
	flags.StringVarP(&opts.format, "output", "o", "", "output format: text or json (default text)")
	flags.BoolVarP(&opts.quiet, "quiet", "q", false, "print only the primary value, one per line")
	flags.StringVar(&opts.colorMode, "color", "", "colorize output: auto, always, or never (default auto)")
	flags.BoolVarP(&opts.verbose, "verbose", "v", false, "print request diagnostics on stderr")
	flags.StringVar(&opts.profile, "profile", "", "configuration profile name")

	cmd.SetOut(streams.Out)
	cmd.SetErr(streams.Err)
	cmd.SetIn(streams.In)
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return exitcode.UsageError(err)
	})

	cmd.AddCommand(
		publishCmd(opts),
		resolveCmd(opts),
		getCmd(opts),
		listCmd(opts),
		shareStartCmd(opts),
		shareStopCmd(opts),
		shareRestartCmd(opts),
		shareStatusCmd(opts),
		shareInspectCmd(opts),
		deleteCmd(opts),
		daemonCmd(opts),
		loginCmd(opts),
		logoutCmd(opts),
		whoamiCmd(opts),
		versionCmd(version),
		completionCmd(),
		docsCmd(),
	)

	return cmd, opts
}

func (o *globalOpts) applyDefaults() {
	if o.configDir == "" {
		o.configDir = config.DefaultDir()
	}
	if o.newCredentialStore == nil {
		o.newCredentialStore = auth.NewStore
	}
	if o.openBrowser == nil {
		launcher := &consume.Launcher{LookupEnv: o.lookupEnv, GOOS: runtime.GOOS}
		o.openBrowser = launcher.Open
	}
	if o.enterPortal == nil {
		opener := &consume.AccessOpener{LookupEnv: o.lookupEnv}
		o.enterPortal = opener.Open
	}
	if o.redirectFRPLogs == nil {
		o.redirectFRPLogs = func() { redirectFRPLogsToStderr(o) }
	}
	if o.runForegroundDaemon == nil {
		o.runForegroundDaemon = runShareDaemon
	}
	if o.resolveShareStateDir == nil {
		o.resolveShareStateDir = connectorstate.ResolveDir
	}
	if o.loadLocalShares == nil {
		o.loadLocalShares = func(ctx context.Context) ([]connectorstate.LocalShare, error) {
			dir, err := o.resolveShareStateDir("")
			if err != nil {
				return nil, err
			}
			shares, _, err := connectorstate.ReadLocalSharesIfPresent(ctx, dir)
			return shares, err
		}
	}
	if o.openShareRegistry == nil {
		o.openShareRegistry = func(dir string) (localShareRegistry, error) {
			return connectorstate.OpenLocalShareRegistry(dir)
		}
	}
	if o.newShareDaemon == nil {
		o.newShareDaemon = func(stateDir, logDir string) shareDaemonController {
			return connectordaemon.NewJobController(stateDir, logDir, o.version, o.resolvedEndpoint, o.resolveHubBootstrap)
		}
	}
	if o.preflightTarget == nil {
		o.preflightTarget = preflightLocalTarget
	}
	if o.resolveLocalResource == nil {
		o.resolveLocalResource = resolveLocalPublishResource
	}
	if o.resolveHubBootstrap == nil {
		o.resolveHubBootstrap = hub.Bootstrap
	}
	if o.resolveSessionConfig == nil {
		o.resolveSessionConfig = sessionconfig.Resolve
	}
	if o.sharingWaitLimit <= 0 {
		o.sharingWaitLimit = 30 * time.Second
	}
	if o.openRegisteredClient == nil {
		o.openRegisteredClient = o.openNativeRegisteredClient
	}
	if o.openNativeRuntime == nil {
		o.openNativeRuntime = func(ctx context.Context, cfg connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
			return connectorshare.OpenNativeRuntime(ctx, cfg)
		}
	}
}

// resolveSettings applies the precedence chain (flag > env > profile >
// default) once per invocation and validates enum-valued settings.
func (o *globalOpts) resolveSettings() error {
	profile := o.profile
	if profile == "" {
		if v, ok := o.lookupEnv("QURL_PROFILE"); ok {
			profile = v
		}
	}
	cfg, err := config.LoadProfile(o.configDir, profile)
	if err != nil {
		return err
	}

	o.resolvedEndpoint = config.Resolve(o.endpoint, "QURL_ENDPOINT", o.lookupEnv, cfg.Endpoint, config.DefaultEndpoint)

	format := config.Resolve(o.format, "QURL_OUTPUT", o.lookupEnv, cfg.Output, string(output.FormatText))
	o.resolvedFormat = output.Format(format)
	if !output.ValidFormat(o.resolvedFormat) {
		return exitcode.UsageError(fmt.Errorf("invalid output format %q: must be %q or %q", format, output.FormatText, output.FormatJSON))
	}

	colorMode := config.Resolve(o.colorMode, "QURL_COLOR", o.lookupEnv, cfg.Color, output.ColorAuto)
	if colorMode != output.ColorAuto && colorMode != output.ColorAlways && colorMode != output.ColorNever {
		return exitcode.UsageError(fmt.Errorf("invalid color mode %q: must be auto, always, or never", colorMode))
	}
	o.outColor = output.ResolveColor(colorMode, o.lookupEnv, o.streams.OutIsTTY)
	o.errColorOn = output.ResolveColor(colorMode, o.lookupEnv, o.streams.ErrIsTTY)
	o.ascii = output.ResolveASCII(o.lookupEnv)
	o.profileConnectorID = cfg.ConnectorID
	o.resolved = true
	return nil
}

// printer builds the per-invocation Printer from the resolved settings.
func (o *globalOpts) printer() *output.Printer {
	return output.New(o.streams, o.resolvedFormat, o.quiet, o.outColor, o.ascii, o.now)
}

// insecureEndpointWarning returns a warning when the endpoint would carry an
// authorization credential over cleartext http to a non-loopback host. Loopback
// is exempt: local mocks and harnesses are legitimately plain http. The
// transport already refuses redirects so the credential cannot follow a
// Location elsewhere; this closes the sibling misconfiguration.
func insecureEndpointWarning(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != httpURLScheme {
		return ""
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return ""
	}
	return fmt.Sprintf(msgInsecureEndpoint, endpoint)
}

// warnInsecureEndpoint emits at most one warning per CLI invocation. Both the
// one-time account bootstrap and the steady-state registered-device client
// call it, and recovery can use both paths during one invocation.
func (o *globalOpts) warnInsecureEndpoint() {
	if o.warnedCleartextAuth {
		return
	}
	warning := insecureEndpointWarning(o.resolvedEndpoint)
	if warning == "" {
		return
	}
	o.warnedCleartextAuth = true
	o.printer().Warnf("%s", warning)
}

// skipsSettings reports whether cmd (or an ancestor) must answer without
// touching configuration: version, completion (and cobra's hidden __complete
// machinery, which runs on every shell TAB), docs, and help. A malformed or
// secret-bearing legacy config file must never brick shell startup
// (`eval "$(qurl completion bash)"`) or `qurl version`; none of these
// commands read settings, credentials, or the network.
func skipsSettings(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		switch c.Name() {
		case "version", "completion", "docs", "help", "validate-test-resource":
			return true
		}
		if strings.HasPrefix(c.Name(), "__complete") {
			return true
		}
	}
	return false
}

// errColor answers whether stderr rendering gets color, falling back to a
// pre-resolution default when flag parsing itself failed.
func (o *globalOpts) errColor() bool {
	if o.resolved {
		return o.errColorOn
	}
	return output.ResolveColor(output.ColorAuto, o.lookupEnv, o.streams.ErrIsTTY)
}

// credentialStore builds the legacy account-key storage chain. Current login
// never writes it; successful enrollment and logout use it only to remove
// copies left by earlier CLI builds.
func (o *globalOpts) credentialStore() *auth.Chain {
	return o.newCredentialStore(o.configDir, func() {
		o.printer().Warnf("%s", msgKeyringUnavailable)
	})
}

// newClient opens the persisted registered-device identity. The account key
// is consulted only when the native state is missing or the Hub explicitly
// rejects the stored device credential. A warm command does not read it.
func (o *globalOpts) newClient(ctx context.Context) (qurlapi.Client, error) {
	o.warnInsecureEndpoint()
	if o.registeredClient != nil {
		return o.registeredClient, nil
	}
	if o.openAPIClient != nil {
		client, err := o.openAPIClient(ctx)
		if err != nil {
			return nil, err
		}
		o.registeredClient = client
		return client, nil
	}
	client, identity, err := o.openRegisteredClient(ctx, nil, "", nil)
	if err != nil {
		return nil, err
	}
	o.registeredClient = client
	o.registeredIdentity = identity
	return client, nil
}

// apiCredential resolves one account credential from the current process
// environment without retaining it. Native Connector recovery calls this
// lazily only after the pinned Hub has rejected a persisted device credential;
// ordinary warm starts do not read it or pass it into the background daemon.
// v2 has no stored-account-key compatibility path.
func (o *globalOpts) apiCredential() (string, error) {
	key, _, err := auth.Resolve(o.lookupEnv, nil)
	if err != nil {
		return "", err
	}
	if err := auth.ValidateKeyShape(key); err != nil {
		return "", err
	}
	return key, nil
}

// apiClient builds the API client around one explicit key. login uses it
// directly (the key it validates is the one just typed, never a stored one);
// everything else goes through newClient.
func (o *globalOpts) apiClient(key string) (qurlapi.AccountClient, error) {
	o.warnInsecureEndpoint()
	return qurlapi.New(&qurlapi.Config{
		BaseURL:      o.resolvedEndpoint,
		APIKey:       key,
		Version:      o.version,
		Verbose:      o.verboseLogger(),
		Sleep:        o.sleep,
		NewRequestID: o.newRequestID,
	})
}

// registeredAccountBootstrap owns the account-key capability only during one
// registration or recovery attempt. It loads the key lazily unless login
// supplies it explicitly.
type registeredAccountBootstrap struct {
	opts                     *globalOpts
	client                   qurlapi.AccountClient
	key                      string
	identity                 *qurlapi.Identity
	enrollmentIdempotencyKey string
	lazy                     bool
	used                     bool
}

type deviceAccountConflictError struct {
	stateDir       string
	deviceKeyID    string
	currentOwner   string
	requestedOwner string
}

func (e *deviceAccountConflictError) Error() string {
	device := "the registered device"
	if e.deviceKeyID != "" {
		device = fmt.Sprintf("registered device key %q", e.deviceKeyID)
	}
	return fmt.Sprintf(
		"%s in %q belongs to account %q, not %q; to switch accounts, first revoke that device key in the qURL dashboard, then move or remove the complete state directory and run `qurl login` again; do not edit individual state files",
		device, e.stateDir, e.currentOwner, e.requestedOwner,
	)
}

func (*deviceAccountConflictError) Unwrap() error { return auth.ErrDeviceAccountConflict }

func bindRegisteredDeviceOwner(
	ctx context.Context,
	registry localShareRegistry,
	stateDir, deviceKeyID, deviceOwner string,
) error {
	boundOwner, bound, err := registry.OwnerID(ctx)
	if err != nil {
		return err
	}
	if bound && boundOwner != deviceOwner {
		return &deviceAccountConflictError{
			stateDir: stateDir, deviceKeyID: deviceKeyID,
			currentOwner: boundOwner, requestedOwner: deviceOwner,
		}
	}
	if bound {
		return nil
	}
	if err := registry.BindOwner(ctx, deviceOwner); err != nil {
		if !errors.Is(err, connectorstate.ErrLocalShareOwnerConflict) {
			return err
		}
		// Another process can bind the registry between OwnerID and
		// BindOwner. Re-read it so the recovery message identifies the
		// account that actually won the durable race.
		if latestOwner, present, readErr := registry.OwnerID(ctx); readErr == nil && present {
			boundOwner = latestOwner
		}
		return &deviceAccountConflictError{
			stateDir: stateDir, deviceKeyID: deviceKeyID,
			currentOwner: boundOwner, requestedOwner: deviceOwner,
		}
	}
	return nil
}

func newRegisteredAccountBootstrap(opts *globalOpts, client qurlapi.AccountClient, key string, identity *qurlapi.Identity) *registeredAccountBootstrap {
	return &registeredAccountBootstrap{opts: opts, client: client, key: key, identity: identity, lazy: client == nil}
}

func (b *registeredAccountBootstrap) load(ctx context.Context) (qurlapi.AccountClient, string, *qurlapi.Identity, error) {
	if b.client == nil {
		key, err := b.opts.apiCredential()
		if err != nil {
			return nil, "", nil, err
		}
		client, err := b.opts.apiClient(key)
		if err != nil {
			return nil, "", nil, err
		}
		b.client, b.key = client, key
	}
	if b.identity == nil {
		identity, err := b.client.Me(ctx)
		if err != nil {
			return nil, "", nil, err
		}
		b.identity = identity
	}
	b.used = true
	return b.client, b.key, b.identity, nil
}

func (b *registeredAccountBootstrap) enrollmentCredential(ctx context.Context, request qurl.AgentEnrollmentCredentialRequest) (string, error) {
	if strings.TrimSpace(request.AgentID) == "" {
		return "", errors.New("registered-device enrollment has no durable agent ID")
	}
	client, _, _, err := b.load(ctx)
	if err != nil {
		return "", err
	}
	if b.enrollmentIdempotencyKey == "" {
		var nonce [32]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return "", fmt.Errorf("create device enrollment request identity: %w", err)
		}
		// Scope the key to this enrollment attempt. Repeated provider calls and
		// HTTP retries reuse it, but a new process can never receive a cached,
		// expired one-shot token solely because an operator pinned the agent ID.
		b.enrollmentIdempotencyKey = hex.EncodeToString(nonce[:])
	}
	token, err := client.MintAgentEnrollmentToken(ctx, qurlapi.MintAgentEnrollmentTokenOptions{
		IdempotencyKey: b.enrollmentIdempotencyKey,
	})
	if err != nil {
		return "", err
	}
	if token == nil || strings.TrimSpace(token.Token) == "" {
		return "", errors.New("qURL service returned an empty device enrollment credential")
	}
	return token.Token, nil
}

func (b *registeredAccountBootstrap) recoveryCredential(ctx context.Context) (string, error) {
	_, key, _, err := b.load(ctx)
	return key, err
}

func (b *registeredAccountBootstrap) retireLegacyKey() {
	if !b.lazy || !b.used {
		return
	}
	if _, err := b.opts.credentialStore().Delete(); err != nil {
		// Registration is already durable. Match explicit login: a stale
		// compatibility-key cleanup failure must not turn enrollment into a
		// false command failure.
		b.opts.printer().Warnf("machine enrollment succeeded, but qurl could not remove a legacy stored account key: %v", err)
	}
}

// openNativeRegisteredClient opens or creates the machine identity through
// NHP, then builds the narrow REST client from the durable device credential.
// account is non-nil only for an explicit login. Otherwise the enrollment and
// recovery callbacks resolve an account key lazily, after the Hub proves one
// is needed.
func (o *globalOpts) openNativeRegisteredClient(
	ctx context.Context,
	account qurlapi.AccountClient,
	accountKey string,
	accountIdentity *qurlapi.Identity,
) (_ qurlapi.Client, _ *qurlapi.Identity, retErr error) {
	if o.nativeRuntime != nil {
		return nil, nil, errors.New("registered-device runtime is already open")
	}
	stateDir, err := o.resolveShareStateDir("")
	if err != nil {
		return nil, nil, err
	}
	hubBootstrap, err := o.resolveHubBootstrap()
	if err != nil {
		return nil, nil, err
	}
	origin, err := agent.ResourceSDKOrigin(o.resolvedEndpoint)
	if err != nil {
		return nil, nil, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return nil, nil, fmt.Errorf("read local hostname: %w", err)
	}

	bootstrap := newRegisteredAccountBootstrap(o, account, accountKey, accountIdentity)

	nativeRuntime, err := o.openNativeRuntime(ctx, connectorshare.NativeRuntimeConfig{
		StateDir:                     stateDir,
		AgentID:                      connectorstate.ConfiguredAgentID(),
		Hub:                          hubBootstrap,
		Hostname:                     hostname,
		Version:                      o.version,
		ClientBaseURL:                origin,
		EnrollmentCredentialProvider: bootstrap.enrollmentCredential,
		RecoveryCredentialProvider:   bootstrap.recoveryCredential,
		RefreshMode:                  connectorRefreshModeAuto,
		// This runtime only establishes the device credential used by the
		// registered REST client. It never starts or changes a local share, so
		// the owner-bound SessionOperations authority is intentionally absent.
	})
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, nativeRuntime.Close())
		}
	}()
	store, err := nativeRuntime.Handoff()
	if err != nil {
		return nil, nil, err
	}
	client, err := qurlapi.NewRegistered(ctx, &qurlapi.Config{
		BaseURL:      origin,
		Version:      o.version,
		Verbose:      o.verboseLogger(),
		Sleep:        o.sleep,
		NewRequestID: o.newRequestID,
	}, store)
	if err != nil {
		return nil, nil, err
	}
	deviceIdentity, err := client.Me(ctx)
	if err != nil {
		return nil, nil, err
	}
	if deviceIdentity == nil {
		return nil, nil, errors.New("qURL account identity response is empty")
	}
	deviceKeyID := ""
	if deviceIdentity.Key != nil {
		deviceKeyID = deviceIdentity.Key.KeyID
	}
	if bootstrap.identity != nil && bootstrap.identity.OwnerID != deviceIdentity.OwnerID {
		return nil, nil, &deviceAccountConflictError{
			stateDir: stateDir, deviceKeyID: deviceKeyID,
			currentOwner: deviceIdentity.OwnerID, requestedOwner: bootstrap.identity.OwnerID,
		}
	}
	registry, err := o.openShareRegistry(stateDir)
	if err != nil {
		return nil, nil, err
	}
	if err := bindRegisteredDeviceOwner(ctx, registry, stateDir, deviceKeyID, deviceIdentity.OwnerID); err != nil {
		return nil, nil, err
	}
	bootstrap.retireLegacyKey()
	o.nativeRuntime = nativeRuntime
	return client, deviceIdentity, nil
}

func (o *globalOpts) closeAPIClient() error {
	if o.nativeRuntime == nil {
		return nil
	}
	err := o.nativeRuntime.Close()
	o.nativeRuntime = nil
	o.registeredClient = nil
	o.registeredIdentity = nil
	return err
}

func (o *globalOpts) verboseLogger() func(string, ...any) {
	if !o.verbose {
		return nil
	}
	return func(format string, args ...any) {
		// Diagnostics are best-effort; a broken stderr must not fail the run.
		_, _ = fmt.Fprintf(o.streams.Err, "[debug] "+format+"\n", args...)
	}
}

// productionEndpoint reports whether the resolved endpoint is production,
// for the CRID environment guard.
func (o *globalOpts) productionEndpoint() bool {
	return config.IsProductionEndpoint(o.resolvedEndpoint)
}

// exactArgs wraps cobra.ExactArgs so arity mistakes map to the usage exit
// code.
func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := cobra.ExactArgs(n)(cmd, args); err != nil {
			return exitcode.UsageError(err)
		}
		return nil
	}
}

// noArgs wraps cobra.NoArgs the same way.
func noArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return exitcode.UsageError(err)
	}
	return nil
}
