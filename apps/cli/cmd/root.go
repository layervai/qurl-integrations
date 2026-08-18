package main

import (
	"context"
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

	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
	"github.com/layervai/qurl-integrations/apps/cli/internal/config"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/supervisor"
	"github.com/layervai/qurl-integrations/apps/cli/internal/consume"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
	"github.com/layervai/qurl-integrations/apps/cli/internal/output"
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

	// Connector seams. openConnectorRuntime walks the agent enroll/open
	// ladder (production: agent.Open) and newConnectorKnocker builds the
	// per-cycle platform client from the opened runtime (production:
	// knock.NewNative over the runtime's binding); tests inject fakes so cmd
	// tests never touch the real UDP wire. tuneConnectorSupervisor, when
	// non-nil, adjusts the supervisor config before construction — test-only,
	// mirroring the supervisor package's own timing seams.
	openConnectorRuntime    func(ctx context.Context, cfg *agent.Config) (*agent.Runtime, error)
	newConnectorKnocker     func(rt *agent.Runtime, knockResourceID string) (connectorKnocker, error)
	tuneConnectorSupervisor func(cfg *supervisor.Config)
	// redirectFRPLogs rebinds the FRP library's process-global logger to this
	// invocation's stderr (production default). The cmd test binary injects a
	// no-op and pins the global once in TestMain instead, because its
	// in-process tunnel server logs through the same global concurrently.
	redirectFRPLogs func()

	// Resolved in PersistentPreRunE.
	resolved           bool
	resolvedEndpoint   string
	resolvedFormat     output.Format
	outColor           bool
	errColorOn         bool
	ascii              bool
	profileConnectorID string
	// profileConnectorSlug carries the deprecated v1.1.0 profile key
	// (connector_slug), honored below profileConnectorID.
	//
	// Deprecated: remove at the next major.
	profileConnectorSlug string
}

// rootOption is a test hook for injecting process context.
type rootOption func(*globalOpts)

// Main wires the real process context and runs the CLI. It returns the exit
// code; main() is the only caller of os.Exit. SIGTERM joins the interrupt
// set for the long-running serve command (`connector run` under an
// orchestrator stops via SIGTERM); on Windows the extra signal is simply
// never delivered.
func Main(version string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	root, opts := newRoot(version, output.Detect())
	return run(ctx, root, opts)
}

// run executes the tree, renders any error to stderr, and maps it to the one
// exit code. A cancellation the user caused (Ctrl-C / SIGTERM) keeps the
// Interrupted exit code but renders no error anatomy: the interrupt was the
// user's own act, and commands that want a farewell print their own note
// (connector run's msgConnectorStopped).
func run(ctx context.Context, root *cobra.Command, opts *globalOpts) int {
	err := root.ExecuteContext(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		output.RenderError(opts.streams.Err, err, opts.errColor())
	}
	return exitcode.FromError(err)
}

