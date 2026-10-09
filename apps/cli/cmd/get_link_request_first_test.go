package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"
	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
	"github.com/layervai/qurl-integrations/apps/cli/internal/consume"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

// Tests for the order in which a device with an identity makes its two
// requests where the link request is offered and no share option is set: the
// link request first, and the share request only when no link is given. In
// one case the link request is then made once more: the first one had no
// answer when its short time limit ran out, and the share request said "not
// found". get_crid_link.go has the tables these tests pin.

// getFileMode returns get's file action: it runs piped, so stderr holds
// plain text that can be compared byte for byte.
func getFileMode(t *testing.T) shareMode {
	t.Helper()
	mode := getModes()[1]
	if mode.name != "get file" {
		t.Fatalf("second get mode is %q, want get file", mode.name)
	}
	return mode
}

// shareRequestAnswer is one answer of the share request.
type shareRequestAnswer struct {
	name    string
	prepare func(t *testing.T, srv *apitest.Server)
	// link says the share request gives a link, and notFound that it answers
	// "not found". Any other answer is a failure of the share request.
	link, notFound bool
}

// shareRequestAnswers returns the answers a share request can get: a link, the two
// forms of "not found", and failures that are the share request's own.
func shareRequestAnswers() []shareRequestAnswer {
	return []shareRequestAnswer{
		{name: "link", link: true, prepare: func(*testing.T, *apitest.Server) {}},
		{
			name: "not found", notFound: true,
			prepare: func(t *testing.T, srv *apitest.Server) { shareNotFoundTwice(t, srv) },
		},
		{
			name: "not found, the other code", notFound: true,
			prepare: func(t *testing.T, srv *apitest.Server) {
				srv.Script(http.MethodPost, shareRoute(srv), apitest.HandlerNotFound404(t, "not_found"))
			},
		},
		{name: "deleted, told to the owner", prepare: func(t *testing.T, srv *apitest.Server) {
			srv.Script(http.MethodPost, shareRoute(srv), apitest.HandlerRevoked400(t))
		}},
		{name: "links not served", prepare: func(t *testing.T, srv *apitest.Server) {
			srv.Script(http.MethodPost, shareRoute(srv), apitest.HandlerDark503(t))
		}},
		{name: "account frozen", prepare: func(t *testing.T, srv *apitest.Server) {
			srv.Script(http.MethodPost, shareRoute(srv), apitest.HandlerAccountFrozen403(t))
		}},
		{name: "device credential rejected", prepare: func(t *testing.T, srv *apitest.Server) {
			srv.Script(http.MethodGet, "/v1/me", apitest.HandlerAPIKeyInvalid401(t))
		}},
	}
}

// linkRequestAnswer is one answer of the link request, in a class of the second
// table of get_crid_link.go.
type linkRequestAnswer struct {
	name string
	// link says the request is answered with a link.
	link bool
	// err is the SDK's answer otherwise. nil is the answer that has neither
	// a link nor an error.
	err error
	// notSent says the SDK will not ask for the CRID and sent nothing.
	notSent bool
	// interrupted says the user interrupted the command.
	interrupted bool
	// wantCode and golden are the result when the share request then says
	// "not found": the link request's own answer, as the user is told it.
	wantCode int
	golden   string
}

// linkRequestAnswers returns every answer of the link request: a link, every answer
// of refusalRows, the two answers for a CRID the SDK will not ask for, and an
// interrupt.
func linkRequestAnswers() []linkRequestAnswer {
	answers := []linkRequestAnswer{{name: "link", link: true}}
	for _, row := range refusalRows() {
		answers = append(answers, linkRequestAnswer{name: row.name, err: row.err, wantCode: row.wantCode, golden: row.golden})
	}
	return append(answers,
		linkRequestAnswer{name: "not sent, CRID version this client cannot check", err: errCRIDVersionTheSDKCannotCheck, notSent: true},
		linkRequestAnswer{name: "not sent, CRID the SDK calls invalid", err: errCRIDTheSDKCallsInvalid, notSent: true},
		linkRequestAnswer{name: "interrupted", err: errCRIDLinkInterrupted, interrupted: true},
	)
}

// TestGetLinkRequestFirstAnswerPairs is the second table of
// get_crid_link.go, one row for every pair of an answer to the link request
// and an answer to the share request, for a device that can read its key and
// for one that cannot.
//
// Each pair is checked in two ways.
//
// First against what the table says: a link from the link request is used
// and no share request is sent; an interrupt stops the command; a link from
// the share request is used; on a share "not found" the result is the link
// request's own answer, or the share request's "not found" when nothing was
// sent for a link.
//
// Second against the result get gave before the link request came first.
// That earlier order is still in the command: a share option keeps it. So
// the same two answers are given to a second run with --session-duration,
// and where the link request gives no link, the two runs must tell the user
// the same thing, byte for byte, with the same exit code and the same
// content. Only the order of the two requests may differ.
//
// Every link request here is answered at once, so its short time limit
// never runs out, and it is made exactly once. That holds for the row "timed
// out" too: there the SDK reports a timeout while the limit still has time
// left. TestGetMakesTheLinkRequestOnceMoreAfterItsShortLimitRanOut has the
// runs in which the limit does run out.
func TestGetLinkRequestFirstAnswerPairs(t *testing.T) {
	state := bootstrapRegisteredState(t)
	mode := getFileMode(t)

	// outcome is what one run told the user and did.
	type outcome struct {
		code      int
		stderr    string
		delivered string
		shares    int
		asks      int
		asDevice  int
		api       []string
	}
	// runPair runs get once with the two answers. key is the read of the
	// device key; flags selects the order.
	runPair := func(t *testing.T, link linkRequestAnswer, share shareRequestAnswer, read func(context.Context) ([]byte, connectorstate.NoDeviceKey), flags ...string) outcome {
		t.Helper()
		srv := downloadServer(t)
		share.prepare(t, srv)
		requests := &linkRequests{err: link.err}
		if link.link {
			requests = &linkRequests{link: issuedLink(srv.URL + apitest.DownloadPath)}
		}
		configure := withDeviceKey(withLinkRequestsOf(withArgs(enrolledDevice(t, state), flags...), requests), read)
		run := runShareMode(t, srv, srv.URL, mode, configure)
		got := outcome{
			// The message for a saved file names the file, and each run saves
			// to a place of its own. A fixed word stands for it, so two runs
			// can be compared.
			code: run.result.code, stderr: strings.ReplaceAll(run.result.stderr.String(), run.dest, "<file>"),
			shares: len(shareRequests(srv)), asks: len(requests.asked), asDevice: requests.askedAsDevice(), api: apiRequests(srv),
		}
		if got.code == exitcode.Success {
			got.delivered = mode.delivered(t, run)
		} else {
			run.mustNotHaveActed(t)
		}
		return got
	}

	for _, link := range linkRequestAnswers() {
		for _, share := range shareRequestAnswers() {
			// The result of the earlier order for the same two answers. It is
			// not a subtest of its own, so each subtest below has it when it
			// runs alone.
			before := runPair(t, link, share, mustNotReadTheDeviceKey(t), "--session-duration", "5m")

			for _, readable := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/share %s/key readable=%t", link.name, share.name, readable), func(t *testing.T) {
					keyReads := noDeviceKey(connectorstate.NoDeviceKeyUnreadable)
					if readable {
						keyReads = deviceKeyOf(t, state)
					}
					got := runPair(t, link, share, keyReads.read)

					// The link request: first, once, in the form the key allows.
					wantAsDevice := 0
					if readable {
						wantAsDevice = 1
					}
					if got.asks != 1 || got.asDevice != wantAsDevice || len(keyReads.given) != 1 {
						t.Errorf("made the link request %d times, %d of them as this device, and read the key %d times; want 1, %d and 1",
							got.asks, got.asDevice, len(keyReads.given), wantAsDevice)
					}

					// What the table says.
					switch {
					case link.link:
						if got.code != exitcode.Success || got.delivered != apitest.DefaultDownloadPayload || len(got.api) != 0 {
							t.Fatalf("exit = %d, delivered %q, qURL API requests %q; want the content of the link from the link request and no request to the API\nstderr: %s",
								got.code, got.delivered, got.api, got.stderr)
						}
						return
					case link.interrupted:
						if got.code != exitcode.Interrupted || got.stderr != "" || len(got.api) != 0 {
							t.Fatalf("exit = %d, stderr = %q, qURL API requests %q; want %d, nothing printed and no request to the API",
								got.code, got.stderr, got.api, exitcode.Interrupted)
						}
						return
					case share.link:
						if got.code != exitcode.Success || got.delivered != apitest.DefaultDownloadPayload || got.shares != 1 {
							t.Fatalf("exit = %d, delivered %q after %d share request(s); want the content of the share link after one share request\nstderr: %s",
								got.code, got.delivered, got.shares, got.stderr)
						}
					case share.notFound && link.notSent:
						if want := goldenBytes(t, "error_share_notfound.plain.stderr.golden"); got.code != exitcode.NotFound || got.stderr != want {
							t.Fatalf("exit = %d, stderr = %q; want %d and the share request's own not-found %q", got.code, got.stderr, exitcode.NotFound, want)
						}
					case share.notFound:
						if want := goldenBytes(t, link.golden+".plain.stderr.golden"); got.code != link.wantCode || got.stderr != want {
							t.Fatalf("exit = %d, stderr = %q; want %d and the link request's own answer %q", got.code, got.stderr, link.wantCode, want)
						}
					default:
						// A failure of the share request. What it is exactly is
						// compared with the earlier order below.
						if got.code == exitcode.Success || got.code == exitcode.Interrupted {
							t.Fatalf("exit = %d, want the share request's failure; stderr: %s", got.code, got.stderr)
						}
					}

					// What get gave before, for the same two answers.
					if got.code != before.code || got.stderr != before.stderr || got.delivered != before.delivered {
						t.Errorf("the link request first gave\nexit %d, delivered %q, stderr %q\nwant what the share request first gave:\nexit %d, delivered %q, stderr %q",
							got.code, got.delivered, got.stderr, before.code, before.delivered, before.stderr)
					}
					if got.shares != before.shares {
						t.Errorf("the share request was sent %d times, want %d as in the earlier order", got.shares, before.shares)
					}
				})
			}
		}
	}
}

