package main

import (
	"context"
	"net/http"
	"net/http/httptest"
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
		{"an older version fails, and is not fetched again", []func(http.ResponseWriter){body(versioned(minimum - 1))}, "carries version 2, and this release needs version 3 or later", 1},
		{"no version fails", []func(http.ResponseWriter){body(skill(""))}, "carries no version the CLI can read", 1},
		{
			"an error page that holds a version is not a skill",
			[]func(http.ResponseWriter){status(http.StatusNotFound, versioned(minimum))},
			"in 3 attempts: the server answered 404", 3,
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
