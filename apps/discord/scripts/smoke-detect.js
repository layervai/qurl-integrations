// Live check: use the deployed bot's QURL_ENDPOINT, QURL_API_KEY,
// DETECT_TUNNEL_SLUG, QURL_DEPLOYMENT, and non-prod host overrides.
// An optional image path plus DETECT_SMOKE_QURL_ID checks known attribution.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const { spawnSync } = require('node:child_process');
const { createHash } = require('node:crypto');
const { QURLClient } = require('@layervai/qurl');
const { hasPersistableQurlIdShape } = require('../src/utils/qurl-id');
const { validateResourceId } = require('../src/utils/resource-id');

// Smoke-only interception: persist the actual detector child before the caller
// can start native opening. The detected watermark's qurl_id is a different ID.
function installMintReceipt(Client, owner) {
  const original = Client.prototype.createQurlForResource;
  assert.equal(typeof original, 'function', 'SDK mint interception point missing');
  const restore = () => { Client.prototype.createQurlForResource = original; };
  restore.captured = 0;
  Client.prototype.createQurlForResource = async function (crid, ...args) {
    const minted = await original.call(this, crid, ...args);
    try {
      validateResourceId(minted.resource_id);
      assert.ok(hasPersistableQurlIdShape(minted.qurl_id) && minted.crid === crid);
      assert.ok(Number.isFinite(Date.parse(minted.expires_at)));
      const verified = spawnSync(process.env.QURL_OWNERSHIP_VERIFIER, [], {
        input: minted.qurl_link, encoding: 'utf8', timeout: 10000, maxBuffer: 16384,
      });
      assert.ok(!verified.error && verified.status === 0, 'signed ownership verification failed');
      const identity = JSON.parse(verified.stdout);
      const publicIdentity = Object.fromEntries(['agent_public_key', 'resource_public_key_b64', 'cell_public_key_b64',
        'cell_id', 'signed_jti', 'signed_expiry_unix'].map(key => [key, identity[key]]));
      assert.ok(publicIdentity.agent_public_key && publicIdentity.resource_public_key_b64 && publicIdentity.cell_public_key_b64);
      const now = new Date();
      fs.appendFileSync(process.env.QURL_OWNERSHIP_RECEIPTS, JSON.stringify({
        event: 'owned_admission_attempt', purpose: 'discord_detect_smoke_detector_child',
        owner_id: owner, resource_id: minted.resource_id, crid, qurl_id: minted.qurl_id,
        expires_at: minted.expires_at, observed_at: now.toISOString(),
        // Native start deadline begins after verification; expires_at separately
        // bounds the child credential. Neither field claims the session is CLOSED.
        attempt_deadline: new Date(now.getTime() + 15000).toISOString(),
        public_identity: publicIdentity, catalog_binding: 'pending_independent_readback',
        child_binding: 'pending_independent_readback',
        agent_membership_pk: 'AGENT#' + createHash('sha256').update(publicIdentity.agent_public_key).digest('hex'),
      }) + '\n', { mode: 0o600 });
      restore.captured += 1;
    } catch {
      // No native opening occurred. Revoke only this returned child, never its
      // shared detector resource. Preserve cleanup failure as a failed smoke.
      if (hasPersistableQurlIdShape(minted?.qurl_id)) {
        try {
          await this.revokeResourceQurl(crid, minted.qurl_id);
        } catch {
          // Restricted CI log fallback if the required private file could not
          // be written. Never include dependency errors or the signed link.
          const safeID = value => { try { validateResourceId(value); return value; } catch { return null; } };
          console.error('Detector child cleanup required', {
            event: 'detector_child_cleanup_required', owner_id: safeID(owner),
            resource_id: safeID(minted.resource_id), qurl_id: minted.qurl_id, crid: safeID(crid),
          });
        }
      }
      throw new Error('detector child ownership receipt could not be persisted');
    }
    return minted;
  };
  return restore;
}

async function main() {
  const guildId = process.env.DETECT_SMOKE_GUILD_ID;
  assert.match(guildId || '', /^[0-9]{17,20}$/, 'Set DETECT_SMOKE_GUILD_ID to the test server ID');
  const imagePath = process.argv[2];
  const expectedId = process.env.DETECT_SMOKE_QURL_ID;
  assert.equal(Boolean(imagePath), Boolean(expectedId), 'Supply both an image path and DETECT_SMOKE_QURL_ID');
  const bytes = imagePath ? fs.readFileSync(imagePath) : Buffer.from(
    'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=',
    'base64',
  );
  for (const name of ['QURL_OWNERSHIP_VERIFIER', 'QURL_OWNERSHIP_RECEIPTS', 'QURL_PUBLIC_CONFIG_URL', 'QURL_ENDPOINT', 'QURL_API_KEY']) {
    assert.ok(process.env[name], `Missing ${name}`);
  }
  fs.accessSync(process.env.QURL_OWNERSHIP_VERIFIER, fs.constants.X_OK);
  fs.appendFileSync(process.env.QURL_OWNERSHIP_RECEIPTS, '', { mode: 0o600 });
  fs.chmodSync(process.env.QURL_OWNERSHIP_RECEIPTS, 0o600);
  const checked = spawnSync(process.env.QURL_OWNERSHIP_VERIFIER, ['--check-config'], { timeout: 10000 });
  assert.ok(!checked.error && checked.status === 0, 'ownership trust unavailable');
  const me = await fetch(new URL('/v1/me', process.env.QURL_ENDPOINT), {
    headers: { Authorization: `Bearer ${process.env.QURL_API_KEY}` }, signal: AbortSignal.timeout(10000),
  });
  assert.ok(me.ok, 'ownership owner read failed');
  // TODO(upstream-contract): qurl-service GET /v1/me returns data.owner_id.
  const owner = (await me.json()).data?.owner_id;
  validateResourceId(owner);
  const restore = installMintReceipt(QURLClient, owner);
  try {
    const { detectWatermark } = require('../src/connector');
    const result = await detectWatermark(bytes, { guildId, contentType: 'image/png' });
    assert.ok(restore.captured > 0, 'detector child receipt missing');
    assert.equal(result.detected, Boolean(expectedId));
    assert.equal(result.qurl_id, expectedId || null);
    console.log('Discord detect live smoke passed');
  } finally {
    restore(); // detectWatermark retains its own opener.close() finally.
  }
}

if (require.main === module) main().catch(error => {
  // Error messages from dependencies can contain credentials; print only type/status.
  console.error('Discord detect live smoke failed', { name: error.name, status: error.status });
  process.exitCode = 1;
});

module.exports = { installMintReceipt };
