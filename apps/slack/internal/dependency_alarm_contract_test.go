package internal

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
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
