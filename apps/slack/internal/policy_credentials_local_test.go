package internal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/apps/slack/internal/testddb"
	"github.com/layervai/qurl-integrations/shared/client"
)

type policyCredentialProvider struct {
	recordingAuthProvider
	resolve func(context.Context, string) (string, error)
}

func (p *policyCredentialProvider) APIKey(ctx context.Context, team string) (string, error) {
	return p.resolve(ctx, team)
}

func TestPolicyCredentialScopeDynamoDBLocal(t *testing.T) {
	for _, operation := range []string{"create URL", "set-display-name app NewName", "unset-display-name app"} {
		for _, mode := range []string{"unchanged", "reinstalled during key lookup", "revoked during key lookup", "stale before client"} {
			t.Run(operation+"/"+mode, func(t *testing.T) {
				ddb, workspaceTable, policyTable := testddb.New(t)
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				store, err := slackdata.NewStore(ctx, slackdata.WithDynamoDBClient(ddb), slackdata.WithTableNames(workspaceTable, policyTable))
				if err != nil {
					t.Fatal(err)
				}
				const team, channel = "TCRED001", "CCRED001"
				bind := func(owner, admin string) error {
					return store.BindWorkspace(ctx, &slackdata.WorkspaceMapping{TeamID: team, OwnerID: owner}, admin)
				}
				if err := bind(testAdminOwnerID, testAdminUserID); err != nil {
					t.Fatal(err)
				}
				owner, err := store.AuthorizePolicyWrite(ctx, team, testAdminOwnerID)
				if err != nil {
					t.Fatal(err)
				}
				reinstall := func() error {
					if err := store.DeleteWorkspaceMapping(ctx, team); err != nil {
						return err
					}
					return bind("UOTHER001", "UOTHER001")
				}
				var reads, writes atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet && r.URL.Path == testResourcesPath {
						respondQURLEnvelope(t, w, []map[string]any{{testKeyResourceID: "r_tunnel", testKeyType: client.ResourceTypeTunnel, testKeySlug: "app", testKeyStatus: client.StatusActive}})
						return
					}
					if (r.Method != http.MethodPost || r.URL.Path != testResourcesPath) && (r.Method != http.MethodPatch || r.URL.Path != testResourcesPath+"/r_tunnel") {
						http.Error(w, "unexpected request", http.StatusNotFound)
						return
					}
					writes.Add(1)
					respondQURLEnvelope(t, w, map[string]any{testKeyResourceID: "r_created", testKeyType: client.ResourceTypeURL, testKeyStatus: client.StatusActive, testKeyTargetURL: testResourceExposeURL})
				}))
				t.Cleanup(upstream.Close)
				h := newTestHandler(t, upstream)
				h.cfg.AdminStore = store
				h.SetAliasStore(store)
				h.cfg.AuthProvider = &policyCredentialProvider{resolve: func(_ context.Context, gotTeam string) (string, error) {
					reads.Add(1)
					if gotTeam != team {
						t.Error("credential lookup used wrong workspace")
					}
					if mode == "reinstalled during key lookup" {
						if err := reinstall(); err != nil {
							return "", err
						}
					}
					if mode == "revoked during key lookup" {
						if err := store.RemoveAdmin(owner.Context(ctx), team, testAdminUserID); err != nil {
							return "", err
						}
					}
					return "local-selected-key", nil
				}}
				if mode == "stale before client" {
					actor, err := store.AuthorizePolicyWrite(ctx, team, testAdminUserID)
					if err != nil {
						t.Fatal(err)
					}
					if err := reinstall(); err != nil {
						t.Fatal(err)
					}
					if _, err := h.authenticatedClient(actor.Context(ctx), team); err == nil {
						t.Fatal("old scope acquired a replacement credential")
					}
					if reads.Load() != 0 {
						t.Fatal("stale scope reached credential provider")
					}
					return
				}
				inv := newAdminSlashInvoker(t, h)
				if operation != "create URL" {
					inv.invokeAdminAsync(operation, team, testAdminUserID)
				} else {
					meta := ExposeURLModalMetadata{TeamID: team, ChannelID: channel, UserID: testAdminUserID, ResponseURL: inv.responseU.URL}
					body := exposeURLCreateViewSubmissionBody(t, meta, team, testAdminUserID, testResourceExposeURL, "$requested")
					response := httptest.NewRecorder()
					h.ServeHTTP(response, newSignedRequest(t, pathSlackInteractions, body, body))
					if response.Code != http.StatusOK {
						t.Fatalf("signed modal rejected: %d", response.Code)
					}
					_ = inv.captured.waitForBody(t, 3*time.Second)
				}
				if reads.Load() != 1 {
					t.Fatalf("credential seam not reached exactly once: %d", reads.Load())
				}
				wantWrites := int32(0)
				if mode == "unchanged" {
					wantWrites = 1
				}
				if writes.Load() != wantWrites {
					t.Fatalf("scope performed unexpected upstream mutation: writes=%d want=%d", writes.Load(), wantWrites)
				}
			})
		}
	}
}
