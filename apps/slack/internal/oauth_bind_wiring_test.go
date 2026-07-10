package internal

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/oauth"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

// TestClassifyBindErrorMapping locks the slackdata.Error.Code to
// oauth.BindConflictCode mapping that wires the callback's switch arm to
// slackdata's 409 surface. The reflect-shape fence covers the struct; this
// covers the code-string mapping which is its own drift surface.
func TestClassifyBindErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want oauth.BindConflictCode
	}{
		{
			"already bound to caller (idempotent re-entry)",
			&slackdata.Error{StatusCode: http.StatusConflict, Code: slackdata.ErrCodeWorkspaceAlreadyBoundToCaller},
			oauth.BindConflictAlreadyBoundToCaller,
		},
		{
			"already bound to different admin (rebind-refused)",
			&slackdata.Error{StatusCode: http.StatusConflict, Code: slackdata.ErrCodeWorkspaceAlreadyBound},
			oauth.BindConflictAlreadyBound,
		},
		{
			"bind held but disambig read failed (unverified)",
			&slackdata.Error{StatusCode: http.StatusConflict, Code: slackdata.ErrCodeWorkspaceBindUnverified},
			oauth.BindConflictUnverified,
		},
		{
			"non-409 *slackdata.Error -> empty (generic failure)",
			&slackdata.Error{StatusCode: http.StatusServiceUnavailable, Code: "ddb_error"},
			"",
		},
		{
			"409 with unknown Code -> empty (default arm)",
			&slackdata.Error{StatusCode: http.StatusConflict, Code: "future_unmapped_code"},
			"",
		},
		{
			"non-*slackdata.Error -> empty",
			errors.New("plain string error"),
			"",
		},
		{"nil -> empty", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyOAuthBindError(c.err); got != c.want {
				t.Errorf("ClassifyOAuthBindError(%v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}

// TestAdminStoreAdapterForwardsAllFields exercises the production OAuth
// admin-store adapter against a captor that satisfies SlackdataBinder, with a
// non-zero CreatedAt. The reflect-shape test fences the struct field set; this
// fences the adapter's translation line.
func TestAdminStoreAdapterForwardsAllFields(t *testing.T) {
	captured := &capturingSlackdataStore{}
	adapter := NewOAuthAdminStoreAdapter(captured)
	want := oauth.WorkspaceMapping{
		TeamID:    "T_capture",
		OwnerID:   "auth0|capture-owner",
		CreatedAt: mustParseTime(t, "2026-05-20T12:34:56Z"),
	}
	if err := adapter.BindWorkspace(context.Background(), &want, "U_seed"); err != nil {
		t.Fatalf("BindWorkspace: %v", err)
	}
	if captured.gotMapping == nil {
		t.Fatal("adapter did not forward to the wrapped store")
	}
	if captured.gotMapping.TeamID != want.TeamID ||
		captured.gotMapping.OwnerID != want.OwnerID ||
		!captured.gotMapping.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("forwarded mapping mismatch:\nwant TeamID=%q OwnerID=%q CreatedAt=%v\ngot  TeamID=%q OwnerID=%q CreatedAt=%v",
			want.TeamID, want.OwnerID, want.CreatedAt,
			captured.gotMapping.TeamID, captured.gotMapping.OwnerID, captured.gotMapping.CreatedAt)
	}
	if captured.gotSeedAdmin != "U_seed" {
		t.Errorf("seedAdmin: got %q want %q", captured.gotSeedAdmin, "U_seed")
	}
}

// capturingSlackdataStore satisfies SlackdataBinder so the production OAuth
// admin-store adapter can be exercised without standing up a real slackdata.Store.
type capturingSlackdataStore struct {
	gotMapping   *slackdata.WorkspaceMapping
	gotSeedAdmin string
}

// BindWorkspace records the mapping and seed admin passed by the adapter.
func (c *capturingSlackdataStore) BindWorkspace(_ context.Context, m *slackdata.WorkspaceMapping, seedAdmin string) error {
	c.gotMapping = m
	c.gotSeedAdmin = seedAdmin
	return nil
}

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

// TestAdminStoreAdapterMappingShapesMatch fences the field-for-field
// equivalence of oauth.WorkspaceMapping and slackdata.WorkspaceMapping.
func TestAdminStoreAdapterMappingShapesMatch(t *testing.T) {
	oauthFields := structFieldSet(reflect.TypeOf(oauth.WorkspaceMapping{}))
	storeFields := structFieldSet(reflect.TypeOf(slackdata.WorkspaceMapping{}))
	if !reflect.DeepEqual(oauthFields, storeFields) {
		t.Errorf("oauth.WorkspaceMapping vs slackdata.WorkspaceMapping fields differ; OAuth admin-store adapter copy would silently drop the diff\noauth:     %v\nslackdata: %v", oauthFields, storeFields)
	}
}

// structFieldSet returns the name-to-type map for a struct type.
func structFieldSet(t reflect.Type) map[string]string {
	out := make(map[string]string, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		out[f.Name] = f.Type.String()
	}
	return out
}