// newRoot builds the v2 command tree.
func newRoot(version string, streams *output.Streams, options ...rootOption) (*cobra.Command, *globalOpts) {
	opts := &globalOpts{
		version:   version,
		streams:   streams,
		lookupEnv: os.LookupEnv,
		now:       time.Now,
	}
	for _, opt := range options {
		opt(opts)
	}
	if opts.configDir == "" {
		opts.configDir = config.DefaultDir()
	}
	if opts.newCredentialStore == nil {
		opts.newCredentialStore = auth.NewStore
	}
	if opts.openBrowser == nil {
		// The launcher reads the override variables through the same
		// injected environment the rest of the CLI uses.
		launcher := &consume.Launcher{LookupEnv: opts.lookupEnv, GOOS: runtime.GOOS}
		opts.openBrowser = launcher.Open
	}
	if opts.openConnectorRuntime == nil {
		opts.openConnectorRuntime = agent.Open
	}
	if opts.newConnectorKnocker == nil {
		opts.newConnectorKnocker = newNativeConnectorKnocker
	}
	if opts.redirectFRPLogs == nil {
		opts.redirectFRPLogs = func() { redirectFRPLogsToStderr(opts) }
	}

	cmd := &cobra.Command{
		Use:   "qurl",
		Short: "Publish, resolve, and manage qURL resources by CRID",
		Long: `The qURL CLI publishes URLs as protected resources and turns their CRIDs
back into working access links.

A CRID is a resource's permanent, verifiable ID. Publish once, share the
CRID anywhere, and anyone authorized can resolve it into a short-lived
access link when they need one.

Authentication: set QURL_API_KEY (recommended for scripts and CI), or use
` + "`qurl login`" + ` to store a key on this machine.`,
		Example: "  qurl publish https://api.example.com/reports\n" +
			"  qurl resolve " + exampleCRID + "\n" +
			"  qurl list",
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
		deleteCmd(opts),
		connectorCmd(opts),
		loginCmd(opts),
		logoutCmd(opts),
		whoamiCmd(opts),
		versionCmd(version),
		completionCmd(),
		docsCmd(),
	)

	return cmd, opts
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
	// Free-form profile settings the connector command resolves flag-first at
	// its own run time (config.Resolve needs the flag value, which only the
	// command has).
	o.profileConnectorID = cfg.ConnectorID
	o.profileConnectorSlug = cfg.ConnectorSlug //nolint:staticcheck // deliberate compatibility read of the deprecated v1.1.0 key; dies with it at the next major
	o.resolved = true
	return nil
}

// printer builds the per-invocation Printer from the resolved settings.
func (o *globalOpts) printer() *output.Printer {
	return output.New(o.streams, o.resolvedFormat, o.quiet, o.outColor, o.ascii, o.now)
}

// insecureEndpointWarning returns a warning when the endpoint would carry
// the bearer credential over cleartext http to a non-loopback host. Loopback
// is exempt: local mocks and harnesses are legitimately plain http. The
// transport already refuses redirects so the credential cannot follow a
// Location elsewhere; this closes the sibling misconfiguration.
func insecureEndpointWarning(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" {
		return ""
	}
	host := u.Hostname()
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return ""
	}
	return fmt.Sprintf(msgInsecureEndpoint, endpoint)
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
		case "version", "completion", "docs", "help":
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

// credentialStore builds this invocation's storage chain, wired so any read
// served from the file fallback warns (once) that the OS keyring is
// unavailable and the key sits in a mode-0600 file.
func (o *globalOpts) credentialStore() *auth.Chain {
	return o.newCredentialStore(o.configDir, func() {
		o.printer().Warnf("%s", msgKeyringUnavailable)
	})
}

// newClient resolves the credential and builds the one API client. There is
// deliberately no --api-key flag: argv leaks into shell history and process
// lists, so the key comes from QURL_API_KEY (hermetic — the credential store
// is bypassed entirely) or from the store `qurl login` manages (the OS
// keyring, falling back to the 0600 credential file where no keyring is
// available).
func (o *globalOpts) newClient() (qurlapi.Client, error) {
	key, _, err := auth.Resolve(o.lookupEnv, o.credentialStore())
	if err != nil {
		return nil, err
	}
	if err := auth.ValidateKeyShape(key); err != nil {
		return nil, err
	}
	return o.apiClient(key)
}

// apiClient builds the API client around one explicit key. login uses it
// directly (the key it validates is the one just typed, never a stored one);
// everything else goes through newClient.
func (o *globalOpts) apiClient(key string) (qurlapi.Client, error) {
	if warning := insecureEndpointWarning(o.resolvedEndpoint); warning != "" {
		o.printer().Warnf("%s", warning)
	}
	return qurlapi.New(&qurlapi.Config{
		BaseURL:      o.resolvedEndpoint,
		APIKey:       key,
		Version:      o.version,
		Verbose:      o.verboseLogger(),
		Sleep:        o.sleep,
		NewRequestID: o.newRequestID,
	})
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
