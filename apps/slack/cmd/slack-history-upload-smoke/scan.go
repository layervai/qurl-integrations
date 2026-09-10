package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/layervai/qurl-integrations/apps/slack/internal"
)

const (
	// defaultConversationTypes includes im and mpim on purpose: the agent's primary
	// surface IS the DM, so a scan that skipped them would measure a narrower surface
	// than the seam it is checking, and the recorded measurement covered them too. The
	// command stays content-free either way — but it does read DM message objects into
	// memory, so the runbook says so and -conversation-types narrows it.
	defaultConversationTypes = "public_channel,private_channel,im,mpim"
	defaultMaxConversations  = 25
	defaultMaxPages          = 4
	defaultPageLimit         = 200
	defaultMaxThreads        = 5

	// maxPageLimit is Slack's documented ceiling for the `limit` parameter on the
	// conversations.* read methods.
	maxPageLimit = 1000

	// maxThreadsCeiling bounds -max-threads the way maxPageLimit bounds -page-limit:
	// sampling more threads than a page can hold cannot find more threads.
	maxThreadsCeiling = maxPageLimit

	// maxConversationsCeiling and maxPagesCeiling exist so every bound flag has one.
	// Without them -timeout was the only thing standing between a mistyped value and a
	// scan that reads until the budget runs out, which then reports as a truncation
	// rather than as the typo it was. Generous: the point is refusing absurd values, not
	// second-guessing a deliberate one.
	maxConversationsCeiling = 10_000
	maxPagesCeiling         = 1_000

	// maxSlackResponseBytes is deliberately far above the production seam's 512 KB
	// (slackAgentThreadHistoryResponseBodyLimit): that one reads a bounded thread
	// window, while this reads whole history pages whose messages carry full file
	// objects. A page that would be too large here says nothing about production.
	maxSlackResponseBytes = 4 << 20

	// maxRateLimitWait bounds how long a single 429 may park the scan. Slack's
	// Retry-After is advisory and the overall timeout still applies on top.
	maxRateLimitWait = 30 * time.Second

	// maxSlackReasonBytes bounds an error string copied out of a response body into the
	// report. Slack sends short enum codes, so this never truncates a real one — but the
	// package comment promises a content-free report, and against a -base-url that is not
	// Slack that promise would otherwise be the other end's to keep.
	maxSlackReasonBytes = 200

	// The two surface names double as the Web API method names they come from.
	methodConversationsHistory = "conversations.history"
	methodConversationsList    = "conversations.list"
	methodConversationsReplies = "conversations.replies"
)

var (
	errNoConversationsRead   = errors.New("no conversation could be read")
	errExpectUploadFormat    = errors.New("-expect-upload must be CHANNEL:TIMESTAMP")
	errConversationIDInvalid = errors.New("invalid conversation ID")
)

// scanConfig is the validated invocation. Every field is either an operator flag or
// something run supplies (token, clock, HTTP client).
type scanConfig struct {
	Token             string
	BaseURL           string
	UserAgent         string
	Channels          []string
	ConversationTypes string
	MaxConversations  int
	MaxPages          int
	PageLimit         int
	MaxThreads        int
	SkipReplies       bool
	MinUploads        int
	ExpectUploads     messageRefList
	StrictUncountable bool
	WorkspaceShape    string
	TokenOwner        string
	Scopes            string
	HTTPClient        *http.Client
	StartedAt         time.Time
	// Sleep overrides the rate-limit wait. Nil means a real timer, which is what the CLI
	// uses; tests inject one so a fake returning "ratelimited" costs no wall-clock time.
	Sleep func(context.Context, time.Duration) error
}

