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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Slack-shaped test user IDs — kept in real Slack-ID shape (U-prefix
// + uppercase-alphanumeric, matching `looksLikeSlackUserID`) so these
// fixtures mirror production owner_id/admin values rather than ad-hoc
// strings. These tests assert at the store boundary (BindWorkspace
// classification), not the handler renderer, but matching the
// production shape keeps the fixtures honest and copy-safe. The paired
// fixtures (testCallerSlackID for the BindWorkspace caller,
// testOtherSlackID for a different Slack user holding the workspace)
// let a same-caller rebind assert AlreadyBoundToCaller while a
// different-caller rebind asserts AlreadyBound.
const (
	testCallerSlackID = "UCALLER01"
	testOtherSlackID  = "UOTHER001"
	testOwnerSlackID  = "UOWNER001"
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
//   - row exists, owner_id matches the caller's seedAdmin →
//     ErrCodeWorkspaceAlreadyBoundToCaller (idempotent rerun by owner)
//   - row exists, owner_id is a different Slack user (including added
//     admins) → ErrCodeWorkspaceAlreadyBound (refuse — owner-only
//     rebind, the rest of the admin verbs gate via admin_set instead)
//
// Drift either constant or invert the branch and the handler's
// user-copy will desynchronize from the actual workspace state.
func TestBindWorkspace_DistinguishesSameCallerFromDifferentAdmin(t *testing.T) {
	t.Run("owner reruns setup → idempotent", func(t *testing.T) {
		store := newStore(&stubDDB{
			putItemFn: func(_ *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
				return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("exists")}
			},
			getItemFn: func(_ *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				return &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
					attrSlackTeamID: &ddbtypes.AttributeValueMemberS{Value: "T"},
					attrOwnerID:     &ddbtypes.AttributeValueMemberS{Value: testCallerSlackID},
					attrAdminSlackUserIDs: &ddbtypes.AttributeValueMemberSS{
						Value: []string{testCallerSlackID, testOtherSlackID},
					},
				}}, nil
			},
		})
		err := store.BindWorkspace(context.Background(), &WorkspaceMapping{TeamID: "T", OwnerID: testCallerSlackID}, testCallerSlackID)
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

	t.Run("added admin reruns setup → refuse (owner-only rebind)", func(t *testing.T) {
		// Admin set contains the caller (they were /qurl admin add'd
		// after first bind), but they're NOT the owner. Pre-owner-gate
		// this returned AlreadyBoundToCaller (idempotent); post-gate
		// it must return AlreadyBound (refuse) so the added admin
		// can't re-point the workspace credential to their own Auth0.
		store := newStore(&stubDDB{
			putItemFn: func(_ *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
				return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("exists")}
			},
			getItemFn: func(_ *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				return &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
					attrSlackTeamID: &ddbtypes.AttributeValueMemberS{Value: "T"},
					attrOwnerID:     &ddbtypes.AttributeValueMemberS{Value: testOwnerSlackID},
					attrAdminSlackUserIDs: &ddbtypes.AttributeValueMemberSS{
						Value: []string{testOwnerSlackID, testCallerSlackID},
					},
				}}, nil
			},
		})
		err := store.BindWorkspace(context.Background(), &WorkspaceMapping{TeamID: "T", OwnerID: testCallerSlackID}, testCallerSlackID)
		var ae *Error
		if !errors.As(err, &ae) {
			t.Fatalf("got %v, want *Error", err)
		}
		if ae.Code != ErrCodeWorkspaceAlreadyBound {
			t.Errorf("Code = %q, want %q", ae.Code, ErrCodeWorkspaceAlreadyBound)
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
					attrOwnerID:     &ddbtypes.AttributeValueMemberS{Value: testOtherSlackID},
					attrAdminSlackUserIDs: &ddbtypes.AttributeValueMemberSS{
						Value: []string{testOtherSlackID},
					},
				}}, nil
			},
		})
		err := store.BindWorkspace(context.Background(), &WorkspaceMapping{TeamID: "T", OwnerID: testCallerSlackID}, testCallerSlackID)
		var ae *Error
		if !errors.As(err, &ae) {
			t.Fatalf("got %v, want *Error", err)
		}
		if ae.Code != ErrCodeWorkspaceAlreadyBound {
			t.Errorf("Code = %q, want %q", ae.Code, ErrCodeWorkspaceAlreadyBound)
		}
	})

	t.Run("row exists with empty owner_id → refuse (no hijack to AlreadyBoundToCaller)", func(t *testing.T) {
		// Operational-corruption guard: a manually edited / truncated row
		// can exist with no owner_id. The owner comparison must NOT treat
		// "" == "" as a same-owner match (which would hand the workspace to
		// any caller via AlreadyBoundToCaller). It must refuse with the
		// safe default, AlreadyBound. seedAdmin is always non-empty here,
		// so the empty existingOwner can never equal it.
		store := newStore(&stubDDB{
			putItemFn: func(_ *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
				return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("exists")}
			},
			getItemFn: func(_ *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				return &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
					attrSlackTeamID: &ddbtypes.AttributeValueMemberS{Value: "T"},
					// owner_id intentionally absent (corrupt row).
					attrAdminSlackUserIDs: &ddbtypes.AttributeValueMemberSS{
						Value: []string{testCallerSlackID},
					},
				}}, nil
			},
		})
		err := store.BindWorkspace(context.Background(), &WorkspaceMapping{TeamID: "T", OwnerID: testCallerSlackID}, testCallerSlackID)
		var ae *Error
		if !errors.As(err, &ae) {
			t.Fatalf("got %v, want *Error", err)
		}
		if ae.Code != ErrCodeWorkspaceAlreadyBound {
			t.Errorf("Code = %q, want %q (empty owner_id must not short-circuit to AlreadyBoundToCaller)", ae.Code, ErrCodeWorkspaceAlreadyBound)
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
					attrAdminSlackUserIDs: &ddbtypes.AttributeValueMemberSS{Value: []string{testOtherSlackID}},
				}}, nil
			},
		})
		_ = store.BindWorkspace(context.Background(), &WorkspaceMapping{TeamID: "T", OwnerID: "u_owner"}, testCallerSlackID)
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
		err := store.BindWorkspace(context.Background(), &WorkspaceMapping{TeamID: "T", OwnerID: "u_owner"}, testCallerSlackID)
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

