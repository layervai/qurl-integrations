// Package main implements statecrawl, an operational reconciler for the qURL
// Slack bot's channel_policies DynamoDB table. It crawls every (team, channel)
// policy row and cross-references each referenced resource against the owning
// workspace's live qURL resources, surfacing — and optionally repairing — the
// state that PRs #654 and #669 leave us in.
//
//   - #654 (merged) taught the bot to cascade PurgeResourceFromChannel on every
//     revoke, so a destroyed resource no longer orphans a channel `$alias`. It
//     does NOT backfill orphans that already exist (pre-fix revokes, or
//     out-of-band deletes via API/SDK/MCP/CLI/token expiry the bot never
//     observed). statecrawl finds those orphans — an `$alias` (or an
//     allowed_resource_ids member) still pointing at a revoked/deleted resource
//     — and, when run with mutations enabled, clears them via the SAME
//     slackdata.Store.PurgeResourceFromChannel verb the bot now runs, so the
//     manual backfill is behaviorally identical to the live cascade. This is the
//     lever for unblocking a customer stuck in the contradictory "alias already
//     bound, yet /qurl list shows nothing" state ASAP.
//
//   - #669 (open) taught set/unset-display-name to resolve a channel `$alias`
//     whose name differs from the connector slug. statecrawl flags those live
//     name≠slug bindings so an operator can see which connectors became
//     display-name-targetable by alias. These are a LIVE, healthy state and are
//     never purged.
//
// Safety model (mirrors qurl-service's cmd/qurl-scanner + cmd/qurl-bucket-backfill):
//
//   - -dry-run defaults TRUE. A dry run makes zero mutations: it only reads
//     (DynamoDB Scan + GetItem, KMS Decrypt for the per-workspace key, and
//     GET /v1/resources) and reports what it WOULD purge.
//   - Mutating requires -dry-run=false. Against a prod-looking deployment
//     (the -env label or any table name says "prod") it ALSO requires the
//     explicit -allow-prod-purge opt-in — the "rail" that makes an irreversible
//     prod write a deliberate act, not a flag-default accident.
//   - Only confirmed orphans (resource absent or revoked, verified against a
//     fully paginated resource list) are ever purged. A workspace whose API key
//     can't be resolved, or whose list fails, is reported indeterminate and
//     never purged.
//
// Output is structured slog (JSON by default, -log-format=text for triage),
// so every finding and every purge is an auditable, greppable record, ending
// in one summary line carrying the counter snapshot.
//
// Required wiring (env var, or the matching -flag override):
//
//	QURL_CHANNEL_POLICIES_TABLE   channel_policies table to crawl
//	QURL_WORKSPACE_MAPPINGS_TABLE workspace_mappings table (constructs the Store)
//	WORKSPACE_STATE_TABLE         per-workspace qURL API key table
//	WORKSPACE_STATE_KMS_KEY_ARN   CMK that envelope-encrypts the API key column
//	QURL_ENDPOINT                 qurl-service base URL for the liveness check
//
// AWS credentials/region come from the ambient environment (the standard AWS
// SDK chain): a role that can read both DynamoDB tables, KMS Decrypt on the CMK,
// and — for a mutating run — UpdateItem on channel_policies.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

// userAgent identifies this tool to qurl-service so a server-side trace can
// attribute a liveness read to the reconciler rather than the live bot.
const userAgent = "qurl-slack-statecrawl/1.0"

// flags is the raw, validated run configuration — env vars merged with flag
// overrides, with the rails in parseFlags already enforced.
type flags struct {
	envLabel               string
	channelPoliciesTable   string
	workspaceMappingsTable string
	workspaceStateTable    string
	kmsKeyARN              string
	qurlEndpoint           string
	onlyTeam               string
	logFormat              string
	pageLimit              int
	dryRun                 bool
	allowProdPurge         bool
}

func main() {
	f, err := parseFlags(flag.NewFlagSet("statecrawl", flag.ContinueOnError), os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "statecrawl: "+err.Error())
		os.Exit(2)
	}
	logger := newLogger(f.logFormat)
	if err := run(context.Background(), f, logger); err != nil {
		logger.Error("statecrawl failed", "error", err)
		os.Exit(1)
	}
}

