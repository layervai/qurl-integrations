package qurlapi

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestTransportBoundsInjectedHTTPClientWithoutTimeout(t *testing.T) {
	injected := &http.Client{}
	transport := newTransport(&Config{HTTPClient: injected, Version: "test"})
	if transport.next.Timeout != defaultHTTPClientTimeout {
		t.Fatalf("injected client timeout = %s, want %s", transport.next.Timeout, defaultHTTPClientTimeout)
	}
	if injected.Timeout != 0 {
		t.Fatalf("caller-owned client timeout mutated to %s", injected.Timeout)
	}

	const explicit = 7 * time.Second
	transport = newTransport(&Config{HTTPClient: &http.Client{Timeout: explicit}, Version: "test"})
	if transport.next.Timeout != explicit {
		t.Fatalf("explicit injected client timeout = %s, want %s", transport.next.Timeout, explicit)
	}
}

func TestTransportRefusesRedirectFromInjectedHTTPClient(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	t.Cleanup(destination.Close)

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, destination.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)

	injected := source.Client()
	injected.CheckRedirect = func(*http.Request, []*http.Request) error {
		t.Fatal("injected redirect policy was used")
		return nil
	}
	transport := newTransport(&Config{HTTPClient: injected, Version: "test"})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, source.URL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer must-not-follow")

	resp, err := transport.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status=%d, want %d", resp.StatusCode, http.StatusTemporaryRedirect)
	}
	if redirected.Load() != 0 {
		t.Fatalf("redirect destination received %d requests, want zero", redirected.Load())
	}
}

func TestTransportExplicitNoReplayIsPathIndependent(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	transport := newTransport(&Config{HTTPClient: srv.Client(), Version: "test", Sleep: func(time.Duration) {}})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/not-a-special-path", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := transport.Do(withRequestRetryIntent(req, false))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusTooManyRequests || requests.Load() != 1 {
		t.Fatalf("explicit no-replay response = HTTP %d after %d requests, want HTTP 429 after one", resp.StatusCode, requests.Load())
	}
}

func TestTransportNeverRetriesUnsafe429WithoutIdempotencyKey(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit=%t", explicit), func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			t.Cleanup(srv.Close)

			transport := newTransport(&Config{HTTPClient: srv.Client(), Version: "test", Sleep: func(time.Duration) {}})
			req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/v1/resources", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			if explicit {
				req = withRequestRetryIntent(req, true)
			}
			resp, err := transport.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = resp.Body.Close() })
			if resp.StatusCode != http.StatusTooManyRequests || requests.Load() != 1 {
				t.Fatalf("unsafe response = HTTP %d after %d requests, want HTTP 429 after one", resp.StatusCode, requests.Load())
			}
		})
	}
}

func TestTransportRetriesSafe429(t *testing.T) {
	for _, test := range []struct {
		name, method, idempotencyKey string
	}{
		{name: "read", method: http.MethodGet},
		{name: "idempotent mutation", method: http.MethodPost, idempotencyKey: "publish-attempt-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			t.Cleanup(srv.Close)

			transport := newTransport(&Config{HTTPClient: srv.Client(), Version: "test", Sleep: func(time.Duration) {}})
			req, err := http.NewRequestWithContext(context.Background(), test.method, srv.URL, http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Idempotency-Key", test.idempotencyKey)
			resp, err := transport.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = resp.Body.Close() })
			if resp.StatusCode != http.StatusTooManyRequests || requests.Load() != maxAttempts {
				t.Fatalf("safe response = HTTP %d after %d requests, want HTTP 429 after %d", resp.StatusCode, requests.Load(), maxAttempts)
			}
		})
	}
}

func TestTransportRetriesSafe503ButNeverUnkeyedMutation(t *testing.T) {
	for _, test := range []struct {
		name, method, idempotencyKey string
		wantRequests                 int32
	}{
		{name: "read", method: http.MethodGet, wantRequests: maxAttempts},
		{name: "idempotent mutation", method: http.MethodPost, idempotencyKey: "publish-attempt-1", wantRequests: maxAttempts},
		{name: "unkeyed mutation", method: http.MethodPost, wantRequests: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			t.Cleanup(srv.Close)

			transport := newTransport(&Config{HTTPClient: srv.Client(), Version: "test", Sleep: func(time.Duration) {}})
			req, err := http.NewRequestWithContext(context.Background(), test.method, srv.URL, http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Idempotency-Key", test.idempotencyKey)
			resp, err := transport.Do(withRequestRetryIntent(req, true))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = resp.Body.Close() })
			if resp.StatusCode != http.StatusServiceUnavailable || requests.Load() != test.wantRequests {
				t.Fatalf("response = HTTP %d after %d requests, want HTTP 503 after %d", resp.StatusCode, requests.Load(), test.wantRequests)
			}
		})
	}
}

func TestTransportDoesNotMutateCallerRequestAcrossRetry(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) < maxAttempts {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	transport := newTransport(&Config{BaseURL: srv.URL, HTTPClient: srv.Client(), Version: "test", Sleep: func(time.Duration) {}})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		srv.URL+"/v1/resources/qexample/resolve", bytes.NewBufferString("payload"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Caller", "preserved")
	originalBody := req.Body
	resp, err := transport.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK || requests.Load() != maxAttempts {
		t.Fatalf("response = HTTP %d after %d requests", resp.StatusCode, requests.Load())
	}
	if req.Body != originalBody || req.Header.Get("User-Agent") != "" || req.Header.Get("X-Request-Id") != "" || req.Header.Get("X-Caller") != "preserved" {
		t.Fatalf("caller request was mutated: body_same=%t headers=%v", req.Body == originalBody, req.Header)
	}
}
