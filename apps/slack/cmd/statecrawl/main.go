// Package main implements statecrawl, an operational reconciler for the qURL
// Slack bot's channel_policies DynamoDB table. It crawls every (team, channel) policy row
// and cross-references each referenced resource against the owning workspace's
// live qURL resources, surfacing the state that PRs #654 and #669 leave us in:
//
//   - #654 (merged) taught the bot to cascade PurgeResourceFromChannel on every
//     revoke, so a destroyed resource no longer orphans a channel `$alias`. It
//     does NOT backfill orphans that already exist (pre-fix revokes, or
//     out-of-band deletes via API/SDK/MCP/CLI/token expiry the bot never
//     observed). This tool finds those orphans — an `$alias` (or an
//     allowed_resource_ids member) that still points at a revoked or deleted
//     resource — and, with -apply, runs the SAME slackdata.Store.PurgeResourceFromChannel
//     verb the bot now runs, so the manual backfill matches the live cascade.
//
//   - #669 (open) taught set/unset-display-name to resolve a channel `$alias`
//     whose name differs from the connector slug. This tool flags those live
//     name≠slug bindings so an operator can see which connectors became
//     display-name-targetable by alias. These are informational only — they are
//     a LIVE, healthy state and are never purged.
//
// Dry run is the default and makes zero mutations: it only reads
// (DynamoDB Scan + GetItem, KMS Decrypt for the per-workspace key, and
// GET /v1/resources). Pass -apply to perform the orphan purge. The crawl is
// run once per Slack-bot deployment (sandbox, then prod) by pointing the same
// env vars the bot uses at that deployment; -env stamps the chosen label into
// the report and the apply banner so output is never ambiguous about which
// environment was touched.
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
// SDK chain), so run it with a role that can read both DynamoDB tables, KMS
// Decrypt on the CMK, and — for -apply — UpdateItem on channel_policies.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

// userAgent identifies this tool to qurl-service so a server-side trace can
// attribute a liveness read to the reconciler rather than the live bot.
const userAgent = "qurl-slack-statecrawl/1.0"

// config is the fully resolved run configuration — env vars merged with flag
// overrides and validated for completeness.
type config struct {
	envLabel               string
	channelPoliciesTable   string
	workspaceMappingsTable string
	workspaceStateTable    string
	kmsKeyARN              string
	qurlEndpoint           string
	onlyTeam               string
	pageLimit              int
	apply                  bool
}

func main() {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "statecrawl: "+err.Error())
		os.Exit(2)
	}
	if err := run(context.Background(), cfg); err != nil {
		fmt.Fprintln(os.Stderr, "statecrawl: "+err.Error())
		os.Exit(1)
	}
}

