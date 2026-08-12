package internal

import (
	"bytes"
	"context"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestRenderKubernetesTunnelInstructionsYAMLAndSecurityContext(t *testing.T) {
	t.Parallel()
	args := &tunnelInstallArgs{
		Slug:               testTunnelSlug,
		Alias:              testTunnelSlug,
		LocalPort:          9090,
		Environment:        tunnelEnvKubernetes,
		ResourceID:         testTunnelResourceID,
		ConnectorRoutingID: testTunnelRoutingID,
		KnockResourceID:    testTunnelKnockID,
		APIURL:             testTunnelAPIURL,
	}
	got := mustRenderKubernetesTunnelInstructions(t, args, testTunnelImageRef)

	for _, want := range []string{
		"QURL_BOOTSTRAP_SECRET='qurl-connector-" + testTunnelSlug + "'",
		testTunnelKeyPromptLine,
		`head -c "$QURL_BOOTSTRAP_KEY_LEN" <<QURL_BOOTSTRAP_KEY_EOF | kubectl create secret generic "$QURL_BOOTSTRAP_SECRET" --from-file=api_key=/dev/stdin --dry-run=client -o yaml | kubectl apply -f -`,
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
	if len(docs) != 3 {
		t.Fatalf("Kubernetes bootstrap docs = %d, want ConfigMap + state PVC + audit PVC: %#v", len(docs), docs)
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
	wantConfig, err := renderTunnelConfigYAML(args)
	if err != nil {
		t.Fatalf("renderTunnelConfigYAML() err = %v", err)
	}
	if gotConfig := configMap.Data["qurl-proxy.yaml"]; gotConfig != wantConfig {
		t.Fatalf("ConfigMap qurl-proxy.yaml = %q, want %q", gotConfig, wantConfig)
	}
	patchMarker := "Pod spec additions:\n"
	patchSectionStart := strings.Index(got, patchMarker)
	if patchSectionStart < 0 {
		t.Fatalf("Kubernetes instructions missing pod spec additions:\n%s", got)
	}
	patchCodeStart := strings.Index(got[patchSectionStart:], "```\n")
	if patchCodeStart < 0 {
		t.Fatalf("Kubernetes instructions missing pod spec code block:\n%s", got)
	}
	patchCodeStart += patchSectionStart + len("```\n")
	patchCodeEnd := strings.Index(got[patchCodeStart:], "\n```")
	if patchCodeEnd < 0 {
		t.Fatalf("Kubernetes instructions missing pod spec code block terminator:\n%s", got)
	}
	var podSpecFragment struct {
		SecurityContext map[string]any `yaml:"securityContext"`
		InitContainers  []struct {
			Name  string `yaml:"name"`
			Image string `yaml:"image"`
		} `yaml:"initContainers"`
		Containers []struct {
			Name            string              `yaml:"name"`
			SecurityContext map[string]any      `yaml:"securityContext"`
			Env             []ecsEnvironmentVar `yaml:"env"`
		} `yaml:"containers"`
		Volumes []map[string]any `yaml:"volumes"`
	}
	if err := yaml.Unmarshal([]byte(got[patchCodeStart:patchCodeStart+patchCodeEnd]), &podSpecFragment); err != nil {
		t.Fatalf("PodSpec fragment YAML did not parse: %v", err)
	}
	if len(podSpecFragment.SecurityContext) != 0 || len(podSpecFragment.InitContainers) != 2 || len(podSpecFragment.Containers) != 1 || len(podSpecFragment.Volumes) != 6 || podSpecFragment.Containers[0].Name != "qurl-connector" {
		t.Fatalf("PodSpec fragment = %+v, want permissions/copy init containers, one qurl-connector container, and six volumes without pod fsGroup", podSpecFragment)
	}
	if podSpecFragment.InitContainers[0].Image != connectorVolumePermissionsImage {
		t.Fatalf("permissions image = %q, want %q", podSpecFragment.InitContainers[0].Image, connectorVolumePermissionsImage)
	}
	if podSpecFragment.Containers[0].SecurityContext["readOnlyRootFilesystem"] != true {
		t.Fatalf("Connector securityContext = %+v, want readOnlyRootFilesystem", podSpecFragment.Containers[0].SecurityContext)
	}
	if got := ecsEnvMap(podSpecFragment.Containers[0].Env)[connectorAuditFileEnv]; got != connectorAuditFilePath {
		t.Fatalf("Kubernetes %s = %q, want %q", connectorAuditFileEnv, got, connectorAuditFilePath)
	}
	for _, want := range []string{
		"init-container/sidecar/volumes block",
		"initContainers:",
		"name: qurl-volume-permissions",
		"name: qurl-bootstrap-copy",
		connectorVolumePermissionsImage,
		"runAsUser: 0",
		"qurl-go rejects group-writable identity state",
		"admission policy must permit the two root init containers",
		"securityContext:",
		"name: qurl-connector",
		"value: '" + testTunnelSlug + "'",
		"resource_id: '" + testTunnelResourceID + "'",
		"connector_routing_id: '" + testTunnelRoutingID + "'",
		"name: QURL_API_URL",
		"value: '" + testTunnelAPIURL + "'",
		"runAsUser: 65532",
		"runAsGroup: 65532",
		"runAsNonRoot: true",
		"allowPrivilegeEscalation: false",
		"readOnlyRootFilesystem: true",
		"drop: [\"ALL\"]",
		"type: RuntimeDefault",
		"name: QURL_AUDIT_FILE",
		"value: /var/log/layerv/qurl-connector/audit.log",
		"name: qurl-tmp",
		"mountPath: /tmp",
		"name: qurl-audit",
		"mountPath: /var/log/layerv",
		"defaultMode: 0400",
		"cp /bootstrap-source/api_key /bootstrap/api_key",
		"chmod 0400 /bootstrap/api_key",
		"chown 65532:65532 /bootstrap/api_key",
		"chown 65532:65532 /tmp-runtime",
		"chmod 0700 /tmp-runtime",
		"separate state and audit PVCs",
		"local shell into `kubectl`",
		"shared, recorded",
		"command-traced terminal session",
		"generated Secret manifest",
		"warm-start workload revision",
		"deleting it first prevents a replacement pod from starting",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Kubernetes instructions missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{
		"fsGroup:",
		"fsGroupChangePolicy:",
		"QURL_BOOTSTRAP_URL",
		"knock_resource_id",
		"LAYERV_KNOCK_RESOURCE_ID",
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("Kubernetes instructions included pod-level or unreadable secret setting %q:\n%s", forbidden, got)
		}
	}
	for _, forbidden := range []string{testTunnelAPIKey, testForbiddenBootstrapArgv} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("Kubernetes instructions leaked %q:\n%s", forbidden, got)
		}
	}
}

func TestRenderKubernetesTunnelInstructionsYAMLQuotesAPIURL(t *testing.T) {
	t.Parallel()
	args := testTunnelInstallArgs()
	args.Environment = tunnelEnvKubernetes
	args.APIURL = testShellSignificantTunnelAPIURL

	got := mustRenderKubernetesTunnelInstructions(t, args, testTunnelImageRef)
	quoted, err := yamlSingleQuoted(args.APIURL)
	if err != nil {
		t.Fatalf("yamlSingleQuoted: %v", err)
	}
	if count := strings.Count(got, "value: "+quoted); count != 1 {
		t.Fatalf("Kubernetes instructions contain %d quoted API URL values, want 1:\n%s", count, got)
	}
}

func TestRenderKubernetesPodSpecFragmentDryRunsWithKubectl(t *testing.T) {
	t.Parallel()
	kubectl, err := exec.LookPath("kubectl")
	if err != nil {
		t.Skip("kubectl not on PATH")
	}
	got := mustRenderKubernetesTunnelInstructions(t, &tunnelInstallArgs{
		Slug:               testTunnelSlug,
		Alias:              testTunnelSlug,
		LocalPort:          9090,
		Environment:        tunnelEnvKubernetes,
		ResourceID:         testTunnelResourceID,
		ConnectorRoutingID: testTunnelRoutingID,
		KnockResourceID:    testTunnelKnockID,
		APIURL:             testTunnelAPIURL,
	}, testTunnelImageRef)
	fragment := kubernetesPodSpecFragmentFromInstructions(t, got)
	pod := "apiVersion: v1\nkind: Pod\nmetadata:\n  name: qurl-connector-render-test\nspec:\n" + indentLines(fragment, 2) + "\n"
	const kubectlDryRunTimeout = 20 * time.Second
	ctx, cancel := context.WithTimeout(t.Context(), kubectlDryRunTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, kubectl, "apply", "--dry-run=client", "--validate=false", "-f", "-") //nolint:gosec // G204: kubectl path comes from exec.LookPath and no user input reaches argv.
	cmd.Stdin = strings.NewReader(pod)
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			t.Skipf("kubectl dry-run exceeded %s in this environment", kubectlDryRunTimeout)
		}
		if bytes.Contains(out, []byte("couldn't get current server API group list")) {
			t.Skipf("kubectl dry-run needs cluster discovery in this environment: %s", out)
		}
		t.Fatalf("kubectl dry-run failed: %v\n%s\n--- pod ---\n%s", err, out, pod)
	}
}

