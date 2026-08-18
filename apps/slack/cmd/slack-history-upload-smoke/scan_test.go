package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// slackFileObject is a Slack `files` entry carrying the field set the 2026-08-14 scan
// recorded for a hosted file — not a byte-for-byte capture, and not claimed as one.
// Its job here is to be wider than the `{"id":"F1"}` stub the seam tests use, so a
// classifier that started reading INTO the entries has something real to trip on.
const slackFileObject = `{
  "id": "F00000000AA",
  "created": 1723600000,
  "timestamp": 1723600000,
  "name": "quarterly-plan.pdf",
  "title": "quarterly-plan.pdf",
  "mimetype": "application/pdf",
  "filetype": "pdf",
  "pretty_type": "PDF",
  "user": "U0000000001",
  "user_team": "T0000000001",
  "editable": false,
  "size": 24576,
  "mode": "hosted",
  "is_external": false,
  "external_type": "",
  "is_public": true,
  "public_url_shared": false,
  "display_as_bot": false,
  "url_private": "https://files.slack.com/files-pri/T0000000001-F00000000AA/quarterly-plan.pdf",
  "permalink": "https://example.slack.com/files/U0000000001/F00000000AA/quarterly-plan.pdf",
  "file_access": "visible"
}`

// slackDeniedFileObject is the shape the scan recorded for a file the token could not
// read: null metadata, file_access access_denied, and STILL an entry in the array.
// Presence detection has to survive it, which is the whole reason it is here.
//
// Kept field-for-field in step with the denied entry in
// cmd/testdata/conversations_replies_uploads.json. The two describe one observed file,
// and letting them disagree about which fields come back null would leave the repo with
// two contradictory records of it and no way to tell which is right.
const slackDeniedFileObject = `{"id":"F00000000BB","created":null,"timestamp":null,"name":null,` +
	`"title":null,"mimetype":null,"filetype":null,"pretty_type":null,"user":null,` +
	`"user_team":null,"mode":null,"file_access":"access_denied"}`

const (
	testChannel      = "C0000000001"
	testThreadParent = "1723600000.000100"
	// testRequestTimeout is generous: these tests assert behavior, not timing, and the
	// fakes answer immediately.
	testRequestTimeout = 30 * time.Second
)

func uploadMessage(ts string) string {
	return `{"type":"message","user":"U0000000001","text":"protect everything in this","ts":"` + ts + `","files":[` + slackFileObject + `]}`
}

func textMessage(ts string) string {
	return `{"type":"message","user":"U0000000001","text":"what can I reach?","ts":"` + ts + `"}`
}

func messagesBody(messages ...string) string {
	return `{"ok":true,"messages":[` + strings.Join(messages, ",") + `]}`
}

func listBody(channelIDs ...string) string {
	entries := make([]string, 0, len(channelIDs))
	for _, id := range channelIDs {
		entries = append(entries, `{"id":"`+id+`","is_member":true,"is_private":false}`)
	}
	return `{"ok":true,"channels":[` + strings.Join(entries, ",") + `]}`
}

// fakeSlack serves the three read methods from canned bodies keyed by method name, so
// each test states only the shape it cares about.
type fakeSlack struct {
	mu       sync.Mutex
	bodies   map[string]string
	handlers map[string]http.HandlerFunc
	calls    []string
}

