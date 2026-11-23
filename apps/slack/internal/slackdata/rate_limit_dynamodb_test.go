package slackdata

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestRateLimitDynamoDBClockSkew(t *testing.T) {
	endpoint := os.Getenv("QURL_TEST_DYNAMODB_ENDPOINT")
	if endpoint == "" {
		t.Skip("set QURL_TEST_DYNAMODB_ENDPOINT to run the DynamoDB Local contract")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || !net.ParseIP(u.Hostname()).IsLoopback() || u.User != nil {
		t.Fatal("DynamoDB fixture requires an HTTP loopback endpoint")
	}
	client := dynamodb.New(dynamodb.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(endpoint),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "local", SecretAccessKey: "local"}, nil
		}),
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	table := "qurl-limit-skew-" + strings.ToLower(rand.Text())
	_, err = client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(table), BillingMode: ddbtypes.BillingModePayPerRequest,
		KeySchema:            []ddbtypes.KeySchemaElement{{AttributeName: aws.String(attrSlackTeamID), KeyType: ddbtypes.KeyTypeHash}},
		AttributeDefinitions: []ddbtypes.AttributeDefinition{{AttributeName: aws.String(attrSlackTeamID), AttributeType: ddbtypes.ScalarAttributeTypeS}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := client.DeleteTable(cleanup, &dynamodb.DeleteTableInput{TableName: aws.String(table)}); err != nil {
			t.Errorf("delete owned fixture table: %v", err)
		}
	})
	_, err = client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(table), Item: map[string]ddbtypes.AttributeValue{
		attrSlackTeamID: stringAttr(testRateLimitTeamID),
	}})
	if err != nil {
		t.Fatal(err)
	}
	before := time.Date(2026, 6, 17, 12, 59, 59, 0, time.UTC)
	after := before.Add(2 * time.Second)
	slow, fast := newRateLimitStore(client), newRateLimitStore(client)
	for _, s := range []*Store{slow, fast} {
		s.WorkspaceMappingsName, s.RateLimitEnabled = table, true
	}
	slow.Now = func() time.Time { return before }
	fast.Now = func() time.Time { return after }
	allowed, _, err := fast.CheckRateLimit(ctx, testRateLimitUserID, testRateLimitTeamID)
	if err != nil || !allowed {
		t.Fatalf("seed peer window: allowed=%v err=%v", allowed, err)
	}
	type result struct {
		allowed bool
		retry   time.Duration
		err     error
	}
	results := make(chan result, 2*defaultRateLimitLimit)
	for i := range 2 * defaultRateLimitLimit {
		writer := fast
		if i%2 != 0 {
			writer = slow
		}
		go func(s *Store) {
			allowed, retry, err := s.CheckRateLimit(ctx, testRateLimitUserID, testRateLimitTeamID)
			results <- result{allowed, retry, err}
		}(writer)
	}
	granted := 1
	for range 2 * defaultRateLimitLimit {
		r := <-results
		if r.err != nil || (!r.allowed && r.retry <= 0) {
			t.Fatalf("concurrent writer: %#v", r)
		}
		if r.allowed {
			granted++
		}
	}
	key := map[string]ddbtypes.AttributeValue{attrSlackTeamID: stringAttr(rateLimitKey(testRateLimitTeamID, testRateLimitUserID))}
	state, err := client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(table), Key: key, ConsistentRead: aws.Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	if granted != defaultRateLimitLimit || readNumber(state.Item, attrRateLimitCount) != defaultRateLimitLimit ||
		readNumber(state.Item, attrRateLimitWindow) != after.Truncate(time.Hour).Unix() ||
		readNumber(state.Item, attrRateLimitExpiresAt) != after.Truncate(time.Hour).Add(2*time.Hour).Unix() {
		t.Fatalf("concurrent budget or peer window changed: grants=%d state=%#v", granted, state.Item)
	}
	item := cloneRateLimitTestItem(state.Item)
	item[attrRateLimitWindow], item[attrRateLimitCount] = numberAttr(1<<63-1), numberAttr(1)
	_, err = client.PutItem(ctx, &dynamodb.PutItemInput{TableName: aws.String(table), Item: item})
	if err != nil {
		t.Fatal(err)
	}
	allowed, retry, err := slow.CheckRateLimit(ctx, testRateLimitUserID, testRateLimitTeamID)
	var storeErr *Error
	if allowed || retry != 0 || !errors.As(err, &storeErr) || storeErr.Code != ErrCodeRateLimitClockSkew {
		t.Fatalf("corrupt window was not rejected: %v, %s, %v", allowed, retry, err)
	}
	state, err = client.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(table), Key: key, ConsistentRead: aws.Bool(true)})
	if err != nil || readNumber(state.Item, attrRateLimitWindow) != 1<<63-1 || readNumber(state.Item, attrRateLimitCount) != 1 ||
		readNumber(state.Item, attrRateLimitExpiresAt) != readNumber(item, attrRateLimitExpiresAt) {
		t.Fatalf("corrupt counter was repaired or consumed: %#v, %v", state, err)
	}
}
