package internal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
)

type lifecycleQueueStub struct {
	enqueue func(context.Context, *LifecyclePurgeJob) error
	receive func(context.Context) (*LifecyclePurgeDelivery, error)
	delete  func(context.Context, string) error
}

func (q *lifecycleQueueStub) Enqueue(ctx context.Context, job *LifecyclePurgeJob) error {
	return q.enqueue(ctx, job)
}
func (q *lifecycleQueueStub) Receive(ctx context.Context) (*LifecyclePurgeDelivery, error) {
	if q.receive != nil {
		return q.receive(ctx)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}
func (q *lifecycleQueueStub) Delete(ctx context.Context, receipt string) error {
	if q.delete != nil {
		return q.delete(ctx, receipt)
	}
	return nil
}

// Existing cascade tests model an accepted durable delivery immediately picked
// up by a worker. Restart/replay tests below keep acceptance and execution apart.
func attachLifecycleTestQueue(h *Handler) {
	h.cfg.LifecyclePurgeQueue = &lifecycleQueueStub{enqueue: func(_ context.Context, job *LifecyclePurgeJob) error {
		h.Go(func() { _ = h.processLifecyclePurgeJob(h.baseCtx, job) })
		return nil
	}}
}

type lifecycleSQSStub struct {
	send    func(context.Context, *sqs.SendMessageInput) (*sqs.SendMessageOutput, error)
	receive func(context.Context, *sqs.ReceiveMessageInput) (*sqs.ReceiveMessageOutput, error)
	delete  func(context.Context, *sqs.DeleteMessageInput) (*sqs.DeleteMessageOutput, error)
}

func (s *lifecycleSQSStub) SendMessage(ctx context.Context, in *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	return s.send(ctx, in)
}
func (s *lifecycleSQSStub) ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	return s.receive(ctx, in)
}
func (s *lifecycleSQSStub) DeleteMessage(ctx context.Context, in *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	return s.delete(ctx, in)
}

func lifecycleJobFixture() *LifecyclePurgeJob {
	return &LifecyclePurgeJob{Version: 1, WorkspaceIDs: []string{"T_fixture"}, AgentStateOnlyIDs: []string{"T_child"}, Cutoff: time.Unix(1700000000, 0).UTC()}
}

func TestLifecycleQueuePublishRequiresPersistenceReceipt(t *testing.T) {
	for _, tt := range []struct {
		name      string
		out       *sqs.SendMessageOutput
		err       error
		wantError bool
	}{
		{"accepted", &sqs.SendMessageOutput{MessageId: aws.String("persisted")}, nil, false},
		{"transport failure", nil, errors.New("connection lost"), true},
		{"missing response", nil, nil, true},
		{"missing receipt", &sqs.SendMessageOutput{}, nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q := &sqsLifecyclePurgeQueue{url: "https://sqs.example.invalid/fixture.fifo", client: &lifecycleSQSStub{send: func(ctx context.Context, in *sqs.SendMessageInput) (*sqs.SendMessageOutput, error) {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > lifecyclePurgePublishTimeout {
					t.Fatal("publish is not bounded before Slack's ACK deadline")
				}
				var body map[string]json.RawMessage
				if err := json.Unmarshal([]byte(*in.MessageBody), &body); err != nil {
					t.Fatal(err)
				}
				if len(body) != 4 || body["workspace_ids"] == nil || body["cutoff"] == nil {
					t.Fatal("job must contain only the metadata contract")
				}
				if len(aws.ToString(in.MessageGroupId)) != 64 || len(aws.ToString(in.MessageDeduplicationId)) != 64 {
					t.Fatal("missing stable FIFO identities")
				}
				return tt.out, tt.err
			}}}
			if err := q.Enqueue(context.Background(), lifecycleJobFixture()); (err != nil) != tt.wantError {
				t.Fatalf("enqueue error=%v", err)
			}
		})
	}
}

