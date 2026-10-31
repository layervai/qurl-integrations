package internal

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

func TestSlashSynchronousWorkCancellation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		base, before bool
	}{
		{"request_already_canceled", false, true}, {"request_canceled_during_call", false, false},
		{"shutdown_already_canceled", true, true}, {"shutdown_during_call", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := feedbackTestHandler(t)
			base, cancelBase := context.WithCancel(context.Background())
			defer cancelBase()
			h.baseCtx = base
			type requestKey struct{}
			parent := context.WithValue(context.Background(), requestKey{}, "request-value")
			requestCtx, cancelRequest := context.WithTimeout(parent, time.Minute)
			defer cancelRequest()
			cancel := cancelRequest
			if tc.base {
				cancel = cancelBase
			}
			if tc.before {
				cancel()
			}
			opened := make(chan context.Context, 1)
			h.cfg.OpenView = func(ctx context.Context, _, _ string, _ []byte) error {
				opened <- ctx
				<-ctx.Done()
				return ctx.Err()
			}
			body := feedbackCommandBody()
			req := newSignedRequest(t, "/slack/commands", body, body).WithContext(requestCtx)
			done := make(chan struct{})
			go func() { defer close(done); h.ServeHTTP(httptest.NewRecorder(), req) }()
			var callCtx context.Context
			select {
			case callCtx = <-opened:
			case <-time.After(3 * time.Second):
				t.Fatal("OpenView not reached")
			}
			if callCtx.Value(requestKey{}) != "request-value" {
				t.Error("request value lost")
			}
			if _, ok := callCtx.Deadline(); !ok {
				t.Error("call budget lost")
			}
			if !tc.before {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("synchronous call did not stop")
			}
			if !errors.Is(callCtx.Err(), context.Canceled) {
				t.Errorf("call error=%v, want cancellation", callCtx.Err())
			}
		})
	}
}

type requestContextAuthProvider struct {
	recordingAuthProvider
	started chan context.Context
}

func (p *requestContextAuthProvider) APIKey(ctx context.Context, _ string) (string, error) {
	p.started <- ctx
	<-ctx.Done()
	return "", ctx.Err()
}

func TestSlashAcceptedJobOutlivesRequest(t *testing.T) {
	ts := newAdminTestServers(t)
	h := newAdminTestHandler(t, ts)
	base, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	h.baseCtx = base
	provider := &requestContextAuthProvider{started: make(chan context.Context, 1)}
	h.cfg.AuthProvider = provider
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	body := url.Values{fieldCommand: {commandUser}, fieldText: {"aliases"}, fieldTeamID: {testAdminTeamID}, fieldUserID: {testAdminUserID}, fieldChannelID: {getTokenCommandTestChannelID}}.Encode()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body).WithContext(requestCtx))
	var jobCtx context.Context
	select {
	case jobCtx = <-provider.started:
	case <-time.After(3 * time.Second):
		t.Fatal("accepted job did not start")
	}
	cancelRequest()
	if jobCtx.Err() != nil {
		t.Fatalf("request canceled accepted job: %v", jobCtx.Err())
	}
	if w.Code != http.StatusOK {
		t.Fatalf("ack status=%d", w.Code)
	}
	cancelBase()
	select {
	case <-jobCtx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not cancel job")
	}
	h.Wait()
}

type requestContextDDB struct {
	slackdata.DynamoDBClient
	observed context.Context
}

func (d *requestContextDDB) GetItem(ctx context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	d.observed = ctx
	<-ctx.Done()
	return nil, ctx.Err()
}
func TestSlashAdminReadUsesRequestContext(t *testing.T) {
	h := newTestHandler(t, noopQURLServer(t))
	ddb := &requestContextDDB{}
	store, err := slackdata.NewStore(context.Background(), slackdata.WithDynamoDBClient(ddb), slackdata.WithTableNames("workspace_mappings", "channel_policies"))
	if err != nil {
		t.Fatal(err)
	}
	h.cfg.AdminStore = store
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	body := url.Values{fieldCommand: {commandAdmin}, fieldText: {testAdminListCmd}, fieldTeamID: {testAdminTeamID}, fieldUserID: {testAdminUserID}}.Encode()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, "/slack/commands", body, body).WithContext(ctx))
	if ddb.observed == nil {
		t.Fatal("admin read not reached")
	}
	if !errors.Is(ddb.observed.Err(), context.Canceled) {
		t.Errorf("admin read error=%v, want request cancellation", ddb.observed.Err())
	}
	if !strings.Contains(w.Body.String(), "failed to verify admin status") {
		t.Errorf("canceled gate must fail closed: %s", w.Body.String())
	}
}
