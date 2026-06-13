package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/slack/internal"
	"github.com/layervai/qurl-integrations/shared/auth"
)

const testSlackTeamID = "T_team"
const testEnterpriseSlackBearer = "Bearer xoxb-enterprise-token"

func staticTokenLookup(token string) slackBotTokenLookup {
	return func(context.Context, string) (string, error) { return token, nil }
}

func TestSlackPostMessageFuncPostsThreadedPayload(t *testing.T) {
	t.Parallel()
	var gotAuth, gotUA, gotContentType string
	var gotBody struct {
		Channel  string `json:"channel"`
		ThreadTS string `json:"thread_ts"`
		Text     string `json:"text"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	post := newSlackPostMessageFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "qurl-slack/test", srv.URL, nil)
	if err := post(context.Background(), "T_test", "E_test", "C_chan", "1700000000.000100", "hello"); err != nil {
		t.Fatalf("chat.postMessage: %v", err)
	}
	if gotAuth != "Bearer xoxb-test" {
		t.Fatalf("Authorization = %q, want Bearer token", gotAuth)
	}
	if gotUA != "qurl-slack/test" {
		t.Fatalf("User-Agent = %q", gotUA)
	}
	if gotContentType != "application/json" {
		t.Fatalf("Content-Type = %q", gotContentType)
	}
	if gotBody.Channel != "C_chan" || gotBody.Text != "hello" || gotBody.ThreadTS != "1700000000.000100" {
		t.Fatalf("body = %+v, want channel/text/thread_ts populated", gotBody)
	}
}

func TestSlackPostMessageFuncOmitsEmptyThreadTS(t *testing.T) {
	t.Parallel()
	var rawBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rawBody = string(raw)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	post := newSlackPostMessageFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "", srv.URL, nil)
	if err := post(context.Background(), "T_test", "", "C_chan", "", "top-level reply"); err != nil {
		t.Fatalf("chat.postMessage: %v", err)
	}
	if strings.Contains(rawBody, "thread_ts") {
		t.Fatalf("body %q should omit thread_ts when empty", rawBody)
	}
}

func TestSlackPostMessageFuncUsesWorkspaceTokenLookup(t *testing.T) {
	t.Parallel()
	var gotTeam, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	post := newSlackPostMessageFuncWithTokenLookup(func(_ context.Context, teamID string) (string, error) {
		gotTeam = teamID
		return "xoxb-workspace-token", nil
	}, "qurl-slack/test", srv.URL, nil)
	if err := post(context.Background(), "T_lookup", "", "C_chan", "", "hi"); err != nil {
		t.Fatalf("chat.postMessage: %v", err)
	}
	if gotTeam != "T_lookup" {
		t.Fatalf("lookup teamID = %q, want T_lookup", gotTeam)
	}
	if gotAuth != "Bearer xoxb-workspace-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
}

func TestSlackPostDMFuncOpensIMThenPostsWithGridFallback(t *testing.T) {
	t.Parallel()
	var openAuth, postAuth string
	var gotOpenBody struct {
		Users string `json:"users"`
	}
	var gotPostBody struct {
		Channel string `json:"channel"`
		Text    string `json:"text"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/conversations.open":
			openAuth = r.Header.Get("Authorization")
			if err := json.NewDecoder(r.Body).Decode(&gotOpenBody); err != nil {
				t.Fatalf("decode open body: %v", err)
			}
			_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"D_admin"}}`))
		case "/chat.postMessage":
			postAuth = r.Header.Get("Authorization")
			if err := json.NewDecoder(r.Body).Decode(&gotPostBody); err != nil {
				t.Fatalf("decode post body: %v", err)
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	var lookups []string
	postDM := newSlackPostDMFuncWithTokenLookup(func(_ context.Context, ownerID string) (string, error) {
		lookups = append(lookups, ownerID)
		if ownerID == testSlackTeamID {
			return "", auth.ErrSlackBotTokenNotConfigured
		}
		return "xoxb-enterprise-token", nil
	}, "", srv.URL+"/conversations.open", srv.URL+"/chat.postMessage", nil)
	if err := postDM(context.Background(), testSlackTeamID, "E_org", "U_admin", "secret text"); err != nil {
		t.Fatalf("PostDM: %v", err)
	}
	wantLookups := []string{testSlackTeamID, "E_org", testSlackTeamID, "E_org"}
	if strings.Join(lookups, ",") != strings.Join(wantLookups, ",") {
		t.Fatalf("lookups = %v, want %v", lookups, wantLookups)
	}
	if openAuth != testEnterpriseSlackBearer || postAuth != testEnterpriseSlackBearer {
		t.Fatalf("Authorization open/post = %q/%q, want enterprise token", openAuth, postAuth)
	}
	if gotOpenBody.Users != "U_admin" {
		t.Fatalf("open body = %+v, want Slack user", gotOpenBody)
	}
	if gotPostBody.Channel != "D_admin" || gotPostBody.Text != "secret text" {
		t.Fatalf("post body = %+v, want opened DM channel/text", gotPostBody)
	}
}

func TestSlackPostDMFuncMissingOpenScopeWrapsSentinel(t *testing.T) {
	t.Parallel()
	var postCalled bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/conversations.open":
			_, _ = w.Write([]byte(`{"ok":false,"error":"missing_scope"}`))
		case "/chat.postMessage":
			postCalled = true
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	postDM := newSlackPostDMFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "", srv.URL+"/conversations.open", srv.URL+"/chat.postMessage", nil)
	err := postDM(context.Background(), testSlackTeamID, "", "U_admin", "secret text")
	if !errors.Is(err, internal.ErrSlackMissingScope) {
		t.Fatalf("PostDM error = %v, want ErrSlackMissingScope", err)
	}
	if postCalled {
		t.Fatal("chat.postMessage should not run after conversations.open missing_scope")
	}
}

func TestSlackPostMessageFuncGridFallback(t *testing.T) {
	t.Parallel()
	t.Run("retries with enterprise token when workspace token missing", func(t *testing.T) {
		t.Parallel()
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		t.Cleanup(srv.Close)

		var lookups []string
		post := newSlackPostMessageFuncWithTokenLookup(func(_ context.Context, ownerID string) (string, error) {
			lookups = append(lookups, ownerID)
			if ownerID == testSlackTeamID {
				return "", auth.ErrSlackBotTokenNotConfigured
			}
			return "xoxb-enterprise-token", nil
		}, "", srv.URL, nil)
		if err := post(context.Background(), testSlackTeamID, "E_org", "C_chan", "", "hi"); err != nil {
			t.Fatalf("chat.postMessage: %v", err)
		}
		if len(lookups) != 2 || lookups[0] != testSlackTeamID || lookups[1] != "E_org" {
			t.Fatalf("lookups = %v, want [T_team E_org]", lookups)
		}
		if gotAuth != testEnterpriseSlackBearer {
			t.Fatalf("Authorization = %q, want enterprise token", gotAuth)
		}
	})

	t.Run("no fallback when enterprise equals team or is empty", func(t *testing.T) {
		t.Parallel()
		for _, entID := range []string{"", testSlackTeamID} {
			var lookups int
			post := newSlackPostMessageFuncWithTokenLookup(func(context.Context, string) (string, error) {
				lookups++
				return "", auth.ErrSlackBotTokenNotConfigured
			}, "", "https://slack.invalid/chat.postMessage", nil)
			err := post(context.Background(), testSlackTeamID, entID, "C_chan", "", "hi")
			if !errors.Is(err, auth.ErrSlackBotTokenNotConfigured) {
				t.Fatalf("enterpriseID %q: error = %v, want token-not-configured", entID, err)
			}
			if lookups != 1 {
				t.Fatalf("enterpriseID %q: lookups = %d, want 1 (no fallback)", entID, lookups)
			}
		}
	})
}

func TestSlackPostMessageFuncSurfacesSlackError(t *testing.T) {
	t.Parallel()
	// chat.postMessage returns HTTP 200 with ok:false for most failures — the
	// parser must branch on the ok field, not the status code.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
	}))
	t.Cleanup(srv.Close)

	post := newSlackPostMessageFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "", srv.URL, nil)
	err := post(context.Background(), "T_test", "", "C_gone", "", "hi")
	if err == nil || !strings.Contains(err.Error(), "channel_not_found") {
		t.Fatalf("error = %v, want channel_not_found", err)
	}
}

func TestSlackPostMessageFuncWrapsMissingScope(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"missing_scope"}`))
	}))
	t.Cleanup(srv.Close)

	post := newSlackPostMessageFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "", srv.URL, nil)
	err := post(context.Background(), "T_test", "", "C_chan", "", "hi")
	if !errors.Is(err, internal.ErrSlackMissingScope) {
		t.Fatalf("error = %v, want missing-scope sentinel", err)
	}
}

