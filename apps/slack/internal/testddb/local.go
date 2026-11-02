// Package testddb provides loopback-only DynamoDB fixtures. Import it only from tests.
package testddb

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// New creates isolated workspace and policy tables, removed when the test ends.
func New(t *testing.T) (client *dynamodb.Client, workspaceTable, policyTable string) {
	t.Helper()
	endpoint := os.Getenv("QURL_TEST_DYNAMODB_LOCAL_ENDPOINT")
	if endpoint == "" {
		t.Skip("set QURL_TEST_DYNAMODB_LOCAL_ENDPOINT to run native policy transactions")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Port() == "" || !net.ParseIP(parsed.Hostname()).IsLoopback() || (parsed.Path != "" && parsed.Path != "/") {
		t.Fatal("policy tests require an explicit loopback DynamoDB Local endpoint")
	}
	transport := &http.Transport{Proxy: nil}
	t.Cleanup(transport.CloseIdleConnections)
	client = dynamodb.New(dynamodb.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(endpoint), RetryMaxAttempts: 1,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "local", SecretAccessKey: "local"}, nil
		}),
		HTTPClient: &http.Client{Transport: transport},
	})
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	workspaceTable, policyTable = "slack931-workspace-"+suffix, "slack931-policy-"+suffix
	for _, table := range []struct {
		name string
		sort bool
	}{{workspaceTable, false}, {policyTable, true}} {
		attributes := []ddbtypes.AttributeDefinition{{AttributeName: aws.String("slack_team_id"), AttributeType: ddbtypes.ScalarAttributeTypeS}}
		keys := []ddbtypes.KeySchemaElement{{AttributeName: aws.String("slack_team_id"), KeyType: ddbtypes.KeyTypeHash}}
		if table.sort {
			attributes = append(attributes, ddbtypes.AttributeDefinition{AttributeName: aws.String("slack_channel_id"), AttributeType: ddbtypes.ScalarAttributeTypeS})
			keys = append(keys, ddbtypes.KeySchemaElement{AttributeName: aws.String("slack_channel_id"), KeyType: ddbtypes.KeyTypeRange})
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := client.CreateTable(ctx, &dynamodb.CreateTableInput{TableName: aws.String(table.name), AttributeDefinitions: attributes, KeySchema: keys, BillingMode: ddbtypes.BillingModePayPerRequest})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := client.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String(table.name)}); err != nil {
				t.Errorf("delete local test table: %v", err)
			}
		})
	}
	return client, workspaceTable, policyTable
}
