package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"

	goliblog "github.com/fatedier/golib/log"
	"github.com/spf13/cobra"

	frplog "github.com/fatedier/frp/pkg/util/log"

	"github.com/layervai/qurl-integrations/apps/cli/internal/config"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/frpgen"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/knock"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/replica"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/supervisor"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// connectorKnocker is the per-cycle platform admission client the run command
// hands the supervisor, plus the Close that releases the device runtime it
// owns. knock.Native satisfies it; tests inject a loopback fake.
type connectorKnocker interface {
	knock.CycleKnocker
	Close()
}

// The customer term for a Connector's identity is its ID — the route name
// the app serves under — matching the standalone qurl-connector's contract
// (QURL_CONNECTOR_ID, YAML `id:`). "Slug" is internal/platform-wire
// vocabulary only (qurl-go's GetConnectorResourceBySlug, the `slug` wire
// param, frpgen.Route.Slug); it must not appear on customer surfaces (the
// jargon gate enforces this).
const (
	// envConnectorID supplies the Connector ID when --id is not passed. It
	// is deliberately the SAME variable the standalone qurl-connector reads,
	// so one setting covers a machine moving between the two tools.
	envConnectorID = "QURL_CONNECTOR_ID"

	// envConnectorSlugDeprecated is v1.1.0's spelling, still honored at
	// lower precedence than envConnectorID.
	//
	// Deprecated: remove at the next major, together with the hidden --slug
	// alias and the connector_slug profile key.
	envConnectorSlugDeprecated = "QURL_CONNECTOR_SLUG"
)

// connectorCmd is the `qurl connector` group. Today it carries run; the group
// exists so future Connector subcommands (status, list, ...) land beside it
// instead of as new top-level verbs.
func connectorCmd(opts *globalOpts) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "connector",
		Short: "Serve local apps through the qURL platform",
		Long: `Connectors publish apps running on your machine through the qURL platform.

A Connector serves one local app under its ID — the route name your app
serves under. Callers never reach your machine directly: the platform
verifies each caller and grants access before any request is forwarded
to your app.`,
		Example:       "  qurl connector run --id billing --target :8080",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return exitcode.UsageError(fmt.Errorf("unknown command %q — run `qurl connector --help` for the command list", args[0]))
		},
	}
	cmd.AddCommand(connectorRunCmd(opts))
	return cmd
}

// connectorRunFlags carries run's flag values into the wiring.
type connectorRunFlags struct {
	id string
	// slugAlias holds the hidden deprecated --slug alias of --id, kept so
	// v1.1.0 command lines keep working.
	//
	// Deprecated: remove at the next major.
	slugAlias   string
	target      string
	stateDir    string
	refreshMode string
}

func connectorRunCmd(opts *globalOpts) *cobra.Command {
	var flags connectorRunFlags

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Serve a local app through the qURL platform",
		Long: `Serve a local app through the qURL platform.

Your app keeps listening on localhost; this command connects outward and
serves it under your Connector's ID — the route name your app serves
under. The platform verifies each caller and grants access per caller —
your machine never opens a listening port to the internet.

The first start enrolls this machine and needs a one-time enrollment
token: set QURL_CONNECTOR_TOKEN, or point QURL_CONNECTOR_TOKEN_FILE at a
file holding it. The token is used once and never stored; later starts
reuse the identity saved in the state directory. There is deliberately no
token flag — command-line arguments leak into shell history and process
lists.

If the platform stops answering for long enough, the Connector exits and
the next start may need an assignment refresh; --refresh-mode controls
that self-healing (manual asks you to approve the refresh, auto approves
it once, disabled never refreshes).

Stop with Ctrl-C or SIGTERM; teardown gets a short grace period.`,
		Example: "  qurl connector run --id billing --target :8080\n" +
			"  QURL_CONNECTOR_TOKEN_FILE=/run/secrets/qurl-token qurl connector run --id billing --target 127.0.0.1:3000",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runConnector(cmd.Context(), opts, &flags)
		},
	}

	f := cmd.Flags()
	f.StringVar(&flags.id, "id", "", "which Connector to run: its ID in qURL — the route name your app serves under (or set connector_id in your profile)")
	// Hidden deprecated alias of --id: v1.1.0 shipped --slug, so it keeps
	// working, invisibly. connectorIDFromFlags refuses the two disagreeing.
	// Deprecated: remove at the next major. The usage string is neutral
	// (no banned vocabulary) so the jargon gate stays clean even if it ever
	// starts walking hidden flags.
	f.StringVar(&flags.slugAlias, "slug", "", "deprecated alias of --id")
	if err := f.MarkHidden("slug"); err != nil {
		// Unreachable: the flag is registered two lines up. panic keeps the
		// deprecation wiring honest instead of silently unhiding it.
		panic(err)
	}
	f.StringVar(&flags.target, "target", "", "local app to serve, as host:port (\":8080\" means 127.0.0.1:8080)")
	f.StringVar(&flags.stateDir, "state-dir", "", "directory holding this Connector's identity (default: your user state directory)")
	f.StringVar(&flags.refreshMode, "refresh-mode", "", "whether the Connector may refresh its platform assignment after sustained failures: manual, auto, or disabled (default manual)")

	return cmd
}