func TestSlackPostMessageFuncSurfacesRateLimit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		handler        http.HandlerFunc
		wantRetryAfter string
	}{
		{
			name:           "http 429",
			wantRetryAfter: "2",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(http.StatusTooManyRequests)
			},
		},
		{
			name:           "json ratelimited",
			wantRetryAfter: "3",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "3")
				_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
			},
		},
		{
			// Slack typically only sets Retry-After on the 429 path, so a 200 +
			// ok:false:ratelimited can arrive with no hint. The sentinel must still
			// fire (with an empty Retry-After), not slip through as a generic error.
			name:           "json ratelimited without retry-after",
			wantRetryAfter: "",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)

			post := newSlackPostMessageFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "", srv.URL, nil)
			err := post(context.Background(), "T_test", "", "C_chan", "", "hi")
			if !errors.Is(err, internal.ErrSlackRateLimited) {
				t.Fatalf("error = %v, want rate-limited sentinel", err)
			}
			if got := internal.SlackRateLimitRetryAfter(err); got != tc.wantRetryAfter {
				t.Fatalf("Retry-After = %q, want %q", got, tc.wantRetryAfter)
			}
		})
	}
}

func TestSlackPostMessageFuncRejectsOversizedResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Write more than the bounded-read limit so the seam trips its cap.
		_, _ = w.Write([]byte(`{"ok":true,"padding":"`))
		_, _ = w.Write([]byte(strings.Repeat("a", slackWebAPIResponseBodyLimit+1024)))
		_, _ = w.Write([]byte(`"}`))
	}))
	t.Cleanup(srv.Close)

	post := newSlackPostMessageFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "", srv.URL, nil)
	err := post(context.Background(), "T_test", "", "C_chan", "", "hi")
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("error = %v, want response-exceeded", err)
	}
}

