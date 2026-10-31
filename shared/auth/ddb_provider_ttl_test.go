package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestCredentialReadsReportUnexpectedTTLWithoutRowData(t *testing.T) {
	readers := []struct {
		name string
		read func(*testing.T, *DDBProvider) error
	}{
		{"full API key", func(t *testing.T, p *DDBProvider) error {
			_, err := p.fetchAPIKeyItem(context.Background(), "T_PRIVATE_WORKSPACE", false, "test")
			return err
		}},
		{"cached API key", func(t *testing.T, p *DDBProvider) error {
			current, err := p.cachedAPIKeyStillCurrent(context.Background(), "T_PRIVATE_WORKSPACE", newAPIKeyCacheToken([]byte("api-secret"), []byte(passthroughWrappedKey)))
			if err == nil && !current {
				t.Error("TTL detection changed key validation")
			}
			return err
		}},
		{"Slack token", func(t *testing.T, p *DDBProvider) error {
			token, err := p.SlackBotToken(context.Background(), "T_PRIVATE_WORKSPACE")
			if err == nil && token != "slack-secret" {
				t.Error("TTL detection changed credential read")
			}
			return err
		}},
	}
	for _, reader := range readers {
		t.Run(reader.name, func(t *testing.T) {
			for name, ttl := range map[string]ddbtypes.AttributeValue{
				"absent":     nil,
				"numeric":    &ddbtypes.AttributeValueMemberN{Value: "1999999999"},
				"wrong type": &ddbtypes.AttributeValueMemberS{Value: "secret-ttl-value"},
				"null":       &ddbtypes.AttributeValueMemberNULL{Value: true},
			} {
				t.Run(name, func(t *testing.T) {
					var logs bytes.Buffer
					previous := slog.Default()
					slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
					t.Cleanup(func() { slog.SetDefault(previous) })
					item := map[string]ddbtypes.AttributeValue{
						attrTeamID:          &ddbtypes.AttributeValueMemberS{Value: "T_PRIVATE_WORKSPACE"},
						attrQURLAPIKey:      &ddbtypes.AttributeValueMemberB{Value: []byte("api-secret")},
						attrDataKeyCT:       &ddbtypes.AttributeValueMemberB{Value: []byte(passthroughWrappedKey)},
						attrSlackBotToken:   &ddbtypes.AttributeValueMemberB{Value: []byte("slack-secret")},
						attrSlackBotTokenDK: &ddbtypes.AttributeValueMemberB{Value: []byte(passthroughWrappedKey)},
					}
					if ttl != nil {
						item["ttl"] = ttl
					}
					ddb := &fakeDDBClient{getFunc: func(_ context.Context, in *dynamodb.GetItemInput) (*dynamodb.GetItemOutput, error) {
						if in.ProjectionExpression == nil {
							return &dynamodb.GetItemOutput{Item: item}, nil
						}
						projected := map[string]ddbtypes.AttributeValue{}
						for alias, attr := range in.ExpressionAttributeNames {
							if strings.Contains(aws.ToString(in.ProjectionExpression), alias) {
								if value, ok := item[attr]; ok {
									projected[attr] = value
								}
							}
						}
						return &dynamodb.GetItemOutput{Item: projected}, nil
					}}
					p := &DDBProvider{Client: ddb, TableName: "private-table", Encryptor: &passthroughEncryptor{}}
					if err := reader.read(t, p); err != nil {
						t.Fatal(err)
					}
					if ttl == nil {
						if logs.Len() != 0 {
							t.Fatalf("clean credential emitted signal: %s", logs.String())
						}
						return
					}
					var event map[string]any
					if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
						t.Fatalf("missing JSON signal: %v (%s)", err, logs.String())
					}
					if event["security_event"] != "workspace_credential_ttl_present" || event["level"] != "WARN" || len(event) != 4 {
						t.Fatalf("unexpected signal fields: %v", event)
					}
					for _, secret := range []string{"T_PRIVATE_WORKSPACE", "api-secret", "slack-secret", "private-table", "secret-ttl-value", "1999999999"} {
						if strings.Contains(logs.String(), secret) {
							t.Errorf("signal leaked %q", secret)
						}
					}
				})
			}
		})
	}
}