func TestKubernetesTunnelObjectNamesShortenLongSlug(t *testing.T) {
	t.Parallel()
	slug := strings.Repeat("a", 42) + "-" + strings.Repeat("b", 21)
	dns1123Label := regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)
	args := &tunnelInstallArgs{
		Slug:               slug,
		Alias:              slug,
		LocalPort:          9090,
		Environment:        tunnelEnvKubernetes,
		ResourceID:         testTunnelResourceID,
		ConnectorRoutingID: testTunnelRoutingID,
		KnockResourceID:    testTunnelKnockID,
		APIURL:             testTunnelAPIURL,
	}
	names := kubernetesTunnelObjectNames(slug)
	for label, name := range map[string]string{
		"secret":     names.secret,
		"config_map": names.configMap,
		"agent_pvc":  names.agentPVC,
		"audit_pvc":  names.auditPVC,
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

	got := mustRenderKubernetesTunnelInstructions(t, args, testTunnelImageRef)
	for _, want := range []string{
		"QURL_BOOTSTRAP_SECRET='" + names.secret + "'",
		"name: '" + names.configMap + "'",
		"name: '" + names.agentPVC + "'",
		"claimName: '" + names.agentPVC + "'",
		"claimName: '" + names.auditPVC + "'",
		"secretName: '" + names.secret + "'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Kubernetes instructions missing shortened name %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{
		"qurl-connector-" + slug,
		"qurl-proxy-" + slug,
		"qurl-agent-" + slug,
		"qurl-audit-" + slug,
	} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("Kubernetes instructions contain overlong name %q:\n%s", forbidden, got)
		}
	}
}

