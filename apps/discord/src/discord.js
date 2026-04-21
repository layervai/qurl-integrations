const { Client, GatewayIntentBits, EmbedBuilder } = require('discord.js');
const cron = require('node-cron');
const config = require('./config');
const logger = require('./logger');
const db = require('./database');
const { escapeDiscordMarkdown: md } = require('./utils/sanitize');

const client = new Client({
  intents: [
    GatewayIntentBits.Guilds,
    GatewayIntentBits.GuildMembers,
    GatewayIntentBits.GuildVoiceStates,
  ],
});

// Cache for quick lookups
let guild = null;
let roles = {
  contributor: null,
  activeContributor: null,
  coreContributor: null,
  champion: null,
};
let channels = {
  general: null,
  announcements: null,
  contribute: null,
  githubFeed: null,
};

// Auto-create missing roles and channels
// Returns { createdRoles: Map<name, Role>, createdChannels: Map<name, Channel> }
// so refreshCache can seed the cache from the create() return values instead
// of relying on the subsequent fetch() — Discord's API doesn't guarantee a
// freshly-created resource shows up in the very next list call, so the cache
// slot could stay null for an eventual-consistency window otherwise.
async function ensureRolesAndChannels() {
  if (!guild) return { createdRoles: new Map(), createdChannels: new Map() };

  // Gated on ENABLE_OPENNHP_FEATURES. In any guild that hasn't explicitly
  // opted in to OpenNHP community features, the bot only has the 4
  // runtime permissions it was invited with (View Channels, Send
  // Messages, Embed Links, Use Application Commands) — ManageRoles and
  // ManageChannels are intentionally NOT granted. Attempting create()
  // here produces a cascade of "Missing Permissions" errors on every
  // boot + cache refresh, which obscures real issues in the logs.
  if (!config.ENABLE_OPENNHP_FEATURES) {
    return { createdRoles: new Map(), createdChannels: new Map() };
  }

  const { ChannelType } = require('discord.js');

  const requiredRoles = [
    { name: config.CONTRIBUTOR_ROLE_NAME, color: 0x3498DB, hoist: false },
    { name: config.ACTIVE_CONTRIBUTOR_ROLE_NAME, color: 0x2ECC71, hoist: true },
    { name: config.CORE_CONTRIBUTOR_ROLE_NAME, color: 0x9B59B6, hoist: true },
    { name: config.CHAMPION_ROLE_NAME, color: 0xF1C40F, hoist: true },
  ];

  const requiredChannels = [
    { name: config.CONTRIBUTE_CHANNEL_NAME, topic: 'Good first issues and contribution opportunities' },
    { name: config.GITHUB_FEED_CHANNEL_NAME, topic: 'GitHub activity feed' },
  ];

  const allRoles = await guild.roles.fetch();
  const allChannels = await guild.channels.fetch();
  const createdRoles = new Map();
  const createdChannels = new Map();

  for (const roleConfig of requiredRoles) {
    const exists = allRoles.find(r => r.name === roleConfig.name);
    if (!exists) {
      try {
        const created = await guild.roles.create({
          name: roleConfig.name,
          color: roleConfig.color,
          hoist: roleConfig.hoist,
          reason: 'Auto-created by OpenNHP bot',
        });
        createdRoles.set(roleConfig.name, created);
        logger.info(`Created role: ${roleConfig.name}`);
      } catch (error) {
        logger.error(`Failed to create role ${roleConfig.name}`, { error: error.message });
      }
    }
  }

  for (const channelConfig of requiredChannels) {
    const exists = allChannels.find(c => c.name === channelConfig.name);
    if (!exists) {
      try {
        const created = await guild.channels.create({
          name: channelConfig.name,
          type: ChannelType.GuildText,
          topic: channelConfig.topic,
          reason: 'Auto-created by OpenNHP bot',
        });
        createdChannels.set(channelConfig.name, created);
        logger.info(`Created channel: #${channelConfig.name}`);
      } catch (error) {
        logger.error(`Failed to create channel #${channelConfig.name}`, { error: error.message });
      }
    }
  }

  return { createdRoles, createdChannels };
}

