import { createHash } from 'node:crypto';
import { describe, expect, it } from 'vitest';
import { OAuthCoreError } from '../src/errors.js';
import { OAUTH_STATE_TTL_SECONDS, OAuthStateManager } from '../src/state.js';
import type { MintOAuthStateInput } from '../src/state.js';
import {
  InMemoryStatePersistence,
  TEST_ACTOR_A_ID,
  TEST_NOW,
  TEST_TENANT_ID,
  deterministicRandom,
  fixedClock,
} from './helpers.js';

const mintInput = {
  teamsTenantId: TEST_TENANT_ID,
  actorAadObjectId: TEST_ACTOR_A_ID,
  actorDeliveryId: '29:synthetic-delivery-a',
  setupEmail: '  Admin@Example.com ',
  setupMode: 'bind' as const,
};

function expectCode(error: unknown, code: OAuthCoreError['code']): boolean {
  return error instanceof OAuthCoreError && error.code === code;
}

describe('OAuthStateManager', () => {
  it('stores only the SHA-256 lookup key and the bound transaction fields', async () => {
    const persistence = new InMemoryStatePersistence();
    const manager = new OAuthStateManager({
      persistence,
      clock: fixedClock(),
      randomBytes: deterministicRandom(),
    });

    const minted = await manager.mint(mintInput);
    const stored = persistence.creates[0];
    expect(stored).toBeDefined();
    expect(stored?.stateKey).toBe(createHash('sha256').update(minted.handle).digest('hex'));
    expect(JSON.stringify(stored)).not.toContain(minted.handle);
    expect(Object.keys(stored ?? {}).sort()).toEqual([
      'actorAadObjectId',
      'actorDeliveryId',
      'expiresAtEpochSeconds',
      'oidcNonce',
      'pkceVerifier',
      'setupEmail',
      'setupMode',
      'stateKey',
      'teamsTenantId',
    ]);
    expect(stored?.setupEmail).toBe('admin@example.com');
    expect(stored?.expiresAtEpochSeconds).toBe(TEST_NOW + OAUTH_STATE_TTL_SECONDS);
  });

  it('accepts UUID-shaped Entra IDs without requiring RFC version or variant nibbles', async () => {
    const persistence = new InMemoryStatePersistence();
    const manager = new OAuthStateManager({
      persistence,
      clock: fixedClock(),
      randomBytes: deterministicRandom(),
    });

    const minted = await manager.mint({
      ...mintInput,
      teamsTenantId: 'AAAAAAAA-BBBB-FCCC-0DDD-EEEEEEEEEEEE',
      actorAadObjectId: '11111111-2222-E333-0444-555555555555',
    });
    expect(minted.transaction.teamsTenantId).toBe('aaaaaaaa-bbbb-fccc-0ddd-eeeeeeeeeeee');
    expect(minted.transaction.actorAadObjectId).toBe('11111111-2222-e333-0444-555555555555');
  });

  it('rejects undeclared future setup modes at the runtime boundary', async () => {
    const persistence = new InMemoryStatePersistence();
    const manager = new OAuthStateManager({
      persistence,
      clock: fixedClock(),
      randomBytes: deterministicRandom(),
    });
    const futureModeInput = {
      ...mintInput,
      setupMode: 'rotate',
    } as unknown as MintOAuthStateInput;

    await expect(manager.mint(futureModeInput)).rejects.toSatisfy(
      (error: unknown) => expectCode(error, 'INVALID_INPUT'),
    );
    expect(persistence.creates).toHaveLength(0);
  });

  it('atomically consumes a state once and fails closed on replay', async () => {
    const persistence = new InMemoryStatePersistence();
    const manager = new OAuthStateManager({
      persistence,
      clock: fixedClock(),
      randomBytes: deterministicRandom(),
    });
    const minted = await manager.mint(mintInput);

    await expect(manager.consume(minted.handle)).resolves.toMatchObject({ setupEmail: 'admin@example.com' });
    await expect(manager.consume(minted.handle)).rejects.toSatisfy(
      (error: unknown) => expectCode(error, 'STATE_NOT_FOUND'),
    );
  });

  it('rejects an expired state even while its TTL row still exists', async () => {
    let current = TEST_NOW;
    const persistence = new InMemoryStatePersistence();
    const manager = new OAuthStateManager({
      persistence,
      clock: { now: () => current },
      randomBytes: deterministicRandom(),
    });
    const minted = await manager.mint(mintInput);
    current += OAUTH_STATE_TTL_SECONDS;

    await expect(manager.consume(minted.handle)).rejects.toSatisfy(
      (error: unknown) => expectCode(error, 'STATE_EXPIRED'),
    );
    expect(persistence.records.size).toBe(1);
  });

  it('double-checks expiry if persistence returns an expired row as consumed', async () => {
    const persistence = new InMemoryStatePersistence();
    const mintManager = new OAuthStateManager({
      persistence,
      clock: fixedClock(),
      randomBytes: deterministicRandom(),
    });
    const minted = await mintManager.mint(mintInput);
    const stored = persistence.creates[0];
    expect(stored).toBeDefined();
    const manager = new OAuthStateManager({
      persistence: {
        conditionalCreate: async () => ({ status: 'created' }),
        conditionalConsume: async () => ({ status: 'consumed', state: structuredClone(stored!) }),
      },
      clock: fixedClock(TEST_NOW + OAUTH_STATE_TTL_SECONDS),
    });

    await expect(manager.consume(minted.handle)).rejects.toSatisfy(
      (error: unknown) => expectCode(error, 'STATE_EXPIRED'),
    );
  });

  it('fails closed if conditional creation reports a collision', async () => {
    const persistence = new InMemoryStatePersistence();
    const random = deterministicRandom();
    const first = new OAuthStateManager({ persistence, clock: fixedClock(), randomBytes: random });
    await first.mint(mintInput);

    const replayedRandom = deterministicRandom();
    const second = new OAuthStateManager({ persistence, clock: fixedClock(), randomBytes: replayedRandom });
    await expect(second.mint(mintInput)).rejects.toSatisfy(
      (error: unknown) => expectCode(error, 'STATE_COLLISION'),
    );
  });
});