// TestGetRefreshIsDecidedAgainAfterALinkFromTheLinkRequest pins the second
// row of the refresh table in get_crid_link.go. The first link of a download
// came from the link request, and the share request was not asked for it.
// The link expires before any byte is served, and the download asks again.
//
// The refresh is decided again by the first table: the link request first,
// and the share request only if it gives no link. The share request can
// still help here, because it was never asked. So a link request that gets
// no link for a moment, for example "too many requests", does not end a
// download that the share request can serve.
//
// The device key is read again for the refresh, and wiped again.
func TestGetRefreshIsDecidedAgainAfterALinkFromTheLinkRequest(t *testing.T) {
	state := bootstrapRegisteredState(t)
	rateLimited := sdkRefusal(qurl.ErrCRIDLinkRateLimited, "52603")
	downloads := func() []shareMode {
		var modes []shareMode
		for _, mode := range getModes() {
			if mode.downloads {
				modes = append(modes, mode)
			}
		}
		return modes
	}
	if len(downloads()) == 0 {
		t.Fatal("no get mode downloads; this test would pin nothing")
	}

	for _, tc := range []struct {
		name string
		// answers are the answers to the link requests, in order: true is a
		// link, false is "too many requests".
		answers []bool
		// prepare scripts the share route.
		prepare func(t *testing.T, srv *apitest.Server)
		// wantCode is the exit code; golden, if set, the stderr the output
		// must end with.
		wantCode   int
		golden     string
		wantShares int
	}{
		{
			// Both links from the link request. No share request at all.
			name: "the link request gives the second link too", answers: []bool{true, true},
			prepare: func(*testing.T, *apitest.Server) {}, wantCode: exitcode.Success,
		},
		{
			// The link request is refused at the refresh. The share request
			// was never asked, so it is asked now, and it gives the link.
			name: "refused at the refresh, the share request gives the link", answers: []bool{true, false},
			prepare: func(*testing.T, *apitest.Server) {}, wantCode: exitcode.Success, wantShares: 1,
		},
		{
			name: "refused at the refresh, the share request says not found", answers: []bool{true, false},
			prepare:  func(t *testing.T, srv *apitest.Server) { shareNotFoundTwice(t, srv) },
			wantCode: exitcode.RateLimited, golden: "error_get_crid_ratelimited", wantShares: 1,
		},
		{
			name: "refused at the refresh, the share request fails", answers: []bool{true, false},
			prepare: func(t *testing.T, srv *apitest.Server) {
				srv.ScriptRepeat(http.MethodPost, shareRoute(srv), 3, apitest.HandlerAccountFrozen403(t))
			},
			wantCode: exitcode.Forbidden, wantShares: 1,
		},
		{
			// The other way round: the first link from the share request,
			// the second from the link request.
			name: "refused first, then the link request gives the second link", answers: []bool{false, true},
			prepare: func(*testing.T, *apitest.Server) {}, wantCode: exitcode.Success, wantShares: 1,
		},
	} {
		for _, readable := range []bool{true, false} {
			for _, mode := range downloads() {
				t.Run(fmt.Sprintf("%s/key readable=%t/%s", tc.name, readable, mode.name), func(t *testing.T) {
					srv := downloadServer(t)
					srv.Script(http.MethodGet, apitest.DownloadPath, handlerGone)
					tc.prepare(t, srv)
					requests := &linkRequests{}
					next := func() (*qurl.CRIDLink, error) {
						if n := len(requests.asked); n > len(tc.answers) || !tc.answers[n-1] {
							return nil, rateLimited
						}
						return issuedLink(srv.URL + apitest.DownloadPath), nil
					}
					keyReads := noDeviceKey(connectorstate.NoDeviceKeyUnreadable)
					if readable {
						keyReads = deviceKeyOf(t, state)
					}
					offer := &linkOffer{offered: true}
					configure := func(args []string) *runOpts {
						opts := withDeviceKey(withLinkOffer(enrolledDevice(t, state), offer.answer), keyReads.read)(args)
						opts.requestCRIDLink = func(ctx context.Context, resourceCRID string) (*qurl.CRIDLink, error) {
							_, _ = requests.answer(ctx, resourceCRID)
							return next()
						}
						opts.requestCRIDLinkAsDevice = func(ctx context.Context, key []byte, resourceCRID string) (*qurl.CRIDLink, error) {
							_, _ = requests.answerAsDevice(ctx, key, resourceCRID)
							return next()
						}
						return opts
					}

					run := runShareMode(t, srv, srv.URL, mode, configure)
					stderr := run.result.stderr.String()
					if run.result.code != tc.wantCode {
						t.Fatalf("exit = %d, want %d; stderr: %s", run.result.code, tc.wantCode, stderr)
					}
					if tc.wantCode == exitcode.Success {
						run.mustHaveDelivered(t, mode)
						if strings.Count(stderr, "UNVERIFIED publisher") != 1 {
							t.Errorf("stderr = %q, want the publisher notice once", stderr)
						}
					} else {
						run.mustNotHaveActed(t)
					}
					if tc.golden != "" {
						if want := goldenBytes(t, tc.golden+".plain.stderr.golden"); !strings.HasSuffix(stderr, want) {
							t.Errorf("stderr = %q, want it to end with the golden %q", stderr, want)
						}
					}

					// Each of the two links was decided by the first table: the
					// offer was looked at twice, the link request was made
					// twice, and the key was read twice and wiped twice.
					wantAsDevice := 0
					if readable {
						wantAsDevice = 2
					}
					if len(requests.asked) != 2 || requests.askedAsDevice() != wantAsDevice || offer.checks != 2 || len(keyReads.given) != 2 {
						t.Errorf("made the link request %d times, %d of them as this device, asked whether it is offered %d times and read the key %d times; want 2, %d, 2 and 2",
							len(requests.asked), requests.askedAsDevice(), offer.checks, len(keyReads.given), wantAsDevice)
					}
					for i, given := range keyReads.given {
						if given != nil && !bytes.Equal(given, make([]byte, len(given))) {
							t.Errorf("the device key of read %d was not wiped", i+1)
						}
					}
					if got := len(shareRequests(srv)); got != tc.wantShares {
						t.Errorf("the share request was sent %d times, want %d", got, tc.wantShares)
					}
					if strings.Contains(stderr, msgSessionDurationNotApplied) {
						t.Errorf("stderr = %q, must not carry the session-duration note: the flag was not given", stderr)
					}
				})
			}
		}
	}
}

