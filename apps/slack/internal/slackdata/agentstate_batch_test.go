package slackdata

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func seedAgentBatchRows(fake *agentFakeDDB, partition string, count int) {
	for i := 0; i < count; i++ {
		sk := fmt.Sprintf("pend#%03d", i)
		fake.items[partition+"|"+sk] = map[string]ddbtypes.AttributeValue{attrAgentPK: stringAttr(partition), attrAgentSK: stringAttr(sk)}
	}
}

func TestAgentPurgeBatchesAreBoundedAndPartitionScoped(t *testing.T) {
	fake := newAgentFakeDDB()
	seedAgentBatchRows(fake, "T1", 57)
	seedAgentBatchRows(fake, "T2", 7)
	var sizes []int
	fake.batchFunc = func(_ context.Context, in *dynamodb.BatchWriteItemInput) (*dynamodb.BatchWriteItemOutput, error) {
		if len(in.RequestItems) != 1 {
			t.Fatal("batch includes unexpected tables")
		}
		requests := in.RequestItems["agent_state"]
		sizes = append(sizes, len(requests))
		for _, request := range requests {
			if request.DeleteRequest == nil || request.PutRequest != nil {
				t.Fatal("purge must only delete")
			}
			if readString(request.DeleteRequest.Key, attrAgentPK) != "T1" {
				t.Fatal("purge crossed partition")
			}
			delete(fake.items, keyOf(request.DeleteRequest.Key))
		}
		return &dynamodb.BatchWriteItemOutput{}, nil
	}
	if err := newTestAgentStore(fake).PurgeWorkspaceAgentState(context.Background(), "T1"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sizes, []int{25, 25, 7}) || fake.deleteCalls != 0 || len(fake.items) != 7 {
		t.Fatalf("sizes=%v single deletes=%d remaining=%d", sizes, fake.deleteCalls, len(fake.items))
	}
}

func TestAgentPurgeRetriesOnlyUnprocessedDeletes(t *testing.T) {
	fake := newAgentFakeDDB()
	seedAgentBatchRows(fake, "T1", 3)
	var sizes []int
	fake.batchFunc = func(_ context.Context, in *dynamodb.BatchWriteItemInput) (*dynamodb.BatchWriteItemOutput, error) {
		requests := in.RequestItems["agent_state"]
		sizes = append(sizes, len(requests))
		if fake.batchCalls == 1 {
			for _, request := range requests[:2] {
				delete(fake.items, keyOf(request.DeleteRequest.Key))
			}
			requests = requests[2:]
		}
		if readString(requests[0].DeleteRequest.Key, attrAgentSK) != "pend#002" {
			t.Fatal("retry included a completed delete")
		}
		if fake.batchCalls < 3 {
			return &dynamodb.BatchWriteItemOutput{UnprocessedItems: map[string][]ddbtypes.WriteRequest{"agent_state": requests}}, nil
		}
		delete(fake.items, keyOf(requests[0].DeleteRequest.Key))
		return &dynamodb.BatchWriteItemOutput{}, nil
	}
	if err := newTestAgentStore(fake).PurgeWorkspaceAgentState(context.Background(), "T1"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sizes, []int{3, 1, 1}) || len(fake.items) != 0 {
		t.Fatalf("sizes=%v remaining=%d", sizes, len(fake.items))
	}
}

func TestAgentPurgeExhaustionReportsResidue(t *testing.T) {
	fake := newAgentFakeDDB()
	seedAgentBatchRows(fake, "T1", 2)
	fake.batchFunc = func(_ context.Context, in *dynamodb.BatchWriteItemInput) (*dynamodb.BatchWriteItemOutput, error) {
		return &dynamodb.BatchWriteItemOutput{UnprocessedItems: in.RequestItems}, nil
	}
	err := newTestAgentStore(fake).PurgeWorkspaceAgentState(context.Background(), "T1")
	var stateErr *Error
	if !errors.As(err, &stateErr) || stateErr.StatusCode != http.StatusServiceUnavailable || !strings.Contains(err.Error(), "2 deletes unprocessed after 4 attempts") {
		t.Fatalf("unexpected residue error: %v", err)
	}
	if fake.batchCalls != 4 || len(fake.items) != 2 {
		t.Fatalf("calls=%d remaining=%d", fake.batchCalls, len(fake.items))
	}
}

func TestAgentPurgeContinuesAfterBatchFailure(t *testing.T) {
	fake := newAgentFakeDDB()
	seedAgentBatchRows(fake, "T1", 30)
	fake.batchFunc = func(_ context.Context, in *dynamodb.BatchWriteItemInput) (*dynamodb.BatchWriteItemOutput, error) {
		if fake.batchCalls == 1 {
			return nil, errors.New("batch unavailable")
		}
		for _, request := range in.RequestItems["agent_state"] {
			delete(fake.items, keyOf(request.DeleteRequest.Key))
		}
		return &dynamodb.BatchWriteItemOutput{}, nil
	}
	err := newTestAgentStore(fake).PurgeWorkspaceAgentState(context.Background(), "T1")
	var stateErr *Error
	if !errors.As(err, &stateErr) || stateErr.StatusCode != http.StatusServiceUnavailable || fake.batchCalls != 2 || len(fake.items) != 25 {
		t.Fatalf("err=%v calls=%d remaining=%d", err, fake.batchCalls, len(fake.items))
	}
}

func TestAgentPurgeCancellationStopsRequests(t *testing.T) {
	for _, before := range []bool{false, true} {
		t.Run(strconv.FormatBool(before), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fake := newAgentFakeDDB()
			seedAgentBatchRows(fake, "T1", 2)
			fake.batchFunc = func(_ context.Context, in *dynamodb.BatchWriteItemInput) (*dynamodb.BatchWriteItemOutput, error) {
				cancel()
				return &dynamodb.BatchWriteItemOutput{UnprocessedItems: in.RequestItems}, nil
			}
			if before {
				cancel()
				fake.queryErr = errors.New("must not query canceled context")
			}
			err := newTestAgentStore(fake).PurgeWorkspaceAgentState(ctx, "T1")
			wantCalls := 1
			if before {
				wantCalls = 0
			}
			if !errors.Is(err, context.Canceled) || fake.batchCalls != wantCalls || len(fake.items) != 2 {
				t.Fatalf("err=%v calls=%d remaining=%d", err, fake.batchCalls, len(fake.items))
			}
		})
	}
}

func TestAgentPurgeEmptyBatchResponseFails(t *testing.T) {
	fake := newAgentFakeDDB()
	seedAgentBatchRows(fake, "T1", 1)
	fake.batchFunc = func(context.Context, *dynamodb.BatchWriteItemInput) (*dynamodb.BatchWriteItemOutput, error) {
		return nil, nil //nolint:nilnil // Deliberately model a broken SDK response.
	}
	err := newTestAgentStore(fake).PurgeWorkspaceAgentState(context.Background(), "T1")
	if err == nil || !strings.Contains(err.Error(), "empty batch response") || len(fake.items) != 1 {
		t.Fatalf("err=%v remaining=%d", err, len(fake.items))
	}
}
