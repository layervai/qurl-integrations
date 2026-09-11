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

const hub = { host: 'hub.nhp.layerv.xyz', port: '443', serverPublicKeyB64: 'A'.repeat(43) + '=' } as const;

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

  it('renders the Hub triple only when it is configured', () => {
    expect(renderTunnelConfigYAML(base)).not.toContain('hub:');
    const withHub = renderTunnelConfigYAML({ ...base, hub });
    expect(withHub).toContain('hub:');
    expect(withHub).toContain("host: 'hub.nhp.layerv.xyz'");
    expect(withHub).toContain('port: 443');
  });

  it('validates the Hub triple', () => {
    expect(() => validateTunnelHub(undefined)).not.toThrow();
    expect(() => validateTunnelHub(hub)).not.toThrow();
    expect(() => validateTunnelHub({ ...hub, host: 'not a host' })).toThrow('hub host');
    expect(() => validateTunnelHub({ ...hub, port: '70000' })).toThrow('hub port');
    expect(() => validateTunnelHub({ ...hub, serverPublicKeyB64: 'short' })).toThrow('hub server public key');
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
    expect(compose).toContain('network_mode: service:web');
    expect(compose).toContain('- --headless-config');

    const ecs = renderTunnelInstallMessage({ ...base, environment: 'ecs-fargate' });
    expect(ecs).toContain('"entryPoint"');
    expect(ecs).toContain('"/usr/local/bin/qurl"');
    expect(ecs).toContain('"QURL_ENDPOINT"');

    const k8s = renderTunnelInstallMessage({ ...base, environment: 'kubernetes' });
    expect(k8s).toContain('readOnlyRootFilesystem: true');
    expect(k8s).toContain('--enrollment-token-file');
  });

  it('rejects a non-HTTPS or credential-bearing endpoint', () => {
    expect(() => renderTunnelInstallMessage({ ...base, endpoint: 'http://api.layerv.xyz' })).toThrow('connector endpoint');
    expect(() => renderTunnelInstallMessage({ ...base, endpoint: 'https://u:p@api.layerv.xyz' })).toThrow('connector endpoint');
  });
});
