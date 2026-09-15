import type {
  Clock,
  ConditionalConsumeResult,
  OAuthStatePersistence,
  StoredOAuthState,
} from '../src/interfaces.js';
import type { RandomBytes } from '../src/pkce.js';

export const TEST_NOW = 2_000_000_000;
export const TEST_TENANT_ID = '00000000-0000-4000-8000-000000000001';
export const TEST_ACTOR_A_ID = '00000000-0000-4000-8000-000000000002';
export const TEST_ACTOR_B_ID = '00000000-0000-4000-8000-000000000003';

export const fixedClock = (now = TEST_NOW): Clock => ({ now: () => now });

export function deterministicRandom(): RandomBytes {
  let next = 1;
  return (size) => {
    const bytes = new Uint8Array(size);
    bytes.fill(next);
    next = next === 255 ? 1 : next + 1;
    return bytes;
  };
}

export class InMemoryStatePersistence implements OAuthStatePersistence {
  readonly records = new Map<string, StoredOAuthState>();
  readonly creates: StoredOAuthState[] = [];

  async conditionalCreate(state: StoredOAuthState): Promise<{ readonly status: 'created' | 'conflict' }> {
    this.creates.push(structuredClone(state));
    if (this.records.has(state.stateKey)) {
      return { status: 'conflict' };
    }
    this.records.set(state.stateKey, structuredClone(state));
    return { status: 'created' };
  }

  async conditionalConsume(stateKey: string, nowEpochSeconds: number): Promise<ConditionalConsumeResult> {
    const state = this.records.get(stateKey);
    if (state === undefined) {
      return { status: 'missing' };
    }
    if (nowEpochSeconds >= state.expiresAtEpochSeconds) {
      return { status: 'expired' };
    }
    this.records.delete(stateKey);
    return { status: 'consumed', state: structuredClone(state) };
  }
}

