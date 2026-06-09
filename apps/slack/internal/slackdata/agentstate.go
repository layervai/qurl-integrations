package slackdata

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// EnvAgentStateTable names the DynamoDB table backing conversation-mode state
// (per-thread conversation history and Slack event-id dedupe). Provisioned in
// the infra repo; the table is unused until conversation mode is wired.
const EnvAgentStateTable = "QURL_AGENT_STATE_TABLE"

// Agent-state table attribute names. The table is a single partition-keyed
// store holding several item types under one (pk, sk) schema, discriminated by
// the sort-key prefix:
//
//   - conversation history: sk = "conv#<thread_key>", carries the serialized
//     transcript blob + an optimistic-concurrency version.
//   - event dedupe markers: sk = "evt#<event_id>", existence-only.
//   - pending confirm-action payloads: sk = "pend#<id>", carries the serialized
//     proposal snapshot awaiting an Approve/Reject click.
//   - pending-action claim markers: sk = "pendclaim#<id>", existence-only — the
//     consume-once latch so a proposal executes at most once.
//
// Every item carries a `ttl` epoch the table's DynamoDB TTL reaps. `conv_version`
// is a deliberately non-reserved attribute name (DDB reserves "VERSION") so the
// optimistic-write condition needs no expression-name alias.
const (
	attrAgentPK       = "pk"
	attrAgentSK       = "sk"
	attrAgentMessages = "messages"
	attrAgentVersion  = "conv_version"
	attrAgentTTL      = "ttl"
	attrPendPayload   = "pend_payload"

	convSKPrefix      = "conv#"
	eventSKPrefix     = "evt#"
	pendSKPrefix      = "pend#"
	pendClaimSKPrefix = "pendclaim#"
)

// Default TTLs. Conversations live long enough to span a thread's natural pace
// but short enough that stale context is dropped. The dedupe marker must outlive
// Slack's full retry schedule — Slack re-delivers an un-acked event up to a few
// times spaced out to roughly half an hour — or a late retry could land after
// the marker expired and be processed twice. One hour clears that window with
// margin. (We ack 200 immediately, so retries should be rare regardless.)
const (
	defaultConversationTTL = 30 * time.Minute
	defaultDedupeTTL       = 1 * time.Hour
	// defaultPendingActionTTL bounds how long a proposed mutation stays clickable.
	// Long enough for a human (often a different admin than the asker) to notice
	// and approve, short enough that a stale confirm card can't execute much later.
	// Enforced at read time in LoadPendingAction (not just by the lagging DynamoDB
	// TTL reaper), so the window is a real bound.
	defaultPendingActionTTL = 10 * time.Minute
)

// ErrConversationConflict is returned by [AgentStore.SaveConversation] when a
// concurrent turn advanced the thread's version between load and save. The
// caller should reload and retry (or drop the racing turn).
var ErrConversationConflict = errors.New("slackdata: conversation version conflict")

// AgentStore is the DDB-direct accessor for conversation-mode state. It owns one
// table (EnvAgentStateTable), separate from the [Store] tables, so the
// conversation surface's lifecycle and IAM grants stay independent of the
// admin/policy surface.
//
// The zero value is not usable — construct via [NewAgentStore] or set Client +
// TableName explicitly in tests.
type AgentStore struct {
	Client    DynamoDBClient
	TableName string

	// Now is injected so tests can pin the clock. Defaults to time.Now.
	Now func() time.Time
	// ConversationTTL / DedupeTTL / PendingActionTTL default to the package
	// defaults when zero.
	ConversationTTL  time.Duration
	DedupeTTL        time.Duration
	PendingActionTTL time.Duration
}

// NewAgentStore constructs an [AgentStore]. The table name falls back to
// EnvAgentStateTable when empty; a missing name is an error (there is no safe
// default for which environment's data to write).
func NewAgentStore(client DynamoDBClient, tableName string) (*AgentStore, error) {
	if tableName == "" {
		tableName = os.Getenv(EnvAgentStateTable)
	}
	if tableName == "" {
		return nil, &Error{StatusCode: http.StatusInternalServerError, Title: "NewAgentStore: " + EnvAgentStateTable + " is required"}
	}
	if client == nil {
		return nil, &Error{StatusCode: http.StatusInternalServerError, Title: "NewAgentStore: client is required"}
	}
	return &AgentStore{
		Client:          client,
		TableName:       tableName,
		Now:             time.Now,
		ConversationTTL: defaultConversationTTL,
		DedupeTTL:       defaultDedupeTTL,
	}, nil
}

func (s *AgentStore) now() time.Time {
	return resolveNow(s.Now)
}

