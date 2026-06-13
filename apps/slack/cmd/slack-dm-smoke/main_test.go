package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	testPathAuthTest          = "/auth.test"
	testPathConversationsOpen = "/conversations.open"
	testPathChatPostMessage   = "/chat.postMessage"
	testAdminUserID           = "U_admin"
	testSmokeText             = "No secrets."
	testSmokeToken            = "xoxb-test-token"
	testSecretSmokeToken      = "xoxb-super-secret"
	testSmokeTokenEnv         = "SMOKE_TOKEN"
	testSlackAPIBaseURL       = "https://slack.test"
	testFlagTokenEnv          = "-token-env"
	testFlagUser              = "-user"
	testFlagText              = "-text"
	testFlagBaseURL           = "-base-url"
	testFlagUserAgent         = "-user-agent"
	testFlagTimeout           = "-timeout"
	testFlagRequestTimeout    = "-request-timeout"
)

func testServerErrorf(t *testing.T, w http.ResponseWriter, format string, args ...any) {
	t.Helper()
	t.Errorf(format, args...)
	http.Error(w, "test server error", http.StatusInternalServerError)
}

func TestRunSmokeOpenPostAndDirectProbe(t *testing.T) {
	t.Parallel()

	var openBody struct {
		Users string `json:"users"`
	}
	var mu sync.Mutex
	var postedChannels []string
	var postedTexts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testSmokeToken {
			testServerErrorf(t, w, "Authorization = %q, want bearer token", got)
			return
		}
		switch r.URL.Path {
		case testPathAuthTest:
			if contentType := r.Header.Get("Content-Type"); contentType != "" {
				testServerErrorf(t, w, "auth.test Content-Type = %q, want empty for no-arg request", contentType)
				return
			}
			rawBody, err := io.ReadAll(r.Body)
			if err != nil {
				testServerErrorf(t, w, "read auth.test body: %v", err)
				return
			}
			if len(rawBody) != 0 {
				testServerErrorf(t, w, "auth.test body = %q, want empty", string(rawBody))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"team_id":"T_smoke","enterprise_id":"E_grid","bot_id":"B_bot","user_id":"U_bot"}`))
		case testPathConversationsOpen:
			var body struct {
				Users string `json:"users"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				testServerErrorf(t, w, "decode open body: %v", err)
				return
			}
			mu.Lock()
			openBody = body
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"D_smoke"}}`))
		case testPathChatPostMessage:
			var body struct {
				Channel string `json:"channel"`
				Text    string `json:"text"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				testServerErrorf(t, w, "decode post body: %v", err)
				return
			}
			if !strings.Contains(body.Text, "No secrets") {
				testServerErrorf(t, w, "smoke text = %q, want non-secret marker", body.Text)
				return
			}
			if strings.ContainsAny(body.Text, "\n\r\t") {
				testServerErrorf(t, w, "smoke text = %q, want control chars cleaned", body.Text)
				return
			}
			mu.Lock()
			postedChannels = append(postedChannels, body.Channel)
			postedTexts = append(postedTexts, body.Text)
			mu.Unlock()
			if body.Channel == testAdminUserID {
				_, _ = w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"channel":"D_smoke","ts":"1700000000.000100"}`))
		default:
			testServerErrorf(t, w, "unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	result, err := runSmoke(context.Background(), &smokeConfig{
		Token:           testSmokeToken,
		UserID:          testAdminUserID,
		Text:            "No secrets\nin this smoke.",
		BaseURL:         srv.URL,
		WorkspaceShape:  " Enterprise\nGrid org install ",
		TokenOwner:      "enterprise\towner",
		Scopes:          "commands,chat:write\rim:write",
		DirectUserProbe: true,
		StartedAt:       time.Unix(1800000000, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("runSmoke: %v", err)
	}
	if result.StartedAt != "2027-01-15T08:00:00Z" {
		t.Fatalf("StartedAt = %q", result.StartedAt)
	}
	if result.WorkspaceShape != "Enterprise Grid org install" || result.TokenOwner != "enterprise owner" || result.Scopes != "commands,chat:write im:write" {
		t.Fatalf("metadata = %+v", result)
	}
	if result.Auth == nil || result.Auth.TeamID != "T_smoke" || result.Auth.EnterpriseID != "E_grid" || result.Auth.BotID != "B_bot" {
		t.Fatalf("auth result = %+v", result.Auth)
	}
	mu.Lock()
	defer mu.Unlock()
	if openBody.Users != testAdminUserID {
		t.Fatalf("conversations.open users = %q, want %s", openBody.Users, testAdminUserID)
	}
	if strings.Join(postedChannels, ",") != "D_smoke,"+testAdminUserID {
		t.Fatalf("posted channels = %v, want production D channel plus direct probe user", postedChannels)
	}
	if len(postedTexts) != 2 {
		t.Fatalf("posted texts = %v, want production and direct probe messages", postedTexts)
	}
	if strings.Contains(postedTexts[0], directUserProbeSuffix) {
		t.Fatalf("production text = %q, want no direct probe suffix", postedTexts[0])
	}
	if !strings.Contains(postedTexts[1], directUserProbeSuffix) || postedTexts[0] == postedTexts[1] {
		t.Fatalf("posted texts = %v, want distinguishable direct probe message", postedTexts)
	}
	if len(result.ProductionPath) != 2 || !result.ProductionPath[0].OK || !result.ProductionPath[1].OK {
		t.Fatalf("production path = %+v, want two successful steps", result.ProductionPath)
	}
	if result.ProductionPath[0].ChannelID != "D_smoke" || result.ProductionPath[1].PostedChannel != "D_smoke" {
		t.Fatalf("production path channels = %+v", result.ProductionPath)
	}
	if result.DirectUserProbe == nil || result.DirectUserProbe.OK || result.DirectUserProbe.Error != "channel_not_found" {
		t.Fatalf("direct probe = %+v, want recorded channel_not_found", result.DirectUserProbe)
	}
}

func TestRunSmokeDefaultsEmptyText(t *testing.T) {
	t.Parallel()

	startedAt := time.Unix(1800000000, 0).UTC()
	wantText := "qURL Slack DM delivery smoke 2027-01-15T08:00:00Z. No secrets are included."

	var mu sync.Mutex
	var postedText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testPathAuthTest:
			_, _ = w.Write([]byte(`{"ok":true}`))
		case testPathConversationsOpen:
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"D_smoke"}}`))
		case testPathChatPostMessage:
			var body struct {
				Text string `json:"text"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				testServerErrorf(t, w, "decode post body: %v", err)
				return
			}
			mu.Lock()
			postedText = body.Text
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"channel":"D_smoke","ts":"1700000000.000100"}`))
		default:
			testServerErrorf(t, w, "unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	result, err := runSmoke(context.Background(), &smokeConfig{
		Token:     testSmokeToken,
		UserID:    testAdminUserID,
		BaseURL:   srv.URL,
		StartedAt: startedAt,
	})
	if err != nil {
		t.Fatalf("runSmoke: %v", err)
	}
	if len(result.ProductionPath) != 2 || !result.ProductionPath[1].OK {
		t.Fatalf("production path = %+v, want successful open-then-post", result.ProductionPath)
	}
	mu.Lock()
	defer mu.Unlock()
	if postedText != wantText {
		t.Fatalf("posted text = %q, want %q", postedText, wantText)
	}
}

