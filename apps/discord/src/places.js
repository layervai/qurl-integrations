const config = require('./config');
const logger = require('./logger');

const PLACES_AUTOCOMPLETE_URL = 'https://maps.googleapis.com/maps/api/place/autocomplete/json';
const PLACES_FINDPLACE_URL = 'https://maps.googleapis.com/maps/api/place/findplacefromtext/json';
const PLACES_DETAILS_URL = 'https://maps.googleapis.com/maps/api/place/details/json';

// Wire literal that round-trips a chosen place_id through the Discord
// slash-option `location:` value. Stable across deploys — any in-flight
// confirm-card flow_state row carrying a placeId-sentinel in actualUrl
// is keyed against this literal.
const PLACE_ID_SENTINEL_PREFIX = 'qurl_place:';

function encodePlaceIdSentinel(placeId) {
  return `${PLACE_ID_SENTINEL_PREFIX}${placeId}`;
}

function decodePlaceIdSentinel(value) {
  if (typeof value !== 'string' || !value.startsWith(PLACE_ID_SENTINEL_PREFIX)) return null;
  const placeId = value.slice(PLACE_ID_SENTINEL_PREFIX.length);
  // Reject an empty payload: `qurl_place:` with no id behind it would
  // round-trip as a falsy string, and `parseLocationInput`'s
  // `if (decodedPlaceId)` check would silently fall through to the
  // URL/text branches — a sentinel that decoded to "nothing" should
  // surface as "no match" instead.
  return placeId.length > 0 ? placeId : null;
}

// Single timeout for every Places call. Both autocomplete (per
// keystroke) and resolveLocation (slash submit) share Discord's 3 s
// ACK window; 1500 ms is well above Places p99 for all three endpoints
// while leaving budget for the rest of each handler.
const PLACES_REQUEST_TIMEOUT_MS = 1500;

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
async function placesFetch(url, params) {
  const qs = new URLSearchParams({ ...params, key: config.GOOGLE_MAPS_API_KEY });
  const response = await fetch(`${url}?${qs}`, { signal: AbortSignal.timeout(PLACES_REQUEST_TIMEOUT_MS) });
  if (!response.ok) {
    throw new Error(`Places API error: ${response.status}`);
  }
  // Places occasionally returns an HTML error page during outages;
  // .json() on that throws a SyntaxError with no context.
  let data;
  try {
    data = await response.json();
  } catch (err) {
    throw new Error(`Places API returned non-JSON response (${response.status}): ${err.message}`);
  }
  return data;
}

// Bounded TTL cache for autocomplete results. Discord fires
// `searchPlaces` per keystroke; a 60 s TTL collapses a typing session
// to one Places call per distinct prefix. Cap keeps memory bounded —
// when full, drop the oldest entry (FIFO; Map preserves insertion
// order).
const AUTOCOMPLETE_CACHE_TTL_MS = 60_000;
const AUTOCOMPLETE_CACHE_MAX = 500;
const autocompleteCache = new Map();

// In-flight request dedup. Fast typists can fire multiple keystrokes
// for the same prefix before the first Places response settles; this
// returns the same in-flight promise for duplicate concurrent calls
// (single-flight) rather than paying twice.
const autocompleteInflight = new Map();

// Cache + single-flight key. Normalize to lowercase + collapsed
// whitespace so "Whitehouse", "whitehouse", and " White House "
// share the same entry — Places Autocomplete is case-insensitive,
// so the cache should be too.
function cacheKey(query) {
  return query.toLowerCase().replace(/\s+/g, ' ').trim();
}

function cacheGet(key) {
  const entry = autocompleteCache.get(key);
  if (!entry) return null;
  if (entry.expiresAt < Date.now()) {
    autocompleteCache.delete(key);
    return null;
  }
  return entry.results;
}

function cacheSet(key, results) {
  if (autocompleteCache.size >= AUTOCOMPLETE_CACHE_MAX) {
    const oldest = autocompleteCache.keys().next().value;
    if (oldest !== undefined) autocompleteCache.delete(oldest);
  }
  autocompleteCache.set(key, { results, expiresAt: Date.now() + AUTOCOMPLETE_CACHE_TTL_MS });
}

async function searchPlaces(query) {
  if (!config.GOOGLE_MAPS_API_KEY) {
    logger.warn('GOOGLE_MAPS_API_KEY not set, skipping places autocomplete');
    return [];
  }
  const safeQuery = sanitizeQueryParam(query);
  const key = cacheKey(safeQuery);
  const cached = cacheGet(key);
  if (cached) return cached;
  const inflight = autocompleteInflight.get(key);
  if (inflight) return inflight;
  const promise = (async () => {
    const data = await placesFetch(PLACES_AUTOCOMPLETE_URL, {
      input: safeQuery,
      types: 'establishment|geocode',
    });
    if (data.status !== 'OK' && data.status !== 'ZERO_RESULTS') {
      throw new Error(`Places API status: ${data.status}`);
    }
    const results = (data.predictions || []).map(p => ({
      placeId: p.place_id,
      name: p.structured_formatting?.main_text || p.description,
      address: p.structured_formatting?.secondary_text || '',
    }));
    cacheSet(key, results);
    return results;
  })().finally(() => autocompleteInflight.delete(key));
  autocompleteInflight.set(key, promise);
  return promise;
}

// Returns null when Find Place returns zero candidates; the caller
// surfaces this as a "no match" error to the sender.
async function findPlaceFromText(query) {
  if (!config.GOOGLE_MAPS_API_KEY) {
    throw new Error('GOOGLE_MAPS_API_KEY not set');
  }
  const data = await placesFetch(PLACES_FINDPLACE_URL, {
    input: sanitizeQueryParam(query),
    inputtype: 'textquery',
    fields: 'place_id,name,formatted_address',
  });
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

async function getPlaceDetails(placeId) {
  if (!config.GOOGLE_MAPS_API_KEY) {
    throw new Error('GOOGLE_MAPS_API_KEY not set');
  }
  const data = await placesFetch(PLACES_DETAILS_URL, {
    place_id: sanitizeQueryParam(placeId),
    fields: 'place_id,name,formatted_address',
  });
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

// Per Google's URL spec the `query` param is required even when
// `query_place_id` is set; we pass the canonical place name there.
// query_place_id pins the result so every recipient opens the same
// destination regardless of viewer geo.
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
  encodePlaceIdSentinel,
  decodePlaceIdSentinel,
  // Test-only: clear the cache + in-flight map between tests so leftover
  // state from one test can't satisfy a fetch expectation in the next.
  ...(process.env.NODE_ENV !== 'production' && {
    _resetAutocompleteCache: () => {
      autocompleteCache.clear();
      autocompleteInflight.clear();
    },
  }),
};
