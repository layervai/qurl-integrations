package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	testTeam      = "T1"
	testChannel   = "C1"
	testDeadID    = "r_dead0001"
	testLiveID    = "r_app00001"
	testLiveSlug  = "app"
	testDeadAlias = "dashboard"
)

// crawlFlags returns a flags value pointed at the qURL test server, in either
// dry-run or apply mode. Tables are sandbox-named so the prod rail stays off.
func crawlFlags(endpoint string, dryRun bool) *flags {
	return &flags{
		envLabel:               "sandbox",
		channelPoliciesTable:   "qurl-bot-slack-sandbox-channel-policies",
		workspaceMappingsTable: "qurl-bot-slack-sandbox-workspace-mappings",
		workspaceStateTable:    "qurl-sandbox-workspace-state",
		kmsKeyARN:              "arn:aws:kms:us-east-1:111122223333:key/abc",
		qurlEndpoint:           endpoint,
		logFormat:              "json",
		pageLimit:              100,
		dryRun:                 dryRun,
	}
}

// orphanScenario wires a fake DDB whose one channel binds a healthy alias
// (app→live tunnel, name==slug) and an orphaned alias (dashboard→dead id), with
// the dead id also lingering in allowed_resource_ids. The qURL server returns
// only the live resource, so the dead id resolves to nothing. The same fake is
// used as both the Scan source and the Store's backing client.
func orphanScenario(t *testing.T, dryRun bool) (*flags, *fakeDDB, fakeProvider, *slackdata.Store) {
	t.Helper()
	row := policyItem(testTeam, testChannel,
		map[string]string{testDeadAlias: testDeadID, "app": testLiveID},
		[]string{testDeadID, testLiveID},
	)
	fake := &fakeDDB{
		scanPages: []*dynamodb.ScanOutput{{Items: []map[string]ddbtypes.AttributeValue{row}}},
		items:     map[string]map[string]ddbtypes.AttributeValue{testChannel: row},
	}
	srv := qurlServer(t, []map[string]any{
		qurlResource(testLiveID, client.ResourceTypeTunnel, testLiveSlug, client.StatusActive),
	})
	provider := fakeProvider{keys: map[string]string{testTeam: "key"}}
	store, err := slackdata.NewStore(context.Background(),
		slackdata.WithDynamoDBClient(fake),
		slackdata.WithTableNames("qurl-bot-slack-sandbox-workspace-mappings", "qurl-bot-slack-sandbox-channel-policies"),
	)
	if err != nil {
		t.Fatalf("build store: %v", err)
	}
	return crawlFlags(srv.URL, dryRun), fake, provider, store
}

// TestCrawl_DryRunMakesNoMutations is the central safety guarantee: a dry run
// finds the orphan and reports it as "would purge" but issues ZERO DynamoDB
// writes — the proof that running this against prod for triage is safe.
func TestCrawl_DryRunMakesNoMutations(t *testing.T) {
	f, fake, provider, store := orphanScenario(t, true)

	snap, err := crawl(context.Background(), f, discardLogger(), store, provider, fake)
	if err != nil {
		t.Fatalf("crawl: %v", err)
	}
	if got := fake.mutationCount(); got != 0 {
		t.Fatalf("dry run issued %d DynamoDB mutations, want 0 — dry run MUST be read-only", got)
	}
	if snap.DryRunWouldPurge != 1 {
		t.Errorf("dry_run_would_purge = %d, want 1 (the dead id, deduped across alias + SS)", snap.DryRunWouldPurge)
	}
	if snap.Purged != 0 {
		t.Errorf("purged = %d, want 0 in a dry run", snap.Purged)
	}
	// The orphan is surfaced on both surfaces; the healthy alias==slug is silent.
	if snap.OrphanAliases != 1 || snap.OrphanAllowedIDs != 1 {
		t.Errorf("orphan counts = (alias %d, allowed %d), want (1, 1)", snap.OrphanAliases, snap.OrphanAllowedIDs)
	}
	if snap.AliasNameMismatch != 0 || snap.LegacyAliases != 0 {
		t.Errorf("unexpected informational findings: %+v", snap)
	}
}

// TestCrawl_ApplyPurgesOrphanViaBotVerb proves the apply path clears the orphan
// through the real slackdata.Store.PurgeResourceFromChannel: exactly one
// UpdateItem that DELETEs the dead id from allowed_resource_ids and REMOVEs the
// orphaned alias key, leaving the healthy binding untouched.
func TestCrawl_ApplyPurgesOrphanViaBotVerb(t *testing.T) {
	f, fake, provider, store := orphanScenario(t, false)

	snap, err := crawl(context.Background(), f, discardLogger(), store, provider, fake)
	if err != nil {
		t.Fatalf("crawl: %v", err)
	}
	if snap.Purged != 1 || snap.PurgeErrors != 0 {
		t.Fatalf("purged = %d, purge_errors = %d, want (1, 0)", snap.Purged, snap.PurgeErrors)
	}
	if snap.AliasesUnbound != 1 {
		t.Errorf("aliases_unbound = %d, want 1 (the $dashboard orphan)", snap.AliasesUnbound)
	}
	if len(fake.updateItems) != 1 {
		t.Fatalf("UpdateItem calls = %d, want exactly 1", len(fake.updateItems))
	}
	expr := aws.ToString(fake.updateItems[0].UpdateExpression)
	if !strings.Contains(expr, "DELETE "+attrAllowedResourceIDs) {
		t.Errorf("UpdateExpression missing SS DELETE: %q", expr)
	}
	if !strings.Contains(expr, "REMOVE") {
		t.Errorf("UpdateExpression missing alias REMOVE: %q", expr)
	}
	// The REMOVE must target the orphaned alias, never the healthy "app".
	for _, name := range fake.updateItems[0].ExpressionAttributeNames {
		if name == "app" {
			t.Errorf("purge removed the healthy alias 'app': %v", fake.updateItems[0].ExpressionAttributeNames)
		}
	}
}

