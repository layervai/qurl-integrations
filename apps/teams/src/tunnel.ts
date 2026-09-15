import { createHash, createPrivateKey, createPublicKey, diffieHellman } from 'node:crypto';
import { isChannelAlias } from './alias.js';
import { UserFacingError } from './user-facing-error.js';

const slugPattern = /^[a-z][a-z0-9-]{1,62}[a-z0-9]$/;
const servicePattern = /^[A-Za-z0-9_-]{1,64}$/;
// The released qURL CLI image, digest-pinned by the deployment. `qurl-connector`
// is the retired standalone image and is rejected here so a stale deployment
// cannot render an install that will never enrol.
const imagePattern = /^ghcr\.io\/layervai\/qurl@sha256:[a-f0-9]{64}$/;
const hubHostPattern = /^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$/;
const hubKeyPattern = /^[A-Za-z0-9+/]{43}=$/;

export type TunnelEnvironment = 'docker' | 'compose' | 'ecs-fargate' | 'kubernetes';

/**
 * The NHP Hub the rendered Connector must knock against. All three values are
 * set together or not at all; a partial triple is a deployment error.
 */
export interface TunnelHub { readonly host: string; readonly port: string; readonly serverPublicKeyB64: string; }

export interface TunnelInstallArgs {
  readonly slug: string;
  readonly alias: string;
  readonly environment: TunnelEnvironment;
  readonly port: number;
  readonly service?: string;
  readonly image: string;
  readonly endpoint: string;
  /** Account owner from GET /v1/me. `owner_id` in the headless config. */
  readonly ownerId: string;
  readonly crid: string;
  readonly resourceId: string;
  readonly connectorRoutingId: string;
  readonly knockResourceId: string;
  readonly servingEpoch: number;
  readonly hub?: TunnelHub;
}

/**
 * Empty is allowed and means "not configured yet" -- `env('QURL_IMAGE')` and
 * `validateInstallArgs` both reject that separately, so this function only
 * answers "is this a usable ref if one is present".
 */
export function validateTunnelImageRef(image: string): void {
  if (image && !imagePattern.test(image)) {
    throw new UserFacingError('invalid connector image reference');
  }
}
export function normalizeTunnelEnvironment(environment: string): TunnelEnvironment {
  const value = environment.trim().toLowerCase();
  if (value === '' || value === 'docker') return 'docker';
  if (value === 'compose' || value === 'docker-compose') return 'compose';
  if (value === 'ecs-fargate') return 'ecs-fargate';
  if (value === 'kubernetes') return 'kubernetes';
  throw new UserFacingError('invalid connector environment');
}
export function validateTunnelSlug(slug: string): void { if (!slugPattern.test(slug)) throw new UserFacingError('connector id must be 3-64 lowercase letters, numbers, or hyphens'); }
export function validateTunnelService(service: string): void { if (service && !servicePattern.test(service)) throw new UserFacingError('connector service is invalid'); }

export function validateTunnelHub(hub: TunnelHub | undefined): void {
  if (!hub) return;
  // Shape only -- no domain allowlist and no fixed port. Which Hub this
  // environment trusts is infra's decision, validated by
  // `qurl_connector_hub_{host,port}` in qurl-bot-teams/terraform, exactly as
  // apps/slack does it: Slack's Go source pins no Hub domain either. Pinning
  // one here would also put our pre-prod domain in a PUBLIC repo, and would
  // silently reject a Hub the deployment legitimately configured.
  if (hub.host.length > 253 || hub.host !== hub.host.toLowerCase() || !hubHostPattern.test(hub.host)) {
    throw new UserFacingError('connector hub host is invalid');
  }
  if (!/^[1-9][0-9]{0,4}$/.test(hub.port) || Number(hub.port) > 65_535) throw new UserFacingError('connector hub port is invalid');
  try {
    const key = Buffer.from(hub.serverPublicKeyB64, 'base64');
    if (!hubKeyPattern.test(hub.serverPublicKeyB64) || key.toString('base64') !== hub.serverPublicKeyB64
      || BigInt(`0x${Buffer.from(key).reverse().toString('hex')}`) >= (1n << 255n) - 19n) throw new Error('noncanonical key');
    // TODO(upstream-contract): the CLI requires canonical, usable X25519 pins.
    // A fixed validation scalar is public test material, never an identity;
    // OpenSSL rejects low-order public keys during this native exchange.
    diffieHellman({
      privateKey: createPrivateKey({ key: Buffer.from('302e020100300506032b656e04220420' + '00'.repeat(32), 'hex'), format: 'der', type: 'pkcs8' }),
      publicKey: createPublicKey({ key: Buffer.concat([Buffer.from('302a300506032b656e032100', 'hex'), key]), format: 'der', type: 'spki' }),
    });
  } catch {
    throw new UserFacingError('connector hub server public key is invalid');
  }
}

