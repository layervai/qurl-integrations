package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/layervai/qurl-integrations/apps/slack/internal/agent"
	"github.com/layervai/qurl-integrations/apps/slack/internal/slackdata"
	"github.com/layervai/qurl-integrations/shared/auth"
	"github.com/layervai/qurl-integrations/shared/client"
)

const (
	testTunnelSlug         = "prod-dashboard"
	testTunnelAliasDash    = "dash" // sample channel alias used across get/tunnel tests
	testTunnelResourceID   = "r_prod_dash01"
	testTunnelWizardCmd    = "protect-connector"                        // bare verb → guided modal
	testTunnelInstallCmd   = testTunnelWizardCmd + " " + testTunnelSlug // typed: `protect-connector prod-dashboard`
	testTunnelChannelID    = "C_test"
	testTunnelImageRef     = "ghcr.io/layervai/qurl-connector:v-test"
	testTunnelAPIKey       = "lv_live_test_bootstrap"
	testTunnelAPIKeyID     = "key_tunnel_bootstrap"
	testSlackResponseURL   = "https://hooks.slack.test/response"
	testAgentAuditTable    = "agent_state"
	testTunnelAgentReason  = "customer requested connector setup"
	testTunnelDockerLine   = `CONNECTOR_CONTAINER="qurl-connector-${QURL_CONNECTOR_ID}"`
	testTunnelModalKey     = "lv_live_modal_bootstrap"
	testTunnelPipefailLine = "set -o pipefail"
	testTunnelComposeWeb   = "web_1"
	testTunnelDockerWeb    = "web_1-2"
	testSlackTriggerID     = "trigger_test"
	testEnterpriseID       = "E_GRID"
)

type failingAuthProvider struct{ err error }

func (p failingAuthProvider) APIKey(context.Context, string) (string, error) {
	return "", p.err
}

type protectConnectorProposalLLM struct{}

func (protectConnectorProposalLLM) Complete(context.Context, *agent.Request) (agent.Response, error) {
	input, err := json.Marshal(map[string]any{
		"alias":  testTunnelAliasDash,
		"env":    string(tunnelEnvDocker),
		"port":   8080,
		"reason": testTunnelAgentReason,
	})
	if err != nil {
		return agent.Response{}, err
	}
	return agent.Response{
		ToolCalls: []agent.ToolCall{{
			ID:    "tool_protect_connector",
			Name:  "propose_protect_connector",
			Input: input,
		}},
		StopReason: "tool_use",
	}, nil
}

type unusedAgentReadBackend struct{}

func (unusedAgentReadBackend) ListResources(context.Context, *agent.TurnContext) (string, error) {
	return "", nil
}

func (unusedAgentReadBackend) ListAliases(context.Context, *agent.TurnContext) (string, error) {
	return "", nil
}

func (unusedAgentReadBackend) ResolveToken(context.Context, *agent.TurnContext, string) (string, error) {
	return "", nil
}

func (unusedAgentReadBackend) Quota(context.Context, *agent.TurnContext) (string, error) {
	return "", nil
}

type blockingPutAgentDDB struct {
	*memAgentDDB
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingPutAgentDDB() *blockingPutAgentDDB {
	return &blockingPutAgentDDB{
		memAgentDDB: newMemAgentDDB(),
		started:     make(chan struct{}),
		release:     make(chan struct{}),
	}
}

func (f *blockingPutAgentDDB) PutItem(ctx context.Context, in *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.once.Do(func() {
		close(f.started)
	})
	select {
	case <-f.release:
		return f.memAgentDDB.PutItem(ctx, in, optFns...)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func protectConnectorAgentMetadataFromToolCall(t *testing.T) *TunnelInstallAgentMetadata {
	t.Helper()

	res, _, err := agent.New(protectConnectorProposalLLM{}, unusedAgentReadBackend{}).Run(context.Background(), &agent.TurnContext{
		TeamID:        testAdminTeamID,
		ChannelID:     testTunnelChannelID,
		UserID:        testAdminUserID,
		CallerIsAdmin: true,
	}, nil, "protect the prod connector for customer setup")
	if err != nil {
		t.Fatalf("agent Run: %v", err)
	}
	if res.Proposal == nil {
		t.Fatalf("expected protect-connector proposal, got reply %q", res.Reply)
	}
	prop := res.Proposal
	if prop.Action != agent.ActionProtectConnector || prop.Reason != testTunnelAgentReason {
		t.Fatalf("proposal = %+v, want protect-connector reason %q", prop, testTunnelAgentReason)
	}
	return tunnelInstallAgentMetadata(&pendingAction{
		Action: prop.Action,
		Reason: prop.Reason,
	})
}

const (
	testForbiddenResourceLabel   = "Resource:"
	testForbiddenSlackYAMLFence  = "```yaml"
	testForbiddenSlackShellFence = "```sh"
	testForbiddenBootstrapArgv   = `printf '%s' "$QURL_BOOTSTRAP_KEY"`
	testTunnelAgentDirFragment   = `/var/lib/layerv/qurl-connector/${QURL_CONNECTOR_ID}/agent`
	testTunnelLocalPort9090Line  = "local_port: 9090"
	testTunnelKeyHistoryNote     = "prompts for the bootstrap key"
	testTunnelKeyPromptLine      = "Paste qURL bootstrap key (input hidden)"
	testTunnelKeyInstallLine     = `QURL_BOOTSTRAP_KEY_LEN=${#QURL_BOOTSTRAP_KEY}`
	testTunnelECSAPIKeyNameLine  = `"name": "QURL_API_KEY"`
)

func freezeTunnelBootstrapNow(t *testing.T, h *Handler, now time.Time) {
	t.Helper()
	previous := h.now
	h.now = func() time.Time { return now }
	t.Cleanup(func() { h.now = previous })
}

type tunnelDMPost struct {
	teamID       string
	enterpriseID string
	userID       string
	text         string
}

func captureTunnelPostDMSuccess(h *Handler) *[]tunnelDMPost {
	posts := []tunnelDMPost{}
	h.cfg.PostDM = func(_ context.Context, teamID, enterpriseID, userID, text string) error {
		posts = append(posts, tunnelDMPost{
			teamID:       teamID,
			enterpriseID: enterpriseID,
			userID:       userID,
			text:         text,
		})
		return nil
	}
	return &posts
}

func mustRenderDockerTunnelInstructions(t *testing.T, args *tunnelInstallArgs, image string) string {
	t.Helper()
	got, err := renderDockerTunnelInstructions(args, image)
	if err != nil {
		t.Fatalf("renderDockerTunnelInstructions: %v", err)
	}
	return got
}

func mustRenderDockerComposeTunnelInstructions(t *testing.T, args *tunnelInstallArgs, image string) string {
	t.Helper()
	got, err := renderDockerComposeTunnelInstructions(args, image)
	if err != nil {
		t.Fatalf("renderDockerComposeTunnelInstructions: %v", err)
	}
	return got
}

func mustRenderKubernetesTunnelInstructions(t *testing.T, args *tunnelInstallArgs, image string) string {
	t.Helper()
	got, err := renderKubernetesTunnelInstructions(args, image)
	if err != nil {
		t.Fatalf("renderKubernetesTunnelInstructions: %v", err)
	}
	return got
}

func mustRenderECSFargateTunnelInstructions(t *testing.T, args *tunnelInstallArgs, image string) string {
	t.Helper()
	got, err := renderECSFargateTunnelInstructions(args, image)
	if err != nil {
		t.Fatalf("renderECSFargateTunnelInstructions: %v", err)
	}
	return got
}

func TestRenderTunnelConfigYAMLUsesRouteID(t *testing.T) {
	got, err := renderTunnelConfigYAML(&tunnelInstallArgs{Slug: testTunnelSlug, LocalPort: 9090})
	if err != nil {
		t.Fatalf("renderTunnelConfigYAML: %v", err)
	}
	if !strings.Contains(got, "  - id: '"+testTunnelSlug+"'") {
		t.Fatalf("config missing route id:\n%s", got)
	}
	if strings.Contains(got, "  - name:") {
		t.Fatalf("config should not emit legacy route name:\n%s", got)
	}
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
		{name: "minimal", text: testTunnelInstallCmd, wantSlug: testTunnelSlug, wantAlias: testTunnelSlug, wantPort: defaultTunnelLocalPort, wantEnv: tunnelEnvDocker},
		{name: "slug with alias sigil", text: "protect-connector $" + testTunnelSlug, wantSlug: testTunnelSlug, wantAlias: testTunnelSlug, wantPort: defaultTunnelLocalPort},
		{name: "port and alias", text: testTunnelInstallCmd + " port:9090 alias:$dash", wantSlug: testTunnelSlug, wantAlias: testTunnelAliasDash, wantPort: 9090},
		{name: "alias without sigil", text: testTunnelInstallCmd + " alias:dash", wantSlug: testTunnelSlug, wantAlias: testTunnelAliasDash, wantPort: defaultTunnelLocalPort},
		{name: "docker environment", text: testTunnelInstallCmd + " env:docker", wantSlug: testTunnelSlug, wantAlias: testTunnelSlug, wantPort: defaultTunnelLocalPort, wantEnv: tunnelEnvDocker},
		{name: "environment", text: testTunnelInstallCmd + " env:ecs-fargate", wantSlug: testTunnelSlug, wantAlias: testTunnelSlug, wantPort: defaultTunnelLocalPort, wantEnv: tunnelEnvECSFargate},
		{name: "compose alias and service", text: testTunnelInstallCmd + " env:compose service:" + testTunnelComposeWeb, wantSlug: testTunnelSlug, wantAlias: testTunnelSlug, wantPort: defaultTunnelLocalPort, wantEnv: tunnelEnvCompose, wantWebRef: testTunnelComposeWeb},
		{name: "compose service before environment", text: testTunnelInstallCmd + " service:" + testTunnelComposeWeb + " env:compose", wantSlug: testTunnelSlug, wantAlias: testTunnelSlug, wantPort: defaultTunnelLocalPort, wantEnv: tunnelEnvCompose, wantWebRef: testTunnelComposeWeb},
		{name: "container ref", text: testTunnelInstallCmd + " container:" + testTunnelDockerWeb, wantSlug: testTunnelSlug, wantAlias: testTunnelSlug, wantPort: defaultTunnelLocalPort, wantWebRef: testTunnelDockerWeb},
		{name: "web container ref", text: testTunnelInstallCmd + " web_container:web", wantSlug: testTunnelSlug, wantAlias: testTunnelSlug, wantPort: defaultTunnelLocalPort, wantWebRef: "web"},
		{name: "bad slug uppercase", text: "protect-connector Prod", wantErr: true},
		{name: "empty slug after sigil", text: "protect-connector $", wantErr: true},
		{name: "double sigil slug", text: "protect-connector $$prod", wantErr: true},
		{name: "verb boundary", text: "tunnelhats install prod", wantErr: true},
		{name: "bad port", text: testTunnelInstallCmd + " port:70000", wantErr: true},
		{name: "empty environment option", text: testTunnelInstallCmd + " env:", wantErr: true},
		{name: "old docker vm spelling rejected", text: testTunnelInstallCmd + " env:docker-vm", wantErr: true},
		{name: "bad environment", text: testTunnelInstallCmd + " env:prod", wantErr: true},
		{name: "bad container ref", text: testTunnelInstallCmd + " container:../web", wantErr: true},
		{name: "container rejects semicolon", text: testTunnelInstallCmd + " container:web;rm", wantErr: true},
		{name: "container rejects expansion", text: testTunnelInstallCmd + " container:$WEB", wantErr: true},
		{name: "container rejects newline split", text: testTunnelInstallCmd + " container:web\nbad", wantErr: true},
		{name: "docker rejects service ref", text: testTunnelInstallCmd + " service:web", wantErr: true},
		{name: "kubernetes rejects container ref", text: testTunnelInstallCmd + " env:kubernetes container:web", wantErr: true},
		{name: "ecs rejects container ref", text: testTunnelInstallCmd + " env:ecs-fargate container:web", wantErr: true},
		{name: "compose rejects dotted service", text: testTunnelInstallCmd + " service:web.1 env:compose", wantErr: true},
		{name: "compose rejects slash service", text: testTunnelInstallCmd + " service:web/bad env:compose", wantErr: true},
		{name: "compose rejects dotted container ref", text: testTunnelInstallCmd + " env:compose container:web.1", wantErr: true},
		{name: "compose rejects container ref", text: testTunnelInstallCmd + " env:compose container:web", wantErr: true},
		{name: "empty alias option", text: testTunnelInstallCmd + " alias:", wantErr: true},
		{name: "empty alias after sigil", text: testTunnelInstallCmd + " alias:$", wantErr: true},
		{name: "alias rejects semicolon", text: testTunnelInstallCmd + " alias:$bad;rm", wantErr: true},
		{name: "slug rejects shell metacharacter", text: "protect-connector prod;bad", wantErr: true},
		{name: "slug rejects command substitution", text: "protect-connector prod$(whoami)", wantErr: true},
		{name: "slug rejects newline split", text: "protect-connector prod\nbad", wantErr: true},
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

func FuzzParseTunnelInstall(f *testing.F) {
	for _, seed := range []string{
		testTunnelInstallCmd,
		testTunnelInstallCmd + " port:9090 alias:$dash env:docker container:web",
		testTunnelInstallCmd + " env:compose service:web",
		testTunnelInstallCmd + " alias:$",
		"protect-connector prod$(whoami)",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 512 {
			return
		}
		_, _ = parseTunnelInstall(s)
	})
}

func FuzzParseTunnelInstallModalArgs(f *testing.F) {
	for _, seed := range []struct {
		slug     string
		shortcut string
		env      string
		port     string
		webRef   string
	}{
		{testTunnelSlug, "", string(tunnelEnvDocker), "8080", ""},
		{testTunnelSlug, "dash", string(tunnelEnvCompose), "9090", "web"},
		{"Upper", "$bad_alias", "bogus", "0", "../bad"},
		{"prod$(whoami)", strings.Repeat("a", aliasMaxLen+1), string(tunnelEnvKubernetes), "70000", "web.1"},
	} {
		f.Add(seed.slug, seed.shortcut, seed.env, seed.port, seed.webRef)
	}
	f.Fuzz(func(t *testing.T, slug, shortcut, env, port, webRef string) {
		if len(slug)+len(shortcut)+len(env)+len(port)+len(webRef) > 1024 {
			return
		}
		_, _ = parseTunnelInstallModalArgs(tunnelInstallModalValues(slug, shortcut, env, port, webRef))
	})
}

func TestTunnelWebRefKindValidationMessageMatrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		env     tunnelInstallEnvironment
		kind    tunnelInstallWebRefKind
		wantErr bool
	}{
		{name: "docker none", env: tunnelEnvDocker, kind: tunnelWebRefKindNone},
		{name: "docker container", env: tunnelEnvDocker, kind: tunnelWebRefKindContainer},
		{name: "docker service", env: tunnelEnvDocker, kind: tunnelWebRefKindService, wantErr: true},
		{name: "compose none", env: tunnelEnvCompose, kind: tunnelWebRefKindNone},
		{name: "compose service", env: tunnelEnvCompose, kind: tunnelWebRefKindService},
		{name: "compose container", env: tunnelEnvCompose, kind: tunnelWebRefKindContainer, wantErr: true},
		{name: "ecs container", env: tunnelEnvECSFargate, kind: tunnelWebRefKindContainer, wantErr: true},
		{name: "kubernetes container", env: tunnelEnvKubernetes, kind: tunnelWebRefKindContainer, wantErr: true},
		{name: "unknown container", env: tunnelInstallEnvironment("other"), kind: tunnelWebRefKindContainer, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg := tunnelWebRefKindValidationMessage(tc.env, tc.kind)
			if (msg != "") != tc.wantErr {
				t.Fatalf("tunnelWebRefKindValidationMessage(%q, %q) = %q, wantErr=%v", tc.env, tc.kind, msg, tc.wantErr)
			}
		})
	}
}