func kubernetesPodSpecFragmentFromInstructions(t *testing.T, got string) string {
	t.Helper()
	patchMarker := "Pod spec additions:\n"
	patchSectionStart := strings.Index(got, patchMarker)
	if patchSectionStart < 0 {
		t.Fatalf("Kubernetes instructions missing pod spec additions:\n%s", got)
	}
	patchCodeStart := strings.Index(got[patchSectionStart:], "```\n")
	if patchCodeStart < 0 {
		t.Fatalf("Kubernetes instructions missing pod spec code block:\n%s", got)
	}
	patchCodeStart += patchSectionStart + len("```\n")
	patchCodeEnd := strings.Index(got[patchCodeStart:], "\n```")
	if patchCodeEnd < 0 {
		t.Fatalf("Kubernetes instructions missing pod spec code block terminator:\n%s", got)
	}
	return got[patchCodeStart : patchCodeStart+patchCodeEnd]
}

func TestRenderKubernetesTunnelInstructionsHubTrustSet(t *testing.T) {
	t.Parallel()
	args := testTunnelInstallArgs()
	args.HubTrust = testTunnelHubTrust()

	got := mustRenderKubernetesTunnelInstructions(t, args, testTunnelImageRef)
	fragment := kubernetesPodSpecFragmentFromInstructions(t, got)

	var podSpecFragment struct {
		Containers []struct {
			Name string              `yaml:"name"`
			Env  []ecsEnvironmentVar `yaml:"env"`
		} `yaml:"containers"`
	}
	if err := yaml.Unmarshal([]byte(fragment), &podSpecFragment); err != nil {
		t.Fatalf("PodSpec fragment YAML did not parse: %v", err)
	}
	if len(podSpecFragment.Containers) != 1 || podSpecFragment.Containers[0].Name != "qurl-connector" {
		t.Fatalf("PodSpec fragment containers = %+v, want one qurl-connector container", podSpecFragment.Containers)
	}
	env := map[string]string{}
	names := make([]string, 0, len(podSpecFragment.Containers[0].Env))
	for _, e := range podSpecFragment.Containers[0].Env {
		env[e.Name] = e.Value
		names = append(names, e.Name)
	}
	for name, want := range map[string]string{
		"QURL_CONNECTOR_HUB_HOST":                  testTunnelHubHost,
		"QURL_CONNECTOR_HUB_PORT":                  testTunnelHubPort,
		"QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64": testTunnelHubKeyB64,
	} {
		if got := env[name]; got != want {
			t.Fatalf("Kubernetes container env %s = %q, want %q", name, got, want)
		}
	}
	idIdx := slices.Index(names, "QURL_CONNECTOR_ID")
	hubIdx := slices.Index(names, "QURL_CONNECTOR_HUB_HOST")
	apiIdx := slices.Index(names, "QURL_API_URL")
	if idIdx < 0 || hubIdx < 0 || apiIdx < 0 || idIdx >= hubIdx || hubIdx >= apiIdx {
		t.Fatalf("Kubernetes container env order = %v, want QURL_CONNECTOR_ID before Hub trust before QURL_API_URL", names)
	}
}

