package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/layervai/qurl-integrations/apps/slack/internal/connectorimage"
	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	testS3WebsiteBucket        = "stats-site-bucket"
	testS3WebsitePrefix        = "website"
	testS3WebsiteIndex         = "index.html"
	testS3WebsiteRegion        = "us-east-1"
	testS3WebsiteKnockResource = "qurl-connector-server"
	testS3WebsiteDisplayName   = "Stats S3 website"
	testS3OriginContainer      = "s3-static-origin"
	testS3EnvBucket            = "S3_BUCKET"
	testS3EnvRegion            = "AWS_REGION"
	testS3EnvPrefix            = "S3_PREFIX"
	testS3EnvIndex             = "INDEX_DOCUMENT"
	testS3EnvCacheConnector    = "CACHE_CONNECTOR_ID"
	testS3OriginImageRef       = "ghcr.io/layervai/qurl-integrations/s3-static-connector@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testSlackAppInstallHint    = "latest qURL Slack app install"
	testSlackAppInstallLink    = "<https://slack-bot.example/oauth/slack/install|the qURL Slack install link>"
	testProtectConnectorCmd    = "/qurl-admin protect-connector"
	testInstallBlockLead       = "Run this whole block"
	testDockerRunDetached      = "docker run -d"
)

func TestS3WebsiteOriginPortMatchesOriginImageContract(t *testing.T) {
	// origins/s3-static-connector defaults LISTEN_ADDR to 127.0.0.1:8080.
	// Fail loudly if the generic tunnel default drifts away from the origin
	// image contract this renderer deliberately reuses.
	if s3WebsiteOriginPort != 8080 {
		t.Fatalf("s3WebsiteOriginPort = %d, want 8080", s3WebsiteOriginPort)
	}
}

func TestDefaultS3StaticConnectorImageIsAcceptedDigestPin(t *testing.T) {
	if got := connectorimage.ClassifyPin(defaultS3StaticConnectorImage); got != connectorimage.Accepted {
		t.Fatalf("ClassifyPin(defaultS3StaticConnectorImage) = %v, want %v", got, connectorimage.Accepted)
	}
}

func TestRequireS3OriginImageDigestRejectsMalformedDigest(t *testing.T) {
	for _, image := range []string{
		"ghcr.io/layervai/qurl-integrations/s3-static-connector:main",
		"ghcr.io/layervai/qurl-integrations/s3-static-connector@sha256:abc",
		"ghcr.io/layervai/qurl-integrations/s3-static-connector@sha256:" + strings.Repeat("A", 64),
	} {
		t.Run(image, func(t *testing.T) {
			if err := RequireS3OriginImageDigest(image); err == nil || !strings.Contains(err.Error(), S3OriginImageDigestRequired) {
				t.Fatalf("RequireS3OriginImageDigest(%q) err = %v, want digest-pin rejection", image, err)
			}
		})
	}
}

func TestS3WebsiteInstallModalSurfacesPrefixLimit(t *testing.T) {
	body, err := S3WebsiteInstallModal(&TunnelInstallModalMetadata{
		TeamID: testAdminTeamID, ChannelID: testTunnelChannelID,
		UserID: testAdminUserID, ResponseURL: testSlackResponseURL,
		CreatedAtUnix: fixedNow.Unix(),
	})
	if err != nil {
		t.Fatalf("S3WebsiteInstallModal: %v", err)
	}
	if !strings.Contains(string(body), "up to 256 characters") {
		t.Fatalf("S3 website modal does not surface prefix limit: %s", body)
	}
}

func TestValidS3WebsiteBucketName(t *testing.T) {
	for _, bucket := range []string{"my-bucket", "my--bucket", "bucket-123"} {
		if !validS3WebsiteBucketName(bucket) {
			t.Errorf("validS3WebsiteBucketName(%q) = false, want true", bucket)
		}
	}
	for _, bucket := range []string{
		"bad.bucket", "Bad-Bucket", "xn--bucket", "sthree-bucket", "amzn-s3-demo-bucket",
		"bucket-s3alias", "bucket--ol-s3", "bucket--x-s3", "bucket--table-s3",
	} {
		if validS3WebsiteBucketName(bucket) {
			t.Errorf("validS3WebsiteBucketName(%q) = true, want false", bucket)
		}
	}
}

func TestSanitizeS3WebsiteLogValueEscapesLineBreaks(t *testing.T) {
	got := sanitizeS3WebsiteLogValue("team\r\nforged\nentry\rtail")
	if want := `team\r\nforged\nentry\rtail`; got != want {
		t.Fatalf("sanitizeS3WebsiteLogValue() = %q, want %q", got, want)
	}
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("sanitizeS3WebsiteLogValue() retained a line break: %q", got)
	}
}

func TestConnectorSetupSubmissionRoutesExistingServiceAndS3Website(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)

	cases := []struct {
		name           string
		setupType      string
		wantCallbackID string
		wantText       string
	}{
		{
			name:           "existing service",
			setupType:      connectorSetupExistingService,
			wantCallbackID: callbackIDTunnelInstall,
			wantText:       "Local HTTP port",
		},
		{
			name:           "S3 hosted website",
			setupType:      connectorSetupS3Website,
			wantCallbackID: callbackIDS3WebsiteInstall,
			wantText:       "S3 bucket",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			createdAt := fixedNow.Add(-time.Minute).Unix()
			meta := TunnelInstallModalMetadata{
				TeamID:        testAdminTeamID,
				ChannelID:     testTunnelChannelID,
				UserID:        testAdminUserID,
				ResponseURL:   testSlackResponseURL,
				CreatedAtUnix: createdAt,
			}
			body := connectorSetupViewSubmissionBody(t, &meta, tc.setupType)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
			}
			var got map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("unmarshal response: %v body=%s", err, w.Body.String())
			}
			if got[respFieldResponseAction] != respActionUpdate {
				t.Fatalf("response_action = %v, want %s; body=%s", got[respFieldResponseAction], respActionUpdate, w.Body.String())
			}
			viewRaw, ok := got[respFieldView].(map[string]any)
			if !ok {
				t.Fatalf("view = %#v, want object", got[respFieldView])
			}
			if viewRaw[blockKitFieldCallbackID] != tc.wantCallbackID {
				t.Fatalf("callback_id = %v, want %s", viewRaw[blockKitFieldCallbackID], tc.wantCallbackID)
			}
			privateMetadata, _ := viewRaw[blockKitFieldPrivateMetadata].(string)
			var forwarded TunnelInstallModalMetadata
			if err := json.Unmarshal([]byte(privateMetadata), &forwarded); err != nil {
				t.Fatalf("next-modal private_metadata: %v", err)
			}
			if forwarded.CreatedAtUnix != createdAt {
				t.Fatalf("next-modal created_at_unix = %d, want original slash-command timestamp %d", forwarded.CreatedAtUnix, createdAt)
			}
			if !strings.Contains(w.Body.String(), tc.wantText) {
				t.Fatalf("updated view missing %q: %s", tc.wantText, w.Body.String())
			}
		})
	}
}

func TestConnectorSetupSubmissionRejectsExpiredChooser(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	meta := TunnelInstallModalMetadata{
		TeamID:        testAdminTeamID,
		ChannelID:     testTunnelChannelID,
		UserID:        testAdminUserID,
		ResponseURL:   testSlackResponseURL,
		CreatedAtUnix: fixedNow.Add(-tunnelInstallModalTTL - time.Second).Unix(),
	}
	body := connectorSetupViewSubmissionBody(t, &meta, connectorSetupS3Website)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "modal expired") {
		t.Fatalf("modal response = %s, want stale chooser rejection", w.Body.String())
	}
	if strings.Contains(w.Body.String(), callbackIDS3WebsiteInstall) || strings.Contains(w.Body.String(), callbackIDTunnelInstall) {
		t.Fatalf("modal response opened install form despite expired chooser: %s", w.Body.String())
	}
}

func TestConnectorSetupSubmissionRejectsUnknownSetupType(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	meta := TunnelInstallModalMetadata{
		TeamID:        testAdminTeamID,
		ChannelID:     testTunnelChannelID,
		UserID:        testAdminUserID,
		ResponseURL:   testSlackResponseURL,
		CreatedAtUnix: fixedNow.Unix(),
	}
	body := connectorSetupViewSubmissionBody(t, &meta, "unsupported")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Choose one of the listed qURL Connector setup types") {
		t.Fatalf("modal response = %s, want setup-type validation error", w.Body.String())
	}
	if strings.Contains(w.Body.String(), callbackIDS3WebsiteInstall) || strings.Contains(w.Body.String(), callbackIDTunnelInstall) {
		t.Fatalf("modal response opened install form despite unknown setup type: %s", w.Body.String())
	}
}

func TestConnectorSetupSubmissionRejectsChooserIdentityMismatch(t *testing.T) {
	for _, tc := range []struct {
		name, teamID, userID, want string
	}{
		{name: "team", teamID: "T_other", userID: testAdminUserID, want: "different workspace"},
		{name: "user", teamID: testAdminTeamID, userID: "U_other", want: "Only the admin who opened this modal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newAdminTestServers(t)
			ts.seedAdmin(t)
			h := newAdminTestHandler(t, ts)
			h.SetAliasStore(h.cfg.AdminStore)
			meta := TunnelInstallModalMetadata{
				TeamID:        testAdminTeamID,
				ChannelID:     testTunnelChannelID,
				UserID:        testAdminUserID,
				ResponseURL:   testSlackResponseURL,
				CreatedAtUnix: fixedNow.Unix(),
			}
			pm, err := json.Marshal(meta)
			if err != nil {
				t.Fatalf("marshal private_metadata: %v", err)
			}
			body := viewSubmissionBody(t, "V_test_connector_setup", callbackIDConnectorSetup, string(pm), tc.teamID, tc.userID,
				map[string]map[string]interactionStateValue{
					connectorSetupBlockType: {
						connectorSetupActionType: {SelectedOption: &interactionSelectedOption{Value: connectorSetupS3Website}},
					},
				})

			w := httptest.NewRecorder()
			h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("modal response = %s, want identity mismatch rejection %q", w.Body.String(), tc.want)
			}
			if strings.Contains(w.Body.String(), callbackIDS3WebsiteInstall) || strings.Contains(w.Body.String(), callbackIDTunnelInstall) {
				t.Fatalf("modal response opened install form despite identity mismatch: %s", w.Body.String())
			}
		})
	}
}