func TestDockerWebRefPatternsRejectShellMetacharacters(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"$WEB", "`cmd`", `"quoted"`, "web;rm", "web|cat", "web name", "../web", "web\nnext"} {
		t.Run(fmt.Sprintf("container/%q", input), func(t *testing.T) {
			t.Parallel()
			if dockerContainerRefPattern.MatchString(input) {
				t.Fatalf("dockerContainerRefPattern accepted %q", input)
			}
		})
		t.Run(fmt.Sprintf("compose/%q", input), func(t *testing.T) {
			t.Parallel()
			if dockerComposeServicePattern.MatchString(input) {
				t.Fatalf("dockerComposeServicePattern accepted %q", input)
			}
		})
	}
}

func TestInteractionStateLogValuesAllowlistsKnownNonSecretBlocks(t *testing.T) {
	t.Parallel()
	got := interactionStateLogValues(map[string]map[string]interactionStateValue{
		tunnelInstallBlockSlug: {
			tunnelInstallActionSlug: {Value: testTunnelSlug},
			"unexpected_action":     {Value: "should-not-log"},
		},
		tunnelInstallBlockEnvironment: {
			tunnelInstallActionEnvironment: {SelectedOption: &interactionSelectedOption{Value: string(tunnelEnvECSFargate)}},
		},
		"future_secret_block": {
			"secret_input": {Value: "lv_live_should_not_log"},
		},
	})
	if got[tunnelInstallBlockSlug][tunnelInstallActionSlug] != testTunnelSlug {
		t.Fatalf("known state value missing from log values: %#v", got)
	}
	if got[tunnelInstallBlockEnvironment][tunnelInstallActionEnvironment] != string(tunnelEnvECSFargate) {
		t.Fatalf("selected option state value missing from log values: %#v", got)
	}
	if _, ok := got[tunnelInstallBlockSlug]["unexpected_action"]; ok {
		t.Fatalf("unexpected known-block action logged: %#v", got)
	}
	if _, ok := got["future_secret_block"]; ok {
		t.Fatalf("unknown future block logged: %#v", got)
	}
}

