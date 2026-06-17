const crypto = require('crypto');

const PKCE_VERIFIER_PATTERN = /^[A-Za-z0-9._~-]{43,128}$/;

function b64urlEncode(buf) {
  return Buffer.from(buf).toString('base64')
    .replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

function isPkceVerifier(codeVerifier) {
  return typeof codeVerifier === 'string' && PKCE_VERIFIER_PATTERN.test(codeVerifier);
}

function pkceChallengeForVerifier(codeVerifier) {
  if (!isPkceVerifier(codeVerifier)) {
    throw new TypeError('PKCE code_verifier must be 43-128 URL-safe characters');
  }
  return b64urlEncode(crypto.createHash('sha256').update(codeVerifier).digest());
}

function createPkcePair() {
  const codeVerifier = b64urlEncode(crypto.randomBytes(32));
  return {
    codeVerifier,
    codeChallenge: pkceChallengeForVerifier(codeVerifier),
  };
}

module.exports = {
  createPkcePair,
  isPkceVerifier,
  pkceChallengeForVerifier,
};
