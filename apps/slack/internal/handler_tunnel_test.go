package internal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	testTunnelSlug        = "prod-dashboard"
	testTunnelResourceID  = "r_prod_dash01"
	testTunnelInstallVerb = "install"
	testTunnelWizardCmd   = "tunnel " + testTunnelInstallVerb
	testTunnelInstallCmd  = testTunnelWizardCmd + " " + testTunnelSlug
	testTunnelChannelID   = "C_test"
	testTunnelImageRef    = "ghcr.io/layervai/qurl-reverse-tunnel-client:v-test"
	testTunnelAPIKey      = "lv_live_test_bootstrap"
	testTunnelAPIKeyID    = "key_tunnel_bootstrap"
	testSlackResponseURL  = "https://hooks.slack.test/response"
	testTunnelDockerLine  = `TUNNEL_CONTAINER="qurl-tunnel-${QURL_TUNNEL_SLUG}"`
	testTunnelModalKey    = "lv_live_modal_bootstrap"
)

const (
	testForbiddenResourceLabel   = "Resource:"
	testForbiddenSlackYAMLFence  = "```yaml"
	testForbiddenSlackShellFence = "```sh"
	testTunnelAgentDirFragment   = `/var/lib/layerv/qurl-tunnel/${QURL_TUNNEL_SLUG}/agent`
	testTunnelLocalPort9090Line  = "local_port: 9090"
	testTunnelKeyHistoryNote     = "prompts for the bootstrap key"
	testTunnelKeyPromptLine      = "Paste qURL bootstrap key (input hidden)"
	testTunnelKeyInstallLine     = `printf '%s' "$QURL_BOOTSTRAP_KEY"`
)

func freezeTunnelBootstrapNow(t *testing.T, now time.Time) {
	t.Helper()
	previous := tunnelBootstrapNow
	tunnelBootstrapNow = func() time.Time { return now }
	t.Cleanup(func() { tunnelBootstrapNow = previous })
}

func TestParseTunnelInstall(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		wantErr    bool
		wantSlug   string
		wantAlias  string
		wantPort   int
		wantEnv    tunnelInstallEnvironment
		wantWebRef string
	}{
		{name: "minimal", text: testTunnelInstallCmd, wantSlug: testTunnelSlug, wantAlias: testTunnelSlug, wantPort: defaultTunnelLocalPort},
		{name: "slug with alias sigil", text: "tunnel install $" + testTunnelSlug, wantSlug: testTunnelSlug, wantAlias: testTunnelSlug, wantPort: defaultTunnelLocalPort},
		{name: "port and alias", text: testTunnelInstallCmd + " port:9090 alias:$dash", wantSlug: testTunnelSlug, wantAlias: "dash", wantPort: 9090},
		{name: "alias without sigil", text: testTunnelInstallCmd + " alias:dash", wantSlug: testTunnelSlug, wantAlias: "dash", wantPort: defaultTunnelLocalPort},
		{name: "docker environment", text: testTunnelInstallCmd + " env:docker", wantSlug: testTunnelSlug, wantAlias: testTunnelSlug, wantPort: defaultTunnelLocalPort, wantEnv: tunnelEnvDocker},
		{name: "environment", text: testTunnelInstallCmd + " env:ecs-fargate", wantSlug: testTunnelSlug, wantAlias: testTunnelSlug, wantPort: defaultTunnelLocalPort, wantEnv: tunnelEnvECSFargate},
		{name: "compose alias and service", text: testTunnelInstallCmd + " env:compose service:web_1", wantSlug: testTunnelSlug, wantAlias: testTunnelSlug, wantPort: defaultTunnelLocalPort, wantEnv: tunnelEnvCompose, wantWebRef: "web_1"},
		{name: "container ref", text: testTunnelInstallCmd + " container:web_1-2", wantSlug: testTunnelSlug, wantAlias: testTunnelSlug, wantPort: defaultTunnelLocalPort, wantWebRef: "web_1-2"},
		{name: "web container ref", text: testTunnelInstallCmd + " web_container:web", wantSlug: testTunnelSlug, wantAlias: testTunnelSlug, wantPort: defaultTunnelLocalPort, wantWebRef: "web"},
		{name: "bad slug uppercase", text: "tunnel install Prod", wantErr: true},
		{name: "empty slug after sigil", text: "tunnel install $", wantErr: true},
		{name: "double sigil slug", text: "tunnel install $$prod", wantErr: true},
		{name: "verb boundary", text: "tunnelhats install prod", wantErr: true},
		{name: "bad port", text: testTunnelInstallCmd + " port:70000", wantErr: true},
		{name: "old docker vm spelling rejected", text: testTunnelInstallCmd + " env:docker-vm", wantErr: true},
		{name: "bad environment", text: testTunnelInstallCmd + " env:prod", wantErr: true},
		{name: "bad container ref", text: testTunnelInstallCmd + " container:../web", wantErr: true},
		{name: "compose rejects dotted service", text: testTunnelInstallCmd + " service:web.1 env:compose", wantErr: true},
		{name: "compose rejects dotted container ref", text: testTunnelInstallCmd + " env:compose container:web.1", wantErr: true},
		{name: "unknown option", text: testTunnelInstallCmd + " mode:fast", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := parseTunnelInstall(tc.text)
			if tc.wantErr {
				if msg == "" {
					t.Fatalf("expected rejection, got %+v", got)
				}
				return
			}
			if msg != "" {
				t.Fatalf("unexpected rejection: %s", msg)
			}
			if got.Slug != tc.wantSlug || got.Alias != tc.wantAlias || got.LocalPort != tc.wantPort {
				t.Errorf("got %+v, want slug=%q alias=%q port=%d", got, tc.wantSlug, tc.wantAlias, tc.wantPort)
			}
			wantEnv := tc.wantEnv
			if wantEnv == "" {
				wantEnv = tunnelEnvDocker
			}
			if got.Environment != wantEnv {
				t.Errorf("environment = %q, want %q", got.Environment, wantEnv)
			}
			if got.WebRef != tc.wantWebRef {
				t.Errorf("web container = %q, want %q", got.WebRef, tc.wantWebRef)
			}
		})
	}
}

