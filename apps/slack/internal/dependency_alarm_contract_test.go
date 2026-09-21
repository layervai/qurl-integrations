package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/kms"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
	"github.com/layervai/qurl-integrations/shared/observability"
)

// TODO(upstream-contract): qurl-integrations-infra#964 requires ERROR and a
// top-level error containing the real client's "http request:" transport wrap.
func TestHandleGet_DependencyTransportAlarmContract(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
	h := newAdminTestHandler(t, ts)
	h.cfg.NewClient = func(key string) *client.Client {
		return client.New(ts.customerServer.URL, key, client.WithRetry(0), client.WithHTTPClient(&http.Client{
			Transport: testRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("dial tcp: connection refused")
			}),
		}))
	}
	logs := &capturedLogs{}
	previous := slog.Default()
	slog.SetDefault(slog.New(observability.NewRedactingJSONHandler(logs, nil)))
	t.Cleanup(func() { h.Wait(); slog.SetDefault(previous) })
	inv := newAdminSlashInvoker(t, h)
	_, _, reply := inv.invokeAdminAsync("get $prod-db", testAdminTeamID, testAdminUserID)
	if !strings.Contains(reply, "Could not reach qURL") {
		t.Fatalf("unexpected reply: %s", reply)
	}
	h.Wait()
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		text, _ := record["error"].(string)
		if strings.Contains(text, "http request:") && strings.Contains(text, "connection refused") {
			if record["msg"] != "get: mint failed" {
				t.Fatalf("unexpected transport message: %v", record["msg"])
			}
			if record["level"] != "ERROR" {
				t.Fatalf("dependency alarm ignores level %v; record=%s", record["level"], line)
			}
			return
		}
	}
	t.Fatalf("missing dependency transport log: %s", logs.String())
}

func TestHandleGet_StoreAlarmContract(t *testing.T) {
	for _, tc := range []struct {
		name        string
		err         error
		code, level string
	}{
		{"operational", errors.New("DynamoDB unavailable"), "ddb_error", "ERROR"},
		{"conditional", &types.ConditionalCheckFailedException{}, "conditional_check_failed", "WARN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newAdminTestServers(t)
			ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
			ts.ddb.SetGetItemErr(ts.tableNames.channelPolicy, tc.err)
			h := newAdminTestHandler(t, ts)
			logs := &capturedLogs{}
			previous := slog.Default()
			slog.SetDefault(slog.New(observability.NewRedactingJSONHandler(logs, nil)))
			t.Cleanup(func() { h.Wait(); slog.SetDefault(previous) })
			newAdminSlashInvoker(t, h).invokeAdminAsync("get $prod-db", testAdminTeamID, testAdminUserID)
			h.Wait()
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var record map[string]any
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal(err)
				}
				if record["msg"] != "get: alias lookup failed" {
					continue
				}
				text, _ := record["error"].(string)
				if record["level"] != tc.level || !strings.Contains(text, "["+tc.code+"]") {
					t.Fatalf("wrong store alarm record: %s", line)
				}
				t.Logf("store alarm record: %s", line)
				return
			}
			t.Fatalf("missing store failure record: %s", logs.String())
		})
	}
}

func TestStoreErrorLogLevel(t *testing.T) {
	for _, fallback := range []slog.Level{slog.LevelDebug, slog.LevelWarn} {
		for _, code := range []string{"ddb_error", "conditional_check_failed", "quota_exceeded", "not_found", ""} {
			err := fmt.Errorf("caller: %w", &slackdata.Error{Code: code})
			want := fallback
			if code == "ddb_error" {
				want = slog.LevelError
			}
			if got := storeErrorLogLevel(err, fallback); got != want {
				t.Fatalf("%s: got %v want %v", code, got, want)
			}
		}
		if got := storeErrorLogLevel(errors.New("ddb_error"), fallback); got != fallback {
			t.Fatal("untyped text promoted")
		}
	}
}

// credentialFailureKMS uses the real KMSEncryptor wrapper while failing only the
// AWS call. No plaintext credential or live AWS client is involved.
type credentialFailureKMS struct {
	auth.KMSClient
}

func (credentialFailureKMS) Decrypt(context.Context, *kms.DecryptInput, ...func(*kms.Options)) (*kms.DecryptOutput, error) {
	return nil, errors.New("KMS unavailable")
}