// requestContexts records, for each link request, whether its context had a
// time limit and how far away that limit was.
type requestContexts struct {
	bounded []bool
	limits  []time.Duration
}

func (c *requestContexts) record(ctx context.Context) {
	deadline, bounded := ctx.Deadline()
	c.bounded = append(c.bounded, bounded)
	c.limits = append(c.limits, time.Until(deadline))
}

// TestGetGivesTheLinkRequestBeforeAShareRequestAShorterTimeLimit pins the two
// time limits. A link request that is followed by a share request when it
// gives no link is bounded by cridLinkTimeoutBeforeShare, in both of its
// forms: a relay that does not answer must not hold back the share request
// for long. A link request whose answer is final has no limit from this
// file; internal/consume gives it the longer one.
//
// The read of the device key comes before the short limit starts, with the
// context of the command.
func TestGetGivesTheLinkRequestBeforeAShareRequestAShorterTimeLimit(t *testing.T) {
	state := bootstrapRegisteredState(t)
	mode := getFileMode(t)
	run := func(t *testing.T, machine func(args []string) *runOpts) (*requestContexts, *shareRun) {
		t.Helper()
		srv := downloadServer(t)
		shareNotFoundTwice(t, srv)
		contexts := &requestContexts{}
		notFound := sdkRefusal(qurl.ErrCRIDLinkNotFound, "52602")
		configure := func(args []string) *runOpts {
			opts := machine(args)
			opts.requestCRIDLink = func(ctx context.Context, _ string) (*qurl.CRIDLink, error) {
				contexts.record(ctx)
				return nil, notFound
			}
			opts.requestCRIDLinkAsDevice = func(ctx context.Context, _ []byte, _ string) (*qurl.CRIDLink, error) {
				contexts.record(ctx)
				return nil, notFound
			}
			return opts
		}
		return contexts, runShareMode(t, srv, srv.URL, mode, configure)
	}

	for name, keyReads := range map[string]*deviceKeyReads{
		"as this device":      deviceKeyOf(t, state),
		"with the CRID alone": noDeviceKey(connectorstate.NoDeviceKeyUnreadable),
	} {
		t.Run("before a share request/"+name, func(t *testing.T) {
			contexts, _ := run(t, withDeviceKey(enrolledDevice(t, state), keyReads.read))
			if len(contexts.bounded) != 1 || !contexts.bounded[0] || contexts.limits[0] <= 0 || contexts.limits[0] > cridLinkTimeoutBeforeShare {
				t.Fatalf("link request contexts: limits set %v, limits %v; want one request with a limit of at most %s", contexts.bounded, contexts.limits, cridLinkTimeoutBeforeShare)
			}
			// No test set another limit here, so this is the production one.
			// The request sees most of it: half is a wide margin for a slow
			// machine.
			if contexts.limits[0] < cridLinkTimeoutBeforeShare/2 {
				t.Errorf("the link request had %s left of its limit, want most of %s", contexts.limits[0], cridLinkTimeoutBeforeShare)
			}
			if len(keyReads.bounded) != 1 || keyReads.bounded[0] {
				t.Errorf("the device key was read %d times, with a time limit: %v; want one read with the context of the command, which has none here",
					len(keyReads.bounded), keyReads.bounded)
			}
		})
	}

	stateDir := filepath.Join(t.TempDir(), "no-device-state")
	for name, machine := range map[string]func(args []string) *runOpts{
		"machine with no identity":               machineWithNoIdentity(t, stateDir),
		"after the share request said not found": withArgs(enrolledDevice(t, state), "--session-duration", "5m"),
	} {
		t.Run("final answer/"+name, func(t *testing.T) {
			contexts, _ := run(t, machine)
			if len(contexts.bounded) != 1 || contexts.bounded[0] {
				t.Fatalf("link request contexts: limits set %v, limits %v; want one request with no limit from the command", contexts.bounded, contexts.limits)
			}
		})
	}

	if cridLinkTimeoutBeforeShare != 10*time.Second {
		t.Errorf("cridLinkTimeoutBeforeShare = %s, want 10s", cridLinkTimeoutBeforeShare)
	}
}

// testShortLimit stands in for cridLinkTimeoutBeforeShare in the tests of a
// link request that gets no answer in time. Such a request waits until its
// context ends, so this value only says how long each of them waits. It does
// not decide a result.
const testShortLimit = 5 * time.Millisecond

// noAnswerUntilTheContextEnds is the link request to a relay that does not
// answer. It waits until the context of the request ends, and then returns
// the error the SDK builds for that: the context's own error, beside the
// error of the transport. It sets no time itself. The request ends when the
// command's limit for it runs out, or when the command's own context ends.
//
// A context that never ends would hold the test for ever. So after one
// minute this function fails the test.
func noAnswerUntilTheContextEnds(ctx context.Context, t *testing.T) error {
	t.Helper()
	select {
	case <-ctx.Done():
		return fmt.Errorf("qurl: CRID link request did not complete: %w: %w", ctx.Err(), &qurl.RelayError{Msg: "relay POST https://endpoint.example.test/x failed"})
	case <-time.After(time.Minute):
		t.Error("the context of the link request did not end within one minute")
		return errors.New("the context of the link request did not end")
	}
}

// slowRelay is the injected link request, in both of its forms, for a relay
// that is slow. The first request gets no answer until its context ends.
// Every later request is answered at once with the answer in requests.
//
// It records what each request looked like: requests has the form and the
// key, contexts has the time limit, and apiSeen has what the qURL API had
// seen when the request was made.
type slowRelay struct {
	t        *testing.T
	srv      *apitest.Server
	requests *linkRequests
	contexts requestContexts
	apiSeen  [][]string
}

func (r *slowRelay) next(ctx context.Context) (*qurl.CRIDLink, error) {
	r.contexts.record(ctx)
	r.apiSeen = append(r.apiSeen, apiRequests(r.srv))
	if len(r.requests.asked) == 1 {
		return nil, noAnswerUntilTheContextEnds(ctx, r.t)
	}
	return r.requests.result()
}

// wire sets both forms of the link request of an invocation to r, and gives
// the link request that comes before a share request the limit
// testShortLimit.
func (r *slowRelay) wire(configure func(args []string) *runOpts) func(args []string) *runOpts {
	return func(args []string) *runOpts {
		opts := configure(args)
		opts.linkTimeoutBeforeShare = testShortLimit
		opts.requestCRIDLink = func(ctx context.Context, resourceCRID string) (*qurl.CRIDLink, error) {
			_, _ = r.requests.answer(ctx, resourceCRID)
			return r.next(ctx)
		}
		opts.requestCRIDLinkAsDevice = func(ctx context.Context, key []byte, resourceCRID string) (*qurl.CRIDLink, error) {
			_, _ = r.requests.answerAsDevice(ctx, key, resourceCRID)
			return r.next(ctx)
		}
		return opts
	}
}

// mustHaveAsked checks the link requests of one run. The first was made in
// the form the device key allows, before anything was sent to the qURL API,
// and with the short limit. When onceMore is set there was exactly one more:
// with the CRID alone, after the share request, and with no time limit from
// the command, so internal/consume gives it the long one. Otherwise there
// was no second request.
func (r *slowRelay) mustHaveAsked(readable, onceMore bool) {
	r.t.Helper()
	asked, asDevice, contexts := r.requests.asked, r.requests.asDevice, &r.contexts
	wantAsked := 1
	if onceMore {
		wantAsked = 2
	}
	if len(asked) != wantAsked {
		r.t.Fatalf("made the link request %d times, want %d", len(asked), wantAsked)
	}
	if asDevice[0] != readable || !contexts.bounded[0] || contexts.limits[0] > testShortLimit || len(r.apiSeen[0]) != 0 {
		r.t.Errorf("the first link request: as this device = %t, time limit set = %t (%s), made after %q; want as this device = %t, a limit of at most %s, and nothing sent to the qURL API before it",
			asDevice[0], contexts.bounded[0], contexts.limits[0], r.apiSeen[0], readable, testShortLimit)
	}
	if !onceMore {
		return
	}
	wantSeen := []string{"GET /v1/me", "POST " + shareRoute(r.srv)}
	if asDevice[1] || contexts.bounded[1] || strings.Join(r.apiSeen[1], "\n") != strings.Join(wantSeen, "\n") {
		r.t.Errorf("the link request made once more: as this device = %t, time limit from the command = %t, made after %q; want the CRID alone, no limit from the command, and after the share request: %q",
			asDevice[1], contexts.bounded[1], r.apiSeen[1], wantSeen)
	}
}