func (s *AgentStore) conversationTTL() time.Duration {
	if s.ConversationTTL > 0 {
		return s.ConversationTTL
	}
	return defaultConversationTTL
}

func (s *AgentStore) dedupeTTL() time.Duration {
	if s.DedupeTTL > 0 {
		return s.DedupeTTL
	}
	return defaultDedupeTTL
}

func (s *AgentStore) pendingActionTTL() time.Duration {
	if s.PendingActionTTL > 0 {
		return s.PendingActionTTL
	}
	return defaultPendingActionTTL
}

// MarkEventSeen records a Slack event id under partition and reports whether
// this is the first time it has been seen. Slack delivers events at least once
// and retries on a slow ack, so a handler must dedupe before acting. The write
// is a conditional PutItem (attribute_not_exists), so concurrent deliveries on
// different instances race to a single winner: exactly one call returns
// firstTime=true.
func (s *AgentStore) MarkEventSeen(ctx context.Context, partition, eventID string) (firstTime bool, err error) {
	if partition == "" || eventID == "" {
		return false, &Error{StatusCode: http.StatusBadRequest, Title: "MarkEventSeen: partition and event_id are required"}
	}
	created, err := s.putMarkerIfAbsent(ctx, partition, eventSKPrefix+eventID, s.dedupeTTL())
	if err != nil {
		return false, ddbToError("MarkEventSeen", err)
	}
	return created, nil // false → already seen (a retry/duplicate)
}

// putMarkerIfAbsent conditionally creates an existence-only marker (pk=partition,
// sk, ttl) and reports whether THIS call created it (true) vs found it already
// present (false). The attribute_not_exists(pk) condition makes concurrent writers
// on different instances race to a single winner. Shared by [AgentStore.MarkEventSeen]
// (event dedupe) and [AgentStore.ClaimPendingAction] (consume-once latch). Returns
// the raw client error for the caller to wrap with its op context.
func (s *AgentStore) putMarkerIfAbsent(ctx context.Context, partition, sk string, ttl time.Duration) (created bool, err error) {
	_, err = s.Client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.TableName),
		Item: map[string]ddbtypes.AttributeValue{
			attrAgentPK:  stringAttr(partition),
			attrAgentSK:  stringAttr(sk),
			attrAgentTTL: numberAttr(s.now().Add(ttl).Unix()),
		},
		ConditionExpression: aws.String("attribute_not_exists(" + attrAgentPK + ")"),
	})
	if err != nil {
		var cond *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// LoadConversation returns the stored transcript blob and its version for a
// thread, or (nil, 0, nil) when no conversation exists yet. The returned version
// must be passed back to [AgentStore.SaveConversation] to detect a concurrent
// writer.
func (s *AgentStore) LoadConversation(ctx context.Context, partition, threadKey string) (history []byte, version int64, err error) {
	if partition == "" || threadKey == "" {
		return nil, 0, &Error{StatusCode: http.StatusBadRequest, Title: "LoadConversation: partition and thread_key are required"}
	}
	out, err := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.TableName),
		Key: map[string]ddbtypes.AttributeValue{
			attrAgentPK: stringAttr(partition),
			attrAgentSK: stringAttr(convSKPrefix + threadKey),
		},
	})
	if err != nil {
		return nil, 0, ddbToError("LoadConversation", err)
	}
	if len(out.Item) == 0 {
		return nil, 0, nil
	}
	blob := readString(out.Item, attrAgentMessages)
	return []byte(blob), readNumber(out.Item, attrAgentVersion), nil
}

// SaveConversation writes the transcript blob for a thread with optimistic
// concurrency. expectedVersion is the version returned by the matching
// [AgentStore.LoadConversation] (0 for a brand-new thread). It returns
// [ErrConversationConflict] when a concurrent turn advanced the version, so two
// in-flight turns on one thread can't silently clobber each other.
func (s *AgentStore) SaveConversation(ctx context.Context, partition, threadKey string, history []byte, expectedVersion int64) error {
	if partition == "" || threadKey == "" {
		return &Error{StatusCode: http.StatusBadRequest, Title: "SaveConversation: partition and thread_key are required"}
	}
	_, err := s.Client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.TableName),
		Item: map[string]ddbtypes.AttributeValue{
			attrAgentPK:       stringAttr(partition),
			attrAgentSK:       stringAttr(convSKPrefix + threadKey),
			attrAgentMessages: stringAttr(string(history)),
			attrAgentVersion:  numberAttr(expectedVersion + 1),
			attrAgentTTL:      numberAttr(s.now().Add(s.conversationTTL()).Unix()),
		},
		// First write (no row) OR the stored version still matches what we read.
		ConditionExpression: aws.String("attribute_not_exists(" + attrAgentPK + ") OR " + attrAgentVersion + " = :ev"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":ev": numberAttr(expectedVersion),
		},
	})
	if err != nil {
		var cond *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return ErrConversationConflict
		}
		return ddbToError("SaveConversation", err)
	}
	return nil
}

