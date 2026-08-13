package main

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

// TestSchemaConstants_MatchSlackdataWritePath is the lockstep guard for the
// channel_policies attribute names statecrawl re-declares in scan.go (slackdata
// keeps them unexported). It runs slackdata's REAL write verbs — the same code
// the bot uses to expose a resource and bind an alias — over a capturing fake,
// then asserts statecrawl's local constants appear in the DynamoDB requests
// slackdata actually issues.
//
// Why it matters: if the bot ever renames an attribute (say alias_bindings),
// statecrawl's stale constant would make parsePolicyRow read an empty row,
// classifyRow would find nothing, and the crawl would report EVERY workspace as
// healthy — the worst possible failure for an orphan-finder. Every other test
// builds rows via statecrawl's own constants, so a divergence would pass them
// all; this test is the only thing that fails CI on drift. (Alternative fix if
// this ever gets brittle: ask slackdata to export the four names.)
func TestSchemaConstants_MatchSlackdataWritePath(t *testing.T) {
	fake := &fakeDDB{}
	store, err := slackdata.NewStore(context.Background(),
		slackdata.WithDynamoDBClient(fake),
		slackdata.WithTableNames("wm", "cp"),
	)
	if err != nil {
		t.Fatalf("build store: %v", err)
	}
	ctx := context.Background()
	// ExposeResourceToChannel writes the allowed_resource_ids SS; BindChannelAlias
	// writes the alias_bindings map. Both key the row by team/channel. Between
	// them they exercise all four attribute names statecrawl depends on.
	if err := store.ExposeResourceToChannel(ctx, "T1", "C1", "r_x"); err != nil {
		t.Fatalf("ExposeResourceToChannel: %v", err)
	}
	if err := store.BindChannelAlias(ctx, "T1", "C1", "dash", "r_x"); err != nil {
		t.Fatalf("BindChannelAlias: %v", err)
	}

	keysSeen := map[string]bool{}
	nameVals := map[string]bool{}
	var exprs strings.Builder
	for _, in := range fake.updateItems {
		for k := range in.Key {
			keysSeen[k] = true
		}
		for _, v := range in.ExpressionAttributeNames {
			nameVals[v] = true
		}
		exprs.WriteString(aws.ToString(in.UpdateExpression) + "\n")
	}

	if !keysSeen[attrSlackTeamID] || !keysSeen[attrSlackChannelID] {
		t.Errorf("slackdata write path keys the row by %v, not statecrawl's %q/%q — PK/SK constants drifted",
			mapKeys(keysSeen), attrSlackTeamID, attrSlackChannelID)
	}
	// ExposeResourceToChannel emits `ADD allowed_resource_ids :rids` with the
	// literal attribute name in the expression.
	if !strings.Contains(exprs.String(), attrAllowedResourceIDs) {
		t.Errorf("slackdata write path does not reference %q in %q — allowed_resource_ids constant drifted",
			attrAllowedResourceIDs, exprs.String())
	}
	// BindChannelAlias name-aliases the alias_bindings map attribute, so its
	// literal name shows up as an ExpressionAttributeNames value.
	if !nameVals[attrAliasBindings] {
		t.Errorf("slackdata write path does not use %q as an attribute name (saw %v) — alias_bindings constant drifted",
			attrAliasBindings, mapKeys(nameVals))
	}
}

func mapKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