// TestBindWorkspace_ReclaimsLegacyAuth0SubRow fences the self-heal path
// for a pre-pivot row whose owner_id is an Auth0 sub (not a Slack ID).
// No Slack user can ever match it, so the workspace would be permanently
// locked; BindWorkspace must reclaim the orphaned row for the caller via
// a compare-and-swap on the exact legacy owner_id.
func TestBindWorkspace_ReclaimsLegacyAuth0SubRow(t *testing.T) {
	const legacyAuth0Sub = "auth0|653fpre-pivot-subxyz"
	const legacyCreatedAt = "2026-05-01T00:00:00Z"

	t.Run("shape-bad owner_id is reclaimed for the caller", func(t *testing.T) {
		var putCalls int
		var reclaimPut *dynamodb.PutItemInput
		store := newStore(&stubDDB{
			putItemFn: func(in *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
				putCalls++
				if putCalls == 1 {
					// Initial conditional bind: the row already exists.
					return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("exists")}
				}
				// Second PutItem is the reclaim CAS.
				reclaimPut = in
				return &dynamodb.PutItemOutput{}, nil
			},
			getItemFn: func(_ *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				return &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
					attrSlackTeamID: &ddbtypes.AttributeValueMemberS{Value: "T"},
					attrOwnerID:     &ddbtypes.AttributeValueMemberS{Value: legacyAuth0Sub},
					attrCreatedAt:   &ddbtypes.AttributeValueMemberS{Value: legacyCreatedAt},
				}}, nil
			},
		})
		err := store.BindWorkspace(context.Background(), &WorkspaceMapping{TeamID: "T", OwnerID: testCallerSlackID}, testCallerSlackID)
		if err != nil {
			t.Fatalf("BindWorkspace reclaim: got %v, want nil (legacy row should be reclaimed)", err)
		}
		if putCalls != 2 {
			t.Fatalf("PutItem calls = %d, want 2 (initial conditional bind + reclaim CAS)", putCalls)
		}
		if reclaimPut == nil {
			t.Fatal("reclaim PutItem not captured")
		}
		// The reclaim must be a CAS on the exact legacy owner_id so a
		// concurrent reclaim can't double-write.
		if got := aws.ToString(reclaimPut.ConditionExpression); got != "owner_id = :legacy" {
			t.Errorf("reclaim ConditionExpression = %q, want %q", got, "owner_id = :legacy")
		}
		legacyVal, ok := reclaimPut.ExpressionAttributeValues[":legacy"].(*ddbtypes.AttributeValueMemberS)
		if !ok || legacyVal.Value != legacyAuth0Sub {
			t.Errorf("reclaim CAS :legacy = %+v, want %q", reclaimPut.ExpressionAttributeValues[":legacy"], legacyAuth0Sub)
		}
		// The reclaimed row's owner_id must be the caller's Slack ID.
		newOwner, ok := reclaimPut.Item[attrOwnerID].(*ddbtypes.AttributeValueMemberS)
		if !ok || newOwner.Value != testCallerSlackID {
			t.Errorf("reclaim new owner_id = %+v, want %q", reclaimPut.Item[attrOwnerID], testCallerSlackID)
		}
		// The orphaned row's original created_at must be preserved (the
		// durable "predates #510" signal), not overwritten with `now`.
		gotCreated, ok := reclaimPut.Item[attrCreatedAt].(*ddbtypes.AttributeValueMemberS)
		if !ok || gotCreated.Value != legacyCreatedAt {
			t.Errorf("reclaim created_at = %+v, want preserved %q", reclaimPut.Item[attrCreatedAt], legacyCreatedAt)
		}
	})

	t.Run("reclaim loses the race → AlreadyBound", func(t *testing.T) {
		var putCalls int
		store := newStore(&stubDDB{
			putItemFn: func(_ *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error) {
				putCalls++
				// Both the initial bind and the reclaim CAS hit a CCFE:
				// a concurrent caller already replaced the legacy owner_id
				// with a valid Slack owner.
				return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("exists")}
			},
			getItemFn: func(_ *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				return &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
					attrSlackTeamID: &ddbtypes.AttributeValueMemberS{Value: "T"},
					attrOwnerID:     &ddbtypes.AttributeValueMemberS{Value: legacyAuth0Sub},
				}}, nil
			},
		})
		err := store.BindWorkspace(context.Background(), &WorkspaceMapping{TeamID: "T", OwnerID: testCallerSlackID}, testCallerSlackID)
		var ae *Error
		if !errors.As(err, &ae) {
			t.Fatalf("got %v, want *Error", err)
		}
		if ae.Code != ErrCodeWorkspaceAlreadyBound {
			t.Errorf("Code = %q, want %q (lost reclaim race must refuse)", ae.Code, ErrCodeWorkspaceAlreadyBound)
		}
		if putCalls != 2 {
			t.Errorf("PutItem calls = %d, want 2 (initial bind + reclaim attempt)", putCalls)
		}
	})
}