func TestInteractionStateLogAllowlistCoversParsedTunnelInstallBlocks(t *testing.T) {
	t.Parallel()
	for blockID, actionID := range map[string]string{
		tunnelInstallBlockSlug:        tunnelInstallActionSlug,
		tunnelInstallBlockShortcut:    tunnelInstallActionShortcut,
		tunnelInstallBlockEnvironment: tunnelInstallActionEnvironment,
		tunnelInstallBlockLocalPort:   tunnelInstallActionLocalPort,
		tunnelInstallBlockWebRef:      tunnelInstallActionWebRef,
	} {
		actions, ok := interactionStateLogAllowlist[blockID]
		if !ok {
			t.Fatalf("interactionStateLogAllowlist missing block %q parsed by parseTunnelInstallModalArgs", blockID)
		}
		if _, ok := actions[actionID]; !ok {
			t.Fatalf("interactionStateLogAllowlist[%q] missing action %q parsed by parseTunnelInstallModalArgs", blockID, actionID)
		}
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
		{text: testTunnelInstallCmd, want: false},
		{text: "protect-connector prod", want: false},
		{text: "tunnel install", want: false},
		{text: "expose", want: false},
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
	now := fixedNow

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
			"name":            "Slack qURL Connector bootstrap " + testTunnelSlug,
			"scopes":          []string{tunnelScopeAgent, tunnelScopeWrite},
			testKeyStatus:     client.StatusActive,
			testKeyPurpose:    client.APIKeyPurposeTunnelBootstrap,
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
	status, ack := inv.invokeAdmin(testTunnelInstallCmd+" port:9090", testAdminTeamID, testAdminUserID)
	// The install message now posts as Block Kit (copyable rich_text snippets)
	// with the full text carried as the accessibility/notification fallback, so
	// the response_url body carries both `text` and `blocks` — decode into
	// map[string]any (a map[string]string unmarshal fails on the blocks array).
	var asyncEnvelope map[string]any
	if err := json.Unmarshal(inv.captured.waitForBody(t, 2*time.Second), &asyncEnvelope); err != nil {
		t.Fatalf("unmarshal protect-connector response_url body: %v", err)
	}
	async, _ := asyncEnvelope[respFieldText].(string)
	if asyncEnvelope[blockKitFieldBlocks] == nil {
		t.Errorf("protect-connector body missing blocks (expected Block Kit rendering)")
	}

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
	// Install seeds the description (which doubles as the Display Name) with
	// the install default, so every tunnel has a Display Name from creation;
	// admins refine it with `/qurl-admin set-display-name`.
	if got, want := resourceBody[testKeyDescription], defaultTunnelDisplayName(testTunnelSlug); got != want {
		t.Errorf("resource body description = %v, want install default %q", got, want)
	}
	if apiKeyBody[testKeyPurpose] != client.APIKeyPurposeTunnelBootstrap || apiKeyBody[testKeyTunnelSlug] != testTunnelSlug || apiKeyBody["expires_in"] != tunnelBootstrapTTL {
		t.Errorf("api key body = %+v, want constrained tunnel bootstrap key", apiKeyBody)
	}
	if idempotencyKey == "" {
		t.Error("Idempotency-Key header was empty")
	}
	wantIdempotencyKey := tunnelBootstrapIdempotencyKey(testAdminTeamID, testTunnelChannelID, testAdminUserID, testTunnelSlug, tunnelBootstrapTypedAttemptID(testSlackTriggerID, now))
	if idempotencyKey != wantIdempotencyKey {
		t.Fatalf("Idempotency-Key = %q, want %q", idempotencyKey, wantIdempotencyKey)
	}
	for _, want := range []string{
		"qURL Connector `" + testTunnelSlug + "` is ready to install.",
		"qURL alias `$" + testTunnelSlug + "` is ready in this channel.",
		"temporary bootstrap key expires in 1 hour and was sent separately by DM",
		"The install instructions below either prompt for it or reference your platform secret manager",
		"Paste the DM key only when prompted or into your secret manager",
		"Run this whole block on the Linux Docker host",
		testTunnelKeyHistoryNote,
		"set -eu",
		"Sidecar image: `" + testTunnelImageRef + "`.",
		testTunnelKeyPromptLine,
		"cat > \"$CONFIG_FILE\" <<'QURL_PROXY_YAML_EOF'",
		"QURL_CONNECTOR_ID='" + testTunnelSlug + "'",
		testTunnelKeyInstallLine,
		testTunnelLocalPort9090Line,
		"WEB_CONTAINER='YOUR_WEB_CONTAINER_NAME'",
		testTunnelDockerLine,
		`docker rm -f "$CONNECTOR_CONTAINER"`,
		`--network "container:${WEB_CONTAINER}"`,
		testTunnelAgentDirFragment,
		testTunnelImageRef,
		"Treat the separate bootstrap-key DM as secret",
		"Keep the qURL agent-state directory, volume, or PVC",
		"/qurl get $" + testTunnelSlug,
	} {
		if !strings.Contains(async, want) {
			t.Errorf("async reply missing %q:\n%s", want, async)
		}
	}
	for _, forbidden := range []string{testForbiddenResourceLabel, testTunnelResourceID, testTunnelAPIKey, "expires at", "`qurl-proxy.yaml`", testForbiddenSlackYAMLFence, testForbiddenSlackShellFence, "connect.layerv", "proxy.layerv", "frps-", "<web-container>", "QURL_CONNECTOR_SLUG"} {
		if strings.Contains(async, forbidden) {
			t.Errorf("async reply leaked %q:\n%s", forbidden, async)
		}
	}
	if len(*dmPosts) != 1 {
		t.Fatalf("PostDM calls = %d, want 1", len(*dmPosts))
	}
	dm := (*dmPosts)[0]
	if dm.teamID != testAdminTeamID || dm.userID != testAdminUserID {
		t.Fatalf("PostDM target = team %q user %q, want %q/%q", dm.teamID, dm.userID, testAdminTeamID, testAdminUserID)
	}
	for _, want := range []string{
		"Temporary qURL Connector bootstrap key for `" + testTunnelSlug + "` expires in 1 hour.",
		"install instructions were sent separately",
		"Delete this DM from Slack history",
		testTunnelAPIKey,
	} {
		if !strings.Contains(dm.text, want) {
			t.Errorf("bootstrap DM missing %q:\n%s", want, dm.text)
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

// TestTunnelInstallReinstallShowsExistingDisplayName pins the PR's core UX
// promise end-to-end: when find_or_create returns an EXISTING tunnel whose
// description (its Display Name) an admin already customized, the install
// confirmation renders that admin name — not defaultTunnelDisplayName. It
// guards the processTunnelInstall→render linkage (render is fed
// resource.Description, the server's value, not a locally-built default), so
// re-installing an admin-renamed tunnel never silently clobbers the name in
// the confirmation. (render's own show/hide-on-empty behavior is unit-tested
// by TestRenderTunnelInstall_ShowsDisplayNameOnReinstall.)
func TestTunnelInstallReinstallShowsExistingDisplayName(t *testing.T) {
	now := fixedNow
	const existingDisplayName = "Admin renamed prod gateway"

	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	ts.addCustomer(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
		// find_or_create returns the EXISTING resource, carrying the admin's
		// previously-set Display Name in description (not the install default).
		respondQURLEnvelope(t, w, map[string]any{
			testKeyResourceID:   testTunnelResourceID,
			testKeyType:         client.ResourceTypeTunnel,
			testKeySlug:         testTunnelSlug,
			testKeyStatus:       client.StatusActive,
			testKeyDescription:  existingDisplayName,
			"knock_resource_id": "qurl-tunnel-server",
		})
	})
	ts.addCustomer(http.MethodPost, "/v1/api-keys", func(w http.ResponseWriter, _ *http.Request) {
		respondQURLEnvelope(t, w, map[string]any{
			testKeyKeyID:      testTunnelAPIKeyID,
			testKeyAPIKey:     testTunnelAPIKey,
			"name":            "Slack qURL Connector bootstrap " + testTunnelSlug,
			"scopes":          []string{tunnelScopeAgent, tunnelScopeWrite},
			testKeyStatus:     client.StatusActive,
			testKeyPurpose:    client.APIKeyPurposeTunnelBootstrap,
			testKeyTunnelSlug: testTunnelSlug,
			testKeyExpiresAt:  now.Add(time.Hour).Format(time.RFC3339),
		})
	})

	h := newAdminTestHandler(t, ts)
	freezeTunnelBootstrapNow(t, h, now)
	h.cfg.TunnelImage = testTunnelImageRef
	captureTunnelPostDMSuccess(h)
	h.SetAliasStore(h.cfg.AdminStore)

	inv := newAdminSlashInvoker(t, h)
	if _, ack := inv.invokeAdmin(testTunnelInstallCmd+" port:9090", testAdminTeamID, testAdminUserID); !strings.Contains(ack, "Working") {
		t.Fatalf("ack = %q, want async working copy", ack)
	}
	var asyncEnvelope map[string]any
	if err := json.Unmarshal(inv.captured.waitForBody(t, 2*time.Second), &asyncEnvelope); err != nil {
		t.Fatalf("unmarshal protect-connector response_url body: %v", err)
	}
	async, _ := asyncEnvelope[respFieldText].(string)

	if !strings.Contains(async, existingDisplayName) {
		t.Errorf("re-install confirmation missing the admin's existing Display Name %q:\n%s", existingDisplayName, async)
	}
	if strings.Contains(async, defaultTunnelDisplayName(testTunnelSlug)) {
		t.Errorf("re-install confirmation rendered the install default instead of the admin's Display Name:\n%s", async)
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
	status, ack := inv.invokeAdmin("protect-connector", testAdminTeamID, testAdminUserID)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if ack != ackWorkingOnIt {
		t.Fatalf("ack = %q, want %q", ack, ackWorkingOnIt)
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
	if call.triggerID != testSlackTriggerID {
		t.Fatalf("trigger_id = %q, want %q", call.triggerID, testSlackTriggerID)
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
	for _, want := range []string{"Target channel", testTunnelChannelID, "qURL Connector ID", "Target environment", string(tunnelEnvCompose), string(tunnelEnvECSFargate), string(tunnelEnvKubernetes)} {
		if !strings.Contains(body, want) {
			t.Errorf("modal missing %q:\n%s", want, body)
		}
	}
	assertWizardAckReplaced(t, inv.captured.waitForBody(t, 2*time.Second), "Opened guided qURL Connector setup", "successful modal open")
}

func TestTunnelInstallBareFallsBackToEnterpriseInstallToken(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	adminChecks := make(chan string, 4)
	ts.ddb.SetGetItemHook(func(table string, key map[string]string) {
		if table == ts.tableNames.workspace {
			adminChecks <- key[fAttrSlackTeamID]
		}
	})
	type openViewCall struct {
		tokenOwnerID string
		triggerID    string
		view         []byte
	}
	calls := make(chan openViewCall, 2)
	unexpectedTokenOwner := make(chan string, 1)
	h.cfg.OpenView = func(_ context.Context, tokenOwnerID, triggerID string, viewJSON []byte) error {
		calls <- openViewCall{
			tokenOwnerID: tokenOwnerID,
			triggerID:    triggerID,
			view:         append([]byte(nil), viewJSON...),
		}
		if tokenOwnerID == testAdminTeamID {
			return fmt.Errorf("token lookup: %w", auth.ErrSlackBotTokenNotConfigured)
		}
		if tokenOwnerID != testEnterpriseID {
			unexpectedTokenOwner <- tokenOwnerID
			return fmt.Errorf("unexpected token lookup id %q", tokenOwnerID)
		}
		return nil
	}

	inv := newAdminSlashInvoker(t, h)
	inv.enterpriseID = testEnterpriseID
	status, ack := inv.invokeAdmin("protect-connector", testAdminTeamID, testAdminUserID)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if ack != ackWorkingOnIt {
		t.Fatalf("ack = %q, want %q", ack, ackWorkingOnIt)
	}
	var first, second openViewCall
	select {
	case first = <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("workspace OpenView lookup was not called")
	}
	select {
	case second = <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("enterprise OpenView fallback was not called")
	}
	if first.tokenOwnerID != testAdminTeamID || second.tokenOwnerID != testEnterpriseID {
		t.Fatalf("OpenView token lookup order = %q, %q; want team then enterprise", first.tokenOwnerID, second.tokenOwnerID)
	}
	select {
	case tokenOwnerID := <-unexpectedTokenOwner:
		t.Fatalf("unexpected token lookup id %q", tokenOwnerID)
	default:
	}
	select {
	case adminTeamID := <-adminChecks:
		if adminTeamID != testAdminTeamID {
			t.Fatalf("admin check teamID = %q, want workspace team %q", adminTeamID, testAdminTeamID)
		}
	default:
		t.Fatal("admin check was not recorded")
	}
	var modal map[string]any
	if err := json.Unmarshal(second.view, &modal); err != nil {
		t.Fatalf("modal JSON: %v", err)
	}
	pm, ok := modal[blockKitFieldPrivateMetadata].(string)
	if !ok || pm == "" {
		t.Fatalf("private_metadata = %T %q, want non-empty string", modal[blockKitFieldPrivateMetadata], modal[blockKitFieldPrivateMetadata])
	}
	var meta TunnelInstallModalMetadata
	if err := json.Unmarshal([]byte(pm), &meta); err != nil {
		t.Fatalf("private_metadata JSON: %v", err)
	}
	if meta.TeamID != testAdminTeamID {
		t.Fatalf("metadata TeamID = %q, want workspace team %q", meta.TeamID, testAdminTeamID)
	}
	assertWizardAckReplaced(t, inv.captured.waitForBody(t, 2*time.Second), "Opened guided qURL Connector setup", "enterprise-token modal open")
}

func TestTunnelInstallBareReportsInstallLinkWhenWorkspaceAndEnterpriseTokensMissing(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.SetSlackInstallURL("https://slack-bot.example/oauth/slack/install")
	calls := make(chan string, 2)
	h.cfg.OpenView = func(_ context.Context, tokenOwnerID, _ string, _ []byte) error {
		calls <- tokenOwnerID
		return fmt.Errorf("token lookup: %w", auth.ErrSlackBotTokenNotConfigured)
	}

	inv := newAdminSlashInvoker(t, h)
	inv.enterpriseID = testEnterpriseID
	status, ack := inv.invokeAdmin("protect-connector", testAdminTeamID, testAdminUserID)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(ack, "Working on it") {
		t.Fatalf("ack = %q, want immediate guided setup copy", ack)
	}
	var gotCalls []string
	for len(gotCalls) < 2 {
		select {
		case tokenOwnerID := <-calls:
			gotCalls = append(gotCalls, tokenOwnerID)
		case <-time.After(2 * time.Second):
			t.Fatalf("OpenView token lookups = %v, want workspace then enterprise", gotCalls)
		}
	}
	if strings.Join(gotCalls, ",") != testAdminTeamID+","+testEnterpriseID {
		t.Fatalf("OpenView token lookups = %v, want workspace then enterprise", gotCalls)
	}
	async := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
	for _, want := range []string{
		"latest qURL Slack app install",
		"<https://slack-bot.example/oauth/slack/install|the qURL Slack install link>",
		"/qurl-admin protect-connector",
	} {
		if !strings.Contains(async, want) {
			t.Fatalf("async reply = %q, missing %q", async, want)
		}
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
	for _, want := range []string{"/qurl-admin protect-connector`", "Guided connector setup", "/qurl-admin protect-connector <id>", "Typed connector options", "env:docker|docker-compose|ecs-fargate|kubernetes", "`env:compose` also works", "container:<name>", "service:<name>", "web_container:<name>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("/qurl help = %q, missing %q", got, want)
		}
	}
}

func TestTunnelInstallUsageSplitsTypedEnvironmentExamples(t *testing.T) {
	t.Parallel()

	got := tunnelInstallUsage()
	for _, want := range []string{
		"Guided setup is exactly `/qurl-admin protect-connector`",
		"• Docker:",
		"container:<name>|web_container:<name>",
		"• Compose:",
		"service:<name>",
		"• ECS/Fargate or Kubernetes:",
		"`env:compose` is accepted as shorthand",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("tunnelInstallUsage() = %q, missing %q", got, want)
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
		fieldText:      {"protect-connector"},
		fieldTeamID:    {testAdminTeamID},
		fieldUserID:    {testAdminUserID},
		fieldChannelID: {testTunnelChannelID},
	}
	w := httptest.NewRecorder()

	h.handleExposeConnector(w, values)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := parseSlackText(t, w.Body.Bytes())
	if !strings.Contains(got, "trigger_id") || !strings.Contains(got, "/qurl-admin protect-connector <id>") {
		t.Fatalf("response = %q, want trigger_id fallback guidance", got)
	}
}

func TestTunnelInstallBareWithoutOpenViewFallsBackToTypedInstall(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = nil
	values := url.Values{
		fieldText:      {"protect-connector"},
		fieldTeamID:    {testAdminTeamID},
		fieldUserID:    {testAdminUserID},
		fieldChannelID: {testTunnelChannelID},
		fieldTriggerID: {testSlackTriggerID},
	}
	w := httptest.NewRecorder()

	h.handleExposeConnector(w, values)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := parseSlackText(t, w.Body.Bytes())
	for _, want := range []string{"Guided qURL Connector setup is not configured", "/qurl-admin protect-connector <id>", "port:8080"} {
		if !strings.Contains(got, want) {
			t.Fatalf("response = %q, missing %q", got, want)
		}
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
	status, ack := inv.invokeAdmin("protect-connector", testAdminTeamID, testAdminUserID)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(ack, "Working on it") {
		t.Fatalf("ack = %q, want immediate guided setup copy", ack)
	}
	asyncBody := inv.captured.waitForBody(t, 2*time.Second)
	async := parseSlackText(t, asyncBody)
	if !strings.Contains(async, "Could not open guided qURL Connector setup") {
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
	status, ack := inv.invokeAdmin("protect-connector", testAdminTeamID, testAdminUserID)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(ack, "Working on it") {
		t.Fatalf("ack = %q, want immediate guided setup copy", ack)
	}
	async := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
	if !strings.Contains(async, "setup window expired") || !strings.Contains(async, "/qurl-admin protect-connector") {
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
	status, ack := inv.invokeAdmin("protect-connector", testAdminTeamID, testAdminUserID)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(ack, "Working on it") {
		t.Fatalf("ack = %q, want immediate guided setup copy", ack)
	}
	async := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
	if !strings.Contains(async, "Wait 2 seconds") || !strings.Contains(async, "/qurl-admin protect-connector") {
		t.Fatalf("async reply = %q, want retry-after guidance", async)
	}
}

func TestTunnelInstallBareReportsSlackInstallLinkWhenWorkspaceBotTokenMissing(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.SetSlackInstallURL("https://slack-bot.example/oauth/slack/install")
	h.cfg.OpenView = func(context.Context, string, string, []byte) error {
		return fmt.Errorf("token lookup: %w", auth.ErrSlackBotTokenNotConfigured)
	}

	inv := newAdminSlashInvoker(t, h)
	status, ack := inv.invokeAdmin("protect-connector", testAdminTeamID, testAdminUserID)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(ack, "Working on it") {
		t.Fatalf("ack = %q, want immediate guided setup copy", ack)
	}
	async := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
	for _, want := range []string{
		"latest qURL Slack app install",
		"<https://slack-bot.example/oauth/slack/install|the qURL Slack install link>",
		"/qurl-admin protect-connector",
	} {
		if !strings.Contains(async, want) {
			t.Fatalf("async reply = %q, missing %q", async, want)
		}
	}
	if strings.Contains(async, "operator provided") {
		t.Fatalf("async reply = %q, should include the configured install link", async)
	}
}

func TestTunnelInstallBareReportsOperatorFallbackWhenSlackInstallURLUnset(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.cfg.OpenView = func(context.Context, string, string, []byte) error {
		return fmt.Errorf("token lookup: %w", auth.ErrSlackBotTokenNotConfigured)
	}

	inv := newAdminSlashInvoker(t, h)
	status, ack := inv.invokeAdmin("protect-connector", testAdminTeamID, testAdminUserID)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(ack, "Working on it") {
		t.Fatalf("ack = %q, want immediate guided setup copy", ack)
	}
	async := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
	if !strings.Contains(async, "operator provided") {
		t.Fatalf("async reply = %q, want operator-provided fallback copy", async)
	}
	if strings.Contains(async, "|the qURL Slack install link>") {
		t.Fatalf("async reply = %q, should not render a Slack link without a configured install URL", async)
	}
}

func TestGuidedTunnelSlackAppInstallMessageRejectsMrkdwnBreakout(t *testing.T) {
	h := &Handler{cfg: Config{SlackInstallURL: "https://slack-bot.example/oauth/slack/install|bad"}}
	got := h.guidedTunnelSlackAppInstallMessage()

	if !strings.Contains(got, "operator provided") {
		t.Fatalf("message = %q, want operator-provided fallback copy", got)
	}
	if strings.Contains(got, "|the qURL Slack install link>") {
		t.Fatalf("message = %q, should not render a Slack link for malformed install URL", got)
	}
}

func TestSlackTriggerBudgetsFitWithinTriggerWindow(t *testing.T) {
	t.Parallel()

	if got := adminGateBudget + slackTriggerOpenViewBudget; got >= slackTriggerMaxAge {
		t.Fatalf("admin + views.open budgets = %s, want below Slack trigger max age %s", got, slackTriggerMaxAge)
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
	status, ack := inv.invokeAdmin("protect-connector", testAdminTeamID, testAdminUserID)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(ack, "Working on it") {
		t.Fatalf("ack = %q, want immediate guided setup copy", ack)
	}
	async := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
	if !strings.Contains(async, "Wait up to 5 minutes") || strings.Contains(async, "Wed, 21 Oct") {
		t.Fatalf("async reply = %q, want generic retry guidance without raw Retry-After", async)
	}
}

func TestSlackRetryAfterLabelCapsLargeValues(t *testing.T) {
	t.Parallel()

	if got := slackRetryAfterLabel("3600"); got != "at least 5 minutes" {
		t.Fatalf("slackRetryAfterLabel(3600) = %q, want capped copy", got)
	}
	if got := slackRetryAfterLabel("61"); got != "1 minute 1 second" {
		t.Fatalf("slackRetryAfterLabel(61) = %q, want friendly copy", got)
	}
}

func TestTunnelInstallBareSkipsOpenViewWhenTriggerWindowAlreadySpent(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	h.now = func() time.Time { return fixedNow.Add(slackTriggerMaxAge + time.Millisecond) }
	h.cfg.OpenView = func(context.Context, string, string, []byte) error {
		t.Fatal("OpenView should not be called after the trigger window is spent")
		return nil
	}
	inv := newAdminSlashInvoker(t, h)

	h.openTunnelInstallWizard(context.Background(), slog.Default(), testAdminTeamID, "", testTunnelChannelID, testAdminUserID, testSlackTriggerID, inv.responseU.URL, fixedNow)

	async := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
	if !strings.Contains(async, "setup window expired") || !strings.Contains(async, "/qurl-admin protect-connector") {
		t.Fatalf("async reply = %q, want trigger-expiry retry copy", async)
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
		status, ack := inv.invokeAdmin("protect-connector", testAdminTeamID, testAdminUserID)
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
		if !strings.Contains(got.ack, "Working on it") {
			t.Fatalf("ack = %q, want immediate guided setup copy", got.ack)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slash-command ack waited for views.open to finish")
	}
}

func TestTunnelInstallBareCancelsSlowOpenViewAtBudget(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	openViewStarted := make(chan struct{})
	openViewCanceled := make(chan error, 1)
	h.cfg.OpenView = func(ctx context.Context, _ string, _ string, _ []byte) error {
		close(openViewStarted)
		<-ctx.Done()
		openViewCanceled <- ctx.Err()
		return ctx.Err()
	}

	inv := newAdminSlashInvoker(t, h)
	status, ack := inv.invokeAdmin("protect-connector", testAdminTeamID, testAdminUserID)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !strings.Contains(ack, "Working on it") {
		t.Fatalf("ack = %q, want immediate guided setup copy", ack)
	}
	select {
	case <-openViewStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("OpenView did not start")
	}
	select {
	case err := <-openViewCanceled:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("OpenView cancel err = %v, want deadline exceeded", err)
		}
	case <-time.After(slackTriggerOpenViewBudget + time.Second):
		t.Fatal("OpenView context did not cancel within budget")
	}
	async := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
	if !strings.Contains(async, "Slack did not respond") || !strings.Contains(async, "/qurl-admin protect-connector") {
		t.Fatalf("async reply = %q, want deadline-expiry retry copy", async)
	}
}

func TestTunnelInstallModalSubmissionMintsKubernetesInstructions(t *testing.T) {
	now := fixedNow
	modalCreatedAt := now.Add(-10 * time.Minute)

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
			"name":            "Slack qURL Connector bootstrap " + testTunnelSlug,
			"scopes":          []string{tunnelScopeAgent, tunnelScopeWrite},
			testKeyStatus:     client.StatusActive,
			testKeyPurpose:    client.APIKeyPurposeTunnelBootstrap,
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
		CreatedAtUnix: modalCreatedAt.Unix(),
	}
	body := tunnelInstallViewSubmissionBody(t, &meta, map[string]map[string]interactionStateValue{
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
	if len(*dmPosts) != 1 || !strings.Contains((*dmPosts)[0].text, testTunnelModalKey) {
		t.Fatalf("bootstrap DM posts = %+v, want one containing modal key", *dmPosts)
	}
	if (*dmPosts)[0].enterpriseID != testEnterpriseID {
		t.Fatalf("bootstrap DM enterpriseID = %q, want %q", (*dmPosts)[0].enterpriseID, testEnterpriseID)
	}
	wantIdempotencyKey := tunnelBootstrapIdempotencyKey(testAdminTeamID, testTunnelChannelID, testAdminUserID, testTunnelSlug, tunnelBootstrapModalAttemptID("V_test_tunnel", modalCreatedAt))
	if idempotencyKey != wantIdempotencyKey {
		t.Fatalf("Idempotency-Key = %q, want %q", idempotencyKey, wantIdempotencyKey)
	}
	for _, want := range []string{
		"qURL Connector `" + testTunnelSlug + "` is ready to install.",
		"qURL alias `$team-dash` is ready in this channel.",
		"Target environment: Kubernetes.",
		"The install instructions below either prompt for it or reference your platform secret manager",
		"QURL_BOOTSTRAP_SECRET='qurl-connector-" + testTunnelSlug + "'",
		testTunnelPipefailLine,
		testTunnelKeyPromptLine,
		`kubectl create secret generic "$QURL_BOOTSTRAP_SECRET" --from-file=api_key=/dev/stdin`,
		"kubectl apply -f -",
		"kind: ConfigMap",
		"name: 'qurl-proxy-" + testTunnelSlug + "'",
		"kind: PersistentVolumeClaim",
		"Pod spec additions:",
		"Append the `qurl-connector` container under your existing `containers:` list",
		"fsGroup: 65532",
		"fsGroupChangePolicy: OnRootMismatch",
		"securityContext:",
		"runAsUser: 65532",
		"runAsNonRoot: true",
		"drop: [\"ALL\"]",
		"type: RuntimeDefault",
		"claimName: 'qurl-agent-" + testTunnelSlug + "'",
		"secretName: 'qurl-connector-" + testTunnelSlug + "'",
		"defaultMode: 0440",
		"QURL_CONNECTOR_ID",
		"value: '" + testTunnelSlug + "'",
		testTunnelLocalPort9090Line,
		testTunnelImageRef,
		"/qurl get $team-dash",
	} {
		if !strings.Contains(async, want) {
			t.Errorf("async reply missing %q:\n%s", want, async)
		}
	}
	for _, forbidden := range []string{testForbiddenResourceLabel, testTunnelResourceID, testTunnelModalKey, testForbiddenSlackYAMLFence, testForbiddenSlackShellFence, "connect.layerv", "proxy.layerv", "frps-", "initContainers:", "runAsUser: 0", "QURL_CONNECTOR_SLUG"} {
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
	now := fixedNow
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
				"docker logs -f qurl-connector-" + testTunnelSlug,
			},
		},
		{
			name: string(tunnelEnvCompose),
			env:  tunnelEnvCompose,
			web:  testTunnelComposeWeb,
			want: []string{
				"Target environment: Docker Compose.",
				"WEB_SERVICE='" + testTunnelComposeWeb + "'",
				"CONNECTOR_SERVICE='qurl-connector-" + testTunnelSlug + "'",
				"'qurl-connector-" + testTunnelSlug + "':",
				"docker compose -f compose.yaml -f qurl-connector-" + testTunnelSlug + ".compose.yaml logs -f qurl-connector-" + testTunnelSlug,
			},
		},
		{
			name: string(tunnelEnvECSFargate),
			env:  tunnelEnvECSFargate,
			want: []string{
				"Target environment: AWS ECS/Fargate.",
				ecsFargateChecklistText,
				ecsFargateRegionPlaceholderNote,
				testTunnelECSAPIKeyNameLine,
				`REPLACE_WITH_SECRET_ARN_FOR_QURL_CONNECTOR_` + testTunnelSlug,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
			freezeTunnelBootstrapNow(t, h, now)
			h.cfg.TunnelImage = testTunnelImageRef
			captureTunnelPostDMSuccess(h)
			h.SetAliasStore(h.cfg.AdminStore)
			inv := newAdminSlashInvoker(t, h)
			meta := TunnelInstallModalMetadata{
				TeamID:        testAdminTeamID,
				ChannelID:     testTunnelChannelID,
				UserID:        testAdminUserID,
				ResponseURL:   inv.responseU.URL,
				CreatedAtUnix: now.Unix(),
			}
			body := tunnelInstallViewSubmissionBody(t, &meta, tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tc.env), "9090", tc.web))
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

func TestTunnelInstallSubmissionAuditsOnlyAgentProtectConnector(t *testing.T) {
	cases := []struct {
		name      string
		agentMeta func(*testing.T) *TunnelInstallAgentMetadata
		wantAudit bool
	}{
		{
			name:      "agent initiated from tool call",
			agentMeta: protectConnectorAgentMetadataFromToolCall,
			wantAudit: true,
		},
		{
			name:      "slash initiated",
			agentMeta: func(*testing.T) *TunnelInstallAgentMetadata { return nil },
			wantAudit: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := fixedNow
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
			agentStore := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: testAgentAuditTable, Now: func() time.Time { return now }}
			h.cfg.AgentStore = agentStore
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
				Agent:         tc.agentMeta(t),
			}
			body := tunnelInstallViewSubmissionBody(t, &meta, tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvDocker), "8080", ""))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
			}
			async := parseSlackText(t, inv.captured.waitForBody(t, 2*time.Second))
			if strings.Contains(async, testTunnelModalKey) {
				t.Fatalf("async reply leaked bootstrap key after DM delivery split:\n%s", async)
			}
			if len(*dmPosts) != 1 || !strings.Contains((*dmPosts)[0].text, testTunnelModalKey) {
				t.Fatalf("bootstrap DM posts = %+v, want one containing modal key", *dmPosts)
			}
			h.Wait()

			got, err := agentStore.ListAuditEntries(context.Background(), testAdminTeamID, testAdminUserID, 10)
			if err != nil {
				t.Fatalf("list audit entries: %v", err)
			}
			if !tc.wantAudit {
				if len(got) != 0 {
					t.Fatalf("slash-initiated modal must not record agent audit, got %+v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("agent-initiated modal should record one audit entry, got %d: %+v", len(got), got)
			}
			entry := got[0]
			if entry.Actor != testAdminUserID || entry.Action != string(agent.ActionProtectConnector) || entry.Target != testTunnelSlug || entry.Channel != testTunnelChannelID {
				t.Fatalf("audit entry identity mismatch: %+v", entry)
			}
			if entry.Reason != testTunnelAgentReason {
				t.Fatalf("audit reason = %q, want modal provenance reason", entry.Reason)
			}
			if entry.Outcome == "" || strings.Contains(entry.Outcome, testTunnelModalKey) {
				t.Fatalf("audit outcome must be non-empty and must not store the bootstrap key: %+v", entry)
			}
			if entry.Result != agentProtectConnectorAuditOutcome {
				t.Fatalf("audit result = %q, want %q", entry.Result, agentProtectConnectorAuditOutcome)
			}
			if entry.ResultSuccess == nil || !*entry.ResultSuccess {
				t.Fatalf("audit result success = %v, want true", entry.ResultSuccess)
			}
		})
	}
}

func TestTunnelInstallAgentAuditFromMetadataTruncatesReason(t *testing.T) {
	longReason := strings.Repeat("r", agentConnectorAuditReasonMaxRunes+20)
	audit := tunnelInstallAgentAuditFromMetadata(&TunnelInstallModalMetadata{
		Agent: &TunnelInstallAgentMetadata{
			Action: string(agent.ActionProtectConnector),
			Reason: "  " + longReason + "  ",
		},
	}, &tunnelInstallArgs{Slug: testTunnelSlug})

	if audit == nil {
		t.Fatal("audit = nil, want protect-connector audit")
	}
	if audit.target != testTunnelSlug {
		t.Fatalf("target = %q, want submitted slug %q", audit.target, testTunnelSlug)
	}
	if got := len([]rune(audit.reason)); got != agentConnectorAuditReasonMaxRunes {
		t.Fatalf("reason runes = %d, want %d", got, agentConnectorAuditReasonMaxRunes)
	}
	if !strings.HasSuffix(audit.reason, "…") {
		t.Fatalf("reason = %q, want ellipsis suffix", audit.reason)
	}
}

func TestTunnelInstallAgentAuditFromMetadataSkipsUnexpectedAction(t *testing.T) {
	audit := tunnelInstallAgentAuditFromMetadata(&TunnelInstallModalMetadata{
		Agent: &TunnelInstallAgentMetadata{
			Action: string(agent.ActionProtectURL),
			Reason: testTunnelAgentReason,
		},
	}, &tunnelInstallArgs{Slug: testTunnelSlug})

	if audit != nil {
		t.Fatalf("audit = %+v, want nil for non-connector action", audit)
	}
}

func TestTunnelInstallModalRejectsUnsafeWebRefBeforeMintingKey(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	h.SetAliasStore(h.cfg.AdminStore)
	body := tunnelInstallViewSubmissionBody(t, &TunnelInstallModalMetadata{
		TeamID:        testAdminTeamID,
		ChannelID:     testTunnelChannelID,
		UserID:        testAdminUserID,
		ResponseURL:   testSlackResponseURL,
		CreatedAtUnix: fixedNow.Unix(),
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
		CreatedAtUnix: fixedNow.Unix(),
	}
	body := tunnelInstallViewSubmissionBodyWithIdentity(t, &meta, "", "", tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvDocker), "8080", ""))

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
		CreatedAtUnix: fixedNow.Unix(),
	}
	body := tunnelInstallViewSubmissionBodyWithIdentity(t, &meta, testAdminTeamID, "U_other_admin", tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvDocker), "8080", ""))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	for _, want := range []string{"Only the admin who opened this modal", "Run /qurl-admin protect-connector again"} {
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
		CreatedAtUnix: fixedNow.Unix(),
	}
	body := tunnelInstallViewSubmissionBody(t, &meta, tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvDocker), "8080", ""))

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
		CreatedAtUnix: fixedNow.Unix(),
	}
	body := tunnelInstallViewSubmissionBody(t, &meta, tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvDocker), "8080", ""))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Channel alias storage is not configured") {
		t.Fatalf("modal response = %s, want channel-shortcut store rejection", w.Body.String())
	}
}

