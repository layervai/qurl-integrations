#!/usr/bin/env node

const fs = require('fs');
const path = require('path');

const root = path.resolve(__dirname, '..');
const metadataPath = path.join(root, 'discord-metadata.json');
const metadata = JSON.parse(fs.readFileSync(metadataPath, 'utf8'));

const dryRun = process.argv.includes('--dry-run');
const token = process.env.DISCORD_TOKEN;

function dataUri(relPath) {
  if (!relPath) return undefined;
  const filePath = path.join(root, relPath);
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

async function main() {
  const botPatch = {
    username: metadata.bot.username,
    avatar: dataUri(metadata.bot.avatar),
  };
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
      bot: summarize(botPatch),
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

  const updatedUser = await request('PATCH', '/users/@me', botPatch);
  console.log(`Updated bot user: ${updatedUser.username}#${updatedUser.discriminator}`);

  if (botBannerPatch) {
    try {
      const bannerUser = await request('PATCH', '/users/@me', botBannerPatch);
      console.log(`Updated bot banner: ${Boolean(bannerUser.banner)}`);
    } catch (err) {
      console.warn(`Bot banner update skipped: ${err.status || 'error'} ${JSON.stringify(err.body || err.message)}`);
    }
  }

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
      console.warn(`Application name must be updated in Discord Developer Portal: ${err.status || 'error'} ${JSON.stringify(err.body || err.message)}`);
    }
  }

  console.log(`Portal-only URLs: terms=${metadata.application.terms_of_service_url}, privacy=${metadata.application.privacy_policy_url}`);
}

main().catch((err) => {
  console.error(err.message);
  if (err.body) console.error(JSON.stringify(err.body));
  process.exit(1);
});