// mustHaveReadTheKeyOnce checks that the device key was read once in the
// run, and that the bytes the read handed out were wiped.
func mustHaveReadTheKeyOnce(t *testing.T, keyReads *deviceKeyReads) {
	t.Helper()
	if len(keyReads.given) != 1 {
		t.Errorf("the device key was read %d times, want once", len(keyReads.given))
	}
	for i, given := range keyReads.given {
		if given != nil && !bytes.Equal(given, make([]byte, len(given))) {
			t.Errorf("the device key of read %d was not wiped", i+1)
		}
	}
}

// TestGetMakesTheLinkRequestOnceMoreAfterItsShortLimitRanOut pins the two
// rows "the short time limit ran out" of the second table in
// get_crid_link.go, and the text below that table.
//
// The first link request of every run gets no answer: it waits until its
// short limit runs out. The run goes on to the share request. Every answer
// of the share request is tried with every answer the link request can get
// when it is made once more, for a device that can read its key and for one
// that cannot.
//
//   - The share request gives a link, or fails in its own way. That is the
//     result, and the link request is not made once more.
//   - The share request says "not found". The link request is made once
//     more: after the share request, with the CRID alone, and with no time
//     limit from the command. Its answer is the result. No third request is
//     made, and the device key is not read again.
//
// Each result is also compared with the order where the share request comes
// first, which a share option keeps. That order makes its one link request
// with the long limit, so a relay that is slow costs it nothing: the answers
// here are the ones it gets. For the same answers the two orders must tell
// the user the same thing, byte for byte. Without the request made once
// more, this order said "the service did not answer" where that order gave
// a link.
func TestGetMakesTheLinkRequestOnceMoreAfterItsShortLimitRanOut(t *testing.T) {
	state := bootstrapRegisteredState(t)
	mode := getFileMode(t)

	// outcome is what one run told the user and did.
	type outcome struct {
		code      int
		stderr    string
		delivered string
		shares    int
	}
	outcomeOf := func(t *testing.T, run *shareRun, srv *apitest.Server) outcome {
		t.Helper()
		got := outcome{
			code: run.result.code, stderr: strings.ReplaceAll(run.result.stderr.String(), run.dest, "<file>"),
			shares: len(shareRequests(srv)),
		}
		if got.code == exitcode.Success {
			got.delivered = mode.delivered(t, run)
		} else {
			run.mustNotHaveActed(t)
		}
		return got
	}
	// answered returns the injected link request that gets answer at once.
	answered := func(srv *apitest.Server, answer linkRequestAnswer) *linkRequests {
		if answer.link {
			return &linkRequests{link: issuedLink(srv.URL + apitest.DownloadPath)}
		}
		return &linkRequests{err: answer.err}
	}

	for _, again := range linkRequestAnswers() {
		for _, share := range shareRequestAnswers() {
			// The result of the order where the share request comes first, for
			// the same answers. It is not a subtest of its own, so each subtest
			// below has it when it runs alone.
			beforeSrv := downloadServer(t)
			share.prepare(t, beforeSrv)
			earlierOrder := withDeviceKey(withLinkRequestsOf(withArgs(enrolledDevice(t, state), "--session-duration", "5m"), answered(beforeSrv, again)), mustNotReadTheDeviceKey(t))
			before := outcomeOf(t, runShareMode(t, beforeSrv, beforeSrv.URL, mode, earlierOrder), beforeSrv)

			for _, readable := range []bool{true, false} {
				t.Run(fmt.Sprintf("once more %s/share %s/key readable=%t", again.name, share.name, readable), func(t *testing.T) {
					srv := downloadServer(t)
					share.prepare(t, srv)
					keyReads := noDeviceKey(connectorstate.NoDeviceKeyUnreadable)
					if readable {
						keyReads = deviceKeyOf(t, state)
					}
					relay := &slowRelay{t: t, srv: srv, requests: answered(srv, again)}
					got := outcomeOf(t, runShareMode(t, srv, srv.URL, mode, relay.wire(withDeviceKey(enrolledDevice(t, state), keyReads.read))), srv)

					// The link request is made once more only after a share "not
					// found".
					relay.mustHaveAsked(readable, share.notFound)
					mustHaveReadTheKeyOnce(t, keyReads)

					// What the text below the table says.
					fromLinkRequest := false
					switch {
					case share.link:
						if got.code != exitcode.Success || got.delivered != apitest.DefaultDownloadPayload || got.shares != 1 {
							t.Fatalf("exit = %d, delivered %q after %d share request(s); want the content of the share link after one share request\nstderr: %s",
								got.code, got.delivered, got.shares, got.stderr)
						}
					case !share.notFound:
						// A failure of the share request. What it is exactly is
						// compared with the earlier order below.
						if got.code == exitcode.Success || got.code == exitcode.Interrupted {
							t.Fatalf("exit = %d, want the share request's failure; stderr: %s", got.code, got.stderr)
						}
					case again.link:
						fromLinkRequest = true
						if got.code != exitcode.Success || got.delivered != apitest.DefaultDownloadPayload || got.shares != 1 {
							t.Fatalf("exit = %d, delivered %q after %d share request(s); want the content of the link from the request made once more, after one share request\nstderr: %s",
								got.code, got.delivered, got.shares, got.stderr)
						}
					case again.interrupted:
						if got.code != exitcode.Interrupted || got.stderr != "" {
							t.Fatalf("exit = %d, stderr = %q; want %d and nothing printed", got.code, got.stderr, exitcode.Interrupted)
						}
					case again.notSent:
						if want := goldenBytes(t, "error_share_notfound.plain.stderr.golden"); got.code != exitcode.NotFound || got.stderr != want {
							t.Fatalf("exit = %d, stderr = %q; want %d and the share request's own not-found %q", got.code, got.stderr, exitcode.NotFound, want)
						}
					default:
						if want := goldenBytes(t, again.golden+".plain.stderr.golden"); got.code != again.wantCode || got.stderr != want {
							t.Fatalf("exit = %d, stderr = %q; want %d and the answer of the request made once more %q", got.code, got.stderr, again.wantCode, want)
						}
					}

					// What the order with the share request first gave, for the
					// same answers. A link from the link request is the one case
					// where stderr differs: that order has a share option, and
					// says that the link could not carry it.
					if got.code != before.code || got.delivered != before.delivered || (!fromLinkRequest && got.stderr != before.stderr) {
						t.Errorf("after the short limit ran out, get gave\nexit %d, delivered %q, stderr %q\nwant what the share request first gave:\nexit %d, delivered %q, stderr %q",
							got.code, got.delivered, got.stderr, before.code, before.delivered, before.stderr)
					}
					if got.shares != before.shares {
						t.Errorf("the share request was sent %d times, want %d as in the earlier order", got.shares, before.shares)
					}
				})
			}
		}
	}
}