func TestTunnelInstallModalRejectsNonAdminSubmitter(t *testing.T) {
	const nonAdminUserID = "U_non_admin"

	cases := []struct {
		name      string
		agentMeta *TunnelInstallAgentMetadata
		wantAudit bool
	}{
		{
			name:      "slash initiated",
			wantAudit: false,
		},
		{
			name: "agent initiated",
			agentMeta: &TunnelInstallAgentMetadata{
				Action: string(agent.ActionProtectConnector),
				Reason: testTunnelAgentReason,
			},
			wantAudit: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newAdminTestServers(t)
			ts.seedAdmin(t)

			agentStore := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: testAgentAuditTable, Now: func() time.Time { return fixedNow }}
			h := newAdminTestHandler(t, ts)
			h.cfg.AgentStore = agentStore
			h.SetAliasStore(h.cfg.AdminStore)
			meta := TunnelInstallModalMetadata{
				TeamID:        testAdminTeamID,
				ChannelID:     testTunnelChannelID,
				UserID:        nonAdminUserID,
				ResponseURL:   testSlackResponseURL,
				CreatedAtUnix: fixedNow.Unix(),
				Agent:         tc.agentMeta,
			}
			body := tunnelInstallViewSubmissionBodyWithIdentity(t, &meta, testAdminTeamID, nonAdminUserID, tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvDocker), "8080", ""))

			w := httptest.NewRecorder()
			h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "admin-only") {
				t.Fatalf("modal response = %s, want non-admin rejection", w.Body.String())
			}
			h.Wait()

			got, err := agentStore.ListAuditEntries(context.Background(), testAdminTeamID, nonAdminUserID, 10)
			if err != nil {
				t.Fatalf("list audit entries: %v", err)
			}
			if !tc.wantAudit {
				if len(got) != 0 {
					t.Fatalf("slash-initiated modal must not record agent audit, got %+v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("agent-initiated modal should record one audit entry, got %d: %+v", len(got), got)
			}
			entry := got[0]
			if entry.Actor != nonAdminUserID || entry.Action != string(agent.ActionProtectConnector) || entry.Target != testTunnelSlug || entry.Channel != testTunnelChannelID {
				t.Fatalf("audit entry identity mismatch: %+v", entry)
			}
			if entry.Reason != testTunnelAgentReason {
				t.Fatalf("audit reason = %q, want modal provenance reason", entry.Reason)
			}
			if entry.Outcome != agentProtectConnectorAuditAdminRejectedOutcome || entry.Result != agentProtectConnectorAuditAdminRejectedOutcome {
				t.Fatalf("audit outcome/result = %q/%q, want %q", entry.Outcome, entry.Result, agentProtectConnectorAuditAdminRejectedOutcome)
			}
			if entry.ResultSuccess == nil || *entry.ResultSuccess {
				t.Fatalf("audit result success = %v, want false", entry.ResultSuccess)
			}
		})
	}
}

