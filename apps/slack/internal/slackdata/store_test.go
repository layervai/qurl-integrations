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
	"fmt"
	"net/http"
	"strings"
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
		BootstrapCodesName:    "bc",
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
			e:    Error{StatusCode: 410, Code: ErrCodeBootstrapInvalid, Title: "RedeemBootstrap: code is invalid, expired, or already used"},
			want: "RedeemBootstrap: code is invalid, expired, or already used [bootstrap_code_invalid] (410)",
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

	// Also fence the degenerate GetItem-fails fallthrough: even if
	// the disambiguating Get fails, the function still returns a
	// 409 (rather than a 503 from the failed Get) — the binding is
	// still held, just by an unknown admin.
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
			t.Errorf("StatusCode = %d, want 409 (binding is still held; race only affects message variant)", ae.StatusCode)
		}
		if ae.Code != ErrCodeWorkspaceAlreadyBound {
			t.Errorf("Code = %q, want %q (default to different-admin variant when caller-id check is uncertain)", ae.Code, ErrCodeWorkspaceAlreadyBound)
		}
	})
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

// TestRedeemBootstrap_ConditionalCheckFailedMapsToGone fences the
// "code is invalid, expired, or already used" mapping. The handler
// branches on `ae.Code == ErrCodeBootstrapInvalid` to render the
// retry-friendly copy; drift the Code constant and the user sees a
// generic "could not redeem code" instead.
func TestRedeemBootstrap_ConditionalCheckFailedMapsToGone(t *testing.T) {
	store := newStore(&stubDDB{
		updateItemFn: func(_ *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
			return nil, &ddbtypes.ConditionalCheckFailedException{Message: aws.String("expired or used")}
		},
	})
	_, err := store.RedeemBootstrap(context.Background(), "BOOT-VALID-CODE", "T", "U_caller")
	var ae *Error
	if !errors.As(err, &ae) {
		t.Fatalf("got %v, want *Error", err)
	}
	if ae.StatusCode != http.StatusGone {
		t.Errorf("StatusCode = %d, want 410", ae.StatusCode)
	}
	if ae.Code != ErrCodeBootstrapInvalid {
		t.Errorf("Code = %q, want %q", ae.Code, ErrCodeBootstrapInvalid)
	}
}

// TestRedeemBootstrap_ValidationGuards fences the input-validation
// 400 surface: empty code / team_id / user_id bail before touching
// DDB.
func TestRedeemBootstrap_ValidationGuards(t *testing.T) {
	cases := []struct {
		name             string
		code, team, user string
	}{
		{"empty code", "", "T", "U"},
		{"empty team_id", "BOOT-VALID-CODE", "", "U"},
		{"empty user_id", "BOOT-VALID-CODE", "T", ""},
	}
	store := newStore(&stubDDB{
		updateItemFn: func(_ *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
			t.Fatalf("UpdateItem must not be called when validation rejects upstream")
			return nil, errors.New("unreachable")
		},
	})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := store.RedeemBootstrap(context.Background(), c.code, c.team, c.user)
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

// TestHashBootstrapCode_RejectsShortPlaintext fences the runtime
// tripwire added in round-15. The unsalted-sha256 posture depends on
// the issuer mint shape carrying ≥80 bits of entropy; a regression
// at the issuer side (e.g. swapping in a 6-digit OTP) silently
// weakens the hash unless this panic fires. The plaintext length
// floor is intentionally lower than the production floor — see
// MinBootstrapPlaintextLen's doc.
func TestHashBootstrapCode_RejectsShortPlaintext(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("hashBootstrapCode did not panic on short plaintext")
		}
		if !strings.Contains(fmt.Sprint(r), "entropy floor") {
			t.Errorf("panic message did not mention entropy floor: %q", r)
		}
	}()
	// 6-digit OTP — the regression class the cr called out.
	_ = hashBootstrapCode("123456")
}

// TestHashBootstrapCode_AcceptsAtAndAboveFloor sanity-checks the
// boundary: exactly MinBootstrapPlaintextLen chars must not panic.
func TestHashBootstrapCode_AcceptsAtAndAboveFloor(t *testing.T) {
	plain := strings.Repeat("a", MinBootstrapPlaintextLen)
	hashBootstrapCode(plain) // must not panic
	// And one over the floor — sanity.
	hashBootstrapCode(plain + "b")
}
