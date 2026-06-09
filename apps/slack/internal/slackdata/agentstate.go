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
// store holding two item types under one (pk, sk) schema, discriminated by the
// sort-key prefix:
//
//   - conversation history: sk = "conv#<thread_key>", carries the serialized
//     transcript blob + an optimistic-concurrency version.
//   - event dedupe markers: sk = "evt#<event_id>", existence-only.
//
// Both carry a `ttl` epoch the table's DynamoDB TTL reaps. `conv_version` is a
// deliberately non-reserved attribute name (DDB reserves "VERSION") so the
// optimistic-write condition needs no expression-name alias.
const (
	attrAgentPK       = "pk"
	attrAgentSK       = "sk"
	attrAgentMessages = "messages"
	attrAgentVersion  = "conv_version"
	attrAgentTTL      = "ttl"

	convSKPrefix  = "conv#"
	eventSKPrefix = "evt#"
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
	// ConversationTTL / DedupeTTL default to the package defaults when zero.
	ConversationTTL time.Duration
	DedupeTTL       time.Duration
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
	_, err = s.Client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.TableName),
		Item: map[string]ddbtypes.AttributeValue{
			attrAgentPK:  stringAttr(partition),
			attrAgentSK:  stringAttr(eventSKPrefix + eventID),
			attrAgentTTL: numberAttr(s.now().Add(s.dedupeTTL()).Unix()),
		},
		ConditionExpression: aws.String("attribute_not_exists(" + attrAgentPK + ")"),
	})
	if err != nil {
		var cond *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return false, nil // already seen — a retry/duplicate
		}
		return false, ddbToError("MarkEventSeen", err)
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
