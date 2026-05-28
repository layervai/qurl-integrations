package internal

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/layervai/qurl-integrations/shared/client"
)

func renderKubernetesTunnelInstructions(args *tunnelInstallArgs, _ *client.APIKey, image string) string {
	names := kubernetesTunnelObjectNames(args.Slug)
	objects := fmt.Sprintf(`set -eu
%s

QURL_BOOTSTRAP_SECRET=%s
%s
printf '%%s' "$QURL_BOOTSTRAP_KEY" | kubectl create secret generic "$QURL_BOOTSTRAP_SECRET" --from-file=api_key=/dev/stdin --dry-run=client -o yaml | kubectl apply -f -
unset QURL_BOOTSTRAP_KEY

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
QURL_K8S_YAML_EOF`, renderPortablePipefailShell(), shellSingleQuote(names.secret), renderBootstrapKeyPromptShell(), names.configMap, indentLines(renderTunnelConfigYAML(args), 4), names.agentPVC)

	patch := fmt.Sprintf(`securityContext:
  fsGroup: 65532
  fsGroupChangePolicy: OnRootMismatch
containers:
  - name: qurl-tunnel
    image: %s
    securityContext:
      runAsUser: 65532
      runAsGroup: 65532
      runAsNonRoot: true
      allowPrivilegeEscalation: false
      capabilities:
        drop: ["ALL"]
      seccompProfile:
        type: RuntimeDefault
    env:
      - name: QURL_API_KEY_FILE
        value: /run/secrets/qurl-tunnel/api_key
      - name: QURL_TUNNEL_SLUG
        value: %s
    volumeMounts:
      - name: qurl-agent-state
        mountPath: /var/lib/layerv/agent
      - name: qurl-bootstrap
        mountPath: /run/secrets/qurl-tunnel
        readOnly: true
      - name: qurl-proxy
        mountPath: /work/qurl-proxy.yaml
        subPath: qurl-proxy.yaml
        readOnly: true
volumes:
  - name: qurl-agent-state
    persistentVolumeClaim:
      claimName: %s
  - name: qurl-bootstrap
    secret:
      secretName: %s
      defaultMode: 0440
  - name: qurl-proxy
    configMap:
      name: %s`, yamlSingleQuoted(image), yamlSingleQuoted(args.Slug), names.agentPVC, names.secret, names.configMap)

	intro := strings.Join([]string{
		"Run this once in the target namespace, then add the sidecar/securityContext/volumes block to the same pod spec as the target container so `127.0.0.1:" + strconv.Itoa(args.LocalPort) + "` reaches the local service.",
		"- Use one PVC per sidecar replica; if you scale replicas, use a StatefulSet with a volumeClaimTemplate instead of sharing this PVC.",
		"- The fragment is compatible with Kubernetes Pod Security Admission `restricted`: no root initContainer, `runAsNonRoot: true`, `seccompProfile: RuntimeDefault`, and all capabilities dropped.",
		"- The pod-level `fsGroup: 65532` lets the sidecar read the bootstrap Secret and write the qURL agent-state PVC. If your app cannot accept that fsGroup, pre-provision qURL agent-state ownership separately before merging the fragment.",
		"- Delete the bootstrap Secret after the pod logs show the tunnel connected.",
	}, "\n")
	return intro + "\n\n" + slackCodeBlock(objects) + "\n\nPod spec additions:\nMerge this fragment into the target pod spec under the same indentation as the existing `containers` and `volumes` fields.\n\n" + slackCodeBlock(patch)
}

type kubernetesTunnelNames struct {
	secret    string
	configMap string
	agentPVC  string
}

func kubernetesTunnelObjectNames(slug string) kubernetesTunnelNames {
	return kubernetesTunnelNames{
		secret:    kubernetesNameWithSlug("qurl-tunnel-", slug),
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
	hash := hex.EncodeToString(sum[:kubernetesNameHashLen/2])
	maxSlugLen := kubernetesNameMaxLen - len(prefix) - 1 - len(hash)
	if maxSlugLen <= 0 {
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