func TestTunnelInstallWizardRequest(t *testing.T) {
	t.Parallel()
	cases := []struct {
		text string
		want bool
	}{
		{text: testTunnelWizardCmd, want: true},
		{text: " " + testTunnelWizardCmd + " ", want: true},
		{text: testTunnelInstallVerb, want: false},
		{text: testTunnelInstallCmd, want: false},
		{text: "tunnel", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			t.Parallel()
			if got := tunnelInstallWizardRequest(tc.text); got != tc.want {
				t.Fatalf("tunnelInstallWizardRequest(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestTunnelInstallCreatesResourceBindsAliasAndMintsBootstrapKey(t *testing.T) {
	now := time.Date(2026, 5, 27, 4, 30, 0, 0, time.UTC)
	freezeTunnelBootstrapNow(t, now)

	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	var resourceBody map[string]any
	var apiKeyBody map[string]any
	var idempotencyKey string
	ts.addCustomer(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&resourceBody); err != nil {
			t.Fatalf("decode resource body: %v", err)
		}
		respondQURLEnvelope(t, w, map[string]any{
			testKeyResourceID:   testTunnelResourceID,
			testKeyType:         client.ResourceTypeTunnel,
			testKeySlug:         testTunnelSlug,
			testKeyStatus:       client.StatusActive,
			"knock_resource_id": "qurl-tunnel-server",
		})
	})
	ts.addCustomer(http.MethodPost, "/v1/api-keys", func(w http.ResponseWriter, r *http.Request) {
		idempotencyKey = r.Header.Get(client.HeaderIdempotencyKey)
		if err := json.NewDecoder(r.Body).Decode(&apiKeyBody); err != nil {
			t.Fatalf("decode api key body: %v", err)
		}
		respondQURLEnvelope(t, w, map[string]any{
			testKeyKeyID:      testTunnelAPIKeyID,
			testKeyAPIKey:     testTunnelAPIKey,
			"name":            "Slack tunnel bootstrap " + testTunnelSlug,
			"scopes":          []string{tunnelScopeAgent, tunnelScopeWrite},
			testKeyStatus:     client.StatusActive,
			testKeyPurpose:    client.APIKeyPurposeTunnelBootstrap,
			testKeyTunnelSlug: testTunnelSlug,
			testKeyExpiresAt:  now.Add(time.Hour).Format(time.RFC3339),
		})
	})

	h := newAdminTestHandler(t, ts)
	h.cfg.TunnelImage = testTunnelImageRef
	h.SetAliasStore(h.cfg.AdminStore)
	inv := newAdminSlashInvoker(t, h)
	status, ack := inv.invokeAdmin(testTunnelInstallCmd+" port:9090", testAdminTeamID, testAdminUserID)
	var asyncEnvelope map[string]string
	if err := json.Unmarshal(inv.captured.waitForBody(t, 2*time.Second), &asyncEnvelope); err != nil {
		t.Fatalf("unmarshal tunnel install response_url body: %v", err)
	}
	async := asyncEnvelope[respFieldText]

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(ack, "Working") {
		t.Fatalf("ack = %q, want async working copy", ack)
	}
	if asyncEnvelope[respFieldResponseType] != respTypeEphemeral {
		t.Fatalf("response_url response_type = %q, want ephemeral", asyncEnvelope[respFieldResponseType])
	}
	if resourceBody[testKeyType] != client.ResourceTypeTunnel || resourceBody[testKeySlug] != testTunnelSlug || resourceBody["find_or_create"] != true {
		t.Errorf("resource body = %+v, want tunnel find-or-create slug", resourceBody)
	}
	if apiKeyBody[testKeyPurpose] != client.APIKeyPurposeTunnelBootstrap || apiKeyBody[testKeyTunnelSlug] != testTunnelSlug || apiKeyBody["expires_in"] != tunnelBootstrapTTL {
		t.Errorf("api key body = %+v, want constrained tunnel bootstrap key", apiKeyBody)
	}
	if idempotencyKey == "" {
		t.Error("Idempotency-Key header was empty")
	}
	wantIdempotencyKey := tunnelBootstrapIdempotencyKey(testAdminTeamID, testTunnelChannelID, testAdminUserID, testTunnelSlug, now)
	if idempotencyKey != wantIdempotencyKey {
		t.Fatalf("Idempotency-Key = %q, want %q", idempotencyKey, wantIdempotencyKey)
	}
	for _, want := range []string{
		"Tunnel `" + testTunnelSlug + "` is ready to install.",
		"qURL shortcut `$" + testTunnelSlug + "` is ready in this channel.",
		"Paste it only when prompted or into your secret manager",
		"Run this whole block on the Linux Docker host",
		testTunnelKeyHistoryNote,
		"set -eu",
		testTunnelAPIKey,
		testTunnelKeyPromptLine,
		"cat > \"$CONFIG_FILE\" <<'QURL_PROXY_YAML_EOF'",
		"QURL_TUNNEL_SLUG='" + testTunnelSlug + "'",
		testTunnelKeyInstallLine,
		testTunnelLocalPort9090Line,
		"WEB_CONTAINER='YOUR_WEB_CONTAINER_NAME'",
		testTunnelDockerLine,
		`docker rm -f "$TUNNEL_CONTAINER"`,
		`--network "container:${WEB_CONTAINER}"`,
		testTunnelAgentDirFragment,
		testTunnelImageRef,
		"Bootstrap key expires in 1 hour.",
		"Treat this ephemeral Slack message as a secret",
		"Keep the qURL agent-state directory, volume, or PVC",
		"/qurl get $" + testTunnelSlug,
	} {
		if !strings.Contains(async, want) {
			t.Errorf("async reply missing %q:\n%s", want, async)
		}
	}
	for _, forbidden := range []string{testForbiddenResourceLabel, testTunnelResourceID, "expires at", "`qurl-proxy.yaml`", testForbiddenSlackYAMLFence, testForbiddenSlackShellFence, "connect.layerv", "proxy.layerv", "frps-", "<web-container>"} {
		if strings.Contains(async, forbidden) {
			t.Errorf("async reply leaked %q:\n%s", forbidden, async)
		}
	}
	gotRID, found, err := h.cfg.AdminStore.LookupChannelAlias(context.Background(), testAdminTeamID, testTunnelChannelID, testTunnelSlug)
	if err != nil {
		t.Fatalf("LookupChannelAlias: %v", err)
	}
	if !found || gotRID != testTunnelResourceID {
		t.Fatalf("alias lookup = (%q, %v), want (%s, true)", gotRID, found, testTunnelResourceID)
	}
}

func TestTunnelInstallBareOpensGuidedModal(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	type openViewCall struct {
		teamID    string
		triggerID string
		view      []byte
		deadline  time.Time
		hasDL     bool
	}
	calls := make(chan openViewCall, 1)
	h.cfg.OpenView = func(ctx context.Context, teamID, triggerID string, viewJSON []byte) error {
		deadline, ok := ctx.Deadline()
		calls <- openViewCall{
			teamID:    teamID,
			triggerID: triggerID,
			view:      append([]byte(nil), viewJSON...),
			deadline:  deadline,
			hasDL:     ok,
		}
		return nil
	}

	inv := newAdminSlashInvoker(t, h)
	status, ack := inv.invokeAdmin("tunnel install", testAdminTeamID, testAdminUserID)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(ack, "Opening guided tunnel setup") {
		t.Fatalf("ack = %q, want guided setup copy", ack)
	}
	var call openViewCall
	select {
	case call = <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("OpenView was not called")
	}
	if !call.hasDL {
		t.Fatal("OpenView context missing deadline")
	}
	if remaining := time.Until(call.deadline); remaining <= 0 || remaining > slackTriggerOpenViewBudget {
		t.Fatalf("OpenView deadline remaining = %s, want within %s", remaining, slackTriggerOpenViewBudget)
	}
	if call.triggerID != "trigger_test" {
		t.Fatalf("trigger_id = %q, want trigger_test", call.triggerID)
	}
	if call.teamID != testAdminTeamID {
		t.Fatalf("team_id = %q, want %s", call.teamID, testAdminTeamID)
	}
	var modal map[string]any
	if err := json.Unmarshal(call.view, &modal); err != nil {
		t.Fatalf("modal JSON: %v", err)
	}
	if modal[testFieldCallbackID] != callbackIDTunnelInstall {
		t.Fatalf("callback_id = %v, want %s", modal[testFieldCallbackID], callbackIDTunnelInstall)
	}
	pm, ok := modal[blockKitFieldPrivateMetadata].(string)
	if !ok || pm == "" {
		t.Fatalf("private_metadata = %T %q, want non-empty string", modal[blockKitFieldPrivateMetadata], modal[blockKitFieldPrivateMetadata])
	}
	var meta TunnelInstallModalMetadata
	if err := json.Unmarshal([]byte(pm), &meta); err != nil {
		t.Fatalf("private_metadata JSON: %v", err)
	}
	if meta.TeamID != testAdminTeamID || meta.ChannelID != testTunnelChannelID || meta.UserID != testAdminUserID || meta.ResponseURL == "" || meta.CreatedAtUnix == 0 {
		t.Fatalf("metadata = %+v, want team/channel/user/response_url", meta)
	}
	body := string(call.view)
	for _, want := range []string{"Target channel", testTunnelChannelID, "Tunnel slug", "Target environment", string(tunnelEnvCompose), string(tunnelEnvECSFargate), string(tunnelEnvKubernetes)} {
		if !strings.Contains(body, want) {
			t.Errorf("modal missing %q:\n%s", want, body)
		}
	}
	deleteBody := inv.captured.waitForBody(t, 2*time.Second)
	if got := parseSlackReplyBool(t, deleteBody, "delete_original"); !got {
		t.Fatalf("delete_original = %v, want true after successful modal open", got)
	}
}

func TestHelpListsGuidedAndTypedTunnelInstall(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error { return nil }

	status, got := newAdminSlashInvoker(t, h).invokeAdmin("help", testAdminTeamID, testAdminUserID)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	for _, want := range []string{"/qurl tunnel install`", "/qurl tunnel install <slug>`"} {
		if !strings.Contains(got, want) {
			t.Fatalf("/qurl help = %q, missing %q", got, want)
		}
	}
}

func TestTunnelInstallBareWithoutTriggerIDFallsBackToTypedInstall(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error {
		t.Fatal("OpenView should not be called without a trigger_id")
		return nil
	}
	values := url.Values{
		fieldText:      {"tunnel install"},
		fieldTeamID:    {testAdminTeamID},
		fieldUserID:    {testAdminUserID},
		fieldChannelID: {testTunnelChannelID},
	}
	w := httptest.NewRecorder()

	h.handleTunnel(w, values)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := parseSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "trigger_id") || !strings.Contains(got, "/qurl tunnel install <slug>") {
		t.Fatalf("response = %q, want trigger_id fallback guidance", got)
	}
}

func TestTunnelInstallBareReportsOpenViewFailure(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error {
		return errors.New("slack unavailable")
	}

	inv := newAdminSlashInvoker(t, h)
	status, ack := inv.invokeAdmin("tunnel install", testAdminTeamID, testAdminUserID)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(ack, "Opening guided tunnel setup") {
		t.Fatalf("ack = %q, want immediate guided setup copy", ack)
	}
	asyncBody := inv.captured.waitForBody(t, 2*time.Second)
	async := parseSlackText(t, asyncBody)
	if !strings.Contains(async, "Could not open guided tunnel setup") {
		t.Fatalf("async reply = %q, want OpenView failure copy", async)
	}
	if got := parseSlackReplyBool(t, asyncBody, "replace_original"); !got {
		t.Fatalf("replace_original = %v, want true for guided setup failure", got)
	}
}

func TestTunnelInstallBareReportsTriggerExpiry(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error {
		return ErrSlackTriggerExpired
	}

	inv := newAdminSlashInvoker(t, h)
	status, ack := inv.invokeAdmin("tunnel install", testAdminTeamID, testAdminUserID)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(ack, "Opening guided tunnel setup") {
		t.Fatalf("ack = %q, want immediate guided setup copy", ack)
	}
	async := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
	if !strings.Contains(async, "setup window expired") || !strings.Contains(async, "/qurl tunnel install") {
		t.Fatalf("async reply = %q, want trigger-expiry retry copy", async)
	}
}

func TestTunnelInstallBareReportsRateLimitRetryAfter(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error {
		return NewSlackRateLimitError("2")
	}

	inv := newAdminSlashInvoker(t, h)
	status, ack := inv.invokeAdmin("tunnel install", testAdminTeamID, testAdminUserID)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(ack, "Opening guided tunnel setup") {
		t.Fatalf("ack = %q, want immediate guided setup copy", ack)
	}
	async := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
	if !strings.Contains(async, "Wait 2 seconds") || !strings.Contains(async, "/qurl tunnel install") {
		t.Fatalf("async reply = %q, want retry-after guidance", async)
	}
}

func TestTunnelInstallBareIgnoresInvalidRateLimitRetryAfter(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error {
		return NewSlackRateLimitError("Wed, 21 Oct 2026 07:28:00 GMT")
	}

	inv := newAdminSlashInvoker(t, h)
	status, ack := inv.invokeAdmin("tunnel install", testAdminTeamID, testAdminUserID)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(ack, "Opening guided tunnel setup") {
		t.Fatalf("ack = %q, want immediate guided setup copy", ack)
	}
	async := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
	if !strings.Contains(async, "Wait a moment") || strings.Contains(async, "Wed, 21 Oct") {
		t.Fatalf("async reply = %q, want generic retry guidance without raw Retry-After", async)
	}
}

func TestTunnelInstallBareAcksBeforeSlowOpenView(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	openViewStarted := make(chan struct{})
	allowOpenViewReturn := make(chan struct{})
	defer close(allowOpenViewReturn)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error {
		close(openViewStarted)
		<-allowOpenViewReturn
		return nil
	}

	inv := newAdminSlashInvoker(t, h)
	type reply struct {
		status int
		ack    string
	}
	returned := make(chan reply, 1)
	go func() {
		status, ack := inv.invokeAdmin("tunnel install", testAdminTeamID, testAdminUserID)
		returned <- reply{status: status, ack: ack}
	}()
	select {
	case <-openViewStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("OpenView did not start")
	}
	select {
	case got := <-returned:
		if got.status != http.StatusOK {
			t.Fatalf("status = %d, want 200", got.status)
		}
		if !strings.Contains(got.ack, "Opening guided tunnel setup") {
			t.Fatalf("ack = %q, want immediate guided setup copy", got.ack)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slash-command ack waited for views.open to finish")
	}
}

func TestTunnelInstallModalSubmissionMintsKubernetesInstructions(t *testing.T) {
	now := time.Date(2026, 5, 27, 4, 30, 0, 0, time.UTC)
	freezeTunnelBootstrapNow(t, now)

	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	var resourceBody map[string]any
	var apiKeyBody map[string]any
	var idempotencyKey string
	ts.addCustomer(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&resourceBody); err != nil {
			t.Fatalf("decode resource body: %v", err)
		}
		respondQURLEnvelope(t, w, map[string]any{
			testKeyResourceID: testTunnelResourceID,
			testKeyType:       client.ResourceTypeTunnel,
			testKeySlug:       testTunnelSlug,
			testKeyStatus:     client.StatusActive,
		})
	})
	ts.addCustomer(http.MethodPost, "/v1/api-keys", func(w http.ResponseWriter, r *http.Request) {
		idempotencyKey = r.Header.Get(client.HeaderIdempotencyKey)
		if err := json.NewDecoder(r.Body).Decode(&apiKeyBody); err != nil {
			t.Fatalf("decode api key body: %v", err)
		}
		respondQURLEnvelope(t, w, map[string]any{
			testKeyKeyID:      testTunnelAPIKeyID,
			testKeyAPIKey:     testTunnelModalKey,
			"name":            "Slack tunnel bootstrap " + testTunnelSlug,
			"scopes":          []string{tunnelScopeAgent, tunnelScopeWrite},
			testKeyStatus:     client.StatusActive,
			testKeyPurpose:    client.APIKeyPurposeTunnelBootstrap,
			testKeyTunnelSlug: testTunnelSlug,
			testKeyExpiresAt:  now.Add(time.Hour).Format(time.RFC3339),
		})
	})

	h := newAdminTestHandler(t, ts)
	h.cfg.TunnelImage = testTunnelImageRef
	h.SetAliasStore(h.cfg.AdminStore)
	inv := newAdminSlashInvoker(t, h)
	meta := TunnelInstallModalMetadata{
		TeamID:        testAdminTeamID,
		ChannelID:     testTunnelChannelID,
		UserID:        testAdminUserID,
		ResponseURL:   inv.responseU.URL,
		CreatedAtUnix: now.Unix(),
	}
	body := tunnelInstallViewSubmissionBody(t, meta, map[string]map[string]interactionStateValue{
		tunnelInstallBlockSlug: {
			tunnelInstallActionSlug: {Value: "$" + testTunnelSlug},
		},
		tunnelInstallBlockShortcut: {
			tunnelInstallActionShortcut: {Value: "$team-dash"},
		},
		tunnelInstallBlockEnvironment: {
			tunnelInstallActionEnvironment: {SelectedOption: &interactionSelectedOption{Value: string(tunnelEnvKubernetes)}},
		},
		tunnelInstallBlockLocalPort: {
			tunnelInstallActionLocalPort: {Value: "9090"},
		},
	})
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
		t.Errorf("resource body = %+v, want tunnel find-or-create slug", resourceBody)
	}
	if apiKeyBody[testKeyPurpose] != client.APIKeyPurposeTunnelBootstrap || apiKeyBody[testKeyTunnelSlug] != testTunnelSlug {
		t.Errorf("api key body = %+v, want tunnel bootstrap key", apiKeyBody)
	}
	wantIdempotencyKey := tunnelBootstrapIdempotencyKey(testAdminTeamID, testTunnelChannelID, testAdminUserID, testTunnelSlug, now)
	if idempotencyKey != wantIdempotencyKey {
		t.Fatalf("Idempotency-Key = %q, want %q", idempotencyKey, wantIdempotencyKey)
	}
	for _, want := range []string{
		"Tunnel `" + testTunnelSlug + "` is ready to install.",
		"qURL shortcut `$team-dash` is ready in this channel.",
		"Target environment: Kubernetes.",
		"Paste it only when prompted or into your secret manager",
		"QURL_BOOTSTRAP_SECRET='qurl-tunnel-" + testTunnelSlug + "'",
		testTunnelKeyPromptLine,
		`kubectl create secret generic "$QURL_BOOTSTRAP_SECRET" --from-file=api_key=/dev/stdin`,
		"kubectl apply -f -",
		"kind: ConfigMap",
		"name: qurl-proxy-" + testTunnelSlug,
		"kind: PersistentVolumeClaim",
		"Pod spec additions:",
		"initContainers:",
		"name: qurl-agent-state-permissions",
		"securityContext:",
		"runAsUser: 65532",
		"claimName: qurl-agent-" + testTunnelSlug,
		"secretName: qurl-tunnel-" + testTunnelSlug,
		"defaultMode: 0444",
		"QURL_TUNNEL_SLUG",
		"value: " + testTunnelSlug,
		testTunnelModalKey,
		testTunnelLocalPort9090Line,
		testTunnelImageRef,
		"/qurl get $team-dash",
	} {
		if !strings.Contains(async, want) {
			t.Errorf("async reply missing %q:\n%s", want, async)
		}
	}
	for _, forbidden := range []string{testForbiddenResourceLabel, testTunnelResourceID, testForbiddenSlackYAMLFence, testForbiddenSlackShellFence, "connect.layerv", "proxy.layerv", "frps-", "\nfsGroup:"} {
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

func TestTunnelInstallModalSubmissionRendersDockerTargets(t *testing.T) {
	now := time.Date(2026, 5, 27, 4, 30, 0, 0, time.UTC)
	cases := []struct {
		name string
		env  tunnelInstallEnvironment
		web  string
		want []string
	}{
		{
			name: "docker",
			env:  tunnelEnvDocker,
			web:  "web.1",
			want: []string{
				"Target environment: Docker sidecar.",
				"WEB_CONTAINER='web.1'",
				testTunnelDockerLine,
				"docker logs -f qurl-tunnel-" + testTunnelSlug,
			},
		},
		{
			name: "compose",
			env:  tunnelEnvCompose,
			web:  "web_1",
			want: []string{
				"Target environment: Docker Compose.",
				"WEB_SERVICE='web_1'",
				"TUNNEL_SERVICE='qurl-tunnel-" + testTunnelSlug + "'",
				"qurl-tunnel-" + testTunnelSlug + ":",
				"docker compose -f compose.yaml -f qurl-tunnel-" + testTunnelSlug + ".compose.yaml logs -f qurl-tunnel-" + testTunnelSlug,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			freezeTunnelBootstrapNow(t, now)
			ts := newAdminTestServers(t)
			ts.seedAdmin(t)
			ts.addCustomer(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, r *http.Request) {
				respondQURLEnvelope(t, w, map[string]any{
					testKeyResourceID: testTunnelResourceID,
					testKeyType:       client.ResourceTypeTunnel,
					testKeySlug:       testTunnelSlug,
					testKeyStatus:     client.StatusActive,
				})
			})
			ts.addCustomer(http.MethodPost, "/v1/api-keys", func(w http.ResponseWriter, r *http.Request) {
				respondQURLEnvelope(t, w, map[string]any{
					testKeyKeyID:      testTunnelAPIKeyID,
					testKeyAPIKey:     testTunnelModalKey,
					testKeyStatus:     client.StatusActive,
					testKeyPurpose:    client.APIKeyPurposeTunnelBootstrap,
					testKeyTunnelSlug: testTunnelSlug,
					testKeyExpiresAt:  now.Add(time.Hour).Format(time.RFC3339),
				})
			})

			h := newAdminTestHandler(t, ts)
			h.cfg.TunnelImage = testTunnelImageRef
			h.SetAliasStore(h.cfg.AdminStore)
			inv := newAdminSlashInvoker(t, h)
			meta := TunnelInstallModalMetadata{
				TeamID:        testAdminTeamID,
				ChannelID:     testTunnelChannelID,
				UserID:        testAdminUserID,
				ResponseURL:   inv.responseU.URL,
				CreatedAtUnix: now.Unix(),
			}
			body := tunnelInstallViewSubmissionBody(t, meta, tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tc.env), "9090", tc.web))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
			}
			async := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
			for _, want := range tc.want {
				if !strings.Contains(async, want) {
					t.Fatalf("async reply missing %q:\n%s", want, async)
				}
			}
		})
	}
}

