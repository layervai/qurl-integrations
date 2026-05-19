package internal

// Test-only helpers for wiring the post-pivot Slack admin handler
// against a fake DynamoDB. The pre-pivot version of this file
// (kept until the cutover) declared `NewAdminClient` and
// `testInternalToken` as compat shims because the test bodies
// pre-dated the slackdata package. The fake-DDB harness (this
// file's [adminTestServers] + [newAdminTestHandler]) lets those
// tests exercise the real slackdata.Store against an in-memory
// table set instead of the old `/internal/v1/admin/*` HTTP fake.
//
// The two pieces:
//   - [adminTestServers] owns the customer-facing httptest server
//     (qurl-service `/v1/...`) plus the fake DDB the slackdata
//     Store reads/writes. The "admin server" is gone — there is
//     no HTTP surface in the post-pivot architecture.
//   - The seed helpers (seedAdmin / seedNonAdmin / seedPolicy /
//     seedPolicySet) translate the test fixture's intent into
//     DDB rows. Tests call these instead of the old
//     `ts.addAdmin("GET","/internal/v1/admin/check", ...)` pattern.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// adminTestServers owns the customer httptest server and the
// fake-DDB-backed slackdata.Store the handler runs against.
type adminTestServers struct {
	customerServer   *httptest.Server
	customerRoutes   map[string]http.HandlerFunc
	customerPrefixes []customerPrefixRoute
	ddb              *fakeDDB
	tableNames       tableNames
}

