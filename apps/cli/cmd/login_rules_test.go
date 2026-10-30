package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	connectorshare "github.com/layervai/qurl-connector/pkg/share"

	"github.com/layervai/qurl-integrations/apps/cli/internal/accessrules"
	"github.com/layervai/qurl-integrations/apps/cli/internal/apitest"
	connectorstate "github.com/layervai/qurl-integrations/apps/cli/internal/connector/state"
)

func TestAccountLoginsReconcileGuestRulesWithoutChangingLoginResult(t *testing.T) {
	for _, external := range []bool{false, true} {
		for _, status := range []int{http.StatusOK, http.StatusForbidden, http.StatusServiceUnavailable} {
			t.Run(http.StatusText(status)+map[bool]string{false: "/plain", true: "/external"}[external], func(t *testing.T) {
				if external && runtime.GOOS == "windows" {
					t.Skip("external token files are unsupported; covered by TestExternalLoginUnsupportedPlatformLeavesNamespaceUntouched")
				}
				srv := apitest.NewServer(t)
				dir := connectorStateTestDir(t)
				store, err := connectorstate.OpenAccessRules(dir, srv.URL)
				if err != nil {
					t.Fatal(err)
				}
				guest := accessrules.Definition{ID: "11111111-1111-4111-8111-111111111111", Name: "Guest rule", Mode: "keys"}
				if _, err = store.Save(t.Context(), 0, &guest); err != nil {
					t.Fatal(err)
				}
				account := accessrules.AccountLibrary{AccountID: "account-one", Version: 4, Items: []accessrules.Definition{{ID: "22222222-2222-4222-8222-222222222222", Name: "Account rule", Mode: "review"}}}
				loaded, merged := false, false
				srv.Script(http.MethodGet, "/v1/me/access-rules", func(w http.ResponseWriter, _ *http.Request) {
					loaded = true
					if status != http.StatusOK {
						apitest.WriteProblem(t, w, status, "unavailable", "not available", "account-secret-must-not-leak")
						return
					}
					apitest.WriteEnvelope(t, w, status, account, nil)
				})
				if status == http.StatusOK {
					srv.Script(http.MethodPut, "/v1/me/access-rules", func(w http.ResponseWriter, r *http.Request) {
						var request struct {
							Version int64                    `json:"version"`
							Items   []accessrules.Definition `json:"items"`
						}
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Fatal(err)
						}
						if request.Version != 4 || len(request.Items) != 2 || !accessrules.SameItem(request.Items, []accessrules.Definition{guest}, guest.ID) {
							t.Fatalf("guest/account merge lost: %+v", request)
						}
						merged = true
						account.Version++
						account.Items = request.Items
						apitest.WriteEnvelope(t, w, http.StatusOK, account, nil)
					})
				}
				options := &runOpts{args: []string{"--endpoint", srv.URL, "login", "-o", "json"}, env: map[string]string{}, stdin: strings.NewReader(testAPIKey + "\n"), shareStateDir: dir}
				if external {
					options.args = append(options.args, "--supervision", "external", "--enrollment-token-file", filepath.Join(t.TempDir(), "enrollment-token"))
					options.env = externalLoginEnv()
					options.stdin = strings.NewReader("")
					options.openNativeRuntime = func(context.Context, connectorshare.NativeRuntimeConfig) (registeredNativeRuntime, error) {
						return &bootstrapNativeRuntime{store: &bootstrapAgentStateStore{state: bootstrapRegisteredState(t)}}, nil
					}
				}
				result := runCLI(t, options)
				if result.code != 0 || !loaded || merged != (status == http.StatusOK) {
					t.Fatalf("login did not reconcile: exit=%d loaded=%v merged=%v stderr=%s", result.code, loaded, merged, result.stderr.String())
				}
				expected := map[string]any{"owner_id": apitest.MeOwnerID, "auth_type": "api_key", "device_enrolled": true, "device_key_id": apitest.MeKeyID}
				var actual map[string]any
				if err = json.Unmarshal(result.stdout.Bytes(), &actual); err != nil {
					t.Fatal(err)
				}
				want, _ := json.Marshal(expected)
				got, _ := json.Marshal(actual)
				if !bytes.Equal(want, got) {
					t.Fatalf("login JSON changed: %s", result.stdout.String())
				}
				snapshot, err := store.Snapshot(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if !accessrules.SameItem(snapshot.Items, []accessrules.Definition{guest}, guest.ID) {
					t.Fatal("deferred sync lost guest work")
				}
				if strings.Contains(result.stdout.String(), testAPIKey) || strings.Contains(result.stderr.String(), "account-secret-must-not-leak") {
					t.Fatal("login or sync leaked a secret")
				}
				root, err := os.OpenRoot(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = root.Close() }()
				if err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
					if walkErr != nil {
						return walkErr
					}
					if entry.IsDir() {
						return nil
					}
					relative, relErr := filepath.Rel(dir, path)
					if relErr != nil {
						return relErr
					}
					data, readErr := root.ReadFile(relative)
					if readErr != nil {
						return readErr
					}
					if bytes.Contains(data, []byte(testAPIKey)) {
						t.Errorf("account API key persisted in %s", entry.Name())
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func assertLoginIdentityAndRuleRead(t *testing.T, requests []apitest.RecordedRequest, key string) {
	t.Helper()
	if len(requests) != 2 {
		t.Fatalf("login requests = %+v, want identity validation followed by rule reconciliation", requests)
	}
	for index, path := range []string{"/v1/me", "/v1/me/access-rules"} {
		if requests[index].Method != http.MethodGet || requests[index].Path != path || requests[index].Header.Get("Authorization") != "Bearer "+key {
			t.Fatalf("login request %d lost its identity or route: %+v", index, requests[index])
		}
	}
}
