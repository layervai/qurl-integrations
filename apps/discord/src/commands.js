const {
  SlashCommandBuilder,
  EmbedBuilder,
  PermissionFlagsBits,
  ActionRowBuilder,
  ButtonBuilder,
  ButtonStyle,
  ComponentType,
  StringSelectMenuBuilder,
  UserSelectMenuBuilder,
  ModalBuilder,
  TextInputBuilder,
  TextInputStyle,
} = require('discord.js');
const crypto = require('crypto');
const config = require('./config');
const db = require('./database');
const logger = require('./logger');
const { COLORS, TIMEOUTS, LIMITS } = require('./constants');
const { requireAdmin } = require('./utils/admin');
const { createOneTimeLink, deleteLink, getResourceStatus } = require('./qurl');
const { uploadToConnector, mintLinks } = require('./connector');
const { getVoiceChannelMembers, getTextChannelMembers, sendDM } = require('./discord');
const { searchPlaces } = require('./places');

// Generate secure random state
function generateState() {
  return crypto.randomBytes(16).toString('hex');
}

// --- QURL send helpers ---

function isGoogleMapsURL(url) {
  try {
    const parsed = new URL(url);
    if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') return false;
    const host = parsed.hostname.toLowerCase();
    // Match google.com, google.co.uk, google.com.au but not google.evil.com
    const isGoogleHost = /^(www\.)?google\.[a-z]{2,3}(\.[a-z]{2})?$/.test(host) ||
                         /^maps\.google\.[a-z]{2,3}(\.[a-z]{2})?$/.test(host) ||
                         host === 'goo.gl' ||
                         host === 'maps.app.goo.gl';
    if (!isGoogleHost) return false;
    if (host === 'goo.gl') return parsed.pathname.startsWith('/maps/');
    if (host === 'maps.app.goo.gl') return true;
    if (host.startsWith('maps.google.')) return true;
    return parsed.pathname.startsWith('/maps');
  } catch {
    return false;
  }
}



