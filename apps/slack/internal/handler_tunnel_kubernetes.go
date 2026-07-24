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
	quotedAuditPVC, err := yamlSingleQuoted(names.auditPVC)
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
	quotedAPIURL, err := yamlSingleQuoted(args.APIURL)
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
  qurl-proxy.yaml: |
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
QURL_K8S_YAML_EOF`, renderPortablePipefailShell(), shellSingleQuote(names.secret), renderBootstrapKeyPromptShell(), renderBootstrapKeyToCommandShell(`kubectl create secret generic "$QURL_BOOTSTRAP_SECRET" --from-file=api_key=/dev/stdin --dry-run=client -o yaml | kubectl apply -f -`), quotedConfigMap, indentLines(configYAML, 4), quotedAgentPVC, quotedAuditPVC)

	patch := renderKubernetesConnectorPodSpec(&kubernetesConnectorPodSpecArgs{
		imageYAML:     quotedImage,
		slugYAML:      quotedSlug,
		apiURLYAML:    quotedAPIURL,
		agentPVCYAML:  quotedAgentPVC,
		auditPVCYAML:  quotedAuditPVC,
		secretYAML:    quotedSecret,
		configMapYAML: quotedConfigMap,
	})

	objectsBlock, err := slackCodeBlock(objects)
	if err != nil {
		return "", err
	}
	patchBlock, err := slackCodeBlock(patch)
	if err != nil {
		return "", err
	}
	intro := strings.Join([]string{
		"Run this once in the target namespace, then add the init-container/sidecar/volumes block to the same pod spec as the target container so `127.0.0.1:" + strconv.Itoa(args.LocalPort) + "` reaches the local service.",
		"- Use one PVC per sidecar replica; if you scale replicas, use a StatefulSet with a volumeClaimTemplate instead of sharing this PVC.",
		"- The Connector uses separate state and audit PVCs. qurl-go rejects group-writable identity state, so do not add pod-level `fsGroup`; the permissions init container enforces owner-only state modes before each start.",
		"- Your admission policy must permit the two root init containers: volume permissions uses CHOWN, DAC_OVERRIDE, and FOWNER, while the one-time bootstrap copy uses CHOWN only. The long-running Connector remains nonroot, read-only-root, seccomp-confined, and capability-free.",
		"- The bootstrap key is streamed through your local shell into `kubectl`; do not run this from a shared, recorded, or command-traced terminal session. The apply pipeline briefly carries a generated Secret manifest between `kubectl` processes.",
		"- After the pod connects, create and roll out a warm-start workload revision that removes `qurl-bootstrap-copy`, both bootstrap volumes and their mounts, and `QURL_API_KEY_FILE`. Verify the replacement pod connects from its persisted state, then delete the bootstrap Secret; deleting it first prevents a replacement pod from starting.",
	}, "\n")
	return intro + "\n\n" + objectsBlock + "\n\nPod spec additions:\nAppend both generated init containers under your existing `initContainers:` list, append the `qurl-connector` container under `containers:`, and append the volumes under `volumes:`. Do not add pod-level `fsGroup` and do not duplicate existing YAML keys.\n\n" + patchBlock, nil
}

type kubernetesConnectorPodSpecArgs struct {
	precedingContainers string
	imageYAML           string
	slugYAML            string
	apiURLYAML          string
	agentPVCYAML        string
	auditPVCYAML        string
	secretYAML          string
	configMapYAML       string
}

func renderKubernetesConnectorPodSpec(args *kubernetesConnectorPodSpecArgs) string {
	precedingContainers := args.precedingContainers
	if precedingContainers != "" {
		precedingContainers += "\n"
	}
	return fmt.Sprintf(`initContainers:
  - name: qurl-volume-permissions
    image: %s
    command:
      - sh
      - -ceu
      - |
        find /state -type d -exec chmod 0700 '{}' ';'
        find /state -type f -exec chmod 0600 '{}' ';'
        chown -R 65532:65532 /state
        mkdir -p /audit/qurl-connector
        find /audit -type d -exec chmod 0750 '{}' ';'
        find /audit -type f -exec chmod 0640 '{}' ';'
        chown -R 65532:65532 /audit
        chown 65532:65532 /tmp-runtime
        chmod 0700 /tmp-runtime
    securityContext:
      runAsUser: 0
      runAsNonRoot: false
      readOnlyRootFilesystem: true
      allowPrivilegeEscalation: false
      capabilities:
        drop: ["ALL"]
        add: ["CHOWN", "DAC_OVERRIDE", "FOWNER"]
      seccompProfile:
        type: RuntimeDefault
    volumeMounts:
      - name: qurl-agent-state
        mountPath: /state
      - name: qurl-audit
        mountPath: /audit
      - name: qurl-tmp
        mountPath: /tmp-runtime
  - name: qurl-bootstrap-copy
    image: %s
    command:
      - sh
      - -ceu
      - |
        cp /bootstrap-source/api_key /bootstrap/api_key
        chmod 0400 /bootstrap/api_key
        chown 65532:65532 /bootstrap/api_key
    securityContext:
      runAsUser: 0
      runAsNonRoot: false
      readOnlyRootFilesystem: true
      allowPrivilegeEscalation: false
      capabilities:
        drop: ["ALL"]
        add: ["CHOWN"]
      seccompProfile:
        type: RuntimeDefault
    volumeMounts:
      - name: qurl-bootstrap-source
        mountPath: /bootstrap-source
        readOnly: true
      - name: qurl-bootstrap
        mountPath: /bootstrap
