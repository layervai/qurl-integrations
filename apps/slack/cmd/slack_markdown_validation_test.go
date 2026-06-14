package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal"
)

const testSlackValidationToken = "xoxb-123456789012345678901234567890"

const (
	testSlackValidationChannel    = "C123"
	testSlackValidationTeam       = "T123"
	testSlackValidationEnterprise = "E123"
	testSlackValidationUA         = "qurl-slack-markdown-validator/test"
	testSlackValidationTS         = "1700000000.000100"
	testSlackAssistantChannel     = "D123"
	testSlackAssistantThreadTS    = "1700000000.000001"
	testSlackAssistantUser        = "U123"
	testSlackValidationStartPath  = "/start"
	testSlackValidationAppendPath = "/append"
	testSlackValidationStopPath   = "/stop"
	testSlackValidationMaskedLink = "Use [billing](https://example.com/login)."
	testSlackValidationCheck      = "check"
	testSlackValidationTimeoutArg = "--timeout"
	testSlackValidationHelpArg    = "--help"
	testSlackValidationAckValue   = "true"
)

func TestParseSlackMarkdownValidationConfigRequiresTokenAndChannel(t *testing.T) {
	t.Parallel()
	env := map[string]string{}
	getenv := func(key string) string { return env[key] }

	if _, err := parseSlackMarkdownValidationConfig(nil, getenv); err == nil || !strings.Contains(err.Error(), envSlackMarkdownValidationToken) {
		t.Fatalf("missing token error = %v, want token requirement", err)
	}
	env[envSlackMarkdownValidationToken] = testSlackValidationToken
	if _, err := parseSlackMarkdownValidationConfig(nil, getenv); err == nil || !strings.Contains(err.Error(), envSlackMarkdownValidationChannel) {
		t.Fatalf("missing channel error = %v, want channel requirement", err)
	}
	env[envSlackMarkdownValidationChannel] = testSlackValidationChannel
	if _, err := parseSlackMarkdownValidationConfig(nil, getenv); err == nil || !strings.Contains(err.Error(), envSlackMarkdownValidationPersistentAck) {
		t.Fatalf("missing persistent-message ack error = %v, want ack requirement", err)
	}
	env[envSlackMarkdownValidationPersistentAck] = testSlackValidationAckValue
	env[envSlackMarkdownValidationTimeout] = "2m"
	cfg, err := parseSlackMarkdownValidationConfig([]string{"--team-id", testSlackValidationTeam, testSlackValidationTimeoutArg, "30s"}, getenv)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if cfg.token != testSlackValidationToken || cfg.channelID != testSlackValidationChannel || cfg.teamID != testSlackValidationTeam {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.timeout != 30*time.Second {
		t.Fatalf("timeout = %s, want flag override", cfg.timeout)
	}
	env[envSlackMarkdownValidationAssistantChannel] = testSlackAssistantChannel
	if _, err := parseSlackMarkdownValidationConfig(nil, getenv); err == nil || !strings.Contains(err.Error(), "partial assistant-pane config") {
		t.Fatalf("partial assistant config error = %v, want fail-fast config error", err)
	}
}

func TestParseSlackMarkdownValidationConfigRejectsInvalidTimeout(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		envSlackMarkdownValidationToken:         testSlackValidationToken,
		envSlackMarkdownValidationChannel:       testSlackValidationChannel,
		envSlackMarkdownValidationTimeout:       "later",
		envSlackMarkdownValidationPersistentAck: testSlackValidationAckValue,
	}
	if _, err := parseSlackMarkdownValidationConfig(nil, func(key string) string { return env[key] }); err == nil || !strings.Contains(err.Error(), envSlackMarkdownValidationTimeout) {
		t.Fatalf("invalid timeout error = %v, want timeout requirement", err)
	}
	cfg, err := parseSlackMarkdownValidationConfig([]string{testSlackValidationTimeoutArg, "30s"}, func(key string) string { return env[key] })
	if err != nil {
		t.Fatalf("flag timeout should override invalid env timeout: %v", err)
	}
	if cfg.timeout != 30*time.Second {
		t.Fatalf("timeout = %s, want flag override", cfg.timeout)
	}
	if _, err := parseSlackMarkdownValidationConfig([]string{testSlackValidationHelpArg}, func(key string) string { return env[key] }); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help error = %v, want flag.ErrHelp before env timeout validation", err)
	}
	env[envSlackMarkdownValidationTimeout] = "1m"
	if _, err := parseSlackMarkdownValidationConfig([]string{testSlackValidationTimeoutArg, "0"}, func(key string) string { return env[key] }); err == nil || !strings.Contains(err.Error(), "greater than zero") {
		t.Fatalf("zero timeout error = %v, want positive timeout requirement", err)
	}
}

