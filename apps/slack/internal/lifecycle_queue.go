package internal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

const (
	lifecyclePurgeJobVersion        = 1
	lifecyclePurgeWorkers           = 2
	lifecyclePurgeJobTimeout        = 90 * time.Second
	lifecyclePurgePublishTimeout    = 1500 * time.Millisecond
	lifecyclePurgeVisibilitySeconds = 120
	lifecyclePurgeMaxJobBytes       = maxRequestBodyBytes
)

// LifecyclePurgeJob carries only local partition identities and the original
// signed-event cutoff. Never enqueue the Slack envelope or its token fields.
type LifecyclePurgeJob struct {
	Version           int       `json:"version"`
	WorkspaceIDs      []string  `json:"workspace_ids"`
	AgentStateOnlyIDs []string  `json:"agent_state_only_ids,omitempty"`
	Cutoff            time.Time `json:"cutoff"`
}

// LifecyclePurgeDelivery binds a persisted job to its completion receipt.
type LifecyclePurgeDelivery struct {
	Job     *LifecyclePurgeJob
	Receipt string
}

// LifecyclePurgeQueue persists and replays metadata-only teardown work.
type LifecyclePurgeQueue interface {
	Enqueue(context.Context, *LifecyclePurgeJob) error
	Receive(context.Context) (*LifecyclePurgeDelivery, error)
	Delete(context.Context, string) error
}

type lifecycleSQSClient interface {
	SendMessage(context.Context, *sqs.SendMessageInput, ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
	ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

type sqsLifecyclePurgeQueue struct {
	client lifecycleSQSClient
	url    string
}

// NewLifecyclePurgeQueue validates the queue and builds the AWS client.
func NewLifecyclePurgeQueue(ctx context.Context, queueURL string) (LifecyclePurgeQueue, error) {
	parsed, err := url.Parse(queueURL)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || !strings.HasSuffix(parsed.Path, ".fifo") ||
		(parsed.Scheme != resourceExposeSchemeHTTPS && (parsed.Scheme != resourceExposeSchemeHTTP || (parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1"))) {
		return nil, errors.New("SLACK_LIFECYCLE_QUEUE_URL must name an HTTPS FIFO queue (loopback HTTP is allowed for local tests)")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRetryMaxAttempts(1))
	if err != nil {
		return nil, fmt.Errorf("load lifecycle queue AWS config: %w", err)
	}
	return &sqsLifecyclePurgeQueue{client: sqs.NewFromConfig(cfg), url: queueURL}, nil
}

func (job *LifecyclePurgeJob) validate() error {
	if job == nil || job.Version != lifecyclePurgeJobVersion || job.Cutoff.IsZero() || job.Cutoff.After(time.Now().Add(5*time.Minute)) || len(job.WorkspaceIDs)+len(job.AgentStateOnlyIDs) == 0 {
		return errors.New("invalid lifecycle purge job version, cutoff, or partitions")
	}
	for _, ids := range [][]string{job.WorkspaceIDs, job.AgentStateOnlyIDs} {
		for _, id := range ids {
			if id == "" || strings.TrimSpace(id) != id || len(id) > 2048 {
				return errors.New("invalid lifecycle purge partition identity")
			}
		}
	}
	return nil
}

// Enqueue requires a persistence receipt before the caller can ACK Slack.
func (q *sqsLifecyclePurgeQueue) Enqueue(ctx context.Context, job *LifecyclePurgeJob) error {
	if err := job.validate(); err != nil {
		return err
	}
	job.Cutoff = job.Cutoff.UTC()
	body, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("encode lifecycle purge job: %w", err)
	}
	if len(body) > lifecyclePurgeMaxJobBytes {
		return errors.New("lifecycle purge job exceeds the metadata size limit")
	}
	partition := job.WorkspaceIDs
	if len(partition) == 0 {
		partition = job.AgentStateOnlyIDs
	}
	group := sha256.Sum256([]byte(partition[0]))
	dedup := sha256.Sum256(body)
	ctx, cancel := context.WithTimeout(ctx, lifecyclePurgePublishTimeout)
	defer cancel()
	out, err := q.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl: aws.String(q.url), MessageBody: aws.String(string(body)),
		MessageGroupId: aws.String(hex.EncodeToString(group[:])), MessageDeduplicationId: aws.String(hex.EncodeToString(dedup[:])),
	})
	if err != nil {
		return fmt.Errorf("persist lifecycle purge job: %w", err)
	}
	if out == nil || aws.ToString(out.MessageId) == "" {
		return errors.New("lifecycle purge queue did not confirm persistence")
	}
	return nil
}

// Receive leases one job so FIFO ordering survives multiple workers.
func (q *sqsLifecyclePurgeQueue) Receive(ctx context.Context) (*LifecyclePurgeDelivery, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	// TODO(upstream-contract): FIFO groups stay serial only when a worker receives
	// one message at a time; queue visibility must exceed the job deadline (#927/#930).
	out, err := q.client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl: aws.String(q.url), MaxNumberOfMessages: 1, WaitTimeSeconds: 20, VisibilityTimeout: lifecyclePurgeVisibilitySeconds,
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, errors.New("lifecycle purge queue returned no response")
	}
	if len(out.Messages) == 0 {
		return nil, nil //nolint:nilnil // An empty long poll is a successful receive without work.
	}
	if len(out.Messages) != 1 {
		return nil, errors.New("lifecycle purge queue exceeded the one-message receive contract")
	}
	message := out.Messages[0]
	if message.Body == nil || len(*message.Body) > lifecyclePurgeMaxJobBytes || aws.ToString(message.ReceiptHandle) == "" {
		return nil, errors.New("lifecycle purge queue returned malformed delivery metadata")
	}
	var job *LifecyclePurgeJob
	if err := json.Unmarshal([]byte(*message.Body), &job); err != nil {
		return nil, errors.New("invalid lifecycle purge job encoding")
	}
	if err := job.validate(); err != nil {
		return nil, err
	}
	return &LifecyclePurgeDelivery{Job: job, Receipt: *message.ReceiptHandle}, nil
}