// messageRef names one message an operator has looked at in Slack and knows carries
// an upload. It is the only ground truth in this command that does not come from
// Slack's own wire format, which is what makes it worth the flag: every other check
// here compares two readings of the same bytes.
type messageRef struct {
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

// messageRefList implements flag.Value so -expect-upload can repeat.
type messageRefList []messageRef

func (l *messageRefList) String() string {
	if l == nil || len(*l) == 0 {
		return ""
	}
	parts := make([]string, 0, len(*l))
	for _, ref := range *l {
		parts = append(parts, ref.Channel+":"+ref.TS)
	}
	return strings.Join(parts, ",")
}

func (l *messageRefList) Set(raw string) error {
	channel, ts, found := strings.Cut(strings.TrimSpace(raw), ":")
	channel, ts = strings.TrimSpace(channel), strings.TrimSpace(ts)
	if !found || channel == "" || ts == "" {
		return errExpectUploadFormat
	}
	if err := validateConversationID(channel); err != nil {
		return err
	}
	if strings.ContainsFunc(ts, func(r rune) bool { return r != '.' && (r < '0' || r > '9') }) {
		return fmt.Errorf("%w: timestamp %q is not a Slack ts", errExpectUploadFormat, ts)
	}
	*l = append(*l, messageRef{Channel: channel, TS: ts})
	return nil
}

// filesShape is what a deliberately dumb reader can say about a message's raw `files`
// value: which JSON shape arrived, and nothing about what it means.
type filesShape string

const (
	filesShapeAbsent    filesShape = "absent"
	filesShapeNull      filesShape = "null"
	filesShapePopulated filesShape = "populated_array"
	filesShapeEmpty     filesShape = "empty_array"
	filesShapeOther     filesShape = "other"
)

// messageObservation is one message seen twice.
type messageObservation struct {
	shape filesShape
	// ts identifies the message across surfaces so the same one is not counted twice.
	// A Slack timestamp, not content.
	ts string
	// threadTS and replyCount are what threadParents selects on. Carried here rather than
	// re-decoded because this envelope has already walked the message: reading them
	// separately parsed every message a second time, on every history page, and threw the
	// result away entirely under -skip-replies.
	threadTS         string
	replyCount       int
	entries          int
	fileShareSubtype bool
	// classified is internal.SlackMessageHasUpload's verdict — the same function the
	// production thread-history seam runs on the same two inputs.
	classified bool
}

// missedUpload reports the one disagreement this command can call a defect. See
// observeMessage for why it is deliberately one-directional.
//
// Against today's classifier it cannot fire: a populated array sets present, and
// present alone returns true. That is the point rather than a gap — this is the check
// that survives someone rewriting SlackMessageHasUpload, and it costs one comparison
// per message to keep. The classifier's own undecodable-value branch is kept for the
// same reason and says so in its doc comment.
func (o messageObservation) missedUpload() bool {
	return o.shape == filesShapePopulated && !o.classified
}

// observeMessage looks at one raw Slack message twice: once with a reader that only
// asks what JSON shape the `files` value has, and once with
// internal.SlackMessageHasUpload — the exact classifier the production thread-history
// seam runs (see fetchSlackAgentThreadHistoryPage in slack_webapi.go).
//
// The two are not two copies of one rule, which is the trap that makes the existing
// unit tests unable to see this rot: they pin the classifier against itself. The dumb
// reader here makes exactly ONE claim, the only one it can make with certainty — a
// `files` value that is a non-empty JSON array is an upload. It says nothing about
// file_share, nothing about shapes it does not recognize, and nothing about an absent
// key; those are the classifier's judgment calls, and restating them would turn this
// into a mirror instead of a witness.
//
// So the check runs in one direction only: the dumb reader is sure, the classifier
// disagreed. That is also the direction that costs something — a missed attachment
// replays a caption to the model stripped of the fact that it described a file, while
// a false positive only annotates a caption that had nothing to annotate.
//
// Telling an absent `files` key from an explicit null is the distinction the shape
// column exists for — "Slack stopped sending the array" and "Slack sent null for a
// message with no files" are indistinguishable to the classifier and could not be more
// different to an operator. One decode is enough for it: json.RawMessage implements
// Unmarshaler, and encoding/json calls Unmarshaler even for a JSON null, so an absent
// key leaves the field nil while an explicit null stores the literal bytes "null". The
// standard library pins that pair directly in TestNullRawMessage (encoding/json), and
// TestObserveMessageDistinguishesAbsentFromNull holds it down from this side.
//
// Note the predicate is `!= nil`, not a length test: "null" is four bytes, so only an
// absent key is nil.
func observeMessage(raw json.RawMessage) (messageObservation, error) {
	var envelope struct {
		TS         string          `json:"ts"`
		ThreadTS   string          `json:"thread_ts"`
		ReplyCount int             `json:"reply_count"`
		Subtype    string          `json:"subtype"`
		Files      json.RawMessage `json:"files"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return messageObservation{}, fmt.Errorf("message JSON: %w", err)
	}

	observation := messageObservation{
		shape:            filesShapeAbsent,
		ts:               envelope.TS,
		threadTS:         envelope.ThreadTS,
		replyCount:       envelope.ReplyCount,
		fileShareSubtype: envelope.Subtype == slackSubtypeFileShare,
		classified:       internal.SlackMessageHasUpload(envelope.Files, envelope.Subtype),
	}
	if envelope.Files != nil {
		observation.shape, observation.entries = classifyFilesShape(envelope.Files)
	}
	return observation, nil
}

// classifyFilesShape buckets a present `files` value by JSON shape alone.
func classifyFilesShape(files json.RawMessage) (shape filesShape, entries int) {
	// Compared directly rather than via TrimSpace: encoding/json hands a RawMessage its
	// value unpadded, a guarantee this repo already pins in TestSlackEventFilesNestedDecodeIsUnpadded.
	// Going through TrimSpace also copied the entire files array — potentially several KB
	// — solely to test it against a four-byte literal.
	if string(files) == "null" {
		return filesShapeNull, 0
	}
	var decoded []json.RawMessage
	if err := json.Unmarshal(files, &decoded); err != nil {
		// Not an array. Whether the classifier is right to treat this as an upload it
		// cannot count is its call, not this reader's; it is recorded so an operator
		// sees the wire format moved.
		return filesShapeOther, 0
	}
	if len(decoded) == 0 {
		return filesShapeEmpty, 0
	}
	return filesShapePopulated, len(decoded)
}

// slackSubtypeFileShare mirrors internal's constant, which is unexported. Only the
// tally uses it; the classification itself goes through SlackMessageHasUpload, so a
// drift here shows up as a tally that stops counting rather than as a wrong verdict.
//
// TODO(upstream-contract): this is a Slack wire value copied into a second place. If
// Slack renames the subtype, this tally silently reports zero file_share messages —
// which is also what a healthy history surface reports, so the two are indistinguishable
// from the output alone.
const slackSubtypeFileShare = "file_share"

// surfaceTally is the per-surface measurement. Counts only: no file name, message
// text, user name or mimetype ever reaches this struct.
type surfaceTally struct {
	Surface           string `json:"surface"`
	Conversations     int    `json:"conversations"`
	Messages          int    `json:"messages"`
	FilesKeyPresent   int    `json:"files_key_present"`
	PopulatedArrays   int    `json:"populated_arrays"`
	FileEntries       int    `json:"file_entries"`
	EmptyArrays       int    `json:"empty_arrays"`
	NullFiles         int    `json:"null_files"`
	UncountableShapes int    `json:"uncountable_shapes"`
	FileShareSubtypes int    `json:"file_share_subtypes"`
	ClassifiedUploads int    `json:"classified_uploads"`
	MissedUploads     int    `json:"missed_uploads"`
	DecodeFailures    int    `json:"decode_failures"`
}

func (t *surfaceTally) add(o messageObservation) {
	t.Messages++
	if o.fileShareSubtype {
		t.FileShareSubtypes++
	}
	if o.classified {
		t.ClassifiedUploads++
	}
	if o.missedUpload() {
		t.MissedUploads++
	}
	switch o.shape {
	case filesShapePopulated:
		t.FilesKeyPresent++
		t.PopulatedArrays++
		t.FileEntries += o.entries
	case filesShapeEmpty:
		t.FilesKeyPresent++
		t.EmptyArrays++
	case filesShapeNull:
		t.FilesKeyPresent++
		t.NullFiles++
	case filesShapeOther:
		t.FilesKeyPresent++
		t.UncountableShapes++
	case filesShapeAbsent:
	}
}

// conversationResult is the per-conversation line of the report. It carries the
// conversation ID and counts, never a name — a private channel's name is content.
type conversationResult struct {
	ID                string `json:"id"`
	Kind              string `json:"kind,omitempty"`
	HistoryMessages   int    `json:"history_messages"`
	RepliesMessages   int    `json:"replies_messages,omitempty"`
	ThreadsSampled    int    `json:"threads_sampled,omitempty"`
	ClassifiedUploads int    `json:"classified_uploads"`
	// MorePages is true when -max-pages ran out with a cursor still outstanding. Without
	// it a conversation holding 50,000 messages reports the same 800 as one holding 800.
	MorePages bool   `json:"more_pages,omitempty"`
	Error     string `json:"error,omitempty"`
}

// expectationResult is one -expect-upload ground truth checked against the classifier.
type expectationResult struct {
	messageRef
	Found      bool   `json:"found"`
	Classified bool   `json:"classified"`
	Shape      string `json:"files_shape,omitempty"`
	Error      string `json:"error,omitempty"`
}

// contractVerdict is the answer the command exists to give.
type contractVerdict struct {
	Holds bool `json:"holds"`
	// DistinctUploads counts upload-bearing MESSAGES, not per-surface hits. Summing the
	// two surfaces would double-count every thread root: conversations.replies returns
	// the parent as its first message and conversations.history already returned it, so
	// a workspace with one upload can report two — and -min-uploads, the tripwire this
	// number feeds, would pass on the phantom.
	DistinctUploads   int      `json:"distinct_uploads"`
	MinUploads        int      `json:"min_uploads"`
	MissedUploads     int      `json:"missed_uploads"`
	UncountableShapes int      `json:"uncountable_shapes"`
	DecodeFailures    int      `json:"decode_failures"`
	Failures          []string `json:"failures,omitempty"`
}

// scanBounds is what shaped the sample. Without it a replies block of all zeros means
// any of four things — -skip-replies, -max-threads 0, no thread had replies, or every
// replies call failed — and only the last is visible from the per-conversation errors.
// Since the replies surface is the whole reason this command reads more than history,
// an all-zero block that silently means "never measured" is the worst ambiguity here.
//
// The base URL is included for the same reason: a fat-fingered -base-url pointed at a
// mock otherwise produces a report that looks entirely normal. It is operator input, not
// user content.
type scanBounds struct {
	BaseURL           string `json:"base_url"`
	ConversationTypes string `json:"conversation_types,omitempty"`
	ExplicitChannels  int    `json:"explicit_channels,omitempty"`
	MaxConversations  int    `json:"max_conversations"`
	MaxPages          int    `json:"max_pages"`
	PageLimit         int    `json:"page_limit"`
	MaxThreads        int    `json:"max_threads"`
	SkipReplies       bool   `json:"skip_replies"`
}

type scanResult struct {
	StartedAt       string               `json:"started_at"`
	Bounds          scanBounds           `json:"bounds"`
	WorkspaceShape  string               `json:"workspace_shape,omitempty"`
	TokenOwner      string               `json:"token_owner,omitempty"`
	Scopes          string               `json:"scopes,omitempty"`
	Conversations   []conversationResult `json:"conversations"`
	History         surfaceTally         `json:"history"`
	Replies         surfaceTally         `json:"replies"`
	ExpectedUploads []expectationResult  `json:"expected_uploads,omitempty"`
	Contract        contractVerdict      `json:"contract"`
}

func newScanResult(cfg *scanConfig) scanResult {
	return scanResult{
		StartedAt: cfg.StartedAt.UTC().Format(time.RFC3339),
		Bounds: scanBounds{
			BaseURL:           cfg.BaseURL,
			ConversationTypes: cfg.ConversationTypes,
			ExplicitChannels:  len(cfg.Channels),
			MaxConversations:  cfg.MaxConversations,
			MaxPages:          cfg.MaxPages,
			PageLimit:         cfg.PageLimit,
			MaxThreads:        cfg.MaxThreads,
			SkipReplies:       cfg.SkipReplies,
		},
		WorkspaceShape: sanitizeReportText(cfg.WorkspaceShape),
		TokenOwner:     sanitizeReportText(cfg.TokenOwner),
		Scopes:         sanitizeReportText(cfg.Scopes),
		Conversations:  []conversationResult{},
		History:        surfaceTally{Surface: methodConversationsHistory},
		Replies:        surfaceTally{Surface: methodConversationsReplies},
	}
}

// runScan reads the workspace and returns the evidence plus a non-nil error when the
// contract no longer holds. The result is returned in both cases: the counts are the
// diagnosis, so they must reach stdout even on failure.
func runScan(ctx context.Context, cfg *scanConfig) (scanResult, error) {
	result := newScanResult(cfg)
	client := &slackClient{
		token:      cfg.Token,
		baseURL:    cfg.BaseURL,
		userAgent:  cfg.UserAgent,
		httpClient: cfg.HTTPClient,
		sleep:      cfg.Sleep,
	}

	conversations, err := resolveConversations(ctx, client, cfg)
	if err != nil {
		// The verdict has to be filled in even here. Returning the zero value would put
		// "min_uploads": 0 in a report run with -min-uploads 7, and leave the failures
		// array empty on the one path where the operator is told to read the report.
		result.Contract = contractVerdict{MinUploads: cfg.MinUploads, Failures: []string{err.Error()}}
		return result, err
	}

	ledger := map[string]struct{}{}
	truncated := false
	for _, conversation := range conversations {
		result.Conversations = append(result.Conversations, scanConversation(ctx, client, cfg, conversation, &result, ledger))
		if ctx.Err() != nil {
			truncated = true
			break
		}
	}
	if !truncated {
		// Skipped on a truncated scan rather than run and failed: with the budget
		// already gone, every lookup would error and each one would be reported as a
		// classifier miss, burying the timeout under failures it caused.
		result.ExpectedUploads = checkExpectations(ctx, client, cfg)
	}
	result.Contract = evaluateContract(cfg, &result, len(ledger), truncated)
	if !result.Contract.Holds {
		return result, fmt.Errorf("upload-detection contract does not hold: %s", strings.Join(result.Contract.Failures, "; "))
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ctxErr
	}
	return result, nil
}

// evaluateContract turns the counts into the verdict. Read the failures as a list of
// distinct things that can rot, not as severity levels: each one is on its own.
//
// result.History.Conversations IS the coverage gate: it counts conversations whose
// HISTORY surface was read, and that surface alone is a complete reading — the replies
// sample is supplementary evidence on top of it (see scanConversation). distinctUploads
// is passed rather than summed from the tallies because it is deduplicated across
// surfaces; truncated is the only input that is not derivable from the report.
func evaluateContract(cfg *scanConfig, result *scanResult, distinctUploads int, truncated bool) contractVerdict {
	verdict := contractVerdict{
		DistinctUploads:   distinctUploads,
		MinUploads:        cfg.MinUploads,
		MissedUploads:     result.History.MissedUploads + result.Replies.MissedUploads,
		UncountableShapes: result.History.UncountableShapes + result.Replies.UncountableShapes,
		DecodeFailures:    result.History.DecodeFailures + result.Replies.DecodeFailures,
	}
	// Both branches below are about this command's REACH rather than Slack's wire
	// format, and both bottom out at zero uploads. Saying either one alongside "the
	// files array has stopped arriving" would point the operator at Slack when the
	// answer is a missing scope or an exhausted budget, so they suppress that failure
	// rather than stacking with it.
	conversationsRead := result.History.Conversations
	switch {
	case truncated:
		verdict.Failures = append(verdict.Failures, "the scan did not finish within -timeout; these counts describe a partial read")
	case conversationsRead == 0:
		verdict.Failures = append(verdict.Failures, errNoConversationsRead.Error()+"; the scan proves nothing about the upload contract")
	}
	if verdict.MissedUploads > 0 {
		verdict.Failures = append(verdict.Failures, fmt.Sprintf(
			"%d message(s) carried a populated files array that SlackMessageHasUpload did not report as an upload", verdict.MissedUploads))
	}
	if conversationsRead > 0 && !truncated && verdict.DistinctUploads < cfg.MinUploads {
		verdict.Failures = append(verdict.Failures, fmt.Sprintf(
			"classified %d distinct upload(s), below -min-uploads %d; if this workspace does contain uploads, the files array has stopped arriving",
			verdict.DistinctUploads, cfg.MinUploads))
	}
	// Both counts mean the same thing — something arrived that this command could not
	// count — so one flag governs both. An undecodable message must not be able to sit
	// under holds:true any more than an unreadable files shape can.
	if cfg.StrictUncountable && verdict.UncountableShapes > 0 {
		verdict.Failures = append(verdict.Failures, fmt.Sprintf(
			"%d message(s) carried a files value that is not a JSON array, i.e. Slack changed the wire format", verdict.UncountableShapes))
	}
	if cfg.StrictUncountable && verdict.DecodeFailures > 0 {
		verdict.Failures = append(verdict.Failures, fmt.Sprintf(
			"%d message(s) could not be decoded at all, i.e. Slack changed the message shape", verdict.DecodeFailures))
	}
	// Both arms fail — the operator asked for a verdict and has to get one — but they are
	// different problems. Only the second is evidence about the classifier; the first is
	// this command not managing to look, and wording it as a classifier defect sends the
	// operator hunting a bug that the run never actually tested for.
	for _, expectation := range result.ExpectedUploads {
		switch {
		case expectation.Error != "":
			verdict.Failures = append(verdict.Failures, fmt.Sprintf(
				"-expect-upload %s:%s could not be verified: %s", expectation.Channel, expectation.TS, expectation.Error))
		case !expectation.Classified:
			verdict.Failures = append(verdict.Failures, fmt.Sprintf(
				"-expect-upload %s:%s was found but not classified as an upload", expectation.Channel, expectation.TS))
		}
	}
	verdict.Holds = len(verdict.Failures) == 0
	return verdict
}

// sanitizeReportText strips control characters from free text so it cannot corrupt the
// JSON report's readability. Used on operator notes and on strings copied out of Slack
// response bodies, which is why it also bounds the length; see maxSlackReasonBytes.
func sanitizeReportText(note string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(note))
	if len(cleaned) <= maxSlackReasonBytes {
		return cleaned
	}
	// Trimmed on a rune boundary so the report stays valid UTF-8.
	truncated := cleaned[:maxSlackReasonBytes]
	for truncated != "" && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated + "…(truncated)"
}

func validateConversationID(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: empty", errConversationIDInvalid)
	}
	// Slack conversation IDs are a single uppercase-alphanumeric token. The exact
	// alphabet stays Slack's to define; this only refuses what cannot be one.
	if strings.ContainsFunc(id, func(r rune) bool {
		return (r < 'A' || r > 'Z') && (r < '0' || r > '9')
	}) {
		return fmt.Errorf("%w: %q", errConversationIDInvalid, id)
	}
	return nil
}

// splitConversationIDs accepts commas or whitespace. strings.FieldsFunc never yields an
// empty field, so no trimming is needed — but duplicates are removed, because scanning
// one conversation twice reports the second pass with classified_uploads: 0 (the ledger
// delta is already spent) while double-counting both surface tallies.
func splitConversationIDs(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	seen := make(map[string]struct{}, len(fields))
	ids := make([]string, 0, len(fields))
	for _, field := range fields {
		if _, duplicate := seen[field]; duplicate {
			continue
		}
		seen[field] = struct{}{}
		ids = append(ids, field)
	}
	return ids
}

// conversationRef is a conversation the scan will read.
type conversationRef struct {
	ID   string
	Kind string
}

// resolveConversations honors -channels when given and otherwise discovers what the
// token can see. Discovery failure is fatal — with nothing to read, every later count
// would be zero for a reason that has nothing to do with the upload contract.
func resolveConversations(ctx context.Context, client *slackClient, cfg *scanConfig) ([]conversationRef, error) {
	if len(cfg.Channels) > 0 {
		// No cap applied here: validateBounds refuses a -channels list longer than
		// -max-conversations rather than letting this quietly measure a subset.
		refs := make([]conversationRef, 0, len(cfg.Channels))
		for _, id := range cfg.Channels {
			refs = append(refs, conversationRef{ID: id})
		}
		return refs, nil
	}
	return listConversations(ctx, client, cfg)
}

// listConversations pages until it has MaxConversations readable ones. MaxPages doubles
// as the page bound here so an unexpected cursor loop cannot run forever; with the
// default page limit the first page already carries far more conversations than the
// default cap, so it is a backstop rather than a tuning knob.
func listConversations(ctx context.Context, client *slackClient, cfg *scanConfig) ([]conversationRef, error) {
	var refs []conversationRef
	cursor := ""
	for page := 0; page < cfg.MaxPages && len(refs) < cfg.MaxConversations; page++ {
		params := url.Values{}
		params.Set("types", cfg.ConversationTypes)
		params.Set("exclude_archived", "true")
		params.Set("limit", strconv.Itoa(cfg.PageLimit))
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var out struct {
			slackResponseStatus
			Channels []struct {
				ID         string `json:"id"`
				IsIM       bool   `json:"is_im"`
				IsMPIM     bool   `json:"is_mpim"`
				IsPrivate  bool   `json:"is_private"`
				IsMember   bool   `json:"is_member"`
				IsArchived bool   `json:"is_archived"`
			} `json:"channels"`
			Metadata slackResponseMetadata `json:"response_metadata"`
		}
		if err := client.get(ctx, methodConversationsList, params, &out); err != nil {
			return nil, err
		}
		for _, channel := range out.Channels {
			if len(refs) >= cfg.MaxConversations {
				break
			}
			if channel.IsArchived || (!channel.IsIM && !channel.IsMPIM && !channel.IsMember) {
				continue
			}
			refs = append(refs, conversationRef{ID: channel.ID, Kind: conversationKind(channel.IsIM, channel.IsMPIM, channel.IsPrivate)})
		}
		cursor = strings.TrimSpace(out.Metadata.NextCursor)
		if cursor == "" {
			break
		}
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("%s returned no readable conversation: %w", methodConversationsList, errNoConversationsRead)
	}
	return refs, nil
}

func conversationKind(isIM, isMPIM, isPrivate bool) string {
	switch {
	case isIM:
		return "im"
	case isMPIM:
		return "mpim"
	case isPrivate:
		return "private_channel"
	default:
		return "public_channel"
	}
}

// scanConversation reads one conversation's history, then samples its threads on the
// replies surface. A per-conversation error is recorded and the scan moves on: one
// channel the bot was removed from must not decide the measurement.
// Coverage is credited by incrementing result.History.Conversations on the history read
// alone, NOT by record.Error being empty. The two answer different questions: a
// conversation whose history read cleanly HAS been measured, even if the supplementary
// replies sample then failed. Keying coverage off record.Error let a rate-limited replies
// call void a fully-read conversation, so a workspace where every thread sample 429s
// reported "no conversation could be read" beside a non-zero upload count.
//
// The return value is named so the deferred delta below lands in what the caller
// receives; assigning a local after `return record` would be copied over and lost.
func scanConversation(ctx context.Context, client *slackClient, cfg *scanConfig, conversation conversationRef, result *scanResult, ledger map[string]struct{}) (record conversationResult) {
	record = conversationResult{ID: conversation.ID, Kind: conversation.Kind}
	before := len(ledger)
	// Whatever pages did come back before an error stay in the tally, so the delta is
	// taken on every path out. A conversation that failed halfway measured half a
	// conversation; discarding that would understate the workspace, and the error is
	// recorded beside it either way.
	defer func() {
		record.ClassifiedUploads = len(ledger) - before
	}()

	// Derived once, here, so readHistory does not have to know about -skip-replies and
	// cannot select thread roots nobody will read.
	threadBudget := cfg.MaxThreads
	if cfg.SkipReplies {
		threadBudget = 0
	}
	threads, err := readHistory(ctx, client, cfg, conversation.ID, threadBudget, &result.History, &record, ledger)
	if err != nil {
		record.Error = err.Error()
		return record
	}
	result.History.Conversations++

	if threadBudget > 0 {
		record.ThreadsSampled, err = readThreads(ctx, client, cfg, conversation.ID, threads, &result.Replies, &record, ledger)
		if err != nil {
			record.Error = err.Error()
		}
		// Counted on both paths: a sample that failed on its third thread still measured
		// two, and their messages are already in the tally. Skipping the increment would
		// report replies.messages > 0 beside replies.conversations = 0.
		if record.ThreadsSampled > 0 {
			result.Replies.Conversations++
		}
	}
	return record
}

// readHistory tallies a conversation's history pages and returns the thread parents
// worth sampling on the replies surface.
func readHistory(ctx context.Context, client *slackClient, cfg *scanConfig, channelID string, threadBudget int, tally *surfaceTally, record *conversationResult, ledger map[string]struct{}) ([]string, error) {
	var threads []string
	cursor := ""
	for page := 0; page < cfg.MaxPages; page++ {
		params := url.Values{}
		params.Set("channel", channelID)
		params.Set("limit", strconv.Itoa(cfg.PageLimit))
		if cursor != "" {
			params.Set("cursor", cursor)
		}
		var out slackMessagesResponse
		if err := client.get(ctx, methodConversationsHistory, params, &out); err != nil {
			return nil, err
		}
		record.HistoryMessages += len(out.Messages)
		observed := tallyMessages(channelID, out.Messages, tally, ledger)
		threads = append(threads, threadParents(observed, threadBudget-len(threads))...)
		cursor = strings.TrimSpace(out.Metadata.NextCursor)
		if cursor == "" {
			break
		}
	}
	record.MorePages = cursor != ""
	return threads, nil
}

// readThreads tallies the replies surface for the sampled threads. This is the half
// the manual measurement never covered: production reads conversations.replies, and
// "same message objects, same API family" was an assumption, not a reading.
func readThreads(ctx context.Context, client *slackClient, cfg *scanConfig, channelID string, threads []string, tally *surfaceTally, record *conversationResult, ledger map[string]struct{}) (int, error) {
	sampled := 0
	for _, threadTS := range threads {
		params := url.Values{}
		params.Set("channel", channelID)
		params.Set("ts", threadTS)
		params.Set("limit", strconv.Itoa(cfg.PageLimit))
		var out slackMessagesResponse
		if err := client.get(ctx, methodConversationsReplies, params, &out); err != nil {
			return sampled, err
		}
		record.RepliesMessages += len(out.Messages)
		_ = tallyMessages(channelID, out.Messages, tally, ledger)
		sampled++
	}
	return sampled, nil
}

// tallyMessages records each message on its surface and, for the ones that classify as
// uploads, in the cross-surface ledger. The ledger is keyed by channel and ts because the
// same message legitimately appears on both surfaces, and only the per-surface tallies
// should say so twice.
func tallyMessages(channelID string, messages []json.RawMessage, tally *surfaceTally, ledger map[string]struct{}) []messageObservation {
	observed := make([]messageObservation, 0, len(messages))
	for _, raw := range messages {
		observation, err := observeMessage(raw)
		if err != nil {
			// Counted rather than returned: a message this command cannot read is a
			// hole in the measurement, not a reason to discard the rest of it.
			tally.DecodeFailures++
			continue
		}
		tally.add(observation)
		observed = append(observed, observation)
		// Safe as an unambiguous key because neither half can contain a colon: Slack
		// conversation IDs are uppercase alphanumerics and a ts is digits and a dot.
		if observation.classified && observation.ts != "" {
			ledger[channelID+":"+observation.ts] = struct{}{}
		}
	}
	return observed
}

// threadParents picks up to limit thread roots out of an already-observed history page,
// keeping only the ones with replies — those are the messages the production seam
// actually reads back. A message whose thread_ts points elsewhere is a reply, not a root.
func threadParents(observed []messageObservation, limit int) []string {
	if limit <= 0 {
		return nil
	}
	// Capped by the page as well as by the limit: -max-threads is operator-supplied,
	// and sizing off it alone turns a fat-fingered value into a huge allocation per page.
	parents := make([]string, 0, min(limit, len(observed)))
	for _, observation := range observed {
		if observation.replyCount <= 0 || observation.ts == "" ||
			(observation.threadTS != "" && observation.threadTS != observation.ts) {
			continue
		}
		parents = append(parents, observation.ts)
		if len(parents) >= limit {
			break
		}
	}
	return parents
}

// checkExpectations reads each -expect-upload message on its own and asks the
// classifier about it. Failures are recorded per expectation rather than aborting:
// an operator who named five messages wants all five verdicts, not the first.
func checkExpectations(ctx context.Context, client *slackClient, cfg *scanConfig) []expectationResult {
	if len(cfg.ExpectUploads) == 0 {
		return nil
	}
	results := make([]expectationResult, 0, len(cfg.ExpectUploads))
	for _, ref := range cfg.ExpectUploads {
		results = append(results, checkExpectation(ctx, client, cfg, ref))
	}
	return results
}

func checkExpectation(ctx context.Context, client *slackClient, cfg *scanConfig, ref messageRef) expectationResult {
	result := expectationResult{messageRef: ref}
	params := url.Values{}
	params.Set("channel", ref.Channel)
	params.Set("latest", ref.TS)
	params.Set("oldest", ref.TS)
	params.Set("inclusive", "true")
	params.Set("limit", "1")
	var out slackMessagesResponse
	if err := client.get(ctx, methodConversationsHistory, params, &out); err != nil {
		result.Error = err.Error()
		return result
	}
	// Confirmed rather than assumed, the way the thread fallback below already does. This
	// is the one check whose oracle is a human, so verdicting on the wrong message would
	// be worse here than anywhere else in the command.
	raw := out.Messages
	if len(raw) > 0 && !messageHasTS(raw[0], ref.TS) {
		raw = nil
	}
	if len(raw) == 0 {
		// conversations.history does not return thread replies, and an upload posted
		// into a thread is exactly the kind of message an operator picks for this flag.
		// conversations.replies takes any ts in the thread, so ask it for the thread and
		// pick the message back out by ts.
		var err error
		if raw, err = fetchThreadMessage(ctx, client, cfg, ref); err != nil {
			result.Error = err.Error()
			return result
		}
		if len(raw) == 0 {
			result.Error = "message not found"
			return result
		}
	}
	observation, err := observeMessage(raw[0])
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Found = true
	result.Classified = observation.classified
	result.Shape = string(observation.shape)
	return result
}

// messageHasTS reports whether a raw message carries exactly this ts.
func messageHasTS(raw json.RawMessage, ts string) bool {
	var message struct {
		TS string `json:"ts"`
	}
	return json.Unmarshal(raw, &message) == nil && message.TS == ts
}

// fetchThreadMessage returns the single thread message whose ts matches ref, or an
// empty slice when the thread does not contain it. It reads ONE page, so an upload named
// past the -page-limit'th reply of a long thread reports "message not found" rather than
// a verdict — which the contract words as unverifiable rather than as a classifier
// defect, so it cannot masquerade as evidence about SlackMessageHasUpload.
func fetchThreadMessage(ctx context.Context, client *slackClient, cfg *scanConfig, ref messageRef) ([]json.RawMessage, error) {
	params := url.Values{}
	params.Set("channel", ref.Channel)
	params.Set("ts", ref.TS)
	params.Set("limit", strconv.Itoa(cfg.PageLimit))
	var out slackMessagesResponse
	if err := client.get(ctx, methodConversationsReplies, params, &out); err != nil {
		return nil, err
	}
	for _, raw := range out.Messages {
		if messageHasTS(raw, ref.TS) {
			return []json.RawMessage{raw}, nil
		}
	}
	return nil, nil
}
