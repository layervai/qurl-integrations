package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const (
	testTokenEnv    = "SLACK_BOT_TOKEN_TEST"
	testToken       = "xoxb-test"
	flagTokenEnv    = "-token-env"
	flagBaseURL     = "-base-url"
	flagTimeout     = "-timeout"
	flagReqTimeout  = "-request-timeout"
	flagMaxPages    = "-max-pages"
	testTimeoutSpan = "10s"
)

func testEnv(token string) func(string) string {
	return func(name string) string {
		if name == testTokenEnv {
			return token
		}
		return ""
	}
}

func fixedNow() time.Time { return time.Unix(1723600000, 0).UTC() }

// runCLI drives run end to end and returns the exit code plus both streams.
func runCLI(t *testing.T, args []string, getenv func(string) string) (code int, stdoutText, stderrText string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code = run(context.Background(), &stdout, &stderr, args, getenv, fixedNow)
	return code, stdout.String(), stderr.String()
}

func TestRunSucceedsWhenTheContractHolds(t *testing.T) {
	t.Parallel()

	srv, _ := newFakeSlack(t, map[string]string{
		methodConversationsList: listBody(testChannel),
		surfaceHistory:          messagesBody(textMessage("100.1"), uploadMessage("100.2")),
	})

	code, stdout, stderr := runCLI(t, []string{
		flagTokenEnv, testTokenEnv, flagBaseURL, srv.URL, flagMaxPages, "1",
	}, testEnv(testToken))
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	var result scanResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not the JSON report: %v (%q)", err, stdout)
	}
	if !result.Contract.Holds || result.Contract.DistinctUploads != 1 {
		t.Errorf("contract = %+v", result.Contract)
	}
	if result.StartedAt != fixedNow().Format(time.RFC3339) {
		t.Errorf("started_at = %q, want the injected clock", result.StartedAt)
	}
}

// TestRunEmitsTheReportWhenTheContractBreaks pins that a failing scan still writes its
// evidence to stdout. The counts ARE the diagnosis — an operator who gets exit 1 and an
// empty report learns only that something went wrong, which is what this command is
// meant to stop being the answer.
func TestRunEmitsTheReportWhenTheContractBreaks(t *testing.T) {
	t.Parallel()

	srv, _ := newFakeSlack(t, map[string]string{
		methodConversationsList: listBody(testChannel),
		surfaceHistory:          messagesBody(textMessage("100.1")),
	})

	code, stdout, stderr := runCLI(t, []string{
		flagTokenEnv, testTokenEnv, flagBaseURL, srv.URL, flagMaxPages, "1",
	}, testEnv(testToken))
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	var result scanResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not the JSON report: %v (%q)", err, stdout)
	}
	if result.Contract.Holds || len(result.Contract.Failures) == 0 {
		t.Errorf("contract = %+v, want the failure recorded in the report", result.Contract)
	}
	if result.History.Messages != 1 {
		t.Errorf("history = %+v, want the messages it did read", result.History)
	}
	if !strings.Contains(stderr, "upload-detection contract does not hold") {
		t.Errorf("stderr = %q, want the reason on stderr too", stderr)
	}
}

