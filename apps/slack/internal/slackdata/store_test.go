package slackdata

// Package-level tests for slackdata. The handler-layer tests in
// apps/slack/internal/ already exercise the Store end-to-end against
// the same fakeDDB, but those tests can drift the *Error contract
// (StatusCode/Code/Title that handlers branch on via errors.As) and
// still pass at the handler level. These tests fence the contract
// inside the package so a refactor of the conditional-check paths is
// caught here before it reaches the handler layer.

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// stubDDB is a minimal in-package DynamoDBClient that returns
// pre-canned responses keyed by op. Just enough for the package-
// boundary tests below — the cross-handler integration tests have a
// fuller fakeDDB in the internal/ package.
type stubDDB struct {
	getItemFn    func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error)
	putItemFn    func(*dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error)
	updateItemFn func(*dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error)
	deleteItemFn func(*dynamodb.DeleteItemInput) (*dynamodb.DeleteItemOutput, error)
	queryFn      func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error)
}

func (s *stubDDB) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	if s.getItemFn == nil {
		return &dynamodb.GetItemOutput{}, nil
	}
	return s.getItemFn(in)
}

func (s *stubDDB) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	if s.putItemFn == nil {
		return &dynamodb.PutItemOutput{}, nil
	}
	return s.putItemFn(in)
}

func (s *stubDDB) UpdateItem(_ context.Context, in *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	if s.updateItemFn == nil {
		return &dynamodb.UpdateItemOutput{}, nil
	}
	return s.updateItemFn(in)
}

func (s *stubDDB) DeleteItem(_ context.Context, in *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	if s.deleteItemFn == nil {
		return &dynamodb.DeleteItemOutput{}, nil
	}
	return s.deleteItemFn(in)
}

func (s *stubDDB) Query(_ context.Context, in *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	if s.queryFn == nil {
		return &dynamodb.QueryOutput{}, nil
	}
	return s.queryFn(in)
}

func newStore(client DynamoDBClient) *Store {
	return &Store{
		Client:                client,
		WorkspaceMappingsName: "ws",
		ChannelPoliciesName:   "cp",
		Now:                   func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	}
}

// TestError_FormatPreservesAdminErrorShape fences the legacy
// `Title [Code] (StatusCode)` / `Title [Code] (StatusCode): Detail`
// format. Handlers don't read these strings, but operators grepping
// CloudWatch for the old AdminError shape rely on them — drift the
// format and the runbooks break.
func TestError_FormatPreservesAdminErrorShape(t *testing.T) {
	cases := []struct {
		name string
		e    Error
		want string
	}{
		{
			name: "all fields",
			e:    Error{StatusCode: 409, Code: "x_conflict", Title: "Op", Detail: "row exists"},
			want: "Op [x_conflict] (409): row exists",
		},
		{
			name: "no detail",
			e:    Error{StatusCode: 409, Code: ErrCodeAdminAlreadyExists, Title: "AddAdmin: user already on admin set"},
			want: "AddAdmin: user already on admin set [admin_already_exists] (409)",
		},
		{
			name: "no code",
			e:    Error{StatusCode: 503, Title: "ddb timeout", Detail: "i/o"},
			want: "ddb timeout (503): i/o",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.e.Error(); got != c.want {
				t.Errorf("Error() = %q, want %q", got, c.want)
			}
		})
	}
}

// TestError_ErrorsAsRoundTrip fences the [errors.As] contract that
// every handler-side branch reads through (`errors.As(err, &ae) &&
// ae.StatusCode == X`). The Store returns `*Error`; consumers must
// be able to unwrap that into a value they can dispatch on.
func TestError_ErrorsAsRoundTrip(t *testing.T) {
	wrapped := errors.Join(errors.New("outer"), &Error{StatusCode: 409, Code: ErrCodeWorkspaceAlreadyBound, Title: "x"})
	var ae *Error
	if !errors.As(wrapped, &ae) {
		t.Fatalf("errors.As did not unwrap *Error from join-wrapped error")
	}
	if ae.StatusCode != 409 {
		t.Errorf("unwrapped StatusCode = %d, want 409", ae.StatusCode)
	}
	if ae.Code != ErrCodeWorkspaceAlreadyBound {
		t.Errorf("unwrapped Code = %q, want %q", ae.Code, ErrCodeWorkspaceAlreadyBound)
	}
}

