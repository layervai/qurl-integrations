package slackdata

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Attribute names on the bootstrap_codes table. PK=code_hash (sha256
// of the plaintext code received from the modal). Carries:
//   - key_id (the qurl-service API key the binding will be paired with)
//   - owner_id (Auth0 sub of the qurl owner that minted the code)
//   - redeemed (bool: true once consumed)
//   - expires_at (epoch seconds, used as DDB TTL)
//
// The hashing means the plaintext code never lives at rest — see the
// "Security contract for the redemption path" comment in
// modules/qurl-slack-ddb/main.tf.
// attrCodeHash is the bootstrap_codes PK (sha256 of the plaintext code
// received from the modal). All other attributes are referenced via
// string literals inside the UpdateExpression — DDB requires
// expression-attribute aliases for reserved words anyway, and there's
// no second call-site for the other names to drift from.
const attrCodeHash = "code_hash"

// ErrCodeBootstrapInvalid is the error code surfaced on the *Error
// returned from RedeemBootstrap when the bootstrap code is
// wrong/expired/already-used. Exported so handlers can pattern-match
// the [*Error.Code] field without redeclaring the literal — a
// future rename would break the type-checker rather than silently
// desynchronizing.
const ErrCodeBootstrapInvalid = "bootstrap_code_invalid"

// RedeemBootstrap consumes a one-time bootstrap code, atomically
// flipping the row's `redeemed` flag from false → true via a DDB
// conditional UpdateItem. If the conditional check fails (code
// already used, code doesn't exist, or expires_at is in the past)
// the handler surfaces the "code invalid or expired" message.
//
// Two-step flow (per Appendix A of SLACK_QURL_ROLLOUT.md):
//  1. Atomically consume the code on the bot-owned bootstrap_codes
//     table (this method).
//  2. Call qurl-service `POST /v1/external-identity-bindings`
//     (s.ExternalIdentityBindings, if wired) to mint the API key
//     and record the (provider=slack, external_id=team_id) binding
//     against the owner that minted the bootstrap code.
//
// Step 2 is gated on the s.ExternalIdentityBindings field — that
// endpoint doesn't exist in qurl-service yet. When the field is nil
// (production today), step 2 is skipped and the method returns the
// row data from the consumed bootstrap_codes row, leaving the
// workspace-mapping write to a follow-up.
//
// TTL note: DDB TTL is best-effort (~48h lag). The expires_at
// comparison MUST happen here at read time — relying on TTL to
// gate access would let an expired-but-not-yet-swept code redeem.
func (s *Store) RedeemBootstrap(ctx context.Context, code, teamID, slackUserID string) (*WorkspaceMapping, error) {
	if code == "" || teamID == "" || slackUserID == "" {
		return nil, &Error{
			StatusCode: http.StatusBadRequest,
			Title:      "RedeemBootstrap: code, team_id, user_id are required",
		}
	}
	codeHash := hashBootstrapCode(code)
	nowEpoch := s.nowOrDefault().Unix()

	// Conditional UpdateItem:
	//   - attribute_exists(code_hash)    → row must exist
	//   - redeemed = :false              → not already consumed
	//   - expires_at > :now              → not expired
	// All three folded into one ConditionExpression so a single
	// failed-condition surfaces as one "code invalid or expired"
	// signal rather than three branches.
	out, err := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.BootstrapCodesName),
		Key: map[string]ddbtypes.AttributeValue{
			attrCodeHash: stringAttr(codeHash),
		},
		UpdateExpression: aws.String("SET redeemed = :true, redeemed_by = :user, redeemed_at = :now_iso"),
		ConditionExpression: aws.String(
			"attribute_exists(code_hash) AND redeemed = :false AND expires_at > :now",
		),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":true":    boolAttr(true),
			":false":   boolAttr(false),
			exprNow:    &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(nowEpoch, 10)},
			":now_iso": stringAttr(s.nowOrDefault().UTC().Format(time.RFC3339)),
			":user":    stringAttr(slackUserID),
		},
		ReturnValues: ddbtypes.ReturnValueAllNew,
	})
	if err != nil {
		var ccfe *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &ccfe) {
			return nil, &Error{
				StatusCode: http.StatusGone,
				Code:       ErrCodeBootstrapInvalid,
				Title:      "RedeemBootstrap: code is invalid, expired, or already used",
			}
		}
		return nil, ddbToError("RedeemBootstrap", err)
	}

	ownerID := readString(out.Attributes, attrOwnerID)
	// CreatedAt is "when the workspace was claimed" — the moment
	// the bootstrap code was redeemed — NOT the time the bootstrap
	// code was originally minted. BindWorkspace propagates this onto
	// the workspace_mappings row's `created_at` attribute so the row
	// timestamps reflect first-claim time. The bootstrap_codes row's
	// own minted-at lives on a separate column and isn't surfaced.
	mapping := &WorkspaceMapping{
		TeamID:    teamID,
		OwnerID:   ownerID,
		CreatedAt: s.nowOrDefault().UTC(),
	}

	// Step 2 (call qurl-service POST /v1/external-identity-bindings
	// to mint the API key + record the binding) is deferred until
	// qurl-service #547 ships the endpoint AND this bot plumbs the
	// Auth0 Bearer through to the call site. The Store field
	// `ExternalIdentityBindings` stays nil in cmd/main.go on
	// purpose — calling Create() with an empty Bearer would burn
	// this bootstrap code and then fail at the HTTP layer ("Bearer
	// is required"), leaving the user stuck with a dead code and
	// no usable workspace binding.
	//
	// The follow-up PR that flips the field non-nil lands the
	// Bearer plumbing in the same change so the two-step flow
	// stays atomic from the user's perspective. The interface
	// shape lives in external_identity_bindings.go so the call-
	// site signature is stable.
	return mapping, nil
}

