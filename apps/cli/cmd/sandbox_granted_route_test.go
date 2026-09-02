//go:build clisandbox

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/cli/internal/consume"
)

const (
	sandboxDeletedRouteProbeWindow   = 60 * time.Second
	sandboxDeletedRouteProbeTimeout  = 2 * time.Second
	sandboxDeletedRouteProbePoll     = 500 * time.Millisecond
	sandboxDeletedRouteServeGrace    = 15 * time.Second
	sandboxDeletedRouteRequiredQuiet = 4 * time.Second
	sandboxDeletedRouteGrantMargin   = 5 * time.Second
	sandboxGrantedRouteReadyWindow   = 15 * time.Second
)

var (
	errSandboxGrantedRouteUnexpectedSuccess    = errors.New("granted route returned an unexpected successful response")
	errSandboxGrantedRouteMissingAuthorization = errors.New("granted route omitted application authorization")
	errSandboxGrantedRouteAuthorization        = errors.New("granted route authorization failed")
	errSandboxGrantedRouteConfiguration        = errors.New("granted route probe configuration is invalid")
	errSandboxGrantedRouteTransport            = errors.New("granted route transport failed")
)

type sandboxGrantedRouteProbeState uint8

const (
	sandboxGrantedRouteServed sandboxGrantedRouteProbeState = iota + 1
	sandboxGrantedRouteRefused
)

// sandboxGrantedRoute retains one working access path across delete. The URL
// carries access authority, so this type and every diagnostic keep it private.
type sandboxGrantedRoute struct {
	url         string
	marker      string
	backendHits func() uint64
	expiresAt   time.Time
	authorize   func(*http.Request) error
}

func prepareSandboxGrantedRoute(
	t *testing.T,
	env map[string]string,
	shareLink string,
	marker string,
	backendHits func() uint64,
) *sandboxGrantedRoute {
	t.Helper()
	link := strings.TrimSpace(shareLink)
	if link == "" || strings.ContainsAny(link, " \t\r\n") || backendHits == nil {
		t.Fatal("pre-delete Connector access input is incomplete")
	}
	lookupEnv := func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	grantStartedAt := time.Now()
	grant, err := (&consume.AccessOpener{LookupEnv: lookupEnv}).Grant(ctx, link)
	if err != nil {
		t.Fatalf(
			"pre-delete Connector access grant failed: %s; private details withheld",
			sandboxGrantedRouteAccessFailureCategory(err),
		)
	}
	route := &sandboxGrantedRoute{
		url:         grant.ContentURL,
		marker:      marker,
		backendHits: backendHits,
		expiresAt:   grantStartedAt.Add(time.Duration(grant.OpenSeconds) * time.Second),
		authorize:   grant.AuthorizeContentRequest,
	}
	readyCtx, cancelReady := context.WithTimeout(ctx, sandboxGrantedRouteReadyWindow)
	defer cancelReady()
	err = waitSandboxGrantedRouteReady(
		readyCtx,
		sandboxDeletedRouteProbePoll,
		sandboxDeletedRouteProbeTimeout,
		route.probe,
		backendHits,
	)
	if err != nil {
		t.Fatalf("%v; private details withheld", err)
	}
	return route
}

func sandboxGrantedRouteAccessFailureCategory(err error) string {
	switch {
	case errors.Is(err, consume.ErrAccessNotConfigured):
		return "deployment trust is not configured"
	case errors.Is(err, consume.ErrAccessSettingsMismatch):
		return "deployment trust does not match"
	case errors.Is(err, consume.ErrLinkVerification):
		return "access link verification failed"
	case errors.Is(err, consume.ErrAccessDenied):
		return "access grant was denied"
	case errors.Is(err, consume.ErrAccessBusy):
		return "access grant is busy"
	case errors.Is(err, consume.ErrUnopenableLink):
		return "granted content URL is invalid"
	default:
		return "unclassified access failure"
	}
}

type sandboxGrantedRouteProbe func(context.Context) (sandboxGrantedRouteProbeState, error)

