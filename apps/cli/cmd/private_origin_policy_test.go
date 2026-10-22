package main

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	"github.com/layervai/qurl-integrations/apps/cli/internal/connector/agent"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

func TestPrivateOriginTCPCommandsRejectBeforeChangingSharing(t *testing.T) {
	for _, action := range []string{"publish", "publish-default", "restart"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			srv := apitest.NewServer(t)
			dir := connectorStateTestDir(t)
			if err := connectorstate.EstablishExternalRuntimeMode(ctx, dir); err != nil {
				t.Fatal(err)
			}
			registry, err := openOwnedTestShareRegistry(dir)
			if err != nil {
				t.Fatal(err)
			}
			row := localShareFixture(srv)
			row.DesiredState = "on"
			if err := registry.Put(ctx, &row); err != nil {
				t.Fatal(err)
			}
			if _, err := registry.RetargetStoppedToPrivate(ctx, "own_cli_fixture", "local-", privateCmdTarget(t, privateTestTempRoot()).URL); err != nil {
				t.Fatal(err)
			}
			before, err := registry.Get(ctx, row.CRID)
			if err != nil {
				t.Fatal(err)
			}
			path := "/v1/resources/" + row.CRID + "/sharing"
			srv.Script(http.MethodGet, path, sharingResponse(t, srv, "on", row.ServingEpoch, "serving"))
			srv.Script(http.MethodPost, path+"/restart", sharingResponse(t, srv, "on", row.ServingEpoch+1, "connecting"))
			srv.Script(http.MethodPut, path, sharingResponse(t, srv, "off", row.ServingEpoch+2, "stopped"))
			args := []string{"--endpoint", srv.URL, "publish", "http://127.0.0.1:4000", "--id", row.ConnectorID}
			if action == "publish-default" {
				args = args[:4]
			}
			if action == "restart" {
				args = []string{"--endpoint", srv.URL, "restart", row.CRID, "--target", "http://127.0.0.1:4000"}
			}
			// Resource discovery mints the Connector enrollment credential and
			// creates the Connector resource, so a refused target must never
			// reach it — including the default, generated-ID publish.
			var discovered atomic.Int32
			resolve := resolvedLocalResource(srv, true)
			result := runCLI(t, &runOpts{
				args: args, env: map[string]string{"QURL_API_KEY": testAPIKey, connectorstate.EnvRuntimeSupervision: "external"},
				shareStateDir: dir, shareRegistry: registry, shareDaemon: &recordingShareDaemon{},
				preflightTarget: func(context.Context, string, int) error { return nil },
				localResource: func(ctx context.Context, cfg *connectorshare.NativeRuntimeConfig, credential func(string) (string, error)) (*agent.ResolvedResource, error) {
					discovered.Add(1)
					return resolve(ctx, cfg, credential)
				},
			})
			if result.code == 0 || !strings.Contains(result.stderr.String(), "private origin shares require private transport") {
				t.Fatalf("expected private target refusal: exit=%d stderr=%s", result.code, result.stderr.String())
			}
			if got := discovered.Load(); got != 0 {
				t.Fatalf("rejected target reached Connector resource discovery %d times", got)
			}
			for _, request := range srv.Requests() {
				if strings.HasPrefix(request.Path, path) && request.Method != http.MethodGet {
					t.Errorf("rejected target changed cloud sharing: %s %s", request.Method, request.Path)
				}
			}
			after, err := registry.Get(ctx, row.CRID)
			if err != nil || *after != *before {
				t.Fatalf("rejected target changed local sharing: before=%+v after=%+v err=%v", before, after, err)
			}
		})
	}
}
