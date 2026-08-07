package teamsdata

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestSavePersonalConversationRefUsesAtomicMapKeyUpdate(t *testing.T) {
	var getCalls int
	var captured *dynamodb.UpdateItemInput
	store := &Store{
		Client: &workspaceStubDDBClient{
			getItemFunc: func(context.Context, *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
				getCalls++
				return &dynamodb.GetItemOutput{}, nil
			},
			updateItemFunc: func(_ context.Context, params *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error) {
				captured = params
				return &dynamodb.UpdateItemOutput{}, nil
			},
		},
		WorkspaceMappingsName: "workspaces",
		Now: func() time.Time {
			return time.Unix(1_700_000_000, 0).UTC()
		},
	}

	err := store.SavePersonalConversationRef(context.Background(), "tenant-1", "user-1", &PersonalConversationRef{
		ServiceURL:     "https://service.example.test",
		ConversationID: "conv-1",
	})
	if err != nil {
		t.Fatalf("SavePersonalConversationRef error = %v", err)
	}
	if getCalls != 0 {
		t.Fatalf("GetItem calls = %d, want 0", getCalls)
	}
	if captured == nil {
		t.Fatal("expected UpdateItem call")
	}
	if got := aws.ToString(captured.UpdateExpression); !strings.Contains(got, "personal_conversation_refs.#actor = :ref") {
		t.Fatalf("UpdateExpression = %q, want atomic actor slot update", got)
	}
	if got := aws.ToString(captured.ConditionExpression); got != "attribute_exists(teams_tenant_id)" {
		t.Fatalf("ConditionExpression = %q", got)
	}
	if got := captured.ExpressionAttributeNames["#actor"]; got != "user-1" {
		t.Fatalf("ExpressionAttributeNames[#actor] = %q, want user-1", got)
	}
	refValue, ok := captured.ExpressionAttributeValues[":ref"].(*ddbtypes.AttributeValueMemberS)
	if !ok {
		t.Fatalf(":ref value type = %T, want string", captured.ExpressionAttributeValues[":ref"])
	}
	if !strings.Contains(refValue.Value, `"conversation_id":"conv-1"`) || !strings.Contains(refValue.Value, `"service_url":"https://service.example.test"`) {
		t.Fatalf(":ref value = %q", refValue.Value)
	}
}

type workspaceStubDDBClient struct {
	getItemFunc    func(ctx context.Context, params *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error)
	putItemFunc    func(ctx context.Context, params *dynamodb.PutItemInput) (*dynamodb.PutItemOutput, error)
	updateItemFunc func(ctx context.Context, params *dynamodb.UpdateItemInput) (*dynamodb.UpdateItemOutput, error)
	deleteItemFunc func(ctx context.Context, params *dynamodb.DeleteItemInput) (*dynamodb.DeleteItemOutput, error)
	queryFunc      func(ctx context.Context, params *dynamodb.QueryInput) (*dynamodb.QueryOutput, error)
}

func (s *workspaceStubDDBClient) GetItem(ctx context.Context, params *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	if s.getItemFunc != nil {
		return s.getItemFunc(ctx, params)
	}
	return &dynamodb.GetItemOutput{}, nil
}

func (s *workspaceStubDDBClient) PutItem(ctx context.Context, params *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	if s.putItemFunc != nil {
		return s.putItemFunc(ctx, params)
	}
	return &dynamodb.PutItemOutput{}, nil
}

func (s *workspaceStubDDBClient) UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error) {
	if s.updateItemFunc != nil {
		return s.updateItemFunc(ctx, params)
	}
	return &dynamodb.UpdateItemOutput{}, nil
}

func (s *workspaceStubDDBClient) DeleteItem(ctx context.Context, params *dynamodb.DeleteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error) {
	if s.deleteItemFunc != nil {
		return s.deleteItemFunc(ctx, params)
	}
	return &dynamodb.DeleteItemOutput{}, nil
}

func (s *workspaceStubDDBClient) Query(ctx context.Context, params *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	if s.queryFunc != nil {
		return s.queryFunc(ctx, params)
	}
	return &dynamodb.QueryOutput{}, nil
}