func TestTunnelInstallModalRejectsUnsafeWebRefBeforeMintingKey(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	body := tunnelInstallViewSubmissionBody(t, TunnelInstallModalMetadata{
		TeamID:        testAdminTeamID,
		ChannelID:     testTunnelChannelID,
		UserID:        testAdminUserID,
		ResponseURL:   testSlackResponseURL,
		CreatedAtUnix: tunnelBootstrapNow().Unix(),
	}, tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvDocker), "8080", "abc```def"))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), tunnelInstallBlockWebRef) || !strings.Contains(w.Body.String(), "Docker container name") {
		t.Fatalf("modal response = %s, want web_container field error", w.Body.String())
	}
}

func TestTunnelInstallModalRejectsEmptyPayloadIdentity(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	meta := TunnelInstallModalMetadata{
		TeamID:        testAdminTeamID,
		ChannelID:     testTunnelChannelID,
		UserID:        testAdminUserID,
		ResponseURL:   testSlackResponseURL,
		CreatedAtUnix: tunnelBootstrapNow().Unix(),
	}
	body := tunnelInstallViewSubmissionBodyWithIdentity(t, meta, "", "", tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvDocker), "8080", ""))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "different workspace") {
		t.Fatalf("modal response = %s, want identity rejection", w.Body.String())
	}
}

