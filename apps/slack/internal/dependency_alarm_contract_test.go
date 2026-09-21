package internal

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

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