// Refresh cache - call this to update stale references. Concurrent callers
// (roleDelete + channelDelete + guildMemberAdd firing in quick succession)
// would otherwise interleave the two fetches and cache an inconsistent
// roles/channels snapshot. Coalesce into a single in-flight refresh.
let refreshCacheInFlight = null;
async function refreshCache() {
  if (refreshCacheInFlight) return refreshCacheInFlight;
  refreshCacheInFlight = (async () => {
    try {
      guild = await client.guilds.fetch(config.GUILD_ID);

      // Auto-create missing roles and channels first. Keep the returned
      // objects so we can seed the cache directly — otherwise the immediate
      // fetch() below may not yet list a freshly-created resource.
      const { createdRoles, createdChannels } = await ensureRolesAndChannels();

      const [allRoles, allChannels] = await Promise.all([
        guild.roles.fetch(),
        guild.channels.fetch(),
      ]);

      const pickRole = (name) => allRoles.find(r => r.name === name) || createdRoles.get(name) || null;
      const pickChannel = (name) => allChannels.find(c => c.name === name) || createdChannels.get(name) || null;

      // Cache roles
      roles.contributor = pickRole(config.CONTRIBUTOR_ROLE_NAME);
      roles.activeContributor = pickRole(config.ACTIVE_CONTRIBUTOR_ROLE_NAME);
      roles.coreContributor = pickRole(config.CORE_CONTRIBUTOR_ROLE_NAME);
      roles.champion = pickRole(config.CHAMPION_ROLE_NAME);

      // Cache channels
      channels.general = pickChannel(config.GENERAL_CHANNEL_NAME);
      channels.announcements = pickChannel(config.ANNOUNCEMENTS_CHANNEL_NAME);
      channels.contribute = pickChannel(config.CONTRIBUTE_CHANNEL_NAME);
      channels.githubFeed = pickChannel(config.GITHUB_FEED_CHANNEL_NAME);

      // Log what was found
      const foundRoles = Object.entries(roles).filter(([, v]) => v).map(([k]) => k);
      const foundChannels = Object.entries(channels).filter(([, v]) => v).map(([k]) => k);
      logger.info('Cache refreshed', {
        guild: guild?.name,
        roles: foundRoles.join(', '),
        channels: foundChannels.join(', '),
      });
    } catch (error) {
      logger.error('Failed to refresh cache', { error: error.message });
      // Re-throw so callers that `await refreshCache()` can't then assume
      // `guild`/`roles`/`channels` are populated. Previously the error was
      // swallowed here and a downstream `guild.members.fetch()` would crash
      // with an opaque TypeError.
      throw error;
    } finally {
      refreshCacheInFlight = null;
    }
  })();
  return refreshCacheInFlight;
}

// Schedule weekly digest
let digestTask = null;

function setupWeeklyDigest() {
  if (digestTask) {
    digestTask.stop();
  }

  // Validate BEFORE handing to cron.schedule — node-cron throws a generic
  // Error on bad expressions which would otherwise surface as an unhandled
  // rejection from the 'ready' handler and cause a crash-loop.
  if (typeof cron.validate === 'function' && !cron.validate(config.WEEKLY_DIGEST_CRON)) {
    logger.error('Invalid WEEKLY_DIGEST_CRON expression, skipping digest schedule', {
      cron: config.WEEKLY_DIGEST_CRON,
    });
    return;
  }

  try {
    digestTask = cron.schedule(config.WEEKLY_DIGEST_CRON, async () => {
      logger.info('Running weekly digest...');
      await postWeeklyDigest();
    });
    logger.info(`Weekly digest scheduled: ${config.WEEKLY_DIGEST_CRON}`);
  } catch (err) {
    logger.error('cron.schedule failed, digest disabled', { error: err.message });
  }
}