// MinBootstrapPlaintextLen is the runtime tripwire on plaintext
// length passed to [hashBootstrapCode]. The production issuer mints
// codes ≥16 chars over the base32 alphabet (≥80 bits of CSPRNG
// entropy — the rainbow-table-resistance floor the unsalted-sha256
// posture relies on). The tripwire is set at 10 rather than 16 so
// the bot does not gate against the production floor it doesn't own
// — it catches the regression class the cr flagged (a 6-digit OTP
// or similar low-entropy code shape) and lets the issuer raise its
// own floor independently. Tests use short, readable plaintexts
// (e.g. "BOOT-VALID") that stay above this floor.
const MinBootstrapPlaintextLen = 10

// hashBootstrapCode is sha256-hex of the plaintext bootstrap code.
// Matches the schema fenced in modules/qurl-slack-ddb/main.tf:
// `code_hash = SHA-256(plaintext)`; plaintext is never persisted.
//
// Salting is intentionally omitted: the security posture relies on
// the plaintext code itself carrying ≥80 bits of CSPRNG entropy
// (minted by the bootstrap-code issuer, NOT by this bot). At that
// entropy floor the hash is rainbow-table-resistant on its own.
// Plaintext length is asserted at runtime (see
// [MinBootstrapPlaintextLen]) so an issuer-side regression that
// shrinks the entropy floor fails fast rather than silently
// weakening the hash.
//
// If a future refactor swaps in a lower-entropy code shape (e.g. a
// 6-digit OTP) the tripwire panics and the change cannot land
// without ALSO updating this file — the comment-only fence is not
// enough on its own.
func hashBootstrapCode(plaintext string) string {
	if len(plaintext) < MinBootstrapPlaintextLen {
		// Panic rather than return a sentinel: this is a programmer/
		// rotation error, not a user-input failure. The only call
		// site is RedeemBootstrap, which has already validated
		// `code != ""` upstream; reaching here with a short plaintext
		// means the issuer-side contract has drifted.
		panic("hashBootstrapCode: plaintext shorter than MinBootstrapPlaintextLen — entropy floor would silently weaken")
	}
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}