func TestPrepareS3WebsiteInstallMessageRejectsFloatingOriginImage(t *testing.T) {
	h := NewHandler(Config{
		TunnelImage:   testTunnelImageRef,
		S3OriginImage: "ghcr.io/layervai/qurl-integrations/s3-static-connector:main",
	})

	_, err := h.prepareS3WebsiteInstallMessage(testS3WebsiteArgs(tunnelEnvDocker))

	if err == nil || !strings.Contains(err.Error(), "S3 origin image reference must be digest-pinned") {
		t.Fatalf("prepareS3WebsiteInstallMessage() err = %v, want digest-pin rejection", err)
	}
}

func TestPrepareS3WebsiteInstallMessageComposesImageNotes(t *testing.T) {
	t.Parallel()
	const originNote = "S3 origin image is digest-pinned by default; set `QURL_S3_ORIGIN_IMAGE` to a tested digest when rotating it."
	for _, tc := range []struct {
		name        string
		tunnelImage string
		want        string
	}{
		{name: "default connector image", want: tunnelImageNote(true) + "\n" + originNote},
		{name: "explicit connector image", tunnelImage: testTunnelImageRef, want: originNote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := NewHandler(Config{
				TunnelImage:   tc.tunnelImage,
				S3OriginImage: defaultS3StaticConnectorImage,
			})

			prepared, err := h.prepareS3WebsiteInstallMessage(testS3WebsiteArgs(tunnelEnvDocker))
			if err != nil {
				t.Fatalf("prepareS3WebsiteInstallMessage: %v", err)
			}
			if prepared.imageNote != tc.want {
				t.Fatalf("imageNote = %q, want %q", prepared.imageNote, tc.want)
			}
		})
	}
}

func TestPreparedS3WebsiteInstallMessageOmitsGenericResourceDescription(t *testing.T) {
	t.Parallel()
	h := NewHandler(Config{
		TunnelImage:   testTunnelImageRef,
		S3OriginImage: defaultS3StaticConnectorImage,
	})
	args := testS3WebsiteArgs(tunnelEnvDocker)
	prepared, err := h.prepareS3WebsiteInstallMessage(args)
	if err != nil {
		t.Fatalf("prepareS3WebsiteInstallMessage: %v", err)
	}

	msg, err := prepared.render(args, &client.APIKey{APIKey: testTunnelModalKey}, "", defaultS3WebsiteDescription, fixedNow)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(msg, defaultS3WebsiteDescription) {
		t.Fatalf("message repeats generic resource description:\n%s", msg)
	}
}

func TestKubernetesS3WebsiteInstallMessageFitsBlockDeliveryAtMaxSlug(t *testing.T) {
	h := NewHandler(Config{
		TunnelImage:   testTunnelImageRef,
		S3OriginImage: defaultS3StaticConnectorImage,
	})
	args := testS3WebsiteArgs(tunnelEnvKubernetes)
	args.Slug = "a" + strings.Repeat("b", 62) + "c"

	prepared, err := h.prepareS3WebsiteInstallMessage(args)
	if err != nil {
		t.Fatalf("prepareS3WebsiteInstallMessage: %v", err)
	}
	msg, err := prepared.render(args, &client.APIKey{APIKey: testTunnelModalKey}, "", defaultS3WebsiteDescription, fixedNow)
	if err != nil {
		t.Fatalf("render max-slug Kubernetes message: %v", err)
	}
	if len(msg) > 40_000 {
		t.Fatalf("max-slug Kubernetes message length = %d, exceeds Slack text ceiling", len(msg))
	}
	if _, ok := installMessageBlocks(msg); !ok {
		t.Fatal("max-slug Kubernetes message unexpectedly requires plain-text fallback")
	}
}

func TestS3WebsiteInstallModalSubmissionPinsResourceIdentity(t *testing.T) {
	now := fixedNow
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	var resourceBody map[string]any
	var apiKeyBody map[string]any
	var apiKeyHits int
	ts.addCustomer(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&resourceBody); err != nil {
			t.Fatalf("decode resource body: %v", err)
		}
		respondQURLEnvelope(t, w, map[string]any{
			testKeyResourceID:      testTunnelResourceID,
			testKeyKnockResourceID: testS3WebsiteKnockResource,
			testKeyType:            client.ResourceTypeTunnel,
			testKeySlug:            testTunnelSlug,
			testKeyStatus:          client.StatusActive,
			testKeyDescription:     testS3WebsiteDisplayName,
		})
	})
	ts.addCustomer(http.MethodPost, "/v1/api-keys", func(w http.ResponseWriter, r *http.Request) {
		apiKeyHits++
		if err := json.NewDecoder(r.Body).Decode(&apiKeyBody); err != nil {
			t.Fatalf("decode api key body: %v", err)
		}
		respondQURLEnvelope(t, w, map[string]any{
			testKeyKeyID:      testTunnelAPIKeyID,
			testKeyAPIKey:     testTunnelModalKey,
			testKeyStatus:     client.StatusActive,
			testKeyKeyType:    client.APIKeyTypeTunnelBootstrap,
			testKeyTunnelSlug: testTunnelSlug,
			testKeyExpiresAt:  now.Add(time.Hour).Format(time.RFC3339),
		})
	})

	h := newAdminTestHandler(t, ts)
	freezeTunnelBootstrapNow(t, h, now)
	h.cfg.TunnelImage = testTunnelImageRef
	h.cfg.S3OriginImage = testS3OriginImageRef
	dmPosts := captureTunnelPostDMSuccess(h)
	h.SetAliasStore(h.cfg.AdminStore)
	inv := newAdminSlashInvoker(t, h)
	meta := TunnelInstallModalMetadata{
		TeamID:        testAdminTeamID,
		EnterpriseID:  testEnterpriseID,
		ChannelID:     testTunnelChannelID,
		UserID:        testAdminUserID,
		ResponseURL:   inv.responseU.URL,
		CreatedAtUnix: now.Unix(),
	}
	body := s3WebsiteInstallViewSubmissionBody(t, &meta, s3WebsiteInstallModalValues(
		testTunnelSlug,
		"$team-dash",
		string(tunnelEnvDocker),
		testS3WebsiteBucket,
		testS3WebsiteRegion,
		testS3WebsitePrefix,
		testS3WebsiteIndex,
	))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	if strings.TrimSpace(w.Body.String()) != "{}" {
		t.Fatalf("modal ack = %q, want empty JSON object", w.Body.String())
	}
	async := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
	if resourceBody[testKeyType] != client.ResourceTypeTunnel || resourceBody[testKeySlug] != testTunnelSlug || resourceBody["find_or_create"] != true {
		t.Errorf("resource body = %+v, want connector find-or-create slug", resourceBody)
	}
	if got := asString(resourceBody[testKeyDescription]); got != defaultS3WebsiteDescription {
		t.Errorf("resource description = %q, want privacy-safe %q", got, defaultS3WebsiteDescription)
	}
	if apiKeyHits != 1 {
		t.Fatalf("api key hits = %d, want 1", apiKeyHits)
	}
	if apiKeyBody[testKeyKeyType] != client.APIKeyTypeTunnelBootstrap || apiKeyBody[testKeyTunnelSlug] != testTunnelSlug {
		t.Errorf("api key body = %+v, want connector bootstrap key", apiKeyBody)
	}
	if len(*dmPosts) != 1 || !strings.Contains((*dmPosts)[0].text, testTunnelModalKey) {
		t.Fatalf("bootstrap DM posts = %+v, want one containing modal key", *dmPosts)
	}
	for _, want := range []string{
		"S3 website qURL Connector `" + testTunnelSlug + "`",
		testS3WebsiteDisplayName,
		"qURL alias `$team-dash` is ready in this channel.",
		"generated qURL Connector config already includes the qURL resource details",
		"qURL Connector image: `" + testTunnelImageRef + "`",
		"S3 origin image: `" + testS3OriginImageRef + "`",
		"S3_BUCKET='" + testS3WebsiteBucket + "'",
		"AWS_REGION='" + testS3WebsiteRegion + "'",
		"S3_PREFIX='" + testS3WebsitePrefix + "'",
		"INDEX_DOCUMENT='" + testS3WebsiteIndex + "'",
		"resource_id: '" + testTunnelResourceID + "'",
		`--network "container:${ORIGIN_CONTAINER}"`,
		"/qurl get $team-dash",
	} {
		if !strings.Contains(async, want) {
			t.Errorf("async reply missing %q:\n%s", want, async)
		}
	}
	for _, forbidden := range []string{testTunnelModalKey, testForbiddenSlackShellFence, testForbiddenSlackYAMLFence, "find_or_create", "YOUR_WEB_CONTAINER_NAME", "tunnel", "knock_resource_id", "LAYERV_KNOCK_RESOURCE_ID"} {
		if strings.Contains(async, forbidden) {
			t.Errorf("async reply leaked %q:\n%s", forbidden, async)
		}
	}
	gotRID, found, err := h.cfg.AdminStore.LookupChannelAlias(context.Background(), testAdminTeamID, testTunnelChannelID, "team-dash")
	if err != nil {
		t.Fatalf("LookupChannelAlias: %v", err)
	}
	if !found || gotRID != testTunnelResourceID {
		t.Fatalf("alias lookup = (%q, %v), want (%s, true)", gotRID, found, testTunnelResourceID)
	}
}