func TestParseSlackMarkdownValidationConfigPersistentAckPrecedence(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		envSlackMarkdownValidationToken:         testSlackValidationToken,
		envSlackMarkdownValidationChannel:       testSlackValidationChannel,
		envSlackMarkdownValidationPersistentAck: "maybe",
	}
	if _, err := parseSlackMarkdownValidationConfig(nil, func(key string) string { return env[key] }); err == nil || !strings.Contains(err.Error(), envSlackMarkdownValidationPersistentAck) {
		t.Fatalf("invalid persistent-message ack error = %v, want ack requirement", err)
	}
	cfg, err := parseSlackMarkdownValidationConfig([]string{"--ack-persistent-messages"}, func(key string) string { return env[key] })
	if err != nil {
		t.Fatalf("flag ack should override invalid env ack: %v", err)
	}
	if !cfg.ackPersistentMessages {
		t.Fatal("ackPersistentMessages = false, want flag override")
	}
	if _, err := parseSlackMarkdownValidationConfig([]string{testSlackValidationHelpArg}, func(key string) string { return env[key] }); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help error = %v, want flag.ErrHelp before env ack validation", err)
	}
}

func TestParseSlackMarkdownValidationConfigRejectsMalformedToken(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		envSlackMarkdownValidationToken:   "not-a-token",
		envSlackMarkdownValidationChannel: testSlackValidationChannel,
	}
	if _, err := parseSlackMarkdownValidationConfig(nil, func(key string) string { return env[key] }); err == nil {
		t.Fatal("parse config accepted malformed bot token")
	}
}

func TestRunSlackMarkdownRendererValidationCLIHelpReturnsCleanly(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := runSlackMarkdownRendererValidationCLI([]string{testSlackValidationHelpArg}, &out); err != nil {
		t.Fatalf("help error = %v, want nil", err)
	}
	if out.Len() != 0 {
		t.Fatalf("help wrote JSON output: %q", out.String())
	}
}

func TestRunSlackMarkdownRendererValidationPostsEvidenceWithoutAssistantConfig(t *testing.T) {
	t.Parallel()
	var requests []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+testSlackValidationToken {
			t.Fatalf("Authorization = %q, want bearer validation token", got)
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		requests = append(requests, body)
		_, _ = w.Write([]byte(`{"ok":true,"ts":"` + testSlackValidationTS + `"}`))
	}))
	t.Cleanup(srv.Close)

	report, err := runSlackMarkdownRendererValidation(context.Background(), &slackMarkdownValidationConfig{
		token:                 testSlackValidationToken,
		channelID:             testSlackValidationChannel,
		teamID:                testSlackValidationTeam,
		enterpriseID:          testSlackValidationEnterprise,
		ackPersistentMessages: true,
		postMessageURL:        srv.URL,
		startStreamURL:        srv.URL,
		appendStreamURL:       srv.URL,
		stopStreamURL:         srv.URL,
		userAgent:             testSlackValidationUA,
		now:                   func() time.Time { return time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC) },
		httpClient:            srv.Client(),
	})
	if err != nil {
		t.Fatalf("run validation: %v", err)
	}
	if report.GeneratedAt != "2026-06-13T12:00:00Z" || report.ChannelID != testSlackValidationChannel || report.TeamID != testSlackValidationTeam || report.EnterpriseID != testSlackValidationEnterprise {
		t.Fatalf("report metadata = %+v", report)
	}
	if report.Status != slackMarkdownValidationStatusDelivered ||
		report.RendererVerdict != slackMarkdownValidationRendererReviewRequired ||
		report.ReviewInstructions != slackMarkdownValidationReviewInstructions ||
		report.Error != "" {
		t.Fatalf("report status/verdict/instructions/error = %q/%q/%q/%q", report.Status, report.RendererVerdict, report.ReviewInstructions, report.Error)
	}
	if len(report.Cases) != len(slackMarkdownRendererValidationCases())+1 {
		t.Fatalf("cases = %d, want validation cases plus compatibility case", len(report.Cases))
	}
	if len(report.Surfaces) != 2 ||
		report.Surfaces[0].Status != slackMarkdownValidationStatusDelivered ||
		report.Surfaces[1].Status != slackMarkdownValidationStatusSkipped {
		t.Fatalf("surfaces = %+v", report.Surfaces)
	}
	if len(requests) != len(report.Cases) {
		t.Fatalf("requests = %d, cases = %d", len(requests), len(report.Cases))
	}
	for i, req := range requests[1:] {
		if req["thread_ts"] != testSlackValidationTS {
			t.Fatalf("request %d thread_ts = %v, want first case ts %s", i+1, req["thread_ts"], testSlackValidationTS)
		}
	}
	if _, ok := requests[0]["blocks"]; !ok {
		t.Fatalf("first request = %+v, want markdown blocks", requests[0])
	}
	if report.Cases[0].FallbackText == "" || report.Cases[0].FallbackText != requests[0]["text"] {
		t.Fatalf("fallback evidence = %q, request text = %v", report.Cases[0].FallbackText, requests[0]["text"])
	}
	last := requests[len(requests)-1]
	if _, ok := last["markdown_text"]; !ok {
		t.Fatalf("last request = %+v, want markdown_text compatibility body", last)
	}
	if _, ok := last["blocks"]; ok {
		t.Fatalf("compatibility request must not include blocks: %+v", last)
	}
}

