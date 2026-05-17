/**
 * Shared jest.mock factory for `../src/places`.
 *
 * The real places module's behavior (cache, single-flight, Places I/O)
 * is exercised directly in `tests/places.test.js`. Every other test
 * file mocks it, but the mock contract has crept up over time —
 * encode/decode, shape regex, buildPlaceUrl — and the duplicate
 * implementations across `commands-comprehensive.test.js`,
 * `coverage-boost.test.js`, and `qurl-send-map.test.js` were a real
 * drift risk (one file's regex tightens, the other two silently
 * accept the looser shape).
 *
 * Centralizing here. Tests that need to assert against the mock's
 * call history use the `mockSearchPlaces` / etc. spies from the
 * caller-side wiring; the production-equivalent helpers
 * (encode/decode/buildPlaceUrl/shape regex) live here as a single
 * source of truth.
 *
 * Usage:
 *   const {
 *     mockPlacesModule,
 *     mockSearchPlaces,
 *     mockFindPlaceFromText,
 *     mockGetPlaceDetails,
 *   } = require('./helpers/places-mock');
 *   jest.mock('../src/places', () => mockPlacesModule);
 *
 * The `mock`-prefixed export name matters: jest's babel transform
 * hoists jest.mock() to the top of the file, and only `mock*`-named
 * closure variables are exempt from its top-level-reference check.
 */

// Mirror production: PLACE_ID_SHAPE_RE in src/places.js. A drift here
// means tests would smuggle a bad place_id past the mock decode while
// production rejects it — change in lockstep with the prod regex.
const PLACE_ID_SHAPE_RE = /^[A-Za-z0-9_-]{16,}$/;

const mockSearchPlaces = jest.fn().mockResolvedValue([]);
const mockFindPlaceFromText = jest.fn().mockResolvedValue(null);
const mockGetPlaceDetails = jest.fn().mockResolvedValue(null);

const mockPlacesModule = {
  searchPlaces: (...a) => mockSearchPlaces(...a),
  findPlaceFromText: (...a) => mockFindPlaceFromText(...a),
  getPlaceDetails: (...a) => mockGetPlaceDetails(...a),
  buildPlaceUrl: (name, placeId) => {
    const url = new URL('https://www.google.com/maps/search/');
    url.searchParams.set('api', '1');
    url.searchParams.set('query', name || placeId);
    url.searchParams.set('query_place_id', placeId);
    return url.toString();
  },
  PLACE_ID_SENTINEL_PREFIX: 'qurl_place:',
  PLACE_ID_SHAPE_RE,
  encodePlaceIdSentinel: (placeId) => `qurl_place:${placeId}`,
  decodePlaceIdSentinel: (value) => {
    if (typeof value !== 'string' || !value.startsWith('qurl_place:')) return null;
    const placeId = value.slice('qurl_place:'.length);
    return PLACE_ID_SHAPE_RE.test(placeId) ? placeId : null;
  },
};

module.exports = {
  mockPlacesModule,
  mockSearchPlaces,
  mockFindPlaceFromText,
  mockGetPlaceDetails,
  PLACE_ID_SHAPE_RE,
};
