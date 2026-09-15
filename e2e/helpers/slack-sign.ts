/**
 * Slack request signing for the Slack bot HTTP smoke tests.
 *
 * Mirrors the `v0` HMAC-SHA256 scheme the bot verifies in
 * apps/slack/internal/signature.go (and the test helper `signSlackBody` in
 * handler_test.go): the signature base string is `v0:{timestamp}:{body}`,
 * keyed by the workspace signing secret, hex-encoded, prefixed `v0=`.
 *
 * Keeping this in lockstep with the app means a valid-signature smoke proves
 * the *deployed* SLACK_SIGNING_SECRET matches what we signed with — the same
 * config-coherence guarantee the Discord smoke gets from `me.id`.
 */

import { createHmac } from 'crypto';
import { fetchWithTransientRetry } from './http';

/** Slack's signature-version prefix (see signature.go `slackSignatureVersion`). */
const SLACK_SIGNATURE_VERSION = 'v0';

interface SlackSignatureHeaders {
  'X-Slack-Signature': string;
  'X-Slack-Request-Timestamp': string;
}

/**
 * Computes the headers Slack would send to authenticate `body` at `timestamp`.
 * `body` is signed verbatim and must equal the bytes POSTed.
 */
function signSlackRequest(body: string, signingSecret: string): SlackSignatureHeaders {
  const ts = String(Math.floor(Date.now() / 1000));
  const mac = createHmac('sha256', signingSecret);
  mac.update(`${SLACK_SIGNATURE_VERSION}:${ts}:${body}`);
  return {
    'X-Slack-Signature': `${SLACK_SIGNATURE_VERSION}=${mac.digest('hex')}`,
    'X-Slack-Request-Timestamp': ts,
  };
}

/**
 * POSTs a signed request to the live bot and returns the raw Response so callers
 * assert status + body themselves.
 *
 * Goes through `fetchWithTransientRetry` because this suite runs as a deploy
 * gate that can race a rolling deploy — a brief drain-gap 503 shouldn't false-red
 * the gate. That's safe here even on a POST: every probe is side-effect-free, and
 * the helper's retry set only covers statuses that provably didn't reach the app.
 * A bad-signature 401 is non-retryable in the helper, so it still fails fast.
 *
 * `contentType` defaults to JSON (events); pass form-encoded for slash commands.
 * The Go app routes by request PATH, not Content-Type, so this header is cosmetic
 * to the app — set only for fidelity to what Slack actually sends.
 */
export async function postSigned(
  baseUrl: string,
  pathname: string,
  body: string,
  signingSecret: string,
  contentType = 'application/json',
): Promise<Response> {
  return fetchWithTransientRetry(`${baseUrl}${pathname}`, {
    method: 'POST',
    headers: { 'Content-Type': contentType, ...signSlackRequest(body, signingSecret) },
    body,
  });
}
