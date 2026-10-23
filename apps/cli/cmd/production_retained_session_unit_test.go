//go:build clisandbox && cliproduction

package main

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type retainedSessionRoundTrip func(*http.Request) (*http.Response, error)

func (f retainedSessionRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProductionRetainedSessionUsesOriginalCapability(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "https://fixture.invalid/content", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(&http.Cookie{Name: "qurl_vsession", Value: "original-capability"})
	calls := 0
	transport := retainedSessionRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		cookie, err := r.Cookie("qurl_vsession")
		if err != nil || cookie.Value != "original-capability" {
			t.Fatal("original capability changed or was omitted")
		}
		status, body := http.StatusOK, "fixture-content"
		if calls > 1 {
			status, body = http.StatusForbidden, "private error body"
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
	})
	if err := productionRetainedSessionFetch(request, "fixture-content", false, transport); err != nil {
		t.Fatal(err)
	}
	if err := productionRetainedSessionFetch(request, "fixture-content", true, transport); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("unexpected request count")
	}
	cookie, err := request.Cookie("qurl_vsession")
	if err != nil || cookie.Value != "original-capability" {
		t.Fatal("saved request changed")
	}
}

func TestProductionRetainedSessionRejectsRedirectAndSuccessfulContent(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusOK, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			request, err := http.NewRequest(http.MethodGet, "https://fixture.invalid/content", nil)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			transport := retainedSessionRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls > 1 {
					t.Fatal("retained-session assertion followed a redirect")
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Location": []string{"https://foreign.invalid/secret"}}, Body: io.NopCloser(strings.NewReader("sensitive-response")), Request: r}, nil
			})
			err = productionRetainedSessionFetch(request, "marker", true, transport)
			if err == nil || strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), "foreign") {
				t.Fatal("wrong refusal classification or private diagnostic exposed")
			}
		})
	}
}

func TestProductionRetainedSessionSanitizesTransportFailure(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "https://fixture.invalid/private-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := retainedSessionRoundTrip(func(*http.Request) (*http.Response, error) { return nil, errors.New("secret transport diagnostic") })
	err = productionRetainedSessionFetch(request, "marker", true, transport)
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private-token") {
		t.Fatal("transport diagnostic leaked")
	}
}
