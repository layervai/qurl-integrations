process.env.DISCORD_INSTALL_STATE_SECRET = '2'.repeat(64);

const crypto = require('crypto');
const config = require('../src/config');
const {
  STATE_MAX_TTL_SECONDS,
  verifyMarketingInstallState,
} = require('../src/utils/marketing-install-state');

const NOW = 2_000_000;

function b64url(payload) {
  return Buffer.from(JSON.stringify(payload)).toString('base64url');
}

function sign(payload, secret = config.DISCORD_INSTALL_STATE_SECRET) {
  const encoded = b64url(payload);
  return signEncoded(encoded, secret);
}

function signEncoded(encoded, secret = config.DISCORD_INSTALL_STATE_SECRET) {
  const sig = crypto.createHmac('sha256', secret).update(encoded).digest('hex');
  return `${encoded}.${sig}`;
}

describe('verifyMarketingInstallState', () => {
  const basePayload = {
    k: 'discord-install',
    n: 'nonce-1',
    e: NOW + 60,
  };

  beforeEach(() => {
    config.DISCORD_INSTALL_STATE_SECRET = '2'.repeat(64);
  });

  it('accepts a signed state within the bot-enforced TTL ceiling', () => {
    const result = verifyMarketingInstallState(sign(basePayload), NOW);
    expect(result.ok).toBe(true);
    expect(result.payload).toMatchObject(basePayload);
  });

  it('accepts the exact max-TTL-plus-skew expiry boundary', () => {
    const state = sign({ ...basePayload, e: NOW + STATE_MAX_TTL_SECONDS + 30 });
    expect(verifyMarketingInstallState(state, NOW).ok).toBe(true);
  });

  it.each([
    ['missing', '', 'missing'],
    ['malformed', 'bad-state', 'malformed'],
    ['malformed signature', `${b64url(basePayload)}.not-hex`, 'malformed'],
    ['payload', signEncoded(Buffer.from('{').toString('base64url')), 'payload'],
    ['kind', sign({ ...basePayload, k: 'qurl-oauth' }), 'kind'],
    ['expiry missing', sign({ k: 'discord-install', n: 'nonce-1' }), 'expiry_missing'],
    ['expired', sign({ ...basePayload, e: NOW - 120 }), 'expired'],
    ['expiry too far', sign({ ...basePayload, e: NOW + STATE_MAX_TTL_SECONDS + 31 }), 'expiry_too_far'],
  ])('rejects %s state with reason=%s', (_name, state, reason) => {
    expect(verifyMarketingInstallState(state, NOW)).toEqual({ ok: false, reason });
  });

  it('rejects when the signing secret is unset or too short', () => {
    config.DISCORD_INSTALL_STATE_SECRET = '';
    expect(verifyMarketingInstallState(sign(basePayload, '2'.repeat(64)), NOW))
      .toEqual({ ok: false, reason: 'secret_unset' });

    config.DISCORD_INSTALL_STATE_SECRET = '2'.repeat(63);
    expect(verifyMarketingInstallState(sign(basePayload, 'short'), NOW))
      .toEqual({ ok: false, reason: 'secret_too_short' });
  });

  it('rejects signatures minted by a different secret', () => {
    const state = sign(basePayload, '3'.repeat(64));
    expect(verifyMarketingInstallState(state, NOW)).toEqual({ ok: false, reason: 'signature' });
  });

  it('rejects uppercase hex signatures as malformed', () => {
    const signed = sign(basePayload);
    const dot = signed.lastIndexOf('.');
    const encoded = signed.slice(0, dot);
    const sig = signed.slice(dot + 1).toUpperCase();
    expect(verifyMarketingInstallState(`${encoded}.${sig}`, NOW))
      .toEqual({ ok: false, reason: 'malformed' });
  });

  it('rejects a payload segment swapped after signing a different encoded body', () => {
    const signed = sign(basePayload);
    const sig = signed.slice(signed.lastIndexOf('.') + 1);
    const tamperedEncoded = b64url({ ...basePayload, e: NOW + 120 });
    expect(verifyMarketingInstallState(`${tamperedEncoded}.${sig}`, NOW))
      .toEqual({ ok: false, reason: 'signature' });
  });
});