func TestSlackPostMessageFuncRejectsEmptyResponseBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // 200 with no body
	}))
	t.Cleanup(srv.Close)

	post := newSlackPostMessageFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "", srv.URL, nil)
	err := post(context.Background(), "T_test", "", "C_chan", "", "hi")
	if err == nil || !strings.Contains(err.Error(), "empty response body") {
		t.Fatalf("error = %v, want empty response body", err)
	}
}

func TestSlackPostMessageFuncSurfacesHTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream boom"))
	}))
	t.Cleanup(srv.Close)

	post := newSlackPostMessageFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "", srv.URL, nil)
	err := post(context.Background(), "T_test", "", "C_chan", "", "hi")
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("error = %v, want HTTP 500", err)
	}
}

func TestSlackPostMessageFuncLookupErrorSkipsRequest(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("server must not be called when the token lookup fails")
	}))
	t.Cleanup(srv.Close)

	post := newSlackPostMessageFuncWithTokenLookup(func(context.Context, string) (string, error) {
		return "", errors.New("lookup boom")
	}, "", srv.URL, nil)
	err := post(context.Background(), "T_test", "", "C_chan", "", "hi")
	if err == nil || !strings.Contains(err.Error(), "token lookup") {
		t.Fatalf("error = %v, want token lookup failure", err)
	}
}

