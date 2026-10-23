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
	for _, refusal := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusGone} {
		t.Run(http.StatusText(refusal), func(t *testing.T) {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://fixture.invalid/content", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			request.AddCookie(&http.Cookie{Name: "qurl_vsession", Value: "original-capability", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
			calls := 0
			transport := retainedSessionRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				cookie, err := r.Cookie("qurl_vsession")
				if err != nil || cookie.Value != "original-capability" {
					t.Fatal("original capability changed or was omitted")
				}
				status, body := http.StatusOK, "fixture-content"
				if calls > 1 {
					status, body = refusal, "private error body"
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
		})
	}
}

func TestProductionRetainedSessionRejectsRedirectAndSuccessfulContent(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusOK, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://fixture.invalid/content", http.NoBody)
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
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://fixture.invalid/private-token", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	transport := retainedSessionRoundTrip(func(*http.Request) (*http.Response, error) { return nil, errors.New("secret transport diagnostic") })
	err = productionRetainedSessionFetch(request, "marker", true, transport)
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private-token") {
		t.Fatal("transport diagnostic leaked")
	}
}

type retainedSessionCloseError struct{ io.Reader }

func (retainedSessionCloseError) Close() error { return errors.New("private close diagnostic") }

func TestProductionRetainedSessionSanitizesCloseFailure(t *testing.T) {
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://fixture.invalid/content", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	transport := retainedSessionRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: retainedSessionCloseError{strings.NewReader("private response")}, Request: r}, nil
	})
	err = productionRetainedSessionFetch(request, "marker", true, transport)
	if err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("response close failed silently or leaked its diagnostic")
	}
}

func TestProductionRetainedSessionRejectsWrongOrOversizePositiveContent(t *testing.T) {
	for _, body := range []string{"wrong!", "marker-extra"} {
		t.Run(body, func(t *testing.T) {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://fixture.invalid/content", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			transport := retainedSessionRoundTrip(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			if err := productionRetainedSessionFetch(request, "marker", false, transport); err == nil {
				t.Fatal("unexpected content passed original-session positive control")
			}
		})
	}
}