func TestLifecycleQueueReceivePinsOneMessageAndJobDeadline(t *testing.T) {
	job := lifecycleJobFixture()
	body, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	q := &sqsLifecyclePurgeQueue{url: "https://sqs.example.invalid/fixture.fifo", client: &lifecycleSQSStub{receive: func(_ context.Context, in *sqs.ReceiveMessageInput) (*sqs.ReceiveMessageOutput, error) {
		if in.MaxNumberOfMessages != 1 || in.WaitTimeSeconds != 20 || time.Duration(in.VisibilityTimeout)*time.Second <= lifecyclePurgeJobTimeout {
			t.Fatal("receive must preserve FIFO serialization and the work lease")
		}
		return &sqs.ReceiveMessageOutput{Messages: []sqstypes.Message{{Body: aws.String(string(body)), ReceiptHandle: aws.String("fixture-receipt")}}}, nil
	}}}
	delivery, err := q.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if delivery.Receipt != "fixture-receipt" || !delivery.Job.Cutoff.Equal(job.Cutoff) {
		t.Fatal("delivery lost its original cutoff or receipt")
	}
}

func TestLifecycleQueueMissingConfigurationDoesNotACKTeardown(t *testing.T) {
	h := NewHandler(Config{})
	w := httptest.NewRecorder()
	h.handleEvent(w, []byte(`{"type":"event_callback","team_id":"T_fixture","event":{"type":"app_uninstalled"}}`))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d; no durable queue must leave Slack free to retry", w.Code)
	}
}

func TestLifecycleQueueFailureDoesNotStartBestEffortPurge(t *testing.T) {
	h, provider, _ := newLifecycleTestHandler(t)
	h.cfg.LifecyclePurgeQueue = &lifecycleQueueStub{enqueue: func(context.Context, *LifecyclePurgeJob) error { return errors.New("unavailable") }}
	w := httptest.NewRecorder()
	h.handleEvent(w, []byte(`{"type":"event_callback","team_id":"`+testAdminTeamID+`","event":{"type":"app_uninstalled"}}`))
	h.Wait()
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", w.Code)
	}
	if provider.deleteStateCalls != 0 {
		t.Fatal("failed handoff must not run an untracked purge")
	}
}