func TestHandleGet_CredentialProviderAlarmContract(t *testing.T) {
	for _, path := range []string{"direct", "tunnel-slug", "resource-alias"} {
		for _, failure := range []string{"DynamoDB", "KMS", "missing"} {
			if path == "direct" && failure == "missing" {
				continue
			} // Existing direct-call behavior is unchanged.
			t.Run(path+"/"+failure, func(t *testing.T) {
				ts := newAdminTestServers(t)
				ts.seedPolicySet(t, testAdminTeamID, "C_test", "prod-db", []string{testResourceIDFix})
				const table = "credential_alarm_contract"
				ts.ddb.tables[table] = map[string]map[string]types.AttributeValue{}
				ts.ddb.keySchemas[table] = []string{"team_id"}
				switch failure {
				case "DynamoDB":
					ts.ddb.SetGetItemErr(table, errors.New("DynamoDB unavailable"))
				case "KMS":
					ts.ddb.seedItem(t, table, map[string]types.AttributeValue{
						"team_id":         &types.AttributeValueMemberS{Value: testAdminTeamID},
						"qurl_api_key":    &types.AttributeValueMemberB{Value: make([]byte, 12)},
						"qurl_api_key_dk": &types.AttributeValueMemberB{Value: []byte("synthetic-wrapped-key")},
					})
				}
				h := newAdminTestHandler(t, ts)
				h.cfg.AuthProvider = &auth.DDBProvider{Client: ts.ddb, TableName: table, Encryptor: &auth.KMSEncryptor{Client: credentialFailureKMS{}, KeyID: "synthetic-key"}}
				if failure == "missing" {
					_, lookupErr := h.resolveTunnelSlugAliasTarget(context.Background(), testAdminTeamID, "unconfigured")
					if !errors.Is(lookupErr, auth.ErrWorkspaceNotConfigured) || errors.Is(lookupErr, errCredentialLookup) {
						t.Fatalf("missing workspace classification changed: %v", lookupErr)
					}
					_, lookupErr = h.lookupListedResourceAliasesForGet(context.Background(), slog.Default(), testAdminTeamID, "unconfigured")
					if !errors.Is(lookupErr, auth.ErrWorkspaceNotConfigured) || errors.Is(lookupErr, errCredentialLookup) {
						t.Fatalf("missing workspace classification changed: %v", lookupErr)
					}
				}
				logs := &capturedLogs{}
				previous := slog.Default()
				slog.SetDefault(slog.New(observability.NewRedactingJSONHandler(logs, nil)))
				t.Cleanup(func() { h.Wait(); slog.SetDefault(previous) })
				var msg string
				switch path {
				case "direct":
					newAdminSlashInvoker(t, h).invokeAdminAsync("get $prod-db", testAdminTeamID, testAdminUserID)
					msg = "get: API key lookup failed"
				case "tunnel-slug":
					newAdminSlashInvoker(t, h).invokeAdminAsync("get $unconfigured", testAdminTeamID, testAdminUserID)
					msg = "get: tunnel-slug fallback lookup failed"
				case "resource-alias":
					_, _, err := h.resolveListedResourceAliasForGet(context.Background(), slog.Default(), testAdminTeamID, "C_test", testAdminUserID, "unconfigured", map[string]struct{}{testResourceIDFix: {}})
					if err == nil {
						t.Fatal("expected lookup failure")
					}
					msg = "get: resource-alias fallback lookup failed"
				}
				h.Wait()
				wantLevel := "ERROR"
				if failure == "missing" {
					wantLevel = "WARN"
				}
				for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
					var record map[string]any
					if err := json.Unmarshal([]byte(line), &record); err != nil {
						t.Fatal(err)
					}
					if record["msg"] != msg {
						continue
					}
					text, _ := record["error"].(string)
					if record["level"] != wantLevel || !strings.Contains(text, "DDBProvider.APIKey:") {
						t.Fatalf("wrong provider record: %s", line)
					}
					if failure == "KMS" && !strings.Contains(text, "KMSEncryptor.Open: KMS Decrypt:") {
						t.Fatalf("missing real KMS wrapper: %s", line)
					}
					t.Logf("credential alarm record: %s", line)
					return
				}
				t.Fatalf("missing %s record: %s", msg, logs.String())
			})
		}
	}
}