func newAdminTestServers(t *testing.T) *adminTestServers {
	t.Helper()
	names := defaultTestTableNames()
	ts := &adminTestServers{
		customerRoutes: map[string]http.HandlerFunc{},
		ddb:            newFakeDDB(t, names, nil),
		tableNames:     names,
	}
	ts.customerServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		if h, ok := ts.customerRoutes[key]; ok {
			h(w, r)
			return
		}
		// Prefix routes are checked after exact matches so a test
		// that adds both shapes gets deterministic dispatch (exact
		// wins). First registered prefix wins among prefixes.
		for _, p := range ts.customerPrefixes {
			if r.Method == p.method && strings.HasPrefix(r.URL.Path, p.pathPrefix) {
				p.handler(w, r)
				return
			}
		}
		// Default: 404 with a JSON envelope, mirroring qurl-service shape.
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"title":"Not Found","detail":"unmatched test route","code":"not_found","status":404}}`))
	}))
	t.Cleanup(ts.customerServer.Close)
	return ts
}

func (ts *adminTestServers) addCustomer(method, path string, h http.HandlerFunc) {
	ts.customerRoutes[method+" "+path] = h
}

// addCustomerPrefix routes a method+path-prefix through the same
// dispatch map as [adminTestServers.addCustomer] without forcing
// callers to overwrite `customerServer.Config.Handler` (which
// stops `addCustomer` calls registered after the overwrite from
// being seen). Used by tests that need wildcard DELETE routing
// for synthesized qurl_id fixtures (e.g. revoke-all walking
// q_aaa, q_bbb, ...). Exact-match routes added via `addCustomer`
// take precedence.
func (ts *adminTestServers) addCustomerPrefix(method, pathPrefix string, h http.HandlerFunc) {
	ts.customerPrefixes = append(ts.customerPrefixes, customerPrefixRoute{
		method:     method,
		pathPrefix: pathPrefix,
		handler:    h,
	})
}

type customerPrefixRoute struct {
	method, pathPrefix string
	handler            http.HandlerFunc
}

// testAdminTeamID is the workspace ID every admin-seed helper
// defaults to. Lifted because `T_team` appears in every test.
const testAdminTeamID = "T_team"

// testAdminUserID is the admin user ID matching seedAdmin/seedNonAdmin.
// Slack-shape (uppercase alphanumeric, no underscore) so the admin
// add/remove handlers — which parse `<@U…>` mentions through the
// strict userMentionPattern — can accept it as a mention target.
const testAdminUserID = "UADMIN001"

// testAdminOwnerID is the owner ID the workspace_mappings row binds
// to in tests. Slack-shape ID (see [testAdminUserID]) so the admin
// remove handler's owner-check path can be exercised against a
// `<@U…>` mention without tripping the mention validator.
const testAdminOwnerID = "UOWNER001"

// testWorkspaceConfiguredAt is the canonical `created_at` time the
// workspace fixture exposes. Matches the legacy
// `configured_at:"2026-04-20T12:00:00Z"` payload.
var testWorkspaceConfiguredAt = mustParseRFC3339("2026-04-20T12:00:00Z")

func mustParseRFC3339(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// seedAdmin seeds a workspace_mappings row where U_admin is on the
// admin set for T_team. Mirrors the pre-pivot `adminCheckYes()` HTTP
// route. Tests that need a different team/user/owner triple pass
// explicit args via seedWorkspace.
func (ts *adminTestServers) seedAdmin(t *testing.T) {
	t.Helper()
	ts.seedWorkspace(t, testAdminTeamID, testAdminOwnerID, testAdminUserID, testWorkspaceConfiguredAt)
}

// seedNonAdmin seeds a workspace row that exists for T_team but
// does NOT name U_admin. Mirrors the pre-pivot `adminCheckNo()`
// route. CheckAdmin will return (false, owner_id, nil).
func (ts *adminTestServers) seedNonAdmin(t *testing.T) {
	t.Helper()
	ts.ddb.seedItem(t, ts.tableNames.workspace, seedWorkspaceNonAdmin(testAdminTeamID, testAdminOwnerID))
}

// seedWorkspace is the general form — seeds a workspace_mappings
// row that names slackUserID on the admin set for teamID.
func (ts *adminTestServers) seedWorkspace(t *testing.T, teamID, ownerID, slackUserID string, configuredAt time.Time) {
	t.Helper()
	ts.ddb.seedItem(t, ts.tableNames.workspace, seedWorkspaceAdmin(teamID, ownerID, slackUserID, configuredAt))
}

// seedPolicyDualShape seeds a channel_policies row carrying BOTH
// the legacy single-row scalar (alias + resource_id) AND the
// post-pivot shape (alias_bindings Map + allowed_resource_ids SS).
// Use when tests need to exercise ResolvePolicy's gate against both
// shapes on the same row. Tests that need legacy-isolation
// construct their row inline (see TestResolvePolicy_LegacySingleRowShape).
func (ts *adminTestServers) seedPolicyDualShape(t *testing.T, teamID, channelID, alias, resourceID string) {
	t.Helper()
	ts.ddb.seedItem(t, ts.tableNames.channelPolicy, seedChannelPolicyDualShape(teamID, channelID, alias, resourceID))
}

// seedPolicySet seeds a channel_policies row carrying an
// allowed_resource_ids SS attribute. Used for ResolvePolicy tests.
// If alias is non-empty, also seeds a single-binding alias_bindings
// Map so /qurl aliases lists the channel — convenient for tests
// that exercise both surfaces against the same row.
func (ts *adminTestServers) seedPolicySet(t *testing.T, teamID, channelID, alias string, resourceIDs []string) {
	t.Helper()
	ts.ddb.seedItem(t, ts.tableNames.channelPolicy, seedChannelPolicySet(teamID, channelID, alias, resourceIDs))
}

// seedPolicyAliasBindings seeds a channel_policies row with an
// `alias_bindings` Map<alias_name, resource_id>. Used by
// multi-alias /qurl aliases tests. No allowed_resource_ids set is
// attached (orthogonal surface — callers seed both via
// seedPolicySet when both are needed).
func (ts *adminTestServers) seedPolicyAliasBindings(t *testing.T, teamID, channelID string, bindings map[string]string) {
	t.Helper()
	ts.ddb.seedItem(t, ts.tableNames.channelPolicy, seedChannelPolicyAliasBindings(teamID, channelID, bindings))
}

// failOnAdminMutation installs a hook that fails the test if any
// UpdateItem hits the table set. Used to assert the admin-only gate
// short-circuits before any policy or admin-set mutation.
func (ts *adminTestServers) failOnAdminMutation(t *testing.T, msg string) {
	t.Helper()
	ts.ddb.SetUpdateItemHook(func(in interface{}) {
		t.Errorf("admin UpdateItem reached despite gate: %s", msg)
	})
}
