import { execFileSync } from 'node:child_process';
import { describe, expect, it } from 'vitest';
import { normalizeTunnelEnvironment, renderTunnelBootstrapSecretMessage, renderTunnelConfigYAML, renderTunnelInstallMessage, validateTunnelHub, validateTunnelImageRef, validateTunnelSlug } from '../src/tunnel.js';

const IMAGE = 'ghcr.io/layervai/qurl@sha256:d2f9bd33572ffb7212f5b6cfc3fcfa4267344a4a2c2cdd6ce9b7196e3515516b';

const base = {
  slug: 'prod',
  alias: 'prod',
  environment: 'docker' as const,
  port: 8080,
  image: IMAGE,
  endpoint: 'https://api.layerv.xyz',
  ownerId: 'auth0|user123',
  crid: 'crid_abc123',
  resourceId: 'res_abc123',
  connectorRoutingId: 'routing_abc123',
  knockResourceId: 'knock_abc123',
  servingEpoch: 7,
} as const;

const hub = { host: 'hub.nhp.layerv.xyz', port: '443', serverPublicKeyB64: 'CQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=' } as const;

describe('connector tunnel rendering', () => {
  it('validates slugs, image references, aliases, ports, and services', () => {
    expect(() => validateTunnelSlug('prod')).not.toThrow();
    expect(() => validateTunnelSlug('bad_slug')).toThrow('connector id');
    expect(() => renderTunnelInstallMessage({ ...base, alias: 'bad_alias' })).toThrow('connector alias');
    expect(() => renderTunnelInstallMessage({ ...base, port: 0 })).toThrow('connector port');
    expect(() => renderTunnelInstallMessage({ ...base, service: 'bad service' })).toThrow('connector service');
  });

  it('accepts only the released qURL CLI image', () => {
    expect(() => validateTunnelImageRef(IMAGE)).not.toThrow();
    // The retired standalone connector image must not render: it cannot enrol
    // against the current control plane.
    expect(() => validateTunnelImageRef('ghcr.io/layervai/qurl-connector@sha256:' + 'e'.repeat(64))).toThrow('invalid connector image');
    expect(() => validateTunnelImageRef('ghcr.io/layervai/qurl:v2.4.0')).toThrow('invalid connector image');
    expect(() => validateTunnelImageRef('registry.example/qurl:1;bad')).toThrow('invalid connector image');
    expect(() => renderTunnelInstallMessage({ ...base, image: '' })).toThrow('connector image is not configured');
  });

  it('normalizes supported deployment environments', () => {
    expect(normalizeTunnelEnvironment('docker')).toBe('docker');
    expect(normalizeTunnelEnvironment('docker-compose')).toBe('compose');
    expect(normalizeTunnelEnvironment('ecs-fargate')).toBe('ecs-fargate');
    expect(normalizeTunnelEnvironment('kubernetes')).toBe('kubernetes');
    expect(() => normalizeTunnelEnvironment('unknown')).toThrow('invalid connector environment');
  });

  it('renders the v2 headless share config the daemon reads', () => {
    const yaml = renderTunnelConfigYAML(base);
    expect(yaml).toContain('version: 2');
    expect(yaml).toContain("owner_id: 'auth0|user123'");
    expect(yaml).toContain("crid: 'crid_abc123'");
    expect(yaml).toContain("resource_id: 'res_abc123'");
    expect(yaml).toContain("connector_id: 'prod'");
    expect(yaml).toContain("connector_routing_id: 'routing_abc123'");
    expect(yaml).toContain("knock_resource_id: 'knock_abc123'");
    expect(yaml).toContain("target_url: 'http://127.0.0.1:8080'");
    expect(yaml).toContain('desired_state: on');
    expect(yaml).toContain('serving_epoch: 7');
  });

  it('refuses to render a config before sharing has started', () => {
    // serving_epoch 0 means sharing was never started; a daemon handed that
    // config is refused by the control plane, so fail here instead.
    expect(() => renderTunnelConfigYAML({ ...base, servingEpoch: 0 })).toThrow('sharing has not been started');
  });

  it('rejects incomplete or malformed install metadata', () => {
    expect(() => renderTunnelConfigYAML({ ...base, ownerId: '  ' })).toThrow('metadata is incomplete');
    expect(() => renderTunnelConfigYAML({ ...base, crid: 'bad\ncrid' })).toThrow('metadata is invalid');
  });

  it('passes custom Hub settings through the daemon environment, never share YAML', () => {
    expect(renderTunnelConfigYAML({ ...base, hub })).toBe(renderTunnelConfigYAML(base));
    for (const environment of ['docker', 'compose', 'ecs-fargate', 'kubernetes'] as const) {
      const text = renderTunnelInstallMessage({ ...base, environment, hub });
      expect(text).toContain('QURL_CONNECTOR_HUB_HOST');
      expect(text).toContain('QURL_CONNECTOR_HUB_PORT');
      expect(text).toContain('QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64');
      expect(text).toContain(hub.host);
      expect(text).toContain(hub.serverPublicKeyB64);
      expect(renderTunnelInstallMessage({ ...base, environment })).not.toContain('QURL_CONNECTOR_HUB_HOST');
    }
    const ecs = renderTunnelInstallMessage({ ...base, environment: 'ecs-fargate', hub });
    const container = JSON.parse(ecs.match(/```json\n([\s\S]*?)\n```/)?.[1] ?? '');
    expect(container.environment).toContainEqual({ name: 'QURL_CONNECTOR_HUB_HOST', value: hub.host });
  });

  it('validates the Hub triple', () => {
    expect(() => validateTunnelHub(undefined)).not.toThrow();
    expect(() => validateTunnelHub(hub)).not.toThrow();
    expect(() => validateTunnelHub({ ...hub, host: 'not a host' })).toThrow('hub host');
    expect(() => validateTunnelHub({ ...hub, port: '70000' })).toThrow('hub port');
    expect(() => validateTunnelHub({ ...hub, serverPublicKeyB64: 'short' })).toThrow('hub server public key');
  });

  it('rejects Hub settings the CLI cannot use as a pinned trust root', () => {
    for (const host of ['hub.example.com', 'HUB.nhp.layerv.xyz', 'hub.nhp.layerv.xyz.', '127.0.0.1', 'layerv.ai']) {
      expect(() => validateTunnelHub({ ...hub, host })).toThrow('hub host');
    }
    for (const port of ['80', '0443', '+443']) expect(() => validateTunnelHub({ ...hub, port })).toThrow('hub port');
    const invalidKeys = [
      'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=', // low-order zero
      'AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=', // low-order one
      'CQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAB=', // noncanonical base64 padding
      Buffer.from('ed' + 'ff'.repeat(30) + '7f', 'hex').toString('base64'), // field prime
      Buffer.from('09' + '00'.repeat(30) + '80', 'hex').toString('base64'), // high bit set
    ];
    for (const serverPublicKeyB64 of invalidKeys) expect(() => validateTunnelHub({ ...hub, serverPublicKeyB64 })).toThrow('hub server public key');
  });

  it('renders a docker install that runs the CLI daemon, not the retired connector env contract', () => {
    const docker = renderTunnelInstallMessage({ ...base, environment: 'docker' });
    expect(docker).toContain('--entrypoint /usr/local/bin/qurl');
    expect(docker).toContain('daemon run');
    expect(docker).toContain('--headless-config /etc/qurl/share.yaml');
    expect(docker).toContain('--enrollment-token-file /run/secrets/qurl/enrollment-token');
    expect(docker).toContain("-e QURL_ENDPOINT='https://api.layerv.xyz'");
    expect(docker).toContain(IMAGE);
    // Hardening flags must survive; these are the difference between a demo
    // and a root-capable container on a customer host.
    expect(docker).toContain('--read-only');
    expect(docker).toContain('--cap-drop=ALL');
    expect(docker).toContain('--security-opt=no-new-privileges:true');
    // The fabricated contract must be gone entirely.
    expect(docker).not.toContain('QURL_BOOTSTRAP_KEY');
    expect(docker).not.toContain('QURL_LOCAL_PORT');
    expect(docker).not.toContain('qurl-connector');
  });

  it('never renders the one-time enrollment token into any install target', () => {
    // The token is prompted for or placed in a secret store by the operator,
    // and delivered as its own message. Rendering it inline would put it in
    // Teams history attached to instructions nobody needs to delete.
    const token = 'lv_live_abc123';
    for (const environment of ['docker', 'compose', 'ecs-fargate', 'kubernetes'] as const) {
      expect(renderTunnelInstallMessage({ ...base, environment })).not.toContain(token);
    }
  });

  it('delivers the enrollment token as its own message', () => {
    const message = renderTunnelBootstrapSecretMessage('prod', 'lv_live_abc123');
    expect(message).toContain('lv_live_abc123');
    expect(message).toContain('expires in 15 minutes');
    expect(message).toContain('sent separately');
    // No install content here: this is the one message that carries a secret.
    expect(message).not.toContain('docker run');
  });

  it('refuses to render an unusable or missing enrollment token', () => {
    expect(() => renderTunnelBootstrapSecretMessage('prod', '')).toThrow('enrollment token is missing');
    expect(() => renderTunnelBootstrapSecretMessage('prod', 'has space')).toThrow('not renderable');
    expect(() => renderTunnelBootstrapSecretMessage('prod', 'line\nbreak')).toThrow('not renderable');
  });

  it('renders every deployment target with the daemon contract', () => {
    const compose = renderTunnelInstallMessage({ ...base, environment: 'compose' });
    expect(compose).toContain('network_mode: service:${WEB_SERVICE}');
    expect(compose).toContain('- --headless-config');

    const ecs = renderTunnelInstallMessage({ ...base, environment: 'ecs-fargate' });
    expect(ecs).toContain('"entryPoint"');
    expect(ecs).toContain('"/usr/local/bin/qurl"');
    expect(ecs).toContain('"QURL_ENDPOINT"');

    const k8s = renderTunnelInstallMessage({ ...base, environment: 'kubernetes' });
    expect(k8s).toContain('readOnlyRootFilesystem: true');
    expect(k8s).toContain('--enrollment-token-file');
  });

  it('executes the generated docker invocation as one command with the intended arguments', () => {
    const text = renderTunnelInstallMessage({ ...base, service: 'web', hub });
    const command = text.slice(text.indexOf('docker run -d'), text.lastIndexOf('```')).trim();
    const output = execFileSync('bash', ['-eu', '-c', `docker() { printf '%s\\n' "$@"; }; CONNECTOR_CONTAINER=qurl-prod; WEB_CONTAINER=web; AGENT_STATE_DIR=/state; SECRET_DIR=/secret; CONFIG_FILE=/config; ${command}`], { encoding: 'utf8' });
    const args = output.trim().split('\n');
    expect(args).toContain(IMAGE);
    expect(args).toContain('container:web');
    expect(args).toContain(`QURL_CONNECTOR_HUB_HOST=${hub.host}`);
    expect(args).toContain(`QURL_CONNECTOR_HUB_SERVER_PUBLIC_KEY_B64=${hub.serverPublicKeyB64}`);
    expect(args).toContain('/secret:/run/secrets/qurl:ro');
    expect(args).not.toContain('\\');
  });

  it('prepares compose bind mounts for the nonroot daemon and starts the service', () => {
    const text = renderTunnelInstallMessage({ ...base, environment: 'compose', service: 'web' });
    expect(text).toContain('install -d -m 0700 -o 65532 -g 65532');
    expect(text).toContain('chown 65532:65532 "$CONFIG_FILE"');
    expect(text).toContain('docker compose');
    expect(text).toContain('up -d');
    expect(text).toContain('services:');
    const script = text.slice(text.indexOf('cat > "$COMPOSE_FILE"'), text.indexOf('\n```', text.indexOf('cat > "$COMPOSE_FILE"')));
    const output = execFileSync('bash', ['-eu', '-c', `docker() { cat "$COMPOSE_FILE"; printf '%s\\n' "$@"; }; COMPOSE_FILE=$(mktemp); trap 'rm -f "$COMPOSE_FILE"' EXIT; APP_COMPOSE_FILE=compose.yaml; WEB_SERVICE=web; AGENT_STATE_DIR=/state; SECRET_DIR=/secret; QURL_CONNECTOR_ID=prod; QURL_ENDPOINT_YAML='"https://api.layerv.xyz"'; ${script}`], { encoding: 'utf8' });
    expect(output).toContain('network_mode: service:web');
    expect(output).toContain('/state:/var/lib/qurl');
    expect(output).toContain('/secret:/run/secrets/qurl:ro');
    expect(output).toContain('QURL_ENDPOINT: "https://api.layerv.xyz"');
    expect(output).toContain('up\n-d\nqurl-prod');
  });

  it('supplies ECS state/config/bootstrap mounts and a warm restart path', () => {
    const text = renderTunnelInstallMessage({ ...base, environment: 'ecs-fargate' });
    const block = text.match(/```json\n([\s\S]*?)\n```/)?.[1] ?? '';
    const container = JSON.parse(block);
    expect(container.user).toBe('65532:65532');
    expect(container.mountPoints).toEqual(expect.arrayContaining([
      expect.objectContaining({ containerPath: '/var/lib/qurl-volume', readOnly: false }),
      expect.objectContaining({ containerPath: '/etc/qurl', readOnly: true }),
      expect.objectContaining({ containerPath: '/run/secrets/qurl', readOnly: true }),
    ]));
    expect(container.command).toContain('/var/lib/qurl-volume/state');
    expect(container.restartPolicy.enabled).toBe(true);
    expect(text).toContain('EFS');
    expect(text).toContain('0400');
    expect(text).toContain('0644');
    expect(text).toContain('warm-start revision');
    expect(text).toContain('without `--enrollment-token-file`');
  });

  it('supplies Kubernetes volumes, private persistent state, and removable enrollment mounts', () => {
    const text = renderTunnelInstallMessage({ ...base, environment: 'kubernetes' });
    expect(text).toContain('kind: PersistentVolumeClaim');
    expect(text).toContain('fsGroup: 65532');
    expect(text).toContain('volumes:');
    expect(text).toContain('persistentVolumeClaim:');
    expect(text).toContain('configMap:');
    expect(text).toContain('secret:');
    expect(text).toContain('/var/lib/qurl-volume/state');
    expect(text).toContain('--from-file=enrollment-token=/dev/stdin');
    expect(text).not.toContain('--from-literal');
    expect(text).toContain('warm-start revision');
    expect(text).toContain('without `--enrollment-token-file`');
  });

  it('uses valid Kubernetes container names for maximum-length connector slugs', () => {
    const text = renderTunnelInstallMessage({ ...base, environment: 'kubernetes', slug: 'a'.repeat(64) });
    for (const match of text.matchAll(/name: (?:['"])?([a-z0-9-]+)/g)) expect(match[1]!.length).toBeLessThanOrEqual(63);
  });

  it('rejects a non-HTTPS or credential-bearing endpoint', () => {
    expect(() => renderTunnelInstallMessage({ ...base, endpoint: 'http://api.layerv.xyz' })).toThrow('connector endpoint');
    expect(() => renderTunnelInstallMessage({ ...base, endpoint: 'https://u:p@api.layerv.xyz' })).toThrow('connector endpoint');
  });
});