func TestLifecycleQueueRejectsInvalidJobsWithoutCallingSQS(t *testing.T) {
	for _, tt := range []struct {
		name  string
		alter func(*LifecyclePurgeJob)
	}{
		{"version", func(j *LifecyclePurgeJob) { j.Version = 2 }},
		{"no partitions", func(j *LifecyclePurgeJob) { j.WorkspaceIDs = nil; j.AgentStateOnlyIDs = nil }},
		{"empty partition", func(j *LifecyclePurgeJob) { j.WorkspaceIDs = []string{""} }},
		{"zero cutoff", func(j *LifecyclePurgeJob) { j.Cutoff = time.Time{} }},
		{"future cutoff", func(j *LifecyclePurgeJob) { j.Cutoff = time.Now().Add(time.Hour) }},
		{"oversize", func(j *LifecyclePurgeJob) {
			j.WorkspaceIDs = make([]string, 1024)
			for i := range j.WorkspaceIDs {
				j.WorkspaceIDs[i] = strings.Repeat("x", 2048)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := lifecycleJobFixture()
			tt.alter(job)
			q := &sqsLifecyclePurgeQueue{client: &lifecycleSQSStub{send: func(context.Context, *sqs.SendMessageInput) (*sqs.SendMessageOutput, error) {
				t.Fatal("invalid job reached SQS")
				return nil, errors.New("unexpected call")
			}}}
			if q.Enqueue(context.Background(), job) == nil {
				t.Fatal("invalid job accepted")
			}
		})
	}
}

func TestLifecycleQueueReplaysAfterAcceptingHandlerDisappears(t *testing.T) {
	first, provider, _ := newLifecycleTestHandler(t)
	persisted := make(chan *LifecyclePurgeJob, 1)
	completed := make(chan struct{})
	queue := &lifecycleQueueStub{
		enqueue: func(_ context.Context, job *LifecyclePurgeJob) error { persisted <- job; return nil },
		receive: func(ctx context.Context) (*LifecyclePurgeDelivery, error) {
			select {
			case job := <-persisted:
				return &LifecyclePurgeDelivery{Job: job, Receipt: "persisted-receipt"}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
		delete: func(_ context.Context, receipt string) error {
			if receipt != "persisted-receipt" {
				t.Error("wrong receipt completed")
			}
			close(completed)
			return nil
		},
	}
	first.cfg.LifecyclePurgeQueue = queue
	recorder := httptest.NewRecorder()
	body := appUninstalledBody(testAdminTeamID)
	first.ServeHTTP(recorder, newSignedRequest(t, pathSlackEvents, body, body))
	if recorder.Code != http.StatusOK || len(persisted) != 1 || provider.deleteStateCalls != 0 {
		t.Fatal("ACK must leave the job durably accepted before any worker runs")
	}
	// No work from the first Handler survives. A new Handler sees only the
	// external queue and stores, as after a process crash following its 200.
	ctx, cancel := context.WithCancel(context.Background())
	cfg := first.cfg
	cfg.BaseContext = ctx
	restarted := NewHandler(cfg)
	restarted.StartLifecyclePurgeWorkers()
	restarted.StartLifecyclePurgeWorkers() // repeated wiring must not grow the pool
	select {
	case <-completed:
	case <-time.After(3 * time.Second):
		cancel()
		restarted.Wait()
		t.Fatal("accepted delivery was not replayed")
	}
	cancel()
	restarted.Wait()
	if provider.deleteStateCalls != 1 {
		t.Fatalf("workspace-state delete calls=%d", provider.deleteStateCalls)
	}
	if provider.deleteStateCutoff.IsZero() {
		t.Fatal("original cutoff was lost")
	}
	_, _, err := restarted.cfg.AdminStore.ListAdmins(context.Background(), testAdminTeamID)
	var storeError *slackdata.Error
	if !errors.As(err, &storeError) || storeError.StatusCode != http.StatusNotFound {
		t.Fatal("workspace mapping survived replay")
	}
	assertLifecycleAgentStatePurged(t, restarted.cfg.AgentStore, testAdminTeamID)
}

func TestLifecycleQueueAcceptsBurstWithGeneralPoolSaturated(t *testing.T) {
	h, provider, _ := newLifecycleTestHandler(t)
	h.sem = make(chan struct{}, 1)
	h.sem <- struct{}{}
	accepted := 0
	h.cfg.LifecyclePurgeQueue = &lifecycleQueueStub{enqueue: func(_ context.Context, job *LifecyclePurgeJob) error { accepted++; return job.validate() }}
	for i := 0; i < 100; i++ {
		recorder := httptest.NewRecorder()
		body := appUninstalledBody(testAdminTeamID)
		h.ServeHTTP(recorder, newSignedRequest(t, pathSlackEvents, body, body))
		if recorder.Code != http.StatusOK {
			t.Fatalf("burst delivery %d status=%d", i, recorder.Code)
		}
	}
	if accepted != 100 || provider.deleteStateCalls != 0 || h.activeWorkers.Load() != 0 {
		t.Fatal("general saturation dropped a signal or spawned request purge goroutines")
	}
}

func TestLifecycleWorkerPoolStartsOnlyOnceAndStopsOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := NewHandler(Config{BaseContext: ctx, LifecyclePurgeQueue: &lifecycleQueueStub{}})
	h.StartLifecyclePurgeWorkers()
	h.StartLifecyclePurgeWorkers()
	if h.activeWorkers.Load() != int64(lifecyclePurgeWorkers) {
		cancel()
		h.Wait()
		t.Fatal("repeated startup grew the bounded lifecycle pool")
	}
	cancel()
	if !h.WaitTimeout(time.Second) {
		t.Fatal("queue receivers did not stop during shutdown")
	}
}

func TestLifecycleQueueRealSDKLoopbackProtocol(t *testing.T) {
	var savedBody string
	var operations []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		operation := r.Header.Get("X-Amz-Target")
		operations = append(operations, operation)
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		var input map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		switch operation {
		case "AmazonSQS.SendMessage":
			if err := json.Unmarshal(input["MessageBody"], &savedBody); err != nil {
				t.Error(err)
			}
			_, _ = w.Write([]byte(`{"MessageId":"local-message"}`))
		case "AmazonSQS.ReceiveMessage":
			response := map[string]any{"Messages": []map[string]string{{"Body": savedBody, "ReceiptHandle": "local-receipt", "MessageId": "local-message"}}}
			_ = json.NewEncoder(w).Encode(response)
		case "AmazonSQS.DeleteMessage":
			var receipt string
			_ = json.Unmarshal(input["ReceiptHandle"], &receipt)
			if receipt != "local-receipt" {
				t.Error("wrong receipt deleted")
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Error("unexpected SQS operation")
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	sdk := sqs.NewFromConfig(aws.Config{Region: "us-east-2", HTTPClient: server.Client(), Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: "local", SecretAccessKey: "local"}, nil
	})}, func(options *sqs.Options) {
		options.BaseEndpoint = aws.String(server.URL)
		options.RetryMaxAttempts = 1
	})
	queue := &sqsLifecyclePurgeQueue{client: sdk, url: server.URL + "/000000000000/fixture.fifo"}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := queue.Enqueue(ctx, lifecycleJobFixture()); err != nil {
		t.Fatal(err)
	}
	delivery, err := queue.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !delivery.Job.Cutoff.Equal(lifecycleJobFixture().Cutoff) {
		t.Fatal("real SDK roundtrip changed the original cutoff")
	}
	if err := queue.Delete(ctx, delivery.Receipt); err != nil {
		t.Fatal(err)
	}
	if len(operations) != 3 {
		t.Fatalf("operations=%d", len(operations))
	}
}

func TestLifecycleWorkerRetainsFailedPurgeWithoutCompletingDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	received := false
	queue := &lifecycleQueueStub{
		receive: func(context.Context) (*LifecyclePurgeDelivery, error) {
			if received {
				cancel()
				return nil, context.Canceled
			}
			received = true
			return &LifecyclePurgeDelivery{Job: lifecycleJobFixture(), Receipt: "retained"}, nil
		},
		delete: func(context.Context, string) error { t.Fatal("incomplete purge acknowledged"); return nil },
	}
	// Missing mandatory stores forces a terminal processing error before any deletion.
	h := NewHandler(Config{BaseContext: ctx, LifecyclePurgeQueue: queue})
	h.runLifecyclePurgeWorker(ctx)
	if !received {
		t.Fatal("worker never received delivery")
	}
}

