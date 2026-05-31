package internal

import (
	"bytes"
	"context"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestRenderKubernetesTunnelInstructionsYAMLAndSecurityContext(t *testing.T) {
	t.Parallel()
	args := &tunnelInstallArgs{
		Slug:        testTunnelSlug,
		Alias:       testTunnelSlug,
		LocalPort:   9090,
		Environment: tunnelEnvKubernetes,
	}
	got := mustRenderKubernetesTunnelInstructions(t, args, testTunnelImageRef)

	for _, want := range []string{
		"QURL_BOOTSTRAP_SECRET='qurl-tunnel-" + testTunnelSlug + "'",
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
		Containers      []struct {
			Name            string         `yaml:"name"`
			SecurityContext map[string]any `yaml:"securityContext"`
		} `yaml:"containers"`
	}
	if err := yaml.Unmarshal([]byte(got[patchCodeStart:patchCodeStart+patchCodeEnd]), &podSpecFragment); err != nil {
		t.Fatalf("PodSpec fragment YAML did not parse: %v", err)
	}
	if podSpecFragment.SecurityContext["fsGroup"] == nil || len(podSpecFragment.Containers) != 1 || podSpecFragment.Containers[0].Name != "qurl-tunnel" {
		t.Fatalf("PodSpec fragment = %+v, want fsGroup and qurl-tunnel container", podSpecFragment)
	}
	for _, want := range []string{
		"sidecar/securityContext/volumes block",
		"Pod Security Admission `restricted`",
		"fsGroup: 65532",
		"fsGroupChangePolicy: OnRootMismatch",
		"WARNING: pod-level fsGroup applies to every volume in this pod",
		"securityContext:",
		"name: qurl-tunnel",
		"value: '" + testTunnelSlug + "'",
		"runAsUser: 65532",
		"runAsGroup: 65532",
		"runAsNonRoot: true",
		"allowPrivilegeEscalation: false",
		"drop: [\"ALL\"]",
		"type: RuntimeDefault",
		"defaultMode: 0440",
		"pre-provision qURL agent-state ownership separately",
		"including existing app volumes",
		"local shell into `kubectl`",
		"shared, recorded",
		"command-traced terminal session",
		"generated Secret manifest",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Kubernetes instructions missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{
		"initContainers:",
		"runAsUser: 0",
		"defaultMode: 0400",
		"defaultMode: 0444",
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

func TestRenderKubernetesPodSpecFragmentDryRunsWithKubectl(t *testing.T) {
	t.Parallel()
	kubectl, err := exec.LookPath("kubectl")
	if err != nil {
		t.Skip("kubectl not on PATH")
	}
	got := mustRenderKubernetesTunnelInstructions(t, &tunnelInstallArgs{
		Slug:        testTunnelSlug,
		Alias:       testTunnelSlug,
		LocalPort:   9090,
		Environment: tunnelEnvKubernetes,
	}, testTunnelImageRef)
	fragment := kubernetesPodSpecFragmentFromInstructions(t, got)
	pod := "apiVersion: v1\nkind: Pod\nmetadata:\n  name: qurl-tunnel-render-test\nspec:\n" + indentLines(fragment, 2) + "\n"
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

	got := mustRenderKubernetesTunnelInstructions(t, args, testTunnelImageRef)
	for _, want := range []string{
		"QURL_BOOTSTRAP_SECRET='" + names.secret + "'",
		"name: '" + names.configMap + "'",
		"name: '" + names.agentPVC + "'",
		"claimName: '" + names.agentPVC + "'",
		"secretName: '" + names.secret + "'",
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

func TestKubernetesNameWithSlugHandlesEmptyTrimmedBase(t *testing.T) {
	t.Parallel()
	// Production tunnel slugs cannot be all hyphens; this protects the helper
	// for future callers with different validated prefixes or names.
	got := kubernetesNameWithSlug("qurl-tunnel-", strings.Repeat("-", 80))
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
