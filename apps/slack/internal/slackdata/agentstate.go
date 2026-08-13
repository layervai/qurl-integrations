package slackdata

import (
	"context"
	"crypto/rand"
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

// EnvAgentStateTable names the DynamoDB table backing metadata-only
// conversation-mode state. Provisioned in the infra repo; the table is unused
// until conversation mode is wired.
const EnvAgentStateTable = "QURL_AGENT_STATE_TABLE"

// Agent-state table attribute names. The table is a single partition-keyed
// store holding several item types under one (pk, sk) schema, discriminated by
// the sort-key prefix:
//
//   - event dedupe markers: sk = "evt#<event_id>", existence-only.
//   - pending confirm-action payloads: sk = "pend#<id>", carries the serialized
//     proposal snapshot awaiting an Approve/Reject click.
//   - pending-action claim markers: sk = "pendclaim#<id>", existence-only — the
//     consume-once latch so a proposal executes at most once.
//   - unsupported-media notice markers: sk = "media#<channel>:<user>",
//     existence-only — the once-per-window latch that keeps one member's upload
//     burst from drawing one bot reply per file.
//   - assistant pane context: sk = "actx#<thread_key>", carries the channel id a
//     user opened the assistant pane FROM, so a later pane turn (which carries no
//     context of its own) can scope its reads to that channel. Last write wins.
//   - executed-action audit log: sk = "audit#<user_id>#<unix_nanos>", one item per
//     mutation a user confirmed, carrying a serialized [AuditEntry]. Queried
//     newest-first by user for the App Home review surface (see agentaudit.go).
//
// Every item carries a `ttl` epoch the table's DynamoDB TTL reaps; the
// existence-only markers additionally carry a `writer_token` (see putMarker).
const (
	attrAgentPK     = "pk"
	attrAgentSK     = "sk"
	attrAgentTTL    = "ttl"
	attrPendPayload = "pend_payload"
	// attrContextChannel is the channel id a user opened the assistant pane FROM,
	// stored on an "actx#<thread_key>" item for the pane turn to scope its reads to.
	attrContextChannel = "ctx_channel"
	// attrTurnCount is the running tally on a fixed-window turn-rate counter item
	// (sk = "rate#<scope>#<window-start>"), incremented atomically per agent turn.
	attrTurnCount = "turn_count"
	// attrWriterToken is a per-call random value stamped on every marker putMarker
	// writes. It is what lets a failed conditional write tell "another writer won"
	// from "this call won, and the SDK retried a response that was lost" — see
	// putMarker. Never read outside that comparison and never exposed.
	attrWriterToken = "writer_token"

	eventSKPrefix     = "evt#"
	pendSKPrefix      = "pend#"
	pendClaimSKPrefix = "pendclaim#"
	// mediaNoticeSKPrefix namespaces the unsupported-media notice latch; the full
	// sk is "media#<channel_id>:<user_id>" (see AgentStore.MarkMediaNoticeSent).
	mediaNoticeSKPrefix = "media#"
	threadCtxSKPrefix   = "actx#"
	// rateSKPrefix namespaces the per-window turn-rate counters; the full sk is
	// "rate#<scope>#<window-start-unix>" where scope is "team" or "user#<id>".
	rateSKPrefix = "rate#"
)

// Default TTLs. Assistant pane context lives long enough to span a thread's
// natural pace but short enough that stale metadata is dropped. The dedupe marker must outlive
// Slack's full retry schedule — Slack re-delivers an un-acked event up to a few
// times spaced out to roughly half an hour — or a late retry could land after
// the marker expired and be processed twice. One hour clears that window with
// margin. (We ack 200 immediately, so retries should be rare regardless.)
const (
	defaultContextTTL = 30 * time.Minute
	defaultDedupeTTL  = 1 * time.Hour
	// defaultPendingActionTTL bounds how long a proposed mutation stays clickable.
	// Long enough for a human (often a different admin than the asker) to notice
	// and approve, short enough that a stale confirm card can't execute much later.
	// Enforced at read time in LoadPendingAction (not just by the lagging DynamoDB
	// TTL reaper), so the window is a real bound.
	defaultPendingActionTTL = 10 * time.Minute
	// defaultMediaNoticeTTL bounds how long one delivered "I can't read files"
	// notice suppresses the next. Short: the notice is the only thing standing
	// between an upload and silence, so a member who returns to the same
	// conversation minutes later must hear it again. Long enough that a single
	// drag-and-drop burst — which lands in seconds — collapses to one reply.
	// This is a real deadline, not just a cleanup hint: MarkMediaNoticeSent goes
	// through putMarkerIfExpired, which reclaims an expired marker itself rather
	// than waiting for the TTL reaper's multi-day sweep.
	defaultMediaNoticeTTL = 5 * time.Minute
)

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
	// ContextTTL / DedupeTTL / PendingActionTTL / AuditTTL / MediaNoticeTTL
	// default to the package defaults when zero.
	ContextTTL       time.Duration
	DedupeTTL        time.Duration
	PendingActionTTL time.Duration
	AuditTTL         time.Duration
	MediaNoticeTTL   time.Duration
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
		Client:     client,
		TableName:  tableName,
		Now:        time.Now,
		ContextTTL: defaultContextTTL,
		DedupeTTL:  defaultDedupeTTL,
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

func (s *AgentStore) contextTTL() time.Duration {
	if s.ContextTTL > 0 {
		return s.ContextTTL
	}
	return defaultContextTTL
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

func (s *AgentStore) mediaNoticeTTL() time.Duration {
	if s.MediaNoticeTTL > 0 {
		return s.MediaNoticeTTL
	}
	return defaultMediaNoticeTTL
}

// MarkMediaNoticeSent claims the right to send ONE unsupported-media notice for
// conversationKey (channel + user, see agentEventMediaNoticeKey in the slack
// handler) and reports whether this call won it. A member who drags in a hundred
// files sends a hundred deliberate messages, each with its own event id and its
// own ts, so every one clears event dedupe and would otherwise draw its own
// chat.postMessage — and that quota is per-workspace, so the burst degrades agent
// replies for everyone else in the workspace.
//
// Deliberately NOT keyed on the thread: an upload burst arrives as top-level
// messages, which carry no thread_ts, so a thread-keyed latch would be unique per
// message and suppress nothing at all. Keyed WITH the user so one member's burst
// never swallows another member's first notice.
//
// Unlike the turn-rate counters this meters outbound volume, not model spend — a
// suppressed notice costs no tokens either way. Callers treat an error as "send
// it" (fail open): the notice is the only alternative to silence, and failing
// open is no worse than the unsuppressed behavior.
//
// Uses putMarkerIfExpired, NOT putMarkerIfAbsent: over-suppression here IS the
// bug, so the window cannot be left to the TTL reaper. See that method.
//
// One caveat the latch cannot cover: the claim is taken before the reply is
// posted, and the delivery seam reports failure only to the log. If that first
// post fails, the window stays claimed with nothing delivered. Accepted rather
// than compensated — releasing the claim would need the post error threaded back
// through deliverAgentText, and a failed post is already the rarer event.
func (s *AgentStore) MarkMediaNoticeSent(ctx context.Context, partition, conversationKey string) (firstTime bool, err error) {
	if partition == "" || conversationKey == "" {
		return false, &Error{StatusCode: http.StatusBadRequest, Title: "MarkMediaNoticeSent: partition and conversation_key are required"}
	}
	created, err := s.putMarkerIfExpired(ctx, partition, mediaNoticeSKPrefix+conversationKey, s.mediaNoticeTTL())
	if err != nil {
		return false, ddbToError("MarkMediaNoticeSent", err)
	}
	return created, nil // false → a notice is already live in this window
}

// putMarkerIfAbsent conditionally creates an existence-only marker (pk=partition,
// sk, ttl) and reports whether THIS call created it (true) vs found it already
// present (false). The attribute_not_exists(pk) condition makes concurrent writers
// on different instances race to a single winner. Shared by [AgentStore.MarkEventSeen]
// (event dedupe), [AgentStore.ClaimPendingAction] (consume-once latch) and
// [AgentStore.MarkMediaNoticeSent] (once-per-window notice latch). Returns
// the raw client error for the caller to wrap with its op context.
func (s *AgentStore) putMarkerIfAbsent(ctx context.Context, partition, sk string, ttl time.Duration) (created bool, err error) {
	return s.putMarker(ctx, partition, sk, ttl, "attribute_not_exists("+attrAgentPK+")", nil, nil)
}

// putMarkerIfExpired is putMarkerIfAbsent for a marker whose TTL is a real
// deadline rather than just cleanup. It also wins when the stored marker has
// already expired, overwriting it in the same conditional write.
//
// DynamoDB's TTL reaper is documented to delete "within a few days", so for a
// marker keyed to a minutes-long window the reaper is not a clock — an absent-only
// condition would hold the marker for however long the sweep takes. That is
// harmless for the markers whose late-expiry failure mode is "suppress a duplicate
// again" (event dedupe, the consume-once claim latch), and wrong for one whose
// failure mode is a user hearing nothing. This mirrors what LoadPendingAction does
// at read time, enforced at write time instead so it stays a single round trip and
// concurrent writers still race to one winner.
//
// A marker carrying no ttl attribute at all fails the comparison and so counts as
// live; every writer here stamps one.
func (s *AgentStore) putMarkerIfExpired(ctx context.Context, partition, sk string, ttl time.Duration) (created bool, err error) {
	return s.putMarker(ctx, partition, sk, ttl,
		// `ttl` is a DynamoDB reserved word, so it MUST be aliased via an
		// expression-attribute name — a bare `ttl <= :now` 400s with
		// ValidationException. See BumpTurnCount for the same trap.
		"attribute_not_exists("+attrAgentPK+") OR #ttl <= :now",
		map[string]string{"#ttl": attrAgentTTL},
		map[string]ddbtypes.AttributeValue{":now": numberAttr(s.now().Unix())},
	)
}

// putMarker is the shared conditional-create body. names/values MUST be nil (not
// empty maps) when the condition needs no placeholders: the AWS SDK guards on
// != nil, so an empty non-nil map serializes to {} and DynamoDB rejects the call.
//
// Every marker carries a per-call writer token, which is what lets a failed
// condition be told apart from a lost response. No custom retryer is configured,
// so the SDK's standard retryer is live (3 attempts) and it retries INSIDE this
// PutItem call: an attempt can create the item and have its response dropped,
// leaving the next attempt to fail the condition against this call's own marker.
// Reading the stored item back through ReturnValuesOnConditionCheckFailure and
// matching the token reports that as created=true, because it was.
//
// Without it that case is indistinguishable from a genuine race loss, and for
// MarkMediaNoticeSent the caller would conclude another writer won and stay
// silent for the rest of the window — the exact failure the notice exists to
// prevent, and one claimMediaNotice's fail-open branch cannot catch because
// there is no error to fail open on. It is the right answer for the other two
// callers too: both write the marker BEFORE acting, so a call that reported
// false for its own write would drop the event or the click having processed it
// zero times, not once.
//
// The token has to be per-call and random. Comparing the stored `ttl` instead
// would look equivalent and is not: two callers racing within the same second
// compute an identical epoch, so every member of a burst would match and claim
// the latch at once.
func (s *AgentStore) putMarker(ctx context.Context, partition, sk string, ttl time.Duration, condition string, names map[string]string, values map[string]ddbtypes.AttributeValue) (created bool, err error) {
	// rand.Text rather than the repo's hex nonce helper (oauth/state.go): it
	// cannot fail, so a marker write keeps the error surface it has today.
	// MarkEventSeen fails CLOSED on an error, and dropping a Slack event because
	// a token could not be generated would be a worse bug than the one this
	// fixes. Uniqueness per call is the whole requirement — the token never
	// leaves the item, so it is not a security boundary.
	token := rand.Text()
	_, err = s.Client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.TableName),
		Item: map[string]ddbtypes.AttributeValue{
			attrAgentPK:     stringAttr(partition),
			attrAgentSK:     stringAttr(sk),
			attrAgentTTL:    numberAttr(s.now().Add(ttl).Unix()),
			attrWriterToken: stringAttr(token),
		},
		ConditionExpression:                 aws.String(condition),
		ExpressionAttributeNames:            names,
		ExpressionAttributeValues:           values,
		ReturnValuesOnConditionCheckFailure: ddbtypes.ReturnValuesOnConditionCheckFailureAllOld,
	})
	if err != nil {
		var cond *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &cond) {
			return markerWrittenBy(cond.Item, token), nil
		}
		return false, err
	}
	return true, nil
}