// Delete completes only a fully processed job.
func (q *sqsLifecyclePurgeQueue) Delete(ctx context.Context, receipt string) error {
	_, err := q.client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(q.url), ReceiptHandle: aws.String(receipt)})
	return err
}

// StartLifecyclePurgeWorkers starts a fixed pool, separate from slash/agent work.
// Cancellation leaves unfinished deliveries on the durable queue for replay.
func (h *Handler) StartLifecyclePurgeWorkers() {
	if h.cfg.LifecyclePurgeQueue == nil {
		return
	}
	h.lifecycleWorkersOnce.Do(func() {
		for range lifecyclePurgeWorkers {
			h.Go(func() { h.runLifecyclePurgeWorker(h.baseCtx) })
		}
	})
}

func (h *Handler) runLifecyclePurgeWorker(ctx context.Context) {
	for ctx.Err() == nil {
		delivery, err := h.cfg.LifecyclePurgeQueue.Receive(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("lifecycle purge queue receive failed", "cleanup_action_required", true)
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}
		if delivery == nil {
			continue
		}
		jobCtx, cancel := context.WithTimeout(ctx, lifecyclePurgeJobTimeout)
		err = h.processLifecyclePurgeJob(jobCtx, delivery.Job)
		cancel()
		if err != nil {
			slog.Error("durable lifecycle purge incomplete; delivery retained for replay", "cleanup_action_required", true)
			continue
		}
		deleteCtx, deleteCancel := context.WithTimeout(ctx, 5*time.Second)
		err = h.cfg.LifecyclePurgeQueue.Delete(deleteCtx, delivery.Receipt)
		deleteCancel()
		if err != nil {
			slog.Error("lifecycle purge queue completion failed; delivery retained for replay", "cleanup_action_required", true)
		}
	}
}

func (h *Handler) processLifecyclePurgeJob(ctx context.Context, job *LifecyclePurgeJob) error {
	if err := job.validate(); err != nil {
		return err
	}
	if h.cfg.AgentStore == nil || (len(job.WorkspaceIDs) > 0 && h.cfg.AdminStore == nil) {
		return errors.New("durable lifecycle purge stores are not configured")
	}
	if len(job.WorkspaceIDs) > 0 {
		switch h.cfg.AuthProvider.(type) {
		case workspaceStateBeforeIdentityDeleter, workspaceStateIdentityDeleter, workspaceStateDeleter:
		default:
			return errors.New("durable lifecycle purge requires a workspace-state deleter")
		}
	}
	// shortcut: oversized partitions restart on retry; add persisted cursors if measured jobs cannot drain before redrive.
	var failures []error
	log := slog.With("surface", "lifecycle")
	for _, id := range job.WorkspaceIDs {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		purgeCtx, cancel := context.WithTimeout(ctx, lifecyclePurgeTimeout)
		err := h.purgeWorkspaceWithRetry(purgeCtx, log.With("workspace_id", id), id, job.Cutoff, purgeScopeFull)
		cancel()
		failures = append(failures, err)
	}
	for _, id := range job.AgentStateOnlyIDs {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		purgeCtx, cancel := context.WithTimeout(ctx, lifecyclePurgeTimeout)
		err := h.purgeAgentStateWithRetry(purgeCtx, log.With("workspace_id", id, "agent_state_only", true), id)
		cancel()
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}
