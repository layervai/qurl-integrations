package internal

// Test-only fixture helpers for seeding the fake DDB with the
// post-pivot table shapes. Mirrors the workspace_mappings /
// channel_policies / bootstrap_codes schemas fenced in
// modules/qurl-slack-ddb/main.tf.

import (
	"strconv"
	"testing"
	"time"

	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Attribute-name constants shared across fixture + assertion sites.
// Lifted because goconst flags 3+ occurrences of each literal.
const (
	fAttrSlackTeamID        = "slack_team_id"
	fAttrSlackChannelID     = "slack_channel_id"
	fAttrOwnerID            = "owner_id"
	fAttrSeedAdminSlackUser = "seed_admin_slack_user_id"
	fAttrAdminSlackUserIDs  = "admin_slack_user_ids"
	fAttrCreatedAt          = "created_at"
	fAttrUpdatedAt          = "updated_at"
	fAttrAlias              = "alias"
	fAttrResourceID         = "resource_id"
	fAttrAllowedResourceIDs = "allowed_resource_ids"
	fAttrAliasBindings      = "alias_bindings"
)

// seedWorkspaceAdmin returns a workspace_mappings row that marks
// `slackUserID` as admin for `teamID`. Used by the admin-check
// surfaces that pre-pivot exercised `/internal/v1/admin/check`.
func seedWorkspaceAdmin(teamID, ownerID, slackUserID string, configuredAt time.Time) map[string]ddbtypes.AttributeValue {
	at := configuredAt.UTC().Format(time.RFC3339)
	return map[string]ddbtypes.AttributeValue{
		fAttrSlackTeamID:        stringMember(teamID),
		fAttrOwnerID:            stringMember(ownerID),
		fAttrSeedAdminSlackUser: stringMember(slackUserID),
		fAttrAdminSlackUserIDs:  &ddbtypes.AttributeValueMemberSS{Value: []string{slackUserID}},
		fAttrCreatedAt:          stringMember(at),
		fAttrUpdatedAt:          stringMember(at),
	}
}

// seedWorkspaceNonAdmin returns a workspace_mappings row that
// exists for `teamID` but does NOT name `slackUserID` as admin.
// Used by the admin-check-no surfaces.
//
// `U_someone_else` violates the post-PR userMentionPattern (`[UW][A-Z0-9]{8,}`)
// because it's never mention-parsed — DDB only stores it as opaque
// data, CheckAdmin compares the *caller's* user_id (already
// validated upstream) against the SS membership. Kept as-is so the
// non-admin shape is visually distinct from the canonical
// `UADMIN001` / `UOWNER001` fixtures.
func seedWorkspaceNonAdmin(teamID, ownerID string) map[string]ddbtypes.AttributeValue {
	return map[string]ddbtypes.AttributeValue{
		fAttrSlackTeamID:        stringMember(teamID),
		fAttrOwnerID:            stringMember(ownerID),
		fAttrSeedAdminSlackUser: stringMember("U_someone_else"),
		fAttrAdminSlackUserIDs:  &ddbtypes.AttributeValueMemberSS{Value: []string{"U_someone_else"}},
		fAttrCreatedAt:          stringMember("2026-04-20T12:00:00Z"),
		fAttrUpdatedAt:          stringMember("2026-04-20T12:00:00Z"),
	}
}

// seedChannelPolicyDualShape returns a channel_policies row that writes
// both the post-pivot shape (`alias_bindings` Map +
// `allowed_resource_ids` SS) AND the legacy single-row scalar
// (`alias` + `resource_id`) so the fixture exercises ResolvePolicy's
// gate against both shapes in one row. NOT a canonical legacy-only
// row — tests that need the legacy scalar fallback in isolation
// (the actual `TestResolvePolicy_LegacySingleRowShape`) construct
// their row inline.
func seedChannelPolicyDualShape(teamID, channelID, alias, resourceID string) map[string]ddbtypes.AttributeValue {
	return map[string]ddbtypes.AttributeValue{
		fAttrSlackTeamID:    stringMember(teamID),
		fAttrSlackChannelID: stringMember(channelID),
		fAttrAlias:          stringMember(alias),
		fAttrResourceID:     stringMember(resourceID),
		fAttrAliasBindings: &ddbtypes.AttributeValueMemberM{
			Value: map[string]ddbtypes.AttributeValue{
				alias: stringMember(resourceID),
			},
		},
		fAttrAllowedResourceIDs: &ddbtypes.AttributeValueMemberSS{Value: []string{resourceID}},
		fAttrCreatedAt:          stringMember("2026-04-20T12:00:00Z"),
	}
}

// seedChannelPolicySet returns a channel_policies row carrying an
// allowed_resource_ids SS attribute. Used for the multi-resource
// ResolvePolicy gate (`/qurl get`).
//
// `alias_bindings` and `allowed_resource_ids` are orthogonal surfaces
// (see slackdata/policies.go preamble), so this helper attaches both
// when an alias is supplied so existing /qurl get + /qurl aliases
// tests can share a fixture. Callers that need ONLY the allowed-set
// (no alias listing) pass alias="".
func seedChannelPolicySet(teamID, channelID, alias string, resourceIDs []string) map[string]ddbtypes.AttributeValue {
	item := map[string]ddbtypes.AttributeValue{
		fAttrSlackTeamID:        stringMember(teamID),
		fAttrSlackChannelID:     stringMember(channelID),
		fAttrAllowedResourceIDs: &ddbtypes.AttributeValueMemberSS{Value: resourceIDs},
		fAttrCreatedAt:          stringMember("2026-04-20T12:00:00Z"),
	}
	if alias != "" && len(resourceIDs) > 0 {
		// Bind the alias to the first allowed resource so the
		// /qurl aliases listing has something to render. Tests that
		// want a precise (alias→resource) map use
		// seedChannelPolicyAliasBindings instead.
		item[fAttrAliasBindings] = &ddbtypes.AttributeValueMemberM{
			Value: map[string]ddbtypes.AttributeValue{
				alias: stringMember(resourceIDs[0]),
			},
		}
	}
	return item
}

// seedChannelPolicyAliasBindings returns a channel_policies row
// carrying an `alias_bindings` Map<alias_name, resource_id>. Used by
// the multi-alias /qurl aliases tests. Callers that also need an
// allowed_resource_ids set (e.g. to exercise /qurl get against the
// same row) merge the result with seedChannelPolicySet's SS attr at
// the call site.
func seedChannelPolicyAliasBindings(teamID, channelID string, bindings map[string]string) map[string]ddbtypes.AttributeValue {
	m := make(map[string]ddbtypes.AttributeValue, len(bindings))
	for alias, rid := range bindings {
		m[alias] = stringMember(rid)
	}
	return map[string]ddbtypes.AttributeValue{
		fAttrSlackTeamID:    stringMember(teamID),
		fAttrSlackChannelID: stringMember(channelID),
		fAttrAliasBindings:  &ddbtypes.AttributeValueMemberM{Value: m},
		fAttrCreatedAt:      stringMember("2026-04-20T12:00:00Z"),
	}
}

// seedBootstrapCode returns a bootstrap_codes row keyed by the
// sha256(plaintext) code_hash. `expiresAt` is encoded as a numeric
// epoch-seconds attribute (matches the DDB TTL conventions in the
// production schema). When `redeemed` is true the row is already
// consumed — the conditional UpdateItem will fail against it.
// Currently unused on #231 (no claim flow); kept so #233 can reuse
// the same fixture set.
func seedBootstrapCode(_ *testing.T, plaintext, ownerID, keyID string, expiresAt time.Time, redeemed bool) map[string]ddbtypes.AttributeValue {
	return map[string]ddbtypes.AttributeValue{
		"code_hash":  stringMember(plaintextCodeHash(plaintext)),
		fAttrOwnerID: stringMember(ownerID),
		"key_id":     stringMember(keyID),
		"redeemed":   boolMember(redeemed),
		"expires_at": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(expiresAt.Unix(), 10)},
	}
}

// plaintextCodeHash mirrors slackdata.hashBootstrapCode without
// importing it (the helper is unexported on the production side).
// Kept in sync with that implementation; if the algorithm rotates,
// update here too.
func plaintextCodeHash(plaintext string) string {
	return fakeSha256Hex([]byte(plaintext))
}

// stringMember is a 3-character alias for the AttributeValueMemberS
// constructor so fixture rows fit on one line.
func stringMember(v string) *ddbtypes.AttributeValueMemberS {
	return &ddbtypes.AttributeValueMemberS{Value: v}
}

// boolMember mirrors stringMember for boolean attrs.
func boolMember(v bool) *ddbtypes.AttributeValueMemberBOOL {
	return &ddbtypes.AttributeValueMemberBOOL{Value: v}
}
