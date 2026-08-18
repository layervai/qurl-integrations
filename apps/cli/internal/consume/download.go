package consume

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// Doer is the injectable HTTP seam for downloads.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// partSuffix marks the in-flight half of an atomic download; the suffix is
// part of the command's help text, so a user who finds one knows what it is.
const partSuffix = ".part"

// responseHeaderTimeout bounds how long the link host may sit silent before
// the first response byte. There is deliberately no whole-request timeout: a
// large download takes as long as it takes, and Ctrl-C (context
// cancellation) is the user's abort.
const responseHeaderTimeout = 30 * time.Second

// NewHTTPClient builds the download client: the default transport with a
// response-header bound, following redirects (a resolved link may bounce to
// storage). It carries no credentials — the qURL API key must never reach
// the link host — and no overall timeout.
func NewHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = responseHeaderTimeout
	return &http.Client{Transport: transport}
}

// Downloader fetches the bytes behind freshly minted, already-verified
// access links. It is single-use and not safe for concurrent use.
type Downloader struct {
	// Client performs the GETs; nil means NewHTTPClient's default, built on
	// first use and kept on the Downloader so every request of one download
	// — the expiry retry included — shares one connection pool.
	Client Doer
	// Mint resolves a fresh access link. The closure the CLI injects
	// re-runs CRID verification on every call, so the retry path is exactly
	// as fail-closed as the first attempt.
	Mint func(ctx context.Context) (string, error)
}

// SaveTo downloads to path atomically: bytes land in path+".part", which
// becomes path only after a complete, synced write. The .part file is
// removed on every failure, cancellation included. Without force an
// existing destination refuses before any network traffic (and again at
// rename time, so a file that appeared mid-download is not clobbered).
func (d *Downloader) SaveTo(ctx context.Context, path string, force bool) (int64, error) {
	if err := CheckDestination(path, force); err != nil {
		return 0, err
	}
	body, err := d.fetch(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = body.Close() }()

	part := path + partSuffix
	// #nosec G304 -- path is the user's own --file operand; writing there is
	// the command's purpose.
	f, err := os.Create(part)
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", part, err)
	}

	n, err := io.Copy(f, body)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(part)
		return 0, wrapCopyError(ctx, err)
	}
	// Sync then close before rename: the rename must only ever publish a
	// complete, durable file.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(part)
		return 0, fmt.Errorf("write %s: %w", part, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(part)
		return 0, fmt.Errorf("write %s: %w", part, err)
	}
	// Re-check the destination: without --force, a file that appeared while
	// the download ran must survive. The stat-then-rename window is not
	// atomic, but it shrinks the race to microseconds and keeps the common
	// cases honest.
	if err := CheckDestination(path, force); err != nil {
		_ = os.Remove(part)
		return 0, err
	}
	if err := os.Rename(part, path); err != nil {
		_ = os.Remove(part)
		return 0, fmt.Errorf("finish %s: %w", path, err)
	}
	return n, nil
}

// StreamTo downloads to w (stdout for --file -). The bytes are written
// verbatim — no decoration, no trailing newline — so binary payloads
// survive the pipe.
func (d *Downloader) StreamTo(ctx context.Context, w io.Writer) (int64, error) {
	body, err := d.fetch(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = body.Close() }()
	n, err := io.Copy(w, body)
	if err != nil {
		return n, wrapCopyError(ctx, err)
	}
	return n, nil
}

// CheckDestination refuses a destination that already exists (unless force)
// or is a directory (always — replacing a directory is never what --force
// means). A stat failure other than not-exist surfaces as is.
func CheckDestination(path string, force bool) error {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("check %s: %w", path, err)
	case info.IsDir():
		return fmt.Errorf("%w: %s — %s", ErrFileExists, path, msgDirectoryDest)
	case force:
		return nil
	default:
		return fmt.Errorf("%w: %s — %s", ErrFileExists, path, msgForceRemedy)
	}
}

// fetch mints a link and GETs it, retrying exactly once when the link host
// says the link expired: mint a fresh (re-verified) link and start over.
// Expiry is only trusted before any payload byte, so the retry can never
// duplicate output.
//
// TODO(upstream-contract): HTTP 410 is the pinned "this access link
// expired" answer from the link host. If the platform ever adds another
// expiry spelling, widen linkExpired in lockstep.
func (d *Downloader) fetch(ctx context.Context) (io.ReadCloser, error) {
	resp, err := d.get(ctx)
	if err != nil {
		return nil, err
	}
	if linkExpired(resp) {
		discard(resp)
		if resp, err = d.get(ctx); err != nil {
			return nil, err
		}
		if linkExpired(resp) {
			discard(resp)
			return nil, ErrLinkExpired
		}
	}
	if resp.StatusCode/100 != 2 {
		discard(resp)
		return nil, fmt.Errorf("%w: the link answered HTTP %d", ErrLinkFetch, resp.StatusCode)
	}
	return resp.Body, nil
}

// get performs one mint-then-GET round trip. Mint failures — resolve
// errors and CRID verification failures alike — propagate unwrapped so
// they keep their own exit codes.
func (d *Downloader) get(ctx context.Context) (*http.Response, error) {
	link, err := d.Mint(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build download request: %w", err)
	}
	if d.Client == nil {
		// Lazy-init once and keep it: the expiry retry must reach the same
		// transport the drained first response's connection was returned to,
		// or discard's drain-for-reuse buys nothing and the retry pays a
		// second dial.
		d.Client = NewHTTPClient()
	}
	return d.Client.Do(req)
}

func linkExpired(resp *http.Response) bool {
	return resp.StatusCode == http.StatusGone
}

// wrapCopyError classifies a mid-body failure: a canceled invocation keeps
// its context sentinel (Ctrl-C exits 130, deadlines exit as unavailable),
// anything else is the link host breaking off a download it accepted.
func wrapCopyError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("download canceled: %w", ctxErr)
	}
	return fmt.Errorf("%w: %w", ErrLinkFetch, err)
}

// drainLimit bounds how much of a discarded response body is read so the
// connection can be reused for the retry.
const drainLimit = 512 << 10

func discard(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainLimit))
	_ = resp.Body.Close()
}