// Permissions the bot needs to do its job. A missing permission here means
// a slash command will silently fail at runtime — log loud at boot instead
// so misconfigurations surface immediately. Non-fatal: we still boot in
// case the permission gap is intentional for a staging tenant.
//
// The required set depends on ENABLE_OPENNHP_FEATURES. The vanilla /qurl
// send tool only needs 4 perms (ViewChannel for target:channel/voice
// member enumeration, SendMessages for interaction replies, EmbedLinks
// for QURL link previews, UseApplicationCommands for slash commands).
// ManageRoles/ManageChannels are OpenNHP-only — demanding them in every
// guild would produce a confusing "missing permissions" error in guilds
// that correctly granted only the 4 runtime perms.
async function verifyBotPermissions() {
  try {
    const { PermissionFlagsBits } = require('discord.js');
    const me = await guild.members.fetchMe();
    const required = {
      ViewChannel: PermissionFlagsBits.ViewChannel,
      SendMessages: PermissionFlagsBits.SendMessages,
      EmbedLinks: PermissionFlagsBits.EmbedLinks,
      UseApplicationCommands: PermissionFlagsBits.UseApplicationCommands,
    };
    if (config.ENABLE_OPENNHP_FEATURES) {
      required.ManageRoles = PermissionFlagsBits.ManageRoles;
      required.ManageChannels = PermissionFlagsBits.ManageChannels;
      required.ReadMessageHistory = PermissionFlagsBits.ReadMessageHistory;
    }
    const missing = Object.entries(required)
      .filter(([, bit]) => !me.permissions.has(bit))
      .map(([name]) => name);
    if (missing.length > 0) {
      logger.error('Bot is missing required Discord permissions in guild', {
        guild: guild?.name, missing, opennhp: config.ENABLE_OPENNHP_FEATURES,
      });
    } else {
      logger.info('Bot permissions OK', { guild: guild?.name, opennhp: config.ENABLE_OPENNHP_FEATURES });
    }
  } catch (err) {
    logger.warn('Could not verify bot permissions at boot', { error: err.message });
  }
}

client.once('ready', async () => {
  logger.info(`Discord bot logged in as ${client.user.tag}`);
  await refreshCache();
  await verifyBotPermissions();
  // Weekly digest is OpenNHP-specific (star milestones, contributor
  // stats, announcements to #general). No value in a guild running the
  // bot purely for /qurl send.
  if (config.ENABLE_OPENNHP_FEATURES) {
    setupWeeklyDigest();
  }
  logger.info(`Watching guild: ${guild?.name}`, { opennhp: config.ENABLE_OPENNHP_FEATURES });
});

// Handle role/channel deletion - refresh cache
client.on('roleDelete', async (role) => {
  try {
    if (Object.values(roles).some(r => r?.id === role.id)) {
      logger.warn('A tracked role was deleted, refreshing cache');
      await refreshCache();
    }
  } catch (error) {
    logger.error('Error handling roleDelete event', { error: error.message });
  }
});

client.on('channelDelete', async (channel) => {
  try {
    if (Object.values(channels).some(c => c?.id === channel.id)) {
      logger.warn('A tracked channel was deleted, refreshing cache');
      await refreshCache();
    }
  } catch (error) {
    logger.error('Error handling channelDelete event', { error: error.message });
  }
});