// TestBindWorkspace_ValidationGuards fences the input-validation
// surface: zero/missing team_id / owner_id / seed_admin produce a
// 400 [*Error] before the DDB call. Without this, the DDB request
// would land a ValidationException whose error string is less
// actionable for the handler.
func TestBindWorkspace_ValidationGuards(t *testing.T) {
	cases := []struct {
		name      string
		mapping   *WorkspaceMapping
		seedAdmin string
	}{
		{"nil mapping", nil, "U"},
		{"empty team_id", &WorkspaceMapping{OwnerID: "u_owner"}, "U"},
		{"empty owner_id", &WorkspaceMapping{TeamID: "T"}, "U"},
		{"empty seed_admin", &WorkspaceMapping{TeamID: "T", OwnerID: "u_owner"}, ""},
	}
	store := newStore(&stubDDB{
		putItemFn: func(_ *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
			t.Fatalf("PutItem must not be called when validation rejects upstream")
			return nil, errors.New("unreachable")
		},
	})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := store.BindWorkspace(context.Background(), c.mapping, c.seedAdmin)
			var ae *Error
			if !errors.As(err, &ae) {
				t.Fatalf("got %v, want *Error", err)
			}
			if ae.StatusCode != http.StatusBadRequest {
				t.Errorf("StatusCode = %d, want 400", ae.StatusCode)
			}
		})
	}
}