// connectorIDFromFlags merges --id with its deprecated --slug alias. Both
// given with different values is a contradiction the command refuses rather
// than guesses; both given with the same value is harmlessly redundant.
func connectorIDFromFlags(flags *connectorRunFlags) (string, error) {
	id := strings.TrimSpace(flags.id)
	alias := strings.TrimSpace(flags.slugAlias)
	if id != "" && alias != "" && id != alias {
		return "", exitcode.UsageError(fmt.Errorf(msgConnectorIDConflict, id, alias))
	}
	if id != "" {
		return id, nil
	}
	return alias, nil
}

// resolveConnectorID applies the flag > env > profile ladder for the
// Connector ID. Within the env and profile tiers the deprecated v1.1.0
// names are honored below their ID twins (QURL_CONNECTOR_SLUG under
// QURL_CONNECTOR_ID, connector_slug under connector_id), so an existing
// setup keeps working while any ID-termed setting wins its tier.
// Deprecated fallbacks go away at the next major.
func resolveConnectorID(opts *globalOpts, flags *connectorRunFlags) (string, error) {
	flagID, err := connectorIDFromFlags(flags)
	if err != nil {
		return "", err
	}
	profileID := opts.profileConnectorID
	if profileID == "" {
		profileID = opts.profileConnectorSlug
	}
	// Compose the tiers through the one canonical precedence helper: the
	// inner Resolve is the deprecated-env-then-profile tail, the outer one
	// puts flag and QURL_CONNECTOR_ID above it.
	tail := config.Resolve("", envConnectorSlugDeprecated, opts.lookupEnv, profileID, "")
	return strings.TrimSpace(config.Resolve(flagID, envConnectorID, opts.lookupEnv, tail, "")), nil
}

// connectorRunInputs is one run invocation's validated command line.
type connectorRunInputs struct {
	id          string
	localIP     string
	localPort   int
	refreshMode string
}

// validateConnectorRunInputs resolves and validates everything runConnector
// needs from the command line before any side effect (no state directory is
// created, no network is touched): the Connector ID ladder, the --target
// grammar, and the --refresh-mode vocabulary. The customer's Connector ID
// is the platform's "slug" on the wire — qurl-go and the internal packages
// keep that vocabulary; the customer surface says ID.
func validateConnectorRunInputs(opts *globalOpts, flags *connectorRunFlags) (*connectorRunInputs, error) {
	id, err := resolveConnectorID(opts, flags)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, exitcode.UsageError(errors.New(msgConnectorIDRequired))
	}
	localIP, localPort, err := parseConnectorTarget(flags.target)
	if err != nil {
		return nil, err
	}
	refreshMode := strings.ToLower(strings.TrimSpace(flags.refreshMode))
	switch refreshMode {
	case "", agent.RefreshModeManual, agent.RefreshModeAuto, agent.RefreshModeDisabled:
		// Empty falls through to the agent package's env-then-manual chain;
		// a flag typo is a usage error here, while a bad ENV spelling maps to
		// the configuration exit through agent.ErrRefreshModeInvalid.
	default:
		return nil, exitcode.UsageError(fmt.Errorf(msgConnectorRefreshModeInvalid, flags.refreshMode))
	}
	return &connectorRunInputs{id: id, localIP: localIP, localPort: localPort, refreshMode: refreshMode}, nil
}