function sanitizeFilename(name) {
  return name
    .replace(/[/\\:*?"<>|]/g, '_')
    .replace(/\.\./g, '_')
    .replace(/[\x00-\x1f\x7f]/g, '')
    .substring(0, 200);
}

function sanitizeMessage(msg) {
  return msg
    .replace(/@(everyone|here)/gi, '@\u200b$1')
    .replace(/<@[!&]?\d+>/g, '[mention]')
    .slice(0, 500); // cap to fit Discord embed field limit (1024) with room for formatting
}

const ALLOWED_FILE_TYPES = [
  'image/', 'application/pdf', 'video/', 'audio/',
  'text/plain', 'text/csv',
  'application/zip', 'application/x-zip-compressed',
  'application/vnd.openxmlformats',
  'application/vnd.ms-',
  'application/msword',
];

function isAllowedFileType(contentType) {
  if (!contentType) return false;
  return ALLOWED_FILE_TYPES.some(prefix => contentType.startsWith(prefix));
}

const sendCooldowns = new Map();

function isOnCooldown(userId) {
  const last = sendCooldowns.get(userId);
  if (!last) return false;
  return Date.now() - last < config.QURL_SEND_COOLDOWN_MS;
}

function setCooldown(userId) {
  sendCooldowns.set(userId, Date.now());
}

function clearCooldown(userId) {
  sendCooldowns.delete(userId);
}

async function batchSettled(items, fn, batchSize = 5) {
  const results = [];
  for (let i = 0; i < items.length; i += batchSize) {
    const batch = items.slice(i, i + batchSize);
    const batchResults = await Promise.allSettled(batch.map(fn));
    results.push(...batchResults);
  }
  return results;
}

function expiryToISO(expiresIn) {
  const units = { m: 60, h: 3600, d: 86400 };
  const match = expiresIn.match(/^(\d+)([mhd])$/);
  if (!match) return new Date(Date.now() + 86400000).toISOString();
  const ms = parseInt(match[1]) * units[match[2]] * 1000;
  return new Date(Date.now() + ms).toISOString();
}

// --- Shared DM embed builder ---
function buildDeliveryEmbed({ senderUsername, resourceType, resourceLabel, qurlLink, expiresIn, filename, personalMessage }) {
  const isFile = resourceType === 'file';
  const divider = '\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500';
  const embed = new EmbedBuilder()
    .setColor(0x00d4ff)
    .setAuthor({ name: 'QURL Secure Delivery' })
    .setDescription(
      `**${senderUsername}** shared a protected resource with you\n${divider}`
    )
  if (isFile) {
    embed.addFields(
      { name: 'Resource Type', value: 'File', inline: true },
      { name: 'Filename', value: filename, inline: true },
      { name: '\u200b', value: '\u200b', inline: true },
    );
  } else {
    embed.addFields(
      { name: 'Resource Type', value: 'Location', inline: true },
    );
  }

  if (personalMessage) {
    embed.addFields({ name: 'Message', value: `> ${personalMessage}` });
  }

  embed.addFields(
    { name: 'QURL Link', value: qurlLink },
    { name: divider, value:
      `\u23f3 Expires in **${expiresIn}**\n` +
      '\ud83d\udd12 One-time access\n' +
      `${divider}\n` +
      '\u26a0\ufe0f Invisible before accessed\n' +
      '\ud83d\udca5 Self-destructs after viewing\n' +
      divider,
    }
  )
  .setFooter({ text: '\ud83d\udd10 QURL (Quantum URL): Invisible by default. Visible by permission.' })
  .setTimestamp();

  return embed;
}

// --- Link status monitor ---
function monitorLinkStatus(sendId, interaction, qurlLinks, recipients, expiresIn, baseMsg, buttonRow, delivered) {
  // Returns a control object: call monitor.addRecipients(count) when new recipients are added
  let currentBaseMsg = baseMsg;
  let stopped = false;
  let timer = null; // assigned after control object
  const control = {
    addRecipients(count, newResourceIds) {
      expectedCount += count;
      if (newResourceIds) {
        for (const rid of newResourceIds) {
          if (!resourceIds.includes(rid)) resourceIds.push(rid);
        }
      }
      trackedQurlIds = null;
    },
    stop() {
      stopped = true;
      clearInterval(timer);
    },
    updateBaseMsg(msg) {
      currentBaseMsg = msg;
    },
    getFullMsg() {
      return buildStatusMsg();
    },
  };
  const expiryUnits = { m: 60000, h: 3600000, d: 86400000 };
  const match = expiresIn.match(/^(\d+)([mhd])$/);
  const expiryMs = match ? parseInt(match[1]) * expiryUnits[match[2]] : 86400000;
  const maxMonitorMs = expiryMs + 60000;

  const resourceIds = [...new Set(qurlLinks.map(l => l.resourceId))];
  let expectedCount = delivered;

  // Track status per qurl_id: { status, username }
  const linkStatus = new Map();
  let trackedQurlIds = null;
  let allDone = false;

  const pollInterval = Math.max(15000, Math.min(60000, expiryMs / 10));
  const startTime = Date.now();

  const MAX_NAMES_SHOWN = 5;
  let expanded = false;

  function truncateNames(names) {
    if (names.length <= MAX_NAMES_SHOWN || expanded) return names.join(', ');
    return names.slice(0, MAX_NAMES_SHOWN).join(', ') + ` +${names.length - MAX_NAMES_SHOWN} more`;
  }

  function buildStatusMsg() {
    let opened = 0, expired = 0, pending = 0;
    for (const s of linkStatus.values()) {
      if (s.status === 'opened') opened++;
      else if (s.status === 'expired') expired++;
      else pending++;
    }
    let msg = currentBaseMsg;
    if (linkStatus.size > 0) {
      msg += '\n\n**Link Status:**';
      if (opened > 0) msg += `\n\u2705 ${opened} of ${linkStatus.size} opened`;
      if (expired > 0) msg += `\n\u23f0 ${expired} expired`;
      if (pending > 0) msg += `\n\u23f3 ${pending} pending`;
      if (pending === 0) msg += linkStatus.size === 1
        ? `\n\n\u2714\ufe0f **Link resolved**`
        : `\n\n\u2714\ufe0f **All ${linkStatus.size} links resolved**`;
    }
    return msg;
  }

  timer = setInterval(async () => {
    if (stopped || allDone || Date.now() - startTime > maxMonitorMs) {
      clearInterval(timer);
      const finalMsg = buildStatusMsg() + '\n(Use `/qurl revoke` to revoke later)';
      await interaction.editReply({ content: finalMsg, components: [] }).catch(() => {});
      return;
    }
    try {
      let changed = false;

      // Initialize tracking on first poll — scan all resources
      if (!trackedQurlIds) {
        trackedQurlIds = new Set();
        let recipientIdx = 0;

        if (resourceIds.length === 1) {
          // File send: one resource, N qurls — pick the N most recent
          const data = await getResourceStatus(resourceIds[0]);
          if (data && data.qurls) {
            const sorted = [...data.qurls].sort((a, b) => new Date(a.created_at) - new Date(b.created_at));
            const recentN = sorted.slice(-expectedCount);
            recentN.forEach((q) => {
              trackedQurlIds.add(q.qurl_id);
              const username = recipients[recipientIdx] ? recipients[recipientIdx].username : `user-${recipientIdx + 1}`;
              linkStatus.set(q.qurl_id, { status: 'pending', username });
              recipientIdx++;
            });
          }
        } else {
          // Location send: one resource per recipient — pick the most recent qurl from each
          for (const resourceId of resourceIds) {
            const data = await getResourceStatus(resourceId);
            if (!data || !data.qurls || data.qurls.length === 0) continue;
            const sorted = [...data.qurls].sort((a, b) => new Date(b.created_at) - new Date(a.created_at));
            const q = sorted[0]; // most recent
            trackedQurlIds.add(q.qurl_id);
            const username = recipients[recipientIdx] ? recipients[recipientIdx].username : `user-${recipientIdx + 1}`;
            linkStatus.set(q.qurl_id, { status: 'pending', username });
            recipientIdx++;
          }
        }
        logger.info('Link monitor tracking', { sendId, tracked: trackedQurlIds.size, resources: resourceIds.length });
      }

      // Poll all resources for status changes
      for (const resourceId of resourceIds) {
        const data = await getResourceStatus(resourceId);
        if (!data || !data.qurls) continue;
        for (const qurl of data.qurls) {
          if (!trackedQurlIds.has(qurl.qurl_id)) continue;
          const current = linkStatus.get(qurl.qurl_id);
          if (qurl.use_count > 0 && current.status !== 'opened') {
            linkStatus.set(qurl.qurl_id, { ...current, status: 'opened' }); changed = true;
          } else if (qurl.status === 'expired' && current.status === 'pending') {
            linkStatus.set(qurl.qurl_id, { ...current, status: 'expired' }); changed = true;
          }
        }
      }
      if (changed) {
        const pending = [...linkStatus.values()].filter(s => s.status === 'pending').length;
        await interaction.editReply({ content: buildStatusMsg(), components: pending > 0 ? [buttonRow] : [] }).catch(() => {});
        if (pending === 0) { allDone = true; clearInterval(timer); }
      }
    } catch (err) {
      logger.error('Link monitor poll failed', { sendId, error: err.message });
    }
  }, pollInterval);

  return control;
}

// --- /qurl send handler ---

async function handleSend(interaction) {
  const target = interaction.options.getString('target');
  const expiresIn = interaction.options.getString('expiry') || '24h';
  const rawMessage = interaction.options.getString('message');
  const personalMessage = rawMessage ? sanitizeMessage(rawMessage) : null;
  const commandAttachment = interaction.options.getAttachment('file_optional');

  if (isOnCooldown(interaction.user.id)) {
    return interaction.reply({ content: 'Please wait before sending again.', ephemeral: true });
  }
  // Set cooldown immediately to prevent concurrent request bypass
  setCooldown(interaction.user.id);

  if (!config.QURL_API_KEY) {
    clearCooldown(interaction.user.id);
    return interaction.reply({ content: 'QURL is not configured. Contact an admin.', ephemeral: true });
  }

  const sendNonce = crypto.randomBytes(8).toString('hex');

  // --- Step 1: Collect user (if specific user target) ---
  let recipients = [];

  if (target === 'user') {
    const userSelectRow = new ActionRowBuilder().addComponents(
      new UserSelectMenuBuilder()
        .setCustomId(`qurl_user_${sendNonce}`)
        .setPlaceholder('Select a user to send to')
        .setMinValues(1)
        .setMaxValues(1)
    );

    await interaction.reply({ content: '**Select the user to send to:**', components: [userSelectRow], ephemeral: true });

    try {
      const selectInteraction = await interaction.channel.awaitMessageComponent({
        filter: (i) => i.customId === `qurl_user_${sendNonce}` && i.user.id === interaction.user.id,
        componentType: ComponentType.UserSelect,
        time: 60000,
      });

      const selectedUser = selectInteraction.users.first();
      if (selectedUser.bot) {
        clearCooldown(interaction.user.id);
        return selectInteraction.update({ content: 'Cannot send to a bot.', components: [] });
      }
      if (selectedUser.id === interaction.user.id) {
        clearCooldown(interaction.user.id);
        return selectInteraction.update({ content: 'Cannot send to yourself.', components: [] });
      }
      recipients = [selectedUser];
      await selectInteraction.deferUpdate();
    } catch (err) {
      logger.debug('User select timed out or dismissed', { error: err?.message });
      clearCooldown(interaction.user.id);
      return interaction.editReply({ content: 'No user selected. Send cancelled.', components: [] });
    }
  } else if (target === 'channel') {
    await interaction.deferReply({ ephemeral: true });
    try {
      await interaction.guild.members.fetch();
    } catch (err) {
      logger.error('Failed to fetch guild members', { error: err.message });
      clearCooldown(interaction.user.id);
      return interaction.editReply({ content: 'Failed to load channel members. Please try again.' });
    }
    recipients = getTextChannelMembers(interaction.channel, interaction.user.id);
    if (recipients.length === 0) {
      clearCooldown(interaction.user.id);
      return interaction.editReply({ content: 'No other members in this channel.' });
    }
  } else if (target === 'voice') {
    await interaction.deferReply({ ephemeral: true });
    const result = getVoiceChannelMembers(interaction.guild, interaction.user.id);
    if (result.error === 'not_in_voice') {
      clearCooldown(interaction.user.id);
      return interaction.editReply({ content: 'You must be in a voice channel to use this target.' });
    }
    if (result.members.length === 0) {
      clearCooldown(interaction.user.id);
      return interaction.editReply({ content: 'No other users in your voice channel.' });
    }
    recipients = result.members;
  }

  if (recipients.length > config.QURL_SEND_MAX_RECIPIENTS) {
    clearCooldown(interaction.user.id);
    return interaction.editReply({ content: `Too many recipients (${recipients.length}). Maximum is ${config.QURL_SEND_MAX_RECIPIENTS}.`, components: [] });
  }

  // --- Step 2: Resource — auto-detect from file attachment ---
  let attachment = null;
  let locationUrl = null;
  let locationName = null;
  let resourceType = null;
  let resourceLabel = null;

  const targetLabel = target === 'user' ? recipients[0].username : target === 'channel' ? 'this channel' : 'voice channel';

  if (commandAttachment) {
    // File attached — show only Send File button
    const fileRow = new ActionRowBuilder().addComponents(
      new ButtonBuilder()
        .setCustomId(`qurl_res_file_${sendNonce}`)
        .setLabel('\ud83d\udcc1 Send File')
        .setStyle(ButtonStyle.Primary),
    );

    await interaction.editReply({
      content: `**Sending to ${targetLabel}** — file detected: **${commandAttachment.name}**`,
      components: [fileRow],
    });
  } else {
    // No file — show only Send Location button
    const locRow = new ActionRowBuilder().addComponents(
      new ButtonBuilder()
        .setCustomId(`qurl_res_loc_${sendNonce}`)
        .setLabel('\ud83d\uddfa\ufe0f Send Location')
        .setStyle(ButtonStyle.Primary),
    );

    await interaction.editReply({
      content: `**Sending to ${targetLabel}** — share a location:`,
      components: [locRow],
    });
  }

  let resInteraction;
  try {
    resInteraction = await interaction.channel.awaitMessageComponent({
      filter: (i) => i.user.id === interaction.user.id &&
        (i.customId === `qurl_res_file_${sendNonce}` || i.customId === `qurl_res_loc_${sendNonce}`),
      time: 60000,
    });
  } catch (err) {
    logger.debug('Resource selection timed out or dismissed', { error: err?.message });
    clearCooldown(interaction.user.id);
    return interaction.editReply({ content: 'No selection made. Send cancelled.', components: [] });
  }

  if (resInteraction.customId === `qurl_res_loc_${sendNonce}`) {
    // --- Location: show modal ---
    resourceType = 'maps';

    const modal = new ModalBuilder()
      .setCustomId(`qurl_loc_modal_${sendNonce}`)
      .setTitle('Share a Location');

    const locationInput = new TextInputBuilder()
      .setCustomId('location_value')
      .setLabel('Google Maps link or place name')
      .setPlaceholder('https://maps.app.goo.gl/... or Eiffel Tower, Paris')
      .setStyle(TextInputStyle.Short)
      .setRequired(true);

    modal.addComponents(new ActionRowBuilder().addComponents(locationInput));
    await resInteraction.showModal(modal);

    let modalSubmit;
    try {
      modalSubmit = await resInteraction.awaitModalSubmit({
        filter: (i) => i.customId === `qurl_loc_modal_${sendNonce}`,
        time: 120000,
      });
    } catch (err) {
      logger.debug('Location modal timed out or dismissed', { error: err?.message });
      clearCooldown(interaction.user.id);
      return interaction.editReply({ content: 'Location input timed out. Send cancelled.', components: [] });
    }

    const rawLocationValue = modalSubmit.fields.getTextInputValue('location_value').trim();
    await modalSubmit.deferUpdate();

    // Handle place_id: prefix from autocomplete selections
    const placeIdMatch = rawLocationValue.match(/^place_id:([^|]+)\|(.+)$/);
    if (placeIdMatch) {
      locationUrl = `https://www.google.com/maps/place/?q=place_id:${encodeURIComponent(placeIdMatch[1])}`;
      locationName = placeIdMatch[2];
    }

    const locationValue = placeIdMatch ? null : rawLocationValue; // skip pattern matching if place_id handled

    if (locationValue) {
      const mapsPatterns = [
        /https?:\/\/(?:www\.)?google\.com\/maps\/(?:place|search|dir|@)[\w/.,@?=&+%-]+/,
        /https?:\/\/(?:goo\.gl\/maps|maps\.app\.goo\.gl)\/[\w-]+/,
        /https?:\/\/(?:www\.)?google\.com\/maps\/embed\/v1\/\w+\?[^\s]+/,
      ];

      let detectedUrl = null;
      for (const pattern of mapsPatterns) {
        const match = locationValue.match(pattern);
        if (match) { detectedUrl = match[0]; break; }
      }

      if (detectedUrl && isGoogleMapsURL(detectedUrl)) {
        locationUrl = detectedUrl;
        const queryMatch = detectedUrl.match(/[?&]q=([^&]+)/);
        const placeMatch = detectedUrl.match(/\/place\/([^/@]+)/);
        if (queryMatch) locationName = decodeURIComponent(queryMatch[1].replace(/\+/g, ' '));
        else if (placeMatch) locationName = decodeURIComponent(placeMatch[1].replace(/\+/g, ' '));
      } else if (detectedUrl) {
        // Regex matched but isGoogleMapsURL() rejected — treat as text search
        locationName = locationValue;
        locationUrl = `https://www.google.com/maps/search/${encodeURIComponent(locationValue)}`;
      } else {
        locationName = locationValue;
        locationUrl = `https://www.google.com/maps/search/${encodeURIComponent(locationValue)}`;
      }
    }
    resourceLabel = locationName || 'Location';

  } else {
    // --- File: use attachment from slash command ---
    resourceType = 'file';
    attachment = commandAttachment;

    if (!attachment) {
      clearCooldown(interaction.user.id);
      await resInteraction.update({
        content: 'No file attached. Please rerun `/qurl send` with a file in the `file` option.',
        components: [],
      });
      return;
    }

    await resInteraction.deferUpdate();

    if (!isAllowedFileType(attachment.contentType)) {
      clearCooldown(interaction.user.id);
      return interaction.editReply({
        content: `File type \`${attachment.contentType}\` is not allowed. Supported: images, PDFs, videos, audio, Office docs, text, CSV, ZIP.`,
        components: [],
      });
    }
    const MAX_FILE_SIZE = 25 * 1024 * 1024;
    if (attachment.size > MAX_FILE_SIZE) {
      clearCooldown(interaction.user.id);
      return interaction.editReply({
        content: `File too large (${Math.round(attachment.size / 1024 / 1024)}MB). Maximum is 25MB.`,
        components: [],
      });
    }
    resourceLabel = `File (${sanitizeFilename(attachment.name)})`;
  }

  // --- Step 3: Process and send ---
  await interaction.editReply({ content: `Preparing links for ${recipients.length} recipient(s)...`, components: [] });

  const sendId = crypto.randomUUID();
  let qurlLinks = [];
  let connectorResourceId = null;

  try {
    if (resourceType === 'file') {
      const filename = sanitizeFilename(attachment.name);
      const uploadResult = await uploadToConnector(attachment.url, filename, attachment.contentType);
      connectorResourceId = uploadResult.resource_id;

      const expiresAt = expiryToISO(expiresIn);
      const allLinks = [];
      for (let i = 0; i < recipients.length; i += 10) {
        const batchSize = Math.min(10, recipients.length - i);
        const minted = await mintLinks(connectorResourceId, expiresAt, batchSize);
        allLinks.push(...minted);
      }

      if (allLinks.length < recipients.length) {
        logger.error('mintLinks returned fewer links than expected', { expected: recipients.length, got: allLinks.length });
        clearCooldown(interaction.user.id);
        return interaction.editReply({ content: `Only ${allLinks.length} of ${recipients.length} links could be created. Please try again.` });
      }

      qurlLinks = recipients.map((r, i) => ({
        recipientId: r.id,
        qurlLink: allLinks[i].qurl_link,
        resourceId: connectorResourceId,
      }));
    } else {
      const description = locationName || locationUrl;
      const results = await batchSettled(recipients, async (recipient) => {
        const result = await createOneTimeLink(locationUrl, expiresIn, description);
        return { recipientId: recipient.id, qurlLink: result.qurl_link, resourceId: result.resource_id };
      }, 5);

      for (const r of results) {
        if (r.status === 'fulfilled') qurlLinks.push(r.value);
        else logger.error('Failed to create QURL link', { error: r.reason?.message });
      }
    }
  } catch (error) {
    logger.error('Failed to prepare QURL links', { error: error.message });
    clearCooldown(interaction.user.id); // allow retry on failure
    return interaction.editReply({ content: 'Failed to create links. Please try again.' });
  }

  if (qurlLinks.length === 0) {
    clearCooldown(interaction.user.id);
    return interaction.editReply({ content: 'Failed to create any links. Please try again.' });
  }

  // Persist ALL links to DB BEFORE sending DMs
  for (const link of qurlLinks) {
    db.recordQURLSend(
      sendId, interaction.user.id, link.recipientId, link.resourceId,
      resourceType, link.qurlLink, expiresIn, interaction.channelId, target,
    );
  }

  // Send DMs
  let delivered = 0;
  let failed = 0;
  const failedUsers = [];

  const dmResults = await batchSettled(qurlLinks, async (link) => {
    const recipient = recipients.find(r => r.id === link.recipientId);
    const embed = buildDeliveryEmbed({
      senderUsername: interaction.user.username,
      resourceType,
      resourceLabel,
      qurlLink: link.qurlLink,
      expiresIn,
      filename: attachment ? sanitizeFilename(attachment.name) : null,
      personalMessage,
    });

    const sent = await sendDM(link.recipientId, { embeds: [embed] });
    db.updateSendDMStatus(sendId, link.recipientId, sent ? 'sent' : 'failed');
    return { recipientId: link.recipientId, username: recipient?.username, sent };
  }, 5);

  for (const r of dmResults) {
    if (r.status === 'fulfilled' && r.value.sent) {
      delivered++;
    } else {
      failed++;
      const username = r.status === 'fulfilled' ? r.value.username : 'unknown';
      failedUsers.push(username);
    }
  }

  // Save send config for "Add Recipients" reuse (uses first file resource if present)
  db.saveSendConfig(
    sendId, interaction.user.id, resourceType, connectorResourceId,
    locationUrl || null, expiresIn, personalMessage, locationName, attachment?.name || null,
  );

  // Ephemeral confirmation with Add Recipients + Revoke buttons
  const TRUNC_LIMIT = 5;
  const successNames = recipients.filter(r => !failedUsers.includes(r.username)).map(r => r.username);

  function buildConfirmMsg(showAll) {
    let msg = `Sent to ${delivered} user${delivered !== 1 ? 's' : ''} | Expires: ${expiresIn} | One-time links`;
    if (failed > 0) {
      msg += `\n${failed} could not be reached: ${failedUsers.join(', ')}`;
    }
    if (successNames.length > 0) {
      if (showAll || successNames.length <= TRUNC_LIMIT) {
        msg += `\nRecipients: ${successNames.join(', ')}`;
      } else {
        msg += `\nRecipients: ${successNames.slice(0, TRUNC_LIMIT).join(', ')} +${successNames.length - TRUNC_LIMIT} more`;
      }
    }
    return msg;
  }

  let confirmMsg = buildConfirmMsg(false);
  const needsExpand = successNames.length > TRUNC_LIMIT;

  const buttonRow = new ActionRowBuilder().addComponents(
    new ButtonBuilder()
      .setCustomId(`qurl_add_${sendId}`)
      .setLabel('Add Recipients')
      .setStyle(ButtonStyle.Primary),
    new ButtonBuilder()
      .setCustomId(`qurl_revoke_${sendId}`)
      .setLabel('Revoke All Links')
      .setStyle(ButtonStyle.Danger),
  );
  if (needsExpand) {
    buttonRow.addComponents(
      new ButtonBuilder()
        .setCustomId(`qurl_expand_${sendId}`)
        .setLabel('Show All')
        .setStyle(ButtonStyle.Secondary),
    );
  }

  const response = await interaction.editReply({
    content: confirmMsg,
    components: delivered > 0 ? [buttonRow] : [],
  });

  logger.info('/qurl send completed', {
    sender: interaction.user.id, sendId, target, resourceType, delivered, failed, expiresIn,
  });

  // Start link status monitor BEFORE collector so `monitor` is available in callbacks
  const monitor = delivered > 0
    ? monitorLinkStatus(sendId, interaction, qurlLinks, recipients, expiresIn, confirmMsg, buttonRow, delivered)
    : null;

  // Collector handles multiple button clicks (Add Recipients can be clicked multiple times)
  if (delivered > 0) {
    let addRecipientsCount = 0; // Track cumulative adds for cap enforcement

    const collector = response.createMessageComponentCollector({
      filter: (i) => i.user.id === interaction.user.id,
      time: TIMEOUTS.QURL_REVOKE_WINDOW,
    });

    let showAllRecipients = false;

    collector.on('collect', async (btnInteraction) => {
      if (btnInteraction.customId === `qurl_expand_${sendId}`) {
        await btnInteraction.deferUpdate().catch(() => {});
        showAllRecipients = !showAllRecipients;
        confirmMsg = buildConfirmMsg(showAllRecipients);
        monitor.updateBaseMsg(confirmMsg);
        const fullMsg = monitor.getFullMsg();
        const updatedRow = new ActionRowBuilder().addComponents(
          new ButtonBuilder().setCustomId(`qurl_add_${sendId}`).setLabel('Add Recipients').setStyle(ButtonStyle.Primary),
          new ButtonBuilder().setCustomId(`qurl_revoke_${sendId}`).setLabel('Revoke All Links').setStyle(ButtonStyle.Danger),
          new ButtonBuilder().setCustomId(`qurl_expand_${sendId}`).setLabel(showAllRecipients ? 'Show Less' : 'Show All').setStyle(ButtonStyle.Secondary),
        );
        await interaction.editReply({ content: fullMsg, components: [updatedRow] }).catch(() => {});
        return;
      }

      if (btnInteraction.customId === `qurl_revoke_${sendId}`) {
        await btnInteraction.deferUpdate().catch(() => {});
        await interaction.editReply({ content: 'Revoking links...', components: [] }).catch(() => {});
        try {
          const revoked = await revokeAllLinks(sendId, interaction.user.id);
          await interaction.editReply({
            content: `Revoked ${revoked.success}/${revoked.total} links.`,
            components: [],
          }).catch(() => {});
        } catch (err) {
          logger.error('Revoke failed', { sendId, error: err.message });
          await interaction.editReply({
            content: 'Failed to revoke links. Try `/qurl revoke` instead.',
            components: [],
          }).catch(() => {});
        }
        if (monitor) monitor.stop();
        collector.stop('revoked');

      } else if (btnInteraction.customId === `qurl_add_${sendId}`) {
        // Enforce cumulative recipient cap
        const remaining = config.QURL_SEND_MAX_RECIPIENTS - delivered - addRecipientsCount;
        if (remaining <= 0) {
          await btnInteraction.reply({
            content: `Recipient limit reached (${config.QURL_SEND_MAX_RECIPIENTS} max).`,
            ephemeral: true,
          });
          return;
        }

        // Enforce cooldown on add-recipients too
        if (isOnCooldown(interaction.user.id)) {
          await btnInteraction.reply({ content: 'Please wait before adding more recipients.', ephemeral: true });
          return;
        }

        // Show user select menu — collect the response on the REPLY message
        const maxSelect = Math.min(10, remaining);
        const userSelectRow = new ActionRowBuilder().addComponents(
          new UserSelectMenuBuilder()
            .setCustomId(`qurl_addusers_${sendId}`)
            .setPlaceholder('Select users to send to')
            .setMinValues(1)
            .setMaxValues(maxSelect)
        );
        const selectReply = await btnInteraction.reply({
          content: 'Select additional recipients:',
          components: [userSelectRow],
          ephemeral: true,
          fetchReply: true,
        });

        // Await the user-select interaction on the REPLY message (not the parent)
        try {
          const selectInteraction = await selectReply.awaitMessageComponent({
            componentType: ComponentType.UserSelect,
            time: 60000,
          });

          await selectInteraction.deferUpdate();
          const addResult = await handleAddRecipients(
            sendId, interaction.user.id, selectInteraction.users, interaction,
          );

          // Count how many were added — update monitor if new recipients were delivered
          const addedCount = addResult.delivered || 0;
          if (addedCount > 0) {
            addRecipientsCount += addedCount;
            monitor.addRecipients(addedCount, addResult.newResourceIds);
            const totalSent = delivered + addRecipientsCount;
            confirmMsg = `Sent to ${totalSent} user${totalSent !== 1 ? 's' : ''} | Expires: ${expiresIn} | One-time links`;
            if (failed > 0) confirmMsg += `\n${failed} could not be reached`;
            monitor.updateBaseMsg(confirmMsg);
            await interaction.editReply({ content: monitor.getFullMsg(), components: [buttonRow] });
          }

          setCooldown(interaction.user.id);
          await btnInteraction.editReply({ content: addResult.msg, components: [] });
        } catch (err) {
          const isTimeout = err?.code === 'InteractionCollectorError' || err?.message?.includes('time');
          const msg = isTimeout ? 'Selection timed out.' : `Failed to add recipients: ${err.message || 'unknown error'}`;
          logger.error('Add recipients failed', { sendId, error: err.message, isTimeout });
          await btnInteraction.editReply({ content: msg, components: [] }).catch(() => {});
        }
      }
    });

    collector.on('end', (_, reason) => {
      // Stop monitor polling when collector ends for any reason
      if (monitor) monitor.stop();
      if (reason === 'time') {
        interaction.editReply({
          content: (monitor ? monitor.getFullMsg() : confirmMsg) + '\n\n\u23f0 **Management window closed** — use `/qurl revoke` to revoke later.',
          components: [],
        }).catch(() => {});
      }
    });
  }

}

// Handle adding new recipients to an existing send
async function handleAddRecipients(sendId, senderDiscordId, usersCollection, originalInteraction) {
  const sendConfig = db.getSendConfig(sendId, senderDiscordId);
  if (!sendConfig) {
    return { msg: 'Send configuration not found.', newResourceIds: [] };
  }

  // Filter out bots and the sender
  const newRecipients = usersCollection
    .filter(u => !u.bot && u.id !== senderDiscordId)
    .map(u => u);

  if (newRecipients.length === 0) {
    return { msg: 'No valid recipients selected (bots and yourself are excluded).', newResourceIds: [] };
  }

  // Create new QURL links for each resource type in the send config
  // recipientLinks[recipientId] = [{ qurlLink, resourceId, resType, label }]
  const recipientLinks = {};
  const hasFile = sendConfig.connector_resource_id;
  const hasLocation = sendConfig.actual_url;

  if (!hasFile && !hasLocation) {
    return { msg: 'Cannot add recipients — send configuration is incomplete.', newResourceIds: [] };
  }

  try {
    if (hasFile) {
      const expiresAt = expiryToISO(sendConfig.expires_in);
      const allLinks = [];
      for (let i = 0; i < newRecipients.length; i += 10) {
        const batchSize = Math.min(10, newRecipients.length - i);
        const minted = await mintLinks(sendConfig.connector_resource_id, expiresAt, batchSize);
        allLinks.push(...minted);
      }
      if (allLinks.length < newRecipients.length) {
        logger.error('mintLinks returned fewer links than expected in addRecipients', { expected: newRecipients.length, got: allLinks.length });
        return { msg: `Only ${allLinks.length} of ${newRecipients.length} links created. Try again.`, newResourceIds: [] };
      }
      newRecipients.forEach((r, i) => {
        if (!recipientLinks[r.id]) recipientLinks[r.id] = [];
        recipientLinks[r.id].push({
          qurlLink: allLinks[i].qurl_link,
          resourceId: sendConfig.connector_resource_id,
          resType: 'file',
          label: `File (${sanitizeFilename(sendConfig.attachment_name || 'file')})`,
        });
      });
    }
    if (hasLocation) {
      const description = sendConfig.location_name || 'Google Maps Location';
      const results = await batchSettled(newRecipients, async (recipient) => {
        const result = await createOneTimeLink(sendConfig.actual_url, sendConfig.expires_in, description);
        return { recipientId: recipient.id, qurlLink: result.qurl_link, resourceId: result.resource_id };
      }, 5);
      for (const r of results) {
        if (r.status === 'fulfilled') {
          const v = r.value;
          if (!recipientLinks[v.recipientId]) recipientLinks[v.recipientId] = [];
          recipientLinks[v.recipientId].push({
            qurlLink: v.qurlLink, resourceId: v.resourceId,
            resType: 'maps', label: sendConfig.location_name || 'Google Maps',
          });
        }
      }
    }
  } catch (error) {
    logger.error('Failed to create links for additional recipients', { error: error.message });
    return { msg: 'Failed to create links for new recipients.', newResourceIds: [] };
  }

  const recipientIds = Object.keys(recipientLinks);
  if (recipientIds.length === 0) {
    return { msg: 'Failed to create any links.', newResourceIds: [] };
  }

  // Persist to DB before DMs
  for (const [rid, links] of Object.entries(recipientLinks)) {
    for (const link of links) {
      db.recordQURLSend(
        sendId, senderDiscordId, rid, link.resourceId,
        link.resType, link.qurlLink, sendConfig.expires_in,
        originalInteraction.channelId, 'user',
      );
    }
  }

  // Send DMs — one message per recipient with all their links
  let delivered = 0;
  let failed = 0;

  const dmResults = await batchSettled(newRecipients, async (recipient) => {
    const links = recipientLinks[recipient.id];
    if (!links || links.length === 0) return { sent: false, username: recipient.username };

    const link = links[0];
    const embed = buildDeliveryEmbed({
      senderUsername: originalInteraction.user.username,
      resourceType: link.resType,
      resourceLabel: link.label,
      qurlLink: link.qurlLink,
      expiresIn: sendConfig.expires_in,
      filename: sendConfig.attachment_name ? sanitizeFilename(sendConfig.attachment_name) : null,
      personalMessage: sendConfig.personal_message,
    });

    const sent = await sendDM(recipient.id, { embeds: [embed] });
    for (const l of links) {
      db.updateSendDMStatus(sendId, recipient.id, sent ? 'sent' : 'failed');
    }
    return { sent, username: recipient.username };
  }, 5);

  for (const r of dmResults) {
    if (r.status === 'fulfilled' && r.value.sent) delivered++;
    else failed++;
  }

  const allLinks = Object.values(recipientLinks).flat();
  const newResourceIds = [...new Set(allLinks.map(l => l.resourceId))];
  let msg = `Added ${delivered} recipient${delivered !== 1 ? 's' : ''}`;
  if (failed > 0) msg += ` (${failed} could not be reached)`;
  logger.info('/qurl add recipients', { sendId, delivered, failed });
  return { msg, newResourceIds, delivered };
}

// --- /qurl revoke handler ---

async function handleRevoke(interaction) {
  await interaction.deferReply({ ephemeral: true });

  const recentSends = db.getRecentSends(interaction.user.id, 5);

  if (recentSends.length === 0) {
    return interaction.editReply({ content: 'No recent sends to revoke.' });
  }

  const select = new StringSelectMenuBuilder()
    .setCustomId('qurl_revoke_select')
    .setPlaceholder('Select a send to revoke')
    .addOptions(recentSends.map(s => ({
      label: `${s.resource_type} to ${s.recipient_count} users (${s.target_type})`,
      description: `${new Date(s.created_at).toLocaleString()} | ${s.delivered_count} delivered | Expires: ${s.expires_in}`,
      value: s.send_id,
    })));

  const row = new ActionRowBuilder().addComponents(select);
  const response = await interaction.editReply({
    content: 'Select a send to revoke all its links:',
    components: [row],
  });

  try {
    const selectInteraction = await response.awaitMessageComponent({
      componentType: ComponentType.StringSelect,
      time: 60000,
    });

    const sendId = selectInteraction.values[0];
    const revoked = await revokeAllLinks(sendId, interaction.user.id);

    await selectInteraction.update({
      content: `Revoked ${revoked.success}/${revoked.total} links.`,
      components: [],
    });
  } catch {
    await interaction.editReply({ content: 'Revocation timed out.', components: [] }).catch(() => {});
  }
}

async function revokeAllLinks(sendId, senderDiscordId) {
  const resourceIds = db.getSendResourceIds(sendId, senderDiscordId);
  let success = 0;

  const results = await batchSettled(resourceIds, async (resourceId) => {
    await deleteLink(resourceId);
    return resourceId;
  }, 5);

  for (const r of results) {
    if (r.status === 'fulfilled') success++;
    else logger.error('Failed to revoke QURL', { error: r.reason?.message });
  }

  logger.info('Revoked send', { sendId, success, total: resourceIds.length });
  return { success, total: resourceIds.length };
}

// --- Location autocomplete handler ---

// Per-user autocomplete throttle: max 5 requests per 10 seconds
const autocompleteLimits = new Map();
const AUTOCOMPLETE_WINDOW_MS = 10000;
const AUTOCOMPLETE_MAX_PER_WINDOW = 5;

function isAutocompleteLimited(userId) {
  const now = Date.now();
  const entry = autocompleteLimits.get(userId);
  if (!entry || now - entry.windowStart > AUTOCOMPLETE_WINDOW_MS) {
    autocompleteLimits.set(userId, { windowStart: now, count: 1 });
    return false;
  }
  entry.count++;
  return entry.count > AUTOCOMPLETE_MAX_PER_WINDOW;
}

// Evict stale cooldown/autocomplete entries every 5 minutes to prevent slow memory leak
setInterval(() => {
  const now = Date.now();
  for (const [k, v] of sendCooldowns) {
    if (now - v > config.QURL_SEND_COOLDOWN_MS * 2) sendCooldowns.delete(k);
  }
  for (const [k, v] of autocompleteLimits) {
    if (now - v.windowStart > AUTOCOMPLETE_WINDOW_MS * 2) autocompleteLimits.delete(k);
  }
}, 5 * 60 * 1000).unref();

async function handleLocationAutocomplete(interaction, query) {
  if (!query || query.length < 2) {
    return interaction.respond([]);
  }

  if (isAutocompleteLimited(interaction.user.id)) {
    return interaction.respond([]);
  }

  if (isGoogleMapsURL(query)) {
    return interaction.respond([
      { name: `Maps link: ${query.substring(0, 90)}`, value: query },
    ]);
  }

  try {
    const results = await searchPlaces(query);
    const choices = results.map(place => ({
      name: `${place.name} — ${place.address}`.substring(0, 100),
      value: `place_id:${place.placeId}|${place.name}`,
    }));
    await interaction.respond(choices.slice(0, 5));
  } catch (error) {
    logger.error('Places autocomplete failed', { error: error.message });
    await interaction.respond([]);
  }
}

// Command definitions
const commands = [
  {
    data: new SlashCommandBuilder()
      .setName('link')
      .setDescription('Link your GitHub account to receive Contributor role when PRs are merged'),
    async execute(interaction) {
      const discordId = interaction.user.id;

      // Check if already linked
      const existing = db.getLinkByDiscord(discordId);

      // Generate state and create pending link
      const state = generateState();
      db.createPendingLink(state, discordId);

      const authUrl = `${config.BASE_URL}/auth/github?state=${state}`;

      const embed = new EmbedBuilder()
        .setColor(COLORS.PRIMARY)
        .setTitle('🔗 Link Your GitHub Account')
        .setDescription(
          existing
            ? `You're currently linked to **@${existing.github_username}**.\n\nClick the button below to link a different account or re-verify.`
            : 'Click the button below to verify your GitHub identity.\n\n' +
              'Once linked, you\'ll automatically receive the **@Contributor** role when your PRs to OpenNHP repos are merged!'
        )
        .addFields({
          name: '🔒 Privacy',
          value: 'We only request permission to read your public profile (username). We cannot access your repositories or private information.',
        })
        .setFooter({ text: `Link expires in ${config.PENDING_LINK_EXPIRY_MINUTES} minutes` });

      const row = new ActionRowBuilder().addComponents(
        new ButtonBuilder()
          .setLabel(existing ? '🔄 Re-link GitHub' : '🔗 Link GitHub Account')
          .setStyle(ButtonStyle.Link)
          .setURL(authUrl)
      );

      await interaction.reply({
        embeds: [embed],
        components: [row],
        ephemeral: true,
      });

      logger.info('User initiated /link', { discordId, relink: !!existing });
    },
  },
  {
    data: new SlashCommandBuilder()
      .setName('unlink')
      .setDescription('Unlink your GitHub account'),
    async execute(interaction) {
      const discordId = interaction.user.id;

      const existing = db.getLinkByDiscord(discordId);
      if (!existing) {
        return interaction.reply({
          content: 'You don\'t have a GitHub account linked.',
          ephemeral: true,
        });
      }

      // Confirmation prompt
      const embed = new EmbedBuilder()
        .setColor(COLORS.ERROR)
        .setTitle('⚠️ Confirm Unlink')
        .setDescription(
          `Are you sure you want to unlink your GitHub account **@${existing.github_username}**?\n\n` +
          'You will no longer automatically receive the @Contributor role for future PRs.'
        );

      const row = new ActionRowBuilder().addComponents(
        new ButtonBuilder()
          .setCustomId('unlink_confirm')
          .setLabel('Yes, Unlink')
          .setStyle(ButtonStyle.Danger),
        new ButtonBuilder()
          .setCustomId('unlink_cancel')
          .setLabel('Cancel')
          .setStyle(ButtonStyle.Secondary)
      );

      const response = await interaction.reply({
        embeds: [embed],
        components: [row],
        ephemeral: true,
      });

      try {
        const buttonInteraction = await response.awaitMessageComponent({
          componentType: ComponentType.Button,
          time: TIMEOUTS.BUTTON_INTERACTION,
        });

        if (buttonInteraction.customId === 'unlink_confirm') {
          db.deleteLink(discordId);
          await buttonInteraction.update({
            content: `✓ Unlinked from GitHub **@${existing.github_username}**.\n\nYou can link a new account anytime with \`/link\`.`,
            embeds: [],
            components: [],
          });
          logger.info('User unlinked', { discordId, github: existing.github_username });
        } else {
          await buttonInteraction.update({
            content: 'Unlink cancelled. Your GitHub account is still linked.',
            embeds: [],
            components: [],
          });
        }
      } catch {
        await interaction.editReply({
          content: 'Confirmation timed out. Your GitHub account is still linked.',
          embeds: [],
          components: [],
        });
      }
    },
  },
  {
    data: new SlashCommandBuilder()
      .setName('whois')
      .setDescription('Check GitHub link for a user')
      .addUserOption(option =>
        option
          .setName('user')
          .setDescription('The Discord user to check (leave empty for yourself)')
          .setRequired(false)
      ),
    async execute(interaction) {
      const targetUser = interaction.options.getUser('user') || interaction.user;
      const link = db.getLinkByDiscord(targetUser.id);

      if (link) {
        const contributions = db.getContributions(targetUser.id);
        const badges = db.getBadges(targetUser.id);
        const streak = db.getStreak(targetUser.id);

        const embed = new EmbedBuilder()
          .setColor(COLORS.SUCCESS)
          .setTitle(`GitHub Link for ${targetUser.username}`)
          .addFields(
            { name: 'GitHub', value: `[@${link.github_username}](https://github.com/${link.github_username})`, inline: true },
            { name: 'Linked Since', value: new Date(link.linked_at).toLocaleDateString(), inline: true },
            { name: 'PRs Merged', value: `${contributions.length}`, inline: true }
          );

        // Add badges
        if (badges.length > 0) {
          const badgeDisplay = badges
            .map(b => {
              const info = db.BADGE_INFO[b.badge_type];
              return info ? `${info.emoji} ${info.name}` : b.badge_type;
            })
            .join(' • ');
          embed.addFields({ name: '🏅 Badges', value: badgeDisplay });
        }

        // Add streak (monthly tracking)
        if (streak && streak.current_streak > 0) {
          embed.addFields({
            name: '🔥 Streak',
            value: `${streak.current_streak} month${streak.current_streak > 1 ? 's' : ''} (Best: ${streak.longest_streak})`,
            inline: true,
          });
        }

        // Add recent contributions
        if (contributions.length > 0) {
          const recent = contributions.slice(0, 3)
            .map(c => `• ${c.repo} #${c.pr_number}`)
            .join('\n');
          embed.addFields({ name: 'Recent Contributions', value: recent });
        }

        const row = new ActionRowBuilder().addComponents(
          new ButtonBuilder()
            .setLabel('View on GitHub')
            .setStyle(ButtonStyle.Link)
            .setURL(`https://github.com/${link.github_username}`)
        );

        await interaction.reply({ embeds: [embed], components: [row], ephemeral: true });
      } else {
        await interaction.reply({
          content: targetUser.id === interaction.user.id
            ? 'You haven\'t linked your GitHub account yet. Use `/link` to get started!'
            : `${targetUser.username} hasn't linked their GitHub account.`,
          ephemeral: true,
        });
      }
    },
  },
  {
    data: new SlashCommandBuilder()
      .setName('contributions')
      .setDescription('View your contribution history')
      .addUserOption(option =>
        option
          .setName('user')
          .setDescription('The user to check (leave empty for yourself)')
          .setRequired(false)
      ),
    async execute(interaction) {
      const targetUser = interaction.options.getUser('user') || interaction.user;
      const contributions = db.getContributions(targetUser.id);

      if (contributions.length === 0) {
        return interaction.reply({
          content: targetUser.id === interaction.user.id
            ? 'You don\'t have any recorded contributions yet. Link your GitHub with `/link` and merge a PR!'
            : `${targetUser.username} doesn't have any recorded contributions.`,
          ephemeral: true,
        });
      }

      // Group by repo
      const byRepo = {};
      for (const c of contributions) {
        if (!byRepo[c.repo]) byRepo[c.repo] = [];
        byRepo[c.repo].push(c);
      }

      const embed = new EmbedBuilder()
        .setColor(COLORS.PURPLE)
        .setTitle(`📊 Contributions by ${targetUser.username}`)
        .setDescription(`**${contributions.length}** PRs merged across **${Object.keys(byRepo).length}** repos`)
        .setTimestamp();

      for (const [repo, prs] of Object.entries(byRepo)) {
        const prList = prs.slice(0, 5)
          .map(p => `• #${p.pr_number}${p.pr_title ? `: ${p.pr_title.substring(0, 40)}${p.pr_title.length > 40 ? '...' : ''}` : ''}`)
          .join('\n');
        embed.addFields({
          name: `${repo} (${prs.length})`,
          value: prList + (prs.length > 5 ? `\n... and ${prs.length - 5} more` : ''),
        });
      }

      await interaction.reply({ embeds: [embed], ephemeral: true });
    },
  },
  {
    data: new SlashCommandBuilder()
      .setName('stats')
      .setDescription('Show bot statistics'),
    async execute(interaction) {
      const stats = db.getStats();

      const embed = new EmbedBuilder()
        .setColor(COLORS.PURPLE)
        .setTitle('📊 OpenNHP Bot Stats')
        .addFields(
          { name: 'Linked Users', value: `${stats.linkedUsers}`, inline: true },
          { name: 'Total PRs', value: `${stats.totalContributions}`, inline: true },
          { name: 'Contributors', value: `${stats.uniqueContributors}`, inline: true }
        );

      if (stats.byRepo.length > 0) {
        const repoList = stats.byRepo
          .slice(0, 5)
          .map(r => `• ${r.repo}: ${r.count} PRs`)
          .join('\n');
        embed.addFields({ name: 'Top Repositories', value: repoList });
      }

      // Add leaderboard
      const topContributors = db.getTopContributors(5);
      if (topContributors.length > 0) {
        const leaderboard = topContributors
          .map((c, i) => {
            const medal = i === 0 ? '🥇' : i === 1 ? '🥈' : i === 2 ? '🥉' : `${i + 1}.`;
            return `${medal} <@${c.discord_id}>: ${c.count} PRs`;
          })
          .join('\n');
        embed.addFields({ name: '🏆 Top Contributors', value: leaderboard });
      }

      embed.setTimestamp();

      await interaction.reply({ embeds: [embed], ephemeral: true });
    },
  },
  {
    data: new SlashCommandBuilder()
      .setName('leaderboard')
      .setDescription('Show contribution leaderboard'),
    async execute(interaction) {
      const topContributors = db.getTopContributors(10);

      if (topContributors.length === 0) {
        return interaction.reply({
          content: 'No contributions recorded yet!',
          ephemeral: true,
        });
      }

      const embed = new EmbedBuilder()
        .setColor(COLORS.GOLD)
        .setTitle('🏆 Contribution Leaderboard')
        .setDescription(
          topContributors
            .map((c, i) => {
              const medal = i === 0 ? '🥇' : i === 1 ? '🥈' : i === 2 ? '🥉' : `**${i + 1}.**`;
              return `${medal} <@${c.discord_id}> — **${c.count}** PRs`;
            })
            .join('\n')
        )
        .setTimestamp();

      await interaction.reply({ embeds: [embed] });
    },
  },
  {
    data: new SlashCommandBuilder()
      .setName('forcelink')
      .setDescription('(Admin) Force link a Discord user to a GitHub account')
      .addUserOption(option =>
        option
          .setName('user')
          .setDescription('Discord user to link')
          .setRequired(true)
      )
      .addStringOption(option =>
        option
          .setName('github')
          .setDescription('GitHub username (without @)')
          .setRequired(true)
      )
      .setDefaultMemberPermissions(PermissionFlagsBits.Administrator),
    async execute(interaction) {
      if (!await requireAdmin(interaction)) return;

      const targetUser = interaction.options.getUser('user');
      const githubUsername = interaction.options.getString('github').replace(/^@+/, '');

      const existingLink = db.getLinkByGithub(githubUsername);
      if (existingLink && existingLink.discord_id !== targetUser.id) {
        return interaction.reply({
          content: `⚠️ GitHub **@${githubUsername}** is already linked to <@${existingLink.discord_id}>. Unlink them first.`,
          ephemeral: true,
        });
      }

      db.forceLink(targetUser.id, githubUsername);

      await interaction.reply({
        content: `✓ Linked <@${targetUser.id}> to GitHub **@${githubUsername}**`,
        ephemeral: true,
      });

      logger.info('Admin force-linked user', {
        admin: interaction.user.id,
        target: targetUser.id,
        github: githubUsername,
      });
    },
  },
  {
    data: new SlashCommandBuilder()
      .setName('bulklink')
      .setDescription('(Admin) Bulk link users from a list')
      .addStringOption(option =>
        option
          .setName('mappings')
          .setDescription('Comma-separated discord_id:github pairs (e.g., 123:user1,456:user2)')
          .setRequired(true)
      )
      .setDefaultMemberPermissions(PermissionFlagsBits.Administrator),
    async execute(interaction) {
      if (!await requireAdmin(interaction)) return;

      const mappings = interaction.options.getString('mappings');
      const pairs = mappings.split(',').map(s => s.trim());

      let success = 0;
      let failed = 0;
      const errors = [];

      for (const pair of pairs) {
        const [discordId, github] = pair.split(':').map(s => s.trim());
        if (!discordId || !github) {
          failed++;
          errors.push(`Invalid format: "${pair}"`);
          continue;
        }
        if (!/^\d{17,20}$/.test(discordId)) {
          failed++;
          errors.push(`Invalid Discord ID: "${discordId}"`);
          continue;
        }

        try {
          const existing = db.getLinkByGithub(github);
          if (existing && existing.discord_id !== discordId) {
            failed++;
            errors.push(`@${github} already linked to another user`);
            continue;
          }

          db.forceLink(discordId, github);
          success++;
        } catch (error) {
          failed++;
          errors.push(`Error linking ${discordId}: ${error.message}`);
        }
      }

      const embed = new EmbedBuilder()
        .setColor(failed === 0 ? COLORS.SUCCESS : COLORS.WARNING)
        .setTitle('📦 Bulk Link Results')
        .addFields(
          { name: '✓ Success', value: `${success}`, inline: true },
          { name: '✗ Failed', value: `${failed}`, inline: true }
        );

      if (errors.length > 0) {
        embed.addFields({
          name: 'Errors',
          value: errors.slice(0, 10).join('\n') + (errors.length > 10 ? `\n... and ${errors.length - 10} more` : ''),
        });
      }

      await interaction.reply({ embeds: [embed], ephemeral: true });

      logger.info('Admin bulk-linked users', {
        admin: interaction.user.id,
        success,
        failed,
      });
    },
  },
  {
    data: new SlashCommandBuilder()
      .setName('backfill-milestones')
      .setDescription('(Admin) Backfill star milestones for a repo that already has stars')
      .addStringOption(option =>
        option
          .setName('repo')
          .setDescription('Full repo name (e.g., OpenNHP/opennhp)')
          .setRequired(true)
      )
      .addIntegerOption(option =>
        option
          .setName('stars')
          .setDescription('Current star count')
          .setRequired(true)
      )
      .setDefaultMemberPermissions(PermissionFlagsBits.Administrator),
    async execute(interaction) {
      if (!await requireAdmin(interaction)) return;

      const repo = interaction.options.getString('repo');
      if (!/^[\w.-]+\/[\w.-]+$/.test(repo)) {
        return interaction.reply({ content: 'Invalid repo format. Use `owner/repo` (e.g., `OpenNHP/opennhp`).', ephemeral: true });
      }
      const stars = interaction.options.getInteger('stars');

      let backfilled = 0;
      let skipped = 0;

      for (const milestone of config.STAR_MILESTONES) {
        if (stars >= milestone) {
          if (!db.hasMilestoneBeenAnnounced('stars', milestone, repo)) {
            if (db.recordMilestone('stars', milestone, repo)) {
              backfilled++;
            }
          } else {
            skipped++;
          }
        }
      }

      const embed = new EmbedBuilder()
        .setColor(COLORS.SUCCESS)
        .setTitle('✓ Milestones Backfilled')
        .setDescription(`Backfilled milestones for **${repo}** (${stars} stars)`)
        .addFields(
          { name: 'Backfilled', value: `${backfilled}`, inline: true },
          { name: 'Already Recorded', value: `${skipped}`, inline: true }
        );

      await interaction.reply({ embeds: [embed], ephemeral: true });

      logger.info('Admin backfilled milestones', {
        admin: interaction.user.id,
        repo,
        stars,
        backfilled,
      });
    },
  },
  {
    data: new SlashCommandBuilder()
      .setName('unlinked')
      .setDescription('(Admin) Show contributors who haven\'t linked their GitHub')
      .setDefaultMemberPermissions(PermissionFlagsBits.Administrator),
    async execute(interaction) {
      if (!await requireAdmin(interaction)) return;

      await interaction.deferReply({ ephemeral: true });

      try {
        const guild = interaction.guild;
        const contributorRole = guild.roles.cache.find(r => r.name === config.CONTRIBUTOR_ROLE_NAME);

        if (!contributorRole) {
          return interaction.editReply({
            content: `❌ Could not find role "${config.CONTRIBUTOR_ROLE_NAME}"`,
          });
        }

        // Get all members with Contributor role
        const members = await guild.members.fetch();
        const contributors = members.filter(m => m.roles.cache.has(contributorRole.id));

        // Check which ones are not linked
        const unlinked = [];
        for (const [id, member] of contributors) {
          const link = db.getLinkByDiscord(id);
          if (!link) {
            unlinked.push(member);
          }
        }

        if (unlinked.length === 0) {
          return interaction.editReply({
            content: '✓ All contributors have linked their GitHub accounts!',
          });
        }

        const embed = new EmbedBuilder()
          .setColor(COLORS.ERROR)
          .setTitle('⚠️ Unlinked Contributors')
          .setDescription(
            `${unlinked.length} contributor(s) have the @${config.CONTRIBUTOR_ROLE_NAME} role but haven't linked their GitHub:\n\n` +
            unlinked.slice(0, 20).map(m => `• <@${m.id}> (${m.user.tag})`).join('\n') +
            (unlinked.length > 20 ? `\n... and ${unlinked.length - 20} more` : '')
          )
          .setFooter({ text: 'Use /forcelink to manually link these users' });

        await interaction.editReply({ embeds: [embed] });
      } catch (error) {
        logger.error('Error in /unlinked', { error: error.message });
        await interaction.editReply({
          content: '❌ An error occurred while checking unlinked contributors.',
        });
      }
    },
  },
  {
    data: new SlashCommandBuilder()
      .setName('qurl')
      .setDescription('Share resources securely via QURL')
      .addSubcommand(sub =>
        sub.setName('send')
          .setDescription('Send a resource to users via one-time secure link')
          .addStringOption(opt =>
            opt.setName('target')
              .setDescription('Who receives this')
              .setRequired(true)
              .addChoices(
                { name: 'Everyone in this channel', value: 'channel' },
                { name: 'Users in my voice channel', value: 'voice' },
                { name: 'A specific user', value: 'user' },
              ))
          .addStringOption(opt =>
            opt.setName('expiry')
              .setDescription('Link expiry (default: 24h)')
              .setRequired(false)
              .addChoices(
                { name: '30 minutes', value: '30m' },
                { name: '1 hour', value: '1h' },
                { name: '6 hours', value: '6h' },
                { name: '24 hours', value: '24h' },
                { name: '7 days', value: '7d' },
              ))
          .addAttachmentOption(opt =>
            opt.setName('file_optional')
              .setDescription('Optional — attach a file here if sharing a file')
              .setRequired(false))
          .addStringOption(opt =>
            opt.setName('message')
              .setDescription('Optional note sent alongside the link')
              .setRequired(false))
      )
      .addSubcommand(sub =>
        sub.setName('revoke')
          .setDescription('Revoke links from a previous send')
      )
      .addSubcommand(sub =>
        sub.setName('help')
          .setDescription('Show QURL bot help')
      ),
    async execute(interaction) {
      const sub = interaction.options.getSubcommand();
      if (sub === 'send') return handleSend(interaction);
      if (sub === 'revoke') return handleRevoke(interaction);
      if (sub === 'help') {
        return interaction.reply({
          content: '**Qurl Bot — Help**\n\n' +
            '**Share resources securely via one-time links:**\n' +
            '  `/qurl send` — send a file and/or location to users\n' +
            '  `/qurl revoke` — revoke links from a previous send\n' +
            '  `/qurl help` — show this message\n\n' +
            '**How it works:**\n' +
            '  1. Use `/qurl send` and choose a target (user, channel, or voice)\n' +
            '  2. Attach a file and/or search for a location\n' +
            '  3. Each recipient gets a unique, single-use link by DM\n' +
            '  4. Links self-destruct on access\n\n' +
            'Use **Add Recipients** to forward to more users after sending.',
          ephemeral: true,
        });
      }
    },
  },
];

// Register commands with Discord
async function registerCommands(client) {
  const commandData = commands.map(cmd => cmd.data.toJSON());

  try {
    logger.info('Registering slash commands...');
    await client.application.commands.set(commandData, config.GUILD_ID);
    logger.info(`Registered ${commands.length} slash commands!`);
  } catch (error) {
    logger.error('Failed to register commands', { error: error.message });
  }
}

// Handle command interactions
async function handleCommand(interaction) {
  // Autocomplete for /qurl send location
  if (interaction.isAutocomplete()) {
    if (interaction.commandName === 'qurl') {
      const focused = interaction.options.getFocused(true);
      if (focused.name === 'location') {
        return handleLocationAutocomplete(interaction, focused.value);
      }
    }
    return;
  }

  if (!interaction.isChatInputCommand()) return;

  const command = commands.find(cmd => cmd.data.name === interaction.commandName);
  if (!command) return;

  try {
    await command.execute(interaction);
  } catch (error) {
    logger.error(`Error executing ${interaction.commandName}`, { error: error.message });
    const reply = {
      content: 'There was an error executing this command.',
      ephemeral: true,
    };

    try {
      if (interaction.replied || interaction.deferred) {
        await interaction.followUp(reply);
      } else {
        await interaction.reply(reply);
      }
    } catch (replyError) {
      logger.error('Failed to send error response', { error: replyError.message });
    }
  }
}

module.exports = {
  commands,
  registerCommands,
  handleCommand,
  // Exported for testing
  _test: {
    isGoogleMapsURL,
    sanitizeFilename,
    sanitizeMessage,
    isAllowedFileType,
    isOnCooldown,
    setCooldown,
    batchSettled,
    expiryToISO,
    sendCooldowns,
    handleAddRecipients,
  },
};