func TestDefaultOverallTimeoutCoversDirectProbeBudget(t *testing.T) {
	t.Parallel()

	minimum := time.Duration(minDirectProbeFactor) * defaultRequestTimeout
	if defaultOverallTimeout <= minimum {
		t.Fatalf("defaultOverallTimeout = %s, want more than four request timeouts (%s)", defaultOverallTimeout, minimum)
	}
}

func TestDirectUserProbeTextFitsSmokeTextLimit(t *testing.T) {
	t.Parallel()

	got := directUserProbeText(strings.Repeat("x", maxSmokeTextBytes))
	if len(got) > maxSmokeTextBytes {
		t.Fatalf("directUserProbeText length = %d, want at most %d", len(got), maxSmokeTextBytes)
	}
	if !strings.HasSuffix(got, directUserProbeSuffix) {
		t.Fatalf("directUserProbeText = %q, want suffix %q", got, directUserProbeSuffix)
	}
}

func TestDirectUserProbeTextDoesNotSplitMultibyteRune(t *testing.T) {
	t.Parallel()

	text := strings.Repeat("x", maxSmokeTextBytes-len(directUserProbeSuffix)-1) + "\u00e9"
	got := directUserProbeText(text)
	if len(got) > maxSmokeTextBytes {
		t.Fatalf("directUserProbeText length = %d, want at most %d", len(got), maxSmokeTextBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("directUserProbeText returned invalid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, directUserProbeSuffix) {
		t.Fatalf("directUserProbeText = %q, want suffix %q", got, directUserProbeSuffix)
	}
}

func TestDirectUserProbeTextHandlesEmptyBaseText(t *testing.T) {
	t.Parallel()

	got := directUserProbeText("")
	want := strings.TrimSpace(directUserProbeSuffix)
	if got != want {
		t.Fatalf("directUserProbeText = %q, want trimmed suffix %q", got, want)
	}
}

func TestDirectUserProbeSuffixFitsSmokeTextLimit(t *testing.T) {
	t.Parallel()

	if len(directUserProbeSuffix) > maxSmokeTextBytes {
		t.Fatalf("directUserProbeSuffix length = %d, want at most %d", len(directUserProbeSuffix), maxSmokeTextBytes)
	}
}

func TestPrepareSmokeConfigIsIdempotent(t *testing.T) {
	t.Parallel()

	cfg := smokeConfig{
		Token:     " " + testSmokeToken + " ",
		UserID:    " " + testAdminUserID + " ",
		Text:      "No secrets\nin this smoke.",
		BaseURL:   "http://127.0.0.1:8080/api/",
		StartedAt: time.Unix(1800000000, 0).UTC(),
	}

	if err := prepareSmokeConfig(&cfg); err != nil {
		t.Fatalf("first prepareSmokeConfig: %v", err)
	}
	first := cfg
	if err := prepareSmokeConfig(&cfg); err != nil {
		t.Fatalf("second prepareSmokeConfig: %v", err)
	}
	if cfg != first {
		t.Fatalf("second prepareSmokeConfig changed cfg\nfirst: %+v\nsecond: %+v", first, cfg)
	}
	if cfg.BaseURL != "http://127.0.0.1:8080/api" || cfg.Text != "No secrets in this smoke." {
		t.Fatalf("prepared cfg = %+v, want normalized base URL and cleaned text", cfg)
	}
}

func TestRunSmokeFailsWhenProductionOpenFails(t *testing.T) {
	t.Parallel()

	var postCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testPathAuthTest:
			_, _ = w.Write([]byte(`{"ok":true}`))
		case testPathConversationsOpen:
			_, _ = w.Write([]byte(`{"ok":false,"error":"missing_scope","needed":"im:write","provided":"chat:write"}`))
		case testPathChatPostMessage:
			postCalled = true
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			testServerErrorf(t, w, "unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	result, err := runSmoke(context.Background(), &smokeConfig{
		Token:   testSmokeToken,
		UserID:  testAdminUserID,
		Text:    testSmokeText,
		BaseURL: srv.URL,
	})
	if err == nil || !strings.Contains(err.Error(), "missing_scope") {
		t.Fatalf("runSmoke error = %v, want missing_scope", err)
	}
	if postCalled {
		t.Fatal("chat.postMessage should not run when conversations.open fails")
	}
	if len(result.ProductionPath) != 1 || result.ProductionPath[0].Needed != "im:write" || result.ProductionPath[0].Provided != "chat:write" {
		t.Fatalf("production path = %+v, want open failure details", result.ProductionPath)
	}
}

func TestRunSmokeFailsWhenOpenReturnsNoChannelID(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testPathAuthTest:
			_, _ = w.Write([]byte(`{"ok":true}`))
		case testPathConversationsOpen:
			_, _ = w.Write([]byte(`{"ok":true,"channel":{}}`))
		default:
			testServerErrorf(t, w, "unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	result, err := runSmoke(context.Background(), &smokeConfig{
		Token:   testSmokeToken,
		UserID:  testAdminUserID,
		Text:    testSmokeText,
		BaseURL: srv.URL,
	})
	if err == nil || !strings.Contains(err.Error(), "no channel id") {
		t.Fatalf("runSmoke error = %v, want missing channel id", err)
	}
	if len(result.ProductionPath) != 1 {
		t.Fatalf("production path = %+v, want one open result", result.ProductionPath)
	}
	open := result.ProductionPath[0]
	if open.OK || open.Error != "missing_dm_channel_id" {
		t.Fatalf("open result = %+v, want missing_dm_channel_id failure", open)
	}
}

