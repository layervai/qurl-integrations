import { describe, expect, it } from 'vitest';
import { OAuthCallbackCore } from '../src/callback.js';
import { OAuthCoreError } from '../src/errors.js';
import type {
  ConfidentialTokenClient,
  IdTokenVerifier,
  OAuthStateConsumer,
  ProviderBinder,
  ProviderBindingRequest,
  ProviderBindingResult,
} from '../src/interfaces.js';
import { OAuthStateManager } from '../src/state.js';
import {
  InMemoryStatePersistence,
  TEST_ACTOR_A_ID,
  TEST_ACTOR_B_ID,
  TEST_TENANT_ID,
  deterministicRandom,
  fixedClock,
} from './helpers.js';

function expectCode(error: unknown, code: OAuthCoreError['code']): boolean {
  return error instanceof OAuthCoreError && error.code === code;
}

function stubTokenClient(onExchange?: () => void): ConfidentialTokenClient {
  return {
    createAuthorizationUrl: () => new URL('https://auth.example.com/authorize'),
    exchangeAuthorizationCode: async () => {
      onExchange?.();
      return {
        accessToken: 'synthetic-access-token',
        idToken: 'synthetic-id-token',
      };
    },
  };
}

const identityVerifier: IdTokenVerifier = {
  verify: async (_idToken, expected) => ({
    subject: `provider|${expected.normalizedEmail}`,
    email: expected.normalizedEmail,
  }),
};

class FirstBinderWins implements ProviderBinder {
  readonly #bindings = new Map<string, string>();

  async bind(request: ProviderBindingRequest): Promise<ProviderBindingResult> {
    const existing = this.#bindings.get(request.teamsTenantId);
    if (existing === undefined) {
      this.#bindings.set(request.teamsTenantId, request.providerSubject);
      return { status: 'bound', bindingReference: 'synthetic-binding-reference' };
    }
    if (existing === request.providerSubject) {
      return { status: 'already_bound', bindingReference: 'synthetic-binding-reference' };
    }
    return { status: 'conflict', reason: 'tenant_bound_to_another_account' };
  }
}

async function mintFor(
  manager: OAuthStateManager,
  actorAadObjectId: string,
  deliverySuffix: string,
  email: string,
) {
  return manager.mint({
    teamsTenantId: TEST_TENANT_ID,
    actorAadObjectId,
    actorDeliveryId: `29:${deliverySuffix}`,
    setupEmail: email,
    setupMode: 'bind',
  });
}

describe('OAuthCallbackCore', () => {
  it('rejects CSRF cookie mismatch before consuming state or exchanging a token', async () => {
    let consumed = 0;
    let exchanged = 0;
    const state: OAuthStateConsumer = {
      consume: async () => {
        consumed += 1;
        throw new Error('must not run');
      },
    };
    const core = new OAuthCallbackCore({
      state,
      tokenClient: stubTokenClient(() => { exchanged += 1; }),
      idTokenVerifier: identityVerifier,
      providerBinder: new FirstBinderWins(),
    });
    const callbackState = Buffer.alloc(32, 20).toString('base64url');
    const wrongCookie = Buffer.alloc(32, 21).toString('base64url');

    await expect(core.complete({
      state: callbackState,
      cookieState: wrongCookie,
      code: 'synthetic-authorization-code',
    })).rejects.toSatisfy((error: unknown) => expectCode(error, 'COOKIE_MISMATCH'));
    expect(consumed).toBe(0);
    expect(exchanged).toBe(0);
  });

  it('consumes state before token exchange and prevents callback reuse', async () => {
    const events: string[] = [];
    const persistence = new InMemoryStatePersistence();
    const stateManager = new OAuthStateManager({
      persistence,
      clock: fixedClock(),
      randomBytes: deterministicRandom(),
    });
    const minted = await mintFor(stateManager, TEST_ACTOR_A_ID, 'synthetic-delivery-a', 'admin-a@example.com');
    const state: OAuthStateConsumer = {
      consume: async (handle) => {
        events.push('consume');
        return stateManager.consume(handle);
      },
    };
    let tokenCalls = 0;
    const core = new OAuthCallbackCore({
      state,
      tokenClient: stubTokenClient(() => {
        events.push('exchange');
        tokenCalls += 1;
      }),
      idTokenVerifier: identityVerifier,
      providerBinder: new FirstBinderWins(),
    });
    const input = {
      state: minted.handle,
      cookieState: minted.handle,
      code: 'synthetic-authorization-code',
    };

    await expect(core.complete(input)).resolves.toMatchObject({ binding: { status: 'bound' } });
    await expect(core.complete(input)).rejects.toSatisfy(
      (error: unknown) => expectCode(error, 'STATE_NOT_FOUND'),
    );
    expect(events.slice(0, 2)).toEqual(['consume', 'exchange']);
    expect(tokenCalls).toBe(1);
  });

  it('enforces first-binder-wins for concurrent setup attempts on one tenant', async () => {
    const persistence = new InMemoryStatePersistence();
    const stateManager = new OAuthStateManager({
      persistence,
      clock: fixedClock(),
      randomBytes: deterministicRandom(),
    });
    const first = await mintFor(stateManager, TEST_ACTOR_A_ID, 'synthetic-delivery-a', 'admin-a@example.com');
    const second = await mintFor(stateManager, TEST_ACTOR_B_ID, 'synthetic-delivery-b', 'admin-b@example.com');
    const core = new OAuthCallbackCore({
      state: stateManager,
      tokenClient: stubTokenClient(),
      idTokenVerifier: identityVerifier,
      providerBinder: new FirstBinderWins(),
    });

    const results = await Promise.all([
      core.complete({ state: first.handle, cookieState: first.handle, code: 'synthetic-code-a' }),
      core.complete({ state: second.handle, cookieState: second.handle, code: 'synthetic-code-b' }),
    ]);
    expect(results.map((result) => result.binding.status).sort()).toEqual(['bound', 'conflict']);
    const conflict = results.find((result) => result.binding.status === 'conflict');
    expect(conflict?.binding).toEqual({
      status: 'conflict',
      reason: 'tenant_bound_to_another_account',
    });
  });
});