func newFakeSlack(t *testing.T, bodies map[string]string) (*httptest.Server, *fakeSlack) {
	t.Helper()
	fake := &fakeSlack{bodies: bodies, handlers: map[string]http.HandlerFunc{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/")
		fake.mu.Lock()
		fake.calls = append(fake.calls, method)
		handler, hasHandler := fake.handlers[method]
		body, hasBody := fake.bodies[method]
		fake.mu.Unlock()
		switch {
		case hasHandler:
			handler(w, r)
		case hasBody:
			_, _ = w.Write([]byte(body))
		default:
			_, _ = w.Write([]byte(`{"ok":false,"error":"unexpected_method"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, fake
}

func (f *fakeSlack) setHandler(method string, handler http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[method] = handler
}

func (f *fakeSlack) callCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, call := range f.calls {
		if call == method {
			count++
		}
	}
	return count
}

func testScanConfig(srv *httptest.Server) *scanConfig {
	return &scanConfig{
		Token:             testToken,
		BaseURL:           srv.URL,
		UserAgent:         defaultUserAgent,
		ConversationTypes: defaultConversationTypes,
		MaxConversations:  defaultMaxConversations,
		MaxPages:          1,
		PageLimit:         defaultPageLimit,
		MaxThreads:        defaultMaxThreads,
		MinUploads:        1,
		HTTPClient:        newSlackHTTPClient(testRequestTimeout),
		StartedAt:         time.Unix(1723600000, 0).UTC(),
	}
}

// TestObserveMessageSeparatesShapeFromClassification pins the two readings apart. The
// shape column is what a reader that knows nothing about this app's rules can see; the
// classified column is what production would decide. A change that made the first
// column derive from the second would delete the only independent witness here.
func TestObserveMessageSeparatesShapeFromClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		message        string
		wantShape      filesShape
		wantEntries    int
		wantFileShare  bool
		wantClassified bool
	}{
		{"no files key", textMessage("100.1"), filesShapeAbsent, 0, false, false},
		{"populated array", uploadMessage("100.2"), filesShapePopulated, 1, false, true},
		{
			"denied file still occupies an entry",
			`{"user":"U1","ts":"100.3","files":[` + slackDeniedFileObject + `]}`,
			filesShapePopulated, 1, false, true,
		},
		{
			"two entries",
			`{"user":"U1","ts":"100.4","files":[` + slackFileObject + `,` + slackDeniedFileObject + `]}`,
			filesShapePopulated, 2, false, true,
		},
		{"empty array", `{"user":"U1","ts":"100.5","files":[]}`, filesShapeEmpty, 0, false, false},
		{"explicit null", `{"user":"U1","ts":"100.6","files":null}`, filesShapeNull, 0, false, false},
		{"object instead of array", `{"user":"U1","ts":"100.7","files":{"id":"F1"}}`, filesShapeOther, 0, false, true},
		{"string instead of array", `{"user":"U1","ts":"100.8","files":"F1"}`, filesShapeOther, 0, false, true},
		{
			"file_share with no files key",
			`{"user":"U1","ts":"100.9","subtype":"file_share"}`,
			filesShapeAbsent, 0, true, true,
		},
		{
			"file_share alongside a populated array",
			`{"user":"U1","ts":"101.0","subtype":"file_share","files":[` + slackFileObject + `]}`,
			filesShapePopulated, 1, true, true,
		},
		{
			"an unrelated subtype is not file_share",
			`{"user":"U1","ts":"101.1","subtype":"message_changed"}`,
			filesShapeAbsent, 0, false, false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := observeMessage(json.RawMessage(tt.message))
			if err != nil {
				t.Fatalf("observeMessage: %v", err)
			}
			if got.shape != tt.wantShape {
				t.Errorf("shape = %q, want %q", got.shape, tt.wantShape)
			}
			if got.entries != tt.wantEntries {
				t.Errorf("entries = %d, want %d", got.entries, tt.wantEntries)
			}
			if got.fileShareSubtype != tt.wantFileShare {
				t.Errorf("fileShareSubtype = %v, want %v", got.fileShareSubtype, tt.wantFileShare)
			}
			if got.classified != tt.wantClassified {
				t.Errorf("classified = %v, want %v", got.classified, tt.wantClassified)
			}
		})
	}
}

// TestObserveMessageDistinguishesAbsentFromNull pins the distinction the map decode
// exists for. Both land as a nil RawMessage at the classifier and are indistinguishable
// there, but "Slack stopped sending the array" and "this message had no files" are the
// two readings an operator has to tell apart, and only the shape column can.
func TestObserveMessageDistinguishesAbsentFromNull(t *testing.T) {
	t.Parallel()

	absent, err := observeMessage(json.RawMessage(`{"user":"U1","ts":"100.1"}`))
	if err != nil {
		t.Fatalf("observeMessage absent: %v", err)
	}
	explicitNull, err := observeMessage(json.RawMessage(`{"user":"U1","ts":"100.1","files":null}`))
	if err != nil {
		t.Fatalf("observeMessage null: %v", err)
	}
	if absent.classified != explicitNull.classified {
		t.Fatalf("the classifier is expected to agree on both: absent=%v null=%v", absent.classified, explicitNull.classified)
	}
	if absent.shape == explicitNull.shape {
		t.Errorf("shape = %q for both; the map decode exists precisely to separate them", absent.shape)
	}
}

func TestObserveMessageRejectsUnreadableJSON(t *testing.T) {
	t.Parallel()

	if _, err := observeMessage(json.RawMessage(`{"unterminated`)); err == nil {
		t.Error("an unreadable message must be reported, not silently observed as text-only")
	}
}

// TestMissedUploadIsOneDirectional pins the asymmetry. A populated array the classifier
// called text-only is a defect; every other combination is a judgment call this command
// does not second-guess — most importantly file_share with no array, which is the
// EVENT surface's normal shape and must never read as a disagreement here.
func TestMissedUploadIsOneDirectional(t *testing.T) {
	t.Parallel()

	if !(messageObservation{shape: filesShapePopulated, entries: 1}).missedUpload() {
		t.Error("a populated array the classifier called text-only is the one real disagreement")
	}
	notDefects := []messageObservation{
		{shape: filesShapePopulated, entries: 1, classified: true},
		{shape: filesShapeAbsent, fileShareSubtype: true, classified: true},
		{shape: filesShapeAbsent},
		{shape: filesShapeNull},
		{shape: filesShapeEmpty},
		{shape: filesShapeOther, classified: true},
		{shape: filesShapeOther},
	}
	for _, observation := range notDefects {
		if observation.missedUpload() {
			t.Errorf("%+v must not read as a missed upload", observation)
		}
	}
}

func TestSurfaceTallyBucketsEveryShape(t *testing.T) {
	t.Parallel()

	tally := surfaceTally{Surface: surfaceHistory}
	messages := []string{
		textMessage("100.1"),
		uploadMessage("100.2"),
		`{"user":"U1","ts":"100.3","files":[` + slackFileObject + `,` + slackDeniedFileObject + `]}`,
		`{"user":"U1","ts":"100.4","files":[]}`,
		`{"user":"U1","ts":"100.5","files":null}`,
		`{"user":"U1","ts":"100.6","files":{"id":"F1"}}`,
		`{"user":"U1","ts":"100.7","subtype":"file_share"}`,
		`{"unterminated`,
	}
	raw := make([]json.RawMessage, 0, len(messages))
	for _, message := range messages {
		raw = append(raw, json.RawMessage(message))
	}
	tallyMessages(testChannel, raw, &tally, map[string]struct{}{})

	want := surfaceTally{
		Surface: surfaceHistory, Messages: 7, FilesKeyPresent: 5, PopulatedArrays: 2,
		FileEntries: 3, EmptyArrays: 1, NullFiles: 1, UncountableShapes: 1,
		FileShareSubtypes: 1, ClassifiedUploads: 4, MissedUploads: 0, DecodeFailures: 1,
	}
	if tally != want {
		t.Errorf("tally  = %+v\nwanted = %+v", tally, want)
	}
}

func TestEvaluateContract(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		history           surfaceTally
		replies           surfaceTally
		expected          []expectationResult
		minUploads        int
		strictUncountable bool
		conversationsRead int
		distinctUploads   int
		truncated         bool
		wantHolds         bool
		wantFailure       string
	}{
		{
			name: "uploads found on both surfaces", history: surfaceTally{ClassifiedUploads: 3},
			replies: surfaceTally{ClassifiedUploads: 2}, minUploads: 1, conversationsRead: 2,
			distinctUploads: 4, wantHolds: true,
		},
		{
			name: "no conversation could be read", minUploads: 1, conversationsRead: 0,
			wantFailure: "no conversation could be read",
		},
		{
			// A budget that ran out looks exactly like a wire-format break in the counts.
			// Only the coverage flag can tell them apart, and blaming Slack for a timeout
			// sends the operator to the wrong place entirely.
			name: "a truncated scan blames the budget, not Slack", history: surfaceTally{Messages: 400},
			minUploads: 1, conversationsRead: 2, truncated: true,
			wantFailure: "did not finish within -timeout",
		},
		{
			name:              "nothing classified is the files array disappearing",
			history:           surfaceTally{Messages: 400},
			minUploads:        1,
			conversationsRead: 3,
			wantFailure:       "the files array has stopped arriving",
		},
		{
			name:              "a populated array the classifier missed",
			history:           surfaceTally{ClassifiedUploads: 5, MissedUploads: 2},
			minUploads:        1,
			conversationsRead: 1,
			distinctUploads:   5,
			wantFailure:       "did not report as an upload",
		},
		{
			name:              "an unrecognized shape only fails under strict",
			history:           surfaceTally{ClassifiedUploads: 5, UncountableShapes: 1},
			minUploads:        1,
			conversationsRead: 1,
			distinctUploads:   5,
			wantHolds:         true,
		},
		{
			name:              "an unrecognized shape under strict",
			history:           surfaceTally{ClassifiedUploads: 5, UncountableShapes: 1},
			minUploads:        1,
			strictUncountable: true,
			conversationsRead: 1,
			distinctUploads:   5,
			wantFailure:       "Slack changed the wire format",
		},
		{
			name:              "a named ground-truth message the classifier missed",
			history:           surfaceTally{ClassifiedUploads: 5},
			expected:          []expectationResult{{messageRef: messageRef{Channel: testChannel, TS: "100.1"}, Found: true}},
			minUploads:        1,
			conversationsRead: 1,
			distinctUploads:   5,
			wantFailure:       "was found but not classified",
		},
		{
			// The replies half of every verdict sum needs its own row: a wire-format
			// break visible ONLY on conversations.replies is exactly what this command
			// was built to see, and summing only the history side would hide it.
			name:              "a missed upload on the replies surface alone",
			history:           surfaceTally{ClassifiedUploads: 5},
			replies:           surfaceTally{ClassifiedUploads: 2, MissedUploads: 1},
			minUploads:        1,
			conversationsRead: 1,
			distinctUploads:   6,
			wantFailure:       "did not report as an upload",
		},
		{
			// Same reasoning for the uncountable-shape sum.
			name:              "an unrecognized shape on the replies surface alone",
			history:           surfaceTally{ClassifiedUploads: 5},
			replies:           surfaceTally{ClassifiedUploads: 2, UncountableShapes: 1},
			minUploads:        1,
			strictUncountable: true,
			conversationsRead: 1,
			distinctUploads:   6,
			wantFailure:       "Slack changed the wire format",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &scanConfig{MinUploads: tt.minUploads, StrictUncountable: tt.strictUncountable}
			result := &scanResult{History: tt.history, Replies: tt.replies, ExpectedUploads: tt.expected}
			verdict := evaluateContract(cfg, result, scanCoverage{
				conversationsRead: tt.conversationsRead,
				distinctUploads:   tt.distinctUploads,
				truncated:         tt.truncated,
			})
			if verdict.Holds != tt.wantHolds {
				t.Fatalf("holds = %v, want %v (failures: %v)", verdict.Holds, tt.wantHolds, verdict.Failures)
			}
			// Assert the evidence fields, not just Holds: dropping either replies term
			// from the sums changes only these, and a Holds-only assertion sails past it.
			if verdict.DistinctUploads != tt.distinctUploads {
				t.Errorf("distinct uploads = %d, want %d", verdict.DistinctUploads, tt.distinctUploads)
			}
			if verdict.MissedUploads != tt.history.MissedUploads+tt.replies.MissedUploads {
				t.Errorf("missed uploads = %d, want both surfaces totalled", verdict.MissedUploads)
			}
			if verdict.UncountableShapes != tt.history.UncountableShapes+tt.replies.UncountableShapes {
				t.Errorf("uncountable shapes = %d, want both surfaces totalled", verdict.UncountableShapes)
			}
			if verdict.MinUploads != tt.minUploads {
				t.Errorf("min uploads = %d, want %d carried into the report", verdict.MinUploads, tt.minUploads)
			}
			if tt.wantHolds {
				return
			}
			joined := strings.Join(verdict.Failures, "; ")
			if !strings.Contains(joined, tt.wantFailure) {
				t.Errorf("failures = %q, want one containing %q", joined, tt.wantFailure)
			}
		})
	}
}

// TestEvaluateContractSaysNothingAboutUploadsWhenItReadNothing pins that an
// unreadable workspace does not also report the min-uploads failure. Both are true at
// zero, but only the first is the diagnosis; stacking the second sends an operator
// looking at Slack's wire format when the answer is a missing scope.
func TestEvaluateContractSaysNothingAboutUploadsWhenItReadNothing(t *testing.T) {
	t.Parallel()

	verdict := evaluateContract(&scanConfig{MinUploads: 1}, &scanResult{}, scanCoverage{})
	if verdict.Holds {
		t.Fatal("a scan that read nothing must not report a holding contract")
	}
	if len(verdict.Failures) != 1 {
		t.Fatalf("failures = %v, want only the unreadable-workspace one", verdict.Failures)
	}
}

// TestRunScanMeasuresBothSurfaces covers the caveat the TODO leaves open: production
// reads conversations.replies while the recorded measurement read conversations.history,
// and "same message objects, same API family" was an assumption. This scan tallies both
// and keeps them apart, so the next run answers it with numbers.
func TestRunScanMeasuresBothSurfaces(t *testing.T) {
	t.Parallel()

	srv, fake := newFakeSlack(t, map[string]string{
		methodConversationsList: listBody(testChannel),
		surfaceHistory: messagesBody(
			textMessage("100.1"),
			`{"type":"message","user":"U1","text":"thread root","ts":"`+testThreadParent+`","thread_ts":"`+testThreadParent+`","reply_count":2}`,
			uploadMessage("100.3"),
		),
		surfaceReplies: messagesBody(
			`{"type":"message","user":"U1","text":"thread root","ts":"`+testThreadParent+`","thread_ts":"`+testThreadParent+`"}`,
			uploadMessage("100.4"),
		),
	})

	result, err := runScan(context.Background(), testScanConfig(srv))
	if err != nil {
		t.Fatalf("runScan: %v", err)
	}
	if !result.Contract.Holds {
		t.Fatalf("contract must hold: %v", result.Contract.Failures)
	}
	if result.History.Messages != 3 || result.History.ClassifiedUploads != 1 || result.History.Conversations != 1 {
		t.Errorf("history = %+v", result.History)
	}
	if result.Replies.Messages != 2 || result.Replies.ClassifiedUploads != 1 || result.Replies.Conversations != 1 {
		t.Errorf("replies = %+v", result.Replies)
	}
	// Both surfaces saw an upload the other did not, so the distinct count is 2 — and the
	// thread root they share is not what makes it 2. TestRunScanCountsAThreadRootOnce
	// pins the deduplication itself.
	if result.Contract.DistinctUploads != 2 {
		t.Errorf("distinct uploads = %d, want one per surface-unique message", result.Contract.DistinctUploads)
	}
	if got := fake.callCount(surfaceReplies); got != 1 {
		t.Errorf("conversations.replies calls = %d, want the one thread with replies", got)
	}
	if len(result.Conversations) != 1 || result.Conversations[0].ClassifiedUploads != 2 || result.Conversations[0].ThreadsSampled != 1 {
		t.Errorf("conversations = %+v", result.Conversations)
	}
}

// TestRunScanDetectsTheFilesArrayDisappearing is the reason this command exists. Slack
// keeps sending the same file-bearing messages, minus the `files` key — the rot
// SlackMessageHasUpload's TODO(upstream-contract) names, which every unit test in the
// repo sails straight past because they supply the field themselves.
func TestRunScanDetectsTheFilesArrayDisappearing(t *testing.T) {
	t.Parallel()

	srv, _ := newFakeSlack(t, map[string]string{
		methodConversationsList: listBody(testChannel),
		// The captions are still here. Only the files array is gone, and with it the
		// only signal this surface has — the subtype stays "" exactly as measured.
		surfaceHistory: messagesBody(
			`{"type":"message","user":"U1","text":"protect everything in this","ts":"100.1"}`,
			`{"type":"message","user":"U1","text":"and this one too","ts":"100.2"}`,
		),
	})

	result, err := runScan(context.Background(), testScanConfig(srv))
	if err == nil {
		t.Fatal("a workspace whose files arrays stopped arriving must fail the smoke")
	}
	if result.Contract.Holds {
		t.Fatal("contract must not hold")
	}
	if result.History.Messages != 2 {
		t.Fatalf("the scan must still have read the messages: %+v", result.History)
	}
	joined := strings.Join(result.Contract.Failures, "; ")
	if !strings.Contains(joined, "the files array has stopped arriving") {
		t.Errorf("failures = %q, want the files-array diagnosis", joined)
	}
	// The counts are the diagnosis, so they have to survive the failure and reach the
	// report rather than being swallowed with the error.
	if result.History.FilesKeyPresent != 0 || result.History.FileShareSubtypes != 0 {
		t.Errorf("history = %+v, want the shape evidence recorded alongside the failure", result.History)
	}
}

// TestRunScanReportsUncountableShapeWithoutFailingByDefault covers the history-surface
// twin of claimMediaNotice's alertable files_field_present=true / files_visible=0 pair.
// It is reported by default and fatal only under -strict-uncountable, matching
// slack-dm-smoke's -strict-direct-user-probe: a shape change worth seeing is not
// automatically a reason to fail an operator's scan.
func TestRunScanReportsUncountableShapeWithoutFailingByDefault(t *testing.T) {
	t.Parallel()

	bodies := map[string]string{
		methodConversationsList: listBody(testChannel),
		surfaceHistory: messagesBody(
			uploadMessage("100.1"),
			`{"type":"message","user":"U1","text":"caption","ts":"100.2","files":{"id":"F1"}}`,
		),
	}
	srv, _ := newFakeSlack(t, bodies)

	cfg := testScanConfig(srv)
	result, err := runScan(context.Background(), cfg)
	if err != nil {
		t.Fatalf("runScan: %v", err)
	}
	if result.History.UncountableShapes != 1 {
		t.Errorf("uncountable shapes = %d, want 1", result.History.UncountableShapes)
	}
	if !result.Contract.Holds {
		t.Errorf("an unrecognized shape must not fail the default scan: %v", result.Contract.Failures)
	}

	strictSrv, _ := newFakeSlack(t, bodies)
	strictCfg := testScanConfig(strictSrv)
	strictCfg.StrictUncountable = true
	strictResult, err := runScan(context.Background(), strictCfg)
	if err == nil {
		t.Fatal("-strict-uncountable must fail on an unrecognized shape")
	}
	if strictResult.Contract.Holds {
		t.Error("contract must not hold under -strict-uncountable")
	}
}

// TestRunScanContinuesPastAnUnreadableConversation pins that one channel the bot was
// removed from does not decide the measurement. The failure is recorded beside the
// conversation, and the readable ones still count.
func TestRunScanContinuesPastAnUnreadableConversation(t *testing.T) {
	t.Parallel()

	const otherChannel = "C0000000002"
	srv, fake := newFakeSlack(t, map[string]string{methodConversationsList: listBody(testChannel, otherChannel)})
	fake.setHandler(surfaceHistory, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("channel") == testChannel {
			_, _ = w.Write([]byte(`{"ok":false,"error":"not_in_channel"}`))
			return
		}
		_, _ = w.Write([]byte(messagesBody(uploadMessage("100.1"))))
	})

	result, err := runScan(context.Background(), testScanConfig(srv))
	if err != nil {
		t.Fatalf("runScan: %v", err)
	}
	if len(result.Conversations) != 2 {
		t.Fatalf("conversations = %+v, want both recorded", result.Conversations)
	}
	if !strings.Contains(result.Conversations[0].Error, "not_in_channel") {
		t.Errorf("first conversation error = %q, want the Slack reason preserved", result.Conversations[0].Error)
	}
	if result.Conversations[1].Error != "" || result.Conversations[1].ClassifiedUploads != 1 {
		t.Errorf("second conversation = %+v, want a clean read", result.Conversations[1])
	}
	if !result.Contract.Holds {
		t.Errorf("one unreadable conversation must not fail the scan: %v", result.Contract.Failures)
	}
}

// TestRunScanFailsWhenEveryConversationIsUnreadable pins the diagnosis that separates a
// scope or membership problem from a wire-format one. Both bottom out at zero uploads;
// only one of them is about the contract.
func TestRunScanFailsWhenEveryConversationIsUnreadable(t *testing.T) {
	t.Parallel()

	srv, _ := newFakeSlack(t, map[string]string{
		methodConversationsList: listBody(testChannel),
		surfaceHistory:          `{"ok":false,"error":"missing_scope","needed":"channels:history","provided":"chat:write"}`,
	})

	result, err := runScan(context.Background(), testScanConfig(srv))
	if err == nil {
		t.Fatal("a scan that read nothing must fail")
	}
	joined := strings.Join(result.Contract.Failures, "; ")
	if !strings.Contains(joined, "no conversation could be read") {
		t.Errorf("failures = %q, want the unreadable-workspace diagnosis", joined)
	}
	if strings.Contains(joined, "the files array has stopped arriving") {
		t.Errorf("failures = %q must not also blame the wire format", joined)
	}
	if !strings.Contains(result.Conversations[0].Error, "channels:history") {
		t.Errorf("conversation error = %q, want Slack's needed scope carried through", result.Conversations[0].Error)
	}
}

// TestRunScanFailsWhenListingFindsNothing pins that discovery failure is fatal up
// front rather than silently producing an empty, passing scan.
func TestRunScanFailsWhenListingFindsNothing(t *testing.T) {
	t.Parallel()

	srv, _ := newFakeSlack(t, map[string]string{methodConversationsList: `{"ok":true,"channels":[]}`})
	if _, err := runScan(context.Background(), testScanConfig(srv)); err == nil {
		t.Fatal("a workspace with no readable conversation must fail rather than report a holding contract")
	}
}

// TestRunScanSkipsChannelsTheBotIsNotIn pins the discovery filter: conversations.list
// returns channels the token can see but is not a member of, and reading those would
// spend the scan's budget on guaranteed not_in_channel errors.
func TestRunScanSkipsChannelsTheBotIsNotIn(t *testing.T) {
	t.Parallel()

	srv, fake := newFakeSlack(t, map[string]string{
		methodConversationsList: `{"ok":true,"channels":[` +
			`{"id":"C0000000009","is_member":false},` +
			`{"id":"C0000000010","is_member":true,"is_archived":true},` +
			`{"id":"` + testChannel + `","is_member":true},` +
			`{"id":"D0000000001","is_im":true}]}`,
		surfaceHistory: messagesBody(uploadMessage("100.1")),
	})

	result, err := runScan(context.Background(), testScanConfig(srv))
	if err != nil {
		t.Fatalf("runScan: %v", err)
	}
	if got := fake.callCount(surfaceHistory); got != 2 {
		t.Errorf("history calls = %d, want only the member channel and the IM", got)
	}
	if len(result.Conversations) != 2 {
		t.Fatalf("conversations = %+v", result.Conversations)
	}
	if result.Conversations[1].Kind != "im" {
		t.Errorf("second conversation kind = %q, want im", result.Conversations[1].Kind)
	}
}

// TestRunScanHonorsExplicitChannels pins that -channels skips discovery entirely, which
// is what an operator uses when the token cannot list but can read a known channel.
func TestRunScanHonorsExplicitChannels(t *testing.T) {
	t.Parallel()

	srv, fake := newFakeSlack(t, map[string]string{surfaceHistory: messagesBody(uploadMessage("100.1"))})
	cfg := testScanConfig(srv)
	cfg.Channels = []string{testChannel}

	if _, err := runScan(context.Background(), cfg); err != nil {
		t.Fatalf("runScan: %v", err)
	}
	if got := fake.callCount(methodConversationsList); got != 0 {
		t.Errorf("conversations.list calls = %d, want none", got)
	}
}

// TestCheckExpectationFallsBackToTheThread pins the lookup an operator actually needs:
// conversations.history does not return thread replies, and an upload posted into a
// thread is exactly the kind of message someone names with -expect-upload.
func TestCheckExpectationFallsBackToTheThread(t *testing.T) {
	t.Parallel()

	const replyTS = "1723600000.000200"
	srv, fake := newFakeSlack(t, map[string]string{
		methodConversationsList: listBody(testChannel),
		surfaceReplies: messagesBody(
			`{"user":"U1","text":"thread root","ts":"`+testThreadParent+`"}`,
			`{"user":"U1","text":"caption","ts":"`+replyTS+`","files":[`+slackFileObject+`]}`,
		),
	})
	fake.setHandler(surfaceHistory, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("latest") == replyTS {
			// What Slack really returns for a threaded reply asked of history.
			_, _ = w.Write([]byte(`{"ok":true,"messages":[]}`))
			return
		}
		_, _ = w.Write([]byte(messagesBody(uploadMessage("100.1"))))
	})

	cfg := testScanConfig(srv)
	cfg.MaxThreads = 0
	cfg.ExpectUploads = messageRefList{{Channel: testChannel, TS: replyTS}}

	result, err := runScan(context.Background(), cfg)
	if err != nil {
		t.Fatalf("runScan: %v", err)
	}
	if len(result.ExpectedUploads) != 1 {
		t.Fatalf("expected uploads = %+v", result.ExpectedUploads)
	}
	got := result.ExpectedUploads[0]
	if !got.Found || !got.Classified || got.Shape != string(filesShapePopulated) {
		t.Errorf("expectation = %+v, want the threaded upload found and classified", got)
	}
}

// TestCheckExpectationFailsTheScanWhenGroundTruthIsMissed pins the one check here whose
// oracle is a human rather than another reading of the same bytes.
func TestCheckExpectationFailsTheScanWhenGroundTruthIsMissed(t *testing.T) {
	t.Parallel()

	srv, fake := newFakeSlack(t, map[string]string{
		methodConversationsList: listBody(testChannel),
		surfaceReplies:          `{"ok":true,"messages":[]}`,
	})
	fake.setHandler(surfaceHistory, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("latest") != "" {
			// The operator saw a file on this message; Slack no longer returns it.
			_, _ = w.Write([]byte(`{"ok":true,"messages":[]}`))
			return
		}
		_, _ = w.Write([]byte(messagesBody(uploadMessage("100.1"))))
	})
	cfg := testScanConfig(srv)
	cfg.MaxThreads = 0
	cfg.ExpectUploads = messageRefList{{Channel: testChannel, TS: "999.9"}}

	result, err := runScan(context.Background(), cfg)
	if err == nil {
		t.Fatal("a named upload the classifier does not see must fail the scan")
	}
	if len(result.ExpectedUploads) != 1 || result.ExpectedUploads[0].Found {
		t.Fatalf("expected uploads = %+v", result.ExpectedUploads)
	}
	if !strings.Contains(strings.Join(result.Contract.Failures, "; "), "-expect-upload "+testChannel+":999.9") {
		t.Errorf("failures = %v, want the named message", result.Contract.Failures)
	}
}