func TestRunSmokeFailsWhenStrictDirectProbeFails(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testPathAuthTest:
			_, _ = w.Write([]byte(`{"ok":true}`))
		case testPathConversationsOpen:
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"D_smoke"}}`))
		case testPathChatPostMessage:
			var body struct {
				Channel string `json:"channel"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				testServerErrorf(t, w, "decode post body: %v", err)
				return
			}
			if body.Channel == testAdminUserID {
				_, _ = w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"channel":"D_smoke","ts":"1700000000.000100"}`))
		default:
			testServerErrorf(t, w, "unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	result, err := runSmoke(context.Background(), &smokeConfig{
		Token:             testSmokeToken,
		UserID:            testAdminUserID,
		Text:              testSmokeText,
		BaseURL:           srv.URL,
		DirectUserProbe:   true,
		ForceDirectStrict: true,
	})
	if err == nil || !strings.Contains(err.Error(), "channel_not_found") {
		t.Fatalf("runSmoke error = %v, want strict direct probe failure", err)
	}
	if len(result.ProductionPath) != 2 || !result.ProductionPath[1].OK {
		t.Fatalf("production path = %+v, want successful open-then-post", result.ProductionPath)
	}
	if result.DirectUserProbe == nil || result.DirectUserProbe.OK || result.DirectUserProbe.Error != "channel_not_found" {
		t.Fatalf("direct probe = %+v, want recorded channel_not_found", result.DirectUserProbe)
	}
}

func TestRunSmokeRejectsStrictDirectProbeWithoutProbe(t *testing.T) {
	t.Parallel()

	result, err := runSmoke(context.Background(), &smokeConfig{
		Token:             testSmokeToken,
		UserID:            testAdminUserID,
		Text:              testSmokeText,
		ForceDirectStrict: true,
	})
	if !errors.Is(err, errStrictDirectProbeRequiresProbe) {
		t.Fatalf("runSmoke error = %v, want %v", err, errStrictDirectProbeRequiresProbe)
	}
	if result.Auth != nil || len(result.ProductionPath) != 0 || result.DirectUserProbe != nil {
		t.Fatalf("result = %+v, want no Slack calls after validation failure", result)
	}
}

func TestRunSmokeRecordsNonStrictDirectProbeTransportError(t *testing.T) {
	t.Parallel()

	// runSmoke sends Slack requests sequentially; this is mutated by a synchronous RoundTripper.
	var postedChannels []string
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case testPathAuthTest:
			return testJSONResponse(http.StatusOK, `{"ok":true}`), nil
		case testPathConversationsOpen:
			return testJSONResponse(http.StatusOK, `{"ok":true,"channel":{"id":"D_smoke"}}`), nil
		case testPathChatPostMessage:
			var body struct {
				Channel string `json:"channel"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				return nil, err
			}
			postedChannels = append(postedChannels, body.Channel)
			if body.Channel == testAdminUserID {
				return nil, errors.New("direct probe transport failed")
			}
			return testJSONResponse(http.StatusOK, `{"ok":true,"channel":"D_smoke","ts":"1700000000.000100"}`), nil
		default:
			return nil, errors.New("unexpected path " + req.URL.Path)
		}
	})}

	result, err := runSmoke(context.Background(), &smokeConfig{
		Token:           testSmokeToken,
		UserID:          testAdminUserID,
		Text:            testSmokeText,
		BaseURL:         testSlackAPIBaseURL,
		DirectUserProbe: true,
		HTTPClient:      client,
	})
	if err != nil {
		t.Fatalf("runSmoke: %v", err)
	}
	if strings.Join(postedChannels, ",") != "D_smoke,"+testAdminUserID {
		t.Fatalf("posted channels = %v, want production D channel plus direct probe user", postedChannels)
	}
	if result.DirectUserProbe == nil || result.DirectUserProbe.OK || result.DirectUserProbe.Error != apiErrorRequestFailed {
		t.Fatalf("direct probe = %+v, want recorded non-strict transport failure", result.DirectUserProbe)
	}
	if len(result.ProductionPath) != 2 || !result.ProductionPath[1].OK {
		t.Fatalf("production path = %+v, want successful production path", result.ProductionPath)
	}
}

func TestRunSmokeReturnsContextDeadlineDuringNonStrictDirectProbe(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case testPathAuthTest:
			return testJSONResponse(http.StatusOK, `{"ok":true}`), nil
		case testPathConversationsOpen:
			return testJSONResponse(http.StatusOK, `{"ok":true,"channel":{"id":"D_smoke"}}`), nil
		case testPathChatPostMessage:
			var body struct {
				Channel string `json:"channel"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				return nil, err
			}
			if body.Channel == testAdminUserID {
				<-req.Context().Done()
				return nil, req.Context().Err()
			}
			return testJSONResponse(http.StatusOK, `{"ok":true,"channel":"D_smoke","ts":"1700000000.000100"}`), nil
		default:
			return nil, errors.New("unexpected path " + req.URL.Path)
		}
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	result, err := runSmoke(ctx, &smokeConfig{
		Token:           testSmokeToken,
		UserID:          testAdminUserID,
		Text:            testSmokeText,
		BaseURL:         testSlackAPIBaseURL,
		DirectUserProbe: true,
		HTTPClient:      client,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runSmoke error = %v, want context deadline", err)
	}
	if len(result.ProductionPath) != 2 || !result.ProductionPath[1].OK {
		t.Fatalf("production path = %+v, want successful production path before probe timeout", result.ProductionPath)
	}
	if result.DirectUserProbe == nil || result.DirectUserProbe.Error != apiErrorBudgetExhausted {
		t.Fatalf("direct probe = %+v, want recorded context failure", result.DirectUserProbe)
	}
}

func TestRunSmokeRecordsPerRequestTimeoutDuringNonStrictDirectProbe(t *testing.T) {
	t.Parallel()

	client := &http.Client{
		Timeout: 50 * time.Millisecond,
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case testPathAuthTest:
				return testJSONResponse(http.StatusOK, `{"ok":true}`), nil
			case testPathConversationsOpen:
				return testJSONResponse(http.StatusOK, `{"ok":true,"channel":{"id":"D_smoke"}}`), nil
			case testPathChatPostMessage:
				var body struct {
					Channel string `json:"channel"`
				}
				if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
					return nil, err
				}
				if body.Channel == testAdminUserID {
					<-req.Context().Done()
					return nil, req.Context().Err()
				}
				return testJSONResponse(http.StatusOK, `{"ok":true,"channel":"D_smoke","ts":"1700000000.000100"}`), nil
			default:
				return nil, errors.New("unexpected path " + req.URL.Path)
			}
		}),
	}

	result, err := runSmoke(context.Background(), &smokeConfig{
		Token:           testSmokeToken,
		UserID:          testAdminUserID,
		Text:            testSmokeText,
		BaseURL:         testSlackAPIBaseURL,
		DirectUserProbe: true,
		HTTPClient:      client,
	})
	if err != nil {
		t.Fatalf("runSmoke: %v", err)
	}
	if len(result.ProductionPath) != 2 || !result.ProductionPath[1].OK {
		t.Fatalf("production path = %+v, want successful production path before probe timeout", result.ProductionPath)
	}
	if result.DirectUserProbe == nil || result.DirectUserProbe.Error != apiErrorRequestTimeout {
		t.Fatalf("direct probe = %+v, want recorded per-request timeout", result.DirectUserProbe)
	}
}

func TestRunSmokeClassifiesRealHTTPClientTimeout(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testPathAuthTest:
			_, _ = w.Write([]byte(`{"ok":true}`))
		case testPathConversationsOpen:
			time.Sleep(200 * time.Millisecond)
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"D_smoke"}}`))
		default:
			testServerErrorf(t, w, "unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	result, err := runSmoke(context.Background(), &smokeConfig{
		Token:   testSmokeToken,
		UserID:  testAdminUserID,
		Text:    testSmokeText,
		BaseURL: srv.URL,
		HTTPClient: &http.Client{
			Timeout: 20 * time.Millisecond,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "Client.Timeout exceeded") {
		t.Fatalf("runSmoke error = %v, want real http.Client timeout", err)
	}
	if result.Auth == nil || !result.Auth.OK {
		t.Fatalf("auth result = %+v, want auth success before client timeout", result.Auth)
	}
	if len(result.ProductionPath) != 1 || result.ProductionPath[0].Error != apiErrorRequestTimeout {
		t.Fatalf("production path = %+v, want request-timeout open request", result.ProductionPath)
	}
}

