#!/usr/bin/env node

const fs = require('fs');
const path = require('path');

const root = path.resolve(__dirname, '..');
const metadataPath = path.join(root, 'discord-metadata.json');
const metadata = JSON.parse(fs.readFileSync(metadataPath, 'utf8'));

const dryRun = process.argv.includes('--dry-run');
const token = process.env.DISCORD_TOKEN;

function validateMetadata(doc = metadata) {
  if (!doc.bot?.username) throw new Error('discord-metadata.json must set bot.username.');
  if (!doc.application?.id || !/^\d+$/.test(doc.application.id)) {
    throw new Error('discord-metadata.json must set application.id to the LayerV Discord application ID.');
  }
  if (!doc.application?.public_key || !/^[a-f0-9]{64}$/.test(doc.application.public_key)) {
    throw new Error('discord-metadata.json must set application.public_key to the LayerV Discord public key.');
  }
}

function dataUri(relPath, baseRoot = root) {
  if (!relPath) return undefined;
  const filePath = path.join(baseRoot, relPath);
  if (!fs.existsSync(filePath)) {
    throw new Error(`Discord metadata asset ${relPath} does not exist at ${filePath}. Check discord-metadata.json.`);
  }
  const ext = path.extname(filePath).toLowerCase();
  const mime = ext === '.jpg' || ext === '.jpeg' ? 'image/jpeg' : 'image/png';
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

function botTag(user) {
  if (user.discriminator && user.discriminator !== '0') return `${user.username}#${user.discriminator}`;
  return user.username;
}

function errorDetails(err) {
  const retry = err.retryAfter ? ` retry_after=${err.retryAfter}s` : '';
  return `${err.status || 'error'}${retry} ${JSON.stringify(err.body || err.message)}`;
}

function assertExpectedApplication(app, doc = metadata) {
  if (String(app.id) !== doc.application.id) {
    throw new Error(`DISCORD_TOKEN belongs to application ${app.id}; expected LayerV application ${doc.application.id}. Refusing to update the wrong Discord app.`);
  }
  const actualPublicKey = app.verify_key || app.public_key;
  if (actualPublicKey && actualPublicKey !== doc.application.public_key) {
    throw new Error(`Discord application ${app.id} has public key ${actualPublicKey}; expected ${doc.application.public_key}. Refusing to update mismatched app metadata.`);
  }
}

async function main() {
  validateMetadata();
  let hadPartialFailure = false;

  const botUsernamePatch = { username: metadata.bot.username };
  const botAvatarPatch = metadata.bot.avatar ? { avatar: dataUri(metadata.bot.avatar) } : null;
  const botBannerPatch = metadata.bot.banner ? { banner: dataUri(metadata.bot.banner) } : null;
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
      bot_avatar: botAvatarPatch ? summarize(botAvatarPatch) : null,
      bot_banner: botBannerPatch ? summarize(botBannerPatch) : null,
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

  if (metadata.application.name && metadata.application.name !== updatedApp.name) {
    try {
      const renamedApp = await request('PATCH', '/applications/@me', { name: metadata.application.name });
      if (renamedApp.name === metadata.application.name) {
        console.log(`Updated application name: ${renamedApp.name}`);
      } else {
        console.warn(`Application name remains ${JSON.stringify(renamedApp.name)}; update it in Discord Developer Portal.`);
      }
    } catch (err) {
      hadPartialFailure = true;
      console.warn(`Application name must be updated in Discord Developer Portal: ${errorDetails(err)}`);
    }
  }

  if (currentUser.username === metadata.bot.username) {
    console.log(`Bot username already ${metadata.bot.username}; skipping username update.`);
  } else {
    try {
      const updatedUser = await request('PATCH', '/users/@me', botUsernamePatch);
      console.log(`Updated bot username: ${botTag(updatedUser)}`);
    } catch (err) {
      hadPartialFailure = true;
      console.warn(`Bot username update skipped: ${errorDetails(err)}`);
    }
  }

  if (botAvatarPatch) {
    try {
      const avatarUser = await request('PATCH', '/users/@me', botAvatarPatch);
      console.log(`Updated bot avatar: ${Boolean(avatarUser.avatar)}`);
    } catch (err) {
      hadPartialFailure = true;
      console.warn(`Bot avatar update skipped: ${errorDetails(err)}`);
    }
  }

  if (botBannerPatch) {
    try {
      const bannerUser = await request('PATCH', '/users/@me', botBannerPatch);
      console.log(`Updated bot banner: ${Boolean(bannerUser.banner)}`);
    } catch (err) {
      hadPartialFailure = true;
      console.warn(`Bot banner update skipped: ${errorDetails(err)}`);
    }
  }

  console.log(`Portal-only URLs: terms=${metadata.application.terms_of_service_url}, privacy=${metadata.application.privacy_policy_url}`);
  if (hadPartialFailure) {
    throw new Error('Discord metadata apply completed with skipped fields; see warnings above.');
  }
}

if (require.main === module) {
  main().catch((err) => {
    console.error(err.message);
    if (err.body) console.error(JSON.stringify(err.body));
    process.exit(1);
  });
}

module.exports = {
  assertExpectedApplication,
  botTag,
  dataUri,
  summarize,
  validateMetadata,
};