func TestTunnelInstallModalNonAdminAgentAuditDoesNotDelayAck(t *testing.T) {
	const nonAdminUserID = "U_non_admin"

	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	ddb := newBlockingPutAgentDDB()
	releaseAudit := func() {
		select {
		case <-ddb.release:
		default:
			close(ddb.release)
		}
	}
	agentStore := &slackdata.AgentStore{Client: ddb, TableName: testAgentAuditTable, Now: func() time.Time { return fixedNow }}
	h := newAdminTestHandler(t, ts)
	h.cfg.AgentStore = agentStore
	h.SetAliasStore(h.cfg.AdminStore)
	defer func() {
		releaseAudit()
		h.Wait()
	}()

	meta := TunnelInstallModalMetadata{
		TeamID:        testAdminTeamID,
		ChannelID:     testTunnelChannelID,
		UserID:        nonAdminUserID,
		ResponseURL:   testSlackResponseURL,
		CreatedAtUnix: fixedNow.Unix(),
		Agent: &TunnelInstallAgentMetadata{
			Action: string(agent.ActionProtectConnector),
			Reason: testTunnelAgentReason,
		},
	}
	body := tunnelInstallViewSubmissionBodyWithIdentity(t, &meta, testAdminTeamID, nonAdminUserID, tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvDocker), "8080", ""))
	req := newSignedRequest(t, pathSlackInteractions, body, body)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		done <- w
	}()

	var w *httptest.ResponseRecorder
	select {
	case w = <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("modal response waited for rejected-path audit write")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "admin-only") {
		t.Fatalf("modal response = %s, want non-admin rejection", w.Body.String())
	}

	select {
	case <-ddb.started:
	case <-time.After(time.Second):
		t.Fatal("rejected-path audit write did not start")
	}
	releaseAudit()
	h.Wait()

	got, err := agentStore.ListAuditEntries(context.Background(), testAdminTeamID, nonAdminUserID, 10)
	if err != nil {
		t.Fatalf("list audit entries: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("agent-initiated modal should record one audit entry, got %d: %+v", len(got), got)
	}
	if got[0].Outcome != agentProtectConnectorAuditAdminRejectedOutcome || got[0].ResultSuccess == nil || *got[0].ResultSuccess {
		t.Fatalf("audit entry = %+v, want admin-rejected failure outcome", got[0])
	}
}

func TestTunnelInstallModalRejectsStaleSubmissionBeforeMintingKey(t *testing.T) {
	now := fixedNow

	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	freezeTunnelBootstrapNow(t, h, now)
	captureTunnelPostDMSuccess(h)
	h.SetAliasStore(h.cfg.AdminStore)
	meta := TunnelInstallModalMetadata{
		TeamID:        testAdminTeamID,
		ChannelID:     testTunnelChannelID,
		UserID:        testAdminUserID,
		ResponseURL:   testSlackResponseURL,
		CreatedAtUnix: now.Add(-tunnelInstallModalTTL - time.Minute).Unix(),
	}
	body := tunnelInstallViewSubmissionBody(t, &meta, tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvDocker), "8080", ""))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newSignedRequest(t, pathSlackInteractions, body, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "modal expired") {
		t.Fatalf("modal response = %s, want stale modal rejection", w.Body.String())
	}
}

func TestTunnelInstallModalRejectsFarFutureSubmissionBeforeMintingKey(t *testing.T) {
	now := fixedNow

	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	h := newAdminTestHandler(t, ts)
	freezeTunnelBootstrapNow(t, h, now)
	captureTunnelPostDMSuccess(h)
	h.SetAliasStore(h.cfg.AdminStore)
	meta := TunnelInstallModalMetadata{
		TeamID:        testAdminTeamID,
		ChannelID:     testTunnelChannelID,
		UserID:        testAdminUserID,
		ResponseURL:   testSlackResponseURL,
		CreatedAtUnix: now.Add(tunnelBootstrapSkew + time.Minute).Unix(),
	}
	body := tunnelInstallViewSubmissionBody(t, &meta, tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvDocker), "8080", ""))

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
	body := tunnelInstallViewSubmissionBody(t, &meta, tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvDocker), "8080", ""))

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

func TestParseTunnelInstallModalArgsReadsInitialEnvironmentSelection(t *testing.T) {
	t.Parallel()
	values := tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvKubernetes), "8080", "")

	args, fieldErrors := parseTunnelInstallModalArgs(values)

	if len(fieldErrors) != 0 {
		t.Fatalf("field errors = %+v, want none", fieldErrors)
	}
	if args.Environment != tunnelEnvKubernetes {
		t.Fatalf("environment = %q, want %q", args.Environment, tunnelEnvKubernetes)
	}
}

func TestInteractionStateValueTextPrefersDirectValue(t *testing.T) {
	t.Parallel()
	got := (interactionStateValue{
		Value:          string(tunnelEnvCompose),
		SelectedOption: &interactionSelectedOption{Value: string(tunnelEnvKubernetes)},
	}).text()

	if got != string(tunnelEnvCompose) {
		t.Fatalf("interactionStateValue.text() = %q, want direct value precedence", got)
	}
}

func TestInteractionStateValueTextUsesSelectedOption(t *testing.T) {
	t.Parallel()
	got := (interactionStateValue{
		SelectedOption: &interactionSelectedOption{Value: string(tunnelEnvKubernetes)},
	}).text()

	if got != string(tunnelEnvKubernetes) {
		t.Fatalf("interactionStateValue.text() = %q, want selected option value", got)
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

func TestParseTunnelInstallModalArgsSkipsWebRefValidationWhenEnvironmentInvalid(t *testing.T) {
	t.Parallel()
	values := tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, "bogus", "8080", "../bad")

	args, fieldErrors := parseTunnelInstallModalArgs(values)

	if args != nil {
		t.Fatalf("args = %+v, want nil", args)
	}
	if fieldErrors[tunnelInstallBlockEnvironment] == "" {
		t.Fatalf("field errors = %+v, want target environment error", fieldErrors)
	}
	if fieldErrors[tunnelInstallBlockWebRef] != "" {
		t.Fatalf("field errors = %+v, want web container validation deferred until environment is valid", fieldErrors)
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

func TestParseTunnelInstallModalArgsRejectsWebRefForTaskAndPodEnvironments(t *testing.T) {
	t.Parallel()
	for _, env := range []tunnelInstallEnvironment{tunnelEnvECSFargate, tunnelEnvKubernetes} {
		t.Run(string(env), func(t *testing.T) {
			t.Parallel()
			values := tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(env), "8080", "web")

			args, fieldErrors := parseTunnelInstallModalArgs(values)

			if args != nil {
				t.Fatalf("args = %+v, want nil", args)
			}
			got := fieldErrors[tunnelInstallBlockWebRef]
			if !strings.Contains(got, "Leave blank") || !strings.Contains(got, "same task or pod") {
				t.Fatalf("web_container error = %q, want ECS/Kubernetes blank-field guidance", got)
			}
		})
	}
}

func TestParseTunnelInstallModalArgsRejectsEmptyPort(t *testing.T) {
	t.Parallel()
	values := tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvDocker), "", "")

	args, fieldErrors := parseTunnelInstallModalArgs(values)

	if args != nil {
		t.Fatalf("args = %+v, want nil", args)
	}
	if fieldErrors[tunnelInstallBlockLocalPort] == "" {
		t.Fatalf("field errors = %+v, want local port error", fieldErrors)
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

func TestParseTunnelInstallModalArgsRejectsZeroPort(t *testing.T) {
	t.Parallel()
	values := tunnelInstallModalValues(testTunnelSlug, testTunnelSlug, string(tunnelEnvDocker), "0", "")

	args, fieldErrors := parseTunnelInstallModalArgs(values)

	if args != nil {
		t.Fatalf("args = %+v, want nil", args)
	}
	if got := fieldErrors[tunnelInstallBlockLocalPort]; !strings.Contains(got, "1 to 65535") {
		t.Fatalf("local port error = %q, want range copy", got)
	}
}

func TestParseTunnelInstallModalArgsRendersShortcutValidationReason(t *testing.T) {
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
			if !strings.Contains(got, tc.wantReason) || strings.Contains(got, "Usage:") || strings.Contains(got, "Alias") {
				t.Fatalf("shortcut error = %q, want shortcut-flavored %q without usage suffix", got, tc.wantReason)
			}
		})
	}
}

func TestValidateChannelShortcutTokenUsesShortcutCopy(t *testing.T) {
	t.Parallel()
	for _, token := range []string{"", "prod", "$", "$" + strings.Repeat("a", aliasMaxLen+1), "$bad_alias"} {
		t.Run(token, func(t *testing.T) {
			t.Parallel()
			_, reason := validateChannelShortcutToken(token)
			if reason == "" {
				t.Fatalf("validateChannelShortcutToken(%q) reason = empty, want rejection", token)
			}
			if strings.Contains(reason, "Alias ") {
				t.Fatalf("validateChannelShortcutToken(%q) reason = %q, want shortcut copy", token, reason)
			}
		})
	}
}

func TestRenderTunnelInstallMessageWarnsOnDefaultImage(t *testing.T) {
	now := fixedNow
	expiresAt := now.Add(time.Hour)

	h := NewHandler(Config{})
	freezeTunnelBootstrapNow(t, h, now)
	got, err := h.renderTunnelInstallMessage(&tunnelInstallArgs{
		Slug:        testTunnelSlug,
		Alias:       testTunnelSlug,
		LocalPort:   defaultTunnelLocalPort,
		Environment: tunnelEnvDocker,
	}, &client.APIKey{APIKey: testTunnelAPIKey, ExpiresAt: &expiresAt}, "qURL alias `$prod-dashboard` is ready in this channel.")
	if err != nil {
		t.Fatalf("renderTunnelInstallMessage: %v", err)
	}

	if !strings.Contains(got, ":warning: Image: using the dev/sandbox fallback") || !strings.Contains(got, defaultTunnelImage) {
		t.Fatalf("rendered install message missing fallback image warning:\n%s", got)
	}
	if !strings.Contains(got, "Sidecar image: `"+defaultTunnelImage+"`.") {
		t.Fatalf("rendered install message missing sidecar image audit line:\n%s", got)
	}
	imageIdx := strings.Index(got, "Image: using the dev/sandbox fallback")
	envIdx := strings.Index(got, "Target environment:")
	instructionsIdx := strings.Index(got, "Run this whole block")
	if imageIdx < 0 || envIdx < 0 || instructionsIdx < 0 || imageIdx > envIdx || envIdx > instructionsIdx {
		t.Fatalf("fallback image warning should appear before target environment and install block:\n%s", got)
	}
	if strings.Contains(got, testForbiddenResourceLabel) || strings.Contains(got, testTunnelResourceID) {
		t.Fatalf("rendered install message leaked resource details:\n%s", got)
	}
}

func TestRenderTunnelInstallMessageRejectsUnsafeBootstrapKey(t *testing.T) {
	t.Parallel()
	expiresAt := time.Date(2026, 5, 27, 5, 30, 0, 0, time.UTC)

	_, err := NewHandler(Config{}).renderTunnelInstallMessage(&tunnelInstallArgs{
		Slug:        testTunnelSlug,
		Alias:       testTunnelSlug,
		LocalPort:   defaultTunnelLocalPort,
		Environment: tunnelEnvDocker,
	}, &client.APIKey{APIKey: "lv_live_bad`key", ExpiresAt: &expiresAt}, "qURL alias `$prod-dashboard` is ready in this channel.")
	if err == nil || !strings.Contains(err.Error(), "unsupported characters") {
		t.Fatalf("renderTunnelInstallMessage err = %v, want unsupported-character rejection", err)
	}
}

// TestRenderTunnelInstall_ShowsDisplayNameOnReinstall fences the install
// confirmation's id line: it shows the tunnel's Display Name (resource
// description, always set in production) next to the id; the empty-guard
// case (defensive — a blank description) shows just the id with no
// dangling em-dash.
func TestRenderTunnelInstall_ShowsDisplayNameOnReinstall(t *testing.T) {
	now := fixedNow
	expiresAt := now.Add(time.Hour)
	args := &tunnelInstallArgs{
		Slug:        testTunnelSlug,
		Alias:       testTunnelSlug,
		LocalPort:   defaultTunnelLocalPort,
		Environment: tunnelEnvDocker,
	}
	h := NewHandler(Config{TunnelImage: testTunnelImageRef})
	freezeTunnelBootstrapNow(t, h, now)
	prepared, err := h.prepareTunnelInstallMessage(args)
	if err != nil {
		t.Fatalf("prepareTunnelInstallMessage: %v", err)
	}
	key := &client.APIKey{APIKey: testTunnelAPIKey, ExpiresAt: &expiresAt}
	const aliasStatus = "qURL alias `$prod-dashboard` is ready in this channel."

	withName, err := prepared.render(args, key, aliasStatus, "Prod <!channel> <@U123> & *gateway*", now)
	if err != nil {
		t.Fatalf("render with Display Name: %v", err)
	}
	if !strings.Contains(withName, "qURL Connector `"+testTunnelSlug+"` — Prod &lt;!channel&gt; &lt;@U123&gt; &amp; ∗gateway∗ is ready to install.") {
		t.Errorf("install confirmation missing Display Name on id line:\n%s", withName)
	}
	if strings.Contains(withName, "<!channel>") || strings.Contains(withName, "<@U123>") {
		t.Errorf("install confirmation rendered raw Slack control sequence:\n%s", withName)
	}

	withoutName, err := prepared.render(args, key, aliasStatus, "", now)
	if err != nil {
		t.Fatalf("render without Display Name: %v", err)
	}
	if !strings.Contains(withoutName, "qURL Connector `"+testTunnelSlug+"` is ready to install.") {
		t.Errorf("install confirmation should show a bare id line when no Display Name is set:\n%s", withoutName)
	}
	if strings.Contains(withoutName, "—") {
		t.Errorf("install confirmation should not render an em-dash with no Display Name:\n%s", withoutName)
	}
}