func TestRunSlackMarkdownRendererValidationPostsAssistantSurface(t *testing.T) {
	t.Parallel()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/post":
			_, _ = w.Write([]byte(`{"ok":true,"ts":"` + testSlackValidationTS + `"}`))
		case testSlackValidationStartPath:
			_, _ = w.Write([]byte(`{"ok":true,"ts":"1700000000.000300"}`))
		case testSlackValidationAppendPath, testSlackValidationStopPath:
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	report, err := runSlackMarkdownRendererValidation(context.Background(), &slackMarkdownValidationConfig{
		token:                    testSlackValidationToken,
		channelID:                testSlackValidationChannel,
		teamID:                   testSlackValidationTeam,
		ackPersistentMessages:    true,
		postMessageURL:           srv.URL + "/post",
		startStreamURL:           srv.URL + testSlackValidationStartPath,
		appendStreamURL:          srv.URL + testSlackValidationAppendPath,
		stopStreamURL:            srv.URL + testSlackValidationStopPath,
		userAgent:                testSlackValidationUA,
		assistantChannelID:       testSlackAssistantChannel,
		assistantThreadTS:        testSlackAssistantThreadTS,
		assistantRecipientTeamID: testSlackValidationTeam,
		assistantRecipientUserID: testSlackAssistantUser,
		now:                      func() time.Time { return time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC) },
		httpClient:               srv.Client(),
	})
	if err != nil {
		t.Fatalf("run validation with assistant config: %v", err)
	}
	wantCases := len(slackMarkdownRendererValidationCases())*2 + 1
	if len(report.Cases) != wantCases {
		t.Fatalf("cases = %d, want channel cases + compatibility + assistant cases (%d)", len(report.Cases), wantCases)
	}
	if len(report.Surfaces) != 2 ||
		report.Surfaces[0].Status != slackMarkdownValidationStatusDelivered ||
		report.Surfaces[1].Status != slackMarkdownValidationStatusDelivered {
		t.Fatalf("surfaces = %+v", report.Surfaces)
	}
	var streamCases int
	for _, c := range report.Cases {
		if c.Surface != slackMarkdownValidationSurfaceAssistantPaneStream {
			continue
		}
		streamCases++
		if c.RequestShape != slackMarkdownValidationShapeAssistantStreamMessage || len(c.Attempts) != 3 {
			t.Fatalf("assistant case = %+v", c)
		}
	}
	if streamCases != len(slackMarkdownRendererValidationCases()) {
		t.Fatalf("assistant cases = %d", streamCases)
	}
	wantRequests := len(slackMarkdownRendererValidationCases()) + 1 + len(slackMarkdownRendererValidationCases())*3
	if len(paths) != wantRequests {
		t.Fatalf("requests = %d, want %d: %v", len(paths), wantRequests, paths)
	}
}

func TestRunSlackMarkdownRendererValidationFailsOnPartialAssistantConfig(t *testing.T) {
	t.Parallel()
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts++
		_, _ = w.Write([]byte(`{"ok":true,"ts":"` + testSlackValidationTS + `"}`))
	}))
	t.Cleanup(srv.Close)

	report, err := runSlackMarkdownRendererValidation(context.Background(), &slackMarkdownValidationConfig{
		token:                 testSlackValidationToken,
		channelID:             testSlackValidationChannel,
		teamID:                testSlackValidationTeam,
		ackPersistentMessages: true,
		assistantChannelID:    testSlackAssistantChannel,
		postMessageURL:        srv.URL,
		startStreamURL:        srv.URL,
		appendStreamURL:       srv.URL,
		stopStreamURL:         srv.URL,
		userAgent:             testSlackValidationUA,
		now:                   func() time.Time { return time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC) },
		httpClient:            srv.Client(),
	})
	if err == nil || !strings.Contains(err.Error(), "partial assistant-pane config") {
		t.Fatalf("partial assistant config error = %v", err)
	}
	if posts != 0 {
		t.Fatalf("posts = %d, want fail-fast before Slack calls", posts)
	}
	if report.Status != slackMarkdownValidationStatusConfigFailed ||
		report.GeneratedAt != "2026-06-13T12:00:00Z" ||
		!strings.Contains(report.Error, "assistant-thread-ts") ||
		report.RendererVerdict != "" ||
		report.ReviewInstructions != "" ||
		len(report.Surfaces) != 0 ||
		len(report.Cases) != 0 {
		t.Fatalf("partial assistant report = %+v", report)
	}
}

