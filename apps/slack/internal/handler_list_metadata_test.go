package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/shared/client"
)

func writeMetadataFixture(t *testing.T, w http.ResponseWriter, resources []client.Resource) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"data": resources, "meta": map[string]any{"has_more": false}}); err != nil {
		t.Error(err)
	}
}

func TestHandleListMetadataGhostDoesNotScanOrPrune(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	var metadataHits, catalogHits atomic.Int32
	ts.addCustomer(http.MethodPost, "/v1/resources/batch-get", func(w http.ResponseWriter, r *http.Request) {
		metadataHits.Add(1)
		var body struct {
			IDs []string `json:"resource_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if !reflect.DeepEqual(body.IDs, []string{"r_ghost_bb", "r_live_aa"}) {
			t.Errorf("wrong allow-set: %v", body.IDs)
		}
		writeMetadataFixture(t, w, []client.Resource{{ResourceID: "r_live_aa", Type: client.ResourceTypeTunnel, Slug: "live-tun"}})
	})
	ts.addCustomer(http.MethodGet, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		catalogHits.Add(1)
		http.Error(w, "catalog must not be scanned", http.StatusInternalServerError)
	})
	ts.seedChannelExposure(t, testAdminTeamID, "C_test", "r_live_aa", "r_ghost_bb")
	h := newAdminTestHandler(t, ts)
	before, err := h.cfg.AdminStore.ChannelScopedResourceIDs(context.Background(), testAdminTeamID, "C_test")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		_, _, reply := newAdminSlashInvoker(t, h).invokeAdminAsync("list", testAdminTeamID, testAdminUserID)
		if !strings.Contains(reply, "`$live-tun`") {
			t.Fatalf("live resource missing: %s", reply)
		}
	}
	after, err := h.cfg.AdminStore.ChannelScopedResourceIDs(context.Background(), testAdminTeamID, "C_test")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) || metadataHits.Load() != 2 || catalogHits.Load() != 0 {
		t.Fatalf("bindings changed or scan repeated: metadata=%d catalog=%d", metadataHits.Load(), catalogHits.Load())
	}
}

func TestFetchAllowedMetadataChunksAndBoundsConcurrency(t *testing.T) {
	allowed := make(map[string]struct{}, 701)
	for i := range 701 {
		allowed[fmt.Sprintf("resource-%04d", i)] = struct{}{}
	}
	var active, peak, hits atomic.Int32
	entered, release := make(chan struct{}, 20), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit := hits.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		var body struct {
			IDs []string `json:"resource_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body.IDs) > 100 || len(body.IDs) == 0 {
			t.Errorf("unbounded batch: %d", len(body.IDs))
		}
		if hit > 1 {
			entered <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		rows := make([]client.Resource, 0, len(body.IDs))
		for _, id := range body.IDs {
			rows = append(rows, client.Resource{ResourceID: id, Type: client.ResourceTypeTunnel})
		}
		writeMetadataFixture(t, w, rows)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		rows, err := (&Handler{}).fetchAllowedResources(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), client.New(server.URL, "key", client.WithRetry(0)), allowed)
		if err == nil && len(rows) != 701 {
			err = fmt.Errorf("got %d rows", len(rows))
		}
		done <- err
	}()
	for range 5 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("batch fanout did not start")
		}
	}
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("metadata lookup did not finish")
	}
	if hits.Load() != 8 || peak.Load() != 5 {
		t.Fatalf("hits=%d peak=%d", hits.Load(), peak.Load())
	}
}

func TestFetchAllowedMetadataDoesNotFallbackOnFailure(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var catalogHits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					catalogHits.Add(1)
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			_, err := (&Handler{}).fetchAllowedResources(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), client.New(server.URL, "key", client.WithRetry(0)), map[string]struct{}{"known": {}})
			if err == nil || catalogHits.Load() != 0 {
				t.Fatalf("status %d fell back: %v", status, err)
			}
		})
	}
}