// TestBindWorkspace_DistinguishesSameCallerFromDifferentAdmin fences
// the two-branch 409 mapping that the handler's `surfaceBindError`
// branches on via `ae.Code`:
//
//   - row exists, admin_slack_user_ids contains the caller →
//     ErrCodeWorkspaceAlreadyBoundToCaller
//   - row exists, admin_slack_user_ids does not contain the caller →
//     ErrCodeWorkspaceAlreadyBound
//
// Drift either constant or invert the branch and the handler's
// user-copy will desynchronize from the actual workspace state.
func TestBindWorkspace_DistinguishesSameCallerFromDifferentAdmin(t *testing.T) {
	t.Run("same caller already bound", func(t *testing.T) {
		store := newStore(&stubDDB{
			putItemFn: func(_ *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
				return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("exists")}
			},
			getItemFn: func(_ *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				return &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
					attrSlackTeamID: &ddbtypes.AttributeValueMemberS{Value: "T"},
					attrAdminSlackUserIDs: &ddbtypes.AttributeValueMemberSS{
						Value: []string{"U_caller", "U_other"},
					},
				}}, nil
			},
		})
		err := store.BindWorkspace(context.Background(), &WorkspaceMapping{TeamID: "T", OwnerID: "u_owner"}, "U_caller")
		var ae *Error
		if !errors.As(err, &ae) {
			t.Fatalf("got %v, want *Error", err)
		}
		if ae.StatusCode != http.StatusConflict {
			t.Errorf("StatusCode = %d, want 409", ae.StatusCode)
		}
		if ae.Code != ErrCodeWorkspaceAlreadyBoundToCaller {
			t.Errorf("Code = %q, want %q", ae.Code, ErrCodeWorkspaceAlreadyBoundToCaller)
		}
	})

	t.Run("different admin holds workspace", func(t *testing.T) {
		store := newStore(&stubDDB{
			putItemFn: func(_ *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
				return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("exists")}
			},
			getItemFn: func(_ *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				return &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
					attrSlackTeamID: &ddbtypes.AttributeValueMemberS{Value: "T"},
					attrAdminSlackUserIDs: &ddbtypes.AttributeValueMemberSS{
						Value: []string{"U_other"},
					},
				}}, nil
			},
		})
		err := store.BindWorkspace(context.Background(), &WorkspaceMapping{TeamID: "T", OwnerID: "u_owner"}, "U_caller")
		var ae *Error
		if !errors.As(err, &ae) {
			t.Fatalf("got %v, want *Error", err)
		}
		if ae.Code != ErrCodeWorkspaceAlreadyBound {
			t.Errorf("Code = %q, want %q", ae.Code, ErrCodeWorkspaceAlreadyBound)
		}
	})

	// Fence round-19 cr #1: the post-CCFE disambiguation GetItem
	// MUST use ConsistentRead=true. The CCFE confirms a row exists,
	// but an eventually-consistent read on a stale replica could
	// miss it and route a same-caller re-entry to the "different
	// admin" branch. A refactor that drops the flag would replay
	// that race silently.
	t.Run("disambig GetItem uses ConsistentRead=true", func(t *testing.T) {
		var disambigConsistent bool
		store := newStore(&stubDDB{
			putItemFn: func(_ *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
				return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("exists")}
			},
			getItemFn: func(in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				disambigConsistent = aws.ToBool(in.ConsistentRead)
				return &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
					attrSlackTeamID:       &ddbtypes.AttributeValueMemberS{Value: "T"},
					attrAdminSlackUserIDs: &ddbtypes.AttributeValueMemberSS{Value: []string{"U_other"}},
				}}, nil
			},
		})
		_ = store.BindWorkspace(context.Background(), &WorkspaceMapping{TeamID: "T", OwnerID: "u_owner"}, "U_caller")
		if !disambigConsistent {
			t.Errorf("disambig GetItem ConsistentRead = false, want true (regression would replay the eventual-consistency race)")
		}
	})

	// Fence the disambiguation-GetItem-fails path: the binding is
	// still held (the CCFE confirmed that), but the post-CCFE Get
	// failed. Surface the [ErrCodeWorkspaceBindUnverified] variant
	// so the handler can render the "couldn't confirm — please
	// retry" copy instead of defaulting to "different admin" (which
	// would tell a same-caller re-entry to ask themselves for
	// help). Round-17 cr #3.
	t.Run("disambiguating GetItem fails", func(t *testing.T) {
		store := newStore(&stubDDB{
			putItemFn: func(_ *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
				return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("exists")}
			},
			getItemFn: func(_ *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				return nil, errors.New("transport blip")
			},
		})
		err := store.BindWorkspace(context.Background(), &WorkspaceMapping{TeamID: "T", OwnerID: "u_owner"}, "U_caller")
		var ae *Error
		if !errors.As(err, &ae) {
			t.Fatalf("got %v, want *Error", err)
		}
		if ae.StatusCode != http.StatusConflict {
			t.Errorf("StatusCode = %d, want 409 (binding is still held; only the disambig read is uncertain)", ae.StatusCode)
		}
		if ae.Code != ErrCodeWorkspaceBindUnverified {
			t.Errorf("Code = %q, want %q (disambig read failed → 'couldn't confirm' variant)", ae.Code, ErrCodeWorkspaceBindUnverified)
		}
	})
}

// TestBindWorkspace_WritesSeedAdminAttribute fences the forensic-
// attribution promise documented at workspace.go's
// attrSeedAdminSlackUser comment block: BindWorkspace MUST stamp the
// seedAdmin Slack user ID onto a write-only `seed_admin_slack_user_id`
// attribute so on-call can answer "who was the original installer?"
// after the admin set churns. A refactor that drops this line from
// the PutItem (e.g., by inlining the item map) would silently lose
// the attribution; no other test exercises this attr.
func TestBindWorkspace_WritesSeedAdminAttribute(t *testing.T) {
	var captured *dynamodb.PutItemInput
	store := newStore(&stubDDB{
		putItemFn: func(in *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
			captured = in
			return &dynamodb.PutItemOutput{}, nil
		},
	})
	const (
		teamID    = "T_seed"
		ownerID   = "auth0|owner-1"
		seedAdmin = "USEEDADMIN"
	)
	if err := store.BindWorkspace(context.Background(),
		&WorkspaceMapping{TeamID: teamID, OwnerID: ownerID}, seedAdmin); err != nil {
		t.Fatalf("BindWorkspace: %v", err)
	}
	if captured == nil {
		t.Fatal("PutItem not invoked")
	}
	got, ok := captured.Item[attrSeedAdminSlackUser].(*ddbtypes.AttributeValueMemberS)
	if !ok {
		t.Fatalf("seed_admin_slack_user_id missing or wrong type in PutItem (item=%+v)", captured.Item)
	}
	if got.Value != seedAdmin {
		t.Errorf("seed_admin_slack_user_id: got %q want %q", got.Value, seedAdmin)
	}
}