func TestYAMLSingleQuotedRejectsControlsAndNewlines(t *testing.T) {
	t.Parallel()
	cases := []string{"bad\nvalue", "bad\rvalue", "bad\x00value", "bad\x7fvalue", "bad\u2028value", "badévalue"}
	for _, input := range cases {
		t.Run(fmt.Sprintf("%q", input), func(t *testing.T) {
			t.Parallel()
			if got, err := yamlSingleQuoted(input); err == nil {
				t.Fatalf("yamlSingleQuoted(%q) = %q, want error", input, got)
			}
		})
	}
}

func TestRenderedInstallShellBlocksParseAfterValidatedInputs(t *testing.T) {
	t.Parallel()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	const renderShellTestSlug = "prod-dash-1"
	cases := []struct {
		name   string
		render func(*testing.T) string
	}{
		{
			name: "docker",
			render: func(t *testing.T) string {
				return mustRenderDockerTunnelInstructions(t, &tunnelInstallArgs{
					Slug:        renderShellTestSlug,
					Alias:       renderShellTestSlug,
					LocalPort:   9090,
					Environment: tunnelEnvDocker,
					WebRef:      "web.1_2-3",
				}, testTunnelImageRef)
			},
		},
		{
			name: string(tunnelEnvCompose),
			render: func(t *testing.T) string {
				return mustRenderDockerComposeTunnelInstructions(t, &tunnelInstallArgs{
					Slug:        renderShellTestSlug,
					Alias:       renderShellTestSlug,
					LocalPort:   9090,
					Environment: tunnelEnvCompose,
					WebRef:      "web_1-2",
				}, testTunnelImageRef)
			},
		},
		{
			name: "kubernetes",
			render: func(t *testing.T) string {
				return mustRenderKubernetesTunnelInstructions(t, &tunnelInstallArgs{
					Slug:        renderShellTestSlug,
					Alias:       renderShellTestSlug,
					LocalPort:   9090,
					Environment: tunnelEnvKubernetes,
				}, testTunnelImageRef)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, sh, "-n")
			cmd.Stdin = strings.NewReader(firstSlackCodeBlock(t, tc.render(t)))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s shell block did not parse: %v\n%s", tc.name, err, out)
			}
		})
	}
}

func firstSlackCodeBlock(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, "```\n")
	if start < 0 {
		t.Fatalf("missing Slack code block:\n%s", body)
	}
	start += len("```\n")
	end := strings.Index(body[start:], "\n```")
	if end < 0 {
		t.Fatalf("missing Slack code block terminator:\n%s", body)
	}
	return body[start : start+end]
}

func TestValidateTunnelImageRefRejectsBackticks(t *testing.T) {
	t.Parallel()

	err := ValidateTunnelImageRef("ghcr.io/layervai/qurl```bad")

	if err == nil || !strings.Contains(err.Error(), "backticks") {
		t.Fatalf("ValidateTunnelImageRef error = %v, want backtick rejection", err)
	}
}

func TestValidateTunnelImageRefRejectsShellSyntaxBytes(t *testing.T) {
	t.Parallel()
	badTag := "ghcr.io/layervai/qurl-connector:bad"
	for i, image := range []string{
		badTag + " tag",
		badTag + "$tag",
		badTag + "'tag",
		badTag + "\"tag",
		badTag + "\ntag",
		badTag + "\x00tag",
	} {
		t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
			t.Parallel()
			if err := ValidateTunnelImageRef(image); err == nil {
				t.Fatalf("ValidateTunnelImageRef(%q) = nil, want rejection", image)
			}
		})
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
	captureTunnelPostDMSuccess(h)
	h.SetAliasStore(h.cfg.AdminStore)
	_, _, async := newAdminSlashInvoker(t, h).invokeAdminAsync(testTunnelInstallCmd, testAdminTeamID, testAdminUserID)

	if !strings.Contains(async, "did not return a bootstrap key") {
		t.Fatalf("async reply = %q, want missing-plaintext copy", async)
	}
	if revokeHits != 1 {
		t.Fatalf("bootstrap key revoke hits = %d, want 1", revokeHits)
	}
}

func TestTunnelInstallRefusesWhenPostDMUnwiredBeforeMintingKey(t *testing.T) {
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
	_, _, async := newAdminSlashInvoker(t, h).invokeAdminAsync(testTunnelInstallCmd, testAdminTeamID, testAdminUserID)

	if !strings.Contains(async, "No bootstrap key was minted") || !strings.Contains(async, "Slack DM delivery") {
		t.Fatalf("async reply = %q, want DM-unwired pre-mint refusal", async)
	}
	if resourceHits != 0 || apiKeyHits != 0 {
		t.Fatalf("resource/api-key hits = %d/%d, want 0/0 before DM wiring", resourceHits, apiKeyHits)
	}
}

func TestRevokeBootstrapKeyAfterInstallFailureNoopsWithoutKeyID(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	c := client.New("http://127.0.0.1", "unused", client.WithRetry(0))
	revokeBootstrapKeyAfterInstallFailure(context.Background(), log, c, &client.APIKey{}, "missing_key_id")
	if !strings.Contains(logs.String(), "missing_key_id") || !strings.Contains(logs.String(), "missing key_id") {
		t.Fatalf("log = %q, want missing-key-id warning", logs.String())
	}
}

func TestRevokeBootstrapKeyAfterInstallFailureLogsRevokeError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/api-keys/key_cleanup_failed" {
			t.Fatalf("request = %s %s, want DELETE /v1/api-keys/key_cleanup_failed", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"title":"cleanup failed","status":500}}`))
	}))
	t.Cleanup(server.Close)

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	c := client.New(server.URL, "unused", client.WithRetry(0))

	revokeBootstrapKeyAfterInstallFailure(context.Background(), log, c, &client.APIKey{KeyID: "key_cleanup_failed"}, "render_failed")

	if got := logs.String(); !strings.Contains(got, "cleanup failed") || !strings.Contains(got, "render_failed") {
		t.Fatalf("log = %q, want cleanup error and reason", got)
	}
}

func TestRevokeBootstrapKeyAfterInstallFailureTreatsNotFoundAsBenign(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1/api-keys/key_already_absent" {
			t.Fatalf("request = %s %s, want DELETE /v1/api-keys/key_already_absent", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"title":"not found","status":404}}`))
	}))
	t.Cleanup(server.Close)

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	c := client.New(server.URL, "unused", client.WithRetry(0))

	revokeBootstrapKeyAfterInstallFailure(context.Background(), log, c, &client.APIKey{KeyID: "key_already_absent"}, "response_url_delivery_failed")

	got := logs.String()
	if !strings.Contains(got, "already absent") || !strings.Contains(got, "response_url_delivery_failed") {
		t.Fatalf("log = %q, want benign already-absent cleanup log", got)
	}
	if strings.Contains(got, "cleanup failed") {
		t.Fatalf("log = %q, 404 should not be logged as cleanup failed", got)
	}
}

func TestRevokeBootstrapKeyAfterInstallFailureHonorsParentCancellation(t *testing.T) {
	t.Parallel()

	hits := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	c := client.New(server.URL, "unused", client.WithRetry(0))
	parent, cancel := context.WithCancel(context.Background())
	cancel()

	revokeBootstrapKeyAfterInstallFailure(parent, log, c, &client.APIKey{KeyID: "key_shutdown"}, "shutdown")

	select {
	case <-hits:
		t.Fatal("revoke request reached server despite canceled parent context")
	default:
	}
	if got := logs.String(); !strings.Contains(got, context.Canceled.Error()) || !strings.Contains(got, "shutdown") {
		t.Fatalf("log = %q, want parent cancellation and reason", got)
	}
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
	captureTunnelPostDMSuccess(h)
	h.SetAliasStore(h.cfg.AdminStore)
	_, _, async := newAdminSlashInvoker(t, h).invokeAdminAsync(testTunnelInstallCmd, testAdminTeamID, testAdminUserID)

	if !strings.Contains(async, "unexpected format") {
		t.Fatalf("async reply = %q, want shell-validation copy", async)
	}
	if revokeHits != 1 {
		t.Fatalf("bootstrap key revoke hits = %d, want 1", revokeHits)
	}
}

func TestTunnelInstallRevokesBootstrapKeyWhenSlackFollowupFails(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	now := fixedNow

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
			testKeyAPIKey:     testTunnelAPIKey,
			testKeyPurpose:    client.APIKeyPurposeTunnelBootstrap,
			testKeyTunnelSlug: testTunnelSlug,
		})
	})
	ts.addCustomer(http.MethodDelete, "/v1/api-keys/"+testTunnelAPIKeyID, func(w http.ResponseWriter, _ *http.Request) {
		revokeHits++
		w.WriteHeader(http.StatusNoContent)
	})
	processCtx, cancelProcess := context.WithCancel(context.Background())
	defer cancelProcess()
	var responseBodies []string
	responseURL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read response_url body: %v", err)
		}
		responseBodies = append(responseBodies, string(body))
		if len(responseBodies) == 3 {
			cancelProcess()
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(responseURL.Close)

	h := newAdminTestHandler(t, ts)
	agentStore := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: testAgentAuditTable, Now: func() time.Time { return now }}
	h.cfg.AgentStore = agentStore
	h.cfg.TunnelImage = testTunnelImageRef
	dmPosts := captureTunnelPostDMSuccess(h)
	h.SetAliasStore(h.cfg.AdminStore)
	h.processTunnelInstall(processCtx, slog.Default(), &tunnelInstallRequest{
		teamID:       testAdminTeamID,
		enterpriseID: "",
		channelID:    testTunnelChannelID,
		userID:       testAdminUserID,
		responseURL:  responseURL.URL,
		args: &tunnelInstallArgs{
			Slug:        testTunnelSlug,
			Alias:       testTunnelSlug,
			LocalPort:   defaultTunnelLocalPort,
			Environment: tunnelEnvDocker,
		},
		attemptID: tunnelBootstrapTimeAttemptID("test-attempt", now),
		agentAudit: &tunnelInstallAgentAudit{
			target: testTunnelSlug,
			reason: testTunnelAgentReason,
		},
	})

	if revokeHits != 1 {
		t.Fatalf("bootstrap key revoke hits = %d, want 1", revokeHits)
	}
	// When Slack rejects every response_url post, the delivery sequence is: the
	// Block Kit install post, the plain-text fallback, the plain-text retry, then
	// the revoked-key discard notice. The bootstrap key was delivered only by DM
	// and is revoked because the install instructions were never confirmed.
	if len(responseBodies) != 4 {
		t.Fatalf("response_url posts = %d, want 4 (blocks attempt, text fallback, text retry, discard notice): %v", len(responseBodies), responseBodies)
	}
	if strings.Contains(strings.Join(responseBodies, "\n"), testTunnelAPIKey) {
		t.Fatalf("response_url bodies leaked bootstrap key: %v", responseBodies)
	}
	last := responseBodies[len(responseBodies)-1]
	if !strings.Contains(last, "bootstrap key was revoked") || !strings.Contains(last, "discard it") {
		t.Fatalf("last response_url body = %q, want revoked-key discard follow-up", last)
	}
	if len(*dmPosts) != 2 {
		t.Fatalf("bootstrap DM posts = %d, want key DM and discard notice: %+v", len(*dmPosts), *dmPosts)
	}
	if !strings.Contains((*dmPosts)[0].text, testTunnelAPIKey) {
		t.Fatalf("first DM = %q, want bootstrap key", (*dmPosts)[0].text)
	}
	if strings.Contains((*dmPosts)[1].text, testTunnelAPIKey) || !strings.Contains((*dmPosts)[1].text, "was revoked") || !strings.Contains((*dmPosts)[1].text, "Discard that key") {
		t.Fatalf("second DM = %q, want discard notice without key", (*dmPosts)[1].text)
	}
	got, err := agentStore.ListAuditEntries(context.Background(), testAdminTeamID, testAdminUserID, 10)
	if err != nil {
		t.Fatalf("list audit entries: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("agent-initiated delivery failure should record one audit entry, got %d: %+v", len(got), got)
	}
	if got[0].Outcome != agentProtectConnectorAuditInstructionsDeliveryFailedOutcome {
		t.Fatalf("audit outcome = %q, want %q", got[0].Outcome, agentProtectConnectorAuditInstructionsDeliveryFailedOutcome)
	}
	if got[0].Result != agentProtectConnectorAuditInstructionsDeliveryFailedOutcome {
		t.Fatalf("audit result = %q, want %q", got[0].Result, agentProtectConnectorAuditInstructionsDeliveryFailedOutcome)
	}
	if got[0].ResultSuccess == nil || *got[0].ResultSuccess {
		t.Fatalf("audit result success = %v, want false", got[0].ResultSuccess)
	}
	if strings.Contains(got[0].Outcome, testTunnelAPIKey) {
		t.Fatalf("audit outcome must not store the bootstrap key: %+v", got[0])
	}
}