// TestGetRefreshAfterTheLinkRequestWasMadeOnceMore pins where a link from
// the request made once more comes from: the link request, in a run whose
// share request cannot give a link. That is the third row of the refresh
// table in get_crid_link.go.
//
// The first link request gets no answer until its short limit runs out, the
// share request says "not found", and the request made once more gives the
// link. That link expires before any byte is served, and the download asks
// again. The refresh asks with the CRID alone and with no time limit from
// the command. It does not read the device key again, and it sends no share
// request: here a second share request would be answered "unavailable", and
// would end the download.
func TestGetRefreshAfterTheLinkRequestWasMadeOnceMore(t *testing.T) {
	state := bootstrapRegisteredState(t)
	ran := 0
	for _, mode := range getModes() {
		if !mode.downloads {
			continue
		}
		for _, readable := range []bool{true, false} {
			ran++
			t.Run(fmt.Sprintf("%s/key readable=%t", mode.name, readable), func(t *testing.T) {
				srv := downloadServer(t)
				srv.Script(http.MethodGet, apitest.DownloadPath, handlerGone)
				srv.Script(http.MethodPost, shareRoute(srv), apitest.HandlerNotFound404(t, "resource_not_found"))
				srv.ScriptRepeat(http.MethodPost, shareRoute(srv), 3, apitest.HandlerDark503(t))
				keyReads := noDeviceKey(connectorstate.NoDeviceKeyUnreadable)
				if readable {
					keyReads = deviceKeyOf(t, state)
				}
				relay := &slowRelay{t: t, srv: srv, requests: &linkRequests{link: issuedLink(srv.URL + apitest.DownloadPath)}}

				run := runShareMode(t, srv, srv.URL, mode, relay.wire(withDeviceKey(enrolledDevice(t, state), keyReads.read)))
				run.mustHaveDelivered(t, mode)

				asked, asDevice, bounded := relay.requests.asked, relay.requests.asDevice, relay.contexts.bounded
				if len(asked) != 3 {
					t.Fatalf("made the link request %d times, want 3: the first, the one made once more, and the refresh", len(asked))
				}
				if asDevice[0] != readable || !bounded[0] || asDevice[1] || bounded[1] {
					t.Errorf("the first two link requests: as this device %v, time limit from the command %v; want the first in the form the key allows with the short limit, and the second with the CRID alone and no limit",
						asDevice[:2], bounded[:2])
				}
				if asDevice[2] || bounded[2] {
					t.Errorf("the refresh: as this device = %t, time limit from the command = %t; want the CRID alone and no limit from the command", asDevice[2], bounded[2])
				}
				mustHaveReadTheKeyOnce(t, keyReads)
				if got := len(shareRequests(srv)); got != 1 {
					t.Errorf("the share request was sent %d times, want once: the refresh must not send it again", got)
				}
				if stderr := run.result.stderr.String(); strings.Count(stderr, "UNVERIFIED publisher") != 1 {
					t.Errorf("stderr = %q, want the publisher notice once", stderr)
				}
			})
		}
	}
	if ran == 0 {
		t.Fatal("no get mode downloads; this test would pin nothing")
	}
}

// TestLinkRequestBeforeAShareRequestSaysWhenItsShortLimitRanOut pins how get
// tells "the short limit ran out" from every other way the link request
// before a share request can end. Only that one ending leads to the link
// request made once more.
//
// It is true when the request gave no answer, the short limit ended the
// request, and the command's own context had not ended. Each row below takes
// one of the three away, or is the case itself.
func TestLinkRequestBeforeAShareRequestSaysWhenItsShortLimitRanOut(t *testing.T) {
	state := bootstrapRegisteredState(t)
	// request is the answer of one link request. interrupt ends the command's
	// own context, as the user does with an interrupt.
	type request func(ctx context.Context, t *testing.T, interrupt context.CancelFunc) (*qurl.CRIDLink, error)
	noAnswer := func(ctx context.Context, t *testing.T, _ context.CancelFunc) (*qurl.CRIDLink, error) {
		return nil, noAnswerUntilTheContextEnds(ctx, t)
	}
	atOnce := func(link *qurl.CRIDLink, err error) request {
		return func(context.Context, *testing.T, context.CancelFunc) (*qurl.CRIDLink, error) { return link, err }
	}

	for _, tc := range []struct {
		name string
		// shortLimit is the limit of the request. Zero leaves the production
		// limit, which no row here waits for.
		shortLimit time.Duration
		// commandLimit, if set, is a time limit of the command's own context.
		commandLimit time.Duration
		request      request
		wantRanOut   bool
		// wantErr is the error the request must end with; nil is a link.
		wantErr error
	}{
		{
			name: "no answer until the short limit runs out", shortLimit: testShortLimit, request: noAnswer,
			wantRanOut: true, wantErr: consume.ErrCRIDLinkNoAnswer,
		},
		{
			// A limit of the command that has not run out changes nothing.
			name: "no answer until the short limit runs out, the command has a longer limit", shortLimit: testShortLimit, commandLimit: time.Hour,
			request: noAnswer, wantRanOut: true, wantErr: consume.ErrCRIDLinkNoAnswer,
		},
		{
			// The second error the SDK has for a context that ended: a device
			// got "not found" for its first request, and the limit ran out
			// before the request under its key was sent.
			name: "the short limit runs out before the request under the device key", shortLimit: testShortLimit,
			request: func(ctx context.Context, _ *testing.T, _ context.CancelFunc) (*qurl.CRIDLink, error) {
				<-ctx.Done()
				return nil, fmt.Errorf("qurl: the CRID link request with the device key was not sent because the context ended first: %w", ctx.Err())
			},
			wantRanOut: true, wantErr: consume.ErrCRIDLinkNoAnswer,
		},
		{
			name: "no answer until the command's own limit runs out", commandLimit: testShortLimit, request: noAnswer,
			wantErr: consume.ErrCRIDLinkNoAnswer,
		},
		{
			name: "the user interrupts the command while it waits",
			request: func(ctx context.Context, t *testing.T, interrupt context.CancelFunc) (*qurl.CRIDLink, error) {
				interrupt()
				return nil, noAnswerUntilTheContextEnds(ctx, t)
			},
			wantErr: context.Canceled,
		},
		{
			// The short limit ended the request, and the user interrupted the
			// command before the request returned.
			name: "the user interrupts the command after the short limit ran out", shortLimit: testShortLimit,
			request: func(ctx context.Context, t *testing.T, interrupt context.CancelFunc) (*qurl.CRIDLink, error) {
				err := noAnswerUntilTheContextEnds(ctx, t)
				interrupt()
				return nil, err
			},
			wantErr: consume.ErrCRIDLinkNoAnswer,
		},
		{
			name:    "the service cannot be reached",
			request: atOnce(nil, &qurl.RelayError{Msg: "relay POST https://endpoint.example.test/x failed: connection refused"}),
			wantErr: consume.ErrCRIDLinkNoAnswer,
		},
		{
			name: "the SDK reports a timeout while the short limit has time left",
			request: atOnce(nil, fmt.Errorf("qurl: CRID link request did not complete: %w: %w",
				context.DeadlineExceeded, &qurl.RelayError{Msg: "relay POST https://endpoint.example.test/x failed"})),
			wantErr: consume.ErrCRIDLinkNoAnswer,
		},
		{
			name: "a refusal that comes when the short limit has run out", shortLimit: testShortLimit,
			request: func(ctx context.Context, _ *testing.T, _ context.CancelFunc) (*qurl.CRIDLink, error) {
				<-ctx.Done()
				return nil, sdkRefusal(qurl.ErrCRIDLinkRateLimited, "52603")
			},
			wantErr: consume.ErrCRIDLinkRateLimited,
		},
		{
			name: "a link that comes when the short limit has run out", shortLimit: testShortLimit,
			request: func(ctx context.Context, _ *testing.T, _ context.CancelFunc) (*qurl.CRIDLink, error) {
				<-ctx.Done()
				return issuedLink("https://link.example.test/x"), nil
			},
		},
	} {
		for _, readable := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/key readable=%t", tc.name, readable), func(t *testing.T) {
				ctx, interrupt := context.WithCancel(t.Context())
				defer interrupt()
				if tc.commandLimit > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.commandLimit)
					defer cancel()
				}
				keyReads := noDeviceKey(connectorstate.NoDeviceKeyUnreadable)
				if readable {
					keyReads = deviceKeyOf(t, state)
				}
				asked, askedAsDevice := 0, 0
				opts := &globalOpts{
					linkTimeoutBeforeShare: tc.shortLimit,
					readDeviceKey:          keyReads.read,
					requestCRIDLink: func(ctx context.Context, _ string) (*qurl.CRIDLink, error) {
						asked++
						return tc.request(ctx, t, interrupt)
					},
					requestCRIDLinkAsDevice: func(ctx context.Context, _ []byte, _ string) (*qurl.CRIDLink, error) {
						asked++
						askedAsDevice++
						return tc.request(ctx, t, interrupt)
					},
				}

				link, ranOut, err := opts.linkByRequestBeforeShare(ctx, exampleCRID)
				if ranOut != tc.wantRanOut {
					t.Errorf("the short limit ran out = %t, want %t (error %v)", ranOut, tc.wantRanOut, err)
				}
				if !errors.Is(err, tc.wantErr) || (tc.wantErr == nil) != (link != nil) {
					t.Errorf("the request ended with link present = %t and error %v, want error %v", link != nil, err, tc.wantErr)
				}
				wantAsDevice := 0
				if readable {
					wantAsDevice = 1
				}
				if asked != 1 || askedAsDevice != wantAsDevice {
					t.Errorf("made the link request %d times, %d of them as this device; want 1 and %d", asked, askedAsDevice, wantAsDevice)
				}
			})
		}
	}
}

