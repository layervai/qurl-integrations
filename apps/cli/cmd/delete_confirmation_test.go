package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	qurlapi "github.com/layervai/qurl-integrations/apps/cli/internal/api"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/exitcode"
)

func pendingDeleteResponse(t *testing.T, code, delay string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", delay)
		apitest.WriteProblem(t, w, 503, code, "Revocation Pending", "Revocation committed; protection update is pending. Retry to confirm.")
	}
}

func TestDeleteConfirmationRequiresCommittedCode(t *testing.T) {
	for _, tc := range []struct {
		name, code, delay   string
		confirmation, calls int
		wait                time.Duration
		success             bool
	}{
		{"confirmed", "revocation_pending", "30", 204, 2, 30 * time.Second, true},
		{"gone", "revocation_pending", "35", 404, 2, 35 * time.Second, true},
		{"still pending", "revocation_pending", "30", 503, 2, 30 * time.Second, false},
		{"confirmation refused", "revocation_pending", "30", 403, 2, 30 * time.Second, false},
		{"generic 503", "service_unavailable", "30", 204, 1, 0, false},
		{"missing code", "", "30", 204, 1, 0, false},
		{"missing delay", "revocation_pending", "", 204, 1, 0, false},
		{"over budget", "revocation_pending", "36", 204, 1, 0, false},
		{"zero delay", "revocation_pending", "0", 204, 1, 0, false},
		{"negative delay", "revocation_pending", "-1", 204, 1, 0, false},
		{"overflow delay", "revocation_pending", "999999999999999999999", 204, 1, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID, pendingDeleteResponse(t, tc.code, tc.delay), func(w http.ResponseWriter, _ *http.Request) {
				if tc.confirmation == 204 {
					w.WriteHeader(204)
					return
				}
				apitest.WriteProblem(t, w, tc.confirmation, "confirmation_failed", "Confirmation Failed", "No proof")
			})
			var sleeps []time.Duration
			res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "delete", srv.Key.CRID, "--yes"}, sleeps: &sleeps})
			wantExit := exitcode.Unavailable
			if tc.success {
				wantExit = 0
			}
			if res.code != wantExit {
				t.Fatalf("exit=%d want %d; stderr=%s", res.code, wantExit, res.stderr.String())
			}
			if !tc.success && !strings.Contains(res.stderr.String(), "Revocation committed") {
				t.Fatalf("lost commit explanation: %s", res.stderr.String())
			}
			calls := 0
			for _, request := range srv.Requests() {
				if request.Method == http.MethodDelete {
					calls++
				}
			}
			if calls != tc.calls {
				t.Fatalf("DELETE calls=%d, want %d", calls, tc.calls)
			}
			if tc.wait == 0 {
				if len(sleeps) != 0 {
					t.Fatalf("unexpected waits: %v", sleeps)
				}
			} else if len(sleeps) != 1 || sleeps[0] != tc.wait {
				t.Fatalf("waits=%v, want %s", sleeps, tc.wait)
			}
		})
	}
}

func TestDeleteWithdrawsLocalRouteBeforeConfirmationWait(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID, pendingDeleteResponse(t, "revocation_pending", "30"), func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	stateDir := connectorStateTestDir(t)
	registry, err := openOwnedTestShareRegistry(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	local := localShareFixture(srv)
	seedLocalConnectorResourceBinding(t, stateDir, &local)
	if err := registry.Put(context.Background(), &local); err != nil {
		t.Fatal(err)
	}
	daemon := &recordingShareDaemon{}
	waited := false
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "delete", srv.Key.CRID, "--yes"},
		env:           map[string]string{"QURL_API_KEY": testAPIKey, "QURL_CONNECTOR_STATE_DIR": stateDir},
		shareRegistry: registry, shareStateDir: stateDir, shareDaemon: daemon,
		retrySleep: func(delay time.Duration) {
			waited = true
			if delay != 30*time.Second {
				t.Errorf("delay=%s, want full 30s directive", delay)
			}
			_, err := registry.Get(context.Background(), local.ResourceID)
			if !errors.Is(err, os.ErrNotExist) || daemon.reloads != 1 || daemon.ensures != 0 {
				t.Errorf("route remained live at confirmation wait: error=%v daemon=%+v", err, daemon)
			}
			assertLocalConnectorResourceRetired(t, stateDir, local.ConnectorID)
		},
	})
	if !waited || res.code != 0 {
		t.Fatalf("waited=%v exit=%d stderr=%s", waited, res.code, res.stderr.String())
	}
}

func TestDeleteConfirmationCancellationKeepsCommitEvidence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	original := &qurlapi.Error{StatusCode: 503, Code: "revocation_pending", RetryAfter: 30}
	opts := &globalOpts{sleep: func(time.Duration) { cancel() }}
	_, err := confirmPendingDelete(ctx, opts, nil, "unused", original)
	var pending *qurlapi.Error
	if !errors.Is(err, context.Canceled) || !errors.As(err, &pending) || pending != original {
		t.Fatalf("lost cancellation or commit evidence: %v", err)
	}
}

func TestDeleteLostConfirmationKeepsCommitExplanation(t *testing.T) {
	srv := apitest.NewServer(t)
	srv.Script(http.MethodDelete, "/v1/resources/"+srv.Key.CRID, pendingDeleteResponse(t, "revocation_pending", "30"), func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	})
	res := runCLI(t, &runOpts{args: []string{"--endpoint", srv.URL, "delete", srv.Key.CRID, "--yes"}})
	if res.code != exitcode.Unavailable || !strings.Contains(res.stderr.String(), "Revocation committed") {
		t.Fatalf("lost pending state on disconnect: exit=%d stderr=%s", res.code, res.stderr.String())
	}
	calls := 0
	for _, request := range srv.Requests() {
		if request.Method == http.MethodDelete {
			calls++
		}
	}
	if calls != 2 {
		t.Fatalf("confirmation disconnect replayed: %d DELETEs", calls)
	}
}