// TestRunRejectsBadInvocationBeforeCallingSlack pins that every invocation error costs
// zero Slack calls: the base URL is a loopback the fake never serves, so a call would
// surface as a connection error rather than exit 2.
func TestRunRejectsBadInvocationBeforeCallingSlack(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		env  string
	}{
		{"no token in the environment", nil, ""},
		{"blank token env name", []string{flagTokenEnv, "  "}, testToken},
		{"token env name with a newline", []string{flagTokenEnv, "SLACK\nTOKEN"}, testToken},
		{"token env name with a hyphen", []string{flagTokenEnv, "SLACK-TOKEN"}, testToken},
		{"token env name starting with a digit", []string{flagTokenEnv, "1TOKEN"}, testToken},
		{"token with control characters", nil, "xoxb-\ntest"},
		{"user agent with control characters", []string{"-user-agent", "smoke\r\nX: y"}, testToken},
		{"non-https base url", []string{flagBaseURL, "http://slack.example.com"}, testToken},
		{"base url with query", []string{flagBaseURL, "https://slack.example.com?a=b"}, testToken},
		{"base url with userinfo", []string{flagBaseURL, "https://user:pw@slack.example.com"}, testToken},
		{"unparsable channel id", []string{"-channels", "not-a-channel"}, testToken},
		{"expect-upload without a timestamp", []string{"-expect-upload", testChannel}, testToken},
		{"expect-upload with a non-numeric timestamp", []string{"-expect-upload", testChannel + ":yesterday"}, testToken},
		{"zero timeout", []string{flagTimeout, "0"}, testToken},
		{"zero request timeout", []string{flagReqTimeout, "0"}, testToken},
		{"request timeout at the overall budget", []string{flagTimeout, testTimeoutSpan, flagReqTimeout, testTimeoutSpan}, testToken},
		{"request timeout too close to the budget", []string{flagTimeout, testTimeoutSpan, flagReqTimeout, "5s"}, testToken},
		{"zero conversations", []string{"-max-conversations", "0"}, testToken},
		{"zero pages", []string{flagMaxPages, "0"}, testToken},
		{"page limit past Slack's ceiling", []string{"-page-limit", "1001"}, testToken},
		{"negative threads", []string{"-max-threads", "-1"}, testToken},
		{"negative min uploads", []string{"-min-uploads", "-1"}, testToken},
		{"unknown flag", []string{"-nope"}, testToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			args := append([]string{flagTokenEnv, testTokenEnv, flagBaseURL, "https://127.0.0.1:1"}, tt.args...)
			code, stdout, stderr := runCLI(t, args, testEnv(tt.env))
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (stdout %q, stderr %q)", code, stdout, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want nothing written for an invocation error", stdout)
			}
			if strings.TrimSpace(stderr) == "" {
				t.Error("an invocation error must say what was wrong")
			}
		})
	}
}

func TestRunHelpExitsZero(t *testing.T) {
	t.Parallel()

	code, stdout, _ := runCLI(t, []string{"-h"}, testEnv(testToken))
	if code != 0 {
		t.Errorf("exit = %d, want 0 for -h", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want the usage on stderr only", stdout)
	}
}

// TestRunNamesTheTokenEnvVarItLookedAt pins the one error message an operator hits
// first. "missing Slack bot token" without the variable name is a guessing game when
// -token-env has been pointed somewhere custom.
func TestRunNamesTheTokenEnvVarItLookedAt(t *testing.T) {
	t.Parallel()

	_, _, stderr := runCLI(t, []string{flagTokenEnv, testTokenEnv}, testEnv(""))
	if !strings.Contains(stderr, testTokenEnv) {
		t.Errorf("stderr = %q, want the environment variable named", stderr)
	}
}

// TestIsEnvVarName pins the charset that keeps writeConfigValidationError's echo of
// this flag safe to print.
func TestIsEnvVarName(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"SLACK_BOT_TOKEN", "_x", "A1", "lower_case_9"} {
		if !isEnvVarName(name) {
			t.Errorf("isEnvVarName(%q) = false", name)
		}
	}
	for _, name := range []string{"", "1TOKEN", "SLACK-TOKEN", "SLACK TOKEN", "SLACK\nTOKEN", "SLACK$TOKEN", "SLACK\x7fTOKEN"} {
		if isEnvVarName(name) {
			t.Errorf("isEnvVarName(%q) = true", name)
		}
	}
}