func TestSlackPostMessageFuncRejectsEmptyToken(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("server must not be called when the token is empty")
	}))
	t.Cleanup(srv.Close)

	post := newSlackPostMessageFuncWithTokenLookup(staticTokenLookup("   "), "", srv.URL, nil)
	err := post(context.Background(), "T_test", "", "C_chan", "", "hi")
	if err == nil || !strings.Contains(err.Error(), "empty token") {
		t.Fatalf("error = %v, want empty token", err)
	}
}

func TestSlackPostMessageFuncDefaultsUserAgent(t *testing.T) {
	t.Parallel()
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	post := newSlackPostMessageFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "", srv.URL, nil)
	if err := post(context.Background(), "T_test", "", "C_chan", "", "hi"); err != nil {
		t.Fatalf("chat.postMessage: %v", err)
	}
	if gotUA != defaultSlackAPIUserAgent {
		t.Fatalf("User-Agent = %q, want default %q", gotUA, defaultSlackAPIUserAgent)
	}
}

func TestSlackPostMessageBlocksFuncPostsBlocksAndFallback(t *testing.T) {
	t.Parallel()
	var gotBody struct {
		Channel  string           `json:"channel"`
		ThreadTS string           `json:"thread_ts"`
		Text     string           `json:"text"`
		Blocks   []map[string]any `json:"blocks"`
		Mrkdwn   *bool            `json:"mrkdwn"` // pointer: assert it was explicitly sent, not merely absent
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	post := newSlackPostMessageBlocksFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "qurl-slack/test", srv.URL, nil)
	blocks := []any{map[string]any{"type": "section", "text": map[string]any{"type": "plain_text", "text": "Revoke $x?"}}}
	if err := post(context.Background(), "T1", "E1", "C_chan", "1700.0001", blocks, "Revoke $x? (fallback)"); err != nil {
		t.Fatalf("postBlocks: %v", err)
	}
	if gotBody.Channel != "C_chan" || gotBody.ThreadTS != "1700.0001" {
		t.Fatalf("body channel/thread = %+v", gotBody)
	}
	// text is the notification / non-block-client fallback; blocks carry the card.
	if gotBody.Text != "Revoke $x? (fallback)" {
		t.Fatalf("fallback text = %q, want the fallback", gotBody.Text)
	}
	if len(gotBody.Blocks) != 1 || gotBody.Blocks[0]["type"] != "section" {
		t.Fatalf("blocks = %+v, want one section block", gotBody.Blocks)
	}
	// Defense-in-depth: mrkdwn must be explicitly false so the fallback renders literally.
	if gotBody.Mrkdwn == nil || *gotBody.Mrkdwn {
		t.Fatalf("mrkdwn = %v, want explicit false (literal fallback)", gotBody.Mrkdwn)
	}
}

func TestSlackPostMessageBlocksFuncGridFallback(t *testing.T) {
	t.Parallel()
	var owners []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	// The blocks seam shares the poster (token lookup + Grid fallback) with the text
	// seam — confirm it threads enterpriseID through to the org-token retry.
	post := newSlackPostMessageBlocksFuncWithTokenLookup(func(_ context.Context, ownerID string) (string, error) {
		owners = append(owners, ownerID)
		if ownerID == "T1" {
			return "", auth.ErrSlackBotTokenNotConfigured
		}
		return "xoxb-enterprise", nil
	}, "", srv.URL, nil)
	if err := post(context.Background(), "T1", "E1", "C_chan", "", []any{}, "x"); err != nil {
		t.Fatalf("postBlocks: %v", err)
	}
	if len(owners) != 2 || owners[0] != "T1" || owners[1] != "E1" {
		t.Fatalf("Grid fallback owners = %v, want [T1 E1]", owners)
	}
}