func TestS3WebsiteInstallDMFailureMissingScopeIncludesInstallHint(t *testing.T) {
	now := fixedNow
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	var revokeHits int
	ts.addCustomer(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		respondQURLEnvelope(t, w, map[string]any{
			testKeyResourceID:      testTunnelResourceID,
			testKeyKnockResourceID: testS3WebsiteKnockResource,
			testKeyType:            client.ResourceTypeTunnel,
			testKeySlug:            testTunnelSlug,
			testKeyStatus:          client.StatusActive,
		})
	})
	ts.addCustomer(http.MethodPost, "/v1/api-keys", func(w http.ResponseWriter, _ *http.Request) {
		respondQURLEnvelope(t, w, map[string]any{
			testKeyKeyID:      testTunnelAPIKeyID,
			testKeyAPIKey:     testTunnelModalKey,
			testKeyStatus:     client.StatusActive,
			testKeyKeyType:    client.APIKeyTypeTunnelBootstrap,
			testKeyTunnelSlug: testTunnelSlug,
			testKeyExpiresAt:  now.Add(time.Hour).Format(time.RFC3339),
		})
	})
	ts.addCustomer(http.MethodDelete, "/v1/api-keys/"+testTunnelAPIKeyID, func(w http.ResponseWriter, _ *http.Request) {
		revokeHits++
		w.WriteHeader(http.StatusNoContent)
	})

	h := newAdminTestHandler(t, ts)
	freezeTunnelBootstrapNow(t, h, now)
	h.SetSlackInstallURL("https://slack-bot.example/oauth/slack/install")
	h.cfg.TunnelImage = testTunnelImageRef
	h.cfg.PostDM = func(context.Context, string, string, string, string) error {
		return fmt.Errorf("chat.postMessage: %w", ErrSlackMissingScope)
	}
	h.SetAliasStore(h.cfg.AdminStore)
	inv := newAdminSlashInvoker(t, h)
	meta := TunnelInstallModalMetadata{
		TeamID:        testAdminTeamID,
		ChannelID:     testTunnelChannelID,
		UserID:        testAdminUserID,
		ResponseURL:   inv.responseU.URL,
		CreatedAtUnix: now.Unix(),
	}
	body := s3WebsiteInstallViewSubmissionBody(t, &meta, s3WebsiteInstallModalValues(
		testTunnelSlug,
		"$team-dash",
		string(tunnelEnvDocker),
		testS3WebsiteBucket,
		testS3WebsiteRegion,
		testS3WebsitePrefix,
		testS3WebsiteIndex,
	))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	failure := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
	if revokeHits != 1 {
		t.Fatalf("bootstrap key revoke hits = %d, want 1", revokeHits)
	}
	for _, want := range []string{
		"temporary key was revoked",
		testSlackAppInstallHint,
		testSlackAppInstallLink,
		testProtectConnectorCmd,
	} {
		if !strings.Contains(failure, want) {
			t.Fatalf("failure notice = %s, missing %q", failure, want)
		}
	}
	for _, forbidden := range []string{testTunnelModalKey, testInstallBlockLead, testDockerRunDetached} {
		if strings.Contains(failure, forbidden) {
			t.Fatalf("missing-scope notice leaked install secret/details %q: %s", forbidden, failure)
		}
	}
}

func TestS3WebsiteInstallRefusesWhenPostDMUnwiredBeforeMintingKey(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	var resourceHits, apiKeyHits int
	ts.addCustomer(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		resourceHits++
		w.WriteHeader(http.StatusInternalServerError)
	})
	ts.addCustomer(http.MethodPost, "/v1/api-keys", func(w http.ResponseWriter, _ *http.Request) {
		apiKeyHits++
		w.WriteHeader(http.StatusInternalServerError)
	})

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	inv := newAdminSlashInvoker(t, h)
	h.processS3WebsiteInstall(context.Background(), slog.Default(), testS3WebsiteInstallRequest(inv.responseU.URL, fixedNow, tunnelEnvDocker))

	async := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
	if !strings.Contains(async, "No bootstrap key was minted") || !strings.Contains(async, "Slack DM delivery") {
		t.Fatalf("async reply = %q, want DM-unwired pre-mint refusal", async)
	}
	if resourceHits != 0 || apiKeyHits != 0 {
		t.Fatalf("resource/api-key hits = %d/%d, want 0/0 before DM wiring", resourceHits, apiKeyHits)
	}
}

func TestS3WebsiteInstallInstructionsDeliveryFailureRevokesAndSendsDiscardNotices(t *testing.T) {
	now := fixedNow
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	var revokeHits int
	ts.addCustomer(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		respondQURLEnvelope(t, w, map[string]any{
			testKeyResourceID:      testTunnelResourceID,
			testKeyKnockResourceID: testS3WebsiteKnockResource,
			testKeyType:            client.ResourceTypeTunnel,
			testKeySlug:            testTunnelSlug,
			testKeyStatus:          client.StatusActive,
		})
	})
	ts.addCustomer(http.MethodPost, "/v1/api-keys", func(w http.ResponseWriter, _ *http.Request) {
		respondQURLEnvelope(t, w, map[string]any{
			testKeyKeyID:      testTunnelAPIKeyID,
			testKeyAPIKey:     testTunnelModalKey,
			testKeyStatus:     client.StatusActive,
			testKeyKeyType:    client.APIKeyTypeTunnelBootstrap,
			testKeyTunnelSlug: testTunnelSlug,
			testKeyExpiresAt:  now.Add(time.Hour).Format(time.RFC3339),
		})
	})
	ts.addCustomer(http.MethodDelete, "/v1/api-keys/"+testTunnelAPIKeyID, func(w http.ResponseWriter, _ *http.Request) {
		revokeHits++
		w.WriteHeader(http.StatusNoContent)
	})

	var responseBodies []string
	responseURL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read response_url body: %v", err)
		}
		responseBodies = append(responseBodies, string(body))
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(responseURL.Close)

	h := newAdminTestHandler(t, ts)
	freezeTunnelBootstrapNow(t, h, now)
	h.cfg.TunnelImage = testTunnelImageRef
	h.cfg.S3OriginImage = testS3OriginImageRef
	dmPosts := captureTunnelPostDMSuccess(h)
	h.SetAliasStore(h.cfg.AdminStore)
	h.processS3WebsiteInstall(context.Background(), slog.Default(), testS3WebsiteInstallRequest(responseURL.URL, now, tunnelEnvDocker))

	if revokeHits != 1 {
		t.Fatalf("bootstrap key revoke hits = %d, want 1", revokeHits)
	}
	if len(responseBodies) != 4 {
		t.Fatalf("response_url posts = %d, want 4 (blocks attempt, text fallback, text retry, discard notice): %v", len(responseBodies), responseBodies)
	}
	if strings.Contains(strings.Join(responseBodies, "\n"), testTunnelModalKey) {
		t.Fatalf("response_url bodies leaked bootstrap key: %v", responseBodies)
	}
	last := responseBodies[len(responseBodies)-1]
	if !strings.Contains(last, "bootstrap key was revoked") || !strings.Contains(last, "discard") {
		t.Fatalf("last response_url body = %q, want revoked-key discard follow-up", last)
	}
	if len(*dmPosts) != 2 {
		t.Fatalf("bootstrap DM posts = %d, want key DM and discard notice: %+v", len(*dmPosts), *dmPosts)
	}
	if !strings.Contains((*dmPosts)[0].text, testTunnelModalKey) {
		t.Fatalf("first DM = %q, want bootstrap key", (*dmPosts)[0].text)
	}
	if strings.Contains((*dmPosts)[1].text, testTunnelModalKey) || !strings.Contains((*dmPosts)[1].text, "was revoked") || !strings.Contains((*dmPosts)[1].text, "Discard that key") {
		t.Fatalf("second DM = %q, want discard notice without key", (*dmPosts)[1].text)
	}
}

func TestS3WebsiteInstallRevokesWhenAPIKeyPlaintextMissing(t *testing.T) {
	now := fixedNow
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	var revokeHits int
	ts.addCustomer(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		respondQURLEnvelope(t, w, map[string]any{
			testKeyResourceID:      testTunnelResourceID,
			testKeyKnockResourceID: testS3WebsiteKnockResource,
			testKeyType:            client.ResourceTypeTunnel,
			testKeySlug:            testTunnelSlug,
			testKeyStatus:          client.StatusActive,
		})
	})
	ts.addCustomer(http.MethodPost, "/v1/api-keys", func(w http.ResponseWriter, _ *http.Request) {
		respondQURLEnvelope(t, w, map[string]any{
			testKeyKeyID:      testTunnelAPIKeyID,
			testKeyAPIKey:     "",
			testKeyStatus:     client.StatusActive,
			testKeyKeyType:    client.APIKeyTypeTunnelBootstrap,
			testKeyTunnelSlug: testTunnelSlug,
		})
	})
	ts.addCustomer(http.MethodDelete, "/v1/api-keys/"+testTunnelAPIKeyID, func(w http.ResponseWriter, _ *http.Request) {
		revokeHits++
		w.WriteHeader(http.StatusNoContent)
	})

	h := newAdminTestHandler(t, ts)
	freezeTunnelBootstrapNow(t, h, now)
	h.cfg.TunnelImage = testTunnelImageRef
	dmPosts := captureTunnelPostDMSuccess(h)
	h.SetAliasStore(h.cfg.AdminStore)
	inv := newAdminSlashInvoker(t, h)
	h.processS3WebsiteInstall(context.Background(), slog.Default(), testS3WebsiteInstallRequest(inv.responseU.URL, now, tunnelEnvDocker))

	async := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
	if !strings.Contains(async, "did not return a bootstrap key") {
		t.Fatalf("async reply = %q, want missing-plaintext copy", async)
	}
	if revokeHits != 1 {
		t.Fatalf("bootstrap key revoke hits = %d, want 1", revokeHits)
	}
	if len(*dmPosts) != 0 {
		t.Fatalf("bootstrap DM posts = %+v, want none when build revokes before DM delivery", *dmPosts)
	}
}