func TestTunnelInstallModalRejectsDifferentSubmitterWithRetryCopy(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	meta := TunnelInstallModalMetadata{
		TeamID:        testAdminTeamID,
		ChannelID:     testTunnelChannelID,
		UserID:        testAdminUserID,
		ResponseURL:   testSlackResponseURL,
		CreatedAtUnix: tunnelBootstrapNow().Unix(),
	}
	body := tunnelInstallViewSubmissionBodyWithIdentity(t, meta, testAdminTeamID, "U_other_admin", tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvDocker), "8080", ""))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	for _, want := range []string{"Only the admin who opened this modal", "Run /qurl tunnel install again"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("modal response = %s, want %q", w.Body.String(), want)
		}
	}
}

func TestTunnelInstallModalRejectsMissingAdminStore(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.AdminStore = nil
	meta := TunnelInstallModalMetadata{
		TeamID:        testAdminTeamID,
		ChannelID:     testTunnelChannelID,
		UserID:        testAdminUserID,
		ResponseURL:   testSlackResponseURL,
		CreatedAtUnix: tunnelBootstrapNow().Unix(),
	}
	body := tunnelInstallViewSubmissionBody(t, meta, tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvDocker), "8080", ""))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Admin features are not configured") {
		t.Fatalf("modal response = %s, want admin-store configuration rejection", w.Body.String())
	}
}

func TestTunnelInstallModalRejectsMissingAliasStore(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	meta := TunnelInstallModalMetadata{
		TeamID:        testAdminTeamID,
		ChannelID:     testTunnelChannelID,
		UserID:        testAdminUserID,
		ResponseURL:   testSlackResponseURL,
		CreatedAtUnix: tunnelBootstrapNow().Unix(),
	}
	body := tunnelInstallViewSubmissionBody(t, meta, tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvDocker), "8080", ""))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Channel shortcut storage is not configured") {
		t.Fatalf("modal response = %s, want channel-shortcut store rejection", w.Body.String())
	}
}

func TestTunnelInstallModalRejectsNonAdminSubmitter(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	const nonAdminUserID = "U_non_admin"
	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	meta := TunnelInstallModalMetadata{
		TeamID:        testAdminTeamID,
		ChannelID:     testTunnelChannelID,
		UserID:        nonAdminUserID,
		ResponseURL:   testSlackResponseURL,
		CreatedAtUnix: tunnelBootstrapNow().Unix(),
	}
	body := tunnelInstallViewSubmissionBodyWithIdentity(t, meta, testAdminTeamID, nonAdminUserID, tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvDocker), "8080", ""))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "admin-only") {
		t.Fatalf("modal response = %s, want non-admin rejection", w.Body.String())
	}
}

func TestTunnelInstallModalRejectsStaleSubmissionBeforeMintingKey(t *testing.T) {
	now := time.Date(2026, 5, 27, 4, 30, 0, 0, time.UTC)
	freezeTunnelBootstrapNow(t, now)

	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	meta := TunnelInstallModalMetadata{
		TeamID:        testAdminTeamID,
		ChannelID:     testTunnelChannelID,
		UserID:        testAdminUserID,
		ResponseURL:   testSlackResponseURL,
		CreatedAtUnix: now.Add(-tunnelInstallModalTTL - time.Minute).Unix(),
	}
	body := tunnelInstallViewSubmissionBody(t, meta, tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvDocker), "8080", ""))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "modal expired") {
		t.Fatalf("modal response = %s, want stale modal rejection", w.Body.String())
	}
}

func TestTunnelInstallModalRejectsFutureSubmissionBeforeMintingKey(t *testing.T) {
	now := time.Date(2026, 5, 27, 4, 30, 0, 0, time.UTC)
	freezeTunnelBootstrapNow(t, now)

	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	meta := TunnelInstallModalMetadata{
		TeamID:        testAdminTeamID,
		ChannelID:     testTunnelChannelID,
		UserID:        testAdminUserID,
		ResponseURL:   testSlackResponseURL,
		CreatedAtUnix: now.Add(tunnelBootstrapSkew + time.Minute).Unix(),
	}
	body := tunnelInstallViewSubmissionBody(t, meta, tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvDocker), "8080", ""))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "modal expired") {
		t.Fatalf("modal response = %s, want future modal rejection", w.Body.String())
	}
}

func TestTunnelInstallModalRejectsMissingCreatedAtBeforeMintingKey(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	meta := TunnelInstallModalMetadata{
		TeamID:      testAdminTeamID,
		ChannelID:   testTunnelChannelID,
		UserID:      testAdminUserID,
		ResponseURL: testSlackResponseURL,
	}
	body := tunnelInstallViewSubmissionBody(t, meta, tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvDocker), "8080", ""))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "modal expired") {
		t.Fatalf("modal response = %s, want missing-created-at rejection", w.Body.String())
	}
}

func TestParseTunnelInstallModalArgsRejectsMissingEnvironment(t *testing.T) {
	t.Parallel()
	values := tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvDocker), "8080", "")
	delete(values, tunnelInstallBlockEnvironment)

	args, fieldErrors := parseTunnelInstallModalArgs(values)

	if args != nil {
		t.Fatalf("args = %+v, want nil", args)
	}
	if fieldErrors[tunnelInstallBlockEnvironment] == "" {
		t.Fatalf("field errors = %+v, want target environment error", fieldErrors)
	}
}

func TestParseTunnelInstallModalArgsSkipsWebRefValidationWhenEnvironmentMissing(t *testing.T) {
	t.Parallel()
	values := tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvDocker), "8080", "../bad")
	delete(values, tunnelInstallBlockEnvironment)

	args, fieldErrors := parseTunnelInstallModalArgs(values)

	if args != nil {
		t.Fatalf("args = %+v, want nil", args)
	}
	if fieldErrors[tunnelInstallBlockEnvironment] == "" {
		t.Fatalf("field errors = %+v, want target environment error", fieldErrors)
	}
	if fieldErrors[tunnelInstallBlockWebRef] != "" {
		t.Fatalf("field errors = %+v, want web container validation deferred until environment is known", fieldErrors)
	}
}