// newLogger builds the structured logger. JSON is the default (matches
// qurl-service operational CLIs and gives a machine-auditable record); text is
// available for live triage.
func newLogger(format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if format == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

// parseFlags merges flags over env-var defaults and enforces the safety rails.
// Pure and testable — the FlagSet and args are injected, and every reject path
// names the opt-in flag an operator must add, mirroring qurl-service's
// parseFlags contract.
func parseFlags(fs *flag.FlagSet, args []string) (*flags, error) {
	f := &flags{}
	fs.StringVar(&f.envLabel, "env", os.Getenv("STATECRAWL_ENV"), "deployment label for the summary/rails (e.g. sandbox, prod) — does NOT pick tables")
	fs.StringVar(&f.channelPoliciesTable, "channel-policies-table", os.Getenv(slackdata.EnvChannelPoliciesTable), "channel_policies DynamoDB table name")
	fs.StringVar(&f.workspaceMappingsTable, "workspace-mappings-table", os.Getenv(slackdata.EnvWorkspaceMappingsTable), "workspace_mappings DynamoDB table name")
	fs.StringVar(&f.workspaceStateTable, "workspace-state-table", os.Getenv(auth.EnvWorkspaceStateTable), "workspace_state DynamoDB table name (per-workspace API keys)")
	fs.StringVar(&f.kmsKeyARN, "kms-key-arn", os.Getenv(auth.EnvWorkspaceStateKMSKeyARN), "CMK ARN that envelope-encrypts the qurl_api_key column")
	fs.StringVar(&f.qurlEndpoint, "qurl-endpoint", os.Getenv("QURL_ENDPOINT"), "qurl-service base URL for the liveness check")
	fs.StringVar(&f.onlyTeam, "team", "", "restrict the crawl to a single Slack team_id (optional)")
	fs.StringVar(&f.logFormat, "log-format", "json", "log format: json or text")
	fs.IntVar(&f.pageLimit, "page-limit", 100, "GET /v1/resources page size while paginating the owner's resources")
	fs.BoolVar(&f.dryRun, "dry-run", true, "read-only crawl; report what WOULD be purged but mutate nothing (default true)")
	fs.BoolVar(&f.allowProdPurge, "allow-prod-purge", false, "required opt-in to purge against a prod-looking deployment (rail)")
	if err := fs.Parse(args); err != nil {
		return nil, err //nolint:wrapcheck // flag already prints a usage error; the caller exits.
	}
	if err := validateConfig(f); err != nil {
		return nil, err
	}
	if err := validateRails(f); err != nil {
		return nil, err
	}
	if f.envLabel == "" {
		f.envLabel = "(unlabeled)"
	}
	return f, nil
}

// validateConfig fails fast when a required table/endpoint is missing, so a
// half-wired run never reaches AWS.
func validateConfig(f *flags) error {
	var missing []string
	for _, req := range []struct{ name, val string }{
		{slackdata.EnvChannelPoliciesTable, f.channelPoliciesTable},
		{slackdata.EnvWorkspaceMappingsTable, f.workspaceMappingsTable},
		{auth.EnvWorkspaceStateTable, f.workspaceStateTable},
		{auth.EnvWorkspaceStateKMSKeyARN, f.kmsKeyARN},
		{"QURL_ENDPOINT", f.qurlEndpoint},
	} {
		if strings.TrimSpace(req.val) == "" {
			missing = append(missing, req.name)
		}
	}
	if len(missing) > 0 {
		return errors.New("missing required config (set env var or flag): " + strings.Join(missing, ", "))
	}
	if f.logFormat != "json" && f.logFormat != "text" {
		return errors.New("-log-format must be json or text, got " + f.logFormat)
	}
	return nil
}

// validateRails enforces the prod-purge opt-in. A mutating run against a
// prod-looking deployment without -allow-prod-purge is rejected with an error
// that names the flag — the irreversible case the rail exists to prevent.
func validateRails(f *flags) error {
	if f.dryRun {
		return nil
	}
	if looksProd(f) && !f.allowProdPurge {
		return errors.New("refusing to purge a prod-looking deployment (" + f.envLabel +
			") without -allow-prod-purge: re-run with -allow-prod-purge once you've reviewed a -dry-run")
	}
	return nil
}

// looksProd reports whether this run targets production, by either the operator
// label or a "prod" substring in any resolved table name. Defense-in-depth: a
// forgotten -env=prod still trips the rail when the table names say prod.
func looksProd(f *flags) bool {
	switch strings.ToLower(strings.TrimSpace(f.envLabel)) {
	case "prod", "production":
		return true
	}
	for _, t := range []string{f.channelPoliciesTable, f.workspaceMappingsTable, f.workspaceStateTable} {
		if strings.Contains(strings.ToLower(t), "prod") {
			return true
		}
	}
	return false
}

// run wires the AWS clients and drives the crawl: scan policies, group by team,
// reconcile each team against its live resources, emit findings, then either
// report the dry-run plan or apply the purge.
func run(ctx context.Context, f *flags, logger *slog.Logger) error {
	started := time.Now()
	logger.Info("statecrawl starting", "deployment", f.envLabel, "mode", modeString(f),
		"channel_policies_table", f.channelPoliciesTable, "only_team", f.onlyTeam)
	if f.dryRun && f.allowProdPurge {
		logger.Warn("-allow-prod-purge has no effect under -dry-run (no mutations happen in a dry run)")
	}

	store, keys, ddbClient, err := buildClients(ctx, f)
	if err != nil {
		return err
	}

	rows, err := scanPolicyRows(ctx, ddbClient, f.channelPoliciesTable, f.onlyTeam)
	if err != nil {
		return fmt.Errorf("scan channel_policies: %w", err)
	}

	rep := newReport(f)
	for _, teamID := range teamIDs(rows) {
		live := resolveLiveness(ctx, keys, f, teamID)
		if live.resolved {
			rep.stats.TeamsResolved.Add(1)
		} else {
			rep.stats.TeamsIndeterminate.Add(1)
			logger.Warn("team liveness unverifiable; references reported indeterminate, never purged",
				"team_id", teamID, "reason", live.reason)
		}
		for _, row := range rowsForTeam(rows, teamID) {
			rep.stats.ChannelsScanned.Add(1)
			classifyRow(row, live, rep)
		}
	}

	rep.emitFindings(logger)
	if err := rep.settle(ctx, store, logger); err != nil {
		return err
	}
	logger.Info("statecrawl complete", append([]any{
		"deployment", f.envLabel, "mode", modeString(f), "elapsed", time.Since(started).String(),
	}, rep.stats.Snapshot().logAttrs()...)...)
	if n := rep.stats.PurgeErrors.Load(); n > 0 {
		return fmt.Errorf("%d purge(s) failed; re-run to retry (purge is idempotent)", n)
	}
	return nil
}

// buildClients constructs the slackdata Store (for the purge verb), the API-key
// provider (KMS-envelope reads), and the raw DynamoDB client (for the Scan).
func buildClients(ctx context.Context, f *flags) (*slackdata.Store, auth.Provider, *dynamodb.Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load AWS config: %w", err)
	}
	ddbClient := dynamodb.NewFromConfig(awsCfg)

	store, err := slackdata.NewStore(ctx,
		slackdata.WithDynamoDBClient(ddbClient),
		slackdata.WithTableNames(f.workspaceMappingsTable, f.channelPoliciesTable),
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("construct slackdata store: %w", err)
	}

	keys, err := auth.NewDDBProvider(ctx,
		auth.WithTableName(f.workspaceStateTable),
		auth.WithKMSKeyARN(f.kmsKeyARN),
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("construct API-key provider: %w", err)
	}
	return store, keys, ddbClient, nil
}