func TestSlackClientRetriesOnceAfterRateLimit(t *testing.T) {
	t.Parallel()

	var attempts int
	var mu sync.Mutex
	srv, fake := newFakeSlack(t, nil)
	fake.setHandler(surfaceHistory, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		attempts++
		attempt := attempts
		mu.Unlock()
		if attempt == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(messagesBody(uploadMessage("100.1"))))
	})

	var slept time.Duration
	client := &slackClient{
		token: testToken, baseURL: srv.URL, userAgent: defaultUserAgent, httpClient: newSlackHTTPClient(testRequestTimeout),
		sleep: func(_ context.Context, d time.Duration) error { slept = d; return nil },
	}
	var out slackMessagesResponse
	if err := client.get(context.Background(), surfaceHistory, nil, &out); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(out.Messages) != 1 {
		t.Errorf("messages = %d, want the retry to have succeeded", len(out.Messages))
	}
	if slept != time.Second {
		t.Errorf("slept = %s, want Slack's Retry-After", slept)
	}
}

func TestSlackClientRefusesAnUnreasonableRetryAfter(t *testing.T) {
	t.Parallel()

	srv, fake := newFakeSlack(t, nil)
	fake.setHandler(surfaceHistory, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	client := &slackClient{
		token: testToken, baseURL: srv.URL, userAgent: defaultUserAgent, httpClient: newSlackHTTPClient(testRequestTimeout),
		sleep: func(context.Context, time.Duration) error {
			t.Error("a Retry-After past the cap must not park the scan")
			return nil
		},
	}
	var out slackMessagesResponse
	err := client.get(context.Background(), surfaceHistory, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "exceeds the") {
		t.Errorf("err = %v, want the capped-Retry-After refusal", err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		header string
		want   time.Duration
	}{
		{"5", 5 * time.Second},
		{" 5 ", 5 * time.Second},
		// Slack omits or garbles the header often enough that a floor beats a guess.
		{"", time.Second},
		{"soon", time.Second},
		{"0", time.Second},
		{"-3", time.Second},
	}
	for _, tt := range tests {
		if got := parseRetryAfter(tt.header); got != tt.want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", tt.header, got, tt.want)
		}
	}
}

func TestThreadParentsPicksOnlyRootsWithReplies(t *testing.T) {
	t.Parallel()

	messages := []json.RawMessage{
		json.RawMessage(textMessage("100.1")),
		json.RawMessage(`{"ts":"100.2","thread_ts":"100.2","reply_count":3}`),
		// A reply, not a root: its thread_ts points elsewhere.
		json.RawMessage(`{"ts":"100.3","thread_ts":"100.2"}`),
		json.RawMessage(`{"ts":"100.4","reply_count":0}`),
		json.RawMessage(`{"ts":"100.5","thread_ts":"100.5","reply_count":1}`),
	}
	got := threadParents(messages, 5)
	if len(got) != 2 || got[0] != "100.2" || got[1] != "100.5" {
		t.Errorf("threadParents = %v, want the two roots with replies", got)
	}
	if limited := threadParents(messages, 1); len(limited) != 1 {
		t.Errorf("threadParents with limit 1 = %v", limited)
	}
	if none := threadParents(messages, 0); none != nil {
		t.Errorf("threadParents with limit 0 = %v, want nil", none)
	}
}

// TestScanResultCarriesNoMessageContent pins the field discipline the package comment
// promises. Every count here comes from a message whose text, file name and mimetype
// are distinctive, and none of them may appear in the encoded report.
func TestScanResultCarriesNoMessageContent(t *testing.T) {
	t.Parallel()

	srv, _ := newFakeSlack(t, map[string]string{
		methodConversationsList: listBody(testChannel),
		surfaceHistory:          messagesBody(uploadMessage("100.1")),
	})
	result, err := runScan(context.Background(), testScanConfig(srv))
	if err != nil {
		t.Fatalf("runScan: %v", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	for _, content := range []string{"quarterly-plan", "application/pdf", "protect everything", "files.slack.com", "F00000000AA"} {
		if strings.Contains(string(encoded), content) {
			t.Errorf("report leaked %q; the report carries counts and Slack conversation IDs only", content)
		}
	}
}

// TestRunScanBlamesTheBudgetWhenItRunsOut is the end-to-end half of the coverage rule.
// A scan cut short mid-workspace produces exactly the counts a vanished files array
// produces — text-only messages and nothing classified — and the only thing that can
// tell the two apart is knowing the scan never finished.
func TestRunScanBlamesTheBudgetWhenItRunsOut(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	const secondChannel = "C0000000002"
	srv, fake := newFakeSlack(t, map[string]string{methodConversationsList: listBody(testChannel, secondChannel)})
	var mu sync.Mutex
	var calls int
	fake.setHandler(surfaceHistory, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if !first {
			cancel()
			return
		}
		_, _ = w.Write([]byte(messagesBody(textMessage("100.1"))))
	})

	cfg := testScanConfig(srv)
	cfg.ExpectUploads = messageRefList{{Channel: testChannel, TS: "100.1"}}
	result, err := runScan(ctx, cfg)
	if err == nil {
		t.Fatal("a scan that ran out of budget must not report success")
	}
	joined := strings.Join(result.Contract.Failures, "; ")
	if !strings.Contains(joined, "did not finish within -timeout") {
		t.Errorf("failures = %q, want the budget diagnosis", joined)
	}
	if strings.Contains(joined, "the files array has stopped arriving") {
		t.Errorf("failures = %q must not blame Slack's wire format for a timeout", joined)
	}
	// The named ground truth is never even looked up: with the budget gone every lookup
	// would error, and each error would be reported as a classifier miss.
	if len(result.ExpectedUploads) != 0 {
		t.Errorf("expected uploads = %+v, want none attempted on a truncated scan", result.ExpectedUploads)
	}
}

// TestRunScanKeepsAHistoryReadWhenTheThreadSampleFails pins that the supplementary
// replies sample cannot void what history already proved. Every conversation here reads
// its history cleanly and then fails its thread sample — the shape a rate-limited run
// takes, since the replies calls come after history in each conversation. Counting those
// conversations as unread would report "no conversation could be read" beside a non-zero
// upload count: a verdict that contradicts its own evidence.
func TestRunScanKeepsAHistoryReadWhenTheThreadSampleFails(t *testing.T) {
	t.Parallel()

	srv, fake := newFakeSlack(t, map[string]string{
		methodConversationsList: listBody(testChannel),
		surfaceHistory: messagesBody(
			uploadMessage("100.1"),
			`{"type":"message","user":"U1","text":"root","ts":"`+testThreadParent+`","thread_ts":"`+testThreadParent+`","reply_count":2}`,
		),
		surfaceReplies: `{"ok":false,"error":"ratelimited"}`,
	})

	result, err := runScan(context.Background(), testScanConfig(srv))
	if err != nil {
		t.Fatalf("a failed thread sample must not fail a scan whose history read: %v", err)
	}
	if !result.Contract.Holds {
		t.Fatalf("contract must hold: %v", result.Contract.Failures)
	}
	if result.History.ClassifiedUploads != 1 || result.History.Conversations != 1 {
		t.Errorf("history = %+v, want the surface counted as read", result.History)
	}
	if result.Replies.Conversations != 0 {
		t.Errorf("replies conversations = %d, want the failed sample uncounted", result.Replies.Conversations)
	}
	// The operator still has to see that the sample failed, and which surface failed it.
	if !strings.Contains(result.Conversations[0].Error, "ratelimited") ||
		!strings.Contains(result.Conversations[0].Error, surfaceReplies) {
		t.Errorf("conversation error = %q, want the replies failure named", result.Conversations[0].Error)
	}
	if got := fake.callCount(surfaceReplies); got != 1 {
		t.Errorf("replies calls = %d, want the one sampled thread", got)
	}
}

// TestRunScanFailsWhenEveryHistorySurfaceIsUnreadable is the companion to the test
// above: history itself failing IS the unreadable case, and must still say so.
func TestRunScanFailsWhenEveryHistorySurfaceIsUnreadable(t *testing.T) {
	t.Parallel()

	srv, _ := newFakeSlack(t, map[string]string{
		methodConversationsList: listBody(testChannel),
		surfaceHistory:          `{"ok":false,"error":"ratelimited"}`,
	})
	result, err := runScan(context.Background(), testScanConfig(srv))
	if err == nil {
		t.Fatal("a scan whose history reads all failed must fail")
	}
	if !strings.Contains(strings.Join(result.Contract.Failures, "; "), "no conversation could be read") {
		t.Errorf("failures = %v, want the unreadable-workspace diagnosis", result.Contract.Failures)
	}
}

// TestEvaluateContractSeparatesUnverifiableFromUnclassified pins that a lookup this
// command could not perform is not reported as a classifier defect. Both fail the run —
// the operator asked for a verdict — but only one of them is evidence about the
// classifier, and blaming it for a transient fetch error sends the next reader hunting a
// bug the run never tested for.
func TestEvaluateContractSeparatesUnverifiableFromUnclassified(t *testing.T) {
	t.Parallel()

	result := &scanResult{
		History: surfaceTally{ClassifiedUploads: 3},
		ExpectedUploads: []expectationResult{
			{messageRef: messageRef{Channel: testChannel, TS: "100.1"}, Error: "conversations.history: ratelimited"},
			{messageRef: messageRef{Channel: testChannel, TS: "100.2"}, Found: true},
		},
	}
	verdict := evaluateContract(&scanConfig{MinUploads: 1}, result, scanCoverage{conversationsRead: 1, distinctUploads: 3})
	if verdict.Holds || len(verdict.Failures) != 2 {
		t.Fatalf("verdict = %+v, want both expectations failing", verdict)
	}
	if !strings.Contains(verdict.Failures[0], "could not be verified") ||
		!strings.Contains(verdict.Failures[0], "ratelimited") {
		t.Errorf("failure[0] = %q, want the unverifiable wording carrying the cause", verdict.Failures[0])
	}
	if !strings.Contains(verdict.Failures[1], "found but not classified") {
		t.Errorf("failure[1] = %q, want the classifier wording", verdict.Failures[1])
	}
}

// TestSurfaceTallyCountsAMissedUpload wires the one counter no amount of real JSON can
// reach. Against today's classifier a populated array always classifies, so
// observeMessage cannot produce this observation — which is exactly why the counter
// needs a direct test. Without it, deleting the increment leaves every other test green
// and the "two independent readings disagreed" claim has no wiring at all.
func TestSurfaceTallyCountsAMissedUpload(t *testing.T) {
	t.Parallel()

	tally := surfaceTally{Surface: surfaceHistory}
	tally.add(messageObservation{shape: filesShapePopulated, entries: 2, classified: false})
	want := surfaceTally{
		Surface: surfaceHistory, Messages: 1, FilesKeyPresent: 1,
		PopulatedArrays: 1, FileEntries: 2, MissedUploads: 1,
	}
	if tally != want {
		t.Errorf("tally  = %+v\nwanted = %+v", tally, want)
	}
}

// TestReadHistoryFollowsCursorsAndStopsAtMaxPages pins both halves of pagination: pages
// after the first are tallied, and MaxPages actually stops a cursor that never ends.
// Without the second half the bound is documented but never observed to bound anything.
func TestReadHistoryFollowsCursorsAndStopsAtMaxPages(t *testing.T) {
	t.Parallel()

	t.Run("follows a cursor to the last page", func(t *testing.T) {
		t.Parallel()
		srv, fake := newFakeSlack(t, map[string]string{methodConversationsList: listBody(testChannel)})
		fake.setHandler(surfaceHistory, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("cursor") == "" {
				_, _ = w.Write([]byte(`{"ok":true,"messages":[` + uploadMessage("100.1") +
					`],"response_metadata":{"next_cursor":"page two"}}`))
				return
			}
			_, _ = w.Write([]byte(messagesBody(uploadMessage("100.2"))))
		})
		cfg := testScanConfig(srv)
		cfg.MaxPages = 3
		result, err := runScan(context.Background(), cfg)
		if err != nil {
			t.Fatalf("runScan: %v", err)
		}
		if got := fake.callCount(surfaceHistory); got != 2 {
			t.Errorf("history calls = %d, want the cursor followed once then stopped", got)
		}
		if result.History.Messages != 2 || result.History.ClassifiedUploads != 2 {
			t.Errorf("history = %+v, want both pages tallied", result.History)
		}
	})

	t.Run("stops an endless cursor at MaxPages", func(t *testing.T) {
		t.Parallel()
		srv, fake := newFakeSlack(t, map[string]string{
			methodConversationsList: listBody(testChannel),
			surfaceHistory: `{"ok":true,"messages":[` + uploadMessage("100.1") +
				`],"response_metadata":{"next_cursor":"always more"}}`,
		})
		cfg := testScanConfig(srv)
		cfg.MaxPages = 2
		if _, err := runScan(context.Background(), cfg); err != nil {
			t.Fatalf("runScan: %v", err)
		}
		if got := fake.callCount(surfaceHistory); got != 2 {
			t.Errorf("history calls = %d, want MaxPages to stop the loop", got)
		}
	})
}

// TestRunScanSkipRepliesLeavesTheSurfaceUnmeasured pins the flag against its own
// condition. Every other test reaches the same skip through MaxThreads == 0, so
// inverting this half of the && would go unnoticed.
func TestRunScanSkipRepliesLeavesTheSurfaceUnmeasured(t *testing.T) {
	t.Parallel()

	srv, fake := newFakeSlack(t, map[string]string{
		methodConversationsList: listBody(testChannel),
		surfaceHistory: messagesBody(
			uploadMessage("100.1"),
			`{"type":"message","user":"U1","text":"root","ts":"`+testThreadParent+`","thread_ts":"`+testThreadParent+`","reply_count":4}`,
		),
		surfaceReplies: messagesBody(uploadMessage("100.2")),
	})
	cfg := testScanConfig(srv)
	cfg.SkipReplies = true

	result, err := runScan(context.Background(), cfg)
	if err != nil {
		t.Fatalf("runScan: %v", err)
	}
	if got := fake.callCount(surfaceReplies); got != 0 {
		t.Errorf("replies calls = %d, want none under -skip-replies", got)
	}
	if result.Replies.Messages != 0 || result.Replies.Conversations != 0 {
		t.Errorf("replies = %+v, want the surface untouched", result.Replies)
	}
	// The zeros above are ambiguous on their own — skipped, no threads, or every call
	// failed all look identical — so the report has to say which.
	if !result.Bounds.SkipReplies {
		t.Error("the report must record that the replies surface was skipped, not just show zeros")
	}
	if result.History.ClassifiedUploads != 1 {
		t.Errorf("history = %+v, want the history surface still measured", result.History)
	}
}

// TestNewSlackHTTPClientDoesNotFollowRedirects pins a security control that otherwise
// never executes. -base-url is operator-supplied and the bearer token rides on every
// request; restoring Go's default redirect following would replay it down a chain this
// command never inspected.
func TestNewSlackHTTPClientDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	var followed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed = true
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	client := newSlackHTTPClient(time.Second)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/conversations.history", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if followed {
		t.Error("the client followed a redirect; the bearer token must not be replayed down an uninspected chain")
	}
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want the 302 surfaced rather than followed", resp.StatusCode)
	}
}

