package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
)

func TestBatchGetResourcesWireAndBoundaries(t *testing.T) {
	hits := 0
	ids := make([]string, ResourceBatchGetMaxIDs)
	for i := range ids {
		ids[i] = "public-" + strconv.Itoa(i)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/resources/batch-get" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("wrong metadata request")
		}
		var body struct {
			IDs []string `json:"resource_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if !reflect.DeepEqual(body.IDs, ids) {
			t.Error("request IDs changed")
		}
		apiEnvelope(t, w, []Resource{{ResourceID: ids[0]}})
	}))
	defer server.Close()
	c := New(server.URL, "test-key", WithRetry(0))
	got, err := c.BatchGetResources(context.Background(), ids)
	if err != nil || len(got) != 1 {
		t.Fatalf("result=%v err=%v", got, err)
	}
	for _, invalid := range [][]string{nil, {}, {""}, {" x"}, {"x", "x"}, append(ids, "extra")} {
		if _, err := c.BatchGetResources(context.Background(), invalid); err == nil {
			t.Error("invalid input accepted")
		}
	}
	if hits != 1 {
		t.Fatalf("invalid input made HTTP requests: %d", hits)
	}
}

func TestBatchGetResourcesRejectsIncompleteOrForeignResponse(t *testing.T) {
	for _, body := range []string{`{}`, `{"data":null,"meta":{}}`, `{"data":[]}`, `{"data":[],"meta":{"has_more":true}}`, `{"data":[],"meta":{"next_cursor":"more"}}`, `{"data":[{"resource_id":"foreign"}],"meta":{}}`, `{"data":[{"resource_id":"known"},{"resource_id":"known"}],"meta":{}}`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			if _, err := New(server.URL, "key", WithRetry(0)).BatchGetResources(context.Background(), []string{"known"}); err == nil {
				t.Fatal("malformed response accepted")
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { apiEnvelope(t, w, []Resource{}) }))
	defer server.Close()
	got, err := New(server.URL, "key", WithRetry(0)).BatchGetResources(context.Background(), []string{"gone"})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty batch: %v %v", got, err)
	}
}
