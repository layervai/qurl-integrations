package main

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// channel_policies attribute names. These MUST stay in lockstep with the
// unexported literals in apps/slack/internal/slackdata (workspace.go +
// policies.go) — the bot owns the schema, and this tool only reads it. They are
// duplicated rather than exported to avoid widening the slackdata API surface
// for a one-off reconciler.
const (
	attrSlackTeamID        = "slack_team_id"
	attrSlackChannelID     = "slack_channel_id"
	attrAliasBindings      = "alias_bindings"
	attrAllowedResourceIDs = "allowed_resource_ids"
)

// policyRow is one (team, channel) channel_policies row, flattened to the two
// surfaces #654/#669 act on: alias_bindings and allowed_resource_ids. These are
// exactly what slackdata.allowedResourceIDsFromItem unions and what
// PurgeResourceFromChannel clears, kept separate here so the report can tell an
// operator WHICH surface carried an orphan. The legacy pre-pivot `resource_id`
// scalar is intentionally out of scope — the purge verb never touches it.
type policyRow struct {
	teamID             string
	channelID          string
	aliasBindings      map[string]string // alias name -> resource id
	allowedResourceIDs []string
}

// policyTableReader is the read surface scanPolicyRows needs: a Scan for the
// full sweep and a Query for the single-team fast path. Both are SDK interfaces
// satisfied by *dynamodb.Client, so tests can inject a fake without localstack.
type policyTableReader interface {
	dynamodb.ScanAPIClient
	dynamodb.QueryAPIClient
}

// scanPolicyRows returns the parsed channel_policies rows, either for one team
// or the whole table. Rows missing the team/channel keys are skipped — they
// can't be acted on and shouldn't abort the crawl.
//
// Single-team (onlyTeam set) is a Query on the partition key — channel_policies
// is PK=slack_team_id, so this reads only that team's rows. This is the
// documented "unblock a customer fast" path, exactly where billing/latency for
// a full-table read would hurt most. The unscoped sweep is a Scan (the right
// tool for a rare, out-of-band whole-table pass).
func scanPolicyRows(ctx context.Context, reader policyTableReader, table, onlyTeam string) ([]policyRow, error) {
	if onlyTeam != "" {
		return queryTeamRows(ctx, reader, table, onlyTeam)
	}
	return scanAllRows(ctx, reader, table)
}

// queryTeamRows pages one team's rows via a partition-key Query (matching
// slackdata.ChannelsForResource's pattern), so a scoped run never reads the
// whole table.
func queryTeamRows(ctx context.Context, q dynamodb.QueryAPIClient, table, teamID string) ([]policyRow, error) {
	in := &dynamodb.QueryInput{
		TableName:              aws.String(table),
		KeyConditionExpression: aws.String("#tid = :tid"),
		ExpressionAttributeNames: map[string]string{
			"#tid": attrSlackTeamID,
		},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":tid": &ddbtypes.AttributeValueMemberS{Value: teamID},
		},
	}
	var rows []policyRow
	paginator := dynamodb.NewQueryPaginator(q, in)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("query page: %w", err)
		}
		rows = appendParsedRows(rows, page.Items)
	}
	return rows, nil
}

// scanAllRows pages the whole table via Scan for the unscoped full sweep.
func scanAllRows(ctx context.Context, s dynamodb.ScanAPIClient, table string) ([]policyRow, error) {
	var rows []policyRow
	paginator := dynamodb.NewScanPaginator(s, &dynamodb.ScanInput{TableName: aws.String(table)})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("scan page: %w", err)
		}
		rows = appendParsedRows(rows, page.Items)
	}
	return rows, nil
}

// appendParsedRows parses each DDB item and appends the valid ones (keyless
// rows skipped) to rows.
func appendParsedRows(rows []policyRow, items []map[string]ddbtypes.AttributeValue) []policyRow {
	for _, item := range items {
		if row, ok := parsePolicyRow(item); ok {
			rows = append(rows, row)
		}
	}
	return rows
}

// parsePolicyRow flattens a DynamoDB item into a policyRow. Returns ok=false
// when the partition/sort keys are missing or empty — such a row can't be
// reconciled or purged, so it's dropped rather than half-processed.
func parsePolicyRow(item map[string]ddbtypes.AttributeValue) (policyRow, bool) {
	teamID := readString(item, attrSlackTeamID)
	channelID := readString(item, attrSlackChannelID)
	if teamID == "" || channelID == "" {
		return policyRow{}, false
	}
	return policyRow{
		teamID:             teamID,
		channelID:          channelID,
		aliasBindings:      readStringMap(item, attrAliasBindings),
		allowedResourceIDs: readStringSet(item, attrAllowedResourceIDs),
	}, true
}

// readString reads a string attribute, or "" when missing/wrong-type.
func readString(item map[string]ddbtypes.AttributeValue, key string) string {
	v, ok := item[key].(*ddbtypes.AttributeValueMemberS)
	if !ok {
		return ""
	}
	return v.Value
}

// readStringSet reads a string-set attribute, or nil when missing/wrong-type.
func readStringSet(item map[string]ddbtypes.AttributeValue, key string) []string {
	v, ok := item[key].(*ddbtypes.AttributeValueMemberSS)
	if !ok {
		return nil
	}
	return v.Value
}

// readStringMap reads a Map<string,string> attribute, skipping any non-string
// value, or nil when missing/wrong-type. Mirrors slackdata.readStringMap.
func readStringMap(item map[string]ddbtypes.AttributeValue, key string) map[string]string {
	m, ok := item[key].(*ddbtypes.AttributeValueMemberM)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m.Value))
	for k, v := range m.Value {
		s, ok := v.(*ddbtypes.AttributeValueMemberS)
		if !ok {
			continue
		}
		out[k] = s.Value
	}
	return out
}