func TestS3WebsiteInstallRevokesWhenAPIKeyFailsShellValidation(t *testing.T) {
	now := fixedNow
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	var revokeHits int
	ts.addCustomer(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		respondQURLEnvelope(t, w, map[string]any{
			testKeyResourceID:      testTunnelResourceID,
			testKeyKnockResourceID: testS3WebsiteKnockResource,
			testKeyType:            client.ResourceTypeTunnel,
			testKeySlug:            testTunnelSlug,
			testKeyStatus:          client.StatusActive,
		})
	})
	ts.addCustomer(http.MethodPost, "/v1/api-keys", func(w http.ResponseWriter, _ *http.Request) {
		respondQURLEnvelope(t, w, map[string]any{
			testKeyKeyID:      testTunnelAPIKeyID,
			testKeyAPIKey:     "lv_live_bad$bootstrap",
			testKeyStatus:     client.StatusActive,
			testKeyKeyType:    client.APIKeyTypeTunnelBootstrap,
			testKeyTunnelSlug: testTunnelSlug,
		})
	})
	ts.addCustomer(http.MethodDelete, "/v1/api-keys/"+testTunnelAPIKeyID, func(w http.ResponseWriter, _ *http.Request) {
		revokeHits++
		w.WriteHeader(http.StatusNoContent)
	})

	h := newAdminTestHandler(t, ts)
	freezeTunnelBootstrapNow(t, h, now)
	h.cfg.TunnelImage = testTunnelImageRef
	dmPosts := captureTunnelPostDMSuccess(h)
	h.SetAliasStore(h.cfg.AdminStore)
	inv := newAdminSlashInvoker(t, h)
	h.processS3WebsiteInstall(context.Background(), slog.Default(), testS3WebsiteInstallRequest(inv.responseU.URL, now, tunnelEnvDocker))

	async := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
	if !strings.Contains(async, "unexpected format") {
		t.Fatalf("async reply = %q, want shell-validation failure copy", async)
	}
	if strings.Contains(async, "lv_live_bad$bootstrap") {
		t.Fatalf("async reply leaked rejected bootstrap key: %q", async)
	}
	if revokeHits != 1 {
		t.Fatalf("bootstrap key revoke hits = %d, want 1", revokeHits)
	}
	if len(*dmPosts) != 0 {
		t.Fatalf("bootstrap DM posts = %+v, want none when build revokes before DM delivery", *dmPosts)
	}
}

func TestS3WebsiteInstallRejectsIncompleteResourceBeforeMintingBootstrapKey(t *testing.T) {
	now := fixedNow
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	var apiKeyHits int
	ts.addCustomer(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
		respondQURLEnvelope(t, w, map[string]any{
			testKeyResourceID:         testTunnelResourceID,
			testKeyConnectorRoutingID: "",
			testKeyType:               client.ResourceTypeTunnel,
			testKeySlug:               testTunnelSlug,
			testKeyStatus:             client.StatusActive,
		})
	})
	ts.addCustomer(http.MethodPost, "/v1/api-keys", func(w http.ResponseWriter, r *http.Request) {
		apiKeyHits++
		t.Fatalf("api key should not be minted when qURL resource identity is incomplete")
	})

	h := newAdminTestHandler(t, ts)
	freezeTunnelBootstrapNow(t, h, now)
	h.cfg.TunnelImage = testTunnelImageRef
	dmPosts := captureTunnelPostDMSuccess(h)
	h.SetAliasStore(h.cfg.AdminStore)
	inv := newAdminSlashInvoker(t, h)
	meta := TunnelInstallModalMetadata{
		TeamID:        testAdminTeamID,
		ChannelID:     testTunnelChannelID,
		UserID:        testAdminUserID,
		ResponseURL:   inv.responseU.URL,
		CreatedAtUnix: now.Unix(),
	}
	body := s3WebsiteInstallViewSubmissionBody(t, &meta, s3WebsiteInstallModalValues(
		testTunnelSlug,
		"$team-dash",
		string(tunnelEnvDocker),
		testS3WebsiteBucket,
		testS3WebsiteRegion,
		"",
		testS3WebsiteIndex,
	))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	async := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
	if apiKeyHits != 0 {
		t.Fatalf("api key hits = %d, want 0", apiKeyHits)
	}
	if len(*dmPosts) != 0 {
		t.Fatalf("bootstrap DM posts = %+v, want none", *dmPosts)
	}
	if !strings.Contains(async, "No bootstrap key was minted") || !strings.Contains(async, "connector_routing_id") {
		t.Fatalf("async reply = %q, want incomplete identity error before key mint", async)
	}
	if _, found, err := h.cfg.AdminStore.LookupChannelAlias(context.Background(), testAdminTeamID, testTunnelChannelID, "team-dash"); err != nil || found {
		t.Fatalf("alias lookup found=%v err=%v, want no alias bound before complete identity", found, err)
	}
}

func TestS3WebsiteInstallModalRejectsExpiredModal(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	meta := TunnelInstallModalMetadata{
		TeamID:        testAdminTeamID,
		ChannelID:     testTunnelChannelID,
		UserID:        testAdminUserID,
		ResponseURL:   testSlackResponseURL,
		CreatedAtUnix: fixedNow.Add(-tunnelInstallModalTTL - time.Second).Unix(),
	}
	body := s3WebsiteInstallViewSubmissionBody(t, &meta, s3WebsiteInstallModalValues(
		testTunnelSlug,
		"$team-dash",
		string(tunnelEnvDocker),
		testS3WebsiteBucket,
		testS3WebsiteRegion,
		testS3WebsitePrefix,
		testS3WebsiteIndex,
	))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "This modal expired") {
		t.Fatalf("modal response = %s, want expiry rejection", w.Body.String())
	}
}

func TestS3WebsiteInstallModalChecksFreshnessBeforeFields(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	meta := TunnelInstallModalMetadata{
		TeamID:        testAdminTeamID,
		ChannelID:     testTunnelChannelID,
		UserID:        testAdminUserID,
		ResponseURL:   testSlackResponseURL,
		CreatedAtUnix: fixedNow.Add(-tunnelInstallModalTTL - time.Second).Unix(),
	}
	body := s3WebsiteInstallViewSubmissionBody(t, &meta, s3WebsiteInstallModalValues(
		"bad slug with spaces",
		"$team-dash",
		string(tunnelEnvDocker),
		testS3WebsiteBucket,
		testS3WebsiteRegion,
		testS3WebsitePrefix,
		testS3WebsiteIndex,
	))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "This modal expired") {
		t.Fatalf("modal response = %s, want expiry rejection before field validation", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "Use 3-64 lowercase") {
		t.Fatalf("modal response returned field validation before freshness check: %s", w.Body.String())
	}
}

func TestS3WebsiteInstallModalRejectsNonAdmin(t *testing.T) {
	const nonAdminUserID = "UMEMBER01"

	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	meta := TunnelInstallModalMetadata{
		TeamID:        testAdminTeamID,
		ChannelID:     testTunnelChannelID,
		UserID:        nonAdminUserID,
		ResponseURL:   testSlackResponseURL,
		CreatedAtUnix: fixedNow.Unix(),
	}
	body := s3WebsiteInstallViewSubmissionBody(t, &meta, s3WebsiteInstallModalValues(
		"INVALID SLUG",
		"$team-dash",
		string(tunnelEnvDocker),
		"bad.bucket",
		testS3WebsiteRegion,
		testS3WebsitePrefix,
		testS3WebsiteIndex,
	))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "admin-only") {
		t.Fatalf("modal response = %s, want admin-only rejection", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "Use 3-64 lowercase") || strings.Contains(w.Body.String(), "non-dotted") {
		t.Fatalf("modal response exposed field validation before admin rejection: %s", w.Body.String())
	}
}

func TestParseS3WebsiteInstallModalArgsValidatesS3Fields(t *testing.T) {
	values := s3WebsiteInstallModalValues(
		testTunnelSlug,
		"$team-dash",
		string(tunnelEnvDocker),
		"bad.bucket",
		"us-gov-west-1",
		"website//prod",
		"../index.html",
	)
	args, fieldErrors := parseS3WebsiteInstallModalArgs(values)
	if args != nil {
		t.Fatalf("args = %+v, want nil", args)
	}
	for _, blockID := range []string{
		s3WebsiteInstallBlockBucket,
		s3WebsiteInstallBlockRegion,
		s3WebsiteInstallBlockPrefix,
		s3WebsiteInstallBlockIndex,
	} {
		if _, ok := fieldErrors[blockID]; !ok {
			t.Fatalf("fieldErrors missing %s: %+v", blockID, fieldErrors)
		}
	}
}