// TestLooksLikeSlackUserID fences the shape predicate that both the
// handler (mention-surface guard) and BindWorkspace (legacy-reclaim
// detection) depend on — a pre-pivot Auth0 sub must read as invalid so
// the reclaim path fires, and real Slack IDs must read as valid so a
// healthy owner_id is never mistaken for a legacy one.
func TestLooksLikeSlackUserID(t *testing.T) {
	valid := []string{
		"UCALLER01", "WENTERPRISE01", "U012345678",
		"U" + strings.Repeat("A", 8),  // 9 chars — lower length bound.
		"U" + strings.Repeat("A", 63), // 64 chars — upper length bound.
	}
	invalid := []string{
		"", "auth0|653fpre-pivot-subxyz", "google-oauth2|123", "u012345678", "U12", "UABCDEF!1",
		"U" + strings.Repeat("A", 7),  // 8 chars — one under the lower bound.
		"U" + strings.Repeat("A", 64), // 65 chars — one over the upper bound.
	}
	for _, s := range valid {
		if !LooksLikeSlackUserID(s) {
			t.Errorf("LooksLikeSlackUserID(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if LooksLikeSlackUserID(s) {
			t.Errorf("LooksLikeSlackUserID(%q) = true, want false", s)
		}
	}
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
	err := store.BindWorkspace(context.Background(), &WorkspaceMapping{TeamID: "T", OwnerID: "u_owner"}, testCallerSlackID)
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

// TestCheckAdmin_OwnerIsAdminOffOwnerID fences the owner→admin self-heal:
// the owner is recorded in owner_id, but a legacy row or an idempotent
// setup rerun can leave them off admin_slack_user_ids. CheckAdmin must
// still report the owner as admin (off owner_id) so they keep every admin
// affordance — including the /qurl list Edit button — while a stranger who
// is neither owner nor on the admin set stays denied (no over-grant).
func TestCheckAdmin_OwnerIsAdminOffOwnerID(t *testing.T) {
	newStoreOwnerOffAdminSet := func() *Store {
		return newStore(&stubDDB{
			getItemFn: func(_ *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				return &dynamodb.GetItemOutput{Item: map[string]ddbtypes.AttributeValue{
					attrSlackTeamID:       &ddbtypes.AttributeValueMemberS{Value: "T"},
					attrOwnerID:           &ddbtypes.AttributeValueMemberS{Value: testOwnerSlackID},
					attrAdminSlackUserIDs: &ddbtypes.AttributeValueMemberSS{Value: []string{testOtherSlackID}},
				}}, nil
			},
		})
	}

	t.Run("owner absent from admin set is still admin", func(t *testing.T) {
		isAdmin, ownerID, err := newStoreOwnerOffAdminSet().CheckAdmin(context.Background(), "T", testOwnerSlackID)
		if err != nil {
			t.Fatalf("CheckAdmin err: %v", err)
		}
		if !isAdmin {
			t.Errorf("isAdmin = false for owner not on admin set, want true")
		}
		if ownerID != testOwnerSlackID {
			t.Errorf("ownerID = %q, want %q", ownerID, testOwnerSlackID)
		}
	})

	t.Run("stranger neither owner nor on admin set is denied", func(t *testing.T) {
		isAdmin, _, err := newStoreOwnerOffAdminSet().CheckAdmin(context.Background(), "T", testCallerSlackID)
		if err != nil {
			t.Fatalf("CheckAdmin err: %v", err)
		}
		if isAdmin {
			t.Errorf("isAdmin = true for non-owner, non-admin caller, want false (over-grant)")
		}
	})
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

// testTargetResourceID is the resource the ChannelsForResource tests search
// for; lifted to a constant to satisfy goconst.
const testTargetResourceID = "r_target"

// channelPolicyRow builds a channel_policies item for the ChannelsForResource
// tests: optional allowed_resource_ids SS and/or alias_bindings map, on
// (T1, channelID).
func channelPolicyRow(channelID string, allowed []string, aliasBindings map[string]string) map[string]ddbtypes.AttributeValue {
	item := map[string]ddbtypes.AttributeValue{
		attrSlackTeamID:    stringAttr("T1"),
		attrSlackChannelID: stringAttr(channelID),
	}
	if allowed != nil {
		item[attrAllowedResourceIDs] = &ddbtypes.AttributeValueMemberSS{Value: allowed}
	}
	if aliasBindings != nil {
		m := make(map[string]ddbtypes.AttributeValue, len(aliasBindings))
		for a, r := range aliasBindings {
			m[a] = stringAttr(r)
		}
		item[attrAliasBindings] = &ddbtypes.AttributeValueMemberM{Value: m}
	}
	return item
}

// TestExposeResourceToChannel_AddsToAllowedSet fences the expose write: a
// set-union `ADD allowed_resource_ids :rids` on the (team, channel) row so the
// grant is idempotent and materializes the row if absent.
func TestExposeResourceToChannel_AddsToAllowedSet(t *testing.T) {
	var got *dynamodb.UpdateItemInput
	store := newStore(&stubDDB{
		updateItemFn: func(in *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
			got = in
			return &dynamodb.UpdateItemOutput{}, nil
		},
	})
	if err := store.ExposeResourceToChannel(context.Background(), "T1", "C9", "r_target01"); err != nil {
		t.Fatalf("ExposeResourceToChannel: %v", err)
	}
	if got == nil {
		t.Fatal("UpdateItem not called")
	}
	if exp := aws.ToString(got.UpdateExpression); exp != "ADD allowed_resource_ids :rids" {
		t.Errorf("UpdateExpression = %q, want ADD allowed_resource_ids :rids", exp)
	}
	if tbl := aws.ToString(got.TableName); tbl != "cp" {
		t.Errorf("TableName = %q, want cp", tbl)
	}
	if rids, ok := got.ExpressionAttributeValues[":rids"].(*ddbtypes.AttributeValueMemberSS); !ok || len(rids.Value) != 1 || rids.Value[0] != "r_target01" {
		t.Errorf(":rids = %#v, want SS[r_target01]", got.ExpressionAttributeValues[":rids"])
	}
	if k, _ := got.Key[attrSlackTeamID].(*ddbtypes.AttributeValueMemberS); k == nil || k.Value != "T1" {
		t.Errorf("Key team = %#v, want T1", got.Key[attrSlackTeamID])
	}
	if k, _ := got.Key[attrSlackChannelID].(*ddbtypes.AttributeValueMemberS); k == nil || k.Value != "C9" {
		t.Errorf("Key channel = %#v, want C9", got.Key[attrSlackChannelID])
	}
}

// TestRevokeResourceFromChannel_DeletesFromAllowedSet fences the revoke write:
// a `DELETE allowed_resource_ids :rids` so removing a non-member (or the last
// member) is a harmless no-op at DDB.
func TestRevokeResourceFromChannel_DeletesFromAllowedSet(t *testing.T) {
	var got *dynamodb.UpdateItemInput
	store := newStore(&stubDDB{
		updateItemFn: func(in *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
			got = in
			return &dynamodb.UpdateItemOutput{}, nil
		},
	})
	if err := store.RevokeResourceFromChannel(context.Background(), "T1", "C9", "r_target01"); err != nil {
		t.Fatalf("RevokeResourceFromChannel: %v", err)
	}
	if got == nil {
		t.Fatal("UpdateItem not called")
	}
	if exp := aws.ToString(got.UpdateExpression); exp != "DELETE allowed_resource_ids :rids" {
		t.Errorf("UpdateExpression = %q, want DELETE allowed_resource_ids :rids", exp)
	}
	if rids, ok := got.ExpressionAttributeValues[":rids"].(*ddbtypes.AttributeValueMemberSS); !ok || len(rids.Value) != 1 || rids.Value[0] != "r_target01" {
		t.Errorf(":rids = %#v, want SS[r_target01]", got.ExpressionAttributeValues[":rids"])
	}
}

// TestExposeRevokeChannel_RejectEmptyArgs fences the bad-request guards: an
// empty team/channel/resource must fail before any write.
func TestExposeRevokeChannel_RejectEmptyArgs(t *testing.T) {
	store := newStore(&stubDDB{
		updateItemFn: func(*dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
			t.Fatal("UpdateItem must not be called on a bad-request guard")
			return &dynamodb.UpdateItemOutput{}, nil // unreachable after Fatal; non-nil to satisfy nilnil
		},
	})
	if err := store.ExposeResourceToChannel(context.Background(), "", "C1", "r1"); err == nil {
		t.Error("ExposeResourceToChannel(empty team): want error")
	}
	if err := store.ExposeResourceToChannel(context.Background(), "T1", "C1", ""); err == nil {
		t.Error("ExposeResourceToChannel(empty resource): want error")
	}
	if err := store.RevokeResourceFromChannel(context.Background(), "T1", "", "r1"); err == nil {
		t.Error("RevokeResourceFromChannel(empty channel): want error")
	}
}

// TestChannelsForResource_UnionSortAndQueryShape fences the enumeration: it
// Queries the partition key and returns every channel whose row makes the
// resource available via EITHER surface (allowed_resource_ids SS or
// alias_bindings values), sorted, excluding unrelated channels.
func TestChannelsForResource_UnionSortAndQueryShape(t *testing.T) {
	var got *dynamodb.QueryInput
	store := newStore(&stubDDB{
		queryFn: func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
			got = in
			return &dynamodb.QueryOutput{Items: []map[string]ddbtypes.AttributeValue{
				channelPolicyRow("C_set", []string{testTargetResourceID, "r_other"}, nil),         // via allowed_resource_ids
				channelPolicyRow("C_alias", nil, map[string]string{"dash": testTargetResourceID}), // via alias binding
				channelPolicyRow("C_unrelated", []string{"r_nope"}, nil),                          // neither
			}}, nil
		},
	})
	channels, err := store.ChannelsForResource(context.Background(), "T1", testTargetResourceID)
	if err != nil {
		t.Fatalf("ChannelsForResource: %v", err)
	}
	if want := []string{"C_alias", "C_set"}; !reflect.DeepEqual(channels, want) {
		t.Errorf("channels = %v, want %v (union of SS + alias values, sorted, unrelated excluded)", channels, want)
	}
	if exp := aws.ToString(got.KeyConditionExpression); exp != "slack_team_id = :tid" {
		t.Errorf("KeyConditionExpression = %q, want slack_team_id = :tid", exp)
	}
	if tid, _ := got.ExpressionAttributeValues[":tid"].(*ddbtypes.AttributeValueMemberS); tid == nil || tid.Value != "T1" {
		t.Errorf(":tid = %#v, want T1", got.ExpressionAttributeValues[":tid"])
	}
	if tbl := aws.ToString(got.TableName); tbl != "cp" {
		t.Errorf("TableName = %q, want cp", tbl)
	}
}

// TestChannelsForResource_PagesAllResults fences the LastEvaluatedKey paging
// loop: results spanning two pages are all returned, and the second Query
// carries the first page's LastEvaluatedKey as its ExclusiveStartKey.
func TestChannelsForResource_PagesAllResults(t *testing.T) {
	page := 0
	store := newStore(&stubDDB{
		queryFn: func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
			page++
			if page == 1 {
				if in.ExclusiveStartKey != nil {
					t.Errorf("page 1 ExclusiveStartKey = %v, want nil", in.ExclusiveStartKey)
				}
				return &dynamodb.QueryOutput{
					Items:            []map[string]ddbtypes.AttributeValue{channelPolicyRow("C_p1", []string{testTargetResourceID}, nil)},
					LastEvaluatedKey: map[string]ddbtypes.AttributeValue{attrSlackChannelID: stringAttr("C_p1")},
				}, nil
			}
			if len(in.ExclusiveStartKey) == 0 {
				t.Errorf("page 2 ExclusiveStartKey is empty, want the prior LastEvaluatedKey")
			}
			return &dynamodb.QueryOutput{
				Items: []map[string]ddbtypes.AttributeValue{channelPolicyRow("C_p2", []string{testTargetResourceID}, nil)},
			}, nil
		},
	})
	channels, err := store.ChannelsForResource(context.Background(), "T1", testTargetResourceID)
	if err != nil {
		t.Fatalf("ChannelsForResource: %v", err)
	}
	if want := []string{"C_p1", "C_p2"}; !reflect.DeepEqual(channels, want) {
		t.Errorf("channels = %v, want %v (both pages)", channels, want)
	}
	if page != 2 {
		t.Errorf("query pages = %d, want 2", page)
	}
}

// TestChannelsForResource_QueryErrorSurfaces fences the failure paths: a Query
// error (e.g. a missing dynamodb:Query grant surfacing as AccessDenied) and an
// empty team both return an error so the caller can degrade.
func TestChannelsForResource_QueryErrorSurfaces(t *testing.T) {
	store := newStore(&stubDDB{
		queryFn: func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
			return nil, errors.New("AccessDenied: not authorized to perform dynamodb:Query")
		},
	})
	if _, err := store.ChannelsForResource(context.Background(), "T1", testTargetResourceID); err == nil {
		t.Error("ChannelsForResource: want error when Query fails")
	}
	if _, err := store.ChannelsForResource(context.Background(), "", testTargetResourceID); err == nil {
		t.Error("ChannelsForResource(empty team): want bad-request error")
	}
}