func TestLifecycleHandlerWaitsForDurableAcceptanceBeforeACK(t *testing.T) {
	h, provider, _ := newLifecycleTestHandler(t)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	h.cfg.LifecyclePurgeQueue = &lifecycleQueueStub{enqueue: func(context.Context, *LifecyclePurgeJob) error {
		close(entered)
		<-release
		return nil
	}}
	recorder := httptest.NewRecorder()
	go func() { h.handleEvent(recorder, []byte(appUninstalledBody(testAdminTeamID))); close(done) }()
	<-entered
	select {
	case <-done:
		t.Fatal("ACK returned before persistence")
	default:
	}
	close(release)
	<-done
	if recorder.Code != http.StatusOK || provider.deleteStateCalls != 0 {
		t.Fatal("handoff started untracked cleanup or failed ACK")
	}
}

func TestLifecycleQueueURLTrustBoundary(t *testing.T) {
	for _, value := range []string{"", "http://sqs.example.invalid/work.fifo", "https://sqs.example.invalid/work", "https://user:password@sqs.example.invalid/work.fifo", "https://sqs.example.invalid/work.fifo?token=synthetic", "https://sqs.example.invalid/work.fifo#fragment", "%invalid"} {
		if _, err := NewLifecyclePurgeQueue(context.Background(), value); err == nil {
			t.Fatal("invalid queue URL accepted")
		}
	}
	t.Setenv("AWS_REGION", "us-east-2")
	t.Setenv("AWS_ACCESS_KEY_ID", "synthetic")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "synthetic")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	for _, value := range []string{"https://sqs.example.invalid/work.fifo", "http://127.0.0.1:8000/work.fifo"} {
		if _, err := NewLifecyclePurgeQueue(context.Background(), value); err != nil {
			t.Fatalf("valid queue URL rejected: %v", err)
		}
	}
}
