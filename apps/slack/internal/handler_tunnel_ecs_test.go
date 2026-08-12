package internal

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestRenderECSFargateTunnelInstructions(t *testing.T) {
	t.Parallel()
	got := mustRenderECSFargateTunnelInstructions(t, &tunnelInstallArgs{
		Slug:               testTunnelSlug,
		Alias:              testTunnelSlug,
		LocalPort:          9090,
		Environment:        tunnelEnvECSFargate,
		ResourceID:         testTunnelResourceID,
		ConnectorRoutingID: testTunnelRoutingID,
		KnockResourceID:    testTunnelKnockID,
		APIURL:             testTunnelAPIURL,
	}, testTunnelImageRef)

	for _, want := range []string{
		ecsFargateChecklistText,
		"non-essential sidecar container",
		"Fargate's awsvpc network mode",
		"Replace `REPLACE_WITH_SECRET_ARN_FOR_QURL_CONNECTOR_" + testTunnelSlug + "`",
		ecsFargateRegionPlaceholderNote,
		"AWS appends a random suffix",
		"127.0.0.1:9090",
		"POSIX UID/GID `65532:65532`",
		"qurl-audit",
		"read-only root filesystem",
		"root-directory modes 0700, 0750, and 0755",
		"warm-start task revision",
		"Deleting it first prevents replacement tasks from starting",
		"AWS Secrets Manager",
		"Store the enrollment token from the separate DM",
		"install-instructions message intentionally does not contain the token",
		"ECS injects this enrollment-token secret as `QURL_API_KEY`, which is an environment variable",
		"file-mounted secret runtimes should use `QURL_API_KEY_FILE` instead",
		"prefer a file-mounted secret runtime",
		"secret as `qurl-connector-" + testTunnelSlug + "`",
		testTunnelImageRef,
		"Put qurl-proxy.yaml at `/work/qurl-proxy.yaml` on an EFS access point",
		"mounted into the task as the `qurl-config` volume",
		testTunnelLocalPort9090Line,
		"resource_id: '" + testTunnelResourceID + "'",
		"connector_routing_id: '" + testTunnelRoutingID + "'",
		`"name": "QURL_CONNECTOR_ID"`,
		`"value": "` + testTunnelSlug + `"`,
		`"name": "QURL_API_URL"`,
		`"value": "` + testTunnelAPIURL + `"`,
		`"user": "65532:65532"`,
		testTunnelECSAPIKeyNameLine,
		`REPLACE_WITH_SECRET_ARN_FOR_QURL_CONNECTOR_` + testTunnelSlug,
		`"sourceVolume": "qurl-agent-state"`,
		`"sourceVolume": "qurl-config"`,
		`"sourceVolume": "qurl-audit"`,
		`"readonlyRootFilesystem": true`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("ECS instructions missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{testForbiddenSlackYAMLFence, testForbiddenSlackShellFence, testForbiddenResourceLabel, testTunnelAPIKey, "QURL_CONNECTOR_SLUG", "QURL_BOOTSTRAP_URL", "knock_resource_id", "LAYERV_KNOCK_RESOURCE_ID"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("ECS instructions leaked %q:\n%s", forbidden, got)
		}
	}
	if strings.Contains(got, `\u003c`) || strings.Contains(got, `\u003e`) {
		t.Fatalf("ECS instructions escaped placeholder angle brackets:\n%s", got)
	}
	if gotFenceCount := strings.Count(got, "```"); gotFenceCount != 4 {
		t.Fatalf("ECS instructions rendered %d code fences, want 4 for two independently copyable artifacts:\n%s", gotFenceCount, got)
	}

	containerJSON, err := renderECSSidecarContainerJSON(&tunnelInstallArgs{
		Slug:               testTunnelSlug,
		Alias:              testTunnelSlug,
		LocalPort:          9090,
		Environment:        tunnelEnvECSFargate,
		ResourceID:         testTunnelResourceID,
		ConnectorRoutingID: testTunnelRoutingID,
		KnockResourceID:    testTunnelKnockID,
		APIURL:             testTunnelAPIURL,
	}, testTunnelImageRef)
	if err != nil {
		t.Fatalf("renderECSSidecarContainerJSON: %v", err)
	}
	var container ecsContainerDefinition
	if err := json.Unmarshal([]byte(containerJSON), &container); err != nil {
		t.Fatalf("ECS sidecar JSON did not parse: %v", err)
	}
	if container.Essential {
		t.Fatal("ECS sidecar Essential = true, want false so the tunnel does not take down the app task")
	}
	if container.User != ecsConnectorUser {
		t.Fatalf("ECS sidecar User = %q, want connector image UID/GID", container.User)
	}
	if !container.ReadonlyRootFilesystem {
		t.Fatal("ECS sidecar ReadonlyRootFilesystem = false, want true")
	}
	if got := container.LinuxParameters.Capabilities.Drop; len(got) != 1 || got[0] != testCapabilityAll {
		t.Fatalf("ECS sidecar capability drop = %v, want [ALL]", got)
	}
	if len(container.Secrets) != 1 || container.Image != testTunnelImageRef || container.Secrets[0].Name != tunnelEnvAPIKey {
		t.Fatalf("ECS sidecar = %+v, want image and bootstrap secret wiring", container)
	}
	if container.Secrets[0].ValueFrom != "REPLACE_WITH_SECRET_ARN_FOR_QURL_CONNECTOR_"+testTunnelSlug {
		t.Fatalf("ECS secret ValueFrom = %q, want unmistakable replacement placeholder", container.Secrets[0].ValueFrom)
	}
	env := map[string]string{}
	for _, e := range container.Environment {
		env[e.Name] = e.Value
	}
	if got := env[connectorAuditFileEnv]; got != connectorAuditFilePath {
		t.Fatalf("ECS %s = %q, want %q", connectorAuditFileEnv, got, connectorAuditFilePath)
	}
	if !ecsMountPointPresent(container.MountPoints, "qurl-audit", connectorAuditDir, false) {
		t.Fatalf("ECS mountPoints = %+v, want writable qurl-audit mount", container.MountPoints)
	}
	if _, ok := env["LAYERV_KNOCK_RESOURCE_ID"]; ok {
		t.Fatal("ECS environment rendered the advanced knock-resource override")
	}
}