func TestRunSmokeRecordsSuccessfulDirectProbe(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var postedChannels []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testPathAuthTest:
			_, _ = w.Write([]byte(`{"ok":true}`))
		case testPathConversationsOpen:
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"D_smoke"}}`))
		case testPathChatPostMessage:
			var body struct {
				Channel string `json:"channel"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				testServerErrorf(t, w, "decode post body: %v", err)
				return
			}
			mu.Lock()
			postedChannels = append(postedChannels, body.Channel)
			mu.Unlock()
			if body.Channel == testAdminUserID {
				_, _ = w.Write([]byte(`{"ok":true,"channel":"D_probe","ts":"1700000000.000200"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"channel":"D_smoke","ts":"1700000000.000100"}`))
		default:
			testServerErrorf(t, w, "unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	result, err := runSmoke(context.Background(), &smokeConfig{
		Token:           testSmokeToken,
		UserID:          testAdminUserID,
		Text:            testSmokeText,
		BaseURL:         srv.URL,
		DirectUserProbe: true,
	})
	if err != nil {
		t.Fatalf("runSmoke: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(postedChannels, ",") != "D_smoke,"+testAdminUserID {
		t.Fatalf("posted channels = %v, want production D channel plus direct probe user", postedChannels)
	}
	if result.DirectUserProbe == nil || !result.DirectUserProbe.OK {
		t.Fatalf("direct probe = %+v, want success", result.DirectUserProbe)
	}
	if result.DirectUserProbe.PostedChannel != testAdminUserID || result.DirectUserProbe.ChannelID != "D_probe" {
		t.Fatalf("direct probe = %+v, want user-posted successful probe", result.DirectUserProbe)
	}
}