// runConnector wires the Connector serve loop: agent open/enroll ladder →
// resource ensure by ID → FRP config generation → supervised knock-then-login
// serving, with INT/TERM handing the supervisor a bounded graceful stop.
func runConnector(ctx context.Context, opts *globalOpts, flags *connectorRunFlags) error {
	// Local validation first: nothing below runs until the command line
	// itself is coherent.
	in, err := validateConnectorRunInputs(opts, flags)
	if err != nil {
		return err
	}

	logger := connectorLogger(opts)
	opts.redirectFRPLogs()
	printer := opts.printer()

	rt, err := opts.openConnectorRuntime(ctx, &agent.Config{
		APIBaseURL:  opts.resolvedEndpoint,
		StateDir:    flags.stateDir,
		RefreshMode: in.refreshMode,
		Version:     opts.version,
		Logger:      logger,
	})
	if err != nil {
		return err
	}
	defer func() { _ = rt.Close() }()

	resource, err := agent.ResolveResource(ctx, rt.Client, in.id)
	if err != nil {
		return err
	}
	knockResourceID, err := agent.KnockResourceID(resource)
	if err != nil {
		return err
	}

	// Boot-time replica salt. The Warnings()/meta.Warning surfacing below is
	// load-bearing, not decorative: the resolver deliberately degrades to a
	// random or sentinel salt instead of failing boot, and this stderr
	// surface is the ONLY place the operator learns their replicas will
	// re-salt across restarts (or that an explicit override was dropped).
	// Removing it would turn those degradations silent.
	resolver := &replica.Resolver{}
	salt, meta, err := resolver.Resolve(ctx)
	if err != nil {
		return err
	}
	for _, warning := range resolver.Warnings() {
		printer.Warnf("%s", warning)
	}
	if meta.Warning != "" {
		printer.Warnf("%s", meta.Warning)
	}
	logger.InfoContext(ctx, "connector: replica identity resolved",
		"source", string(meta.Source), "discriminator", salt)

	clientCfg, err := frpgen.Generate(&frpgen.Route{
		Slug:               resource.Slug,
		ResourceID:         resource.ResourceID,
		ConnectorRoutingID: resource.ConnectorRoutingID,
		LocalIP:            in.localIP,
		LocalPort:          in.localPort,
	}, &frpgen.Options{
		ReplicaDiscriminator: salt,
		ClientVersion:        opts.version,
	})
	if err != nil {
		return err
	}
	common, proxies := clientCfg.FRPClientConfig()
	if err := common.Complete(); err != nil {
		return fmt.Errorf("complete tunnel client configuration: %w", err)
	}

	knocker, err := opts.newConnectorKnocker(rt, knockResourceID)
	if err != nil {
		return err
	}
	defer knocker.Close()

	factory, err := supervisor.NewFRPRunnerFactory(supervisor.FRPFactoryConfig{
		Knocker:    knocker,
		ResourceID: knockResourceID,
		Proxies:    proxies,
		Logger:     logger,
	})
	if err != nil {
		return err
	}
	supCfg := &supervisor.Config{
		Common:          common,
		Knocker:         knocker,
		KnockResourceID: knockResourceID,
		RunnerFactory:   factory,
		Marker:          rt.Store,
		Logger:          logger,
	}
	if opts.tuneConnectorSupervisor != nil {
		opts.tuneConnectorSupervisor(supCfg)
	}
	sup, err := supervisor.New(supCfg)
	if err != nil {
		return err
	}

	// The resource resolved above already carries its own CRID — on the
	// read-by-ID leg and the ensure leg alike — so the serve note can hand it
	// to the customer with no extra lookup and no extra request.
	serveTarget := net.JoinHostPort(in.localIP, strconv.Itoa(in.localPort))
	printer.ConnectorServing(resource.Slug, serveTarget, resource.CRID)
	// The same identity as a structured event, because the note above is prose
	// written for a person: an unattended runner would have to scrape it, and
	// the wording is free to change. A CRID is a public identifier — it names
	// a resource and grants nothing — so it is safe in a log an operator
	// collects, unlike a resolved link. It is omitted rather than logged empty
	// when the platform returned none, so a consumer can treat presence as
	// meaning.
	servingAttrs := []any{
		"event", "connector_starting",
		"connector_id", resource.Slug,
		"target", serveTarget,
	}
	if resource.CRID != "" {
		servingAttrs = append(servingAttrs, "crid", resource.CRID)
	}
	logger.InfoContext(ctx, "connector: starting to serve local app", servingAttrs...)
	if err := sup.Start(ctx); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		// INT/TERM: graceful stop bounded by the supervisor's own teardown
		// deadline, then the Interrupted exit (130) with a quiet note — the
		// signal path deliberately renders no error anatomy.
		stopCtx, cancel := context.WithTimeout(context.Background(), supervisor.StopWait)
		defer cancel()
		if err := sup.Stop(stopCtx); err != nil {
			return err
		}
		printer.Notef(msgConnectorStopped)
		return ctx.Err()
	case <-sup.Done():
		// Autonomous exit: the serve loop decided to stop (budget exhaustion
		// or a fatal wiring error). The error carries its own exit code
		// through the exitcode contract.
		return sup.Err()
	}
}