func TestRunSlackMarkdownRendererValidationFailsFastOnDirectConfigErrors(t *testing.T) {
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts++
		_, _ = w.Write([]byte(`{"ok":true,"ts":"` + testSlackValidationTS + `"}`))
	}))
	t.Cleanup(srv.Close)

	tests := []struct {
		name    string
		mutate  func(*slackMarkdownValidationConfig)
		wantErr string
	}{
		{
			name: "missing token",
			mutate: func(cfg *slackMarkdownValidationConfig) {
				cfg.token = ""
			},
			wantErr: envSlackMarkdownValidationToken,
		},
		{
			name: "malformed token",
			mutate: func(cfg *slackMarkdownValidationConfig) {
				cfg.token = "not-a-token"
			},
			wantErr: "slack bot token",
		},
		{
			name: "missing channel",
			mutate: func(cfg *slackMarkdownValidationConfig) {
				cfg.channelID = ""
			},
			wantErr: envSlackMarkdownValidationChannel,
		},
		{
			name: "missing persistent-message ack",
			mutate: func(cfg *slackMarkdownValidationConfig) {
				cfg.ackPersistentMessages = false
			},
			wantErr: envSlackMarkdownValidationPersistentAck,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := slackMarkdownValidationConfig{
				token:                 testSlackValidationToken,
				channelID:             testSlackValidationChannel,
				ackPersistentMessages: true,
				postMessageURL:        srv.URL,
				userAgent:             testSlackValidationUA,
				now:                   func() time.Time { return time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC) },
				httpClient:            srv.Client(),
			}
			tc.mutate(&cfg)
			report, err := runSlackMarkdownRendererValidation(context.Background(), &cfg)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
			if report.Status != slackMarkdownValidationStatusConfigFailed ||
				report.RendererVerdict != "" ||
				report.ReviewInstructions != "" ||
				len(report.Surfaces) != 0 ||
				len(report.Cases) != 0 {
				t.Fatalf("direct config report = %+v", report)
			}
		})
	}
	if posts != 0 {
		t.Fatalf("posts = %d, want direct config errors before Slack calls", posts)
	}
}

func TestRunSlackMarkdownRendererValidationDoesNotMutateInputDefaults(t *testing.T) {
	t.Parallel()
	cfg := slackMarkdownValidationConfig{
		token:                 testSlackValidationToken,
		channelID:             testSlackValidationChannel,
		teamID:                testSlackValidationTeam,
		ackPersistentMessages: true,
		assistantChannelID:    testSlackAssistantChannel,
	}

	if _, err := runSlackMarkdownRendererValidation(context.Background(), &cfg); err == nil || !strings.Contains(err.Error(), "partial assistant-pane config") {
		t.Fatalf("partial assistant config error = %v, want config validation error", err)
	}
	if cfg.now != nil || cfg.httpClient != nil || cfg.timeout != 0 {
		t.Fatalf("input config mutated defaults: now_set=%t httpClient=%v timeout=%s", cfg.now != nil, cfg.httpClient, cfg.timeout)
	}
}

func TestRunSlackMarkdownRendererValidationMarksAssistantSurfaceFailed(t *testing.T) {
	t.Parallel()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/post":
			_, _ = w.Write([]byte(`{"ok":true,"ts":"` + testSlackValidationTS + `"}`))
		case testSlackValidationStartPath:
			_, _ = w.Write([]byte(`{"ok":true,"ts":"1700000000.000300"}`))
		case testSlackValidationAppendPath:
			_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_arguments"}`))
		case testSlackValidationStopPath:
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	report, err := runSlackMarkdownRendererValidation(context.Background(), &slackMarkdownValidationConfig{
		token:                    testSlackValidationToken,
		channelID:                testSlackValidationChannel,
		teamID:                   testSlackValidationTeam,
		ackPersistentMessages:    true,
		postMessageURL:           srv.URL + "/post",
		startStreamURL:           srv.URL + testSlackValidationStartPath,
		appendStreamURL:          srv.URL + testSlackValidationAppendPath,
		stopStreamURL:            srv.URL + testSlackValidationStopPath,
		userAgent:                testSlackValidationUA,
		assistantChannelID:       testSlackAssistantChannel,
		assistantThreadTS:        testSlackAssistantThreadTS,
		assistantRecipientTeamID: testSlackValidationTeam,
		assistantRecipientUserID: testSlackAssistantUser,
		now:                      func() time.Time { return time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC) },
		httpClient:               srv.Client(),
	})
	if err == nil || !strings.Contains(err.Error(), "append stream") {
		t.Fatalf("assistant append error = %v", err)
	}
	if len(report.Surfaces) != 2 ||
		report.Surfaces[0].Status != slackMarkdownValidationStatusDelivered ||
		report.Surfaces[1].Status != slackMarkdownValidationStatusDeliveryFailed ||
		report.Status != slackMarkdownValidationStatusDeliveryFailed {
		t.Fatalf("assistant failure report = %+v", report)
	}
	wantRequests := len(slackMarkdownRendererValidationCases()) + 1 + 3
	if len(paths) != wantRequests {
		t.Fatalf("requests = %d, want channel cases + compatibility + start/append/stop cleanup: %v", len(paths), paths)
	}
}