// TestBindWorkspace_TransportErrorMapsTo503 fences the
// ddbToError fallthrough: a non-CCFE PutItem error becomes
// 503/ddb_error, NOT 409. The handler's `surfaceBindError` routes
// 409 to the workspace-conflict copy and everything else to the
// "code redeemed but bind failed — contact support" copy; drift
// this and the user gets the wrong escalation path.
func TestBindWorkspace_TransportErrorMapsTo503(t *testing.T) {
	store := newStore(&stubDDB{
		putItemFn: func(_ *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
			return nil, errors.New("dial tcp: timeout")
		},
	})
	err := store.BindWorkspace(context.Background(), &WorkspaceMapping{TeamID: "T", OwnerID: "u_owner"}, "U_caller")
	var ae *Error
	if !errors.As(err, &ae) {
		t.Fatalf("got %v, want *Error", err)
	}
	if ae.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("StatusCode = %d, want 503", ae.StatusCode)
	}
	if ae.Code != "ddb_error" {
		t.Errorf("Code = %q, want %q", ae.Code, "ddb_error")
	}
}

// TestLookupChannelAlias_ValidationGuards fences the empty-input
// contract: empty teamID, channelID, or aliasName all return a
// 400-bracketed *Error before the DDB call. The handler layer guards
// these upstream, but this is defense-in-depth — a future caller
// that skips its own validation gets a typed error rather than a
// DDB ValidationException on the wire.
func TestLookupChannelAlias_ValidationGuards(t *testing.T) {
	cases := []struct {
		name                     string
		team, channel, aliasName string
	}{
		{"empty team", "", "C1", "a"},
		{"empty channel", "T1", "", "a"},
		{"empty alias", "T1", "C1", ""},
		{"all empty", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var dbHits int
			ddb := &stubDDB{
				getItemFn: func(_ *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
					dbHits++
					return &dynamodb.GetItemOutput{}, nil
				},
			}
			store := newStore(ddb)
			rid, found, err := store.LookupChannelAlias(context.Background(), c.team, c.channel, c.aliasName)
			if rid != "" || found {
				t.Errorf("got (rid=%q, found=%v), want zero-valued returns", rid, found)
			}
			var ae *Error
			if !errors.As(err, &ae) {
				t.Fatalf("got %v, want *Error", err)
			}
			if ae.StatusCode != http.StatusBadRequest {
				t.Errorf("StatusCode = %d, want 400", ae.StatusCode)
			}
			if dbHits != 0 {
				t.Errorf("DDB GetItem reached despite validation guard (hits = %d)", dbHits)
			}
		})
	}
}

// TestLookupChannelAlias_MissingRow fences the row-missing branch:
// no channel_policies row for (team, channel) returns
// (resourceID="", found=false, err=nil). DDB returns an empty
// Item map; we must NOT treat that as a 5xx surface.
func TestLookupChannelAlias_MissingRow(t *testing.T) {
	ddb := &stubDDB{
		getItemFn: func(_ *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{}, nil
		},
	}
	rid, found, err := newStore(ddb).LookupChannelAlias(context.Background(), "T1", "C1", "prod-db")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if found {
		t.Errorf("found = true, want false on missing row")
	}
	if rid != "" {
		t.Errorf("resourceID = %q, want empty on missing row", rid)
	}
}

