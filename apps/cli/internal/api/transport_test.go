package qurlapi

import (
	"context"
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
