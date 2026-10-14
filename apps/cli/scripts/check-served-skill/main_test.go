package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func skill(metadata string) string {
	return "---\nname: qurl\ndescription: Share an app.\n" + metadata + "---\n\n# qURL\n"
}

func versioned(version int) string {
	return skill("metadata:\n  version: \"" + strconv.Itoa(version) + "\"\n")
}

// serve answers every request with the next of answers, and the last one
// from then on. It returns the address and a count of the requests made.
func serve(t *testing.T, answers ...func(http.ResponseWriter)) (address string, requests *atomic.Int32) {
	t.Helper()
	requests = &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		index := int(requests.Add(1)) - 1
		if index >= len(answers) {
			index = len(answers) - 1
		}
		answers[index](w)
	}))
	t.Cleanup(server.Close)
	return server.URL, requests
}

func body(content string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) { _, _ = w.Write([]byte(content)) }
}

func status(code int, content string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(content))
	}
}

// cutOff promises more of the file than it sends.
func cutOff(content string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Length", strconv.Itoa(len(content)+100))
		_, _ = w.Write([]byte(content))
	}
}

func TestCheck(t *testing.T) {
	const minimum = 3

	tests := []struct {
		name         string
		answers      []func(http.ResponseWriter)
		wantErr      string // a part of the error; empty when the check passes
		wantRequests int32
	}{
		{"the oldest version the release works with passes", []func(http.ResponseWriter){body(versioned(minimum))}, "", 1},
		{"a newer version passes", []func(http.ResponseWriter){body(versioned(minimum + 1))}, "", 1},
		{"an older version fails, and is not fetched again", []func(http.ResponseWriter){body(versioned(minimum - 1))}, "carries version 2, and this CLI needs version 3 or later", 1},
		{"no version fails", []func(http.ResponseWriter){body(skill(""))}, "carries no version the CLI can read", 1},
		{
			"an error page that holds a version is not a skill",
			[]func(http.ResponseWriter){status(http.StatusNotFound, versioned(minimum))},
			"the server will not serve the file: it answered 404", 1,
		},
		{
			"a server that asks for a slower pace is tried again",
			[]func(http.ResponseWriter){status(http.StatusTooManyRequests, ""), body(versioned(minimum))},
			"", 2,
		},
		{
			"a server in trouble every time fails after the last attempt",
			[]func(http.ResponseWriter){status(http.StatusBadGateway, "")},
			"in 3 attempts: the server answered 502", 3,
		},
		{
			"an answer cut off inside what is read is tried again, and is not a skill with no version",
			[]func(http.ResponseWriter){cutOff(skill("")), body(versioned(minimum))},
			"", 2,
		},
		{
			"a fetch that fails is tried again",
			[]func(http.ResponseWriter){status(http.StatusBadGateway, ""), status(http.StatusBadGateway, ""), body(versioned(minimum))},
			"", 3,
		},
		{
			"a fetch that fails and then finds an old skill fails for the old skill",
			[]func(http.ResponseWriter){status(http.StatusBadGateway, ""), body(skill(""))},
			"carries no version the CLI can read", 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			address, requests := serve(t, tc.answers...)
			err := check(context.Background(), http.DefaultClient, address, minimum, 0)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("check = %v, want no error", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("check = %v, want an error holding %q", err, tc.wantErr)
			}
			if got := requests.Load(); got != tc.wantRequests {
				t.Errorf("made %d requests, want %d", got, tc.wantRequests)
			}
		})
	}
}

func TestCheckFollowsARedirectAsTheNotesCommandDoes(t *testing.T) {
	target, _ := serve(t, body(versioned(1)))
	redirect := httptest.NewServer(http.RedirectHandler(target, http.StatusFound))
	t.Cleanup(redirect.Close)
	if err := check(context.Background(), http.DefaultClient, redirect.URL, 1, 0); err != nil {
		t.Errorf("check = %v, want no error", err)
	}
}