func TestRunSmokeFailsWhenAuthTestFails(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var openCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testPathAuthTest:
			_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
		case testPathConversationsOpen:
			mu.Lock()
			openCalled = true
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"D_smoke"}}`))
		default:
			testServerErrorf(t, w, "unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	result, err := runSmoke(context.Background(), &smokeConfig{
		Token:   testSmokeToken,
		UserID:  testAdminUserID,
		Text:    testSmokeText,
		BaseURL: srv.URL,
	})
	if err == nil || !strings.Contains(err.Error(), "invalid_auth") {
		t.Fatalf("runSmoke error = %v, want invalid_auth", err)
	}
	if result.Auth == nil || result.Auth.OK || result.Auth.Error != "invalid_auth" {
		t.Fatalf("auth result = %+v, want invalid_auth failure", result.Auth)
	}
	if len(result.ProductionPath) != 0 {
		t.Fatalf("production path = %+v, want no production calls after auth failure", result.ProductionPath)
	}
	if result.ProductionPath == nil {
		t.Fatal("production path is nil, want stable empty evidence array")
	}
	raw, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		t.Fatalf("marshal result: %v", marshalErr)
	}
	if !strings.Contains(string(raw), `"production_path":[]`) {
		t.Fatalf("result JSON = %s, want empty production_path array", string(raw))
	}
	mu.Lock()
	defer mu.Unlock()
	if openCalled {
		t.Fatal("conversations.open should not run when auth.test fails")
	}
}

func TestRunSmokeReturnsContextCancellation(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		called = true
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := runSmoke(ctx, &smokeConfig{
		Token:   testSmokeToken,
		UserID:  testAdminUserID,
		Text:    testSmokeText,
		BaseURL: srv.URL,
	})
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("runSmoke error = %v, want context canceled", err)
	}
	if result.Auth == nil || result.Auth.Error != apiErrorRequestCanceled {
		t.Fatalf("auth result = %+v, want %s", result.Auth, apiErrorRequestCanceled)
	}
	mu.Lock()
	defer mu.Unlock()
	if called {
		t.Fatal("server should not receive a request after context cancellation")
	}
}

func TestRunSmokeRejectsControlCharacterToken(t *testing.T) {
	t.Parallel()

	result, err := runSmoke(context.Background(), &smokeConfig{
		Token:  "xoxb-test\nbad",
		UserID: testAdminUserID,
		Text:   testSmokeText,
	})
	if err == nil || !strings.Contains(err.Error(), "control characters") {
		t.Fatalf("runSmoke error = %v, want control character token error", err)
	}
	if result.UserID != testAdminUserID {
		t.Fatalf("result = %+v, want sanitized user id evidence", result)
	}
}

func TestRunSmokeRejectsEmptyUserIDAfterCleaning(t *testing.T) {
	t.Parallel()

	result, err := runSmoke(context.Background(), &smokeConfig{
		Token:  testSmokeToken,
		UserID: " \t\r\n ",
		Text:   testSmokeText,
	})
	if err == nil || !strings.Contains(err.Error(), "missing Slack user ID") {
		t.Fatalf("runSmoke error = %v, want missing user id", err)
	}
	if result.UserID != "" {
		t.Fatalf("result = %+v, want empty sanitized user id evidence", result)
	}
	if len(result.ProductionPath) != 0 || result.Auth != nil {
		t.Fatalf("result = %+v, want no Slack calls", result)
	}
}

func TestRunSmokeRejectsUserIDSeparatorWhitespaceOrControlCharacters(t *testing.T) {
	t.Parallel()

	for _, userID := range []string{"U_admin\nbad", "U_admin,U_other"} {
		t.Run(userID, func(t *testing.T) {
			t.Parallel()

			result, err := runSmoke(context.Background(), &smokeConfig{
				Token:  testSmokeToken,
				UserID: userID,
				Text:   testSmokeText,
			})
			if !errors.Is(err, errSlackUserIDSeparatorControl) {
				t.Fatalf("runSmoke error = %v, want %v", err, errSlackUserIDSeparatorControl)
			}
			if result.UserID != "" || result.Auth != nil || len(result.ProductionPath) != 0 {
				t.Fatalf("result = %+v, want no Slack calls or mangled user id", result)
			}
		})
	}
}

func TestRunSmokeRejectsOverlongText(t *testing.T) {
	t.Parallel()

	result, err := runSmoke(context.Background(), &smokeConfig{
		Token:   testSmokeToken,
		UserID:  testAdminUserID,
		Text:    strings.Repeat("x", maxSmokeTextBytes+1),
		BaseURL: testSlackAPIBaseURL,
	})
	if !errors.Is(err, errSmokeTextTooLong) {
		t.Fatalf("runSmoke error = %v, want %v", err, errSmokeTextTooLong)
	}
	if result.UserID != testAdminUserID {
		t.Fatalf("result user_id = %q, want sanitized user id evidence", result.UserID)
	}
	if result.Auth != nil || len(result.ProductionPath) != 0 {
		t.Fatalf("result = %+v, want no Slack calls after text validation failure", result)
	}
}

func TestRunSmokeRejectsUserAgentControlCharacters(t *testing.T) {
	t.Parallel()

	result, err := runSmoke(context.Background(), &smokeConfig{
		Token:     testSmokeToken,
		UserID:    testAdminUserID,
		Text:      testSmokeText,
		BaseURL:   testSlackAPIBaseURL,
		UserAgent: "qurl-smoke\nbad",
	})
	if !errors.Is(err, errUserAgentControlCharacters) {
		t.Fatalf("runSmoke error = %v, want %v", err, errUserAgentControlCharacters)
	}
	if result.UserID != testAdminUserID {
		t.Fatalf("result user_id = %q, want sanitized user id evidence", result.UserID)
	}
	if result.Auth != nil || len(result.ProductionPath) != 0 {
		t.Fatalf("result = %+v, want no Slack calls after user-agent validation failure", result)
	}
}

func TestRunSmokeRejectsInsecureRemoteBaseURL(t *testing.T) {
	t.Parallel()

	result, err := runSmoke(context.Background(), &smokeConfig{
		Token:   testSmokeToken,
		UserID:  testAdminUserID,
		Text:    testSmokeText,
		BaseURL: "http://slack.example/api",
	})
	if !errors.Is(err, errBaseURLRequiresHTTPS) {
		t.Fatalf("runSmoke error = %v, want %v", err, errBaseURLRequiresHTTPS)
	}
	if result.UserID != testAdminUserID {
		t.Fatalf("result user_id = %q, want sanitized user id evidence", result.UserID)
	}
	if result.Auth != nil || len(result.ProductionPath) != 0 || result.DirectUserProbe != nil {
		t.Fatalf("result = %+v, want no Slack calls after base URL validation failure", result)
	}
}

func TestNormalizeSlackBaseURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr error
	}{
		{
			name: "empty defaults",
			raw:  "",
			want: defaultSlackAPIBaseURL,
		},
		{
			name: "trims https trailing slash",
			raw:  "https://slack.com/api/",
			want: defaultSlackAPIBaseURL,
		},
		{
			name: "returns parsed clean URL",
			raw:  "https://slack.com/api/~smoke/",
			want: "https://slack.com/api/~smoke",
		},
		{
			name: "allows localhost http",
			raw:  "http://localhost:1234/api/",
			want: "http://localhost:1234/api",
		},
		{
			name: "allows ipv4 loopback http",
			raw:  "http://127.0.0.1:1234/api",
			want: "http://127.0.0.1:1234/api",
		},
		{
			name: "allows ipv6 loopback http",
			raw:  "http://[::1]:1234/api",
			want: "http://[::1]:1234/api",
		},
		{
			name:    "rejects query",
			raw:     "https://slack.com/api?x=1",
			wantErr: errBaseURLQueryFragment,
		},
		{
			name:    "rejects fragment",
			raw:     "https://slack.com/api#token",
			wantErr: errBaseURLQueryFragment,
		},
		{
			name:    "rejects userinfo",
			raw:     "https://user:pass@slack.com/api",
			wantErr: errBaseURLUserinfo,
		},
		{
			name:    "rejects malformed URL",
			raw:     "http://[::1",
			wantErr: errors.New("invalid -base-url"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := normalizeSlackBaseURL(tc.raw)
			if tc.wantErr != nil {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr.Error()) {
					t.Fatalf("normalizeSlackBaseURL(%q) error = %v, want %v", tc.raw, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeSlackBaseURL(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("normalizeSlackBaseURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestPostRawRejectsOversizeResponse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxSlackResponseBytes+1)))
	}))
	t.Cleanup(srv.Close)

	client := slackClient{
		token:      testSmokeToken,
		baseURL:    srv.URL,
		userAgent:  defaultUserAgent,
		httpClient: newSlackHTTPClient(defaultRequestTimeout),
	}
	result, _, err := client.postRaw(context.Background(), "auth.test", map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("postRaw error = %v, want oversize response", err)
	}
	if result.Error != apiErrorResponseTooLarge {
		t.Fatalf("result = %+v, want %s", result, apiErrorResponseTooLarge)
	}
}

func TestPostRawDrainsOversizeResponse(t *testing.T) {
	t.Parallel()

	body := &drainTrackingBody{reader: strings.NewReader(strings.Repeat("x", maxSlackResponseBytes+2))}
	client := slackClient{
		token:     testSmokeToken,
		baseURL:   testSlackAPIBaseURL,
		userAgent: defaultUserAgent,
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       body,
			}, nil
		})},
	}
	result, _, err := client.postRaw(context.Background(), "auth.test", nil)
	if err == nil || result.Error != apiErrorResponseTooLarge {
		t.Fatalf("postRaw result=%+v error=%v, want oversize response", result, err)
	}
	if !body.drained {
		t.Fatal("oversize response body was not drained")
	}
	if !body.closed {
		t.Fatal("oversize response body was not closed")
	}
}

func TestPostRawReturnsHTTPStatusError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	client := slackClient{
		token:      testSmokeToken,
		baseURL:    srv.URL,
		userAgent:  defaultUserAgent,
		httpClient: newSlackHTTPClient(defaultRequestTimeout),
	}
	result, _, err := client.postRaw(context.Background(), "auth.test", map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("postRaw error = %v, want HTTP 503", err)
	}
	if result.StatusCode != http.StatusServiceUnavailable || result.Error != "http_503" {
		t.Fatalf("result = %+v, want HTTP status details", result)
	}
}

func TestPostRawDoesNotFollowRedirect(t *testing.T) {
	t.Parallel()

	followed := make(chan string, 1)
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		followed <- r.Header.Get("Authorization")
	}))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	client := slackClient{
		token:      testSmokeToken,
		baseURL:    redirector.URL,
		userAgent:  defaultUserAgent,
		httpClient: newSlackHTTPClient(defaultRequestTimeout),
	}
	result, _, err := client.postRaw(context.Background(), "auth.test", nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("postRaw error = %v, want HTTP 302", err)
	}
	if result.StatusCode != http.StatusFound || result.Error != "http_302" {
		t.Fatalf("result = %+v, want redirect surfaced as HTTP status", result)
	}
	select {
	case auth := <-followed:
		t.Fatalf("redirect target was followed with Authorization = %q", auth)
	default:
	}
}

func TestPostRawRecordsRetryAfterForRateLimit(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "12")
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	client := slackClient{
		token:      testSmokeToken,
		baseURL:    srv.URL,
		userAgent:  defaultUserAgent,
		httpClient: newSlackHTTPClient(defaultRequestTimeout),
	}
	result, _, err := client.postRaw(context.Background(), "chat.postMessage", map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 429") {
		t.Fatalf("postRaw error = %v, want HTTP 429", err)
	}
	if result.StatusCode != http.StatusTooManyRequests || result.Error != "http_429" || result.RetryAfter != "12" {
		t.Fatalf("result = %+v, want HTTP 429 with retry_after", result)
	}
}

func TestPostRawRateLimitWinsOverOversizeBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(strings.Repeat("x", maxSlackResponseBytes+1)))
	}))
	t.Cleanup(srv.Close)

	client := slackClient{
		token:      testSmokeToken,
		baseURL:    srv.URL,
		userAgent:  defaultUserAgent,
		httpClient: newSlackHTTPClient(defaultRequestTimeout),
	}
	result, _, err := client.postRaw(context.Background(), "chat.postMessage", nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 429") {
		t.Fatalf("postRaw error = %v, want HTTP 429", err)
	}
	if result.StatusCode != http.StatusTooManyRequests || result.Error != "http_429" || result.RetryAfter != "12" {
		t.Fatalf("result = %+v, want HTTP 429 to win over oversize body", result)
	}
}

func TestCleanSlackFieldSanitizesControlCharacters(t *testing.T) {
	t.Parallel()

	got := cleanSlackField(" \talpha\nbeta\rgamma\tdelta\x01\x7f ")
	want := "alpha beta gamma delta??"
	if got != want {
		t.Fatalf("cleanSlackField = %q, want %q", got, want)
	}
}

func TestPostRawIgnoresRetryAfterOnSuccess(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "12")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	client := slackClient{
		token:      testSmokeToken,
		baseURL:    srv.URL,
		userAgent:  defaultUserAgent,
		httpClient: newSlackHTTPClient(defaultRequestTimeout),
	}
	result, _, err := client.postRaw(context.Background(), "auth.test", map[string]string{})
	if err != nil {
		t.Fatalf("postRaw: %v", err)
	}
	if result.RetryAfter != "" {
		t.Fatalf("result = %+v, want no retry_after on success", result)
	}
}

func TestPostRawInvalidJSONErrorOmitsResponseBody(t *testing.T) {
	t.Parallel()

	const marker = `{"ok":true,"sensitive":"payload-marker`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(marker))
	}))
	t.Cleanup(srv.Close)

	client := slackClient{
		token:      testSmokeToken,
		baseURL:    srv.URL,
		userAgent:  defaultUserAgent,
		httpClient: newSlackHTTPClient(defaultRequestTimeout),
	}
	result, _, err := client.postRaw(context.Background(), "auth.test", map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "response JSON") {
		t.Fatalf("postRaw error = %v, want response JSON error", err)
	}
	if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "payload-marker") {
		t.Fatalf("postRaw error exposed response body marker: %v", err)
	}
	if result.Error != "response_json" {
		t.Fatalf("result = %+v, want response_json", result)
	}
}