// TestPurgeResourceFromChannel fences the revoke/delete cascade verb. It must
// clear EVERY reference to the resource from the channel_policies row — DELETE
// the id from allowed_resource_ids AND REMOVE only the alias_bindings keys that
// point at it (leaving unrelated aliases intact) — and must NOT materialize a
// row for a channel that doesn't exist. This is the integrity guarantee behind
// the orphaned-`$alias` fix: a revoked resource must not leave its slug alias
// "bound" with nothing behind it.
func TestPurgeResourceFromChannel(t *testing.T) {
	const (
		deadID     = "r_dead0001"
		liveID     = "r_live0001"
		staleAlias = "dashboard"
		keepAlias  = "keepme"
	)
	dualRow := func() map[string]ddbtypes.AttributeValue {
		return map[string]ddbtypes.AttributeValue{
			attrSlackTeamID:    &ddbtypes.AttributeValueMemberS{Value: "T1"},
			attrSlackChannelID: &ddbtypes.AttributeValueMemberS{Value: "C1"},
			attrAliasBindings: &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{
				staleAlias: &ddbtypes.AttributeValueMemberS{Value: deadID},
				keepAlias:  &ddbtypes.AttributeValueMemberS{Value: liveID},
			}},
			attrAllowedResourceIDs: &ddbtypes.AttributeValueMemberSS{Value: []string{deadID, liveID}},
		}
	}

	t.Run("removes only the matching alias and the SS member", func(t *testing.T) {
		var captured *dynamodb.UpdateItemInput
		store := newStore(&stubDDB{
			getItemFn: func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				return &dynamodb.GetItemOutput{Item: dualRow()}, nil
			},
			updateItemFn: func(in *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
				captured = in
				return &dynamodb.UpdateItemOutput{}, nil
			},
		})

		unbound, err := store.PurgeResourceFromChannel(context.Background(), "T1", "C1", deadID)
		if err != nil {
			t.Fatalf("PurgeResourceFromChannel: %v", err)
		}
		if !reflect.DeepEqual(unbound, []string{staleAlias}) {
			t.Errorf("unbound = %v, want [%q]", unbound, staleAlias)
		}
		if captured == nil {
			t.Fatal("no UpdateItem issued")
		}
		expr := aws.ToString(captured.UpdateExpression)
		if !strings.Contains(expr, "DELETE "+attrAllowedResourceIDs+" :rid") {
			t.Errorf("UpdateExpression missing SS DELETE of the revoked id: %q", expr)
		}
		if !strings.Contains(expr, "REMOVE") {
			t.Errorf("UpdateExpression missing alias REMOVE: %q", expr)
		}
		ss, ok := captured.ExpressionAttributeValues[":rid"].(*ddbtypes.AttributeValueMemberSS)
		if !ok || !reflect.DeepEqual(ss.Value, []string{deadID}) {
			t.Errorf(":rid = %#v, want string-set [%q]", captured.ExpressionAttributeValues[":rid"], deadID)
		}
		// Every name ref other than the alias_bindings attr resolves to an alias
		// key slated for REMOVE — it must be the STALE alias, never the survivor.
		var removed []string
		for ref, name := range captured.ExpressionAttributeNames {
			if ref == exprAliasBindings {
				continue
			}
			removed = append(removed, name)
		}
		if !reflect.DeepEqual(removed, []string{staleAlias}) {
			t.Errorf("REMOVE targets %v, want [%q] (the unrelated alias %q must survive)", removed, staleAlias, keepAlias)
		}
	})

	t.Run("DELETE-only when no alias points at the resource", func(t *testing.T) {
		row := dualRow()
		// Only keepAlias→liveID remains; the revoked id lives solely in the SS.
		row[attrAliasBindings] = &ddbtypes.AttributeValueMemberM{Value: map[string]ddbtypes.AttributeValue{
			keepAlias: &ddbtypes.AttributeValueMemberS{Value: liveID},
		}}
		var captured *dynamodb.UpdateItemInput
		store := newStore(&stubDDB{
			getItemFn: func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				return &dynamodb.GetItemOutput{Item: row}, nil
			},
			updateItemFn: func(in *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
				captured = in
				return &dynamodb.UpdateItemOutput{}, nil
			},
		})
		unbound, err := store.PurgeResourceFromChannel(context.Background(), "T1", "C1", deadID)
		if err != nil {
			t.Fatalf("PurgeResourceFromChannel: %v", err)
		}
		if len(unbound) != 0 {
			t.Errorf("unbound = %v, want empty", unbound)
		}
		if captured == nil {
			t.Fatal("no UpdateItem issued (the SS member must still be cleared)")
		}
		if expr := aws.ToString(captured.UpdateExpression); strings.Contains(expr, "REMOVE") {
			t.Errorf("UpdateExpression should be DELETE-only, got %q", expr)
		}
		if len(captured.ExpressionAttributeNames) != 0 {
			t.Errorf("DELETE-only update should carry no name refs, got %v", captured.ExpressionAttributeNames)
		}
	})

	t.Run("absent row issues no UpdateItem", func(t *testing.T) {
		updateCalled := false
		store := newStore(&stubDDB{
			getItemFn: func(*dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				return &dynamodb.GetItemOutput{}, nil // no Item → row absent
			},
			updateItemFn: func(*dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
				updateCalled = true
				return &dynamodb.UpdateItemOutput{}, nil
			},
		})
		unbound, err := store.PurgeResourceFromChannel(context.Background(), "T1", "C1", deadID)
		if err != nil {
			t.Fatalf("PurgeResourceFromChannel: %v", err)
		}
		if unbound != nil {
			t.Errorf("unbound = %v, want nil for an absent row", unbound)
		}
		if updateCalled {
			t.Error("an absent row must not issue an UpdateItem (it would materialize an empty row)")
		}
	})

	t.Run("missing args rejected", func(t *testing.T) {
		store := newStore(&stubDDB{})
		for _, args := range [][3]string{{"", "C1", deadID}, {"T1", "", deadID}, {"T1", "C1", ""}} {
			if _, err := store.PurgeResourceFromChannel(context.Background(), args[0], args[1], args[2]); err == nil {
				t.Errorf("PurgeResourceFromChannel(%q,%q,%q): want bad-request error", args[0], args[1], args[2])
			}
		}
	})
}

