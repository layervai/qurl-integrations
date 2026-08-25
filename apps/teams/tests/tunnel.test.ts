import { describe, expect, it } from 'vitest';
import { normalizeTunnelEnvironment, renderTunnelInstallMessage, validateTunnelImageRef, validateTunnelSlug } from '../src/tunnel.js';

const base = { slug: 'prod', alias: 'prod', environment: 'docker' as const, port: 8080, image: 'registry.example/qurl:1', bootstrapKey: "key'with space" } as const;

describe('connector tunnel rendering', () => {
  it('validates slugs, image references, aliases, ports, and services', () => {
    expect(() => validateTunnelSlug('prod')).not.toThrow();
    expect(() => validateTunnelSlug('bad_slug')).toThrow('connector id');
    expect(() => validateTunnelImageRef('registry.example/qurl:1;bad')).toThrow('invalid connector image');
    expect(() => renderTunnelInstallMessage({ ...base, alias: 'bad_alias' })).toThrow('connector alias');
    expect(() => renderTunnelInstallMessage({ ...base, port: 0 })).toThrow('connector port');
    expect(() => renderTunnelInstallMessage({ ...base, service: 'bad service' })).toThrow('connector service');
  });

  it('normalizes supported deployment environments', () => {
    expect(normalizeTunnelEnvironment('docker')).toBe('docker');
    expect(normalizeTunnelEnvironment('docker-compose')).toBe('compose');
    expect(normalizeTunnelEnvironment('ecs-fargate')).toBe('ecs-fargate');
    expect(normalizeTunnelEnvironment('kubernetes')).toBe('kubernetes');
    expect(() => normalizeTunnelEnvironment('unknown')).toThrow('invalid connector environment');
  });

  it('shell-quotes Docker secrets and renders every deployment target', () => {
    expect(renderTunnelInstallMessage({ ...base, environment: 'docker' })).toContain("QURL_BOOTSTRAP_KEY='key'\"'\"'with space'");
    expect(renderTunnelInstallMessage({ ...base, environment: 'compose' })).toContain('QURL_BOOTSTRAP_KEY: "key\'with space"');
    expect(renderTunnelInstallMessage({ ...base, environment: 'ecs-fargate' })).toContain('ECS/Fargate environment');
    expect(renderTunnelInstallMessage({ ...base, environment: 'kubernetes' })).toContain('kind: Deployment');
  });
});
