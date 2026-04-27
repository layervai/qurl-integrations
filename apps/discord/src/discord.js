const { Client, GatewayIntentBits, EmbedBuilder, ChannelType, PermissionFlagsBits } = require('discord.js');
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
  if (!config.isOpenNHPActive) {
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
//
// Return shape: the function resolves to `undefined` regardless of
// mode. In multi-tenant mode it short-circuits immediately (no work,
// no state mutation); in single-guild mode it populates the
// module-level `guild` / `roles` / `channels` caches as a side
// effect, but the resolved value is still undefined. All call-sites
// `await refreshCache()` for sequencing and then read the cached
// state directly — none inspect the return value, which is why the
// side-effect-only contract is safe.
let refreshCacheInFlight = null;
async function refreshCache() {
  // Multi-tenant mode: there is no single watched guild to cache.
  // client.guilds.fetch(null) would return ALL guilds the bot is in as a
  // Collection (not a single Guild), and the downstream guild.roles.fetch()
  // call would then crash on a Collection that has no .roles. Short-circuit
  // to a no-op — all callers already check `if (!guild)` before using cached
  // state, and this function doesn't populate `guild` so those sites skip
  // gracefully. Belt-and-suspenders: OpenNHP-command registration and
  // /auth + /webhook route mounting are also gated in multi-tenant mode.
  if (!config.GUILD_ID) return;

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
// for qURL link previews, UseApplicationCommands for slash commands).
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
    if (config.isOpenNHPActive) {
      required.ManageRoles = PermissionFlagsBits.ManageRoles;
      required.ManageChannels = PermissionFlagsBits.ManageChannels;
      // Note: `ReadMessageHistory` was previously demanded here but no
      // OpenNHP code path actually reads message history (verified by
      // grep — `messages.fetch` / `channel.messages.*` / thread backfill
      // all absent). Adding it to the OAuth invite bitmask creates
      // friction for guild admins for no operational gain. Re-add only
      // if a future OpenNHP feature genuinely needs it.
    }
    const missing = Object.entries(required)
      .filter(([, bit]) => !me.permissions.has(bit))
      .map(([name]) => name);
    if (missing.length > 0) {
      logger.error('Bot is missing required Discord permissions in guild', {
        guild: guild?.name, missing, opennhp_active: config.isOpenNHPActive,
      });
    } else {
      logger.info('Bot permissions OK', { guild: guild?.name, opennhp_active: config.isOpenNHPActive });
    }
  } catch (err) {
    logger.warn('Could not verify bot permissions at boot', { error: err.message });
  }
}