// TestGetDoesNotMakeTheLinkRequestOnceMoreWhenTheCommandEnded runs get for
// the two endings that are not "the short limit ran out", although the link
// request waited until its context ended. In both, the short limit is the
// production one and has time left.
//
// The user interrupts the command: it stops at once, with exit code 130. It
// sends nothing to the qURL API and makes no second link request.
//
// The command's own context has a time limit, and it runs out: the command
// goes on to its share request, which cannot be answered any more, and that
// failure is the result. No second link request is made.
func TestGetDoesNotMakeTheLinkRequestOnceMoreWhenTheCommandEnded(t *testing.T) {
	state := bootstrapRegisteredState(t)
	mode := getFileMode(t)
	for _, readable := range []bool{true, false} {
		for _, tc := range []struct {
			name string
			// command returns the command's own context, and the function that
			// ends it as an interrupt does.
			command func(t *testing.T) (context.Context, context.CancelFunc)
			// interrupted says the user interrupts while the request waits.
			interrupted bool
		}{
			{
				name: "interrupted by the user", interrupted: true,
				command: func(t *testing.T) (context.Context, context.CancelFunc) { return context.WithCancel(t.Context()) },
			},
			{
				name: "the command's own limit ran out",
				command: func(t *testing.T) (context.Context, context.CancelFunc) {
					return context.WithTimeout(t.Context(), testShortLimit)
				},
			},
		} {
			t.Run(fmt.Sprintf("%s/key readable=%t", tc.name, readable), func(t *testing.T) {
				srv := downloadServer(t)
				shareNotFoundTwice(t, srv)
				ctx, end := tc.command(t)
				defer end()
				keyReads := noDeviceKey(connectorstate.NoDeviceKeyUnreadable)
				if readable {
					keyReads = deviceKeyOf(t, state)
				}
				requests := &linkRequests{}
				wait := func(ctx context.Context) (*qurl.CRIDLink, error) {
					if tc.interrupted {
						end()
					}
					return nil, noAnswerUntilTheContextEnds(ctx, t)
				}
				configure := func(args []string) *runOpts {
					opts := withDeviceKey(enrolledDevice(t, state), keyReads.read)(args)
					opts.ctx = ctx
					opts.requestCRIDLink = func(ctx context.Context, resourceCRID string) (*qurl.CRIDLink, error) {
						_, _ = requests.answer(ctx, resourceCRID)
						return wait(ctx)
					}
					opts.requestCRIDLinkAsDevice = func(ctx context.Context, key []byte, resourceCRID string) (*qurl.CRIDLink, error) {
						_, _ = requests.answerAsDevice(ctx, key, resourceCRID)
						return wait(ctx)
					}
					return opts
				}

				run := runShareMode(t, srv, srv.URL, mode, configure)
				run.mustNotHaveActed(t)
				if len(requests.asked) != 1 {
					t.Errorf("made the link request %d times, want once", len(requests.asked))
				}
				if got := len(shareRequests(srv)); got != 0 {
					t.Errorf("the share request was answered %d times, want none: the command's context had ended", got)
				}
				stderr := run.result.stderr.String()
				if tc.interrupted {
					if run.result.code != exitcode.Interrupted || stderr != "" || len(srv.Requests()) != 0 {
						t.Errorf("exit = %d, stderr = %q, %d request(s) to the qURL API; want %d, nothing printed and no request",
							run.result.code, stderr, len(srv.Requests()), exitcode.Interrupted)
					}
					return
				}
				// The share request's own failure: it is not the link request's
				// "the service did not answer".
				if run.result.code == exitcode.Success || run.result.code == exitcode.Interrupted || strings.Contains(stderr, consume.MsgCRIDLinkNoAnswer) {
					t.Errorf("exit = %d, stderr = %q; want the failure of the share request", run.result.code, stderr)
				}
			})
		}
	}
}

// TestGetAsksWithTheKeyItReadAndWipesIt pins what get does with the device
// key. The request as this device is given exactly the key the read
// returned, and when the request has been answered, the bytes the read
// handed out hold only zeros. That holds whatever the answer was.
func TestGetAsksWithTheKeyItReadAndWipesIt(t *testing.T) {
	state := bootstrapRegisteredState(t)
	mode := getFileMode(t)
	for name, answer := range map[string]*linkRequests{
		"link":      {link: issuedLink("")},
		"not found": {err: sdkRefusal(qurl.ErrCRIDLinkNotFound, "52602")},
		"no answer": {err: &qurl.RelayError{Msg: "relay POST failed"}},
	} {
		t.Run(name, func(t *testing.T) {
			srv := downloadServer(t)
			if answer.link != nil {
				answer.link.Link = srv.URL + apitest.DownloadPath
			}
			keyReads := deviceKeyOf(t, state)
			run := runShareMode(t, srv, srv.URL, mode, withDeviceKey(withLinkRequestsOf(enrolledDevice(t, state), answer), keyReads.read))
			run.mustHaveDelivered(t, mode)

			if len(answer.keys) != 1 || !bytes.Equal(answer.keys[0], keyReads.key) {
				t.Fatalf("the request as this device was given %d key(s), want exactly the key that was read", len(answer.keys))
			}
			if len(keyReads.given) != 1 || !bytes.Equal(keyReads.given[0], make([]byte, 32)) {
				t.Errorf("the key that was read was not wiped after the request")
			}
		})
	}
}

// errKeyTheSDKWillNotUse is the SDK's refusal of a device key, as it gives it
// before it looks at the CRID or sends anything.
var errKeyTheSDKWillNotUse = fmt.Errorf("%w: it must be 32 bytes", qurl.ErrInvalidDeviceKey)

// TestGetAsksWithTheCRIDAloneWhenTheSDKWillNotUseTheKey covers a key that
// the read accepted and the SDK refuses. The SDK sent nothing, so the device
// asks with the CRID alone, as a device does that could not read its key.
//
// The refusal of a key must never be read as "this client cannot ask for
// this CRID". That would send the device straight to its share request, and
// on a share "not found" it would tell the user that the CRID is the
// problem.
func TestGetAsksWithTheCRIDAloneWhenTheSDKWillNotUseTheKey(t *testing.T) {
	state := bootstrapRegisteredState(t)
	mode := getFileMode(t)

	// With the refusal given at the seam.
	t.Run("the request is refused for its key", func(t *testing.T) {
		srv := downloadServer(t)
		shareNotFoundTwice(t, srv)
		alone := &linkRequests{link: issuedLink(srv.URL + apitest.DownloadPath)}
		asDevice := 0
		configure := func(args []string) *runOpts {
			opts := withDeviceKey(enrolledDevice(t, state), deviceKeyOf(t, state).read)(append(args, "--verbose"))
			opts.requestCRIDLink = alone.answer
			opts.requestCRIDLinkAsDevice = func(context.Context, []byte, string) (*qurl.CRIDLink, error) {
				asDevice++
				return nil, errKeyTheSDKWillNotUse
			}
			return opts
		}
		run := runShareMode(t, srv, srv.URL, mode, configure)
		run.mustHaveDelivered(t, mode)
		if asDevice != 1 || len(alone.asked) != 1 {
			t.Errorf("asked as this device %d times and with the CRID alone %d times, want once each", asDevice, len(alone.asked))
		}
		if got := apiRequests(srv); len(got) != 0 {
			t.Errorf("qURL API requests = %q, want none: the request with the CRID alone gave the link", got)
		}
		stderr := run.result.stderr.String()
		if want := "[debug] " + fmt.Sprintf(msgCRIDLinkDeviceKeyNotRead, connectorstate.NoDeviceKeyInvalid) + "\n"; strings.Count(stderr, want) != 1 {
			t.Errorf("stderr = %q, want the line %q once", stderr, want)
		}
		if strings.Contains(stderr, msgCRIDLinkAsDevice) || strings.Contains(stderr, "CRID link request not sent") {
			t.Errorf("stderr = %q, must not say that the device asked as itself, or that the CRID cannot be asked for", stderr)
		}
	})

	// Through the real SDK: the read hands out bytes that are not a key, the
	// SDK itself refuses them, and its test server shows what was sent.
	for name, badKey := range map[string][]byte{
		"31 bytes":        bytes.Repeat([]byte{7}, 31),
		"only zero bytes": make([]byte, 32),
	} {
		t.Run("the SDK refuses "+name, func(t *testing.T) {
			path := newSDKLinkPath(t, nil)
			srv := serverForCRID(t, path.server.CRID())
			shareNotFoundTwice(t, srv)
			var verified, granted []string
			keyReads := &deviceKeyReads{key: badKey}

			run := runShareMode(t, srv, srv.URL, mode, withDeviceKey(path.wire(t, srv, enrolledDevice(t, state), &verified, &granted), keyReads.read))
			run.link = path.link
			run.mustHaveDelivered(t, mode)
			requests := path.server.Requests()
			if len(requests) != 1 || requests[0].AsDevice {
				t.Errorf("the service answered %+v, want one request, not under a device key", requests)
			}
			if got := apiRequests(srv); len(got) != 0 {
				t.Errorf("qURL API requests = %q, want none", got)
			}
		})
	}
}