func TestRunSlackMarkdownRendererValidationFailsWhenFirstPostReturnsNoTimestamp(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	report, err := runSlackMarkdownRendererValidation(context.Background(), &slackMarkdownValidationConfig{
		token:                 testSlackValidationToken,
		channelID:             testSlackValidationChannel,
		ackPersistentMessages: true,
		postMessageURL:        srv.URL,
		userAgent:             testSlackValidationUA,
		now:                   func() time.Time { return time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC) },
		httpClient:            srv.Client(),
	})
	if err == nil || !strings.Contains(err.Error(), "missing Slack ts") {
		t.Fatalf("error = %v, want missing Slack ts", err)
	}
	if report.Status != slackMarkdownValidationStatusDeliveryFailed ||
		len(report.Cases) != 1 ||
		len(report.Surfaces) != 1 ||
		report.Surfaces[0].Status != slackMarkdownValidationStatusDeliveryFailed {
		t.Fatalf("report = %+v", report)
	}
}

func TestRunSlackMarkdownRendererValidationReturnsPartialEvidenceOnFailure(t *testing.T) {
	t.Parallel()
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests == 1 {
			_, _ = w.Write([]byte(`{"ok":true,"ts":"` + testSlackValidationTS + `"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
	}))
	t.Cleanup(srv.Close)

	report, err := runSlackMarkdownRendererValidation(context.Background(), &slackMarkdownValidationConfig{
		token:                 testSlackValidationToken,
		channelID:             testSlackValidationChannel,
		ackPersistentMessages: true,
		postMessageURL:        srv.URL,
		userAgent:             testSlackValidationUA,
		now:                   func() time.Time { return time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC) },
		httpClient:            srv.Client(),
	})
	if err == nil || !strings.Contains(err.Error(), "channel_not_found") {
		t.Fatalf("error = %v, want channel_not_found", err)
	}
	if report.Status != slackMarkdownValidationStatusDeliveryFailed ||
		report.RendererVerdict != slackMarkdownValidationRendererIncomplete ||
		report.ReviewInstructions != slackMarkdownValidationIncompleteInstructions ||
		!strings.Contains(report.Error, "channel_not_found") {
		t.Fatalf("partial report status/verdict/instructions/error = %q/%q/%q/%q", report.Status, report.RendererVerdict, report.ReviewInstructions, report.Error)
	}
	if len(report.Cases) != 2 {
		t.Fatalf("partial cases = %d, want first success plus failing case", len(report.Cases))
	}
	if len(report.Surfaces) != 1 || report.Surfaces[0].Status != slackMarkdownValidationStatusDeliveryFailed {
		t.Fatalf("partial surfaces = %+v", report.Surfaces)
	}
	if report.Cases[0].SlackTS != testSlackValidationTS {
		t.Fatalf("first case evidence missing SlackTS: %+v", report.Cases[0])
	}
	failed := report.Cases[1]
	if failed.ID != slackMarkdownValidationCaseInlineMaskedLink || len(failed.Attempts) != 1 || failed.Attempts[0].ErrorCode != "channel_not_found" {
		t.Fatalf("failing case evidence = %+v", failed)
	}
}

func TestRunSlackMarkdownRendererValidationCLIDoesNotEmitJSONForConfigErrors(t *testing.T) {
	t.Setenv(envSlackMarkdownValidationToken, "")
	t.Setenv(envSlackMarkdownValidationChannel, testSlackValidationChannel)

	var out bytes.Buffer
	if err := runSlackMarkdownRendererValidationCLI(nil, &out); err == nil || !strings.Contains(err.Error(), envSlackMarkdownValidationToken) {
		t.Fatalf("error = %v, want missing token", err)
	}
	if out.Len() != 0 {
		t.Fatalf("config error wrote JSON: %s", out.String())
	}
}

func TestRunSlackMarkdownRendererValidationCLIDoesNotEmitJSONForPartialAssistantConfig(t *testing.T) {
	t.Setenv(envSlackMarkdownValidationToken, testSlackValidationToken)
	t.Setenv(envSlackMarkdownValidationChannel, testSlackValidationChannel)
	t.Setenv(envSlackMarkdownValidationPersistentAck, testSlackValidationAckValue)
	t.Setenv(envSlackMarkdownValidationAssistantChannel, testSlackAssistantChannel)

	var out bytes.Buffer
	if err := runSlackMarkdownRendererValidationCLI(nil, &out); err == nil || !strings.Contains(err.Error(), "partial assistant-pane config") {
		t.Fatalf("error = %v, want partial assistant config", err)
	}
	if out.Len() != 0 {
		t.Fatalf("partial assistant config wrote JSON: %s", out.String())
	}
}

func TestRunSlackMarkdownRendererValidationCLIEmitsPartialReportOnFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
	}))
	t.Cleanup(srv.Close)

	cfg := slackMarkdownValidationConfig{
		token:                 testSlackValidationToken,
		channelID:             testSlackValidationChannel,
		ackPersistentMessages: true,
		postMessageURL:        srv.URL,
		userAgent:             testSlackValidationUA,
		now:                   func() time.Time { return time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC) },
		httpClient:            srv.Client(),
	}
	var out bytes.Buffer
	report, err := runSlackMarkdownRendererValidation(context.Background(), &cfg)
	if err == nil {
		t.Fatal("run validation unexpectedly succeeded")
	}
	if writeErr := writeSlackMarkdownValidationReport(&out, &report, err); writeErr == nil || !strings.Contains(writeErr.Error(), "channel_not_found") {
		t.Fatalf("write partial report error = %v, want original validation error", writeErr)
	}
	var decoded slackMarkdownValidationReport
	if decodeErr := json.Unmarshal(out.Bytes(), &decoded); decodeErr != nil {
		t.Fatalf("decode partial report: %v", decodeErr)
	}
	if decoded.Status != slackMarkdownValidationStatusDeliveryFailed ||
		decoded.RendererVerdict != slackMarkdownValidationRendererIncomplete ||
		decoded.ReviewInstructions != slackMarkdownValidationIncompleteInstructions ||
		!strings.Contains(decoded.Error, "channel_not_found") ||
		len(decoded.Cases) != 1 {
		t.Fatalf("decoded partial report = %+v", decoded)
	}
}

func TestWriteSlackMarkdownValidationReportEncodeErrorPreservesDeliveryError(t *testing.T) {
	t.Parallel()
	validationErr := errors.New("channel_not_found")
	writeErr := writeSlackMarkdownValidationReport(failingSlackMarkdownValidationWriter{}, &slackMarkdownValidationReport{
		Status: slackMarkdownValidationStatusDeliveryFailed,
		Error:  validationErr.Error(),
	}, validationErr)
	if writeErr == nil ||
		!strings.Contains(writeErr.Error(), validationErr.Error()) ||
		!strings.Contains(writeErr.Error(), "encode partial report") ||
		!strings.Contains(writeErr.Error(), "writer closed") {
		t.Fatalf("write error = %v, want delivery and encode errors", writeErr)
	}
}

func TestAssistantValidationSkipReasonNamesMissingPartialConfig(t *testing.T) {
	t.Parallel()
	cfg := &slackMarkdownValidationConfig{
		assistantChannelID:       testSlackAssistantChannel,
		assistantThreadTS:        testSlackAssistantThreadTS,
		assistantRecipientTeamID: testSlackValidationTeam,
	}
	got := assistantValidationSkipReason(cfg)
	if !strings.Contains(got, "partial assistant-pane config") || !strings.Contains(got, "assistant-recipient-user-id") {
		t.Fatalf("skip reason = %q, want partial config with missing recipient user", got)
	}
	if strings.Contains(got, "assistant-channel") || strings.Contains(got, "assistant-thread-ts") || strings.Contains(got, "assistant-recipient-team-id") {
		t.Fatalf("skip reason named fields that were present: %q", got)
	}
}

func TestPostSlackMarkdownValidationCaseRecordsMarkdownTextRetry(t *testing.T) {
	t.Parallel()
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if requests == 1 {
			if _, ok := body["blocks"]; !ok {
				t.Fatalf("first request = %+v, want blocks", body)
			}
			_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_blocks"}`))
			return
		}
		if _, ok := body["markdown_text"]; !ok {
			t.Fatalf("retry request = %+v, want markdown_text", body)
		}
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1700000000.000200"}`))
	}))
	t.Cleanup(srv.Close)

	cfg := &slackMarkdownValidationConfig{
		token:          testSlackValidationToken,
		channelID:      testSlackValidationChannel,
		postMessageURL: srv.URL,
		userAgent:      testSlackValidationUA,
		httpClient:     srv.Client(),
	}
	result, err := postSlackMarkdownValidationCase(context.Background(), cfg, newSlackMarkdownValidationPostMessagePoster(cfg), slackMarkdownValidationCase{
		id:            slackMarkdownValidationCaseInlineMaskedLink,
		input:         testSlackValidationMaskedLink,
		operatorCheck: testSlackValidationCheck,
	}, "")
	if err != nil {
		t.Fatalf("post validation case: %v", err)
	}
	if result.SlackTS != "1700000000.000200" {
		t.Fatalf("SlackTS = %q", result.SlackTS)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("attempts = %+v, want block attempt plus retry", result.Attempts)
	}
	if result.Attempts[0].ErrorCode != slackAPIInvalidBlocks || result.Attempts[1].RequestShape != slackMarkdownValidationShapeMarkdownText {
		t.Fatalf("attempts = %+v", result.Attempts)
	}
	if result.RequestShape != slackMarkdownValidationShapePostMarkdownText {
		t.Fatalf("request shape = %q, want successful markdown_text retry", result.RequestShape)
	}
	if result.DeliveredMarkdown != "Use billing (https://example.com/login)." {
		t.Fatalf("delivered markdown = %q", result.DeliveredMarkdown)
	}
	if result.FallbackText != "" {
		t.Fatalf("fallback text = %q, want empty after markdown_text retry", result.FallbackText)
	}
}