// TestRunScanCountsAThreadRootOnce pins the deduplication the tripwire rests on.
// conversations.replies returns the thread parent as its first message and
// conversations.history already returned it, so summing the two surfaces reports two
// uploads for a workspace that contains one — and -min-uploads, which the runbook tells
// operators to raise when they know how many to expect, would pass on the phantom.
func TestRunScanCountsAThreadRootOnce(t *testing.T) {
	t.Parallel()

	// One upload-bearing message in the whole workspace, and it is a thread root.
	root := `{"type":"message","user":"U1","text":"protect this","ts":"` + testThreadParent +
		`","thread_ts":"` + testThreadParent + `","reply_count":1,"files":[` + slackFileObject + `]}`
	srv, _ := newFakeSlack(t, map[string]string{
		methodConversationsList: listBody(testChannel),
		surfaceHistory:          messagesBody(root),
		surfaceReplies:          messagesBody(root, textMessage("100.9")),
	})

	result, err := runScan(context.Background(), testScanConfig(srv))
	if err != nil {
		t.Fatalf("runScan: %v", err)
	}
	// Each surface faithfully reports what IT returned — that is what the two tallies are
	// for — so both count the root.
	if result.History.ClassifiedUploads != 1 || result.Replies.ClassifiedUploads != 1 {
		t.Errorf("history = %+v, replies = %+v; each surface must report what it returned",
			result.History, result.Replies)
	}
	if result.Contract.DistinctUploads != 1 {
		t.Errorf("distinct uploads = %d, want 1: the workspace contains one upload",
			result.Contract.DistinctUploads)
	}
	if result.Conversations[0].ClassifiedUploads != 1 {
		t.Errorf("conversation uploads = %d, want the same distinct rule", result.Conversations[0].ClassifiedUploads)
	}
}