func TestRenderECSSidecarContainerJSONHubTrustSet(t *testing.T) {
	t.Parallel()
	args := testTunnelInstallArgs()
	args.HubTrust = testTunnelHubTrust()

	containerJSON, err := renderECSSidecarContainerJSON(args, testTunnelImageRef)
	if err != nil {
		t.Fatalf("renderECSSidecarContainerJSON: %v", err)
	}
	var container ecsContainerDefinition
	if err := json.Unmarshal([]byte(containerJSON), &container); err != nil {
		t.Fatalf("ECS sidecar JSON did not parse: %v", err)
	}
	env := map[string]string{}
	names := make([]string, 0, len(container.Environment))
	for _, e := range container.Environment {
		env[e.Name] = e.Value
		names = append(names, e.Name)
	}
	for name, want := range map[string]string{
		"QURL_CONNECTOR_HUB_HOST":                  testTunnelHubHost,
		"QURL_CONNECTOR_HUB_PORT":                  testTunnelHubPort,
		"QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64": testTunnelHubKeyB64,
	} {
		if got := env[name]; got != want {
			t.Fatalf("ECS environment %s = %q, want %q", name, got, want)
		}
	}
	idIdx := slices.Index(names, ecsConnectorIDEnv)
	hubIdx := slices.Index(names, "QURL_CONNECTOR_HUB_HOST")
	apiIdx := slices.Index(names, "QURL_API_URL")
	if idIdx < 0 || hubIdx < 0 || apiIdx < 0 || idIdx >= hubIdx || hubIdx >= apiIdx {
		t.Fatalf("ECS environment order = %v, want %s before Hub trust before QURL_API_URL", names, ecsConnectorIDEnv)
	}
}

