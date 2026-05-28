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
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/layervai/qurl-integrations/apps/slack/internal"
)

func TestSlackOpenViewFuncPostsViewsOpenPayload(t *testing.T) {
	t.Parallel()
	var gotAuth string
	var gotUA string
	var gotBody struct {
		TriggerID string          `json:"trigger_id"`
		View      json.RawMessage `json:"view"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUA = r.Header.Get("User-Agent")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	err := slackOpenViewFuncWithURL("xoxb-test", "qurl-slack/test", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err != nil {
		t.Fatalf("views.open: %v", err)
	}
	if gotAuth != "Bearer xoxb-test" {
		t.Fatalf("Authorization = %q, want Bearer token", gotAuth)
	}
	if gotUA != "qurl-slack/test" {
		t.Fatalf("User-Agent = %q, want qurl-slack/test", gotUA)
	}
	if gotBody.TriggerID != "trigger_test" {
		t.Fatalf("trigger_id = %q, want trigger_test", gotBody.TriggerID)
	}
	if string(gotBody.View) != `{"type":"modal"}` {
		t.Fatalf("view = %s, want raw modal JSON", string(gotBody.View))
	}
}

func TestSlackOpenViewFuncSurfacesSlackError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_trigger"}`))
	}))
	t.Cleanup(srv.Close)

	err := slackOpenViewFuncWithURL("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if !errors.Is(err, internal.ErrSlackTriggerExpired) || !strings.Contains(err.Error(), "invalid_trigger") {
		t.Fatalf("error = %v, want trigger-expired sentinel wrapping invalid_trigger", err)
	}
}

func TestSlackOpenViewFuncSurfacesRateLimit(t *testing.T) {
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)

			err := slackOpenViewFuncWithURL("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
			if !errors.Is(err, internal.ErrSlackRateLimited) {
				t.Fatalf("error = %v, want rate-limited sentinel", err)
			}
			if got := internal.SlackRateLimitRetryAfter(err); got != tc.wantRetryAfter {
				t.Fatalf("Retry-After = %q, want %q", got, tc.wantRetryAfter)
			}
		})
	}
}

func TestSlackOpenViewFuncRejectsInvalidViewJSON(t *testing.T) {
	t.Parallel()

	err := slackOpenViewFuncWithURL("xoxb-test", "", "https://slack.invalid/views.open")(context.Background(), "T_test", "trigger_test", []byte(`not-json`))
	if err == nil || !strings.Contains(err.Error(), "invalid view JSON") {
		t.Fatalf("error = %v, want invalid view JSON", err)
	}
}

func TestSlackOpenViewFuncRejectsNonObjectViewJSON(t *testing.T) {
	t.Parallel()

	for _, raw := range [][]byte{
		[]byte(`null`),
		[]byte(`[1,2]`),
		[]byte(`"str"`),
		[]byte(`42`),
	} {
		err := slackOpenViewFuncWithURL("xoxb-test", "", "https://slack.invalid/views.open")(context.Background(), "T_test", "trigger_test", raw)
		if err == nil || !strings.Contains(err.Error(), "invalid view JSON") {
			t.Fatalf("input %s error = %v, want invalid view JSON", raw, err)
		}
	}
}

func TestSlackOpenViewFuncSurfacesHTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	err := slackOpenViewFuncWithURL("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("error = %v, want HTTP 502", err)
	}
}

func TestSlackOpenViewFuncSurfacesRedirectAsHTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	err := slackOpenViewFuncWithURL("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("error = %v, want HTTP 302 redirect error", err)
	}
	if strings.Contains(err.Error(), "response JSON") {
		t.Fatalf("error = %v, want redirect handled before JSON parse", err)
	}
}

