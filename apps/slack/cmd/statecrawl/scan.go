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

// scanPolicyRows pages the entire channel_policies table (or a single team when
// onlyTeam is set, via a server-side FilterExpression) and returns the parsed
// rows. A Scan is the right tool here: the reconciler is a full sweep, run
// rarely and out-of-band, not a hot path. Rows missing the team/channel keys
// are skipped — they can't be acted on and shouldn't abort the crawl. The
// scanner is the SDK's ScanAPIClient interface (satisfied by *dynamodb.Client)
// so tests can inject a fake without localstack.
func scanPolicyRows(ctx context.Context, scanner dynamodb.ScanAPIClient, table, onlyTeam string) ([]policyRow, error) {
	in := &dynamodb.ScanInput{TableName: aws.String(table)}
	if onlyTeam != "" {
		in.FilterExpression = aws.String("#tid = :tid")
		in.ExpressionAttributeNames = map[string]string{"#tid": attrSlackTeamID}
		in.ExpressionAttributeValues = map[string]ddbtypes.AttributeValue{
			":tid": &ddbtypes.AttributeValueMemberS{Value: onlyTeam},
		}
	}

	var rows []policyRow
	paginator := dynamodb.NewScanPaginator(scanner, in)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("scan page: %w", err)
		}
		for _, item := range page.Items {
			row, ok := parsePolicyRow(item)
			if ok {
				rows = append(rows, row)
			}
		}
	}
	return rows, nil
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
