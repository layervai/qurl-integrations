package internal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	testFeedbackTeamDomain = "acme"
	testFeedbackUserName   = "dana"
	testPlainTextType      = "plain_text"
	testHooksURL           = "https://hooks.slack.com/x"
	testFeedbackSummary    = "summary"
)

func TestParseFeedbackModalArgs(t *testing.T) {
	t.Parallel()
	state := func(typeValue, summary, details string) map[string]map[string]interactionStateValue {
		return map[string]map[string]interactionStateValue{
			feedbackBlockType:    {feedbackActionType: {SelectedOption: &interactionSelectedOption{Value: typeValue}}},
			feedbackBlockSummary: {feedbackActionSummary: {Value: summary}},
			feedbackBlockDetails: {feedbackActionDetails: {Value: details}},
		}
	}
	tests := []struct {
		name      string
		values    map[string]map[string]interactionStateValue
		wantErrAt string // block ID expected to carry a field error; "" = success
	}{
		{name: "happy with details", values: state(feedbackTypeBug, "App crashes on login", "Open app, tap login")},
		{name: "happy no details", values: state(feedbackTypeFeature, "Add dark mode", "")},
		{name: "missing summary", values: state(feedbackTypeBug, "  ", "details"), wantErrAt: feedbackBlockSummary},
		{name: "unknown type", values: state("totally-not-a-type", "summary", ""), wantErrAt: feedbackBlockType},
		{name: "summary too long", values: state(feedbackTypeBug, strings.Repeat("x", feedbackSummaryMaxLen+1), ""), wantErrAt: feedbackBlockSummary},
		{name: "details too long", values: state(feedbackTypeOther, "summary", strings.Repeat("y", feedbackDetailsMaxLen+1)), wantErrAt: feedbackBlockDetails},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args, fieldErrors := parseFeedbackModalArgs(tc.values)
			if tc.wantErrAt != "" {
				if _, ok := fieldErrors[tc.wantErrAt]; !ok {
					t.Fatalf("expected a field error on %q, got %v", tc.wantErrAt, fieldErrors)
				}
				if args != nil {
					t.Errorf("expected nil args on validation failure, got %+v", args)
				}
				return
			}
			if len(fieldErrors) != 0 {
				t.Fatalf("unexpected field errors: %v", fieldErrors)
			}
			if args == nil || args.Summary == "" {
				t.Fatalf("expected populated args, got %+v", args)
			}
		})
	}
}