func waitSandboxGrantedRouteReady(
	ctx context.Context,
	poll time.Duration,
	probeTimeout time.Duration,
	probe sandboxGrantedRouteProbe,
	backendHits func() uint64,
) error {
	if poll <= 0 || probeTimeout <= 0 || probe == nil || backendHits == nil {
		return errors.New("pre-delete granted Connector route readiness configuration is invalid")
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	lastCategory := "route was not sampled"
	for {
		before := backendHits()
		probeCtx, cancelProbe := context.WithTimeout(ctx, probeTimeout)
		state, probeErr := probe(probeCtx)
		cancelProbe()
		after := backendHits()
		if state == sandboxGrantedRouteServed && probeErr == nil && after > before {
			return nil
		}
		// These failures describe local probe construction. Waiting cannot repair
		// them, so retain the cause and fail before the readiness deadline.
		switch {
		case errors.Is(probeErr, errSandboxGrantedRouteMissingAuthorization):
			return fmt.Errorf("pre-delete granted Connector route is invalid: access grant omitted application authorization: %w", errSandboxGrantedRouteMissingAuthorization)
		case errors.Is(probeErr, errSandboxGrantedRouteConfiguration):
			return fmt.Errorf("pre-delete granted Connector route is invalid: probe configuration is invalid: %w", errSandboxGrantedRouteConfiguration)
		}
		switch {
		case errors.Is(probeErr, errSandboxGrantedRouteUnexpectedSuccess):
			lastCategory = "route returned non-matching successful bytes"
		case errors.Is(probeErr, errSandboxGrantedRouteAuthorization):
			lastCategory = "access grant authorization failed"
		case errors.Is(probeErr, context.DeadlineExceeded):
			lastCategory = "probe timed out"
		case probeErr != nil:
			lastCategory = "probe failed"
		case state == sandboxGrantedRouteRefused:
			lastCategory = "route refused access"
		case state == sandboxGrantedRouteServed:
			lastCategory = "matching response did not register at the local backend"
		default:
			lastCategory = "route did not return the local backend marker"
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("pre-delete granted Connector route did not become ready: %s: %w", lastCategory, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (r *sandboxGrantedRoute) probe(ctx context.Context) (sandboxGrantedRouteProbeState, error) {
	if r == nil {
		return 0, errSandboxGrantedRouteConfiguration
	}
	// Keep this redirect-policy implementation independent from Downloader's
	// grant-scoped client. The exact-origin predicate is shared intentionally
	// and pinned separately by consume's origin table tests.
	// This probe validates a qv2 Connector grant, not Downloader's direct-URL
	// mode. A missing authorizer means the protected route was not granted.
	if r.authorize == nil {
		return 0, errSandboxGrantedRouteMissingAuthorization
	}
	client := consume.NewHTTPClient()
	if transport, ok := client.Transport.(*http.Transport); ok {
		transport.DisableKeepAlives = true
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, http.NoBody)
	if err != nil {
		return 0, errors.New("build granted-route request")
	}
	unprivilegedHeaders := req.Header.Clone()
	if err := r.authorize(req); err != nil {
		return 0, errSandboxGrantedRouteAuthorization
	}
	grantedURL := req.URL
	priorCheckRedirect := client.CheckRedirect
	client.CheckRedirect = func(redirectReq *http.Request, via []*http.Request) error {
		// #nosec G119 -- this snapshot was captured before the grant
		// authorizer ran, so it cannot contain the application bearer.
		redirectReq.Header = unprivilegedHeaders.Clone()
		if priorCheckRedirect != nil {
			if err := priorCheckRedirect(redirectReq, via); err != nil {
				return err
			}
		}
		if !consume.SameHTTPOrigin(grantedURL, redirectReq.URL) {
			return nil
		}
		if err := r.authorize(redirectReq); err != nil {
			return errSandboxGrantedRouteAuthorization
		}
		return nil
	}
	req.Close = true
	resp, err := client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		if errors.Is(err, errSandboxGrantedRouteAuthorization) {
			return 0, errSandboxGrantedRouteAuthorization
		}
		return 0, errSandboxGrantedRouteTransport
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, int64(len(r.marker)+1)))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		return 0, errors.New("read granted-route response")
	}
	if resp.StatusCode/100 != 2 {
		return sandboxGrantedRouteRefused, nil
	}
	if string(payload) == r.marker {
		return sandboxGrantedRouteServed, nil
	}
	return 0, errSandboxGrantedRouteUnexpectedSuccess
}

func classifySandboxGrantedRouteProbe(
	state sandboxGrantedRouteProbeState,
	probeErr error,
	outerErr error,
) (sandboxGrantedRouteProbeState, bool, bool) {
	switch {
	case errors.Is(probeErr, errSandboxGrantedRouteUnexpectedSuccess):
		return 0, false, true
	case errors.Is(probeErr, context.DeadlineExceeded) && outerErr == nil:
		// A live outer bound plus an expired per-probe bound is an observed
		// blackhole. Deletion may deny with a response or silently drop.
		return sandboxGrantedRouteRefused, false, false
	default:
		return state, probeErr != nil, false
	}
}

func sandboxGrantedRouteSettlementAllowed(settled bool, outerErr error) bool {
	return settled && outerErr == nil
}

func validateSandboxGrantedRouteLifetime(now, expiresAt time.Time, required time.Duration) error {
	if now.IsZero() || expiresAt.IsZero() || required <= 0 {
		return errors.New("granted-route access lifetime check configuration is invalid")
	}
	if expiresAt.Sub(now) < required {
		return errors.New("pre-delete access grant lifetime does not cover the bounded deletion-fence check")
	}
	return nil
}

type sandboxGrantedRouteFenceValidator struct {
	settle      time.Duration
	stableSince time.Time
	stableHits  uint64
}

func (v *sandboxGrantedRouteFenceValidator) observe(
	probeStartedAt time.Time,
	probeCompletedAt time.Time,
	state sandboxGrantedRouteProbeState,
	probeFailed bool,
	hitsBefore uint64,
	hitsAfter uint64,
) (bool, error) {
	if v.settle <= 0 {
		return false, errors.New("granted-route fence timing is invalid")
	}
	if probeCompletedAt.Before(probeStartedAt) {
		return false, errors.New("granted-route probe completed before it started")
	}
	if hitsBefore != v.stableHits {
		return false, fmt.Errorf("deleted Connector reached the local backend between post-grace probes: hits advanced from %d to %d", v.stableHits, hitsBefore)
	}
	if hitsAfter != hitsBefore {
		return false, fmt.Errorf("deleted Connector reached the local backend during a post-grace probe: hits advanced from %d to %d", hitsBefore, hitsAfter)
	}
	if probeFailed {
		v.stableSince = time.Time{}
		return false, nil
	}
	switch state {
	case sandboxGrantedRouteServed:
		return false, errors.New("deleted Connector still served the unique local backend bytes after the convergence grace")
	case sandboxGrantedRouteRefused:
	default:
		return false, errors.New("granted-route probe returned an unknown state")
	}
	if v.stableSince.IsZero() {
		v.stableSince = probeCompletedAt
		return false, nil
	}
	return probeCompletedAt.Sub(v.stableSince) >= v.settle, nil
}

func assertSandboxGrantedRouteFenced(t *testing.T, route *sandboxGrantedRoute) {
	t.Helper()
	if route == nil || route.backendHits == nil || route.url == "" || route.marker == "" {
		t.Fatal("post-delete Connector route check is incomplete")
	}
	if err := validateSandboxGrantedRouteLifetime(
		time.Now(),
		route.expiresAt,
		sandboxDeletedRouteProbeWindow+sandboxDeletedRouteGrantMargin,
	); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), sandboxDeletedRouteProbeWindow)
	defer cancel()
	outerDeadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("post-delete Connector route check has no outer deadline")
	}
	graceTimer := time.NewTimer(sandboxDeletedRouteServeGrace)
	select {
	case <-ctx.Done():
		graceTimer.Stop()
		t.Fatalf("deleted Connector route convergence grace exceeded the %s bound", sandboxDeletedRouteProbeWindow)
	case <-graceTimer.C:
	}
	// No request is issued during the convergence grace. This avoids carrying
	// an in-grace request across the measurement boundary and makes every later
	// backend hit an unambiguous fence failure.
	baseline := route.backendHits()
	ticker := time.NewTicker(sandboxDeletedRouteProbePoll)
	defer ticker.Stop()
	validator := sandboxGrantedRouteFenceValidator{
		// Keep issuing access-link probes through both the base settlement and
		// final confirmation periods. A passive final sleep could miss a cached
		// successful response that never reaches the local backend.
		settle:     sandboxDeletedRouteRequiredQuiet,
		stableHits: baseline,
	}
	for {
		probeStartedAt := time.Now()
		hitsBefore := route.backendHits()
		probeCtx, cancelProbe := context.WithTimeout(ctx, sandboxDeletedRouteProbeTimeout)
		state, probeErr := route.probe(probeCtx)
		cancelProbe()
		probeCompletedAt := time.Now()
		outerErr := ctx.Err()
		if outerErr == nil && !probeCompletedAt.Before(outerDeadline) {
			outerErr = context.DeadlineExceeded
		}
		state, probeFailed, unexpectedSuccess := classifySandboxGrantedRouteProbe(state, probeErr, outerErr)
		if unexpectedSuccess {
			// Before deletion, a transient non-matching 2xx is a readiness state.
			// After the convergence grace, any 2xx on this protected content route
			// violates revocation, even when an edge-generated body is not the
			// unique local backend marker.
			t.Fatal("deleted Connector returned successful bytes that did not match the local backend")
		}
		hitsAfter := route.backendHits()
		settled, err := validator.observe(probeStartedAt, probeCompletedAt, state, probeFailed, hitsBefore, hitsAfter)
		if err != nil {
			t.Fatal(err)
		}
		if sandboxGrantedRouteSettlementAllowed(settled, outerErr) {
			return
		}
		if outerErr != nil {
			t.Fatalf("deleted Connector route did not stay fenced for %s within the %s bound", sandboxDeletedRouteRequiredQuiet, sandboxDeletedRouteProbeWindow)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("deleted Connector route did not stay fenced for %s within the %s bound", sandboxDeletedRouteRequiredQuiet, sandboxDeletedRouteProbeWindow)
		case <-ticker.C:
		}
	}
}