client.once('ready', async () => {
  logger.info(`Discord bot logged in as ${client.user.tag}`);

  // Multi-tenant mode: GUILD_ID unset means no single "watched" guild.
  // refreshCache() / verifyBotPermissions() / setupWeeklyDigest() are all
  // single-guild operations (the first fetches one guild's roles/channels,
  // the second checks perms in one guild, the third posts to one channel).
  // In multi-tenant mode these are dormant — lazy refreshCache() calls in
  // downstream OpenNHP features are already gated behind routes that don't
  // fire without GITHUB_WEBHOOK_SECRET + BASE_URL wiring.
  if (!config.GUILD_ID) {
    logger.info('Multi-tenant mode: GUILD_ID unset — skipping single-guild cache init.');
    logger.info('Bot is ready. /qurl commands will appear in any guild the bot joins.');
    return;
  }

  await refreshCache();
  await verifyBotPermissions();
  // Weekly digest is OpenNHP-specific (star milestones, contributor
  // stats, announcements to #general). No value in a guild running the
  // bot purely for /qurl send.
  if (config.isOpenNHPActive) {
    setupWeeklyDigest();
  }
  logger.info(`Watching guild: ${guild?.name}`, { opennhp_active: config.isOpenNHPActive });
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
  // OpenNHP-only behavior. Two short-circuits before any work:
  //   1. Flag off → no welcome DM / "Welcome Back, Contributor!" embed
  //      anywhere, regardless of whether the bot is single-guild or
  //      multi-tenant. A vanilla /qurl send install shouldn't introduce
  //      itself as the OpenNHP community bot.
  //   2. In multi-tenant mode (no single GUILD_ID to scope to) there is
  //      no cached `guild`/`channels` state to post into anyway. The
  //      existing `member.guild.id !== config.GUILD_ID` below would also
  //      catch that (null !== any-id is always true), but an explicit
  //      guard keeps the intent readable.
  if (!config.isOpenNHPActive) return;
  if (!config.GUILD_ID) return;
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
  // Log at debug so the "why did user X not get the role" question is
  // answerable from prod logs without adding noise for every PR merge.
  if (!config.isOpenNHPActive) {
    logger.debug('assignContributorRole skipped: OpenNHP features disabled', {
      discordId, prNumber, repo, githubUsername,
    });
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
  // Same OpenNHP gate as the other notifier helpers — debug-log the
  // skip so prod triage of "why was a PR merge not announced" is
  // answerable without reading #general directly, and the "channel not
  // found" warn below stays reserved for actual misconfigurations
  // (OpenNHP active but #general missing) rather than the normal
  // non-OpenNHP fall-through. Defense-in-depth; /webhook routes aren't
  // mounted outside OpenNHP mode so this is also unreachable.
  if (!config.isOpenNHPActive) {
    logger.debug('notifyPRMerge skipped: OpenNHP features disabled', { prNumber, repo });
    return null;
  }

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
  if (!config.isOpenNHPActive) {
    logger.debug('notifyBadgeEarned skipped: OpenNHP features disabled', {
      discordId, badgeCount: badgeTypes?.length ?? 0,
    });
    return;
  }

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
  if (!config.isOpenNHPActive) {
    logger.debug('postGoodFirstIssue skipped: OpenNHP features disabled', {
      repo, issueNumber,
    });
    return null;
  }

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
  if (!config.isOpenNHPActive) {
    logger.debug('postReleaseAnnouncement skipped: OpenNHP features disabled', { repo, tagName });
    return null;
  }

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
  if (!config.isOpenNHPActive) {
    logger.debug('postStarMilestone skipped: OpenNHP features disabled', { repo, stars });
    return null;
  }

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
  if (!config.isOpenNHPActive) {
    logger.debug('postToGitHubFeed skipped: OpenNHP features disabled');
    return null;
  }

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
  if (!config.isOpenNHPActive) {
    // The ready handler already gates setupWeeklyDigest() on the flag,
    // so this branch is only reachable via a direct caller in a future
    // refactor. Defense-in-depth — matches every other notifier.
    logger.debug('postWeeklyDigest skipped: OpenNHP features disabled');
    return null;
  }

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

// Send DM to user. Returns { ok, channelId, messageId } so callers that
// need to edit the message later (e.g. the expired-dm-label sweeper, which
// flips "Portal closes" → "Portal closed" once the link has expired) can
// persist the identifiers. `ok=false` means delivery failed (user blocked
// DMs, fetch error, etc.); channel/message IDs are null in that case.
async function sendDM(discordId, message) {
  try {
    const user = await client.users.fetch(discordId);
    const sent = await user.send(message);
    logger.debug('Sent DM', { discordId });
    return { ok: true, channelId: sent.channelId, messageId: sent.id };
  } catch (error) {
    logger.warn(`Failed to DM user ${discordId}`, { error: error.message });
    return { ok: false, channelId: null, messageId: null };
  }
}

// Edit a previously-sent DM to flip the present-tense expiry verb to past
// tense. Used by the expired-dm-label sweeper once `<t:N:R>` has already
// rolled over to "X minutes ago". Discord's relative-time markdown updates
// client-side automatically, but the surrounding literal "closes" stays
// present-tense unless we rewrite the embed.
//
// Returns:
//   true  — edit succeeded OR the embed was already past-tense / didn't
//           contain the expected prefix (treat as done; don't retry).
//   false — permanent failure (channel/message gone, missing access).
//           Caller marks the row as edited to stop the sweeper from
//           retrying every minute forever.
//   throw — transient failure (network, 5xx). Caller leaves row unedited
//           so the next sweep retries.
async function editDMToPastTense(channelId, messageId, fromPrefix, toPrefix) {
  try {
    const channel = await client.channels.fetch(channelId);
    if (!channel) return false;
    const msg = await channel.messages.fetch(messageId);
    if (!msg) return false;

    let mutated = false;
    const newEmbeds = msg.embeds.map(e => {
      const json = e.toJSON();
      if (Array.isArray(json.fields)) {
        json.fields = json.fields.map(f => {
          if (typeof f.value === 'string' && f.value.includes(fromPrefix)) {
            mutated = true;
            return { ...f, value: f.value.replace(fromPrefix, toPrefix) };
          }
          return f;
        });
      }
      return EmbedBuilder.from(json);
    });

    // No prefix found ⇒ either the embed shape changed or the message
    // was already edited (e.g. an in-flight retry from a prior crash).
    // Treat as done so the row is marked edited and we don't loop.
    if (!mutated) return true;

    await msg.edit({ embeds: newEmbeds });
    return true;
  } catch (err) {
    // Discord error codes that mean "this DM is gone, don't retry":
    //   10003 Unknown Channel       — channel was deleted
    //   10008 Unknown Message       — message was deleted
    //   50001 Missing Access        — bot was blocked / removed from DM
    //   50007 Cannot send DM to user — recipient blocked DMs since send
    if ([10003, 10008, 50001, 50007].includes(err.code)) return false;
    throw err;
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

// Get non-bot members who can view a channel (excludes the sender).
//
// Voice/stage-voice channels have a gotcha: in discord.js, `channel.members`
// returns members CURRENTLY CONNECTED to voice — NOT everyone who can see
// the channel. For text channels `channel.members` is computed off guild
// members + ViewChannel permission, which is the semantic we want for
// "Everyone in this channel." So for voice/stage-voice we compute the
// viewer set explicitly from guild.members.cache.
//
// Callers rely on the sender having already done `guild.members.fetch()`
// so the cache is warm; see the `target === 'channel'` branch in
// handleSend (commands.js) for the fetch + call site pairing.
function getTextChannelMembers(channel, senderUserId) {
  const isVoice = channel.type === ChannelType.GuildVoice ||
                  channel.type === ChannelType.GuildStageVoice;

  let source;
  if (isVoice) {
    // Voice path: enumerate guild viewers via permissionsFor. If the
    // channel is voice-typed but `.guild` is somehow missing (partial
    // cache / unusual gateway state), do NOT silently fall back to
    // `channel.members` — that's the voice-connected-only set which
    // is exactly the bug this helper exists to avoid. Return empty and
    // log so a caller's "no recipients" branch triggers loudly instead
    // of shipping to a wrong subset.
    if (!channel.guild) {
      logger.warn('getTextChannelMembers: voice channel missing .guild; returning empty', {
        channelId: channel.id, channelType: channel.type,
      });
      return [];
    }
    source = channel.guild.members.cache.filter(m => {
      const perms = channel.permissionsFor(m);
      return perms && perms.has(PermissionFlagsBits.ViewChannel);
    });
  } else {
    // Text channel: `channel.members` is already the viewer set.
    source = channel.members;
  }

  return source
    .filter(m => m.id !== senderUserId && !m.user.bot)
    .map(m => m.user);
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
  editDMToPastTense,
  refreshCache,
  shutdown,
  getVoiceChannelMembers,
  getTextChannelMembers,
  getGuild: () => guild,
  getRoles: () => roles,
  getChannels: () => channels,
};
