package qurlapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/layervai/qurl-go/qurl"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

func batchFixtureItem(t *testing.T, index int, success bool) BatchItemResult {
	t.Helper()
	if !success {
		return BatchItemResult{Index: index, Error: &BatchItemError{Code: "quota_exceeded", Message: "resolve plan limit"}}
	}
	key := apitest.FixedResourceKey(t)
	expiry := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	return BatchItemResult{Index: index, Success: true, ResourceID: key.ResourceID, CRID: key.CRID, QURLLink: "https://qurl.link/#fixture", ExpiresAt: &expiry}
}

func TestBatchCreateRetainsAllOutcomesAndOriginalIndexes(t *testing.T) {
	for _, succeeded := range []int{0, 1, 2} {
		calls := 0
		status := http.StatusMultiStatus
		if succeeded == 0 {
			status = http.StatusBadRequest
		}
		if succeeded == 2 {
			status = http.StatusCreated
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if r.Method != http.MethodPost || r.URL.Path != "/v1/qurls/batch" || r.Header.Get("Authorization") != "Bearer fixture-account" || r.Header.Get("Idempotency-Key") != "" {
				t.Error("wrong batch authority, route or replay")
			}
			var wire map[string][]map[string]any
			if json.NewDecoder(r.Body).Decode(&wire) != nil || len(wire["items"]) != 2 {
				t.Error("wrong batch envelope")
			}
			if _, exists := wire["items"][0]["private"]; exists {
				t.Error("omitted privacy was overwritten")
			}
			if wire["items"][1]["private"] != false || wire["items"][1]["one_time_use"] != false || wire["items"][1]["max_sessions"] != float64(0) {
				t.Error("explicit false/zero became omitted")
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": BatchCreateResult{Succeeded: succeeded, Failed: 2 - succeeded, Results: []BatchItemResult{batchFixtureItem(t, 1, succeeded == 2), batchFixtureItem(t, 0, succeeded > 0)}}})
		}))
		input, err := ParseBatchCreateInput([]byte(`{"items":[{"target_url":"https://a.example"},{"target_url":"https://b.example","private":false,"one_time_use":false,"max_sessions":0}]}`))
		if err != nil {
			t.Fatal(err)
		}
		client, err := NewAccountManagement(&Config{BaseURL: server.URL, APIKey: "fixture-account"})
		if err != nil {
			t.Fatal(err)
		}
		result, err := client.BatchCreate(context.Background(), input)
		server.Close()
		if err != nil || calls != 1 || result.Succeeded != succeeded || result.Failed != 2-succeeded || result.Results[0].Index != 0 || result.Results[1].Index != 1 {
			t.Fatal("lost or replayed batch outcomes", calls, err)
		}
	}
}

func TestBatchInputRejectsMistakesBeforeAnyWrite(t *testing.T) {
	for _, raw := range []string{`{"items":[{"target_url":"https://example.com","private":null}]}`, `{"items":[{"target_url":"https://example.com","target_url":"https://other.example"}]}`, `{"items":[],"items":[{"target_url":"https://example.com"}]}`, `null`, `{}`, `{"items":[]}`, `{"items":[null]}`, `{"items":[{"target_url":"file:///secret"}]}`, `{"items":[{"target_url":"https://user:secret@example.com"}]}`, `{"items":[{"target_url":"https://example.com","typo":true}]}`, `{"items":[{"target_url":"https://example.com","type":"tunnel"}]}`, `{"items":[{"target_url":"https://example.com","max_sessions":1001}]}`, `{"items":[{"target_url":"https://example.com"}],"typo":true}`, `{"items":[{"target_url":"https://example.com"}]} {}`} {
		if _, err := ParseBatchCreateInput([]byte(raw)); !errors.Is(err, qurl.ErrInvalidResourceRequest) {
			t.Fatal("invalid batch accepted", raw, err)
		}
	}
	input := BatchCreateInput{Items: make([]BatchCreateItem, 101)}
	if ValidateBatchCreateInput(input) == nil {
		t.Fatal("unbounded batch accepted")
	}
	valid := `{"items":[{"target_url":"https://example.com","access_policy":{"ai_agent_policy":{"allow_categories":["claude"]}},"expires_in":"1h","session_duration":"30m","label":"fixture","custom_domain":"secure.example.com"}]}`
	parsed, err := ParseBatchCreateInput([]byte(valid))
	if err != nil || !strings.Contains(string(parsed.Items[0].AccessPolicy), "claude") {
		t.Fatal("public create policy lost", err)
	}
}

func TestBatchCreateRejectsInconsistentResults(t *testing.T) {
	for _, variation := range []string{"duplicate-index", "missing-index", "missing-success", "wrong-count", "wrong-status", "wrong-identity", "missing-error", "unsafe-link", "missing-expiry", "missing-data"} {
		t.Run(variation, func(t *testing.T) {
			item := batchFixtureItem(t, 0, true)
			raw, _ := json.Marshal(item)
			var row map[string]any
			_ = json.Unmarshal(raw, &row)
			data := map[string]any{"succeeded": 1, "failed": 0, "results": []any{row}}
			switch variation {
			case "duplicate-index":
				data["succeeded"] = 2
				data["results"] = []any{row, row}
			case "missing-index":
				delete(row, "index")
			case "missing-success":
				delete(row, "success")
			case "wrong-count":
				data["succeeded"] = 0
			case "wrong-identity":
				row["resource_id"] = apitest.GenerateResourceKey(t).ResourceID
			case "missing-error":
				row["success"] = false
				delete(row, "qurl_link")
			case "unsafe-link":
				row["qurl_link"] = "https://user:secret@qurl.link/#fixture"
			case "missing-expiry":
				delete(row, "expires_at")
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				status := http.StatusCreated
				if variation == "wrong-status" {
					status = http.StatusMultiStatus
				}
				w.WriteHeader(status)
				if variation == "missing-data" {
					_, _ = w.Write([]byte(`{}`))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
			}))
			defer server.Close()
			client, err := NewAccountManagement(&Config{BaseURL: server.URL, APIKey: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			input := BatchCreateInput{Items: []BatchCreateItem{{TargetURL: "https://example.com"}}}
			if variation == "duplicate-index" {
				input.Items = append(input.Items, input.Items[0])
			}
			_, err = client.BatchCreate(context.Background(), input)
			if !errors.Is(err, qurl.ErrInvalidAPIResponse) || calls != 1 {
				t.Fatal("inconsistent batch accepted or replayed", calls, err)
			}
		})
	}
}

func TestBatchWritesNeverReplayTransientOrValidationResponses(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls++
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"title":"fixture problem","code":"invalid_request"}`))
		}))
		client, err := NewAccountManagement(&Config{BaseURL: server.URL, APIKey: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.BatchCreate(context.Background(), BatchCreateInput{Items: []BatchCreateItem{{TargetURL: "https://example.com"}}})
		server.Close()
		var problem *Error
		if calls != 1 || !errors.As(err, &problem) || problem.StatusCode != status {
			t.Fatal("batch replayed or lost definitive API problem", calls, err)
		}
	}
}