// parseConnectorTarget parses --target as host:port, with the ":port"
// shorthand meaning 127.0.0.1. Everything wrong here is a usage error: the
// value can never become valid without editing the command line.
func parseConnectorTarget(target string) (host string, port int, err error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", 0, exitcode.UsageError(errors.New(msgConnectorTargetRequired))
	}
	hostPort := target
	if strings.HasPrefix(hostPort, ":") {
		hostPort = "127.0.0.1" + hostPort
	}
	host, portRaw, splitErr := net.SplitHostPort(hostPort)
	if splitErr != nil || strings.TrimSpace(host) == "" {
		return "", 0, exitcode.UsageError(fmt.Errorf(msgConnectorTargetInvalid, target))
	}
	port, portErr := strconv.Atoi(portRaw)
	if portErr != nil || port < 1 || port > 65535 {
		return "", 0, exitcode.UsageError(fmt.Errorf(msgConnectorTargetInvalid, target))
	}
	return host, port, nil
}

// newNativeConnectorKnocker is the production knocker constructor: it hands
// the runtime's binding to knock.Native, which then owns it (Close destroys
// it), so the runtime must forget the binding rather than Destroy it again.
func newNativeConnectorKnocker(rt *agent.Runtime, knockResourceID string) (connectorKnocker, error) {
	knocker, err := knock.NewNative(rt.Binding, knockResourceID)
	if err != nil {
		return nil, err
	}
	rt.Binding = nil
	return knocker, nil
}

// connectorLogger is the serve loop's structured operator log on stderr.
// Operator telemetry deliberately keeps its technical vocabulary; the
// customer-language surface is the final error rendering and the fixed
// messages, which the jargon gate walks.
func connectorLogger(opts *globalOpts) *slog.Logger {
	level := slog.LevelInfo
	if opts.verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(opts.streams.Err, &slog.HandlerOptions{Level: level}))
}

// redirectFRPLogsToStderr points the linked FRP client's process-global
// logger at this invocation's stderr, capped at warnings (info with
// --verbose). Without this the library writes to process stdout, which
// belongs to data. Swapping a global is safe in production: the CLI runs one
// command per process and the swap happens before any FRP goroutine exists.
// The cmd test binary is the one place that is NOT true (its in-process
// tunnel server logs through the same global from its own goroutines), so
// the call site goes through the opts.redirectFRPLogs seam and the test
// harness pins the logger once in TestMain instead.
func redirectFRPLogsToStderr(opts *globalOpts) {
	level := goliblog.WarnLevel
	if opts.verbose {
		level = goliblog.InfoLevel
	}
	frplog.Logger = goliblog.New(
		goliblog.WithCaller(false),
		goliblog.WithLevel(level),
		goliblog.WithOutput(goliblog.NewConsoleWriter(goliblog.ConsoleConfig{}, opts.streams.Err)),
	)
}