// wantKubernetesHubUnsetGolden is byte-for-byte what renderKubernetesTunnelInstructions
// produced for testTunnelInstallArgs()+testTunnelImageRef before Hub trust
// passthrough existed, captured mechanically (not hand-transcribed) to prove
// the unset path is unchanged rather than assume it.
const wantKubernetesHubUnsetGolden = "Run this once in the target namespace, then add the init-container/sidecar/volumes block to the same pod spec as the target container so `127.0.0.1:8080` reaches the local service.\n- Use one PVC per sidecar replica; if you scale replicas, use a StatefulSet with a volumeClaimTemplate instead of sharing this PVC.\n- The Connector uses separate state and audit PVCs. qurl-go rejects group-writable identity state, so do not add pod-level `fsGroup`; the permissions init container enforces owner-only state modes before each start.\n- Your admission policy must permit the two root init containers: volume permissions uses CHOWN, DAC_OVERRIDE, and FOWNER, while the one-time bootstrap copy uses CHOWN only. The long-running Connector remains nonroot, read-only-root, seccomp-confined, and capability-free.\n- The enrollment token is streamed through your local shell into `kubectl`; do not run this from a shared, recorded, or command-traced terminal session. The apply pipeline briefly carries a generated Secret manifest between `kubectl` processes.\n- After the pod connects, create and roll out a warm-start workload revision that removes `qurl-bootstrap-copy`, both enrollment-token volumes and their mounts, and `QURL_API_KEY_FILE`. Verify the replacement pod connects from its persisted state, then delete the enrollment-token Secret; deleting it first prevents a replacement pod from starting.\n\n```\nset -eu\nif (set -o pipefail) 2>/dev/null; then\n  set -o pipefail\nfi\n\nQURL_BOOTSTRAP_SECRET='qurl-connector-prod-dashboard'\nif [ -z \"${QURL_BOOTSTRAP_KEY:-}\" ]; then\n  if [ ! -t 0 ]; then\n    echo \"Set QURL_BOOTSTRAP_KEY or run this block from an interactive terminal.\" >&2\n    exit 1\n  fi\n  printf 'Paste qURL enrollment token (input hidden): ' >&2\n  STTY_STATE=\"$(stty -g 2>/dev/null | tr -d '[:space:]' || true)\"\n  if [ -n \"$STTY_STATE\" ]; then\n    stty -echo\n    trap 'if [ -n \"$STTY_STATE\" ]; then stty \"$STTY_STATE\" 2>/dev/null || true; fi' INT TERM EXIT\n  fi\n  if ! IFS= read -r QURL_BOOTSTRAP_KEY; then\n    if [ -n \"$STTY_STATE\" ]; then\n      stty \"$STTY_STATE\"\n      trap - INT TERM EXIT\n    fi\n    printf '\\n' >&2\n    echo \"Enrollment token is required.\" >&2\n    exit 1\n  fi\n  if [ -n \"$STTY_STATE\" ]; then\n    stty \"$STTY_STATE\"\n    trap - INT TERM EXIT\n  fi\n  printf '\\n' >&2\nfi\nif [ -z \"$QURL_BOOTSTRAP_KEY\" ]; then\n  echo \"Enrollment token is required.\" >&2\n  exit 1\nfi\nQURL_BOOTSTRAP_KEY_LEN=${#QURL_BOOTSTRAP_KEY}\nhead -c \"$QURL_BOOTSTRAP_KEY_LEN\" <<QURL_BOOTSTRAP_KEY_EOF | kubectl create secret generic \"$QURL_BOOTSTRAP_SECRET\" --from-file=api_key=/dev/stdin --dry-run=client -o yaml | kubectl apply -f -\n$QURL_BOOTSTRAP_KEY\nQURL_BOOTSTRAP_KEY_EOF\nunset QURL_BOOTSTRAP_KEY QURL_BOOTSTRAP_KEY_LEN\n\nkubectl apply -f - <<'QURL_K8S_YAML_EOF'\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: 'qurl-proxy-prod-dashboard'\ndata:\n  qurl-proxy.yaml: |\n    routes:\n      - id: 'prod-dashboard'\n        type: http\n        local_ip: 127.0.0.1\n        local_port: 8080\n        resource_id: 'MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE2cTVv5_3eeYCcLLq5ROYCqcmY50HiKZ9ATglIkPnCji1E_S63UMtXba1moR8-Q6EV7oM6zwwh9_j2CDujzXvLA'\n        connector_routing_id: 'c-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'\n---\napiVersion: v1\nkind: PersistentVolumeClaim\nmetadata:\n  name: 'qurl-agent-prod-dashboard'\nspec:\n  accessModes: [\"ReadWriteOnce\"]\n  resources:\n    requests:\n      storage: 1Gi\n---\napiVersion: v1\nkind: PersistentVolumeClaim\nmetadata:\n  name: 'qurl-audit-prod-dashboard'\nspec:\n  accessModes: [\"ReadWriteOnce\"]\n  resources:\n    requests:\n      storage: 1Gi\nQURL_K8S_YAML_EOF\n```\n\nPod spec additions:\nAppend both generated init containers under your existing `initContainers:` list, append the `qurl-connector` container under `containers:`, and append the volumes under `volumes:`. Do not add pod-level `fsGroup` and do not duplicate existing YAML keys.\n\n```\ninitContainers:\n  - name: qurl-volume-permissions\n    image: docker.io/library/busybox:1.37.0@sha256:9532d8c39891ca2ecde4d30d7710e01fb739c87a8b9299685c63704296b16028\n    command:\n      - sh\n      - -ceu\n      - |\n        find /state -type d -exec chmod 0700 '{}' ';'\n        find /state -type f -exec chmod 0600 '{}' ';'\n        chown -R 65532:65532 /state\n        mkdir -p /audit/qurl-connector\n        find /audit -type d -exec chmod 0750 '{}' ';'\n        find /audit -type f -exec chmod 0640 '{}' ';'\n        chown -R 65532:65532 /audit\n        chown 65532:65532 /tmp-runtime\n        chmod 0700 /tmp-runtime\n    securityContext:\n      runAsUser: 0\n      runAsNonRoot: false\n      readOnlyRootFilesystem: true\n      allowPrivilegeEscalation: false\n      capabilities:\n        drop: [\"ALL\"]\n        add: [\"CHOWN\", \"DAC_OVERRIDE\", \"FOWNER\"]\n      seccompProfile:\n        type: RuntimeDefault\n    volumeMounts:\n      - name: qurl-agent-state\n        mountPath: /state\n      - name: qurl-audit\n        mountPath: /audit\n      - name: qurl-tmp\n        mountPath: /tmp-runtime\n  - name: qurl-bootstrap-copy\n    image: docker.io/library/busybox:1.37.0@sha256:9532d8c39891ca2ecde4d30d7710e01fb739c87a8b9299685c63704296b16028\n    command:\n      - sh\n      - -ceu\n      - |\n        cp /bootstrap-source/api_key /bootstrap/api_key\n        chmod 0400 /bootstrap/api_key\n        chown 65532:65532 /bootstrap/api_key\n    securityContext:\n      runAsUser: 0\n      runAsNonRoot: false\n      readOnlyRootFilesystem: true\n      allowPrivilegeEscalation: false\n      capabilities:\n        drop: [\"ALL\"]\n        add: [\"CHOWN\"]\n      seccompProfile:\n        type: RuntimeDefault\n    volumeMounts:\n      - name: qurl-bootstrap-source\n        mountPath: /bootstrap-source\n        readOnly: true\n      - name: qurl-bootstrap\n        mountPath: /bootstrap\ncontainers:\n  - name: qurl-connector\n    image: 'ghcr.io/layervai/qurl-connector:v-test'\n    securityContext:\n      runAsUser: 65532\n      runAsGroup: 65532\n      runAsNonRoot: true\n      readOnlyRootFilesystem: true\n      allowPrivilegeEscalation: false\n      capabilities:\n        drop: [\"ALL\"]\n      seccompProfile:\n        type: RuntimeDefault\n    env:\n      - name: QURL_API_KEY_FILE\n        value: /run/secrets/qurl-connector/api_key\n      - name: QURL_CONNECTOR_ID\n        value: 'prod-dashboard'\n      - name: QURL_API_URL\n        value: 'https://api.sandbox.example/v1'\n      - name: QURL_AUDIT_FILE\n        value: /var/log/layerv/qurl-connector/audit.log\n    volumeMounts:\n      - name: qurl-tmp\n        mountPath: /tmp\n      - name: qurl-agent-state\n        mountPath: /var/lib/layerv/agent\n      - name: qurl-audit\n        mountPath: /var/log/layerv\n      - name: qurl-bootstrap\n        mountPath: /run/secrets/qurl-connector\n        readOnly: true\n      - name: qurl-proxy\n        mountPath: /work/qurl-proxy.yaml\n        subPath: qurl-proxy.yaml\n        readOnly: true\nvolumes:\n  - name: qurl-tmp\n    emptyDir:\n      sizeLimit: 64Mi\n  - name: qurl-agent-state\n    persistentVolumeClaim:\n      claimName: 'qurl-agent-prod-dashboard'\n  - name: qurl-audit\n    persistentVolumeClaim:\n      claimName: 'qurl-audit-prod-dashboard'\n  - name: qurl-bootstrap-source\n    secret:\n      secretName: 'qurl-connector-prod-dashboard'\n      # Mounted only into the root copy init; the runtime receives UID-65532 0400.\n      defaultMode: 0400\n  - name: qurl-bootstrap\n    emptyDir:\n      medium: Memory\n      sizeLimit: 1Mi\n  - name: qurl-proxy\n    configMap:\n      name: 'qurl-proxy-prod-dashboard'\n```"