func TestFeedbackModalJSON(t *testing.T) {
	t.Parallel()
	meta := &FeedbackModalMetadata{TeamID: "T1", UserID: "U1", ResponseURL: testHooksURL}
	raw, err := FeedbackModal(meta)
	if err != nil {
		t.Fatalf("FeedbackModal: %v", err)
	}
	var view struct {
		Type            string `json:"type"`
		CallbackID      string `json:"callback_id"`
		PrivateMetadata string `json:"private_metadata"`
		Blocks          []struct {
			BlockID string `json:"block_id"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatalf("unmarshal modal: %v", err)
	}
	if view.Type != "modal" || view.CallbackID != callbackIDFeedback {
		t.Errorf("type=%q callback_id=%q, want modal/%s", view.Type, view.CallbackID, callbackIDFeedback)
	}
	wantBlocks := map[string]bool{feedbackBlockType: false, feedbackBlockSummary: false, feedbackBlockDetails: false}
	for _, b := range view.Blocks {
		if _, ok := wantBlocks[b.BlockID]; ok {
			wantBlocks[b.BlockID] = true
		}
	}
	for id, seen := range wantBlocks {
		if !seen {
			t.Errorf("modal missing input block %q", id)
		}
	}
	// private_metadata must round-trip the submitter attribution.
	var gotMeta FeedbackModalMetadata
	if err := json.Unmarshal([]byte(view.PrivateMetadata), &gotMeta); err != nil {
		t.Fatalf("private_metadata not valid JSON: %v", err)
	}
	if gotMeta.TeamID != "T1" || gotMeta.ResponseURL != testHooksURL {
		t.Errorf("private_metadata round-trip = %+v", gotMeta)
	}
}

// section is the decoded shape of a Block Kit block carrying a single text
// object (header/section). Context blocks use `elements` and decode with a nil
// Text, which is what the attribution assertions rely on.
type decodedBlock struct {
	Type string `json:"type"`
	Text *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"text"`
}

func TestFeedbackMessageJSON(t *testing.T) {
	t.Parallel()
	meta := &FeedbackModalMetadata{TeamID: "T_ABC", TeamDomain: testFeedbackTeamDomain, UserID: "U_XYZ", UserName: testFeedbackUserName}
	// A mention-injection attempt in user content must NOT render as mrkdwn.
	summary := "<!channel> everything is broken"
	details := "<@U999> please look"
	raw, err := FeedbackMessage(meta, feedbackTypeBug, summary, details)
	if err != nil {
		t.Fatalf("FeedbackMessage: %v", err)
	}
	var msg struct {
		Text   string         `json:"text"`
		Blocks []decodedBlock `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	// Notification fallback carries the label, not user content.
	if msg.Text != "New qURL feedback — Bug report" {
		t.Errorf("text fallback = %q", msg.Text)
	}
	if strings.Contains(msg.Text, "channel") {
		t.Errorf("fallback text leaked user content: %q", msg.Text)
	}
	// User-provided summary and details must render as plain_text so Slack does
	// no mrkdwn parsing (no mention/<!channel> expansion).
	for _, want := range []string{summary, details} {
		var found bool
		for _, b := range msg.Blocks {
			if b.Text != nil && b.Text.Text == want {
				found = true
				if b.Text.Type != testPlainTextType {
					t.Errorf("user content %q rendered as %q, want plain_text", want, b.Text.Type)
				}
			}
		}
		if !found {
			t.Errorf("message missing a block for %q", want)
		}
	}
	// Attribution must carry the stable IDs for the triage routine.
	if !strings.Contains(string(raw), "T_ABC") || !strings.Contains(string(raw), "U_XYZ") {
		t.Errorf("attribution missing team/user IDs: %s", raw)
	}

	if _, err := FeedbackMessage(meta, "bogus", "s", ""); err == nil {
		t.Error("expected error for unknown feedback type")
	}
}

// feedbackTestHandler returns a handler with the feedback seams wired to stubs:
// OpenView records the opened view; PostFeedback forwards each payload to the
// returned channel. response_url validation is relaxed (newTestHandler sets
// url.Parse) so the async confirmation can target an httptest server.
func feedbackTestHandler(t *testing.T) (handler *Handler, openedView *[]byte, posted chan []byte) {
	t.Helper()
	handler = newTestHandler(t, noopQURLServer(t))
	var view []byte
	handler.cfg.OpenView = func(_ context.Context, _, _ string, viewJSON []byte) error {
		view = viewJSON
		return nil
	}
	posted = make(chan []byte, 1)
	handler.cfg.PostFeedback = func(_ context.Context, payload []byte) error {
		posted <- payload
		return nil
	}
	return handler, &view, posted
}

func feedbackCommandBody() string {
	return url.Values{
		fieldCommand:     {commandUser},
		fieldText:        {"feedback"},
		fieldTeamID:      {testAdminTeamID},
		fieldTeamDomain:  {testFeedbackTeamDomain},
		fieldUserID:      {testAdminUserID},
		fieldUserName:    {testFeedbackUserName},
		fieldChannelID:   {getTokenCommandTestChannelID},
		fieldTriggerID:   {testSlackTriggerID},
		fieldResponseURL: {"https://hooks.slack.com/commands/T/X"},
	}.Encode()
}

func TestHandleFeedbackOpensModal(t *testing.T) {
	t.Parallel()
	h, openedView, _ := feedbackTestHandler(t)
	body := feedbackCommandBody()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if len(*openedView) == 0 {
		t.Fatal("OpenView was not called")
	}
	var view struct {
		CallbackID      string `json:"callback_id"`
		PrivateMetadata string `json:"private_metadata"`
	}
	if err := json.Unmarshal(*openedView, &view); err != nil {
		t.Fatalf("opened view not valid JSON: %v", err)
	}
	if view.CallbackID != callbackIDFeedback {
		t.Errorf("opened view callback_id = %q, want %q", view.CallbackID, callbackIDFeedback)
	}
	var meta FeedbackModalMetadata
	if err := json.Unmarshal([]byte(view.PrivateMetadata), &meta); err != nil {
		t.Fatalf("private_metadata: %v", err)
	}
	if meta.UserName != testFeedbackUserName || meta.TeamDomain != testFeedbackTeamDomain || meta.ResponseURL == "" {
		t.Errorf("captured metadata = %+v", meta)
	}
}

func TestHandleFeedbackNotEnabled(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t, noopQURLServer(t))
	// OpenView wired but PostFeedback nil — feedback has nowhere to land.
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }
	body := feedbackCommandBody()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body))
	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(result["text"], "isn't enabled") {
		t.Errorf("reply = %q, want not-enabled message", result["text"])
	}
}

func feedbackSubmissionBody(t *testing.T, meta *FeedbackModalMetadata, payloadTeamID, payloadUserID, typeValue, summary, details string) string {
	t.Helper()
	pm, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal private_metadata: %v", err)
	}
	return viewSubmissionBody(t, "V_feedback", callbackIDFeedback, string(pm), payloadTeamID, payloadUserID,
		map[string]map[string]interactionStateValue{
			feedbackBlockType:    {feedbackActionType: {SelectedOption: &interactionSelectedOption{Value: typeValue}}},
			feedbackBlockSummary: {feedbackActionSummary: {Value: summary}},
			feedbackBlockDetails: {feedbackActionDetails: {Value: details}},
		})
}

func TestHandleFeedbackSubmissionPostsAndConfirms(t *testing.T) {
	t.Parallel()
	h, _, posted := feedbackTestHandler(t)

	confirmed := make(chan string, 1)
	rs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		confirmed <- string(b)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(rs.Close)

	meta := &FeedbackModalMetadata{TeamID: testAdminTeamID, UserID: testAdminUserID, ResponseURL: rs.URL}
	body := feedbackSubmissionBody(t, meta, testAdminTeamID, testAdminUserID, feedbackTypeBug, "App crashes on login", "Tap login")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/interactions", body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}

	select {
	case payload := <-posted:
		if !strings.Contains(string(payload), "App crashes on login") {
			t.Errorf("posted feedback missing summary: %s", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PostFeedback was not called")
	}

	select {
	case got := <-confirmed:
		if !strings.Contains(got, "Thanks") {
			t.Errorf("confirmation = %q, want a thank-you", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no confirmation posted to response_url")
	}
}

func TestHandleFeedbackSubmissionTeamMismatch(t *testing.T) {
	t.Parallel()
	h, _, posted := feedbackTestHandler(t)
	meta := &FeedbackModalMetadata{TeamID: testAdminTeamID, UserID: testAdminUserID, ResponseURL: testHooksURL}
	// Payload team differs from the metadata team — a cross-workspace replay.
	body := feedbackSubmissionBody(t, meta, "T_OTHER", testAdminUserID, feedbackTypeBug, testFeedbackSummary, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/interactions", body, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var result struct {
		ResponseAction string `json:"response_action"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.ResponseAction != respActionUpdate {
		t.Errorf("response_action = %q, want %q (error modal)", result.ResponseAction, respActionUpdate)
	}
	select {
	case <-posted:
		t.Fatal("PostFeedback must not run on a team mismatch")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestHandleFeedbackSubmissionDeliveryFailure(t *testing.T) {
	t.Parallel()
	h := newTestHandler(t, noopQURLServer(t))
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }
	h.cfg.PostFeedback = func(context.Context, []byte) error { return errors.New("webhook 503") }

	notified := make(chan string, 1)
	rs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		notified <- string(b)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(rs.Close)

	meta := &FeedbackModalMetadata{TeamID: testAdminTeamID, UserID: testAdminUserID, ResponseURL: rs.URL}
	body := feedbackSubmissionBody(t, meta, testAdminTeamID, testAdminUserID, feedbackTypeBug, testFeedbackSummary, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/interactions", body, body))

	// A delivery failure must surface a retry ephemeral — never silently drop.
	select {
	case got := <-notified:
		if strings.Contains(got, "Thanks") || !strings.Contains(got, "Couldn't send") {
			t.Errorf("delivery-failure ephemeral = %q, want retry copy", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no failure ephemeral posted to response_url")
	}
}

func TestHandleFeedbackSubmissionUserMismatch(t *testing.T) {
	t.Parallel()
	h, _, posted := feedbackTestHandler(t)
	// Team matches but the submitting user differs — symmetric modal-replay guard.
	meta := &FeedbackModalMetadata{TeamID: testAdminTeamID, UserID: testAdminUserID, ResponseURL: testHooksURL}
	body := feedbackSubmissionBody(t, meta, testAdminTeamID, "U_OTHER", feedbackTypeBug, testFeedbackSummary, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/interactions", body, body))
	var result struct {
		ResponseAction string `json:"response_action"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.ResponseAction != respActionUpdate {
		t.Errorf("response_action = %q, want %q (error modal)", result.ResponseAction, respActionUpdate)
	}
	select {
	case <-posted:
		t.Fatal("PostFeedback must not run on a user mismatch")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestHandleFeedbackSubmissionMissingResponseURL(t *testing.T) {
	t.Parallel()
	h, _, posted := feedbackTestHandler(t)
	// Empty response_url → the async worker can't confirm receipt, so the submit
	// is refused (error modal) rather than posted into the void.
	meta := &FeedbackModalMetadata{TeamID: testAdminTeamID, UserID: testAdminUserID, ResponseURL: ""}
	body := feedbackSubmissionBody(t, meta, testAdminTeamID, testAdminUserID, feedbackTypeBug, testFeedbackSummary, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/interactions", body, body))
	var result struct {
		ResponseAction string `json:"response_action"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if result.ResponseAction != respActionUpdate {
		t.Errorf("response_action = %q, want %q (error modal)", result.ResponseAction, respActionUpdate)
	}
	select {
	case <-posted:
		t.Fatal("PostFeedback must not run with a missing response_url")
	case <-time.After(100 * time.Millisecond):
	}
}
