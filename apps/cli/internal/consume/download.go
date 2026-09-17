package consume

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
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

const maxDownloadRedirects = 10

// TODO(upstream-contract): grantSessionCookie mirrors the cookie name set by
// qurl-go ResourceHandle.AuthorizeContentRequest. Downloader's header snapshot
// is the full containment boundary; this name remains only for the independent
// defense in NewHTTPClient and its focused regression test.
const grantSessionCookie = "qurl_vsession"

const (
	grantPropagationWindow      = 2 * time.Second
	grantPropagationFirstDelay  = 100 * time.Millisecond
	grantPropagationMaxDelay    = 500 * time.Millisecond
	grantPropagationMaxAttempts = 5
)

var (
	errGrantPropagationPending       = errors.New("grant propagation remained pending")
	errDownloadRedirectLimit         = fmt.Errorf("download stopped after %d redirects", maxDownloadRedirects)
	errDownloadRedirectAuthorization = errors.New("download redirect authorization failed")
)

// DownloadTarget is a freshly verified URL and, for an acknowledged qURL
// access grant, the conservative instant through which that grant remains
// valid. A zero ValidUntil is a direct URL with no retained grant lifetime.
type DownloadTarget struct {
	URL        string
	ValidUntil time.Time
	// Authorize applies an opaque per-grant application credential to the exact
	// HTTPS request origin. A nil function is a direct URL with no extra
	// authority.
	Authorize func(*http.Request) error
}

// NewHTTPClient builds the download client: the default transport with a
// response-header bound and no overall timeout. It follows redirects, but
// removes known grant credentials before each redirect as defense in depth.
// Downloader provides the full credential-shape-independent boundary: it
// restores a pre-authorization header snapshot, then re-authorizes only an
// exact same-origin redirect. The qURL API key is never added.
func NewHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = responseHeaderTimeout
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if err := checkDownloadRedirectLimit(via); err != nil {
				return err
			}
			stripGrantCredentials(req)
			return nil
		},
	}
}

func checkDownloadRedirectLimit(via []*http.Request) error {
	if len(via) >= maxDownloadRedirects {
		return errDownloadRedirectLimit
	}
	return nil
}

func stripCookie(req *http.Request, name string) {
	if req == nil {
		return
	}
	cookies := req.Cookies()
	req.Header.Del("Cookie")
	for _, cookie := range cookies {
		if cookie.Name != name {
			req.AddCookie(cookie)
		}
	}
}

func stripGrantCredentials(req *http.Request) {
	if req == nil {
		return
	}
	stripCookie(req, grantSessionCookie)
	// The current SDK uses only the cookie. Remove standard credential headers
	// as defense in depth if that upstream contract changes before this mirror.
	req.Header.Del("Authorization")
	req.Header.Del("Proxy-Authorization")
}

// SameHTTPOrigin reports whether two URLs have the same normalized web origin.
// It fails closed for malformed URLs, user info, and non-HTTP schemes.
func SameHTTPOrigin(left, right *url.URL) bool {
	leftScheme, leftHost, leftPort, leftOK := normalizedHTTPOrigin(left)
	rightScheme, rightHost, rightPort, rightOK := normalizedHTTPOrigin(right)
	return leftOK && rightOK && leftScheme == rightScheme && leftHost == rightHost && leftPort == rightPort
}

func normalizedHTTPOrigin(u *url.URL) (scheme, host, port string, ok bool) {
	if u == nil || u.User != nil {
		return "", "", "", false
	}
	scheme = strings.ToLower(u.Scheme)
	host = strings.ToLower(u.Hostname())
	if host == "" || (scheme != webSchemeHTTP && scheme != webSchemeHTTPS) {
		return "", "", "", false
	}
	port = u.Port()
	if port == "" {
		if scheme == webSchemeHTTPS {
			port = "443"
		} else {
			port = "80"
		}
	} else {
		portNumber, err := strconv.ParseUint(port, 10, 16)
		if err != nil || portNumber == 0 {
			return "", "", "", false
		}
		port = strconv.FormatUint(portNumber, 10)
	}
	return scheme, host, port, true
}