// TestDeleteWorkspaceMapping fences the workspace-forget half of the
// Slack-lifecycle / `/qurl uninstall` cascade: a single unconditional DeleteItem
// keyed by team_id, idempotent on an absent row, and a 400 on an empty team_id
// before any DDB call.
func TestDeleteWorkspaceMapping(t *testing.T) {
	t.Run("deletes the row by team_id", func(t *testing.T) {
		var captured *dynamodb.DeleteItemInput
		store := newStore(&stubDDB{
			deleteItemFn: func(in *dynamodb.DeleteItemInput) (*dynamodb.DeleteItemOutput, error) {
				captured = in
				return &dynamodb.DeleteItemOutput{}, nil
			},
		})
		if err := store.DeleteWorkspaceMapping(context.Background(), "T1"); err != nil {
			t.Fatalf("DeleteWorkspaceMapping: %v", err)
		}
		if captured == nil {
			t.Fatal("no DeleteItem issued")
		}
		if got := aws.ToString(captured.TableName); got != "ws" {
			t.Errorf("DeleteItem table = %q, want %q", got, "ws")
		}
		if v, ok := captured.Key[attrSlackTeamID].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != "T1" {
			t.Errorf("DeleteItem key = %v, want slack_team_id=T1", captured.Key)
		}
		// Idempotent forget: unconditional delete (no ConditionExpression) so an
		// absent row is a no-op rather than a ConditionalCheckFailed.
		if captured.ConditionExpression != nil {
			t.Errorf("ConditionExpression = %q, want none", aws.ToString(captured.ConditionExpression))
		}
	})

	t.Run("absent row is a no-op", func(t *testing.T) {
		// stubDDB's default DeleteItem returns success with no state — the same
		// no-op DynamoDB performs for a missing key. A nil error proves the method
		// does not synthesize a not-found.
		store := newStore(&stubDDB{})
		if err := store.DeleteWorkspaceMapping(context.Background(), "T_absent"); err != nil {
			t.Fatalf("DeleteWorkspaceMapping on absent row: %v, want nil", err)
		}
	})

	t.Run("empty team_id rejected before DDB", func(t *testing.T) {
		called := false
		store := newStore(&stubDDB{
			deleteItemFn: func(*dynamodb.DeleteItemInput) (*dynamodb.DeleteItemOutput, error) {
				called = true
				return &dynamodb.DeleteItemOutput{}, nil
			},
		})
		err := store.DeleteWorkspaceMapping(context.Background(), "")
		var ae *Error
		if !errors.As(err, &ae) || ae.StatusCode != http.StatusBadRequest {
			t.Fatalf("DeleteWorkspaceMapping(\"\") err = %v, want 400 *Error", err)
		}
		if called {
			t.Error("must reject empty team_id before issuing DeleteItem")
		}
	})

	t.Run("transport error maps to 503", func(t *testing.T) {
		store := newStore(&stubDDB{
			deleteItemFn: func(*dynamodb.DeleteItemInput) (*dynamodb.DeleteItemOutput, error) {
				return nil, errors.New("ddb down")
			},
		})
		err := store.DeleteWorkspaceMapping(context.Background(), "T1")
		var ae *Error
		if !errors.As(err, &ae) || ae.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("DeleteWorkspaceMapping transport err = %v, want 503 *Error", err)
		}
	})
}

