package consume

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// downloadHost is a scriptable link host: statuses queues per-request
// answers (0 means 200 + payload), and hits counts GETs race-safely.
type downloadHost struct {
	*httptest.Server
	payload []byte
	hits    atomic.Int32
	// onRequest, when set, runs before each response is written.
	onRequest func(hit int)
	statuses  []int
}

type failingDownloadDoer struct {
	err error
}

func (d failingDownloadDoer) Do(*http.Request) (*http.Response, error) {
	return nil, d.err
}

func newDownloadHost(t *testing.T, payload []byte, statuses ...int) *downloadHost {
	t.Helper()
	h := &downloadHost{payload: payload, statuses: statuses}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit := int(h.hits.Add(1))
		if h.onRequest != nil {
			h.onRequest(hit)
		}
		if hit <= len(h.statuses) && h.statuses[hit-1] != 0 {
			w.WriteHeader(h.statuses[hit-1])
			return
		}
		_, _ = w.Write(h.payload)
	}))
	t.Cleanup(h.Close)
	return h
}

// mint returns a Mint closure that serves the host's URL and counts calls.
func (h *downloadHost) mint(calls *int) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		*calls++
		return h.URL, nil
	}
}

func TestSaveToWritesAtomically(t *testing.T) {
	payload := []byte("hello download\n")
	host := newDownloadHost(t, payload)
	dest := filepath.Join(t.TempDir(), "out.bin")

	mints := 0
	d := &Downloader{Mint: host.mint(&mints)}
	n, err := d.SaveTo(context.Background(), dest, false)
	if err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	if n != int64(len(payload)) {
		t.Errorf("bytes = %d, want %d", n, len(payload))
	}
	if got := readTestFile(t, dest); !bytes.Equal(got, payload) {
		t.Errorf("destination = %q, want the payload", got)
	}
	mustNotExist(t, dest+partSuffix)
	if mints != 1 {
		t.Errorf("mint calls = %d, want 1", mints)
	}
}

func TestSaveToRefusesExistingWithoutForce(t *testing.T) {
	host := newDownloadHost(t, []byte("x"))
	dest := filepath.Join(t.TempDir(), "out.bin")
	if err := os.WriteFile(dest, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}

	mints := 0
	d := &Downloader{Mint: host.mint(&mints)}
	_, err := d.SaveTo(context.Background(), dest, false)
	if !errors.Is(err, ErrFileExists) {
		t.Fatalf("err = %v, want ErrFileExists", err)
	}
	// The refusal must be free: no resolve minted, no request sent.
	if mints != 0 || host.hits.Load() != 0 {
		t.Errorf("mints = %d, GETs = %d; want 0 and 0 before any network", mints, host.hits.Load())
	}
	if got := readTestFile(t, dest); string(got) != "precious" {
		t.Errorf("existing file was touched: %q", got)
	}
}

