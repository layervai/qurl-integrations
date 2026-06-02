#!/usr/bin/env node
// Operator note: live applies are not fully idempotent yet. Image/app
// PATCHes re-upload assets on each run until #588 adds safe no-op detection.

const fs = require('fs');
const path = require('path');

const root = path.resolve(__dirname, '..');
const metadataPath = path.join(root, 'discord-metadata.json');
let metadataCache;
const oneMiB = 1024 * 1024;
const twoMiB = 2 * oneMiB;
const aspectRatioTolerance = 0.01; // Keep committed Discord brand assets on exact target ratios.
const imageRules = {
  'bot.avatar': { maxBytes: oneMiB, minWidth: 128, minHeight: 128, aspect: [1, 1] },
  'bot.banner': { maxBytes: twoMiB, minWidth: 600, minHeight: 240 },
  'application.icon': { maxBytes: oneMiB, minWidth: 512, minHeight: 512, aspect: [1, 1] },
  'application.cover_image': { maxBytes: twoMiB, minWidth: 800, minHeight: 450, aspect: [16, 9] },
};

class PortalActionRequiredError extends Error {
  constructor(message) {
    super(message);
    this.exitCode = 2;
  }
}

function loadMetadata() {
  if (metadataCache) return metadataCache;
  try {
    metadataCache = JSON.parse(fs.readFileSync(metadataPath, 'utf8'));
    return metadataCache;
  } catch (err) {
    throw new Error(`Failed to read ${metadataPath}. Check discord-metadata.json: ${err.message}`);
  }
}

function validateHttpsUrl(value, field) {
  try {
    const url = new URL(value);
    if (url.protocol === 'https:' && url.hostname) return;
  } catch {
    // Fall through to the guided metadata error below.
  }
  throw new Error(`discord-metadata.json must set ${field} to an https URL.`);
}

function validateMetadata(doc = loadMetadata()) {
  if (!doc.bot?.username) throw new Error('discord-metadata.json must set bot.username.');
  if (!doc.application?.name) throw new Error('discord-metadata.json must set application.name.');
  if (!doc.application?.description) throw new Error('discord-metadata.json must set application.description.');
  for (const [field, value] of [
    ['bot.avatar', doc.bot?.avatar],
    ['bot.banner', doc.bot?.banner],
    ['application.icon', doc.application?.icon],
    ['application.cover_image', doc.application?.cover_image],
  ]) {
    if (!value) throw new Error(`discord-metadata.json must set ${field}.`);
  }
  if (!doc.application?.id || !/^\d+$/.test(doc.application.id)) {
    throw new Error('discord-metadata.json must set application.id to the LayerV Discord application ID.');
  }
  if (!doc.application?.public_key || !/^[a-f0-9]{64}$/.test(doc.application.public_key)) {
    throw new Error('discord-metadata.json must set application.public_key to the LayerV Discord public key.');
  }
  const scopes = doc.application?.install_params?.scopes;
  if (!Array.isArray(scopes) || !scopes.includes('bot') || !scopes.includes('applications.commands')) {
    throw new Error('discord-metadata.json must set application.install_params.scopes to include bot and applications.commands.');
  }
  if (!/^\d+$/.test(doc.application?.install_params?.permissions || '')) {
    throw new Error('discord-metadata.json must set application.install_params.permissions to a numeric string.');
  }
  validateHttpsUrl(doc.application.terms_of_service_url, 'application.terms_of_service_url');
  validateHttpsUrl(doc.application.privacy_policy_url, 'application.privacy_policy_url');
}

