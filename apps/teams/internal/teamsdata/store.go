package teamsdata

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	EnvWorkspaceMappingsTable = "QURL_TEAMS_WORKSPACE_MAPPINGS_TABLE"
	EnvChannelPoliciesTable   = "QURL_TEAMS_CHANNEL_POLICIES_TABLE"
)

type DynamoDBClient interface {
	GetItem(ctx context.Context, params *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(ctx context.Context, params *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	UpdateItem(ctx context.Context, params *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	DeleteItem(ctx context.Context, params *dynamodb.DeleteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
	Query(ctx context.Context, params *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
}

type Store struct {
	Client                DynamoDBClient
	WorkspaceMappingsName string
	ChannelPoliciesName   string
	Now                   func() time.Time
}

type StoreOption func(*storeOptions)

type storeOptions struct {
	workspaceMappingsName string
	channelPoliciesName   string
	ddbClient             DynamoDBClient
	awsConfigFns          []func(*awsconfig.LoadOptions) error
}

func WithDynamoDBClient(c DynamoDBClient) StoreOption {
	return func(o *storeOptions) { o.ddbClient = c }
}

func WithTableNames(workspaceMappings, channelPolicies string) StoreOption {
	return func(o *storeOptions) {
		o.workspaceMappingsName = workspaceMappings
		o.channelPoliciesName = channelPolicies
	}
}

func NewStore(ctx context.Context, opts ...StoreOption) (*Store, error) {
	o := &storeOptions{}
	for _, fn := range opts {
		fn(o)
	}
	if o.workspaceMappingsName == "" {
		o.workspaceMappingsName = os.Getenv(EnvWorkspaceMappingsTable)
	}
	if o.channelPoliciesName == "" {
		o.channelPoliciesName = os.Getenv(EnvChannelPoliciesTable)
	}
	switch {
	case o.workspaceMappingsName == "":
		return nil, fmt.Errorf("teamsdata.NewStore: %s is required", EnvWorkspaceMappingsTable)
	case o.channelPoliciesName == "":
		return nil, fmt.Errorf("teamsdata.NewStore: %s is required", EnvChannelPoliciesTable)
	}
	if o.ddbClient == nil {
		cfg, err := awsconfig.LoadDefaultConfig(ctx, o.awsConfigFns...)
		if err != nil {
			return nil, fmt.Errorf("teamsdata.NewStore: load AWS config: %w", err)
		}
		o.ddbClient = dynamodb.NewFromConfig(cfg)
	}
	return &Store{
		Client:                o.ddbClient,
		WorkspaceMappingsName: o.workspaceMappingsName,
		ChannelPoliciesName:   o.channelPoliciesName,
		Now:                   time.Now,
	}, nil
}

type Error struct {
	StatusCode int
	Code       string
	Title      string
	Detail     string
}

func (e *Error) Error() string {
	codeSuffix := ""
	if e.Code != "" {
		codeSuffix = " [" + e.Code + "]"
	}
	if e.Detail != "" {
		return fmt.Sprintf("%s%s (%d): %s", e.Title, codeSuffix, e.StatusCode, e.Detail)
	}
	return fmt.Sprintf("%s%s (%d)", e.Title, codeSuffix, e.StatusCode)
}

func (s *Store) nowOrDefault() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func stringAttr(value string) ddbtypes.AttributeValue {
	return &ddbtypes.AttributeValueMemberS{Value: value}
}

func boolAttr(value bool) ddbtypes.AttributeValue {
	return &ddbtypes.AttributeValueMemberBOOL{Value: value}
}

func unixNanoAttr(t time.Time) ddbtypes.AttributeValue {
	return &ddbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", t.UnixNano())}
}

func readString(item map[string]ddbtypes.AttributeValue, key string) string {
	if s, ok := item[key].(*ddbtypes.AttributeValueMemberS); ok {
		return strings.TrimSpace(s.Value)
	}
	return ""
}

func readStringSet(item map[string]ddbtypes.AttributeValue, key string) []string {
	ss, ok := item[key].(*ddbtypes.AttributeValueMemberSS)
	if !ok || len(ss.Value) == 0 {
		return nil
	}
	out := make([]string, 0, len(ss.Value))
	for _, v := range ss.Value {
		v = strings.TrimSpace(v)
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

func readStringMap(item map[string]ddbtypes.AttributeValue, key string) map[string]string {
	m, ok := item[key].(*ddbtypes.AttributeValueMemberM)
	if !ok || len(m.Value) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(m.Value))
	for k, v := range m.Value {
		if s, ok := v.(*ddbtypes.AttributeValueMemberS); ok {
			out[k] = strings.TrimSpace(s.Value)
		}
	}
	return out
}

func ddbToError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{
		StatusCode: http.StatusServiceUnavailable,
		Title:      operation + ": DynamoDB operation failed",
		Detail:     err.Error(),
	}
}
