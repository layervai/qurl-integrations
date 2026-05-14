const config = require('./config');
const logger = require('./logger');

const PLACES_AUTOCOMPLETE_URL = 'https://maps.googleapis.com/maps/api/place/autocomplete/json';
const PLACES_FINDPLACE_URL = 'https://maps.googleapis.com/maps/api/place/findplacefromtext/json';
const PLACES_DETAILS_URL = 'https://maps.googleapis.com/maps/api/place/details/json';

// Wire literal used to round-trip a chosen place_id through the Discord
// slash-option `location:` value. The autocomplete handler encodes
// selected places as `<prefix><place_id>`; parseLocationInput recognizes
// this prefix on submit and routes to a Places Details lookup so all
// recipients see the same canonical place. Without this round-trip the
// /maps/search/<text> URL we synthesize is per-viewer geo-biased (the
// bug #322-followup is fixing). Keep this string stable across deploys —
// any in-flight confirm-card flow_state row carrying a placeId-sentinel
// in actualUrl is keyed against this literal.
const PLACE_ID_SENTINEL_PREFIX = 'qurl_place:';

// Strip ASCII control chars from user-supplied input before it goes
// into the request URL. Google's own limit is generous but a 500-char
// cap bounds request size + log line length, and the control-char
// strip prevents header-injection-style payloads from smuggling
// newlines or NULs into the outgoing request URL.
function sanitizeQueryParam(value) {
  // eslint-disable-next-line no-control-regex
  return String(value || '').replace(/[\x00-\x1f\x7f]/g, '').slice(0, 500);
}

// SECURITY: every Places request URL embeds GOOGLE_MAPS_API_KEY as a
// query param (Google Places doesn't support header auth). DO NOT log
// the URL, the params, or the Request object anywhere — the logger's
// substring redact list only catches top-level meta keys, not values
// embedded inside strings. Any future error handler that touches
// `response.url` or the full request needs to strip the `key` param
// first.
async function placesFetch(url, params, timeoutMs = 5000) {
  const qs = new URLSearchParams({ ...params, key: config.GOOGLE_MAPS_API_KEY });
  const response = await fetch(`${url}?${qs}`, { signal: AbortSignal.timeout(timeoutMs) });
  if (!response.ok) {
    throw new Error(`Places API error: ${response.status}`);
  }
  // The Places API occasionally returns an HTML error page during
  // outages; .json() on that throws a SyntaxError with no context.
  let data;
  try {
    data = await response.json();
  } catch (err) {
    throw new Error(`Places API returned non-JSON response (${response.status}): ${err.message}`);
  }
  return data;
}

async function searchPlaces(query) {
  if (!config.GOOGLE_MAPS_API_KEY) {
    logger.warn('GOOGLE_MAPS_API_KEY not set, skipping places autocomplete');
    return [];
  }
  const data = await placesFetch(PLACES_AUTOCOMPLETE_URL, {
    input: sanitizeQueryParam(query),
    types: 'establishment|geocode',
  });
  if (data.status !== 'OK' && data.status !== 'ZERO_RESULTS') {
    throw new Error(`Places API status: ${data.status}`);
  }
  return (data.predictions || []).map(p => ({
    placeId: p.place_id,
    name: p.structured_formatting?.main_text || p.description,
    address: p.structured_formatting?.secondary_text || '',
  }));
}

// Resolve free text to a single canonical place. Used by the /qurl map
// server-side fallback when a sender ignores the autocomplete dropdown
// and submits raw text — we resolve to a place_id-pinned URL at send
// time so every recipient sees the same destination (vs. Google's
// per-viewer search-bias on /maps/search/<text>).
//
// Returns `null` when Find Place returns zero candidates; the caller
// surfaces this as an honest "no match" error to the sender.
async function findPlaceFromText(query, { timeoutMs } = {}) {
  if (!config.GOOGLE_MAPS_API_KEY) {
    throw new Error('GOOGLE_MAPS_API_KEY not set');
  }
  const data = await placesFetch(PLACES_FINDPLACE_URL, {
    input: sanitizeQueryParam(query),
    inputtype: 'textquery',
    fields: 'place_id,name,formatted_address',
  }, timeoutMs);
  if (data.status !== 'OK' && data.status !== 'ZERO_RESULTS') {
    throw new Error(`Places API status: ${data.status}`);
  }
  const candidate = (data.candidates || [])[0];
  if (!candidate) return null;
  return {
    placeId: candidate.place_id,
    name: candidate.name || candidate.formatted_address || '',
    address: candidate.formatted_address || '',
  };
}

// Resolve a known place_id to its canonical name + address. Used when
// the sender picks a suggestion from the autocomplete dropdown — the
// dropdown value carries the place_id sentinel only (no name, kept
// short to fit Discord's 100-char choice-value cap), so this call
// hydrates the human-readable label for the recipient embed.
async function getPlaceDetails(placeId, { timeoutMs } = {}) {
  if (!config.GOOGLE_MAPS_API_KEY) {
    throw new Error('GOOGLE_MAPS_API_KEY not set');
  }
  const data = await placesFetch(PLACES_DETAILS_URL, {
    place_id: sanitizeQueryParam(placeId),
    fields: 'place_id,name,formatted_address',
  }, timeoutMs);
  if (data.status !== 'OK') {
    if (data.status === 'NOT_FOUND' || data.status === 'INVALID_REQUEST') return null;
    throw new Error(`Places API status: ${data.status}`);
  }
  const r = data.result;
  if (!r) return null;
  return {
    placeId: r.place_id || placeId,
    name: r.name || r.formatted_address || '',
    address: r.formatted_address || '',
  };
}

// Build the canonical "show this exact place" Maps URL using Google's
// documented `?api=1` form with `query_place_id` pinning the result to
// a specific place. This bypasses the per-viewer geo bias that affects
// /maps/search/<text> URLs — the place_id parameter is the contract
// that makes every recipient open the same destination.
//
// Per Google's URL spec the `query` param is required even when
// `query_place_id` is set; we pass the canonical place name there.
function buildPlaceUrl(placeName, placeId) {
  const url = new URL('https://www.google.com/maps/search/');
  url.searchParams.set('api', '1');
  url.searchParams.set('query', placeName || placeId);
  url.searchParams.set('query_place_id', placeId);
  return url.toString();
}

module.exports = {
  searchPlaces,
  findPlaceFromText,
  getPlaceDetails,
  buildPlaceUrl,
  PLACE_ID_SENTINEL_PREFIX,
};