// TestRunScanMinUploadsSeesThroughTheDuplicate is the consequence spelled out: with the
// old summing rule this workspace passed -min-uploads 2 on a single real upload.
func TestRunScanMinUploadsSeesThroughTheDuplicate(t *testing.T) {
	t.Parallel()

	root := `{"type":"message","user":"U1","text":"protect this","ts":"` + testThreadParent +
		`","thread_ts":"` + testThreadParent + `","reply_count":1,"files":[` + slackFileObject + `]}`
	srv, _ := newFakeSlack(t, map[string]string{
		methodConversationsList: listBody(testChannel),
		surfaceHistory:          messagesBody(root),
		surfaceReplies:          messagesBody(root),
	})
	cfg := testScanConfig(srv)
	cfg.MinUploads = 2

	result, err := runScan(context.Background(), cfg)
	if err == nil {
		t.Fatal("one real upload must not satisfy -min-uploads 2")
	}
	if !strings.Contains(strings.Join(result.Contract.Failures, "; "), "1 distinct upload") {
		t.Errorf("failures = %v, want the distinct count named", result.Contract.Failures)
	}
}

// TestRunScanFillsTheVerdictWhenDiscoveryFails pins the one path that used to return a
// zero-value contract block. The runbook tells the operator the report IS the diagnosis,
// so "min_uploads": 0 on a run configured with 7, and an empty failures array, made that
// instruction false exactly where it mattered.
func TestRunScanFillsTheVerdictWhenDiscoveryFails(t *testing.T) {
	t.Parallel()

	srv, _ := newFakeSlack(t, map[string]string{
		methodConversationsList: `{"ok":false,"error":"missing_scope","needed":"channels:read","provided":"chat:write"}`,
	})
	cfg := testScanConfig(srv)
	cfg.MinUploads = 7

	result, err := runScan(context.Background(), cfg)
	if err == nil {
		t.Fatal("discovery failure must fail the scan")
	}
	if result.Contract.Holds {
		t.Error("contract must not hold")
	}
	if result.Contract.MinUploads != 7 {
		t.Errorf("min_uploads = %d, want the configured 7 rather than the zero value", result.Contract.MinUploads)
	}
	if len(result.Contract.Failures) == 0 ||
		!strings.Contains(strings.Join(result.Contract.Failures, "; "), "channels:read") {
		t.Errorf("failures = %v, want the discovery error carried into the report", result.Contract.Failures)
	}
}