func TestSaveToForceReplaces(t *testing.T) {
	payload := []byte("fresh bytes")
	host := newDownloadHost(t, payload)
	dest := filepath.Join(t.TempDir(), "out.bin")
	if err := os.WriteFile(dest, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	mints := 0
	d := &Downloader{Mint: host.mint(&mints)}
	if _, err := d.SaveTo(context.Background(), dest, true); err != nil {
		t.Fatalf("SaveTo --force: %v", err)
	}
	if got := readTestFile(t, dest); !bytes.Equal(got, payload) {
		t.Errorf("destination = %q, want the fresh payload", got)
	}
	mustNotExist(t, dest+partSuffix)
}

// TestCheckDestinationTable pins the preflight rules, including that a
// directory refuses even with --force.
func TestCheckDestinationTable(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "have.bin")
	if err := os.WriteFile(existing, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := CheckDestination(filepath.Join(dir, "missing.bin"), false); err != nil {
		t.Errorf("missing destination: %v, want nil", err)
	}
	if err := CheckDestination(existing, false); !errors.Is(err, ErrFileExists) {
		t.Errorf("existing without force: %v, want ErrFileExists", err)
	}
	if err := CheckDestination(existing, true); err != nil {
		t.Errorf("existing with force: %v, want nil", err)
	}
	for _, force := range []bool{false, true} {
		if err := CheckDestination(dir, force); !errors.Is(err, ErrFileExists) {
			t.Errorf("directory (force=%t): %v, want ErrFileExists", force, err)
		}
	}
}

func TestSaveToRetriesExactlyOnceOnExpiry(t *testing.T) {
	payload := []byte("second try wins")
	host := newDownloadHost(t, payload, http.StatusGone, 0)
	dest := filepath.Join(t.TempDir(), "out.bin")

	mints := 0
	d := &Downloader{Mint: host.mint(&mints)}
	n, err := d.SaveTo(context.Background(), dest, false)
	if err != nil {
		t.Fatalf("SaveTo after one expiry: %v", err)
	}
	if n != int64(len(payload)) {
		t.Errorf("bytes = %d, want %d", n, len(payload))
	}
	// Expiry costs one fresh mint (re-verified by the caller's closure) and
	// one more GET — exactly one of each.
	if mints != 2 {
		t.Errorf("mint calls = %d, want 2", mints)
	}
	if got := host.hits.Load(); got != 2 {
		t.Errorf("GETs = %d, want 2", got)
	}
}

func TestSaveToExpirySurvivingRetryFails(t *testing.T) {
	host := newDownloadHost(t, []byte("never"), http.StatusGone, http.StatusGone, 0)
	dest := filepath.Join(t.TempDir(), "out.bin")

	mints := 0
	d := &Downloader{Mint: host.mint(&mints)}
	_, err := d.SaveTo(context.Background(), dest, false)
	if !errors.Is(err, ErrLinkExpired) {
		t.Fatalf("err = %v, want ErrLinkExpired", err)
	}
	// Retry once means once: two GETs, never a third.
	if got := host.hits.Load(); got != 2 {
		t.Errorf("GETs = %d, want exactly 2", got)
	}
	if mints != 2 {
		t.Errorf("mint calls = %d, want exactly 2", mints)
	}
	mustNotExist(t, dest)
	mustNotExist(t, dest+partSuffix)
}

func TestDownloadLiveGrantRetriesSameURL(t *testing.T) {
	payload := []byte("grant converged")
	host := newDownloadHost(t, payload, http.StatusGone, 0)
	now := time.Unix(1_700_000_000, 0)
	mints := 0
	d := &Downloader{
		MintTarget: func(context.Context) (DownloadTarget, error) {
			mints++
			return DownloadTarget{URL: host.URL, ValidUntil: now.Add(time.Minute)}, nil
		},
		now: func() time.Time { return now },
		wait: func(_ context.Context, delay time.Duration) error {
			now = now.Add(delay)
			return nil
		},
	}

	var out bytes.Buffer
	if _, err := d.StreamTo(context.Background(), &out); err != nil {
		t.Fatalf("StreamTo: %v", err)
	}
	if got := out.String(); got != string(payload) {
		t.Errorf("payload = %q, want %q", got, payload)
	}
	if mints != 1 || host.hits.Load() != 2 {
		t.Errorf("mints = %d, GETs = %d; want 1 and 2", mints, host.hits.Load())
	}
}

func TestDownloadLiveGrantRetryIsBounded(t *testing.T) {
	host := newDownloadHost(t, nil,
		http.StatusGone, http.StatusGone, http.StatusGone,
		http.StatusGone, http.StatusGone, http.StatusGone)
	now := time.Unix(1_700_000_000, 0)
	started := now
	mints := 0
	d := &Downloader{
		MintTarget: func(context.Context) (DownloadTarget, error) {
			mints++
			return DownloadTarget{URL: host.URL, ValidUntil: now.Add(time.Minute)}, nil
		},
		now: func() time.Time { return now },
		wait: func(_ context.Context, delay time.Duration) error {
			now = now.Add(delay)
			return nil
		},
	}

	_, err := d.StreamTo(context.Background(), io.Discard)
	if !errors.Is(err, ErrLinkFetch) {
		t.Fatalf("err = %v, want ErrLinkFetch", err)
	}
	if mints != 1 || host.hits.Load() != 1+grantPropagationMaxAttempts {
		t.Errorf("mints = %d, GETs = %d; want 1 and at most %d", mints, host.hits.Load(), 1+grantPropagationMaxAttempts)
	}
	if elapsed := now.Sub(started); elapsed > grantPropagationWindow {
		t.Errorf("retry elapsed = %s, want at most %s", elapsed, grantPropagationWindow)
	}
}

func TestDownloadLiveGrantRetryHonorsCancellation(t *testing.T) {
	host := newDownloadHost(t, nil, http.StatusGone)
	now := time.Unix(1_700_000_000, 0)
	mints := 0
	ctx, cancel := context.WithCancel(context.Background())
	d := &Downloader{
		MintTarget: func(context.Context) (DownloadTarget, error) {
			mints++
			return DownloadTarget{URL: host.URL, ValidUntil: now.Add(time.Minute)}, nil
		},
		now: func() time.Time { return now },
		wait: func(ctx context.Context, _ time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}

	_, err := d.StreamTo(ctx, io.Discard)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if mints != 1 || host.hits.Load() != 1 {
		t.Errorf("mints = %d, GETs = %d; want 1 and 1", mints, host.hits.Load())
	}
}

func TestDownloadExpiredGrantMintsOneFreshTarget(t *testing.T) {
	payload := []byte("fresh grant")
	host := newDownloadHost(t, payload, http.StatusGone, 0)
	now := time.Unix(1_700_000_000, 0)
	mints := 0
	d := &Downloader{
		MintTarget: func(context.Context) (DownloadTarget, error) {
			mints++
			validUntil := now
			if mints == 2 {
				validUntil = now.Add(time.Minute)
			}
			return DownloadTarget{URL: host.URL, ValidUntil: validUntil}, nil
		},
		now: func() time.Time { return now },
	}

	var out bytes.Buffer
	if _, err := d.StreamTo(context.Background(), &out); err != nil {
		t.Fatalf("StreamTo: %v", err)
	}
	if got := out.String(); got != string(payload) {
		t.Errorf("payload = %q, want %q", got, payload)
	}
	if mints != 2 || host.hits.Load() != 2 {
		t.Errorf("mints = %d, GETs = %d; want 2 and 2", mints, host.hits.Load())
	}
}

func TestDownloadGrantExpiryDuringPropagationMintsOneFreshTarget(t *testing.T) {
	payload := []byte("fresh after retained grant expired")
	host := newDownloadHost(t, payload, http.StatusGone, http.StatusGone, 0)
	now := time.Unix(1_700_000_000, 0)
	mints := 0
	d := &Downloader{
		MintTarget: func(context.Context) (DownloadTarget, error) {
			mints++
			validUntil := now.Add(time.Minute)
			if mints == 1 {
				validUntil = now.Add(150 * time.Millisecond)
			}
			return DownloadTarget{URL: host.URL, ValidUntil: validUntil}, nil
		},
		now: func() time.Time { return now },
		wait: func(_ context.Context, delay time.Duration) error {
			now = now.Add(delay)
			return nil
		},
	}

	var out bytes.Buffer
	if _, err := d.StreamTo(context.Background(), &out); err != nil {
		t.Fatalf("StreamTo: %v", err)
	}
	if got := out.String(); got != string(payload) {
		t.Errorf("payload = %q, want %q", got, payload)
	}
	if mints != 2 || host.hits.Load() != 3 {
		t.Errorf("mints = %d, GETs = %d; want 2 and 3", mints, host.hits.Load())
	}
}

func TestDownloadTransportErrorDoesNotExposeGrantedURL(t *testing.T) {
	const secretURL = "https://download.example/capability-secret"
	d := &Downloader{
		Client: failingDownloadDoer{err: errors.New("Get \"" + secretURL + "\": dial failed")},
		MintTarget: func(context.Context) (DownloadTarget, error) {
			return DownloadTarget{URL: secretURL, ValidUntil: time.Now().Add(time.Minute)}, nil
		},
	}

	_, err := d.StreamTo(context.Background(), io.Discard)
	if !errors.Is(err, ErrLinkFetch) {
		t.Fatalf("err = %v, want ErrLinkFetch", err)
	}
	if strings.Contains(err.Error(), secretURL) || strings.Contains(err.Error(), "capability-secret") {
		t.Fatalf("transport error exposed granted authority: %q", err)
	}
}

func TestDownloadRequestBuildErrorDoesNotExposeGrantedURL(t *testing.T) {
	const secretURL = "https://download.example/capability-secret\n"
	d := &Downloader{MintTarget: func(context.Context) (DownloadTarget, error) {
		return DownloadTarget{URL: secretURL, ValidUntil: time.Now().Add(time.Minute)}, nil
	}}

	_, err := d.StreamTo(context.Background(), io.Discard)
	if !errors.Is(err, ErrLinkFetch) {
		t.Fatalf("err = %v, want ErrLinkFetch", err)
	}
	if strings.Contains(err.Error(), secretURL) || strings.Contains(err.Error(), "capability-secret") {
		t.Fatalf("request-build error exposed granted authority: %q", err)
	}
}

func TestSaveToNonExpiryStatusDoesNotRetry(t *testing.T) {
	host := newDownloadHost(t, []byte("never"), http.StatusInternalServerError)
	dest := filepath.Join(t.TempDir(), "out.bin")

	mints := 0
	d := &Downloader{Mint: host.mint(&mints)}
	_, err := d.SaveTo(context.Background(), dest, false)
	if !errors.Is(err, ErrLinkFetch) {
		t.Fatalf("err = %v, want ErrLinkFetch", err)
	}
	if got := host.hits.Load(); got != 1 {
		t.Errorf("GETs = %d, want 1 (only expiry retries)", got)
	}
	if mints != 1 {
		t.Errorf("mint calls = %d, want 1", mints)
	}
	mustNotExist(t, dest)
	mustNotExist(t, dest+partSuffix)
}

func TestSaveToCleansPartOnTruncatedBody(t *testing.T) {
	// A Content-Length the server never honors surfaces as a body read
	// error mid-copy — the closest honest stand-in for a dropped
	// connection.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("only a little"))
	}))
	t.Cleanup(srv.Close)
	dest := filepath.Join(t.TempDir(), "out.bin")

	d := &Downloader{Mint: func(context.Context) (string, error) { return srv.URL, nil }}
	_, err := d.SaveTo(context.Background(), dest, false)
	if !errors.Is(err, ErrLinkFetch) {
		t.Fatalf("err = %v, want ErrLinkFetch for a broken-off body", err)
	}
	mustNotExist(t, dest)
	mustNotExist(t, dest+partSuffix)
}

