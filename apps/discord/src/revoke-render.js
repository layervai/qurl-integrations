// Pure-string revoke rendering. No discord.js / store / config /
// logger deps — keep this file dep-free so the e2e wording-drift
// smoke can require it without the bot's runtime.

const { escapeDiscordMarkdown } = require('./utils/sanitize');

// Shared truncation limit for both send + revoke recipient lists.
const REVOKE_TRUNC_LIMIT = 5;

// Discord message content cap is 2000 chars. Leave headroom for the
// header + "Revoked for: " prefix; force truncation in renderRevokeContent
// even when showAll=true if the full names list would push the total
// over this. Without it, a Show All on ~80+ recipients would exceed
// Discord's limit and the editReply would error.
const REVOKE_CONTENT_SAFE_MAX = 1900;

const REVOKE_FOR_PREFIX = '\nRevoked for: ';

// Single source of truth for the "Revoked X/Y users[.]" header +
// already-opened note. Used by both the inline-button path (via
// renderRevokeContent) and the slash-command /qurl revoke handler so
// a future wording change lands in one place.
function buildRevokeHeader(success, total) {
  const note = total > 0 ? ' Note: already-opened links cannot be revoked.' : '';
  return `Revoked ${success}/${total} user${total !== 1 ? 's' : ''}.${note}`;
}

// Builds the post-revoke confirmation message body. User-centric:
// `success`/`total` are unique-recipient counts; `names` is the
// strict-success list (plain — sanitizeDisplayNamePlain at the call
// site). Returns `attachmentText` (newline-joined full list) when the
// inline rendering would exceed Discord's 2000-char content cap —
// caller wraps in an AttachmentBuilder. In attachment mode
// `needsExpand=false` so the caller suppresses the Show All button
// (the file IS the full list).
//
// `success` defaults to `names.length`. Pass it explicitly when the
// authoritative count (e.g. DDB strict-success) may exceed the names
// the caller could resolve — header reflects truth, names list
// reflects what's renderable.
function renderRevokeContent({ names, total, showAll, success = names.length }) {
  let content = buildRevokeHeader(success, total);

  if (names.length === 0) {
    return { content, needsExpand: false, attachmentText: null };
  }

  // `names` are plain so they can land verbatim in the .txt attachment.
  // Message content needs markdown escape per name to defuse `*phish*`
  // / `[t](url)` injection — render-context split.
  const escapedNames = names.map(escapeDiscordMarkdown);
  const fullLine = REVOKE_FOR_PREFIX + escapedNames.join(', ');

  if (content.length + fullLine.length > REVOKE_CONTENT_SAFE_MAX) {
    // Full list won't fit inline → emit as a file attachment. Inline
    // shows the first REVOKE_TRUNC_LIMIT names + "(see attached)" pointer.
    const preview = escapedNames.slice(0, REVOKE_TRUNC_LIMIT).join(', ');
    content += `${REVOKE_FOR_PREFIX}${preview} +${names.length - REVOKE_TRUNC_LIMIT} more (see attached)`;
    return { content, needsExpand: false, attachmentText: names.join('\n') };
  }

  // Full list fits inline — current Show All / Show Less behavior.
  if (showAll || names.length <= REVOKE_TRUNC_LIMIT) {
    content += fullLine;
  } else {
    content += `${REVOKE_FOR_PREFIX}${escapedNames.slice(0, REVOKE_TRUNC_LIMIT).join(', ')} +${names.length - REVOKE_TRUNC_LIMIT} more`;
  }
  return { content, needsExpand: names.length > REVOKE_TRUNC_LIMIT, attachmentText: null };
}

module.exports = {
  REVOKE_TRUNC_LIMIT,
  REVOKE_CONTENT_SAFE_MAX,
  buildRevokeHeader,
  renderRevokeContent,
};