func TestRenderKubernetesTunnelInstructionsHubTrustUnsetOutputUnchanged(t *testing.T) {
	t.Parallel()
	got := mustRenderKubernetesTunnelInstructions(t, testTunnelInstallArgs(), testTunnelImageRef)
	if got != wantKubernetesHubUnsetGolden {
		t.Fatalf("Kubernetes instructions changed with HubTrust unset:\ngot:\n%s\nwant:\n%s", got, wantKubernetesHubUnsetGolden)
	}
}

func TestKubernetesNameWithSlugHandlesEmptyTrimmedBase(t *testing.T) {
	t.Parallel()
	// Production tunnel slugs cannot be all hyphens; this protects the helper
	// for future callers with different validated prefixes or names.
	got := kubernetesNameWithSlug("qurl-connector-", strings.Repeat("-", 80))
	if strings.Contains(got, "--") {
		t.Fatalf("name = %q, want no doubled hyphen when trimmed base is empty", got)
	}
	if len(got) > kubernetesNameMaxLen {
		t.Fatalf("name length = %d for %q, want <= %d", len(got), got, kubernetesNameMaxLen)
	}
}

func TestKubernetesNameWithSlugHandlesLongPrefix(t *testing.T) {
	t.Parallel()
	got := kubernetesNameWithSlug(strings.Repeat("a", kubernetesNameMaxLen), testTunnelSlug)
	if len(got) > kubernetesNameMaxLen {
		t.Fatalf("name length = %d for %q, want <= %d", len(got), got, kubernetesNameMaxLen)
	}
	if strings.HasSuffix(got, "-") {
		t.Fatalf("name = %q, must end with hash characters", got)
	}
}
