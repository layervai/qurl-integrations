package matchedcohort

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/knock"
)

// DurableCycleKnocker adapts one offline-prepared operation to the existing
// qURL Connector supervisor. It admits exactly once. A physical reconnect may
// not reuse the operation: the NHP duplicate reply deliberately requires
// recovery, so the short canary cycle fails closed instead of opening another
// session behind the durable row.
type DurableCycleKnocker struct {
	mu sync.Mutex

	consumer             *Consumer
	key                  string
	resourceID           string
	runID                string
	expectedResourceHost string
	begun                bool
	knocked              bool
	ended                bool
	live                 *LiveSession
}

var _ knock.CycleKnocker = (*DurableCycleKnocker)(nil)

// NewDurableCycleKnocker validates the exact PREPARED record without network.
func NewDurableCycleKnocker(ctx context.Context, consumer *Consumer, key, resourceID string,
	selector FRPSSelector,
) (*DurableCycleKnocker, error) {
	if consumer == nil || consumer.Blobs == nil || !validText(key) || !validText(resourceID) ||
		!validDNS(selector.Host) || selector.Port < 1 || selector.Port > 65535 {
		return nil, fmt.Errorf("%w: durable knocker input", errInvalidAuthority)
	}
	record, _, err := loadOperation(ctx, consumer.Blobs, key)
	if err != nil {
		return nil, err
	}
	if record.Status != OperationPrepared || record.Operation.ResourceID != resourceID {
		return nil, fmt.Errorf("%w: durable knocker operation", errStateConflict)
	}
	return &DurableCycleKnocker{consumer: consumer, key: key, resourceID: resourceID, runID: record.Operation.RunID,
		expectedResourceHost: net.JoinHostPort(selector.Host, strconv.Itoa(selector.Port))}, nil
}

// BeginCycle binds the supervisor to the already persisted RunID.
func (k *DurableCycleKnocker) BeginCycle() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.begun || k.ended || k.runID == "" {
		return errors.New("durable matched-cohort cycle is not startable")
	}
	k.begun = true
	return nil
}

// CycleRunID returns the offline-prepared operation RunID.
func (k *DurableCycleKnocker) CycleRunID() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.begun || k.ended {
		return ""
	}
	return k.runID
}

// Knock admits the one exact operation and exposes only its short-lived result.
func (k *DurableCycleKnocker) Knock(ctx context.Context) (*knock.Result, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.begun || k.ended || k.knocked {
		return nil, errSessionRecoveryRequired
	}
	k.knocked = true
	live, err := k.consumer.Admit(ctx, k.key)
	if err != nil {
		return nil, err
	}
	if live.ResourceHost != k.expectedResourceHost {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, retireErr := k.consumer.Retire(cleanupCtx, k.key, live)
		terminal, recoverErr := k.consumer.Recover(cleanupCtx, k.key)
		var terminalErr error
		if terminal.State != OperationClosed {
			terminalErr = errSessionRecoveryRequired
		}
		return nil, errors.Join(fmt.Errorf("%w: authenticated resource host", errStateConflict), retireErr, recoverErr, terminalErr)
	}
	k.live = live
	return &knock.Result{ACTokens: map[string]string{k.resourceID: live.ACToken},
		ResourceHost: map[string]string{k.resourceID: live.ResourceHost}}, nil
}

// EndCycle retires a live exact session or recovers a dispatch that did not
// return. CLOSING stays nonterminal and requires a later recovery call.
func (k *DurableCycleKnocker) EndCycle(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.begun || k.ended {
		return nil
	}
	k.ended = true
	var terminal SessionTerminal
	var err error
	if k.live != nil {
		terminal, err = k.consumer.Retire(ctx, k.key, k.live)
		k.live = nil
	} else {
		terminal, err = k.consumer.Recover(ctx, k.key)
	}
	if err != nil {
		return err
	}
	if terminal.State != OperationClosed && terminal.State != OperationCanceled {
		return errSessionRecoveryRequired
	}
	return nil
}