// TestCheckExpectationRejectsAMessageThatIsNotTheOneNamed pins the guard on the one
// check whose oracle is a human. Verdicting on whatever the lookup happened to return
// would be worse here than anywhere else in the command.
func TestCheckExpectationRejectsAMessageThatIsNotTheOneNamed(t *testing.T) {
	t.Parallel()

	srv, _ := newFakeSlack(t, map[string]string{
		methodConversationsList: listBody(testChannel),
		// Both lookups answer with a DIFFERENT message that happens to carry a file.
		surfaceHistory: messagesBody(uploadMessage("100.1")),
		surfaceReplies: messagesBody(uploadMessage("100.1")),
	})
	cfg := testScanConfig(srv)
	cfg.MaxThreads = 0
	cfg.ExpectUploads = messageRefList{{Channel: testChannel, TS: "999.9"}}

	result, err := runScan(context.Background(), cfg)
	if err == nil {
		t.Fatal("a lookup that returned a different message must not pass as a verdict")
	}
	if len(result.ExpectedUploads) != 1 || result.ExpectedUploads[0].Found {
		t.Fatalf("expected uploads = %+v, want the mismatch rejected", result.ExpectedUploads)
	}
	if !strings.Contains(strings.Join(result.Contract.Failures, "; "), "could not be verified") {
		t.Errorf("failures = %v, want the unverifiable wording", result.Contract.Failures)
	}
}