// TestCrawl_IndeterminateNeverPurged is the hard safety fence: even in APPLY
// mode, a team whose API key can't be resolved is reported indeterminate and
// NOTHING is mutated — we never purge a binding we couldn't verify as dead.
func TestCrawl_IndeterminateNeverPurged(t *testing.T) {
	f, fake, _, store := orphanScenario(t, false)
	// Provider with no key for the team → ErrWorkspaceNotConfigured.
	provider := fakeProvider{}

	snap, err := crawl(context.Background(), f, discardLogger(), store, provider, fake)
	if err != nil {
		t.Fatalf("crawl: %v", err)
	}
	if got := fake.mutationCount(); got != 0 {
		t.Fatalf("apply run mutated %d times for an unverifiable team, want 0", got)
	}
	if snap.TeamsIndeterminate != 1 {
		t.Errorf("teams_indeterminate = %d, want 1", snap.TeamsIndeterminate)
	}
	// The scenario row carries 2 alias bindings + 2 allowed_resource_ids = 4
	// references; on an unverifiable team every one is reported indeterminate.
	if snap.Indeterminate != 4 {
		t.Errorf("indeterminate references = %d, want 4 (2 aliases + 2 allowed-ids)", snap.Indeterminate)
	}
	if snap.OrphanAliases != 0 || snap.Purged != 0 {
		t.Errorf("an unverifiable team must yield no orphans/purges: %+v", snap)
	}
}

// TestCrawl_ScopedZeroRowsWarns pins the disambiguation warning: a -team run
// that matches no policy rows must say so loudly, because a typo'd team id and
// a policy-free workspace otherwise produce the same "clean" exit-0 output.
func TestCrawl_ScopedZeroRowsWarns(t *testing.T) {
	fake := &fakeDDB{} // no query pages: the scoped Query matches nothing
	store, err := slackdata.NewStore(context.Background(),
		slackdata.WithDynamoDBClient(fake),
		slackdata.WithTableNames("wm", "cp"),
	)
	if err != nil {
		t.Fatalf("build store: %v", err)
	}
	f := crawlFlags("http://unused.invalid", true)
	f.onlyTeam = "T404"

	var buf bytes.Buffer
	snap, err := crawl(context.Background(), f, slog.New(slog.NewJSONHandler(&buf, nil)), store, fakeProvider{}, fake)
	if err != nil {
		t.Fatalf("crawl: %v", err)
	}
	if snap.ChannelsScanned != 0 {
		t.Errorf("channels_scanned = %d, want 0", snap.ChannelsScanned)
	}
	if !strings.Contains(buf.String(), "scoped crawl matched no channel_policies rows") {
		t.Errorf("zero-row scoped crawl must WARN about a possible -team typo; logs:\n%s", buf.String())
	}
}

// TestCrawl_AllHealthyNoFindings confirms a clean workspace yields zero findings
// and zero mutations regardless of mode.
func TestCrawl_AllHealthyNoFindings(t *testing.T) {
	row := policyItem(testTeam, testChannel, map[string]string{"app": testLiveID}, []string{testLiveID})
	fake := &fakeDDB{
		scanPages: []*dynamodb.ScanOutput{{Items: []map[string]ddbtypes.AttributeValue{row}}},
		items:     map[string]map[string]ddbtypes.AttributeValue{testChannel: row},
	}
	srv := qurlServer(t, []map[string]any{
		qurlResource(testLiveID, client.ResourceTypeTunnel, testLiveSlug, client.StatusActive),
	})
	store, err := slackdata.NewStore(context.Background(),
		slackdata.WithDynamoDBClient(fake),
		slackdata.WithTableNames("wm", "cp"),
	)
	if err != nil {
		t.Fatalf("build store: %v", err)
	}

	snap, err := crawl(context.Background(), crawlFlags(srv.URL, false), discardLogger(), store, fakeProvider{keys: map[string]string{testTeam: "key"}}, fake)
	if err != nil {
		t.Fatalf("crawl: %v", err)
	}
	if fake.mutationCount() != 0 {
		t.Errorf("healthy workspace triggered %d mutations, want 0", fake.mutationCount())
	}
	if snap.OrphanAliases+snap.OrphanAllowedIDs+snap.AliasNameMismatch+snap.LegacyAliases != 0 {
		t.Errorf("healthy workspace produced findings: %+v", snap)
	}
	if snap.ChannelsScanned != 1 || snap.TeamsResolved != 1 {
		t.Errorf("coverage counters = (channels %d, teams_resolved %d), want (1, 1)", snap.ChannelsScanned, snap.TeamsResolved)
	}
}
