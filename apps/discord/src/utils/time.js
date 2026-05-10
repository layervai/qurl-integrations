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
// modal. Discord modals are TextInput-only, so we surface the choices in
// the placeholder and accept either the friendly label or the seconds
// value below. The set is intentionally narrow so creators don't ship a
// 0.7s viewer that hits the 500ms-floor edge case in the connector by
// accident — every preset is a value we'd recommend out loud.
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

const SELF_DESTRUCT_OPTIONS_TEXT = SELF_DESTRUCT_PRESETS.map((p) => p.label).join(', ');

// Single source of truth for the modal's setMaxLength + the parser's
// length cap — both bound the input the same way so the parser never
// has to defend against strings the modal would have rejected.
const SELF_DESTRUCT_INPUT_MAX_LENGTH = 32;

// Strict decimal-seconds shape — gates the numeric branch of the parser
// so hex (`0x1`, `0x1e`) and scientific notation (`5e-1`) can't coerce
// through Number() into a preset value. Optional sign + digits + optional
// fractional part. See parseSelfDestructSeconds for the rationale.
const DECIMAL_SECONDS_RE = /^[+-]?\d+(\.\d+)?$/;

function canonicalize(s) {
  return String(s).toLowerCase().replace(/\s+/g, ' ').trim();
}

function findPresetByLabel(canonical) {
  for (const preset of SELF_DESTRUCT_PRESETS) {
    if (canonical === canonicalize(preset.label)) return preset;
  }
  return null;
}

function findPresetBySeconds(n) {
  if (!Number.isFinite(n)) return null;
  for (const preset of SELF_DESTRUCT_PRESETS) {
    if (n === preset.seconds) return preset;
  }
  return null;
}

// parseSelfDestructSeconds — strict preset matcher used by the modal handler
// in /qurl send. Empty / whitespace-only input means "no timer" (returns
// {seconds: null, error: null}). Any other input that does not match one
// of the 7 presets returns an error string with the full option list so
// the modal handler renders an inline retry, not a hard rejection.
//
// Accepted forms (case-insensitive, internal whitespace tolerated):
//   - the friendly label: "1/2 second", "5 minutes", "1 hour", ...
//   - the raw seconds value: "0.5", "30", "300", ...
function parseSelfDestructSeconds(raw) {
  const trimmed = String(raw ?? '').trim();
  if (trimmed === '') return { seconds: null, error: null };
  // Length cap bounds CPU on hostile input before any parse. The modal
  // already enforces SELF_DESTRUCT_INPUT_MAX_LENGTH via setMaxLength, so
  // a string longer than this is either an upstream caller misuse or a
  // forged interaction — fail loud.
  if (trimmed.length > SELF_DESTRUCT_INPUT_MAX_LENGTH) {
    return { seconds: null, error: 'Value is too long.' };
  }

  const canonical = canonicalize(trimmed);

  const labelMatch = findPresetByLabel(canonical);
  if (labelMatch) return { seconds: labelMatch.seconds, error: null };

  // Strict decimal notation only — Number() also accepts hex integers
  // (`0x1` → 1, `0x1e` → 30, `0x12c` → 300) and scientific notation
  // (`5e-1` → 0.5), all of which would silently coerce into preset
  // values and bypass the user-visible label set. The placeholder
  // advertises decimal seconds; honor that exactly.
  if (DECIMAL_SECONDS_RE.test(canonical)) {
    const numericMatch = findPresetBySeconds(Number(canonical));
    if (numericMatch) return { seconds: numericMatch.seconds, error: null };
  }

  return {
    seconds: null,
    error: `Choose one of: ${SELF_DESTRUCT_OPTIONS_TEXT}.`,
  };
}

// formatSelfDestructLabel — renders a stored seconds value as the matching
// preset's friendly label (e.g., 0.5 → "1/2 second"). Used by the form to
// echo what the user picked. Falls back to a compact "Ns" rendering for
// any seconds value that isn't a known preset, which is unreachable today
// but defends against a future caller (e.g., a backfilled config) feeding
// in an off-preset value.
function formatSelfDestructLabel(seconds) {
  const match = findPresetBySeconds(seconds);
  if (match) return match.label;
  return `${seconds}s`;
}

module.exports = {
  expiryToISO,
  expiryToMs,
  parseSelfDestructSeconds,
  formatSelfDestructLabel,
  SELF_DESTRUCT_PRESETS,
  SELF_DESTRUCT_MIN_SECONDS,
  SELF_DESTRUCT_MAX_SECONDS,
  SELF_DESTRUCT_OPTIONS_TEXT,
  SELF_DESTRUCT_INPUT_MAX_LENGTH,
};