containers:
%s  - name: qurl-connector
    image: %s
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
      - name: QURL_API_KEY_FILE
        value: /run/secrets/qurl-connector/api_key
      - name: QURL_CONNECTOR_ID
        value: %s
      - name: QURL_API_URL
        value: %s
      - name: QURL_AUDIT_FILE
        value: /var/log/layerv/qurl-connector/audit.log
    volumeMounts:
      - name: qurl-tmp
        mountPath: /tmp
      - name: qurl-agent-state
        mountPath: /var/lib/layerv/agent
      - name: qurl-audit
        mountPath: /var/log/layerv
      - name: qurl-bootstrap
        mountPath: /run/secrets/qurl-connector
        readOnly: true
      - name: qurl-proxy
        mountPath: /work/qurl-proxy.yaml
        subPath: qurl-proxy.yaml
        readOnly: true
volumes:
  - name: qurl-tmp
    emptyDir:
      sizeLimit: 64Mi
  - name: qurl-agent-state
    persistentVolumeClaim:
      claimName: %s
  - name: qurl-audit
    persistentVolumeClaim:
      claimName: %s
  - name: qurl-bootstrap-source
    secret:
      secretName: %s
      # Mounted only into the root copy init; the runtime receives UID-65532 0400.
      defaultMode: 0400
  - name: qurl-bootstrap
    emptyDir:
      medium: Memory
      sizeLimit: 1Mi
  - name: qurl-proxy
    configMap:
      name: %s`, connectorVolumePermissionsImage, connectorVolumePermissionsImage, precedingContainers, args.imageYAML, args.slugYAML, args.apiURLYAML, args.agentPVCYAML, args.auditPVCYAML, args.secretYAML, args.configMapYAML)
}

type kubernetesTunnelNames struct {
	secret    string
	configMap string
	agentPVC  string
	auditPVC  string
}

func kubernetesTunnelObjectNames(slug string) kubernetesTunnelNames {
	return kubernetesTunnelNames{
		secret:    kubernetesNameWithSlug("qurl-connector-", slug),
		configMap: kubernetesNameWithSlug("qurl-proxy-", slug),
		agentPVC:  kubernetesNameWithSlug("qurl-agent-", slug),
		auditPVC:  kubernetesNameWithSlug("qurl-audit-", slug),
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