func TestSaveToRefusesDestinationThatAppearedMidDownload(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "out.bin")
	host := newDownloadHost(t, []byte("payload"))
	// The destination appears while the response is being served: the
	// finalize re-check must refuse rather than clobber it.
	host.onRequest = func(int) {
		if err := os.WriteFile(dest, []byte("appeared"), 0o600); err != nil {
			t.Errorf("plant destination: %v", err)
		}
	}

	d := &Downloader{Mint: func(context.Context) (string, error) { return host.URL, nil }}
	_, err := d.SaveTo(context.Background(), dest, false)
	if !errors.Is(err, ErrFileExists) {
		t.Fatalf("err = %v, want ErrFileExists at finalize", err)
	}
	if got := readTestFile(t, dest); string(got) != "appeared" {
		t.Errorf("mid-download file was clobbered: %q", got)
	}
	mustNotExist(t, dest+partSuffix)
}

func TestSaveToCanceledMidDownloadCleansPart(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("first bytes"))
		w.(http.Flusher).Flush()
		close(started)
		<-release
	}))
	t.Cleanup(srv.Close)
	// Registered after srv.Close so it runs FIRST (cleanups are LIFO):
	// Close waits for active handlers, and the handler waits on release.
	t.Cleanup(func() { close(release) })
	dest := filepath.Join(t.TempDir(), "out.bin")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	d := &Downloader{Mint: func(context.Context) (string, error) { return srv.URL, nil }}
	go func() {
		_, err := d.SaveTo(ctx, dest, false)
		done <- err
	}()

	<-started
	cancel()
	err := <-done
	// Ctrl-C must keep its identity end to end so the process exits 130.
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled in the chain", err)
	}
	mustNotExist(t, dest)
	mustNotExist(t, dest+partSuffix)
}

