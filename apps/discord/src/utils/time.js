const logger = require('../logger');

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
  if (!Number.isFinite(ms) || ms <= 0) return null;
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

// Self-destruct timer presets — the 7 durations exposed in the /qurl send
// dropdown. Curated narrow set so creators don't ship a 0.7s viewer that
// hits the 500ms-floor edge case in the connector by accident; every
// preset is a value we'd recommend out loud.
//
// The wire field forwarded to the connector is `viewer_ttl_seconds`
// (qurl-s3-connector PR #477). Connector contract is 0.5–3600 inclusive,
// so every preset here is in range; clamping is unreachable.
const SELF_DESTRUCT_PRESETS = Object.freeze([
  Object.freeze({ seconds: 0.5, label: '1/2 second' }),
  Object.freeze({ seconds: 1, label: '1 second' }),
  Object.freeze({ seconds: 5, label: '5 seconds' }),
  Object.freeze({ seconds: 30, label: '30 seconds' }),
  Object.freeze({ seconds: 300, label: '5 minutes' }),
  Object.freeze({ seconds: 1800, label: '30 minutes' }),
  Object.freeze({ seconds: 3600, label: '1 hour' }),
]);

const SELF_DESTRUCT_MIN_SECONDS = SELF_DESTRUCT_PRESETS[0].seconds;
const SELF_DESTRUCT_MAX_SECONDS = SELF_DESTRUCT_PRESETS[SELF_DESTRUCT_PRESETS.length - 1].seconds;

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
// from the /qurl send form into the seconds the bot persists and forwards.
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
  if (!Number.isFinite(seconds)) return '(invalid)';
  return `${seconds}s`;
}

module.exports = {
  expiryToISO,
  expiryToMs,
  formatSelfDestructLabel,
  selfDestructSelectValueToSeconds,
  SELF_DESTRUCT_PRESETS,
  SELF_DESTRUCT_MIN_SECONDS,
  SELF_DESTRUCT_MAX_SECONDS,
  SELF_DESTRUCT_NO_TIMER_VALUE,
};