func TestParseTunnelInstallModalArgsRejectsDottedComposeService(t *testing.T) {
	t.Parallel()
	values := tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvCompose), "8080", "web.1")

	args, fieldErrors := parseTunnelInstallModalArgs(values)

	if args != nil {
		t.Fatalf("args = %+v, want nil", args)
	}
	got := fieldErrors[tunnelInstallBlockWebRef]
	if !strings.Contains(got, "Compose service name") || !strings.Contains(got, "Dots are not allowed") {
		t.Fatalf("web_container error = %q, want Compose service dot rejection", got)
	}
}

func TestParseTunnelInstallModalArgsDefaultsEmptyPort(t *testing.T) {
	t.Parallel()
	values := tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvDocker), "", "")

	args, fieldErrors := parseTunnelInstallModalArgs(values)

	if len(fieldErrors) != 0 {
		t.Fatalf("field errors = %+v, want none", fieldErrors)
	}
	if args == nil || args.LocalPort != defaultTunnelLocalPort {
		t.Fatalf("args = %+v, want default local port %d", args, defaultTunnelLocalPort)
	}
}

func TestParseTunnelInstallModalArgsRejectsMissingPortBlock(t *testing.T) {
	t.Parallel()
	values := tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvDocker), "8080", "")
	delete(values, tunnelInstallBlockLocalPort)

	args, fieldErrors := parseTunnelInstallModalArgs(values)

	if args != nil {
		t.Fatalf("args = %+v, want nil", args)
	}
	if fieldErrors[tunnelInstallBlockLocalPort] == "" {
		t.Fatalf("field errors = %+v, want local port error", fieldErrors)
	}
}

func TestParseTunnelInstallModalArgsPreservesAliasValidationReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		shortcut   string
		wantReason string
	}{
		{name: "too long", shortcut: strings.Repeat("a", aliasMaxLen+1), wantReason: "longer than"},
		{name: "bad character", shortcut: "bad_alias", wantReason: "lowercase alphanumeric + dashes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			values := tunnelInstallModalValues(testTunnelSlug, tc.shortcut, string(tunnelEnvDocker), "8080", "")

			args, fieldErrors := parseTunnelInstallModalArgs(values)

			if args != nil {
				t.Fatalf("args = %+v, want nil", args)
			}
			got := fieldErrors[tunnelInstallBlockShortcut]
			if !strings.Contains(got, tc.wantReason) || strings.Contains(got, "Usage:") {
				t.Fatalf("shortcut error = %q, want %q without usage suffix", got, tc.wantReason)
			}
		})
	}
}

func TestRenderTunnelInstallMessageWarnsOnDefaultImage(t *testing.T) {
	now := time.Date(2026, 5, 27, 4, 30, 0, 0, time.UTC)
	freezeTunnelBootstrapNow(t, now)
	expiresAt := now.Add(time.Hour)

	got, err := NewHandler(Config{}).renderTunnelInstallMessage(&tunnelInstallArgs{
		Slug:        testTunnelSlug,
		Alias:       testTunnelSlug,
		LocalPort:   defaultTunnelLocalPort,
		Environment: tunnelEnvDocker,
	}, &client.APIKey{APIKey: testTunnelAPIKey, ExpiresAt: &expiresAt}, "qURL shortcut `$prod-dashboard` is ready in this channel.")
	if err != nil {
		t.Fatalf("renderTunnelInstallMessage: %v", err)
	}

	if !strings.Contains(got, "Image: using the dev/sandbox fallback") || !strings.Contains(got, defaultTunnelImage) {
		t.Fatalf("rendered install message missing fallback image warning:\n%s", got)
	}
	if strings.Contains(got, testForbiddenResourceLabel) || strings.Contains(got, testTunnelResourceID) {
		t.Fatalf("rendered install message leaked resource details:\n%s", got)
	}
}

func TestValidateTunnelImageRefRejectsBackticks(t *testing.T) {
	t.Parallel()

	err := ValidateTunnelImageRef("ghcr.io/layervai/qurl```bad")

	if err == nil || !strings.Contains(err.Error(), "backticks") {
		t.Fatalf("ValidateTunnelImageRef error = %v, want backtick rejection", err)
	}
}

func TestTunnelInstallRejectsMissingPlaintextBootstrapKey(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	var revokeHits int
	ts.addCustomer(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		respondQURLEnvelope(t, w, map[string]any{
			testKeyResourceID: testTunnelResourceID,
			testKeyType:       client.ResourceTypeTunnel,
			testKeySlug:       testTunnelSlug,
			testKeyStatus:     client.StatusActive,
		})
	})
	ts.addCustomer(http.MethodPost, "/v1/api-keys", func(w http.ResponseWriter, _ *http.Request) {
		respondQURLEnvelope(t, w, map[string]any{
			testKeyKeyID:      testTunnelAPIKeyID,
			testKeyAPIKey:     "",
			testKeyPurpose:    client.APIKeyPurposeTunnelBootstrap,
			testKeyTunnelSlug: testTunnelSlug,
		})
	})
	ts.addCustomer(http.MethodDelete, "/v1/api-keys/"+testTunnelAPIKeyID, func(w http.ResponseWriter, _ *http.Request) {
		revokeHits++
		w.WriteHeader(http.StatusNoContent)
	})

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	_, _, async := newAdminSlashInvoker(t, h).invokeAdminAsync(testTunnelInstallCmd, testAdminTeamID, testAdminUserID)

	if !strings.Contains(async, "did not return a bootstrap key") {
		t.Fatalf("async reply = %q, want missing-plaintext copy", async)
	}
	if revokeHits != 1 {
		t.Fatalf("bootstrap key revoke hits = %d, want 1", revokeHits)
	}
}

func TestRevokeBootstrapKeyAfterInstallFailureNoopsWithoutKeyID(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := client.New("http://127.0.0.1", "unused", client.WithRetry(0))
	revokeBootstrapKeyAfterInstallFailure(context.Background(), log, c, &client.APIKey{}, "missing_key_id")
}

func TestTunnelInstallRevokesBootstrapKeyWhenShellValidationFails(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	var revokeHits int
	ts.addCustomer(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		respondQURLEnvelope(t, w, map[string]any{
			testKeyResourceID: testTunnelResourceID,
			testKeyType:       client.ResourceTypeTunnel,
			testKeySlug:       testTunnelSlug,
			testKeyStatus:     client.StatusActive,
		})
	})
	ts.addCustomer(http.MethodPost, "/v1/api-keys", func(w http.ResponseWriter, _ *http.Request) {
		respondQURLEnvelope(t, w, map[string]any{
			testKeyKeyID:      testTunnelAPIKeyID,
			testKeyAPIKey:     "lv_live_bad'quote",
			testKeyPurpose:    client.APIKeyPurposeTunnelBootstrap,
			testKeyTunnelSlug: testTunnelSlug,
		})
	})
	ts.addCustomer(http.MethodDelete, "/v1/api-keys/"+testTunnelAPIKeyID, func(w http.ResponseWriter, _ *http.Request) {
		revokeHits++
		w.WriteHeader(http.StatusNoContent)
	})

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	_, _, async := newAdminSlashInvoker(t, h).invokeAdminAsync(testTunnelInstallCmd, testAdminTeamID, testAdminUserID)

	if !strings.Contains(async, "unexpected format") {
		t.Fatalf("async reply = %q, want shell-validation copy", async)
	}
	if revokeHits != 1 {
		t.Fatalf("bootstrap key revoke hits = %d, want 1", revokeHits)
	}
}