func TestParseS3WebsiteInstallModalArgsRejectsComposeShellMetacharacters(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		blockID  string
		actionID string
		value    string
	}{
		{
			name:     "slug dollar",
			blockID:  s3WebsiteInstallBlockSlug,
			actionID: s3WebsiteInstallActionSlug,
			value:    "stats$site",
		},
		{
			name:     "slug backtick",
			blockID:  s3WebsiteInstallBlockSlug,
			actionID: s3WebsiteInstallActionSlug,
			value:    "stats`site",
		},
		{
			name:     "slug backslash",
			blockID:  s3WebsiteInstallBlockSlug,
			actionID: s3WebsiteInstallActionSlug,
			value:    `stats\site`,
		},
		{
			name:     "slug whitespace",
			blockID:  s3WebsiteInstallBlockSlug,
			actionID: s3WebsiteInstallActionSlug,
			value:    "stats site",
		},
		{
			name:     "bucket dollar",
			blockID:  s3WebsiteInstallBlockBucket,
			actionID: s3WebsiteInstallActionBucket,
			value:    "stats$site",
		},
		{
			name:     "bucket backtick",
			blockID:  s3WebsiteInstallBlockBucket,
			actionID: s3WebsiteInstallActionBucket,
			value:    "stats`site",
		},
		{
			name:     "bucket backslash",
			blockID:  s3WebsiteInstallBlockBucket,
			actionID: s3WebsiteInstallActionBucket,
			value:    `stats\site`,
		},
		{
			name:     "bucket whitespace",
			blockID:  s3WebsiteInstallBlockBucket,
			actionID: s3WebsiteInstallActionBucket,
			value:    "stats site",
		},
		{
			name:     "region dollar",
			blockID:  s3WebsiteInstallBlockRegion,
			actionID: s3WebsiteInstallActionRegion,
			value:    "us$east-1",
		},
		{
			name:     "region backtick",
			blockID:  s3WebsiteInstallBlockRegion,
			actionID: s3WebsiteInstallActionRegion,
			value:    "us`east-1",
		},
		{
			name:     "region backslash",
			blockID:  s3WebsiteInstallBlockRegion,
			actionID: s3WebsiteInstallActionRegion,
			value:    `us\east-1`,
		},
		{
			name:     "region whitespace",
			blockID:  s3WebsiteInstallBlockRegion,
			actionID: s3WebsiteInstallActionRegion,
			value:    "us east-1",
		},
		{
			name:     "prefix dollar",
			blockID:  s3WebsiteInstallBlockPrefix,
			actionID: s3WebsiteInstallActionPrefix,
			value:    "website$prod",
		},
		{
			name:     "prefix backtick",
			blockID:  s3WebsiteInstallBlockPrefix,
			actionID: s3WebsiteInstallActionPrefix,
			value:    "website`prod",
		},
		{
			name:     "prefix backslash",
			blockID:  s3WebsiteInstallBlockPrefix,
			actionID: s3WebsiteInstallActionPrefix,
			value:    `website\prod`,
		},
		{
			name:     "prefix whitespace",
			blockID:  s3WebsiteInstallBlockPrefix,
			actionID: s3WebsiteInstallActionPrefix,
			value:    "website prod",
		},
		{
			name:     "index dollar",
			blockID:  s3WebsiteInstallBlockIndex,
			actionID: s3WebsiteInstallActionIndex,
			value:    "index$.html",
		},
		{
			name:     "index backtick",
			blockID:  s3WebsiteInstallBlockIndex,
			actionID: s3WebsiteInstallActionIndex,
			value:    "index`.html",
		},
		{
			name:     "index backslash",
			blockID:  s3WebsiteInstallBlockIndex,
			actionID: s3WebsiteInstallActionIndex,
			value:    `index\.html`,
		},
		{
			name:     "index whitespace",
			blockID:  s3WebsiteInstallBlockIndex,
			actionID: s3WebsiteInstallActionIndex,
			value:    "index file.html",
		},
		{
			name: "baseline valid",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			values := s3WebsiteInstallModalValues(
				testTunnelSlug,
				"$team-dash",
				string(tunnelEnvDocker),
				testS3WebsiteBucket,
				testS3WebsiteRegion,
				testS3WebsitePrefix,
				testS3WebsiteIndex,
			)
			if tc.blockID != "" {
				state := values[tc.blockID][tc.actionID]
				state.Value = tc.value
				values[tc.blockID][tc.actionID] = state
			}
			args, fieldErrors := parseS3WebsiteInstallModalArgs(values)
			if tc.blockID == "" {
				if len(fieldErrors) > 0 {
					t.Fatalf("fieldErrors = %+v, want none", fieldErrors)
				}
				if args == nil {
					t.Fatal("args = nil, want parsed args")
				}
				return
			}
			if args != nil {
				t.Fatalf("args = %+v, want nil", args)
			}
			if _, ok := fieldErrors[tc.blockID]; !ok {
				t.Fatalf("fieldErrors missing %s: %+v", tc.blockID, fieldErrors)
			}
		})
	}
}

func TestParseS3WebsiteInstallModalArgsRejectsDotDotPrefixSegment(t *testing.T) {
	t.Parallel()

	values := s3WebsiteInstallModalValues(
		testTunnelSlug,
		"$team-dash",
		string(tunnelEnvDocker),
		testS3WebsiteBucket,
		testS3WebsiteRegion,
		"a/../b",
		testS3WebsiteIndex,
	)

	args, fieldErrors := parseS3WebsiteInstallModalArgs(values)

	if args != nil {
		t.Fatalf("args = %+v, want nil", args)
	}
	if _, ok := fieldErrors[s3WebsiteInstallBlockPrefix]; !ok {
		t.Fatalf("fieldErrors missing prefix traversal rejection: %+v", fieldErrors)
	}
}

func TestParseS3WebsiteInstallModalArgsDefaultsDirectoryIndex(t *testing.T) {
	values := s3WebsiteInstallModalValues(
		testTunnelSlug,
		"$team-dash",
		string(tunnelEnvDocker),
		testS3WebsiteBucket,
		testS3WebsiteRegion,
		testS3WebsitePrefix,
		"",
	)
	args, fieldErrors := parseS3WebsiteInstallModalArgs(values)
	if len(fieldErrors) > 0 {
		t.Fatalf("fieldErrors = %+v, want none", fieldErrors)
	}
	if args == nil {
		t.Fatal("args = nil, want parsed args")
	}
	if args.IndexDocument != defaultS3WebsiteIndexDocument {
		t.Fatalf("IndexDocument = %q, want %q", args.IndexDocument, defaultS3WebsiteIndexDocument)
	}
}

func TestParseS3WebsiteInstallModalArgsAllowsDotLeadingIndex(t *testing.T) {
	values := s3WebsiteInstallModalValues(
		testTunnelSlug,
		"$team-dash",
		string(tunnelEnvDocker),
		testS3WebsiteBucket,
		testS3WebsiteRegion,
		testS3WebsitePrefix,
		".htaccess",
	)
	args, fieldErrors := parseS3WebsiteInstallModalArgs(values)
	if len(fieldErrors) > 0 || args == nil {
		t.Fatalf("args = %+v, fieldErrors = %+v, want accepted dot-leading index", args, fieldErrors)
	}
	if args.IndexDocument != ".htaccess" {
		t.Fatalf("IndexDocument = %q, want .htaccess", args.IndexDocument)
	}
}

func TestParseS3WebsiteInstallModalArgsRejectsPunctuationOnlyIndex(t *testing.T) {
	t.Parallel()

	for _, index := range []string{".", "..", "...", "-", "___", "-._-"} {
		t.Run(index, func(t *testing.T) {
			t.Parallel()
			values := s3WebsiteInstallModalValues(
				testTunnelSlug,
				"$team-dash",
				string(tunnelEnvDocker),
				testS3WebsiteBucket,
				testS3WebsiteRegion,
				testS3WebsitePrefix,
				index,
			)
			args, fieldErrors := parseS3WebsiteInstallModalArgs(values)
			if args != nil {
				t.Fatalf("args = %+v, want nil", args)
			}
			if _, ok := fieldErrors[s3WebsiteInstallBlockIndex]; !ok {
				t.Fatalf("fieldErrors missing index for %q: %+v", index, fieldErrors)
			}
		})
	}
}

func TestParseS3WebsiteInstallModalArgsRejectsUnsupportedPartitions(t *testing.T) {
	for _, region := range []string{"cn-north-1", "us-gov-west-1", "us-iso-east-1", "us-isob-east-1"} {
		t.Run(region, func(t *testing.T) {
			values := s3WebsiteInstallModalValues(
				testTunnelSlug,
				"$team-dash",
				string(tunnelEnvDocker),
				testS3WebsiteBucket,
				region,
				testS3WebsitePrefix,
				testS3WebsiteIndex,
			)
			args, fieldErrors := parseS3WebsiteInstallModalArgs(values)
			if args != nil {
				t.Fatalf("args = %+v, want nil", args)
			}
			if _, ok := fieldErrors[s3WebsiteInstallBlockRegion]; !ok {
				t.Fatalf("fieldErrors missing region for %s: %+v", region, fieldErrors)
			}
		})
	}
}

func TestRenderS3WebsiteConnectorConfigYAMLPinsResourceIdentity(t *testing.T) {
	configYAML, err := renderS3WebsiteConnectorConfigYAML(testS3WebsiteArgs(tunnelEnvDocker))
	if err != nil {
		t.Fatalf("renderS3WebsiteConnectorConfigYAML: %v", err)
	}
	for _, want := range []string{
		"routes:",
		"id: '" + testTunnelSlug + "'",
		"type: http",
		"local_ip: 127.0.0.1",
		"local_port: 8080",
		"resource_id: '" + testTunnelResourceID + "'",
		"connector_routing_id: '" + testTunnelRoutingID + "'",
	} {
		if !strings.Contains(configYAML, want) {
			t.Fatalf("config missing %q:\n%s", want, configYAML)
		}
	}
	if strings.Contains(configYAML, "knock_resource_id") {
		t.Fatalf("config rendered runtime-only knock_resource_id:\n%s", configYAML)
	}
	missingResource := *testS3WebsiteArgs(tunnelEnvDocker)
	missingResource.ResourceID = ""
	if _, err := renderS3WebsiteConnectorConfigYAML(&missingResource); err == nil || !strings.Contains(err.Error(), "resource_id") {
		t.Fatalf("render without resource_id err = %v, want resource_id rejection", err)
	}
}