// Welcome new members. Gated on ENABLE_OPENNHP_FEATURES — the handler
// posts "🎉 Welcome Back, Contributor!" to #general and a DM that
// explicitly greets the user as joining the OpenNHP community, including
// promises of contributor-role progression. Neither is appropriate in a
// guild that hasn't opted in. Skipping the handler outright also avoids
// the DB read (getContributions) for every join in vanilla guilds.
client.on('guildMemberAdd', async (member) => {
  if (!config.ENABLE_OPENNHP_FEATURES) return;
  if (member.guild.id !== config.GUILD_ID) return;

  logger.info(`New member joined: ${member.user.tag}`);

  // Check if returning contributor
  const contributions = db.getContributions(member.id);

  if (contributions.length > 0) {
    // Returning contributor - assign appropriate role
    logger.info(`Returning contributor ${member.user.tag} with ${contributions.length} PRs`);
    await updateMemberRoles(member, contributions.length);

    if (channels.general) {
      const embed = new EmbedBuilder()
        .setColor(0x3498DB)
        .setTitle('🎉 Welcome Back, Contributor!')
        .setDescription(
          `<@${member.id}> has rejoined with **${contributions.length}** merged PR(s)!`
        )
        .setTimestamp();

      await channels.general.send({ embeds: [embed] });
    }
    return;
  }

  // New member - send welcome DM
  if (config.WELCOME_DM_ENABLED) {
    try {
      const embed = new EmbedBuilder()
        .setColor(0x3498DB)
        .setTitle('👋 Welcome to OpenNHP!')
        .setDescription(
          'Thanks for joining the OpenNHP community! We\'re building the future of Zero Trust networking.\n\n' +
          '**Get Started:**\n' +
          '• Check out our repos at [github.com/OpenNHP](https://github.com/OpenNHP)\n' +
          '• Look for issues labeled `good first issue`\n' +
          '• Link your GitHub with `/link` to earn contributor badges!\n\n' +
          '**When you contribute:**\n' +
          `• 1 PR → **@${config.CONTRIBUTOR_ROLE_NAME}** role\n` +
          `• ${config.ACTIVE_CONTRIBUTOR_THRESHOLD} PRs → **@${config.ACTIVE_CONTRIBUTOR_ROLE_NAME}**\n` +
          `• ${config.CORE_CONTRIBUTOR_THRESHOLD} PRs → **@${config.CORE_CONTRIBUTOR_ROLE_NAME}**\n` +
          `• ${config.CHAMPION_THRESHOLD} PRs → **@${config.CHAMPION_ROLE_NAME}** 🏆\n\n` +
          'Happy contributing! 🚀'
        )
        .setFooter({ text: 'OpenNHP - Network-resource Hiding Protocol' });

      await member.send({ embeds: [embed] });
      logger.debug('Sent welcome DM', { userId: member.id });
    } catch (error) {
      logger.warn(`Failed to send welcome DM to ${member.user.tag}`, { error: error.message });
    }
  }
});

// Update member roles based on contribution count
async function updateMemberRoles(member, contributionCount) {
  if (!guild) await refreshCache();
  if (!member) return { success: false, reason: 'no_member' };

  const rolesToAdd = [];
  const currentRoles = member.roles.cache;

  // Determine which roles they should have
  if (contributionCount >= 1 && roles.contributor && !currentRoles.has(roles.contributor.id)) {
    rolesToAdd.push(roles.contributor);
  }
  if (contributionCount >= config.ACTIVE_CONTRIBUTOR_THRESHOLD && roles.activeContributor && !currentRoles.has(roles.activeContributor.id)) {
    rolesToAdd.push(roles.activeContributor);
  }
  if (contributionCount >= config.CORE_CONTRIBUTOR_THRESHOLD && roles.coreContributor && !currentRoles.has(roles.coreContributor.id)) {
    rolesToAdd.push(roles.coreContributor);
  }
  if (contributionCount >= config.CHAMPION_THRESHOLD && roles.champion && !currentRoles.has(roles.champion.id)) {
    rolesToAdd.push(roles.champion);
  }

  if (rolesToAdd.length === 0) {
    return { success: true, rolesAdded: [] };
  }

  try {
    await member.roles.add(rolesToAdd);
    const roleNames = rolesToAdd.map(r => r.name);
    logger.info(`Added roles to ${member.user.tag}`, { roles: roleNames });
    return { success: true, rolesAdded: roleNames };
  } catch (error) {
    logger.error(`Failed to add roles to ${member.user.tag}`, { error: error.message });
    return { success: false, reason: 'error', error: error.message };
  }
}