func TestCheckStopsWaitingWhenItsContextEnds(t *testing.T) {
	address, requests := serve(t, status(http.StatusBadGateway, ""))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := check(ctx, http.DefaultClient, address, 1, 0); err == nil {
		t.Error("check returned no error")
	}
	if got := requests.Load(); got > 1 {
		t.Errorf("made %d requests after the context ended", got)
	}
}

func TestCheckNamesItselfAndAsksForTheFileAsItIsNow(t *testing.T) {
	var agent, cache atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent.Store(r.Header.Get("User-Agent"))
		cache.Store(r.Header.Get("Cache-Control"))
		_, _ = w.Write([]byte(versioned(1)))
	}))
	t.Cleanup(server.Close)
	if err := check(context.Background(), http.DefaultClient, server.URL, 1, 0); err != nil {
		t.Fatalf("check = %v, want no error", err)
	}
	if got := agent.Load(); got != "qurl-served-skill-check/1" {
		t.Errorf("User-Agent = %q", got)
	}
	if got := cache.Load(); got != "no-cache" {
		t.Errorf("Cache-Control = %q", got)
	}
}

// bundledCopy writes content where -bundled would read it.
func bundledCopy(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCompare(t *testing.T) {
	served := versioned(2)

	tests := []struct {
		name         string
		local        string
		answers      []func(http.ResponseWriter)
		wantErr      string // a part of the error; empty when the copies match
		wantRequests int32
	}{
		{"the same bytes pass", served, []func(http.ResponseWriter){body(served)}, "", 1},
		{
			"a copy that differs fails, and says where and how to fix it",
			versioned(1), []func(http.ResponseWriter){body(served)},
			"first difference at byte " + strconv.Itoa(strings.Index(served, `"2"`)+1) + "). Copy the served file over it", 1,
		},
		{
			"line endings are bytes too",
			strings.ReplaceAll(served, "\n", "\r\n"), []func(http.ResponseWriter){body(served)},
			"first difference at byte 3)", 1,
		},
		{
			"a copy that is the start of the served file fails",
			served[:len(served)-1], []func(http.ResponseWriter){body(served)},
			"first difference at byte " + strconv.Itoa(len(served)-1) + ")", 1,
		},
		{
			"an error page that holds the same bytes is not the skill",
			served, []func(http.ResponseWriter){status(http.StatusNotFound, served)},
			"the server will not serve the file: it answered 404", 1,
		},
		{
			"a server in trouble is tried again",
			served, []func(http.ResponseWriter){status(http.StatusBadGateway, ""), body(served)},
			"", 2,
		},
		{
			"an answer cut off is tried again, and is not a copy that differs",
			served, []func(http.ResponseWriter){cutOff(served), body(served)},
			"", 2,
		},
		{
			"a file too long to be the skill fails, and is not fetched again",
			served, []func(http.ResponseWriter){body(strings.Repeat("x", fileLimit+1))},
			"too long to be the skill", 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			address, requests := serve(t, tc.answers...)
			err := compare(context.Background(), http.DefaultClient, address, bundledCopy(t, tc.local), 0)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("compare = %v, want no error", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("compare = %v, want an error holding %q", err, tc.wantErr)
			}
			if got := requests.Load(); got != tc.wantRequests {
				t.Errorf("made %d requests, want %d", got, tc.wantRequests)
			}
		})
	}
}

func TestCompareReadsTheCopyBeforeItFetches(t *testing.T) {
	address, requests := serve(t, body(versioned(1)))
	err := compare(context.Background(), http.DefaultClient, address, filepath.Join(t.TempDir(), "missing.md"), 0)
	if err == nil || !strings.Contains(err.Error(), "could not read the bundled copy") {
		t.Errorf("compare = %v, want an error about the bundled copy", err)
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("made %d requests for a copy that does not exist", got)
	}
}
