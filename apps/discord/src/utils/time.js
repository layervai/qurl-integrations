const logger = require('../logger');

// isPositiveFinite — single predicate for "valid positive numeric
// seconds/count/TTL" gate. Rejects null, undefined, NaN, ±Infinity,
// 0, and negative numbers. Strict Number.isFinite (NOT global
// isFinite) so non-numbers like '1', true, {} are also rejected
// without coercion. Use whenever you need "definitely a positive
// number" — TTL gates, count validators, threshold checks. Hoisted
// to file top so readers see the definition before its (lexically
// lower) callers in this file.
function isPositiveFinite(n) {
  return Number.isFinite(n) && n > 0;
}

const EXPIRY_UNITS = { m: 60, h: 3600, d: 86400 };
// Cap the expiry at 30 days in ms. An arbitrarily large numeric component in
// the expiry string (e.g. "99999999999d") would otherwise overflow Number
// arithmetic and make `new Date(...).toISOString()` throw RangeError.
const MAX_EXPIRY_MS = 30 * 86400 * 1000;
const DEFAULT_EXPIRY_MS = 86400000;

function parseExpiryMs(expiresIn) {
  const match = String(expiresIn ?? '').match(/^(\d{1,6})([mhd])$/);
  if (!match) return null;
  const ms = Number(match[1]) * EXPIRY_UNITS[match[2]] * 1000;
  if (!isPositiveFinite(ms)) return null;
  return Math.min(ms, MAX_EXPIRY_MS);
}

function expiryToISO(expiresIn) {
  const ms = parseExpiryMs(expiresIn);
  if (ms === null) {
    logger.warn('expiryToISO: invalid expiry format, defaulting to 24h', { expiresIn });
    return new Date(Date.now() + DEFAULT_EXPIRY_MS).toISOString();
  }
  return new Date(Date.now() + ms).toISOString();
}

function expiryToMs(expiresIn) {
  const ms = parseExpiryMs(expiresIn);
  return ms === null ? DEFAULT_EXPIRY_MS : ms;
}

// Self-destruct timer presets — the 7 durations exposed in the
// /qurl send + /qurl map confirm-card dropdown. Curated narrow set so
// creators don't ship a 0.7s viewer that hits the 500ms-floor edge
// case in the connector by accident; every preset is a value we'd
// recommend out loud.
//
// The wire field forwarded to the connector is `viewer_ttl_seconds`
// (qurl-s3-connector PR #477). Connector contract is 0.5–3600 inclusive,
// so every preset here is in range; clamping is unreachable.
const SELF_DESTRUCT_PRESETS = Object.freeze([
  // 0.5s residual: fileviewer blanks at 500ms (client-side), but the
  // L7 session_duration floors at 1s per qurl-service's
  // MinSessionDuration. A recipient refreshing between t=500ms and
  // t=1000ms still re-renders. The preset is intentionally retained
  // because the client-side blank is the perceived "self destruct"
  // and the 500ms residual closes on retry past 1s. If qurl-service
  // ever lowers MinSessionDuration to sub-second, this gap closes.
  Object.freeze({ seconds: 0.5, label: '1/2 second' }),
  Object.freeze({ seconds: 1, label: '1 second' }),
  Object.freeze({ seconds: 5, label: '5 seconds' }),
  Object.freeze({ seconds: 30, label: '30 seconds' }),
  Object.freeze({ seconds: 300, label: '5 minutes' }),
  Object.freeze({ seconds: 1800, label: '30 minutes' }),
  Object.freeze({ seconds: 3600, label: '1 hour' }),
]);

// Sentinel value for the "No timer" dropdown option. Distinct from any
// stringified preset seconds so a Number() coercion can't accidentally
// match a preset and set a timer instead of clearing it.
const SELF_DESTRUCT_NO_TIMER_VALUE = 'no-timer';

function findPresetBySeconds(n) {
  // No explicit isFinite guard — NaN is never === anything, so the loop
  // returns null naturally for it. ±Infinity also fails the equality
  // check against every (finite) preset. formatSelfDestructLabel does
  // its own non-finite handling for the "(invalid)" fallback.
  for (const preset of SELF_DESTRUCT_PRESETS) {
    if (n === preset.seconds) return preset;
  }
  return null;
}

// selfDestructSelectValueToSeconds — converts a StringSelectMenu value
// from the /qurl send + /qurl map confirm card into the seconds the
// bot persists and forwards.
// The select's option set is fixed: the no-timer sentinel, or
// `String(preset.seconds)` for each preset. Strict string equality (not
// Number() coercion) so a forged `'0x1'` can't slip past as `1` — same
// hex/scientific-notation foot-gun the previous parser also defended
// against. Anything not in the option set ⇒ null ("no timer"), the
// safe default.
function selfDestructSelectValueToSeconds(value) {
  if (value === SELF_DESTRUCT_NO_TIMER_VALUE) return null;
  for (const preset of SELF_DESTRUCT_PRESETS) {
    if (value === String(preset.seconds)) return preset.seconds;
  }
  return null;
}