function dataUri(relPath, field) {
  if (!relPath) return undefined;
  const filePath = path.join(root, relPath);
  if (!fs.existsSync(filePath)) {
    throw new Error(`Discord metadata asset ${relPath} does not exist at ${filePath}. Check discord-metadata.json.`);
  }
  const ext = path.extname(filePath).toLowerCase();
  const mimeByExt = {
    '.jpg': 'image/jpeg',
    '.jpeg': 'image/jpeg',
    '.png': 'image/png',
  };
  const mime = mimeByExt[ext];
  if (!mime) {
    throw new Error(`Discord metadata asset ${relPath} uses unsupported image extension ${ext || '(none)'}. Use PNG or JPEG.`);
  }
  const bytes = fs.readFileSync(filePath);
  const detectedMime = detectImageMime(bytes);
  if (!detectedMime) {
    throw new Error(`Discord metadata asset ${relPath} content is not a PNG or JPEG image.`);
  }
  if (detectedMime !== mime) {
    throw new Error(`Discord metadata asset ${relPath} extension ${ext} does not match detected ${detectedMime}.`);
  }
  if (field) validateImageRule(relPath, bytes, detectedMime, imageRules[field]);
  return `data:${mime};base64,${bytes.toString('base64')}`;
}

function detectImageMime(bytes) {
  if (bytes.length >= 8
    && bytes[0] === 0x89
    && bytes.toString('ascii', 1, 4) === 'PNG'
    && bytes[4] === 0x0d
    && bytes[5] === 0x0a
    && bytes[6] === 0x1a
    && bytes[7] === 0x0a) {
    return 'image/png';
  }
  if (bytes.length >= 3 && bytes[0] === 0xff && bytes[1] === 0xd8 && bytes[2] === 0xff) {
    return 'image/jpeg';
  }
  return undefined;
}

function detectImageDimensions(bytes, mime) {
  if (mime === 'image/png' && bytes.length >= 24) {
    return {
      width: bytes.readUInt32BE(16),
      height: bytes.readUInt32BE(20),
    };
  }
  if (mime === 'image/jpeg') {
    return detectJpegDimensions(bytes);
  }
  return undefined;
}

function detectJpegDimensions(bytes) {
  let offset = 2;
  while (offset < bytes.length) {
    if (bytes[offset] !== 0xff) return undefined;
    while (offset < bytes.length && bytes[offset] === 0xff) offset += 1;
    if (offset >= bytes.length) return undefined;
    const marker = bytes[offset];
    offset += 1;
    if (marker === 0xd9 || marker === 0xda) return undefined;
    if (marker === 0x01 || (marker >= 0xd0 && marker <= 0xd7)) continue;
    if (offset + 2 > bytes.length) return undefined;
    const segmentLength = bytes.readUInt16BE(offset);
    if (segmentLength < 2 || offset + segmentLength > bytes.length) return undefined;
    if (
      (marker >= 0xc0 && marker <= 0xc3)
      || (marker >= 0xc5 && marker <= 0xc7)
      || (marker >= 0xc9 && marker <= 0xcb)
      || (marker >= 0xcd && marker <= 0xcf)
    ) {
      if (segmentLength < 7) return undefined;
      return {
        width: bytes.readUInt16BE(offset + 5),
        height: bytes.readUInt16BE(offset + 3),
      };
    }
    offset += segmentLength;
  }
  return undefined;
}

function validateImageRule(relPath, bytes, mime, rule) {
  if (!rule) return;
  if (bytes.length > rule.maxBytes) {
    throw new Error(`Discord metadata asset ${relPath} is ${bytes.length} bytes; max is ${rule.maxBytes} bytes.`);
  }
  const dimensions = detectImageDimensions(bytes, mime);
  if (!dimensions) {
    throw new Error(`Discord metadata asset ${relPath} dimensions could not be read.`);
  }
  if (dimensions.width < rule.minWidth || dimensions.height < rule.minHeight) {
    throw new Error(`Discord metadata asset ${relPath} is ${dimensions.width}x${dimensions.height}; minimum is ${rule.minWidth}x${rule.minHeight}.`);
  }
  if (rule.aspect) {
    const [widthRatio, heightRatio] = rule.aspect;
    const expectedRatio = widthRatio / heightRatio;
    const actualRatio = dimensions.width / dimensions.height;
    if (Math.abs(actualRatio - expectedRatio) > aspectRatioTolerance) {
      throw new Error(`Discord metadata asset ${relPath} is ${dimensions.width}x${dimensions.height}; expected approximately ${widthRatio}:${heightRatio} aspect ratio.`);
    }
  }
}

