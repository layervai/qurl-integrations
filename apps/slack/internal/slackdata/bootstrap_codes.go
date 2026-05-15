package slackdata

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
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
const (
	attrCodeHash  = "code_hash"
	attrKeyID     = "key_id"
	attrRedeemed  = "redeemed"
	attrExpiresAt = "expires_at"
)

// errCodeBootstrapInvalid is the error code surfaced when the
// bootstrap code is wrong/expired/already-used. The handler maps this
// to the user-facing "code is invalid or expired" copy (see
// handler_admin_claim.go's surfaceClaimError).
const errCodeBootstrapInvalid = "bootstrap_code_invalid"

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
			exprNow:    &ddbtypes.AttributeValueMemberN{Value: epochSecondsString(nowEpoch)},
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
				Code:       errCodeBootstrapInvalid,
				Title:      "RedeemBootstrap: code is invalid, expired, or already used",
			}
		}
		return nil, ddbToError("RedeemBootstrap", err)
	}

	ownerID := readString(out.Attributes, attrOwnerID)
	keyID := readString(out.Attributes, attrKeyID)
	mapping := &WorkspaceMapping{
		TeamID:    teamID,
		OwnerID:   ownerID,
		CreatedAt: s.nowOrDefault().UTC(),
	}

	// Step 2: call qurl-service POST /v1/external-identity-bindings
	// to mint the API key and record the binding. The endpoint
	// doesn't exist yet (see Appendix A of SLACK_QURL_ROLLOUT.md);
	// when ExternalIdentityBindings is nil we return mapping as-is
	// and leave the binding/key-mint to a follow-up.
	//
	// TODO: implement once qurl-service ships the endpoint
	if s.ExternalIdentityBindings != nil {
		_, bindErr := s.ExternalIdentityBindings.Create(ctx, &CreateBindingRequest{
			Provider:    "slack",
			ExternalID:  teamID,
			DisplayName: "", // optional; filled in by a follow-up
			Bearer:      "", // Auth0 JWT plumbed via context in caller
		})
		if bindErr != nil {
			// External binding failed AFTER the local bootstrap-code
			// consume — we've burned the one-time code with nothing
			// to show for it. Surface the error but leave the
			// consume in place (DDB doesn't support multi-table
			// rollback without TransactWriteItems against the
			// remote table, which lives in qurl-service's account).
			// Operators will need to mint a fresh code; tracked as
			// an open question in Appendix A.
			return nil, bindErr
		}
	}

	// keep keyID referenced for future use (DM rendering)
	_ = keyID
	return mapping, nil
}

// hashBootstrapCode is sha256-hex of the plaintext bootstrap code.
// Matches the schema fenced in modules/qurl-slack-ddb/main.tf:
// `code_hash = SHA-256(plaintext)`; plaintext is never persisted.
func hashBootstrapCode(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// epochSecondsString renders an int64 epoch as decimal for use in a
// DDB Number attribute. Direct strconv.FormatInt would work too;
// this wrapper keeps the AttributeValue construction grep-able.
func epochSecondsString(n int64) string {
	// strconv.FormatInt is the standard, but to keep this file
	// import-light we inline a small base-10 render.
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