// markerWrittenBy reports whether the marker that beat a conditional write is one
// THIS call already wrote — the write landed and only its response was lost.
// Everything else reads as false, which is exactly the pre-token behavior: a
// marker from another writer, one written before this attribute existed, or a
// response that carried no item back at all.
//
// The empty-token guard is load-bearing, not defensive. A marker written by a
// deployment that predates attrWriterToken reads back as "", so an empty token on
// this side would match every one of them and hand the latch to every caller at
// once — during a rolling deploy, precisely when both shapes are in the table.
func markerWrittenBy(stored map[string]ddbtypes.AttributeValue, token string) bool {
	return token != "" && readString(stored, attrWriterToken) == token
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
		// `ttl` is a DynamoDB reserved word, so it MUST be aliased via an expression-
		// attribute name here — a bare `SET ttl = :ttl` 400s with ValidationException
		// "reserved keyword: ttl", which (the rate-limit gate being fail-open) silently
		// disabled the per-user/team cap. The PutItem-based markers (putMarkerIfAbsent)
		// set `ttl` as a raw item-map key, which is fine — only EXPRESSIONS reserve it.
		UpdateExpression:         aws.String("ADD " + attrTurnCount + " :one SET #ttl = :ttl"),
		ExpressionAttributeNames: map[string]string{"#ttl": attrAgentTTL},
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

// PutThreadContext records the channel a user opened the assistant pane FROM
// (assistant_thread.context.channel_id), keyed by the pane thread, so a later pane
// turn — which carries no context of its own — can scope its reads to that channel.
// Last write wins (no create-condition): an assistant_thread_context_changed event,
// fired when the user switches the channel they're viewing, overwrites it. TTL'd via
// contextTTL so the context stays short-lived; the turn path refreshes it
// while the assistant pane remains active.
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
			attrAgentTTL:       numberAttr(s.now().Add(s.contextTTL()).Unix()),
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

// PurgeWorkspaceAgentState deletes every qurl_agent_state row under partition.
// It is part of the Slack app-uninstall / token-revoke cascade: unlike the table's
// normal TTL cleanup, this explicitly removes dedupe markers, pending actions,
// pane context, rate counters, audit entries, and any legacy `conv#` transcript
// rows left by deployments before the zero-copy migration when a workspace
// install is being forgotten.
//
// The table is keyed by (pk, sk), so the purge queries one partition and deletes
// each observed sort key. Deletes are unconditional and therefore idempotent; an
// already-removed row is a no-op. Unlike the durable workspace_state /
// workspace_mappings / channel_policies rows, this ephemeral TTL-backed state is
// intentionally not reinstall-cutoff guarded, so delayed teardown can remove
// fresh agent metadata or pane context but cannot remove credentials or
// policy. The method attempts every delete in a page even after an individual
// DeleteItem error, then returns the joined errors so the lifecycle retry path
// can retry any residue.
func (s *AgentStore) PurgeWorkspaceAgentState(ctx context.Context, partition string) error {
	if partition == "" {
		return &Error{StatusCode: http.StatusBadRequest, Title: "PurgeWorkspaceAgentState: partition is required"}
	}
	var startKey map[string]ddbtypes.AttributeValue
	var deleteErrs []error
	for {
		out, err := s.Client.Query(ctx, &dynamodb.QueryInput{
			TableName:              aws.String(s.TableName),
			KeyConditionExpression: aws.String("#pk = :pk"),
			ExpressionAttributeNames: map[string]string{
				"#pk": attrAgentPK,
				"#sk": attrAgentSK,
			},
			ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
				":pk": stringAttr(partition),
			},
			// Only the SK is needed because the PK is the requested partition.
			ProjectionExpression: aws.String("#sk"),
			ExclusiveStartKey:    startKey,
		})
		if err != nil {
			return joinSweepErrors(deleteErrs, ddbToError("PurgeWorkspaceAgentState", err))
		}
		for _, item := range out.Items {
			sk := readString(item, attrAgentSK)
			if sk == "" {
				deleteErrs = append(deleteErrs, &Error{
					StatusCode: http.StatusInternalServerError,
					Title:      "PurgeWorkspaceAgentState: queried row missing sk",
				})
				continue
			}
			if _, err := s.Client.DeleteItem(ctx, &dynamodb.DeleteItemInput{
				TableName: aws.String(s.TableName),
				Key: map[string]ddbtypes.AttributeValue{
					attrAgentPK: stringAttr(partition),
					attrAgentSK: stringAttr(sk),
				},
			}); err != nil {
				deleteErrs = append(deleteErrs, ddbToError("PurgeWorkspaceAgentState", err))
			}
		}
		if len(out.LastEvaluatedKey) == 0 {
			break
		}
		startKey = out.LastEvaluatedKey
	}
	return errors.Join(deleteErrs...)
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