async function request(method, apiPath, body, { token, fetchImpl = fetch } = {}) {
  const headers = {
    Authorization: `Bot ${token}`,
  };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  const res = await fetchImpl(`https://discord.com/api/v10${apiPath}`, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  let parsed = {};
  if (text) {
    try {
      parsed = JSON.parse(text);
    } catch {
      parsed = { raw: text };
    }
  }
  if (!res.ok) {
    const err = new Error(`${method} ${apiPath} failed with ${res.status}`);
    err.status = res.status;
    err.body = parsed;
    const retryAfterHeader = res.headers.get('retry-after');
    const retryAfter = retryAfterHeader ?? parsed.retry_after;
    if (retryAfter !== undefined && retryAfter !== null) err.retryAfter = String(retryAfter);
    throw err;
  }
  return parsed;
}

function summarize(value) {
  if (typeof value === 'string' && value.startsWith('data:image/')) return '<image-data>';
  if (Array.isArray(value)) return value.map(summarize);
  if (value && typeof value === 'object') return Object.fromEntries(Object.entries(value).map(([k, v]) => {
    return [k, summarize(v)];
  }));
  return value;
}

function errorDetails(err) {
  const retry = err.retryAfter ? ` retry_after=${err.retryAfter}s` : '';
  return `${err.status || 'error'}${retry} ${JSON.stringify(err.body || err.message)}`;
}

function assertExpectedApplication(app, doc = loadMetadata()) {
  if (app.id !== doc.application.id) {
    throw new Error(`DISCORD_TOKEN belongs to application ${app.id}; expected LayerV application ${doc.application.id}. Refusing to update the wrong Discord app.`);
  }
  const actualPublicKey = app.verify_key || app.public_key;
  if (!actualPublicKey) {
    throw new Error(`Discord application ${app.id} did not include a public key. Refusing to update mismatched app metadata.`);
  }
  if (actualPublicKey !== doc.application.public_key) {
    throw new Error(`Discord application ${app.id} has public key ${actualPublicKey}; expected ${doc.application.public_key}. Refusing to update mismatched app metadata.`);
  }
}

async function main({
  dryRun = process.argv.includes('--dry-run'),
  token = process.env.DISCORD_TOKEN,
  fetchImpl = fetch,
  logger = console,
} = {}) {
  const doc = loadMetadata();
  validateMetadata(doc);
  if (!dryRun && !token) {
    throw new Error('DISCORD_TOKEN is required. Export the target bot token before running this script.');
  }

  let hadPartialFailure = false;
  let hadPortalActionRequired = false;

  // Build payloads before the dry-run branch so dry-run validates local assets.
  const botUsernamePatch = { username: doc.bot.username };
  const botImagePatch = {
    ...(doc.bot.avatar ? { avatar: dataUri(doc.bot.avatar, 'bot.avatar') } : {}),
    ...(doc.bot.banner ? { banner: dataUri(doc.bot.banner, 'bot.banner') } : {}),
  };
  // Discord returns stored asset hashes, not source-file hashes; keep this
  // authoritative PATCH fatal instead of guessing at image no-op detection.
  const appPatch = {
    description: doc.application.description,
    icon: dataUri(doc.application.icon, 'application.icon'),
    cover_image: dataUri(doc.application.cover_image, 'application.cover_image'),
    tags: doc.application.tags,
    install_params: doc.application.install_params,
  };

  if (dryRun) {
    logger.log(JSON.stringify({
      expected_application: {
        id: doc.application.id,
        public_key: doc.application.public_key,
      },
      bot_username: summarize(botUsernamePatch),
      bot_images: Object.keys(botImagePatch).length ? summarize(botImagePatch) : null,
      application: summarize(appPatch),
      application_name: doc.application.name,
      portal_only: {
        terms_of_service_url: doc.application.terms_of_service_url,
        privacy_policy_url: doc.application.privacy_policy_url,
      },
    }, null, 2));
    return;
  }

  const requestOptions = { token, fetchImpl };
  const currentApp = await request('GET', '/applications/@me', undefined, requestOptions);
  assertExpectedApplication(currentApp, doc);
  logger.log(`Verified Discord application: ${currentApp.name} (${currentApp.id})`);

  const currentUser = await request('GET', '/users/@me', undefined, requestOptions);
  if (!currentUser.username) {
    throw new Error('GET /users/@me did not include username. Refusing to apply bot identity metadata.');
  }

  if (currentUser.username === doc.bot.username) {
    logger.log(`Bot username already ${doc.bot.username}; skipping username update.`);
  } else if (currentUser.username.toLowerCase() === doc.bot.username.toLowerCase()) {
    if (currentUser.discriminator === '0') {
      logger.warn(`Bot username is ${currentUser.username}; Discord unique usernames are lowercase. Treating case-only match as applied; verify the live display outcome in #860.`);
    } else {
      hadPartialFailure = true;
      logger.warn(`Bot username is ${currentUser.username}; desired ${doc.bot.username}. Skipping case-only update to avoid rate-limit churn; verify and resolve the live username outcome in #860.`);
    }
  } else {
    try {
      // Keep username separate from bot images so a name conflict/rate limit
      // does not block avatar/banner updates for the same verified app.
      const updatedUser = await request('PATCH', '/users/@me', botUsernamePatch, requestOptions);
      logger.log(`Updated bot username: ${updatedUser.username}`);
    } catch (err) {
      hadPartialFailure = true;
      logger.warn(`Bot username update skipped: ${errorDetails(err)}`);
    }
  }

  if (Object.keys(botImagePatch).length) {
    try {
      // Discord returns stored asset hashes, not source-file hashes; upload bot
      // images together to limit request count until safe no-op detection exists.
      const imageUser = await request('PATCH', '/users/@me', botImagePatch, requestOptions);
      if (botImagePatch.avatar && !imageUser.avatar) {
        hadPartialFailure = true;
        logger.warn('Bot avatar update skipped: Discord response did not include an avatar hash.');
      }
      if (botImagePatch.banner && !imageUser.banner) {
        hadPartialFailure = true;
        logger.warn('Bot banner update skipped: Discord response did not include a banner hash.');
      }
      logger.log(`Updated bot images: avatar=${Boolean(imageUser.avatar)} banner=${Boolean(imageUser.banner)}`);
    } catch (err) {
      hadPartialFailure = true;
      logger.warn(`Bot image update skipped: ${errorDetails(err)}`);
    }
  }

  let updatedApp;
  try {
    updatedApp = await request('PATCH', '/applications/@me', appPatch, requestOptions);
  } catch (err) {
    err.exitCode = 3;
    err.message = `Discord application metadata update failed: ${err.message}`;
    if (hadPartialFailure) {
      err.message = `${err.message}; bot identity fields were also skipped earlier, see warnings above.`;
    }
    throw err;
  }
  logger.log(`Updated application metadata: icon=${Boolean(updatedApp.icon)} cover=${Boolean(updatedApp.cover_image)} description=${Boolean(updatedApp.description)}`);

  if (doc.application.name !== updatedApp.name) {
    hadPortalActionRequired = true;
    logger.warn(`Developer Portal action required: application name remains ${JSON.stringify(updatedApp.name)}; update it to ${JSON.stringify(doc.application.name)} in Discord Developer Portal.`);
  }

  logger.log(`Portal-only URLs: terms=${doc.application.terms_of_service_url}, privacy=${doc.application.privacy_policy_url}`);
  if (hadPartialFailure) {
    const portalSuffix = hadPortalActionRequired ? ' Developer Portal action is also required; see warnings above.' : '';
    throw new Error(`Discord metadata apply completed with skipped fields; see warnings above.${portalSuffix}`);
  }
  if (hadPortalActionRequired) {
    throw new PortalActionRequiredError('Discord metadata API apply completed, but Developer Portal action is still required; see warnings above.');
  }
}

if (require.main === module) {
  main().catch((err) => {
    console.error(err.message);
    if (err.retryAfter) console.error(`retry_after=${err.retryAfter}s`);
    if (err.body) console.error(JSON.stringify(err.body));
    process.exit(err.exitCode || 1);
  });
}

module.exports = {
  assertExpectedApplication,
  dataUri,
  detectImageDimensions,
  errorDetails,
  main,
  PortalActionRequiredError,
  request,
  summarize,
  validateMetadata,
  validateImageRule,
};
