package qurlapi

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

func TestDeleteLeavesPendingConfirmationToTheCommand(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		apitest.WriteProblem(t, w, 503, "revocation_pending", "Revocation Pending", "Protection update is pending")
	})
	var sleeps []time.Duration
	client := newTestClient(t, srv, &sleeps)
	_, err := client.Delete(context.Background(), srv.Key.CRID)
	var pending *Error
	if !errors.As(err, &pending) || pending.Code != "revocation_pending" || pending.RetryAfter != 30 {
		t.Fatalf("lost pending contract: %v", err)
	}
	if len(srv.Requests()) != 1 || len(sleeps) != 0 {
		t.Fatal("API delete waited or retried before the command could stop local sharing")
	}
}