func TestSandboxGrantedRouteReadiness(t *testing.T) {
	t.Run("retries a non-matching successful response", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		calls := 0
		hits := uint64(0)
		err := waitSandboxGrantedRouteReady(
			ctx,
			time.Millisecond,
			50*time.Millisecond,
			func(context.Context) (sandboxGrantedRouteProbeState, error) {
				calls++
				if calls == 1 {
					return 0, errSandboxGrantedRouteUnexpectedSuccess
				}
				hits++
				return sandboxGrantedRouteServed, nil
			},
			func() uint64 { return hits },
		)
		if err != nil || calls != 2 {
			t.Fatalf("readiness retry = calls %d, error %v", calls, err)
		}
	})

	t.Run("bounds a permanent non-matching response without exposing it", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		const secret = "qv3.readiness-secret"
		err := waitSandboxGrantedRouteReady(
			ctx,
			time.Millisecond,
			5*time.Millisecond,
			func(context.Context) (sandboxGrantedRouteProbeState, error) {
				return 0, fmt.Errorf("%w: %s", errSandboxGrantedRouteUnexpectedSuccess, secret)
			},
			func() uint64 { return 0 },
		)
		if !errors.Is(err, context.DeadlineExceeded) ||
			!strings.Contains(err.Error(), "non-matching successful bytes") ||
			strings.Contains(err.Error(), secret) {
			t.Fatalf("bounded readiness error = %v", err)
		}
	})

	t.Run("classifies authorization failure", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		err := waitSandboxGrantedRouteReady(
			ctx,
			time.Millisecond,
			5*time.Millisecond,
			func(context.Context) (sandboxGrantedRouteProbeState, error) {
				return 0, errSandboxGrantedRouteAuthorization
			},
			func() uint64 { return 0 },
		)
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "access grant authorization failed") {
			t.Fatalf("authorization readiness error = %v", err)
		}
	})

	for name, test := range map[string]struct {
		probeErr error
		want     error
		category string
	}{
		"fails fast when authorization is missing": {
			probeErr: fmt.Errorf("%w: qv3.secret", errSandboxGrantedRouteMissingAuthorization),
			want:     errSandboxGrantedRouteMissingAuthorization,
			category: "access grant omitted application authorization",
		},
		"fails fast on invalid probe configuration": {
			probeErr: fmt.Errorf("%w: qv3.secret", errSandboxGrantedRouteConfiguration),
			want:     errSandboxGrantedRouteConfiguration,
			category: "probe configuration is invalid",
		},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			err := waitSandboxGrantedRouteReady(
				context.Background(),
				time.Hour,
				time.Second,
				func(context.Context) (sandboxGrantedRouteProbeState, error) {
					calls++
					return 0, test.probeErr
				},
				func() uint64 { return 0 },
			)
			if calls != 1 || !errors.Is(err, test.want) || !strings.Contains(err.Error(), test.category) ||
				errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "qv3.secret") {
				t.Fatalf("terminal readiness error = calls %d, error %v", calls, err)
			}
		})
	}

	if err := waitSandboxGrantedRouteReady(context.Background(), 0, time.Second, nil, nil); err == nil {
		t.Fatal("invalid readiness configuration was accepted")
	}
}