func TestS3WebsiteInstallArgsRequirePinnedTunnelResourceValidatesAPIURL(t *testing.T) {
	args := testS3WebsiteArgs(tunnelEnvDocker)
	args.APIURL = testInvalidRemoteConnectorAPIURL

	err := args.requirePinnedConnectorResource()

	if err == nil || !strings.Contains(err.Error(), "QURL_API_URL is invalid") {
		t.Fatalf("requirePinnedConnectorResource err = %v, want invalid API URL", err)
	}
}

func TestRenderDockerS3WebsiteInstructionsMentionsOriginAutoRestart(t *testing.T) {
	got, err := renderDockerS3WebsiteInstructions(testS3WebsiteArgs(tunnelEnvDocker), testTunnelImageRef, defaultS3StaticConnectorImage)
	if err != nil {
		t.Fatalf("renderDockerS3WebsiteInstructions: %v", err)
	}
	for _, want := range []string{
		"Docker auto-restarts it after a crash",
		"recreate or restart the qURL Connector container",
		"QURL_API_URL='" + testTunnelAPIURL + "'",
		`$SUDO chmod 0644 "$CONFIG_FILE"`,
		`AUDIT_DIR="/var/log/layerv/qurl-connector/${QURL_CONNECTOR_ID}"`,
		`$SUDO install -d -m 0700 -o 65532 -g 65532 "$AUDIT_DIR"`,
		"--read-only",
		"--tmpfs /tmp:rw,size=64m",
		"--pids-limit=512",
		`-v "$AUDIT_DIR:/var/log/layerv/qurl-connector"`,
		"-e QURL_AUDIT_FILE='/var/log/layerv/qurl-connector/audit.log'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Docker instructions missing %q:\n%s", want, got)
		}
	}
	for _, want := range []string{"--user 65532:65532", "--cap-drop=ALL", "--security-opt=no-new-privileges:true"} {
		if gotCount := strings.Count(got, want); gotCount != 2 {
			t.Fatalf("Docker instructions contain %q %d times, want once per container:\n%s", want, gotCount, got)
		}
	}
	if strings.Contains(got, "QURL_BOOTSTRAP_URL") {
		t.Fatalf("Docker instructions rendered retired bootstrap URL:\n%s", got)
	}
	assertNoS3SecretLeaks(t, got)
}