// TestLookupChannelAlias_MissingMap fences the row-present-but-no-
// alias_bindings branch: the projection comes back with no
// `alias_bindings` attribute at all. Treated as not-found, never as
// an error.
func TestLookupChannelAlias_MissingMap(t *testing.T) {
	ddb := &stubDDB{
		getItemFn: func(_ *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			// Non-empty Item with a placeholder attribute so the
			// `len(out.Item) == 0` early-return doesn't fire — exercises
			// the readStringMap-misses-key branch instead.
			return &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
				"placeholder": &ddbtypes.AttributeValueMemberS{Value: "x"},
			}}, nil
		},
	}
	rid, found, err := newStore(ddb).LookupChannelAlias(context.Background(), "T1", "C1", "prod-db")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if found || rid != "" {
		t.Errorf("got (rid=%q, found=%v), want zero values when alias_bindings is missing", rid, found)
	}
}

// TestLookupChannelAlias_MissingKey fences the alias_bindings-present-
// but-key-missing branch: the projection returns alias_bindings as a
// Map with other entries but not the requested alias. Same disposition
// as missing-row and missing-map: (found=false, err=nil).
func TestLookupChannelAlias_MissingKey(t *testing.T) {
	ddb := &stubDDB{
		getItemFn: func(_ *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			return &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
				attrAliasBindings: &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{
					"someone-else": &ddbtypes.AttributeValueMemberS{Value: "r_other"},
				}},
			}}, nil
		},
	}
	rid, found, err := newStore(ddb).LookupChannelAlias(context.Background(), "T1", "C1", "prod-db")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if found || rid != "" {
		t.Errorf("got (rid=%q, found=%v), want zero values when alias key is missing", rid, found)
	}
}

// TestLookupChannelAlias_HappyPath fences the binding-present return:
// alias_bindings carries the requested key → (resource_id, true, nil).
// Also asserts the GetItem input shape: PK/SK on the channel-policies
// table and ProjectionExpression scoped to the single map key (no
// over-projection that pulls allowed_resource_ids or audit columns).
func TestLookupChannelAlias_HappyPath(t *testing.T) {
	var captured *dynamodb.GetItemInput
	ddb := &stubDDB{
		getItemFn: func(in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			captured = in
			return &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
				attrAliasBindings: &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{
					"prod-db": &ddbtypes.AttributeValueMemberS{Value: "r_prod_db"},
				}},
			}}, nil
		},
	}
	rid, found, err := newStore(ddb).LookupChannelAlias(context.Background(), "T1", "C1", "prod-db")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !found {
		t.Errorf("found = false, want true on binding present")
	}
	if rid != "r_prod_db" {
		t.Errorf("resourceID = %q, want r_prod_db", rid)
	}
	if captured == nil {
		t.Fatal("GetItem was not called")
	}
	if aws.ToString(captured.TableName) != "cp" {
		t.Errorf("TableName = %q, want cp", aws.ToString(captured.TableName))
	}
	if aws.ToString(captured.ProjectionExpression) != "#ab.#a" {
		t.Errorf("ProjectionExpression = %q, want #ab.#a (single map key, not the full row)", aws.ToString(captured.ProjectionExpression))
	}
	if captured.ExpressionAttributeNames["#ab"] != attrAliasBindings {
		t.Errorf("ExpressionAttributeNames[#ab] = %q, want %q", captured.ExpressionAttributeNames["#ab"], attrAliasBindings)
	}
	if captured.ExpressionAttributeNames["#a"] != "prod-db" {
		t.Errorf("ExpressionAttributeNames[#a] = %q, want prod-db (alias must flow through escaping, not literal expression)", captured.ExpressionAttributeNames["#a"])
	}
	teamAttr, _ := captured.Key[attrSlackTeamID].(*ddbtypes.AttributeValueMemberS)
	if teamAttr == nil || teamAttr.Value != "T1" {
		t.Errorf("Key[%s] = %#v, want T1", attrSlackTeamID, captured.Key[attrSlackTeamID])
	}
	channelAttr, _ := captured.Key[attrSlackChannelID].(*ddbtypes.AttributeValueMemberS)
	if channelAttr == nil || channelAttr.Value != "C1" {
		t.Errorf("Key[%s] = %#v, want C1", attrSlackChannelID, captured.Key[attrSlackChannelID])
	}
}

