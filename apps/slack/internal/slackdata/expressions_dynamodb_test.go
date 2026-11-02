package slackdata

import (
	"context"
	"crypto/rand"
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

// CI supplies the existing local service; real parsing catches expressions the fakes accept.
func TestSlackdataDynamoDBExpressions(t *testing.T) {
	endpoint := os.Getenv("QURL_TEST_DYNAMODB_ENDPOINT")
	if endpoint == "" {
		t.Skip("set QURL_TEST_DYNAMODB_ENDPOINT to run the DynamoDB Local contract")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || !net.ParseIP(u.Hostname()).IsLoopback() || u.User != nil {
		t.Fatal("DynamoDB fixture requires an HTTP loopback endpoint")
	}
	client := dynamodb.New(dynamodb.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(endpoint),
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "local", SecretAccessKey: "local"}, nil
		}),
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	create := func(keys ...string) string {
		t.Helper()
		name := "qurl-expressions-" + strings.ToLower(rand.Text())
		input := &dynamodb.CreateTableInput{TableName: aws.String(name), BillingMode: ddbtypes.BillingModePayPerRequest}
		for i, key := range keys {
			kind := ddbtypes.KeyTypeHash
			if i > 0 {
				kind = ddbtypes.KeyTypeRange
			}
			input.KeySchema = append(input.KeySchema, ddbtypes.KeySchemaElement{AttributeName: aws.String(key), KeyType: kind})
			input.AttributeDefinitions = append(input.AttributeDefinitions, ddbtypes.AttributeDefinition{AttributeName: aws.String(key), AttributeType: ddbtypes.ScalarAttributeTypeS})
		}
		if _, err := client.CreateTable(ctx, input); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
			defer done()
			if _, err := client.DeleteTable(cleanup, &dynamodb.DeleteTableInput{TableName: aws.String(name)}); err != nil {
				t.Errorf("delete owned fixture: %v", err)
			}
		})
		return name
	}
	s := &Store{Client: client, WorkspaceMappingsName: create(attrSlackTeamID), ChannelPoliciesName: create(attrSlackTeamID, attrSlackChannelID)}
	a, err := NewAgentStore(client, create(attrAgentPK, attrAgentSK))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	s.Now = func() time.Time { return now }
	a.Now = func() time.Time { return now }
	const team, channel, owner, admin = "T12345678", "C12345678", "U12345678", "U87654321"
	check := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Run("workspace", func(t *testing.T) {
		check(t, s.BindWorkspace(ctx, &WorkspaceMapping{TeamID: team, OwnerID: owner}, owner))
		check(t, s.SetAgentEnabled(ctx, team, true))
		check(t, s.AddAdmin(ctx, team, admin))
		check(t, s.RemoveAdmin(ctx, team, admin))
		check(t, s.TransferOwnership(ctx, team, owner, admin))
		actual, _, err := s.ListAdmins(ctx, team)
		check(t, err)
		if actual != admin {
			t.Fatalf("owner = %q", actual)
		}
	})
	t.Run("aliases and policy sets", func(t *testing.T) {
		check(t, s.ExposeResourceToChannel(ctx, team, channel, "resource-a"))
		check(t, s.BindChannelAlias(ctx, team, channel, "size", "resource-a"))
		actual, found, err := s.LookupChannelAlias(ctx, team, channel, "size")
		check(t, err)
		if !found || actual != "resource-a" {
			t.Fatalf("alias = %q, found=%v", actual, found)
		}
		check(t, s.UnbindChannelAlias(ctx, team, channel, "size"))
		check(t, s.BindChannelAlias(ctx, team, channel, "ttl", "resource-a"))
		channels, err := s.ChannelsForResource(ctx, team, "resource-a")
		check(t, err)
		if len(channels) != 1 || channels[0] != channel {
			t.Fatalf("channels = %v", channels)
		}
		aliases, err := s.PurgeResourceFromChannel(ctx, team, channel, "resource-a")
		check(t, err)
		if len(aliases) != 1 || aliases[0] != "ttl" {
			t.Fatalf("purged aliases = %v", aliases)
		}
		check(t, s.ExposeResourceToChannel(ctx, team, channel, "resource-b"))
		check(t, s.RevokeResourceFromChannel(ctx, team, channel, "resource-b"))
		check(t, s.PurgeTeamChannelPoliciesBefore(ctx, team, now.Add(time.Minute)))
	})
	t.Run("agent counter and markers", func(t *testing.T) {
		for want := int64(1); want <= 2; want++ {
			count, err := a.BumpTurnCount(ctx, team, "user", time.Minute)
			check(t, err)
			if count != want {
				t.Fatalf("count = %d, want %d", count, want)
			}
		}
		for _, want := range []bool{true, false} {
			first, err := a.MarkEventSeen(ctx, team, "event")
			check(t, err)
			if first != want {
				t.Fatalf("first = %v, want %v", first, want)
			}
		}
		first, err := a.MarkMediaNoticeSent(ctx, team, "conversation")
		check(t, err)
		if !first {
			t.Fatal("first media notice rejected")
		}
		check(t, a.PutPendingAction(ctx, team, "action", []byte("synthetic")))
		claimed, err := a.ClaimPendingAction(ctx, team, "action")
		check(t, err)
		if !claimed {
			t.Fatal("pending action not claimed")
		}
		check(t, a.PurgeWorkspaceAgentState(ctx, team))
	})
	check(t, s.DeleteWorkspaceMappingBefore(ctx, team, now.Add(time.Minute)))
}