// True if `value` matches the closed legitimate option set of the
// form-side self-destruct StringSelectMenu (the no-timer sentinel OR
// a stringified preset seconds value). Use this to gate forgery-
// rejection paths — `selfDestructSelectValueToSeconds` returns null
// for BOTH legitimate "no timer" AND forged values, so callers that
// need to distinguish (reject + warn-log vs. apply) want this
// predicate, not the converter's return value.
function isLegitimateSelfDestructSelectValue(value) {
  if (value === SELF_DESTRUCT_NO_TIMER_VALUE) return true;
  for (const preset of SELF_DESTRUCT_PRESETS) {
    if (value === String(preset.seconds)) return true;
  }
  return false;
}

// formatSelfDestructLabel — renders a stored seconds value as the matching
// preset's friendly label (e.g., 0.5 → "1/2 second"). Used by the form to
// echo what the user picked. Falls back to a compact "Ns" rendering for
// any finite off-preset value (unreachable through the dropdown today but
// defends against a backfilled config). Non-finite (NaN/Infinity) returns
// "(invalid)" so a corrupted DB row doesn't surface as the literal string
// "NaNs" / "Infinitys" in the form preview.
function formatSelfDestructLabel(seconds) {
  const match = findPresetBySeconds(seconds);
  if (match) return match.label;
  // Intentionally NOT isPositiveFinite — this branch's purpose is
  // to flag *non-finite* (NaN/Infinity) as "(invalid)" while still
  // letting 0 / negative off-preset values render as "0s" / "-5s"
  // for the next gate to handle (the form caller maps a finite-but-
  // off-preset value to the underlying numeric formatting). Don't
  // "finish the refactor" by switching to isPositiveFinite — that
  // would silently re-route 0 / negatives to "(invalid)" too.
  if (!Number.isFinite(seconds)) return '(invalid)';
  return `${seconds}s`;
}

// formatSelfDestructSegment — renders the self-destruct status as a
// stand-alone segment for the post-send confirm header
// (`Sent to N | Expires: 24h | Self-destruct: ...`). A null/undefined/
// non-finite/non-positive value renders as "off" — the "no timer
// selected" sentinel — so the segment is always present and aligned
// with the rest of the header.
function formatSelfDestructSegment(seconds) {
  if (isPositiveFinite(seconds)) {
    return `Self-destruct: ${formatSelfDestructLabel(seconds)}`;
  }
  return 'Self-destruct: off';
}

// formatSessionDurationSeconds maps a SELF_DESTRUCT_PRESETS-shaped
// number to the qurl-service-format duration string the connector's
// `mint_link` body and the upload-time `CreateQURL` body both expect.
//
// Co-located with SELF_DESTRUCT_PRESETS because the preset values are
// the ONLY legitimate input source — keeping the formatter here
// prevents the bot's wire-format mapping from drifting if the preset
// set ever changes (e.g., adding a 100ms preset would force a decision
// here about how to floor it).
//
// Returns the duration string (e.g. "1s", "30s") when the input is a
// finite positive number; returns null otherwise. Callers should omit
// the wire field entirely when the result is null (qurl-service then
// uses QURL_SESSION_TTL as the default).
//
// Value mapping:
//   null / undefined / non-finite / non-numeric / ≤0  → null (omit)
//   0.5 (the only fractional preset)                  → "1s" (qurl-service
//                                                       MinSessionDuration
//                                                       floor — fileviewer's
//                                                       500ms client-side
//                                                       blank still fires)
//   N >= 1                                            → "Ns" (Math.ceil
//                                                       defensively floors
//                                                       any non-preset
//                                                       fractional input)
//
// Mirrors qurl-s3-connector's `sessionDurationFor()` (Go) so the
// upload-time and mint-time wire mappings stay in lockstep. If one
// changes, the other must too — fenced by qurl-integrations-infra
// PR #764 + this PR landing as a coordinated pair.
function formatSessionDurationSeconds(seconds) {
  if (!isPositiveFinite(seconds)) return null;
  // Math.ceil of any value in (0, 1] is 1; of any positive finite
  // number is ≥1. Combined with the isPositiveFinite guard above,
  // this never emits "0s".
  return `${Math.ceil(seconds)}s`;
}

module.exports = {
  expiryToISO,
  expiryToMs,
  formatSelfDestructLabel,
  formatSelfDestructSegment,
  formatSessionDurationSeconds,
  isPositiveFinite,
  selfDestructSelectValueToSeconds,
  isLegitimateSelfDestructSelectValue,
  SELF_DESTRUCT_PRESETS,
  SELF_DESTRUCT_NO_TIMER_VALUE,
};
