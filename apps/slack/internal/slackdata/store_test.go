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
		// can't rotate the workspace credential to their own Auth0.
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
