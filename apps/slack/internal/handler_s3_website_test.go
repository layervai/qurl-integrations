package internal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	testS3WebsiteBucket      = "stats-site-bucket"
	testS3WebsitePrefix      = "website"
	testS3WebsiteIndex       = "index.html"
	testS3WebsiteRegion      = "us-east-1"
	testS3WebsiteDisplayName = "Stats S3 website"
	testS3OriginContainer    = "s3-static-origin"
	testS3EnvBucket          = "S3_BUCKET"
	testS3EnvRegion          = "AWS_REGION"
	testS3EnvPrefix          = "S3_PREFIX"
	testS3EnvIndex           = "INDEX_DOCUMENT"
	testS3EnvCacheConnector  = "CACHE_CONNECTOR_ID"
)

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
			meta := TunnelInstallModalMetadata{
				TeamID:        testAdminTeamID,
				ChannelID:     testTunnelChannelID,
				UserID:        testAdminUserID,
				ResponseURL:   testSlackResponseURL,
				CreatedAtUnix: fixedNow.Unix(),
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
			if !strings.Contains(w.Body.String(), tc.wantText) {
				t.Fatalf("updated view missing %q: %s", tc.wantText, w.Body.String())
			}
		})
	}
}

func TestS3WebsiteInstallModalSubmissionLetsBootstrapBindResource(t *testing.T) {
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
			testKeyResourceID:  testTunnelResourceID,
			testKeyType:        client.ResourceTypeTunnel,
			testKeySlug:        testTunnelSlug,
			testKeyStatus:      client.StatusActive,
			testKeyDescription: testS3WebsiteDisplayName,
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
	if !strings.Contains(asString(resourceBody[testKeyDescription]), testS3WebsiteBucket) {
		t.Errorf("resource description = %v, want S3 website context", resourceBody[testKeyDescription])
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
		"first agent bootstrap response seeds the qURL resource identity",
		"qURL Connector image: `" + testTunnelImageRef + "`",
		"S3 origin image: `" + defaultS3StaticConnectorImage + "`",
		"S3_BUCKET='" + testS3WebsiteBucket + "'",
		"AWS_REGION='" + testS3WebsiteRegion + "'",
		"S3_PREFIX='" + testS3WebsitePrefix + "'",
		"INDEX_DOCUMENT='" + testS3WebsiteIndex + "'",
		`--network "container:${ORIGIN_CONTAINER}"`,
		"/qurl get $team-dash",
	} {
		if !strings.Contains(async, want) {
			t.Errorf("async reply missing %q:\n%s", want, async)
		}
	}
	for _, forbidden := range []string{testTunnelModalKey, testForbiddenSlackShellFence, testForbiddenSlackYAMLFence, "find_or_create", "YOUR_WEB_CONTAINER_NAME", testTunnelResourceID, testForbiddenKnockResourceEnv, testKeyResourceID, testKeyKnockResourceID} {
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

func TestS3WebsiteInstallRejectsMissingResourceIDBeforeMintingBootstrapKey(t *testing.T) {
	now := fixedNow
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	var apiKeyHits int
	ts.addCustomer(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
		respondQURLEnvelope(t, w, map[string]any{
			testKeyType:   client.ResourceTypeTunnel,
			testKeySlug:   testTunnelSlug,
			testKeyStatus: client.StatusActive,
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
	if !strings.Contains(async, "No bootstrap key was minted") || !strings.Contains(async, fAttrResourceID) {
		t.Fatalf("async reply = %q, want missing resource_id error before key mint", async)
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
	if !strings.Contains(w.Body.String(), "admin-only") {
		t.Fatalf("modal response = %s, want admin-only rejection", w.Body.String())
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

func TestRenderS3WebsiteConnectorConfigYAMLDoesNotPinResourceIdentity(t *testing.T) {
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
	} {
		if !strings.Contains(configYAML, want) {
			t.Fatalf("config missing %q:\n%s", want, configYAML)
		}
	}
	for _, forbidden := range []string{fAttrResourceID, testKeyKnockResourceID, testForbiddenKnockResourceEnv} {
		if strings.Contains(configYAML, forbidden) {
			t.Fatalf("config leaked %q:\n%s", forbidden, configYAML)
		}
	}
}

func TestRenderDockerComposeS3WebsiteInstructionsEmitsParseableCompose(t *testing.T) {
	got, err := renderDockerComposeS3WebsiteInstructions(testS3WebsiteArgs(tunnelEnvCompose), testTunnelImageRef, defaultS3StaticConnectorImage)
	if err != nil {
		t.Fatalf("renderDockerComposeS3WebsiteInstructions: %v", err)
	}
	body := extractS3TestBlock(t, got, "cat > \"$QURL_COMPOSE_FILE\" <<QURL_COMPOSE_YAML_EOF\n", "\nQURL_COMPOSE_YAML_EOF")

	var parsed struct {
		Services map[string]struct {
			Image       string            `yaml:"image"`
			Environment map[string]string `yaml:"environment"`
			NetworkMode string            `yaml:"network_mode"`
			Volumes     []string          `yaml:"volumes"`
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
	if connector.NetworkMode != "service:qurl-s3-origin-"+testTunnelSlug {
		t.Fatalf("connector network_mode = %q, want origin service network", connector.NetworkMode)
	}
	if got := connector.Environment[ecsConnectorIDEnv]; got != testTunnelSlug {
		t.Fatalf("connector QURL_CONNECTOR_ID = %q, want %q", got, testTunnelSlug)
	}
	assertNoPinnedS3Identity(t, got)
}

func TestRenderS3WebsiteECSContainerJSONUsesBootstrapIdentity(t *testing.T) {
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
	}
	connectorEnv := ecsEnvMap(connector.Environment)
	if connector.Name != ecsConnectorContainerName || connector.Image != testTunnelImageRef {
		t.Fatalf("connector container = %+v", connector)
	}
	if got := connectorEnv[ecsConnectorIDEnv]; got != testTunnelSlug {
		t.Fatalf("connector %s = %q, want %q", ecsConnectorIDEnv, got, testTunnelSlug)
	}
	if _, ok := connectorEnv[testForbiddenKnockResourceEnv]; ok {
		t.Fatalf("connector env should not include %s: %+v", testForbiddenKnockResourceEnv, connectorEnv)
	}
	assertNoPinnedS3Identity(t, containerJSON)
}

func TestRenderKubernetesS3WebsiteInstructionsYAMLAndBootstrapIdentity(t *testing.T) {
	got, err := renderKubernetesS3WebsiteInstructions(testS3WebsiteArgs(tunnelEnvKubernetes), testTunnelImageRef, defaultS3StaticConnectorImage)
	if err != nil {
		t.Fatalf("renderKubernetesS3WebsiteInstructions: %v", err)
	}
	objects := extractS3TestBlock(t, got, "kubectl apply -f - <<'QURL_K8S_YAML_EOF'\n", "\nQURL_K8S_YAML_EOF")
	docs := strings.Split(objects, "\n---\n")
	if len(docs) != 2 {
		t.Fatalf("Kubernetes bootstrap docs = %d, want ConfigMap + PVC:\n%s", len(docs), objects)
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
		Containers      []struct {
			Name         string              `yaml:"name"`
			Image        string              `yaml:"image"`
			Env          []ecsEnvironmentVar `yaml:"env"`
			VolumeMounts []struct {
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
	if podSpec.SecurityContext["fsGroup"] == nil || len(podSpec.Containers) != 2 || len(podSpec.Volumes) != 3 {
		t.Fatalf("pod spec = %+v, want fsGroup, two containers, and three volumes", podSpec)
	}
	origin, connector := podSpec.Containers[0], podSpec.Containers[1]
	if origin.Name != testS3OriginContainer || origin.Image != defaultS3StaticConnectorImage {
		t.Fatalf("origin pod container = %+v", origin)
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
	}
	connectorEnv := ecsEnvMap(connector.Env)
	if connector.Name != ecsConnectorContainerName || connector.Image != testTunnelImageRef {
		t.Fatalf("connector pod container = %+v", connector)
	}
	if got := connectorEnv[ecsConnectorIDEnv]; got != testTunnelSlug {
		t.Fatalf("connector %s = %q, want %q", ecsConnectorIDEnv, got, testTunnelSlug)
	}
	assertNoPinnedS3Identity(t, got)
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
		Slug:          testTunnelSlug,
		Alias:         "team-dash",
		Environment:   env,
		Bucket:        testS3WebsiteBucket,
		Region:        testS3WebsiteRegion,
		Prefix:        testS3WebsitePrefix,
		IndexDocument: testS3WebsiteIndex,
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

func assertNoPinnedS3Identity(t *testing.T, got string) {
	t.Helper()
	for _, forbidden := range []string{
		fAttrResourceID,
		testKeyKnockResourceID,
		testForbiddenKnockResourceEnv,
		testForbiddenSlackShellFence,
		testForbiddenSlackYAMLFence,
		testTunnelModalKey,
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("S3 website instructions leaked %q:\n%s", forbidden, got)
		}
	}
}
