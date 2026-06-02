#!/usr/bin/env node

const fs = require('fs');
const path = require('path');

const root = path.resolve(__dirname, '..');
const metadataPath = path.join(root, 'discord-metadata.json');
const metadata = JSON.parse(fs.readFileSync(metadataPath, 'utf8'));

const dryRun = process.argv.includes('--dry-run');
const token = process.env.DISCORD_TOKEN;

class PortalActionRequiredError extends Error {
  constructor(message) {
    super(message);
    this.exitCode = 2;
  }
}

function validateMetadata(doc = metadata) {
  if (!doc.bot?.username) throw new Error('discord-metadata.json must set bot.username.');
  if (!doc.application?.name) throw new Error('discord-metadata.json must set application.name.');
  if (!doc.application?.id || !/^\d+$/.test(doc.application.id)) {
    throw new Error('discord-metadata.json must set application.id to the LayerV Discord application ID.');
  }
  if (!doc.application?.public_key || !/^[a-f0-9]{64}$/.test(doc.application.public_key)) {
    throw new Error('discord-metadata.json must set application.public_key to the LayerV Discord public key.');
  }
}

function dataUri(relPath) {
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
    '.webp': 'image/webp',
  };
  const mime = mimeByExt[ext];
  if (!mime) {
    throw new Error(`Discord metadata asset ${relPath} uses unsupported image extension ${ext || '(none)'}. Use PNG, JPEG, or WebP.`);
  }
  return `data:${mime};base64,${fs.readFileSync(filePath).toString('base64')}`;
}

async function request(method, apiPath, body) {
  const res = await fetch(`https://discord.com/api/v10${apiPath}`, {
    method,
    headers: {
      Authorization: `Bot ${token}`,
      'Content-Type': 'application/json',
    },
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
    err.retryAfter = res.headers.get('retry-after') || parsed.retry_after;
    throw err;
  }
  return parsed;
}

function summarize(value) {
  if (Array.isArray(value)) return value;
  if (value && typeof value === 'object') return Object.fromEntries(Object.entries(value).map(([k, v]) => {
    if (typeof v === 'string' && v.startsWith('data:image/')) return [k, '<image-data>'];
    return [k, summarize(v)];
  }));
  return value;
}

function errorDetails(err) {
  const retry = err.retryAfter ? ` retry_after=${err.retryAfter}s` : '';
  return `${err.status || 'error'}${retry} ${JSON.stringify(err.body || err.message)}`;
}

function assertExpectedApplication(app, doc = metadata) {
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

async function main() {
  validateMetadata();
  let hadPartialFailure = false;
  let hadPortalActionRequired = false;

  const botUsernamePatch = { username: metadata.bot.username };
  const botImagePatch = {
    ...(metadata.bot.avatar ? { avatar: dataUri(metadata.bot.avatar) } : {}),
    ...(metadata.bot.banner ? { banner: dataUri(metadata.bot.banner) } : {}),
  };
  // Discord returns stored asset hashes, not source-file hashes; keep this
  // authoritative PATCH fatal instead of guessing at image no-op detection.
  const appPatch = {
    description: metadata.application.description,
    icon: dataUri(metadata.application.icon),
    cover_image: dataUri(metadata.application.cover_image),
    tags: metadata.application.tags,
    install_params: metadata.application.install_params,
  };

  if (dryRun) {
    console.log(JSON.stringify({
      expected_application: {
        id: metadata.application.id,
        public_key: metadata.application.public_key,
      },
      bot_username: summarize(botUsernamePatch),
      bot_images: Object.keys(botImagePatch).length ? summarize(botImagePatch) : null,
      application: summarize(appPatch),
      application_name: metadata.application.name,
      portal_only: {
        terms_of_service_url: metadata.application.terms_of_service_url,
        privacy_policy_url: metadata.application.privacy_policy_url,
      },
    }, null, 2));
    return;
  }

  if (!token) {
    throw new Error('DISCORD_TOKEN is required. Export the target bot token before running this script.');
  }

  const currentApp = await request('GET', '/applications/@me');
  assertExpectedApplication(currentApp);
  console.log(`Verified Discord application: ${currentApp.name} (${currentApp.id})`);

  const currentUser = await request('GET', '/users/@me');

  const updatedApp = await request('PATCH', '/applications/@me', appPatch);
  console.log(`Updated application metadata: icon=${Boolean(updatedApp.icon)} cover=${Boolean(updatedApp.cover_image)} description=${Boolean(updatedApp.description)}`);

  if (metadata.application.name !== updatedApp.name) {
    hadPortalActionRequired = true;
    console.warn(`Developer Portal action required: application name remains ${JSON.stringify(updatedApp.name)}; update it to ${JSON.stringify(metadata.application.name)} in Discord Developer Portal.`);
  }

  if (currentUser.username === metadata.bot.username) {
    console.log(`Bot username already ${metadata.bot.username}; skipping username update.`);
  } else {
    try {
      const updatedUser = await request('PATCH', '/users/@me', botUsernamePatch);
      console.log(`Updated bot username: ${updatedUser.username}`);
    } catch (err) {
      hadPartialFailure = true;
      console.warn(`Bot username update skipped: ${errorDetails(err)}`);
    }
  }

  if (Object.keys(botImagePatch).length) {
    try {
      // Discord returns stored asset hashes, not source-file hashes; upload bot
      // images together to limit request count until safe no-op detection exists.
      const imageUser = await request('PATCH', '/users/@me', botImagePatch);
      console.log(`Updated bot images: avatar=${Boolean(imageUser.avatar)} banner=${Boolean(imageUser.banner)}`);
    } catch (err) {
      hadPartialFailure = true;
      console.warn(`Bot image update skipped: ${errorDetails(err)}`);
    }
  }

  console.log(`Portal-only URLs: terms=${metadata.application.terms_of_service_url}, privacy=${metadata.application.privacy_policy_url}`);
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
  errorDetails,
  PortalActionRequiredError,
  summarize,
  validateMetadata,
};
