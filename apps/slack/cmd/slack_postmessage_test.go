package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/slack/internal"
	"github.com/layervai/qurl-integrations/shared/auth"
)

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
			if ownerID == "T_team" {
				return "", auth.ErrSlackBotTokenNotConfigured
			}
			return "xoxb-enterprise-token", nil
		}, "", srv.URL, nil)
		if err := post(context.Background(), "T_team", "E_org", "C_chan", "", "hi"); err != nil {
			t.Fatalf("chat.postMessage: %v", err)
		}
		if len(lookups) != 2 || lookups[0] != "T_team" || lookups[1] != "E_org" {
			t.Fatalf("lookups = %v, want [T_team E_org]", lookups)
		}
		if gotAuth != "Bearer xoxb-enterprise-token" {
			t.Fatalf("Authorization = %q, want enterprise token", gotAuth)
		}
	})

	t.Run("no fallback when enterprise equals team or is empty", func(t *testing.T) {
		t.Parallel()
		for _, entID := range []string{"", "T_team"} {
			var lookups int
			post := newSlackPostMessageFuncWithTokenLookup(func(context.Context, string) (string, error) {
				lookups++
				return "", auth.ErrSlackBotTokenNotConfigured
			}, "", "https://slack.invalid/chat.postMessage", nil)
			err := post(context.Background(), "T_team", entID, "C_chan", "", "hi")
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
