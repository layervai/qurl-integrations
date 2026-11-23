package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"

	"github.com/layervai/qurl-integrations/shared/auth"

	"github.com/layervai/qurl-integrations/shared/observability"
)

func TestStatePutTelemetryMeasuresSDKRequest(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		name := "success"
		if timeout {
			name = "timeout"
		}
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(observability.NewRedactingJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Amz-Target") != "DynamoDB_20120810.PutItem" {
					t.Error("unexpected SDK operation")
				}
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					return
				}
				if timeout {
					select {
					case <-r.Context().Done():
					case <-time.After(3 * time.Second):
						t.Error("SDK request did not cancel before server cleanup")
					}
					return
				}
				time.Sleep(100 * time.Millisecond)
				w.Header().Set("Content-Type", "application/x-amz-json-1.0")
				_, _ = w.Write([]byte("{}"))
			}))
			defer server.Close()
			ddb := dynamodb.New(dynamodb.Options{Region: "us-east-2", BaseEndpoint: aws.String(server.URL), Credentials: aws.AnonymousCredentials{}, RetryMaxAttempts: 1})
			store, err := NewDDBStateStore(&auth.DDBProvider{Client: ddb, TableName: "synthetic-workspace-state"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = MintStoredStateWithEmailMode(context.Background(), store, "T_PRIVATE", "U_PRIVATE", "private@example.invalid", SetupModeReuse, time.Now())
			if timeout {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("timeout changed: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			var record map[string]any
			if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			minimum := 90.0
			if timeout {
				minimum = 1500
			}
			elapsed, ok := record["duration_ms"].(float64)
			if !ok || elapsed < minimum || record["result"] != name || len(record) != 5 {
				t.Fatalf("unexpected telemetry: %v", record)
			}
			t.Logf("telemetry_json=%s", logs.Bytes())
		})
	}
}