// Assign Contributor role and check for progression
async function assignContributorRole(discordId, prNumber, repo, githubUsername) {
  // No-op in guilds that haven't opted in to OpenNHP community features.
  // Callers (oauth.js, webhooks.js) still record the contribution in the
  // DB before calling this; the role-assign + announcement is the only
  // piece gated off. Returning { success: false, reason: 'opennhp-disabled' }
  // so callers can distinguish "flag off" from "Discord API error" in logs.
  if (!config.ENABLE_OPENNHP_FEATURES) {
    return { success: false, reason: 'opennhp-disabled' };
  }

  if (!guild) await refreshCache();

  try {
    const member = await guild.members.fetch(discordId);
    const contributionCount = db.getContributionCount(discordId);

    // Update all appropriate roles
    const result = await updateMemberRoles(member, contributionCount);

    // Check for role progression announcement
    if (result.rolesAdded && result.rolesAdded.length > 0) {
      // Announce new roles (skip basic Contributor for repeat contributors)
      const significantRoles = result.rolesAdded.filter(r => r !== config.CONTRIBUTOR_ROLE_NAME);

      if (significantRoles.length > 0 && channels.general) {
        const highestNewRole = significantRoles[significantRoles.length - 1];
        const embed = new EmbedBuilder()
          .setColor(0xF1C40F)
          .setTitle('🎖️ Role Upgrade!')
          .setDescription(
            `<@${discordId}> just reached **${contributionCount}** PRs and earned **@${highestNewRole}**!`
          )
          .setTimestamp();

        await channels.general.send({ embeds: [embed] });
      }

      // Announce first-time contributor
      if (contributionCount === 1 && result.rolesAdded.includes(config.CONTRIBUTOR_ROLE_NAME) && channels.general) {
        const embed = new EmbedBuilder()
          .setColor(0x3498DB)
          .setTitle('🎉 New Contributor!')
          .setDescription(
            `<@${discordId}> just got their first PR merged and earned **@Contributor**!`
          )
          .addFields(
            { name: 'Repository', value: repo, inline: true },
            { name: 'PR', value: `#${prNumber}`, inline: true },
            { name: 'GitHub', value: `@${githubUsername}`, inline: true }
          )
          .setTimestamp();

        await channels.general.send({ embeds: [embed] });
      }
    }

    return { success: true, rolesAdded: result.rolesAdded };
  } catch (error) {
    if (error.code === 10007 || error.message.includes('Unknown Member')) {
      logger.warn(`Member ${discordId} not found in server (may have left)`);
      return { success: false, reason: 'member_not_found' };
    }

    logger.error(`Failed to assign role to ${discordId}`, { error: error.message });
    return { success: false, reason: 'error', error: error.message };
  }
}

// Notify about a PR merge (for unlinked users)
async function notifyPRMerge(prNumber, repo, githubUsername, prTitle, prUrl) {
  if (!channels.general) await refreshCache();
  if (!channels.general) {
    logger.warn('Cannot notify PR merge - general channel not found');
    return null;
  }

  // Always escape at embed-construction time — callers reach this from
  // multiple paths (webhooks, historical contribution backfill) with
  // inconsistent escaping states. Double-escaping is preferable to a
  // gap that leaves a masked-link injection reachable.
  const embed = new EmbedBuilder()
    .setColor(0x2ECC71)
    .setTitle('🚀 PR Merged!')
    .setDescription(`**${md(prTitle)}**`)
    .addFields(
      { name: 'Author', value: `@${md(githubUsername)}`, inline: true },
      { name: 'Repository', value: md(repo), inline: true },
      { name: 'PR', value: `[#${prNumber}](${prUrl})`, inline: true }
    )
    .setFooter({ text: 'Link your GitHub with /link to auto-receive @Contributor role!' })
    .setTimestamp();

  try {
    const message = await channels.general.send({ embeds: [embed] });
    logger.info('Posted PR merge notification', { pr: prNumber, repo, author: githubUsername });
    return message;
  } catch (error) {
    logger.error('Failed to post PR merge notification', { error: error.message });
    return null;
  }
}

// Notify about badges earned
async function notifyBadgeEarned(discordId, badgeTypes) {
  // No-op when OpenNHP features are disabled — the bot has no standing
  // expectation that #general exists, and a 🏅 Badge Earned embed is
  // nonsensical in a guild that never opted in to the contributor
  // workflow in the first place.
  if (!config.ENABLE_OPENNHP_FEATURES) return;

  if (!channels.general) await refreshCache();
  if (!channels.general || badgeTypes.length === 0) return;

  const badgeInfo = badgeTypes.map(type => db.BADGE_INFO[type]);
  const badgeDisplay = badgeInfo.map(b => `${b.emoji} **${b.name}**`).join(', ');

  const embed = new EmbedBuilder()
    .setColor(0x9B59B6)
    .setTitle('🏅 Badge Earned!')
    .setDescription(`<@${discordId}> earned: ${badgeDisplay}`)
    .setTimestamp();

  try {
    await channels.general.send({ embeds: [embed] });
  } catch (error) {
    logger.error('Failed to post badge notification', { error: error.message });
  }
}

