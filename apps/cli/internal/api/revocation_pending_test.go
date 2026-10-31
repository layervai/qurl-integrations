package qurlapi

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

func TestDeleteConfirmsOnlyStructurallyCommittedRevocation(t *testing.T) {
	for _, tc := range []struct {
		name, code, retryAfter  string
		confirmStatus, requests int
		delay                   time.Duration
		success                 bool
	}{
		{"confirmed", "revocation_pending", "30", 204, 2, 30 * time.Second, true},
		{"gone after confirmation", "revocation_pending", "35", 404, 2, 35 * time.Second, true},
		{"proof still pending", "revocation_pending", "30", 503, 2, 30 * time.Second, false},
		{"confirmation refused", "revocation_pending", "30", 403, 2, 30 * time.Second, false},
		{"generic unavailable", "service_unavailable", "30", 204, 1, 0, false},
		{"missing code", "", "30", 204, 1, 0, false},
		{"missing delay", "revocation_pending", "", 204, 1, 0, false},
		{"over wait budget", "revocation_pending", "36", 204, 1, 0, false},
		{"zero delay", "revocation_pending", "0", 204, 1, 0, false},
		{"negative delay", "revocation_pending", "-1", 204, 1, 0, false},
		{"overflow delay", "revocation_pending", "999999999999999999999", 204, 1, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			first := func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", tc.retryAfter)
				// Same prose for both codes: only the structural code permits confirmation.
				apitest.WriteProblem(t, w, 503, tc.code, "Unavailable", "Revocation committed; protection update is pending. Retry to confirm.")
			}
			confirm := func(w http.ResponseWriter, _ *http.Request) {
				if tc.confirmStatus == 204 {
					w.WriteHeader(204)
					return
				}
				apitest.WriteProblem(t, w, tc.confirmStatus, "service_unavailable", "Confirmation failed", "Not confirmed")
			}
			srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID, first, confirm)
			var sleeps []time.Duration
			client := newTestClient(t, srv, &sleeps)
			result, err := client.Delete(context.Background(), srv.Key.CRID)
			if tc.success {
				if err != nil || result == nil || result.AlreadyGone != (tc.confirmStatus == 404) {
					t.Fatalf("delete result=%+v error=%v", result, err)
				}
			} else {
				var problem *Error
				if !errors.As(err, &problem) || problem.Code != tc.code {
					t.Fatalf("delete lost its original pending/unavailable result: %v", err)
				}
			}
			if got := len(srv.Requests()); got != tc.requests {
				t.Fatalf("requests=%d, want %d", got, tc.requests)
			}
			if tc.delay == 0 {
				if len(sleeps) != 0 {
					t.Fatalf("unexpected wait %v", sleeps)
				}
			} else if len(sleeps) != 1 || sleeps[0] != tc.delay {
				t.Fatalf("waits=%v, want exactly %s", sleeps, tc.delay)
			}
		})
	}
}

func TestDeleteCancellationDuringConfirmationKeepsCommittedState(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		apitest.WriteProblem(t, w, 503, "revocation_pending", "Revocation Pending", "Protection update is pending")
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, err := New(&Config{BaseURL: srv.URL, APIKey: "test-key", Sleep: func(time.Duration) { cancel() }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Delete(ctx, srv.Key.CRID)
	var pending *Error
	if !errors.Is(err, context.Canceled) || !errors.As(err, &pending) || pending.Code != "revocation_pending" {
		t.Fatalf("lost cancellation or commit evidence: %v", err)
	}
	if len(srv.Requests()) != 1 {
		t.Fatal("canceled confirmation sent another DELETE")
	}
}

func TestDeleteLostConfirmationKeepsCommittedState(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID,
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "30")
			apitest.WriteProblem(t, w, 503, "revocation_pending", "Revocation Pending", "Protection update is pending")
		},
		func(w http.ResponseWriter, _ *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
		},
	)
	client := newTestClient(t, srv, nil)
	_, err := client.Delete(context.Background(), srv.Key.CRID)
	var pending *Error
	if !errors.As(err, &pending) || pending.Code != "revocation_pending" {
		t.Fatalf("lost commit evidence after confirmation disconnect: %v", err)
	}
	if len(srv.Requests()) != 2 {
		t.Fatalf("confirmation disconnect was replayed: %d requests", len(srv.Requests()))
	}
}
