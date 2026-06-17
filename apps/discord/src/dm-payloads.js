// DM payload builders for messages that EDIT an already-delivered DM
// (PATCH /channels/{id}/messages/{id}). Pure functions — no I/O, no
// process state. Lives in its own leaf module so the qurl-webhook
// receiver route can import it without pulling in commands.js's
// require graph (Discord REST, view-update-registry, flow-state,
// etc.) at module-init time during tests.
//
// Today buildExpiredDMPayload + buildConsumedDMPayload live here.
// buildRevokedDMPayload stays in commands.js next to its sole caller
// (the /qurl revoke editTargets loop) until a second consumer needs it.
//
// CONTRACT: every builder MUST pass `components: []` explicitly.
// Discord's PATCH /messages does NOT clear fields that aren't
// supplied — omitting components would leave the original Step
// Through button live in the recipient's DM, pointing at a now-dead
// qURL resource.

const { EmbedBuilder } = require('discord.js');
const { COLORS } = require('./constants');

// Companion to buildRevokedDMPayload for the qurl.expired webhook
// path: the qURL hit its expiry without a sender-initiated revoke.
// The expiry instant is rendered with Discord's `<t:N:R>` relative-
// time marker so the client-side tense flips automatically as time
// passes (no follow-on edits needed).
//
// UX choices (intentional):
//   - The replacement embed wholly replaces the original delivery
//     embed — sender alias / resource label / Trust footer are
//     dropped. The PATCH semantic only supports whole-array embed
//     replacement, and once the qURL is dead the original
//     "tap to step through" framing is misleading. Mirrors
//     buildRevokedDMPayload's wholesale replacement.
//   - Color stays on QURL_BRAND for symmetry with the revoke path
//     (no muted/grey constant exists today; adding one for one site
//     isn't worth the brand-palette expansion).
function buildExpiredDMPayload({ expiresAtSeconds }) {
  // Defensive coercion — qurl-service emits `expires_at` as a UNIX
  // second-precision integer, but a future shape drift to ms or to
  // a stringified value would render as `<t:NaN:R>` and Discord
  // would strip it silently. Reject non-finite / non-positive
  // values rather than ship a malformed marker; the caller skips
  // the edit when this returns null.
  if (!Number.isFinite(expiresAtSeconds) || expiresAtSeconds <= 0) return null;
  const embed = new EmbedBuilder()
    .setColor(COLORS.QURL_BRAND)
    .setDescription(`⌛ This qURL expired <t:${Math.floor(expiresAtSeconds)}:R>.\nIt is no longer active.`);
  return { embeds: [embed], components: [] };
}

// Companion to buildExpiredDMPayload for the qurl.accessed webhook path
// when `data.consumed === true`: the recipient opened a one-time qURL
// and the content has been served, so for THEM the door has already
// closed — the link is dead even though its 30m expiry hasn't elapsed.
//
// COPY IS DELIBERATELY MARKER-FREE (past/perfect tense, no <t:N:R>):
// at consumption time the link's `expires_at` is still ~minutes in the
// FUTURE. Rendering it through Discord's relative-time marker (as the
// expired payload does) would read "expired in 25 minutes" — a
// future-tense "expired", exactly the confusing state this whole change
// kills. Static copy is the fix: it says "you opened it, it's done"
// without any time anchor that could re-render future-tense. (A
// `<t:now:R>` "opened just now" anchor is possible but adds nothing —
// the recipient just clicked, they know when.)
//
// UX choices (whole-embed replacement, QURL_BRAND color, `components: []`
// to clear the now-dead Step Through button) mirror buildExpiredDMPayload
// — see that function's comment for the why.
function buildConsumedDMPayload() {
  const embed = new EmbedBuilder()
    .setColor(COLORS.QURL_BRAND)
    .setDescription('🔓 You opened this one-time qURL.\nIt has been used and is no longer active.');
  return { embeds: [embed], components: [] };
}

module.exports = { buildExpiredDMPayload, buildConsumedDMPayload };
