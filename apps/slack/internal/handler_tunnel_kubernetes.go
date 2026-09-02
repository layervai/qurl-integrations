package internal

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

func renderKubernetesTunnelInstructions(args *tunnelInstallArgs, image string) (string, error) {
	names := kubernetesTunnelObjectNames(args.Slug)
	quotedConfigMap, err := yamlSingleQuoted(names.configMap)
	if err != nil {
		return "", err
	}
	quotedAgentPVC, err := yamlSingleQuoted(names.agentPVC)
	if err != nil {
		return "", err
	}
	quotedImage, err := yamlSingleQuoted(image)
	if err != nil {
		return "", err
	}
	quotedSlug, err := yamlSingleQuoted(args.Slug)
	if err != nil {
		return "", err
	}
	quotedSecret, err := yamlSingleQuoted(names.secret)
	if err != nil {
		return "", err
	}
	endpoint, err := qurlEndpointFromConnectorAPIURL(args.APIURL)
	if err != nil {
		return "", err
	}
	quotedEndpoint, err := yamlSingleQuoted(endpoint)
	if err != nil {
		return "", err
	}
	configYAML, err := renderTunnelConfigYAML(args)
	if err != nil {
		return "", err
	}
	objects := fmt.Sprintf(`set -eu
%s

QURL_BOOTSTRAP_SECRET=%s
%s
%s

kubectl apply -f - <<'QURL_K8S_YAML_EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: %s
data:
  share.yaml: |
%s
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 1Gi
QURL_K8S_YAML_EOF`, renderPortablePipefailShell(), shellSingleQuote(names.secret), renderBootstrapKeyPromptShell(), renderBootstrapKeyToCommandShell(`kubectl create secret generic "$QURL_BOOTSTRAP_SECRET" --from-file=enrollment-token=/dev/stdin --dry-run=client -o yaml | kubectl apply -f -`), quotedConfigMap, indentLines(configYAML, 4), quotedAgentPVC)

	patch := renderKubernetesConnectorPodSpec(&kubernetesConnectorPodSpecArgs{
		imageYAML:     quotedImage,
		slugYAML:      quotedSlug,
		endpointYAML:  quotedEndpoint,
		agentPVCYAML:  quotedAgentPVC,
		secretYAML:    quotedSecret,
		configMapYAML: quotedConfigMap,
	})

	objectsBlock, err := slackCodeBlock(objects)
	if err != nil {
		return "", err
	}
	patch, err = withHubTrustKubernetesEnv(patch, args.Hub)
	if err != nil {
		return "", err
	}
	patchBlock, err := slackCodeBlock(patch)
	if err != nil {
		return "", err
	}
	intro := strings.Join([]string{
		"Run this once in the target namespace, then add the qURL sidecar/volumes block to the same pod spec as the target container so `127.0.0.1:" + strconv.Itoa(args.LocalPort) + "` reaches the local service.",
		"- Use one PVC per sidecar replica; if you scale replicas, use a StatefulSet with a volumeClaimTemplate instead of sharing this PVC.",
		"- The pod-level fsGroup makes only the PVC mount root writable; qURL creates its nested state directory as UID 65532 with owner-only permissions.",
		"- The enrollment token is streamed through your local shell into `kubectl`; do not run this from a shared, recorded, or command-traced terminal session. The apply pipeline briefly carries a generated Secret manifest between `kubectl` processes.",
		"- After the pod connects, create and roll out a warm-start workload revision that removes `--enrollment-token-file`, its Secret volume and mount. Verify the replacement pod connects from persisted state, then delete the enrollment-token Secret.",
	}, "\n")
	return intro + "\n\n" + objectsBlock + "\n\nPod spec additions:\nMerge the generated pod `securityContext`, append the `qurl` container under `containers:`, and append the volumes under `volumes:`. Do not duplicate existing YAML keys.\n\n" + patchBlock, nil
}

type kubernetesConnectorPodSpecArgs struct {
	precedingContainers string
	imageYAML           string
	slugYAML            string
	endpointYAML        string
	agentPVCYAML        string
	secretYAML          string
	configMapYAML       string
}

// anchor: withHubTrustKubernetesEnv splices env entries after the adjacent
// `- name: QURL_ENDPOINT` / `value:` pair, inheriting its indent.
func renderKubernetesConnectorPodSpec(args *kubernetesConnectorPodSpecArgs) string {
	precedingContainers := args.precedingContainers
	if precedingContainers != "" {
		precedingContainers += "\n"
	}
	return fmt.Sprintf(`securityContext:
  fsGroup: 65532
  fsGroupChangePolicy: OnRootMismatch
containers:
%s  - name: qurl
    image: %s
    command: ['/usr/local/bin/qurl']
    args: ['daemon', 'run', '--state-dir', '/var/lib/qurl-volume/state', '--headless-config', '/etc/qurl/share.yaml', '--enrollment-token-file', '/run/secrets/qurl/enrollment-token']
    securityContext:
      runAsUser: 65532
      runAsGroup: 65532
      runAsNonRoot: true
      readOnlyRootFilesystem: true
      allowPrivilegeEscalation: false
      capabilities:
        drop: ["ALL"]
      seccompProfile:
        type: RuntimeDefault
    env:
      - name: QURL_ENDPOINT
        value: %s
    volumeMounts:
      - name: qurl-tmp
        mountPath: /tmp
      - name: qurl-agent-state
        mountPath: /var/lib/qurl-volume
      - name: qurl-bootstrap-source
        mountPath: /run/secrets/qurl
        readOnly: true
      - name: qurl-proxy
        mountPath: /etc/qurl/share.yaml
        subPath: share.yaml
        readOnly: true
volumes:
  - name: qurl-tmp
    emptyDir:
      sizeLimit: 64Mi
  - name: qurl-agent-state
    persistentVolumeClaim:
      claimName: %s
  - name: qurl-bootstrap-source
    secret:
      secretName: %s
      defaultMode: 0440
  - name: qurl-proxy
    configMap:
      name: %s`, precedingContainers, args.imageYAML, args.endpointYAML, args.agentPVCYAML, args.secretYAML, args.configMapYAML)
}

type kubernetesTunnelNames struct {
	secret    string
	configMap string
	agentPVC  string
}

func kubernetesTunnelObjectNames(slug string) kubernetesTunnelNames {
	return kubernetesTunnelNames{
		secret:    kubernetesNameWithSlug("qurl-connector-", slug),
		configMap: kubernetesNameWithSlug("qurl-proxy-", slug),
		agentPVC:  kubernetesNameWithSlug("qurl-agent-", slug),
	}
}

func kubernetesNameWithSlug(prefix, slug string) string {
	name := prefix + slug
	if len(name) <= kubernetesNameMaxLen {
		return name
	}
	sum := sha256.Sum256([]byte(slug))
	hash := hex.EncodeToString(sum[:])[:kubernetesNameHashHexLen]
	maxSlugLen := kubernetesNameMaxLen - len(prefix) - 1 - len(hash)
	if maxSlugLen <= 0 {
		// Current qURL prefixes do not hit this path; keep the helper safe for
		// future callers that pass a longer Kubernetes object prefix.
		maxPrefixLen := kubernetesNameMaxLen - len(hash) - 1
		prefixBase := strings.TrimRight(prefix[:maxPrefixLen], "-")
		if prefixBase == "" {
			return hash
		}
		return prefixBase + "-" + hash
	}
	base := strings.TrimRight(slug[:maxSlugLen], "-")
	if base == "" {
		return prefix + hash
	}
	return prefix + base + "-" + hash
}
