package internal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/layervai/qurl-integrations/shared/client"
)

func TestConnectorInstallModalReportsResourceQuota(t *testing.T) {
	for _, s3Website := range []bool{false, true} {
		name := "existing service"
		if s3Website {
			name = "S3 website"
		}
		t.Run(name, func(t *testing.T) {
			ts := newAdminTestServers(t)
			ts.seedAdmin(t)
			var creates, keyMints atomic.Int32
			ts.addCustomer(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
				creates.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"error":{"code":"quota_exceeded","title":"Quota Exceeded","detail":"quota exceeded: protected resource limit reached (33/10) — private-upstream-detail"},"meta":{"request_id":"ea96b2e6459e6be9"}}`)
			})
			ts.addCustomer(http.MethodPost, "/v1/api-keys", func(w http.ResponseWriter, _ *http.Request) {
				keyMints.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			})
			h := newAdminTestHandler(t, ts)
			freezeTunnelBootstrapNow(t, h, fixedNow)
			dmPosts := captureTunnelPostDMSuccess(h)
			h.SetAliasStore(h.cfg.AdminStore)
			inv := newAdminSlashInvoker(t, h)
			meta := TunnelInstallModalMetadata{
				TeamID: testAdminTeamID, ChannelID: testTunnelChannelID,
				UserID: testAdminUserID, ResponseURL: inv.responseU.URL,
				CreatedAtUnix: fixedNow.Unix(),
			}
			body := tunnelInstallViewSubmissionBody(t, &meta, map[string]map[string]interactionStateValue{
				tunnelInstallBlockSlug: {tunnelInstallActionSlug: {Value: testTunnelSlug}},
				tunnelInstallBlockEnvironment: {tunnelInstallActionEnvironment: {
					SelectedOption: &interactionSelectedOption{Value: string(tunnelEnvDocker)},
				}},
				tunnelInstallBlockLocalPort: {tunnelInstallActionLocalPort: {Value: "8080"}},
			})
			if s3Website {
				body = s3WebsiteInstallViewSubmissionBody(t, &meta, s3WebsiteInstallModalValues(
					testTunnelSlug, "", string(tunnelEnvDocker), testS3WebsiteBucket,
					testS3WebsiteRegion, testS3WebsitePrefix, testS3WebsiteIndex,
				))
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))
			if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "{}" {
				t.Fatalf("modal acknowledgement = %d %s", w.Code, w.Body.String())
			}
			message := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
			h.Wait()
			for _, want := range []string{"protected resource limit", "revoke", "upgrade", "No enrollment token was minted", "ea96b2e6459e6be9"} {
				if !strings.Contains(message, want) {
					t.Errorf("quota reply missing %q: %s", want, message)
				}
			}
			if strings.Contains(message, "private-upstream-detail") {
				t.Errorf("quota reply leaked upstream detail: %s", message)
			}
			if creates.Load() != 1 || keyMints.Load() != 0 || len(*dmPosts) != 0 {
				t.Errorf("quota refusal: creates=%d key mints=%d DMs=%d", creates.Load(), keyMints.Load(), len(*dmPosts))
			}
			_, found, err := h.cfg.AdminStore.LookupChannelAlias(context.Background(), testAdminTeamID, testTunnelChannelID, testTunnelSlug)
			if err != nil || found {
				t.Errorf("quota refusal bound an alias: found=%v err=%v", found, err)
			}
		})
	}
}

func TestConnectorCreateNonQuotaErrorsStaySanitized(t *testing.T) {
	t.Parallel()
	for _, apiErr := range []*client.APIError{
		{StatusCode: http.StatusForbidden, Code: "forbidden", Detail: "quota_exceeded private-upstream-detail", RequestID: "ref-denied"},
		{StatusCode: http.StatusInternalServerError, Code: "quota_exceeded", Detail: "private-upstream-detail", RequestID: "ref-server"},
	} {
		got := connectorResourceCreateErrorMessage(apiErr)
		if !strings.Contains(got, apiErr.RequestID) || strings.Contains(got, "protected resource limit") || strings.Contains(got, "private-upstream-detail") {
			t.Errorf("non-quota error was misclassified or leaked: %s", got)
		}
	}
	if got := connectorResourceCreateErrorMessage(errors.New("private-transport-detail")); got != "Failed to create or find the qURL Connector resource." {
		t.Errorf("transport failure = %q", got)
	}
	wrapped := fmt.Errorf("create: %w", &client.APIError{StatusCode: http.StatusForbidden, Code: "quota_exceeded"})
	if got := connectorResourceCreateErrorMessage(wrapped); !strings.Contains(got, "protected resource limit") || strings.Contains(got, "Reference:") {
		t.Errorf("wrapped quota without reference = %q", got)
	}
}