func TestSlackPostMessageBlocksFuncSurfacesSlackError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
	}))
	t.Cleanup(srv.Close)
	post := newSlackPostMessageBlocksFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "", srv.URL, nil)
	err := post(context.Background(), "T1", "", "C_gone", "", []any{}, "x")
	if err == nil || !strings.Contains(err.Error(), "channel_not_found") {
		t.Fatalf("error = %v, want channel_not_found", err)
	}
}

// mdTestChannel is the channel id the markdown builder tests post to. A
// shared const keeps the literal out of goconst's repeated-string count.
const mdTestChannel = "C_chan"

func TestSlackPostMarkdownMessageFuncPostsMarkdownBlockAndFallback(t *testing.T) {
	t.Parallel()
	var rawBody string
	var gotBody struct {
		Channel  string `json:"channel"`
		ThreadTS string `json:"thread_ts"`
		Text     string `json:"text"`
		Blocks   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"blocks"`
		Mrkdwn       *bool  `json:"mrkdwn"`
		MarkdownText string `json:"markdown_text"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rawBody = string(raw)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	post := newSlackPostMarkdownMessageFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "qurl-slack/test", srv.URL, nil)
	if err := post(context.Background(), "T_test", "E_test", mdTestChannel, "1700000000.000100", "Use **bold** and `code`"); err != nil {
		t.Fatalf("chat.postMessage markdown_text: %v", err)
	}
	if gotBody.Channel != mdTestChannel || gotBody.ThreadTS != "1700000000.000100" {
		t.Fatalf("body = %+v, want channel/thread_ts populated", gotBody)
	}
	if gotBody.Text != "Use **bold** and `code`" {
		t.Fatalf("fallback text = %q, want the answer text", gotBody.Text)
	}
	if len(gotBody.Blocks) != 1 || gotBody.Blocks[0].Type != "markdown" || gotBody.Blocks[0].Text != "Use **bold** and `code`" {
		t.Fatalf("blocks = %+v, want one standard-Markdown block", gotBody.Blocks)
	}
	if gotBody.Mrkdwn == nil || *gotBody.Mrkdwn {
		t.Fatalf("mrkdwn = %v, want explicit false for the fallback text", gotBody.Mrkdwn)
	}
	if gotBody.MarkdownText != "" || strings.Contains(rawBody, `"markdown_text"`) {
		t.Fatalf("body %q must not use markdown_text on the fallback-capable path", rawBody)
	}
}

func TestSlackPostMarkdownMessageFuncOmitsEmptyThreadTS(t *testing.T) {
	t.Parallel()
	var rawBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rawBody = string(raw)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	post := newSlackPostMarkdownMessageFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "", srv.URL, nil)
	if err := post(context.Background(), "T_test", "", mdTestChannel, "", "top-level **answer**"); err != nil {
		t.Fatalf("chat.postMessage markdown_text: %v", err)
	}
	if strings.Contains(rawBody, "thread_ts") {
		t.Fatalf("body %q should omit thread_ts when empty", rawBody)
	}
}

