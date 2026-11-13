package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
)

const limitedRule = `{"id":"11111111-1111-4111-8111-111111111111","name":"Hour","mode":"public","link_limits":{"expires_in":"1h"}}`

// A local publish mints a rule's link only once the share serves.
func TestLocalPublishMintsTheRuleLinkOnlyOnceServing(t *testing.T) {
	for _, tc := range []struct {
		name          string
		ensureErr     error
		mintFails     bool
		foundExisting bool
	}{
		{name: "serving"},
		{name: "existing without saved limits", foundExisting: true},
		{name: "mint fails", mintFails: true},
		{name: "daemon fails", ensureErr: errors.New("synthetic daemon failure")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := apitest.NewServer(t)
			if tc.foundExisting {
				srv.SetResourceAccess(false)
			}
			stateDir := foregroundLifecycleStateDir(t, "qurl-rule-bg-")
			registry, err := openOwnedTestShareRegistry(stateDir)
			if err != nil {
				t.Fatal(err)
			}
			path := "/v1/resources/" + srv.Key.CRID + "/sharing"
			srv.Script(http.MethodGet, path, sharingResponse(t, srv, "off", 0, "stopped"))
			srv.Script(http.MethodPut, path, sharingResponse(t, srv, "on", 1, "connecting"), sharingResponse(t, srv, "off", 2, "stopped"))
			srv.Script(http.MethodGet, path, sharingResponse(t, srv, "on", 1, "serving"))
			var mints []map[string]any
			if tc.mintFails {
				srv.Script(http.MethodPost, "/v1/resources/"+srv.Key.CRID+"/qurls", apitest.HandlerRevoked400(t))
			} else {
				scriptMint(t, srv, &mints)
			}
			res := runCLI(t, &runOpts{
				args:          []string{"--endpoint", srv.URL, "-o", "json", "publish", "http://127.0.0.1:3000", "--rule-file", "-"},
				stdin:         strings.NewReader(limitedRule),
				shareRegistry: registry, shareStateDir: stateDir, shareDaemon: &recordingShareDaemon{ensureErr: tc.ensureErr},
				preflightTarget: func(context.Context, string, int) error { return nil },
				localResource:   resolvedLocalResource(srv, tc.foundExisting),
			})
			local, err := registry.Get(context.Background(), srv.Key.CRID)
			if err != nil {
				t.Fatalf("registry: %v; exit=%d stdout=%s stderr=%s", err, res.code, res.stdout.String(), res.stderr.String())
			}
			switch {
			case tc.ensureErr != nil:
				if res.code == 0 || len(mints) != 0 || local.DesiredState != "off" {
					t.Fatalf("daemon failure: exit %d mints %d desired %s", res.code, len(mints), local.DesiredState)
				}
				// Saved before serving, so the same publish mints with them.
				if data, err := os.ReadFile(filepath.Join(stateDir, "share_rules.json")); err != nil || !strings.Contains(string(data), srv.Key.CRID) { //nolint:gosec // G304: test-owned state directory and fixed filename.
					t.Fatalf("limits not saved before serving: %v", err)
				}
			case tc.mintFails:
				if res.code == 0 || !strings.Contains(res.stderr.String(), "The link with the rule's limits was not made for "+srv.Key.CRID) || local.DesiredState != "on" {
					t.Fatalf("mint failure: exit %d desired %s %s", res.code, local.DesiredState, res.stderr.String())
				}
			default:
				if res.code != 0 || len(mints) != 1 || !strings.Contains(res.stdout.String(), `"link"`) {
					t.Fatalf("publish: exit %d mints %d %s %s", res.code, len(mints), res.stdout.String(), res.stderr.String())
				}
				requests := srv.Requests()
				if last := requests[len(requests)-1]; last.Method != http.MethodPost || !strings.HasSuffix(last.Path, "/qurls") {
					t.Fatalf("the link was not minted after serving: %+v", requests)
				}
			}
		})
	}
}