// Downloader fetches the bytes behind freshly minted, already-verified
// links. It is single-use and not safe for concurrent use (the
// client-caching test reuses one instance only to assert the lazily built
// Client is retained — a white-box check, not a supported pattern).
type Downloader struct {
	// Client performs the GETs; nil means NewHTTPClient's default, built on
	// first use and kept on the Downloader so every request of one download
	// — the expiry retry included — shares one connection pool. A grant-bearing
	// target requires *http.Client so Downloader can enforce redirect containment.
	Client Doer
	// Mint shares a lifetime-free URL whose plain GET serves the content.
	Mint func(ctx context.Context) (string, error)
	// MintTarget is the grant-aware form of Mint. When present it takes
	// precedence and lets an immediate 410 retry the same acknowledged grant
	// instead of opening a second NHP session. Mint remains for direct callers
	// that do not have grant metadata.
	MintTarget func(ctx context.Context) (DownloadTarget, error)
	now        func() time.Time
	wait       func(context.Context, time.Duration) error
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

// fetch mints a fetchable URL and GETs it. An immediate 410 on a live,
// acknowledged grant gets a short bounded propagation retry against the same
// URL. Only a grant that has expired gets one fresh, re-verified grant. Expiry
// is trusted only before any payload byte, so a retry cannot duplicate output.
//
// TODO(upstream-contract): HTTP 410 is the pinned "this access link
// expired" answer from the link host. If the platform ever adds another
// expiry spelling, widen linkExpired in lockstep.
func (d *Downloader) fetch(ctx context.Context) (io.ReadCloser, error) {
	target, resp, err := d.getFresh(ctx)
	if err != nil {
		return nil, err
	}
	if linkExpired(resp) {
		discard(resp)
		if d.grantIsLive(target) {
			resp, err = d.retryLiveGrant(ctx, target)
			if err != nil && !errors.Is(err, errGrantPropagationPending) {
				return nil, err
			}
			if resp != nil && !linkExpired(resp) {
				return acceptedBody(resp)
			}
			if resp != nil {
				discard(resp)
			}
		}
		if target.ValidUntil.IsZero() || !d.grantIsLive(target) {
			_, resp, err = d.getFresh(ctx)
			if err != nil {
				return nil, err
			}
			if linkExpired(resp) {
				discard(resp)
				return nil, ErrLinkExpired
			}
			return acceptedBody(resp)
		}
		return nil, fmt.Errorf("%w: acknowledged access stayed unavailable", ErrLinkFetch)
	}
	return acceptedBody(resp)
}

func acceptedBody(resp *http.Response) (io.ReadCloser, error) {
	if resp.StatusCode/100 != 2 {
		discard(resp)
		return nil, fmt.Errorf("%w: the link answered HTTP %d", ErrLinkFetch, resp.StatusCode)
	}
	return resp.Body, nil
}

func (d *Downloader) retryLiveGrant(ctx context.Context, target DownloadTarget) (*http.Response, error) {
	deadline := d.currentTime().Add(grantPropagationWindow)
	if target.ValidUntil.Before(deadline) {
		deadline = target.ValidUntil
	}
	delay := grantPropagationFirstDelay
	for attempt := 0; attempt < grantPropagationMaxAttempts; attempt++ {
		remaining := deadline.Sub(d.currentTime())
		if remaining <= 0 {
			return nil, errGrantPropagationPending
		}
		if err := d.waitFor(ctx, min(delay, remaining)); err != nil {
			return nil, err
		}
		if !d.currentTime().Before(deadline) {
			return nil, errGrantPropagationPending
		}
		resp, err := d.getTarget(ctx, target)
		if err != nil {
			return nil, err
		}
		if !linkExpired(resp) {
			return resp, nil
		}
		discard(resp)
		delay = min(delay*2, grantPropagationMaxDelay)
	}
	return nil, errGrantPropagationPending
}

func (d *Downloader) grantIsLive(target DownloadTarget) bool {
	return !target.ValidUntil.IsZero() && d.currentTime().Before(target.ValidUntil)
}

func (d *Downloader) currentTime() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

func (d *Downloader) waitFor(ctx context.Context, delay time.Duration) error {
	if d.wait != nil {
		return d.wait(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// get performs one mint-then-GET round trip. Mint failures — share
// errors and CRID verification failures alike — propagate unwrapped so
// they keep their own exit codes.
func (d *Downloader) getFresh(ctx context.Context) (DownloadTarget, *http.Response, error) {
	target, err := d.mintTarget(ctx)
	if err != nil {
		return DownloadTarget{}, nil, err
	}
	resp, err := d.getTarget(ctx, target)
	return target, resp, err
}

func (d *Downloader) mintTarget(ctx context.Context) (DownloadTarget, error) {
	if d.MintTarget != nil {
		return d.MintTarget(ctx)
	}
	link, err := d.Mint(ctx)
	return DownloadTarget{URL: link}, err
}

func (d *Downloader) getTarget(ctx context.Context, target DownloadTarget) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL, http.NoBody)
	if err != nil {
		// URL parsing errors can echo link. Granted URLs carry short-lived
		// authority, so keep the same fixed capability boundary as Do below.
		return nil, fmt.Errorf("%w: invalid download link", ErrLinkFetch)
	}
	if d.Client == nil {
		// Lazy-init once and keep it: the expiry retry must reach the same
		// transport the drained first response's connection was returned to,
		// or discard's drain-for-reuse buys nothing and the retry pays a
		// second dial.
		d.Client = NewHTTPClient()
	}
	client := d.Client
	if target.Authorize != nil {
		httpClient, ok := client.(*http.Client)
		if !ok {
			return nil, fmt.Errorf("%w: could not secure the content request", ErrLinkFetch)
		}
		// net/http applies a Jar after CheckRedirect. A grant-bearing request
		// therefore cannot use one: a Jar could restore cookies after this code
		// removed the SDK credential from a cross-origin redirect.
		if httpClient.Jar != nil {
			return nil, fmt.Errorf("%w: could not secure the content request", ErrLinkFetch)
		}
		// Save the request headers before the opaque SDK authorizer runs. Redirects
		// start from this unprivileged snapshot, so containment does not depend on
		// the current credential name or header shape.
		unprivilegedHeaders := req.Header.Clone()
		if err := target.Authorize(req); err != nil {
			// The authorizer can hold an application bearer. Keep both its value and
			// its underlying diagnostic out of customer output.
			return nil, fmt.Errorf("%w: could not authorize the content request", ErrLinkFetch)
		}
		// Keep the shared transport and connection pool, but make redirect
		// authorization specific to this short-lived grant. Restore the headers
		// from before authorization, let the caller's redirect policy run without
		// that bearer, then re-authorize only the exact granted origin.
		// Cross-origin storage redirects still work.
		requestClient := *httpClient
		priorCheckRedirect := requestClient.CheckRedirect
		grantedURL := req.URL
		requestClient.CheckRedirect = func(redirectReq *http.Request, via []*http.Request) error {
			if err := checkDownloadRedirectLimit(via); err != nil {
				return err
			}
			// #nosec G119 -- this is the header snapshot captured before the
			// grant authorizer added any credential. This also discards the
			// automatically generated Referer. The cross-origin leak test covers
			// cookie, standard authorization, and unknown header shapes.
			redirectReq.Header = unprivilegedHeaders.Clone()
			if priorCheckRedirect != nil {
				if err := priorCheckRedirect(redirectReq, via); err != nil {
					return err
				}
			}
			if SameHTTPOrigin(grantedURL, redirectReq.URL) {
				if err := target.Authorize(redirectReq); err != nil {
					return errDownloadRedirectAuthorization
				}
			}
			return nil
		}
		client = &requestClient
	}
	resp, err := client.Do(req)
	if err == nil {
		return resp, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("download canceled: %w", ctxErr)
	}
	if errors.Is(err, errDownloadRedirectLimit) {
		return nil, fmt.Errorf("%w: %w", ErrLinkUnavailable, errDownloadRedirectLimit)
	}
	if errors.Is(err, errDownloadRedirectAuthorization) {
		return nil, fmt.Errorf("%w: could not authorize the content request", ErrLinkFetch)
	}
	// net/http transport errors normally include req.URL. A granted content
	// URL is short-lived authority, so never retain that error text or chain.
	return nil, fmt.Errorf("%w: request failed", ErrLinkUnavailable)
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