func TestSlackPostMarkdownMessageFuncFallsBackToMarkdownTextWhenBlocksRejected(t *testing.T) {
	t.Parallel()
	for _, code := range []string{slackAPIInvalidBlocks, slackAPIInvalidBlocksFormat, slackAPIInvalidBlockType, slackAPIInvalidArguments} {
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			var bodies []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				bodies = append(bodies, string(raw))
				if len(bodies) == 1 {
					_, _ = w.Write([]byte(`{"ok":false,"error":"` + code + `"}`))
					return
				}
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			t.Cleanup(srv.Close)

			post := newSlackPostMarkdownMessageFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "", srv.URL, nil)
			if err := post(context.Background(), "T_test", "", mdTestChannel, "1700000000.000100", "Use **bold**"); err != nil {
				t.Fatalf("chat.postMessage markdown fallback: %v", err)
			}
			if len(bodies) != 2 {
				t.Fatalf("requests = %d, want markdown block then markdown_text fallback: %v", len(bodies), bodies)
			}
			if !strings.Contains(bodies[0], `"blocks"`) || strings.Contains(bodies[0], `"markdown_text"`) {
				t.Fatalf("first request should use markdown blocks only: %s", bodies[0])
			}
			if !strings.Contains(bodies[1], `"markdown_text":"Use **bold**"`) || strings.Contains(bodies[1], `"blocks"`) || strings.Contains(bodies[1], `"text":`) {
				t.Fatalf("second request should use markdown_text only: %s", bodies[1])
			}
		})
	}
}

func TestSlackChatPostMessageErrorCodeUsesTypedError(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("wrapped: %w", &slackWebAPIError{op: "chat.postMessage", code: slackAPIInvalidBlockType})
	if got := slackChatPostMessageErrorCode(err); got != slackAPIInvalidBlockType {
		t.Fatalf("error code = %q, want %s", got, slackAPIInvalidBlockType)
	}
	if got := slackChatPostMessageErrorCode(errors.New("chat.postMessage: " + slackAPIInvalidBlockType)); got != "" {
		t.Fatalf("plain error-string code = %q, want empty", got)
	}
}

// chat.postEphemeral shares the poster (token lookup + Grid fallback + rate-limit/oversized
// handling) with chat.postMessage above, so these focus on the ephemeral-specific shape:
// the `user` scoping field, thread omitempty, and ok:false surfacing (so the confirm flow's
// delivered=false card-downgrade fires).

func TestSlackPostEphemeralFuncPostsScopedThreadedPayload(t *testing.T) {
	t.Parallel()
	var gotBody struct {
		Channel  string `json:"channel"`
		User     string `json:"user"`
		ThreadTS string `json:"thread_ts"`
		Text     string `json:"text"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	post := newSlackPostEphemeralFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "qurl-slack/test", srv.URL, nil)
	if err := post(context.Background(), "T1", "E1", mdTestChannel, "1700.0001", "U_clicker", "your link"); err != nil {
		t.Fatalf("chat.postEphemeral: %v", err)
	}
	if gotBody.Channel != mdTestChannel || gotBody.User != "U_clicker" || gotBody.ThreadTS != "1700.0001" || gotBody.Text != "your link" {
		t.Fatalf("body = %+v, want channel/user/thread_ts/text populated", gotBody)
	}
}

func TestSlackPostEphemeralFuncOmitsEmptyThreadTS(t *testing.T) {
	t.Parallel()
	var rawBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rawBody = string(raw)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	post := newSlackPostEphemeralFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "", srv.URL, nil)
	if err := post(context.Background(), "T1", "", mdTestChannel, "", "U_clicker", "top-level"); err != nil {
		t.Fatalf("chat.postEphemeral: %v", err)
	}
	if strings.Contains(rawBody, "thread_ts") {
		t.Fatalf("body %q should omit thread_ts when empty", rawBody)
	}
}

func TestSlackPostEphemeralFuncSurfacesSlackError(t *testing.T) {
	t.Parallel()
	// 200 + ok:false (e.g. user_not_in_channel) must surface as an error so the caller
	// downgrades the card rather than claiming a delivery that didn't happen.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"user_not_in_channel"}`))
	}))
	t.Cleanup(srv.Close)

	post := newSlackPostEphemeralFuncWithTokenLookup(staticTokenLookup("xoxb-test"), "", srv.URL, nil)
	err := post(context.Background(), "T1", "", mdTestChannel, "", "U_clicker", "x")
	if err == nil || !strings.Contains(err.Error(), "user_not_in_channel") {
		t.Fatalf("error = %v, want user_not_in_channel surfaced", err)
	}
}