// PutPendingAction stores a proposed-mutation snapshot under partition, keyed by
// a caller-generated unguessable id, awaiting an Approve/Reject click. The write
// is a conditional create (attribute_not_exists) — the id is globally unique, so
// this only guards against the astronomically-unlikely id collision rather than
// overwriting a live pending action. TTL'd via pendingActionTTL.
//
// partition is the SLACK TEAM id (not the enterprise-grid-aware conversation
// partition): the propose surface (events) and the click surface (interactions)
// both carry team id identically, whereas the enterprise field can differ — so
// keying on team id is what lets the click find what propose stored.
func (s *AgentStore) PutPendingAction(ctx context.Context, partition, id string, payload []byte) error {
	if partition == "" || id == "" {
		return &Error{StatusCode: http.StatusBadRequest, Title: "PutPendingAction: partition and id are required"}
	}
	_, err := s.Client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.TableName),
		Item: map[string]ddbtypes.AttributeValue{
			attrAgentPK:     stringAttr(partition),
			attrAgentSK:     stringAttr(pendSKPrefix + id),
			attrPendPayload: stringAttr(string(payload)),
			attrAgentTTL:    numberAttr(s.now().Add(s.pendingActionTTL()).Unix()),
		},
		ConditionExpression: aws.String("attribute_not_exists(" + attrAgentPK + ")"),
	})
	if err != nil {
		return ddbToError("PutPendingAction", err)
	}
	return nil
}

// LoadPendingAction returns the stored snapshot for a pending-action id, or
// (nil, false, nil) when none exists (never written, already TTL-reaped, or a
// forged id). The caller must treat found=false as "expired" and not execute.
func (s *AgentStore) LoadPendingAction(ctx context.Context, partition, id string) (payload []byte, found bool, err error) {
	if partition == "" || id == "" {
		return nil, false, &Error{StatusCode: http.StatusBadRequest, Title: "LoadPendingAction: partition and id are required"}
	}
	out, err := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.TableName),
		Key: map[string]ddbtypes.AttributeValue{
			attrAgentPK: stringAttr(partition),
			attrAgentSK: stringAttr(pendSKPrefix + id),
		},
	})
	if err != nil {
		return nil, false, ddbToError("LoadPendingAction", err)
	}
	if len(out.Item) == 0 {
		return nil, false, nil
	}
	// Enforce the TTL at read time. DynamoDB's TTL reaper only deletes "within a
	// few days" (commonly hours of lag), so a plain GetItem could otherwise return a
	// long-stale pending action. Treating a past-TTL item as already gone makes the
	// pendingActionTTL window a real bound, not just a reaper hint — the click-time
	// admin re-check and the consume-once claim are independent backstops regardless.
	if ttl := readNumber(out.Item, attrAgentTTL); ttl > 0 && s.now().Unix() >= ttl {
		return nil, false, nil
	}
	return []byte(readString(out.Item, attrPendPayload)), true, nil
}

// ClaimPendingAction is the consume-once latch: the first caller to claim an id
// gets claimed=true (proceed to execute/cancel); every later caller — a
// double-click, a concurrent click on another instance, or a replay — gets
// claimed=false and MUST NOT execute. Implemented as a conditional create of the
// claim marker (attribute_not_exists), the same race-to-one-winner mechanism as
// [AgentStore.MarkEventSeen], so it is both concurrency- and replay-safe without
// a conditional delete (which the payload item is left for TTL to reap).
func (s *AgentStore) ClaimPendingAction(ctx context.Context, partition, id string) (claimed bool, err error) {
	if partition == "" || id == "" {
		return false, &Error{StatusCode: http.StatusBadRequest, Title: "ClaimPendingAction: partition and id are required"}
	}
	claimed, err = s.putMarkerIfAbsent(ctx, partition, pendClaimSKPrefix+id, s.pendingActionTTL())
	if err != nil {
		return false, ddbToError("ClaimPendingAction", err)
	}
	return claimed, nil // false → already claimed (double-click / replay)
}

// numberAttr builds a DynamoDB Number attribute from an int64.
func numberAttr(n int64) ddbtypes.AttributeValue {
	return &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(n, 10)}
}

// readNumber reads an int64 Number attribute, returning 0 when absent or
// unparseable (a fresh row has no version).
func readNumber(item map[string]ddbtypes.AttributeValue, key string) int64 {
	v, ok := item[key].(*ddbtypes.AttributeValueMemberN)
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(v.Value, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
