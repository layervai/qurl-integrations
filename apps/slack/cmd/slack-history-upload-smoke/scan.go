package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal"
)

const (
	defaultConversationTypes = "public_channel,private_channel,im,mpim"
	defaultMaxConversations  = 25
	defaultMaxPages          = 4
	defaultPageLimit         = 200
	defaultMaxThreads        = 5

	// maxPageLimit is Slack's documented ceiling for the `limit` parameter on the
	// conversations.* read methods.
	maxPageLimit = 1000

	// maxSlackResponseBytes is deliberately far above the production seam's 512 KB
	// (slackAgentThreadHistoryResponseBodyLimit): that one reads a bounded thread
	// window, while this reads whole history pages whose messages carry full file
	// objects. A page that would be too large here says nothing about production.
	maxSlackResponseBytes = 4 << 20

	// maxRateLimitWait bounds how long a single 429 may park the scan. Slack's
	// Retry-After is advisory and the overall timeout still applies on top.
	maxRateLimitWait = 30 * time.Second

	// The two surface names double as the Web API method names they come from.
	surfaceHistory          = "conversations.history"
	methodConversationsList = "conversations.list"
	surfaceReplies          = "conversations.replies"
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
	shape            filesShape
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
// The raw message is decoded twice on purpose. The map decode is how the shape reader
// learns whether the `files` KEY was there at all, which a struct decode erases: an
// absent key and an explicit null both land as a nil RawMessage. That distinction is
// the whole point here, because "Slack stopped sending the array" and "Slack sent null
// for a message with no files" look identical to the classifier and could not be more
// different to an operator.
func observeMessage(raw json.RawMessage) (messageObservation, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return messageObservation{}, fmt.Errorf("message JSON: %w", err)
	}
	var envelope struct {
		Subtype string          `json:"subtype"`
		Files   json.RawMessage `json:"files"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return messageObservation{}, fmt.Errorf("message envelope JSON: %w", err)
	}

	files, keyPresent := fields["files"]
	observation := messageObservation{
		shape:            filesShapeAbsent,
		fileShareSubtype: envelope.Subtype == slackSubtypeFileShare,
		classified:       internal.SlackMessageHasUpload(envelope.Files, envelope.Subtype),
	}
	if keyPresent {
		observation.shape, observation.entries = classifyFilesShape(files)
	}
	return observation, nil
}

// classifyFilesShape buckets a present `files` value by JSON shape alone.
func classifyFilesShape(files json.RawMessage) (shape filesShape, entries int) {
	trimmed := strings.TrimSpace(string(files))
	if trimmed == "null" {
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
	Error             string `json:"error,omitempty"`
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
	Holds             bool     `json:"holds"`
	ClassifiedUploads int      `json:"classified_uploads"`
	MinUploads        int      `json:"min_uploads"`
	MissedUploads     int      `json:"missed_uploads"`
	UncountableShapes int      `json:"uncountable_shapes"`
	Failures          []string `json:"failures,omitempty"`
}

type scanResult struct {
	StartedAt       string               `json:"started_at"`
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
		StartedAt:      cfg.StartedAt.UTC().Format(time.RFC3339),
		WorkspaceShape: cleanOperatorNote(cfg.WorkspaceShape),
		TokenOwner:     cleanOperatorNote(cfg.TokenOwner),
		Scopes:         cleanOperatorNote(cfg.Scopes),
		Conversations:  []conversationResult{},
		History:        surfaceTally{Surface: surfaceHistory},
		Replies:        surfaceTally{Surface: surfaceReplies},
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
	}

	conversations, err := resolveConversations(ctx, client, cfg)
	if err != nil {
		return result, err
	}

	coverage := scanCoverage{}
	for _, conversation := range conversations {
		record, historyRead := scanConversation(ctx, client, cfg, conversation, &result)
		if historyRead {
			coverage.conversationsRead++
		}
		result.Conversations = append(result.Conversations, record)
		if ctx.Err() != nil {
			coverage.truncated = true
			break
		}
	}
	if !coverage.truncated {
		// Skipped on a truncated scan rather than run and failed: with the budget
		// already gone, every lookup would error and each one would be reported as a
		// classifier miss, burying the timeout under failures it caused.
		result.ExpectedUploads = checkExpectations(ctx, client, cfg)
	}
	result.Contract = evaluateContract(cfg, &result, coverage)
	if !result.Contract.Holds {
		return result, fmt.Errorf("upload-detection contract does not hold: %s", strings.Join(result.Contract.Failures, "; "))
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ctxErr
	}
	return result, nil
}

// scanCoverage is how much of the workspace the scan actually got through. It gates the
// counts-based failures in evaluateContract, which are only meaningful over a complete
// read.
type scanCoverage struct {
	// conversationsRead counts conversations whose HISTORY surface was read. The replies
	// sample is supplementary evidence on top of that; see scanConversation.
	conversationsRead int
	truncated         bool
}

// evaluateContract turns the counts into the verdict. Read the failures as a list of
// distinct things that can rot, not as severity levels: each one is on its own.
func evaluateContract(cfg *scanConfig, result *scanResult, coverage scanCoverage) contractVerdict {
	verdict := contractVerdict{
		ClassifiedUploads: result.History.ClassifiedUploads + result.Replies.ClassifiedUploads,
		MinUploads:        cfg.MinUploads,
		MissedUploads:     result.History.MissedUploads + result.Replies.MissedUploads,
		UncountableShapes: result.History.UncountableShapes + result.Replies.UncountableShapes,
	}
	// Both branches below are about this command's REACH rather than Slack's wire
	// format, and both bottom out at zero uploads. Saying either one alongside "the
	// files array has stopped arriving" would point the operator at Slack when the
	// answer is a missing scope or an exhausted budget, so they suppress that failure
	// rather than stacking with it.
	switch {
	case coverage.truncated:
		verdict.Failures = append(verdict.Failures, "the scan did not finish within -timeout; these counts describe a partial read")
	case coverage.conversationsRead == 0:
		verdict.Failures = append(verdict.Failures, errNoConversationsRead.Error()+"; the scan proves nothing about the upload contract")
	}
	if verdict.MissedUploads > 0 {
		verdict.Failures = append(verdict.Failures, fmt.Sprintf(
			"%d message(s) carried a populated files array that SlackMessageHasUpload did not report as an upload", verdict.MissedUploads))
	}
	if coverage.conversationsRead > 0 && !coverage.truncated && verdict.ClassifiedUploads < cfg.MinUploads {
		verdict.Failures = append(verdict.Failures, fmt.Sprintf(
			"classified %d upload(s), below -min-uploads %d; if this workspace does contain uploads, the files array has stopped arriving",
			verdict.ClassifiedUploads, cfg.MinUploads))
	}
	if cfg.StrictUncountable && verdict.UncountableShapes > 0 {
		verdict.Failures = append(verdict.Failures, fmt.Sprintf(
			"%d message(s) carried a files value that is not a JSON array, i.e. Slack changed the wire format", verdict.UncountableShapes))
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

// cleanOperatorNote strips control characters from a free-text operator note so it
// cannot corrupt the JSON report's readability.
func cleanOperatorNote(note string) string {
	return strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(note))
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

func splitConversationIDs(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' })
	ids := make([]string, 0, len(fields))
	for _, field := range fields {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			ids = append(ids, trimmed)
		}
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
		refs := make([]conversationRef, 0, len(cfg.Channels))
		for _, id := range cfg.Channels {
			if len(refs) >= cfg.MaxConversations {
				break
			}
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
// It reports historyRead separately from record.Error because the two answer different
// questions. A conversation whose history read cleanly HAS been measured — that surface
// alone is a complete reading — even if the supplementary replies sample then failed.
// Inferring coverage from record.Error instead would let a rate-limited replies call
// void a fully-read conversation, and a workspace where every thread sample 429s would
// report "no conversation could be read" beside a non-zero upload count.
//
// The return values are named so the deferred delta below lands in what the caller
// receives; assigning a local after `return record` would be copied over and lost.
func scanConversation(ctx context.Context, client *slackClient, cfg *scanConfig, conversation conversationRef, result *scanResult) (record conversationResult, historyRead bool) {
	record = conversationResult{ID: conversation.ID, Kind: conversation.Kind}
	before := result.History.ClassifiedUploads + result.Replies.ClassifiedUploads
	// Whatever pages did come back before an error stay in the tally, so the delta is
	// taken on every path out. A conversation that failed halfway measured half a
	// conversation; discarding that would understate the workspace, and the error is
	// recorded beside it either way.
	defer func() {
		record.ClassifiedUploads = result.History.ClassifiedUploads + result.Replies.ClassifiedUploads - before
	}()

	threads, err := readHistory(ctx, client, cfg, conversation.ID, &result.History, &record)
	if err != nil {
		record.Error = err.Error()
		return record, false
	}
	historyRead = true
	result.History.Conversations++

	if !cfg.SkipReplies && cfg.MaxThreads > 0 {
		record.ThreadsSampled, err = readThreads(ctx, client, cfg, conversation.ID, threads, &result.Replies, &record)
		if err != nil {
			record.Error = err.Error()
		} else if record.ThreadsSampled > 0 {
			result.Replies.Conversations++
		}
	}
	return record, historyRead
}

// readHistory tallies a conversation's history pages and returns the thread parents
// worth sampling on the replies surface.
func readHistory(ctx context.Context, client *slackClient, cfg *scanConfig, channelID string, tally *surfaceTally, record *conversationResult) ([]string, error) {
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
		if err := client.get(ctx, surfaceHistory, params, &out); err != nil {
			return nil, err
		}
		record.HistoryMessages += len(out.Messages)
		tallyMessages(out.Messages, tally)
		threads = append(threads, threadParents(out.Messages, cfg.MaxThreads-len(threads))...)
		cursor = strings.TrimSpace(out.Metadata.NextCursor)
		if cursor == "" {
			break
		}
	}
	return threads, nil
}

// readThreads tallies the replies surface for the sampled threads. This is the half
// the manual measurement never covered: production reads conversations.replies, and
// "same message objects, same API family" was an assumption, not a reading.
func readThreads(ctx context.Context, client *slackClient, cfg *scanConfig, channelID string, threads []string, tally *surfaceTally, record *conversationResult) (int, error) {
	sampled := 0
	for _, threadTS := range threads {
		params := url.Values{}
		params.Set("channel", channelID)
		params.Set("ts", threadTS)
		params.Set("limit", strconv.Itoa(cfg.PageLimit))
		var out slackMessagesResponse
		if err := client.get(ctx, surfaceReplies, params, &out); err != nil {
			return sampled, err
		}
		record.RepliesMessages += len(out.Messages)
		tallyMessages(out.Messages, tally)
		sampled++
	}
	return sampled, nil
}

func tallyMessages(messages []json.RawMessage, tally *surfaceTally) {
	for _, raw := range messages {
		observation, err := observeMessage(raw)
		if err != nil {
			// Counted rather than returned: a message this command cannot read is a
			// hole in the measurement, not a reason to discard the rest of it.
			tally.DecodeFailures++
			continue
		}
		tally.add(observation)
	}
}

// threadParents picks up to limit thread roots out of a history page, preferring the
// ones with replies — those are the messages the production seam actually reads back.
func threadParents(messages []json.RawMessage, limit int) []string {
	if limit <= 0 {
		return nil
	}
	parents := make([]string, 0, limit)
	for _, raw := range messages {
		var message struct {
			TS         string `json:"ts"`
			ThreadTS   string `json:"thread_ts"`
			ReplyCount int    `json:"reply_count"`
		}
		if err := json.Unmarshal(raw, &message); err != nil {
			continue
		}
		if message.ReplyCount <= 0 || message.TS == "" || (message.ThreadTS != "" && message.ThreadTS != message.TS) {
			continue
		}
		parents = append(parents, message.TS)
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
		results = append(results, checkExpectation(ctx, client, ref))
	}
	return results
}

func checkExpectation(ctx context.Context, client *slackClient, ref messageRef) expectationResult {
	result := expectationResult{messageRef: ref}
	params := url.Values{}
	params.Set("channel", ref.Channel)
	params.Set("latest", ref.TS)
	params.Set("oldest", ref.TS)
	params.Set("inclusive", "true")
	params.Set("limit", "1")
	var out slackMessagesResponse
	if err := client.get(ctx, surfaceHistory, params, &out); err != nil {
		result.Error = err.Error()
		return result
	}
	raw := out.Messages
	if len(raw) == 0 {
		// conversations.history does not return thread replies, and an upload posted
		// into a thread is exactly the kind of message an operator picks for this flag.
		// conversations.replies takes any ts in the thread, so ask it for the thread and
		// pick the message back out by ts.
		var err error
		if raw, err = fetchThreadMessage(ctx, client, ref); err != nil {
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

// fetchThreadMessage returns the single thread message whose ts matches ref, or an
// empty slice when the thread does not contain it.
func fetchThreadMessage(ctx context.Context, client *slackClient, ref messageRef) ([]json.RawMessage, error) {
	params := url.Values{}
	params.Set("channel", ref.Channel)
	params.Set("ts", ref.TS)
	params.Set("limit", strconv.Itoa(defaultPageLimit))
	var out slackMessagesResponse
	if err := client.get(ctx, surfaceReplies, params, &out); err != nil {
		return nil, err
	}
	for _, raw := range out.Messages {
		var message struct {
			TS string `json:"ts"`
		}
		if err := json.Unmarshal(raw, &message); err != nil {
			continue
		}
		if message.TS == ref.TS {
			return []json.RawMessage{raw}, nil
		}
	}
	return nil, nil
}

type slackResponseStatus struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	Needed   string `json:"needed"`
	Provided string `json:"provided"`
}

type slackResponseMetadata struct {
	NextCursor string `json:"next_cursor"`
}

// slackMessagesResponse keeps the messages raw so observeMessage can see the whole
// object, including whether the `files` key was present at all.
type slackMessagesResponse struct {
	slackResponseStatus
	Messages []json.RawMessage     `json:"messages"`
	Metadata slackResponseMetadata `json:"response_metadata"`
}

type slackClient struct {
	token      string
	baseURL    string
	userAgent  string
	httpClient *http.Client
	// sleep is the 429 wait, injected so tests do not spend real time in it.
	sleep func(context.Context, time.Duration) error
}

// get issues one read and decodes it into out, retrying once when Slack rate-limits.
// The retry is here rather than left to the operator because a tier-3 method read
// across a couple of dozen conversations will hit 429 on a busy workspace, and a scan
// that dies there measures nothing.
func (c *slackClient) get(ctx context.Context, method string, params url.Values, out any) error {
	retryAfter, err := c.getOnce(ctx, method, params, out)
	if err == nil || retryAfter <= 0 {
		return err
	}
	if retryAfter > maxRateLimitWait {
		return fmt.Errorf("%s: rate limited, Retry-After %s exceeds the %s cap", method, retryAfter, maxRateLimitWait)
	}
	if sleepErr := c.wait(ctx, retryAfter); sleepErr != nil {
		return sleepErr
	}
	_, err = c.getOnce(ctx, method, params, out)
	return err
}

func (c *slackClient) wait(ctx context.Context, d time.Duration) error {
	if c.sleep != nil {
		return c.sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// getOnce returns the Retry-After duration alongside the error when Slack rate-limits,
// and zero otherwise, so get can tell a retryable refusal from a terminal one.
func (c *slackClient) getOnce(ctx context.Context, method string, params url.Values, out any) (time.Duration, error) {
	endpoint := c.baseURL + "/" + method
	if encoded := params.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return 0, fmt.Errorf("%s request build: %w", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s request: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusTooManyRequests {
		drainSlackResponseBody(resp.Body)
		return parseRetryAfter(resp.Header.Get("Retry-After")), fmt.Errorf("%s: rate limited", method)
	}
	if resp.StatusCode >= 300 {
		drainSlackResponseBody(resp.Body)
		return 0, fmt.Errorf("%s returned HTTP %d", method, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSlackResponseBytes+1))
	if err != nil {
		return 0, fmt.Errorf("%s response read: %w", method, err)
	}
	if len(raw) > maxSlackResponseBytes {
		drainSlackResponseBody(resp.Body)
		return 0, fmt.Errorf("%s response exceeded %d bytes", method, maxSlackResponseBytes)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return 0, fmt.Errorf("%s response JSON invalid", method)
	}
	return 0, slackStatusError(method, raw)
}

// slackStatusError re-reads the envelope's ok/error fields rather than requiring every
// caller's out type to expose them, so a new read method cannot forget the check.
func slackStatusError(method string, raw []byte) error {
	var status slackResponseStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		return fmt.Errorf("%s response status JSON invalid", method)
	}
	if status.OK {
		return nil
	}
	reason := cleanOperatorNote(status.Error)
	if reason == "" {
		reason = "not_ok"
	}
	if needed := cleanOperatorNote(status.Needed); needed != "" {
		return fmt.Errorf("%s: %s (needed %s, provided %s)", method, reason, needed, cleanOperatorNote(status.Provided))
	}
	return fmt.Errorf("%s: %s", method, reason)
}

func parseRetryAfter(header string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || seconds < 0 {
		return time.Second
	}
	if seconds == 0 {
		return time.Second
	}
	return time.Duration(seconds) * time.Second
}

func drainSlackResponseBody(body io.Reader) {
	// Best-effort connection reuse for moderately oversized bodies. Close tears down
	// the response if bytes still remain.
	_, _ = io.Copy(io.Discard, io.LimitReader(body, maxSlackResponseBytes+1))
}