// parseFlags merges flags over env-var defaults and validates that every
// required table/endpoint is set. Flags win over env so an operator can target
// one deployment without re-exporting the whole environment.
func parseFlags(args []string) (config, error) {
	fs := flag.NewFlagSet("statecrawl", flag.ContinueOnError)
	var cfg config
	fs.StringVar(&cfg.envLabel, "env", os.Getenv("STATECRAWL_ENV"), "deployment label for the report/apply banner (e.g. sandbox, prod) — does NOT pick tables")
	fs.StringVar(&cfg.channelPoliciesTable, "channel-policies-table", os.Getenv(slackdata.EnvChannelPoliciesTable), "channel_policies DynamoDB table name")
	fs.StringVar(&cfg.workspaceMappingsTable, "workspace-mappings-table", os.Getenv(slackdata.EnvWorkspaceMappingsTable), "workspace_mappings DynamoDB table name")
	fs.StringVar(&cfg.workspaceStateTable, "workspace-state-table", os.Getenv(auth.EnvWorkspaceStateTable), "workspace_state DynamoDB table name (per-workspace API keys)")
	fs.StringVar(&cfg.kmsKeyARN, "kms-key-arn", os.Getenv(auth.EnvWorkspaceStateKMSKeyARN), "CMK ARN that envelope-encrypts the qurl_api_key column")
	fs.StringVar(&cfg.qurlEndpoint, "qurl-endpoint", os.Getenv("QURL_ENDPOINT"), "qurl-service base URL for the liveness check")
	fs.StringVar(&cfg.onlyTeam, "team", "", "restrict the crawl to a single Slack team_id (optional)")
	fs.IntVar(&cfg.pageLimit, "page-limit", 100, "GET /v1/resources page size while paginating the owner's resources")
	fs.BoolVar(&cfg.apply, "apply", false, "perform the orphan purge (default is a read-only dry run)")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}

	var missing []string
	for _, req := range []struct{ name, val string }{
		{slackdata.EnvChannelPoliciesTable, cfg.channelPoliciesTable},
		{slackdata.EnvWorkspaceMappingsTable, cfg.workspaceMappingsTable},
		{auth.EnvWorkspaceStateTable, cfg.workspaceStateTable},
		{auth.EnvWorkspaceStateKMSKeyARN, cfg.kmsKeyARN},
		{"QURL_ENDPOINT", cfg.qurlEndpoint},
	} {
		if strings.TrimSpace(req.val) == "" {
			missing = append(missing, req.name)
		}
	}
	if len(missing) > 0 {
		return config{}, errors.New("missing required config (set env var or flag): " + strings.Join(missing, ", "))
	}
	if cfg.envLabel == "" {
		cfg.envLabel = "(unlabeled)"
	}
	return cfg, nil
}

// run wires the AWS clients and drives the crawl: scan policies, group by team,
// reconcile each team against its live resources, print the report, and (only
// under -apply) purge the confirmed orphans.
func run(ctx context.Context, cfg config) error {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}
	ddbClient := dynamodb.NewFromConfig(awsCfg)

	store, err := slackdata.NewStore(ctx,
		slackdata.WithDynamoDBClient(ddbClient),
		slackdata.WithTableNames(cfg.workspaceMappingsTable, cfg.channelPoliciesTable),
	)
	if err != nil {
		return fmt.Errorf("construct slackdata store: %w", err)
	}

	keys, err := auth.NewDDBProvider(ctx,
		auth.WithTableName(cfg.workspaceStateTable),
		auth.WithKMSKeyARN(cfg.kmsKeyARN),
	)
	if err != nil {
		return fmt.Errorf("construct API-key provider: %w", err)
	}

	rows, err := scanPolicyRows(ctx, ddbClient, cfg.channelPoliciesTable, cfg.onlyTeam)
	if err != nil {
		return fmt.Errorf("scan channel_policies: %w", err)
	}

	rep := newReport(cfg)
	for _, teamID := range teamIDs(rows) {
		live := resolveLiveness(ctx, keys, cfg, teamID)
		for _, row := range rowsForTeam(rows, teamID) {
			classifyRow(row, live, rep)
		}
	}

	rep.printSummary()
	if !cfg.apply {
		rep.printDryRunFooter()
		return nil
	}
	return rep.applyPurge(ctx, store)
}

// newClient builds a qURL API client for the resolved per-workspace key. Retry
// is left at default (0) — a reconciler can re-run, and we'd rather surface a
// transient error against a team than silently retry and slow a large crawl.
func newClient(endpoint, apiKey string) *client.Client {
	return client.New(endpoint, apiKey, client.WithUserAgent(userAgent))
}

// teamIDs returns the de-duplicated, sorted set of team_ids across the scanned
// rows so the report is deterministic regardless of DynamoDB scan order.
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

// rowsForTeam returns the policy rows belonging to teamID, sorted by channel so
// the per-team report section is stable.
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
// bad -page-limit can't send limit=0 (server default) when an explicit cap was
// intended.
func pageLimitOrDefault(n int) int {
	if n <= 0 {
		return 100
	}
	return n
}

// itoa is a tiny strconv wrapper so call sites read naturally in report copy.
func itoa(n int) string { return strconv.Itoa(n) }