func TestMessageRefListSet(t *testing.T) {
	t.Parallel()

	var list messageRefList
	for _, raw := range []string{testChannel + ":1723600000.000100", " C0000000002 : 100.2 "} {
		if err := list.Set(raw); err != nil {
			t.Fatalf("Set(%q): %v", raw, err)
		}
	}
	if len(list) != 2 || list[0].Channel != testChannel || list[1].TS != "100.2" {
		t.Fatalf("list = %+v", list)
	}
	if got := list.String(); got != testChannel+":1723600000.000100,C0000000002:100.2" {
		t.Errorf("String() = %q", got)
	}
	for _, raw := range []string{"", testChannel, ":100.1", testChannel + ":", "c0000000001:100.1", testChannel + ":100.1a"} {
		var rejected messageRefList
		if err := rejected.Set(raw); err == nil {
			t.Errorf("Set(%q) must be rejected", raw)
		}
	}
}

func TestNormalizeSlackBaseURL(t *testing.T) {
	t.Parallel()

	accepted := map[string]string{
		"":                              defaultSlackAPIBaseURL,
		"  ":                            defaultSlackAPIBaseURL,
		"https://slack.example.com/api": "https://slack.example.com/api",
		"https://slack.example.com/":    "https://slack.example.com",
		"http://localhost:8080":         "http://localhost:8080",
		"http://127.0.0.1:8080/":        "http://127.0.0.1:8080",
		"http://[::1]:8080":             "http://[::1]:8080",
	}
	for raw, want := range accepted {
		got, err := normalizeSlackBaseURL(raw)
		if err != nil {
			t.Errorf("normalizeSlackBaseURL(%q): %v", raw, err)
			continue
		}
		if got != want {
			t.Errorf("normalizeSlackBaseURL(%q) = %q, want %q", raw, got, want)
		}
	}
	for _, raw := range []string{"http://slack.example.com", "slack.example.com", "https://x?a=b", "https://x#f", "https://u:p@x", "://"} {
		if _, err := normalizeSlackBaseURL(raw); err == nil {
			t.Errorf("normalizeSlackBaseURL(%q) must be rejected", raw)
		}
	}
}

func TestPrepareScanConfigFillsDefaults(t *testing.T) {
	t.Parallel()

	cfg := &scanConfig{Token: " xoxb-test ", UserAgent: "  "}
	if err := prepareScanConfig(cfg); err != nil {
		t.Fatalf("prepareScanConfig: %v", err)
	}
	if cfg.Token != testToken {
		t.Errorf("token = %q, want it trimmed", cfg.Token)
	}
	if cfg.UserAgent != defaultUserAgent || cfg.ConversationTypes != defaultConversationTypes {
		t.Errorf("cfg = %+v, want the blank fields defaulted", cfg)
	}
	if cfg.BaseURL != defaultSlackAPIBaseURL {
		t.Errorf("base url = %q, want the Slack default", cfg.BaseURL)
	}
	if cfg.HTTPClient == nil || cfg.StartedAt.IsZero() {
		t.Error("prepareScanConfig must supply a client and a start time for direct callers")
	}
}

func TestValidateConversationID(t *testing.T) {
	t.Parallel()

	for _, id := range []string{"C0000000001", "D01AB2CD3", "G9"} {
		if err := validateConversationID(id); err != nil {
			t.Errorf("validateConversationID(%q): %v", id, err)
		}
	}
	for _, id := range []string{"", "  ", "c0000000001", "C000-0001", "C000 0001", "C0000000001\n"} {
		if err := validateConversationID(id); err == nil {
			t.Errorf("validateConversationID(%q) must be rejected", id)
		}
	}
}

func TestSplitConversationIDs(t *testing.T) {
	t.Parallel()

	got := splitConversationIDs(" C1, C2 ,,C3 C4 ")
	want := []string{"C1", "C2", "C3", "C4"}
	if len(got) != len(want) {
		t.Fatalf("splitConversationIDs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitConversationIDs = %v, want %v", got, want)
		}
	}
	if len(splitConversationIDs("  ")) != 0 {
		t.Error("a blank -channels must discover instead of scanning nothing")
	}
}

func TestCleanOperatorNote(t *testing.T) {
	t.Parallel()

	if got := cleanOperatorNote("  Enterprise\tGrid\norg install  "); got != "EnterpriseGridorg install" {
		t.Errorf("cleanOperatorNote = %q", got)
	}
}