func TestTunnelInstallRetryRemintsWhenAliasAlreadyMatches(t *testing.T) {
	now := time.Date(2026, 5, 27, 4, 30, 0, 0, time.UTC)
	freezeTunnelBootstrapNow(t, now)

	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	var apiKeyHits int
	var idempotencyKeys []string
	ts.addCustomer(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		respondQURLEnvelope(t, w, map[string]any{
			testKeyResourceID: testTunnelResourceID,
			testKeyType:       client.ResourceTypeTunnel,
			testKeySlug:       testTunnelSlug,
			testKeyStatus:     client.StatusActive,
		})
	})
	ts.addCustomer(http.MethodPost, "/v1/api-keys", func(w http.ResponseWriter, r *http.Request) {
		idempotencyKeys = append(idempotencyKeys, r.Header.Get(client.HeaderIdempotencyKey))
		apiKeyHits++
		if apiKeyHits == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"title":"boom","status":500}}`))
			return
		}
		respondQURLEnvelope(t, w, map[string]any{
			testKeyKeyID:      testTunnelAPIKeyID,
			testKeyAPIKey:     "lv_live_retry_bootstrap",
			testKeyPurpose:    client.APIKeyPurposeTunnelBootstrap,
			testKeyTunnelSlug: testTunnelSlug,
			testKeyExpiresAt:  now.Add(time.Hour).Format(time.RFC3339),
		})
	})

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)

	_, _, first := newAdminSlashInvoker(t, h).invokeAdminAsync(testTunnelInstallCmd, testAdminTeamID, testAdminUserID)
	if !strings.Contains(first, "Failed to mint") {
		t.Fatalf("first async reply = %q, want mint failure", first)
	}
	_, _, second := newAdminSlashInvoker(t, h).invokeAdminAsync(testTunnelInstallCmd, testAdminTeamID, testAdminUserID)
	if !strings.Contains(second, "lv_live_retry_bootstrap") || !strings.Contains(second, "qURL shortcut `$"+testTunnelSlug+"` is ready in this channel.") {
		t.Fatalf("second async reply = %q, want successful remint against existing alias", second)
	}
	if apiKeyHits != 2 {
		t.Fatalf("api key hits = %d, want 2", apiKeyHits)
	}
	if len(idempotencyKeys) != 2 || idempotencyKeys[0] == "" || idempotencyKeys[0] != idempotencyKeys[1] {
		t.Fatalf("idempotency keys = %v, want same non-empty retry key", idempotencyKeys)
	}
	nextHourKey := tunnelBootstrapIdempotencyKey(testAdminTeamID, testTunnelChannelID, testAdminUserID, testTunnelSlug, now.Add(time.Hour))
	if nextHourKey == idempotencyKeys[0] {
		t.Fatal("next-hour tunnel bootstrap idempotency key matched current-hour key")
	}
}

func TestEnsureTunnelAliasRecoversConcurrentSameResourceBind(t *testing.T) {
	ts := newAdminTestServers(t)
	store := newStoreFromFake(t, ts.ddb, ts.tableNames, nil)
	h := NewHandler(Config{AdminStore: store})
	h.aliasStore = raceBindAliasStore{store: store}

	status, err := h.ensureTunnelAlias(context.Background(), testAdminTeamID, testTunnelChannelID, testTunnelSlug, testTunnelResourceID)
	if err != nil {
		t.Fatalf("ensureTunnelAlias: %v", err)
	}
	if !strings.Contains(status, "qURL shortcut `$"+testTunnelSlug+"` is ready in this channel.") {
		t.Fatalf("status = %q, want idempotent ready copy", status)
	}
}

func TestRenderECSFargateTunnelInstructions(t *testing.T) {
	t.Parallel()
	got := renderECSFargateTunnelInstructions(&tunnelInstallArgs{
		Slug:        testTunnelSlug,
		Alias:       testTunnelSlug,
		LocalPort:   9090,
		Environment: tunnelEnvECSFargate,
	}, &client.APIKey{APIKey: testTunnelAPIKey}, testTunnelImageRef)

	for _, want := range []string{
		"ECS/Fargate task-definition checklist",
		"non-essential sidecar container",
		"Fargate's awsvpc network mode",
		"Replace `<region>`, `<account-id>`, and `<suffix>`",
		"AWS appends a random suffix",
		"127.0.0.1:9090",
		"AWS Secrets Manager",
		"Store the bootstrap key shown above",
		"ECS injects the bootstrap secret as `QURL_API_KEY`",
		"file-mounted secret runtimes use `QURL_API_KEY_FILE` instead",
		"secret as `qurl-tunnel-" + testTunnelSlug + "`",
		"treat this Slack message as secret until the sidecar connects",
		testTunnelImageRef,
		"Put qurl-proxy.yaml on an EFS access point",
		testTunnelLocalPort9090Line,
		`"name": "QURL_TUNNEL_SLUG"`,
		`"value": "` + testTunnelSlug + `"`,
		`"name": "QURL_API_KEY"`,
		`"sourceVolume": "qurl-agent-state"`,
		`"sourceVolume": "qurl-config"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("ECS instructions missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{testForbiddenSlackYAMLFence, testForbiddenSlackShellFence, testForbiddenResourceLabel, testTunnelResourceID, testTunnelAPIKey} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("ECS instructions leaked %q:\n%s", forbidden, got)
		}
	}
	if gotFenceCount := strings.Count(got, "```"); gotFenceCount != 4 {
		t.Fatalf("ECS instructions rendered %d code fences, want 4 for two independently copyable artifacts:\n%s", gotFenceCount, got)
	}

	var container ecsContainerDefinition
	if err := json.Unmarshal([]byte(renderECSSidecarContainerJSON(&tunnelInstallArgs{
		Slug:        testTunnelSlug,
		Alias:       testTunnelSlug,
		LocalPort:   9090,
		Environment: tunnelEnvECSFargate,
	}, testTunnelImageRef)), &container); err != nil {
		t.Fatalf("ECS sidecar JSON did not parse: %v", err)
	}
	if container.Essential {
		t.Fatal("ECS sidecar Essential = true, want false so the tunnel does not take down the app task")
	}
	if len(container.Secrets) != 1 || container.Image != testTunnelImageRef || container.Secrets[0].Name != tunnelEnvAPIKey {
		t.Fatalf("ECS sidecar = %+v, want image and bootstrap secret wiring", container)
	}
	if !strings.Contains(container.Secrets[0].ValueFrom, "-<suffix>") {
		t.Fatalf("ECS secret ValueFrom = %q, want full Secrets Manager ARN suffix placeholder", container.Secrets[0].ValueFrom)
	}
}

func TestRenderKubernetesTunnelInstructionsYAMLAndSecurityContext(t *testing.T) {
	t.Parallel()
	args := &tunnelInstallArgs{
		Slug:        testTunnelSlug,
		Alias:       testTunnelSlug,
		LocalPort:   9090,
		Environment: tunnelEnvKubernetes,
	}
	got := renderKubernetesTunnelInstructions(args, &client.APIKey{APIKey: testTunnelAPIKey}, testTunnelImageRef)

	for _, want := range []string{
		"QURL_BOOTSTRAP_SECRET='qurl-tunnel-" + testTunnelSlug + "'",
		testTunnelKeyPromptLine,
		`printf '%s' "$QURL_BOOTSTRAP_KEY" | kubectl create secret generic "$QURL_BOOTSTRAP_SECRET" --from-file=api_key=/dev/stdin --dry-run=client -o yaml | kubectl apply -f -`,
		"unset QURL_BOOTSTRAP_KEY",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Kubernetes instructions missing %q:\n%s", want, got)
		}
	}
	start := "kubectl apply -f - <<'QURL_K8S_YAML_EOF'\n"
	bodyStart := strings.Index(got, start)
	if bodyStart < 0 {
		t.Fatalf("Kubernetes instructions missing apply heredoc:\n%s", got)
	}
	bodyStart += len(start)
	bodyEnd := strings.Index(got[bodyStart:], "\nQURL_K8S_YAML_EOF")
	if bodyEnd < 0 {
		t.Fatalf("Kubernetes instructions missing heredoc terminator:\n%s", got)
	}
	docs := strings.Split(got[bodyStart:bodyStart+bodyEnd], "\n---\n")
	if len(docs) != 2 {
		t.Fatalf("Kubernetes bootstrap docs = %d, want 2: %#v", len(docs), docs)
	}
	for i, doc := range docs {
		var parsed map[string]any
		if err := yaml.Unmarshal([]byte(doc), &parsed); err != nil {
			t.Fatalf("bootstrap YAML doc %d did not parse: %v\n%s", i, err, doc)
		}
	}
	var configMap struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal([]byte(docs[0]), &configMap); err != nil {
		t.Fatalf("ConfigMap YAML did not parse: %v", err)
	}
	if gotConfig := configMap.Data["qurl-proxy.yaml"]; gotConfig != renderTunnelConfigYAML(args) {
		t.Fatalf("ConfigMap qurl-proxy.yaml = %q, want %q", gotConfig, renderTunnelConfigYAML(args))
	}
	for _, want := range []string{
		"sidecar/initContainer/volumes block",
		"no pod-level `securityContext` or `fsGroup` is set",
		"initContainers:",
		"name: qurl-agent-state-permissions",
		"chown -R 65532:65532 /var/lib/layerv/agent",
		"securityContext:",
		"runAsUser: 0",
		"allowPrivilegeEscalation: false",
		"name: qurl-tunnel",
		"runAsUser: 65532",
		"runAsGroup: 65532",
		"defaultMode: 0444",
		"do not co-locate the sidecar with untrusted containers",
		"`fsGroup: 65532`",
		"`0440`",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Kubernetes instructions missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{
		"\nfsGroup:",
		"defaultMode: 0400",
		"securityContext:\n  runAsUser",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("Kubernetes instructions included pod-level or unreadable secret setting %q:\n%s", forbidden, got)
		}
	}
	if strings.Contains(got, testTunnelAPIKey) {
		t.Fatalf("Kubernetes instructions embedded bootstrap key instead of prompting:\n%s", got)
	}
}