// Post good-first-issue to contribute channel. OpenNHP-only — posts to
// #contribute (which only exists in the OpenNHP guild) and is part of
// the community onboarding loop. Skipped entirely when the flag is off.
async function postGoodFirstIssue(repo, issueNumber, title, url, labels) {
  if (!config.ENABLE_OPENNHP_FEATURES) return null;

  if (!channels.contribute) await refreshCache();
  if (!channels.contribute) {
    logger.warn('Cannot post good-first-issue - contribute channel not found');
    return null;
  }

  const embed = new EmbedBuilder()
    .setColor(0x2ECC71)
    .setTitle('🌱 Good First Issue')
    .setDescription(`**${md(title)}**`)
    .addFields(
      { name: 'Repository', value: md(repo), inline: true },
      { name: 'Issue', value: `[#${issueNumber}](${url})`, inline: true }
    )
    .setFooter({ text: 'Great for new contributors!' })
    .setTimestamp();

  if (labels && labels.length > 0) {
    embed.addFields({
      name: 'Labels',
      // Wrapping in backticks is not enough — a label containing a backtick
      // would break out of the code span. escapeDiscordMarkdown neutralizes
      // every markdown metachar defensively.
      value: labels.slice(0, 5).map(l => `\`${md(l)}\``).join(' '),
      inline: false,
    });
  }

  try {
    const message = await channels.contribute.send({ embeds: [embed] });
    logger.info('Posted good-first-issue', { repo, issue: issueNumber });
    return message;
  } catch (error) {
    logger.error('Failed to post good-first-issue', { error: error.message });
    return null;
  }
}

// Post release announcement
async function postReleaseAnnouncement(repo, tagName, releaseName, url, body) {
  if (!channels.announcements) await refreshCache();
  if (!channels.announcements) {
    logger.warn('Cannot post release - announcements channel not found');
    return null;
  }

  const description = body
    ? body.substring(0, 500) + (body.length > 500 ? '...' : '')
    : 'No release notes provided.';
  // Cap each component independently so a very long releaseName can't push
  // the combined description toward Discord's 4096-char embed limit and
  // swallow the body.
  const cappedReleaseName = (releaseName || tagName).slice(0, 256);

  const embed = new EmbedBuilder()
    .setColor(0x3498DB)
    .setTitle(`🚀 New Release: ${md(tagName)}`)
    .setDescription(`**${md(cappedReleaseName)}**\n\n${md(description)}`)
    .addFields(
      { name: 'Repository', value: md(repo), inline: true },
      { name: 'Version', value: `[${md(tagName)}](${url})`, inline: true }
    )
    .setTimestamp();

  try {
    const message = await channels.announcements.send({ embeds: [embed] });
    logger.info('Posted release announcement', { repo, tag: tagName });
    return message;
  } catch (error) {
    logger.error('Failed to post release', { error: error.message });
    return null;
  }
}

// Post star milestone
async function postStarMilestone(repo, stars, repoUrl) {
  if (!channels.announcements) await refreshCache();
  if (!channels.announcements) {
    logger.warn('Cannot post milestone - announcements channel not found');
    return null;
  }

  const embed = new EmbedBuilder()
    .setColor(0xF1C40F)
    .setTitle('⭐ Star Milestone!')
    .setDescription(`**${md(repo)}** just reached **${stars}** stars! 🎉`)
    .addFields(
      { name: 'Repository', value: `[View on GitHub](${repoUrl})`, inline: true }
    )
    .setTimestamp();

  try {
    const message = await channels.announcements.send({ embeds: [embed] });
    logger.info('Posted star milestone', { repo, stars });
    return message;
  } catch (error) {
    logger.error('Failed to post star milestone', { error: error.message });
    return null;
  }
}

// Post to GitHub feed
async function postToGitHubFeed(embed) {
  if (!channels.githubFeed) await refreshCache();
  if (!channels.githubFeed) return null;

  try {
    return await channels.githubFeed.send({ embeds: [embed] });
  } catch (error) {
    logger.error('Failed to post to GitHub feed', { error: error.message });
    return null;
  }
}