// wantECSHubUnsetGolden is byte-for-byte what renderECSFargateTunnelInstructions
// produced for testTunnelInstallArgs()+testTunnelImageRef before Hub trust
// passthrough existed, captured mechanically (not hand-transcribed) to prove
// the unset path is unchanged rather than assume it.
const wantECSHubUnsetGolden = "Use this as an ECS/Fargate task-definition checklist. Create the AWS Secrets Manager secret as `qurl-connector-prod-dashboard` using the temporary enrollment token delivered separately by DM so the task definition's `valueFrom` ARN resolves. Replace `REPLACE_WITH_SECRET_ARN_FOR_QURL_CONNECTOR_prod-dashboard` with the full secret ARN shown by Secrets Manager; AWS appends a random suffix to secret ARNs. Also replace the `<region>` placeholder in the `awslogs-region` field below. Fargate's awsvpc network mode shares one task ENI across containers, so no explicit network_mode is needed; `127.0.0.1:8080` reaches the target container. Configure the qurl-agent-state, qurl-audit, and qurl-config EFS access points with POSIX UID/GID `65532:65532`, matching the connector image's nonroot user; use root-directory modes 0700, 0750, and 0755 respectively, and make qurl-proxy.yaml mode 0644. The qurl-audit volume preserves rotated audit records while the Connector uses a read-only root filesystem. The generated sidecar drops every Linux capability.\n\n1. Store the enrollment token from the separate DM in AWS Secrets Manager. This install-instructions message intentionally does not contain the token.\n\n2. Put qurl-proxy.yaml at `/work/qurl-proxy.yaml` on an EFS access point mounted into the task as the `qurl-config` volume:\n\n```\nroutes:\n  - id: 'prod-dashboard'\n    type: http\n    local_ip: 127.0.0.1\n    local_port: 8080\n    resource_id: 'MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE2cTVv5_3eeYCcLLq5ROYCqcmY50HiKZ9ATglIkPnCji1E_S63UMtXba1moR8-Q6EV7oM6zwwh9_j2CDujzXvLA'\n    connector_routing_id: 'c-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'\n```\n\n3. Add this non-essential sidecar container to the same task definition as the target container. ECS injects this enrollment-token secret as `QURL_API_KEY`, which is an environment variable; file-mounted secret runtimes should use `QURL_API_KEY_FILE` instead:\n\n```\n{\n  \"name\": \"qurl-connector\",\n  \"image\": \"ghcr.io/layervai/qurl-connector:v-test\",\n  \"user\": \"65532:65532\",\n  \"essential\": false,\n  \"readonlyRootFilesystem\": true,\n  \"environment\": [\n    {\n      \"name\": \"QURL_CONNECTOR_ID\",\n      \"value\": \"prod-dashboard\"\n    },\n    {\n      \"name\": \"QURL_API_URL\",\n      \"value\": \"https://api.sandbox.example/v1\"\n    },\n    {\n      \"name\": \"QURL_AUDIT_FILE\",\n      \"value\": \"/var/log/layerv/qurl-connector/audit.log\"\n    }\n  ],\n  \"secrets\": [\n    {\n      \"name\": \"QURL_API_KEY\",\n      \"valueFrom\": \"REPLACE_WITH_SECRET_ARN_FOR_QURL_CONNECTOR_prod-dashboard\"\n    }\n  ],\n  \"mountPoints\": [\n    {\n      \"sourceVolume\": \"qurl-agent-state\",\n      \"containerPath\": \"/var/lib/layerv/agent\",\n      \"readOnly\": false\n    },\n    {\n      \"sourceVolume\": \"qurl-audit\",\n      \"containerPath\": \"/var/log/layerv/qurl-connector\",\n      \"readOnly\": false\n    },\n    {\n      \"sourceVolume\": \"qurl-config\",\n      \"containerPath\": \"/work\",\n      \"readOnly\": true\n    }\n  ],\n  \"logConfiguration\": {\n    \"logDriver\": \"awslogs\",\n    \"options\": {\n      \"awslogs-group\": \"/ecs/qurl-connector\",\n      \"awslogs-region\": \"<region>\",\n      \"awslogs-stream-prefix\": \"qurl\"\n    }\n  },\n  \"linuxParameters\": {\n    \"capabilities\": {\n      \"drop\": [\n        \"ALL\"\n      ]\n    }\n  }\n}\n```\n\n4. Add durable EFS-backed volumes named qurl-agent-state, qurl-audit, and qurl-config. Do not share qurl-agent-state across concurrently running sidecars. After the task logs show the qURL Connector connected, register and deploy a warm-start task revision with the QURL_API_KEY entry removed from `secrets`; verify the replacement task connects from qurl-agent-state, then delete the enrollment-token secret. Deleting it first prevents replacement tasks from starting. For future enrollment, prefer a file-mounted secret runtime so new enrollment tokens are not revealed through task environment variables."

func TestRenderECSFargateTunnelInstructionsHubTrustUnsetOutputUnchanged(t *testing.T) {
	t.Parallel()
	got := mustRenderECSFargateTunnelInstructions(t, testTunnelInstallArgs(), testTunnelImageRef)
	if got != wantECSHubUnsetGolden {
		t.Fatalf("ECS instructions changed with HubTrust unset:\ngot:\n%s\nwant:\n%s", got, wantECSHubUnsetGolden)
	}
}
