const config = require('./config');
const logger = require('./logger');

const PLACES_AUTOCOMPLETE_URL = 'https://maps.googleapis.com/maps/api/place/autocomplete/json';

async function searchPlaces(query) {
  if (!config.GOOGLE_MAPS_API_KEY) {
    logger.warn('GOOGLE_MAPS_API_KEY not set, skipping places autocomplete');
    return [];
  }

  // Cap user-supplied input before it reaches Google's URL. Google's own
  // limit is generous but a defensive 500-char guard bounds our request
  // size and log line length regardless.
  const safeQuery = String(query || '').slice(0, 500);
  const params = new URLSearchParams({
    input: safeQuery,
    key: config.GOOGLE_MAPS_API_KEY,
    types: 'establishment|geocode',
  });

  const response = await fetch(`${PLACES_AUTOCOMPLETE_URL}?${params}`, { signal: AbortSignal.timeout(5000) });
  if (!response.ok) {
    throw new Error(`Places API error: ${response.status}`);
  }

  // The Places API occasionally returns an HTML error page during outages;
  // `.json()` on that throws a SyntaxError with no context.
  let data;
  try {
    data = await response.json();
  } catch (err) {
    throw new Error(`Places API returned non-JSON response (${response.status}): ${err.message}`);
  }
  if (data.status !== 'OK' && data.status !== 'ZERO_RESULTS') {
    throw new Error(`Places API status: ${data.status}`);
  }

  return (data.predictions || []).map(p => ({
    placeId: p.place_id,
    name: p.structured_formatting?.main_text || p.description,
    address: p.structured_formatting?.secondary_text || '',
  }));
}

module.exports = { searchPlaces };
