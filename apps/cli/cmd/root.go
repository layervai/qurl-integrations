package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/spf13/cobra"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/auth"
	"github.com/layervai/qurl-integrations/apps/cli/internal/config"
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

	// Resolved in PersistentPreRunE.
	resolved         bool
	resolvedEndpoint string
	resolvedFormat   output.Format
	outColor         bool
	errColorOn       bool
	ascii            bool
}

// rootOption is a test hook for injecting process context.
type rootOption func(*globalOpts)

// Main wires the real process context and runs the CLI. It returns the exit
// code; main() is the only caller of os.Exit.
func Main(version string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	root, opts := newRoot(version, output.Detect())
	return run(ctx, root, opts)
}

// run executes the tree, renders any error to stderr, and maps it to the one
// exit code.
func run(ctx context.Context, root *cobra.Command, opts *globalOpts) int {
	err := root.ExecuteContext(ctx)
	if err != nil {
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
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
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
	o.resolved = true
	return nil
}

// printer builds the per-invocation Printer from the resolved settings.
func (o *globalOpts) printer() *output.Printer {
	return output.New(o.streams, o.resolvedFormat, o.quiet, o.outColor, o.ascii, o.now)
}

// errColor answers whether stderr rendering gets color, falling back to a
// pre-resolution default when flag parsing itself failed.
func (o *globalOpts) errColor() bool {
	if o.resolved {
		return o.errColorOn
	}
	return output.ResolveColor(output.ColorAuto, o.lookupEnv, o.streams.ErrIsTTY)
}

// newClient resolves the credential and builds the one API client. There is
// deliberately no --api-key flag: argv leaks into shell history and process
// lists, so the key comes from QURL_API_KEY (hermetic — the credential store
// is bypassed entirely) or from the store `qurl login` manages.
func (o *globalOpts) newClient() (qurlapi.Client, error) {
	key, _, err := auth.Resolve(o.lookupEnv, auth.NewFileStore(o.configDir))
	if err != nil {
		return nil, err
	}
	if err := auth.ValidateKeyShape(key); err != nil {
		return nil, err
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
