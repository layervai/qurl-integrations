package slackdata

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
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
//   - assistant pane context: sk = "actx#<thread_key>", carries the channel id a
//     user opened the assistant pane FROM, so a later pane turn (which carries no
//     context of its own) can scope its reads to that channel. Last write wins.
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
	// attrContextChannel is the channel id a user opened the assistant pane FROM,
	// stored on an "actx#<thread_key>" item for the pane turn to scope its reads to.
	attrContextChannel = "ctx_channel"
	// attrTurnCount is the running tally on a fixed-window turn-rate counter item
	// (sk = "rate#<scope>#<window-start>"), incremented atomically per agent turn.
	attrTurnCount = "turn_count"

	convSKPrefix      = "conv#"
	eventSKPrefix     = "evt#"
	pendSKPrefix      = "pend#"
	pendClaimSKPrefix = "pendclaim#"
	threadCtxSKPrefix = "actx#"
	// rateSKPrefix namespaces the per-window turn-rate counters; the full sk is
	// "rate#<scope>#<window-start-unix>" where scope is "team" or "user#<id>".
	rateSKPrefix = "rate#"
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
// EnvAgentStateTable (trimmed) when empty; a missing or whitespace-only name is
// an error (there is no safe default for which environment's data to write).
func NewAgentStore(client DynamoDBClient, tableName string) (*AgentStore, error) {
	if tableName == "" {
		// Trim here too, not just in NewAgentStoreFromEnv: a whitespace-only
		// QURL_AGENT_STATE_TABLE must be rejected as empty for every caller,
		// otherwise a store would be built with a blank table name.
		tableName = strings.TrimSpace(os.Getenv(EnvAgentStateTable))
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

// NewAgentStoreFromEnv constructs an [AgentStore] with a DynamoDB client built
// from the ambient AWS config and the table named by [EnvAgentStateTable]. The
// aws-config plumbing lives here (mirroring [NewStore]) so the composition root
// stays free of SDK wiring. Returns an error when config load fails or the table
// env is unset/blank — callers treat an unset table as "feature dark", so check
// EnvAgentStateTable before calling rather than loading AWS config for nothing.
// The table name is trimmed so a whitespace-only value is rejected as empty
// (not used verbatim) even when this constructor is called directly.
func NewAgentStoreFromEnv(ctx context.Context) (*AgentStore, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("NewAgentStoreFromEnv: load AWS config: %w", err)
	}
	return NewAgentStore(dynamodb.NewFromConfig(cfg), strings.TrimSpace(os.Getenv(EnvAgentStateTable)))
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

// BumpTurnCount atomically increments and returns the agent-turn count for a
// fixed window. teamID is the partition (a workspace, NOT the enterprise-else-team
// event partition — a per-workspace cap shouldn't collapse into one shared bucket
// across an enterprise grid); scope is "team" or "user#<slack_user_id>". The window
// is keyed into the sort key (truncated to window start) so each window is a fresh
// item the table's TTL reaps — no reset write needed.
//
// Uses an atomic ADD (not read-modify-write): the per-team counter is a single hot
// item shared by every member, so a strict atomic increment is the only thing that
// holds the cap under concurrent turns — exactly when a cost backstop matters.
// Returns the NEW count; the caller compares it to its configured limit. A returned
// count above the limit means this turn is the one that crossed it.
func (s *AgentStore) BumpTurnCount(ctx context.Context, teamID, scope string, window time.Duration) (count int64, err error) {
	if teamID == "" || scope == "" {
		return 0, &Error{StatusCode: http.StatusBadRequest, Title: "BumpTurnCount: team_id and scope are required"}
	}
	if window <= 0 {
		return 0, &Error{StatusCode: http.StatusBadRequest, Title: "BumpTurnCount: window must be positive"}
	}
	windowStart := s.now().UTC().Truncate(window)
	sk := fmt.Sprintf("%s%s#%d", rateSKPrefix, scope, windowStart.Unix())
	// TTL a full window past the window's end so a clock running behind the DDB TTL
	// reaper can't drop a still-current counter; the window-keyed sk makes the next
	// window start fresh regardless.
	expiresAt := windowStart.Add(2 * window).Unix()

	out, err := s.Client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.TableName),
		Key: map[string]ddbtypes.AttributeValue{
			attrAgentPK: stringAttr(teamID),
			attrAgentSK: stringAttr(sk),
		},
		UpdateExpression: aws.String("ADD " + attrTurnCount + " :one SET " + attrAgentTTL + " = :ttl"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":one": numberAttr(1),
			":ttl": numberAttr(expiresAt),
		},
		ReturnValues: ddbtypes.ReturnValueUpdatedNew,
	})
	if err != nil {
		return 0, ddbToError("BumpTurnCount", err)
	}
	return readNumber(out.Attributes, attrTurnCount), nil
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

