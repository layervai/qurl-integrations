package slackdata

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	// auditSKPrefix namespaces the executed-action audit log. The full sk is
	// "audit#<user_id>#<unix_nanos>" (the nanos zero-padded so the lexical sort
	// matches chronological order), so begins_with("audit#<user_id>#") returns one
	// user's actions and ScanIndexForward=false returns them newest-first. Per-user
	// BY DESIGN: the App Home surface must never aggregate actions across channels a
	// viewer can't see (that would be a back-door channel-scope leak), so it only
	// ever lists the viewer's OWN confirmed actions.
	auditSKPrefix = "audit#"
	// attrAuditPayload holds the serialized [AuditEntry] on an audit item.
	attrAuditPayload = "audit_payload"
	// auditNanoWidth zero-pads unix-nanoseconds in the sk to a fixed width so the
	// lexical sort matches time order. int64 nanoseconds is 19 digits from 2001
	// through 2262, so 19 needs no leading zeros today but pins the width regardless.
	auditNanoWidth = 19

	// defaultAuditTTL bounds how long a confirmed action stays in the review surface
	// — long enough to be a useful "recent activity" log, short enough to keep the
	// table lean. Tunable via [AgentStore.AuditTTL].
	defaultAuditTTL = 14 * 24 * time.Hour
	// defaultAuditListLimit caps ListAuditEntries when the caller passes a
	// non-positive limit — App Home shows a recent window, not the whole history.
	defaultAuditListLimit = 20
)

// AuditEntry is one confirmed mutation, recorded for the App Home review surface.
// Every field is a plain string the caller derives from the executed pending action.
// Any rendering surface MUST treat the displayed fields (Target, Reason) as untrusted
// echo (they are partly LLM-distilled / user-influenced) and escape or validate them
// exactly as the confirm card does before display — never render them raw.
type AuditEntry struct {
	Actor   string `json:"actor"`             // Slack user id who confirmed the action
	Action  string `json:"action"`            // the mutation kind (get/revoke/set_alias/...)
	Target  string `json:"target,omitempty"`  // the resource token/alias/url acted on
	Channel string `json:"channel,omitempty"` // the channel the action ran in
	Reason  string `json:"reason,omitempty"`  // the audit reason (LLM-distilled intent)
	// Outcome is the formatted public card text. Captured in the record, but the App
	// Home summary does NOT echo it: escaping its intentional backticks for safety renders
	// it degraded, and it largely repeats Target. A clean per-result line is a follow-up.
	Outcome string `json:"outcome,omitempty"`
	UnixSec int64  `json:"ts"` // when it ran, for display (store-stamped)
}

func (s *AgentStore) auditTTL() time.Duration {
	if s.AuditTTL > 0 {
		return s.AuditTTL
	}
	return defaultAuditTTL
}

// PutAuditEntry records one confirmed mutation under partition (the SLACK TEAM id),
// keyed by the actor + the write time so a per-user query returns it newest-first.
// The store stamps the write time onto the stored copy (so the displayed time and the
// sort key share one clock) without mutating the caller's entry. Intended to be called
// best-effort: a failed audit write must never fail the user's already-executed action.
//
// Two actions by the same user collide on an sk only if they land in the same
// nanosecond, which a human-driven confirm click cannot; a collision would overwrite
// the older entry, an acceptable loss for a review log that is never an authority.
func (s *AgentStore) PutAuditEntry(ctx context.Context, partition string, entry *AuditEntry) error {
	if entry == nil || partition == "" || entry.Actor == "" || entry.Action == "" {
		return &Error{StatusCode: http.StatusBadRequest, Title: "PutAuditEntry: partition, actor and action are required"}
	}
	now := s.now()
	stamped := *entry
	stamped.UnixSec = now.Unix()
	payload, err := json.Marshal(stamped)
	if err != nil {
		return fmt.Errorf("PutAuditEntry: marshal entry: %w", err)
	}
	sk := fmt.Sprintf("%s%s#%0*d", auditSKPrefix, entry.Actor, auditNanoWidth, now.UnixNano())
	_, err = s.Client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.TableName),
		Item: map[string]ddbtypes.AttributeValue{
			attrAgentPK:      stringAttr(partition),
			attrAgentSK:      stringAttr(sk),
			attrAuditPayload: stringAttr(string(payload)),
			attrAgentTTL:     numberAttr(now.Add(s.auditTTL()).Unix()),
		},
	})
	if err != nil {
		return ddbToError("PutAuditEntry", err)
	}
	return nil
}

// ListAuditEntries returns up to limit of a user's confirmed actions, newest-first.
// partition is the SLACK TEAM id; userID scopes the query to that user's OWN actions
// (the per-user boundary — never a cross-channel or workspace aggregate). A
// non-positive limit falls back to defaultAuditListLimit. Past-TTL items are filtered
// at read time (DynamoDB's TTL reaper lags by hours/days), and an entry whose payload
// won't decode is skipped rather than failing the whole list. Because Limit caps the
// items SCANNED (newest-first) before that filter runs, a newest-N window heavy with
// expired/corrupt items can return fewer than limit valid entries — fine for a
// recent-actions view, not a fill-to-N guarantee (which would need over-fetch + trim).
func (s *AgentStore) ListAuditEntries(ctx context.Context, partition, userID string, limit int) ([]AuditEntry, error) {
	if partition == "" || userID == "" {
		return nil, &Error{StatusCode: http.StatusBadRequest, Title: "ListAuditEntries: partition and user_id are required"}
	}
	if limit <= 0 {
		limit = defaultAuditListLimit
	}
	out, err := s.Client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.TableName),
		KeyConditionExpression: aws.String(attrAgentPK + " = :pk AND begins_with(" + attrAgentSK + ", :prefix)"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":pk":     stringAttr(partition),
			":prefix": stringAttr(auditSKPrefix + userID + "#"),
		},
		ScanIndexForward: aws.Bool(false), // newest-first
		Limit:            aws.Int32(int32(limit)),
	})
	if err != nil {
		return nil, ddbToError("ListAuditEntries", err)
	}
	nowUnix := s.now().Unix()
	entries := make([]AuditEntry, 0, len(out.Items))
	for _, item := range out.Items {
		if ttl := readNumber(item, attrAgentTTL); ttl > 0 && nowUnix >= ttl {
			continue
		}
		var e AuditEntry
		if err := json.Unmarshal([]byte(readString(item, attrAuditPayload)), &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries, nil
}