func TestTunnelInstallAgentAuditWriteFailureDoesNotBlockInstall(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)
	now := fixedNow

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
		})
	})

	var responseBodies []string
	responseURL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read response_url body: %v", err)
		}
		responseBodies = append(responseBodies, string(body))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(responseURL.Close)

	h := newAdminTestHandler(t, ts)
	h.cfg.AgentStore = &slackdata.AgentStore{
		Client:    &memAgentDDB{items: map[string]map[string]ddbtypes.AttributeValue{}, putErr: errors.New("audit store unavailable")},
		TableName: testAgentAuditTable,
		Now:       func() time.Time { return now },
	}
	h.cfg.TunnelImage = testTunnelImageRef
	dmPosts := captureTunnelPostDMSuccess(h)
	h.SetAliasStore(h.cfg.AdminStore)
	h.processTunnelInstall(context.Background(), slog.Default(), &tunnelInstallRequest{
		teamID:       testAdminTeamID,
		enterpriseID: "",
		channelID:    testTunnelChannelID,
		userID:       testAdminUserID,
		responseURL:  responseURL.URL,
		args: &tunnelInstallArgs{
			Slug:        testTunnelSlug,
			Alias:       testTunnelSlug,
			LocalPort:   defaultTunnelLocalPort,
			Environment: tunnelEnvDocker,
		},
		attemptID: tunnelBootstrapTimeAttemptID("test-attempt", now),
		agentAudit: &tunnelInstallAgentAudit{
			target: testTunnelSlug,
			reason: testTunnelAgentReason,
		},
	})

	if len(responseBodies) == 0 {
		t.Fatal("response_url posts = 0, want install instructions despite audit write failure")
	}
	if strings.Contains(strings.Join(responseBodies, "\n"), testTunnelAPIKey) {
		t.Fatalf("response_url posts leaked bootstrap key despite DM delivery split: %v", responseBodies)
	}
	if len(*dmPosts) != 1 || !strings.Contains((*dmPosts)[0].text, testTunnelAPIKey) {
		t.Fatalf("bootstrap DM posts = %+v, want delivered key despite audit write failure", *dmPosts)
	}
}

func TestTunnelInstallAgentAuditUsesBackgroundWhenBaseContextNil(t *testing.T) {
	now := fixedNow
	agentStore := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: testAgentAuditTable, Now: func() time.Time { return now }}
	h := &Handler{cfg: Config{AgentStore: agentStore}}
	h.recordTunnelInstallAgentAudit(slog.Default(), &tunnelInstallRequest{
		teamID:    testAdminTeamID,
		channelID: testTunnelChannelID,
		userID:    testAdminUserID,
		agentAudit: &tunnelInstallAgentAudit{
			target: testTunnelSlug,
			reason: testTunnelAgentReason,
		},
	}, agentProtectConnectorAuditOutcome, true)

	got, err := agentStore.ListAuditEntries(context.Background(), testAdminTeamID, testAdminUserID, 10)
	if err != nil {
		t.Fatalf("list audit entries: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(got))
	}
	if got[0].Target != testTunnelSlug || got[0].Reason != testTunnelAgentReason {
		t.Fatalf("audit entry = %+v, want target/reason from request audit", got[0])
	}
}

func TestTunnelInstallAgentAuditRecordsOnlyAgentBuildFailure(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(*testing.T, *adminTestServers, *Handler)
		agentAudit *tunnelInstallAgentAudit
		wantAudit  bool
	}{
		{
			name: "agent initiated resource create failure",
			setup: func(t *testing.T, ts *adminTestServers, _ *Handler) {
				t.Helper()
				ts.addCustomer(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
					writeAPIError(t, w, http.StatusInternalServerError, "upstream_error", "resource create failed")
				})
			},
			agentAudit: &tunnelInstallAgentAudit{
				target: testTunnelSlug,
				reason: testTunnelAgentReason,
			},
			wantAudit: true,
		},
		{
			name: "slash initiated resource create failure",
			setup: func(t *testing.T, ts *adminTestServers, _ *Handler) {
				t.Helper()
				ts.addCustomer(http.MethodPost, "/v1/resources", func(w http.ResponseWriter, _ *http.Request) {
					writeAPIError(t, w, http.StatusInternalServerError, "upstream_error", "resource create failed")
				})
			},
			agentAudit: nil,
			wantAudit:  false,
		},
		{
			name: "agent initiated auth recheck failure",
			setup: func(_ *testing.T, _ *adminTestServers, h *Handler) {
				h.cfg.AuthProvider = failingAuthProvider{err: errors.New("auth unavailable")}
			},
			agentAudit: &tunnelInstallAgentAudit{
				target: testTunnelSlug,
				reason: testTunnelAgentReason,
			},
			wantAudit: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newAdminTestServers(t)
			ts.seedAdmin(t)
			now := fixedNow

			var responseBodies []string
			responseURL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read response_url body: %v", err)
				}
				responseBodies = append(responseBodies, string(body))
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(responseURL.Close)

			h := newAdminTestHandler(t, ts)
			agentStore := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: testAgentAuditTable, Now: func() time.Time { return now }}
			h.cfg.AgentStore = agentStore
			captureTunnelPostDMSuccess(h)
			h.SetAliasStore(h.cfg.AdminStore)
			tc.setup(t, ts, h)
			h.processTunnelInstall(context.Background(), slog.Default(), &tunnelInstallRequest{
				teamID:       testAdminTeamID,
				enterpriseID: "",
				channelID:    testTunnelChannelID,
				userID:       testAdminUserID,
				responseURL:  responseURL.URL,
				args: &tunnelInstallArgs{
					Slug:        testTunnelSlug,
					Alias:       testTunnelSlug,
					LocalPort:   defaultTunnelLocalPort,
					Environment: tunnelEnvDocker,
				},
				attemptID:  tunnelBootstrapTimeAttemptID("test-attempt", now),
				agentAudit: tc.agentAudit,
			})

			if len(responseBodies) != 1 {
				t.Fatalf("response_url posts = %d, want build-failure message: %v", len(responseBodies), responseBodies)
			}
			got, err := agentStore.ListAuditEntries(context.Background(), testAdminTeamID, testAdminUserID, 10)
			if err != nil {
				t.Fatalf("list audit entries: %v", err)
			}
			if !tc.wantAudit {
				if len(got) != 0 {
					t.Fatalf("slash-initiated build failure must not record agent audit, got %+v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("agent-initiated build failure should record one audit entry, got %d: %+v", len(got), got)
			}
			if got[0].Outcome != agentProtectConnectorAuditBuildFailedOutcome {
				t.Fatalf("audit outcome = %q, want %q", got[0].Outcome, agentProtectConnectorAuditBuildFailedOutcome)
			}
			if got[0].Result != agentProtectConnectorAuditBuildFailedOutcome {
				t.Fatalf("audit result = %q, want %q", got[0].Result, agentProtectConnectorAuditBuildFailedOutcome)
			}
			if got[0].ResultSuccess == nil || *got[0].ResultSuccess {
				t.Fatalf("audit result success = %v, want false", got[0].ResultSuccess)
			}
			if strings.Contains(got[0].Outcome, testTunnelAPIKey) {
				t.Fatalf("audit outcome must not store the bootstrap key: %+v", got[0])
			}
		})
	}
}

func TestTunnelInstallRetriesTransientTextDeliveryBeforeRevoking(t *testing.T) {
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
		})
	})
	var revokeHits int
	ts.addCustomer(http.MethodDelete, "/v1/api-keys/"+testTunnelAPIKeyID, func(w http.ResponseWriter, _ *http.Request) {
		revokeHits++
		w.WriteHeader(http.StatusNoContent)
	})

	bodyHasBlocks := func(body string) bool {
		var env map[string]any
		_ = json.Unmarshal([]byte(body), &env)
		return env[blockKitFieldBlocks] != nil
	}
	var posts []string
	var textHits int
	responseURL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read response_url body: %v", err)
		}
		bodyText := string(body)
		posts = append(posts, bodyText)
		if bodyHasBlocks(bodyText) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		textHits++
		if textHits == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(responseURL.Close)

	h := newAdminTestHandler(t, ts)
	h.cfg.TunnelImage = testTunnelImageRef
	dmPosts := captureTunnelPostDMSuccess(h)
	h.SetAliasStore(h.cfg.AdminStore)
	h.processTunnelInstall(context.Background(), slog.Default(), &tunnelInstallRequest{
		teamID:       testAdminTeamID,
		enterpriseID: "",
		channelID:    testTunnelChannelID,
		userID:       testAdminUserID,
		responseURL:  responseURL.URL,
		args: &tunnelInstallArgs{
			Slug:        testTunnelSlug,
			Alias:       testTunnelSlug,
			LocalPort:   defaultTunnelLocalPort,
			Environment: tunnelEnvDocker,
		},
		attemptID: tunnelBootstrapTimeAttemptID("test-attempt", h.now()),
	})

	if revokeHits != 0 {
		t.Fatalf("bootstrap key revoke hits = %d, want 0 after text retry delivered the install", revokeHits)
	}
	if len(posts) != 3 {
		t.Fatalf("response_url posts = %d, want 3 (rejected blocks, failed text, delivered text retry): %v", len(posts), posts)
	}
	if !bodyHasBlocks(posts[0]) {
		t.Errorf("first post should be the Block Kit attempt: %s", posts[0])
	}
	if bodyHasBlocks(posts[1]) || bodyHasBlocks(posts[2]) {
		t.Errorf("text fallback and retry should not carry blocks: posts=%v", posts)
	}
	if strings.Contains(strings.Join(posts, "\n"), testTunnelAPIKey) {
		t.Errorf("response_url posts leaked the bootstrap key: posts=%v", posts)
	}
	if len(*dmPosts) != 1 || !strings.Contains((*dmPosts)[0].text, testTunnelAPIKey) {
		t.Fatalf("bootstrap-key DM posts = %+v, want one post carrying the key", *dmPosts)
	}
}

func TestTunnelInstallRevokesBootstrapKeyWhenDMSendFails(t *testing.T) {
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
			testKeyAPIKey:     testTunnelAPIKey,
			testKeyPurpose:    client.APIKeyPurposeTunnelBootstrap,
			testKeyTunnelSlug: testTunnelSlug,
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
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(responseURL.Close)

	h := newAdminTestHandler(t, ts)
	agentStore := &slackdata.AgentStore{Client: newMemAgentDDB(), TableName: testAgentAuditTable}
	h.cfg.AgentStore = agentStore
	h.cfg.TunnelImage = testTunnelImageRef
	h.cfg.PostDM = func(context.Context, string, string, string, string) error {
		return errors.New("dm unavailable")
	}
	h.SetAliasStore(h.cfg.AdminStore)
	h.processTunnelInstall(context.Background(), slog.Default(), &tunnelInstallRequest{
		teamID:       testAdminTeamID,
		enterpriseID: "",
		channelID:    testTunnelChannelID,
		userID:       testAdminUserID,
		responseURL:  responseURL.URL,
		args: &tunnelInstallArgs{
			Slug:        testTunnelSlug,
			Alias:       testTunnelSlug,
			LocalPort:   defaultTunnelLocalPort,
			Environment: tunnelEnvDocker,
		},
		attemptID: tunnelBootstrapTimeAttemptID("test-attempt", h.now()),
		agentAudit: &tunnelInstallAgentAudit{
			target: testTunnelSlug,
			reason: testTunnelAgentReason,
		},
	})

	if revokeHits != 1 {
		t.Fatalf("bootstrap key revoke hits = %d, want 1", revokeHits)
	}
	if len(responseBodies) != 1 {
		t.Fatalf("response_url posts = %d, want 1 failure notice: %v", len(responseBodies), responseBodies)
	}
	failure := parseSlackText(t, []byte(responseBodies[0]))
	for _, forbidden := range []string{testTunnelAPIKey, "Run this whole block", "docker run -d"} {
		if strings.Contains(failure, forbidden) {
			t.Fatalf("DM-failure notice leaked install secret/details %q: %s", forbidden, failure)
		}
	}
	if !strings.Contains(failure, "could not deliver") || !strings.Contains(failure, "temporary key was revoked") {
		t.Fatalf("failure notice = %s, want DM failure and revoke copy", failure)
	}
	got, err := agentStore.ListAuditEntries(context.Background(), testAdminTeamID, testAdminUserID, 10)
	if err != nil {
		t.Fatalf("list audit entries: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("agent-initiated DM failure should record one audit entry, got %d: %+v", len(got), got)
	}
	if got[0].Outcome != agentProtectConnectorAuditBootstrapDMDeliveryFailedOutcome {
		t.Fatalf("audit outcome = %q, want %q", got[0].Outcome, agentProtectConnectorAuditBootstrapDMDeliveryFailedOutcome)
	}
	if got[0].Result != agentProtectConnectorAuditBootstrapDMDeliveryFailedOutcome {
		t.Fatalf("audit result = %q, want %q", got[0].Result, agentProtectConnectorAuditBootstrapDMDeliveryFailedOutcome)
	}
	if got[0].ResultSuccess == nil || *got[0].ResultSuccess {
		t.Fatalf("audit result success = %v, want false", got[0].ResultSuccess)
	}
}

func TestTunnelInstallMissingScopeDMFailureMentionsSlackReinstall(t *testing.T) {
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
			testKeyAPIKey:     testTunnelAPIKey,
			testKeyPurpose:    client.APIKeyPurposeTunnelBootstrap,
			testKeyTunnelSlug: testTunnelSlug,
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
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(responseURL.Close)

	h := newAdminTestHandler(t, ts)
	h.SetSlackInstallURL("https://slack-bot.example/oauth/slack/install")
	h.cfg.TunnelImage = testTunnelImageRef
	h.cfg.PostDM = func(context.Context, string, string, string, string) error {
		return fmt.Errorf("chat.postMessage: %w", ErrSlackMissingScope)
	}
	h.SetAliasStore(h.cfg.AdminStore)
	h.processTunnelInstall(context.Background(), slog.Default(), &tunnelInstallRequest{
		teamID:       testAdminTeamID,
		enterpriseID: "",
		channelID:    testTunnelChannelID,
		userID:       testAdminUserID,
		responseURL:  responseURL.URL,
		args: &tunnelInstallArgs{
			Slug:        testTunnelSlug,
			Alias:       testTunnelSlug,
			LocalPort:   defaultTunnelLocalPort,
			Environment: tunnelEnvDocker,
		},
		attemptID: tunnelBootstrapTimeAttemptID("test-attempt", h.now()),
	})

	if revokeHits != 1 {
		t.Fatalf("bootstrap key revoke hits = %d, want 1", revokeHits)
	}
	if len(responseBodies) != 1 {
		t.Fatalf("response_url posts = %d, want 1 failure notice: %v", len(responseBodies), responseBodies)
	}
	failure := parseSlackText(t, []byte(responseBodies[0]))
	for _, want := range []string{
		"temporary key was revoked",
		"latest qURL Slack app install",
		"<https://slack-bot.example/oauth/slack/install|the qURL Slack install link>",
		"/qurl-admin protect-connector",
	} {
		if !strings.Contains(failure, want) {
			t.Fatalf("failure notice = %s, missing %q", failure, want)
		}
	}
	for _, forbidden := range []string{testTunnelAPIKey, "Run this whole block", "docker run -d"} {
		if strings.Contains(failure, forbidden) {
			t.Fatalf("missing-scope notice leaked install secret/details %q: %s", forbidden, failure)
		}
	}
}

func TestTunnelInstallRetryAfterDMRevokeUsesFreshIdempotencyKey(t *testing.T) {
	ts := newAdminTestServers(t)
	ts.seedAdmin(t)

	var apiKeyHits, revokeHits int
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
		apiKeyHits++
		idempotencyKeys = append(idempotencyKeys, r.Header.Get(client.HeaderIdempotencyKey))
		keyID := fmt.Sprintf("key_retry_%d", apiKeyHits)
		respondQURLEnvelope(t, w, map[string]any{
			testKeyKeyID:      keyID,
			testKeyAPIKey:     fmt.Sprintf("lv_live_retry_bootstrap_%d", apiKeyHits),
			testKeyPurpose:    client.APIKeyPurposeTunnelBootstrap,
			testKeyTunnelSlug: testTunnelSlug,
		})
	})
	ts.addCustomer(http.MethodDelete, "/v1/api-keys/key_retry_1", func(w http.ResponseWriter, _ *http.Request) {
		revokeHits++
		w.WriteHeader(http.StatusNoContent)
	})

	responseURL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(responseURL.Close)

	h := newAdminTestHandler(t, ts)
	h.SetSlackInstallURL("https://slack-bot.example/oauth/slack/install")
	h.cfg.TunnelImage = testTunnelImageRef
	var dmCalls int
	var dmTexts []string
	h.cfg.PostDM = func(_ context.Context, _, _, _, text string) error {
		dmCalls++
		if dmCalls == 1 {
			return fmt.Errorf("chat.postMessage: %w", ErrSlackMissingScope)
		}
		dmTexts = append(dmTexts, text)
		return nil
	}
	h.SetAliasStore(h.cfg.AdminStore)

	firstAttempt := fixedNow
	secondAttempt := firstAttempt.Add(time.Second)
	args := &tunnelInstallArgs{
		Slug:        testTunnelSlug,
		Alias:       testTunnelSlug,
		LocalPort:   defaultTunnelLocalPort,
		Environment: tunnelEnvDocker,
	}
	h.processTunnelInstall(context.Background(), slog.Default(), &tunnelInstallRequest{
		teamID:       testAdminTeamID,
		enterpriseID: "",
		channelID:    testTunnelChannelID,
		userID:       testAdminUserID,
		responseURL:  responseURL.URL,
		args:         args,
		attemptID:    tunnelBootstrapTimeAttemptID("test-attempt", firstAttempt),
	})
	h.processTunnelInstall(context.Background(), slog.Default(), &tunnelInstallRequest{
		teamID:       testAdminTeamID,
		enterpriseID: "",
		channelID:    testTunnelChannelID,
		userID:       testAdminUserID,
		responseURL:  responseURL.URL,
		args:         args,
		attemptID:    tunnelBootstrapTimeAttemptID("test-attempt", secondAttempt),
	})

	if revokeHits != 1 {
		t.Fatalf("bootstrap key revoke hits = %d, want 1", revokeHits)
	}
	if len(idempotencyKeys) != 2 || idempotencyKeys[0] == "" || idempotencyKeys[1] == "" {
		t.Fatalf("idempotency keys = %v, want two non-empty keys", idempotencyKeys)
	}
	if idempotencyKeys[0] == idempotencyKeys[1] {
		t.Fatalf("retry reused revoked-key idempotency key %q", idempotencyKeys[0])
	}
	if len(dmTexts) != 1 || !strings.Contains(dmTexts[0], "lv_live_retry_bootstrap_2") {
		t.Fatalf("successful retry DMs = %+v, want fresh second bootstrap key", dmTexts)
	}
}