func TestSandboxGrantedRouteProbeAuthorizesEverySameOriginRequest(t *testing.T) {
	var authorizations atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if _, err := req.Cookie("qurl_vsession"); err != nil {
			t.Error("granted-route request did not carry its application bearer")
		}
		if req.URL.Path == "/" {
			http.Redirect(w, req, server.URL+"/content", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("marker"))
	}))
	t.Cleanup(server.Close)

	route := &sandboxGrantedRoute{
		url: server.URL, marker: "marker",
		authorize: func(req *http.Request) error {
			authorizations.Add(1)
			addSandboxTestGrantCookie(req)
			return nil
		},
	}
	state, err := route.probe(t.Context())
	if err != nil || state != sandboxGrantedRouteServed || authorizations.Load() != 2 {
		t.Fatalf("same-origin probe = state %d, error %v, authorizations %d", state, err, authorizations.Load())
	}
}

func TestSandboxGrantedRouteProbeAllowsCrossOriginWithoutSendingBearer(t *testing.T) {
	var destinationRequests atomic.Int32
	var leaked atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		destinationRequests.Add(1)
		if _, err := req.Cookie("qurl_vsession"); err == nil || req.Header.Get("X-QURL-Session") != "" {
			leaked.Store(true)
		}
		_, _ = w.Write([]byte("marker"))
	}))
	t.Cleanup(destination.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if _, err := req.Cookie("qurl_vsession"); err != nil {
			t.Error("origin request did not carry its application bearer")
		}
		http.Redirect(w, req, destination.URL, http.StatusFound)
	}))
	t.Cleanup(origin.Close)
	var authorizations atomic.Int32
	route := &sandboxGrantedRoute{
		url: origin.URL, marker: "marker",
		authorize: func(req *http.Request) error {
			authorizations.Add(1)
			addSandboxTestGrantCookie(req)
			req.Header.Set("X-QURL-Session", "opaque-test-token")
			return nil
		},
	}
	state, err := route.probe(t.Context())
	if state != sandboxGrantedRouteServed || err != nil || destinationRequests.Load() != 1 ||
		authorizations.Load() != 1 || leaked.Load() {
		t.Fatalf("cross-origin probe = state %d, error %v, destination requests %d, authorizations %d, leaked %t",
			state, err, destinationRequests.Load(), authorizations.Load(), leaked.Load())
	}
}