func TestContainsHTTPHeaderControl(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"xoxb-\ntest", "xoxb-\rtest", "xoxb-\x00test", "xoxb-\x7ftest"} {
		if !containsHTTPHeaderControl(s) {
			t.Errorf("containsHTTPHeaderControl(%q) = false", s)
		}
	}
	for _, s := range []string{testToken, "qurl-slack-history-upload-smoke", "note with spaces"} {
		if containsHTTPHeaderControl(s) {
			t.Errorf("containsHTTPHeaderControl(%q) = true", s)
		}
	}
}

// TestRunCarriesOperatorNotesIntoTheReport pins the provenance fields. A scan whose
// numbers disagree with the recorded measurement is only actionable if the report says
// which workspace shape and scope set produced it.
func TestRunCarriesOperatorNotesIntoTheReport(t *testing.T) {
	t.Parallel()

	srv, _ := newFakeSlack(t, map[string]string{
		methodConversationsList: listBody(testChannel),
		surfaceHistory:          messagesBody(uploadMessage("100.1")),
	})
	code, stdout, stderr := runCLI(t, []string{
		flagTokenEnv, testTokenEnv, flagBaseURL, srv.URL, flagMaxPages, "1",
		"-workspace-shape", "Enterprise Grid org install",
		"-token-owner", "enterprise",
		"-scopes", "channels:history,groups:history,im:history",
	}, testEnv(testToken))
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	var result scanResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not the JSON report: %v", err)
	}
	if result.WorkspaceShape != "Enterprise Grid org install" || result.TokenOwner != "enterprise" {
		t.Errorf("result = %+v, want the operator notes carried through", result)
	}
	if !strings.Contains(result.Scopes, "channels:history") {
		t.Errorf("scopes = %q", result.Scopes)
	}
}

// TestRunRejectsAChannelListLongerThanTheCap pins that a deliberate -channels list is
// refused rather than silently truncated. -channels is what an operator names when they
// know which conversations carry uploads, so measuring a subset would answer a different
// question than the one asked — and the report would not say so.
func TestRunRejectsAChannelListLongerThanTheCap(t *testing.T) {
	t.Parallel()

	code, stdout, stderr := runCLI(t, []string{
		flagTokenEnv, testTokenEnv, flagBaseURL, "https://127.0.0.1:1",
		"-channels", "C0000000001,C0000000002,C0000000003", "-max-conversations", "2",
	}, testEnv(testToken))
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (stdout %q)", code, stdout)
	}
	if !strings.Contains(stderr, "names 3 conversations but -max-conversations is 2") {
		t.Errorf("stderr = %q, want both numbers named", stderr)
	}
}

// TestRunBoundsMaxThreads pins the ceiling that keeps -max-threads from sizing a huge
// per-page allocation. -page-limit has always had one; this had only a negative check.
func TestRunBoundsMaxThreads(t *testing.T) {
	t.Parallel()

	code, _, stderr := runCLI(t, []string{
		flagTokenEnv, testTokenEnv, flagBaseURL, "https://127.0.0.1:1",
		"-max-threads", "100000000",
	}, testEnv(testToken))
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "-max-threads must be at most") {
		t.Errorf("stderr = %q, want the ceiling named", stderr)
	}
}

// TestThreadParentsCapacityTracksThePage pins the allocation bound directly: sizing the
// slice off -max-threads alone turns a large flag value into a huge allocation for every
// history page, before a single reply is read.
func TestThreadParentsCapacityTracksThePage(t *testing.T) {
	t.Parallel()

	page := []json.RawMessage{json.RawMessage(`{"ts":"100.1","thread_ts":"100.1","reply_count":1}`)}
	got := threadParents(page, maxThreadsCeiling)
	if len(got) != 1 {
		t.Fatalf("threadParents = %v, want the one root", got)
	}
	if cap(got) > len(page) {
		t.Errorf("capacity = %d for a %d-message page; the limit must not size the allocation", cap(got), len(page))
	}
}