// PutThreadContext records the channel a user opened the assistant pane FROM
// (assistant_thread.context.channel_id), keyed by the pane thread, so a later pane
// turn — which carries no context of its own — can scope its reads to that channel.
// Last write wins (no create-condition): an assistant_thread_context_changed event,
// fired when the user switches the channel they're viewing, overwrites it. TTL'd via
// conversationTTL so the context lives exactly as long as the conversation it scopes;
// the turn path refreshes it like SaveConversation refreshes the transcript.
//
// partition is the SLACK TEAM id, not the enterprise-grid-aware conversation
// partition. The context is WRITTEN on assistant_thread_started /
// assistant_thread_context_changed and READ on the message.im turn — three distinct
// event types — and only the team id is guaranteed identical across all of them (the
// enterprise field can vary by event type on Grid), so keying on team id is what lets
// the turn find what the container events stored. The thread key is globally unique,
// so org-grain partitioning would buy nothing.
func (s *AgentStore) PutThreadContext(ctx context.Context, partition, threadKey, channelID string) error {
	if partition == "" || threadKey == "" || channelID == "" {
		return &Error{StatusCode: http.StatusBadRequest, Title: "PutThreadContext: partition, thread_key and channel_id are required"}
	}
	_, err := s.Client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.TableName),
		Item: map[string]ddbtypes.AttributeValue{
			attrAgentPK:        stringAttr(partition),
			attrAgentSK:        stringAttr(threadCtxSKPrefix + threadKey),
			attrContextChannel: stringAttr(channelID),
			attrAgentTTL:       numberAttr(s.now().Add(s.conversationTTL()).Unix()),
		},
	})
	if err != nil {
		return ddbToError("PutThreadContext", err)
	}
	return nil
}

// GetThreadContext returns the channel the assistant pane was opened from for a
// thread, or ("", false, nil) when none was stored (never written, TTL-reaped, or a
// thread that predates context-scoping). A pane turn uses it to scope its reads;
// found=false means "no context — fall back to the DM". partition is the SLACK TEAM
// id (see PutThreadContext). The TTL is enforced at read time, like LoadPendingAction,
// so a long-stale context isn't returned past its window.
func (s *AgentStore) GetThreadContext(ctx context.Context, partition, threadKey string) (channelID string, found bool, err error) {
	if partition == "" || threadKey == "" {
		return "", false, &Error{StatusCode: http.StatusBadRequest, Title: "GetThreadContext: partition and thread_key are required"}
	}
	out, err := s.Client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.TableName),
		Key: map[string]ddbtypes.AttributeValue{
			attrAgentPK: stringAttr(partition),
			attrAgentSK: stringAttr(threadCtxSKPrefix + threadKey),
		},
	})
	if err != nil {
		return "", false, ddbToError("GetThreadContext", err)
	}
	if len(out.Item) == 0 {
		return "", false, nil
	}
	if ttl := readNumber(out.Item, attrAgentTTL); ttl > 0 && s.now().Unix() >= ttl {
		return "", false, nil
	}
	return readString(out.Item, attrContextChannel), true, nil
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