// modeString renders the run mode for logs.
func modeString(f *flags) string {
	if f.dryRun {
		return "dry-run"
	}
	return "apply"
}

// newClient builds a qURL API client for the resolved per-workspace key.
func newClient(endpoint, apiKey string) *client.Client {
	return client.New(endpoint, apiKey, client.WithUserAgent(userAgent))
}

// teamIDs returns the de-duplicated, sorted set of team_ids across the scanned
// rows so the crawl is deterministic regardless of DynamoDB scan order.
func teamIDs(rows []policyRow) []string {
	seen := make(map[string]struct{}, len(rows))
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if _, ok := seen[r.teamID]; ok {
			continue
		}
		seen[r.teamID] = struct{}{}
		out = append(out, r.teamID)
	}
	sort.Strings(out)
	return out
}

// rowsForTeam returns the policy rows belonging to teamID, sorted by channel.
func rowsForTeam(rows []policyRow, teamID string) []policyRow {
	out := make([]policyRow, 0, len(rows))
	for _, r := range rows {
		if r.teamID == teamID {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].channelID < out[j].channelID })
	return out
}

// pageLimitOrDefault clamps a non-positive page limit to the server max so a
// bad -page-limit can't send limit=0 (server default) when a cap was intended.
func pageLimitOrDefault(n int) int {
	if n <= 0 {
		return 100
	}
	return n
}