// debugLines returns the lines of stderr that --verbose adds.
func debugLines(stderr string) []string {
	var lines []string
	for line := range strings.SplitSeq(stderr, "\n") {
		if strings.HasPrefix(line, "[debug] ") {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestGetSaysHowItAskedForALinkOnlyWithVerbose pins the two diagnostic lines
// about the form of the link request. Without --verbose there is no such
// line. With --verbose there is one line for a device that made the link
// request first: that it asked as this device, or that it asked with the
// CRID alone and one fixed word for why the device key was not read.
//
// The message for the user is the same with and without --verbose. No line
// carries any part of the key.
func TestGetSaysHowItAskedForALinkOnlyWithVerbose(t *testing.T) {
	state := bootstrapRegisteredState(t)
	mode := getFileMode(t)
	asDeviceLine := "[debug] " + msgCRIDLinkAsDevice
	notReadLine := func(why connectorstate.NoDeviceKey) string {
		return "[debug] " + fmt.Sprintf(msgCRIDLinkDeviceKeyNotRead, why)
	}
	if !strings.HasPrefix(msgCRIDLinkAsDevice, "> CRID link request as this device") ||
		!strings.HasPrefix(msgCRIDLinkDeviceKeyNotRead, "> CRID link request with the CRID alone") {
		t.Fatalf("the two lines are %q and %q; this test tells them apart by how they start", msgCRIDLinkAsDevice, msgCRIDLinkDeviceKeyNotRead)
	}
	stateDir := filepath.Join(t.TempDir(), "no-device-state")
	notFound := sdkRefusal(qurl.ErrCRIDLinkNotFound, "52602")

	type row struct {
		name    string
		machine func(t *testing.T) func(args []string) *runOpts
		err     error
		// want is the one line about the form of the request; empty means
		// no such line.
		want string
	}
	reasons := []connectorstate.NoDeviceKey{
		connectorstate.NoDeviceKeyNoState, connectorstate.NoDeviceKeyPlatform, connectorstate.NoDeviceKeySupervision,
		connectorstate.NoDeviceKeyStorage, connectorstate.NoDeviceKeyUnreadable, connectorstate.NoDeviceKeyNotRegistered,
		connectorstate.NoDeviceKeyInvalid, connectorstate.NoDeviceKeyAgentID,
	}
	rows := make([]row, 0, 4+len(reasons))
	rows = append(rows, []row{
		{
			name: "device that reads its key", err: notFound, want: asDeviceLine,
			machine: func(t *testing.T) func(args []string) *runOpts {
				return withDeviceKey(enrolledDevice(t, state), deviceKeyOf(t, state).read)
			},
		},
		{
			// Nothing was sent, so the line about the form is left out. The
			// existing line says that the request was not sent.
			name: "device that reads its key, CRID this client cannot ask for", err: errCRIDVersionTheSDKCannotCheck,
			machine: func(t *testing.T) func(args []string) *runOpts {
				return withDeviceKey(enrolledDevice(t, state), deviceKeyOf(t, state).read)
			},
		},
		{
			name: "machine with no identity", err: notFound,
			machine: func(t *testing.T) func(args []string) *runOpts { return machineWithNoIdentity(t, stateDir) },
		},
		{
			name: "device with a share option", err: notFound,
			machine: func(t *testing.T) func(args []string) *runOpts {
				return withDeviceKey(withArgs(enrolledDevice(t, state), "--session-duration", "5m"), mustNotReadTheDeviceKey(t))
			},
		},
	}...)
	// One row for each word the read of the key can give.
	for _, why := range reasons {
		rows = append(rows, row{
			name: "device with no readable key: " + string(why), err: notFound, want: notReadLine(why),
			machine: func(t *testing.T) func(args []string) *runOpts {
				return withDeviceKey(enrolledDevice(t, state), noDeviceKey(why).read)
			},
		})
	}

	key := deviceKeyOf(t, state).key
	keyForms := []string{
		base64.StdEncoding.EncodeToString(key), base64.RawStdEncoding.EncodeToString(key),
		base64.URLEncoding.EncodeToString(key), base64.RawURLEncoding.EncodeToString(key), hex.EncodeToString(key),
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			run := func(t *testing.T, verbose bool) string {
				t.Helper()
				srv := downloadServer(t)
				shareNotFoundTwice(t, srv)
				requests := &linkRequests{err: tc.err}
				machine := withLinkRequestsOf(tc.machine(t), requests)
				if verbose {
					machine = withArgs(machine, "--verbose")
				}
				result := runShareMode(t, srv, srv.URL, mode, machine)
				if result.result.code == exitcode.Success {
					t.Fatalf("verbose=%t: exit = 0, want a refusal", verbose)
				}
				return result.result.stderr.String()
			}

			quiet := run(t, false)
			if lines := debugLines(quiet); len(lines) != 0 {
				t.Errorf("without --verbose stderr has the diagnostic lines %q", lines)
			}

			verbose := run(t, true)
			var about []string
			for _, line := range debugLines(verbose) {
				if strings.HasPrefix(line, "[debug] > CRID link request as this device") || strings.HasPrefix(line, "[debug] > CRID link request with the CRID alone") {
					about = append(about, line)
				}
			}
			if tc.want == "" && len(about) != 0 {
				t.Errorf("stderr has the lines %q about the form of the request, want none", about)
			}
			if tc.want != "" && (len(about) != 1 || about[0] != tc.want) {
				t.Errorf("stderr has the lines %q about the form of the request, want exactly %q", about, tc.want)
			}
			var told strings.Builder
			for line := range strings.SplitAfterSeq(verbose, "\n") {
				if !strings.HasPrefix(line, "[debug] ") {
					told.WriteString(line)
				}
			}
			if told.String() != quiet {
				t.Errorf("with --verbose the user is told %q, want the same message %q", told.String(), quiet)
			}
			for _, form := range keyForms {
				if strings.Contains(verbose, form) {
					t.Error("stderr carries the device key")
				}
			}
		})
	}
}

// stateSnapshot describes every entry below dir: its name, its type and
// permission bits, its size, its modification time, and for a file a digest
// of its bytes.
func stateSnapshot(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(path string, _ fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		digest := "-"
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path) //nolint:gosec // A path below the test's own temporary directory.
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			digest = hex.EncodeToString(sum[:])
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("%s mode=%s size=%d modified=%s sha256=%s",
			filepath.ToSlash(rel), info.Mode(), info.Size(), info.ModTime().UTC().Format(time.RFC3339Nano), digest))
		return nil
	})
	if err != nil {
		t.Fatalf("describe %s: %v", dir, err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// TestGetAnsweredByTheLinkRequestLeavesTheDeviceStateAsItWas runs get on a
// device with real state and the production read of the device key. The link
// request gives the link, so the command never opens the device runtime, and
// the state directory is the same afterwards: no new file, no lock file, and
// no file that was written again.
//
// Where the production read is supported, the request was made as this
// device, with the key in that state.
func TestGetAnsweredByTheLinkRequestLeavesTheDeviceStateAsItWas(t *testing.T) {
	state := bootstrapRegisteredState(t)
	for _, mode := range getModes() {
		t.Run(mode.name, func(t *testing.T) {
			srv := downloadServer(t)
			requests := &linkRequests{link: issuedLink(srv.URL + apitest.DownloadPath)}
			var stateDir, before string
			configure := func(args []string) *runOpts {
				opts := withLinkRequestsOf(enrolledDevice(t, state), requests)(args)
				opts.openNativeRuntime = func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
					t.Error("the device runtime was opened although the link request gave the link")
					return nil, errors.New("unexpected device runtime open")
				}
				stateDir = opts.shareStateDir
				before = stateSnapshot(t, stateDir)
				return opts
			}

			run := runShareMode(t, srv, srv.URL, mode, configure)
			run.mustHaveDelivered(t, mode)
			if after := stateSnapshot(t, stateDir); after != before {
				t.Errorf("the command changed the device state directory.\nbefore:\n%s\nafter:\n%s", before, after)
			}
			wantAsDevice := 0
			if deviceKeyReadable {
				wantAsDevice = 1
			}
			if len(requests.asked) != 1 || requests.askedAsDevice() != wantAsDevice {
				t.Fatalf("made the link request %d times, %d of them as this device; want 1 and %d", len(requests.asked), requests.askedAsDevice(), wantAsDevice)
			}
			if wantAsDevice == 1 && !bytes.Equal(requests.keys[0], deviceKeyOf(t, state).key) {
				t.Error("the request as this device was not given the key in the device state")
			}
		})
	}
}

// TestReadOfTheDeviceKeyWhereTheStateDirectoryIsNotKnown pins the production
// read of the device key for a machine whose state directory cannot be
// named. Nothing is opened, and the answer is no key: "no state" where the
// host has no place for device state at all, and "unreadable" where the
// place could not be worked out for another reason. In that second case the
// machine still counts as one with an identity, so it asks with the CRID
// alone, and its share request then reports the fault as before.
func TestReadOfTheDeviceKeyWhereTheStateDirectoryIsNotKnown(t *testing.T) {
	for _, tc := range []struct {
		name        string
		dirErr      error
		why         connectorstate.NoDeviceKey
		hasIdentity bool
	}{
		{
			name:   "the host has no state directory",
			dirErr: fmt.Errorf("%w: set %s", connectorstate.ErrNoDefaultStateDir, connectorstate.EnvStateDirPrimary),
			why:    connectorstate.NoDeviceKeyNoState,
		},
		{name: "the state directory cannot be resolved", dirErr: errors.New("resolve failed"), why: connectorstate.NoDeviceKeyUnreadable, hasIdentity: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "never-created")
			opts := &globalOpts{
				lookupEnv:            func(string) (string, bool) { return "", false },
				resolveShareStateDir: func(string) (string, error) { return dir, tc.dirErr },
				resolvedSupervision:  connectorstate.RuntimeSupervisionNative,
			}
			if key, why := opts.readDeviceStaticPrivateKey(t.Context()); key != nil || why != tc.why {
				t.Errorf("the read of the device key gave %d bytes and %q, want no key and %q", len(key), why, tc.why)
			}
			if got := opts.hasDeviceIdentity(); got != tc.hasIdentity {
				t.Errorf("hasDeviceIdentity() = %t, want %t", got, tc.hasIdentity)
			}
			mustNotExistCmd(t, dir)
		})
	}
}