// TestPurgeTeamChannelPolicies fences the per-channel-policy half of the
// Slack-lifecycle / `/qurl uninstall` cascade: Query every row for the team
// (paging the LastEvaluatedKey loop) and DeleteItem each by its (team, channel)
// key. It must delete EVERY page's rows, address each by the queried SK, tolerate
// an empty team (nothing to delete), reject an empty team_id, and surface a Query
// error so the caller can decide to retry.
func TestPurgeTeamChannelPolicies(t *testing.T) {
	t.Run("deletes every row across pages", func(t *testing.T) {
		page := 0
		var deletedChannels []string
		store := newStore(&stubDDB{
			queryFn: func(in *dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
				if got := aws.ToString(in.TableName); got != "cp" {
					t.Errorf("Query table = %q, want %q", got, "cp")
				}
				if v, ok := in.ExpressionAttributeValues[":tid"].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != "T1" {
					t.Errorf("Query :tid = %v, want T1", in.ExpressionAttributeValues[":tid"])
				}
				page++
				if page == 1 {
					if in.ExclusiveStartKey != nil {
						t.Errorf("page 1 ExclusiveStartKey = %v, want nil", in.ExclusiveStartKey)
					}
					return &dynamodb.QueryOutput{
						Items:            []map[string]ddbtypes.AttributeValue{channelPolicyRow("C_p1", []string{"r1"}, nil)},
						LastEvaluatedKey: map[string]ddbtypes.AttributeValue{attrSlackChannelID: stringAttr("C_p1")},
					}, nil
				}
				if len(in.ExclusiveStartKey) == 0 {
					t.Errorf("page 2 ExclusiveStartKey is empty, want the prior LastEvaluatedKey")
				}
				return &dynamodb.QueryOutput{
					Items: []map[string]ddbtypes.AttributeValue{channelPolicyRow("C_p2", nil, map[string]string{"a": "r2"})},
				}, nil
			},
			deleteItemFn: func(in *dynamodb.DeleteItemInput) (*dynamodb.DeleteItemOutput, error) {
				if got := aws.ToString(in.TableName); got != "cp" {
					t.Errorf("DeleteItem table = %q, want %q", got, "cp")
				}
				if v, ok := in.Key[attrSlackTeamID].(*ddbtypes.AttributeValueMemberS); !ok || v.Value != "T1" {
					t.Errorf("DeleteItem PK = %v, want slack_team_id=T1", in.Key)
				}
				if in.ConditionExpression != nil {
					t.Errorf("DeleteItem ConditionExpression = %q, want none (idempotent)", aws.ToString(in.ConditionExpression))
				}
				cid := in.Key[attrSlackChannelID].(*ddbtypes.AttributeValueMemberS).Value
				deletedChannels = append(deletedChannels, cid)
				return &dynamodb.DeleteItemOutput{}, nil
			},
		})
		if err := store.PurgeTeamChannelPolicies(context.Background(), "T1"); err != nil {
			t.Fatalf("PurgeTeamChannelPolicies: %v", err)
		}
		if page != 2 {
			t.Errorf("query pages = %d, want 2", page)
		}
		if want := []string{"C_p1", "C_p2"}; !reflect.DeepEqual(deletedChannels, want) {
			t.Errorf("deleted channels = %v, want %v", deletedChannels, want)
		}
	})

	t.Run("empty team deletes nothing", func(t *testing.T) {
		deleteCalled := false
		store := newStore(&stubDDB{
			queryFn: func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
				return &dynamodb.QueryOutput{}, nil
			},
			deleteItemFn: func(*dynamodb.DeleteItemInput) (*dynamodb.DeleteItemOutput, error) {
				deleteCalled = true
				return &dynamodb.DeleteItemOutput{}, nil
			},
		})
		if err := store.PurgeTeamChannelPolicies(context.Background(), "T1"); err != nil {
			t.Fatalf("PurgeTeamChannelPolicies(empty team): %v", err)
		}
		if deleteCalled {
			t.Error("no rows queried — DeleteItem must not be called")
		}
	})

	t.Run("empty team_id rejected before DDB", func(t *testing.T) {
		queried := false
		store := newStore(&stubDDB{
			queryFn: func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
				queried = true
				return &dynamodb.QueryOutput{}, nil
			},
		})
		err := store.PurgeTeamChannelPolicies(context.Background(), "")
		var ae *Error
		if !errors.As(err, &ae) || ae.StatusCode != http.StatusBadRequest {
			t.Fatalf("PurgeTeamChannelPolicies(\"\") err = %v, want 400 *Error", err)
		}
		if queried {
			t.Error("must reject empty team_id before issuing Query")
		}
	})

	t.Run("query error surfaces", func(t *testing.T) {
		store := newStore(&stubDDB{
			queryFn: func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
				return nil, errors.New("AccessDenied: not authorized to perform dynamodb:Query")
			},
		})
		if err := store.PurgeTeamChannelPolicies(context.Background(), "T1"); err == nil {
			t.Error("PurgeTeamChannelPolicies: want error when Query fails")
		}
	})

	t.Run("delete error surfaces", func(t *testing.T) {
		store := newStore(&stubDDB{
			queryFn: func(*dynamodb.QueryInput) (*dynamodb.QueryOutput, error) {
				return &dynamodb.QueryOutput{
					Items: []map[string]ddbtypes.AttributeValue{channelPolicyRow("C1", []string{"r1"}, nil)},
				}, nil
			},
			deleteItemFn: func(*dynamodb.DeleteItemInput) (*dynamodb.DeleteItemOutput, error) {
				return nil, errors.New("ddb delete down")
			},
		})
		if err := store.PurgeTeamChannelPolicies(context.Background(), "T1"); err == nil {
			t.Error("PurgeTeamChannelPolicies: want error when DeleteItem fails")
		}
	})
}