func TestSandboxGrantedRouteProbeRejectsMissingAuthorization(t *testing.T) {
	route := &sandboxGrantedRoute{url: "https://download.example", marker: "marker"}
	state, err := route.probe(t.Context())
	if state != 0 || !errors.Is(err, errSandboxGrantedRouteMissingAuthorization) {
		t.Fatalf("missing-authorization probe = state %d, error %v", state, err)
	}
	var nilRoute *sandboxGrantedRoute
	state, err = nilRoute.probe(t.Context())
	if state != 0 || !errors.Is(err, errSandboxGrantedRouteConfiguration) {
		t.Fatalf("nil-route probe = state %d, error %v", state, err)
	}
}

func addSandboxTestGrantCookie(req *http.Request) {
	req.AddCookie(&http.Cookie{
		Name: "qurl_vsession", Value: "opaque-test-token",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func TestSandboxGrantedRouteAccessFailureCategories(t *testing.T) {
	for name, test := range map[string]struct {
		err  error
		want string
	}{
		"not configured":    {err: consume.ErrAccessNotConfigured, want: "deployment trust is not configured"},
		"settings mismatch": {err: consume.ErrAccessSettingsMismatch, want: "deployment trust does not match"},
		"verification":      {err: consume.ErrLinkVerification, want: "access link verification failed"},
		"denied":            {err: consume.ErrAccessDenied, want: "access grant was denied"},
		"busy":              {err: consume.ErrAccessBusy, want: "access grant is busy"},
		"invalid grant URL": {err: consume.ErrUnopenableLink, want: "granted content URL is invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := sandboxGrantedRouteAccessFailureCategory(test.err); got != test.want {
				t.Fatalf("category = %q, want %q", got, test.want)
			}
		})
	}
	const secret = "https://access.invalid/#qv3.unknown-secret"
	if got := sandboxGrantedRouteAccessFailureCategory(errors.New(secret)); got != "unclassified access failure" || strings.Contains(got, secret) {
		t.Fatalf("unknown category = %q", got)
	}
}

func TestSandboxGrantedRouteFenceValidator(t *testing.T) {
	if minimum := sandboxDeletedRouteServeGrace + 2*sandboxDeletedRouteProbeTimeout + sandboxDeletedRouteRequiredQuiet + sandboxDeletedRouteProbePoll; minimum >= sandboxDeletedRouteProbeWindow {
		t.Fatalf("route-fence minimum budget %s is not below outer window %s", minimum, sandboxDeletedRouteProbeWindow)
	}
	base := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	t.Run("requires a sustained refusal", func(t *testing.T) {
		validator := sandboxGrantedRouteFenceValidator{settle: 2 * time.Second, stableHits: 4}
		first := base.Add(time.Second)
		if settled, err := validator.observe(first, first, sandboxGrantedRouteRefused, false, 4, 4); err != nil || settled {
			t.Fatalf("first refusal = settled %v, error %v", settled, err)
		}
		settledAt := first.Add(2 * time.Second)
		if settled, err := validator.observe(settledAt, settledAt, sandboxGrantedRouteRefused, false, 4, 4); err != nil || !settled {
			t.Fatalf("sustained refusal = settled %v, error %v", settled, err)
		}
	})

	t.Run("cancellation resets the quiet window", func(t *testing.T) {
		validator := sandboxGrantedRouteFenceValidator{settle: 2 * time.Second, stableHits: 4}
		first := base.Add(time.Second)
		if settled, err := validator.observe(first, first, sandboxGrantedRouteRefused, false, 4, 4); err != nil || settled {
			t.Fatalf("first refusal = settled %v, error %v", settled, err)
		}
		canceled := first.Add(3 * time.Second)
		if settled, err := validator.observe(canceled, canceled, 0, true, 4, 4); err != nil || settled || !validator.stableSince.IsZero() {
			t.Fatalf("canceled probe = settled %v, stable %s, error %v", settled, validator.stableSince, err)
		}
		if settled, err := validator.observe(canceled.Add(time.Second), canceled.Add(time.Second), sandboxGrantedRouteRefused, false, 4, 4); err != nil || settled {
			t.Fatalf("first refusal after cancellation = settled %v, error %v", settled, err)
		}
	})

	t.Run("transport failures never establish quiet", func(t *testing.T) {
		validator := sandboxGrantedRouteFenceValidator{settle: time.Second, stableHits: 4}
		for step := 0; step < 3; step++ {
			at := base.Add(time.Duration(step) * time.Second)
			state, failed, fatal := classifySandboxGrantedRouteProbe(0, errSandboxGrantedRouteTransport, nil)
			if fatal {
				t.Fatal("transport failure was classified as a fatal route response")
			}
			settled, err := validator.observe(at, at, state, failed, 4, 4)
			if err != nil || settled || !validator.stableSince.IsZero() {
				t.Fatalf("transport step %d = settled %v, stable %s, error %v", step, settled, validator.stableSince, err)
			}
		}
	})

	t.Run("rejects a cached success before required quiet completes", func(t *testing.T) {
		validator := sandboxGrantedRouteFenceValidator{
			settle:     sandboxDeletedRouteRequiredQuiet,
			stableHits: 4,
		}
		if settled, err := validator.observe(base, base, sandboxGrantedRouteRefused, false, 4, 4); err != nil || settled {
			t.Fatalf("initial refusal = settled %v, error %v", settled, err)
		}
		baseSettledAt := base.Add(sandboxDeletedRouteRequiredQuiet / 2)
		if settled, err := validator.observe(baseSettledAt, baseSettledAt, sandboxGrantedRouteRefused, false, 4, 4); err != nil || settled {
			t.Fatalf("base settlement = settled %v, error %v", settled, err)
		}
		cachedAt := baseSettledAt.Add(time.Second)
		if _, err := validator.observe(cachedAt, cachedAt, sandboxGrantedRouteServed, false, 4, 4); err == nil {
			t.Fatal("cached success during final confirmation was accepted")
		}
	})

	for name, observation := range map[string]struct {
		state      sandboxGrantedRouteProbeState
		hitsBefore uint64
		hitsAfter  uint64
	}{
		"served response":    {state: sandboxGrantedRouteServed, hitsBefore: 4, hitsAfter: 4},
		"hit between probes": {state: sandboxGrantedRouteRefused, hitsBefore: 5, hitsAfter: 5},
		"hit during probe":   {state: sandboxGrantedRouteRefused, hitsBefore: 4, hitsAfter: 5},
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			validator := sandboxGrantedRouteFenceValidator{settle: 2 * time.Second, stableHits: 4}
			if _, err := validator.observe(base, base, observation.state, false, observation.hitsBefore, observation.hitsAfter); err == nil {
				t.Fatal("post-grace access was accepted")
			}
		})
	}

	t.Run("distinguishes a blackhole from outer cancellation", func(t *testing.T) {
		probeState, failed, fatal := classifySandboxGrantedRouteProbe(0, context.DeadlineExceeded, nil)
		if probeState != sandboxGrantedRouteRefused || failed || fatal {
			t.Fatalf("live-outer blackhole classification = state %d, failed %v, fatal %v", probeState, failed, fatal)
		}
		probeState, failed, fatal = classifySandboxGrantedRouteProbe(0, context.DeadlineExceeded, context.DeadlineExceeded)
		if probeState != 0 || !failed || fatal {
			t.Fatalf("outer cancellation classification = state %d, failed %v, fatal %v", probeState, failed, fatal)
		}
		probeState, failed, fatal = classifySandboxGrantedRouteProbe(0, errSandboxGrantedRouteUnexpectedSuccess, nil)
		if probeState != 0 || failed || !fatal {
			t.Fatalf("unexpected-success classification = state %d, failed %v, fatal %v", probeState, failed, fatal)
		}
		if sandboxGrantedRouteSettlementAllowed(true, context.DeadlineExceeded) {
			t.Fatal("refusal settled at the expired outer bound")
		}
		if !sandboxGrantedRouteSettlementAllowed(true, nil) {
			t.Fatal("refusal did not settle while the outer bound was live")
		}
		probeState, failed, fatal = classifySandboxGrantedRouteProbe(0, errSandboxGrantedRouteTransport, nil)
		if probeState != 0 || !failed || fatal {
			t.Fatalf("transport-failure classification = state %d, failed %v, fatal %v", probeState, failed, fatal)
		}
	})

	if _, err := (&sandboxGrantedRouteFenceValidator{}).observe(base, base, sandboxGrantedRouteRefused, false, 0, 0); err == nil {
		t.Fatal("invalid fence timing was accepted")
	}
	if _, err := (&sandboxGrantedRouteFenceValidator{settle: time.Second}).observe(base.Add(time.Second), base, sandboxGrantedRouteRefused, false, 0, 0); err == nil {
		t.Fatal("reversed probe timing was accepted")
	}
}

func TestSandboxGrantedRouteLifetime(t *testing.T) {
	base := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	required := sandboxDeletedRouteProbeWindow + sandboxDeletedRouteGrantMargin
	if err := validateSandboxGrantedRouteLifetime(base, base.Add(required), required); err != nil {
		t.Fatalf("sufficient access lifetime = %v", err)
	}
	if err := validateSandboxGrantedRouteLifetime(base, base.Add(required-time.Nanosecond), required); err == nil {
		t.Fatal("short access lifetime was accepted")
	}
	if err := validateSandboxGrantedRouteLifetime(time.Time{}, time.Time{}, 0); err == nil {
		t.Fatal("invalid access lifetime configuration was accepted")
	}
}
