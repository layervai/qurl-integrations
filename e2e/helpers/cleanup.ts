/**
 * Per-run test-resource hygiene shared by the suites that mint real qURL
 * resources or post real Discord messages.
 *
 * Ports the two patterns proven in location-variants.test.ts (added for
 * qurl-integrations#657 — that file keeps its own inline copy alongside
 * bespoke comments about its raw-unicode URL fixtures):
 *
 *  - RUN_NONCE / withRunNonce(): qurl-service dedups resources by
 *    (owner_id, target_url, type), so a generic fixture URL like
 *    `https://example.com/parallel-0` would resolve to the SAME live
 *    resource across runs (and across concurrently-running suites).
 *    Tagging every minted target_url with a per-run nonce keeps each
 *    run's resources private to that run — which is what makes the
 *    afterAll revocation below unambiguously safe: this run only ever
 *    revokes resources no other run can be holding.
 *
 *  - trackedQurlResources(): record the resource_id of every mint BEFORE
 *    asserting on the mint result (track-before-validate, so a failing
 *    expect can't leak an already-minted resource past cleanup), then
 *    best-effort revoke them all in afterAll. Best-effort: cleanup never
 *    fails the run or masks a real test failure, but each unsuccessful
 *    revoke logs a warning — a systematically-failing cleanup (e.g. the
 *    API key lost `qurl:write`, so every DELETE 403s) must stay visible
 *    in CI logs, since silently-resumed resource leaks are the exact
 *    class this module exists to close. Transient single failures are
 *    harmless: the short-TTL (≤1h) links expire on their own.
 *
 *  - trackedDiscordMessages(): the same idea for the Discord suites,
 *    whose leaked "resources" are bot messages piling up in the shared
 *    test channel — track every sent message, best-effort delete in
 *    afterAll.
 */

import * as discord from './discord-api';
import * as qurl from './qurl-api';

/** Per-RUN nonce baked into every minted target_url. URL-safe
 * (alphanumeric + hyphen), so it never alters the escaping a fixture
 * exercises. */
export const RUN_NONCE = `e2e-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`;

/** Append RUN_NONCE as a query param without disturbing the URL's shape:
 * inserted before any `#fragment`, `?` vs `&` chosen from the existing
 * query. (Same insertion rules as location-variants' inline copy.) */
export function withRunNonce(url: string): string {
  const hashIdx = url.indexOf('#');
  const base = hashIdx === -1 ? url : url.slice(0, hashIdx);
  const fragment = hashIdx === -1 ? '' : url.slice(hashIdx);
  const sep = base.includes('?') ? '&' : '?';
  return `${base}${sep}_e2e_nonce=${RUN_NONCE}${fragment}`;
}

export interface QurlResourceTracker {
  /** Record a minted resource for afterAll revocation. Call BEFORE
   * asserting on the mint result. Guarded on a defined id so a malformed
   * mint fails only its own assertions, not also a spurious
   * `revokeLink(undefined)` warning in afterAll. */
  track(resourceId: string | undefined): void;
  /** Drop a resource the test itself already verifiably revoked, so
   * afterAll doesn't re-revoke it and warn about the expected not-ok. */
  untrack(resourceId: string | undefined): void;
  /** Best-effort revocation of everything still tracked — see the module
   * header for the warn-but-never-throw contract. Wire up as
   * `afterAll(() => tracked.revokeAll())`. */
  revokeAll(): Promise<void>;
}

export function trackedQurlResources(env: {
  MINT_API_URL: string;
  QURL_API_KEY: string;
}): QurlResourceTracker {
  // A Set, not an array: qurl-service may dedup two mints of the same
  // target_url to one resource_id (link-lifecycle's same-target test),
  // and one revoke per resource is enough.
  const ids = new Set<string>();
  return {
    track(resourceId) {
      if (resourceId) ids.add(resourceId);
    },
    untrack(resourceId) {
      if (resourceId) ids.delete(resourceId);
    },
    async revokeAll() {
      // revokeLink returns res.ok (false on a 4xx, NO throw) and only
      // throws on a network error, so surface BOTH paths — the
      // systematic-403 one is the dangerous one (see module header).
      for (const id of ids) {
        try {
          const ok = await qurl.revokeLink(env.MINT_API_URL, env.QURL_API_KEY, id);
          if (!ok) console.warn(`afterAll: best-effort revoke of ${id} returned not-ok`);
        } catch (err) {
          console.warn(`afterAll: best-effort revoke of ${id} threw: ${String(err)}`);
        }
      }
    },
  };
}

export interface DiscordMessageTracker {
  /** Record a bot-sent message for afterAll deletion. Call right after
   * the send, before asserting on it. */
  track(msg: { id: string; channel_id: string }): void;
  /** Best-effort deletion of every tracked message (bots may always
   * delete their own posts). Wire up as
   * `afterAll(() => sentMessages.deleteAll())`. */
  deleteAll(): Promise<void>;
}

export function trackedDiscordMessages(env: { BOT_TOKEN: string }): DiscordMessageTracker {
  const messages: Array<{ id: string; channel_id: string }> = [];
  return {
    track(msg) {
      messages.push({ id: msg.id, channel_id: msg.channel_id });
    },
    async deleteAll() {
      for (const { id, channel_id } of messages) {
        try {
          await discord.deleteMessage(env.BOT_TOKEN, channel_id, id);
        } catch (err) {
          // Best-effort (e.g. a 429 burst on the delete bucket): stale
          // test messages are channel noise, not live grants — warn for
          // visibility, never fail the run.
          console.warn(`afterAll: best-effort delete of message ${id} threw: ${String(err)}`);
        }
      }
    },
  };
}