func TestStreamToIsBinaryClean(t *testing.T) {
	payload := []byte("bin\x00ary\r\npay\x1b[31mload\xff\xfe")
	host := newDownloadHost(t, payload)

	var out bytes.Buffer
	mints := 0
	d := &Downloader{Mint: host.mint(&mints)}
	n, err := d.StreamTo(context.Background(), &out)
	if err != nil {
		t.Fatalf("StreamTo: %v", err)
	}
	if n != int64(len(payload)) {
		t.Errorf("bytes = %d, want %d", n, len(payload))
	}
	if !bytes.Equal(out.Bytes(), payload) {
		t.Errorf("stream = %q, want the exact payload bytes", out.Bytes())
	}
}

func TestStreamToRetriesOnExpiry(t *testing.T) {
	payload := []byte("streamed on retry")
	host := newDownloadHost(t, payload, http.StatusGone, 0)

	var out bytes.Buffer
	mints := 0
	d := &Downloader{Mint: host.mint(&mints)}
	if _, err := d.StreamTo(context.Background(), &out); err != nil {
		t.Fatalf("StreamTo after one expiry: %v", err)
	}
	if !bytes.Equal(out.Bytes(), payload) {
		t.Errorf("stream = %q, want the payload", out.Bytes())
	}
	if mints != 2 || host.hits.Load() != 2 {
		t.Errorf("mints = %d, GETs = %d; want 2 and 2", mints, host.hits.Load())
	}
}

