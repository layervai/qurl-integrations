package internal

import (
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/layervai/qurl-integrations/shared/client"
)

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
		"The initContainer runs `chown -R`",
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