// Post weekly digest
async function postWeeklyDigest() {
  if (!channels.general) await refreshCache();
  if (!channels.general) {
    logger.warn('Cannot post weekly digest - general channel not found');
    return null;
  }

  const data = db.getWeeklyDigestData();

  if (data.totalPRs === 0) {
    logger.info('No activity this week, skipping digest');
    return null;
  }

  const embed = new EmbedBuilder()
    .setColor(0x9B59B6)
    .setTitle('📊 Weekly Digest')
    .setDescription(`Here's what happened in OpenNHP this week!`)
    .addFields(
      { name: '🔀 PRs Merged', value: `${data.totalPRs}`, inline: true },
      { name: '👥 Contributors', value: `${data.uniqueContributors}`, inline: true },
      { name: '🌱 New Contributors', value: `${data.newContributors.length}`, inline: true }
    )
    .setTimestamp();

  // Add top repos
  const repoEntries = Object.entries(data.byRepo).slice(0, 3);
  if (repoEntries.length > 0) {
    const repoList = repoEntries.map(([repo, prs]) => `• ${repo}: ${prs.length} PRs`).join('\n');
    embed.addFields({ name: '🏆 Most Active Repos', value: repoList });
  }

  // Welcome new contributors
  if (data.newContributors.length > 0) {
    const newList = data.newContributors
      .slice(0, 5)
      .map(c => `<@${c.discord_id}>`)
      .join(', ');
    embed.addFields({
      name: '👋 Welcome New Contributors',
      value: newList + (data.newContributors.length > 5 ? ` and ${data.newContributors.length - 5} more!` : ''),
    });
  }

  embed.setFooter({ text: 'Keep up the great work! 🚀' });

  try {
    const message = await channels.general.send({ embeds: [embed] });
    logger.info('Posted weekly digest', { prs: data.totalPRs, contributors: data.uniqueContributors });
    return message;
  } catch (error) {
    logger.error('Failed to post weekly digest', { error: error.message });
    return null;
  }
}

// Send DM to user
async function sendDM(discordId, message) {
  try {
    const user = await client.users.fetch(discordId);
    await user.send(message);
    logger.debug('Sent DM', { discordId });
    return true;
  } catch (error) {
    logger.warn(`Failed to DM user ${discordId}`, { error: error.message });
    return false;
  }
}

// Depends on guild.members.cache being populated (caller must fetch first).
// Discord limits fetch to ~1000 members without GUILD_PRESENCES intent.
// For guilds >1000 members, some viewers may be missed.

// Get members in the sender's voice channel (excludes bots and the sender)
function getVoiceChannelMembers(guildObj, senderUserId) {
  const senderState = guildObj.voiceStates.cache.get(senderUserId);
  if (!senderState || !senderState.channel) {
    return { error: 'not_in_voice', members: [] };
  }

  const members = senderState.channel.members
    .filter(m => m.id !== senderUserId && !m.user.bot)
    .map(m => m.user);

  return {
    error: null,
    members,
    channelName: senderState.channel.name,
  };
}

// Get non-bot members who can view a text channel (excludes the sender)
function getTextChannelMembers(channel, senderUserId) {
  const members = channel.members
    .filter(m => m.id !== senderUserId && !m.user.bot)
    .map(m => m.user);

  return members;
}

// Graceful shutdown — awaits client.destroy() so the caller knows the
// WebSocket is fully closed and no further events will fire. discord.js
// v14 returns a Promise from destroy(); swallowing it risked dropped
// messages during ECS rolling deploys.
async function shutdown() {
  logger.info('Discord client shutting down');
  if (digestTask) {
    digestTask.stop();
  }
  try {
    await client.destroy();
  } catch (err) {
    logger.warn('client.destroy() threw during shutdown (continuing)', { error: err?.message });
  }
}

module.exports = {
  client,
  assignContributorRole,
  notifyPRMerge,
  notifyBadgeEarned,
  postGoodFirstIssue,
  postReleaseAnnouncement,
  postStarMilestone,
  postToGitHubFeed,
  postWeeklyDigest,
  sendDM,
  refreshCache,
  shutdown,
  getVoiceChannelMembers,
  getTextChannelMembers,
  getGuild: () => guild,
  getRoles: () => roles,
  getChannels: () => channels,
};