// TestRunScanCountsARepliesConversationWithAPartialSample pins that threads measured
// before an error still count their conversation, so the report cannot show
// replies.messages > 0 beside replies.conversations = 0.
func TestRunScanCountsARepliesConversationWithAPartialSample(t *testing.T) {
	t.Parallel()

	srv, fake := newFakeSlack(t, map[string]string{
		methodConversationsList: listBody(testChannel),
		surfaceHistory: messagesBody(
			`{"type":"message","user":"U1","text":"a","ts":"200.1","thread_ts":"200.1","reply_count":1}`,
			`{"type":"message","user":"U1","text":"b","ts":"200.2","thread_ts":"200.2","reply_count":1}`,
		),
	})
	var mu sync.Mutex
	var calls int
	fake.setHandler(surfaceReplies, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			_, _ = w.Write([]byte(messagesBody(uploadMessage("200.3"))))
			return
		}
		_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
	})

	result, err := runScan(context.Background(), testScanConfig(srv))
	if err != nil {
		t.Fatalf("runScan: %v", err)
	}
	if result.Replies.Messages == 0 {
		t.Fatalf("replies = %+v, want the first thread's messages tallied", result.Replies)
	}
	if result.Replies.Conversations != 1 {
		t.Errorf("replies conversations = %d, want the partially sampled conversation counted", result.Replies.Conversations)
	}
}

func TestCleanOperatorNoteBoundsALongReason(t *testing.T) {
	t.Parallel()

	// Slack sends short enum codes, but against a -base-url that is not Slack the
	// content-free promise would otherwise be the other end's to keep.
	got := cleanOperatorNote(strings.Repeat("é", 500))
	if len(got) <= maxSlackReasonBytes || !strings.HasSuffix(got, "…(truncated)") {
		t.Errorf("cleanOperatorNote length = %d, want it bounded and marked", len(got))
	}
	if !utf8.ValidString(got) {
		t.Error("truncation must land on a rune boundary")
	}
	if short := cleanOperatorNote(" channel_not_found "); short != "channel_not_found" {
		t.Errorf("a real Slack reason must pass through untouched, got %q", short)
	}
}