func TestSendSlackValidationPayloadRecordsUnstructuredAttemptError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not-json`))
	}))
	t.Cleanup(srv.Close)

	cfg := &slackMarkdownValidationConfig{
		token:          testSlackValidationToken,
		teamID:         testSlackValidationTeam,
		postMessageURL: srv.URL,
		userAgent:      testSlackValidationUA,
		httpClient:     srv.Client(),
	}
	attempt, err := sendSlackValidationPayload(context.Background(), cfg, newSlackMarkdownValidationPostMessagePoster(cfg), slackMarkdownValidationShapeMarkdownText, []byte(`{"channel":"C123","markdown_text":"hello"}`))
	if err == nil {
		t.Fatal("send unexpectedly succeeded")
	}
	if attempt.ErrorCode != "" || attempt.Error == "" {
		t.Fatalf("attempt error evidence = %+v", attempt)
	}
}

func TestSlackValidationErrorCodeIncludesNonChatWebAPIErrors(t *testing.T) {
	t.Parallel()
	err := &slackWebAPIError{op: "chat.startStream", code: slackAPIInvalidArguments}
	if got := slackValidationErrorCode(err); got != slackAPIInvalidArguments {
		t.Fatalf("error code = %q, want %s", got, slackAPIInvalidArguments)
	}
}

func TestRecordSlackValidationAttemptErrorKeepsMessageWithCode(t *testing.T) {
	t.Parallel()

	var attempt slackMarkdownValidationAttempt
	err := &slackWebAPIError{op: "chat.startStream", code: slackAPIInvalidArguments}

	recordSlackValidationAttemptError(&attempt, err)

	if attempt.ErrorCode != slackAPIInvalidArguments || !strings.Contains(attempt.Error, slackAPIInvalidArguments) {
		t.Fatalf("attempt error evidence = %+v", attempt)
	}
}

func TestStreamSlackMarkdownValidationCaseUsesAssistantStreamShape(t *testing.T) {
	t.Parallel()
	var got []struct {
		path string
		body map[string]any
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		got = append(got, struct {
			path string
			body map[string]any
		}{path: r.URL.Path, body: body})
		switch r.URL.Path {
		case testSlackValidationStartPath:
			_, _ = w.Write([]byte(`{"ok":true,"ts":"1700000000.000300"}`))
		case testSlackValidationAppendPath, testSlackValidationStopPath:
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	result, err := streamSlackMarkdownValidationCase(context.Background(), &slackMarkdownValidationConfig{
		token:                    testSlackValidationToken,
		teamID:                   testSlackValidationTeam,
		enterpriseID:             testSlackValidationEnterprise,
		assistantChannelID:       testSlackAssistantChannel,
		assistantThreadTS:        testSlackAssistantThreadTS,
		assistantRecipientTeamID: testSlackValidationTeam,
		assistantRecipientUserID: testSlackAssistantUser,
		startStreamURL:           srv.URL + testSlackValidationStartPath,
		appendStreamURL:          srv.URL + testSlackValidationAppendPath,
		stopStreamURL:            srv.URL + testSlackValidationStopPath,
		userAgent:                testSlackValidationUA,
		httpClient:               srv.Client(),
	}, slackMarkdownValidationCase{
		id:            slackMarkdownValidationCaseInlineMaskedLink,
		input:         testSlackValidationMaskedLink,
		operatorCheck: testSlackValidationCheck,
	})
	if err != nil {
		t.Fatalf("stream validation case: %v", err)
	}
	if result.SlackTS != "1700000000.000300" || len(result.Attempts) != 3 {
		t.Fatalf("result = %+v", result)
	}
	if len(got) != 3 {
		t.Fatalf("requests = %+v", got)
	}
	if got[0].body["recipient_team_id"] != testSlackValidationTeam || got[0].body["recipient_user_id"] != testSlackAssistantUser {
		t.Fatalf("start body = %+v", got[0].body)
	}
	if got[1].body["markdown_text"] != "Use billing (https://example.com/login)." {
		t.Fatalf("append body = %+v", got[1].body)
	}
	if got[2].body["ts"] != "1700000000.000300" {
		t.Fatalf("stop body = %+v", got[2].body)
	}
}

func TestStreamSlackMarkdownValidationCaseFailsWhenStartReturnsNoTimestamp(t *testing.T) {
	t.Parallel()
	port := &emptyStreamTSValidationPort{}

	result, err := streamSlackMarkdownValidationCaseWithPort(context.Background(), &slackMarkdownValidationConfig{
		teamID:                   testSlackValidationTeam,
		assistantChannelID:       testSlackAssistantChannel,
		assistantThreadTS:        testSlackAssistantThreadTS,
		assistantRecipientTeamID: testSlackValidationTeam,
		assistantRecipientUserID: testSlackAssistantUser,
	}, port, slackMarkdownValidationCase{
		id:            slackMarkdownValidationCaseInlineMaskedLink,
		input:         testSlackValidationMaskedLink,
		operatorCheck: testSlackValidationCheck,
	})

	if err == nil || !strings.Contains(err.Error(), "missing Slack ts") {
		t.Fatalf("stream validation error = %v, want missing Slack ts", err)
	}
	if port.starts != 1 || port.appends != 0 || port.stops != 0 || len(result.Attempts) != 1 || !result.Attempts[0].OK {
		t.Fatalf("result = %+v port=%+v, want only successful start attempt", result, port)
	}
}

type emptyStreamTSValidationPort struct {
	starts  int
	appends int
	stops   int
}

func (p *emptyStreamTSValidationPort) StartStream(context.Context, *internal.AgentStreamStart) (string, error) {
	p.starts++
	return "", nil
}

func (p *emptyStreamTSValidationPort) AppendStream(context.Context, string, string, string, string, string) error {
	p.appends++
	return nil
}

func (p *emptyStreamTSValidationPort) StopStream(context.Context, string, string, string, string) error {
	p.stops++
	return nil
}

func TestStreamSlackMarkdownValidationCaseRecordsAppendFailure(t *testing.T) {
	t.Parallel()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case testSlackValidationStartPath:
			_, _ = w.Write([]byte(`{"ok":true,"ts":"1700000000.000300"}`))
		case testSlackValidationAppendPath:
			_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_arguments"}`))
		case testSlackValidationStopPath:
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	result, err := streamSlackMarkdownValidationCase(context.Background(), &slackMarkdownValidationConfig{
		token:                    testSlackValidationToken,
		teamID:                   testSlackValidationTeam,
		assistantChannelID:       testSlackAssistantChannel,
		assistantThreadTS:        testSlackAssistantThreadTS,
		assistantRecipientTeamID: testSlackValidationTeam,
		assistantRecipientUserID: testSlackAssistantUser,
		startStreamURL:           srv.URL + testSlackValidationStartPath,
		appendStreamURL:          srv.URL + testSlackValidationAppendPath,
		stopStreamURL:            srv.URL + testSlackValidationStopPath,
		userAgent:                testSlackValidationUA,
		httpClient:               srv.Client(),
	}, slackMarkdownValidationCase{
		id:            slackMarkdownValidationCaseInlineMaskedLink,
		input:         testSlackValidationMaskedLink,
		operatorCheck: testSlackValidationCheck,
	})
	if err == nil || !strings.Contains(err.Error(), "append stream") {
		t.Fatalf("append failure error = %v", err)
	}
	if len(paths) != 3 {
		t.Fatalf("paths = %v, want start, append, and stop cleanup", paths)
	}
	if len(result.Attempts) != 3 ||
		!result.Attempts[0].OK ||
		result.Attempts[1].OK ||
		result.Attempts[1].ErrorCode != slackAPIInvalidArguments ||
		!result.Attempts[2].OK {
		t.Fatalf("attempts = %+v", result.Attempts)
	}
}

type failingSlackMarkdownValidationWriter struct{}

func (failingSlackMarkdownValidationWriter) Write([]byte) (int, error) {
	return 0, errors.New("writer closed")
}

func streamSlackMarkdownValidationCase(ctx context.Context, cfg *slackMarkdownValidationConfig, tc slackMarkdownValidationCase) (slackMarkdownValidationCaseResult, error) {
	return streamSlackMarkdownValidationCaseWithPort(ctx, cfg, newSlackMarkdownValidationStreamPort(cfg), tc)
}