// TestHarnessRefusesTheLinkRequestByDefault holds the wiring of the harness
// for the link request, as TestHarnessGuardIsTheDefault does for the API
// client. An invocation that gave no answer for a form of the request is
// wired with the guard for that form, so a command that makes the request
// fails the owning test by the name of the request, and sends nothing. The
// functions are read back from the command tree's own options, so removing
// a default of the harness fails here.
//
// It also holds the default for "is the request offered": not offered,
// unless the test gave an answer for either form.
func TestHarnessRefusesTheLinkRequestByDefault(t *testing.T) {
	// version sends nothing and asks for no link.
	res := runCLI(t, &runOpts{args: []string{"version"}})
	if res.code != 0 {
		t.Fatalf("version exit = %d, stderr %q", res.code, res.stderr.String())
	}
	if res.requestCRIDLink == nil || res.requestCRIDLinkAsDevice == nil || res.cridLinkOffered == nil {
		t.Fatal("a hermetic invocation has no function for the link request or for its offer")
	}
	if offered, err := res.cridLinkOffered(); offered || err != nil {
		t.Fatalf("the default answer to \"is the request offered\" is %t, %v; want not offered", offered, err)
	}

	// Stand in for this test only now, after the harness has wired the
	// invocation. The context has ended, so a function that is not the guard
	// could not send a request from here either.
	recorder := &egressRecorder{}
	res.linkRequestGuard.report = recorder.reportf
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	key := bytes.Repeat([]byte{1}, 32)

	if link, err := res.requestCRIDLink(ctx, exampleCRID); link != nil || err == nil {
		t.Errorf("the default request with only the CRID returned %v, %v; want no link and an error", link, err)
	}
	if reports := recorder.all(); len(reports) != 1 || reports[0] != "the command asked for a link with only the CRID" {
		t.Fatalf("reports = %q, want one that names the request with only the CRID", reports)
	}
	if link, err := res.requestCRIDLinkAsDevice(ctx, key, exampleCRID); link != nil || err == nil {
		t.Errorf("the default request as this device returned %v, %v; want no link and an error", link, err)
	}
	if reports := recorder.all(); len(reports) != 2 || reports[1] != "the command asked for a link as this device" {
		t.Fatalf("reports = %q, want a second one that names the request as this device", reports)
	}

	// An answer for either form says that the request is offered, and leaves
	// the guard on the other form.
	alone := runCLI(t, &runOpts{args: []string{"version"}, requestCRIDLink: (&linkRequests{}).answer})
	asDevice := runCLI(t, &runOpts{args: []string{"version"}, requestCRIDLinkAsDevice: (&linkRequests{}).answerAsDevice})
	for name, res := range map[string]*runResult{"only the CRID": alone, "as this device": asDevice} {
		if offered, err := res.cridLinkOffered(); !offered || err != nil {
			t.Errorf("with an answer for the request with %s, the request is offered = %t, %v; want offered", name, offered, err)
		}
	}
	guarded := &egressRecorder{}
	alone.linkRequestGuard.report, asDevice.linkRequestGuard.report = guarded.reportf, guarded.reportf
	if _, err := alone.requestCRIDLinkAsDevice(ctx, key, exampleCRID); err == nil {
		t.Error("with an answer for the request with only the CRID, the request as this device is not guarded")
	}
	if _, err := asDevice.requestCRIDLink(ctx, exampleCRID); err == nil {
		t.Error("with an answer for the request as this device, the request with only the CRID is not guarded")
	}
	if reports := guarded.all(); len(reports) != 2 {
		t.Errorf("reports = %q, want one for each form that had no answer", reports)
	}
}

// TestProductionWiringOfTheRequestAsThisDevice holds what a real invocation
// is wired with, read back from the command tree's own options. realOpener
// leaves the production functions in place.
//
// The request as this device is the SDK's request as a device: it refuses a
// key that is not a device key, which the request with only the CRID cannot
// do, since it takes no key. The SDK gives that refusal before it reads any
// settings, so nothing is sent. The context has ended as well.
//
// The read of the device key is the read-only one over this invocation's
// state directory. That directory does not exist here, so the read gives no
// key, and it does not create the directory.
func TestProductionWiringOfTheRequestAsThisDevice(t *testing.T) {
	t.Setenv(qurl.EnvDeploymentPath, "")
	configDir := t.TempDir()
	res := runCLI(t, &runOpts{args: []string{"version"}, realOpener: true, configDir: configDir})
	if res.code != 0 {
		t.Fatalf("version exit = %d, stderr %q", res.code, res.stderr.String())
	}
	if res.requestCRIDLinkAsDevice == nil || res.readDeviceKey == nil {
		t.Fatal("a real invocation has no request as this device, or no read of the device key")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	link, err := res.requestCRIDLinkAsDevice(ctx, make([]byte, 32), exampleCRID)
	if link != nil || !consume.DeviceKeyRefused(err) {
		t.Errorf("the request as this device with a key of only zero bytes returned %v, %v; want the SDK's refusal of the key", link, err)
	}

	stateDir := filepath.Join(configDir, "connector-state")
	if key, why := res.readDeviceKey(t.Context()); key != nil || why != connectorstate.NoDeviceKeyNoState {
		t.Errorf("the read of the device key gave %d bytes and %q, want no key and %q", len(key), why, connectorstate.NoDeviceKeyNoState)
	}
	mustNotExistCmd(t, stateDir)
}