func TestKubernetesTunnelObjectNamesShortenLongSlug(t *testing.T) {
	t.Parallel()
	slug := strings.Repeat("a", 42) + "-" + strings.Repeat("b", 21)
	dns1123Label := regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)
	args := &tunnelInstallArgs{
		Slug:        slug,
		Alias:       slug,
		LocalPort:   9090,
		Environment: tunnelEnvKubernetes,
	}
	names := kubernetesTunnelObjectNames(slug)
	for label, name := range map[string]string{
		"secret":     names.secret,
		"config_map": names.configMap,
		"agent_pvc":  names.agentPVC,
	} {
		if len(name) > kubernetesNameMaxLen {
			t.Fatalf("%s name length = %d for %q, want <= %d", label, len(name), name, kubernetesNameMaxLen)
		}
		if strings.HasSuffix(name, "-") {
			t.Fatalf("%s name = %q, must end with an alphanumeric hash suffix", label, name)
		}
		if strings.Contains(name, "--") {
			t.Fatalf("%s name = %q, should trim hyphens before hash suffix", label, name)
		}
		if !dns1123Label.MatchString(name) {
			t.Fatalf("%s name = %q, want DNS-1123 label", label, name)
		}
	}

	got := renderKubernetesTunnelInstructions(args, &client.APIKey{APIKey: testTunnelAPIKey}, testTunnelImageRef)
	for _, want := range []string{
		"QURL_BOOTSTRAP_SECRET='" + names.secret + "'",
		"name: " + names.configMap,
		"name: " + names.agentPVC,
		"claimName: " + names.agentPVC,
		"secretName: " + names.secret,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Kubernetes instructions missing shortened name %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{
		"qurl-tunnel-" + slug,
		"qurl-proxy-" + slug,
		"qurl-agent-" + slug,
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("Kubernetes instructions contain overlong name %q:\n%s", forbidden, got)
		}
	}
}

func TestKubernetesNameWithSlugHandlesEmptyTrimmedBase(t *testing.T) {
	t.Parallel()
	got := kubernetesNameWithSlug("qurl-tunnel-", strings.Repeat("-", 80))
	if strings.Contains(got, "--") {
		t.Fatalf("name = %q, want no doubled hyphen when trimmed base is empty", got)
	}
	if len(got) > kubernetesNameMaxLen {
		t.Fatalf("name length = %d for %q, want <= %d", len(got), got, kubernetesNameMaxLen)
	}
}

func TestRenderDockerComposeTunnelInstructionsUsesWebService(t *testing.T) {
	t.Parallel()
	got := renderDockerComposeTunnelInstructions(&tunnelInstallArgs{
		Slug:        testTunnelSlug,
		Alias:       testTunnelSlug,
		LocalPort:   9090,
		Environment: tunnelEnvCompose,
		WebRef:      "web_1-2",
	}, &client.APIKey{APIKey: testTunnelAPIKey}, testTunnelImageRef)

	for _, want := range []string{
		"Run this from your Docker Compose project directory on the Linux Docker host.",
		testTunnelKeyHistoryNote,
		"WEB_SERVICE='web_1-2'",
		"TUNNEL_SERVICE='qurl-tunnel-" + testTunnelSlug + "'",
		`CONFIG_FILE="$PWD/qurl-proxy-${QURL_TUNNEL_SLUG}.yaml"`,
		`QURL_COMPOSE_FILE="$PWD/qurl-tunnel-${QURL_TUNNEL_SLUG}.compose.yaml"`,
		testTunnelKeyPromptLine,
		testTunnelKeyInstallLine,
		"qurl-tunnel-" + testTunnelSlug + ".compose.yaml",
		"qurl-tunnel-" + testTunnelSlug + ":",
		`network_mode: "service:${WEB_SERVICE}"`,
		"depends_on:",
		testTunnelAgentDirFragment,
		"QURL_TUNNEL_SLUG: ${QURL_TUNNEL_SLUG}",
		"QURL_TUNNEL_SLUG='" + testTunnelSlug + "'",
		`docker compose -f "$APP_COMPOSE_FILE" -f "$QURL_COMPOSE_FILE" up -d "$TUNNEL_SERVICE"`,
		"Verify with `docker compose -f compose.yaml -f qurl-tunnel-" + testTunnelSlug + ".compose.yaml logs -f qurl-tunnel-" + testTunnelSlug + "`",
		"if you changed `APP_COMPOSE_FILE`, use that file there too",
		"logs -f qurl-tunnel-" + testTunnelSlug,
		testTunnelLocalPort9090Line,
		testTunnelImageRef,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Docker Compose instructions missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Replace `YOUR_COMPOSE_SERVICE_NAME`") {
		t.Fatalf("Docker Compose instructions still included placeholder warning:\n%s", got)
	}
	for _, forbidden := range []string{
		"\n  qurl-tunnel:\n",
		"up -d qurl-tunnel\n",
		"logs -f qurl-tunnel`;",
		"Verify with `docker compose -f \"$APP_COMPOSE_FILE\"",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("Docker Compose instructions used unscoped service %q:\n%s", forbidden, got)
		}
	}
	for _, forbidden := range []string{testForbiddenSlackYAMLFence, testForbiddenSlackShellFence, testForbiddenResourceLabel, testTunnelResourceID, testTunnelAPIKey} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("Docker Compose instructions leaked %q:\n%s", forbidden, got)
		}
	}
}

func TestRenderDockerComposeTunnelInstructionsEmitsParseableComposeFragment(t *testing.T) {
	t.Parallel()
	got := renderDockerComposeTunnelInstructions(&tunnelInstallArgs{
		Slug:        testTunnelSlug,
		Alias:       testTunnelSlug,
		LocalPort:   9090,
		Environment: tunnelEnvCompose,
		WebRef:      "web",
	}, &client.APIKey{APIKey: testTunnelAPIKey}, testTunnelImageRef)

	start := "cat > \"$QURL_COMPOSE_FILE\" <<QURL_COMPOSE_YAML_EOF\n"
	bodyStart := strings.Index(got, start)
	if bodyStart < 0 {
		t.Fatalf("Compose instructions missing generated fragment heredoc:\n%s", got)
	}
	bodyStart += len(start)
	bodyEnd := strings.Index(got[bodyStart:], "\nQURL_COMPOSE_YAML_EOF")
	if bodyEnd < 0 {
		t.Fatalf("Compose instructions missing generated fragment heredoc terminator:\n%s", got)
	}
	var parsed struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(got[bodyStart:bodyStart+bodyEnd]), &parsed); err != nil {
		t.Fatalf("Compose fragment did not parse: %v", err)
	}
	service := parsed.Services["qurl-tunnel-"+testTunnelSlug]
	if service.Image != testTunnelImageRef {
		t.Fatalf("Compose service image = %q, want %q", service.Image, testTunnelImageRef)
	}
}

func TestRenderDockerTunnelInstructionsUsesWebRef(t *testing.T) {
	t.Parallel()
	got := renderDockerTunnelInstructions(&tunnelInstallArgs{
		Slug:        testTunnelSlug,
		Alias:       testTunnelSlug,
		LocalPort:   9090,
		Environment: tunnelEnvDocker,
		WebRef:      "web.1_2-3",
	}, &client.APIKey{APIKey: testTunnelAPIKey}, testTunnelImageRef)

	for _, want := range []string{
		testTunnelKeyHistoryNote,
		"WEB_CONTAINER='web.1_2-3'",
		`CONFIG_FILE="$PWD/qurl-proxy-${QURL_TUNNEL_SLUG}.yaml"`,
		testTunnelKeyPromptLine,
		testTunnelKeyInstallLine,
		`--network "container:${WEB_CONTAINER}"`,
		testTunnelDockerLine,
		testTunnelAgentDirFragment,
		testTunnelLocalPort9090Line,
		testTunnelImageRef,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Docker instructions missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Replace `YOUR_WEB_CONTAINER_NAME`") {
		t.Fatalf("Docker instructions still included placeholder warning:\n%s", got)
	}
	if strings.Contains(got, testTunnelAPIKey) {
		t.Fatalf("Docker instructions embedded bootstrap key instead of prompting:\n%s", got)
	}
}

func TestTunnelInstallTypedEnvironmentInstructions(t *testing.T) {
	now := time.Date(2026, 5, 27, 4, 30, 0, 0, time.UTC)
	freezeTunnelBootstrapNow(t, now)

	cases := []struct {
		name string
		env  string
		want []string
	}{
		{
			name: "ecs fargate",
			env:  string(tunnelEnvECSFargate),
			want: []string{
				"Target environment: AWS ECS/Fargate.",
				"ECS/Fargate task-definition checklist",
				`"name": "QURL_API_KEY"`,
			},
		},
		{
			name: "kubernetes",
			env:  string(tunnelEnvKubernetes),
			want: []string{
				"Target environment: Kubernetes.",
				"kubectl apply -f -",
				"Pod spec additions:",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newAdminTestServers(t)
			ts.seedAdmin(t)
			ts.addCustomer(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
				respondQURLEnvelope(t, w, map[string]any{
					testKeyResourceID: testTunnelResourceID,
					testKeyType:       client.ResourceTypeTunnel,
					testKeySlug:       testTunnelSlug,
					testKeyStatus:     client.StatusActive,
				})
			})
			ts.addCustomer(http.MethodPost, "/v1/api-keys", func(w http.ResponseWriter, _ *http.Request) {
				respondQURLEnvelope(t, w, map[string]any{
					testKeyKeyID:      testTunnelAPIKeyID,
					testKeyAPIKey:     testTunnelAPIKey,
					testKeyPurpose:    client.APIKeyPurposeTunnelBootstrap,
					testKeyTunnelSlug: testTunnelSlug,
					testKeyExpiresAt:  now.Add(time.Hour).Format(time.RFC3339),
				})
			})

			h := newAdminTestHandler(t, ts)
			h.cfg.TunnelImage = testTunnelImageRef
			h.SetAliasStore(h.cfg.AdminStore)

			status, ack, async := newAdminSlashInvoker(t, h).invokeAdminAsync(testTunnelInstallCmd+" env:"+tc.env, testAdminTeamID, testAdminUserID)

			if status != http.StatusOK || !strings.Contains(ack, "Working") {
				t.Fatalf("status=%d ack=%q, want async accepted", status, ack)
			}
			for _, want := range tc.want {
				if !strings.Contains(async, want) {
					t.Fatalf("%s async reply missing %q:\n%s", tc.name, want, async)
				}
			}
			if strings.Contains(async, testForbiddenResourceLabel) || strings.Contains(async, testTunnelResourceID) {
				t.Fatalf("%s async reply leaked resource details:\n%s", tc.name, async)
			}
		})
	}
}