// TestGetOnceReportsWhatArrivedWhenItIsNotJSON pins the detail an operator needs at 2am.
// A corporate proxy answering with an HTML SSO page is otherwise indistinguishable from
// a Slack outage, and the body itself must stay out of the error.
func TestGetOnceReportsWhatArrivedWhenItIsNotJSON(t *testing.T) {
	t.Parallel()

	srv, fake := newFakeSlack(t, nil)
	fake.setHandler(surfaceHistory, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body>Sign in to continue — secret-plan.pdf</body></html>`))
	})
	client := &slackClient{token: testToken, baseURL: srv.URL, userAgent: defaultUserAgent, httpClient: newSlackHTTPClient(testRequestTimeout)}
	var out slackMessagesResponse
	err := client.get(context.Background(), surfaceHistory, nil, &out)
	if err == nil {
		t.Fatal("an HTML body must not decode as a Slack response")
	}
	for _, want := range []string{"not JSON", "HTTP 200", "text/html"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "secret-plan") || strings.Contains(err.Error(), "Sign in") {
		t.Errorf("err = %q leaked the body; only the status, type and length are safe", err)
	}
}

// TestGetOnceNamesARedirectTarget pins the companion detail: redirects are surfaced
// rather than followed, and a bare "returned HTTP 302" hides an SSO portal.
func TestGetOnceNamesARedirectTarget(t *testing.T) {
	t.Parallel()

	srv, fake := newFakeSlack(t, nil)
	fake.setHandler(surfaceHistory, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://sso.example.com/login", http.StatusFound)
	})
	client := &slackClient{token: testToken, baseURL: srv.URL, userAgent: defaultUserAgent, httpClient: newSlackHTTPClient(testRequestTimeout)}
	var out slackMessagesResponse
	err := client.get(context.Background(), surfaceHistory, nil, &out)
	if err == nil || !strings.Contains(err.Error(), "sso.example.com") || !strings.Contains(err.Error(), "not followed") {
		t.Errorf("err = %v, want the unfollowed redirect target named", err)
	}
}

// TestScanResultRecordsTheSampleItDescribes pins the bounds block. Whatever is left at a
// default IS the sample the numbers describe, so a report showing 25 conversations is
// otherwise indistinguishable from a workspace that has exactly 25.
func TestScanResultRecordsTheSampleItDescribes(t *testing.T) {
	t.Parallel()

	srv, _ := newFakeSlack(t, map[string]string{surfaceHistory: messagesBody(uploadMessage("100.1"))})
	cfg := testScanConfig(srv)
	cfg.Channels = []string{testChannel}
	cfg.MaxConversations = 7
	cfg.MaxThreads = 2
	cfg.PageLimit = 50

	result, err := runScan(context.Background(), cfg)
	if err != nil {
		t.Fatalf("runScan: %v", err)
	}
	want := scanBounds{
		BaseURL: srv.URL, ConversationTypes: defaultConversationTypes, ExplicitChannels: 1,
		MaxConversations: 7, MaxPages: 1, PageLimit: 50, MaxThreads: 2,
	}
	if result.Bounds != want {
		t.Errorf("bounds = %+v\nwanted   = %+v", result.Bounds, want)
	}
}

// TestReadHistoryReportsUnreadPages pins that a conversation cut off by -max-pages says
// so. Without it, 50,000 messages and 800 messages produce the same line.
func TestReadHistoryReportsUnreadPages(t *testing.T) {
	t.Parallel()

	srv, _ := newFakeSlack(t, map[string]string{
		methodConversationsList: listBody(testChannel),
		surfaceHistory: `{"ok":true,"messages":[` + uploadMessage("100.1") +
			`],"response_metadata":{"next_cursor":"always more"}}`,
	})
	result, err := runScan(context.Background(), testScanConfig(srv))
	if err != nil {
		t.Fatalf("runScan: %v", err)
	}
	if !result.Conversations[0].MorePages {
		t.Error("a conversation stopped by -max-pages must say pages were left unread")
	}

	// ...and a conversation that genuinely ended must not claim there is more.
	endsSrv, _ := newFakeSlack(t, map[string]string{
		methodConversationsList: listBody(testChannel),
		surfaceHistory:          messagesBody(uploadMessage("100.1")),
	})
	ended, err := runScan(context.Background(), testScanConfig(endsSrv))
	if err != nil {
		t.Fatalf("runScan: %v", err)
	}
	if ended.Conversations[0].MorePages {
		t.Error("a conversation whose cursor ran out must not report more pages")
	}
}

// TestRunScanSurfacesDecodeFailures pins the count that used to be tallied and then read
// by nothing: a scan could report holds:true beside thousands of unreadable messages,
// while an uncountable files shape — the same class of problem — had a verdict field, a
// docs row and a flag.
func TestRunScanSurfacesDecodeFailures(t *testing.T) {
	t.Parallel()

	bodies := map[string]string{
		methodConversationsList: listBody(testChannel),
		surfaceHistory: messagesBody(
			uploadMessage("100.1"),
			// Valid JSON inside the array, but not a shape this command can read.
			`{"user":"U1","ts":"100.2","subtype":5}`,
		),
	}
	srv, _ := newFakeSlack(t, bodies)
	result, err := runScan(context.Background(), testScanConfig(srv))
	if err != nil {
		t.Fatalf("runScan: %v", err)
	}
	if result.History.DecodeFailures != 1 || result.Contract.DecodeFailures != 1 {
		t.Errorf("decode failures: surface = %d, verdict = %d, want 1 in both",
			result.History.DecodeFailures, result.Contract.DecodeFailures)
	}
	if !result.Contract.Holds {
		t.Errorf("one undecodable message must not fail the default scan: %v", result.Contract.Failures)
	}

	strictSrv, _ := newFakeSlack(t, bodies)
	strictCfg := testScanConfig(strictSrv)
	strictCfg.StrictUncountable = true
	strictResult, err := runScan(context.Background(), strictCfg)
	if err == nil {
		t.Fatal("-strict-uncountable must fail on an undecodable message")
	}
	if !strings.Contains(strings.Join(strictResult.Contract.Failures, "; "), "could not be decoded at all") {
		t.Errorf("failures = %v, want the decode diagnosis", strictResult.Contract.Failures)
	}
}

// TestRunScanMinUploadsZeroIsReportOnly pins that 0 disables the tripwire deliberately
// rather than by accident. The verdict still carries min_uploads so a reader can see the
// primary check was off, which is the only thing that keeps holds:true honest here.
func TestRunScanMinUploadsZeroIsReportOnly(t *testing.T) {
	t.Parallel()

	srv, _ := newFakeSlack(t, map[string]string{
		methodConversationsList: listBody(testChannel),
		surfaceHistory:          messagesBody(textMessage("100.1")),
	})
	cfg := testScanConfig(srv)
	cfg.MinUploads = 0

	result, err := runScan(context.Background(), cfg)
	if err != nil {
		t.Fatalf("runScan: %v", err)
	}
	if !result.Contract.Holds {
		t.Errorf("-min-uploads 0 must not fail on zero uploads: %v", result.Contract.Failures)
	}
	if result.Contract.MinUploads != 0 || result.Contract.DistinctUploads != 0 {
		t.Errorf("contract = %+v, want the disabled threshold visible in the report", result.Contract)
	}
}

// TestWaitHonorsTheContext covers the real-timer path, which no other test reaches:
// every scan test injects a sleep func, so a 429 arriving during a live run would be the
// first time this code executes.
func TestWaitHonorsTheContext(t *testing.T) {
	t.Parallel()

	client := &slackClient{}
	if err := client.wait(context.Background(), time.Millisecond); err != nil {
		t.Errorf("wait: %v", err)
	}

	// A canceled context must abandon the wait rather than park for Retry-After.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.wait(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("wait on a canceled context = %v, want context.Canceled", err)
	}
}

func TestConversationKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		isIM, isMPIM, isPrivate bool
		want                    string
	}{
		{isIM: true, want: "im"},
		{isMPIM: true, want: "mpim"},
		{isPrivate: true, want: "private_channel"},
		{want: "public_channel"},
		// An IM flagged private is still an IM: the arms are ordered most-specific first.
		{isIM: true, isPrivate: true, want: "im"},
	}
	for _, tt := range tests {
		if got := conversationKind(tt.isIM, tt.isMPIM, tt.isPrivate); got != tt.want {
			t.Errorf("conversationKind(%v,%v,%v) = %q, want %q", tt.isIM, tt.isMPIM, tt.isPrivate, got, tt.want)
		}
	}
}

// TestRunScanRecordsEveryConversationKind pins that the kinds reach the report, which is
// how an operator tells a measurement over public channels from one that included DMs.
func TestRunScanRecordsEveryConversationKind(t *testing.T) {
	t.Parallel()

	srv, _ := newFakeSlack(t, map[string]string{
		methodConversationsList: `{"ok":true,"channels":[` +
			`{"id":"C0000000001","is_member":true},` +
			`{"id":"G0000000001","is_member":true,"is_private":true},` +
			`{"id":"D0000000001","is_im":true},` +
			`{"id":"G0000000002","is_mpim":true}]}`,
		surfaceHistory: messagesBody(uploadMessage("100.1")),
	})
	result, err := runScan(context.Background(), testScanConfig(srv))
	if err != nil {
		t.Fatalf("runScan: %v", err)
	}
	want := []string{"public_channel", "private_channel", "im", "mpim"}
	if len(result.Conversations) != len(want) {
		t.Fatalf("conversations = %+v, want %d", result.Conversations, len(want))
	}
	for i, kind := range want {
		if result.Conversations[i].Kind != kind {
			t.Errorf("conversation %d kind = %q, want %q", i, result.Conversations[i].Kind, kind)
		}
	}
}

// TestSlackStatusErrorHandlesAnUnreadableEnvelope covers the branch reached when a
// response decodes into the caller's type but not into the status envelope — the shape a
// non-Slack endpoint returning a bare JSON array produces.
func TestSlackStatusErrorHandlesAnUnreadableEnvelope(t *testing.T) {
	t.Parallel()

	err := slackStatusError(surfaceHistory, []byte(`["not","an","envelope"]`))
	if err == nil || !strings.Contains(err.Error(), "status JSON invalid") {
		t.Errorf("err = %v, want the unreadable-envelope diagnosis", err)
	}
	// ok:true is the only shape that is not an error.
	if err := slackStatusError(surfaceHistory, []byte(`{"ok":true}`)); err != nil {
		t.Errorf("err = %v, want nil for an ok response", err)
	}
	// An error with no reason still has to say something.
	if err := slackStatusError(surfaceHistory, []byte(`{"ok":false}`)); err == nil ||
		!strings.Contains(err.Error(), "not_ok") {
		t.Errorf("err = %v, want the not_ok fallback", err)
	}
}