func TestRunRedactsTokenFromJSONOutput(t *testing.T) {
	t.Parallel()

	// The token is intentionally never added to smokeResult; this guards that
	// result-shape invariant against future evidence fields.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testPathAuthTest:
			_, _ = w.Write([]byte(`{"ok":true}`))
		case testPathConversationsOpen:
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"D_smoke"}}`))
		case testPathChatPostMessage:
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			testServerErrorf(t, w, "unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), &stdout, &stderr, []string{
		testFlagTokenEnv, testSmokeTokenEnv,
		testFlagUser, testAdminUserID,
		testFlagText, testSmokeText,
		testFlagBaseURL, srv.URL,
	}, func(name string) string {
		if name == testSmokeTokenEnv {
			return testSecretSmokeToken
		}
		return ""
	}, func() time.Time {
		return time.Unix(1800000000, 0).UTC()
	})
	if code != 0 {
		t.Fatalf("run code = %d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	if strings.Contains(stdout.String(), testSecretSmokeToken) || strings.Contains(stderr.String(), testSecretSmokeToken) {
		t.Fatalf("token leaked in output: stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
}

func TestRunRedactsTokenFromTransportErrorOutput(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("closed test server should not receive requests")
	}))
	baseURL := srv.URL
	srv.Close()

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), &stdout, &stderr, []string{
		testFlagTokenEnv, testSmokeTokenEnv,
		testFlagUser, testAdminUserID,
		testFlagBaseURL, baseURL,
	}, func(name string) string {
		if name == testSmokeTokenEnv {
			return testSecretSmokeToken
		}
		return ""
	}, func() time.Time {
		return time.Unix(1800000000, 0).UTC()
	})
	if code != 1 {
		t.Fatalf("run code = %d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stderr.String(), "auth.test request") {
		t.Fatalf("stderr=%q, want transport request error", stderr.String())
	}
	if strings.Contains(stdout.String(), testSecretSmokeToken) || strings.Contains(stderr.String(), testSecretSmokeToken) {
		t.Fatalf("token leaked in output: stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
}

func TestRunParsesBaseURLAndStrictDirectProbeFlags(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var gotPaths []string
	var postedChannels []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPaths = append(gotPaths, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case testPathAuthTest:
			_, _ = w.Write([]byte(`{"ok":true}`))
		case testPathConversationsOpen:
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"D_smoke"}}`))
		case testPathChatPostMessage:
			var body struct {
				Channel string `json:"channel"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				testServerErrorf(t, w, "decode post body: %v", err)
				return
			}
			mu.Lock()
			postedChannels = append(postedChannels, body.Channel)
			mu.Unlock()
			if body.Channel == testAdminUserID {
				_, _ = w.Write([]byte(`{"ok":true,"channel":"D_probe","ts":"1700000000.000200"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true,"channel":"D_smoke","ts":"1700000000.000100"}`))
		default:
			testServerErrorf(t, w, "unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), &stdout, &stderr, []string{
		testFlagTokenEnv, testSmokeTokenEnv,
		testFlagUser, testAdminUserID,
		testFlagText, testSmokeText,
		testFlagBaseURL, srv.URL + "/",
		"-direct-user-probe",
		"-strict-direct-user-probe",
	}, func(name string) string {
		if name == testSmokeTokenEnv {
			return testSmokeToken
		}
		return ""
	}, func() time.Time {
		return time.Unix(1800000000, 0).UTC()
	})
	if code != 0 {
		t.Fatalf("run code = %d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}

	var result smokeResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode stdout: %v stdout=%s", err, stdout.String())
	}
	if result.DirectUserProbe == nil || !result.DirectUserProbe.OK {
		t.Fatalf("direct probe = %+v, want strict successful probe", result.DirectUserProbe)
	}
	mu.Lock()
	defer mu.Unlock()
	wantPaths := strings.Join([]string{
		testPathAuthTest,
		testPathConversationsOpen,
		testPathChatPostMessage,
		testPathChatPostMessage,
	}, ",")
	if strings.Join(gotPaths, ",") != wantPaths {
		t.Fatalf("paths = %v, want base URL trimmed and four exact Slack paths", gotPaths)
	}
	if strings.Join(postedChannels, ",") != "D_smoke,"+testAdminUserID {
		t.Fatalf("posted channels = %v, want production D channel plus direct probe user", postedChannels)
	}
}

func TestRunSmokeHonorsOverallTimeoutAcrossRequests(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case testPathAuthTest:
			return testJSONResponse(http.StatusOK, `{"ok":true}`), nil
		case testPathConversationsOpen:
			<-req.Context().Done()
			return nil, req.Context().Err()
		default:
			return nil, errors.New("unexpected path " + req.URL.Path)
		}
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	result, err := runSmoke(ctx, &smokeConfig{
		Token:      testSmokeToken,
		UserID:     testAdminUserID,
		Text:       testSmokeText,
		BaseURL:    testSlackAPIBaseURL,
		HTTPClient: client,
	})
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Fatalf("runSmoke error = %v, want context deadline", err)
	}
	if result.Auth == nil || !result.Auth.OK {
		t.Fatalf("auth result = %+v, want auth success before deadline", result.Auth)
	}
	if len(result.ProductionPath) != 1 || result.ProductionPath[0].Error != apiErrorBudgetExhausted {
		t.Fatalf("production path = %+v, want budget-exhausted open request", result.ProductionPath)
	}
}

func TestRunRejectsInvalidArgs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		args       []string
		env        map[string]string
		wantStderr string
	}{
		{
			name:       "missing token env",
			args:       []string{testFlagUser, testAdminUserID},
			wantStderr: "SLACK_BOT_TOKEN is not set or is empty",
		},
		{
			name:       "missing token env before missing user",
			args:       nil,
			wantStderr: "SLACK_BOT_TOKEN is not set or is empty",
		},
		{
			name:       "empty token env name",
			args:       []string{testFlagTokenEnv, " ", testFlagUser, testAdminUserID},
			wantStderr: "-token-env is required",
		},
		{
			name:       "missing user",
			args:       []string{testFlagTokenEnv, testSmokeTokenEnv},
			env:        map[string]string{testSmokeTokenEnv: testSmokeToken},
			wantStderr: "-user is required",
		},
		{
			name:       "user contains whitespace",
			args:       []string{testFlagTokenEnv, testSmokeTokenEnv, testFlagUser, "U_admin bad"},
			env:        map[string]string{testSmokeTokenEnv: testSmokeToken},
			wantStderr: "-user contains comma, ASCII whitespace, or ASCII control characters",
		},
		{
			name:       "user contains comma separator",
			args:       []string{testFlagTokenEnv, testSmokeTokenEnv, testFlagUser, "U_admin,U_other"},
			env:        map[string]string{testSmokeTokenEnv: testSmokeToken},
			wantStderr: "-user contains comma, ASCII whitespace, or ASCII control characters",
		},
		{
			name:       "token contains control character",
			args:       []string{testFlagTokenEnv, testSmokeTokenEnv, testFlagUser, testAdminUserID},
			env:        map[string]string{testSmokeTokenEnv: "xoxb-test\nbad"},
			wantStderr: "SMOKE_TOKEN contains control characters",
		},
		{
			name:       "non-positive timeout",
			args:       []string{testFlagTokenEnv, testSmokeTokenEnv, testFlagUser, testAdminUserID, testFlagTimeout, "0s"},
			env:        map[string]string{testSmokeTokenEnv: testSmokeToken},
			wantStderr: "-timeout must be greater than 0",
		},
		{
			name:       "non-positive request timeout",
			args:       []string{testFlagTokenEnv, testSmokeTokenEnv, testFlagUser, testAdminUserID, testFlagRequestTimeout, "0s"},
			env:        map[string]string{testSmokeTokenEnv: testSmokeToken},
			wantStderr: "-request-timeout must be greater than 0",
		},
		{
			name:       "request timeout exceeds overall timeout",
			args:       []string{testFlagTokenEnv, testSmokeTokenEnv, testFlagUser, testAdminUserID, testFlagTimeout, "1s", testFlagRequestTimeout, "2s"},
			env:        map[string]string{testSmokeTokenEnv: testSmokeToken},
			wantStderr: "-request-timeout must be less than -timeout",
		},
		{
			name:       "request timeout equals overall timeout",
			args:       []string{testFlagTokenEnv, testSmokeTokenEnv, testFlagUser, testAdminUserID, testFlagTimeout, "1s", testFlagRequestTimeout, "1s"},
			env:        map[string]string{testSmokeTokenEnv: testSmokeToken},
			wantStderr: "-request-timeout must be less than -timeout",
		},
		{
			name:       "request timeout leaves too little headroom",
			args:       []string{testFlagTokenEnv, testSmokeTokenEnv, testFlagUser, testAdminUserID, testFlagTimeout, "29s", testFlagRequestTimeout, "10s"},
			env:        map[string]string{testSmokeTokenEnv: testSmokeToken},
			wantStderr: "-timeout must be at least 3x -request-timeout",
		},
		{
			name:       "direct probe request timeout leaves too little headroom",
			args:       []string{testFlagTokenEnv, testSmokeTokenEnv, testFlagUser, testAdminUserID, "-direct-user-probe", testFlagTimeout, "39s", testFlagRequestTimeout, "10s"},
			env:        map[string]string{testSmokeTokenEnv: testSmokeToken},
			wantStderr: "-timeout must be at least 4x -request-timeout",
		},
		{
			name:       "overlong text",
			args:       []string{testFlagTokenEnv, testSmokeTokenEnv, testFlagUser, testAdminUserID, testFlagText, strings.Repeat("x", maxSmokeTextBytes+1)},
			env:        map[string]string{testSmokeTokenEnv: testSmokeToken},
			wantStderr: "-text must be at most 4000 bytes after cleanup",
		},
		{
			name:       "user agent contains control character",
			args:       []string{testFlagTokenEnv, testSmokeTokenEnv, testFlagUser, testAdminUserID, testFlagUserAgent, "qurl-smoke\nbad"},
			env:        map[string]string{testSmokeTokenEnv: testSmokeToken},
			wantStderr: "-user-agent contains control characters",
		},
		{
			name:       "strict direct probe without direct probe",
			args:       []string{testFlagTokenEnv, testSmokeTokenEnv, testFlagUser, testAdminUserID, "-strict-direct-user-probe"},
			env:        map[string]string{testSmokeTokenEnv: testSmokeToken},
			wantStderr: "-strict-direct-user-probe requires -direct-user-probe",
		},
		{
			name:       "insecure remote base URL",
			args:       []string{testFlagTokenEnv, testSmokeTokenEnv, testFlagUser, testAdminUserID, testFlagBaseURL, "http://slack.example/api"},
			env:        map[string]string{testSmokeTokenEnv: testSmokeToken},
			wantStderr: "-base-url must use https unless host is localhost or loopback",
		},
		{
			name:       "base URL query",
			args:       []string{testFlagTokenEnv, testSmokeTokenEnv, testFlagUser, testAdminUserID, testFlagBaseURL, "https://slack.com/api?x=1"},
			env:        map[string]string{testSmokeTokenEnv: testSmokeToken},
			wantStderr: "-base-url must not include query or fragment",
		},
		{
			name:       "base URL userinfo",
			args:       []string{testFlagTokenEnv, testSmokeTokenEnv, testFlagUser, testAdminUserID, testFlagBaseURL, "https://user:pass@slack.com/api"},
			env:        map[string]string{testSmokeTokenEnv: testSmokeToken},
			wantStderr: "-base-url must not include userinfo",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			code := run(context.Background(), &stdout, &stderr, tc.args, func(name string) string {
				return tc.env[name]
			}, func() time.Time {
				return time.Unix(1800000000, 0).UTC()
			})
			if code != 2 {
				t.Fatalf("run code = %d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout=%q, want empty", stdout.String())
			}
			if !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Fatalf("stderr=%q, want %q", stderr.String(), tc.wantStderr)
			}
		})
	}
}

func TestRunHelpExitsZero(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), &stdout, &stderr, []string{"-h"}, func(string) string {
		return ""
	}, func() time.Time {
		return time.Unix(1800000000, 0).UTC()
	})
	if code != 0 {
		t.Fatalf("run code = %d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stderr.String(), "Usage of slack-dm-smoke") {
		t.Fatalf("stderr=%q, want usage", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout=%q, want empty", stdout.String())
	}
}

func TestRunReportsSmokeErrorWhenJSONOutputFails(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case testPathAuthTest:
			_, _ = w.Write([]byte(`{"ok":true}`))
		case testPathConversationsOpen:
			_, _ = w.Write([]byte(`{"ok":false,"error":"missing_scope"}`))
		default:
			testServerErrorf(t, w, "unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	var stderr bytes.Buffer
	code := run(context.Background(), errWriter{}, &stderr, []string{
		testFlagTokenEnv, testSmokeTokenEnv,
		testFlagUser, testAdminUserID,
		testFlagText, testSmokeText,
		testFlagBaseURL, srv.URL,
	}, func(name string) string {
		if name == testSmokeTokenEnv {
			return testSecretSmokeToken
		}
		return ""
	}, func() time.Time {
		return time.Unix(1800000000, 0).UTC()
	})
	if code != 1 {
		t.Fatalf("run code = %d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "conversations.open: missing_scope") {
		t.Fatalf("stderr=%q, want smoke error", stderr.String())
	}
	if !strings.Contains(stderr.String(), "write result: write failed") {
		t.Fatalf("stderr=%q, want write error", stderr.String())
	}
}

type errWriter struct{}

func (errWriter) Write(_ []byte) (int, error) {
	return 0, errors.New("write failed")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testJSONResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

type drainTrackingBody struct {
	reader  *strings.Reader
	drained bool
	closed  bool
}

func (b *drainTrackingBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if errors.Is(err, io.EOF) {
		b.drained = true
	}
	return n, err
}

func (b *drainTrackingBody) Close() error {
	b.closed = true
	return nil
}