func TestTunnelInstallRefusesExistingDifferentAliasBeforeMintingKey(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.seedPolicyAliasBindings(t, testAdminTeamID, testTunnelChannelID, map[string]string{testTunnelSlug: "r_other"})

	var apiKeyHits int
	ts.addCustomer(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		respondQURLEnvelope(t, w, map[string]any{
			testKeyResourceID: testTunnelResourceID,
			testKeyType:       client.ResourceTypeTunnel,
			testKeySlug:       testTunnelSlug,
			testKeyStatus:     client.StatusActive,
		})
	})
	ts.addCustomer(http.MethodPost, "/v1/api-keys", func(w http.ResponseWriter, _ *http.Request) {
		apiKeyHits++
		w.WriteHeader(http.StatusInternalServerError)
	})

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	_, _, async := newAdminSlashInvoker(t, h).invokeAdminAsync(testTunnelInstallCmd, testAdminTeamID, testAdminUserID)

	if !strings.Contains(async, "qURL shortcut") || !strings.Contains(async, "already used") {
		t.Fatalf("async reply = %q, want already-bound refusal", async)
	}
	if apiKeyHits != 0 {
		t.Fatalf("api key route hit %d times; bootstrap key must not be minted when alias bind fails", apiKeyHits)
	}
}

type raceBindAliasStore struct {
	store *slackdata.Store
}

func (r raceBindAliasStore) BindChannelAlias(ctx context.Context, teamID, channelID, aliasName, resourceID string) error {
	if err := r.store.BindChannelAlias(ctx, teamID, channelID, aliasName, resourceID); err != nil {
		return err
	}
	return slackdata.ErrAliasAlreadyBound
}

func (r raceBindAliasStore) UnbindChannelAlias(ctx context.Context, teamID, channelID, aliasName string) error {
	return r.store.UnbindChannelAlias(ctx, teamID, channelID, aliasName)
}

func TestValidateBootstrapAPIKeyForShell(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{name: "current live-ish shape", key: "lv_live_-Pe7wofxOWsLBlOL1hwPPV491dqNJ4zHbNuRvQUaRHQ"},
		{name: "base64 characters allowed", key: "abc+/def=="},
		{name: "colon allowed", key: "prefix:payload.signature"},
		{name: "empty rejected", key: "", wantErr: true},
		{name: "quote rejected", key: "abc'def", wantErr: true},
		{name: "backtick rejected", key: "abc`def", wantErr: true},
		{name: "newline rejected", key: "abc\ndef", wantErr: true},
		{name: "del rejected", key: "abc\x7fdef", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateBootstrapAPIKeyForShell(tc.key)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateBootstrapAPIKeyForShell(%q) err=%v, wantErr=%v", tc.key, err, tc.wantErr)
			}
		})
	}
}

func TestShellSingleQuote(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{in: "", want: "''"},
		{in: "plain", want: "'plain'"},
		{in: "$HOME && rm -rf /", want: "'$HOME && rm -rf /'"},
		{in: "a'b", want: `'a'"'"'b'`},
		{in: "line\nbreak", want: "'line\nbreak'"},
	}
	for _, tc := range cases {
		if got := shellSingleQuote(tc.in); got != tc.want {
			t.Errorf("shellSingleQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHumanTunnelBootstrapTTL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ttl  string
		want string
	}{
		{ttl: "1h", want: "1 hour"},
		{ttl: "24h", want: "24 hours"},
		{ttl: "30m", want: "30 minutes"},
		{ttl: "75m", want: "1 hour 15 minutes"},
		{ttl: "90s", want: "2 minutes"},
		{ttl: "later", want: "the requested later"},
	}
	for _, tc := range cases {
		t.Run(tc.ttl, func(t *testing.T) {
			t.Parallel()
			if got := humanTunnelBootstrapTTL(tc.ttl); got != tc.want {
				t.Fatalf("humanTunnelBootstrapTTL(%q) = %q, want %q", tc.ttl, got, tc.want)
			}
		})
	}
}

func TestTunnelBootstrapExpiryLabelFallsBackOnClockSkew(t *testing.T) {
	now := time.Date(2026, 5, 27, 4, 30, 0, 0, time.UTC)
	freezeTunnelBootstrapNow(t, now)
	expiresAt := now.Add(-time.Second)

	got := tunnelBootstrapExpiryLabel(&client.APIKey{ExpiresAt: &expiresAt})
	if got != "expires in 1 hour" {
		t.Fatalf("tunnelBootstrapExpiryLabel(skewed key) = %q, want requested TTL fallback", got)
	}
}

func TestTunnelBootstrapExpiryLabelShowsExpiredOutsideSkew(t *testing.T) {
	now := time.Date(2026, 5, 27, 4, 30, 0, 0, time.UTC)
	freezeTunnelBootstrapNow(t, now)
	expiresAt := now.Add(-tunnelBootstrapSkew - time.Second)

	got := tunnelBootstrapExpiryLabel(&client.APIKey{ExpiresAt: &expiresAt})
	if got != "is expired" {
		t.Fatalf("tunnelBootstrapExpiryLabel(expired key) = %q, want expired", got)
	}
}

func TestSlackCodeBlockPanicsOnNestedFence(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("slackCodeBlock did not panic on nested fence")
		}
	}()
	_ = slackCodeBlock("echo before\n```inner\n```")
}

func TestSlackCodeBlock(t *testing.T) {
	t.Parallel()
	got := slackCodeBlock("echo ok")
	if strings.Count(got, "```") != 2 {
		t.Fatalf("slackCodeBlock fences = %q, want one opening and one closing fence", got)
	}
	if strings.Contains(got, testForbiddenSlackShellFence) {
		t.Fatalf("slackCodeBlock = %q, should not render a language suffix for Slack", got)
	}
}

func respondQURLEnvelope(t *testing.T, w http.ResponseWriter, data any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"data": data,
		"meta": map[string]string{"request_id": "req_test"},
	}); err != nil {
		t.Fatalf("encode qurl envelope: %v", err)
	}
}

func tunnelInstallModalValues(slug, shortcut, env, port, webContainer string) map[string]map[string]interactionStateValue {
	values := map[string]map[string]interactionStateValue{
		tunnelInstallBlockSlug: {
			tunnelInstallActionSlug: {Value: slug},
		},
		tunnelInstallBlockShortcut: {
			tunnelInstallActionShortcut: {Value: shortcut},
		},
		tunnelInstallBlockEnvironment: {
			tunnelInstallActionEnvironment: {SelectedOption: &interactionSelectedOption{Value: env}},
		},
		tunnelInstallBlockLocalPort: {
			tunnelInstallActionLocalPort: {Value: port},
		},
	}
	if webContainer != "" {
		values[tunnelInstallBlockWebRef] = map[string]interactionStateValue{
			tunnelInstallActionWebRef: {Value: webContainer},
		}
	}
	return values
}

func tunnelInstallViewSubmissionBody(t *testing.T, meta TunnelInstallModalMetadata, values map[string]map[string]interactionStateValue) string {
	t.Helper()
	return tunnelInstallViewSubmissionBodyWithIdentity(t, meta, meta.TeamID, meta.UserID, values)
}

func tunnelInstallViewSubmissionBodyWithIdentity(t *testing.T, meta TunnelInstallModalMetadata, payloadTeamID, payloadUserID string, values map[string]map[string]interactionStateValue) string {
	t.Helper()
	pm, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal private_metadata: %v", err)
	}
	payload, err := json.Marshal(map[string]any{
		testKeyType: "view_submission",
		"team":      map[string]any{"id": payloadTeamID},
		"user":      map[string]any{"id": payloadUserID},
		"view": map[string]any{
			"id":                         "V_test_tunnel",
			testFieldCallbackID:          callbackIDTunnelInstall,
			blockKitFieldPrivateMetadata: string(pm),
			"state":                      map[string]any{"values": values},
		},
	})
	if err != nil {
		t.Fatalf("marshal interaction payload: %v", err)
	}
	return url.Values{"payload": {string(payload)}}.Encode()
}