// TestLookupChannelAlias_DDBError fences the transport-error
// branch: a non-nil err from GetItem flows through ddbToError into
// a typed *Error with a 5xx-class StatusCode. Handlers branch on
// the typed shape; a refactor that returned a bare error here would
// degrade the handler's "transient retry" routing.
func TestLookupChannelAlias_DDBError(t *testing.T) {
	ddb := &stubDDB{
		getItemFn: func(_ *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
			return nil, errors.New("ddb transport boom")
		},
	}
	rid, found, err := newStore(ddb).LookupChannelAlias(context.Background(), "T1", "C1", "prod-db")
	if rid != "" || found {
		t.Errorf("got (rid=%q, found=%v), want zero-valued returns on error", rid, found)
	}
	var ae *Error
	if !errors.As(err, &ae) {
		t.Fatalf("got %v, want *Error", err)
	}
	if ae.StatusCode < 500 || ae.StatusCode >= 600 {
		t.Errorf("StatusCode = %d, want 5xx (transport-class)", ae.StatusCode)
	}
}

func TestBindChannelAlias_WritesAliasMapEntry(t *testing.T) {
	var calls []*dynamodb.UpdateItemInput
	store := newStore(&stubDDB{
		updateItemFn: func(in *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
			calls = append(calls, in)
			return &dynamodb.UpdateItemOutput{}, nil
		},
	})
	if err := store.BindChannelAlias(context.Background(), "T1", "C1", "prod-dashboard", "r_prod_dash01"); err != nil {
		t.Fatalf("BindChannelAlias: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("UpdateItem calls = %d, want 2", len(calls))
	}
	if got := aws.ToString(calls[0].UpdateExpression); got != "SET #ab = :empty" {
		t.Errorf("seed UpdateExpression = %q", got)
	}
	if got := aws.ToString(calls[1].UpdateExpression); got != "SET #ab.#a = :rid" {
		t.Errorf("write UpdateExpression = %q", got)
	}
	if got := aws.ToString(calls[1].ConditionExpression); got != "attribute_not_exists(#ab.#a)" {
		t.Errorf("write ConditionExpression = %q", got)
	}
	if calls[1].ExpressionAttributeNames["#ab"] != attrAliasBindings || calls[1].ExpressionAttributeNames["#a"] != "prod-dashboard" {
		t.Errorf("ExpressionAttributeNames = %+v, want alias map key escaped", calls[1].ExpressionAttributeNames)
	}
	rid, ok := calls[1].ExpressionAttributeValues[":rid"].(*ddbtypes.AttributeValueMemberS)
	if !ok || rid.Value != "r_prod_dash01" {
		t.Errorf(":rid = %#v, want r_prod_dash01", calls[1].ExpressionAttributeValues[":rid"])
	}
}

func TestBindChannelAlias_DuplicateReturnsSentinel(t *testing.T) {
	var calls int
	store := newStore(&stubDDB{
		updateItemFn: func(_ *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
			calls++
			if calls == 2 {
				return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("exists")}
			}
			return &dynamodb.UpdateItemOutput{}, nil
		},
	})
	err := store.BindChannelAlias(context.Background(), "T1", "C1", "prod-dashboard", "r_prod_dash01")
	if !errors.Is(err, ErrAliasAlreadyBound) {
		t.Fatalf("err = %v, want ErrAliasAlreadyBound", err)
	}
}

func TestUnbindChannelAlias_NotFoundReturnsSentinel(t *testing.T) {
	store := newStore(&stubDDB{
		updateItemFn: func(_ *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
			return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("missing")}
		},
	})
	err := store.UnbindChannelAlias(context.Background(), "T1", "C1", "prod-dashboard")
	if !errors.Is(err, ErrAliasNotFound) {
		t.Fatalf("err = %v, want ErrAliasNotFound", err)
	}
}