// TestNilClientIsCachedAcrossRetry pins the lazy-init contract: with no
// injected Client, the first request builds the default client and KEEPS it,
// so the expiry retry runs on the same connection pool the drained first
// response was returned to (a fresh client per request would make discard's
// drain-for-reuse pointless).
func TestNilClientIsCachedAcrossRetry(t *testing.T) {
	payload := []byte("same pool")
	host := newDownloadHost(t, payload, http.StatusGone, 0)

	var out bytes.Buffer
	mints := 0
	d := &Downloader{Mint: host.mint(&mints)}
	if _, err := d.StreamTo(context.Background(), &out); err != nil {
		t.Fatalf("StreamTo after one expiry: %v", err)
	}
	cached := d.Client
	if cached == nil {
		t.Fatal("Client = nil after a download; want the default client cached on the Downloader")
	}
	if _, err := d.StreamTo(context.Background(), &out); err != nil {
		t.Fatalf("second StreamTo: %v", err)
	}
	if d.Client != cached {
		t.Error("Client was rebuilt between downloads; want the first default client kept")
	}
}

// TestMintFailurePropagatesUnwrapped pins that resolve/verification errors
// keep their identity (and so their exit codes) through the downloader.
func TestMintFailurePropagatesUnwrapped(t *testing.T) {
	sentinel := errors.New("verification says no")
	d := &Downloader{Mint: func(context.Context) (string, error) { return "", sentinel }}
	if _, err := d.StreamTo(context.Background(), &bytes.Buffer{}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the mint error unchanged", err)
	}
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s exists (stat err %v), want absent", path, err)
	}
}

// readTestFile reads a file this test created under t.TempDir.
func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	// #nosec G304 -- path is a t.TempDir destination the test itself chose.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}