function shellQuote(value: string): string { return `'${value.replaceAll("'", `'"'"'`)}'`; }

/**
 * YAML single-quoted scalar. Rejects control characters outright rather than
 * escaping them: every value here is a server-issued identifier, so a control
 * character means the response was malformed, not that we need to quote harder.
 */
function yamlQuote(value: string): string {
  const trimmed = value.trim();
  if (trimmed === '') throw new UserFacingError('connector install metadata is incomplete');
  // eslint-disable-next-line no-control-regex
  if (/[\x00-\x1f\x7f]/.test(trimmed)) throw new UserFacingError('connector install metadata is invalid');
  return `'${trimmed.replaceAll("'", "''")}'`;
}

/**
 * Origin the daemon talks to (QURL_ENDPOINT). The bot holds an API origin; the
 * daemon wants the same origin without a path.
 */
function endpointOrigin(raw: string): string {
  let parsed: URL;
  try {
    parsed = new URL(raw);
  } catch {
    throw new UserFacingError('connector endpoint is invalid');
  }
  if (parsed.protocol !== 'https:' || parsed.username || parsed.password || parsed.search || parsed.hash) {
    throw new UserFacingError('connector endpoint is invalid');
  }
  return parsed.origin;
}

/**
 * The one-share headless config the daemon reads (`--headless-config`).
 *
 * TODO(upstream-contract): mirrors the qurl CLI headless share schema
 * (`version: 2` + top-level `owner_id`, qurl >= v2.1) and apps/slack's
 * renderTunnelConfigYAML. Keep all three in lockstep.
 */
export function renderTunnelConfigYAML(args: TunnelInstallArgs): string {
  if (!args.servingEpoch) throw new UserFacingError('connector sharing has not been started');
  const owner = yamlQuote(args.ownerId);
  const crid = yamlQuote(args.crid);
  const resourceId = yamlQuote(args.resourceId);
  const connectorId = yamlQuote(args.slug);
  const routingId = yamlQuote(args.connectorRoutingId);
  const knockId = yamlQuote(args.knockResourceId);
  const targetUrl = yamlQuote(`http://127.0.0.1:${args.port}`);
  return `version: 2
owner_id: ${owner}
shares:
  - crid: ${crid}
    resource_id: ${resourceId}
    connector_id: ${connectorId}
    connector_routing_id: ${routingId}
    knock_resource_id: ${knockId}
    target_url: ${targetUrl}
    local_ip: 127.0.0.1
    local_port: ${args.port}
    desired_state: on
    serving_epoch: ${args.servingEpoch}`;
}

function validateInstallArgs(args: TunnelInstallArgs): void {
  validateTunnelSlug(args.slug);
  if (!isChannelAlias(args.alias)) throw new UserFacingError('connector alias is invalid');
  if (!Number.isInteger(args.port) || args.port < 1 || args.port > 65_535) throw new UserFacingError('connector port is invalid');
  if (!args.image) throw new UserFacingError('connector image is not configured');
  validateTunnelImageRef(args.image);
  validateTunnelService(args.service ?? '');
  validateTunnelHub(args.hub);
}

/**
 * qurl-service enrollment tokens must be printable single-line ASCII so the
 * rendered secret message and the operator's paste stay byte-identical across
 * shells and locales. Mirrors apps/slack's validateBootstrapAPIKeyForShell.
 */
export function validateBootstrapKey(apiKey: string): void {
  if (!apiKey) throw new UserFacingError('enrollment token is missing');
  if (!/^[\x21-\x7e]+$/.test(apiKey)) throw new UserFacingError('enrollment token is not renderable');
}

/**
 * The one-time token, delivered as its own message. The install instructions
 * are sent separately and deliberately never contain it, so this is the only
 * thing that has to be treated as a secret in chat history.
 */
export function renderTunnelBootstrapSecretMessage(slug: string, bootstrapKey: string): string {
  validateTunnelSlug(slug);
  validateBootstrapKey(bootstrapKey);
  return [
    `Temporary qURL Connector enrollment token for \`${slug}\` (expires in 15 minutes).`,
    '',
    'Paste this only when the install instructions prompt for it, or store it in your platform\'s secret manager. The install instructions were sent separately and intentionally do not include this token.',
    '',
    '```',
    bootstrapKey,
    '```',
    '',
    'After the Connector connects, remove the enrollment token from the runtime and delete this message when your retention policy allows.',
  ].join('\n');
}

// The image runs as UID/GID 65532, matching Slack's Connector installs.
// Mirrors apps/slack's renderBootstrapKeyPromptShell. The TTY guard is the
// load-bearing part: every block below runs under `set -euo pipefail`, so a
// bare `stty` against a non-terminal stdin (piped block, `ssh host bash -s`,
// paste into a non-interactive shell) aborts with `stty: stdin isn't a
// terminal` and no hint that a TTY was required. Saving and restoring the full
// termios state -- rather than a blanket `stty echo` -- means an interrupted
// run leaves the terminal exactly as it was found, and the trap is armed only
// after the state is captured so there is no window where echo is off with no
// restore path.
const tokenPrompt = `if [ ! -t 0 ]; then
  echo 'Run this block from an interactive terminal: it prompts for the enrollment token.' >&2
  exit 1
fi
printf 'Enrollment token (input hidden): ' >&2
STTY_STATE="$(stty -g 2>/dev/null | tr -d '[:space:]' || true)"
if [ -n "$STTY_STATE" ]; then
  stty -echo
  trap 'stty "$STTY_STATE" 2>/dev/null || true' INT TERM EXIT
fi
if ! IFS= read -r QURL_ENROLLMENT_TOKEN; then
  QURL_ENROLLMENT_TOKEN=''
fi
if [ -n "$STTY_STATE" ]; then
  stty "$STTY_STATE" 2>/dev/null || true
  trap - INT TERM EXIT
fi
printf '\\n' >&2
[ -n "$QURL_ENROLLMENT_TOKEN" ] || { echo 'Enrollment token is required.' >&2; exit 1; }`;

function hostSetup(args: TunnelInstallArgs, config: string): string {
  return `SUDO=''
[ "$(id -u)" -eq 0 ] || SUDO='sudo'
QURL_CONNECTOR_ID=${shellQuote(args.slug)}
SECRET_DIR="/run/secrets/qurl/\${QURL_CONNECTOR_ID}"
AGENT_STATE_DIR="/var/lib/layerv/qurl/\${QURL_CONNECTOR_ID}"
CONFIG_FILE="$PWD/qurl-share-\${QURL_CONNECTOR_ID}.yaml"
cat > "$CONFIG_FILE" <<'QURL_SHARE_YAML_EOF'
${config}
QURL_SHARE_YAML_EOF
$SUDO chmod 0644 "$CONFIG_FILE"
$SUDO chown 65532:65532 "$CONFIG_FILE"
$SUDO install -d -m 0700 -o 65532 -g 65532 "$SECRET_DIR"
$SUDO install -d -m 0700 -o 65532 -g 65532 "$AGENT_STATE_DIR"
${tokenPrompt}
printf '%s' "$QURL_ENROLLMENT_TOKEN" | $SUDO tee "$SECRET_DIR/enrollment-token" >/dev/null
unset QURL_ENROLLMENT_TOKEN
$SUDO chmod 0400 "$SECRET_DIR/enrollment-token"
$SUDO chown 65532:65532 "$SECRET_DIR/enrollment-token"`;
}

function kubernetesName(prefix: string, slug: string): string {
  const name = `${prefix}${slug}`;
  return name.length <= 63 ? name : `${name.slice(0, 54).replace(/-+$/, '')}-${createHash('sha256').update(slug).digest('hex').slice(0, 8)}`;
}

export function renderTunnelInstallMessage(args: TunnelInstallArgs): string {
  validateInstallArgs(args);
  const config = renderTunnelConfigYAML(args);
  const endpoint = endpointOrigin(args.endpoint);
  const image = args.image;
  // The strict headless YAML has no Hub field. The CLI resolves this optional
  // environment triple, or its embedded production trust pin when omitted.
  const hubEnvironment = args.hub ? [
    { name: 'QURL_CONNECTOR_HUB_HOST', value: args.hub.host },
    { name: 'QURL_CONNECTOR_HUB_PORT', value: args.hub.port },
    { name: 'QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64', value: args.hub.serverPublicKeyB64 },
  ] : [];

  switch (args.environment) {
    case 'compose': {
      const service = args.service || 'YOUR_COMPOSE_SERVICE_NAME';
      return `Docker Compose: run this Bash block from your project directory on the Linux Docker host. Set WEB_SERVICE to your web service first; set APP_COMPOSE_FILE if your app file is not compose.yaml.

\`\`\`bash
set -euo pipefail
WEB_SERVICE=${shellQuote(service)}
[ "$WEB_SERVICE" != YOUR_COMPOSE_SERVICE_NAME ] || { echo 'Set WEB_SERVICE to your Compose web service.' >&2; exit 1; }
APP_COMPOSE_FILE=\${APP_COMPOSE_FILE:-compose.yaml}
${hostSetup(args, config)}
QURL_ENDPOINT_YAML=${shellQuote(JSON.stringify(endpoint))}
COMPOSE_FILE="$PWD/qurl-\${QURL_CONNECTOR_ID}.compose.yaml"
cat > "$COMPOSE_FILE" <<QURL_COMPOSE_EOF
services:
  qurl-${args.slug}:
    image: ${JSON.stringify(image)}
    user: "65532:65532"
    network_mode: service:\${WEB_SERVICE}
    depends_on:
      \${WEB_SERVICE}:
        condition: service_started
    restart: unless-stopped
    read_only: true
    tmpfs: ["/tmp:rw,size=64m"]
    pids_limit: 512
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    environment:
      QURL_ENDPOINT: \${QURL_ENDPOINT_YAML}${hubEnvironment.map(({ name, value }) => `\n      ${name}: ${JSON.stringify(value)}`).join('')}
    volumes:
      - \${AGENT_STATE_DIR}:/var/lib/qurl
      - \${SECRET_DIR}:/run/secrets/qurl:ro
      - ./qurl-share-${args.slug}.yaml:/etc/qurl/share.yaml:ro
    entrypoint: /usr/local/bin/qurl
    command:
      - daemon
      - run
      - --state-dir
      - /var/lib/qurl
      - --headless-config
      - /etc/qurl/share.yaml
      - --enrollment-token-file
      - /run/secrets/qurl/enrollment-token
QURL_COMPOSE_EOF
docker compose -f "$APP_COMPOSE_FILE" -f "$COMPOSE_FILE" up -d "qurl-\${QURL_CONNECTOR_ID}"
\`\`\`
After qURL connects, delete the enrollment-token file; warm restarts use the persistent state directory. Restart this sidecar after Compose recreates the web container.`;
    }
    case 'ecs-fargate':
      return `ECS/Fargate: add the sidecar to the same awsvpc task as your web container so localhost:${args.port} reaches it.

1. Create three EFS-backed task volumes named qurl-agent-state, qurl-config, and qurl-bootstrap. Use separate access points. Set the state access point's POSIX UID/GID to 65532:65532 with a writable root; qURL creates its private nested state directory. Do not share state across concurrently running sidecars. Stop the old task before a replacement mounts the same state (for an ECS service, use deployment maximumPercent 100 and minimumHealthyPercent 0).
2. Write the separately delivered enrollment token to enrollment-token on qurl-bootstrap, owned by 65532:65532 with mode 0400 (or root:65532 with mode 0440). Write this config to share.yaml on qurl-config, owned by root or UID 65532 with mode 0644; neither file may be writable by group or other users:
\`\`\`yaml
${config}
\`\`\`
3. Add this container. Replace REGION with your AWS region and create its CloudWatch log group:
\`\`\`json
${JSON.stringify({
        name: 'qurl', image, user: '65532:65532', essential: false,
        readonlyRootFilesystem: true,
        entryPoint: ['/usr/local/bin/qurl'],
        command: ['daemon', 'run', '--state-dir', '/var/lib/qurl-volume/state', '--headless-config', '/etc/qurl/share.yaml', '--enrollment-token-file', '/run/secrets/qurl/enrollment-token'],
        environment: [{ name: 'QURL_ENDPOINT', value: endpoint }, ...hubEnvironment],
        mountPoints: [
          { sourceVolume: 'qurl-agent-state', containerPath: '/var/lib/qurl-volume', readOnly: false },
          { sourceVolume: 'qurl-config', containerPath: '/etc/qurl', readOnly: true },
          { sourceVolume: 'qurl-bootstrap', containerPath: '/run/secrets/qurl', readOnly: true },
        ],
        linuxParameters: { capabilities: { drop: ['ALL'] } },
        restartPolicy: { enabled: true, restartAttemptPeriod: 60 },
        logConfiguration: { logDriver: 'awslogs', options: { 'awslogs-group': '/ecs/qurl-connector', 'awslogs-region': 'REGION', 'awslogs-stream-prefix': 'qurl' } },
      }, null, 2)}
\`\`\`
After qURL connects, deploy a warm-start revision without \`--enrollment-token-file\` and its argument or the qurl-bootstrap mount. Verify the replacement task reconnects from persisted state, then delete the enrollment-token file.`;

    case 'kubernetes': {
      const configName = kubernetesName('qurl-share-', args.slug);
      const secretName = kubernetesName('qurl-enrollment-', args.slug);
      const stateName = kubernetesName('qurl-state-', args.slug);
      return `Kubernetes: run qURL as a sidecar in your web app's Pod so it shares the Pod network. Run this Bash block once in the target namespace:

\`\`\`bash
set -euo pipefail
${tokenPrompt}
printf '%s' "$QURL_ENROLLMENT_TOKEN" | kubectl create secret generic ${secretName} --from-file=enrollment-token=/dev/stdin --dry-run=client -o yaml | kubectl apply -f -
unset QURL_ENROLLMENT_TOKEN
kubectl apply -f - <<'QURL_K8S_EOF'
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${configName}
data:
  share.yaml: |
${config.split('\n').map(line => `    ${line}`).join('\n')}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${stateName}
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
QURL_K8S_EOF
\`\`\`
Merge this securityContext, sidecar, and volumes into the web Pod template without duplicating existing keys. Use a single-replica Deployment with strategy Recreate so old and new Pods never share one identity concurrently. For multiple replicas use a StatefulSet with one PVC per replica through volumeClaimTemplates.
\`\`\`yaml
securityContext:
  fsGroup: 65532
  fsGroupChangePolicy: OnRootMismatch
containers:
  - name: qurl
    image: ${JSON.stringify(image)}
    env:
      - name: QURL_ENDPOINT
        value: ${JSON.stringify(endpoint)}${hubEnvironment.map(({ name, value }) => `\n      - name: ${name}\n        value: ${JSON.stringify(value)}`).join('')}
    securityContext:
      runAsUser: 65532
      runAsGroup: 65532
      runAsNonRoot: true
      readOnlyRootFilesystem: true
      allowPrivilegeEscalation: false
      capabilities:
        drop: [ALL]
      seccompProfile:
        type: RuntimeDefault
    command: ["/usr/local/bin/qurl"]
    args: ["daemon", "run", "--state-dir", "/var/lib/qurl-volume/state", "--headless-config", "/etc/qurl/share.yaml", "--enrollment-token-file", "/run/secrets/qurl/enrollment-token"]
    volumeMounts:
      - { name: qurl-tmp, mountPath: /tmp }
      - { name: qurl-state, mountPath: /var/lib/qurl-volume }
      - { name: qurl-share, mountPath: /etc/qurl/share.yaml, subPath: share.yaml, readOnly: true }
      - { name: qurl-enrollment, mountPath: /run/secrets/qurl, readOnly: true }
volumes:
  - name: qurl-tmp
    emptyDir:
      sizeLimit: 64Mi
  - name: qurl-state
    persistentVolumeClaim:
      claimName: ${stateName}
  - name: qurl-share
    configMap:
      name: ${configName}
  - name: qurl-enrollment
    secret:
      secretName: ${secretName}
      defaultMode: 0440
\`\`\`
After qURL connects, deploy a warm-start revision without \`--enrollment-token-file\` and its argument, the qurl-enrollment volume, and its mount. Verify the replacement Pod reconnects from persisted state, then delete the \`${secretName}\` Secret.`;
    }
    default: {
      const webContainer = args.service ? shellQuote(args.service) : shellQuote('YOUR_WEB_CONTAINER_NAME');
      return `Run this whole Bash block on the Linux Docker host where your HTTP server container runs. Set WEB_CONTAINER first. It prompts for the enrollment token so the secret never lands in shell history.

\`\`\`bash
set -euo pipefail
WEB_CONTAINER=${webContainer}
[ "$WEB_CONTAINER" != YOUR_WEB_CONTAINER_NAME ] || { echo 'Set WEB_CONTAINER to your web container name or ID.' >&2; exit 1; }
${hostSetup(args, config)}
CONNECTOR_CONTAINER="qurl-\${QURL_CONNECTOR_ID}"
if docker ps -a --format '{{.Names}}' | grep -Fxq "$CONNECTOR_CONTAINER"; then
  docker rm -f "$CONNECTOR_CONTAINER" >/dev/null
fi

docker run -d \\
  --name "$CONNECTOR_CONTAINER" \\
  --network "container:\${WEB_CONTAINER}" \\
  --restart=unless-stopped \\
  --user 65532:65532 \\
  --read-only \\
  --tmpfs /tmp:rw,size=64m \\
  --cap-drop=ALL \\
  --security-opt=no-new-privileges:true \\
  --pids-limit=512 \\
  -v "$AGENT_STATE_DIR:/var/lib/qurl" \\
  -v "$SECRET_DIR:/run/secrets/qurl:ro" \\
  -v "$CONFIG_FILE:/etc/qurl/share.yaml:ro" \\
  -e QURL_ENDPOINT=${shellQuote(endpoint)} \\
${hubEnvironment.map(({ name, value }) => `  -e ${shellQuote(`${name}=${value}`)} \\\n`).join('')}  --entrypoint /usr/local/bin/qurl \\
  ${shellQuote(image)} daemon run \\
    --state-dir /var/lib/qurl \\
    --headless-config /etc/qurl/share.yaml \\
    --enrollment-token-file /run/secrets/qurl/enrollment-token
\`\`\`
Verify with \`docker logs -f qurl-${args.slug}\`; after qURL connects, delete the enrollment-token file. Warm restarts use the persisted state volume and do not need it. Restart qURL after replacing or recreating the web container.`;
    }
  }
}