func TestRenderDockerS3WebsiteInstructionsEmitsValidShell(t *testing.T) {
	t.Parallel()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	got, err := renderDockerS3WebsiteInstructions(testS3WebsiteArgs(tunnelEnvDocker), testTunnelImageRef, defaultS3StaticConnectorImage)
	if err != nil {
		t.Fatalf("renderDockerS3WebsiteInstructions: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, sh, "-n") //nolint:gosec // G204: sh comes from exec.LookPath and no user input reaches argv.
	cmd.Stdin = strings.NewReader(firstSlackCodeBlock(t, got))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Docker S3 website shell block did not parse: %v\n%s", err, out)
	}
}

func TestRenderDockerComposeS3WebsiteInstructionsEmitsParseableCompose(t *testing.T) {
	got, err := renderDockerComposeS3WebsiteInstructions(testS3WebsiteArgs(tunnelEnvCompose), testTunnelImageRef, defaultS3StaticConnectorImage)
	if err != nil {
		t.Fatalf("renderDockerComposeS3WebsiteInstructions: %v", err)
	}
	if !strings.Contains(got, `$SUDO chmod 0644 "$CONFIG_FILE"`) {
		t.Fatalf("Compose instructions do not make connector config readable by UID 65532:\n%s", got)
	}
	body := extractS3TestBlock(t, got, "cat > \"$QURL_COMPOSE_FILE\" <<QURL_COMPOSE_YAML_EOF\n", "\nQURL_COMPOSE_YAML_EOF")

	var parsed struct {
		Services map[string]struct {
			Image       string            `yaml:"image"`
			User        string            `yaml:"user"`
			Restart     string            `yaml:"restart"`
			Environment map[string]string `yaml:"environment"`
			NetworkMode string            `yaml:"network_mode"`
			Volumes     []string          `yaml:"volumes"`
			CapDrop     []string          `yaml:"cap_drop"`
			SecurityOpt []string          `yaml:"security_opt"`
			ReadOnly    bool              `yaml:"read_only"`
			Tmpfs       []string          `yaml:"tmpfs"`
			PidsLimit   int               `yaml:"pids_limit"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("Compose fragment did not parse: %v\n%s", err, body)
	}
	origin := parsed.Services["qurl-s3-origin-"+testTunnelSlug]
	if origin.Image != defaultS3StaticConnectorImage {
		t.Fatalf("origin image = %q, want %q", origin.Image, defaultS3StaticConnectorImage)
	}
	for name, want := range map[string]string{
		testS3EnvBucket:         testS3WebsiteBucket,
		testS3EnvRegion:         testS3WebsiteRegion,
		testS3EnvPrefix:         testS3WebsitePrefix,
		testS3EnvIndex:          testS3WebsiteIndex,
		testS3EnvCacheConnector: testTunnelSlug,
	} {
		if got := origin.Environment[name]; got != want {
			t.Fatalf("origin env %s = %q, want %q", name, got, want)
		}
	}
	connector := parsed.Services["qurl-connector-"+testTunnelSlug]
	if connector.Image != testTunnelImageRef {
		t.Fatalf("connector image = %q, want %q", connector.Image, testTunnelImageRef)
	}
	if connector.NetworkMode != "service:${ORIGIN_SERVICE_NAME}" {
		t.Fatalf("connector network_mode = %q, want shell variable placeholder", connector.NetworkMode)
	}
	if origin.User != ecsConnectorUser || connector.User != ecsConnectorUser {
		t.Fatalf("Compose users = origin %q connector %q, want 65532:65532", origin.User, connector.User)
	}
	if origin.Restart != "on-failure:5" || connector.Restart != "on-failure:5" {
		t.Fatalf("Compose restart policies = origin %q connector %q, want on-failure:5", origin.Restart, connector.Restart)
	}
	if origin.ReadOnly || connector.ReadOnly != true || connector.PidsLimit != connectorPIDsLimit {
		t.Fatalf("Compose rootfs/pids = origin read_only %v connector read_only %v pids %d", origin.ReadOnly, connector.ReadOnly, connector.PidsLimit)
	}
	if len(connector.Tmpfs) != 1 || connector.Tmpfs[0] != connectorTmpfsCompose {
		t.Fatalf("Compose connector tmpfs = %v, want [%s]", connector.Tmpfs, connectorTmpfsCompose)
	}
	for name, service := range map[string]struct {
		CapDrop     []string
		SecurityOpt []string
	}{
		"origin":    {CapDrop: origin.CapDrop, SecurityOpt: origin.SecurityOpt},
		"connector": {CapDrop: connector.CapDrop, SecurityOpt: connector.SecurityOpt},
	} {
		if !slices.Equal(service.CapDrop, []string{testCapabilityAll}) {
			t.Fatalf("%s cap_drop = %v, want [ALL]", name, service.CapDrop)
		}
		if !slices.Equal(service.SecurityOpt, []string{"no-new-privileges:true"}) {
			t.Fatalf("%s security_opt = %v, want [no-new-privileges:true]", name, service.SecurityOpt)
		}
	}
	if got := connector.Environment[ecsConnectorIDEnv]; got != testTunnelSlug {
		t.Fatalf("connector QURL_CONNECTOR_ID = %q, want %q", got, testTunnelSlug)
	}
	if got := connector.Environment[connectorAuditFileEnv]; got != connectorAuditFilePath {
		t.Fatalf("connector %s = %q, want %q", connectorAuditFileEnv, got, connectorAuditFilePath)
	}
	if _, ok := connector.Environment["LAYERV_KNOCK_RESOURCE_ID"]; ok {
		t.Fatal("Compose connector rendered the advanced knock-resource override")
	}
	for _, name := range []string{"QURL_API_URL"} {
		if got := connector.Environment[name]; got != "${QURL_API_URL_YAML}" {
			t.Fatalf("connector %s = %q, want shell variable placeholder", name, got)
		}
	}
	if _, ok := connector.Environment["QURL_BOOTSTRAP_URL"]; ok {
		t.Fatal("Compose connector rendered retired bootstrap URL")
	}
	if !strings.Contains(got, "ORIGIN_SERVICE_NAME='qurl-s3-origin-"+testTunnelSlug+"'") {
		t.Fatalf("Compose instructions missing shell-quoted origin service assignment:\n%s", got)
	}
	quotedAPIURL, err := yamlSingleQuoted(testTunnelAPIURL)
	if err != nil {
		t.Fatalf("yamlSingleQuoted: %v", err)
	}
	if !strings.Contains(got, "QURL_API_URL_YAML="+shellSingleQuote(quotedAPIURL)) {
		t.Fatalf("Compose instructions missing shell-quoted API URL assignment:\n%s", got)
	}
	if !strings.Contains(got, "After a Docker daemon restart, verify both services are running") {
		t.Fatalf("Compose instructions missing daemon-restart recovery note:\n%s", got)
	}
	if !strings.Contains(got, "Docker auto-restarts the S3 origin service after a crash") {
		t.Fatalf("Compose instructions missing origin auto-restart recovery note:\n%s", got)
	}
	assertNoS3SecretLeaks(t, got)
}

func TestRenderDockerComposeS3WebsiteInstructionsShellQuotesAPIURL(t *testing.T) {
	args := *testS3WebsiteArgs(tunnelEnvCompose)
	args.APIURL = testShellSignificantTunnelAPIURL
	quotedYAML, err := yamlSingleQuoted(args.APIURL)
	if err != nil {
		t.Fatalf("yamlSingleQuoted: %v", err)
	}
	got, err := renderDockerComposeS3WebsiteInstructions(&args, testTunnelImageRef, defaultS3StaticConnectorImage)
	if err != nil {
		t.Fatalf("renderDockerComposeS3WebsiteInstructions: %v", err)
	}
	if !strings.Contains(got, "QURL_API_URL_YAML="+shellSingleQuote(quotedYAML)) {
		t.Fatalf("Compose instructions did not shell-quote the YAML API URL scalar:\n%s", got)
	}
	if strings.Contains(got, "QURL_API_URL: "+quotedYAML) {
		t.Fatalf("Compose heredoc interpolated the API URL directly:\n%s", got)
	}
}

func TestRenderDockerComposeS3WebsiteInstructionsRejectsShellKnockResourceID(t *testing.T) {
	args := *testS3WebsiteArgs(tunnelEnvCompose)
	args.KnockResourceID = "qurl$(touch /tmp/pwned)"

	_, err := renderDockerComposeS3WebsiteInstructions(&args, testTunnelImageRef, defaultS3StaticConnectorImage)

	if err == nil || !strings.Contains(err.Error(), "knock_resource_id") {
		t.Fatalf("render err = %v, want knock_resource_id rejection", err)
	}
}

func TestRenderS3WebsiteECSContainerJSONUsesBootstrapIdentity(t *testing.T) {
	instructions, err := renderECSS3WebsiteInstructions(testS3WebsiteArgs(tunnelEnvECSFargate), testTunnelImageRef, defaultS3StaticConnectorImage)
	if err != nil {
		t.Fatalf("renderECSS3WebsiteInstructions: %v", err)
	}
	if !strings.Contains(instructions, "Do not share qurl-agent-state across concurrently running sidecars") {
		t.Fatalf("ECS instructions missing qurl-agent-state sharing warning:\n%s", instructions)
	}
	if !strings.Contains(instructions, "qurl-audit") || !strings.Contains(instructions, "read-only root filesystem") {
		t.Fatalf("ECS instructions missing durable audit/read-only-root guidance:\n%s", instructions)
	}
	for _, want := range []string{"root-directory modes 0700, 0750, and 0755", "warm-start task revision", "Deleting it first prevents replacement tasks from starting"} {
		if !strings.Contains(instructions, want) {
			t.Fatalf("ECS instructions missing %q:\n%s", want, instructions)
		}
	}
	if !strings.Contains(instructions, "replace each `"+ecsLogRegionPlaceholder+"` with the ECS task region") {
		t.Fatalf("ECS instructions missing awslogs task-region placeholder guidance:\n%s", instructions)
	}
	if !strings.Contains(instructions, "may log local connection errors until the origin is listening") {
		t.Fatalf("ECS instructions missing origin-readiness guidance:\n%s", instructions)
	}
	if !strings.Contains(instructions, "Both containers are essential, so a failure of either one restarts the whole task") {
		t.Fatalf("ECS instructions missing essential-container restart coupling:\n%s", instructions)
	}
	if !strings.Contains(instructions, "Create the CloudWatch Logs group `/ecs/qurl-s3-website` in the ECS task region") {
		t.Fatalf("ECS instructions missing CloudWatch log group setup note:\n%s", instructions)
	}
	if !strings.Contains(instructions, "resource_id: '"+testTunnelResourceID+"'") {
		t.Fatalf("ECS instructions missing pinned resource_id:\n%s", instructions)
	}

	containerJSON, err := renderS3WebsiteECSContainerJSON(testS3WebsiteArgs(tunnelEnvECSFargate), testTunnelImageRef, defaultS3StaticConnectorImage)
	if err != nil {
		t.Fatalf("renderS3WebsiteECSContainerJSON: %v", err)
	}
	var containers []ecsContainerDefinition
	if err := json.Unmarshal([]byte(containerJSON), &containers); err != nil {
		t.Fatalf("ECS container JSON did not parse: %v\n%s", err, containerJSON)
	}
	if len(containers) != 2 {
		t.Fatalf("containers = %+v, want origin + qurl connector", containers)
	}
	origin, connector := containers[0], containers[1]
	if origin.Name != testS3OriginContainer || origin.Image != defaultS3StaticConnectorImage {
		t.Fatalf("origin container = %+v", origin)
	}
	originEnv := ecsEnvMap(origin.Environment)
	for name, want := range map[string]string{
		testS3EnvBucket:         testS3WebsiteBucket,
		testS3EnvRegion:         testS3WebsiteRegion,
		testS3EnvPrefix:         testS3WebsitePrefix,
		testS3EnvIndex:          testS3WebsiteIndex,
		testS3EnvCacheConnector: testTunnelSlug,
	} {
		if got := originEnv[name]; got != want {
			t.Fatalf("origin env %s = %q, want %q", name, got, want)
		}
		assertNoShellMetacharacter(t, "ECS origin env "+name, originEnv[name])
	}
	if got := origin.LogConfiguration.Options[ecsLogRegionOption]; got != ecsLogRegionPlaceholder {
		t.Fatalf("origin awslogs-region = %q, want task-region placeholder", got)
	}
	connectorEnv := ecsEnvMap(connector.Environment)
	if connector.Name != connectorContainerName || connector.Image != testTunnelImageRef {
		t.Fatalf("connector container = %+v", connector)
	}
	if got := connectorEnv[ecsConnectorIDEnv]; got != testTunnelSlug {
		t.Fatalf("connector %s = %q, want %q", ecsConnectorIDEnv, got, testTunnelSlug)
	}
	if got := connector.LogConfiguration.Options[ecsLogRegionOption]; got != ecsLogRegionPlaceholder {
		t.Fatalf("connector awslogs-region = %q, want task-region placeholder", got)
	}
	if len(connector.DependsOn) != 1 ||
		connector.DependsOn[0].ContainerName != s3WebsiteOriginContainerName ||
		connector.DependsOn[0].Condition != "START" {
		t.Fatalf("connector dependsOn = %+v, want START dependency on %s", connector.DependsOn, s3WebsiteOriginContainerName)
	}
	if _, ok := connectorEnv["LAYERV_KNOCK_RESOURCE_ID"]; ok {
		t.Fatal("ECS connector rendered the advanced knock-resource override")
	}
	for _, name := range []string{"QURL_API_URL"} {
		if got := connectorEnv[name]; got != testTunnelAPIURL {
			t.Fatalf("connector %s = %q, want %q", name, got, testTunnelAPIURL)
		}
	}
	if _, ok := connectorEnv["QURL_BOOTSTRAP_URL"]; ok {
		t.Fatal("ECS connector rendered retired bootstrap URL")
	}
	if origin.User != ecsConnectorUser || connector.User != ecsConnectorUser {
		t.Fatalf("ECS users = origin %q connector %q, want 65532:65532", origin.User, connector.User)
	}
	if origin.ReadonlyRootFilesystem || !connector.ReadonlyRootFilesystem {
		t.Fatalf("ECS readonlyRootFilesystem = origin %v connector %v, want false/true", origin.ReadonlyRootFilesystem, connector.ReadonlyRootFilesystem)
	}
	if got := connectorEnv[connectorAuditFileEnv]; got != connectorAuditFilePath {
		t.Fatalf("connector %s = %q, want %q", connectorAuditFileEnv, got, connectorAuditFilePath)
	}
	if !ecsMountPointPresent(connector.MountPoints, "qurl-audit", connectorAuditDir, false) {
		t.Fatalf("connector mountPoints = %+v, want writable qurl-audit mount", connector.MountPoints)
	}
	for _, container := range []ecsContainerDefinition{origin, connector} {
		if got := container.LinuxParameters.Capabilities.Drop; len(got) != 1 || got[0] != testCapabilityAll {
			t.Fatalf("ECS container %s capability drop = %v, want [ALL]", container.Name, got)
		}
	}
	assertNoS3SecretLeaks(t, containerJSON)
}

func TestRenderKubernetesS3WebsiteInstructionsYAMLAndBootstrapIdentity(t *testing.T) {
	got, err := renderKubernetesS3WebsiteInstructions(testS3WebsiteArgs(tunnelEnvKubernetes), testTunnelImageRef, defaultS3StaticConnectorImage)
	if err != nil {
		t.Fatalf("renderKubernetesS3WebsiteInstructions: %v", err)
	}
	objects := extractS3TestBlock(t, got, "kubectl apply -f - <<'QURL_K8S_YAML_EOF'\n", "\nQURL_K8S_YAML_EOF")
	docs := strings.Split(objects, "\n---\n")
	if len(docs) != 3 {
		t.Fatalf("Kubernetes bootstrap docs = %d, want ConfigMap + state PVC + audit PVC:\n%s", len(docs), objects)
	}
	var configMap struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal([]byte(docs[0]), &configMap); err != nil {
		t.Fatalf("ConfigMap YAML did not parse: %v\n%s", err, docs[0])
	}
	configYAML, err := renderS3WebsiteConnectorConfigYAML(testS3WebsiteArgs(tunnelEnvKubernetes))
	if err != nil {
		t.Fatalf("renderS3WebsiteConnectorConfigYAML: %v", err)
	}
	if gotConfig := configMap.Data["qurl-proxy.yaml"]; gotConfig != configYAML {
		t.Fatalf("ConfigMap qurl-proxy.yaml = %q, want %q", gotConfig, configYAML)
	}
	var pvc map[string]any
	if err := yaml.Unmarshal([]byte(docs[1]), &pvc); err != nil {
		t.Fatalf("PVC YAML did not parse: %v\n%s", err, docs[1])
	}

	patchStart := strings.Index(got, "Pod spec additions:")
	if patchStart < 0 {
		t.Fatalf("Kubernetes instructions missing pod spec section:\n%s", got)
	}
	patch := extractS3TestBlock(t, got[patchStart:], "```\n", "\n```")
	var podSpec struct {
		SecurityContext map[string]any `yaml:"securityContext"`
		InitContainers  []struct {
			Name  string `yaml:"name"`
			Image string `yaml:"image"`
		} `yaml:"initContainers"`
		Containers []struct {
			Name            string              `yaml:"name"`
			Image           string              `yaml:"image"`
			SecurityContext map[string]any      `yaml:"securityContext"`
			Env             []ecsEnvironmentVar `yaml:"env"`
			VolumeMounts    []struct {
				Name      string `yaml:"name"`
				MountPath string `yaml:"mountPath"`
				ReadOnly  bool   `yaml:"readOnly"`
			} `yaml:"volumeMounts"`
		} `yaml:"containers"`
		Volumes []map[string]any `yaml:"volumes"`
	}
	if err := yaml.Unmarshal([]byte(patch), &podSpec); err != nil {
		t.Fatalf("Pod spec fragment YAML did not parse: %v\n%s", err, patch)
	}
	if len(podSpec.SecurityContext) != 0 || len(podSpec.InitContainers) != 2 || len(podSpec.Containers) != 2 || len(podSpec.Volumes) != 6 {
		t.Fatalf("pod spec = %+v, want permissions/copy init containers, two runtime containers, and six volumes without pod fsGroup", podSpec)
	}
	if podSpec.InitContainers[0].Image != connectorVolumePermissionsImage {
		t.Fatalf("permissions image = %q, want %q", podSpec.InitContainers[0].Image, connectorVolumePermissionsImage)
	}
	origin, connector := podSpec.Containers[0], podSpec.Containers[1]
	if origin.Name != testS3OriginContainer || origin.Image != defaultS3StaticConnectorImage {
		t.Fatalf("origin pod container = %+v", origin)
	}
	if origin.SecurityContext["runAsNonRoot"] != true || origin.SecurityContext["allowPrivilegeEscalation"] != false {
		t.Fatalf("origin securityContext = %+v, want non-root/no-privilege-escalation", origin.SecurityContext)
	}
	originEnv := ecsEnvMap(origin.Env)
	for name, want := range map[string]string{
		testS3EnvBucket:         testS3WebsiteBucket,
		testS3EnvRegion:         testS3WebsiteRegion,
		testS3EnvPrefix:         testS3WebsitePrefix,
		testS3EnvIndex:          testS3WebsiteIndex,
		testS3EnvCacheConnector: testTunnelSlug,
	} {
		if got := originEnv[name]; got != want {
			t.Fatalf("origin env %s = %q, want %q", name, got, want)
		}
		assertNoShellMetacharacter(t, "Kubernetes origin env "+name, originEnv[name])
	}
	connectorEnv := ecsEnvMap(connector.Env)
	if connector.Name != connectorContainerName || connector.Image != testTunnelImageRef {
		t.Fatalf("connector pod container = %+v", connector)
	}
	if connector.SecurityContext["runAsNonRoot"] != true || connector.SecurityContext["allowPrivilegeEscalation"] != false {
		t.Fatalf("connector securityContext = %+v, want non-root/no-privilege-escalation", connector.SecurityContext)
	}
	if connector.SecurityContext["readOnlyRootFilesystem"] != true {
		t.Fatalf("connector securityContext = %+v, want readOnlyRootFilesystem", connector.SecurityContext)
	}
	if got := connectorEnv[ecsConnectorIDEnv]; got != testTunnelSlug {
		t.Fatalf("connector %s = %q, want %q", ecsConnectorIDEnv, got, testTunnelSlug)
	}
	if got := connectorEnv[connectorAuditFileEnv]; got != connectorAuditFilePath {
		t.Fatalf("connector %s = %q, want %q", connectorAuditFileEnv, got, connectorAuditFilePath)
	}
	if !strings.Contains(got, "qurl-go rejects group-writable identity state") || strings.Contains(got, "fsGroup:") {
		t.Fatalf("Kubernetes instructions did not replace pod fsGroup with exact mode preparation:\n%s", got)
	}
	for _, want := range []string{"qurl-bootstrap-copy", "warm-start workload revision", "deleting it first prevents a replacement pod from starting"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Kubernetes instructions missing %q:\n%s", want, got)
		}
	}
	for _, name := range []string{"QURL_API_URL"} {
		if got := connectorEnv[name]; got != testTunnelAPIURL {
			t.Fatalf("connector %s = %q, want %q", name, got, testTunnelAPIURL)
		}
	}
	if _, ok := connectorEnv["QURL_BOOTSTRAP_URL"]; ok {
		t.Fatal("Kubernetes connector rendered retired bootstrap URL")
	}
	if _, ok := connectorEnv["LAYERV_KNOCK_RESOURCE_ID"]; ok {
		t.Fatal("Kubernetes connector rendered the advanced knock-resource override")
	}
	assertNoS3SecretLeaks(t, got)
}

func connectorSetupViewSubmissionBody(t *testing.T, meta *TunnelInstallModalMetadata, setupType string) string {
	t.Helper()
	pm, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal private_metadata: %v", err)
	}
	return viewSubmissionBody(t, "V_test_connector_setup", callbackIDConnectorSetup, string(pm), meta.TeamID, meta.UserID,
		map[string]map[string]interactionStateValue{
			connectorSetupBlockType: {
				connectorSetupActionType: {SelectedOption: &interactionSelectedOption{Value: setupType}},
			},
		})
}

func s3WebsiteInstallViewSubmissionBody(t *testing.T, meta *TunnelInstallModalMetadata, values map[string]map[string]interactionStateValue) string {
	t.Helper()
	pm, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal private_metadata: %v", err)
	}
	return viewSubmissionBody(t, "V_test_s3_website", callbackIDS3WebsiteInstall, string(pm), meta.TeamID, meta.UserID, values)
}

func s3WebsiteInstallModalValues(slug, shortcut, env, bucket, region, prefix, index string) map[string]map[string]interactionStateValue {
	return map[string]map[string]interactionStateValue{
		s3WebsiteInstallBlockSlug: {
			s3WebsiteInstallActionSlug: {Value: slug},
		},
		s3WebsiteInstallBlockShortcut: {
			s3WebsiteInstallActionShortcut: {Value: shortcut},
		},
		s3WebsiteInstallBlockEnvironment: {
			s3WebsiteInstallActionEnvironment: {SelectedOption: &interactionSelectedOption{Value: env}},
		},
		s3WebsiteInstallBlockBucket: {
			s3WebsiteInstallActionBucket: {Value: bucket},
		},
		s3WebsiteInstallBlockRegion: {
			s3WebsiteInstallActionRegion: {Value: region},
		},
		s3WebsiteInstallBlockPrefix: {
			s3WebsiteInstallActionPrefix: {Value: prefix},
		},
		s3WebsiteInstallBlockIndex: {
			s3WebsiteInstallActionIndex: {Value: index},
		},
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func testS3WebsiteArgs(env tunnelInstallEnvironment) *s3WebsiteInstallArgs {
	return &s3WebsiteInstallArgs{
		Slug:               testTunnelSlug,
		Alias:              "team-dash",
		Environment:        env,
		Bucket:             testS3WebsiteBucket,
		Region:             testS3WebsiteRegion,
		Prefix:             testS3WebsitePrefix,
		IndexDocument:      testS3WebsiteIndex,
		ResourceID:         testTunnelResourceID,
		ConnectorRoutingID: testTunnelRoutingID,
		KnockResourceID:    testS3WebsiteKnockResource,
		APIURL:             testTunnelAPIURL,
	}
}

func testS3WebsiteInstallRequest(responseURL string, now time.Time, env tunnelInstallEnvironment) *s3WebsiteInstallRequest {
	return &s3WebsiteInstallRequest{
		teamID:       testAdminTeamID,
		enterpriseID: "",
		channelID:    testTunnelChannelID,
		userID:       testAdminUserID,
		responseURL:  responseURL,
		args:         testS3WebsiteArgs(env),
		attemptID:    tunnelBootstrapTimeAttemptID("test-attempt", now),
	}
}

func extractS3TestBlock(t *testing.T, got, start, end string) string {
	t.Helper()
	bodyStart := strings.Index(got, start)
	if bodyStart < 0 {
		t.Fatalf("missing block start %q:\n%s", start, got)
	}
	bodyStart += len(start)
	bodyEnd := strings.Index(got[bodyStart:], end)
	if bodyEnd < 0 {
		t.Fatalf("missing block end %q:\n%s", end, got)
	}
	return got[bodyStart : bodyStart+bodyEnd]
}

func ecsEnvMap(vars []ecsEnvironmentVar) map[string]string {
	env := map[string]string{}
	for _, item := range vars {
		env[item.Name] = item.Value
	}
	return env
}

func assertNoS3SecretLeaks(t *testing.T, got string) {
	t.Helper()
	for _, forbidden := range []string{
		testForbiddenSlackShellFence,
		testForbiddenSlackYAMLFence,
		testTunnelModalKey,
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("S3 website instructions leaked %q:\n%s", forbidden, got)
		}
	}
}

func assertNoShellMetacharacter(t *testing.T, name, value string) {
	t.Helper()
	if strings.ContainsAny(value, "$`\\ \t\n\r") {
		t.Fatalf("%s contains shell metacharacter: %q", name, value)
	}
}