func TestSlackOpenViewFuncCapsHTTPErrorBodySnippet(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(strings.Repeat("<html>gateway</html>", 40)))
	}))
	t.Cleanup(srv.Close)

	err := slackOpenViewFuncWithURL("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("error = %v, want HTTP 502", err)
	}
	maxErrLen := len("views.open returned HTTP 502: ") + slackOpenViewMaxErrorSnippetBytes + len("...")
	if len(err.Error()) > maxErrLen {
		t.Fatalf("error length = %d, want <= %d; error = %q", len(err.Error()), maxErrLen, err.Error())
	}
}

func TestSlackOpenViewFuncMakesHTTPErrorBodySnippetPrintable(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<script>\x00alert(1)</script>"))
	}))
	t.Cleanup(srv.Close)

	err := slackOpenViewFuncWithURL("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil || !strings.Contains(err.Error(), "<script>?alert(1)</script>") {
		t.Fatalf("error = %v, want printable body snippet", err)
	}
	if strings.Contains(err.Error(), "\x00") {
		t.Fatalf("error = %q, want control characters replaced", err.Error())
	}
}

func TestSlackOpenViewBodySnippetTruncatesOnUTF8Boundary(t *testing.T) {
	t.Parallel()

	got := slackOpenViewBodySnippet([]byte(strings.Repeat("\U0001F9EA", 100)))

	if !utf8.ValidString(got) {
		t.Fatalf("snippet is not valid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("snippet = %q, want truncation suffix", got)
	}
}

func TestSlackOpenViewBodySnippetRepairsMalformedUTF8(t *testing.T) {
	t.Parallel()

	raw := append(bytes.Repeat([]byte{0x80}, slackOpenViewMaxErrorSnippetBytes+10), []byte("tail")...)
	got := slackOpenViewBodySnippet(raw)

	if !utf8.ValidString(got) {
		t.Fatalf("snippet is not valid UTF-8: %q", got)
	}
	if got != "?tail" {
		t.Fatalf("snippet = %q, want repaired malformed bytes", got)
	}
}

func TestSlackOpenViewFuncSurfacesMalformedJSON(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	t.Cleanup(srv.Close)

	err := slackOpenViewFuncWithURL("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil || !strings.Contains(err.Error(), "response JSON") {
		t.Fatalf("error = %v, want response JSON", err)
	}
	if !strings.Contains(err.Error(), "not json") {
		t.Fatalf("error = %v, want bounded body snippet", err)
	}
}

func TestSlackOpenViewFuncSurfacesHTMLSuccessAsMalformedJSON(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html>not slack json</html>`))
	}))
	t.Cleanup(srv.Close)

	err := slackOpenViewFuncWithURL("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil || !strings.Contains(err.Error(), "response JSON") {
		t.Fatalf("error = %v, want response JSON for HTTP 200 HTML body", err)
	}
	if !strings.Contains(err.Error(), "<html>not slack json</html>") {
		t.Fatalf("error = %v, want HTML body snippet", err)
	}
}

func TestSlackOpenViewFuncSurfacesEmptyResponseBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	err := slackOpenViewFuncWithURL("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil || !strings.Contains(err.Error(), "empty response body") {
		t.Fatalf("error = %v, want empty response body", err)
	}
}

func TestSlackOpenViewFuncAcceptsLargeSuccessfulViewEcho(t *testing.T) {
	t.Parallel()
	successBody, err := json.Marshal(map[string]any{
		"ok": true,
		"view": map[string]any{
			"id":               "V_test",
			"team_id":          "T_test",
			"private_metadata": strings.Repeat("m", 1024),
			"state":            strings.Repeat("s", 8192),
		},
	})
	if err != nil {
		t.Fatalf("marshal success body: %v", err)
	}
	if len(successBody) <= 4096 {
		t.Fatalf("test body length = %d, want larger than old 4096-byte cap", len(successBody))
	}
	if len(successBody) > slackViewsOpenResponseBodyLimit {
		t.Fatalf("test body length = %d, want within views.open cap", len(successBody))
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(successBody)
	}))
	t.Cleanup(srv.Close)

	err = slackOpenViewFuncWithURL("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err != nil {
		t.Fatalf("views.open: %v", err)
	}
}

func TestSlackOpenViewFuncAcceptsResponseAtBodyLimit(t *testing.T) {
	t.Parallel()
	paddingLen := slackViewsOpenResponseBodyLimit - len(`{"ok":true,"padding":""}`)
	if paddingLen <= 0 {
		t.Fatalf("invalid slackViewsOpenResponseBodyLimit: %d", slackViewsOpenResponseBodyLimit)
	}
	successBody := `{"ok":true,"padding":"` + strings.Repeat("x", paddingLen) + `"}`
	if len(successBody) != slackViewsOpenResponseBodyLimit {
		t.Fatalf("test body length = %d, want %d", len(successBody), slackViewsOpenResponseBodyLimit)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(successBody))
	}))
	t.Cleanup(srv.Close)

	err := slackOpenViewFuncWithURL("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err != nil {
		t.Fatalf("views.open exactly at body limit: %v", err)
	}
}

func TestSlackOpenViewFuncSurfacesOversizedResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", slackViewsOpenResponseBodyLimit+1)))
	}))
	t.Cleanup(srv.Close)

	err := slackOpenViewFuncWithURL("xoxb-test", "", srv.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil || !strings.Contains(err.Error(), "exceeded 65536 bytes") {
		t.Fatalf("error = %v, want oversized response", err)
	}
}

func TestSlackOpenViewFuncRefusesRedirects(t *testing.T) {
	t.Parallel()
	var redirected atomic.Bool
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Store(true)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(redirectTarget.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	err := slackOpenViewFuncWithURL("xoxb-test", "", redirector.URL)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil {
		t.Fatal("views.open followed redirect and returned nil error")
	}
	if redirected.Load() {
		t.Fatal("views.open followed redirect target")
	}
}

func TestSlackOpenViewFuncDrainsAndClosesOversizedResponse(t *testing.T) {
	t.Parallel()
	body := &trackingReadCloser{reader: strings.NewReader(strings.Repeat("x", slackViewsOpenResponseBodyLimit+1024))}
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       body,
		}, nil
	})}

	err := slackOpenViewFuncWithHTTPClient("xoxb-test", "", "https://slack.test/views.open", httpClient)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err == nil || !strings.Contains(err.Error(), "exceeded 65536 bytes") {
		t.Fatalf("error = %v, want oversized response", err)
	}
	if !body.sawEOF.Load() {
		t.Fatal("oversized response body was not drained to EOF")
	}
	if !body.closed.Load() {
		t.Fatal("oversized response body was not closed")
	}
}

func TestSlackOpenViewFuncReadsAndClosesSuccessfulResponse(t *testing.T) {
	t.Parallel()
	body := &trackingReadCloser{reader: strings.NewReader(`{"ok":true}`)}
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       body,
		}, nil
	})}

	err := slackOpenViewFuncWithHTTPClient("xoxb-test", "", "https://slack.test/views.open", httpClient)(context.Background(), "T_test", "trigger_test", []byte(`{"type":"modal"}`))
	if err != nil {
		t.Fatalf("views.open: %v", err)
	}
	if !body.sawEOF.Load() {
		t.Fatal("successful response body was not read to EOF")
	}
	if !body.closed.Load() {
		t.Fatal("successful response body was not closed")
	}
}

func TestPrintableLogSnippetNormalizesUnicodeLineSeparators(t *testing.T) {
	t.Parallel()

	got := printableLogSnippet("alpha\u2028beta\u2029gamma")

	if got != "alpha beta gamma" {
		t.Fatalf("printableLogSnippet = %q, want Unicode separators normalized", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type trackingReadCloser struct {
	reader *strings.Reader
	sawEOF atomic.Bool
	closed atomic.Bool
}

func (b *trackingReadCloser) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if errors.Is(err, io.EOF) {
		b.sawEOF.Store(true)
	}
	return n, err
}

func (b *trackingReadCloser) Close() error {
	b.closed.Store(true)
	return nil
}