// TestTunnelInstallFallsBackToTextWhenBlocksRejected fences the delivery safety
// net: if Slack rejects the Block Kit install post (e.g. an over-large
// rich_text payload) but accepts a plain-text post, postInstallInstructions
// retries as text, the install is delivered, and the bootstrap key is NOT
// revoked. This is the property that lets the rich_text rendering be a
// best-effort enhancement layered over the always-safe plain-text post.
func TestTunnelInstallFallsBackToTextWhenBlocksRejected(t *testing.T) {
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
		})
	})
	var revokeHits int
	ts.addCustomer(http.MethodDelete, "/v1/api-keys/"+testTunnelAPIKeyID, func(w http.ResponseWriter, _ *http.Request) {
		revokeHits++
		w.WriteHeader(http.StatusNoContent)
	})

	// bodyHasBlocks decodes a response_url payload and reports whether it
	// carries a Block Kit `blocks` array — a structural check rather than a
	// substring match on the JSON key spelling. Pure (no t.Fatalf), so it is
	// safe to call from the server goroutine below.
	bodyHasBlocks := func(body string) bool {
		var env map[string]any
		_ = json.Unmarshal([]byte(body), &env)
		return env[blockKitFieldBlocks] != nil
	}
	var posts []string
	responseURL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read response_url body: %v", err)
		}
		posts = append(posts, string(body))
		// Reject the Block Kit post (carries a blocks array); accept the
		// plain-text retry.
		if bodyHasBlocks(string(body)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(responseURL.Close)

	h := newAdminTestHandler(t, ts)
	h.cfg.TunnelImage = testTunnelImageRef
	captureTunnelPostDMSuccess(h)
	h.SetAliasStore(h.cfg.AdminStore)
	h.processTunnelInstall(context.Background(), slog.Default(), &tunnelInstallRequest{
		teamID:       testAdminTeamID,
		enterpriseID: "",
		channelID:    testTunnelChannelID,
		userID:       testAdminUserID,
		responseURL:  responseURL.URL,
		args: &tunnelInstallArgs{
			Slug:        testTunnelSlug,
			Alias:       testTunnelSlug,
			LocalPort:   defaultTunnelLocalPort,
			Environment: tunnelEnvDocker,
		},
		attemptID: tunnelBootstrapTimeAttemptID("test-attempt", h.now()),
	})

	if revokeHits != 0 {
		t.Fatalf("bootstrap key revoke hits = %d, want 0 (plain-text fallback delivered the install)", revokeHits)
	}
	if len(posts) != 2 {
		t.Fatalf("response_url posts = %d, want 2 (rejected blocks, then accepted text): %v", len(posts), posts)
	}
	if !bodyHasBlocks(posts[0]) {
		t.Errorf("first post should be the Block Kit attempt: %s", posts[0])
	}
	if bodyHasBlocks(posts[1]) {
		t.Errorf("second post (text retry) should not carry blocks: %s", posts[1])
	}
	if strings.Contains(posts[1], testTunnelAPIKey) {
		t.Errorf("text retry leaked the bootstrap key: %s", posts[1])
	}
}

func TestTunnelBootstrapIdempotencyKeyUsesExactAttemptTime(t *testing.T) {
	t.Parallel()

	setupStartedAt := fixedNow
	attemptID := tunnelBootstrapTimeAttemptID("test-attempt", setupStartedAt)
	first := tunnelBootstrapIdempotencyKey(testAdminTeamID, testTunnelChannelID, testAdminUserID, testTunnelSlug, attemptID)
	sameAttempt := tunnelBootstrapIdempotencyKey(testAdminTeamID, testTunnelChannelID, testAdminUserID, testTunnelSlug, attemptID)
	if first == "" || first != sameAttempt {
		t.Fatalf("first key = %q, same-attempt key = %q, want same non-empty key", first, sameAttempt)
	}
	nextAttempt := tunnelBootstrapIdempotencyKey(testAdminTeamID, testTunnelChannelID, testAdminUserID, testTunnelSlug, tunnelBootstrapTimeAttemptID("test-attempt", setupStartedAt.Add(time.Second)))
	if nextAttempt == first {
		t.Fatalf("next-attempt key matched first key %q", first)
	}
	triggerAttempt := tunnelBootstrapTypedAttemptID(testSlackTriggerID, setupStartedAt)
	triggerRetry := tunnelBootstrapTypedAttemptID(testSlackTriggerID, setupStartedAt.Add(time.Second))
	if triggerAttempt != triggerRetry {
		t.Fatalf("same trigger attempt IDs differed: %q vs %q", triggerAttempt, triggerRetry)
	}
}

func TestTunnelInstallRetryRemintsWhenAliasAlreadyMatches(t *testing.T) {
	now := fixedNow

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
	freezeTunnelBootstrapNow(t, h, now)
	dmPosts := captureTunnelPostDMSuccess(h)
	h.SetAliasStore(h.cfg.AdminStore)

	_, _, first := newAdminSlashInvoker(t, h).invokeAdminAsync(testTunnelInstallCmd, testAdminTeamID, testAdminUserID)
	if !strings.Contains(first, "Failed to mint") {
		t.Fatalf("first async reply = %q, want mint failure", first)
	}
	_, _, second := newAdminSlashInvoker(t, h).invokeAdminAsync(testTunnelInstallCmd, testAdminTeamID, testAdminUserID)
	if strings.Contains(second, "lv_live_retry_bootstrap") || !strings.Contains(second, "qURL alias `$"+testTunnelSlug+"` is ready in this channel.") {
		t.Fatalf("second async reply = %q, want successful remint against existing alias", second)
	}
	if len(*dmPosts) != 1 || !strings.Contains((*dmPosts)[0].text, "lv_live_retry_bootstrap") {
		t.Fatalf("bootstrap DM posts = %+v, want one containing reminted key", *dmPosts)
	}
	if apiKeyHits != 2 {
		t.Fatalf("api key hits = %d, want 2", apiKeyHits)
	}
	if len(idempotencyKeys) != 2 || idempotencyKeys[0] == "" || idempotencyKeys[0] != idempotencyKeys[1] {
		t.Fatalf("idempotency keys = %v, want same non-empty retry key", idempotencyKeys)
	}
	nextAttemptKey := tunnelBootstrapIdempotencyKey(testAdminTeamID, testTunnelChannelID, testAdminUserID, testTunnelSlug, tunnelBootstrapTimeAttemptID("started", now.Add(time.Second)))
	if nextAttemptKey == idempotencyKeys[0] {
		t.Fatal("next-attempt tunnel bootstrap idempotency key matched current-attempt key")
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
	if !strings.Contains(status, "qURL alias `$"+testTunnelSlug+"` is ready in this channel.") {
		t.Fatalf("status = %q, want idempotent ready copy", status)
	}
}

func TestTunnelInstallTypedEnvironmentInstructions(t *testing.T) {
	now := fixedNow

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
				ecsFargateChecklistText,
				testTunnelECSAPIKeyNameLine,
			},
		},
		{
			name: "kubernetes",
			env:  string(tunnelEnvKubernetes),
			want: []string{
				"Target environment: Kubernetes.",
				"kubectl apply -f -",
				"Pod spec additions:",
				"Do not duplicate existing YAML keys.",
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
			freezeTunnelBootstrapNow(t, h, now)
			h.cfg.TunnelImage = testTunnelImageRef
			captureTunnelPostDMSuccess(h)
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
			if strings.Contains(async, testForbiddenResourceLabel) || strings.Contains(async, testTunnelResourceID) || strings.Contains(async, testTunnelAPIKey) {
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
	captureTunnelPostDMSuccess(h)
	h.SetAliasStore(h.cfg.AdminStore)
	_, _, async := newAdminSlashInvoker(t, h).invokeAdminAsync(testTunnelInstallCmd, testAdminTeamID, testAdminUserID)

	if !strings.Contains(async, "qURL alias") || !strings.Contains(async, "already used") {
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
		{name: "dollar rejected", key: "abc$def", wantErr: true},
		{name: "backslash rejected", key: `abc\def`, wantErr: true},
		{name: "newline rejected", key: "abc\ndef", wantErr: true},
		{name: "del rejected", key: "abc\x7fdef", wantErr: true},
		{name: "non ascii rejected", key: "abcédef", wantErr: true},
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
		{ttl: "59m30s", want: "1 hour"},
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
	now := fixedNow
	expiresAt := now.Add(-time.Second)

	got := tunnelBootstrapExpiryLabel(&client.APIKey{ExpiresAt: &expiresAt}, now)
	if got != "expires very soon" {
		t.Fatalf("tunnelBootstrapExpiryLabel(skewed key) = %q, want near-expiry copy", got)
	}
}

func TestTunnelBootstrapExpiryLabelShowsExpiredOutsideSkew(t *testing.T) {
	now := fixedNow
	expiresAt := now.Add(-tunnelBootstrapSkew - time.Second)

	got := tunnelBootstrapExpiryLabel(&client.APIKey{ExpiresAt: &expiresAt}, now)
	if got != "is expired" {
		t.Fatalf("tunnelBootstrapExpiryLabel(expired key) = %q, want expired", got)
	}
}

func TestSlackCodeBlockRejectsNestedFence(t *testing.T) {
	t.Parallel()
	if _, err := slackCodeBlock("echo before\n```inner\n```"); err == nil {
		t.Fatal("slackCodeBlock returned nil error for nested fence")
	}
}

func TestSlackCodeBlock(t *testing.T) {
	t.Parallel()
	got, err := slackCodeBlock("echo ok")
	if err != nil {
		t.Fatalf("slackCodeBlock: %v", err)
	}
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

func tunnelInstallViewSubmissionBody(t *testing.T, meta *TunnelInstallModalMetadata, values map[string]map[string]interactionStateValue) string {
	t.Helper()
	return tunnelInstallViewSubmissionBodyWithIdentity(t, meta, meta.TeamID, meta.UserID, values)
}

func tunnelInstallViewSubmissionBodyWithIdentity(t *testing.T, meta *TunnelInstallModalMetadata, payloadTeamID, payloadUserID string, values map[string]map[string]interactionStateValue) string {
	t.Helper()
	pm, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal private_metadata: %v", err)
	}
	return viewSubmissionBody(t, "V_test_tunnel", callbackIDTunnelInstall, string(pm), payloadTeamID, payloadUserID, values)
}
